/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"context"
	"crypto/tls"
	"flag"
	"os"
	"path/filepath"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	"github.com/JonathanGraniero/kflare/internal/controller"
	cfpkg "github.com/JonathanGraniero/kflare/pkg/cloudflare"
	"github.com/JonathanGraniero/kflare/pkg/metricsauth"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
	//+kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

// defaultLeaderElectionID is the Lease that every release before
// --leader-election-id held, so that upgrading an installation that keeps it
// never runs two leaders at once.
const defaultLeaderElectionID = "a634e97e.kflare.dev"

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(cloudflarev1alpha1.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var metricsCertPath string
	var enableHTTP2 bool
	var defaultDeletionPolicy string
	var reconcileTimeout time.Duration
	var driftCheckInterval time.Duration
	var tokenCheckInterval time.Duration
	var tokenExpiryWarning time.Duration
	var cloudflareRateLimit int
	var maxConcurrentReconciles int
	var leaderElectionID string
	var watchNamespaces string
	var watchSelector string
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.StringVar(&leaderElectionID, "leader-election-id", defaultLeaderElectionID,
		"The name of the Lease, in the manager's namespace, that leader election holds. Installations that share a "+
			"namespace need one each.")
	flag.StringVar(&watchNamespaces, "watch-namespaces", "",
		"The namespaces whose kflare resources this installation manages, separated by commas: it neither watches "+
			"nor caches anything in other namespaces, Secrets included. It manages a CloudflareAccount only if the "+
			"account's token Secret is in one of them. Empty manages every namespace.")
	flag.StringVar(&watchSelector, "watch-selector", "",
		"A label selector, such as kflare.dev/instance=team-a, that the kflare resources and CloudflareAccounts this "+
			"installation manages must match: it leaves the others alone, their finalizers included. Empty manages "+
			"all of them.")
	flag.BoolVar(&secureMetrics, "metrics-secure", false,
		"If set, the metrics endpoint is served over HTTPS, and each request must carry a token that the API server "+
			"authenticates (TokenReview) and authorizes for GET on /metrics (SubjectAccessReview)")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory holding the metrics server's certificate (tls.crt) and key (tls.key), reloaded when they "+
			"change. Without it, the secure metrics endpoint uses a self-signed certificate.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	flag.StringVar(&defaultDeletionPolicy, "default-deletion-policy", reconciler.DeletionPolicyDelete,
		"What deleting a resource without a kflare.dev/deletion-policy annotation does to the Cloudflare object "+
			"kflare created for it: delete or retain it. An object kflare adopted is kept unless the annotation "+
			"says delete.")
	flag.DurationVar(&reconcileTimeout, "reconcile-timeout", 5*time.Minute,
		"How long a reconcile may run. A reconcile that runs out of time, such as one waiting on a call that gets "+
			"no answer, ends with reason APIError and is retried with back-off, and its controller goes on to the "+
			"next resource. 0 turns the timeout off.")
	flag.DurationVar(&driftCheckInterval, "drift-check-interval", 0,
		"How long after a successful reconcile a resource is reconciled again, so that a change made outside kflare "+
			"(in the Cloudflare dashboard, through the API, by Terraform) is corrected within about that long. Each "+
			"check is moved by up to a tenth either way at random, and makes one or more Cloudflare API calls per "+
			"resource, which count against Cloudflare's rate limit. 0 turns the checks off.")
	flag.DurationVar(&tokenCheckInterval, "token-check-interval", time.Hour,
		"How long after a CloudflareAccount is validated it is validated again: its API token's status and expiry "+
			"read from Cloudflare, and the account read with it. A token revoked, disabled or expired in Cloudflare "+
			"then shows on its account within about that long. 0 turns the checks off.")
	flag.DurationVar(&tokenExpiryWarning, "token-expiry-warning", 14*24*time.Hour,
		"How long before its API token expires a CloudflareAccount starts to record a TokenExpiresSoon Warning "+
			"Event at each validation. 0 records none.")
	flag.IntVar(&cloudflareRateLimit, "cloudflare-rate-limit", cfpkg.DefaultRateLimit,
		"How many requests per five minutes kflare sends with one Cloudflare API token, unless the token's "+
			"CloudflareAccount sets spec.rateLimit. Cloudflare's global limit is 1,200, shared with everything else "+
			"using the token. 0 turns the pacing off; kflare still stops when Cloudflare asks it to.")
	flag.IntVar(&maxConcurrentReconciles, "max-concurrent-reconciles", 1,
		"How many resources of one kind each controller reconciles at once. The rules of one zone and phase are "+
			"still written one at a time, as are resources of a kind finding or creating an object with the same "+
			"key. More at once take more CPU and Kubernetes API calls at a time, but no more of an API token's "+
			"Cloudflare budget (--cloudflare-rate-limit), which still paces every call.")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	if err := reconciler.SetDefaultDeletionPolicy(defaultDeletionPolicy); err != nil {
		setupLog.Error(err, "invalid --default-deletion-policy")
		os.Exit(1)
	}
	if reconcileTimeout < 0 {
		setupLog.Error(nil, "--reconcile-timeout must be 0 (off) or a positive duration", "value", reconcileTimeout)
		os.Exit(1)
	}
	if err := reconciler.SetDriftCheckInterval(driftCheckInterval); err != nil {
		setupLog.Error(err, "invalid --drift-check-interval")
		os.Exit(1)
	}
	if tokenCheckInterval < 0 {
		setupLog.Error(nil, "--token-check-interval must be 0 (off) or a positive duration", "value",
			tokenCheckInterval)
		os.Exit(1)
	}
	if tokenExpiryWarning < 0 {
		setupLog.Error(nil, "--token-expiry-warning must be 0 (off) or a positive duration", "value",
			tokenExpiryWarning)
		os.Exit(1)
	}
	if maxConcurrentReconciles < 1 {
		setupLog.Error(nil, "--max-concurrent-reconciles must be 1 or more", "value", maxConcurrentReconciles)
		os.Exit(1)
	}
	if err := cfpkg.SetRateLimit(cloudflareRateLimit); err != nil {
		setupLog.Error(err, "invalid --cloudflare-rate-limit")
		os.Exit(1)
	}
	if problems := validation.IsDNS1123Subdomain(leaderElectionID); len(problems) > 0 {
		setupLog.Error(nil, "invalid --leader-election-id", "value", leaderElectionID, "problems", problems)
		os.Exit(1)
	}

	restConfig := ctrl.GetConfigOrDie()
	// The scope reads past the manager's cache, which holds only what the
	// installation manages, to tell a reference outside it from a missing one.
	apiReader, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		setupLog.Error(err, "unable to create a Kubernetes client")
		os.Exit(1)
	}
	scope, err := controller.NewWatchScope(watchNamespaces, watchSelector, apiReader)
	if err != nil {
		setupLog.Error(err, "invalid --watch-namespaces or --watch-selector")
		os.Exit(1)
	}
	setupLog.Info("managing kflare resources", "scope", scope.String())

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancelation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	tlsOpts := []func(*tls.Config){}
	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	webhookServer := webhook.NewServer(webhook.Options{
		TLSOpts: tlsOpts,
	})

	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}
	if secureMetrics {
		// Authenticate and authorize every request to the metrics endpoint
		// against the API server, so that only callers granted GET on
		// /metrics (the chart's metrics-reader ClusterRole) can scrape it.
		metricsServerOptions.FilterProvider = metricsauth.WithAuthenticationAndAuthorization
	}
	if metricsCertPath != "" {
		if !secureMetrics {
			setupLog.Error(nil, "--metrics-cert-path needs --metrics-secure")
			os.Exit(1)
		}
		// The metrics server falls back to a self-signed certificate when
		// these files are missing, so a mistyped mount would go unnoticed.
		for _, name := range []string{"tls.crt", "tls.key"} {
			if _, err := os.Stat(filepath.Join(metricsCertPath, name)); err != nil {
				setupLog.Error(err, "unable to read the metrics server's certificate")
				os.Exit(1)
			}
		}
		metricsServerOptions.CertDir = metricsCertPath
	}

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		// The cache holds only what the installation manages, and every
		// reconcile and watch handler gets the scope in its context.
		Cache:       scope.CacheOptions(scheme),
		BaseContext: func() context.Context { return controller.WithWatchScope(context.Background(), scope) },
		// Every controller cancels a reconcile's context after
		// --reconcile-timeout, which both Cloudflare SDKs and the Kubernetes
		// client honor, and runs --max-concurrent-reconciles at once: no
		// controller sets a number of its own.
		Controller: config.Controller{
			ReconciliationTimeout:   reconcileTimeout,
			MaxConcurrentReconciles: maxConcurrentReconciles,
		},
		LeaderElection:   enableLeaderElection,
		LeaderElectionID: leaderElectionID,
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
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}
	controller.SetEventRecorder(mgr.GetEventRecorder("kflare"))

	if err = (&controller.CloudflareAccountReconciler{
		Client:             mgr.GetClient(),
		Scheme:             mgr.GetScheme(),
		TokenCheckInterval: tokenCheckInterval,
		TokenExpiryWarning: tokenExpiryWarning,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "CloudflareAccount")
		os.Exit(1)
	}
	// The kinds kflare-gen generates, KVNamespace among them.
	if err = controller.SetupGeneratedReconcilers(mgr); err != nil {
		setupLog.Error(err, "unable to create controllers")
		os.Exit(1)
	}
	//+kubebuilder:scaffold:builder

	if err := controller.SetupMetrics(mgr, scope); err != nil {
		setupLog.Error(err, "unable to set up metrics")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
