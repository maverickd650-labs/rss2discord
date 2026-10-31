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
	"testing"

	"github.com/maverickd650/rss2discord-operator/internal/rss"
)

func TestEntryIdentity_NormalizesTrackingParamsOnURLIdentity(t *testing.T) {
	withTracking := rss.Entry{ID: "https://example.com/article?utm_source=feed&fbclid=abc&id=42"}
	clean := rss.Entry{ID: "https://example.com/article?id=42"}

	if entryIdentity(withTracking) != entryIdentity(clean) {
		t.Fatalf("expected tracking params to be stripped: got %q vs %q",
			entryIdentity(withTracking), entryIdentity(clean))
	}
}

func TestEntryIdentity_PreservesNonURLGUIDUnchanged(t *testing.T) {
	entry := rss.Entry{ID: "tag:example.com,2026:article-123"}
	if got := entryIdentity(entry); got != entry.ID {
		t.Fatalf("expected opaque GUID to pass through unchanged, got %q", got)
	}
}

// URL-looking IDs that don't parse, or parse to a non-http(s) scheme, are
// returned unchanged rather than normalized.
func TestNormalizeIdentity_UnparsableOrNonHTTPURLUnchanged(t *testing.T) {
	for _, id := range []string{"http://[::1", "httpx://example.com/a#frag", "http"} {
		if got := normalizeIdentity(id); got != id {
			t.Errorf("normalizeIdentity(%q) = %q, want unchanged", id, got)
		}
	}
}
