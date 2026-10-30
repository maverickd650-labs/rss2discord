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

package chart

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

const (
	upgradeRelease   = "rel"
	upgradeNamespace = "operator-system"
	tenantNamespace  = "team-a"
	otherNamespace   = "team-other"
	managerRoleName  = upgradeRelease + "-rss2discord-operator-manager-role"
	managerBinding   = upgradeRelease + "-rss2discord-operator-manager-rolebinding"
	managerSAName    = upgradeRelease + "-rss2discord-operator"
	verbList         = "list"
	verbWatch        = "watch"
)

// TestChartUpgradeToWatchNamespacesRevokesClusterWideSecretAccess installs the
// chart against a real API server, checks the default grants get-only Secret
// access, widens it with rbac.clusterWideSecretWatch, narrows it with
// rbac.watchNamespaces through helm upgrade, and asks the API server's RBAC
// authorizer (SubjectAccessReview) what the operator's service account may
// still do. The grant, not --watch-namespaces, is the credential boundary: the
// upgrade must delete the old ClusterRole/ClusterRoleBinding, leave the account
// able to read Secrets in the listed namespace only, and leave no other
// cluster-wide binding (the metrics ones included) that reaches Secrets.
func TestChartUpgradeToWatchNamespacesRevokesClusterWideSecretAccess(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Fatalf("helm not found on PATH (run via `mise run test`): %v", err)
	}
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("KUBEBUILDER_ASSETS is unset -- run via `mise run test`, which provisions the envtest API server")
	}
	ctx := context.Background()

	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("starting envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	admin, err := env.AddUser(envtest.User{Name: "chart-test-admin", Groups: []string{"system:masters"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	kubeconfig, err := admin.KubeConfig()
	if err != nil {
		t.Fatal(err)
	}
	kubeconfigPath := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(kubeconfigPath, kubeconfig, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(cfg, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, ns := range []string{upgradeNamespace, tenantNamespace, otherNamespace} {
		if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			t.Fatal(err)
		}
	}

	helm := func(verb string, values ...string) {
		t.Helper()
		args := make([]string, 0, 9+2*len(values))
		args = append(args, verb, upgradeRelease, filepath.Join("..", "..", "dist", "chart"),
			"--namespace", upgradeNamespace, "--kubeconfig", kubeconfigPath, "--set", "manager.image.tag=upgrade-test")
		for _, v := range values {
			args = append(args, "--set", v)
		}
		if out, err := exec.Command("helm", args...).CombinedOutput(); err != nil {
			t.Fatalf("helm %s failed: %v\n%s", verb, err, out)
		}
	}
	can := func(verb, namespace string) bool {
		t.Helper()
		review := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
			User:   "system:serviceaccount:" + upgradeNamespace + ":" + managerSAName,
			Groups: []string{"system:serviceaccounts", "system:serviceaccounts:" + upgradeNamespace},
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: namespace, Verb: verb, Resource: "secrets",
			},
		}}
		if err := c.Create(ctx, review); err != nil {
			t.Fatal(err)
		}
		return review.Status.Allowed
	}
	exists := func(obj client.Object, key client.ObjectKey) bool {
		t.Helper()
		err := c.Get(ctx, key, obj)
		if err != nil && !apierrors.IsNotFound(err) {
			t.Fatal(err)
		}
		return err == nil
	}

	helm("install")
	if !exists(&rbacv1.ClusterRoleBinding{}, client.ObjectKey{Name: managerBinding}) {
		t.Fatal("the default install has no cluster-wide manager binding; this test no longer exercises narrowing")
	}
	// The default install reads a webhook Secret by name only: no Secret
	// watch, so no list/watch -- a stolen token cannot enumerate Secrets.
	if !can("get", otherNamespace) {
		t.Fatal("the default install must still get webhook Secrets by name in any namespace")
	}
	for _, verb := range []string{verbList, verbWatch} {
		if can(verb, otherNamespace) || can(verb, "") {
			t.Errorf("the default install grants %s on Secrets; only an explicit opt-in may", verb)
		}
	}

	// The widest grant the chart can issue is the starting point narrowed below.
	helm("upgrade", "rbac.clusterWideSecretWatch=true")
	if !can(verbList, otherNamespace) || !can(verbWatch, "") {
		t.Fatal("rbac.clusterWideSecretWatch should grant cluster-wide list/watch (the baseline this test narrows)")
	}

	helm("upgrade", "rbac.watchNamespaces={"+tenantNamespace+"}")

	if exists(&rbacv1.ClusterRole{}, client.ObjectKey{Name: managerRoleName}) {
		t.Error("helm upgrade left the cluster-wide manager ClusterRole behind")
	}
	if exists(&rbacv1.ClusterRoleBinding{}, client.ObjectKey{Name: managerBinding}) {
		t.Error("helm upgrade left the cluster-wide manager ClusterRoleBinding behind")
	}
	if !exists(&rbacv1.RoleBinding{}, client.ObjectKey{Namespace: tenantNamespace, Name: managerBinding}) {
		t.Errorf("no manager RoleBinding in %s after narrowing", tenantNamespace)
	}
	for _, verb := range []string{"get", verbList, verbWatch} {
		if !can(verb, tenantNamespace) {
			t.Errorf("service account cannot %s Secrets in the listed namespace %s", verb, tenantNamespace)
		}
		if can(verb, otherNamespace) {
			t.Errorf("service account can still %s Secrets in %s, outside rbac.watchNamespaces", verb, otherNamespace)
		}
		if can(verb, "") {
			t.Errorf("service account can still %s Secrets cluster-wide", verb)
		}
	}
}
