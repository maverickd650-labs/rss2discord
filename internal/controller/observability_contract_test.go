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
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// allFailureClasses enumerates every failureClass literal in classify.go.
// TestFailureClassExhaustiveness guards this list against going stale: a new
// failureClass{...} added to classify.go without a matching entry here fails
// loudly instead of silently escaping this contract test's coverage.
var allFailureClasses = []failureClass{
	classNotFound,
	classGone,
	classRateLimited,
	classServerError,
	classClientError,
	classTimeout,
	classDNSFailure,
	classParseError,
	classUnrecognizedFormat,
	classWebhookInvalid,
	classBlockedAddress,
	classTLSError,
	classOther,
}

// expectedOutcomes is every outcome label value feedOperationsTotal can ever
// be incremented with in production -- the "reality" side of the contract
// that the dashboard and alerts in dist/chart are checked against. It is
// enumerated from feedAttempt's own outcome kinds (see attemptOutcome.metricLabel,
// the only place a label is chosen), crossed with every failureClass, so a
// kind that starts counting under a new label shows up here without anyone
// remembering to list it.
func expectedOutcomes() []string {
	var outcomes []string
	for kind := attemptFetchSucceeded; kind < attemptKindEnd; kind++ {
		for _, class := range allFailureClasses {
			label := attemptOutcome{kind: kind}.metricLabel(class)
			if label != "" && !slices.Contains(outcomes, label) {
				outcomes = append(outcomes, label)
			}
		}
	}
	return outcomes
}

// metricSelectorPattern finds every rss2discord_feed_operations_total{...}
// label selector in a rendered chart document, so outcome matchers are only
// pulled from selectors against the metric this contract is about.
var metricSelectorPattern = regexp.MustCompile(`rss2discord_feed_operations_total\{([^}]*)\}`)

// outcomeMatcherPattern extracts an outcome<op>"<value>" matcher from a label
// selector body. The dashboard JSON is embedded into its ConfigMap via
// .Files.Get, so its quotes render backslash-escaped (\"sent\"); the
// PrometheusRule's expr is plain YAML with unescaped quotes ("sent"). The
// optional `\\?` handles both. `=~`/`!~` must be tried before the plain `=`
// alternative in the group, or `=~` would only ever match its leading `=`.
var outcomeMatcherPattern = regexp.MustCompile(`outcome(=~|!~|=)\\?"([^"\\]*)\\?"`)

// outcomeMatcher is one outcome label matcher extracted from a rendered
// chart document -- this is a pragmatic string/regex scan, not a PromQL
// parser.
type outcomeMatcher struct {
	op      string // "=", "=~", or "!~"
	pattern string
}

// namesOutcome reports whether m's pattern names/covers outcome, anchoring
// regex matchers the way Prometheus does (^(?:...)$, not substring). This is
// deliberately naming semantics, not Prometheus series-selection semantics:
// for a `!~` matcher (e.g. the send-ratio panel's denominator
// outcome!~"rate_limited"), the dashboard author had to name "rate_limited"
// to carve it out, which is itself evidence the dashboard accounts for that
// outcome -- true negated-selection semantics would instead say
// "rate_limited" is the one outcome that panel *excludes*, which would wrongly
// flag a documented, intentional exception (see CLAUDE.md/the plan) as a
// coverage gap.
func (m outcomeMatcher) namesOutcome(outcome string) bool {
	if m.op == "=" {
		return outcome == m.pattern
	}
	re := regexp.MustCompile("^(?:" + m.pattern + ")$")
	return re.MatchString(outcome)
}

// extractOutcomeMatchers pulls every outcome matcher used against
// rss2discord_feed_operations_total out of a rendered chart document.
func extractOutcomeMatchers(t *testing.T, rendered string) []outcomeMatcher {
	t.Helper()
	var matchers []outcomeMatcher
	for _, sel := range metricSelectorPattern.FindAllStringSubmatch(rendered, -1) {
		for _, m := range outcomeMatcherPattern.FindAllStringSubmatch(sel[1], -1) {
			matchers = append(matchers, outcomeMatcher{op: m[1], pattern: m[2]})
		}
	}
	if len(matchers) == 0 {
		t.Fatalf("no outcome matchers found against rss2discord_feed_operations_total in rendered output -- chart content or extraction regex changed?")
	}
	return matchers
}

// renderChartTemplate runs `helm template`, scoped with -s to a single
// template file, with extra --set flags to enable it. It fails (never
// skips) when helm is missing -- a skippable contract isn't a contract, and
// `mise run test` (which this runs under) provides helm via mise.
func renderChartTemplate(t *testing.T, showOnly string, extraSet ...string) string {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Fatalf("helm not found on PATH -- this test requires it (run via `mise run test`, which installs the mise-pinned helm): %v", err)
	}

	chartDir := filepath.Join("..", "..", "dist", "chart")
	args := append([]string{"template", "observability-contract-test", chartDir, "-s", showOnly}, extraSet...)

	out, err := exec.Command("helm", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template -s %s failed: %v\n%s", showOnly, err, out)
	}
	return string(out)
}

// anyMatcherNames reports whether any matcher in matchers names outcome
// (see outcomeMatcher.namesOutcome).
func anyMatcherNames(matchers []outcomeMatcher, outcome string) bool {
	for _, m := range matchers {
		if m.namesOutcome(outcome) {
			return true
		}
	}
	return false
}

// TestObservabilityContract cross-checks the outcome labels
// internal/controller can actually emit against the matchers in the
// rendered Grafana dashboard and PrometheusRule, in both directions -- see
// CLAUDE.md's "metrics use a single outcome label" section for why this
// contract has to hold.
func TestObservabilityContract(t *testing.T) {
	dashboard := renderChartTemplate(t, "templates/prometheus/grafana-dashboard.yaml", "--set", "grafanaDashboard.enabled=true")
	rule := renderChartTemplate(t, "templates/prometheus/prometheus-rule.yaml", "--set", "prometheusRule.enabled=true")

	dashboardMatchers := extractOutcomeMatchers(t, dashboard)
	ruleMatchers := extractOutcomeMatchers(t, rule)

	outcomes := expectedOutcomes()

	// Reality -> dashboard: every outcome the controller can emit shows up
	// on at least one dashboard panel.
	for _, outcome := range outcomes {
		if !anyMatcherNames(dashboardMatchers, outcome) {
			t.Errorf("outcome %q has no dashboard matcher in dist/chart/dashboards/feedgroup-overview.json", outcome)
		}
	}

	// Reality -> alerts: every fetch_error_*/send_error_*/rate_limited
	// outcome is covered by an alert selector. render_error is deliberately
	// dashboard-only (no alert -- it's a template bug, not feed/webhook
	// health) and "sent" is a success outcome; both are excluded by the
	// prefix/equality check below, not treated as failures.
	for _, outcome := range outcomes {
		isAlertable := outcome == outcomeRateLimited ||
			strings.HasPrefix(outcome, outcomeFetchError+"_") ||
			strings.HasPrefix(outcome, outcomeSendError+"_")
		if !isAlertable {
			continue
		}
		if !anyMatcherNames(ruleMatchers, outcome) {
			t.Errorf("outcome %q has no PrometheusRule alert selector in dist/chart/templates/prometheus/prometheus-rule.yaml", outcome)
		}
	}

	// Dashboard/alerts -> reality: no dead regex left behind naming no
	// outcome the controller actually emits (e.g. a stale or typo'd
	// reason). Negative (!~) matchers are excluded from this direction --
	// they're only ever seeded with a real, deliberately-named exclusion
	// (see namesOutcome), so "dead" isn't a meaningful property for them.
	all := append(append([]outcomeMatcher{}, dashboardMatchers...), ruleMatchers...)
	for _, m := range all {
		if m.op == "!~" {
			continue
		}
		if !anyOutcomeNamedBy(m, outcomes) {
			t.Errorf("matcher outcome%s%q in dist/chart matches no outcome the controller ever emits", m.op, m.pattern)
		}
	}
}

// anyOutcomeNamedBy reports whether m names any outcome in outcomes.
func anyOutcomeNamedBy(m outcomeMatcher, outcomes []string) bool {
	return slices.ContainsFunc(outcomes, m.namesOutcome)
}

// TestFailureClassExhaustiveness reads classify.go's source so a new
// failureClass{...} literal can't be added there without also being added to
// allFailureClasses above (and therefore checked by TestObservabilityContract).
// The count only considers assignment-form literals (`= failureClass{...}`,
// i.e. the class* vars) -- classifyNetworkError's `return failureClass{},
// false` zero-value literal isn't one of the classes and must not count.
func TestFailureClassExhaustiveness(t *testing.T) {
	src, err := os.ReadFile("classify.go")
	if err != nil {
		t.Fatalf("reading classify.go: %v", err)
	}

	literalPattern := regexp.MustCompile(`=\s*failureClass\{[^}]+\}`)
	if got, want := len(literalPattern.FindAllString(string(src), -1)), len(allFailureClasses); got != want {
		t.Fatalf("classify.go defines %d failureClass{...} literals but allFailureClasses has %d -- update allFailureClasses in observability_contract_test.go to match", got, want)
	}
}
