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
	"strings"
	"testing"
	"time"
)

// Every ID fallback records where the ID came from, and an entry with no
// GUID, link or title still gets a non-empty ContentID.
func TestParseFeed_RecordsIDSource(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
	}{
		{name: "RSS 2.0", data: `<rss><channel>
<item><guid>g-1</guid><link>https://example.com/1</link></item>
<item><link>https://example.com/2#v2</link></item>
<item><title>OnlyTitle</title></item>
<item><description>body</description><pubDate>Wed, 21 Oct 2015 07:28:00 +0000</pubDate></item>
</channel></rss>`},
		{name: "Atom 1.0", data: `<feed xmlns="http://www.w3.org/2005/Atom">
<entry><id>g-1</id><link href="https://example.com/1"/></entry>
<entry><link href="https://example.com/2#v2"/></entry>
<entry><title>OnlyTitle</title></entry>
<entry><summary>body</summary><published>2015-10-21T07:28:00Z</published></entry>
</feed>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entries, _, err := parseFeed([]byte(tc.data))
			if err != nil || len(entries) != 4 {
				t.Fatalf("entries=%d err=%v", len(entries), err)
			}
			for i, want := range []struct {
				id     string
				source IDSource
			}{
				{"g-1", IDFromGUID},
				{"https://example.com/2#v2", IDFromLink},
				{"OnlyTitle", IDFromTitle},
				{ContentID(entries[3]), IDFromContent},
			} {
				if entries[i].ID != want.id || entries[i].IDSource != want.source {
					t.Errorf("entry %d: ID=%q source=%d, want %q/%d", i, entries[i].ID, entries[i].IDSource, want.id, want.source)
				}
			}
			if !strings.HasPrefix(entries[3].ID, contentIDPrefix) {
				t.Errorf("content ID %q lacks %q", entries[3].ID, contentIDPrefix)
			}
		})
	}
}

// ContentID is stable for the same entry and differs when any input does.
func TestContentID(t *testing.T) {
	base := Entry{Description: "d", Content: "c", Image: "i", Published: time.Unix(100, 0)}
	if id := ContentID(base); id != ContentID(base) || ContentID(Entry{}) == "" {
		t.Fatal("ContentID is not stable and non-empty")
	}
	// The zone of Published must not matter: parsed times are UTC, but an
	// entry built elsewhere may carry a local time for the same instant.
	inZone := base
	inZone.Published = base.Published.In(time.FixedZone("x", 3600))
	if ContentID(inZone) != ContentID(base) {
		t.Fatal("ContentID depends on Published's time zone")
	}
	for _, changed := range []Entry{
		{Description: "x", Content: "c", Image: "i", Published: base.Published},
		{Description: "d", Content: "x", Image: "i", Published: base.Published},
		{Description: "d", Content: "c", Image: "x", Published: base.Published},
		{Description: "d", Content: "c", Image: "i"},
		// Fields are delimited, so moving text between them changes the ID.
		{Description: "dc", Image: "i", Published: base.Published},
	} {
		if ContentID(changed) == ContentID(base) {
			t.Errorf("ContentID(%+v) collides with the base entry", changed)
		}
	}
}
