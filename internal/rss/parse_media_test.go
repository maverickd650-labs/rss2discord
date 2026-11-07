package rss

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"testing"
)

func TestEnclosureImage(t *testing.T) {
	cases := []struct {
		name  string
		links []atomLink
		want  string
	}{
		{name: caseNoLinks, links: nil, want: ""},
		{
			name:  "image enclosure matched",
			links: []atomLink{{Href: picJPEGURL, Rel: relEnclosure, Type: imageJPEG}},
			want:  picJPEGURL,
		},
		{
			name:  "non-image enclosure ignored",
			links: []atomLink{{Href: "http://file.pdf", Rel: relEnclosure, Type: "application/pdf"}},
			want:  "",
		},
		{
			name: "whitespace-only href skipped for a later usable one",
			links: []atomLink{
				{Href: "  ", Rel: relEnclosure, Type: imageJPEG},
				{Href: picJPEGURL, Rel: relEnclosure, Type: imageJPEG},
			},
			want: picJPEGURL,
		},
		{
			name:  "alternate link with image type ignored",
			links: []atomLink{{Href: picJPEGURL, Rel: relAlternate, Type: imageJPEG}},
			want:  "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := enclosureImage(tc.links); got != tc.want {
				t.Fatalf("enclosureImage() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRSSMediaURL_IsImage(t *testing.T) {
	cases := []struct {
		name string
		m    rssMediaURL
		want bool
	}{
		{name: "medium image", m: rssMediaURL{Medium: mediumImage}, want: true},
		{name: "type image", m: rssMediaURL{Type: "image/png"}, want: true},
		{name: "video medium", m: rssMediaURL{Medium: mediumVideo}, want: false},
		{name: "empty", m: rssMediaURL{}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.m.isImage(); got != tc.want {
				t.Fatalf("isImage() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEntryImage(t *testing.T) {
	cases := []struct {
		name      string
		enclosure *rssEnclosure
		thumbnail *rssMediaURL
		content   []rssMediaURL
		group     *mediaGroup
		want      string
	}{
		{name: "nothing set", want: ""},
		{
			name:      "enclosure preferred",
			enclosure: &rssEnclosure{URL: "http://enc.jpg", Type: imageJPEG},
			thumbnail: &rssMediaURL{URL: thumbJPEGURL},
			want:      "http://enc.jpg",
		},
		{
			name:      "non-image enclosure skipped, falls to thumbnail",
			enclosure: &rssEnclosure{URL: "http://file.mp3", Type: "audio/mpeg"},
			thumbnail: &rssMediaURL{URL: thumbJPEGURL},
			want:      thumbJPEGURL,
		},
		{
			name:    "media content image used when no enclosure/thumbnail",
			content: []rssMediaURL{{URL: "http://c.jpg", Medium: mediumImage}},
			want:    "http://c.jpg",
		},
		{
			name:    "non-image media content ignored",
			content: []rssMediaURL{{URL: "http://c.mp4", Medium: mediumVideo}},
			want:    "",
		},
		{
			name:      "enclosure with empty url skipped",
			enclosure: &rssEnclosure{URL: "", Type: imageJPEG},
			thumbnail: &rssMediaURL{URL: thumbJPEGURL},
			want:      thumbJPEGURL,
		},
		{
			name:    "group thumbnail used after direct sources",
			content: []rssMediaURL{{URL: "http://c.mp4", Medium: mediumVideo}},
			group:   &mediaGroup{Thumbnail: groupJPEGURL},
			want:    groupJPEGURL,
		},
		{
			name:      "direct thumbnail preferred over group",
			thumbnail: &rssMediaURL{URL: thumbJPEGURL},
			group:     &mediaGroup{Thumbnail: groupJPEGURL},
			want:      thumbJPEGURL,
		},
		{
			name:  "group image content used when group has no thumbnail",
			group: &mediaGroup{ImageContent: "http://g.png"},
			want:  "http://g.png",
		},
		{
			// Whitespace-only url attributes at every earlier tier must not
			// shadow the usable group image.
			name:      "whitespace-only urls skipped",
			enclosure: &rssEnclosure{URL: "  ", Type: imageJPEG},
			thumbnail: &rssMediaURL{URL: "\n"},
			content:   []rssMediaURL{{URL: " ", Medium: mediumImage}},
			group:     &mediaGroup{Thumbnail: groupJPEGURL},
			want:      groupJPEGURL,
		},
		{
			name:  "group without usable image",
			group: &mediaGroup{},
			want:  "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := entryImage(tc.enclosure, tc.thumbnail, tc.content, tc.group); got != tc.want {
				t.Fatalf("entryImage() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMediaGroupDecodeKeepsFirstUsable(t *testing.T) {
	cases := []struct {
		name, xml, want string
	}{
		{
			name: "first non-blank thumbnail wins, later ones dropped",
			xml: `<group><thumbnail url=" "/><thumbnail url=" http://g1.jpg "/>` +
				`<thumbnail url="http://g2.jpg"/><content url="http://c.png" medium="image"/></group>`,
			want: "http://g1.jpg",
		},
		{
			name: "first image content wins when no thumbnail",
			xml: `<group><content url="http://g.mp4" medium="video"/>` +
				`<content url="http://first.png" medium="image"/><content url="http://g2.png" type="image/png"/></group>`,
			want: "http://first.png",
		},
		{
			name: "no usable image",
			xml:  `<group><thumbnail/><content url="http://g.mp4" medium="video"/></group>`,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var g mediaGroup
			if err := xml.Unmarshal([]byte(tc.xml), &g); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := g.image(); got != tc.want {
				t.Fatalf("image() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseFeed_YouTubeAtomFixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "youtube.xml"))
	if err != nil {
		t.Fatal(err)
	}

	entries, _, err := parseFeed(data)
	if err != nil {
		t.Fatalf("parseFeed: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}

	first := entries[0]
	if first.ID != "yt:video:abc123" {
		t.Errorf("ID = %q", first.ID)
	}
	if first.Title != "First video" {
		t.Errorf("Title = %q, want the entry title (media:title is nested in the group)", first.Title)
	}
	if first.Link != "https://www.youtube.com/watch?v=abc123" {
		t.Errorf("Link = %q, want the alternate link", first.Link)
	}
	if first.Image != "https://i.ytimg.com/vi/abc123/hqdefault.jpg" {
		t.Errorf("Image = %q", first.Image)
	}
	if first.Description != "Line one of the description.\nLine two." {
		t.Errorf("Description = %q", first.Description)
	}
	if first.Author != "Example Channel" {
		t.Errorf("Author = %q", first.Author)
	}

	// An empty media:description leaves the description empty but the
	// thumbnail still comes through.
	second := entries[1]
	if second.Image != "https://i.ytimg.com/vi/def456/hqdefault.jpg" || second.Description != "" {
		t.Errorf("second entry image/description = %q/%q", second.Image, second.Description)
	}
}

func TestParseFeed_AtomMediaGroupDoesNotClobberCoreFields(t *testing.T) {
	// <media:content> and <media:title> share local names with Atom's
	// <content> and <title>; inside <media:group> they must not touch the
	// entry's own values, in either document order.
	const group = `<media:group><media:title>GroupTitle</media:title>` +
		`<media:content url="https://cdn.example/v.mp4" type="video/mp4"/>` +
		`<media:thumbnail url="https://cdn.example/t.jpg"/>` +
		`<media:description>GroupDesc</media:description></media:group>`
	const core = `<id>e1</id><title>Real</title><link href="https://a/1"/>` +
		`<content type="html">Real content</content>`

	for name, body := range map[string]string{
		"group after core":  core + group,
		"group before core": group + core,
	} {
		t.Run(name, func(t *testing.T) {
			doc := `<feed xmlns="http://www.w3.org/2005/Atom" xmlns:media="http://search.yahoo.com/mrss/"><entry>` + body + `</entry></feed>`
			entries, _, err := parseFeed([]byte(doc))
			if err != nil || len(entries) != 1 {
				t.Fatalf("parseFeed: %v, %d entries", err, len(entries))
			}
			e := entries[0]
			if e.Title != titleReal || e.Description != "Real content" {
				t.Errorf("title/description = %q/%q, want Real/Real content", e.Title, e.Description)
			}
			if e.Image != "https://cdn.example/t.jpg" {
				t.Errorf("Image = %q", e.Image)
			}
		})
	}
}

func TestParseFeed_AtomMediaImagePrecedenceAndDescriptionFallback(t *testing.T) {
	const head = `<feed xmlns="http://www.w3.org/2005/Atom" xmlns:media="http://search.yahoo.com/mrss/" xml:base="https://example.com/blog/"><entry><id>1</id>`
	const enclosure = `<link rel="enclosure" type="image/png" href="enc.png"/>`
	const direct = `<media:thumbnail url="direct.jpg"/>`
	const grpThumb = `<media:group><media:thumbnail url="group.jpg"/>`
	const grpContent = `<media:content url="groupcontent.png" medium="image"/>`

	tests := []struct {
		name      string
		body      string
		wantImage string
		wantDesc  string
	}{
		{
			name:      "enclosure beats everything",
			body:      grpThumb + grpContent + `</media:group>` + direct + enclosure,
			wantImage: "https://example.com/blog/enc.png",
		},
		{
			name:      "direct thumbnail beats group",
			body:      grpThumb + `</media:group>` + direct,
			wantImage: "https://example.com/blog/direct.jpg",
		},
		{
			name:      "group thumbnail beats group content",
			body:      `<media:group>` + grpContent + `<media:thumbnail url="https://abs.example/g.jpg"/></media:group>`,
			wantImage: "https://abs.example/g.jpg",
		},
		{
			name:      "group image content as last resort",
			body:      `<media:group>` + grpContent + `<media:content url="v.mp4" medium="video"/></media:group>`,
			wantImage: "https://example.com/blog/groupcontent.png",
		},
		{
			name:     "summary beats content beats group description",
			body:     `<summary>S</summary><content>C</content><media:group><media:description>G</media:description></media:group>`,
			wantDesc: "S",
		},
		{
			name:     "content beats group description",
			body:     `<content>C</content><media:group><media:description>G</media:description></media:group>`,
			wantDesc: "C",
		},
		{
			name:     "group description when nothing else",
			body:     `<media:group><media:description>  G  </media:description></media:group>`,
			wantDesc: "G",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			entries, _, err := parseFeed([]byte(head + tc.body + `</entry></feed>`))
			if err != nil || len(entries) != 1 {
				t.Fatalf("parseFeed: %v, %d entries", err, len(entries))
			}
			if entries[0].Image != tc.wantImage || entries[0].Description != tc.wantDesc {
				t.Errorf("image/description = %q/%q, want %q/%q", entries[0].Image, entries[0].Description, tc.wantImage, tc.wantDesc)
			}
		})
	}
}

func TestParseFeed_RSSMediaGroup(t *testing.T) {
	const doc = `<rss xmlns:media="http://search.yahoo.com/mrss/"><channel>` +
		`<item><title>WithGroup</title><link>https://a/1</link>` +
		`<media:group><media:content url="https://cdn.example/v.mp4" medium="video"/>` +
		`<media:thumbnail url="https://cdn.example/t.jpg"/>` +
		`<media:description>GroupDesc</media:description></media:group></item>` +
		`<item><title>RealDesc</title><link>https://a/2</link><description>Real</description>` +
		`<media:group><media:description>GroupDesc</media:description></media:group>` +
		`<enclosure url="https://cdn.example/e.jpg" type="image/jpeg"/></item>` +
		`</channel></rss>`

	entries, _, err := parseFeed([]byte(doc))
	if err != nil || len(entries) != 2 {
		t.Fatalf("parseFeed: %v, %d entries", err, len(entries))
	}
	if entries[0].Image != "https://cdn.example/t.jpg" || entries[0].Description != "GroupDesc" {
		t.Errorf("item 0 image/description = %q/%q", entries[0].Image, entries[0].Description)
	}
	// A real description and an enclosure image both outrank the group.
	if entries[1].Image != "https://cdn.example/e.jpg" || entries[1].Description != titleReal {
		t.Errorf("item 1 image/description = %q/%q", entries[1].Image, entries[1].Description)
	}
}
