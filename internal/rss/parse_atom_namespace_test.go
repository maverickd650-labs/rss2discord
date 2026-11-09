package rss

import (
	"testing"
)

// atomDocNS wraps entry in an Atom feed whose default namespace is ns (none if
// ns is empty) and which declares the foreign "ext" prefix.
func atomDocNS(ns, entry string) []byte {
	decl := ""
	if ns != "" {
		decl = ` xmlns="` + ns + `"`
	}
	return []byte(`<?xml version="1.0"?><feed` + decl + ` xmlns:ext="urn:ext"><entry><title>T</title>` + entry + `</entry></feed>`)
}

const (
	nsIDReal    = "urn:real"
	nsIDFake    = "urn:fake"
	nsLinkReal  = "http://example.com/real"
	nsLinkFake  = "http://example.com/fake"
	nsImgReal   = "http://example.com/real.png"
	nsImgFake   = "http://example.com/fake.png"
	nsAtomOld   = "http://purl.org/atom/ns#"
	nsEnclosure = `rel="enclosure" type="image/png" `
	// nsExtIDFake is an extension's <ext:id>, which must never pass for Atom's own.
	nsExtIDFake = "<ext:id>" + nsIDFake + "</ext:id>"
)

// Element builders for the Atom namespace tests.
// nsIDRealEl is the entry's own <id> element.
const nsIDRealEl = "<id>" + nsIDReal + "</id>"

// linkEl is a self-closing <tag> carrying attrs (each ending in a space) and an href.
func linkEl(tag, attrs, href string) string {
	return "<" + tag + " " + attrs + `href="` + href + `"/>`
}

// TestParseFeed_AtomExtensionElementsNeverTouchCoreFields asserts that in an
// Atom 1.0 document an extension's <ext:id> and <ext:link> can neither
// displace Atom's own nor stand in for a missing one, and that other
// documents are still read by local name.
func TestParseFeed_AtomExtensionElementsNeverTouchCoreFields(t *testing.T) {
	for _, tc := range []struct {
		name, ns, entry             string
		wantID, wantLink, wantImage string
	}{
		{name: "foreign id after real", ns: atomNamespace, entry: nsIDRealEl + nsExtIDFake, wantID: nsIDReal},
		{name: "foreign id before real", ns: atomNamespace, entry: nsExtIDFake + nsIDRealEl, wantID: nsIDReal},
		{name: "foreign id alone is ignored", ns: atomNamespace, entry: nsExtIDFake, wantID: "T"},
		{name: "foreign link first", ns: atomNamespace, entry: linkEl("ext:link", "", nsLinkFake) + linkEl("link", `rel="alternate" `, nsLinkReal), wantLink: nsLinkReal, wantID: nsLinkReal},
		{name: "foreign alternate after real", ns: atomNamespace, entry: linkEl("link", "", nsLinkReal) + linkEl("ext:link", `rel="alternate" `, nsLinkFake), wantLink: nsLinkReal, wantID: nsLinkReal},
		{name: "foreign link alone is ignored", ns: atomNamespace, entry: linkEl("ext:link", "", nsLinkFake), wantID: "T"},
		{name: "unnamespaced link is not Atom's", ns: atomNamespace, entry: linkEl("link", `xmlns="" `, nsLinkFake) + linkEl("link", "", nsLinkReal), wantLink: nsLinkReal, wantID: nsLinkReal},
		{
			name:   "foreign image enclosure first",
			ns:     atomNamespace,
			entry:  nsIDRealEl + linkEl("ext:link", nsEnclosure, nsImgFake) + linkEl("link", nsEnclosure, nsImgReal),
			wantID: nsIDReal, wantLink: nsImgReal, wantImage: nsImgReal, // the only native link doubles as the link
		},
		{name: "lookalike default namespace is read by local name", ns: nsAtomOld, entry: nsIDRealEl + linkEl("link", "", nsLinkReal), wantID: nsIDReal, wantLink: nsLinkReal},
		{name: "no namespace is read by local name", entry: nsIDRealEl + linkEl("link", "", nsLinkReal), wantID: nsIDReal, wantLink: nsLinkReal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := mustParseOne(t, atomDocNS(tc.ns, tc.entry))
			type idLinkImage struct{ ID, Link, Image string }
			assertField(t, "ID, Link, Image", idLinkImage{e.ID, e.Link, e.Image},
				idLinkImage{tc.wantID, tc.wantLink, tc.wantImage})
		})
	}
}
