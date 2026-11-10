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
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Lookup is the result of asking the cluster whether something exists. The
// zero value is LookupFailed, so a result nobody set can never read as
// "absent" and so license a cleanup.
type Lookup int

const (
	// LookupFailed means the lookup itself did not succeed: ownership is
	// indeterminate and the caller must stop rather than guess.
	LookupFailed Lookup = iota
	// LookupAbsent means the lookup succeeded and found nothing.
	LookupAbsent
	// LookupPresent means the lookup succeeded and found the resource.
	LookupPresent
)

// List runs a listing command that prints one line per match and nothing when
// there is none (kubectl get --ignore-not-found -o name, helm list --short) and
// classifies the result. Any command failure is LookupFailed with its error.
func List(command ...string) (Lookup, error) {
	out, err := Run(exec.Command(command[0], command[1:]...))
	if err != nil {
		return LookupFailed, err
	}
	if strings.TrimSpace(out) == "" {
		return LookupAbsent, nil
	}
	return LookupPresent, nil
}

// Object is one cluster object pinned to the UID it had when it was
// identified. Deleting through it can only remove that very object: a later
// object of the same kind and name has another UID and is left alone.
type Object struct {
	Kind      string
	Name      string
	Namespace string // "" for a cluster-scoped kind
	UID       string
}

// rawPaths are the API paths of the kinds the e2e suite creates, with %[1]s
// the name and %[2]s the namespace. A raw path is what lets a delete carry a
// UID precondition, which "kubectl delete <kind> <name>" cannot.
var rawPaths = map[string]string{
	"ns":                 "/api/v1/namespaces/%[1]s",
	"pod":                "/api/v1/namespaces/%[2]s/pods/%[1]s",
	"clusterrolebinding": "/apis/rbac.authorization.k8s.io/v1/clusterrolebindings/%[1]s",
	"crd":                "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/%[1]s",
}

// Create runs "kubectl <args...> -o jsonpath={.metadata.uid}" to create an
// object of kind/name/namespace and pins the returned Object to the UID from
// the creation response itself, without a separate post-creation lookup.
func Create(kind, name, namespace string, args ...string) (Object, error) {
	o := Object{Kind: kind, Name: name, Namespace: namespace}
	if _, ok := rawPaths[kind]; !ok {
		return Object{}, fmt.Errorf("no API path known for kind %q", kind)
	}
	fullArgs := append(append([]string(nil), args...), "-o", "jsonpath={.metadata.uid}")
	out, err := Run(exec.Command("kubectl", fullArgs...))
	if err != nil {
		return Object{}, err
	}
	uid := strings.TrimSpace(out)
	if uid == "" {
		return Object{}, fmt.Errorf("create %s %q returned no UID", kind, name)
	}
	o.UID = uid
	return o, nil
}

// Identify pins the named object to its current UID. It fails if the object
// does not exist or the lookup fails, so an Object always names something that
// was seen on the cluster.
func Identify(kind, name, namespace string) (Object, error) {
	o := Object{Kind: kind, Name: name, Namespace: namespace}
	if _, ok := rawPaths[kind]; !ok {
		return Object{}, fmt.Errorf("no API path known for kind %q", kind)
	}
	uid, err := o.currentUID()
	if err != nil {
		return Object{}, err
	}
	if uid == "" {
		return Object{}, fmt.Errorf("%s %q not found", kind, name)
	}
	o.UID = uid
	return o, nil
}

// String names the object for logs and failure messages.
func (o Object) String() string {
	return fmt.Sprintf("%s %s (uid %s)", o.Kind, o.Name, o.UID)
}

// currentUID is the UID of whatever object has this kind and name now, or ""
// when there is none.
func (o Object) currentUID() (string, error) {
	args := []string{"get", o.Kind, o.Name, "--ignore-not-found", "-o", "jsonpath={.metadata.uid}"}
	if o.Namespace != "" {
		args = append(args, "-n", o.Namespace)
	}
	out, err := Run(exec.Command("kubectl", args...))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Exists reports whether this very object (same UID) is still on the cluster.
func (o Object) Exists() (bool, error) {
	uid, err := o.currentUID()
	if err != nil {
		return false, err
	}
	return uid == o.UID, nil
}

// DestructiveCRDTeardownAuthorized reports whether destructive CRD teardown is
// permitted after the suite: either this run exclusively owns the cluster
// (E2E_OWNS_CLUSTER=true, set by `mise run test-e2e` when it created a fresh
// Kind cluster) or the caller explicitly authorized CRD deletion on a retained
// or shared cluster (E2E_DELETE_CRD=true). Absent/present snapshots around
// `helm upgrade --install` cannot prove this run created the CRD on a shared
// cluster, and deleting a CRD cascades to all FeedGroup custom resources.
func DestructiveCRDTeardownAuthorized(getenv func(string) string) bool {
	return getenv("E2E_OWNS_CLUSTER") == "true" || getenv("E2E_DELETE_CRD") == "true"
}

// DeleteOutcome is what a UID-bound delete found.
type DeleteOutcome int

const (
	// Deleted means the API server accepted the delete of this object.
	Deleted DeleteOutcome = iota + 1
	// AlreadyGone means no object of this kind and name exists.
	AlreadyGone
	// Replaced means another object now holds this kind and name: it is not
	// this run's to delete and was left in place.
	Replaced
)

// Delete asks the API server to delete this object on the precondition that
// its UID is still the one recorded, so the check and the delete are one
// atomic request. An error means the outcome is unknown.
func (o Object) Delete() (DeleteOutcome, error) {
	body, err := os.CreateTemp("", "delete-options-*.json")
	if err != nil {
		return 0, err
	}
	defer func() { _ = os.Remove(body.Name()) }()
	_, err = fmt.Fprintf(body, `{"kind":"DeleteOptions","apiVersion":"v1","preconditions":{"uid":%q}}`, o.UID)
	if closeErr := body.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return 0, err
	}
	path := fmt.Sprintf(rawPaths[o.Kind], o.Name, o.Namespace)
	_, err = Run(exec.Command("kubectl", "delete", "--raw", path, "-f", body.Name()))
	return deleteOutcome(err)
}

// deleteOutcome classifies the error of a UID-bound raw delete by the API
// status reason kubectl prints.
func deleteOutcome(err error) (DeleteOutcome, error) {
	if err == nil {
		return Deleted, nil
	}
	if strings.Contains(err.Error(), "NotFound") {
		return AlreadyGone, nil
	}
	if strings.Contains(err.Error(), "Conflict") {
		return Replaced, nil
	}
	return 0, err
}
