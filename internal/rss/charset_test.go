package rss

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// latin1 is "Café" with the é as the single ISO-8859-1 byte 0xE9.
const (
	latin1Title = "Caf\xe9"
	utf8Title   = "Café"
	latin1Type  = "text/xml; charset=iso-8859-1"
)

func TestUTF8FromHeader(t *testing.T) {
	rssDoc := func(prolog string) string {
		return prolog + `<rss><channel><item><title>` + latin1Title + `</title><guid>1</guid></item></channel></rss>`
	}
	atomDoc := func(prolog string) string {
		return prolog + `<feed xmlns="http://www.w3.org/2005/Atom"><entry><id>1</id><title>` + latin1Title + `</title></entry></feed>`
	}
	cases := []struct {
		name        string
		doc         string
		contentType string
		wantOK      bool
	}{
		{"rss header only, no declaration", rssDoc(""), latin1Type, true},
		{"rss header only, bare declaration", rssDoc(`<?xml version="1.0"?>`), "text/xml; charset=ISO-8859-1", true},
		{"rss quoted label", rssDoc(""), `application/rss+xml; charset="latin1"`, true},
		{"rss windows-1252 label", rssDoc(""), "text/xml; charset=windows-1252", true},
		{"atom header only", atomDoc(""), "application/atom+xml; charset=iso-8859-1", true},
		{"atom bare declaration", atomDoc(`<?xml version="1.0"?>`), "application/atom+xml; charset=iso-8859-1", true},
		{"comment before the declaration is fine", rssDoc("<!-- c -->"), latin1Type, true},
		{"declaration wins over header", rssDoc(`<?xml version="1.0" encoding="iso-8859-1"?>`), "text/xml; charset=utf-16", true},
		{"declaration with its own utf-8 wins over header", rssDoc(`<?xml version="1.0" encoding="utf-8"?>`), latin1Type, false},
		{"unknown label behaves as before", rssDoc(""), "text/xml; charset=no-such-charset", false},
		{"no charset parameter", rssDoc(""), "text/xml", false},
		{"no content type", rssDoc(""), "", false},
		{"garbage content type", rssDoc(""), ";;;=", false},
		{"utf-8 label is a no-op", rssDoc(""), "text/xml; charset=utf-8", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entries, _, err := parseFeed(utf8FromHeader([]byte(tc.doc), tc.contentType))
			if !tc.wantOK {
				if err == nil {
					t.Fatalf("parsed %+v, want the same invalid-UTF-8 failure as without a header", entries)
				}
				return
			}
			requireOneTitle(t, entries, err)
		})
	}
}

func TestUTF8FromHeader_LeavesUTF8AndUnparsableBodiesAlone(t *testing.T) {
	for _, tc := range []struct{ name, doc, contentType string }{
		{"utf-8 body, utf-8 label", `<rss><channel><item><title>Café</title></item></channel></rss>`, "text/xml; charset=utf-8"},
		{"declared unknown encoding", `<?xml version="1.0" encoding="no-such"?><rss><channel><title>` + latin1Title + `</title></channel></rss>`, latin1Type},
		{"empty body", ``, latin1Type},
		{"utf-8 body under a mislabelled latin-1 header", `<rss><channel><item><title>Café</title></item></channel></rss>`, latin1Type},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := utf8FromHeader([]byte(tc.doc), tc.contentType)
			if string(got) != tc.doc {
				t.Fatalf("body changed: %q", got)
			}
		})
	}
}

func TestFetchEntries_HonoursContentTypeCharset(t *testing.T) {
	serve := func(t *testing.T, contentType, body string) (FetchResult, error) {
		t.Helper()
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if contentType != "" {
				w.Header().Set("Content-Type", contentType)
			}
			_, _ = w.Write([]byte(body))
		}))
		return NewClient(srv.Client()).FetchEntries(t.Context(), srv.URL, CacheValidators{})
	}
	doc := `<?xml version="1.0"?><rss><channel><title>` + latin1Title + `</title><item><title>` + latin1Title + `</title><guid>1</guid></item></channel></rss>`

	result, err := serve(t, latin1Type, doc)
	requireOneTitle(t, result.Entries, err)
	if result.FeedTitle != utf8Title {
		t.Fatalf("FeedTitle = %q, want %q", result.FeedTitle, utf8Title)
	}

	if _, err := serve(t, "text/xml", doc); err == nil {
		t.Fatal("without a charset parameter the Latin-1 body must still fail as invalid UTF-8")
	}
}

// requireOneTitle fails unless the parse succeeded with exactly one entry
// whose title decoded to utf8Title.
func requireOneTitle(t *testing.T, entries []Entry, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %+v, want exactly one", entries)
	}
	if entries[0].Title != utf8Title {
		t.Fatalf("title = %q, want %q", entries[0].Title, utf8Title)
	}
}
