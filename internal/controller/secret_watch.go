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
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/maverickd650-labs/rss2discord/api/v1alpha1"
)

// feedGroupsForSecret maps a webhook Secret event to the FeedGroups in its
// namespace that reference it. A namespace holds few FeedGroups, so a cached
// List filtered in memory is cheaper than maintaining a field index.
func (r *FeedGroupReconciler) feedGroupsForSecret(ctx context.Context, secret client.Object) []reconcile.Request {
	var groups v1alpha1.FeedGroupList
	if err := r.List(ctx, &groups, client.InNamespace(secret.GetNamespace())); err != nil {
		logf.FromContext(ctx).Error(err, "failed to list FeedGroups for a webhook Secret change",
			"secret", types.NamespacedName{Namespace: secret.GetNamespace(), Name: secret.GetName()})
		return nil
	}
	var requests []reconcile.Request
	for i := range groups.Items {
		if groups.Items[i].Spec.DiscordWebhookSecretRef.Name == secret.GetName() {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&groups.Items[i])})
		}
	}
	return requests
}

// noteWebhookSecretVersion records the webhook Secret's resourceVersion and,
// when it differs from the one recorded before, clears the backoff of every
// feed backed off after a permanent send failure: the most likely fix for a
// deleted or regenerated webhook is replacing the URL in its Secret, which
// doesn't bump the FeedGroup's generation. The first version recorded (an
// existing FeedGroup on its first reconcile by a release that tracks it)
// counts as a change too: the Secret may have been fixed before the upgrade,
// and the cost is at most one extra attempt per send-backed-off feed, once.
func noteWebhookSecretVersion(feedGroup *v1alpha1.FeedGroup, resourceVersion string) {
	previous := feedGroup.Status.WebhookSecretResourceVersion
	feedGroup.Status.WebhookSecretResourceVersion = resourceVersion
	if previous == resourceVersion {
		return
	}
	clearSendBackoffs(feedGroup)
}

// clearSendBackoffs clears the backoff (see retryRun.clear) of feeds whose
// backoff came from a send failure (retryRun.sendCaused). A feed backed off
// because its own URL fails to fetch keeps its backoff: a new webhook can't
// fix that.
func clearSendBackoffs(feedGroup *v1alpha1.FeedGroup) {
	for i := range feedGroup.Status.Feeds {
		if b := runOf(&feedGroup.Status.Feeds[i]); b.sendCaused() {
			b.clear()
		}
	}
}

// StripMetadataOnlyAnnotations is a cache transform that drops annotations and
// managedFields from metadata-only objects. The only metadata-only informer is
// the webhook Secret watch, and a Secret created with `kubectl apply` carries
// its full data in the last-applied-configuration annotation, which would
// otherwise sit in the cache despite the watch being metadata only.
func StripMetadataOnlyAnnotations() toolscache.TransformFunc {
	return func(in any) (any, error) {
		if obj, ok := in.(*metav1.PartialObjectMetadata); ok {
			obj.SetAnnotations(nil)
			obj.SetManagedFields(nil)
		}
		return in, nil
	}
}

// WatchNamespaces turns the --watch-namespaces flag (comma-separated, blanks
// and duplicates ignored) into the cache's DefaultNamespaces, scoping the
// FeedGroup and webhook Secret watches alike. Empty input returns nil, which
// watches every namespace. This only narrows what the operator watches; the
// matching RBAC (a Role per namespace) is what narrows what it may read.
func WatchNamespaces(flagValue string) map[string]cache.Config {
	var namespaces map[string]cache.Config
	for ns := range strings.SplitSeq(flagValue, ",") {
		if ns = strings.TrimSpace(ns); ns == "" {
			continue
		}
		if namespaces == nil {
			namespaces = map[string]cache.Config{}
		}
		namespaces[ns] = cache.Config{}
	}
	return namespaces
}
