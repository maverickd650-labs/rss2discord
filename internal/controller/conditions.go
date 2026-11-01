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

// Condition reasons and Event reasons/actions that are not part of the
// failure classification table (classify.go keeps the HTTP/network reasons it
// maps errors to). Every value is user-visible API -- it appears in
// `kubectl get feedgroup -o yaml`, in Events and in dashboards and alerts --
// so changing one is a breaking change.

// Group-level Ready and FeedReachable condition reasons.
const (
	reasonWebhookUnresolved = "WebhookUnresolved"
	// reasonReconcilePanicked is the Ready reason for a reconcile a feed
	// panicked in (see processFeed): the feeds after it were not processed.
	reasonReconcilePanicked = "ReconcilePanicked"
	reasonReconciled        = "Reconciled"
	reasonFeedErrors        = "FeedErrors"
	reasonAllFeedsReachable = "AllFeedsReachable"
	// reasonInvalidInterval is the Ready condition reason (and Warning Event
	// reason) for a spec.interval/spec.retryInterval that time.ParseDuration
	// rejects.
	reasonInvalidInterval = "InvalidInterval"
)

// Per-feed Reachable and Delivered condition reasons.
const (
	reasonFetchSucceeded = "FetchSucceeded"
	reasonMessageSent    = "MessageSent"

	// reasonConfigError and reasonRenderError are FeedConditionTypeDelivered
	// reasons for failures that have no HTTP status to classify: a
	// compile-time filter/template error (deterministic FeedSpec
	// misconfiguration) vs. a per-entry template execution failure.
	reasonConfigError = "ConfigError"
	reasonRenderError = "RenderError"
	// reasonEntrySkipped marks a Delivered condition left behind by an entry
	// given up on after exhausting retries (see attemptEntrySkipped).
	reasonEntrySkipped = "EntrySkipped"
	// reasonBacklogSkipped marks a Delivered condition left behind by a first
	// sync with catchUpLimit: 0, which sends nothing and starts from the
	// newest entry (see attemptBacklogSkipped).
	reasonBacklogSkipped = "BacklogSkipped"
)

// Warning Event reasons for a failed attempt (the persistent-failure Events a
// feedAttempt fires), and the Event actions.
const (
	eventReasonFetchFailed  = "FetchFailed"
	eventReasonRenderFailed = "RenderFailed"
	eventReasonSendFailed   = "SendFailed"

	eventActionSkippedEntry     = "SkippedEntry"
	eventActionInvalidConfig    = "InvalidConfig"
	eventActionRetriesExhausted = "RetriesExhausted"
	eventActionBackoffCapped    = "BackoffCapped"
)
