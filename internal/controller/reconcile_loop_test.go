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
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	rss2discordv1alpha1 "github.com/maverickd650-labs/rss2discord/api/v1alpha1"
)

// These specs run the real controller under a manager (every other spec calls
// Reconcile directly), because the thing under test is the event wiring in
// SetupWithManager: the controller's own status writes must not re-trigger it.
var _ = Describe("Reconcile triggering under a running manager", func() {
	const loopNamespace = "reconcile-loop"

	It("should not re-reconcile a failing feed in a hot loop off its own status writes", func() {
		By("Starting a manager scoped to a private namespace, with a feed server that always fails")
		var requests atomic.Int32
		rssServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		}))
		DeferCleanup(rssServer.Close)

		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: loopNamespace}})).To(Succeed())
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:  k8sClient.Scheme(),
			Metrics: metricsserver.Options{BindAddress: "0"},
			Cache:   cache.Options{DefaultNamespaces: map[string]cache.Config{loopNamespace: {}}},
			// "feedgroup" is already registered by the SetupWithManager spec.
			Controller: config.Controller{SkipNameValidation: new(true)},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect((&FeedGroupReconciler{
			Client:    mgr.GetClient(),
			Scheme:    mgr.GetScheme(),
			RSSClient: testRSSClient(),
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

		By("Creating a FeedGroup whose only feed returns HTTP 500 (a transient, retryable failure)")
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "webhook", Namespace: loopNamespace},
			Data:       map[string][]byte{secretURLKey: []byte("https://discord.com/api/webhooks/12345/abcde")},
		})).To(Succeed())
		fg := newTestFeedGroup("failing-feed", "webhook", rssServer.URL, withRetries("10s", 3))
		fg.Namespace = loopNamespace
		Expect(k8sClient.Create(ctx, fg)).To(Succeed())

		By("Waiting for the first attempt to be recorded in status")
		Eventually(func() int32 {
			var got rss2discordv1alpha1.FeedGroup
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: fg.Name, Namespace: loopNamespace}, &got); err != nil {
				return 0
			}
			if fs := feedStatusFor(&got, rssServer.URL); fs != nil {
				return fs.RetryCount
			}
			return 0
		}, 10*time.Second, 100*time.Millisecond).Should(BeNumerically(">=", 1))

		By("Verifying it is not retried again until RetryInterval (10s) -- a status-write feedback loop would fire dozens of times")
		Consistently(requests.Load, 3*time.Second, 100*time.Millisecond).Should(BeNumerically("<=", 2))
	})
})
