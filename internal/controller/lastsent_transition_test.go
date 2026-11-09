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

package controller

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

const (
	transitionGroup   = "transition.rss2discord.test"
	transitionFeed    = "feed-a"
	transitionManager = "feedgroup-controller"
)

// loadRealFeedGroupCRD reads the generated FeedGroup CRD from config/crd/bases.
func loadRealFeedGroupCRD() *apiextensionsv1.CustomResourceDefinition {
	GinkgoHelper()
	files, err := filepath.Glob(filepath.Join("..", "..", "config", "crd", "bases", "*_feedgroups.yaml"))
	Expect(err).NotTo(HaveOccurred())
	Expect(files).To(HaveLen(1), "expected exactly one FeedGroup CRD file")
	raw, err := os.ReadFile(files[0])
	Expect(err).NotTo(HaveOccurred())
	crd := &apiextensionsv1.CustomResourceDefinition{}
	Expect(yaml.Unmarshal(raw, crd)).To(Succeed())
	return crd
}

// lastSentSchema walks to status.feeds[].lastSent in the CRD's first version.
func lastSentSchema(crd *apiextensionsv1.CustomResourceDefinition) *apiextensionsv1.JSONSchemaProps {
	GinkgoHelper()
	root := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	feeds := root.Properties["status"].Properties["feeds"]
	lastSent := feeds.Items.Schema.Properties["lastSent"]
	return &lastSent
}

// transitionCRD returns a renamed copy of the real CRD (so the suite's real CRD
// is untouched). With atomic false, lastSent's map type is removed, which is
// the schema earlier releases shipped.
func transitionCRD(real *apiextensionsv1.CustomResourceDefinition, atomic bool) *apiextensionsv1.CustomResourceDefinition {
	GinkgoHelper()
	crd := real.DeepCopy()
	crd.ObjectMeta = metav1.ObjectMeta{Name: crd.Spec.Names.Plural + "." + transitionGroup}
	crd.Spec.Group = transitionGroup
	crd.Status = apiextensionsv1.CustomResourceDefinitionStatus{}
	if atomic {
		return crd
	}
	feeds := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"].Properties["feeds"]
	lastSent := feeds.Items.Schema.Properties["lastSent"]
	lastSent.XMapType = nil
	feeds.Items.Schema.Properties["lastSent"] = lastSent
	return crd
}

func crdEstablished(c client.Client, name string) bool {
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, crd); err != nil {
		return false
	}
	for _, cond := range crd.Status.Conditions {
		if cond.Type == apiextensionsv1.Established {
			return cond.Status == apiextensionsv1.ConditionTrue
		}
	}
	return false
}

func transitionGVK(crd *apiextensionsv1.CustomResourceDefinition) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: transitionGroup, Version: crd.Spec.Versions[0].Name, Kind: crd.Spec.Names.Kind}
}

// newTransitionObject is a minimal valid FeedGroup of the renamed kind.
func newTransitionObject(gvk schema.GroupVersionKind) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"discordWebhookSecretRef": map[string]any{labelName: "any-secret", "key": secretURLKey},
			"feeds":                   []any{map[string]any{labelName: transitionFeed, "url": "https://example.com/feed.xml"}},
		},
	}}
	obj.SetGroupVersionKind(gvk)
	obj.SetName("transition")
	obj.SetNamespace(testNamespace)
	return obj
}

// lastSentApply is a status-only apply payload carrying the given lastSent keys.
func lastSentApply(gvk schema.GroupVersionKind, keys []string) *unstructured.Unstructured {
	sent := map[string]any{}
	for _, k := range keys {
		sent[k] = "2026-01-01T00:00:00Z"
	}
	feed := map[string]any{labelName: transitionFeed}
	if len(keys) > 0 {
		feed["lastSent"] = sent
	}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{"feeds": []any{feed}},
	}}
	obj.SetGroupVersionKind(gvk)
	obj.SetName("transition")
	obj.SetNamespace(testNamespace)
	return obj
}

func applyLastSent(c client.Client, gvk schema.GroupVersionKind, keys []string) error {
	return c.Status().Apply(context.Background(),
		client.ApplyConfigurationFromUnstructured(lastSentApply(gvk, keys)),
		client.FieldOwner(transitionManager), client.ForceOwnership)
}

// managedLastSent returns the raw managedFields JSON the status-subresource
// entry of the controller's manager holds for lastSent ("" if it holds none),
// and the full entry JSON for failure messages.
func managedLastSent(obj *unstructured.Unstructured) (lastSent map[string]any, full string) {
	GinkgoHelper()
	for _, mf := range obj.GetManagedFields() {
		if !ownsTransitionStatus(mf) {
			continue
		}
		full = mf.FieldsV1.GetRawString()
		var tree map[string]any
		Expect(json.Unmarshal(mf.FieldsV1.GetRawBytes(), &tree)).To(Succeed())
		lastSent = descend(tree, "f:status", "f:feeds", `k:{"name":"`+transitionFeed+`"}`, "f:lastSent")
	}
	return lastSent, full
}

// ownsTransitionStatus reports whether mf is the transition manager's entry
// for the status subresource.
func ownsTransitionStatus(mf metav1.ManagedFieldsEntry) bool {
	if mf.Manager != transitionManager {
		return false
	}
	if mf.Subresource != "status" {
		return false
	}
	return mf.FieldsV1 != nil
}

// descend follows keys through nested JSON objects; nil if any is missing.
func descend(tree map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		next, ok := tree[k].(map[string]any)
		if !ok {
			return nil
		}
		tree = next
	}
	return tree
}

func readTransitionObject(c client.Client, gvk schema.GroupVersionKind) *unstructured.Unstructured {
	GinkgoHelper()
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(gvk)
	Expect(c.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "transition"}, got)).To(Succeed())
	return got
}

func storedLastSentKeys(obj *unstructured.Unstructured) []string {
	feeds, _, err := unstructured.NestedSlice(obj.Object, "status", "feeds")
	Expect(err).NotTo(HaveOccurred())
	Expect(feeds).To(HaveLen(1))
	sent, _ := feeds[0].(map[string]any)["lastSent"].(map[string]any)
	keys := make([]string, 0, len(sent))
	for k := range sent {
		keys = append(keys, k)
	}
	return keys
}

// lastSentIsLeaf reports whether the stored ownership of lastSent is the
// atomic shape ({} with no children).
func lastSentIsLeaf(obj *unstructured.Unstructured) bool {
	owned, _ := managedLastSent(obj)
	return owned != nil && len(owned) == 0
}

// applyUntilAtomic keeps applying keys until the API server reflects the
// atomic schema (lastSent owned as a leaf), failing on any apply error.
func applyUntilAtomic(c client.Client, gvk schema.GroupVersionKind, keys []string) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		g.Expect(applyLastSent(c, gvk, keys)).To(Succeed(), "same-manager apply must not conflict or error after the schema became atomic")
		obj := readTransitionObject(c, gvk)
		g.Expect(storedLastSentKeys(obj)).To(ConsistOf(keys), "lastSent must hold exactly the keys just sent")
		_, full := managedLastSent(obj)
		g.Expect(lastSentIsLeaf(obj)).To(BeTrue(), "lastSent ownership should be atomic (leaf); managedFields: %s", full)
	}).WithTimeout(30 * time.Second).WithPolling(250 * time.Millisecond).Should(Succeed())
}

var _ = Describe("lastSent atomic map-type transition", func() {
	It("lets the same field manager re-apply after lastSent goes from granular to atomic", func() {
		bg := context.Background()
		realCRD := loadRealFeedGroupCRD()
		Expect(lastSentSchema(realCRD).XMapType).NotTo(BeNil(), "real CRD lost x-kubernetes-map-type on status.feeds[].lastSent")
		Expect(*lastSentSchema(realCRD).XMapType).To(Equal("atomic"))

		crdScheme := runtime.NewScheme()
		Expect(clientgoscheme.AddToScheme(crdScheme)).To(Succeed())
		Expect(apiextensionsv1.AddToScheme(crdScheme)).To(Succeed())
		crdClient, err := client.New(cfg, client.Options{Scheme: crdScheme})
		Expect(err).NotTo(HaveOccurred())

		oldCRD := transitionCRD(realCRD, false)
		Expect(lastSentSchema(oldCRD).XMapType).To(BeNil())
		Expect(crdClient.Create(bg, oldCRD)).To(Succeed())
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(crdClient.Delete(bg, oldCRD))).To(Succeed())
		})
		Eventually(func() bool { return crdEstablished(crdClient, oldCRD.Name) }).
			WithTimeout(30 * time.Second).WithPolling(250 * time.Millisecond).Should(BeTrue())

		gvk := transitionGVK(oldCRD)
		// A fresh client so its REST mapper discovers the new kind.
		c, err := client.New(cfg, client.Options{Scheme: crdScheme})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() error { return c.Create(bg, newTransitionObject(gvk)) }).
			WithTimeout(30 * time.Second).WithPolling(250 * time.Millisecond).Should(Succeed())
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(c.Delete(bg, newTransitionObject(gvk)))).To(Succeed())
		})

		By("applying three lastSent keys under the granular (old) schema")
		Expect(applyLastSent(c, gvk, []string{"a", "b", "c"})).To(Succeed())
		owned, full := managedLastSent(readTransitionObject(c, gvk))
		Expect(owned).To(HaveLen(3), "old schema should own each lastSent key individually; managedFields: %s", full)
		Expect(owned).To(HaveKey("f:a"))
		GinkgoWriter.Printf("managedFields before: %s\n", full)

		By("switching the CRD to the atomic schema and re-applying fewer keys")
		current := &apiextensionsv1.CustomResourceDefinition{}
		Expect(crdClient.Get(bg, types.NamespacedName{Name: oldCRD.Name}, current)).To(Succeed())
		current.Spec = transitionCRD(realCRD, true).Spec
		Expect(crdClient.Update(bg, current)).To(Succeed())
		applyUntilAtomic(c, gvk, []string{"a", "d"})
		_, full = managedLastSent(readTransitionObject(c, gvk))
		GinkgoWriter.Printf("managedFields after: %s\n", full)

		By("applying with lastSent omitted")
		Expect(applyLastSent(c, gvk, nil)).To(Succeed())
		got := readTransitionObject(c, gvk)
		Expect(storedLastSentKeys(got)).To(BeEmpty(), "lastSent should be removed when the manager stops sending it")
	})
})
