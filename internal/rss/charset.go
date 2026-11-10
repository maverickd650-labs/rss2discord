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
	"io"
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
	label := headerCharset(contentType)
	if label == "" {
		return data
	}
	if utf8.Valid(data) {
		return data
	}
	if declaresEncoding(data) {
		return data
	}
	// charset's decoders substitute U+FFFD for invalid input rather than
	// failing, so there is no error to handle.
	enc, _ := charset.Lookup(label)
	out, _ := enc.NewDecoder().Bytes(data)
	return out
}

// headerCharset is the canonical name of the encoding a Content-Type's charset
// parameter names, or "" when there is none, it is UTF-8, or charset.Lookup
// does not know it.
func headerCharset(contentType string) string {
	_, params, _ := mime.ParseMediaType(contentType)
	enc, name := charset.Lookup(params["charset"])
	if enc == nil {
		return ""
	}
	if name == "utf-8" {
		return ""
	}
	return name
}

// declaresEncoding reports whether data's XML declaration carries an encoding
// pseudo-attribute, reading no further than the first start tag. A pass-through
// CharsetReader lets encoding/xml return the declaration token itself when a
// non-UTF-8 encoding is named, so any token error (such as a non-UTF-8 byte in
// a root attribute when no declaration is present) means no encoding was
// declared before the failure.
func declaresEncoding(data []byte) bool {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	decoder.CharsetReader = noopCharsetReader
	for {
		tok, err := decoder.Token()
		if declares, decided := encodingVerdict(tok, err); decided {
			return declares
		}
	}
}

func noopCharsetReader(_ string, r io.Reader) (io.Reader, error) {
	return r, nil
}

// encodingVerdict is declaresEncoding's answer for one token: decided is false
// while the prolog (comments, other processing instructions) continues.
func encodingVerdict(tok xml.Token, err error) (declares, decided bool) {
	if err != nil {
		return false, true
	}
	switch t := tok.(type) {
	case xml.StartElement:
		return false, true
	case xml.ProcInst:
		return bytes.Contains(t.Inst, []byte("encoding")), t.Target == "xml"
	}
	return false, false
}
