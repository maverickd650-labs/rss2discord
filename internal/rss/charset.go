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
	"mime"
	"unicode/utf8"

	"golang.org/x/net/html/charset"
)

// utf8FromHeader returns data transcoded to UTF-8 when the response's
// Content-Type names a non-UTF-8 charset and the document itself declares no
// encoding, so a feed served as "text/xml; charset=iso-8859-1" with a bare
// (or no) XML declaration decodes instead of failing as invalid UTF-8. It is a
// fallback only: an encoding in the XML declaration wins (encoding/xml and
// unmarshalXML's CharsetReader already handle it), and an absent header or a
// charset label that is empty, UTF-8 or unknown to charset.Lookup returns data
// unchanged. So does a body that is already valid UTF-8: servers commonly send
// a default "charset=ISO-8859-1" with UTF-8 content, and transcoding that would
// corrupt a feed that parses correctly today, so the header only ever rescues a
// body that would otherwise fail. Only the explicit parameter is honoured; the
// body is never sniffed for an encoding.
func utf8FromHeader(data []byte, contentType string) []byte {
	_, params, _ := mime.ParseMediaType(contentType)
	enc, name := charset.Lookup(params["charset"])
	if enc == nil || name == "utf-8" || utf8.Valid(data) || declaresEncoding(data) {
		return data
	}
	// charset's decoders substitute U+FFFD for invalid input rather than
	// failing, so there is no error to handle.
	out, _ := enc.NewDecoder().Bytes(data)
	return out
}

// declaresEncoding reports whether data's XML declaration carries an encoding
// pseudo-attribute, reading no further than the first start tag. Anything it
// cannot tokenize, including a declaration encoding/xml refuses to honour
// without a CharsetReader, counts as declaring one, leaving data to fail
// exactly as it would without a header.
func declaresEncoding(data []byte) bool {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := decoder.Token()
		if err != nil {
			return true
		}
		switch t := tok.(type) {
		case xml.StartElement:
			return false
		case xml.ProcInst:
			if t.Target == "xml" {
				return bytes.Contains(t.Inst, []byte("encoding"))
			}
		}
	}
}
