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
	"strings"

	"github.com/maverickd650-labs/rss2discord/api/v1alpha1"
	"github.com/maverickd650-labs/rss2discord/internal/rss"
)

// entryFilter is a feed's filter compiled once per feed per reconcile: the
// spec together with its compiled regex, so the two can't be mismatched and
// the regex isn't recompiled for every entry. The zero value (and the result of
// compiling a nil filter) matches every entry.
type entryFilter struct {
	spec  *v1alpha1.Filter
	regex *regexp.Regexp
}

// compileEntryFilter compiles filter. A nil filter or a blank regex compiles
// to no regex; an invalid regex is an error.
func compileEntryFilter(filter *v1alpha1.Filter) (entryFilter, error) {
	f := entryFilter{spec: filter}
	if filter == nil || strings.TrimSpace(filter.Regex) == "" {
		return f, nil
	}

	re, err := regexp.Compile(filter.Regex)
	if err != nil {
		return entryFilter{}, fmt.Errorf("invalid filter regex %q: %w", filter.Regex, err)
	}
	f.regex = re
	return f, nil
}

// matches reports whether entry passes the filter: its compiled regex (if
// any) must match the title and description, and, when keywords are set, at
// least one of them must appear (case-insensitively) in them. A nil filter
// matches everything.
func (f entryFilter) matches(entry rss.Entry) bool {
	if f.spec == nil {
		return true
	}

	if f.regex != nil && !f.regex.MatchString(entry.Title+"\n"+entry.Description) {
		return false
	}

	if len(f.spec.Keywords) == 0 {
		return true
	}

	content := strings.ToLower(strings.TrimSpace(entry.Title + "\n" + entry.Description))
	for _, keyword := range f.spec.Keywords {
		keyword = strings.TrimSpace(keyword)
		if keyword == "" {
			continue
		}
		if strings.Contains(content, strings.ToLower(keyword)) {
			return true
		}
	}

	return false
}
