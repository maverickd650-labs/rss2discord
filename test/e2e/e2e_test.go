//go:build e2e
// +build e2e

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

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/maverickd650-labs/rss2discord/test/utils"
)

// namespace where the project is deployed in
const namespace = "rss2discord-operator-system"

// serviceAccountName created for the project. The Helm chart's fullname
// template collapses to the release name alone when the release name already
// contains the chart name (see dist/chart/templates/_helpers.tpl), so this is
// shorter than the "-controller-manager" suffix kustomize would produce.
const serviceAccountName = "rss2discord-operator"

// metricsServiceName is the name of the metrics service of the project
const metricsServiceName = "rss2discord-operator-metrics"

// metricsRoleBindingName is the name of the RBAC that will be created to allow get the metrics data
const metricsRoleBindingName = "rss2discord-operator-metrics-binding"

// feedGroupNamespace is a separate namespace (from the manager's own
// namespace) for the failure-path FeedGroup spec's own namespaced objects
// (Secret, FeedGroup, Events), so it can be torn down independently.
const feedGroupNamespace = "feedgroup-e2e-test"

// feedGroupName is the name of the FeedGroup applied by the failure-path spec.
const feedGroupName = "feedgroup-unreachable"

// feedGroupCurlPodName names the second metrics-fetching curl pod so it
// doesn't collide with the one the "should ensure the metrics endpoint is
// serving metrics" spec already created (and cleans up) in the manager
// namespace.
const feedGroupCurlPodName = "curl-metrics-feedgroup"

var _ = Describe("Manager", Ordered, func() {
	var controllerPodName string

	// Before running the tests, set up the environment by creating the namespace,
	// enforcing the restricted security policy on it, and deploying the controller
	// via the Helm chart (the supported install path; the chart installs CRDs and
	// RBAC as part of the same release, so there's no separate install step).
	BeforeAll(func() {
		By("creating manager namespace")
		cmd := exec.Command("kubectl", "create", "ns", namespace)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create namespace")

		By("labeling the namespace to enforce the restricted security policy")
		cmd = exec.Command("kubectl", "label", "--overwrite", "ns", namespace,
			"pod-security.kubernetes.io/enforce=restricted")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to label namespace with restricted policy")

		By("deploying the controller-manager via Helm")
		// IMG is set in the suite's BeforeSuite and read from the environment by the mise task.
		cmd = exec.Command("mise", "run", "helm-deploy")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager via Helm")
	})

	// After all tests have been executed, clean up by uninstalling the Helm release
	// and deleting the namespace.
	AfterAll(func() {
		By("cleaning up the curl pods for metrics")
		cmd := exec.Command("kubectl", "delete", "pod", "curl-metrics", "-n", namespace)
		_, _ = utils.Run(cmd)
		cmd = exec.Command("kubectl", "delete", "pod", feedGroupCurlPodName, "-n", namespace)
		_, _ = utils.Run(cmd)

		By("removing the feedgroup test namespace")
		cmd = exec.Command("kubectl", "delete", "ns", feedGroupNamespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)

		By("uninstalling the controller-manager Helm release")
		cmd = exec.Command("mise", "run", "helm-uninstall")
		_, _ = utils.Run(cmd)

		By("removing manager namespace")
		cmd = exec.Command("kubectl", "delete", "ns", namespace)
		_, _ = utils.Run(cmd)
	})

	// After each test, check for failures and collect logs, events,
	// and pod descriptions for debugging.
	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			By("Fetching controller manager pod logs")
			cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
			controllerLogs, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n %s", controllerLogs)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Controller logs: %s", err)
			}

			By("Fetching Kubernetes events")
			cmd = exec.Command("kubectl", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp")
			eventsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Kubernetes events:\n%s", eventsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Kubernetes events: %s", err)
			}

			By("Fetching curl-metrics logs")
			cmd = exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
			metricsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Metrics logs:\n %s", metricsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get curl-metrics logs: %s", err)
			}

			By("Fetching controller manager pod description")
			cmd = exec.Command("kubectl", "describe", "pod", controllerPodName, "-n", namespace)
			podDescription, err := utils.Run(cmd)
			if err == nil {
				fmt.Println("Pod description:\n", podDescription)
			} else {
				fmt.Println("Failed to describe controller pod")
			}
		}
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	Context("Manager", func() {
		It("should run successfully", func() {
			By("validating that the controller-manager pod is running as expected")
			verifyControllerUp := func(g Gomega) {
				By("getting the name of the controller-manager pod")
				cmd := exec.Command("kubectl", "get",
					"pods", "-l", "control-plane=controller-manager",
					"-o", "go-template={{ range .items }}"+
						"{{ if not .metadata.deletionTimestamp }}"+
						"{{ .metadata.name }}"+
						"{{ \"\\n\" }}{{ end }}{{ end }}",
					"-n", namespace,
				)

				podOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve controller-manager pod information")
				podNames := utils.GetNonEmptyLines(podOutput)
				g.Expect(podNames).To(HaveLen(1), "expected 1 controller pod running")
				controllerPodName = podNames[0]
				g.Expect(controllerPodName).To(ContainSubstring("rss2discord-operator"))

				By("validating the pod's status")
				cmd = exec.Command("kubectl", "get",
					"pods", controllerPodName, "-o", "jsonpath={.status.phase}",
					"-n", namespace,
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"), "Incorrect controller-manager pod status")
			}
			Eventually(verifyControllerUp).Should(Succeed())
		})

		It("should ensure the metrics endpoint is serving metrics", func() {
			By("creating a ClusterRoleBinding for the service account to allow access to metrics")
			cmd := exec.Command("kubectl", "create", "clusterrolebinding", metricsRoleBindingName,
				"--clusterrole=rss2discord-operator-metrics-reader",
				fmt.Sprintf("--serviceaccount=%s:%s", namespace, serviceAccountName),
			)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ClusterRoleBinding")

			By("validating that the metrics service is available")
			cmd = exec.Command("kubectl", "get", "service", metricsServiceName, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Metrics service should exist")

			By("getting the service account token")
			token, err := serviceAccountToken()
			Expect(err).NotTo(HaveOccurred())
			Expect(token).NotTo(BeEmpty())

			By("ensuring the controller pod is ready")
			verifyControllerPodReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", controllerPodName, "-n", namespace,
					"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"), "Controller pod not ready")
			}
			Eventually(verifyControllerPodReady, 3*time.Minute, time.Second).Should(Succeed())

			By("verifying that the controller manager is serving the metrics server")
			verifyMetricsServerStarted := func(g Gomega) {
				cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("Serving metrics server"),
					"Metrics server not yet started")
			}
			Eventually(verifyMetricsServerStarted, 3*time.Minute, time.Second).Should(Succeed())

			// +kubebuilder:scaffold:e2e-metrics-webhooks-readiness

			By("creating the curl-metrics pod to access the metrics endpoint")
			cmd = exec.Command("kubectl", "run", "curl-metrics", "--restart=Never",
				"--namespace", namespace,
				"--image=curlimages/curl:latest",
				"--overrides",
				fmt.Sprintf(`{
					"spec": {
						"containers": [{
							"name": "curl",
							"image": "curlimages/curl:latest",
							"command": ["/bin/sh", "-c"],
							"args": [
								"for i in $(seq 1 30); do curl -v -k -H 'Authorization: Bearer %s' https://%s.%s.svc.cluster.local:8443/metrics && exit 0 || sleep 2; done; exit 1"
							],
							"securityContext": {
								"readOnlyRootFilesystem": true,
								"allowPrivilegeEscalation": false,
								"capabilities": {
									"drop": ["ALL"]
								},
								"runAsNonRoot": true,
								"runAsUser": 1000,
								"seccompProfile": {
									"type": "RuntimeDefault"
								}
							}
						}],
						"serviceAccountName": "%s"
					}
				}`, token, metricsServiceName, namespace, serviceAccountName))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create curl-metrics pod")

			By("waiting for the curl-metrics pod to complete.")
			verifyCurlUp := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "curl-metrics",
					"-o", "jsonpath={.status.phase}",
					"-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Succeeded"), "curl pod in wrong status")
			}
			Eventually(verifyCurlUp, 5*time.Minute).Should(Succeed())

			By("getting the metrics by checking curl-metrics logs")
			verifyMetricsAvailable := func(g Gomega) {
				metricsOutput, err := getMetricsOutput()
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve logs from curl pod")
				g.Expect(metricsOutput).NotTo(BeEmpty())
				g.Expect(metricsOutput).To(ContainSubstring("< HTTP/1.1 200 OK"))
			}
			Eventually(verifyMetricsAvailable, 2*time.Minute).Should(Succeed())
		})

		// +kubebuilder:scaffold:e2e-webhooks-checks

		// This spec deliberately doesn't exercise the success path (RSS fetch ->
		// Discord send): internal/discord/client.go only sends to real Discord
		// hostnames (AllowedWebhookHosts), so there's no way to point a real
		// in-cluster manager at a mock webhook receiver without weakening that SSRF
		// guard — and the envtest-based suite
		// (internal/controller/feedgroup_controller_test.go) already covers the full
		// reconcile loop against mock RSS/Discord servers in depth, reaching the
		// Discord mock through httptest's in-memory network at a real discord.com
		// webhook URL, so the allowlist is left as it is.
		// It instead exercises the *failure* path (an unreachable feed), which needs
		// no mock at all and covers exactly what envtest can't: CRD schema
		// validation on a real API server, controller RBAC on FeedGroup/status, real
		// Events, and the metrics endpoint reflecting a real reconcile.
		It("should reflect a persistently unreachable feed in status, events, and metrics", func() {
			By("creating the feedgroup test namespace")
			cmd := exec.Command("kubectl", "create", "ns", feedGroupNamespace)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create feedgroup test namespace")

			By("creating the dummy Discord webhook secret")
			cmd = exec.Command("kubectl", "create", "secret", "generic", "discord-webhook",
				"-n", feedGroupNamespace,
				"--from-literal=url=https://discord.com/api/webhooks/000/dummy")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create discord webhook secret")

			By("applying a FeedGroup pointing at an unresolvable host")
			feedGroupManifest := fmt.Sprintf(`apiVersion: rss2discord.maverickd650.dev/v1alpha1
kind: FeedGroup
metadata:
  name: %s
  namespace: %s
spec:
  discordWebhookSecretRef:
    name: discord-webhook
    key: url
  interval: 1m
  retries: 2
  retryInterval: 10s
  feeds:
    - rssUrl: https://feeds.invalid/feed.xml
`, feedGroupName, feedGroupNamespace)
			manifestFile := filepath.Join("/tmp", feedGroupName+".yaml")
			Expect(os.WriteFile(manifestFile, []byte(feedGroupManifest), 0o644)).To(Succeed())
			cmd = exec.Command("kubectl", "apply", "-f", manifestFile)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to apply FeedGroup with an unresolvable feed")

			By("waiting for the feed's Reachable condition to report DNSFailure")
			verifyFeedUnreachable := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "feedgroup", feedGroupName,
					"-n", feedGroupNamespace,
					"-o", "jsonpath={.status.feeds[0].conditions[?(@.type=='Reachable')].status}"+
						"{\"\\n\"}{.status.feeds[0].conditions[?(@.type=='Reachable')].reason}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				lines := utils.GetNonEmptyLines(output)
				g.Expect(lines).To(HaveLen(2))
				g.Expect(lines[0]).To(Equal("False"))
				g.Expect(lines[1]).To(Equal("DNSFailure"))
			}
			Eventually(verifyFeedUnreachable, 90*time.Second, time.Second).Should(Succeed())

			By("waiting for the persistent-failure Warning event")
			verifyPersistentFailureEvent := func(g Gomega) {
				// Fetch all events rather than filtering server-side with
				// --field-selector reason=...: this event is emitted via
				// events.EventRecorder (events.k8s.io/v1), and it's not worth
				// betting on server-side field-selector support for that API on an
				// assertion that can't be exercised locally (no container runtime
				// in this sandbox) before landing.
				cmd := exec.Command("kubectl", "get", "events", "-n", feedGroupNamespace, "-o", "json")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("FetchFailed"))
				g.Expect(output).To(ContainSubstring("giving up after exhausting retries"))
			}
			Eventually(verifyPersistentFailureEvent, 90*time.Second, time.Second).Should(Succeed())

			By("getting the service account token")
			token, err := serviceAccountToken()
			Expect(err).NotTo(HaveOccurred())
			Expect(token).NotTo(BeEmpty())

			By("creating a curl pod to re-fetch the metrics endpoint")
			cmd = exec.Command("kubectl", "run", feedGroupCurlPodName, "--restart=Never",
				"--namespace", namespace,
				"--image=curlimages/curl:latest",
				"--overrides",
				fmt.Sprintf(`{
					"spec": {
						"containers": [{
							"name": "curl",
							"image": "curlimages/curl:latest",
							"command": ["/bin/sh", "-c"],
							"args": [
								"for i in $(seq 1 30); do curl -v -k -H 'Authorization: Bearer %s' https://%s.%s.svc.cluster.local:8443/metrics && exit 0 || sleep 2; done; exit 1"
							],
							"securityContext": {
								"readOnlyRootFilesystem": true,
								"allowPrivilegeEscalation": false,
								"capabilities": {
									"drop": ["ALL"]
								},
								"runAsNonRoot": true,
								"runAsUser": 1000,
								"seccompProfile": {
									"type": "RuntimeDefault"
								}
							}
						}],
						"serviceAccountName": "%s"
					}
				}`, token, metricsServiceName, namespace, serviceAccountName))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create curl pod for feedgroup metrics")

			By("waiting for the feedgroup metrics curl pod to complete")
			verifyFeedGroupCurlUp := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", feedGroupCurlPodName,
					"-o", "jsonpath={.status.phase}",
					"-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Succeeded"), "curl pod in wrong status")
			}
			Eventually(verifyFeedGroupCurlUp, 5*time.Minute).Should(Succeed())

			By("verifying the fetch_error_dns_failure outcome is exported")
			verifyFeedGroupMetric := func(g Gomega) {
				cmd := exec.Command("kubectl", "logs", feedGroupCurlPodName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				// Match each label independently rather than one fixed-order string:
				// the text exposition format serializes labels alphabetically by
				// name, not in the vector's declared (namespace, name, rss_url,
				// outcome) order.
				g.Expect(output).To(ContainSubstring(`rss2discord_feed_operations_total{`))
				g.Expect(output).To(ContainSubstring(fmt.Sprintf(`namespace="%s"`, feedGroupNamespace)))
				g.Expect(output).To(ContainSubstring(fmt.Sprintf(`name="%s"`, feedGroupName)))
				g.Expect(output).To(ContainSubstring(`rss_url="https://feeds.invalid/feed.xml"`))
				g.Expect(output).To(ContainSubstring(`outcome="fetch_error_dns_failure"`))
			}
			Eventually(verifyFeedGroupMetric, 2*time.Minute).Should(Succeed())
		})
	})
})

// serviceAccountToken returns a token for the specified service account in the given namespace.
// It uses the Kubernetes TokenRequest API to generate a token by directly sending a request
// and parsing the resulting token from the API response.
func serviceAccountToken() (string, error) {
	const tokenRequestRawString = `{
		"apiVersion": "authentication.k8s.io/v1",
		"kind": "TokenRequest"
	}`

	By("creating temporary file to store the token request")
	secretName := fmt.Sprintf("%s-token-request", serviceAccountName)
	tokenRequestFile := filepath.Join("/tmp", secretName)
	err := os.WriteFile(tokenRequestFile, []byte(tokenRequestRawString), os.FileMode(0o644))
	if err != nil {
		return "", err
	}

	var out string
	verifyTokenCreation := func(g Gomega) {
		By("executing kubectl command to create the token")
		cmd := exec.Command("kubectl", "create", "--raw", fmt.Sprintf(
			"/api/v1/namespaces/%s/serviceaccounts/%s/token",
			namespace,
			serviceAccountName,
		), "-f", tokenRequestFile)

		output, err := cmd.CombinedOutput()
		g.Expect(err).NotTo(HaveOccurred())

		By("parsing the JSON output to extract the token")
		var token tokenRequest
		err = json.Unmarshal(output, &token)
		g.Expect(err).NotTo(HaveOccurred())

		out = token.Status.Token
	}
	Eventually(verifyTokenCreation).Should(Succeed())

	return out, err
}

// getMetricsOutput retrieves and returns the logs from the curl pod used to access the metrics endpoint.
func getMetricsOutput() (string, error) {
	By("getting the curl-metrics logs")
	cmd := exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
	return utils.Run(cmd)
}

// tokenRequest is a simplified representation of the Kubernetes TokenRequest API response,
// containing only the token field that we need to extract.
type tokenRequest struct {
	Status struct {
		Token string `json:"token"`
	} `json:"status"`
}
