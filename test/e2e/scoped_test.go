/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package e2e

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/JonathanGraniero/kflare/test/utils"
)

// A second installation, scoped to a namespace of its own with the chart's
// namespaced RBAC (watchNamespaces), runs alongside the suite's: it holds a
// Lease of its own, its controllers start with nothing but its Role and its
// ClusterRole's cluster-wide rules, and it can read no other namespace's
// Secrets. Its namespace holds no kflare resource, which the suite's
// installation, which watches every namespace, would also reconcile.
var _ = Describe("an installation scoped to a namespace", Ordered, func() {
	const (
		scopedRelease   = "kflare-scoped"
		scopedNamespace = "kflare-e2e-scoped"
		// scopedLease is the Lease the chart names after a scoped release.
		scopedLease = scopedRelease + ".kflare.dev"
	)
	scopedSelector := "app.kubernetes.io/name=kflare,app.kubernetes.io/instance=" + scopedRelease
	serviceAccount := "system:serviceaccount:" + scopedNamespace + ":" + scopedRelease

	BeforeAll(func() {
		if externalManager() {
			Skip("KFLARE_E2E_MANAGER=external: the manager is not deployed by this suite")
		}
		if upgradeFrom() != "" {
			Skip("KFLARE_E2E_UPGRADE_FROM is set: the suite runs only the upgrade specs")
		}

		By("installing a second release of the chart, scoped to its own namespace, without the CRDs")
		_, err := utils.Run(exec.Command("kubectl", "create", "namespace", scopedNamespace))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_, _ = utils.Run(exec.Command("kubectl", "delete", "namespace", scopedNamespace, "--ignore-not-found"))
		})
		_, err = utils.Run(exec.Command("helm", "install", scopedRelease, "helm/kflare", "--namespace", scopedNamespace,
			"--set", "image.repository="+managerImageRepository+",image.tag="+managerImageTag,
			"--set", "watchNamespaces={"+scopedNamespace+"},crds.enabled=false,rbac.aggregateToDefaultRoles=false",
			"--set", "autoscaling.enabled=false", "--wait", "--timeout", "3m"))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_, _ = utils.Run(exec.Command("helm", "uninstall", scopedRelease, "--namespace", scopedNamespace,
				"--ignore-not-found", "--wait"))
		})
	})

	It("runs one pod, which holds a Lease of its own while the suite's installation holds its own", func() {
		Eventually(func() error {
			pod, err := runningControllerIn(scopedNamespace, scopedSelector)
			if err != nil {
				return err
			}
			if err := holdsLeaseIn(scopedNamespace, scopedLease, pod); err != nil {
				return err
			}
			first, err := runningController()
			if err != nil {
				return err
			}
			return holdsLease(first)
		}, 2*time.Minute, 2*time.Second).Should(Succeed())
	})

	It("starts every controller with the chart's namespaced RBAC alone", func() {
		out, err := utils.Run(exec.Command("kubectl", "get", "crds", "-o", "name"))
		Expect(err).NotTo(HaveOccurred())
		kinds := strings.Count(string(out), ".kflare.dev")
		Expect(kinds).To(BeNumerically(">", 0))

		// A controller starts its workers once its caches sync, which a list or
		// watch its permissions refuse keeps from happening.
		Eventually(func() error {
			log, err := utils.Run(exec.Command("kubectl", "logs", "-n", scopedNamespace, "-l", scopedSelector,
				"-c", "manager", "--tail=-1"))
			if err != nil {
				return err
			}
			if !strings.Contains(string(log), `"scope":"namespaces `+scopedNamespace+`, every resource"`) {
				return fmt.Errorf("the controller has not logged its scope:\n%s", log)
			}
			if started := strings.Count(string(log), `"msg":"Starting workers"`); started < kinds {
				return fmt.Errorf("%d of %d controllers have started their workers", started, kinds)
			}
			return nil
		}, 2*time.Minute, 2*time.Second).Should(Succeed())

		log, err := utils.Run(exec.Command("kubectl", "logs", "-n", scopedNamespace, "-l", scopedSelector, "-c",
			"manager", "--tail=-1"))
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.ToLower(string(log))).NotTo(ContainSubstring("forbidden"),
			"the scoped installation was refused something it needs")
	})

	It("can read the Secrets of the namespace it watches, and of no other", func() {
		// canI returns kubectl auth can-i's answer for the scoped installation's
		// ServiceAccount: its last line, as kubectl prints any warning before it,
		// and exits with 1 for no.
		canI := func(args ...string) string {
			args = append(append([]string{"auth", "can-i"}, args...), "--as", serviceAccount)
			out, _ := utils.Run(exec.Command("kubectl", args...))
			lines := utils.GetNonEmptyLines(string(out))
			if len(lines) == 0 {
				return ""
			}
			return strings.TrimSpace(lines[len(lines)-1])
		}
		Expect(canI("list", "secrets", "-n", scopedNamespace)).To(Equal("yes"))
		Expect(canI("list", "secrets", "-n", managerNamespace)).To(Equal("no"))
		Expect(canI("list", "secrets", "-n", "default")).To(Equal("no"))
		Expect(canI("list", "secrets", "--all-namespaces")).To(Equal("no"))
		Expect(canI("list", "cloudflareaccounts.kflare.dev", "--all-namespaces")).To(Equal("yes"),
			"a scoped installation reads every account")
	})
})
