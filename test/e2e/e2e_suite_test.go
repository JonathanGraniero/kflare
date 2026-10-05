/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package e2e

import (
	"fmt"
	"os/exec"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/JonathanGraniero/kflare/test/utils"
)

const (
	// managerNamespace is where config/default deploys the manager.
	managerNamespace = "cloudflare-operator-system"

	// managerImage is the image built from this checkout and loaded into kind.
	managerImage = "example.com/kflare:e2e"
)

// TestE2E runs the e2e suite against the kind cluster in the current
// kubeconfig context ($KIND_CLUSTER names it for image loading).
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	logf.SetLogger(GinkgoLogr)
	fmt.Fprintf(GinkgoWriter, "Starting kflare e2e suite\n")
	RunSpecs(t, "e2e suite")
}

// The manager is deployed once for the whole suite: Ginkgo shuffles
// top-level containers, and every spec needs it running.
var _ = BeforeSuite(func() {
	By("creating manager namespace")
	_, _ = utils.Run(exec.Command("kubectl", "create", "ns", managerNamespace))

	By("building the manager image")
	_, err := utils.Run(exec.Command("make", "docker-build", "IMG="+managerImage))
	Expect(err).NotTo(HaveOccurred())

	By("loading the manager image on kind")
	Expect(utils.LoadImageToKindClusterWithName(managerImage)).To(Succeed())

	By("installing CRDs")
	_, err = utils.Run(exec.Command("make", "install"))
	Expect(err).NotTo(HaveOccurred())

	By("deploying the controller-manager")
	_, err = utils.Run(exec.Command("make", "deploy", "IMG="+managerImage))
	Expect(err).NotTo(HaveOccurred())

	By("waiting for the controller-manager to become available")
	_, err = utils.Run(exec.Command("kubectl", "wait", "deployment", "-n", managerNamespace,
		"-l", "control-plane=controller-manager", "--for=condition=Available", "--timeout=3m"))
	Expect(err).NotTo(HaveOccurred())
})

// ReportAfterEach prints the manager's recent log after a failed spec, before
// AfterSuite undeploys it.
var _ = ReportAfterEach(func(report SpecReport) {
	if !report.Failed() {
		return
	}
	out, _ := utils.Run(exec.Command("kubectl", "logs", "-n", managerNamespace,
		"-l", "control-plane=controller-manager", "-c", "manager", "--tail=100"))
	fmt.Fprintf(GinkgoWriter, "controller-manager log:\n%s\n", out)
})

var _ = AfterSuite(func() {
	By("undeploying the controller-manager")
	_, _ = utils.Run(exec.Command("make", "undeploy", "ignore-not-found=true"))

	By("removing manager namespace")
	_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", managerNamespace, "--ignore-not-found"))
})
