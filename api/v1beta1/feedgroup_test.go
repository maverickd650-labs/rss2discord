package v1beta1

import (
	"encoding/json"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Explicit zero must survive typed clients' JSON encoding; omission lets
// admission apply the default, while zero requests that the backlog be skipped.
func TestFeedGroupSpec_CatchUpLimitJSON(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit *int32
	}{
		{name: "omitted"},
		{name: "explicit zero", limit: new(int32(0))},
		{name: "upper bound", limit: new(int32(100))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := FeedGroupSpec{CatchUpLimit: tc.limit}
			encoded, err := json.Marshal(original)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &fields); err != nil {
				t.Fatal(err)
			}
			if _, present := fields["catchUpLimit"]; present != (tc.limit != nil) {
				t.Fatalf("catchUpLimit presence changed: %s", encoded)
			}
			var decoded FeedGroupSpec
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatal(err)
			}
			if tc.limit == nil {
				if decoded.CatchUpLimit != nil {
					t.Fatalf("omitted limit decoded as %v", decoded.CatchUpLimit)
				}
			} else if decoded.CatchUpLimit == nil || *decoded.CatchUpLimit != *tc.limit {
				t.Fatalf("catchUpLimit did not round trip: %s", encoded)
			}
		})
	}
}

func TestFeedGroup_DeepCopyCatchUpLimit(t *testing.T) {
	for _, limit := range []*int32{nil, new(int32(0)), new(int32(100))} {
		original := &FeedGroup{Spec: FeedGroupSpec{CatchUpLimit: limit}}
		copied := original.DeepCopy()
		if limit == nil {
			if copied.Spec.CatchUpLimit != nil {
				t.Fatal("DeepCopy changed an omitted catchUpLimit")
			}
			continue
		}
		if copied.Spec.CatchUpLimit == nil || copied.Spec.CatchUpLimit == limit || *copied.Spec.CatchUpLimit != *limit {
			t.Fatal("DeepCopy must preserve the limit through an independent pointer")
		}
		before := *limit
		*copied.Spec.CatchUpLimit = 42
		if *limit != before {
			t.Fatal("mutating the copied limit changed the original")
		}
	}
}

func TestFeedGroup_DeepCopyTimestamps(t *testing.T) {
	now := metav1.NewTime(time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC))
	original := &FeedGroup{
		Status: FeedGroupStatus{
			Feeds: []FeedStatus{
				{
					Name:         "example",
					URL:          "https://example.com/feed.xml",
					LastChecked:  &now,
					BackoffUntil: &now,
				},
			},
		},
	}
	copied := original.DeepCopy()
	if copied.Status.Feeds[0].LastChecked == original.Status.Feeds[0].LastChecked {
		t.Fatal("DeepCopy aliased LastChecked pointer")
	}
	if !copied.Status.Feeds[0].LastChecked.Equal(original.Status.Feeds[0].LastChecked) {
		t.Fatal("DeepCopy changed LastChecked value")
	}
	if copied.Status.Feeds[0].BackoffUntil == original.Status.Feeds[0].BackoffUntil {
		t.Fatal("DeepCopy aliased BackoffUntil pointer")
	}
	if !copied.Status.Feeds[0].BackoffUntil.Equal(original.Status.Feeds[0].BackoffUntil) {
		t.Fatal("DeepCopy changed BackoffUntil value")
	}
}
