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
	"slices"
	"testing"

	v1beta1 "github.com/maverickd650-labs/rss2discord/api/v1beta1"
	"github.com/maverickd650-labs/rss2discord/internal/rss"
)

func TestCompileEntryFilter(t *testing.T) {
	cases := []struct {
		name     string
		filter   *v1beta1.Filter
		wantRe   bool
		wantExRe bool
		wantErr  bool
	}{
		{name: "nil filter yields nil regex"},
		{name: "blank regex yields nil regex", filter: &v1beta1.Filter{Regex: "  "}},
		{name: "valid regex compiled", filter: &v1beta1.Filter{Regex: "foo.*"}, wantRe: true},
		{name: "invalid regex returns error", filter: &v1beta1.Filter{Regex: "("}, wantErr: true},
		{name: "valid excludeRegex compiled", filter: &v1beta1.Filter{ExcludeRegex: "bar.*"}, wantExRe: true},
		{name: "invalid excludeRegex returns error", filter: &v1beta1.Filter{ExcludeRegex: "["}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := compileEntryFilter(tc.filter)
			if (err != nil) != tc.wantErr || (f.regex != nil) != tc.wantRe || (f.excludeRegex != nil) != tc.wantExRe {
				t.Fatalf("got regex %v, exRegex %v, err %v; want regex=%v exRegex=%v err=%v", f.regex, f.excludeRegex, err, tc.wantRe, tc.wantExRe, tc.wantErr)
			}
		})
	}
}

const (
	keywordBreaking = "breaking"
	keywordHappened = "happened"
)

func TestEntryFilterMatches(t *testing.T) {
	entry := rss.Entry{Title: "Breaking News", Description: "something happened"}

	t.Run("zero-value and nil filter match", func(t *testing.T) {
		var zero entryFilter
		f, _ := compileEntryFilter(nil)
		if !zero.matches(entry) || !f.matches(entry) {
			t.Fatal("expected zero-value and nil filter to match everything")
		}
	})

	t.Run("regex non-match rejects", func(t *testing.T) {
		f, _ := compileEntryFilter(&v1beta1.Filter{Regex: "nomatch"})
		if f.matches(entry) {
			t.Fatal("expected non-matching regex to reject entry")
		}
	})

	t.Run("keyword case-insensitive match", func(t *testing.T) {
		f, _ := compileEntryFilter(&v1beta1.Filter{Keywords: []string{"BREAKING"}})
		if !f.matches(entry) {
			t.Fatal("expected case-insensitive keyword match")
		}
	})

	t.Run("blank keywords skipped while non-blank keyword matches description", func(t *testing.T) {
		f, _ := compileEntryFilter(&v1beta1.Filter{Keywords: []string{"  ", "  HAPPENED "}})
		if !f.matches(entry) {
			t.Fatal("expected non-blank keyword to match after skipping blanks")
		}
	})

	t.Run("no keyword match rejects", func(t *testing.T) {
		f, _ := compileEntryFilter(&v1beta1.Filter{Keywords: []string{"weather"}})
		if f.matches(entry) {
			t.Fatal("expected no keyword match to reject")
		}
	})

	t.Run("all-blank keywords match nothing even when regex matches", func(t *testing.T) {
		f, _ := compileEntryFilter(&v1beta1.Filter{Regex: "Breaking", Keywords: []string{"  ", "\t"}})
		if f.matches(entry) {
			t.Fatal("expected all-blank keywords to match nothing")
		}
	})

	// Negative filtering tests (#388)
	t.Run("exclusion without positive filters passes non-matching", func(t *testing.T) {
		f, err := compileEntryFilter(&v1beta1.Filter{ExcludeKeywords: []string{"sponsored"}})
		if err != nil {
			t.Fatal(err)
		}
		if !f.matches(entry) {
			t.Fatal("expected entry to pass when exclude keywords do not match")
		}
	})

	t.Run("exclude keyword in title rejects", func(t *testing.T) {
		f, err := compileEntryFilter(&v1beta1.Filter{ExcludeKeywords: []string{keywordBreaking}})
		if err != nil {
			t.Fatal(err)
		}
		if f.matches(entry) {
			t.Fatal("expected entry to be rejected by exclude keyword in title")
		}
	})

	t.Run("exclude keyword in description rejects", func(t *testing.T) {
		f, err := compileEntryFilter(&v1beta1.Filter{ExcludeKeywords: []string{keywordHappened}})
		if err != nil {
			t.Fatal(err)
		}
		if f.matches(entry) {
			t.Fatal("expected entry to be rejected by exclude keyword in description")
		}
	})

	t.Run("multiple exclude keywords where one matches rejects", func(t *testing.T) {
		f, err := compileEntryFilter(&v1beta1.Filter{ExcludeKeywords: []string{"promo", keywordHappened, "ad"}})
		if err != nil {
			t.Fatal(err)
		}
		if f.matches(entry) {
			t.Fatal("expected entry to be rejected when at least one exclude keyword matches")
		}
	})

	t.Run("exclude regex case-sensitive does not match different case", func(t *testing.T) {
		f, err := compileEntryFilter(&v1beta1.Filter{ExcludeRegex: keywordBreaking}) // lowercase without (?i)
		if err != nil {
			t.Fatal(err)
		}
		// entry.Title is "Breaking News"
		if !f.matches(entry) {
			t.Fatal("expected case-sensitive exclude regex not to match different case")
		}
	})

	t.Run("exclude regex with (?i) rejects", func(t *testing.T) {
		f, err := compileEntryFilter(&v1beta1.Filter{ExcludeRegex: `(?i)` + keywordBreaking})
		if err != nil {
			t.Fatal(err)
		}
		if f.matches(entry) {
			t.Fatal("expected (?i) exclude regex to reject entry")
		}
	})

	t.Run("combined positive match and negative veto", func(t *testing.T) {
		// Matches positive keyword "breaking", but vetoed by excludeKeyword "happened"
		f, err := compileEntryFilter(&v1beta1.Filter{
			Keywords:        []string{keywordBreaking},
			ExcludeKeywords: []string{keywordHappened},
		})
		if err != nil {
			t.Fatal(err)
		}
		if f.matches(entry) {
			t.Fatal("expected entry to be vetoed despite matching positive keyword")
		}
	})

	t.Run("combined positive match passes when negative does not match", func(t *testing.T) {
		f, err := compileEntryFilter(&v1beta1.Filter{
			Keywords:        []string{keywordBreaking},
			ExcludeKeywords: []string{"digest"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if !f.matches(entry) {
			t.Fatal("expected entry to pass positive criteria and negative veto check")
		}
	})
}

func TestCompileEntryFilter_NormalizesKeywordsOnce(t *testing.T) {
	f, err := compileEntryFilter(&v1beta1.Filter{
		Keywords:        []string{"  BREAKING ", " ", "News"},
		ExcludeKeywords: []string{"  PROMO ", " ", "Ad "},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !f.hasKeywords {
		t.Fatal("expected configured keywords to be recorded")
	}
	if want := []string{"breaking", "news"}; !slices.Equal(f.keywords, want) {
		t.Fatalf("keywords = %q, want %q", f.keywords, want)
	}
	if wantEx := []string{"promo", "ad"}; !slices.Equal(f.excludeKeywords, wantEx) {
		t.Fatalf("excludeKeywords = %q, want %q", f.excludeKeywords, wantEx)
	}

	blankOnly, err := compileEntryFilter(&v1beta1.Filter{
		Keywords:        []string{" ", "\t"},
		ExcludeKeywords: []string{" ", "\t"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !blankOnly.hasKeywords || len(blankOnly.keywords) != 0 {
		t.Fatalf("blank-only keywords produced hasKeywords=%v keywords=%q, want true and empty", blankOnly.hasKeywords, blankOnly.keywords)
	}
	if len(blankOnly.excludeKeywords) != 0 {
		t.Fatalf("blank-only excludeKeywords produced %q, want empty", blankOnly.excludeKeywords)
	}
}

func TestEntryFilter_EmptyKeywordsDoNotRestrict(t *testing.T) {
	for _, keywords := range [][]string{nil, {}} {
		f, err := compileEntryFilter(&v1beta1.Filter{Keywords: keywords})
		if err != nil {
			t.Fatal(err)
		}
		if f.hasKeywords || !f.matches(rss.Entry{Title: "anything"}) {
			t.Errorf("keywords %q unexpectedly restricted the entry", keywords)
		}
	}
}
