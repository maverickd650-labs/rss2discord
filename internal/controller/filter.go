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

// entryFilter is a feed's filter compiled once per feed per reconcile: an
// include and an exclude ruleSet. The zero value matches every entry.
type entryFilter struct {
	include, exclude *ruleSet
}

// ruleSet is one compiled v1beta1.FilterRules. Regexes and keywords are
// normalized here rather than for every entry. Each has* flag distinguishes an
// absent criterion from a configured list whose entries are all blank, which
// matches nothing.
type ruleSet struct {
	regex                      *regexp.Regexp
	keywords, categories       []string
	hasKeywords, hasCategories bool
}

// compileEntryFilter compiles filter's two rule sets. A nil filter compiles to
// the zero entryFilter; an invalid regex is an error.
func compileEntryFilter(filter *v1beta1.Filter) (entryFilter, error) {
	if filter == nil {
		return entryFilter{}, nil
	}
	include, err := compileRuleSet("include", filter.Include)
	if err != nil {
		return entryFilter{}, err
	}
	exclude, err := compileRuleSet("exclude", filter.Exclude)
	if err != nil {
		return entryFilter{}, err
	}
	return entryFilter{include: include, exclude: exclude}, nil
}

// compileRuleSet compiles rules (nil for nil rules): its regex, unless blank,
// and its keywords and categories trimmed, lowercased, blanks dropped. which
// names the rule set in an invalid-regex error.
func compileRuleSet(which string, rules *v1beta1.FilterRules) (*ruleSet, error) {
	if rules == nil {
		return nil, nil
	}
	rs := &ruleSet{
		keywords:      normalizeTerms(rules.Keywords),
		hasKeywords:   len(rules.Keywords) > 0,
		categories:    normalizeTerms(rules.Categories),
		hasCategories: len(rules.Categories) > 0,
	}
	if strings.TrimSpace(rules.Regex) != "" {
		re, err := regexp.Compile(rules.Regex)
		if err != nil {
			return nil, fmt.Errorf("invalid filter %s regex %q: %w", which, rules.Regex, err)
		}
		rs.regex = re
	}
	return rs, nil
}

// normalizeTerms trims and lowercases terms, dropping blanks.
func normalizeTerms(terms []string) []string {
	var out []string
	for _, term := range terms {
		if term = strings.ToLower(strings.TrimSpace(term)); term != "" {
			out = append(out, term)
		}
	}
	return out
}

// entryText is what a filter matches entry against: the title and
// description, as given (for regexes) and lowercased (for keywords), and its
// categories, HTML-stripped and lowercased, as a display entry shows them.
type entryText struct {
	text, lower string
	categories  []string
}

func textOf(entry rss.Entry) entryText {
	text := entry.Title + "\n" + entry.Description
	categories := stripCategories(entry.Categories)
	for i, c := range categories {
		categories[i] = strings.ToLower(c)
	}
	return entryText{text: text, lower: strings.ToLower(strings.TrimSpace(text)), categories: categories}
}

// matches reports whether entry passes the filter: it must match every
// criterion include sets, and no criterion exclude sets. The zero value
// matches every entry.
func (f entryFilter) matches(entry rss.Entry) bool {
	if f.include == nil && f.exclude == nil {
		return true
	}
	t := textOf(entry)
	if f.exclude != nil && f.exclude.matchesAny(t) {
		return false
	}
	return f.include == nil || f.include.matchesAll(t)
}

// matchesAll reports whether t matches every criterion rs sets.
func (rs *ruleSet) matchesAll(t entryText) bool {
	if rs.regex != nil && !rs.regex.MatchString(t.text) {
		return false
	}
	if rs.hasKeywords && !rs.keywordMatch(t) {
		return false
	}
	return !rs.hasCategories || rs.categoryMatch(t)
}

// matchesAny reports whether t matches at least one criterion rs sets.
func (rs *ruleSet) matchesAny(t entryText) bool {
	return (rs.regex != nil && rs.regex.MatchString(t.text)) || rs.keywordMatch(t) || rs.categoryMatch(t)
}

// keywordMatch reports whether any of rs's keywords is a substring of t.
func (rs *ruleSet) keywordMatch(t entryText) bool {
	return slices.ContainsFunc(rs.keywords, func(keyword string) bool {
		return strings.Contains(t.lower, keyword)
	})
}

// categoryMatch reports whether any of rs's categories is one of t's.
func (rs *ruleSet) categoryMatch(t entryText) bool {
	return slices.ContainsFunc(rs.categories, func(category string) bool {
		return slices.Contains(t.categories, category)
	})
}
