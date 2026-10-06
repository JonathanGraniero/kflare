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

// The deployment smoke test: the image built from this checkout runs with
// the default kustomization (RBAC, kube-rbac-proxy sidecar, probes).
var _ = Describe("controller-manager", func() {
	BeforeEach(func() {
		if externalManager() {
			Skip("KFLARE_E2E_MANAGER=external: the manager is not deployed by this suite")
		}
	})

	It("runs one pod whose containers are all ready and have not restarted", func() {
		Eventually(func() error {
			out, err := utils.Run(exec.Command("kubectl", "get", "pods", "-n", managerNamespace,
				"-l", "control-plane=controller-manager", "-o",
				`go-template={{ range .items }}{{ if not .metadata.deletionTimestamp }}`+
					`{{ .metadata.name }} {{ .status.phase }}{{ range .status.containerStatuses }}`+
					` {{ .ready }}/{{ .restartCount }}{{ end }}{{ "\n" }}{{ end }}{{ end }}`))
			if err != nil {
				return err
			}
			pods := utils.GetNonEmptyLines(string(out))
			if len(pods) != 1 {
				return fmt.Errorf("expected 1 controller pod, got %d: %v", len(pods), pods)
			}
			fields := strings.Fields(pods[0])
			if fields[1] != "Running" {
				return fmt.Errorf("controller pod is %s", fields[1])
			}
			for _, c := range fields[2:] {
				if c != "true/0" {
					return fmt.Errorf("container not ready or restarted (ready/restarts = %s)", c)
				}
			}
			return nil
		}, 2*time.Minute, time.Second).Should(Succeed())
	})

	It("reaches the API server and becomes leader", func() {
		// A ready pod only proves the probes answer; holding the lease proves
		// the manager can talk to the API server and has started its controllers.
		Eventually(func() ([]string, error) {
			out, err := utils.Run(exec.Command("kubectl", "get", "leases", "-n", managerNamespace,
				"-o", "jsonpath={.items[*].spec.holderIdentity}"))
			return strings.Fields(string(out)), err
		}, 2*time.Minute, 2*time.Second).ShouldNot(BeEmpty())
	})
})
