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

package utils

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// startCluster runs an envtest API server and points kubectl (taken from the
// envtest assets) at it for the rest of the test.
func startCluster(t *testing.T) {
	t.Helper()
	env := &envtest.Environment{}
	if _, err := env.Start(); err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	admin, err := env.AddUser(envtest.User{Name: "admin", Groups: []string{"system:masters"}}, nil)
	if err != nil {
		t.Fatalf("add user: %v", err)
	}
	kubeconfig, err := admin.KubeConfig()
	if err != nil {
		t.Fatalf("kubeconfig: %v", err)
	}
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, kubeconfig, 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	t.Setenv("KUBECONFIG", path)
	t.Setenv("PATH", os.Getenv("KUBEBUILDER_ASSETS")+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func mustRun(t *testing.T, args ...string) {
	t.Helper()
	if _, err := Run(exec.Command("kubectl", args...)); err != nil {
		t.Fatalf("kubectl %v: %v", args, err)
	}
}

func mustIdentify(t *testing.T, kind, name, namespace string) Object {
	t.Helper()
	o, err := Identify(kind, name, namespace)
	if err != nil {
		t.Fatalf("Identify(%s %s): %v", kind, name, err)
	}
	if !strings.Contains(o.String(), o.UID) {
		t.Fatalf("String() = %q, want it to carry the UID %q", o.String(), o.UID)
	}
	return o
}

func requireDelete(t *testing.T, o Object, want DeleteOutcome) {
	t.Helper()
	got, err := o.Delete()
	if err != nil {
		t.Fatalf("Delete(%s): %v", o, err)
	}
	if got != want {
		t.Fatalf("Delete(%s) = %d, want %d", o, got, want)
	}
}

func requireExists(t *testing.T, o Object, want bool) {
	t.Helper()
	got, err := o.Exists()
	if err != nil {
		t.Fatalf("Exists(%s): %v", o, err)
	}
	if got != want {
		t.Fatalf("Exists(%s) = %v, want %v", o, got, want)
	}
}

func createBinding(t *testing.T, name string) Object {
	t.Helper()
	mustRun(t, "create", "clusterrolebinding", name, "--clusterrole=view", "--user=someone")
	return mustIdentify(t, "clusterrolebinding", name, "")
}

// listCommand is the kind of listing List classifies: one line per match.
func listCommand(kind, name string) []string {
	return []string{"kubectl", "get", kind, name, "--ignore-not-found", "-o", "name"}
}

func TestList(t *testing.T) {
	startCluster(t)
	mustRun(t, "create", "ns", "listed")

	cases := []struct {
		name    string
		command []string
		want    Lookup
		wantErr bool
	}{
		{"present", listCommand("ns", "listed"), LookupPresent, false},
		{"absent", listCommand("ns", "missing"), LookupAbsent, false},
		{"failed lookup is not absent", listCommand("no-such-kind", "x"), LookupFailed, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := List(tc.command...)
			if got != tc.want {
				t.Fatalf("List = %d, want %d (err %v)", got, tc.want, err)
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("List error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// A zero Lookup must be the one that stops the caller, never "absent".
func TestLookupZeroValueIsFailed(t *testing.T) {
	var l Lookup
	if l != LookupFailed {
		t.Fatalf("zero Lookup = %d, want LookupFailed", l)
	}
}

func TestIdentify(t *testing.T) {
	startCluster(t)

	if _, err := Identify("clusterrolebinding", "missing", ""); err == nil {
		t.Fatal("Identify of a missing object succeeded")
	}
	if _, err := Identify("secret", "x", "default"); err == nil {
		t.Fatal("Identify of a kind with no delete path succeeded")
	}
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "no-such-kubeconfig"))
	if _, err := Identify("ns", "default", ""); err == nil {
		t.Fatal("Identify succeeded although the lookup failed")
	}
}

// The delete is bound to the object's UID: it removes that object, reports a
// missing one as already gone, and leaves a same-named successor in place.
func TestObjectDeleteIsBoundToUID(t *testing.T) {
	startCluster(t)

	first := createBinding(t, "owned")
	requireExists(t, first, true)
	requireDelete(t, first, Deleted)
	requireExists(t, first, false)
	requireDelete(t, first, AlreadyGone)

	successor := createBinding(t, "owned")
	if successor.UID == first.UID {
		t.Fatal("the recreated binding kept the old UID")
	}
	requireExists(t, first, false)
	requireDelete(t, first, Replaced)
	requireExists(t, successor, true)
}

// Every kind the e2e suite creates has a working delete path.
func TestObjectDeleteCoversEveryOwnedKind(t *testing.T) {
	startCluster(t)

	mustRun(t, "create", "ns", "owned-ns")
	mustRun(t, "run", "owned-pod", "--restart=Never", "--image=busybox", "-n", "default")
	crd := filepath.Join(t.TempDir(), "crd.yaml")
	if err := os.WriteFile(crd, []byte(ownedCRD), 0o600); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "apply", "-f", crd)
	createBinding(t, "owned-binding")

	for _, o := range []Object{
		mustIdentify(t, "pod", "owned-pod", "default"),
		mustIdentify(t, "clusterrolebinding", "owned-binding", ""),
		mustIdentify(t, "crd", "widgets.owned.example.com", ""),
		mustIdentify(t, "ns", "owned-ns", ""),
	} {
		requireDelete(t, o, Deleted)
	}
}

func TestObjectDeleteReportsUnknownOutcomes(t *testing.T) {
	startCluster(t)
	o := createBinding(t, "unreachable")

	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "no-such-kubeconfig"))
	if outcome, err := o.Delete(); err == nil {
		t.Fatalf("Delete = %d with no error although the API server was unreachable", outcome)
	}
	if _, err := o.Exists(); err == nil {
		t.Fatal("Exists succeeded although the API server was unreachable")
	}

	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "no-such-dir"))
	if _, err := o.Delete(); err == nil {
		t.Fatal("Delete succeeded although its options file could not be written")
	}
}

const ownedCRD = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: widgets.owned.example.com
spec:
  group: owned.example.com
  names: {kind: Widget, plural: widgets}
  scope: Namespaced
  versions:
    - name: v1
      served: true
      storage: true
      schema:
        openAPIV3Schema: {type: object, x-kubernetes-preserve-unknown-fields: true}
`
