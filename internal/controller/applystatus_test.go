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
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/maverickd650-labs/rss2discord/api/v1alpha1"
	acv1alpha1 "github.com/maverickd650-labs/rss2discord/api/v1alpha1/applyconfiguration/api/v1alpha1"
)

// TestStatusApplyConfigurationRoundTrip checks that converting a status into
// its ApplyConfiguration produces the exact JSON the typed status would have
// sent, both fully populated and empty -- so a field the apply configuration
// can't carry (e.g. it wasn't regenerated after a FeedGroupStatus change)
// shows up as a diff here instead of being silently dropped from every
// status write.
func TestStatusApplyConfigurationRoundTrip(t *testing.T) {
	transition := metav1.NewTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	full := v1alpha1.FeedGroupStatus{
		ObservedGeneration: 7,
		Conditions: []metav1.Condition{{
			Type:               v1alpha1.ConditionTypeReady,
			Status:             metav1.ConditionFalse,
			Reason:             "FeedErrors",
			Message:            "1 feed(s) reporting errors",
			ObservedGeneration: 7,
			LastTransitionTime: transition,
		}},
		Feeds: []v1alpha1.FeedStatus{{
			RSSUrl:        "https://example.com/feed.xml",
			LastChecked:   "2026-01-02T03:04:05Z",
			Initialized:   true,
			LastSeenEntry: "https://example.com/post",
			LastSent:      map[string]string{"abc123": "2026-01-02T03:04:05Z"},
			LastError:     "feed fetch failed: 404 Not Found",
			ETag:          `"etag"`,
			LastModified:  "Fri, 02 Jan 2026 03:04:05 GMT",
			RetryCount:    3,
			BackoffUntil:  "2026-01-02T04:04:05Z",
			Conditions: []metav1.Condition{{
				Type:               v1alpha1.FeedConditionTypeReachable,
				Status:             metav1.ConditionFalse,
				Reason:             "HTTP404",
				Message:            "not found",
				ObservedGeneration: 7,
				LastTransitionTime: transition,
			}},
		}},
	}

	for name, status := range map[string]v1alpha1.FeedGroupStatus{"full": full, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			ac, err := statusApplyConfiguration(&status)
			if err != nil {
				t.Fatalf("statusApplyConfiguration: %v", err)
			}
			want, err := json.Marshal(status)
			if err != nil {
				t.Fatal(err)
			}
			got, err := json.Marshal(ac)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) {
				t.Errorf("apply configuration JSON differs from typed status JSON:\n got: %s\nwant: %s", got, want)
			}
		})
	}
}

// TestStatusApplyConfigurationCoversEveryField guards the round-trip test
// above against its own fixture going stale: a field added to
// FeedGroupStatus/FeedStatus but not to that fixture would round-trip as
// absent on both sides and pass unnoticed. Comparing the json field names of
// each typed struct against its generated ApplyConfiguration catches a
// missing regeneration regardless of what the fixture sets.
func TestStatusApplyConfigurationCoversEveryField(t *testing.T) {
	pairs := []struct{ typed, ac any }{
		{v1alpha1.FeedGroupStatus{}, acv1alpha1.FeedGroupStatusApplyConfiguration{}},
		{v1alpha1.FeedStatus{}, acv1alpha1.FeedStatusApplyConfiguration{}},
	}
	for _, p := range pairs {
		typed, ac := jsonFieldNames(p.typed), jsonFieldNames(p.ac)
		if !slices.Equal(typed, ac) {
			t.Errorf("%T json fields %v != %T json fields %v; re-run `mise run generate`",
				p.typed, typed, p.ac, ac)
		}
	}
}

func jsonFieldNames(v any) []string {
	rt := reflect.TypeOf(v)
	names := make([]string, 0, rt.NumField())
	for f := range rt.Fields() {
		if name, _, _ := strings.Cut(f.Tag.Get("json"), ","); name != "" && name != "-" {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}
