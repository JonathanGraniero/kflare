/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// startManager runs a manager for the rest of the spec, with what setup
// registers, so that a spec can check a kind's watches: they do nothing outside
// a running manager.
func startManager(ctx context.Context, setup func(mgr manager.Manager) error) {
	GinkgoHelper()
	mgr, err := manager.New(cfg, manager.Options{
		Controller: config.Controller{SkipNameValidation: new(true)},
		Metrics:    metricsserver.Options{BindAddress: "0"},
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
