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
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	rss2discordv1alpha1 "github.com/maverickd650/rss2discord-operator/api/v1alpha1"
	"github.com/maverickd650/rss2discord-operator/internal/discord"
	"github.com/maverickd650/rss2discord-operator/internal/rss"
)

const (
	secretURLKey             = "url"
	defaultInterval          = "30m"
	feedGroupNameBasic       = "test-feedgroup"
	feedGroupNameNoSecret    = "test-feedgroup-no-secret"
	feedGroupNameRetry       = "test-feedgroup-retry"
	feedGroupNameFilter      = "test-feedgroup-filter"
	feedGroupNameKeywords    = "test-feedgroup-keywords"
	feedGroupNamePaused      = "test-feedgroup-paused"
	feedGroupNameTimestamp   = "test-feedgroup-timestamp"
	exampleFeedURL           = "https://example.com/feed.xml"
	discordWebhookSecretName = "discord-webhook"
)

// testRSSClient is a plain (non-SSRF-guarded) RSS client used in tests so
// that reconciliation can reach mock servers bound to loopback addresses.
// Production code goes through rss.NewClient(nil), which guards against
// connecting to non-public addresses.
func testRSSClient() *rss.Client {
	return rss.NewClient(&http.Client{})
}

// feedConditionReason returns the Reason of url's condType condition within
// fg.Status.Feeds, or "" if either the feed or that condition isn't present.
// A handful of envtest assertions below want to check the classified Reason
// (e.g. "HTTP404") on a feed's Reachable/Delivered condition without
// repeating the find-feed-then-find-condition boilerplate at every call
// site.
func feedConditionReason(fg *rss2discordv1alpha1.FeedGroup, url, condType string) string {
	fs := feedStatusFor(fg, url)
	if fs == nil {
		return ""
	}
	cond := apimeta.FindStatusCondition(fs.Conditions, condType)
	if cond == nil {
		return ""
	}
	return cond.Reason
}

// feedsWithError counts how many of fg's feeds currently have a non-empty
// LastError, the slice-based equivalent of the old `len(Status.LastError)`
// map-length check.
func feedsWithError(fg *rss2discordv1alpha1.FeedGroup) int {
	count := 0
	for _, fs := range fg.Status.Feeds {
		if fs.LastError != "" {
			count++
		}
	}
	return count
}

// MockRSSServer provides a mock RSS feed for testing
type MockRSSServer struct {
	server          *httptest.Server
	feedContent     string
	statusCode      int
	etag            string
	requestCount    int
	ifNoneMatchSeen []string
}

// NewMockRSSServer creates a new mock RSS server with test feed data
func NewMockRSSServer(feedContent string) *MockRSSServer {
	m := &MockRSSServer{
		feedContent: feedContent,
		statusCode:  http.StatusOK,
	}

	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		m.requestCount++
		ifNoneMatch := r.Header.Get("If-None-Match")
		m.ifNoneMatchSeen = append(m.ifNoneMatchSeen, ifNoneMatch)

		if m.etag != "" {
			w.Header().Set("ETag", m.etag)
			if ifNoneMatch == m.etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}

		w.WriteHeader(m.statusCode)
		_, _ = w.Write([]byte(m.feedContent))
	}))

	return m
}

// SetETag makes the server emit the given ETag header and respond 304 to
// any request whose If-None-Match matches it.
func (m *MockRSSServer) SetETag(etag string) {
	m.etag = etag
}

// SetFeedContent replaces the feed body returned by subsequent requests, so
// a test can simulate a feed's contents changing between two reconciles.
func (m *MockRSSServer) SetFeedContent(feedContent string) {
	m.feedContent = feedContent
}

// RequestCount returns how many GET requests the server has received.
func (m *MockRSSServer) RequestCount() int {
	return m.requestCount
}

// IfNoneMatchSeen returns the If-None-Match header value from each request
// received so far, in arrival order (empty string if not sent).
func (m *MockRSSServer) IfNoneMatchSeen() []string {
	return m.ifNoneMatchSeen
}

func (m *MockRSSServer) URL() string {
	return m.server.URL
}

func (m *MockRSSServer) Close() {
	m.server.Close()
}

// MockDiscordServer provides a mock Discord webhook for testing
type MockDiscordServer struct {
	server           *httptest.Server
	webhookURL       string
	messagesReceived int
	deliveryAttempts int
	bodiesReceived   []string
	failNext         int
	failStatus       int
	failRetryAfter   string
}

// mockWebhookHost is the host of every mock webhook URL: Discord's own, so
// the URL passes the production allowlist.
const mockWebhookHost = "discord.com"

// mockWebhookIDs numbers the webhook URL of each MockDiscordServer so no two
// servers share one, as no two real webhooks do.
var mockWebhookIDs atomic.Int64

// NewMockDiscordServer creates a new mock Discord server on httptest's
// in-memory network, shut down when tb's test ends. Its client (see
// DiscordClientBuilder) routes every request to it, so URL can be a real
// https://discord.com webhook URL that passes the production
// discord.AllowedWebhookHosts check unchanged -- tests never widen that
// SSRF/domain-confusion guard.
func NewMockDiscordServer(tb testing.TB) *MockDiscordServer {
	webhookPath := fmt.Sprintf("/api/webhooks/%d/mock-token", mockWebhookIDs.Add(1))
	d := &MockDiscordServer{
		webhookURL: "https://" + mockWebhookHost + webhookPath,
	}

	d.server = httptest.NewTestServer(tb, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The client routes every host here, so answer only this server's
		// own webhook -- as Discord would -- and a send to the wrong URL
		// still fails.
		if r.Host != mockWebhookHost || r.URL.Path != webhookPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		// deliveryAttempts counts every POST the controller actually made,
		// including ones that fail, so tests can assert the controller stopped
		// sending (e.g. after a 429) rather than only how many succeeded.
		d.deliveryAttempts++

		if d.failNext > 0 {
			d.failNext--
			if d.failRetryAfter != "" {
				w.Header().Set("Retry-After", d.failRetryAfter)
			}
			w.WriteHeader(d.failStatus)
			return
		}

		body, _ := io.ReadAll(r.Body)
		d.messagesReceived++
		d.bodiesReceived = append(d.bodiesReceived, string(body))
		w.WriteHeader(http.StatusNoContent)
	}))

	return d
}

// FailNextRequests makes the next n webhook deliveries fail with statusCode
// instead of succeeding, so tests can exercise send-failure/retry paths.
func (d *MockDiscordServer) FailNextRequests(n int, statusCode int) {
	d.failNext = n
	d.failStatus = statusCode
}

// FailNextRequestsRateLimited makes the next n webhook deliveries return a
// 429 with the given Retry-After header, so tests can exercise the
// controller's rate-limit backoff path.
func (d *MockDiscordServer) FailNextRequestsRateLimited(n int, retryAfter string) {
	d.failNext = n
	d.failStatus = http.StatusTooManyRequests
	d.failRetryAfter = retryAfter
}

// Bodies returns the raw request bodies received so far, in arrival order.
func (d *MockDiscordServer) Bodies() []string {
	return d.bodiesReceived
}

// URL returns this server's webhook URL. It is only reachable through the
// clients DiscordClientBuilder builds.
func (d *MockDiscordServer) URL() string {
	return d.webhookURL
}

func (d *MockDiscordServer) Close() {
	d.server.Close()
}

func (d *MockDiscordServer) MessageCount() int {
	return d.messagesReceived
}

// DeliveryAttempts returns how many webhook POSTs the controller made,
// counting failed deliveries (e.g. 429s) as well as successful ones.
func (d *MockDiscordServer) DeliveryAttempts() int {
	return d.deliveryAttempts
}

// DiscordClientBuilder returns a DiscordClientBuilder whose clients send
// every request, over TLS, to this mock server.
func (d *MockDiscordServer) DiscordClientBuilder() func(webhookURL string) MessageSender {
	httpClient := d.server.Client()
	return func(webhookURL string) MessageSender {
		return discord.NewClient(webhookURL, discord.WithHTTPClient(httpClient))
	}
}

const testNamespace = "default"

// testEntry describes one <item> in a feed built by createRSSFeed.
type testEntry struct {
	title       string
	description string
	link        string
	pubDate     string
	guid        string
}

// createWebhookSecret creates a Secret in the test namespace whose secretURLKey
// holds webhookURL.
func createWebhookSecret(ctx context.Context, name, webhookURL string) {
	GinkgoHelper()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Data:       map[string][]byte{secretURLKey: []byte(webhookURL)},
	}
	Expect(k8sClient.Create(ctx, secret)).To(Succeed())
}

type feedGroupOption func(*rss2discordv1alpha1.FeedGroup)

// withRetries sets the retry interval and retry count.
func withRetries(retryInterval string, retries int32) feedGroupOption {
	return func(fg *rss2discordv1alpha1.FeedGroup) {
		fg.Spec.RetryInterval = retryInterval
		fg.Spec.Retries = retries
	}
}

// newTestFeedGroup builds a FeedGroup in the test namespace that reads its
// webhook URL from secretName and watches a single feed at feedURL, with
// defaultInterval. Opts adjust the result.
func newTestFeedGroup(name, secretName, feedURL string, opts ...feedGroupOption) *rss2discordv1alpha1.FeedGroup {
	return newTestFeedGroupFeeds(name, secretName, []rss2discordv1alpha1.FeedSpec{{RSSUrl: feedURL}}, opts...)
}

// newTestFeedGroupFeeds is newTestFeedGroup for a FeedGroup that watches the
// given feeds.
func newTestFeedGroupFeeds(name, secretName string, feeds []rss2discordv1alpha1.FeedSpec, opts ...feedGroupOption) *rss2discordv1alpha1.FeedGroup {
	fg := &rss2discordv1alpha1.FeedGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: rss2discordv1alpha1.FeedGroupSpec{
			DiscordWebhookSecretRef: corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
				Key:                  secretURLKey,
			},
			Interval: defaultInterval,
			Feeds:    feeds,
		},
	}
	for _, opt := range opts {
		opt(fg)
	}
	return fg
}

// newTestReconciler builds a reconciler wired to the unguarded RSS client and,
// when discordServer is non-nil, to that mock Discord server.
func newTestReconciler(discordServer *MockDiscordServer) *FeedGroupReconciler {
	r := &FeedGroupReconciler{
		Client:    k8sClient,
		Scheme:    k8sClient.Scheme(),
		RSSClient: testRSSClient(),
	}
	if discordServer != nil {
		r.DiscordClientBuilder = discordServer.DiscordClientBuilder()
	}
	return r
}

// setUpFeedGroup starts mock Discord and RSS servers (closed when the spec
// ends), creates the webhook Secret and a FeedGroup watching the RSS server,
// and returns the servers plus a reconciler wired to them. Opts adjust the
// FeedGroup before it is created.
func setUpFeedGroup(ctx context.Context, name, secretName, feedXML string, opts ...feedGroupOption) (*MockDiscordServer, *MockRSSServer, *FeedGroupReconciler) {
	GinkgoHelper()
	discordServer := NewMockDiscordServer(GinkgoTB())
	DeferCleanup(discordServer.Close)
	rssServer := NewMockRSSServer(feedXML)
	DeferCleanup(rssServer.Close)

	createWebhookSecret(ctx, secretName, discordServer.URL())
	Expect(k8sClient.Create(ctx, newTestFeedGroup(name, secretName, rssServer.URL(), opts...))).To(Succeed())
	return discordServer, rssServer, newTestReconciler(discordServer)
}

// reconcileFeedGroup runs one reconcile of the named FeedGroup in the test
// namespace, as if a full poll interval had passed since the previous one:
// feeds checked "just now" are otherwise skipped as not yet due (see
// feedDue). Use reconcileFeedGroupNow to exercise that gate itself.
func reconcileFeedGroup(ctx context.Context, r *FeedGroupReconciler, name string) (reconcile.Result, error) {
	backdateFeedChecks(ctx, name, time.Hour)
	return reconcileFeedGroupNow(ctx, r, name)
}

// reconcileFeedGroupNow runs one reconcile without advancing the clock.
func reconcileFeedGroupNow(ctx context.Context, r *FeedGroupReconciler, name string) (reconcile.Result, error) {
	return r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: testNamespace},
	})
}

// backdateFeedChecks sets every checked feed's LastChecked to ago in the past,
// e.g. an hour to make the next reconcile treat the feed as due.
func backdateFeedChecks(ctx context.Context, name string, ago time.Duration) {
	GinkgoHelper()
	var fg rss2discordv1alpha1.FeedGroup
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, &fg); err != nil {
		return // not created yet / already gone; the reconcile reports it
	}
	changed := false
	for i := range fg.Status.Feeds {
		if fg.Status.Feeds[i].LastChecked != "" {
			fg.Status.Feeds[i].LastChecked = time.Now().Add(-ago).UTC().Format(time.RFC3339)
			changed = true
		}
	}
	if changed {
		Expect(k8sClient.Status().Update(ctx, &fg)).To(Succeed())
	}
}

// Helper function to create an RSS feed XML
func createRSSFeed(entries ...testEntry) string {
	var feed strings.Builder
	feed.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>Test Feed</title>
    <link>https://example.com</link>
    <description>Test Feed</description>
`)

	for _, entry := range entries {
		fmt.Fprintf(&feed, `    <item>
      <title>%s</title>
      <description>%s</description>
      <link>%s</link>
      <pubDate>%s</pubDate>
      <guid>%s</guid>
    </item>
`, entry.title, entry.description, entry.link, entry.pubDate, entry.guid)
	}

	feed.WriteString(`  </channel>
</rss>`)
	return feed.String()
}

var _ = Describe("FeedGroup Controller", func() {
	const (
		namespace = "default"
	)

	var (
		ctx context.Context
	)

	BeforeEach(func() {
		ctx = context.Background()

		// Create namespace if it doesn't exist
		ns := &corev1.Namespace{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: namespace}, ns); err != nil {
			ns = &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: namespace},
			}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		}
	})

	Describe("Successful RSS to Discord flow", func() {
		It("should fetch RSS entries and track status", func() {
			By("Creating mock Discord and RSS servers, the webhook secret and the FeedGroup")
			_, rssServer, reconciler := setUpFeedGroup(ctx, feedGroupNameBasic, discordWebhookSecretName, createRSSFeed(
				testEntry{
					title:       "Test Article 1",
					description: "This is a test article",
					link:        "https://example.com/article1",
					pubDate:     time.Now().Add(-1 * time.Hour).Format(time.RFC1123Z),
					guid:        "article-1",
				},
				testEntry{
					title:       "Test Article 2",
					description: "This is another test article",
					link:        "https://example.com/article2",
					pubDate:     time.Now().Format(time.RFC1123Z),
					guid:        "article-2",
				},
			), withRetries("5m", 3), func(fg *rss2discordv1alpha1.FeedGroup) {
				fg.Spec.Format = "**{{.Title}}**\n{{.Description}}\n[Read more]({{.Link}})"
			})

			By("Running reconciliation")

			result, err := reconcileFeedGroup(ctx, reconciler, feedGroupNameBasic)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			By("Verifying FeedGroup status was updated")
			updated := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupNameBasic, Namespace: namespace}, updated)).To(Succeed())
			Expect(updated.Spec.CatchUpLimit).To(HaveValue(Equal(int32(5))), "omitted catchUpLimit must default to 5")

			Expect(feedStatusFor(updated, rssServer.URL()).LastChecked).NotTo(BeEmpty())
			Expect(feedStatusFor(updated, rssServer.URL()).LastSeenEntry).NotTo(BeEmpty())

			By("Verifying no errors in status")
			Expect(feedsWithError(updated)).To(Equal(0))

			By("Verifying the Ready condition reflects success")
			readyCondition := apimeta.FindStatusCondition(updated.Status.Conditions, rss2discordv1alpha1.ConditionTypeReady)
			Expect(readyCondition).NotTo(BeNil())
			Expect(readyCondition.Status).To(Equal(metav1.ConditionTrue))
			Expect(updated.Status.ObservedGeneration).To(Equal(updated.Generation))
		})
	})

	Describe("Conditional RSS fetches", func() {
		It("should send a stored ETag on the next reconcile and skip re-processing on 304", func() {
			const feedGroupName = "test-feedgroup-conditional-get"

			By("Creating mock Discord and RSS servers, the webhook secret and the FeedGroup")
			discordServer, rssServer, reconciler := setUpFeedGroup(ctx, feedGroupName, "discord-webhook-conditional", createRSSFeed(
				testEntry{
					title:       "Conditional Article",
					description: "First fetch",
					link:        "https://example.com/conditional-article",
					pubDate:     time.Now().Format(time.RFC1123Z),
					guid:        "conditional-article",
				},
			), withRetries("5m", 3))

			By("Creating a mock RSS feed server that returns an ETag")
			rssServer.SetETag(`"v1"`)

			By("Running the first reconciliation")
			_, err := reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())
			Expect(discordServer.MessageCount()).To(Equal(1))
			Expect(rssServer.IfNoneMatchSeen()).To(Equal([]string{""}))

			afterFirst := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, afterFirst)).To(Succeed())
			Expect(feedStatusFor(afterFirst, rssServer.URL()).ETag).To(Equal(`"v1"`))
			lastSeenAfterFirst := feedStatusFor(afterFirst, rssServer.URL()).LastSeenEntry
			Expect(lastSeenAfterFirst).NotTo(BeEmpty())

			By("Running a second reconciliation against the unchanged feed")
			_, err = reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the second request carried the stored ETag and got a 304")
			Expect(rssServer.RequestCount()).To(Equal(2))
			Expect(rssServer.IfNoneMatchSeen()[1]).To(Equal(`"v1"`))

			By("Verifying no new message was sent and status was left untouched")
			Expect(discordServer.MessageCount()).To(Equal(1))

			afterSecond := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, afterSecond)).To(Succeed())
			Expect(feedStatusFor(afterSecond, rssServer.URL()).LastSeenEntry).To(Equal(lastSeenAfterFirst))
			Expect(feedsWithError(afterSecond)).To(Equal(0))
			Expect(feedStatusFor(afterSecond, rssServer.URL()).RetryCount).To(Equal(int32(0)))
		})

		It("should not refetch a feed that was checked less than one interval ago", func() {
			const feedGroupName = "test-feedgroup-interval-gate"

			discordServer, rssServer, reconciler := setUpFeedGroup(ctx, feedGroupName, "discord-webhook-interval-gate", createRSSFeed(
				testEntry{
					title:   "Gate Article",
					link:    "https://example.com/gate-article",
					pubDate: time.Now().Format(time.RFC1123Z),
					guid:    "gate-article",
				},
			), withRetries("5m", 3))

			_, err := reconcileFeedGroupNow(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())
			Expect(rssServer.RequestCount()).To(Equal(1))

			By("Reconciling again immediately, e.g. after an operator restart or an unrelated trigger")
			_, err = reconcileFeedGroupNow(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())
			Expect(rssServer.RequestCount()).To(Equal(1), "a feed checked moments ago must not be fetched again")
			Expect(discordServer.MessageCount()).To(Equal(1))

			By("Reconciling part-way through the interval, which must requeue for the remaining time, not a full interval")
			backdateFeedChecks(ctx, feedGroupName, 20*time.Minute)
			res, err := reconcileFeedGroupNow(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeNumerically("~", 10*time.Minute, 10*time.Second))
			Expect(rssServer.RequestCount()).To(Equal(1))

			By("Reconciling once the interval has elapsed")
			_, err = reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())
			Expect(rssServer.RequestCount()).To(Equal(2))
		})
	})

	Describe("Conditional GET validators after a send failure", func() {
		It("should not persist the new ETag until the entry is actually sent", func() {
			const feedGroupName = "test-feedgroup-conditional-get-retry"

			By("Creating mock Discord webhook server that fails the first delivery")
			discordServer, rssServer, reconciler := setUpFeedGroup(ctx, feedGroupName, "discord-webhook-conditional-retry", createRSSFeed(
				testEntry{
					title:       "Retry Article",
					description: "Should survive a failed send",
					link:        "https://example.com/retry-article",
					pubDate:     time.Now().Format(time.RFC1123Z),
					guid:        "retry-article",
				},
			), withRetries("5m", 3))
			discordServer.FailNextRequests(1, http.StatusInternalServerError)

			By("Creating a mock RSS feed server that returns an ETag")
			rssServer.SetETag(`"v1"`)

			By("Running the first reconciliation, where the Discord send fails")
			_, err := reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())
			Expect(discordServer.MessageCount()).To(Equal(0))

			By("Verifying the new ETag was NOT persisted, since the entry was never sent")
			afterFirst := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, afterFirst)).To(Succeed())
			Expect(feedStatusFor(afterFirst, rssServer.URL()).ETag).To(BeEmpty())
			Expect(feedStatusFor(afterFirst, rssServer.URL()).LastSeenEntry).To(BeEmpty())
			Expect(feedStatusFor(afterFirst, rssServer.URL()).LastError).NotTo(BeEmpty())

			By("Running a second reconciliation, where the Discord send succeeds")
			_, err = reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the previously unsent entry was retried and delivered")
			Expect(discordServer.MessageCount()).To(Equal(1))
			Expect(rssServer.IfNoneMatchSeen()[1]).To(BeEmpty(), "second fetch should not have sent the never-persisted ETag")

			afterSecond := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, afterSecond)).To(Succeed())
			Expect(feedStatusFor(afterSecond, rssServer.URL()).ETag).To(Equal(`"v1"`))
			Expect(feedStatusFor(afterSecond, rssServer.URL()).LastSeenEntry).NotTo(BeEmpty())
			Expect(feedsWithError(afterSecond)).To(Equal(0))
		})
	})

	Describe("LastSeenEntry no longer present in the feed", func() {
		It("should fall back to catch-up instead of silently skipping every entry forever", func() {
			const feedGroupName = "test-feedgroup-stale-lastseen"

			By("Creating mock Discord and RSS servers, the webhook secret and the FeedGroup")
			discordServer, rssServer, reconciler := setUpFeedGroup(ctx, feedGroupName, "discord-webhook-stale-lastseen", createRSSFeed(testEntry{
				title:       "Original Article",
				description: "The only entry in the feed's initial window",
				link:        "https://example.com/original-article",
				pubDate:     time.Now().Add(-1 * time.Hour).Format(time.RFC1123Z),
				guid:        "original-article",
			}), withRetries("5m", 3), func(fg *rss2discordv1alpha1.FeedGroup) {
				fg.Spec.CatchUpLimit = new(int32(5))
			})

			By("Running the first reconciliation, which sends and records the original entry")
			_, err := reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())
			Expect(discordServer.MessageCount()).To(Equal(1))

			afterFirst := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, afterFirst)).To(Succeed())
			Expect(feedStatusFor(afterFirst, rssServer.URL()).LastSeenEntry).To(Equal("original-article"))

			By("Replacing the feed's window so the recorded entry is no longer present at all")
			rssServer.SetFeedContent(createRSSFeed(
				testEntry{
					title:       "Replacement Article 1",
					description: "Published after the original scrolled out of the feed's window",
					link:        "https://example.com/replacement-1",
					pubDate:     time.Now().Add(-30 * time.Minute).Format(time.RFC1123Z),
					guid:        "replacement-1",
				},
				testEntry{
					title:       "Replacement Article 2",
					description: "Also new",
					link:        "https://example.com/replacement-2",
					pubDate:     time.Now().Format(time.RFC1123Z),
					guid:        "replacement-2",
				},
			))

			By("Running a second reconciliation against the changed feed")
			_, err = reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying both new entries were sent instead of being silently skipped forever")
			Expect(discordServer.MessageCount()).To(Equal(3))

			afterSecond := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, afterSecond)).To(Succeed())
			Expect(feedStatusFor(afterSecond, rssServer.URL()).LastSeenEntry).To(Equal("replacement-2"))

			By("Adding one more entry while keeping the now-recorded entry present")
			rssServer.SetFeedContent(createRSSFeed(
				testEntry{
					title:       "Replacement Article 1",
					description: "Published after the original scrolled out of the feed's window",
					link:        "https://example.com/replacement-1",
					pubDate:     time.Now().Add(-30 * time.Minute).Format(time.RFC1123Z),
					guid:        "replacement-1",
				},
				testEntry{
					title:       "Replacement Article 2",
					description: "Also new",
					link:        "https://example.com/replacement-2",
					pubDate:     time.Now().Format(time.RFC1123Z),
					guid:        "replacement-2",
				},
				testEntry{
					title:       "Replacement Article 3",
					description: "Newest entry, published after replacement-2",
					link:        "https://example.com/replacement-3",
					pubDate:     time.Now().Add(1 * time.Minute).Format(time.RFC1123Z),
					guid:        "replacement-3",
				},
			))

			By("Running a third reconciliation against the feed with the recorded entry still present")
			_, err = reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying only the genuinely new entry was sent, since the recorded one was found in the feed")
			Expect(discordServer.MessageCount()).To(Equal(4))

			afterThird := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, afterThird)).To(Succeed())
			Expect(feedStatusFor(afterThird, rssServer.URL()).LastSeenEntry).To(Equal("replacement-3"))
		})
	})

	Describe("Catch-up limit", func() {
		It("should cap how many backlog entries are sent on first reconcile", func() {
			const feedGroupName = "test-feedgroup-catchup"

			By("Creating a mock RSS feed server with a long backlog")
			entries := make([]testEntry, 0, 10)
			for i := range 10 {
				entries = append(entries, testEntry{
					title:       fmt.Sprintf("Backlog Article %d", i),
					description: "An old article",
					link:        fmt.Sprintf("https://example.com/article%d", i),
					pubDate:     time.Now().Add(-time.Duration(10-i) * time.Hour).Format(time.RFC1123Z),
					guid:        fmt.Sprintf("backlog-%d", i),
				})
			}

			discordServer, _, reconciler := setUpFeedGroup(ctx, feedGroupName, "discord-webhook-catchup", createRSSFeed(entries...), withRetries("5m", 3), func(fg *rss2discordv1alpha1.FeedGroup) {
				fg.Spec.CatchUpLimit = new(int32(3))
			})

			By("Running reconciliation")

			_, err := reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying only the configured catch-up limit was sent")
			Expect(discordServer.MessageCount()).To(Equal(3))
		})

		It("should skip the backlog and deliver only newer entries when catchUpLimit is explicitly 0", func() {
			const feedGroupName = "test-feedgroup-skip-backlog"

			entries := make([]testEntry, 0, 10)
			for i := 1; i <= 10; i++ {
				entries = append(entries, testEntry{
					title:       fmt.Sprintf("Backlog Article %d", i),
					description: "An old article",
					link:        fmt.Sprintf("https://example.com/skip%d", i),
					pubDate:     time.Now().Add(-time.Duration(20-i) * time.Hour).Format(time.RFC1123Z),
					guid:        fmt.Sprintf("skip-backlog-%d", i),
				})
			}
			discordServer, rssServer, reconciler := setUpFeedGroup(ctx, feedGroupName, "discord-webhook-skip-backlog", createRSSFeed(entries...), withRetries("5m", 3), func(fg *rss2discordv1alpha1.FeedGroup) {
				fg.Spec.CatchUpLimit = new(int32(0))
			})

			By("Verifying typed creation preserves an explicit catchUpLimit of 0")
			fg := &rss2discordv1alpha1.FeedGroup{}
			key := types.NamespacedName{Name: feedGroupName, Namespace: namespace}
			Expect(k8sClient.Get(ctx, key, fg)).To(Succeed())
			Expect(fg.Spec.CatchUpLimit).To(HaveValue(Equal(int32(0))))

			By("Reconciling the first sync")
			_, err := reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())
			Expect(discordServer.MessageCount()).To(Equal(0), "no backlog should be sent")

			Expect(k8sClient.Get(ctx, key, fg)).To(Succeed())
			fs := feedStatusFor(fg, rssServer.URL())
			Expect(fs.LastSeenEntry).To(Equal("skip-backlog-10"), "watermark should be the newest entry")
			delivered := apimeta.FindStatusCondition(fs.Conditions, rss2discordv1alpha1.FeedConditionTypeDelivered)
			Expect(delivered).NotTo(BeNil())
			Expect(delivered.Reason).To(Equal("BacklogSkipped"))

			By("Publishing one new entry")
			rssServer.SetFeedContent(createRSSFeed(append(entries, testEntry{
				title:       "Fresh Article",
				description: "Brand new",
				link:        "https://example.com/fresh",
				pubDate:     time.Now().Format(time.RFC1123Z),
				guid:        "skip-backlog-fresh",
			})...))
			_, err = reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying exactly the new entry was sent")
			Expect(discordServer.MessageCount()).To(Equal(1))
			Expect(discordServer.Bodies()[0]).To(ContainSubstring("Fresh Article"))
		})

		It("should persist an empty first sync and deliver entries published later", func() {
			const feedGroupName = "test-feedgroup-empty-first-sync"
			discordServer, rssServer, reconciler := setUpFeedGroup(ctx, feedGroupName, "discord-webhook-empty-first-sync", createRSSFeed(), withRetries("5m", 3), func(fg *rss2discordv1alpha1.FeedGroup) {
				fg.Spec.CatchUpLimit = new(int32(0))
			})

			_, err := reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())
			fg := &rss2discordv1alpha1.FeedGroup{}
			key := types.NamespacedName{Name: feedGroupName, Namespace: namespace}
			Expect(k8sClient.Get(ctx, key, fg)).To(Succeed())
			fs := feedStatusFor(fg, rssServer.URL())
			Expect(fs.Initialized).To(BeTrue(), "initialization must survive a status write and read")
			Expect(fs.LastSeenEntry).To(BeEmpty())
			Expect(discordServer.MessageCount()).To(BeZero())

			rssServer.SetFeedContent(createRSSFeed(testEntry{
				title:       "First Article",
				description: "Published after initialization",
				link:        "https://example.com/first",
				pubDate:     time.Now().Format(time.RFC1123Z),
				guid:        "first-after-empty",
			}))
			_, err = reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())
			Expect(discordServer.MessageCount()).To(Equal(1))
			Expect(discordServer.Bodies()[0]).To(ContainSubstring("First Article"))

			_, err = reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())
			Expect(discordServer.MessageCount()).To(Equal(1), "the entry must not be delivered twice")
		})

		It("should deliver newest entries and advance the watermark for feeds without publish dates", func() {
			const feedGroupName = "test-feedgroup-nodate"

			noDate := func(title, guid string) testEntry {
				// pubDate is left empty on purpose: this feed has no publish
				// dates, which is exactly the case the ordering fallback covers.
				return testEntry{title: title, description: "body", link: "https://example.com/" + guid, pubDate: "", guid: guid}
			}

			By("Creating a mock RSS feed (newest-first, no pubDate) with a 3-entry backlog")
			discordServer, rssServer, reconciler := setUpFeedGroup(ctx, feedGroupName, "discord-webhook-nodate", createRSSFeed(
				noDate("Article C", "c"), // index 0 = newest by feed convention
				noDate("Article B", "b"),
				noDate("Article A", "a"), // index 2 = oldest
			), withRetries("5m", 3), func(fg *rss2discordv1alpha1.FeedGroup) {
				fg.Spec.CatchUpLimit = new(int32(2))
			})

			By("Running the first reconcile")
			_, err := reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying catch-up sent the two NEWEST entries (B and C), not the oldest (A)")
			Expect(discordServer.MessageCount()).To(Equal(2))
			bodies := strings.Join(discordServer.Bodies(), "\n")
			Expect(bodies).To(ContainSubstring("Article B"))
			Expect(bodies).To(ContainSubstring("Article C"))
			Expect(bodies).NotTo(ContainSubstring("Article A"))

			By("Publishing a newer entry at the top of the still-dateless feed")
			rssServer.SetFeedContent(createRSSFeed(
				noDate("Article D", "d"), // brand new, now newest
				noDate("Article C", "c"),
				noDate("Article B", "b"),
				noDate("Article A", "a"),
			))

			By("Running the second reconcile")
			_, err = reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying only the new entry D was delivered (watermark advanced, no silence, no re-send)")
			Expect(discordServer.MessageCount()).To(Equal(3))
			Expect(discordServer.Bodies()[2]).To(ContainSubstring("Article D"))
		})
	})

	Describe("Discord message rendering", func() {
		It("should strip HTML from entry descriptions", func() {
			const feedGroupName = "test-feedgroup-html"

			By("Creating mock Discord and RSS servers, the webhook secret and the FeedGroup")
			discordServer, _, reconciler := setUpFeedGroup(ctx, feedGroupName, "discord-webhook-html", createRSSFeed(
				testEntry{
					title:       "HTML Article",
					description: "&lt;p&gt;First paragraph.&lt;/p&gt;&lt;ul&gt;&lt;li&gt;One&lt;/li&gt;&lt;/ul&gt;",
					link:        "https://example.com/html-article",
					pubDate:     time.Now().Format(time.RFC1123Z),
					guid:        "html-article",
				},
			), withRetries("5m", 3), func(fg *rss2discordv1alpha1.FeedGroup) {
				fg.Spec.Format = defaultDescriptionFormat
			})

			By("Running reconciliation")

			_, err := reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the sent message has no HTML tags")
			Expect(discordServer.Bodies()).To(HaveLen(1))
			Expect(discordServer.Bodies()[0]).NotTo(ContainSubstring("<p>"))
			Expect(discordServer.Bodies()[0]).NotTo(ContainSubstring("<li>"))
			Expect(discordServer.Bodies()[0]).To(ContainSubstring("First paragraph."))
			Expect(discordServer.Bodies()[0]).To(ContainSubstring("One"))
		})

		It("should render an embed with color, thumbnail, and forum thread name when configured", func() {
			const feedGroupName = "test-feedgroup-embed"

			By("Creating mock Discord webhook server")
			discordServer := NewMockDiscordServer(GinkgoTB())
			defer discordServer.Close()

			By("Creating a mock RSS feed server with an enclosure image")
			rssServer := NewMockRSSServer(`<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>Test Feed</title>
    <link>https://example.com</link>
    <description>Test Feed</description>
    <item>
      <title>Embed Article</title>
      <description>Embed body text</description>
      <link>https://example.com/embed-article</link>
      <guid>embed-article</guid>
      <enclosure url="https://example.com/pic.jpg" type="image/jpeg" />
    </item>
  </channel>
</rss>`)
			defer rssServer.Close()

			By("Creating a secret with Discord webhook URL")
			createWebhookSecret(ctx, "discord-webhook-embed", discordServer.URL())

			By("Creating FeedGroup resource with embed and forum thread config")
			feedGroup := newTestFeedGroupFeeds(feedGroupName, "discord-webhook-embed", []rss2discordv1alpha1.FeedSpec{
				{RSSUrl: rssServer.URL(), ForumThreadName: tmplTitle},
			}, withRetries("5m", 3), func(fg *rss2discordv1alpha1.FeedGroup) {
				fg.Spec.Embed = &rss2discordv1alpha1.EmbedSpec{
					Enabled: true,
					Color:   "#00FF00",
				}
			})
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			By("Running reconciliation")
			reconciler := newTestReconciler(discordServer)

			_, err := reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the sent message is an embed with the configured color, thumbnail, and thread name")
			Expect(discordServer.Bodies()).To(HaveLen(1))
			body := discordServer.Bodies()[0]
			Expect(body).To(ContainSubstring(`"title":"Embed Article"`))
			Expect(body).To(ContainSubstring(`"color":65280`))
			Expect(body).To(ContainSubstring(`"thumbnail":{"url":"https://example.com/pic.jpg"}`))
			Expect(body).To(ContainSubstring(`"thread_name":"Embed Article"`))
		})

		It("should drop a non-http(s) entry link/image instead of forwarding it into the embed", func() {
			const feedGroupName = "test-feedgroup-embed-unsafe-url"

			By("Creating mock Discord and RSS servers, the webhook secret and the FeedGroup")
			discordServer, _, reconciler := setUpFeedGroup(ctx, feedGroupName, "discord-webhook-embed-unsafe-url", `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>Test Feed</title>
    <link>https://example.com</link>
    <description>Test Feed</description>
    <item>
      <title>Unsafe URL Article</title>
      <description>Body</description>
      <link>javascript:alert(1)</link>
      <guid>unsafe-url-article</guid>
      <enclosure url="data:image/png;base64,AAAA" type="image/png" />
    </item>
  </channel>
</rss>`, withRetries("5m", 3), func(fg *rss2discordv1alpha1.FeedGroup) {
				fg.Spec.Embed = &rss2discordv1alpha1.EmbedSpec{
					Enabled: true,
				}
			})

			By("Running reconciliation")

			_, err := reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the embed's URL and thumbnail were dropped rather than carrying the unsafe scheme through")
			Expect(discordServer.Bodies()).To(HaveLen(1))
			body := discordServer.Bodies()[0]
			Expect(body).NotTo(ContainSubstring("javascript:"))
			Expect(body).NotTo(ContainSubstring("data:image"))
			Expect(body).NotTo(ContainSubstring(`"thumbnail"`))
		})
	})

	Describe("Error handling", func() {
		It("should handle missing Discord webhook secret", func() {
			By("Creating FeedGroup without secret")
			feedGroup := newTestFeedGroup(feedGroupNameNoSecret, "nonexistent-secret", "https://example.com/feed")
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			By("Running reconciliation")
			reconciler := newTestReconciler(nil)

			result, err := reconcileFeedGroup(ctx, reconciler, feedGroupNameNoSecret)
			// Controller handles missing secret gracefully and requeues
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			By("Verifying FeedGroup still exists and status is updated")
			updated := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupNameNoSecret, Namespace: namespace}, updated)).To(Succeed())
		})

		It("should handle a webhook secret whose value is blank", func() {
			const feedGroupName = "test-feedgroup-blank-secret-value"

			By("Creating a secret whose key holds only whitespace")
			createWebhookSecret(ctx, "discord-webhook-blank-value", "   ")

			feedGroup := newTestFeedGroup(feedGroupName, "discord-webhook-blank-value", exampleFeedURL)
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			By("Running reconciliation")
			reconciler := newTestReconciler(nil)

			result, err := reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			By("Verifying the webhook error was recorded and the Ready condition reflects it")
			updated := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			readyCondition := apimeta.FindStatusCondition(updated.Status.Conditions, rss2discordv1alpha1.ConditionTypeReady)
			Expect(readyCondition).NotTo(BeNil())
			Expect(readyCondition.Status).To(Equal(metav1.ConditionFalse))
			Expect(readyCondition.Reason).To(Equal("WebhookUnresolved"))
			Expect(readyCondition.Message).To(ContainSubstring("empty"))
		})

		It("should handle a webhook secret missing the configured key", func() {
			const feedGroupName = "test-feedgroup-secret-missing-key"

			By("Creating a secret that does not contain the configured key")
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "discord-webhook-missing-key",
					Namespace: namespace,
				},
				Data: map[string][]byte{"wrong-key": []byte("https://discord.com/api/webhooks/12345/abcde")},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			feedGroup := newTestFeedGroup(feedGroupName, "discord-webhook-missing-key", exampleFeedURL)
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			By("Running reconciliation")
			reconciler := newTestReconciler(nil)

			result, err := reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			By("Verifying the missing-key error was recorded on status")
			updated := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			readyCondition := apimeta.FindStatusCondition(updated.Status.Conditions, rss2discordv1alpha1.ConditionTypeReady)
			Expect(readyCondition).NotTo(BeNil())
			Expect(readyCondition.Reason).To(Equal("WebhookUnresolved"))
			Expect(readyCondition.Message).To(ContainSubstring("missing key"))
		})
	})

	Describe("RSS error handling", func() {
		It("should handle RSS fetch errors with retries", func() {
			By("Creating a mock RSS feed server that returns an error")
			rssServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte("Internal Server Error"))
			}))
			defer rssServer.Close()

			By("Creating a secret with Discord webhook URL")
			createWebhookSecret(ctx, "discord-webhook-2", "https://discord.com/api/webhooks/12345/abcde")

			By("Creating FeedGroup resource")
			feedGroup := newTestFeedGroup(feedGroupNameRetry, "discord-webhook-2", rssServer.URL, withRetries("1m", 2))
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			By("Running reconciliation")
			recorder := events.NewFakeRecorder(10)
			reconciler := &FeedGroupReconciler{
				Client:    k8sClient,
				Scheme:    k8sClient.Scheme(),
				RSSClient: testRSSClient(),
				Recorder:  recorder,
			}

			result, err := reconcileFeedGroup(ctx, reconciler, feedGroupNameRetry)
			Expect(err).NotTo(HaveOccurred())
			// Should requeue with retry interval instead of normal interval
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			By("Verifying error is tracked")
			updated := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupNameRetry, Namespace: namespace}, updated)).To(Succeed())
			Expect(feedStatusFor(updated, rssServer.URL).LastError).NotTo(BeEmpty())

			By("Verifying the Ready condition reflects the failure")
			readyCondition := apimeta.FindStatusCondition(updated.Status.Conditions, rss2discordv1alpha1.ConditionTypeReady)
			Expect(readyCondition).NotTo(BeNil())
			Expect(readyCondition.Status).To(Equal(metav1.ConditionFalse))
			Expect(readyCondition.Reason).To(Equal("FeedErrors"))
			Expect(feedStatusFor(updated, rssServer.URL).RetryCount).To(Equal(int32(1)))
			Expect(recorder.Events).To(BeEmpty(), "no event should fire before retries are exhausted")

			By("Reconciling again to exhaust the configured retries")
			_, err = reconcileFeedGroup(ctx, reconciler, feedGroupNameRetry)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying a persistent-failure Event was recorded once retries were exhausted")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupNameRetry, Namespace: namespace}, updated)).To(Succeed())
			Expect(feedStatusFor(updated, rssServer.URL).RetryCount).To(Equal(int32(2)))
			Eventually(recorder.Events).Should(Receive(ContainSubstring("FetchFailed")))

			By("Verifying the FeedReachable condition and classified error reason")
			Expect(feedConditionReason(updated, rssServer.URL, rss2discordv1alpha1.FeedConditionTypeReachable)).To(Equal("ServerError"))
			reachableCondition := apimeta.FindStatusCondition(updated.Status.Conditions, rss2discordv1alpha1.ConditionTypeFeedReachable)
			Expect(reachableCondition).NotTo(BeNil())
			Expect(reachableCondition.Status).To(Equal(metav1.ConditionFalse))
			Expect(reachableCondition.Reason).To(Equal("ServerError"))

			By("Reconciling a third time, with the feed still failing the same way")
			_, err = reconcileFeedGroup(ctx, reconciler, feedGroupNameRetry)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying no second persistent-failure Event fires for the same ongoing failure")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupNameRetry, Namespace: namespace}, updated)).To(Succeed())
			Expect(feedStatusFor(updated, rssServer.URL).RetryCount).To(Equal(int32(3)))
			Consistently(recorder.Events).ShouldNot(Receive())
		})

		It("should fall back to the SSRF-guarded default RSS client when RSSClient is unset", func() {
			// Leaving RSSClient unset on the reconciler exercises the
			// fallback in Reconcile (rssClient := r.RSSClient; if nil {
			// rssClient = rss.NewClient(nil) }), which builds the
			// production SSRF-guarded client. That client refuses to dial a
			// loopback address such as this mock server, so the feed should
			// fail with the guard's error rather than actually fetching.
			const feedGroupName = "test-feedgroup-default-rss-client"

			By("Creating a mock RSS feed server bound to a loopback address")
			rssServer := NewMockRSSServer(createRSSFeed(testEntry{
				title:       "Unreachable",
				description: "Should never be fetched",
				link:        "https://example.com/unreachable",
				pubDate:     time.Now().Format(time.RFC1123Z),
				guid:        "unreachable",
			}))
			defer rssServer.Close()

			By("Creating a secret with Discord webhook URL")
			createWebhookSecret(ctx, "discord-webhook-default-rss-client", "https://discord.com/api/webhooks/12345/abcde")

			By("Creating FeedGroup resource")
			feedGroup := newTestFeedGroup(feedGroupName, "discord-webhook-default-rss-client", rssServer.URL(), withRetries("5m", 3))
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			By("Running reconciliation without setting RSSClient")
			reconciler := &FeedGroupReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the feed failed with the SSRF guard's error, not a real fetch")
			updated := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			Expect(feedStatusFor(updated, rssServer.URL()).LastError).To(ContainSubstring("refusing to connect to non-public address"))
		})

		It("should retry then give up on a deterministically failing render, instead of retrying forever", func() {
			const feedGroupName = "test-feedgroup-render-error"

			By("Creating a mock RSS feed server with one entry")
			rssServer := NewMockRSSServer(`<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>Test Feed</title>
    <link>https://example.com</link>
    <description>Test Feed</description>
    <item>
      <title>Render Error Article</title>
      <description>Body</description>
      <link>https://example.com/render-error-article</link>
      <guid>render-error-article</guid>
    </item>
  </channel>
</rss>`)
			defer rssServer.Close()

			By("Creating a secret with Discord webhook URL")
			createWebhookSecret(ctx, "discord-webhook-render-error", "https://discord.com/api/webhooks/12345/abcde")

			By("Creating FeedGroup with a template field that fails at execution time, not parse time")
			feedGroup := &rss2discordv1alpha1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      feedGroupName,
					Namespace: namespace,
				},
				Spec: rss2discordv1alpha1.FeedGroupSpec{
					DiscordWebhookSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "discord-webhook-render-error"},
						Key:                  secretURLKey,
					},
					Interval:      defaultInterval,
					RetryInterval: "1m",
					Retries:       2,
					// {{.Bogus}} parses fine (the parser doesn't know field
					// names) but fails on every Execute, since Title/
					// Description/Link/Published are the only fields the
					// render data struct has.
					Format: "{{.Bogus}}",
					Feeds: []rss2discordv1alpha1.FeedSpec{
						{RSSUrl: rssServer.URL()},
					},
				},
			}
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			By("Running reconciliation")
			recorder := events.NewFakeRecorder(10)
			reconciler := &FeedGroupReconciler{
				Client:    k8sClient,
				Scheme:    k8sClient.Scheme(),
				RSSClient: testRSSClient(),
				Recorder:  recorder,
			}

			_, err := reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the render error is tracked and counted toward retries")
			updated := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			Expect(feedStatusFor(updated, rssServer.URL()).LastError).NotTo(BeEmpty())
			Expect(feedStatusFor(updated, rssServer.URL()).RetryCount).To(Equal(int32(1)))
			Expect(recorder.Events).To(BeEmpty(), "no event should fire before retries are exhausted")

			By("Reconciling again to exhaust the configured retries")
			_, err = reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying a persistent-failure Event was recorded once retries were exhausted, instead of retrying forever")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			Expect(feedStatusFor(updated, rssServer.URL()).RetryCount).To(Equal(int32(2)))
			Eventually(recorder.Events).Should(Receive(ContainSubstring("RenderFailed")))

			By("Reconciling a third time, with the entry still failing to render the same way")
			_, err = reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying no second persistent-failure Event fires, and the entry is held (not skipped) since the template is broken for every entry")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			Expect(feedStatusFor(updated, rssServer.URL()).RetryCount).To(Equal(int32(3)))
			Expect(feedStatusFor(updated, rssServer.URL()).LastSeenEntry).To(BeEmpty())
			Consistently(recorder.Events).ShouldNot(Receive())
		})

		It("should retry then give up on a persistently failing send, instead of retrying forever", func() {
			const feedGroupName = "test-feedgroup-send-error"

			By("Creating a mock Discord server that always fails the send with a non-rate-limit error")
			discordServer := NewMockDiscordServer(GinkgoTB())
			defer discordServer.Close()
			discordServer.FailNextRequests(3, http.StatusInternalServerError)

			By("Creating a mock RSS feed server with one entry")
			rssServer := NewMockRSSServer(createRSSFeed(
				testEntry{
					title:       "Send Error Article",
					description: "Should never be delivered",
					link:        "https://example.com/send-error-article",
					pubDate:     time.Now().Format(time.RFC1123Z),
					guid:        "send-error-article",
				},
			))
			defer rssServer.Close()

			By("Creating a secret with Discord webhook URL")
			createWebhookSecret(ctx, "discord-webhook-send-error", discordServer.URL())

			By("Creating FeedGroup resource")
			feedGroup := newTestFeedGroup(feedGroupName, "discord-webhook-send-error", rssServer.URL(), withRetries("1m", 2))
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			By("Running reconciliation")
			recorder := events.NewFakeRecorder(10)
			reconciler := &FeedGroupReconciler{
				Client:               k8sClient,
				Scheme:               k8sClient.Scheme(),
				RSSClient:            testRSSClient(),
				DiscordClientBuilder: discordServer.DiscordClientBuilder(),
				Recorder:             recorder,
			}

			_, err := reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the send error is tracked and counted toward retries")
			updated := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			Expect(feedStatusFor(updated, rssServer.URL()).LastError).NotTo(BeEmpty())
			Expect(feedStatusFor(updated, rssServer.URL()).RetryCount).To(Equal(int32(1)))
			Expect(recorder.Events).To(BeEmpty(), "no event should fire before retries are exhausted")

			By("Reconciling again to exhaust the configured retries")
			_, err = reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying a persistent-failure Event was recorded once retries were exhausted, instead of retrying forever")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			Expect(feedStatusFor(updated, rssServer.URL()).RetryCount).To(Equal(int32(2)))
			Expect(discordServer.MessageCount()).To(Equal(0))
			Eventually(recorder.Events).Should(Receive(ContainSubstring("SendFailed")))

			By("Reconciling a third time, with the send still failing the same way")
			_, err = reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying no second persistent-failure Event fires for the same ongoing failure")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			Expect(feedStatusFor(updated, rssServer.URL()).RetryCount).To(Equal(int32(3)))
			Consistently(recorder.Events).ShouldNot(Receive())
		})

		It("should persist what a reconcile sent before a feed panicked, and not send it again", func() {
			const feedGroupName = "test-feedgroup-panic"

			By("Creating a FeedGroup whose feed has two new entries")
			discordServer, rssServer, reconciler := setUpFeedGroup(ctx, feedGroupName, "discord-webhook-panic", createRSSFeed(
				testEntry{
					title:       "Panic Older Article",
					description: "Sent before the panic",
					link:        "https://example.com/panic-older-article",
					pubDate:     time.Now().Add(-2 * time.Hour).Format(time.RFC1123Z),
					guid:        "panic-older-article",
				},
				testEntry{
					title:       "Panic Newer Article",
					description: "Its send panics the first time",
					link:        "https://example.com/panic-newer-article",
					pubDate:     time.Now().Add(-time.Hour).Format(time.RFC1123Z),
					guid:        "panic-newer-article",
				},
			))
			realBuilder := reconciler.DiscordClientBuilder
			reconciler.DiscordClientBuilder = func(webhookURL string) MessageSender {
				return &panicAfterSender{inner: realBuilder(webhookURL), after: 1}
			}

			By("Reconciling with a sender that panics on the second send")
			_, err := reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).To(MatchError(ContainSubstring("panic while processing feed")))
			Expect(discordServer.MessageCount()).To(Equal(1))

			By("Verifying the sent entry was persisted despite the panic")
			updated := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			fs := feedStatusFor(updated, rssServer.URL())
			Expect(fs.LastSeenEntry).To(Equal("panic-older-article"))
			Expect(fs.LastSent).To(HaveLen(1))
			Expect(fs.ETag).To(BeEmpty())
			Expect(fs.LastError).To(ContainSubstring(errDeliveryPanicked.Error()))
			By("Verifying the incomplete pass did not advance ObservedGeneration")
			Expect(updated.Status.ObservedGeneration).To(BeZero())
			By("Verifying the group is not reported Ready while a feed panics")
			ready := apimeta.FindStatusCondition(updated.Status.Conditions, rss2discordv1alpha1.ConditionTypeReady)
			Expect(ready).NotTo(BeNil())
			Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			Expect(ready.Reason).To(Equal(reasonReconcilePanicked))
			Expect(ready.Message).To(ContainSubstring("send exploded"))
			Expect(ready.Message).NotTo(ContainSubstring("goroutine"))

			By("Reconciling again at once, with a working sender")
			reconciler.DiscordClientBuilder = realBuilder
			_, err = reconcileFeedGroupNow(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying only the unsent entry was delivered, not the one sent before the panic")
			Expect(discordServer.MessageCount()).To(Equal(2))
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			Expect(updated.Status.ObservedGeneration).To(Equal(updated.Generation))
			Expect(feedStatusFor(updated, rssServer.URL()).LastError).To(BeEmpty())
			Expect(apimeta.IsStatusConditionTrue(updated.Status.Conditions, rss2discordv1alpha1.ConditionTypeReady)).To(BeTrue())
		})

		It("should persist status and send once when the interval cannot be parsed", func() {
			const feedGroupName = "test-feedgroup-invalid-interval"

			By("Creating a FeedGroup with an unparsable interval and a feed with one new entry")
			discordServer, rssServer, reconciler := setUpFeedGroup(ctx, feedGroupName, "discord-webhook-invalid-interval", createRSSFeed(
				testEntry{
					title:       "Invalid Interval Article",
					description: "Must be sent exactly once",
					link:        "https://example.com/invalid-interval-article",
					pubDate:     time.Now().Format(time.RFC1123Z),
					guid:        "invalid-interval-article",
				},
			))
			recorder := events.NewFakeRecorder(10)
			reconciler.Recorder = recorder
			// Admission now rejects an unparsable interval (CEL duration()),
			// so model an object stored under an older schema by rewriting
			// the interval as the reconciler reads it: this is the runtime
			// backstop the schema check can't replace.
			watchClient, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
			Expect(err).NotTo(HaveOccurred())
			reconciler.Client = interceptor.NewClient(watchClient, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if err := c.Get(ctx, key, obj, opts...); err != nil {
						return err
					}
					if fg, ok := obj.(*rss2discordv1alpha1.FeedGroup); ok {
						fg.Spec.Interval = overflowingInterval
					}
					return nil
				},
			})

			By("Reconciling: the entry is sent and the reconcile reports no error (an error would hot-loop on backoff)")
			result, err := reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">=", 30*time.Minute),
				"the 30m fallback (plus jitter) should be used in place of the unparsable interval")
			Expect(discordServer.MessageCount()).To(Equal(1))

			By("Verifying the delivery state was persisted despite the bad interval")
			updated := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			fs := feedStatusFor(updated, rssServer.URL())
			Expect(fs.LastSent).NotTo(BeEmpty())
			Expect(fs.LastSeenEntry).NotTo(BeEmpty())
			Expect(fs.LastChecked).NotTo(BeEmpty())

			By("Verifying Ready=False/InvalidInterval names the offending value, with one Warning Event")
			ready := apimeta.FindStatusCondition(updated.Status.Conditions, rss2discordv1alpha1.ConditionTypeReady)
			Expect(ready).NotTo(BeNil())
			Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			Expect(ready.Reason).To(Equal("InvalidInterval"))
			Expect(ready.Message).To(ContainSubstring("interval"))
			Expect(ready.Message).To(ContainSubstring(overflowingInterval))
			Eventually(recorder.Events).Should(Receive(ContainSubstring("InvalidInterval")))

			By("Reconciling again, with the feed due: nothing is re-sent and no second Event fires")
			_, err = reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())
			Expect(discordServer.MessageCount()).To(Equal(1))
			Consistently(recorder.Events).ShouldNot(Receive())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			ready = apimeta.FindStatusCondition(updated.Status.Conditions, rss2discordv1alpha1.ConditionTypeReady)
			Expect(ready).NotTo(BeNil())
			Expect(ready.Reason).To(Equal("InvalidInterval"))
		})

		It("should never let a later entry's success skip an earlier entry that failed to send", func() {
			const feedGroupName = "test-feedgroup-send-error-no-skip"

			By("Creating a mock Discord server that fails only the very first delivery")
			discordServer, rssServer, reconciler := setUpFeedGroup(ctx, feedGroupName, "discord-webhook-send-error-no-skip", createRSSFeed(
				testEntry{
					title:       "Older Article",
					description: "Should not be skipped after failing to send",
					link:        "https://example.com/older-article",
					pubDate:     time.Now().Add(-1 * time.Hour).Format(time.RFC1123Z),
					guid:        "older-article",
				},
				testEntry{
					title:       "Newer Article",
					description: "Should not be sent before the older article",
					link:        "https://example.com/newer-article",
					pubDate:     time.Now().Format(time.RFC1123Z),
					guid:        "newer-article",
				},
			), withRetries("1m", 3))
			discordServer.FailNextRequests(1, http.StatusInternalServerError)

			By("Creating a mock RSS feed with an older entry and a newer entry")

			By("Reconciling once: the older entry fails to send")
			_, err := reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the newer entry was NOT sent ahead of the still-unsent older one")
			Expect(discordServer.MessageCount()).To(Equal(0))

			updated := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			Expect(feedStatusFor(updated, rssServer.URL()).LastSeenEntry).To(BeEmpty(),
				"LastSeenEntry must not advance past an entry that failed to send")

			By("Reconciling again, now that delivery succeeds")
			_, err = reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying both entries were eventually delivered, oldest first")
			Expect(discordServer.MessageCount()).To(Equal(2))
			Expect(discordServer.Bodies()[0]).To(ContainSubstring("Older Article"))
			Expect(discordServer.Bodies()[1]).To(ContainSubstring("Newer Article"))
		})

		It("should not persist conditional-GET validators while an entry is still unsent, even after retries are exhausted", func() {
			const feedGroupName = "test-feedgroup-send-error-etag"

			By("Creating a mock Discord server that fails the first two deliveries, then succeeds")
			discordServer, rssServer, reconciler := setUpFeedGroup(ctx, feedGroupName, "discord-webhook-send-error-etag", createRSSFeed(
				testEntry{
					title:       "ETag Article",
					description: "Should not be stranded by a premature 304",
					link:        "https://example.com/etag-article",
					pubDate:     time.Now().Format(time.RFC1123Z),
					guid:        "etag-article",
				},
			), withRetries("1m", 2))
			discordServer.FailNextRequests(2, http.StatusInternalServerError)

			By("Creating a mock RSS feed server that returns an ETag")
			rssServer.SetETag(`"v1"`)

			By("Reconciling twice, exhausting the configured retries on a still-unsent entry")
			for range 2 {
				_, err := reconcileFeedGroup(ctx, reconciler, feedGroupName)
				Expect(err).NotTo(HaveOccurred())
			}

			By("Verifying retries are exhausted but the entry was never delivered")
			updated := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			Expect(feedStatusFor(updated, rssServer.URL()).RetryCount).To(Equal(int32(2)))
			Expect(discordServer.MessageCount()).To(Equal(0))

			By("Verifying the ETag was NOT persisted while the entry is still unsent")
			Expect(feedStatusFor(updated, rssServer.URL()).ETag).To(BeEmpty(),
				"persisting the ETag here would make the next fetch return 304 and strand the unsent entry forever")
			Expect(rssServer.IfNoneMatchSeen()).To(Equal([]string{"", ""}),
				"every fetch so far must have been unconditional, since the ETag was never persisted")

			By("Reconciling a third time, now that delivery succeeds")
			_, err := reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the entry was finally delivered and the ETag is now persisted")
			Expect(discordServer.MessageCount()).To(Equal(1))
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			Expect(feedStatusFor(updated, rssServer.URL()).ETag).To(Equal(`"v1"`))
		})

		It("should back off immediately on a permanently-classified send failure, like a permanent fetch failure", func() {
			const feedGroupName = "test-feedgroup-send-error-permanent-backoff"

			By("Creating a mock Discord server that always 404s, as if the webhook were deleted")
			discordServer := NewMockDiscordServer(GinkgoTB())
			defer discordServer.Close()
			discordServer.FailNextRequests(100, http.StatusNotFound)

			By("Creating a mock RSS feed server with one entry")
			rssServer := NewMockRSSServer(createRSSFeed(
				testEntry{
					title:       "Webhook Deleted Article",
					description: "Should never be delivered",
					link:        "https://example.com/webhook-deleted-article",
					pubDate:     time.Now().Format(time.RFC1123Z),
					guid:        "webhook-deleted-article",
				},
			))
			defer rssServer.Close()

			By("Creating a secret with Discord webhook URL")
			createWebhookSecret(ctx, "discord-webhook-send-error-permanent", discordServer.URL())

			By("Creating FeedGroup resource with Retries=3, so the first failure leaves retries to spare")
			feedGroup := newTestFeedGroup(feedGroupName, "discord-webhook-send-error-permanent", rssServer.URL(), withRetries("5m", 3))
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			recorder := events.NewFakeRecorder(10)
			reconciler := &FeedGroupReconciler{
				Client:               k8sClient,
				Scheme:               k8sClient.Scheme(),
				RSSClient:            testRSSClient(),
				DiscordClientBuilder: discordServer.DiscordClientBuilder(),
				Recorder:             recorder,
			}

			By("Reconciling once")
			result, err := reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">=", 30*time.Minute),
				"a permanent send failure must not pull the group onto the 5m RetryInterval")

			By("Verifying the feed entered backoff on the first failure rather than spending its retries")
			updated := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			fs := feedStatusFor(updated, rssServer.URL())
			Expect(fs.BackoffUntil).NotTo(BeEmpty(), "a permanently-classified send failure should set BackoffUntil")
			Expect(fs.RetryCount).To(Equal(int32(1)))
			Expect(discordServer.MessageCount()).To(Equal(0))

			By("Verifying no persistent-failure Event yet: it comes from the backoff reaching its cap, not from retries")
			Consistently(recorder.Events, 100*time.Millisecond).ShouldNot(Receive(ContainSubstring("SendFailed")))
		})
	})

	Describe("Filtering", func() {
		It("should apply regex filters to RSS entries", func() {
			By("Creating mock Discord webhook server")
			discordServer := NewMockDiscordServer(GinkgoTB())
			defer discordServer.Close()

			By("Creating a mock RSS feed server with multiple entries")
			rssServer := NewMockRSSServer(createRSSFeed(
				testEntry{
					title:       "Kubernetes Release v1.30",
					description: "New Kubernetes version released",
					link:        "https://example.com/k8s",
					pubDate:     time.Now().Add(-1 * time.Hour).Format(time.RFC1123Z),
					guid:        "k8s-1",
				},
				testEntry{
					title:       "Python Conference Schedule",
					description: "Python conference talks announced",
					link:        "https://example.com/python",
					pubDate:     time.Now().Format(time.RFC1123Z),
					guid:        "python-1",
				},
			))
			defer rssServer.Close()

			By("Creating a secret with Discord webhook URL")
			createWebhookSecret(ctx, "discord-webhook-3", discordServer.URL())

			By("Creating FeedGroup with regex filter")
			feedGroup := newTestFeedGroupFeeds(feedGroupNameFilter, "discord-webhook-3", []rss2discordv1alpha1.FeedSpec{
				{
					RSSUrl: rssServer.URL(),
					Filter: &rss2discordv1alpha1.Filter{
						Regex: "Kubernetes|K8s",
					},
				},
			})
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			By("Running reconciliation")
			reconciler := newTestReconciler(discordServer)

			_, err := reconcileFeedGroup(ctx, reconciler, feedGroupNameFilter)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying status was updated")
			updated := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupNameFilter, Namespace: namespace}, updated)).To(Succeed())
			Expect(feedStatusFor(updated, rssServer.URL()).LastSeenEntry).NotTo(BeEmpty())
		})

		It("should apply keyword filters to RSS entries", func() {
			By("Creating mock Discord webhook server")
			discordServer := NewMockDiscordServer(GinkgoTB())
			defer discordServer.Close()

			By("Creating a mock RSS feed server with multiple entries")
			rssServer := NewMockRSSServer(createRSSFeed(
				testEntry{
					title:       "Python Tutorial",
					description: "Learn Python basics",
					link:        "https://example.com/python",
					pubDate:     time.Now().Format(time.RFC1123Z),
					guid:        "python-1",
				},
				testEntry{
					title:       "JavaScript Best Practices",
					description: "Advanced JS patterns",
					link:        "https://example.com/js",
					pubDate:     time.Now().Add(time.Hour).Format(time.RFC1123Z),
					guid:        "js-1",
				},
			))
			defer rssServer.Close()

			By("Creating a secret with Discord webhook URL")
			createWebhookSecret(ctx, "discord-webhook-4", discordServer.URL())

			By("Creating FeedGroup with keyword filter")
			feedGroup := newTestFeedGroupFeeds(feedGroupNameKeywords, "discord-webhook-4", []rss2discordv1alpha1.FeedSpec{
				{
					RSSUrl: rssServer.URL(),
					Filter: &rss2discordv1alpha1.Filter{
						Keywords: []string{"Python", "Django"},
					},
				},
			})
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			By("Running reconciliation")
			reconciler := newTestReconciler(discordServer)

			_, err := reconcileFeedGroup(ctx, reconciler, feedGroupNameKeywords)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying status was updated")
			updated := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupNameKeywords, Namespace: namespace}, updated)).To(Succeed())
			Expect(feedStatusFor(updated, rssServer.URL()).LastSeenEntry).NotTo(BeEmpty())
		})
	})

	Describe("Paused feeds and status tracking", func() {
		It("should skip paused feeds", func() {
			By("Creating a mock RSS feed server")
			rssServer := NewMockRSSServer(createRSSFeed(
				testEntry{
					title:       "Test",
					description: "Test",
					link:        "https://example.com/test",
					pubDate:     time.Now().Format(time.RFC1123Z),
					guid:        "test-1",
				},
			))
			defer rssServer.Close()

			By("Creating a secret with Discord webhook URL")
			createWebhookSecret(ctx, "discord-webhook-5", "https://discord.com/api/webhooks/12345/abcde")

			By("Creating FeedGroup with paused feed")
			feedGroup := newTestFeedGroupFeeds(feedGroupNamePaused, "discord-webhook-5", []rss2discordv1alpha1.FeedSpec{
				{
					RSSUrl: rssServer.URL(),
					Paused: true,
				},
			})
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			By("Running reconciliation")
			reconciler := newTestReconciler(nil)

			_, err := reconcileFeedGroup(ctx, reconciler, feedGroupNamePaused)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying paused feed was not processed")
			updated := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupNamePaused, Namespace: namespace}, updated)).To(Succeed())

			// LastSeenEntry should not be updated for paused feed
			Expect(feedStatusFor(updated, rssServer.URL()).LastSeenEntry).To(BeEmpty())
		})

		It("should track lastChecked timestamp for each feed", func() {
			By("Creating a mock RSS feed server with empty feed")
			rssServer := NewMockRSSServer(`<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>Empty Feed</title>
    <link>https://example.com</link>
    <description>Empty Feed</description>
  </channel>
</rss>`)
			defer rssServer.Close()

			By("Creating a secret with Discord webhook URL")
			createWebhookSecret(ctx, "discord-webhook-6", "https://discord.com/api/webhooks/12345/abcde")

			By("Creating FeedGroup resource")
			feedGroup := newTestFeedGroup(feedGroupNameTimestamp, "discord-webhook-6", rssServer.URL())
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			By("Running reconciliation")
			reconciler := newTestReconciler(nil)

			_, err := reconcileFeedGroup(ctx, reconciler, feedGroupNameTimestamp)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying lastChecked was updated")
			updated := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupNameTimestamp, Namespace: namespace}, updated)).To(Succeed())

			Expect(feedStatusFor(updated, rssServer.URL()).LastChecked).NotTo(BeEmpty())
			// Should be a valid RFC3339 timestamp
			_, err = time.Parse(time.RFC3339, feedStatusFor(updated, rssServer.URL()).LastChecked)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should skip a feed whose permanent-failure backoff has not yet elapsed", func() {
			const feedGroupName = "test-feedgroup-backoff-skip"

			By("Creating a mock RSS feed server")
			rssServer := NewMockRSSServer(createRSSFeed(
				testEntry{
					title:       "Backoff Skip Article",
					description: "Backoff Skip Article",
					link:        "https://example.com/test",
					pubDate:     time.Now().Format(time.RFC1123Z),
					guid:        "test-1",
				},
			))
			defer rssServer.Close()

			By("Creating a secret with Discord webhook URL")
			createWebhookSecret(ctx, "discord-webhook-backoff-skip", "https://discord.com/api/webhooks/12345/abcde")

			By("Creating FeedGroup resource")
			feedGroup := newTestFeedGroup(feedGroupName, "discord-webhook-backoff-skip", rssServer.URL())
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			reconciler := newTestReconciler(nil)

			// The first reconcile after creation has Generation > ObservedGeneration,
			// which clears any BackoffUntil set below it. Reconcile once first so
			// ObservedGeneration catches up, then set BackoffUntil and reconcile
			// again at the same generation, where the skip in Reconcile (the loop
			// building activeFeeds) actually applies.
			By("Running an initial reconciliation to advance ObservedGeneration")
			_, err := reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())
			Expect(rssServer.RequestCount()).To(Equal(1))

			By("Setting a future BackoffUntil on the feed's status")
			afterFirstReconcile := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, afterFirstReconcile)).To(Succeed())
			feedStatusFor(afterFirstReconcile, rssServer.URL()).BackoffUntil = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
			// Make the feed otherwise due, so only the backoff can be what skips it.
			feedStatusFor(afterFirstReconcile, rssServer.URL()).LastChecked = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
			Expect(k8sClient.Status().Update(ctx, afterFirstReconcile)).To(Succeed())

			By("Running reconciliation")
			_, err = reconcileFeedGroupNow(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the backed-off feed was not fetched again")
			Expect(rssServer.RequestCount()).To(Equal(1))

			updated := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			Expect(feedStatusFor(updated, rssServer.URL()).LastChecked).To(Equal(feedStatusFor(afterFirstReconcile, rssServer.URL()).LastChecked))
		})
	})

	Describe("Status pruning for removed feeds", func() {
		It("should drop per-feed status entries once a feed is removed from spec", func() {
			const feedGroupName = "test-feedgroup-removed-feed"

			By("Creating mock Discord webhook server")
			discordServer := NewMockDiscordServer(GinkgoTB())
			defer discordServer.Close()

			By("Creating a mock RSS feed server")
			rssServer := NewMockRSSServer(createRSSFeed(
				testEntry{
					title:       "Soon Removed",
					description: "This feed will be removed from spec",
					link:        "https://example.com/removed",
					pubDate:     time.Now().Format(time.RFC1123Z),
					guid:        "removed-1",
				},
			))
			rssServer.SetETag(`"v1"`)
			defer rssServer.Close()

			By("Creating a secret with Discord webhook URL")
			createWebhookSecret(ctx, "discord-webhook-removed", discordServer.URL())

			const keptFeedURL = "https://example.com/kept-feed.xml"

			By("Creating FeedGroup resource with two feeds")
			feedGroup := newTestFeedGroupFeeds(feedGroupName, "discord-webhook-removed", []rss2discordv1alpha1.FeedSpec{
				{RSSUrl: rssServer.URL()},
				{RSSUrl: keptFeedURL, Paused: true},
			}, withRetries("5m", 3))
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			reconciler := newTestReconciler(discordServer)

			By("Running the first reconciliation so the removed feed accrues status")
			_, err := reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())

			afterFirst := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, afterFirst)).To(Succeed())
			Expect(feedStatusFor(afterFirst, rssServer.URL()).LastChecked).NotTo(BeEmpty())
			Expect(feedStatusFor(afterFirst, rssServer.URL()).ETag).NotTo(BeEmpty())

			By("Removing the feed from spec, keeping the paused one")
			afterFirst.Spec.Feeds = []rss2discordv1alpha1.FeedSpec{
				{RSSUrl: keptFeedURL, Paused: true},
			}
			Expect(k8sClient.Update(ctx, afterFirst)).To(Succeed())

			By("Running another reconciliation")
			_, err = reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the removed feed's status entries were pruned")
			afterSecond := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, afterSecond)).To(Succeed())
			Expect(feedStatusFor(afterSecond, rssServer.URL())).To(BeNil())
		})
	})

	Describe("Server-side apply status writes", func() {
		It("should not conflict when two stale-resourceVersion status writes race", func() {
			// applyStatus uses server-side apply rather than Status().Update,
			// specifically so that a second write computed from a
			// now-stale-resourceVersion read (e.g. two concurrent reconciles of
			// FeedGroups sharing... no, of the *same* FeedGroup, which
			// concurrency > 1 combined with a requeue racing a fresh event could
			// produce) doesn't fail with an optimistic-concurrency conflict the
			// way Status().Update would.
			const feedGroupName = "test-feedgroup-ssa-concurrent"

			feedGroup := newTestFeedGroup(feedGroupName, "discord-webhook-ssa-concurrent", "https://example.com/ssa-concurrent.xml")
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			reconciler := &FeedGroupReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

			By("Reading two independent copies of the FeedGroup at the same resourceVersion")
			fgA := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, fgA)).To(Succeed())
			fgB := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, fgB)).To(Succeed())
			Expect(fgA.ResourceVersion).To(Equal(fgB.ResourceVersion), "both copies must start at the same resourceVersion for this to test anything")

			By("Applying status computed from the first copy")
			ensureFeedStatuses(fgA)
			fgA.Status.ObservedGeneration = fgA.Generation
			setReadyCondition(fgA, nil, nil)
			setFeedReachableCondition(fgA)
			Expect(reconciler.applyStatus(ctx, fgA)).To(Succeed())

			By("Verifying the first apply actually changed the object's resourceVersion server-side")
			updated := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			Expect(updated.ResourceVersion).NotTo(Equal(fgB.ResourceVersion))

			By("Verifying the write was recorded as an Apply on the status subresource by this controller")
			Expect(updated.ManagedFields).To(ContainElement(SatisfyAll(
				HaveField("Manager", statusFieldOwner),
				HaveField("Operation", metav1.ManagedFieldsOperationApply),
				HaveField("Subresource", "status"),
			)))
			Expect(updated.Status.Feeds).To(ConsistOf(HaveField("RSSUrl", "https://example.com/ssa-concurrent.xml")))

			By("Applying status computed from the second, now-stale copy -- must not fail with a conflict")
			ensureFeedStatuses(fgB)
			fgB.Status.ObservedGeneration = fgB.Generation
			setReadyCondition(fgB, nil, nil)
			setFeedReachableCondition(fgB)
			Expect(reconciler.applyStatus(ctx, fgB)).To(Succeed())
		})
	})

	Describe("Requeue behavior", func() {
		It("should use normal interval when no retries needed", func() {
			By("Creating a mock RSS feed server with empty feed")
			rssServer := NewMockRSSServer(`<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>Empty Feed</title>
    <link>https://example.com</link>
    <description>Empty Feed</description>
  </channel>
</rss>`)
			defer rssServer.Close()

			By("Creating a secret with Discord webhook URL")
			createWebhookSecret(ctx, "discord-webhook-7", "https://discord.com/api/webhooks/12345/abcde")

			By("Creating FeedGroup with custom interval")
			feedGroup := newTestFeedGroup("test-feedgroup-interval", "discord-webhook-7", rssServer.URL(), func(fg *rss2discordv1alpha1.FeedGroup) {
				fg.Spec.Interval = "1h"
				fg.Spec.RetryInterval = "5m"
			})
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			By("Running reconciliation")
			reconciler := newTestReconciler(nil)

			result, err := reconcileFeedGroup(ctx, reconciler, "test-feedgroup-interval")
			Expect(err).NotTo(HaveOccurred())

			// Should use normal interval (1 hour), plus up to 10% jitter so
			// FeedGroups sharing an interval don't requeue in lockstep.
			expectedInterval := time.Hour
			Expect(result.RequeueAfter).To(BeNumerically(">=", expectedInterval))
			Expect(result.RequeueAfter).To(BeNumerically("<=", time.Duration(1.1*float64(expectedInterval))))
		})
	})

	Describe("Spec validation", func() {
		It("should reject an Interval that isn't a valid Go duration at apply time", func() {
			createWebhookSecret(ctx, "discord-webhook-bad-interval", "https://discord.com/api/webhooks/12345/abcde")

			feedGroup := newTestFeedGroup("test-feedgroup-bad-interval", "discord-webhook-bad-interval", exampleFeedURL, func(fg *rss2discordv1alpha1.FeedGroup) {
				fg.Spec.Interval = "30 minutes"
			})
			Expect(k8sClient.Create(ctx, feedGroup)).To(MatchError(ContainSubstring("spec.interval")))
		})

		It("should reject a RetryInterval that isn't a valid Go duration at apply time", func() {
			createWebhookSecret(ctx, "discord-webhook-bad-retry-interval", "https://discord.com/api/webhooks/12345/abcde")

			feedGroup := &rss2discordv1alpha1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-feedgroup-bad-retry-interval",
					Namespace: namespace,
				},
				Spec: rss2discordv1alpha1.FeedGroupSpec{
					DiscordWebhookSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "discord-webhook-bad-retry-interval"},
						Key:                  secretURLKey,
					},
					RetryInterval: "five minutes",
					Feeds: []rss2discordv1alpha1.FeedSpec{
						{RSSUrl: exampleFeedURL},
					},
				},
			}
			Expect(k8sClient.Create(ctx, feedGroup)).To(MatchError(ContainSubstring("spec.retryInterval")))
		})

		It("should reject more than 50 Feeds at apply time", func() {
			createWebhookSecret(ctx, "discord-webhook-too-many-feeds", "https://discord.com/api/webhooks/12345/abcde")

			feeds := make([]rss2discordv1alpha1.FeedSpec, 51)
			for i := range feeds {
				feeds[i] = rss2discordv1alpha1.FeedSpec{RSSUrl: fmt.Sprintf("https://example.com/feed-%d.xml", i)}
			}

			feedGroup := &rss2discordv1alpha1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-feedgroup-too-many-feeds",
					Namespace: namespace,
				},
				Spec: rss2discordv1alpha1.FeedGroupSpec{
					DiscordWebhookSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "discord-webhook-too-many-feeds"},
						Key:                  secretURLKey,
					},
					Feeds: feeds,
				},
			}
			Expect(k8sClient.Create(ctx, feedGroup)).To(MatchError(ContainSubstring("spec.feeds")))
		})
	})

	Describe("Discord rate limiting", func() {
		It("should requeue using Discord's Retry-After duration without persisting the unsent entry's validators", func() {
			const feedGroupName = "test-feedgroup-rate-limited"

			By("Creating a mock Discord webhook server that rate-limits the first delivery")
			discordServer, rssServer, reconciler := setUpFeedGroup(ctx, feedGroupName, "discord-webhook-rate-limited", createRSSFeed(
				testEntry{
					title:       "Rate Limited Article",
					description: "Should survive a 429",
					link:        "https://example.com/rate-limited-article",
					pubDate:     time.Now().Format(time.RFC1123Z),
					guid:        "rate-limited-article",
				},
			), func(fg *rss2discordv1alpha1.FeedGroup) {
				fg.Spec.RetryInterval = "5m"
			})
			discordServer.FailNextRequestsRateLimited(1, "2")

			By("Creating a mock RSS feed server that returns an ETag")
			rssServer.SetETag(`"v1"`)

			By("Running reconciliation, where Discord rate-limits the send")
			result, err := reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())
			Expect(discordServer.MessageCount()).To(Equal(0))

			By("Verifying the reconcile requeues after Discord's Retry-After duration, not the normal interval")
			Expect(result.RequeueAfter).To(Equal(2 * time.Second))

			By("Verifying the new ETag was NOT persisted, since the entry was never sent")
			updated := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			Expect(feedStatusFor(updated, rssServer.URL()).ETag).To(BeEmpty())
			Expect(feedStatusFor(updated, rssServer.URL()).LastSeenEntry).To(BeEmpty())
			Expect(feedStatusFor(updated, rssServer.URL()).LastError).To(ContainSubstring("rate limited"))

			By("Reconciling again, where the send succeeds and the previously unsent entry is delivered")
			result, err = reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())
			Expect(discordServer.MessageCount()).To(Equal(1))
			// Normal interval, plus up to 10% jitter.
			Expect(result.RequeueAfter).To(BeNumerically(">=", 30*time.Minute))
			Expect(result.RequeueAfter).To(BeNumerically("<=", 33*time.Minute))

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			Expect(feedStatusFor(updated, rssServer.URL()).ETag).To(Equal(`"v1"`))
			Expect(feedsWithError(updated)).To(Equal(0))
		})

		It("should stop sending the rest of a feed's entries after the first 429", func() {
			const feedGroupName = "test-feedgroup-rate-limited-stop"

			By("Creating a mock RSS feed with several catch-up entries")
			entries := make([]testEntry, 0, 5)
			for i := range 5 {
				entries = append(entries, testEntry{
					title:       fmt.Sprintf("Article %d", i),
					description: "body",
					link:        fmt.Sprintf("https://example.com/article%d", i),
					pubDate:     time.Now().Add(-time.Duration(5-i) * time.Hour).Format(time.RFC1123Z),
					guid:        fmt.Sprintf("guid-%d", i),
				})
			}

			By("Creating a mock Discord server that rate-limits many deliveries")
			discordServer, _, reconciler := setUpFeedGroup(ctx, feedGroupName, "discord-webhook-rl-stop", createRSSFeed(entries...), func(fg *rss2discordv1alpha1.FeedGroup) {
				fg.Spec.RetryInterval = "5m"
				fg.Spec.CatchUpLimit = new(int32(5))
			})
			discordServer.FailNextRequestsRateLimited(10, "2")

			By("Running reconciliation")
			result, err := reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying only ONE POST was attempted before backing off, not one per entry")
			Expect(discordServer.DeliveryAttempts()).To(Equal(1))
			Expect(discordServer.MessageCount()).To(Equal(0))
			Expect(result.RequeueAfter).To(Equal(2 * time.Second))
		})

		It("should stop processing the rest of a FeedGroup's feeds once one feed's send is rate-limited", func() {
			const feedGroupName = "test-feedgroup-rate-limited-multi-feed"

			By("Creating a mock Discord server that rate-limits the very first delivery")
			discordServer := NewMockDiscordServer(GinkgoTB())
			defer discordServer.Close()
			discordServer.FailNextRequestsRateLimited(1, "3")

			By("Creating two mock RSS feed servers, each with one entry")
			firstFeed := NewMockRSSServer(createRSSFeed(
				testEntry{
					title:       "First Feed Article",
					description: "Triggers the rate limit",
					link:        "https://example.com/first-feed-article",
					pubDate:     time.Now().Format(time.RFC1123Z),
					guid:        "first-feed-article",
				},
			))
			defer firstFeed.Close()
			secondFeed := NewMockRSSServer(createRSSFeed(
				testEntry{
					title:       "Second Feed Article",
					description: "Should not be reached this reconcile",
					link:        "https://example.com/second-feed-article",
					pubDate:     time.Now().Format(time.RFC1123Z),
					guid:        "second-feed-article",
				},
			))
			defer secondFeed.Close()

			By("Creating a secret with Discord webhook URL")
			createWebhookSecret(ctx, "discord-webhook-rl-multi-feed", discordServer.URL())

			By("Creating a FeedGroup with both feeds, first feed listed first")
			feedGroup := newTestFeedGroupFeeds(feedGroupName, "discord-webhook-rl-multi-feed", []rss2discordv1alpha1.FeedSpec{
				{RSSUrl: firstFeed.URL()},
				{RSSUrl: secondFeed.URL()},
			}, func(fg *rss2discordv1alpha1.FeedGroup) {
				fg.Spec.RetryInterval = "5m"
			})
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			reconciler := newTestReconciler(discordServer)

			By("Running reconciliation")
			result, err := reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the second feed was fetched concurrently but never processed/sent, since the shared webhook was already rate-limited")
			Expect(discordServer.MessageCount()).To(Equal(0))

			By("Verifying the reconcile requeues after the rate limit's Retry-After duration")
			Expect(result.RequeueAfter).To(Equal(3 * time.Second))

			updated := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			Expect(feedStatusFor(updated, firstFeed.URL()).LastChecked).NotTo(BeEmpty())
			Expect(feedStatusFor(updated, secondFeed.URL()).LastChecked).To(BeEmpty())
		})
	})

	Describe("FeedGroup validation", func() {
		It("should reject a FeedGroup with duplicate feed URLs", func() {
			feedGroup := &rss2discordv1alpha1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{Name: "test-feedgroup-dup-url", Namespace: namespace},
				Spec: rss2discordv1alpha1.FeedGroupSpec{
					DiscordWebhookSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "any-secret"},
						Key:                  secretURLKey,
					},
					Feeds: []rss2discordv1alpha1.FeedSpec{
						{RSSUrl: exampleFeedURL},
						{RSSUrl: exampleFeedURL},
					},
				},
			}
			err := k8sClient.Create(ctx, feedGroup)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("rssUrl must be unique"))
		})

		It("should accept a FeedGroup with distinct feed URLs", func() {
			feedGroup := &rss2discordv1alpha1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{Name: "test-feedgroup-distinct-url", Namespace: namespace},
				Spec: rss2discordv1alpha1.FeedGroupSpec{
					DiscordWebhookSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "any-secret"},
						Key:                  secretURLKey,
					},
					Feeds: []rss2discordv1alpha1.FeedSpec{
						{RSSUrl: "https://example.com/a.xml"},
						{RSSUrl: "https://example.com/b.xml"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())
		})
	})

	Describe("Concurrent feed fetching", func() {
		It("should fetch and send for every feed when the FeedGroup exceeds the concurrent-fetch limit", func() {
			const feedGroupName = "test-feedgroup-concurrent"
			const numFeeds = 15 // exceeds maxConcurrentFetches (10), to exercise the semaphore-bounded fan-out under -race.

			By("Creating one mock RSS server per feed, each with a single unique entry")
			rssServers := make([]*MockRSSServer, numFeeds)
			feeds := make([]rss2discordv1alpha1.FeedSpec, numFeeds)
			for i := range rssServers {
				rssServers[i] = NewMockRSSServer(createRSSFeed(
					testEntry{
						title:       fmt.Sprintf("Concurrent Article %d", i),
						description: "fetched concurrently",
						link:        fmt.Sprintf("https://example.com/concurrent-%d", i),
						pubDate:     time.Now().Format(time.RFC1123Z),
						guid:        fmt.Sprintf("concurrent-%d", i),
					},
				))
				feeds[i] = rss2discordv1alpha1.FeedSpec{RSSUrl: rssServers[i].URL()}
			}
			defer func() {
				for _, s := range rssServers {
					s.Close()
				}
			}()

			By("Creating a mock Discord webhook server shared by all feeds")
			discordServer := NewMockDiscordServer(GinkgoTB())
			defer discordServer.Close()

			By("Creating a secret with Discord webhook URL")
			createWebhookSecret(ctx, "discord-webhook-concurrent", discordServer.URL())

			By("Creating a FeedGroup with more feeds than the concurrent-fetch limit")
			feedGroup := newTestFeedGroupFeeds(feedGroupName, "discord-webhook-concurrent", feeds, func(fg *rss2discordv1alpha1.FeedGroup) {
				fg.Spec.RetryInterval = "5m"
			})
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			reconciler := newTestReconciler(discordServer)

			By("Running reconciliation")
			result, err := reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())
			// Normal interval, plus up to 10% jitter.
			Expect(result.RequeueAfter).To(BeNumerically(">=", 30*time.Minute))
			Expect(result.RequeueAfter).To(BeNumerically("<=", 33*time.Minute))

			By("Verifying every feed was fetched and its entry delivered, despite exceeding the concurrency limit")
			Expect(discordServer.MessageCount()).To(Equal(numFeeds))

			updated := &rss2discordv1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			Expect(feedsWithError(updated)).To(Equal(0))
			for _, s := range rssServers {
				Expect(feedStatusFor(updated, s.URL()).LastChecked).NotTo(BeEmpty())
				Expect(feedStatusFor(updated, s.URL()).LastSeenEntry).NotTo(BeEmpty())
			}
		})
	})

	Describe("Resource not found", func() {
		It("should handle non-existent FeedGroup gracefully", func() {
			By("Running reconciliation for non-existent resource")
			reconciler := newTestReconciler(nil)

			defer groupMetrics{namespace: namespace, name: "nonexistent"}.forget()

			result, err := reconcileFeedGroup(ctx, reconciler, "nonexistent")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Duration(0)))

			By("Verifying no reconcile-duration series was created for a FeedGroup that doesn't exist")
			count, err := reconcileDurationSampleCountErr(namespace, "nonexistent")
			Expect(err).NotTo(HaveOccurred())
			Expect(count).To(Equal(uint64(0)))
		})
	})

	Describe("Username admission validation", func() {
		// minimalFeedGroup returns a FeedGroup that satisfies every field
		// other than Username, so these tests isolate the CEL rule on
		// Username rather than tripping over unrelated required fields.
		minimalFeedGroup := func(name, username string) *rss2discordv1alpha1.FeedGroup {
			return &rss2discordv1alpha1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: namespace,
				},
				Spec: rss2discordv1alpha1.FeedGroupSpec{
					DiscordWebhookSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: discordWebhookSecretName},
						Key:                  secretURLKey,
					},
					Username: username,
					Feeds: []rss2discordv1alpha1.FeedSpec{
						{RSSUrl: exampleFeedURL},
					},
				},
			}
		}

		DescribeTable("should reject usernames Discord itself would reject",
			func(username string) {
				err := k8sClient.Create(ctx, minimalFeedGroup("username-rejected", username))
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("username must not contain"))
			},
			Entry("contains clyde", "ClydeBot"),
			Entry("contains discord", "totally-discord-official"),
		)

		It("should reject a username over 80 characters", func() {
			err := k8sClient.Create(ctx, minimalFeedGroup("username-too-long", strings.Repeat("a", 81)))
			Expect(err).To(HaveOccurred())
		})

		It("should accept a username that doesn't violate either constraint", func() {
			feedGroup := minimalFeedGroup("username-ok", "tech-news-bot")
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())
			Expect(k8sClient.Delete(ctx, feedGroup)).To(Succeed())
		})
	})

	Describe("Bounds and format admission validation", func() {
		build := func(name string, mutate func(*rss2discordv1alpha1.FeedGroup)) *rss2discordv1alpha1.FeedGroup {
			fg := &rss2discordv1alpha1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec: rss2discordv1alpha1.FeedGroupSpec{
					DiscordWebhookSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: discordWebhookSecretName},
						Key:                  secretURLKey,
					},
					Feeds: []rss2discordv1alpha1.FeedSpec{{RSSUrl: exampleFeedURL}},
				},
			}
			mutate(fg)
			return fg
		}

		DescribeTable("should reject out-of-range or malformed values",
			func(mutate func(*rss2discordv1alpha1.FeedGroup), wantErr string) {
				err := k8sClient.Create(ctx, build("admission-rejected", mutate))
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring(wantErr))
			},
			Entry("negative retries", func(fg *rss2discordv1alpha1.FeedGroup) { fg.Spec.Retries = -1 }, "retries"),
			Entry("retries over 100", func(fg *rss2discordv1alpha1.FeedGroup) { fg.Spec.Retries = 101 }, "retries"),
			Entry("negative catchUpLimit", func(fg *rss2discordv1alpha1.FeedGroup) { fg.Spec.CatchUpLimit = new(int32(-1)) }, "catchUpLimit"),
			Entry("catchUpLimit over 100", func(fg *rss2discordv1alpha1.FeedGroup) { fg.Spec.CatchUpLimit = new(int32(101)) }, "catchUpLimit"),
			Entry("sub-minute interval", func(fg *rss2discordv1alpha1.FeedGroup) { fg.Spec.Interval = "1ns" }, "interval must be at least 1m"),
			Entry("overflowing interval", func(fg *rss2discordv1alpha1.FeedGroup) { fg.Spec.Interval = "9999999999999h" }, "interval"),
			Entry("sub-10s retryInterval", func(fg *rss2discordv1alpha1.FeedGroup) { fg.Spec.RetryInterval = "1s" }, "retryInterval must be at least 10s"),
			Entry("microsecond-sign unit", func(fg *rss2discordv1alpha1.FeedGroup) { fg.Spec.Interval = "100\u00b5s" }, "interval"),
			Entry("non-http avatarURL", func(fg *rss2discordv1alpha1.FeedGroup) { fg.Spec.AvatarURL = "ftp://example.com/a.png" }, "avatarURL"),
			Entry("unknown embed imageStyle", func(fg *rss2discordv1alpha1.FeedGroup) {
				fg.Spec.Embed = &rss2discordv1alpha1.EmbedSpec{Enabled: true, ImageStyle: "banner"}
			}, "imageStyle"),
			Entry("non-numeric forumThreadID", func(fg *rss2discordv1alpha1.FeedGroup) { fg.Spec.Feeds[0].ForumThreadID = "abc" }, "forumThreadID"),
		)

		It("should accept values at the boundaries", func() {
			fg := build("admission-ok", func(fg *rss2discordv1alpha1.FeedGroup) {
				fg.Spec.Retries = 100
				fg.Spec.CatchUpLimit = new(int32(100))
				fg.Spec.Interval = "1m"
				fg.Spec.RetryInterval = "10s"
				fg.Spec.AvatarURL = "https://example.com/a.png"
				fg.Spec.Feeds[0].ForumThreadID = "1234567890"
			})
			Expect(k8sClient.Create(ctx, fg)).To(Succeed())
			Expect(k8sClient.Delete(ctx, fg)).To(Succeed())
		})
	})

	Describe("SetupWithManager", func() {
		It("should default the event recorder when none is set", func() {
			mgr, err := ctrl.NewManager(cfg, ctrl.Options{
				Scheme:  k8sClient.Scheme(),
				Metrics: metricsserver.Options{BindAddress: "0"},
			})
			Expect(err).NotTo(HaveOccurred())

			reconciler := &FeedGroupReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}
			Expect(reconciler.Recorder).To(BeNil())

			Expect(reconciler.SetupWithManager(mgr)).To(Succeed())
			Expect(reconciler.Recorder).NotTo(BeNil())
		})
	})
})

// failingClient wraps a real client to inject API errors on specific calls,
// for the reconcile error paths envtest can't produce on demand.
type failingClient struct {
	client.Client
	getFeedGroupErr error
	statusApplyErr  error
}

func (c *failingClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*rss2discordv1alpha1.FeedGroup); ok && c.getFeedGroupErr != nil {
		return c.getFeedGroupErr
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func (c *failingClient) Status() client.SubResourceWriter {
	return &failingStatusWriter{SubResourceWriter: c.Client.Status(), applyErr: c.statusApplyErr}
}

type failingStatusWriter struct {
	client.SubResourceWriter
	applyErr error
}

func (w *failingStatusWriter) Apply(ctx context.Context, obj runtime.ApplyConfiguration, opts ...client.SubResourceApplyOption) error {
	if w.applyErr != nil {
		return w.applyErr
	}
	return w.SubResourceWriter.Apply(ctx, obj, opts...)
}

var _ = Describe("Reconcile API error paths", func() {
	It("should return a non-NotFound error from fetching the FeedGroup, so it is retried with backoff", func() {
		boom := errors.New("apiserver unavailable")
		r := &FeedGroupReconciler{Client: &failingClient{Client: k8sClient, getFeedGroupErr: boom}, Scheme: k8sClient.Scheme()}

		res, err := reconcileFeedGroupNow(ctx, r, "does-not-matter")

		Expect(err).To(MatchError(boom))
		Expect(res).To(Equal(reconcile.Result{}))
	})

	It("should surface a failed status write instead of reporting a successful reconcile", func() {
		const feedGroupName = "test-feedgroup-status-apply-error"
		_, _, reconciler := setUpFeedGroup(ctx, feedGroupName, "discord-webhook-apply-error", createRSSFeed(
			testEntry{title: "Apply Error", link: "https://example.com/apply-error", guid: "apply-error", pubDate: time.Now().Format(time.RFC1123Z)},
		))
		boom := errors.New("status apply rejected")
		reconciler.Client = &failingClient{Client: k8sClient, statusApplyErr: boom}

		_, err := reconcileFeedGroupNow(ctx, reconciler, feedGroupName)

		Expect(err).To(MatchError(boom))
	})

	It("should report both a feed's panic and the failed status write that followed it", func() {
		const feedGroupName = "test-feedgroup-panic-apply-error"
		_, _, reconciler := setUpFeedGroup(ctx, feedGroupName, "discord-webhook-panic-apply-error", createRSSFeed(
			testEntry{title: "Panic Apply Error", link: "https://example.com/panic-apply-error", guid: "panic-apply-error", pubDate: time.Now().Format(time.RFC1123Z)},
		))
		boom := errors.New("status apply rejected")
		reconciler.Client = &failingClient{Client: k8sClient, statusApplyErr: boom}
		reconciler.DiscordClientBuilder = func(string) MessageSender { return &panicAfterSender{} }

		_, err := reconcileFeedGroupNow(ctx, reconciler, feedGroupName)

		Expect(err).To(MatchError(boom))
		Expect(err).To(MatchError(ContainSubstring("panic while processing feed")))
	})
})

// panicAfterSender forwards the first after sends to inner, then panics.
type panicAfterSender struct {
	inner       MessageSender
	after, sent int
}

func (p *panicAfterSender) SendMessage(ctx context.Context, msg discord.Message) error {
	if p.sent >= p.after {
		panic("send exploded")
	}
	p.sent++
	return p.inner.SendMessage(ctx, msg)
}
