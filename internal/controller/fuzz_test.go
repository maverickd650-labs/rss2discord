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
	neturl "net/url"
	"strings"
	"testing"

	"unicode/utf8"

	"github.com/maverickd650/rss2discord-operator/internal/discord"
)

// FuzzControllerSanitizers exercises the two pure text-sanitizing helpers
// feed entry content passes through before becoming a Discord message:
// httpURLOrEmpty (strips any non-http(s) scheme -- javascript:/data: URIs
// included) and discord.Truncate (Discord's message length clamp). Neither
// touches the network or the SSRF/webhook-host guards in internal/rss or
// internal/discord (see CLAUDE.md) -- this fuzz target is deliberately
// scoped to the pure helpers only, per ground rule 1 in
// docs/plans/issue-106-testing-infra.md.
func FuzzControllerSanitizers(f *testing.F) {
	seeds := []string{
		"",
		"https://example.com/feed",
		"http://example.com",
		jsAlertURL,
		"data:text/html,<script>alert(1)</script>",
		"ftp://example.com",
		"not a url",
		"@everyone",
		"@here",
		"\x00\x00\x00",
		"\xff\xfe\x80", // invalid UTF-8 byte sequence
		strings.Repeat("x", discord.MaxContentLength*2),
		strings.Repeat("\U0001F600", discord.MaxContentLength), // 4-byte emoji straddling the clamp boundary
		"é́́",    // base char with combining acute accents
		"日本語テスト", // multibyte (Japanese) text
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, input string) {
		if got := httpURLOrEmpty(input); got != "" {
			if got != input {
				t.Fatalf("httpURLOrEmpty(%q) = %q, want either %q unchanged or empty", input, got, input)
			}
			parsed, err := neturl.Parse(got)
			if err != nil {
				t.Fatalf("httpURLOrEmpty(%q) = %q, which does not itself parse: %v", input, got, err)
			}
			if parsed.Scheme != schemeHTTP && parsed.Scheme != schemeHTTPS {
				t.Fatalf("httpURLOrEmpty(%q) = %q, unexpected scheme %q survived", input, got, parsed.Scheme)
			}
		}

		truncated, overflow := discord.Truncate(input, discord.MaxContentLength)
		if got := len([]rune(truncated)); got > discord.MaxContentLength {
			t.Fatalf("discord.Truncate(%q, %d) returned %d runes, want <= %d", input, discord.MaxContentLength, got, discord.MaxContentLength)
		}
		wantOverflow := max(len([]rune(input))-discord.MaxContentLength, 0)
		if overflow != wantOverflow {
			t.Fatalf("discord.Truncate(%q, %d) overflow = %d, want %d", input, discord.MaxContentLength, overflow, wantOverflow)
		}
		// discord.Truncate
		// passes its input through unchanged when no truncation is needed
		// (overflow == 0) rather than round-tripping it through []rune, so
		// UTF-8 validity is only guaranteed for the branch that actually
		// truncates (fuzz-generated strings, unlike literals, aren't
		// required to be valid UTF-8 to begin with).
		if overflow > 0 && !utf8.ValidString(truncated) {
			t.Fatalf("discord.Truncate(%q, %d) produced invalid UTF-8: %q", input, discord.MaxContentLength, truncated)
		}
	})
}

// FuzzStatusClamps checks the invariants the bounded status fields rely on:
// clampText never exceeds its byte budget or splits a UTF-8 sequence, and
// clampWatermark always fits LastSeenEntry and is stable (the same identity
// maps to the same stored value on every reconcile, and re-clamping a stored
// value is a no-op -- otherwise the watermark would never match).
func FuzzStatusClamps(f *testing.F) {
	f.Add("", 16)
	f.Add("short", 16)
	f.Add(strings.Repeat("é", 100), 51) // 2-byte runes, odd budget forces a mid-rune backoff
	f.Add(strings.Repeat("\U0001F600", 50), 17)
	f.Add("\xff\xfe\x80"+strings.Repeat("x", 100), 20)
	f.Add("https://example.com/"+strings.Repeat("a", 3000), maxStatusTextBytes)

	f.Fuzz(func(t *testing.T, input string, budget int) {
		// clampText is only ever called with the package's positive budgets;
		// room for the 3-byte ellipsis is its documented minimum.
		budget = 3 + int(uint(budget)%5000)

		got := clampText(input, budget)
		if len(got) > budget {
			t.Fatalf("clampText(%q, %d) = %d bytes, over budget", input, budget, len(got))
		}
		if utf8.ValidString(input) && !utf8.ValidString(got) {
			t.Fatalf("clampText(%q, %d) split a UTF-8 sequence: %q", input, budget, got)
		}
		if len(input) <= budget && got != input {
			t.Fatalf("clampText(%q, %d) altered input that already fit: %q", input, budget, got)
		}

		mark := clampWatermark(input)
		if len(mark) > maxStatusTextBytes {
			t.Fatalf("clampWatermark(%q) = %d bytes, over the %d limit", input, len(mark), maxStatusTextBytes)
		}
		if again := clampWatermark(mark); again != mark {
			t.Fatalf("clampWatermark is not idempotent: %q -> %q", mark, again)
		}
		if clampWatermark(input) != mark {
			t.Fatalf("clampWatermark(%q) is not deterministic", input)
		}
	})
}
