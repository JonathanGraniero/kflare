/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
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
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	"github.com/JonathanGraniero/kflare/internal/controller"
	"github.com/JonathanGraniero/kflare/pkg/metricsauth"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
	//+kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

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
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
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

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		// Every controller cancels a reconcile's context after
		// --reconcile-timeout, which both Cloudflare SDKs and the Kubernetes
		// client honor.
		Controller:       config.Controller{ReconciliationTimeout: reconcileTimeout},
		LeaderElection:   enableLeaderElection,
		LeaderElectionID: "a634e97e.kflare.dev",
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
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "CloudflareAccount")
		os.Exit(1)
	}
	if err = (&controller.ZoneReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Zone")
		os.Exit(1)
	}
	if err = (&controller.DNSRecordReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "DNSRecord")
		os.Exit(1)
	}
	if err = (&controller.TunnelReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Tunnel")
		os.Exit(1)
	}
	if err = (&controller.TunnelConfigurationReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "TunnelConfiguration")
		os.Exit(1)
	}
	if err = (&controller.WorkerScriptReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "WorkerScript")
		os.Exit(1)
	}
	// The kinds kflare-gen generates, KVNamespace among them.
	if err = controller.SetupGeneratedReconcilers(mgr); err != nil {
		setupLog.Error(err, "unable to create controllers")
		os.Exit(1)
	}
	//+kubebuilder:scaffold:builder

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
