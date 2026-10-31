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
	"bytes"
	"encoding/xml"

	"golang.org/x/net/html/charset"
)

// This file is the shim behind Entry.PreviousID for Atom 1.0 documents.
//
// Earlier releases read every Atom document by local name alone, so an
// extension's <ext:id> or <ext:link> could stand in for (or displace) Atom's
// own. atomEntry now matches those elements by namespace, which can change
// the identity an entry gets. Consumers carry stored identities over from the
// old choice to the new one, so the parser reports the old one as
// Entry.PreviousID. parseAtom knows nothing of this beyond one call to
// withPreviousIDs.

// priorAtomEntry reads only what an earlier release chose an entry's
// identity from, by local name alone as it did.
type priorAtomEntry struct {
	ID        string    `xml:"id"`
	Updated   string    `xml:"updated"`
	Published string    `xml:"published"`
	Links     atomLinks `xml:"link"`
}

// priorAtomFeed selects the same entries as atomFeed -- Atom-namespace ones, up
// to the same cap -- but reads each as earlier releases did; see priorAtomEntry.
type priorAtomFeed struct {
	Entries entryList[priorAtomEntry] `xml:"http://www.w3.org/2005/Atom entry"`
}

// withPreviousIDs sets PreviousID and PreviousIDSource on the entries of an
// Atom 1.0 document whose identity earlier releases picked differently. entries
// are parseAtom's result for data, items the readings they were built from (the
// same length and order) and feedBase the feed's trimmed xml:base. It returns
// entries, modified in place.
//
// The second, local-name reading of data only happens when the document could
// yield a different identity; see mayReadIdentityDifferently.
func withPreviousIDs(entries []Entry, items []atomItem, feedBase string, data []byte) []Entry {
	if !mayReadIdentityDifferently(data) {
		return entries
	}
	return readPreviousIDs(entries, items, feedBase, data)
}

// readPreviousIDs is withPreviousIDs without the pre-check: it always reads
// data a second time.
func readPreviousIDs(entries []Entry, items []atomItem, feedBase string, data []byte) []Entry {
	var feed priorAtomFeed
	err := unmarshalXML(data, &feed)
	return applyPreviousIDs(entries, items, feedBase, feed.Entries, err)
}

// applyPreviousIDs compares each entry's identity with the one prior, the
// local-name reading of the same entry, would have given it.
//
// prior pairs with entries by index, which holds because atomFeed and
// priorAtomFeed select the same entries in the same order: the Atom-namespace
// <entry> children of the root, in document order, capped at maxFeedEntries.
// Neither reading's remaining fields can fail to decode once the other has
// decoded cleanly, so the lists have the same length. Should that ever not
// hold, or should the second decode fail, no entry gets a PreviousID: an
// entry reporting none is only ever treated as unchanged, whereas a
// PreviousID paired with the wrong entry would move stored state onto it.
func applyPreviousIDs(entries []Entry, items []atomItem, feedBase string, prior []priorAtomEntry, err error) []Entry {
	if err != nil || len(prior) != len(entries) {
		return entries
	}
	for i, p := range prior {
		current := entries[i]
		read := items[i].identityFields(items[i].effectiveBase(feedBase), p.ID, p.Updated, p.Published, p.Links)
		previous := current
		previous.ID, previous.IDSource = read.id, read.idSource
		// A ContentID is also hashed from the date and the image, which
		// earlier releases read by local name too. (A blank alternate link
		// wins primaryLink, so an entry can reach a ContentID with an image
		// enclosure.)
		previous.Published, previous.Image = read.published, read.image
		// Both identities have the ContentID fallback before comparing, so
		// an entry with no id, link or title migrates from or to it too.
		// current already has it: parseAtom applies it to every entry.
		previous = withContentID(previous)
		if previous.ID != current.ID || previous.IDSource != current.IDSource {
			entries[i].PreviousID, entries[i].PreviousIDSource = previous.ID, previous.IDSource
		}
	}
	return entries
}

// mayReadIdentityDifferently reports whether the local-name reading of an Atom
// 1.0 document (priorAtomFeed) could give any entry another identity than the
// namespace reading (atomFeed). It can only if some entry has a direct child
// named id, link, updated or published outside Atom's namespace: those are the
// only elements the identity is derived from (with the title and Media RSS
// elements, which both readings share), and where every such child is in
// Atom's namespace both readings see the same elements in the same order, so
// they derive the same identity. It never answers false for a document that
// would yield a PreviousID; a true is only "might", so the entry is then
// compared. Anything it cannot tokenize also answers true.
func mayReadIdentityDifferently(data []byte) bool {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	decoder.CharsetReader = charset.NewReaderLabel
	depth, inAtomEntry := 0, false
	for {
		tok, err := decoder.Token()
		if err != nil {
			return true
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			switch depth {
			case 2:
				inAtomEntry = t.Name == xml.Name{Space: atomNamespace, Local: "entry"}
			case 3:
				if inAtomEntry && t.Name.Space != atomNamespace {
					switch t.Name.Local {
					case "id", "link", "updated", "published":
						return true
					}
				}
			}
		case xml.EndElement:
			depth--
			if depth == 0 {
				return false
			}
		}
	}
}
