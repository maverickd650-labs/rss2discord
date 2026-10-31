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

package rss

import (
	"fmt"
	"strings"
	"time"
)

func parseTime(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}

	// A failed time.Parse allocates a *ParseError, and RSS pubDates are
	// overwhelmingly RFC 1123 while Atom and RDF dates are ISO 8601, so trying
	// one flat list in a fixed order made nearly every RSS date pay for a
	// failed RFC 3339 attempt first. The two groups can't both match one
	// value (ISO dates start with a four-digit year and a dash), so try the
	// likelier group first and fall back to the other: the accepted inputs
	// and results are unchanged.
	first, second := rfcLayouts, isoLayouts
	if looksISO(value) {
		first, second = isoLayouts, rfcLayouts
	}
	for _, layouts := range [][]string{first, second} {
		for _, layout := range layouts {
			if t, err := time.Parse(layout, value); err == nil {
				return t.UTC(), nil
			}
		}
	}

	return time.Time{}, fmt.Errorf("unsupported time format %q", value)
}

// isoLayouts are the ISO 8601-style formats (RFC 3339 for Atom/RDF, plus
// two common space/date-only variants); rfcLayouts are the RFC 822/1123 and
// asctime formats RSS pubDate uses. Each keeps its relative order from the
// original single list.
var (
	isoLayouts = []string{
		time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02",
	}
	rfcLayouts = []string{
		time.RFC1123Z,
		time.RFC1123,
		time.RFC822Z,
		time.RFC822,
		time.ANSIC,
		"Mon, 2 Jan 2006 15:04:05 MST",
	}
)

// looksISO reports whether value starts like an ISO 8601 date: four digits
// and a dash.
func looksISO(value string) bool {
	if len(value) < 5 || value[4] != '-' {
		return false
	}
	for i := range 4 {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}
