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
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// IDSource records which element an Entry's ID was taken from, so consumers
// can normalize a link-derived ID differently from an opaque GUID.
type IDSource uint8

const (
	// IDFromGUID is an RSS <guid> or Atom <id>. It is the zero value, so an
	// Entry built by hand with an explicit ID is treated as one.
	IDFromGUID IDSource = iota
	// IDFromLink is the entry's link, used when it has no GUID.
	IDFromLink
	// IDFromTitle is the entry's title, used when it has no GUID or link.
	IDFromTitle
	// IDFromContent is ContentID, used when the entry has no GUID, link or
	// title.
	IDFromContent
)

// contentIDPrefix marks a ContentID. It is not an http(s) URL, so identity
// normalization leaves it untouched.
const contentIDPrefix = "content:sha256:"

// ContentID derives a stable, non-empty identity for an entry with no GUID,
// link or title from its description, content, image and publication time.
// Entries that agree on all four are indistinguishable and share an ID.
func ContentID(entry Entry) string {
	published := ""
	if !entry.Published.IsZero() {
		published = entry.Published.UTC().Format(time.RFC3339Nano)
	}
	h := sha256.New()
	for _, part := range []string{entry.Description, entry.Content, entry.Image, published} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return contentIDPrefix + hex.EncodeToString(h.Sum(nil))
}

// withContentID fills in ContentID for an entry whose ID selection found
// nothing, so no parsed entry ever has an empty identity.
func withContentID(entry Entry) Entry {
	if entry.ID == "" {
		entry.ID, entry.IDSource = ContentID(entry), IDFromContent
	}
	return entry
}
