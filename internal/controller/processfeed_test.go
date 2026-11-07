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
	"time"

	v1beta1 "github.com/maverickd650-labs/rss2discord/api/v1beta1"
)

// testReconcileTime is the fixed "now" string handed to processFeed in tests.
const testReconcileTime = "2026-01-01T00:00:00Z"

// testReconcileInstant is testReconcileTime as the time processFeed runs at.
var testReconcileInstant = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// feedUnderTest is the FeedGroup and the one feed of it a processFeed test
// drives, kept together so helpers take them as one argument.
type feedUnderTest struct {
	fg   *v1beta1.FeedGroup
	feed v1beta1.FeedSpec
}
