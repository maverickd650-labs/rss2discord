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
	"testing"
)

// atomReading is what parseAtom hands withPreviousIDs for data, rebuilt here so
// a test can run the second reading with or without its pre-check.
func atomReading(t *testing.T, data []byte) (entries []Entry, items []atomItem, feedBase string) {
	t.Helper()
	entries, _, err := parseAtom(data)
	if err != nil {
		t.Fatalf("parseAtom: %v", err)
	}
	var feed atomFeed
	if err := unmarshalXML(data, &feed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, e := range feed.Entries {
		items = append(items, atomItem{atomEntryCore: e.atomEntryCore, ID: e.ID, Links: e.Links, Updated: e.Updated, Published: e.Published})
	}
	for i := range entries {
		entries[i].PreviousID, entries[i].PreviousIDSource = "", 0
	}
	return entries, items, strings.TrimSpace(feed.Base)
}

func anyPreviousID(entries []Entry) bool {
	for _, e := range entries {
		if e.PreviousID != "" {
			return true
		}
	}
	return false
}

// TestMayReadIdentityDifferently pins the pre-check one condition at a time:
// an entry child named id, link, updated or published outside Atom's namespace.
func TestMayReadIdentityDifferently(t *testing.T) {
	const feed = `<feed xmlns="` + atomNamespace + `" xmlns:ext="urn:ext" xmlns:a="` + atomNamespace + `">`
	for _, tc := range []struct {
		name, doc string
		want      bool
	}{
		{name: "plain entry", doc: feed + `<entry><id>x</id><link href="u"/><updated>d</updated><published>d</published></entry></feed>`},
		{name: "no entries", doc: feed + `<title>t</title></feed>`},
		{name: "foreign id", doc: feed + `<entry><ext:id>x</ext:id></entry></feed>`, want: true},
		{name: "foreign link", doc: feed + `<entry><ext:link href="u"/></entry></feed>`, want: true},
		{name: "foreign updated", doc: feed + `<entry><ext:updated>d</ext:updated></entry></feed>`, want: true},
		{name: "foreign published", doc: feed + `<entry><ext:published>d</ext:published></entry></feed>`, want: true},
		{name: "unnamespaced id", doc: feed + `<entry><id xmlns="">x</id></entry></feed>`, want: true},
		{name: "undeclared prefix", doc: feed + `<entry><nope:id>x</nope:id></entry></feed>`, want: true},
		{name: "foreign id in a later entry", doc: feed + `<entry><id>x</id></entry><entry><ext:id>y</ext:id></entry></feed>`, want: true},
		{name: "atom namespace through a prefix", doc: feed + `<entry><a:id>x</a:id><a:link href="u"/></entry></feed>`},
		{name: "foreign element of another name", doc: feed + `<entry><ext:title>x</ext:title><ext:category/></entry></feed>`},
		{name: "foreign id nested deeper", doc: feed + `<entry><source><ext:id>x</ext:id></source><content><ext:link/></content></entry></feed>`},
		{name: "foreign id outside any entry", doc: feed + `<ext:id>x</ext:id><ext:link/></feed>`},
		{name: "foreign id in a foreign entry", doc: feed + `<ext:entry><ext:id>x</ext:id></ext:entry></feed>`},
		{name: "foreign id after the root", doc: feed + `<entry><id>x</id></entry></feed><entry><ext:id>y</ext:id></entry>`},
		{name: "truncated document", doc: feed + `<entry><id>x</id>`, want: true},
		{name: "empty document", doc: ``, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mayReadIdentityDifferently([]byte(tc.doc)); got != tc.want {
				t.Errorf("mayReadIdentityDifferently = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPreCheckNeverSkipsAPreviousID builds Atom 1.0 entries from every
// combination of native id/link/updated/published elements and one foreign
// element (any of the four names, foreign or unnamespaced, before or after the
// native ones, with and without a title), and asserts that:
//   - whenever the unconditional second reading yields a PreviousID, the
//     pre-check said it might, and
//   - the gated result is always the unconditional one.
func TestPreCheckNeverSkipsAPreviousID(t *testing.T) {
	natives := []string{
		`<id>urn:real</id>`,
		`<link href="http://example.com/real"/>`,
		`<updated>2024-02-03T04:05:06Z</updated>`,
		`<published>2024-01-02T03:04:05Z</published>`,
	}
	// A foreign element is in the ext: namespace, or unnamespaced (xmlns="").
	foreign := func(unnamespaced bool, kind int) string {
		tag, decl := [4]string{"id", "link", "updated", "published"}[kind], ""
		if unnamespaced {
			decl = ` xmlns=""`
		} else {
			tag = "ext:" + tag
		}
		switch kind {
		case 0, 2, 3:
			value := [4]string{nsIDFake, "", "2023-02-03T04:05:06Z", "2023-01-02T03:04:05Z"}[kind]
			return fmt.Sprintf(`<%s%s>%s</%s>`, tag, decl, value, tag)
		default:
			return fmt.Sprintf(`<%s%s rel="alternate" href="http://example.com/fake"/>`, tag, decl)
		}
	}
	var docs []string
	for mask := range 1 << len(natives) {
		var native strings.Builder
		for i, n := range natives {
			if mask&(1<<i) != 0 {
				native.WriteString(n)
			}
		}
		for kind := range 4 {
			for _, ext := range []string{foreign(false, kind), foreign(true, kind)} {
				for _, body := range []string{ext + native.String(), native.String() + ext} {
					for _, title := range []string{"<title>T</title>", ""} {
						docs = append(docs, `<feed xmlns="`+atomNamespace+`" xmlns:ext="urn:ext"><entry>`+title+`<summary>s</summary>`+body+`</entry></feed>`)
					}
				}
			}
		}
	}
	produced := 0
	for _, doc := range docs {
		entries, items, feedBase := atomReading(t, []byte(doc))
		reference := readPreviousIDs(append([]Entry(nil), entries...), items, feedBase, []byte(doc))
		if anyPreviousID(reference) {
			produced++
			if !mayReadIdentityDifferently([]byte(doc)) {
				t.Fatalf("pre-check skipped a document that yields a PreviousID:\n%s", doc)
			}
		}
		gated := withPreviousIDs(append([]Entry(nil), entries...), items, feedBase, []byte(doc))
		for i := range gated {
			if gated[i].PreviousID != reference[i].PreviousID || gated[i].PreviousIDSource != reference[i].PreviousIDSource {
				t.Fatalf("entry %d: gated PreviousID %q, unconditional %q:\n%s", i, gated[i].PreviousID, reference[i].PreviousID, doc)
			}
		}
	}
	if produced == 0 {
		t.Fatal("no generated document yielded a PreviousID; the property is vacuous")
	}
}

// TestApplyPreviousIDsSetsNoneUnlessTheListsPair asserts the safe direction: a
// failed second decode or a prior list of another length than the entries
// (which the shared entry selection rules out) gives no entry a PreviousID.
func TestApplyPreviousIDsSetsNoneUnlessTheListsPair(t *testing.T) {
	entries := []Entry{{ID: "urn:real", IDSource: IDFromGUID}, {ID: "urn:two", IDSource: IDFromGUID}}
	items := make([]atomItem, len(entries))
	prior := []priorAtomEntry{{ID: nsIDFake}, {ID: "urn:other"}}

	for _, tc := range []struct {
		name  string
		prior []priorAtomEntry
		err   error
		want  string
	}{
		{name: "paired lists", prior: prior, want: nsIDFake},
		{name: "decode failure", prior: prior, err: fmt.Errorf("boom")},
		{name: "fewer prior entries", prior: prior[:1]},
		{name: "more prior entries", prior: append(append([]priorAtomEntry(nil), prior...), priorAtomEntry{ID: "urn:extra"})},
		{name: "no prior entries"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := applyPreviousIDs(append([]Entry(nil), entries...), items, "", tc.prior, tc.err)
			if got[0].PreviousID != tc.want {
				t.Errorf("first PreviousID = %q, want %q", got[0].PreviousID, tc.want)
			}
			if tc.want == "" && anyPreviousID(got) {
				t.Errorf("PreviousIDs set despite %s: %+v", tc.name, got)
			}
		})
	}
}

// TestAtomItemEffectiveBase asserts an entry's xml:base resolves against the feed's.
func TestAtomItemEffectiveBase(t *testing.T) {
	var item atomItem
	if got := item.effectiveBase("https://example.com/blog/"); got != "https://example.com/blog/" {
		t.Errorf("no entry base: got %q", got)
	}
	item.Base = " posts/ "
	if got := item.effectiveBase("https://example.com/blog/"); got != "https://example.com/blog/posts/" {
		t.Errorf("relative entry base: got %q", got)
	}
}
