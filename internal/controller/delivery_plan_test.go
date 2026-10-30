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
	"fmt"
	"slices"
	"testing"

	"github.com/maverickd650/rss2discord-operator/internal/rss"
)

func planEntries(n int) []rss.Entry {
	entries := make([]rss.Entry, n)
	for i := range entries {
		entries[i] = rss.Entry{ID: fmt.Sprintf("e%d", i)}
	}
	return entries
}

func TestPlanDelivery(t *testing.T) {
	const big = defaultCatchUpLimit + 5
	all := planEntries(big)
	few := planEntries(3)
	tail := func(e []rss.Entry, n int) []rss.Entry { return e[len(e)-n:] }

	for _, tc := range []struct {
		name string
		in   deliveryInputs
		want deliveryPlan
	}{
		{
			name: "watermark found resumes after it",
			in:   deliveryInputs{entries: all, hadWatermark: true, found: true, watermarkIndex: 4, catchUpLimit: 2},
			want: deliveryPlan{scan: all, resumeAt: 5},
		},
		{
			name: "watermark found ignores catchUpLimit 0, ambiguity and initialization",
			in: deliveryInputs{entries: few, hadWatermark: true, found: true, watermarkIndex: 0,
				catchUpLimit: 0, legacyAmbiguous: true},
			want: deliveryPlan{scan: few, resumeAt: 1},
		},
		{
			name: "watermark at newest entry leaves nothing past it",
			in:   deliveryInputs{entries: few, hadWatermark: true, found: true, watermarkIndex: 2, catchUpLimit: 5, wasInitialized: true},
			want: deliveryPlan{scan: few, resumeAt: 3},
		},
		{
			name: "watermark not found, limit above zero caps catch-up",
			in:   deliveryInputs{entries: all, hadWatermark: true, catchUpLimit: 2, wasInitialized: true},
			want: deliveryPlan{scan: tail(all, 2)},
		},
		{
			name: "watermark not found, limit above feed size keeps all",
			in:   deliveryInputs{entries: few, hadWatermark: true, catchUpLimit: 10},
			want: deliveryPlan{scan: few},
		},
		{
			name: "watermark not found, limit 0 falls back to the default cap",
			in:   deliveryInputs{entries: all, hadWatermark: true, catchUpLimit: 0, wasInitialized: true},
			want: deliveryPlan{scan: tail(all, defaultCatchUpLimit)},
		},
		{
			name: "watermark not found, limit 0, not initialized is not a first sync",
			in:   deliveryInputs{entries: all, hadWatermark: true, catchUpLimit: 0},
			want: deliveryPlan{scan: tail(all, defaultCatchUpLimit)},
		},
		{
			name: "watermark not found, limit 0, ambiguous legacy is not capped as legacy",
			in:   deliveryInputs{entries: all, hadWatermark: true, catchUpLimit: 0, legacyAmbiguous: true},
			want: deliveryPlan{scan: tail(all, defaultCatchUpLimit)},
		},
		{
			name: "no watermark, limit above zero caps catch-up",
			in:   deliveryInputs{entries: all, catchUpLimit: 3},
			want: deliveryPlan{scan: tail(all, 3)},
		},
		{
			name: "no watermark, limit above zero ignores ambiguity and initialization",
			in:   deliveryInputs{entries: all, catchUpLimit: 3, legacyAmbiguous: true, wasInitialized: true},
			want: deliveryPlan{scan: tail(all, 3)},
		},
		{
			name: "no watermark, limit 0, first sync skips the backlog",
			in:   deliveryInputs{entries: all, catchUpLimit: 0},
			want: deliveryPlan{skipBacklog: true},
		},
		{
			name: "no watermark, limit 0, already initialized delivers everything",
			in:   deliveryInputs{entries: all, catchUpLimit: 0, wasInitialized: true},
			want: deliveryPlan{scan: all},
		},
		{
			name: "no watermark, limit 0, ambiguous legacy is capped at the default",
			in:   deliveryInputs{entries: all, catchUpLimit: 0, legacyAmbiguous: true},
			want: deliveryPlan{scan: tail(all, defaultCatchUpLimit), cappedAmbiguousLegacy: true},
		},
		{
			name: "no watermark, limit 0, ambiguous legacy wins over initialized",
			in:   deliveryInputs{entries: all, catchUpLimit: 0, legacyAmbiguous: true, wasInitialized: true},
			want: deliveryPlan{scan: tail(all, defaultCatchUpLimit), cappedAmbiguousLegacy: true},
		},
		{
			name: "no watermark, limit 0, ambiguous legacy, small feed keeps all but still signals",
			in:   deliveryInputs{entries: few, catchUpLimit: 0, legacyAmbiguous: true},
			want: deliveryPlan{scan: few, cappedAmbiguousLegacy: true},
		},
		{
			name: "no entries, limit 0, first sync still plans a skip",
			in:   deliveryInputs{catchUpLimit: 0},
			want: deliveryPlan{skipBacklog: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := planDelivery(tc.in)
			if got.resumeAt != tc.want.resumeAt || got.skipBacklog != tc.want.skipBacklog ||
				got.cappedAmbiguousLegacy != tc.want.cappedAmbiguousLegacy ||
				!slices.EqualFunc(got.scan, tc.want.scan, func(a, b rss.Entry) bool { return a.ID == b.ID }) {
				t.Fatalf("planDelivery = %+v, want %+v", got, tc.want)
			}
		})
	}
}
