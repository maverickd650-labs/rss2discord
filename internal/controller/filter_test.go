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
	"strings"
	"testing"

	v1beta1 "github.com/maverickd650-labs/rss2discord/api/v1beta1"
	"github.com/maverickd650-labs/rss2discord/internal/rss"
)

func TestCompileEntryFilter(t *testing.T) {
	cases := []struct {
		name               string
		filter             *v1beta1.Filter
		wantInRe, wantExRe bool
		wantErr            string
	}{
		{name: "nil filter"},
		{name: "empty filter", filter: &v1beta1.Filter{}},
		{name: "blank regex yields nil regex", filter: &v1beta1.Filter{Include: &v1beta1.FilterRules{Regex: "  "}}},
		{name: "include regex compiled", filter: &v1beta1.Filter{Include: &v1beta1.FilterRules{Regex: "foo.*"}}, wantInRe: true},
		{name: "invalid include regex", filter: &v1beta1.Filter{Include: &v1beta1.FilterRules{Regex: "("}}, wantErr: "include"},
		{name: "exclude regex compiled", filter: &v1beta1.Filter{Exclude: &v1beta1.FilterRules{Regex: "bar.*"}}, wantExRe: true},
		{name: "invalid exclude regex", filter: &v1beta1.Filter{Exclude: &v1beta1.FilterRules{Regex: "["}}, wantErr: "exclude"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := compileEntryFilter(tc.filter)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one naming %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if gotIn := f.include != nil && f.include.regex != nil; gotIn != tc.wantInRe {
				t.Fatalf("include regex compiled = %v, want %v", gotIn, tc.wantInRe)
			}
			if gotEx := f.exclude != nil && f.exclude.regex != nil; gotEx != tc.wantExRe {
				t.Fatalf("exclude regex compiled = %v, want %v", gotEx, tc.wantExRe)
			}
		})
	}
}

const (
	keywordBreaking = "breaking"
	keywordHappened = "happened"
	titleNews       = "News"
)

func TestEntryFilterMatches(t *testing.T) {
	entry := rss.Entry{
		Title:       "Breaking News",
		Description: "something happened",
		Categories:  []string{"<b>Security</b>", "Releases"},
	}
	rules := func(r v1beta1.FilterRules) *v1beta1.FilterRules { return &r }

	cases := []struct {
		name   string
		filter *v1beta1.Filter
		want   bool
	}{
		{name: "nil filter matches", want: true},
		{name: "empty filter matches", filter: &v1beta1.Filter{}, want: true},

		{name: "include regex match", filter: &v1beta1.Filter{Include: rules(v1beta1.FilterRules{Regex: "Breaking"})}, want: true},
		{name: "include regex is case-sensitive", filter: &v1beta1.Filter{Include: rules(v1beta1.FilterRules{Regex: keywordBreaking})}},
		{name: "include regex (?i)", filter: &v1beta1.Filter{Include: rules(v1beta1.FilterRules{Regex: "(?i)breaking"})}, want: true},
		{name: "include keyword case-insensitive", filter: &v1beta1.Filter{Include: rules(v1beta1.FilterRules{Keywords: []string{"BREAKING"}})}, want: true},
		{name: "include keyword in description", filter: &v1beta1.Filter{Include: rules(v1beta1.FilterRules{Keywords: []string{"  ", " HAPPENED "}})}, want: true},
		{name: "include keyword no match", filter: &v1beta1.Filter{Include: rules(v1beta1.FilterRules{Keywords: []string{"weather"}})}},
		{name: "include all-blank keywords match nothing", filter: &v1beta1.Filter{Include: rules(v1beta1.FilterRules{Regex: "Breaking", Keywords: []string{" ", "\t"}})}},
		{name: "include empty keywords do not restrict", filter: &v1beta1.Filter{Include: rules(v1beta1.FilterRules{Keywords: []string{}, Regex: titleNews})}, want: true},
		{name: "include category exact, case-insensitive, HTML-stripped", filter: &v1beta1.Filter{Include: rules(v1beta1.FilterRules{Categories: []string{" security "}})}, want: true},
		{name: "include category is not a substring match", filter: &v1beta1.Filter{Include: rules(v1beta1.FilterRules{Categories: []string{"secure"}})}},
		{name: "include all-blank categories match nothing", filter: &v1beta1.Filter{Include: rules(v1beta1.FilterRules{Categories: []string{" "}})}},
		{name: "include criteria are ANDed: all match", filter: &v1beta1.Filter{Include: rules(v1beta1.FilterRules{Regex: titleNews, Keywords: []string{keywordHappened}, Categories: []string{"releases"}})}, want: true},
		{name: "include criteria are ANDed: one fails", filter: &v1beta1.Filter{Include: rules(v1beta1.FilterRules{Regex: titleNews, Keywords: []string{keywordHappened}, Categories: []string{"weather"}})}},

		{name: "exclude no match passes", filter: &v1beta1.Filter{Exclude: rules(v1beta1.FilterRules{Keywords: []string{"sponsored"}})}, want: true},
		{name: "exclude keyword in title", filter: &v1beta1.Filter{Exclude: rules(v1beta1.FilterRules{Keywords: []string{keywordBreaking}})}},
		{name: "exclude keyword in description", filter: &v1beta1.Filter{Exclude: rules(v1beta1.FilterRules{Keywords: []string{"promo", keywordHappened}})}},
		{name: "exclude regex is case-sensitive", filter: &v1beta1.Filter{Exclude: rules(v1beta1.FilterRules{Regex: keywordBreaking})}, want: true},
		{name: "exclude regex (?i)", filter: &v1beta1.Filter{Exclude: rules(v1beta1.FilterRules{Regex: "(?i)breaking"})}},
		{name: "exclude category", filter: &v1beta1.Filter{Exclude: rules(v1beta1.FilterRules{Categories: []string{"RELEASES"}})}},
		{name: "exclude criteria are ORed", filter: &v1beta1.Filter{Exclude: rules(v1beta1.FilterRules{Regex: "nomatch", Keywords: []string{"nomatch"}, Categories: []string{"security"}})}},
		{name: "exclude all-blank keywords exclude nothing", filter: &v1beta1.Filter{Exclude: rules(v1beta1.FilterRules{Keywords: []string{" "}, Categories: []string{" "}})}, want: true},

		{name: "exclude vetoes an include match", filter: &v1beta1.Filter{
			Include: rules(v1beta1.FilterRules{Keywords: []string{keywordBreaking}}),
			Exclude: rules(v1beta1.FilterRules{Keywords: []string{keywordHappened}}),
		}},
		{name: "include match passes an exclude miss", filter: &v1beta1.Filter{
			Include: rules(v1beta1.FilterRules{Keywords: []string{keywordBreaking}}),
			Exclude: rules(v1beta1.FilterRules{Keywords: []string{"digest"}}),
		}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := compileEntryFilter(tc.filter)
			if err != nil {
				t.Fatal(err)
			}
			if got := f.matches(entry); got != tc.want {
				t.Fatalf("matches = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("zero value matches", func(t *testing.T) {
		if !(entryFilter{}).matches(entry) {
			t.Fatal("zero-value filter rejected the entry")
		}
	})
}

func TestCompileEntryFilter_NormalizesTermsOnce(t *testing.T) {
	f, err := compileEntryFilter(&v1beta1.Filter{
		Include: &v1beta1.FilterRules{Keywords: []string{"  BREAKING ", " ", "News"}, Categories: []string{" Go ", ""}},
		Exclude: &v1beta1.FilterRules{Keywords: []string{" ", "\t"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !f.include.hasKeywords || !slices.Equal(f.include.keywords, []string{"breaking", "news"}) {
		t.Fatalf("include keywords = %v %q", f.include.hasKeywords, f.include.keywords)
	}
	if !f.include.hasCategories || !slices.Equal(f.include.categories, []string{"go"}) {
		t.Fatalf("include categories = %v %q", f.include.hasCategories, f.include.categories)
	}
	if !f.exclude.hasKeywords || len(f.exclude.keywords) != 0 {
		t.Fatalf("blank-only exclude keywords = %v %q, want true and empty", f.exclude.hasKeywords, f.exclude.keywords)
	}
}
