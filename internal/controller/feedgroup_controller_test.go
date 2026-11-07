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

	"k8s.io/utils/ptr"

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

	rss2discordv1beta1 "github.com/maverickd650-labs/rss2discord/api/v1beta1"
	"github.com/maverickd650-labs/rss2discord/internal/discord"
	"github.com/maverickd650-labs/rss2discord/internal/rss"
)

const feedGroupValidationSecretName = "any-secret"

const (
	secretURLKey             = "url"
	defaultInterval          = "30m"
	feedGroupNameBasic       = "test-feedgroup"
	feedGroupNameNoSecret    = "test-feedgroup-no-secret"
	feedGroupNameRetry       = "test-feedgroup-retry"
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
func feedConditionReason(fg *rss2discordv1beta1.FeedGroup, url, condType string) string {
	fs := feedStatusFor(fg, testFeedName(url))
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
func feedsWithError(fg *rss2discordv1beta1.FeedGroup) int {
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
	return NewDiscordClientBuilder(d.server.Client())
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

type feedGroupOption func(*rss2discordv1beta1.FeedGroup)

// withRetries sets the retry interval and retry count.
func withRetries(retryInterval string, retries int32) feedGroupOption {
	return func(fg *rss2discordv1beta1.FeedGroup) {
		fg.Spec.RetryInterval = retryInterval
		fg.Spec.Retries = new(retries)
	}
}

// newTestFeedGroup builds a FeedGroup in the test namespace that reads its
// webhook URL from secretName and watches a single feed at feedURL, with
// defaultInterval. Opts adjust the result.
func newTestFeedGroup(group testGroup, feedURL string, opts ...feedGroupOption) *rss2discordv1beta1.FeedGroup {
	return newTestFeedGroupFeeds(group, []rss2discordv1beta1.FeedSpec{{Name: testFeedName(feedURL), URL: feedURL}}, opts...)
}

// newTestFeedGroupFeeds is newTestFeedGroup for a FeedGroup that watches the
// given feeds.
func newTestFeedGroupFeeds(group testGroup, feeds []rss2discordv1beta1.FeedSpec, opts ...feedGroupOption) *rss2discordv1beta1.FeedGroup {
	fg := &rss2discordv1beta1.FeedGroup{
		ObjectMeta: metav1.ObjectMeta{Name: group.name, Namespace: testNamespace},
		Spec: rss2discordv1beta1.FeedGroupSpec{
			DiscordWebhookSecretRef: corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: group.secret},
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

// testGroup names a FeedGroup and the webhook Secret it reads its URL from.
type testGroup struct {
	name   string
	secret string
}

// setUpFeedGroup starts mock Discord and RSS servers (closed when the spec
// ends), creates the webhook Secret and a FeedGroup watching the RSS server,
// and returns the servers plus a reconciler wired to them. Opts adjust the
// FeedGroup before it is created.
func setUpFeedGroup(ctx context.Context, group testGroup, feedXML string, opts ...feedGroupOption) (*MockDiscordServer, *MockRSSServer, *FeedGroupReconciler) {
	GinkgoHelper()
	discordServer := NewMockDiscordServer(GinkgoTB())
	DeferCleanup(discordServer.Close)
	rssServer := NewMockRSSServer(feedXML)
	DeferCleanup(rssServer.Close)

	createWebhookSecret(ctx, group.secret, discordServer.URL())
	Expect(k8sClient.Create(ctx, newTestFeedGroup(group, rssServer.URL(), opts...))).To(Succeed())
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
	var fg rss2discordv1beta1.FeedGroup
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, &fg); err != nil {
		return // not created yet / already gone; the reconcile reports it
	}
	changed := false
	for i := range fg.Status.Feeds {
		if fg.Status.Feeds[i].LastChecked != nil {
			t := metav1.NewTime(time.Now().Add(-ago).UTC())
			fg.Status.Feeds[i].LastChecked = &t
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
			_, rssServer, reconciler := setUpFeedGroup(ctx, testGroup{name: feedGroupNameBasic, secret: discordWebhookSecretName}, createRSSFeed(
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
			), withRetries("5m", 3), func(fg *rss2discordv1beta1.FeedGroup) {
				fg.Spec.Message = &rss2discordv1beta1.MessageSpec{Content: "**{{.Title}}**\n{{.Description}}\n[Read more]({{.Link}})"}
			})

			By("Running reconciliation")

			result, err := reconcileFeedGroup(ctx, reconciler, feedGroupNameBasic)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			By("Verifying FeedGroup status was updated")
			updated := &rss2discordv1beta1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupNameBasic, Namespace: namespace}, updated)).To(Succeed())
			Expect(updated.Spec.CatchUpLimit).To(HaveValue(Equal(int32(5))), "omitted catchUpLimit must default to 5")

			Expect(feedStatusFor(updated, testFeedName(rssServer.URL())).LastChecked).NotTo(BeNil())
			Expect(feedStatusFor(updated, testFeedName(rssServer.URL())).LastSeenEntry).NotTo(BeEmpty())

			By("Verifying no errors in status")
			Expect(feedsWithError(updated)).To(Equal(0))

			By("Verifying the Ready condition reflects success")
			readyCondition := apimeta.FindStatusCondition(updated.Status.Conditions, rss2discordv1beta1.ConditionTypeReady)
			Expect(readyCondition).NotTo(BeNil())
			Expect(readyCondition.Status).To(Equal(metav1.ConditionTrue))
			Expect(updated.Status.ObservedGeneration).To(Equal(updated.Generation))
		})
	})

	Describe("Conditional RSS fetches", func() {
		It("should send a stored ETag on the next reconcile and skip re-processing on 304", func() {
			const feedGroupName = "test-feedgroup-conditional-get"

			By("Creating mock Discord and RSS servers, the webhook secret and the FeedGroup")
			discordServer, rssServer, reconciler := setUpFeedGroup(ctx, testGroup{name: feedGroupName, secret: "discord-webhook-conditional"}, createRSSFeed(
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

			afterFirst := &rss2discordv1beta1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, afterFirst)).To(Succeed())
			Expect(feedStatusFor(afterFirst, testFeedName(rssServer.URL())).ETag).To(Equal(`"v1"`))
			lastSeenAfterFirst := feedStatusFor(afterFirst, testFeedName(rssServer.URL())).LastSeenEntry
			Expect(lastSeenAfterFirst).NotTo(BeEmpty())

			By("Running a second reconciliation against the unchanged feed")
			_, err = reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the second request carried the stored ETag and got a 304")
			Expect(rssServer.RequestCount()).To(Equal(2))
			Expect(rssServer.IfNoneMatchSeen()[1]).To(Equal(`"v1"`))

			By("Verifying no new message was sent and status was left untouched")
			Expect(discordServer.MessageCount()).To(Equal(1))

			afterSecond := &rss2discordv1beta1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, afterSecond)).To(Succeed())
			Expect(feedStatusFor(afterSecond, testFeedName(rssServer.URL())).LastSeenEntry).To(Equal(lastSeenAfterFirst))
			Expect(feedsWithError(afterSecond)).To(Equal(0))
			Expect(feedStatusFor(afterSecond, testFeedName(rssServer.URL())).RetryCount).To(Equal(int32(0)))
		})

		It("should not refetch a feed that was checked less than one interval ago", func() {
			const feedGroupName = "test-feedgroup-interval-gate"

			discordServer, rssServer, reconciler := setUpFeedGroup(ctx, testGroup{name: feedGroupName, secret: "discord-webhook-interval-gate"}, createRSSFeed(
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
			discordServer, rssServer, reconciler := setUpFeedGroup(ctx, testGroup{name: feedGroupName, secret: "discord-webhook-conditional-retry"}, createRSSFeed(
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
			afterFirst := &rss2discordv1beta1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, afterFirst)).To(Succeed())
			Expect(feedStatusFor(afterFirst, testFeedName(rssServer.URL())).ETag).To(BeEmpty())
			Expect(feedStatusFor(afterFirst, testFeedName(rssServer.URL())).LastSeenEntry).To(BeEmpty())
			Expect(feedStatusFor(afterFirst, testFeedName(rssServer.URL())).LastError).NotTo(BeEmpty())

			By("Running a second reconciliation, where the Discord send succeeds")
			_, err = reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the previously unsent entry was retried and delivered")
			Expect(discordServer.MessageCount()).To(Equal(1))
			Expect(rssServer.IfNoneMatchSeen()[1]).To(BeEmpty(), "second fetch should not have sent the never-persisted ETag")

			afterSecond := &rss2discordv1beta1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, afterSecond)).To(Succeed())
			Expect(feedStatusFor(afterSecond, testFeedName(rssServer.URL())).ETag).To(Equal(`"v1"`))
			Expect(feedStatusFor(afterSecond, testFeedName(rssServer.URL())).LastSeenEntry).NotTo(BeEmpty())
			Expect(feedsWithError(afterSecond)).To(Equal(0))
		})
	})

	Describe("Catch-up limit", func() {
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
			discordServer, rssServer, reconciler := setUpFeedGroup(ctx, testGroup{name: feedGroupName, secret: "discord-webhook-skip-backlog"}, createRSSFeed(entries...), withRetries("5m", 3), func(fg *rss2discordv1beta1.FeedGroup) {
				fg.Spec.CatchUpLimit = new(int32(0))
			})

			By("Verifying typed creation preserves an explicit catchUpLimit of 0")
			fg := &rss2discordv1beta1.FeedGroup{}
			key := types.NamespacedName{Name: feedGroupName, Namespace: namespace}
			Expect(k8sClient.Get(ctx, key, fg)).To(Succeed())
			Expect(fg.Spec.CatchUpLimit).To(HaveValue(Equal(int32(0))))

			By("Reconciling the first sync")
			_, err := reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())
			Expect(discordServer.MessageCount()).To(Equal(0), "no backlog should be sent")

			Expect(k8sClient.Get(ctx, key, fg)).To(Succeed())
			fs := feedStatusFor(fg, testFeedName(rssServer.URL()))
			Expect(fs.LastSeenEntry).To(Equal("skip-backlog-10"), "watermark should be the newest entry")
			delivered := apimeta.FindStatusCondition(fs.Conditions, rss2discordv1beta1.FeedConditionTypeDelivered)
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
			discordServer, rssServer, reconciler := setUpFeedGroup(ctx, testGroup{name: feedGroupName, secret: "discord-webhook-empty-first-sync"}, createRSSFeed(), withRetries("5m", 3), func(fg *rss2discordv1beta1.FeedGroup) {
				fg.Spec.CatchUpLimit = new(int32(0))
			})

			_, err := reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())
			fg := &rss2discordv1beta1.FeedGroup{}
			key := types.NamespacedName{Name: feedGroupName, Namespace: namespace}
			Expect(k8sClient.Get(ctx, key, fg)).To(Succeed())
			fs := feedStatusFor(fg, testFeedName(rssServer.URL()))
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
	})

	Describe("Error handling", func() {
		It("should handle missing Discord webhook secret", func() {
			By("Creating FeedGroup without secret")
			feedGroup := newTestFeedGroup(testGroup{name: feedGroupNameNoSecret, secret: "nonexistent-secret"}, "https://example.com/feed")
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			By("Running reconciliation")
			reconciler := newTestReconciler(nil)

			result, err := reconcileFeedGroup(ctx, reconciler, feedGroupNameNoSecret)
			// Controller handles missing secret gracefully and requeues
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			By("Verifying FeedGroup still exists and status is updated")
			updated := &rss2discordv1beta1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupNameNoSecret, Namespace: namespace}, updated)).To(Succeed())
		})

		It("should handle a webhook secret whose value is blank", func() {
			const feedGroupName = "test-feedgroup-blank-secret-value"

			By("Creating a secret whose key holds only whitespace")
			createWebhookSecret(ctx, "discord-webhook-blank-value", "   ")

			feedGroup := newTestFeedGroup(testGroup{name: feedGroupName, secret: "discord-webhook-blank-value"}, exampleFeedURL)
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			By("Running reconciliation")
			reconciler := newTestReconciler(nil)

			result, err := reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			By("Verifying the webhook error was recorded and the Ready condition reflects it")
			updated := &rss2discordv1beta1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			readyCondition := apimeta.FindStatusCondition(updated.Status.Conditions, rss2discordv1beta1.ConditionTypeReady)
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

			feedGroup := newTestFeedGroup(testGroup{name: feedGroupName, secret: "discord-webhook-missing-key"}, exampleFeedURL)
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			By("Running reconciliation")
			reconciler := newTestReconciler(nil)

			result, err := reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			By("Verifying the missing-key error was recorded on status")
			updated := &rss2discordv1beta1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			readyCondition := apimeta.FindStatusCondition(updated.Status.Conditions, rss2discordv1beta1.ConditionTypeReady)
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
			feedGroup := newTestFeedGroup(testGroup{name: feedGroupNameRetry, secret: "discord-webhook-2"}, rssServer.URL, withRetries("1m", 2))
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
			updated := &rss2discordv1beta1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupNameRetry, Namespace: namespace}, updated)).To(Succeed())
			Expect(feedStatusFor(updated, testFeedName(rssServer.URL)).LastError).NotTo(BeEmpty())

			By("Verifying the Ready condition reflects the failure")
			readyCondition := apimeta.FindStatusCondition(updated.Status.Conditions, rss2discordv1beta1.ConditionTypeReady)
			Expect(readyCondition).NotTo(BeNil())
			Expect(readyCondition.Status).To(Equal(metav1.ConditionFalse))
			Expect(readyCondition.Reason).To(Equal("FeedErrors"))
			Expect(feedStatusFor(updated, testFeedName(rssServer.URL)).RetryCount).To(Equal(int32(1)))
			Expect(recorder.Events).To(BeEmpty(), "no event should fire before retries are exhausted")

			By("Reconciling again to exhaust the configured retries")
			_, err = reconcileFeedGroup(ctx, reconciler, feedGroupNameRetry)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying a persistent-failure Event was recorded once retries were exhausted")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupNameRetry, Namespace: namespace}, updated)).To(Succeed())
			Expect(feedStatusFor(updated, testFeedName(rssServer.URL)).RetryCount).To(Equal(int32(2)))
			Eventually(recorder.Events).Should(Receive(ContainSubstring("FetchFailed")))

			By("Verifying the FeedReachable condition and classified error reason")
			Expect(feedConditionReason(updated, rssServer.URL, rss2discordv1beta1.FeedConditionTypeReachable)).To(Equal("ServerError"))
			reachableCondition := apimeta.FindStatusCondition(updated.Status.Conditions, rss2discordv1beta1.ConditionTypeFeedReachable)
			Expect(reachableCondition).NotTo(BeNil())
			Expect(reachableCondition.Status).To(Equal(metav1.ConditionFalse))
			Expect(reachableCondition.Reason).To(Equal("ServerError"))

			By("Reconciling a third time, with the feed still failing the same way")
			_, err = reconcileFeedGroup(ctx, reconciler, feedGroupNameRetry)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying no second persistent-failure Event fires for the same ongoing failure")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupNameRetry, Namespace: namespace}, updated)).To(Succeed())
			Expect(feedStatusFor(updated, testFeedName(rssServer.URL)).RetryCount).To(Equal(int32(3)))
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
			feedGroup := newTestFeedGroup(testGroup{name: feedGroupName, secret: "discord-webhook-default-rss-client"}, rssServer.URL(), withRetries("5m", 3))
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			By("Running reconciliation without setting RSSClient")
			reconciler := &FeedGroupReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the feed failed with the SSRF guard's error, not a real fetch")
			updated := &rss2discordv1beta1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			Expect(feedStatusFor(updated, testFeedName(rssServer.URL())).LastError).To(ContainSubstring("refusing to connect to non-public address"))
		})

		It("should persist what a reconcile sent before a feed panicked, and not send it again", func() {
			const feedGroupName = "test-feedgroup-panic"

			By("Creating a FeedGroup whose feed has two new entries")
			discordServer, rssServer, reconciler := setUpFeedGroup(ctx, testGroup{name: feedGroupName, secret: "discord-webhook-panic"}, createRSSFeed(
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
			updated := &rss2discordv1beta1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			fs := feedStatusFor(updated, testFeedName(rssServer.URL()))
			Expect(fs.LastSeenEntry).To(Equal("panic-older-article"))
			Expect(fs.LastSent).To(HaveLen(1))
			Expect(fs.ETag).To(BeEmpty())
			Expect(fs.LastError).To(ContainSubstring(errDeliveryPanicked.Error()))
			By("Verifying the incomplete pass did not advance ObservedGeneration")
			Expect(updated.Status.ObservedGeneration).To(BeZero())
			By("Verifying the group is not reported Ready while a feed panics")
			ready := apimeta.FindStatusCondition(updated.Status.Conditions, rss2discordv1beta1.ConditionTypeReady)
			Expect(ready).NotTo(BeNil())
			Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			Expect(ready.Reason).To(Equal(reasonReconcilePanicked))
			Expect(ready.Message).To(ContainSubstring(rssServer.URL()))
			By("Verifying the panic value and stack are kept out of every persisted field")
			Expect(ready.Message).NotTo(ContainSubstring(sendExplodedPanic))
			Expect(ready.Message).NotTo(ContainSubstring("goroutine"))
			Expect(fs.LastError).NotTo(ContainSubstring(sendExplodedPanic))
			for _, c := range fs.Conditions {
				Expect(c.Message).NotTo(ContainSubstring(sendExplodedPanic))
			}

			By("Reconciling again at once, with a working sender")
			reconciler.DiscordClientBuilder = realBuilder
			_, err = reconcileFeedGroupNow(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying only the unsent entry was delivered, not the one sent before the panic")
			Expect(discordServer.MessageCount()).To(Equal(2))
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			Expect(updated.Status.ObservedGeneration).To(Equal(updated.Generation))
			Expect(feedStatusFor(updated, testFeedName(rssServer.URL())).LastError).To(BeEmpty())
			Expect(apimeta.IsStatusConditionTrue(updated.Status.Conditions, rss2discordv1beta1.ConditionTypeReady)).To(BeTrue())
		})

		It("should persist status and send once when the interval cannot be parsed", func() {
			const feedGroupName = "test-feedgroup-invalid-interval"

			By("Creating a FeedGroup with an unparsable interval and a feed with one new entry")
			discordServer, rssServer, reconciler := setUpFeedGroup(ctx, testGroup{name: feedGroupName, secret: "discord-webhook-invalid-interval"}, createRSSFeed(
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
					if fg, ok := obj.(*rss2discordv1beta1.FeedGroup); ok {
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
			updated := &rss2discordv1beta1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			fs := feedStatusFor(updated, testFeedName(rssServer.URL()))
			Expect(fs.LastSent).NotTo(BeEmpty())
			Expect(fs.LastSeenEntry).NotTo(BeEmpty())
			Expect(fs.LastChecked).NotTo(BeNil())

			By("Verifying Ready=False/InvalidInterval names the offending value, with one Warning Event")
			ready := apimeta.FindStatusCondition(updated.Status.Conditions, rss2discordv1beta1.ConditionTypeReady)
			Expect(ready).NotTo(BeNil())
			Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			Expect(ready.Reason).To(Equal("InvalidInterval"))
			Expect(ready.Message).To(ContainSubstring("interval"))
			Expect(ready.Message).To(ContainSubstring(overflowingInterval))
			Eventually(recorder.Events).Should(Receive(ContainSubstring("InvalidInterval")))

			By("Making an unrelated valid spec edit so the generation advances while the invalid interval stays the same")
			generationBefore := updated.Generation
			updated.Spec.Retries = new(ptr.Deref(updated.Spec.Retries, 0) + 1)
			Expect(k8sClient.Update(ctx, updated)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			Expect(updated.Generation).To(BeNumerically(">", generationBefore))

			By("Reconciling again, with the feed due: nothing is re-sent and no second Event fires")
			_, err = reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())
			Expect(discordServer.MessageCount()).To(Equal(1))
			Consistently(recorder.Events).ShouldNot(Receive())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			ready = apimeta.FindStatusCondition(updated.Status.Conditions, rss2discordv1beta1.ConditionTypeReady)
			Expect(ready).NotTo(BeNil())
			Expect(ready.Reason).To(Equal("InvalidInterval"))
			Expect(ready.ObservedGeneration).To(Equal(updated.Generation))
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
			feedGroup := newTestFeedGroupFeeds(testGroup{name: feedGroupName, secret: "discord-webhook-removed"}, []rss2discordv1beta1.FeedSpec{
				{Name: testFeedName(rssServer.URL()), URL: rssServer.URL()},
				{Name: testFeedName(keptFeedURL), URL: keptFeedURL, Suspend: true},
			}, withRetries("5m", 3))
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			reconciler := newTestReconciler(discordServer)

			By("Running the first reconciliation so the removed feed accrues status")
			_, err := reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())

			afterFirst := &rss2discordv1beta1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, afterFirst)).To(Succeed())
			Expect(feedStatusFor(afterFirst, testFeedName(rssServer.URL())).LastChecked).NotTo(BeNil())
			Expect(feedStatusFor(afterFirst, testFeedName(rssServer.URL())).ETag).NotTo(BeEmpty())

			By("Removing the feed from spec, keeping the paused one")
			afterFirst.Spec.Feeds = []rss2discordv1beta1.FeedSpec{
				{Name: testFeedName(keptFeedURL), URL: keptFeedURL, Suspend: true},
			}
			Expect(k8sClient.Update(ctx, afterFirst)).To(Succeed())

			By("Running another reconciliation")
			_, err = reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the removed feed's status entries were pruned")
			afterSecond := &rss2discordv1beta1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, afterSecond)).To(Succeed())
			Expect(feedStatusFor(afterSecond, testFeedName(rssServer.URL()))).To(BeNil())
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

			feedGroup := newTestFeedGroup(testGroup{name: feedGroupName, secret: "discord-webhook-ssa-concurrent"}, "https://example.com/ssa-concurrent.xml")
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())

			reconciler := &FeedGroupReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

			By("Reading two independent copies of the FeedGroup at the same resourceVersion")
			fgA := &rss2discordv1beta1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, fgA)).To(Succeed())
			fgB := &rss2discordv1beta1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, fgB)).To(Succeed())
			Expect(fgA.ResourceVersion).To(Equal(fgB.ResourceVersion), "both copies must start at the same resourceVersion for this to test anything")

			By("Applying status computed from the first copy")
			ensureFeedStatuses(fgA)
			fgA.Status.ObservedGeneration = fgA.Generation
			setReadyCondition(fgA, nil, nil)
			setFeedReachableCondition(fgA)
			Expect(reconciler.applyStatus(ctx, fgA)).To(Succeed())

			By("Verifying the first apply actually changed the object's resourceVersion server-side")
			updated := &rss2discordv1beta1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			Expect(updated.ResourceVersion).NotTo(Equal(fgB.ResourceVersion))

			By("Verifying the write was recorded as an Apply on the status subresource by this controller")
			Expect(updated.ManagedFields).To(ContainElement(SatisfyAll(
				HaveField("Manager", statusFieldOwner),
				HaveField("Operation", metav1.ManagedFieldsOperationApply),
				HaveField("Subresource", "status"),
			)))
			Expect(updated.Status.Feeds).To(ConsistOf(HaveField("URL", "https://example.com/ssa-concurrent.xml")))

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
			feedGroup := newTestFeedGroup(testGroup{name: "test-feedgroup-interval", secret: "discord-webhook-7"}, rssServer.URL(), func(fg *rss2discordv1beta1.FeedGroup) {
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

			feedGroup := newTestFeedGroup(testGroup{name: "test-feedgroup-bad-interval", secret: "discord-webhook-bad-interval"}, exampleFeedURL, func(fg *rss2discordv1beta1.FeedGroup) {
				fg.Spec.Interval = "30 minutes"
			})
			Expect(k8sClient.Create(ctx, feedGroup)).To(MatchError(ContainSubstring("spec.interval")))
		})

		It("should reject a RetryInterval that isn't a valid Go duration at apply time", func() {
			createWebhookSecret(ctx, "discord-webhook-bad-retry-interval", "https://discord.com/api/webhooks/12345/abcde")

			feedGroup := &rss2discordv1beta1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-feedgroup-bad-retry-interval",
					Namespace: namespace,
				},
				Spec: rss2discordv1beta1.FeedGroupSpec{
					DiscordWebhookSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "discord-webhook-bad-retry-interval"},
						Key:                  secretURLKey,
					},
					RetryInterval: "five minutes",
					Feeds: []rss2discordv1beta1.FeedSpec{
						{Name: testFeedName(exampleFeedURL), URL: exampleFeedURL},
					},
				},
			}
			Expect(k8sClient.Create(ctx, feedGroup)).To(MatchError(ContainSubstring("spec.retryInterval")))
		})

		It("should reject more than 50 Feeds at apply time", func() {
			createWebhookSecret(ctx, "discord-webhook-too-many-feeds", "https://discord.com/api/webhooks/12345/abcde")

			feeds := make([]rss2discordv1beta1.FeedSpec, 51)
			for i := range feeds {
				feeds[i] = rss2discordv1beta1.FeedSpec{Name: testFeedName(fmt.Sprintf("https://example.com/feed-%d.xml", i)), URL: fmt.Sprintf("https://example.com/feed-%d.xml", i)}
			}

			feedGroup := &rss2discordv1beta1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-feedgroup-too-many-feeds",
					Namespace: namespace,
				},
				Spec: rss2discordv1beta1.FeedGroupSpec{
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
			discordServer, rssServer, reconciler := setUpFeedGroup(ctx, testGroup{name: feedGroupName, secret: "discord-webhook-rate-limited"}, createRSSFeed(
				testEntry{
					title:       "Rate Limited Article",
					description: "Should survive a 429",
					link:        "https://example.com/rate-limited-article",
					pubDate:     time.Now().Format(time.RFC1123Z),
					guid:        "rate-limited-article",
				},
			), func(fg *rss2discordv1beta1.FeedGroup) {
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
			updated := &rss2discordv1beta1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			Expect(feedStatusFor(updated, testFeedName(rssServer.URL())).ETag).To(BeEmpty())
			Expect(feedStatusFor(updated, testFeedName(rssServer.URL())).LastSeenEntry).To(BeEmpty())
			Expect(feedStatusFor(updated, testFeedName(rssServer.URL())).LastError).To(ContainSubstring("rate limited"))

			By("Waiting for the Retry-After cooldown to expire and reconciling again, where the previously unsent entry is delivered")
			time.Sleep(2 * time.Second)
			result, err = reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())
			Expect(discordServer.MessageCount()).To(Equal(1))
			// Normal interval, plus up to 10% jitter.
			Expect(result.RequeueAfter).To(BeNumerically(">=", 30*time.Minute))
			Expect(result.RequeueAfter).To(BeNumerically("<=", 33*time.Minute))

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			Expect(feedStatusFor(updated, testFeedName(rssServer.URL())).ETag).To(Equal(`"v1"`))
			Expect(feedsWithError(updated)).To(Equal(0))
		})

		It("should halt processing later feeds on rate limit and preserve the second feed status", func() {
			const feedGroupName = "test-feedgroup-rate-limit-multi"

			By("Creating mock Discord webhook server and two mock RSS servers")
			discordServer := NewMockDiscordServer(GinkgoTB())
			DeferCleanup(discordServer.Close)

			rssServerA := NewMockRSSServer(createRSSFeed(testEntry{
				title:   "Feed A Entry",
				guid:    "feed-a-1",
				link:    "https://example.com/a-1",
				pubDate: time.Now().Format(time.RFC1123Z),
			}))
			DeferCleanup(rssServerA.Close)
			rssServerA.SetETag(`"etag-a"`)

			rssServerB := NewMockRSSServer(createRSSFeed(testEntry{
				title:   "Feed B Entry",
				guid:    "feed-b-1",
				link:    "https://example.com/b-1",
				pubDate: time.Now().Format(time.RFC1123Z),
			}))
			DeferCleanup(rssServerB.Close)
			rssServerB.SetETag(`"etag-b"`)

			createWebhookSecret(ctx, "secret-rate-limit-multi", discordServer.URL())

			fg := newTestFeedGroup(testGroup{name: feedGroupName, secret: "secret-rate-limit-multi"}, rssServerA.URL(), func(fg *rss2discordv1beta1.FeedGroup) {
				fg.Spec.RetryInterval = "5m"
				fg.Spec.Feeds = append(fg.Spec.Feeds, rss2discordv1beta1.FeedSpec{
					Name: testFeedName(rssServerB.URL()), URL: rssServerB.URL(),
					Message: &rss2discordv1beta1.MessageSpec{Content: tmplTitle},
				})
			})
			Expect(k8sClient.Create(ctx, fg)).To(Succeed())

			reconciler := newTestReconciler(discordServer)

			By("Simulating feed B previously delivered with persisted status and watermark")
			fgCreated := &rss2discordv1beta1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: testNamespace}, fgCreated)).To(Succeed())
			ensureFeedStatuses(fgCreated)
			statusB := feedStatusFor(fgCreated, testFeedName(rssServerB.URL()))
			statusB.LastSeenEntry = "feed-b-0"
			statusB.ETag = `"etag-b-old"`
			tBack := metav1.NewTime(time.Now().Add(-10 * time.Minute))
			statusB.LastChecked = &tBack
			Expect(k8sClient.Status().Update(ctx, fgCreated)).To(Succeed())

			By("Configuring Discord server to rate-limit feed A delivery")
			discordServer.FailNextRequestsRateLimited(1, "7")

			By("Running reconciliation, where feed A hits rate limiting")
			result, err := reconcileFeedGroup(ctx, reconciler, feedGroupName)
			Expect(err).NotTo(HaveOccurred())
			Expect(discordServer.MessageCount()).To(Equal(0))
			Expect(result.RequeueAfter).To(Equal(7 * time.Second))

			By("Asserting feed A status records the rate limit error and did not persist its new ETag")
			updated := &rss2discordv1beta1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: testNamespace}, updated)).To(Succeed())
			statusA := feedStatusFor(updated, testFeedName(rssServerA.URL()))
			Expect(statusA.LastError).To(ContainSubstring("rate limited"))
			Expect(statusA.ETag).To(BeEmpty())

			By("Asserting feed B was untouched by the rate limit on feed A")
			statusBUpdated := feedStatusFor(updated, testFeedName(rssServerB.URL()))
			Expect(statusBUpdated.LastSeenEntry).To(Equal("feed-b-0"))
			Expect(statusBUpdated.ETag).To(Equal(`"etag-b-old"`))
			Expect(statusBUpdated.LastError).To(BeEmpty())
		})
	})

	Describe("FeedGroup validation", func() {
		It("should reject a FeedGroup with duplicate feed URLs", func() {
			feedGroup := &rss2discordv1beta1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{Name: "test-feedgroup-dup-url", Namespace: namespace},
				Spec: rss2discordv1beta1.FeedGroupSpec{
					DiscordWebhookSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: feedGroupValidationSecretName},
						Key:                  secretURLKey,
					},
					Feeds: []rss2discordv1beta1.FeedSpec{
						{Name: "feed-one", URL: exampleFeedURL},
						{Name: "feed-two", URL: exampleFeedURL},
					},
				},
			}
			err := k8sClient.Create(ctx, feedGroup)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("url must be unique"))
		})

		It("should reject messages the schema rules out", func() {
			for _, tc := range []struct {
				name, resourceName string
				groupMessage       *rss2discordv1beta1.MessageSpec
				feedMessage        *rss2discordv1beta1.MessageSpec
				want               string
			}{
				{
					name:         "group message with neither content nor embed",
					resourceName: "test-message-empty",
					groupMessage: &rss2discordv1beta1.MessageSpec{Silent: true},
					want:         "a message needs content, embed, or both",
				},
				{
					name:         "feed message suppressing previews with an embed",
					resourceName: "test-message-suppress",
					feedMessage:  &rss2discordv1beta1.MessageSpec{Embed: &rss2discordv1beta1.EmbedSpec{}, SuppressLinkPreviews: true},
					want:         "suppressLinkPreviews would also hide the embed",
				},
			} {
				feedGroup := &rss2discordv1beta1.FeedGroup{
					ObjectMeta: metav1.ObjectMeta{Name: tc.resourceName, Namespace: namespace},
					Spec: rss2discordv1beta1.FeedGroupSpec{
						DiscordWebhookSecretRef: corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: feedGroupValidationSecretName},
							Key:                  secretURLKey,
						},
						Message: tc.groupMessage,
						Feeds: []rss2discordv1beta1.FeedSpec{{
							Name: testFeedName(exampleFeedURL), URL: exampleFeedURL,
							Message: tc.feedMessage,
						}},
					},
				}
				err := k8sClient.Create(ctx, feedGroup)
				Expect(err).To(HaveOccurred(), tc.name)
				Expect(err.Error()).To(ContainSubstring(tc.want), tc.name)
			}
		})

		It("should accept a complete message", func() {
			feedGroup := &rss2discordv1beta1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{Name: "test-message-complete", Namespace: namespace},
				Spec: rss2discordv1beta1.FeedGroupSpec{
					DiscordWebhookSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: feedGroupValidationSecretName},
						Key:                  secretURLKey,
					},
					Message: &rss2discordv1beta1.MessageSpec{
						Content:  "New: {{.Title}}",
						Mentions: &rss2discordv1beta1.Mentions{RoleIDs: []string{"123"}, UserIDs: []string{"456"}},
						Silent:   true,
						Embed: &rss2discordv1beta1.EmbedSpec{
							Title:      tmplTitle,
							Color:      "#5865F2",
							ImageStyle: rss2discordv1beta1.EmbedImageStyleImage,
							Timestamp:  rss2discordv1beta1.EmbedTimestampNone,
							Author:     &rss2discordv1beta1.EmbedAuthor{Name: "{{.Author}}", URL: "https://example.com", IconURL: "https://example.com/icon.png"},
							Footer:     &rss2discordv1beta1.EmbedFooter{Text: "Example footer", IconURL: "https://example.com/footer.png"},
						},
					},
					Feeds: []rss2discordv1beta1.FeedSpec{
						{Name: "posts", URL: exampleFeedURL, ForumPost: &rss2discordv1beta1.ForumPostSpec{Name: tmplTitle, AppliedTagIDs: []string{"1", "2", "3", "4", "5"}}},
						{Name: "thread", URL: "https://example.com/other.xml", Thread: &rss2discordv1beta1.ThreadTarget{ID: "1234567890"},
							Message: &rss2discordv1beta1.MessageSpec{Content: tmplTitle, SuppressLinkPreviews: true}},
					},
				},
			}
			Expect(k8sClient.Create(ctx, feedGroup)).To(Succeed())
		})

		It("should accept a FeedGroup with distinct feed URLs", func() {
			feedGroup := &rss2discordv1beta1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{Name: "test-feedgroup-distinct-url", Namespace: namespace},
				Spec: rss2discordv1beta1.FeedGroupSpec{
					DiscordWebhookSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: feedGroupValidationSecretName},
						Key:                  secretURLKey,
					},
					Feeds: []rss2discordv1beta1.FeedSpec{
						{Name: testFeedName("https://example.com/a.xml"), URL: "https://example.com/a.xml"},
						{Name: testFeedName("https://example.com/b.xml"), URL: "https://example.com/b.xml"},
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
			feeds := make([]rss2discordv1beta1.FeedSpec, numFeeds)
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
				feeds[i] = rss2discordv1beta1.FeedSpec{Name: testFeedName(rssServers[i].URL()), URL: rssServers[i].URL()}
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
			feedGroup := newTestFeedGroupFeeds(testGroup{name: feedGroupName, secret: "discord-webhook-concurrent"}, feeds, func(fg *rss2discordv1beta1.FeedGroup) {
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

			updated := &rss2discordv1beta1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: namespace}, updated)).To(Succeed())
			Expect(feedsWithError(updated)).To(Equal(0))
			for _, s := range rssServers {
				Expect(feedStatusFor(updated, testFeedName(s.URL())).LastChecked).NotTo(BeNil())
				Expect(feedStatusFor(updated, testFeedName(s.URL())).LastSeenEntry).NotTo(BeEmpty())
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
			count, err := reconcileDurationSampleCountErr(groupKey(namespace, "nonexistent"))
			Expect(err).NotTo(HaveOccurred())
			Expect(count).To(Equal(uint64(0)))
		})
	})

	Describe("Username admission validation", func() {
		// minimalFeedGroup returns a FeedGroup that satisfies every field
		// other than Username, so these tests isolate the CEL rule on
		// Username rather than tripping over unrelated required fields.
		minimalFeedGroup := func(name, username string) *rss2discordv1beta1.FeedGroup {
			return &rss2discordv1beta1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: namespace,
				},
				Spec: rss2discordv1beta1.FeedGroupSpec{
					DiscordWebhookSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: discordWebhookSecretName},
						Key:                  secretURLKey,
					},
					Username: username,
					Feeds: []rss2discordv1beta1.FeedSpec{
						{Name: testFeedName(exampleFeedURL), URL: exampleFeedURL},
					},
				},
			}
		}

		DescribeTable("should reject usernames Discord itself would reject",
			func(username string) {
				err := k8sClient.Create(ctx, minimalFeedGroup("username-rejected", username))
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("Discord rejects usernames"))
			},
			Entry("contains clyde", "ClydeBot"),
			Entry("contains discord", "totally-discord-official"),
			Entry("contains a colon", "News: Daily"),
			Entry("contains a hash", "news#1"),
			Entry("is here", "HERE"),
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
		build := func(name string, mutate func(*rss2discordv1beta1.FeedGroup)) *rss2discordv1beta1.FeedGroup {
			fg := &rss2discordv1beta1.FeedGroup{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec: rss2discordv1beta1.FeedGroupSpec{
					DiscordWebhookSecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: discordWebhookSecretName},
						Key:                  secretURLKey,
					},
					Feeds: []rss2discordv1beta1.FeedSpec{{Name: testFeedName(exampleFeedURL), URL: exampleFeedURL}},
				},
			}
			mutate(fg)
			return fg
		}

		DescribeTable("should reject out-of-range or malformed values",
			func(mutate func(*rss2discordv1beta1.FeedGroup), wantErr string) {
				err := k8sClient.Create(ctx, build("admission-rejected", mutate))
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring(wantErr))
			},
			Entry("negative retries", func(fg *rss2discordv1beta1.FeedGroup) { fg.Spec.Retries = new(int32(-1)) }, "retries"),
			Entry("retries over 100", func(fg *rss2discordv1beta1.FeedGroup) { fg.Spec.Retries = new(int32(101)) }, "retries"),
			Entry("negative catchUpLimit", func(fg *rss2discordv1beta1.FeedGroup) { fg.Spec.CatchUpLimit = new(int32(-1)) }, "catchUpLimit"),
			Entry("catchUpLimit over 100", func(fg *rss2discordv1beta1.FeedGroup) { fg.Spec.CatchUpLimit = new(int32(101)) }, "catchUpLimit"),
			Entry("sub-minute interval", func(fg *rss2discordv1beta1.FeedGroup) { fg.Spec.Interval = "1ns" }, "interval must be at least 1m"),
			Entry("overflowing interval", func(fg *rss2discordv1beta1.FeedGroup) { fg.Spec.Interval = "9999999999999h" }, "interval"),
			Entry("sub-10s retryInterval", func(fg *rss2discordv1beta1.FeedGroup) { fg.Spec.RetryInterval = "1s" }, "retryInterval must be at least 10s"),
			Entry("microsecond-sign unit", func(fg *rss2discordv1beta1.FeedGroup) { fg.Spec.Interval = "100\u00b5s" }, "interval"),
			Entry("non-http avatarURL", func(fg *rss2discordv1beta1.FeedGroup) { fg.Spec.AvatarURL = "ftp://example.com/a.png" }, "avatarURL"),
			Entry("unknown embed imageStyle", func(fg *rss2discordv1beta1.FeedGroup) {
				fg.Spec.Message = &rss2discordv1beta1.MessageSpec{Embed: &rss2discordv1beta1.EmbedSpec{ImageStyle: "banner"}}
			}, "imageStyle"),
			Entry("lowercase embed imageStyle", func(fg *rss2discordv1beta1.FeedGroup) {
				fg.Spec.Message = &rss2discordv1beta1.MessageSpec{Embed: &rss2discordv1beta1.EmbedSpec{ImageStyle: "image"}}
			}, "imageStyle"),
			Entry("unknown embed timestamp", func(fg *rss2discordv1beta1.FeedGroup) {
				fg.Spec.Message = &rss2discordv1beta1.MessageSpec{Embed: &rss2discordv1beta1.EmbedSpec{Timestamp: "Updated"}}
			}, "timestamp"),
			Entry("non-numeric thread id", func(fg *rss2discordv1beta1.FeedGroup) {
				fg.Spec.Feeds[0].Thread = &rss2discordv1beta1.ThreadTarget{ID: "abc"}
			}, "thread.id"),
			Entry("thread and forumPost together", func(fg *rss2discordv1beta1.FeedGroup) {
				fg.Spec.Feeds[0].Thread = &rss2discordv1beta1.ThreadTarget{ID: "1"}
				fg.Spec.Feeds[0].ForumPost = &rss2discordv1beta1.ForumPostSpec{Name: tmplTitle}
			}, "thread and forumPost are mutually exclusive"),
			Entry("too many forum tags", func(fg *rss2discordv1beta1.FeedGroup) {
				fg.Spec.Feeds[0].ForumPost = &rss2discordv1beta1.ForumPostSpec{Name: tmplTitle, AppliedTagIDs: []string{"1", "2", "3", "4", "5", "6"}}
			}, "appliedTagIDs"),
			Entry("non-numeric mention role", func(fg *rss2discordv1beta1.FeedGroup) {
				fg.Spec.Message = &rss2discordv1beta1.MessageSpec{Content: "x", Mentions: &rss2discordv1beta1.Mentions{RoleIDs: []string{"everyone"}}}
			}, "roleIDs"),
			Entry("missing feed name", func(fg *rss2discordv1beta1.FeedGroup) { fg.Spec.Feeds[0].Name = "" }, "name"),
			Entry("invalid feed name", func(fg *rss2discordv1beta1.FeedGroup) { fg.Spec.Feeds[0].Name = "Invalid_Name" }, "name"),
			Entry("duplicate feed name", func(fg *rss2discordv1beta1.FeedGroup) {
				fg.Spec.Feeds = []rss2discordv1beta1.FeedSpec{
					{Name: "dup", URL: "https://a.com/feed.xml"},
					{Name: "dup", URL: "https://b.com/feed.xml"},
				}
			}, "Duplicate value"),
			Entry("duplicate feed url", func(fg *rss2discordv1beta1.FeedGroup) {
				fg.Spec.Feeds = []rss2discordv1beta1.FeedSpec{
					{Name: "a", URL: exampleFeedURL},
					{Name: "b", URL: exampleFeedURL},
				}
			}, "each feed's url must be unique"),
			Entry("feed username contains clyde", func(fg *rss2discordv1beta1.FeedGroup) { fg.Spec.Feeds[0].Username = "clyde" }, "username"),
			Entry("username contains @", func(fg *rss2discordv1beta1.FeedGroup) { fg.Spec.Username = "news@home" }, "username"),
			Entry("username contains a code fence", func(fg *rss2discordv1beta1.FeedGroup) { fg.Spec.Username = "```news" }, "username"),
			Entry("username is everyone", func(fg *rss2discordv1beta1.FeedGroup) { fg.Spec.Feeds[0].Username = "Everyone" }, "username"),
			Entry("non-http embed author url", func(fg *rss2discordv1beta1.FeedGroup) {
				fg.Spec.Message = &rss2discordv1beta1.MessageSpec{Embed: &rss2discordv1beta1.EmbedSpec{Author: &rss2discordv1beta1.EmbedAuthor{Name: "a", URL: nonHTTPURL}}}
			}, "author.url"),
			Entry("non-http embed footer iconURL", func(fg *rss2discordv1beta1.FeedGroup) {
				fg.Spec.Message = &rss2discordv1beta1.MessageSpec{Embed: &rss2discordv1beta1.EmbedSpec{Footer: &rss2discordv1beta1.EmbedFooter{Text: "f", IconURL: nonHTTPURL}}}
			}, "footer.iconURL"),
			Entry("non-http feed avatarURL", func(fg *rss2discordv1beta1.FeedGroup) {
				fg.Spec.Feeds[0].AvatarURL = "ftp://example.com/avatar.png"
			}, "avatarURL"),
		)

		It("should accept values at the boundaries", func() {
			fg := build("admission-ok", func(fg *rss2discordv1beta1.FeedGroup) {
				fg.Spec.Retries = new(int32(100))
				fg.Spec.CatchUpLimit = new(int32(100))
				fg.Spec.Interval = "1m"
				fg.Spec.RetryInterval = "10s"
				fg.Spec.AvatarURL = "https://example.com/a.png"
				fg.Spec.Username = "Everyone's News"
				fg.Spec.Feeds[0].Thread = &rss2discordv1beta1.ThreadTarget{ID: "12345678901234567890"}
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
	if _, ok := obj.(*rss2discordv1beta1.FeedGroup); ok && c.getFeedGroupErr != nil {
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
		_, _, reconciler := setUpFeedGroup(ctx, testGroup{name: feedGroupName, secret: "discord-webhook-apply-error"}, createRSSFeed(
			testEntry{title: "Apply Error", link: "https://example.com/apply-error", guid: "apply-error", pubDate: time.Now().Format(time.RFC1123Z)},
		))
		boom := errors.New("status apply rejected")
		reconciler.Client = &failingClient{Client: k8sClient, statusApplyErr: boom}

		_, err := reconcileFeedGroupNow(ctx, reconciler, feedGroupName)

		Expect(err).To(MatchError(boom))
	})

	It("should report both a feed's panic and the failed status write that followed it", func() {
		const feedGroupName = "test-feedgroup-panic-apply-error"
		_, _, reconciler := setUpFeedGroup(ctx, testGroup{name: feedGroupName, secret: "discord-webhook-panic-apply-error"}, createRSSFeed(
			testEntry{title: "Panic Apply Error", link: "https://example.com/panic-apply-error", guid: "panic-apply-error", pubDate: time.Now().Format(time.RFC1123Z)},
		))
		boom := errors.New("status apply rejected")
		reconciler.Client = &failingClient{Client: k8sClient, statusApplyErr: boom}
		reconciler.DiscordClientBuilder = func(string) MessageSender { return &panicAfterSender{} }

		_, err := reconcileFeedGroupNow(ctx, reconciler, feedGroupName)

		Expect(err).To(MatchError(boom))
		Expect(err).To(MatchError(ContainSubstring("panic while processing feed")))
	})

	It("should suspend reconciliation when Spec.Suspend is true", func() {
		const feedGroupName = "test-feedgroup-suspend"
		_, _, reconciler := setUpFeedGroup(ctx, testGroup{name: feedGroupName, secret: "discord-webhook-suspend"}, createRSSFeed(
			testEntry{title: "Suspended Entry", link: "https://example.com/suspend", guid: "suspend", pubDate: time.Now().Format(time.RFC1123Z)},
		))

		fg := &rss2discordv1beta1.FeedGroup{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: testNamespace}, fg)).To(Succeed())
		fg.Spec.Suspend = true
		Expect(k8sClient.Update(ctx, fg)).To(Succeed())

		res, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: feedGroupName, Namespace: testNamespace}})
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(ctrl.Result{}))

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: testNamespace}, fg)).To(Succeed())
		ready := apimeta.FindStatusCondition(fg.Status.Conditions, rss2discordv1beta1.ConditionTypeReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionUnknown))
		Expect(ready.Reason).To(Equal(reasonSuspended))
		Expect(ready.Message).To(Equal("Reconciliation is suspended for this FeedGroup"))
		Expect(fg.Status.ObservedGeneration).To(Equal(fg.Generation))
	})

	It("should preserve delivery watermark when a named feed's URL changes", func() {
		const feedGroupName = "test-feedgroup-named-url-change"
		discordServer := NewMockDiscordServer(GinkgoTB())
		DeferCleanup(discordServer.Close)
		secretName := "discord-webhook-named-url-change"
		createWebhookSecret(ctx, secretName, discordServer.URL())

		rssServer1 := NewMockRSSServer(createRSSFeed(
			testEntry{title: "Item 1", link: "https://example.com/item1", guid: "item1", pubDate: time.Now().Format(time.RFC1123Z)},
		))
		DeferCleanup(rssServer1.Close)

		fg := newTestFeedGroupFeeds(testGroup{name: feedGroupName, secret: secretName}, []rss2discordv1beta1.FeedSpec{
			{Name: "my-named-feed", URL: rssServer1.URL()},
		})
		Expect(k8sClient.Create(ctx, fg)).To(Succeed())
		reconciler := newTestReconciler(discordServer)

		_, err := reconcileFeedGroupNow(ctx, reconciler, feedGroupName)
		Expect(err).NotTo(HaveOccurred())
		Expect(discordServer.MessageCount()).To(Equal(1))

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: testNamespace}, fg)).To(Succeed())
		Expect(fg.Status.Feeds[0].LastSeenEntry).To(Equal("item1"))

		rssServer2 := NewMockRSSServer(createRSSFeed(
			testEntry{title: "Item 2", link: "https://example.com/item2", guid: "item2", pubDate: time.Now().Format(time.RFC1123Z)},
			testEntry{title: "Item 1", link: "https://example.com/item1", guid: "item1", pubDate: time.Now().Add(-time.Hour).Format(time.RFC1123Z)},
		))
		DeferCleanup(rssServer2.Close)

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: testNamespace}, fg)).To(Succeed())
		fg.Spec.Feeds[0].URL = rssServer2.URL()
		Expect(k8sClient.Update(ctx, fg)).To(Succeed())

		_, err = reconcileFeedGroup(ctx, reconciler, feedGroupName)
		Expect(err).NotTo(HaveOccurred())
		Expect(discordServer.MessageCount()).To(Equal(2))

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: testNamespace}, fg)).To(Succeed())
		Expect(fg.Status.Feeds[0].URL).To(Equal(rssServer2.URL()))
		Expect(fg.Status.Feeds[0].LastSeenEntry).To(Equal("item2"))
	})
})

// panicAfterSender forwards the first after sends to inner, then panics.
type panicAfterSender struct {
	inner       MessageSender
	after, sent int
}

func (p *panicAfterSender) SendMessage(ctx context.Context, msg discord.Message) error {
	if p.sent >= p.after {
		panic(sendExplodedPanic)
	}
	p.sent++
	return p.inner.SendMessage(ctx, msg)
}
