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

package migrate

import (
	"errors"
	"strings"
	"testing"

	"github.com/maverickd650-labs/rss2discord/api/v1beta1"
	"sigs.k8s.io/yaml"
)

const fullV1alpha1 = `apiVersion: rss2discord.maverickd650.dev/v1alpha1
kind: FeedGroup
metadata:
  name: news
  namespace: default
  labels: {team: a}
  annotations:
    keep: "yes"
    kubectl.kubernetes.io/last-applied-configuration: "{}"
  resourceVersion: "42"
  uid: abc
spec:
  discordWebhookSecretRef: {name: hook, key: url}
  interval: 15m
  retries: 2
  retryInterval: 1m
  catchUpLimit: 0
  username: Bot
  avatarURL: https://example.com/a.png
  format: "{{.Title}}"
  feeds:
    - rssUrl: https://www.example.com/blog/feed.xml
      paused: true
      filter: {regex: Go, keywords: [release]}
    - rssUrl: https://example.com/blog/rss
      format: "custom {{.Link}}"
      forumThreadName: "{{.Title}}"
    - rssUrl: https://other.example.com/
      embed: {enabled: true, color: "#ffffff", descriptionFormat: "{{.Description}}", imageStyle: image, authorName: "Weird {{x", footerText: Footer}
      forumThreadID: "123"
      forumThreadName: ignored
    - rssUrl: https://other.example.com/x.atom
      format: "{{.Title}}"
      filter: {}
status:
  observedGeneration: 3
`

func convert(t *testing.T, in string) (string, []string) {
	t.Helper()
	var out strings.Builder
	var notes []string
	if err := Convert(strings.NewReader(in), &out, func(s string) { notes = append(notes, s) }); err != nil {
		t.Fatal(err)
	}
	return out.String(), notes
}

// convertedFeedGroup is the shape a converted document must decode into,
// strictly.
type convertedFeedGroup struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name            string            `json:"name"`
		Namespace       string            `json:"namespace"`
		ResourceVersion string            `json:"resourceVersion"`
		Labels          map[string]string `json:"labels"`
		Annotations     map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec   v1beta1.FeedGroupSpec `json:"spec"`
	Status any                   `json:"status"`
}

func TestConvert_FullFeedGroup(t *testing.T) {
	out, notes := convert(t, fullV1alpha1)

	var got convertedFeedGroup
	if err := yaml.UnmarshalStrict([]byte(out), &got); err != nil {
		t.Fatalf("output is not a clean v1beta1 FeedGroup: %v\n%s", err, out)
	}
	t.Run("metadata", func(t *testing.T) { checkMetadata(t, got) })
	t.Run("group", func(t *testing.T) { checkGroup(t, got.Spec) })
	t.Run("feeds", func(t *testing.T) { checkFeeds(t, got.Spec.Feeds) })

	if len(notes) != 2 || !strings.Contains(notes[0], `FeedGroup "default/news": spec.feeds[2].embed.authorName`) ||
		!strings.Contains(notes[1], "spec.feeds[2]: forumThreadName dropped") {
		t.Fatalf("notes = %q", notes)
	}
}

func checkMetadata(t *testing.T, got convertedFeedGroup) {
	if got.APIVersion != "rss2discord.maverickd650.dev/v1beta1" || got.Kind != "FeedGroup" || got.Metadata.Name != "news" || got.Metadata.Namespace != "default" {
		t.Fatalf("header = %+v", got)
	}
	if got.Metadata.ResourceVersion != "" || got.Status != nil || got.Metadata.Labels["team"] != "a" {
		t.Fatalf("metadata/status = %+v / %v", got.Metadata, got.Status)
	}
	if len(got.Metadata.Annotations) != 1 || got.Metadata.Annotations["keep"] != "yes" {
		t.Fatalf("annotations = %v, want only the user's", got.Metadata.Annotations)
	}
}

func checkGroup(t *testing.T, s v1beta1.FeedGroupSpec) {
	if s.Interval != "15m" || s.Retries == nil || *s.Retries != 2 || s.RetryInterval != "1m" || s.CatchUpLimit == nil || *s.CatchUpLimit != 0 ||
		s.Username != "Bot" || s.AvatarURL != "https://example.com/a.png" || s.DiscordWebhookSecretRef.Name != "hook" {
		t.Fatalf("group fields = %+v", s)
	}
	if s.Message == nil || s.Message.Content != "{{.Title}}" {
		t.Fatalf("group message = %+v", s.Message)
	}
}

func checkFeeds(t *testing.T, f []v1beta1.FeedSpec) {
	if len(f) != 4 {
		t.Fatalf("feeds = %+v", f)
	}
	if f[0].Name != "example-com-blog" || !f[0].Suspend || f[0].Message != nil ||
		f[0].Filter.Include.Regex != "Go" || f[0].Filter.Include.Keywords[0] != "release" || f[0].Filter.Exclude != nil {
		t.Fatalf("feed 0 = %+v", f[0])
	}
	if f[1].Name != "example-com-blog-2" || f[1].Message.Content != "custom {{.Link}}" || f[1].ForumPost.Name != "{{.Title}}" {
		t.Fatalf("feed 1 = %+v", f[1])
	}
	e := f[2].Message.Embed
	if f[2].Name != "other-example-com" || f[2].Thread.ID != "123" || f[2].ForumPost != nil || f[2].Message.Content != "" {
		t.Fatalf("feed 2 = %+v", f[2])
	}
	if e.Color != "#ffffff" || e.ImageStyle != v1beta1.EmbedImageStyleImage || e.Author.Name != "Weird {{x" || e.Footer.Text != "Footer" {
		t.Fatalf("feed 2 embed = %+v", e)
	}
	if f[3].Name != "other-example-com-x" || f[3].Message != nil || f[3].Filter != nil {
		t.Fatalf("feed 3 = %+v: a format equal to the group's needs no message", f[3])
	}
}

func TestConvert_EmbedInheritance(t *testing.T) {
	out, _ := convert(t, `apiVersion: rss2discord.maverickd650.dev/v1alpha1
kind: FeedGroup
metadata: {name: g}
spec:
  discordWebhookSecretRef: {name: hook, key: url}
  embed: {enabled: true, imageStyle: thumbnail}
  format: ignored-beside-embed
  feeds:
    - rssUrl: https://a.example.com/feed
    - rssUrl: https://b.example.com/feed
      format: "{{.Title}}"
    - rssUrl: https://c.example.com/feed
      embed: {enabled: false}
`)
	var got struct {
		Spec v1beta1.FeedGroupSpec `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if m := got.Spec.Message; m == nil || m.Embed == nil || m.Content != "" || m.Embed.ImageStyle != "" {
		t.Fatalf("group message = %+v: an enabled embed sends no content", m)
	}
	if got.Spec.Feeds[0].Message != nil || got.Spec.Feeds[1].Message != nil {
		t.Fatalf("feeds 0/1 inherit the group's embed: %+v", got.Spec.Feeds[:2])
	}
	if m := got.Spec.Feeds[2].Message; m == nil || m.Embed != nil || m.Content != "ignored-beside-embed" {
		t.Fatalf("feed 2 disabled the embed, so it sends the group's format: %+v", m)
	}
}

// retries: 0 must survive conversion: v1beta1 would default an omitted
// retries to 3.
func TestConvert_Retries(t *testing.T) {
	const doc = "apiVersion: rss2discord.maverickd650.dev/v1alpha1\nkind: FeedGroup\nmetadata: {name: x}\nspec: {%sfeeds: [{rssUrl: 'https://x.example.com/'}]}\n"
	for _, tc := range []struct {
		name, field string
		want        string
	}{
		{name: "explicit zero", field: "retries: 0, ", want: "retries: 0"},
		{name: "explicit value", field: "retries: 4, ", want: "retries: 4"},
		{name: "omitted", field: "", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, _ := convert(t, strings.Replace(doc, "%s", tc.field, 1))
			if tc.want == "" {
				if strings.Contains(out, "retries") {
					t.Fatalf("omitted retries written: %s", out)
				}
			} else if !strings.Contains(out, tc.want) {
				t.Fatalf("output lacks %q: %s", tc.want, out)
			}
		})
	}
}

func TestConvert_ListsAndDocuments(t *testing.T) {
	const item = `{"apiVersion":"rss2discord.maverickd650.dev/v1alpha1","kind":"FeedGroup","metadata":{"name":"%s"},"spec":{"discordWebhookSecretRef":{"name":"h","key":"u"},"feeds":[{"rssUrl":"https://x.example.com/"}]}}`
	in := `null
---
apiVersion: v1
kind: List
items:
- ` + strings.Replace(item, "%s", "a", 1) + `
---
---
` + strings.Replace(item, "%s", "b", 1) + "\n"
	out, notes := convert(t, in)
	docs := strings.Split(out, "---\n")
	if len(docs) != 2 || !strings.Contains(docs[0], "name: a") || !strings.Contains(docs[1], "name: b") || notes != nil {
		t.Fatalf("out = %q, notes = %q", out, notes)
	}
	if strings.Contains(out, "namespace") || strings.Contains(out, "labels") || strings.Contains(out, "annotations") {
		t.Fatalf("empty metadata fields written: %s", out)
	}
	if empty, _ := convert(t, ""); empty != "" {
		t.Fatalf("empty input wrote %q", empty)
	}
}

func TestConvert_Errors(t *testing.T) {
	for name, in := range map[string]string{
		"not yaml":        "{",
		"other kind":      "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: x}\n",
		"v1beta1 already": "apiVersion: rss2discord.maverickd650.dev/v1beta1\nkind: FeedGroup\nmetadata: {name: x}\n",
		"unknown field":   "apiVersion: rss2discord.maverickd650.dev/v1alpha1\nkind: FeedGroup\nmetadata: {name: x}\nspec: {bogus: 1}\n",
		"bad list":        "apiVersion: v1\nkind: List\nitems: [1]\n",
	} {
		t.Run(name, func(t *testing.T) {
			if err := Convert(strings.NewReader(in), &strings.Builder{}, func(string) {}); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

type failingWriter struct{ after int }

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.after == 0 {
		return 0, errors.New("write failed")
	}
	w.after--
	return len(p), nil
}

func TestConvert_WriteErrors(t *testing.T) {
	const two = "apiVersion: rss2discord.maverickd650.dev/v1alpha1\nkind: FeedGroup\nmetadata: {name: a}\nspec: {feeds: [{rssUrl: 'https://a.example.com/'}]}\n---\n" +
		"apiVersion: rss2discord.maverickd650.dev/v1alpha1\nkind: FeedGroup\nmetadata: {name: b}\nspec: {feeds: [{rssUrl: 'https://b.example.com/'}]}\n"
	for after := range 3 {
		if err := Convert(strings.NewReader(two), &failingWriter{after: after}, func(string) {}); err == nil {
			t.Fatalf("write failure after %d writes not reported", after)
		}
	}
}

func TestNamer(t *testing.T) {
	n := newNamer()
	for _, tc := range []struct{ url, want string }{
		{"https://www.example.com/feed.xml", "example-com"},
		{"https://example.com/atom", "example-com-2"},
		{"https://blog.example.com/posts/index.rss", "blog-example-com-posts"},
		{"https://example.com/news/latest.json", "example-com-news-latest"},
		{"https://example.com/a_b/c", "example-com-a-b-c"},
		{"not a url", "not-a-url"},
		{"!!!", "feed"},
		{"https://" + strings.Repeat("a", 70) + ".com/", strings.Repeat("a", 63)},
		{"https://" + strings.Repeat("a", 70) + ".net/", strings.Repeat("a", 61) + "-2"},
	} {
		if got := n.name(tc.url); got != tc.want {
			t.Errorf("name(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}
