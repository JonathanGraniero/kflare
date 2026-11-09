/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// startManager runs a manager for the rest of the spec, with what setup
// registers, so that a spec can check a kind's watches: they do nothing outside
// a running manager.
func startManager(ctx context.Context, setup func(mgr manager.Manager) error) {
	GinkgoHelper()
	startScopedManager(ctx, cfg, WatchScope{}, setup)
}

// startScopedManager runs, for the rest of the spec, a manager of an
// installation scoped by scope, as cmd/main.go sets one up, which calls the API
// server as the user of restConfig, with what setup registers.
func startScopedManager(ctx context.Context, restConfig *rest.Config, scope WatchScope,
	setup func(mgr manager.Manager) error) {
	GinkgoHelper()
	mgr, err := manager.New(restConfig, manager.Options{
		Controller:  config.Controller{SkipNameValidation: new(true)},
		Metrics:     metricsserver.Options{BindAddress: "0"},
		Cache:       scope.CacheOptions(scheme.Scheme),
		BaseContext: func() context.Context { return WithWatchScope(context.Background(), scope) },
	})
	Expect(err).NotTo(HaveOccurred())
	Expect(setup(mgr)).To(Succeed())
	mgrCtx, stop := context.WithCancel(ctx)
	stopped := make(chan struct{})
	go func() {
		defer GinkgoRecover()
		defer close(stopped)
		Expect(mgr.Start(mgrCtx)).To(Succeed())
	}()
	DeferCleanup(func() {
		stop()
		<-stopped
	})
	Expect(mgr.GetCache().WaitForCacheSync(mgrCtx)).To(BeTrue())
}
