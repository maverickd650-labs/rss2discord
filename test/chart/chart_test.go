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

// Package chart holds golden-file regression tests for dist/chart. They
// don't validate template syntax (helm lint already does that in CI) --
// they pin down the *content* of the hand-tuned files mise run
// helm-chart-refresh knows to preserve (see CLAUDE.md), so a refresh
// regression shows up as a test failure instead of only in `git diff`
// during code review.
package chart

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// update regenerates every golden file from the current chart output
// instead of comparing against it. Run `go test ./test/chart -update` after
// an intentional chart change (or after `mise run helm-chart-refresh`) and
// review the diff -- that review is the actual point of this flag.
var update = flag.Bool("update", false, "update golden files instead of comparing against them")

const (
	chartReleaseName = "chart-golden-test"
	chartNamespace   = "chart-golden-test-ns"

	managerTemplate   = "templates/manager/manager.yaml"
	monitorTemplate   = "templates/prometheus/controller-manager-metrics-monitor.yaml"
	prometheusEnabled = "prometheus.enabled=true"
	saDisabled        = "serviceAccount.enabled=false"
)

// helmChartLabelPattern matches the helm.sh/chart label's value, which
// embeds Chart.yaml's version (e.g. "rss2discord-operator-0.10.1"). That
// version bumps on every release-please release, so it's normalized before
// golden comparison rather than pinned literally -- otherwise every release
// PR would spuriously fail this test.
var helmChartLabelPattern = regexp.MustCompile(`helm\.sh/chart: \S+`)

// serviceAccountKindPattern matches a top-level ServiceAccount document (not
// a RoleBinding subject's "- kind: ServiceAccount").
var serviceAccountKindPattern = regexp.MustCompile(`(?m)^kind: ServiceAccount$`)

// blankLinesBeforeSeparatorPattern matches a newline, any blank or
// whitespace-only lines, and a "---" separator line.
var blankLinesBeforeSeparatorPattern = regexp.MustCompile(`\n(?:[ \t]*\n)+---\n`)

// normalize strips volatile content from rendered chart output before it's
// compared against (or written as) a golden file. Besides the chart version
// label, it drops blank/whitespace-only lines before a "---" document
// separator and canonicalizes trailing newlines to exactly one: helm 4.3
// changed how many blank lines it leaves after some rendered documents, so
// that whitespace is not a stable part of the output. The golden comparison
// applies this to the stored golden too, keeping the goldens helm-version
// agnostic without regenerating them.
func normalize(rendered string) string {
	rendered = helmChartLabelPattern.ReplaceAllString(rendered, "helm.sh/chart: SCRUBBED")
	rendered = blankLinesBeforeSeparatorPattern.ReplaceAllString(rendered, "\n---\n")
	return strings.TrimRight(rendered, "\n") + "\n"
}

// renderChartTemplate runs `helm template`, scoped with -s to a single
// template file, against a fixed release name/namespace. setValues are
// "key=value" pairs each passed as a separate --set flag. manager.image.tag
// is always pinned to a fixed value: its default otherwise falls back to
// Chart.yaml's AppVersion, which is just as volatile across releases as the
// helm.sh/chart label. It fails (never skips) when helm is missing -- a
// skippable regression test isn't one, and `mise run test` (which this runs
// under) provides helm via mise.
func renderChartTemplate(t *testing.T, showOnly string, setValues ...string) string {
	t.Helper()
	out, err := runHelmTemplate(t, showOnly, setValues...)
	if err != nil {
		t.Fatalf("helm template -s %s failed: %v\n%s", showOnly, err, out)
	}
	return normalize(out)
}

// runHelmTemplate does the shared work of renderChartTemplate and returns
// helm's combined output plus its error instead of failing the test, so a
// test can assert on an expected render failure. showOnly may be empty to
// render the whole chart.
func runHelmTemplate(t *testing.T, showOnly string, setValues ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Fatalf("helm not found on PATH -- this test requires it (run via `mise run test`, "+
			"which installs the mise-pinned helm): %v", err)
	}

	chartDir := filepath.Join("..", "..", "dist", "chart")
	args := make([]string, 0, 7+2*(len(setValues)+1))
	args = append(args,
		"template", chartReleaseName, chartDir,
		"--namespace", chartNamespace,
	)
	if showOnly != "" {
		args = append(args, "-s", showOnly)
	}
	for _, v := range append([]string{"manager.image.tag=golden-test"}, setValues...) {
		args = append(args, "--set", v)
	}

	out, err := exec.Command("helm", args...).CombinedOutput()
	return string(out), err
}

// TestChartGolden renders each of the chart's hand-tuned files (see
// CLAUDE.md's dist/chart section) under the value combinations that flip
// their content, and compares the result byte-for-byte against a checked-in
// golden file. helm lint (the only other chart check in CI) validates
// syntax, not content, so this is the only thing that would catch e.g.
// mise run helm-chart-refresh silently dropping the ServiceMonitor's
// native-histogram block.
func TestChartGolden(t *testing.T) {
	cases := []struct {
		name      string
		showOnly  string
		setValues []string
	}{
		// manager.yaml under its defaults, and with metrics.secure=false
		// (flips the --metrics-secure=false arg onto the container).
		{
			name:     "manager_defaults",
			showOnly: managerTemplate,
		},
		// The optional manager flags (omitted by default) render as args.
		{
			name:     "manager_extra_flags",
			showOnly: managerTemplate,
			setValues: []string{
				"manager.maxConcurrentReconciles=4",
				"manager.otlpEndpoint=otel-collector.observability:4317",
				"manager.logLevel=debug",
			},
		},
		{
			name:      "manager_metrics_insecure",
			showOnly:  managerTemplate,
			setValues: []string{"metrics.secure=false"},
		},
		// controller-manager-metrics-service.yaml: metrics.secure flips the
		// port name between "https" and "http".
		{
			name:     "metrics_service_defaults",
			showOnly: "templates/metrics/controller-manager-metrics-service.yaml",
		},
		{
			name:      "metrics_service_insecure",
			showOnly:  "templates/metrics/controller-manager-metrics-service.yaml",
			setValues: []string{"metrics.secure=false"},
		},
		// controller-manager-metrics-monitor.yaml (the ServiceMonitor):
		// scrapeNativeHistograms toggles whether the
		// scrapeProtocols/scrapeClassicHistograms/scrapeNativeHistograms
		// block appears at all.
		{
			name:      "metrics_monitor_native_histograms_true",
			showOnly:  monitorTemplate,
			setValues: []string{prometheusEnabled, "prometheus.scrapeNativeHistograms=true"},
		},
		{
			name:      "metrics_monitor_native_histograms_false",
			showOnly:  monitorTemplate,
			setValues: []string{prometheusEnabled, "prometheus.scrapeNativeHistograms=false"},
		},
		// ServiceMonitor with user-supplied labels/annotations: the
		// escaped dot in the annotation key must render as
		// "example.com/owner", and the reserved control-plane label must
		// keep its original value (the template omits it from the user
		// map) rather than being overridden or duplicated.
		{
			name:     "metrics_monitor_custom_labels_annotations",
			showOnly: monitorTemplate,
			setValues: []string{
				prometheusEnabled,
				"prometheus.labels.release=kps",
				`prometheus.annotations.example\.com/owner=team-a`,
				"prometheus.labels.control-plane=override",
			},
		},
		// manager.yaml with serviceAccount.enabled=false and an explicit
		// name: the pod must reference that pre-existing ServiceAccount.
		{
			name:      "manager_existing_service_account",
			showOnly:  managerTemplate,
			setValues: []string{saDisabled, "serviceAccount.name=existing-sa"},
		},
		// prometheus-rule.yaml: wholly custom, no kubebuilder equivalent.
		{
			name:      "prometheus_rule_enabled",
			showOnly:  "templates/prometheus/prometheus-rule.yaml",
			setValues: []string{"prometheusRule.enabled=true"},
		},
		// grafana-dashboard.yaml: emits both the ConfigMap (embedding
		// dashboards/feedgroup-overview.json) and the GrafanaDashboard CR.
		{
			name:      "grafana_dashboard_enabled",
			showOnly:  "templates/prometheus/grafana-dashboard.yaml",
			setValues: []string{"grafanaDashboard.enabled=true"},
		},
		// allow-metrics-traffic.yaml (NetworkPolicy): simple, but flagged as
		// a permutation to cover in the T7 task spec.
		{
			name:      "network_policy_enabled",
			showOnly:  "templates/network-policy/allow-metrics-traffic.yaml",
			setValues: []string{"networkPolicy.enabled=true"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderChartTemplate(t, tc.showOnly, tc.setValues...)
			goldenPath := filepath.Join("testdata", tc.name+".golden.yaml")

			if *update {
				if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
					t.Fatalf("failed to write golden file %s: %v", goldenPath, err)
				}
				return
			}

			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("failed to read golden file %s (run `go test ./test/chart -update` to create it): %v",
					goldenPath, err)
			}
			if wantNorm := normalize(string(want)); got != wantNorm {
				t.Errorf("rendered output for %s does not match %s.\n"+
					"If this is an intentional chart change, run `go test ./test/chart -update` "+
					"and review the golden diff.\n--- got ---\n%s\n--- want ---\n%s",
					tc.showOnly, goldenPath, got, wantNorm)
			}
		})
	}
}

// TestChartServiceAccountNameFallback pins the chart's guard for
// serviceAccount.enabled=false: an unset or blank serviceAccount.name must
// fall back to the name the chart would have created (a pre-created
// ServiceAccount), never to an empty serviceAccountName (which Kubernetes
// silently resolves to the namespace's "default" ServiceAccount), and the
// chart must not render its own ServiceAccount.
func TestChartServiceAccountNameFallback(t *testing.T) {
	const want = "serviceAccountName: " + chartReleaseName + "-rss2discord-operator"
	for name, setValues := range map[string][]string{
		"unset": {saDisabled},
		"blank": {saDisabled, "serviceAccount.name=  "},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := runHelmTemplate(t, "", setValues...)
			if err != nil {
				t.Fatalf("helm template failed: %v\n%s", err, out)
			}
			if !strings.Contains(out, want) {
				t.Errorf("expected rendered chart to contain %q, got:\n%s", want, out)
			}
			if serviceAccountKindPattern.MatchString(out) {
				t.Errorf("expected no chart-managed ServiceAccount when serviceAccount.enabled=false, got:\n%s", out)
			}
		})
	}
}

func TestChartManagerOptionalFlags(t *testing.T) {
	flags := []string{"--max-concurrent-feedgroup-reconciles=", "--otlp-endpoint=", "--zap-log-level="}
	for _, tc := range []struct {
		name   string
		values []string
		want   string
	}{
		{name: "concurrency alone", values: []string{"manager.maxConcurrentReconciles=1"}, want: flags[0] + "1"},
		{name: "tracing alone", values: []string{"manager.otlpEndpoint=collector:4317"}, want: flags[1] + "collector:4317"},
		{name: "numeric log verbosity", values: []string{"manager.logLevel=2"}, want: flags[2] + "2"},
		{name: "explicit empty flags", values: []string{"manager.otlpEndpoint=", "manager.logLevel="}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rendered := renderChartTemplate(t, managerTemplate, tc.values...)
			if tc.want != "" && !strings.Contains(rendered, "- "+tc.want+"\n") {
				t.Fatalf("missing argument %q in rendered manager:\n%s", tc.want, rendered)
			}
			for _, flag := range flags {
				if !strings.HasPrefix(tc.want, flag) && strings.Contains(rendered, flag) {
					t.Errorf("unexpected argument %q with values %v", flag, tc.values)
				}
			}
			if !strings.Contains(rendered, "- --leader-elect\n") {
				t.Fatal("optional flags displaced the configured manager arguments")
			}
		})
	}
}
