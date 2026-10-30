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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/maverickd650/rss2discord-operator/api/v1alpha1"
)

// feedAttempt is the failure/retry bookkeeping shared by the fetch, render and
// send paths of one feed in one reconcile. It holds only the mechanics; each
// call site decides the policy (whether a failure is permanent, when retries
// count as exhausted, whether to back off or skip), because those differ per
// path.
type feedAttempt struct {
	r     *FeedGroupReconciler
	group *v1alpha1.FeedGroup
	fs    *v1alpha1.FeedStatus
	url   string
}

func (r *FeedGroupReconciler) newFeedAttempt(group *v1alpha1.FeedGroup, fs *v1alpha1.FeedStatus, url string) feedAttempt {
	return feedAttempt{r: r, group: group, fs: fs, url: url}
}

// clearFailure resets the retry state after a success (or a skip).
func (a feedAttempt) clearFailure() {
	clearFailure(a.fs)
}

// clearFailure resets fs's retry state: its last error, its retry count and
// any permanent backoff.
func clearFailure(fs *v1alpha1.FeedStatus) {
	fs.LastError = ""
	fs.RetryCount = 0
	fs.BackoffUntil = ""
}

// fail records one counted failure, in this order: LastError (errText, which
// is clamped), RetryCount, the single-outcome metric, then the condition of
// type condType (carrying the full err.Error()).
func (a feedAttempt) fail(condType, outcome, reason string, err error) {
	a.fs.LastError = errText(err)
	a.fs.RetryCount++
	feedOperationsTotal.WithLabelValues(a.group.Namespace, a.group.Name, a.url, outcome).Inc()
	setFeedCondition(a.fs, condType, metav1.ConditionFalse, reason, err.Error(), a.group.Generation)
}

// announceExhaustion fires the persistent-failure Event only on the reconcile
// RetryCount first equals maxRetries (== rather than >=), so an ongoing
// failure doesn't emit a fresh Event every reconcile.
func (a feedAttempt) announceExhaustion(maxRetries int32, reason string, err error) {
	if a.fs.RetryCount == maxRetries {
		a.r.recordPersistentFailure(a.group, a.url, reason, err)
	}
}

// backOff applies the exponential permanent-failure backoff; see
// applyPermanentBackoff.
func (a feedAttempt) backOff(reason string, err error) {
	a.r.applyPermanentBackoff(a.group, a.fs, a.url, reason, err)
}
