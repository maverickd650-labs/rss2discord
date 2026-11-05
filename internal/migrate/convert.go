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

// Package migrate converts v1alpha1 FeedGroup manifests to v1beta1.
//
// v1beta1 renames and restructures fields and makes each feed's name
// required, so a stored v1alpha1 object can't simply be re-read as v1beta1.
// Convert maps every v1alpha1 spec field to its v1beta1 equivalent, keeping
// what each feed actually sends: v1alpha1 resolved a feed's format and embed
// field by field against the group's, while a v1beta1 message replaces the
// group's whole, so a feed whose effective message differs from the group's
// gets its own. Status is dropped: it can't be applied, and the controller
// rebuilds it.
package migrate

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/maverickd650-labs/rss2discord/api/v1alpha1"
	"github.com/maverickd650-labs/rss2discord/api/v1beta1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

// maxNameLength is FeedSpec.Name's DNS-1123 label limit.
const maxNameLength = 63

// Convert reads v1alpha1 FeedGroups from r -- YAML or JSON, one or more
// documents, each a FeedGroup or a List of them (as `kubectl get -o yaml`
// prints) -- and writes them to w as v1beta1 YAML documents. warn receives one
// line per conversion the user should check by hand. A document that isn't a
// v1alpha1 FeedGroup is an error: nothing is written for it, and conversion
// stops.
func Convert(r io.Reader, w io.Writer, warn func(string)) error {
	decoder := utilyaml.NewYAMLOrJSONDecoder(r, 4096)
	first := true
	for {
		var doc unstructured.Unstructured
		if err := decoder.Decode(&doc.Object); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("reading input: %w", err)
		}
		if len(doc.Object) == 0 {
			continue
		}
		items := []unstructured.Unstructured{doc}
		if doc.IsList() {
			list, err := doc.ToList()
			if err != nil {
				return fmt.Errorf("reading list: %w", err)
			}
			items = list.Items
		}
		for _, item := range items {
			out, err := convertObject(item, warn)
			if err != nil {
				return err
			}
			if !first {
				if _, err := io.WriteString(w, "---\n"); err != nil {
					return err
				}
			}
			first = false
			if _, err := w.Write(out); err != nil {
				return err
			}
		}
	}
}

// convertObject converts one decoded FeedGroup to v1beta1 YAML.
func convertObject(obj unstructured.Unstructured, warn func(string)) ([]byte, error) {
	if gvk := obj.GroupVersionKind(); gvk != v1alpha1.GroupVersion.WithKind("FeedGroup") {
		return nil, fmt.Errorf("%s %q is not a %s FeedGroup", gvk, obj.GetName(), v1alpha1.GroupVersion)
	}
	var old v1alpha1.FeedGroup
	if err := runtime.DefaultUnstructuredConverter.FromUnstructuredWithValidation(obj.Object, &old, true); err != nil {
		return nil, fmt.Errorf("FeedGroup %q: %w", obj.GetName(), err)
	}
	where := "FeedGroup " + strconv.Quote(cmp.Or(old.Namespace+"/", "")+old.Name)
	spec := ConvertSpec(old.Spec, func(msg string) { warn(where + ": " + msg) })
	// v1alpha1's int32 retries can't tell an explicit 0 from an omitted field,
	// which v1beta1 defaults to 3, so the input decides.
	if _, set, _ := unstructured.NestedFieldNoCopy(obj.Object, "spec", "retries"); set && spec.Retries == nil {
		spec.Retries = new(int32(0))
	}

	metadata := map[string]any{"name": old.Name}
	if old.Namespace != "" {
		metadata["namespace"] = old.Namespace
	}
	if len(old.Labels) > 0 {
		metadata["labels"] = old.Labels
	}
	if annotations := userAnnotations(old.Annotations); len(annotations) > 0 {
		metadata["annotations"] = annotations
	}
	return yaml.Marshal(map[string]any{
		"apiVersion": v1beta1.GroupVersion.String(),
		"kind":       "FeedGroup",
		"metadata":   metadata,
		"spec":       spec,
	})
}

// userAnnotations drops kubectl's last-applied-configuration, which describes
// the v1alpha1 object and would make the first apply of the v1beta1 one
// compute a bogus three-way diff.
func userAnnotations(annotations map[string]string) map[string]string {
	out := make(map[string]string, len(annotations))
	for k, v := range annotations {
		if k != "kubectl.kubernetes.io/last-applied-configuration" {
			out[k] = v
		}
	}
	return out
}

// ConvertSpec converts a v1alpha1 spec. warn receives the conversions to check.
func ConvertSpec(old v1alpha1.FeedGroupSpec, warn func(string)) v1beta1.FeedGroupSpec {
	spec := v1beta1.FeedGroupSpec{
		DiscordWebhookSecretRef: old.DiscordWebhookSecretRef,
		Interval:                old.Interval,
		Username:                old.Username,
		AvatarURL:               old.AvatarURL,
		Retries:                 retriesOf(old.Retries),
		RetryInterval:           old.RetryInterval,
		CatchUpLimit:            old.CatchUpLimit,
		Message:                 effectiveMessage(old.Format, old.Embed, "spec", warn),
	}
	names := newNamer()
	for i, feed := range old.Feeds {
		where := fmt.Sprintf("spec.feeds[%d]", i)
		name := names.name(feed.RSSUrl)
		converted := v1beta1.FeedSpec{
			Name:    name,
			URL:     feed.RSSUrl,
			Suspend: feed.Paused,
			Filter:  convertFilter(feed.Filter),
		}
		// v1alpha1 resolved format and embed separately; the feed's own
		// message is needed only when its result differs from the group's.
		if feed.Format != "" || feed.Embed != nil {
			message := effectiveMessage(cmp.Or(feed.Format, old.Format), cmp.Or(feed.Embed, old.Embed), where, warn)
			if !sameJSON(message, spec.Message) {
				// A nil message means the default, but an unset feed message
				// inherits the group's: spell the default out.
				converted.Message = cmp.Or(message, &v1beta1.MessageSpec{Content: v1beta1.DefaultMessageContent})
			}
		}
		switch {
		case feed.ForumThreadID != "":
			converted.Thread = &v1beta1.ThreadTarget{ID: feed.ForumThreadID}
			if feed.ForumThreadName != "" {
				warn(where + ": forumThreadName dropped: forumThreadID took precedence over it, and v1beta1 rejects both")
			}
		case feed.ForumThreadName != "":
			converted.ForumPost = &v1beta1.ForumPostSpec{Name: feed.ForumThreadName}
		}
		spec.Feeds = append(spec.Feeds, converted)
	}
	return spec
}

// retriesOf is v1alpha1's retries as v1beta1's: nil (the default, 3) for 0,
// which in v1alpha1 also meant "unset" to typed clients. Convert restores an
// explicit 0 from the input.
func retriesOf(retries int32) *int32 {
	if retries == 0 {
		return nil
	}
	return &retries
}

// effectiveMessage is the v1beta1 message for what v1alpha1 sent with format
// and embed: the embed alone when it is enabled (v1alpha1 sent no content
// beside an embed), otherwise the format as content. nil means the default
// message. where locates the fields in warnings.
func effectiveMessage(format string, embed *v1alpha1.EmbedSpec, where string, warn func(string)) *v1beta1.MessageSpec {
	if embed == nil || !embed.Enabled {
		if format == "" {
			return nil
		}
		return &v1beta1.MessageSpec{Content: format}
	}
	e := &v1beta1.EmbedSpec{Description: embed.DescriptionFormat, Color: embed.Color}
	if embed.ImageStyle == "image" {
		e.ImageStyle = v1beta1.EmbedImageStyleImage
	}
	if embed.AuthorName != "" {
		e.Author = &v1beta1.EmbedAuthor{Name: literalTemplate(embed.AuthorName, where+".embed.authorName", warn)}
	}
	if embed.FooterText != "" {
		e.Footer = &v1beta1.EmbedFooter{Text: literalTemplate(embed.FooterText, where+".embed.footerText", warn)}
	}
	return &v1beta1.MessageSpec{Embed: e}
}

// literalTemplate returns text, which v1alpha1 sent verbatim and v1beta1
// renders as a template. Text without "{{" renders as itself; text with it
// is kept, but flagged, since it now means a template action.
func literalTemplate(text, where string, warn func(string)) string {
	if strings.Contains(text, "{{") {
		warn(where + ` contains "{{", which v1beta1 renders as a template; check it still reads as intended`)
	}
	return text
}

// convertFilter moves v1alpha1's regex and keywords into include.
func convertFilter(old *v1alpha1.Filter) *v1beta1.Filter {
	if old == nil || (old.Regex == "" && len(old.Keywords) == 0) {
		return nil
	}
	return &v1beta1.Filter{Include: &v1beta1.FilterRules{Regex: old.Regex, Keywords: old.Keywords}}
}

// sameJSON reports whether a and b serialize identically.
func sameJSON(a, b *v1beta1.MessageSpec) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return bytes.Equal(ja, jb)
}

// namer derives unique feed names from URLs.
type namer struct{ used map[string]bool }

func newNamer() *namer { return &namer{used: map[string]bool{}} }

var nonLabel = regexp.MustCompile(`[^a-z0-9]+`)

// name returns a DNS-1123 label for feedURL that no earlier call returned:
// its host and path, lowercased, with "www.", the scheme, a "feed"/"rss"/
// "atom" file name and the extension dropped, every other run of characters a
// label can't hold turned into "-", and a numeric suffix added on a clash.
func (n *namer) name(feedURL string) string {
	base := labelFor(feedURL)
	name := base
	for i := 2; n.used[name]; i++ {
		suffix := "-" + strconv.Itoa(i)
		name = strings.TrimRight(truncate(base, maxNameLength-len(suffix)), "-") + suffix
	}
	n.used[name] = true
	return name
}

// feedFileStems are final path segments that say nothing about a feed
// (".../feed.xml", ".../rss", ".../index.atom"), so labelFor drops them.
var feedFileStems = map[string]bool{"": true, "feed": true, "feeds": true, "rss": true, "atom": true, "index": true}

// labelFor is name's suffix-free label for feedURL ("feed" if nothing usable
// is left).
func labelFor(feedURL string) string {
	text := feedURL
	if u, err := url.Parse(feedURL); err == nil && u.Host != "" {
		segments := strings.Split(strings.Trim(u.Path, "/"), "/")
		last := segments[len(segments)-1]
		if dot := strings.LastIndexByte(last, '.'); dot >= 0 {
			last = last[:dot]
		}
		segments = segments[:len(segments)-1]
		if !feedFileStems[strings.ToLower(last)] {
			segments = append(segments, last)
		}
		text = strings.Join(append([]string{strings.TrimPrefix(u.Hostname(), "www.")}, segments...), "-")
	}
	label := strings.Trim(nonLabel.ReplaceAllString(strings.ToLower(text), "-"), "-")
	label = strings.TrimRight(truncate(label, maxNameLength), "-")
	return cmp.Or(label, "feed")
}

// truncate cuts s, which is ASCII, to at most n bytes.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
