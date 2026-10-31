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

	v1alpha1 "github.com/maverickd650/rss2discord-operator/api/v1alpha1"
	"github.com/maverickd650/rss2discord-operator/internal/rss"
)

func TestCompileEntryFilter(t *testing.T) {
	cases := []struct {
		name    string
		filter  *v1alpha1.Filter
		wantRe  bool
		wantErr bool
	}{
		{name: "nil filter yields nil regex"},
		{name: "blank regex yields nil regex", filter: &v1alpha1.Filter{Regex: "  "}},
		{name: "valid regex compiled", filter: &v1alpha1.Filter{Regex: "foo.*"}, wantRe: true},
		{name: "invalid regex returns error", filter: &v1alpha1.Filter{Regex: "("}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := compileEntryFilter(tc.filter)
			if (err != nil) != tc.wantErr || (f.regex != nil) != tc.wantRe {
				t.Fatalf("got regex %v, err %v; want regex=%v err=%v", f.regex, err, tc.wantRe, tc.wantErr)
			}
		})
	}
}

func TestEntryFilterMatches(t *testing.T) {
	entry := rss.Entry{Title: "Breaking News", Description: "something happened"}

	t.Run("nil filter matches", func(t *testing.T) {
		f, _ := compileEntryFilter(nil)
		if !f.matches(entry) {
			t.Fatal("expected nil filter to match everything")
		}
	})

	t.Run("regex non-match rejects", func(t *testing.T) {
		f, _ := compileEntryFilter(&v1alpha1.Filter{Regex: "nomatch"})
		if f.matches(entry) {
			t.Fatal("expected non-matching regex to reject entry")
		}
	})

	t.Run("keyword case-insensitive match", func(t *testing.T) {
		f, _ := compileEntryFilter(&v1alpha1.Filter{Keywords: []string{"BREAKING"}})
		if !f.matches(entry) {
			t.Fatal("expected case-insensitive keyword match")
		}
	})

	t.Run("no keyword match rejects", func(t *testing.T) {
		f, _ := compileEntryFilter(&v1alpha1.Filter{Keywords: []string{"weather"}})
		if f.matches(entry) {
			t.Fatal("expected no keyword match to reject")
		}
	})

	t.Run("blank keywords skipped, empty keyword list matches", func(t *testing.T) {
		f, _ := compileEntryFilter(&v1alpha1.Filter{Keywords: []string{"  "}})
		// All keywords are blank/skipped, so none match.
		if f.matches(entry) {
			t.Fatal("expected all-blank keywords to match nothing")
		}
	})
}
