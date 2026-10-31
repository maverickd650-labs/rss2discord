package rss

import (
	"encoding/xml"
	"errors"
	"testing"
)

const msgWantSyntaxError = "expected *xml.SyntaxError, got %T: %v"

// The bounded unmarshalers hand each matching element to DecodeElement and
// must surface its error instead of retaining a half-decoded value. A
// mismatched end tag inside the element is a syntax error the decoder reports
// from within DecodeElement.
func TestBoundedUnmarshalers_ReturnDecodeErrors(t *testing.T) {
	const atomOpen = `<feed xmlns="http://www.w3.org/2005/Atom"><entry>`
	const atomClose = `</entry></feed>`

	t.Run("atomLinks", func(t *testing.T) {
		var links atomLinks
		err := xml.Unmarshal([]byte(`<entry><link href="https://example.com/a"></bad></entry>`), &struct {
			Links *atomLinks `xml:"link"`
		}{Links: &links})
		if _, ok := errors.AsType[*xml.SyntaxError](err); !ok {
			t.Fatalf(msgWantSyntaxError, err, err)
		}
		if len(links) != 0 {
			t.Fatalf("a failed link must not be retained, got %v", links)
		}
	})

	t.Run("atomLinks through parseAtom", func(t *testing.T) {
		entries, title, err := parseAtom([]byte(atomOpen + `<link href="https://example.com/a"></bad>` + atomClose))
		if _, ok := errors.AsType[*xml.SyntaxError](err); !ok {
			t.Fatalf(msgWantSyntaxError, err, err)
		}
		if entries != nil || title != "" {
			t.Fatalf("got entries=%v title=%q, want none", entries, title)
		}
	})

	t.Run("firstThumbnail", func(t *testing.T) {
		var g mediaGroup
		err := xml.Unmarshal([]byte(`<group><thumbnail url="https://example.com/t.png"></bad></group>`), &g)
		if _, ok := errors.AsType[*xml.SyntaxError](err); !ok {
			t.Fatalf(msgWantSyntaxError, err, err)
		}
		if g.Thumbnail != "" {
			t.Fatalf("a failed thumbnail must not be retained, got %q", g.Thumbnail)
		}
	})

	t.Run("firstImageContent", func(t *testing.T) {
		var g mediaGroup
		err := xml.Unmarshal([]byte(`<group><content url="https://example.com/c.png" type="image/png"></bad></group>`), &g)
		if _, ok := errors.AsType[*xml.SyntaxError](err); !ok {
			t.Fatalf(msgWantSyntaxError, err, err)
		}
		if g.ImageContent != "" {
			t.Fatalf("a failed content must not be retained, got %q", g.ImageContent)
		}
	})

	t.Run("mediaGroup through parseAtom", func(t *testing.T) {
		for _, child := range []string{
			`<media:thumbnail url="https://example.com/t.png"></bad>`,
			`<media:content url="https://example.com/c.png" type="image/png"></bad>`,
		} {
			doc := `<feed xmlns="http://www.w3.org/2005/Atom" xmlns:media="http://search.yahoo.com/mrss/"><entry><media:group>` +
				child + `</media:group></entry></feed>`
			if _, _, err := parseAtom([]byte(doc)); err == nil {
				t.Fatalf("expected an error for %s", child)
			} else if _, ok := errors.AsType[*xml.SyntaxError](err); !ok {
				t.Fatalf(msgWantSyntaxError, err, err)
			}
		}
	})
}

func TestParseAtom_RootElementError(t *testing.T) {
	for name, doc := range map[string]string{
		"no input":     "",
		"no element":   "<!-- just a comment -->",
		"bad charset":  `<?xml version="1.0" encoding="no-such-charset"?><feed/>`,
		"unclosed tag": `<feed`,
	} {
		t.Run(name, func(t *testing.T) {
			entries, title, err := parseAtom([]byte(doc))
			if err == nil {
				t.Fatal("expected an error")
			}
			if entries != nil || title != "" {
				t.Fatalf("got entries=%v title=%q, want none", entries, title)
			}
		})
	}
}
