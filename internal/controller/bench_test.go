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
	"strings"
	"testing"
)

// Entry identity runs several times per entry on every reconcile whose feed
// isn't served a 304, so these guard its per-call cost. Run with
// `go test -run '^$' -bench . -benchmem ./internal/controller`.

func BenchmarkNormalizeIdentity(b *testing.B) {
	cases := map[string]string{
		"opaque_guid":  "tag:example.com,2026:entry-1234567890",
		"url":          "https://Example.com/post/1?utm_source=feed&id=7#top",
		"url_no_query": "https://example.com/post/1",
	}
	for name, id := range cases {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				normalizeIdentity(id)
			}
		})
	}
}

func BenchmarkTruncateMessage(b *testing.B) {
	fits := strings.Repeat("x", 500)
	over := strings.Repeat("x", maxDiscordMessageLength*2)
	b.Run("fits", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			truncateMessage(fits, maxDiscordMessageLength)
		}
	})
	b.Run("overflows", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			truncateMessage(over, maxDiscordMessageLength)
		}
	})
}
