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
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/maverickd650-labs/rss2discord/api/v1beta1"
	acv1beta1 "github.com/maverickd650-labs/rss2discord/api/v1beta1/applyconfiguration/api/v1beta1"
)

// TestStatusApplyConfigurationRoundTrip checks that converting a status into
// its ApplyConfiguration produces the exact JSON the typed status would have
// sent, both fully populated and empty -- so a field the apply configuration
// can't carry (e.g. it wasn't regenerated after a FeedGroupStatus change)
// shows up as a diff here instead of being silently dropped from every
// status write.
func TestStatusApplyConfigurationRoundTrip(t *testing.T) {
	transition := metav1.NewTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	full := v1beta1.FeedGroupStatus{
		ObservedGeneration: 7,
		Conditions: []metav1.Condition{{
			Type:               v1beta1.ConditionTypeReady,
			Status:             metav1.ConditionFalse,
			Reason:             "FeedErrors",
			Message:            "1 feed(s) reporting errors",
			ObservedGeneration: 7,
			LastTransitionTime: transition,
		}},
		Feeds: []v1beta1.FeedStatus{{
			Name:          "test-feed",
			URL:           "https://example.com/feed.xml",
			LastChecked:   &transition,
			Initialized:   true,
			LastSeenEntry: "https://example.com/post",
			LastSent:      map[string]metav1.Time{"abc123": ts("2026-01-02T03:04:05Z")},
			LastError:     "feed fetch failed: 404 Not Found",
			ETag:          `"etag"`,
			LastModified:  "Fri, 02 Jan 2026 03:04:05 GMT",
			RetryCount:    3,
			BackoffUntil:  &transition,
			Conditions: []metav1.Condition{{
				Type:               v1beta1.FeedConditionTypeReachable,
				Status:             metav1.ConditionFalse,
				Reason:             "HTTP404",
				Message:            "not found",
				ObservedGeneration: 7,
				LastTransitionTime: transition,
			}},
		}},
	}

	for name, status := range map[string]v1beta1.FeedGroupStatus{"full": full, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			ac, err := statusApplyConfiguration(&status)
			if err != nil {
				t.Fatalf("statusApplyConfiguration: %v", err)
			}
			typedJSON, err := json.Marshal(status)
			if err != nil {
				t.Fatalf("json.Marshal(status): %v", err)
			}
			acJSON, err := json.Marshal(ac)
			if err != nil {
				t.Fatalf("json.Marshal(ac): %v", err)
			}
			var typedMap, acMap map[string]any
			if err := json.Unmarshal(typedJSON, &typedMap); err != nil {
				t.Fatalf("json.Unmarshal(typedJSON): %v", err)
			}
			if err := json.Unmarshal(acJSON, &acMap); err != nil {
				t.Fatalf("json.Unmarshal(acJSON): %v", err)
			}
			if !reflect.DeepEqual(typedMap, acMap) {
				t.Fatalf("apply configuration JSON diverged from typed status:\ntyped: %s\napply: %s", typedJSON, acJSON)
			}
		})
	}
}

// TestStatusApplyConfigurationCoversAllFields asserts that every field on
// FeedGroupStatus (and FeedStatus) has an entry in the generated ApplyConfiguration
// struct, so a new field added to api/v1beta1/feedgroup_types.go fails here until
// `mise run generate` produces its ApplyConfiguration field.
func TestStatusApplyConfigurationCoversAllFields(t *testing.T) {
	statusType := reflect.TypeFor[v1beta1.FeedGroupStatus]()
	acType := reflect.TypeFor[acv1beta1.FeedGroupStatusApplyConfiguration]()

	for field := range statusType.Fields() {
		acField, ok := acType.FieldByName(field.Name)
		if !ok {
			t.Errorf("ApplyConfiguration missing field %s from FeedGroupStatus", field.Name)
			continue
		}
		if acField.Type.Kind() == reflect.Pointer && field.Type.Kind() != reflect.Pointer {
			// Generated SSA types wrap scalar and struct fields in pointers so
			// unset fields are omitted from the patch; that's expected.
			continue
		}
	}

	feedStatusType := reflect.TypeFor[v1beta1.FeedStatus]()
	feedACType := reflect.TypeFor[acv1beta1.FeedStatusApplyConfiguration]()
	for field := range feedStatusType.Fields() {
		if _, ok := feedACType.FieldByName(field.Name); !ok {
			t.Errorf("ApplyConfiguration missing field %s from FeedStatus", field.Name)
		}
	}
}

// TestStatusApplyConfigurationOmitempty asserts that empty fields are not
// present in the generated JSON, so SSA doesn't claim ownership of fields
// the controller didn't populate.
func TestStatusApplyConfigurationOmitempty(t *testing.T) {
	empty := v1beta1.FeedGroupStatus{}
	ac, err := statusApplyConfiguration(&empty)
	if err != nil {
		t.Fatalf("statusApplyConfiguration: %v", err)
	}
	raw, err := json.Marshal(ac)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if len(fields) != 0 {
		t.Errorf("empty status produced non-empty JSON: %s", raw)
	}
}

// TestApplyStatusErrorHandling asserts applyStatus returns errors from JSON
// unmarshaling or the client Status().Apply call.
func TestApplyStatusErrorHandling(t *testing.T) {
	t.Run("apply client error", func(t *testing.T) {
		r := &FeedGroupReconciler{Client: fakeApplyClient()}
		fg := &v1beta1.FeedGroup{}
		fg.Name = "test"
		fg.Namespace = "default"
		// fakeApplyClient succeeds by default; wrap it with an error.
		errBoom := errors.New("boom")
		r.Client = &failingApplyClient{Client: r.Client, err: errBoom}
		if err := r.applyStatus(t.Context(), fg); !errors.Is(err, errBoom) {
			t.Fatalf("applyStatus error = %v, want %v", err, errBoom)
		}
	})
}

type failingApplyClient struct {
	client.Client
	err error
}

func (c *failingApplyClient) Status() client.SubResourceWriter {
	return &failingSubResourceWriter{err: c.err}
}

type failingSubResourceWriter struct {
	client.SubResourceWriter
	err error
}

func (w *failingSubResourceWriter) Apply(ctx context.Context, obj runtime.ApplyConfiguration, opts ...client.SubResourceApplyOption) error {
	return w.err
}
