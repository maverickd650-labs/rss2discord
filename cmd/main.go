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

package main

import (
	"context"
	"crypto/tls"
	"flag"
	"net/http"
	"os"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	rss2discordv1alpha1 "github.com/maverickd650/rss2discord-operator/api/v1alpha1"
	"github.com/maverickd650/rss2discord-operator/internal/controller"
	"github.com/maverickd650/rss2discord-operator/internal/discord"
	"github.com/maverickd650/rss2discord-operator/internal/rss"
	"github.com/maverickd650/rss2discord-operator/internal/telemetry"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(rss2discordv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// exitOnError logs err with msg and the given key/value pairs and exits the
// process if err is non-nil, mirroring the manager's own fatal-error handling.
func exitOnError(err error, msg string, keysAndValues ...any) {
	if err != nil {
		setupLog.Error(err, msg, keysAndValues...)
		os.Exit(1)
	}
}

// newMetricsServerOptions builds the controller-runtime metrics server
// configuration from the equivalent command-line flags.
func newMetricsServerOptions(metricsAddr string, secureMetrics bool, tlsOpts []func(*tls.Config),
	metricsCertPath, metricsCertName, metricsCertKey string) metricsserver.Options {
	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.25.0/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.25.0/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	return metricsServerOptions
}

func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var maxConcurrentFeedGroupReconciles int
	var otlpEndpoint string
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics server")
	flag.IntVar(&maxConcurrentFeedGroupReconciles, "max-concurrent-feedgroup-reconciles", 1,
		"The maximum number of FeedGroups the controller will reconcile concurrently.")
	flag.StringVar(&otlpEndpoint, "otlp-endpoint", "",
		"OTLP/gRPC endpoint (host:port) to export traces to for outbound RSS fetch and Discord webhook "+
			"requests. Leave empty to disable tracing entirely.")
	// Development defaults to false so production deployments get
	// structured JSON logs at info level rather than zap's human-oriented
	// console encoder with debug-level and stack-traced warnings. Pass
	// -zap-devel to opt into the development encoder locally.
	opts := zap.Options{
		Development: false,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("Disabling HTTP/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	metricsServerOptions := newMetricsServerOptions(metricsAddr, secureMetrics, tlsOpts,
		metricsCertPath, metricsCertName, metricsCertKey)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Client: client.Options{
			// Secrets are only ever fetched by name (the webhook URL secret
			// referenced by a FeedGroup), so route them around the informer
			// cache. Caching them would make the client list/watch *all*
			// Secrets cluster-wide, which needs RBAC far beyond the `get`
			// the operator actually requires.
			Cache: &client.CacheOptions{
				DisableFor: []client.Object{&corev1.Secret{}},
			},
		},
		Metrics:                metricsServerOptions,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "ca714037.rss2discord.maverickd650.dev",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	exitOnError(err, "Failed to start manager")

	// Tracing is opt-in: with no --otlp-endpoint, transportWrap stays nil and
	// rss.NewClientWithTransportWrap / discord.NewHTTPClientWithTransportWrap
	// behave exactly like the unwrapped constructors they replace.
	var transportWrap func(http.RoundTripper) http.RoundTripper
	if otlpEndpoint != "" {
		tp, shutdown, err := telemetry.Setup(context.Background(), otlpEndpoint)
		exitOnError(err, "Failed to set up tracing", "otlp-endpoint", otlpEndpoint)
		transportWrap = telemetry.HTTPTransportWrap(tp)

		exitOnError(mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
			<-ctx.Done()
			return shutdown(context.Background())
		})), "Failed to register tracer shutdown")
	}

	// RSSClient, the Discord HTTP client, and the Discord rate limiter are
	// constructed once here and shared across every reconcile, rather than
	// per-reconcile: the HTTP client so keep-alive connections to
	// feed/webhook hosts are actually pooled instead of being torn down and
	// rebuilt every reconciliation, and the rate limiter so a 429 seen while
	// sending for one FeedGroup also holds off every other FeedGroup sharing
	// the same webhook (or racing it once
	// --max-concurrent-feedgroup-reconciles > 1) instead of each discovering
	// the limit independently.
	rssClient := rss.NewClientWithTransportWrap(transportWrap)
	discordHTTPClient := discord.NewHTTPClientWithTransportWrap(transportWrap)
	discordRateLimiter := discord.NewRateLimiter()

	err = (&controller.FeedGroupReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		RSSClient: rssClient,
		DiscordClientBuilder: func(webhookURL string) *discord.Client {
			return discord.NewClientWithLimiter(webhookURL, discordHTTPClient, discordRateLimiter)
		},
		MaxConcurrentReconciles: maxConcurrentFeedGroupReconciles,
	}).SetupWithManager(mgr)
	exitOnError(err, "Failed to create controller", "controller", "feedgroup")
	// +kubebuilder:scaffold:builder

	exitOnError(mgr.AddHealthzCheck("healthz", healthz.Ping), "Failed to set up health check")
	exitOnError(mgr.AddReadyzCheck("readyz", healthz.Ping), "Failed to set up ready check")

	setupLog.Info("Starting manager")
	exitOnError(mgr.Start(ctrl.SetupSignalHandler()), "Failed to run manager")
}
