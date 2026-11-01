/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/types"

	rss2discordv1alpha1 "github.com/maverickd650-labs/rss2discord/api/v1alpha1"
)

// These specs persist every status through the API server (applyStatus, then
// a fresh Get by a new reconciler), so they check what the deployed CRD
// schema and server-side apply keep across a controller restart or a
// rollback, not just what an in-memory FeedStatus does.
var _ = Describe("FeedGroup status migration across restarts and rollbacks", func() {
	ctx := context.Background()

	// persistedFeedStatus reads name's status for feedURL back from the API server.
	persistedFeedStatus := func(name, feedURL string) *rss2discordv1alpha1.FeedStatus {
		GinkgoHelper()
		fg := &rss2discordv1alpha1.FeedGroup{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, fg)).To(Succeed())
		fs := feedStatusFor(fg, feedURL)
		Expect(fs).NotTo(BeNil())
		return fs
	}

	// writeFeedStatus applies mutate to name's persisted status for feedURL the
	// way the controller writes status: server-side apply as statusFieldOwner,
	// with every field the typed status omits released.
	writeFeedStatus := func(r *FeedGroupReconciler, name, feedURL string, mutate func(*rss2discordv1alpha1.FeedStatus)) {
		GinkgoHelper()
		fg := &rss2discordv1alpha1.FeedGroup{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, fg)).To(Succeed())
		ensureFeedStatuses(fg)
		mutate(feedStatusFor(fg, feedURL))
		Expect(r.applyStatus(ctx, fg)).To(Succeed())
	}

	It("keeps the replay cap across a restart while migrated sends keep failing", func() {
		const name = "test-feedgroup-migration-restart"
		base := time.Now().Add(-48 * time.Hour)
		entries := make([]testEntry, 0, maxLastSentPerFeed+10)
		links := make([]string, 0, maxLastSentPerFeed+10)
		for i := range maxLastSentPerFeed + 10 {
			link := fmt.Sprintf("https://example.com/restart/changelog#v%d", i)
			links = append(links, link)
			entries = append(entries, testEntry{
				title:       fmt.Sprintf("restart v%d", i),
				description: "Changelog entry",
				link:        link,
				pubDate:     base.Add(time.Duration(i) * time.Minute).Format(time.RFC1123Z),
			})
		}
		discordServer, rssServer, reconciler := setUpFeedGroup(ctx, name, "discord-webhook-migration-restart",
			createRSSFeed(entries...), withRetries("5m", 3))

		By("Seeding a status written before stateVersion existed")
		writeFeedStatus(reconciler, name, rssServer.URL(), func(fs *rss2discordv1alpha1.FeedStatus) {
			fs.LastChecked = base.UTC().Format(time.RFC3339)
			fs.Initialized = true
			fs.LastSeenEntry = normalizeIdentity(links[0])
			fs.LastSent[hashIdentity(normalizeIdentity(links[0]))] = base.UTC().Format(time.RFC3339)
		})
		Expect(persistedFeedStatus(name, rssServer.URL()).StateVersion).To(BeZero())

		By("Migrating while every send fails")
		discordServer.FailNextRequests(1000, http.StatusInternalServerError)
		_, err := reconcileFeedGroup(ctx, reconciler, name)
		Expect(err).NotTo(HaveOccurred())
		Expect(discordServer.MessageCount()).To(BeZero())
		fs := persistedFeedStatus(name, rssServer.URL())
		Expect(fs.StateVersion).To(Equal(int32(currentStateVersion)), "the migration must be persisted")
		// The cap keeps the newest defaultCatchUpLimit of the 59 replayed
		// entries, so the persisted watermark sits just before them.
		Expect(fs.LastSeenEntry).To(Equal(normalizeLinkIdentity(links[len(links)-defaultCatchUpLimit-1])))

		By("Restarting the controller once Discord recovers")
		discordServer.FailNextRequests(0, http.StatusInternalServerError)
		restarted := newTestReconciler(discordServer)
		for range 3 {
			_, err = reconcileFeedGroup(ctx, restarted, name)
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(discordServer.MessageCount()).To(Equal(defaultCatchUpLimit), "only the capped replay is delivered, once")
		for i, body := range discordServer.Bodies() {
			Expect(body).To(ContainSubstring(fmt.Sprintf("restart v%d", len(links)-defaultCatchUpLimit+i)))
		}
	})

	It("re-migrates without resending after a rollback to a controller without stateVersion", func() {
		const name = "test-feedgroup-migration-rollback"
		base := time.Now().Add(-48 * time.Hour)
		feed := []testEntry{
			{title: "rollback guid", description: "d", guid: "rollback-guid-1", link: "https://example.com/rollback/guid",
				pubDate: base.Format(time.RFC1123Z)},
			{title: "rollback anchor", description: "d", link: "https://example.com/rollback/news#one",
				pubDate: base.Add(time.Minute).Format(time.RFC1123Z)},
			{title: "rollback ref", description: "d", link: "https://example.com/rollback/news?ref=rss",
				pubDate: base.Add(2 * time.Minute).Format(time.RFC1123Z)},
		}
		discordServer, rssServer, reconciler := setUpFeedGroup(ctx, name, "discord-webhook-migration-rollback",
			createRSSFeed(feed...), withRetries("5m", 3))

		By("Delivering the feed with this controller")
		_, err := reconcileFeedGroup(ctx, reconciler, name)
		Expect(err).NotTo(HaveOccurred())
		Expect(discordServer.MessageCount()).To(Equal(len(feed)))
		Expect(persistedFeedStatus(name, rssServer.URL()).StateVersion).To(Equal(int32(currentStateVersion)))

		By("Writing status the way an older controller would")
		// An older controller identifies links without fragment and ref, so
		// it treats this controller's watermark as scrolled out and records
		// its own keys and watermark. Its typed status has no stateVersion, so
		// its apply, as the same field owner, releases the field.
		writeFeedStatus(reconciler, name, rssServer.URL(), func(fs *rss2discordv1alpha1.FeedStatus) {
			for _, entry := range feed[1:] {
				fs.LastSent[hashIdentity(normalizeIdentity(entry.link))] = base.UTC().Format(time.RFC3339)
			}
			fs.LastSeenEntry = normalizeIdentity(feed[len(feed)-1].link)
			fs.StateVersion = 0
		})
		Expect(persistedFeedStatus(name, rssServer.URL()).StateVersion).To(BeZero(),
			"an apply without stateVersion must clear it, so the next upgrade migrates again")

		By("Upgrading again with one new entry")
		fresh := testEntry{
			title: "rollback fresh", description: "d", link: "https://example.com/rollback/fresh#new",
			pubDate: base.Add(3 * time.Minute).Format(time.RFC1123Z),
		}
		rssServer.SetFeedContent(createRSSFeed(slices.Concat(feed, []testEntry{fresh})...))
		upgraded := newTestReconciler(discordServer)
		for range 2 {
			_, err = reconcileFeedGroup(ctx, upgraded, name)
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(discordServer.MessageCount()).To(Equal(len(feed)+1), "only the new entry may be sent")
		Expect(discordServer.Bodies()[len(feed)]).To(ContainSubstring("rollback fresh"))
		Expect(persistedFeedStatus(name, rssServer.URL()).StateVersion).To(Equal(int32(currentStateVersion)))
	})
})
