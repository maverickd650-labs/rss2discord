package rss

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestParseFeed_AtomPreviousID asserts an Atom 1.0 entry reports the ID earlier
// releases would have picked exactly when that differs, and not otherwise.
func TestParseFeed_AtomPreviousID(t *testing.T) {
	for _, tc := range []struct {
		name, entry    string
		ns             string
		wantPrev       string
		wantPrevSource IDSource
	}{
		{name: "foreign id after real", entry: idEl(nsIDReal) + nsExtIDFake, wantPrev: nsIDFake, wantPrevSource: IDFromGUID},
		{name: "foreign id alone", entry: nsExtIDFake, wantPrev: nsIDFake, wantPrevSource: IDFromGUID},
		{name: "foreign id before real changes nothing", entry: nsExtIDFake + idEl(nsIDReal)},
		{name: "foreign link picked as the link ID", entry: linkEl("ext:link", "", nsLinkFake) + linkEl("link", "", nsLinkReal), wantPrev: nsLinkFake, wantPrevSource: IDFromLink},
		{name: "foreign link behind an id changes nothing", entry: idEl(nsIDReal) + linkEl("ext:link", "", nsLinkFake)},
		{name: "plain entry", entry: idEl(nsIDReal) + linkEl("link", "", nsLinkReal)},
		{name: "loose documents are read as before", ns: nsAtomOld, entry: idEl(nsIDReal) + nsExtIDFake},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns := tc.ns
			if ns == "" {
				ns = atomNamespace
			}
			entries, _, err := parseFeed(atomDocNS(ns, tc.entry))
			if err != nil || len(entries) != 1 {
				t.Fatalf("parse: entries=%d err=%v", len(entries), err)
			}
			if e := entries[0]; e.PreviousID != tc.wantPrev || e.PreviousIDSource != tc.wantPrevSource {
				t.Errorf("PreviousID, PreviousIDSource = %q, %v; want %q, %v", e.PreviousID, e.PreviousIDSource, tc.wantPrev, tc.wantPrevSource)
			}
		})
	}
}

// A foreign alternate link with a blank href is what earlier releases read first,
// so with no id or title they fell back to ContentID. That is the identity to
// migrate from, not "none".
func TestParseFeed_AtomPreviousIDFallsBackToContentID(t *testing.T) {
	doc := `<feed xmlns="` + atomNamespace + `" xmlns:ext="urn:ext"><entry>` +
		linkEl("ext:link", `rel="alternate" `, "") + linkEl("link", `rel="alternate" `, nsLinkReal) +
		`<summary>body</summary></entry></feed>`
	entries, _, err := parseFeed([]byte(doc))
	if err != nil || len(entries) != 1 {
		t.Fatalf("parse: entries=%d err=%v", len(entries), err)
	}
	e := entries[0]
	if e.ID != nsLinkReal || e.IDSource != IDFromLink {
		t.Fatalf("ID, IDSource = %q, %v; want %q, %v", e.ID, e.IDSource, nsLinkReal, IDFromLink)
	}
	if want := ContentID(e); e.PreviousID != want || e.PreviousIDSource != IDFromContent {
		t.Errorf("PreviousID, PreviousIDSource = %q, %v; want %q, %v", e.PreviousID, e.PreviousIDSource, want, IDFromContent)
	}
}

// The content hash covers the publication date and image, which earlier releases
// also read by local name, so a foreign <ext:updated> or image enclosure that
// stood in for a missing one is part of the identity to migrate from. The blank
// alternate link is what leaves the entry with no link identity.
func TestParseFeed_AtomPreviousIDContentHashUsesPriorDateAndImage(t *testing.T) {
	doc := `<feed xmlns="` + atomNamespace + `" xmlns:ext="urn:ext"><entry>` +
		`<ext:updated>2024-01-02T03:04:05Z</ext:updated>` +
		linkEl("ext:link", `rel="alternate" `, "") +
		linkEl("ext:link", `rel="enclosure" type="image/png" `, nsImgFake) +
		`<summary>body</summary></entry></feed>`
	entries, _, err := parseFeed([]byte(doc))
	if err != nil || len(entries) != 1 {
		t.Fatalf("parse: entries=%d err=%v", len(entries), err)
	}
	e := entries[0]
	prior := e
	prior.Published = time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	prior.Image = nsImgFake
	if e.IDSource != IDFromContent || e.PreviousIDSource != IDFromContent || e.PreviousID != ContentID(prior) || e.PreviousID == e.ID {
		t.Errorf("ID source %v, PreviousID, PreviousIDSource = %q, %v; want %q, %v", e.IDSource, e.PreviousID, e.PreviousIDSource, ContentID(prior), IDFromContent)
	}
}

// TestParseFeed_AtomPreviousIDAlignsWithEntries asserts an extension element
// named entry, which atomEntry never reads, can't shift PreviousID onto the
// wrong entry.
func TestParseFeed_AtomPreviousIDAlignsWithEntries(t *testing.T) {
	doc := `<feed xmlns="` + atomNamespace + `" xmlns:ext="urn:ext">` +
		`<ext:entry>` + idEl(nsIDFake) + `</ext:entry>` +
		`<entry>` + idEl(nsIDReal) + nsExtIDFake + `</entry>` +
		`<entry>` + idEl("urn:other") + `</entry></feed>`
	entries, _, err := parseFeed([]byte(doc))
	if err != nil || len(entries) != 2 {
		t.Fatalf("parse: entries=%d err=%v", len(entries), err)
	}
	if entries[0].ID != nsIDReal || entries[0].PreviousID != nsIDFake {
		t.Errorf("first entry ID, PreviousID = %q, %q; want %q, %q", entries[0].ID, entries[0].PreviousID, nsIDReal, nsIDFake)
	}
	if entries[1].ID != "urn:other" || entries[1].PreviousID != "" {
		t.Errorf("second entry ID, PreviousID = %q, %q; want urn:other and none", entries[1].ID, entries[1].PreviousID)
	}
}

// An extension element named entry, ahead of more genuine entries than the cap
// keeps, must not use up the cap in the prior reading: PreviousID is still
// reported for the entries the parser retains.
func TestParseFeed_AtomPreviousIDSurvivesTheCapWithForeignEntries(t *testing.T) {
	var doc strings.Builder
	doc.WriteString(`<feed xmlns="` + atomNamespace + `" xmlns:ext="urn:ext"><ext:entry>` + idEl(nsIDFake) + `</ext:entry>`)
	doc.WriteString(`<entry>` + idEl(nsIDReal) + nsExtIDFake + `</entry>`)
	for i := range maxFeedEntries + 5 {
		doc.WriteString(`<entry>` + idEl(fmt.Sprintf("urn:filler:%d", i)) + `</entry>`)
	}
	doc.WriteString(`</feed>`)

	entries, _, err := parseFeed([]byte(doc.String()))
	if err != nil || len(entries) != maxFeedEntries {
		t.Fatalf("parse: entries=%d err=%v, want %d entries", len(entries), err, maxFeedEntries)
	}
	if entries[0].ID != nsIDReal || entries[0].PreviousID != nsIDFake {
		t.Fatalf("first entry ID, PreviousID = %q, %q; want %q, %q", entries[0].ID, entries[0].PreviousID, nsIDReal, nsIDFake)
	}
}

// TestParseFeed_AtomPreviousIDEmptyForPlainDocument asserts an Atom 1.0 entry
// with dates, an enclosure, a relative link and an xml:base, but no extension
// elements, reports no PreviousID: both vocabularies derive the same identity.
func TestParseFeed_AtomPreviousIDEmptyForPlainDocument(t *testing.T) {
	doc := []byte(`<?xml version="1.0"?>
<feed xmlns="` + atomNamespace + `" xml:base="https://example.com/blog/">
  <title>Plain</title>
  <entry>
    <id>urn:uuid:1</id><title>One</title>
    <link rel="alternate" href="posts/one"/>
    <link rel="enclosure" type="image/png" href="img/one.png"/>
    <updated>2024-01-02T03:04:05Z</updated>
    <published>2024-01-01T03:04:05Z</published>
  </entry>
  <entry>
    <title>Two</title>
    <link href="posts/two"/>
    <updated>2024-01-03T03:04:05Z</updated>
  </entry>
</feed>`)
	entries, _, err := parseFeed(doc)
	if err != nil || len(entries) != 2 {
		t.Fatalf("parse: entries=%d err=%v", len(entries), err)
	}
	for _, e := range entries {
		if e.PreviousID != "" || e.PreviousIDSource != 0 {
			t.Errorf("entry %q: PreviousID, PreviousIDSource = %q, %v; want empty", e.Title, e.PreviousID, e.PreviousIDSource)
		}
	}
}
