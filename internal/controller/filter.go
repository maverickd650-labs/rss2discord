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
	"regexp"
	"slices"
	"strings"

	"github.com/maverickd650-labs/rss2discord/api/v1beta1"
	"github.com/maverickd650-labs/rss2discord/internal/rss"
)

// entryFilter is a feed's filter compiled once per feed per reconcile. Regexes
// and keywords are normalized here rather than for every entry. hasKeywords
// distinguishes an absent keyword restriction from a configured list whose
// entries are all blank (which deliberately matches nothing). The zero value
// matches every entry.
type entryFilter struct {
	regex           *regexp.Regexp
	keywords        []string
	hasKeywords     bool
	excludeRegex    *regexp.Regexp
	excludeKeywords []string
}

// compileEntryFilter compiles filter's regexes and normalizes its keywords
// (trimmed, lowercased, blanks dropped). A nil filter or a blank regex compiles
// to no regex; an invalid regex is an error.
func compileEntryFilter(filter *v1beta1.Filter) (entryFilter, error) {
	if filter == nil {
		return entryFilter{}, nil
	}

	var f entryFilter
	if strings.TrimSpace(filter.Regex) != "" {
		re, err := regexp.Compile(filter.Regex)
		if err != nil {
			return entryFilter{}, fmt.Errorf("invalid filter regex %q: %w", filter.Regex, err)
		}
		f.regex = re
	}

	f.hasKeywords = len(filter.Keywords) > 0
	for _, keyword := range filter.Keywords {
		if keyword = strings.ToLower(strings.TrimSpace(keyword)); keyword != "" {
			f.keywords = append(f.keywords, keyword)
		}
	}

	if strings.TrimSpace(filter.ExcludeRegex) != "" {
		re, err := regexp.Compile(filter.ExcludeRegex)
		if err != nil {
			return entryFilter{}, fmt.Errorf("invalid filter excludeRegex %q: %w", filter.ExcludeRegex, err)
		}
		f.excludeRegex = re
	}

	for _, keyword := range filter.ExcludeKeywords {
		if keyword = strings.ToLower(strings.TrimSpace(keyword)); keyword != "" {
			f.excludeKeywords = append(f.excludeKeywords, keyword)
		}
	}

	return f, nil
}

// matches reports whether entry passes the filter:
// 1. Any matching excludeRegex or excludeKeywords vetoes the entry (returns false).
// 2. If positive criteria are configured (regex or keywords), the entry must match them.
// The zero value matches every entry.
func (f entryFilter) matches(entry rss.Entry) bool {
	content := entry.Title + "\n" + entry.Description

	if f.excludeRegex != nil && f.excludeRegex.MatchString(content) {
		return false
	}
	if len(f.excludeKeywords) > 0 {
		contentLower := strings.ToLower(strings.TrimSpace(content))
		if slices.ContainsFunc(f.excludeKeywords, func(keyword string) bool {
			return strings.Contains(contentLower, keyword)
		}) {
			return false
		}
	}

	if f.hasKeywords && len(f.keywords) == 0 {
		return false
	}
	if f.regex == nil && !f.hasKeywords {
		return true
	}

	if f.regex != nil && !f.regex.MatchString(content) {
		return false
	}
	if !f.hasKeywords {
		return true
	}

	contentLower := strings.ToLower(strings.TrimSpace(content))
	return slices.ContainsFunc(f.keywords, func(keyword string) bool {
		return strings.Contains(contentLower, keyword)
	})
}
