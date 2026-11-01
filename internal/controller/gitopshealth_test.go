/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"net/http"
	"path/filepath"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/JonathanGraniero/kflare/pkg/reconciler"
	"github.com/JonathanGraniero/kflare/test/gitopshealth"
)

// gitopsHealthChecks reads the health checks docs/deploy.md gives Argo CD and Flux (config/argocd/health.lua and
// config/flux/health-check-exprs.yaml), once.
var gitopsHealthChecks = sync.OnceValues(func() (*gitopshealth.Checks, error) {
	return gitopshealth.Load(filepath.Join("..", ".."))
})

// expectGitOpsHealth checks the health Argo CD and Flux give obj, as the controller left it, with those checks. A
// Flux status of Unknown comes with the error Flux reports it for, and any other with none.
func expectGitOpsHealth(obj conditionedObject, argoCD, flux string) {
	GinkgoHelper()
	checks, err := gitopsHealthChecks()
	Expect(err).NotTo(HaveOccurred())
	health, err := checks.ArgoCD(obj)
	Expect(err).NotTo(HaveOccurred())
	Expect(health.Status).To(Equal(argoCD), "Argo CD's health, with the message %q", health.Message)
	status, err := checks.Flux(obj)
	Expect(status).To(Equal(flux), "Flux's status, with the error %v", err)
	if flux == gitopshealth.FluxUnknown {
		Expect(err).To(HaveOccurred())
	} else {
		Expect(err).NotTo(HaveOccurred())
	}
}

// describeCRUDGitOpsHealth runs the specs of a generated kind's health in Argo CD and Flux: the checks the deploy
// guide documents judge the resource by the Ready condition its controller writes, for its current generation.
func describeCRUDGitOpsHealth[O conditionedObject, A any, R any, L any](e *crudSpecEnv[O, A, R, L]) {
	const (
		healthy, progressing = gitopshealth.ArgoCDHealthy, gitopshealth.ArgoCDProgressing
		degraded, suspended  = gitopshealth.ArgoCDDegraded, gitopshealth.ArgoCDSuspended
		current, inProgress  = gitopshealth.FluxCurrent, gitopshealth.FluxInProgress
		failed, unknown      = gitopshealth.FluxFailed, gitopshealth.FluxUnknown
	)
	t, k := e.t, e.t.kind

	Describe("its health in Argo CD and Flux", func() {
		It("is Progressing until it is first reconciled, which Flux cannot judge before it has a status", func() {
			e.createParent(true)
			e.create(e.key, nil)
			expectGitOpsHealth(e.get(), progressing, unknown)
		})

		It("is Healthy and Current once synced, and Progressing and InProgress until a change is reconciled", func() {
			e.synced(nil)
			expectGitOpsHealth(e.get(), healthy, current)
			if len(t.changes) == 0 {
				return
			}

			obj := e.get()
			t.changes[0].edit(obj)
			Expect(k8sClient.Update(e.ctx, obj)).To(Succeed())
			expectGitOpsHealth(e.get(), progressing, inProgress)
			_, err := e.reconcileIt()
			Expect(err).NotTo(HaveOccurred())
			expectGitOpsHealth(e.get(), healthy, current)
		})

		It("is Degraded and Failed when Cloudflare refuses it, and Progressing and InProgress once its spec changes",
			func() {
				e.createParent(true)
				e.create(e.key, nil)
				e.store.createErr = v7APIError(http.MethodPost, "parents/"+e.parentID+"/objects", http.StatusBadRequest,
					1000, "refused")
				_, err := e.reconcileIt()
				Expect(err).NotTo(HaveOccurred())
				Expect(e.ready().Reason).To(Equal("TerminalError"))
				expectGitOpsHealth(e.get(), degraded, failed)
				if len(t.changes) == 0 {
					return
				}

				obj := e.get()
				t.changes[0].edit(obj)
				Expect(k8sClient.Update(e.ctx, obj)).To(Succeed())
				expectGitOpsHealth(e.get(), progressing, inProgress)
			})

		It("is Progressing and InProgress while Cloudflare's API fails", func() {
			e.createParent(true)
			e.create(e.key, nil)
			e.store.createErr = v7APIError(http.MethodPost, "parents/"+e.parentID+"/objects",
				http.StatusServiceUnavailable, 0, "")
			_, err := e.reconcileIt()
			Expect(err).To(HaveOccurred())
			Expect(e.ready().Reason).To(Equal("APIError"))
			expectGitOpsHealth(e.get(), progressing, inProgress)
		})

		It("is Degraded and Failed when its "+k.noun+" exists and it has no adoption policy", func() {
			e.createParent(true)
			obj := e.create(e.key, nil)
			e.seed("existing", k.key(obj))
			_, err := e.reconcileIt()
			Expect(err).NotTo(HaveOccurred())
			Expect(e.ready().Reason).To(Equal(reasonAlreadyExists))
			expectGitOpsHealth(e.get(), degraded, failed)
		})

		It("is Progressing and InProgress while its account is not ready", func() {
			e.createParent(false)
			e.create(e.key, nil)
			_, err := e.reconcileIt()
			Expect(err).NotTo(HaveOccurred())
			Expect(e.ready().Reason).To(Equal("AccountNotReady"))
			expectGitOpsHealth(e.get(), progressing, inProgress)
		})

		It("is Suspended while paused, and Current for Flux while the pause keeps it Ready", func() {
			e.synced(nil)
			setPaused(e.ctx, e.get(), true)
			_, err := e.reconcileIt()
			Expect(err).NotTo(HaveOccurred())
			Expect(e.ready().Reason).To(Equal(reasonPaused))
			Expect(e.ready().Status).To(Equal(metav1.ConditionTrue))
			expectGitOpsHealth(e.get(), suspended, current)
		})

		It("is Suspended when created paused, and InProgress for Flux, as it has never been Ready", func() {
			e.createParent(true)
			obj := t.newResource(e.key, e.parentName)
			obj.SetAnnotations(map[string]string{reconciler.PausedAnnotation: "true"})
			Expect(k8sClient.Create(e.ctx, obj)).To(Succeed())
			_, err := e.reconcileIt()
			Expect(err).NotTo(HaveOccurred())
			Expect(e.ready().Status).To(Equal(metav1.ConditionUnknown))
			expectGitOpsHealth(e.get(), suspended, inProgress)
		})
	})
}
