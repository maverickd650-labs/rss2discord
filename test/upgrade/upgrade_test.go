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

// Package upgrade rehearses the v1alpha1 -> v1beta1 upgrade documented in
// docs/schema.md against a real API server (envtest): the installed CRDs'
// admission rules, conversion of exported manifests, an upgrade interrupted
// at each step, and rollback from saved manifests.
package upgrade

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	"github.com/maverickd650-labs/rss2discord/internal/migrate"
)

const (
	crdName   = "feedgroups.rss2discord.maverickd650.dev"
	namespace = "default"
)

var (
	alphaGVR = feedGroupsAt("v1alpha1")
	betaGVR  = feedGroupsAt("v1beta1")
)

// bothGroups is names() of the FeedGroups v1alpha1Manifests creates.
const bothGroups = "embeds,news"

func feedGroupsAt(version string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "rss2discord.maverickd650.dev", Version: version, Resource: "feedgroups"}
}

// cluster is one envtest API server with clients for CRDs and FeedGroups.
type cluster struct {
	t   *testing.T
	ctx context.Context
	crd *apiextensionsclient.Clientset
	dyn *dynamic.DynamicClient
}

func startCluster(t *testing.T) *cluster {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set; run via `mise run test`")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("starting envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	return &cluster{
		t:   t,
		ctx: t.Context(),
		crd: apiextensionsclient.NewForConfigOrDie(cfg),
		dyn: dynamic.NewForConfigOrDie(cfg),
	}
}

// readCRD reads the FeedGroup CRD from a file that may hold other documents
// (dist/install.yaml) or Helm-rendered output.
func readCRD(t *testing.T, path string) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return crdFrom(t, raw, path)
}

func crdFrom(t *testing.T, raw []byte, source string) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	for {
		var doc map[string]any
		if err := decoder.Decode(&doc); err != nil {
			t.Fatalf("%s: no %s CRD found (%v)", source, crdName, err)
		}
		if doc["kind"] != "CustomResourceDefinition" {
			continue
		}
		out, _ := yaml.Marshal(doc)
		var crd apiextensionsv1.CustomResourceDefinition
		if err := yaml.UnmarshalStrict(out, &crd); err != nil {
			t.Fatalf("%s: %v", source, err)
		}
		if crd.Name == crdName {
			return &crd
		}
	}
}

func repoPath(parts ...string) string {
	return filepath.Join(append([]string{"..", ".."}, parts...)...)
}

// installCRD creates or replaces the CRD with want and waits until it is
// Established. It returns the API server's error, if any, unwaited.
func (c *cluster) installCRD(want *apiextensionsv1.CustomResourceDefinition) error {
	c.t.Helper()
	crds := c.crd.ApiextensionsV1().CustomResourceDefinitions()
	current, err := crds.Get(c.ctx, crdName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		_, err = crds.Create(c.ctx, want.DeepCopy(), metav1.CreateOptions{})
	case err == nil:
		next := want.DeepCopy()
		next.ResourceVersion = current.ResourceVersion
		_, err = crds.Update(c.ctx, next, metav1.UpdateOptions{})
	}
	if err != nil {
		return err
	}
	c.waitEstablished(want)
	return nil
}

func (c *cluster) mustInstallCRD(want *apiextensionsv1.CustomResourceDefinition) {
	c.t.Helper()
	if err := c.installCRD(want); err != nil {
		c.t.Fatalf("installing CRD: %v", err)
	}
}

// waitEstablished waits until the CRD is Established and every version want
// serves is discoverable through the dynamic client.
func (c *cluster) waitEstablished(want *apiextensionsv1.CustomResourceDefinition) {
	c.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		crd, err := c.crd.ApiextensionsV1().CustomResourceDefinitions().Get(c.ctx, crdName, metav1.GetOptions{})
		if err == nil && established(crd) && c.servesExactly(want) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	c.t.Fatal("CRD was not established in time")
}

func established(crd *apiextensionsv1.CustomResourceDefinition) bool {
	for _, cond := range crd.Status.Conditions {
		if cond.Type == apiextensionsv1.Established && cond.Status == apiextensionsv1.ConditionTrue {
			return true
		}
	}
	return false
}

// servesExactly reports whether listing works for each served version of want
// and fails for each unserved one.
func (c *cluster) servesExactly(want *apiextensionsv1.CustomResourceDefinition) bool {
	for _, v := range want.Spec.Versions {
		gvr := schema.GroupVersionResource{Group: want.Spec.Group, Version: v.Name, Resource: want.Spec.Names.Plural}
		_, err := c.dyn.Resource(gvr).Namespace(namespace).List(c.ctx, metav1.ListOptions{})
		if (err == nil) != v.Served {
			return false
		}
	}
	return true
}

func (c *cluster) storedVersions() []string {
	c.t.Helper()
	crd, err := c.crd.ApiextensionsV1().CustomResourceDefinitions().Get(c.ctx, crdName, metav1.GetOptions{})
	if err != nil {
		c.t.Fatal(err)
	}
	return crd.Status.StoredVersions
}

// setStoredVersions does what docs/schema.md's storedVersions patch does.
func (c *cluster) setStoredVersions(versions ...string) error {
	crds := c.crd.ApiextensionsV1().CustomResourceDefinitions()
	crd, err := crds.Get(c.ctx, crdName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	crd.Status.StoredVersions = versions
	_, err = crds.UpdateStatus(c.ctx, crd, metav1.UpdateOptions{})
	return err
}

func (c *cluster) deleteCRD() {
	c.t.Helper()
	crds := c.crd.ApiextensionsV1().CustomResourceDefinitions()
	if err := crds.Delete(c.ctx, crdName, metav1.DeleteOptions{}); err != nil {
		c.t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := crds.Get(c.ctx, crdName, metav1.GetOptions{}); apierrors.IsNotFound(err) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	c.t.Fatal("CRD was not deleted in time")
}

// objects decodes every document in manifest.
func objects(t *testing.T, manifest []byte) []*unstructured.Unstructured {
	t.Helper()
	var out []*unstructured.Unstructured
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(manifest), 4096)
	for {
		obj := &unstructured.Unstructured{}
		if err := decoder.Decode(&obj.Object); err != nil {
			return out
		}
		if len(obj.Object) > 0 {
			out = append(out, obj)
		}
	}
}

func (c *cluster) create(gvr schema.GroupVersionResource, obj *unstructured.Unstructured) error {
	_, err := c.dyn.Resource(gvr).Namespace(namespace).Create(c.ctx, obj.DeepCopy(), metav1.CreateOptions{})
	return err
}

func (c *cluster) apply(gvr schema.GroupVersionResource, manifest []byte) {
	c.t.Helper()
	for _, obj := range objects(c.t, manifest) {
		if err := c.create(gvr, obj); err != nil {
			c.t.Fatalf("creating %s %s: %v", gvr.Version, obj.GetName(), err)
		}
	}
}

func (c *cluster) list(gvr schema.GroupVersionResource) ([]unstructured.Unstructured, error) {
	list, err := c.dyn.Resource(gvr).Namespace(namespace).List(c.ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (c *cluster) deleteAll(gvr schema.GroupVersionResource) {
	c.t.Helper()
	resource := c.dyn.Resource(gvr).Namespace(namespace)
	if err := resource.DeleteCollection(c.ctx, metav1.DeleteOptions{}, metav1.ListOptions{}); err != nil {
		c.t.Fatalf("deleting %s FeedGroups: %v", gvr.Version, err)
	}
}

// export is `kubectl get feedgroups -o yaml`: a List of the objects as the API
// server returns them.
func (c *cluster) export(gvr schema.GroupVersionResource) []byte {
	c.t.Helper()
	items, err := c.list(gvr)
	if err != nil {
		c.t.Fatal(err)
	}
	list := map[string]any{"apiVersion": "v1", "kind": "List", "items": []any{}}
	for _, item := range items {
		list["items"] = append(list["items"].([]any), item.Object)
	}
	out, err := yaml.Marshal(list)
	if err != nil {
		c.t.Fatal(err)
	}
	return out
}

func convert(t *testing.T, exported []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := migrate.Convert(bytes.NewReader(exported), &out, func(string) {}); err != nil {
		t.Fatalf("converting: %v", err)
	}
	return out.Bytes()
}

// v1alpha1Manifests are FeedGroups covering every v1alpha1 field the
// converter maps, as a user would have applied them.
const v1alpha1Manifests = `apiVersion: rss2discord.maverickd650.dev/v1alpha1
kind: FeedGroup
metadata: {name: news}
spec:
  discordWebhookSecretRef: {name: hook, key: url}
  interval: 15m
  retries: 0
  retryInterval: 1m
  catchUpLimit: 0
  username: News Bot
  avatarURL: https://example.com/a.png
  format: "**{{.Title}}**"
  feeds:
    - rssUrl: https://www.example.com/blog/feed.xml
      paused: true
      filter: {regex: Go, keywords: [release]}
    - rssUrl: https://example.com/blog/rss
      format: "{{.Link}}"
      forumThreadName: "{{.Title}}"
    - rssUrl: https://other.example.com/
      embed:
        enabled: true
        color: "#ffffff"
        descriptionFormat: "{{.Description}}"
        imageStyle: image
        authorName: Other
        footerText: Footer
      forumThreadID: "123"
---
apiVersion: rss2discord.maverickd650.dev/v1alpha1
kind: FeedGroup
metadata: {name: embeds}
spec:
  discordWebhookSecretRef: {name: hook, key: url}
  embed: {enabled: true, imageStyle: thumbnail}
  feeds:
    - rssUrl: https://a.example.com/feed
    - rssUrl: https://b.example.com/feed
      embed: {enabled: false}
`

// seedV1alpha1 installs the last v1alpha1 release's CRD and creates the
// v1alpha1 FeedGroups, as a cluster on the old release would hold them. It
// returns the user's saved manifests and the export taken before upgrading.
func seedV1alpha1(c *cluster) (saved, exported []byte) {
	c.t.Helper()
	c.mustInstallCRD(readCRD(c.t, "testdata/v1alpha1-crd.yaml"))
	c.apply(alphaGVR, []byte(v1alpha1Manifests))
	return []byte(v1alpha1Manifests), c.export(alphaGVR)
}

func firstFeedURL(obj *unstructured.Unstructured) string {
	feeds, _, _ := unstructured.NestedSlice(obj.Object, "spec", "feeds")
	feed, _ := feeds[0].(map[string]any)
	url, _ := feed["rssUrl"].(string)
	return url
}

func names(items []unstructured.Unstructured) string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.GetName())
	}
	return strings.Join(out, ",")
}

// TestInstalledCRDsMatchGenerated checks that what users install -- the
// kustomize manifest and the Helm chart, rendered by helm -- carries the
// generated schema unchanged, so the admission rehearsal below covers what
// they will run.
func TestInstalledCRDsMatchGenerated(t *testing.T) {
	t.Parallel()
	generated := readCRD(t, repoPath("config", "crd", "bases", "rss2discord.maverickd650.dev_feedgroups.yaml"))
	if diff := specDiff(generated, readCRD(t, repoPath("dist", "install.yaml"))); diff != "" {
		t.Errorf("dist/install.yaml: CRD spec differs from the generated one: %s", diff)
	}
	out, err := exec.Command("helm", "template", "rss2discord-operator", repoPath("dist", "chart"),
		"--show-only", "templates/crd/feedgroups.rss2discord.maverickd650.dev.yaml").CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	if diff := specDiff(generated, crdFrom(t, out, "helm template")); diff != "" {
		t.Errorf("Helm chart: CRD spec differs from the generated one: %s", diff)
	}
}

func specDiff(a, b *apiextensionsv1.CustomResourceDefinition) string {
	ja, _ := yaml.Marshal(a.Spec)
	jb, _ := yaml.Marshal(b.Spec)
	if bytes.Equal(ja, jb) {
		return ""
	}
	return fmt.Sprintf("%d vs %d bytes", len(ja), len(jb))
}

// feedGroup builds a v1beta1 FeedGroup from a YAML spec.
func feedGroup(t *testing.T, name, spec string) *unstructured.Unstructured {
	t.Helper()
	var s map[string]any
	if err := yaml.Unmarshal([]byte(spec), &s); err != nil {
		t.Fatal(err)
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "rss2discord.maverickd650.dev/v1beta1",
		"kind":       "FeedGroup",
		"metadata":   map[string]any{"name": name}, //nolint:goconst // a Kubernetes field name, not a shared value
		"spec":       s,
	}}
}

// TestInstalledCRDAdmission applies FeedGroups to the CRD from
// dist/install.yaml and checks every uniqueness and field rule a v1 schema
// relies on is enforced by the API server, not just by the controller.
func TestInstalledCRDAdmission(t *testing.T) {
	t.Parallel()
	c := startCluster(t)
	c.mustInstallCRD(readCRD(t, repoPath("dist", "install.yaml")))

	const (
		hook  = "discordWebhookSecretRef: {name: hook, key: url}\n"
		urlA  = "url: 'https://a.example.com/'"
		feedA = "{name: a, " + urlA + "}"
		// oneFeed is a valid feeds list, for cases about other fields.
		oneFeed = "feeds: [" + feedA + "]"
	)
	// withFeed is a spec with hook, extra and oneFeed.
	withFeed := func(extra string) string { return hook + extra + "\n" + oneFeed }
	// feedWith is a spec whose only feed is feedA plus fields.
	feedWith := func(fields string) string { return hook + "feeds: [{name: a, " + urlA + ", " + fields + "}]" }
	rejected := []struct{ name, spec, want string }{
		{"duplicate-name", hook + "feeds: [" + feedA + ", {name: a, url: 'https://b.example.com/'}]", "Duplicate value"},
		{"duplicate-url", hook + "feeds: [" + feedA + ", {name: b, " + urlA + "}]", "url must be unique"},
		{"missing-name", hook + "feeds: [{" + urlA + "}]", "name"},
		{"bad-name", hook + "feeds: [{name: Not_A_Label, " + urlA + "}]", "name"},
		{"long-name", hook + "feeds: [{name: " + strings.Repeat("a", 64) + ", " + urlA + "}]", "name"},
		{"missing-url", hook + "feeds: [{name: a}]", "url"},
		{"non-http-url", hook + "feeds: [{name: a, url: 'ftp://a.example.com/'}]", "url"},
		{"no-feeds", hook + "feeds: []", "feeds"},
		{"missing-spec", "", "spec"},
		{"missing-webhook", oneFeed, "discordWebhookSecretRef"},
		{"short-interval", withFeed("interval: 30s"), "interval must be at least 1m"},
		{"negative-retries", withFeed("retries: -1"), "retries"},
		{"empty-message", withFeed("message: {silent: true}"), "a message needs content, embed, or both"},
		{"previews-with-embed", withFeed("message: {embed: {}, suppressLinkPreviews: true}"), "suppressLinkPreviews"},
		{"thread-and-post", feedWith("thread: {id: '1'}, forumPost: {name: x}"), "mutually exclusive"},
		{"six-tags", feedWith("forumPost: {name: x, appliedTagIDs: ['1','2','3','4','5','6']}"), "appliedTagIDs"},
		{"bad-image-style", withFeed("message: {embed: {imageStyle: image}}"), "imageStyle"},
		{"author-without-name", withFeed("message: {embed: {author: {url: 'https://x.example.com/'}}}"), "author.name"},
		{"bad-mention", withFeed("message: {content: x, mentions: {roleIDs: [everyone]}}"), "roleIDs"},
		{"bad-username", withFeed("username: 'news: daily'"), "Discord rejects usernames"},
		{"empty-filter", feedWith("filter: {}"), "filter"},
		{"empty-rules", feedWith("filter: {include: {}}"), "include"},
		{"unknown-field", withFeed("format: x"), "format"},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			obj := feedGroup(t, tc.name, tc.spec)
			if tc.spec == "" {
				delete(obj.Object, "spec")
			}
			err := c.createStrict(obj)
			if err == nil {
				t.Fatalf("accepted; want an error mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}

	t.Run("defaults and a full spec are accepted", func(t *testing.T) {
		obj := feedGroup(t, "full", hook+`message:
  content: "New: {{.Title}}"
  mentions: {roleIDs: ["1"], userIDs: ["2"]}
  silent: true
  embed: {title: "{{.Title}}", author: {name: "{{.Author}}"}, footer: {text: f}, imageStyle: None, timestamp: None}
feeds:
  - {name: a, url: 'https://a.example.com/', forumPost: {name: "{{.Title}}", appliedTagIDs: ['1','2','3','4','5']}}
  - {name: b, url: 'https://b.example.com/', thread: {id: '12345678901234567890'}, suspend: true,
     filter: {include: {categories: [go]}, exclude: {keywords: [ad]}},
     message: {content: x, suppressLinkPreviews: true}}
`)
		if err := c.createStrict(obj); err != nil {
			t.Fatal(err)
		}
		got, err := c.dyn.Resource(betaGVR).Namespace(namespace).Get(c.ctx, "full", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defaults := map[string]any{
			"interval": "30m", "retryInterval": "5m", "retries": int64(3), "catchUpLimit": int64(5), "suspend": false,
		}
		for path, want := range defaults {
			if v, _, _ := unstructured.NestedFieldNoCopy(got.Object, "spec", path); v != want {
				t.Errorf("spec.%s = %v, want default %v", path, v, want)
			}
		}
	})
}

// createStrict creates obj with field validation Strict, as `kubectl apply`
// does by default, so an unknown field is an error rather than pruned.
func (c *cluster) createStrict(obj *unstructured.Unstructured) error {
	opts := metav1.CreateOptions{FieldValidation: "Strict"}
	_, err := c.dyn.Resource(betaGVR).Namespace(namespace).Create(c.ctx, obj, opts)
	return err
}

// TestUpgradeAsDocumented follows docs/schema.md's upgrade steps exactly.
func TestUpgradeAsDocumented(t *testing.T) {
	t.Parallel()
	c := startCluster(t)
	_, exported := seedV1alpha1(c)

	// 1-2. Export (done by seedV1alpha1) and convert.
	converted := convert(t, exported)

	// 3. Delete the old FeedGroups, then upgrade (install the new CRD).
	c.deleteAll(alphaGVR)
	c.mustInstallCRD(readCRD(t, repoPath("dist", "install.yaml")))

	// 4. Apply the converted manifests: every one must pass the new schema.
	c.apply(betaGVR, converted)
	items, err := c.list(betaGVR)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(items); got != bothGroups {
		t.Fatalf("FeedGroups after upgrade = %s", got)
	}
	news := items[1]
	if v, _, _ := unstructured.NestedInt64(news.Object, "spec", "retries"); v != 0 {
		t.Errorf("news retries = %d, want the explicit 0 carried over", v)
	}
	feeds, _, _ := unstructured.NestedSlice(news.Object, "spec", "feeds")
	if len(feeds) != 3 {
		t.Fatalf("news feeds = %v", feeds)
	}

	// 5. storedVersions still lists v1alpha1 until it is cleaned up; the
	// documented patch succeeds now nothing is stored as v1alpha1.
	if got := c.storedVersions(); strings.Join(got, ",") != "v1alpha1,v1beta1" {
		t.Fatalf("storedVersions = %v", got)
	}
	if err := c.setStoredVersions("v1beta1"); err != nil {
		t.Fatalf("storedVersions cleanup: %v", err)
	}
}

// TestInterruptedUpgrade covers stopping, or skipping a step, part way
// through the documented upgrade.
func TestInterruptedUpgrade(t *testing.T) {
	t.Parallel()
	t.Run("stopped after deleting, before installing the new CRD", func(t *testing.T) {
		t.Parallel()
		c := startCluster(t)
		saved, exported := seedV1alpha1(c)
		c.deleteAll(alphaGVR)
		// Recovery forward: the export is all that's needed.
		c.mustInstallCRD(readCRD(t, repoPath("dist", "install.yaml")))
		c.apply(betaGVR, convert(t, exported))
		// Recovery back instead (a fresh cluster in the same state): the
		// saved manifests re-create the v1alpha1 objects on the old CRD.
		c2 := startCluster(t)
		_, _ = seedV1alpha1(c2)
		c2.deleteAll(alphaGVR)
		c2.apply(alphaGVR, saved)
	})

	t.Run("new CRD installed without deleting the old FeedGroups", func(t *testing.T) {
		t.Parallel()
		c := startCluster(t)
		_, exported := seedV1alpha1(c)
		c.mustInstallCRD(readCRD(t, repoPath("dist", "install.yaml")))

		// The stored v1alpha1 objects read back as v1beta1 with their
		// renamed fields pruned: they are invalid and can't be updated, so
		// they must be deleted, not fixed in place.
		items, err := c.list(betaGVR)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 2 {
			t.Fatalf("stored objects visible as v1beta1 = %d, want 2", len(items))
		}
		feeds, _, _ := unstructured.NestedSlice(items[1].Object, "spec", "feeds")
		feed, _ := feeds[0].(map[string]any)
		if _, hasURL := feed["url"]; hasURL {
			t.Fatalf("expected the v1alpha1 feed to have no url as v1beta1, got %v", feed)
		}
		stale := c.dyn.Resource(betaGVR).Namespace(namespace)
		if _, err := stale.Update(c.ctx, &items[1], metav1.UpdateOptions{}); err == nil {
			t.Fatal("updating a stored v1alpha1 object as v1beta1 succeeded; want a validation error")
		}

		// Recovery: delete them and apply the converted export.
		c.deleteAll(betaGVR)
		c.apply(betaGVR, convert(t, exported))
		if items, _ := c.list(betaGVR); names(items) != bothGroups {
			t.Fatalf("after recovery: %s", names(items))
		}
	})

	t.Run("forgotten export: serving v1alpha1 again recovers the objects", func(t *testing.T) {
		t.Parallel()
		c := startCluster(t)
		saved, _ := seedV1alpha1(c)
		c.mustInstallCRD(readCRD(t, repoPath("dist", "install.yaml")))

		// The new CRD still carries the v1alpha1 schema, unserved. Serving it
		// and making it the storage version again reads the stored objects
		// back whole, so they can be exported after all.
		// docs/schema.md's patch, verbatim.
		const patch = `[
    {"op":"replace","path":"/spec/versions/0/served","value":true},
    {"op":"replace","path":"/spec/versions/0/storage","value":true},
    {"op":"replace","path":"/spec/versions/1/served","value":false},
    {"op":"replace","path":"/spec/versions/1/storage","value":false}]`
		crds := c.crd.ApiextensionsV1().CustomResourceDefinitions()
		if _, err := crds.Patch(c.ctx, crdName, types.JSONPatchType, []byte(patch), metav1.PatchOptions{}); err != nil {
			t.Fatalf("documented recovery patch: %v", err)
		}
		c.waitEstablished(readCRD(t, "testdata/v1alpha1-crd.yaml"))
		exported := c.export(alphaGVR)
		for _, want := range objects(t, saved) {
			if !bytes.Contains(exported, []byte("rssUrl: "+firstFeedURL(want))) {
				t.Fatalf("recovered export lacks %s's feeds:\n%s", want.GetName(), exported)
			}
		}
		_ = convert(t, exported)
	})

	t.Run("export taken after installing the new CRD can't be converted", func(t *testing.T) {
		t.Parallel()
		c := startCluster(t)
		_, _ = seedV1alpha1(c)
		c.mustInstallCRD(readCRD(t, repoPath("dist", "install.yaml")))
		// The old version is no longer served, so the only export left is
		// the pruned v1beta1 view, which the converter refuses: the export
		// must come first.
		var out bytes.Buffer
		if err := migrate.Convert(bytes.NewReader(c.export(betaGVR)), &out, func(string) {}); err == nil {
			t.Fatal("converting a post-upgrade export succeeded; want an error")
		}
	})
}

// TestRollback rolls a converted cluster back to the v1alpha1 release with
// the saved manifests.
func TestRollback(t *testing.T) {
	t.Parallel()
	upgraded := func(t *testing.T) (*cluster, []byte) {
		c := startCluster(t)
		saved, exported := seedV1alpha1(c)
		c.deleteAll(alphaGVR)
		c.mustInstallCRD(readCRD(t, repoPath("dist", "install.yaml")))
		c.apply(betaGVR, convert(t, exported))
		return c, saved
	}
	oldCRD := readCRD(t, "testdata/v1alpha1-crd.yaml")

	t.Run("reinstalling the old CRD in place is refused", func(t *testing.T) {
		t.Parallel()
		c, _ := upgraded(t)
		c.deleteAll(betaGVR)
		// Helm rollback or kubectl apply of the old CRD: v1beta1 is in
		// storedVersions, and the old CRD doesn't list it.
		err := c.installCRD(oldCRD)
		if err == nil || !strings.Contains(err.Error(), "storedVersions") {
			t.Fatalf("installing the old CRD over the new one: %v; want a storedVersions error", err)
		}
		// The documented storedVersions cleanup doesn't help: it leaves
		// v1beta1, which the old CRD still doesn't list.
		if err := c.setStoredVersions("v1beta1"); err != nil {
			t.Fatal(err)
		}
		if err := c.installCRD(oldCRD); err == nil {
			t.Fatal("installing the old CRD after the storedVersions cleanup succeeded; want it refused")
		}
	})

	t.Run("deleting the CRD, then reinstalling the old release, works", func(t *testing.T) {
		t.Parallel()
		c, saved := upgraded(t)
		c.deleteAll(betaGVR)
		c.deleteCRD()
		c.mustInstallCRD(oldCRD)
		c.apply(alphaGVR, saved)
		if items, err := c.list(alphaGVR); err != nil || names(items) != bothGroups {
			t.Fatalf("after rollback: %s (%v)", names(items), err)
		}
	})
}
