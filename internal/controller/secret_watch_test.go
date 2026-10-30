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
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/maverickd650/rss2discord-operator/api/v1alpha1"
	"github.com/maverickd650/rss2discord-operator/internal/discord"
)

// testSecretName names the Secret in the tests that don't need a real one.
const testSecretName = "webhook-url"

// sendBackedOff and fetchBackedOff are feeds in permanent backoff after a
// send failure (fetch still Reachable) and after a fetch failure.
func sendBackedOff(url string) v1alpha1.FeedStatus {
	return v1alpha1.FeedStatus{
		RSSUrl: url, BackoffUntil: "2026-01-01T18:00:00Z", RetryCount: 3,
		Conditions: []metav1.Condition{{Type: v1alpha1.FeedConditionTypeReachable, Status: metav1.ConditionTrue}},
	}
}

func fetchBackedOff(url string) v1alpha1.FeedStatus {
	return v1alpha1.FeedStatus{
		RSSUrl: url, BackoffUntil: "2026-01-01T18:00:00Z", RetryCount: 3,
		Conditions: []metav1.Condition{{Type: v1alpha1.FeedConditionTypeReachable, Status: metav1.ConditionFalse}},
	}
}

func TestNoteWebhookSecretVersion(t *testing.T) {
	tests := []struct {
		name        string
		recorded    string
		observed    string
		wantCleared bool
	}{
		{"first observation only records", "", "100", false},
		{"unchanged Secret keeps backoffs", "100", "100", false},
		{"changed Secret clears send backoffs", "100", "101", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fg := &v1alpha1.FeedGroup{Status: v1alpha1.FeedGroupStatus{
				WebhookSecretResourceVersion: tc.recorded,
				Feeds:                        []v1alpha1.FeedStatus{sendBackedOff("send"), fetchBackedOff("fetch")},
			}}
			noteWebhookSecretVersion(fg, tc.observed)

			if got := fg.Status.WebhookSecretResourceVersion; got != tc.observed {
				t.Errorf("recorded version = %q, want %q", got, tc.observed)
			}
			send, fetch := fg.Status.Feeds[0], fg.Status.Feeds[1]
			if cleared := send.BackoffUntil == "" && send.RetryCount == 0; cleared != tc.wantCleared {
				t.Errorf("send backoff cleared = %v, want %v (%+v)", cleared, tc.wantCleared, send)
			}
			if fetch.BackoffUntil == "" || fetch.RetryCount != 3 {
				t.Errorf("a fetch-failure backoff must survive a Secret change: %+v", fetch)
			}
		})
	}
}

func TestStripMetadataOnlyAnnotations(t *testing.T) {
	transform := StripMetadataOnlyAnnotations()

	meta := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{
		Name:          testSecretName,
		Labels:        map[string]string{"app": "x"},
		Annotations:   map[string]string{"kubectl.kubernetes.io/last-applied-configuration": `{"data":{"url":"c2VjcmV0"}}`},
		ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubectl"}},
	}}
	if _, err := transform(meta); err != nil {
		t.Fatal(err)
	}
	if meta.Annotations != nil || meta.ManagedFields != nil {
		t.Errorf("metadata-only object kept annotations/managedFields: %+v", meta.ObjectMeta)
	}
	if meta.Name != testSecretName || meta.Labels["app"] != "x" {
		t.Errorf("name/labels must survive: %+v", meta.ObjectMeta)
	}

	full := &v1alpha1.FeedGroup{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"keep": "me"}}}
	if _, err := transform(full); err != nil {
		t.Fatal(err)
	}
	if full.Annotations["keep"] != "me" {
		t.Error("typed objects must be left untouched")
	}
}

// TestFeedGroupsForSecretListFailure pins that a failed FeedGroup List maps
// the Secret event to nothing rather than panicking: the dropped wake-up is
// made up by the FeedGroup's normal RequeueAfter.
func TestFeedGroupsForSecretListFailure(t *testing.T) {
	failing := interceptor.NewClient(fake.NewClientBuilder().Build(), interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("cache not synced")
		},
	})
	r := &FeedGroupReconciler{Client: failing}
	secret := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: testSecretName, Namespace: testNamespace}}
	if got := r.feedGroupsForSecret(context.Background(), secret); got != nil {
		t.Errorf("requests = %v, want none when listing FeedGroups fails", got)
	}
}

func TestWatchNamespaces(t *testing.T) {
	if got := WatchNamespaces(" , "); got != nil {
		t.Errorf("blank flag = %v, want nil (every namespace)", got)
	}
	got := WatchNamespaces("team-a, team-b,,team-a")
	if len(got) != 2 {
		t.Fatalf("got %v, want exactly team-a and team-b", got)
	}
	for _, ns := range []string{"team-a", "team-b"} {
		if _, ok := got[ns]; !ok {
			t.Errorf("missing %q in %v", ns, got)
		}
	}
}

var _ = Describe("Webhook Secret watch", func() {
	It("maps a Secret to the FeedGroups in its namespace that reference it", func() {
		const (
			watched     = "secret-watch-a"
			referencesA = "secret-watch-a1"
			alsoA       = "secret-watch-a2"
			referencesB = "secret-watch-b"
		)
		createWebhookSecret(ctx, watched, "https://discord.com/api/webhooks/1/a")
		for name, secret := range map[string]string{
			referencesA: watched, alsoA: watched, referencesB: "secret-watch-other",
		} {
			Expect(k8sClient.Create(ctx, newTestFeedGroup(name, secret, exampleFeedURL))).To(Succeed())
		}

		r := &FeedGroupReconciler{Client: k8sClient}
		secret := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: watched, Namespace: testNamespace}}
		requests := r.feedGroupsForSecret(ctx, secret)
		names := make([]string, 0, len(requests))
		for _, req := range requests {
			Expect(req.Namespace).To(Equal(testNamespace))
			names = append(names, req.Name)
		}
		Expect(names).To(ConsistOf(referencesA, alsoA))
	})

	It("retries a feed backed off after a permanent send failure once its webhook Secret changes", func() {
		const feedGroupName = "test-feedgroup-secret-rotation"
		const secretName = "discord-webhook-secret-rotation"
		discordServer, _, r := setUpFeedGroup(ctx, feedGroupName, secretName, createRSSFeed(testEntry{
			title:   "Delivered After Rotation",
			link:    "https://example.com/delivered-after-rotation",
			pubDate: time.Now().Format(time.RFC1123Z),
			guid:    "delivered-after-rotation",
		}), withRetries("5m", 3))
		discordServer.FailNextRequests(100, http.StatusNotFound)

		feedStatus := func() v1alpha1.FeedStatus {
			GinkgoHelper()
			fg := &v1alpha1.FeedGroup{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: feedGroupName, Namespace: testNamespace}, fg)).To(Succeed())
			return fg.Status.Feeds[0]
		}

		By("Backing off after the webhook 404s")
		_, err := reconcileFeedGroup(ctx, r, feedGroupName)
		Expect(err).NotTo(HaveOccurred())
		Expect(feedStatus().BackoffUntil).NotTo(BeEmpty())
		attempts := discordServer.DeliveryAttempts()

		By("Staying backed off while the Secret is unchanged")
		_, err = reconcileFeedGroup(ctx, r, feedGroupName)
		Expect(err).NotTo(HaveOccurred())
		Expect(feedStatus().BackoffUntil).NotTo(BeEmpty())
		Expect(discordServer.DeliveryAttempts()).To(Equal(attempts))

		By("Replacing the webhook URL in the Secret")
		discordServer.FailNextRequests(0, 0)
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: secretName, Namespace: testNamespace}, secret)).To(Succeed())
		secret.Data[secretURLKey] = []byte(discordServer.URL() + "\n")
		Expect(k8sClient.Update(ctx, secret)).To(Succeed())

		By("Retrying immediately and delivering the entry")
		_, err = reconcileFeedGroupNow(ctx, r, feedGroupName)
		Expect(err).NotTo(HaveOccurred())
		fs := feedStatus()
		Expect(fs.BackoffUntil).To(BeEmpty())
		Expect(fs.RetryCount).To(BeZero())
		Expect(discordServer.MessageCount()).To(Equal(1))
	})

	It("is woken by a Secret change under a running manager, with the production cache settings", func() {
		const watchNamespace = "secret-watch"
		By("Starting a manager configured like cmd/main.go, scoped to a private namespace")
		// Atomics rather than MockDiscordServer: here the handler runs on the
		// server's goroutines while the spec flips and reads its state.
		var failing atomic.Bool
		var delivered atomic.Int32
		failing.Store(true)
		discordServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if failing.Load() {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			delivered.Add(1)
			w.WriteHeader(http.StatusNoContent)
		}))
		host := discordServer.Listener.Addr().(*net.TCPAddr).IP.String()
		discord.AllowedWebhookHosts[host] = true
		DeferCleanup(func() {
			delete(discord.AllowedWebhookHosts, host)
			discordServer.Close()
		})
		rssServer := NewMockRSSServer(createRSSFeed(testEntry{
			title:   "Delivered After Rotation Under A Manager",
			link:    "https://example.com/delivered-after-rotation-manager",
			pubDate: time.Now().Format(time.RFC1123Z),
			guid:    "delivered-after-rotation-manager",
		}))
		DeferCleanup(rssServer.Close)

		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: watchNamespace}})).To(Succeed())
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:  k8sClient.Scheme(),
			Metrics: metricsserver.Options{BindAddress: "0"},
			Client: client.Options{Cache: &client.CacheOptions{
				DisableFor: []client.Object{&corev1.Secret{}},
			}},
			Cache: cache.Options{
				DefaultNamespaces: map[string]cache.Config{watchNamespace: {}},
				DefaultTransform:  StripMetadataOnlyAnnotations(),
			},
			Controller: config.Controller{SkipNameValidation: new(true)},
		})
		Expect(err).NotTo(HaveOccurred())
		httpClient := discordServer.Client()
		Expect((&FeedGroupReconciler{
			Client:    mgr.GetClient(),
			Scheme:    mgr.GetScheme(),
			RSSClient: testRSSClient(),
			DiscordClientBuilder: func(webhookURL string) *discord.Client {
				return discord.NewClientWithHTTP(webhookURL, httpClient)
			},
		}).SetupWithManager(mgr)).To(Succeed())

		mgrCtx, stop := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer GinkgoRecover()
			defer close(done)
			Expect(mgr.Start(mgrCtx)).To(Succeed())
		}()
		DeferCleanup(func() {
			stop()
			Eventually(done).Should(BeClosed())
		})

		By("Creating a FeedGroup whose webhook 404s, so it backs off for well over the spec's runtime")
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "rotated-webhook", Namespace: watchNamespace},
			Data:       map[string][]byte{secretURLKey: []byte(discordServer.URL)},
		}
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())
		fg := newTestFeedGroup("rotated-webhook-feed", secret.Name, rssServer.URL(), withRetries("5m", 3))
		fg.Namespace = watchNamespace
		Expect(k8sClient.Create(ctx, fg)).To(Succeed())

		feedStatus := func() v1alpha1.FeedStatus {
			var got v1alpha1.FeedGroup
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(fg), &got); err != nil || len(got.Status.Feeds) == 0 {
				return v1alpha1.FeedStatus{}
			}
			return got.Status.Feeds[0]
		}
		Eventually(func() string { return feedStatus().BackoffUntil }, 10*time.Second, 100*time.Millisecond).ShouldNot(BeEmpty())
		Expect(delivered.Load()).To(BeZero())

		By("Replacing the webhook URL in the Secret, which the watch should notice")
		failing.Store(false)
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(secret), secret)).To(Succeed())
		secret.Data[secretURLKey] = []byte(discordServer.URL + "\n")
		Expect(k8sClient.Update(ctx, secret)).To(Succeed())

		Eventually(delivered.Load, 10*time.Second, 100*time.Millisecond).Should(BeNumerically("==", 1))
		Eventually(func() string { return feedStatus().BackoffUntil }, 10*time.Second, 100*time.Millisecond).Should(BeEmpty())
	})
})
