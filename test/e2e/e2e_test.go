/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package e2e

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/JonathanGraniero/kflare/test/utils"
)

// The deployment smoke test: the image built from this checkout runs as the
// Helm chart installs it (RBAC, probes, secure metrics, NetworkPolicy).
var _ = Describe("controller-manager", func() {
	BeforeEach(func() {
		if externalManager() {
			Skip("KFLARE_E2E_MANAGER=external: the manager is not deployed by this suite")
		}
	})

	It("runs one pod whose containers are all ready and have not restarted", func() {
		Eventually(func() error {
			out, err := utils.Run(exec.Command("kubectl", "get", "pods", "-n", managerNamespace,
				"-l", managerSelector, "-o",
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
		// the manager can talk to the API server and has started its
		// controllers. It must be the running pod: a pod that is gone keeps
		// its name in the lease until the lease expires.
		Eventually(func() error {
			out, err := utils.Run(exec.Command("kubectl", "get", "pods", "-n", managerNamespace,
				"-l", managerSelector, "-o",
				`go-template={{ range .items }}{{ if not .metadata.deletionTimestamp }}`+
					`{{ .metadata.name }}{{ "\n" }}{{ end }}{{ end }}`))
			if err != nil {
				return err
			}
			pods := utils.GetNonEmptyLines(string(out))
			if len(pods) != 1 {
				return fmt.Errorf("expected 1 controller pod, got %d: %v", len(pods), pods)
			}
			out, err = utils.Run(exec.Command("kubectl", "get", "lease", leaderElectionID, "-n", managerNamespace,
				"-o", "jsonpath={.spec.holderIdentity}"))
			if err != nil {
				return err
			}
			// The holder identity is the pod's name, then "_" and a UUID.
			if holder := string(out); !strings.HasPrefix(holder, pods[0]+"_") {
				return fmt.Errorf("the lease is held by %q, not by the running pod %s", holder, pods[0])
			}
			return nil
		}, 2*time.Minute, 2*time.Second).Should(Succeed())
	})

	It("serves metrics over HTTPS, only to a token allowed to get /metrics", func() {
		By("creating a ServiceAccount bound to the chart's metrics-reader role, and one that is not")
		for _, sa := range []string{"e2e-metrics-reader", "e2e-metrics-stranger"} {
			_, err := utils.Run(exec.Command("kubectl", "create", "serviceaccount", sa, "-n", managerNamespace))
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() {
				_, _ = utils.Run(exec.Command("kubectl", "delete", "serviceaccount", sa, "-n", managerNamespace,
					"--ignore-not-found"))
			})
		}
		_, err := utils.Run(exec.Command("kubectl", "create", "clusterrolebinding", "e2e-metrics-reader",
			"--clusterrole="+releaseName+"-metrics-reader", "--serviceaccount="+managerNamespace+":e2e-metrics-reader"))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_, _ = utils.Run(exec.Command("kubectl", "delete", "clusterrolebinding", "e2e-metrics-reader",
				"--ignore-not-found"))
		})
		token := func(sa string) string {
			out, err := utils.Run(exec.Command("kubectl", "create", "token", sa, "-n", managerNamespace))
			Expect(err).NotTo(HaveOccurred())
			return strings.TrimSpace(string(out))
		}
		reader, stranger := token("e2e-metrics-reader"), token("e2e-metrics-stranger")

		By("forwarding a local port to the metrics Service")
		port, stop, err := utils.PortForward(managerNamespace, "svc/"+releaseName+"-metrics", 8080)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(stop)

		// The certificate is the suite's, from metrics.certSecret: verify it.
		client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: metricsCA, ServerName: metricsServerName},
		}}
		get := func(scheme, bearer string) (int, string, error) {
			req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s://127.0.0.1:%d/metrics", scheme, port), nil)
			if err != nil {
				return 0, "", err
			}
			if bearer != "" {
				req.Header.Set("Authorization", "Bearer "+bearer)
			}
			resp, err := client.Do(req)
			if err != nil {
				return 0, "", err
			}
			defer func() { _ = resp.Body.Close() }()
			body, err := io.ReadAll(resp.Body)
			return resp.StatusCode, string(body), err
		}
		status := func(scheme, bearer string) func() (int, error) {
			return func() (int, error) {
				code, _, err := get(scheme, bearer)
				return code, err
			}
		}

		By("refusing plain HTTP")
		Eventually(status("http", ""), time.Minute, time.Second).Should(Equal(http.StatusBadRequest))
		By("refusing a request without a token")
		Expect(status("https", "")()).To(Equal(http.StatusUnauthorized))
		By("refusing a token the API server does not accept")
		Expect(status("https", "not-a-token")()).To(Equal(http.StatusUnauthorized))
		By("refusing a token without the metrics-reader role")
		Expect(status("https", stranger)()).To(Equal(http.StatusForbidden))
		By("serving the metrics to a token with the role")
		// The binding reaches the API server's authorizer within seconds.
		Eventually(status("https", reader), time.Minute, time.Second).Should(Equal(http.StatusOK))
		_, body, err := get("https", reader)
		Expect(err).NotTo(HaveOccurred())
		Expect(body).To(ContainSubstring("controller_runtime_reconcile_total"))
	})

	It("lets through only DNS, the API server and port 443 under the NetworkPolicy", func() {
		// The controller cannot show this itself: it connects to the API
		// server within a second of starting, before the CNI applies the
		// policy to a new pod (kindnet takes about a second), and keeps that
		// connection. A probe pod under a copy of the policy checks its rules
		// once the policy is in force.
		const probe = "e2e-netpol-probe"
		By("copying the chart's NetworkPolicy onto a probe pod")
		out, err := utils.Run(exec.Command("kubectl", "get", "networkpolicy", releaseName, "-n", managerNamespace,
			"-o", "json"))
		Expect(err).NotTo(HaveOccurred())
		var chartPolicy struct {
			Spec map[string]any `json:"spec"`
		}
		Expect(json.Unmarshal(out, &chartPolicy)).To(Succeed())
		chartPolicy.Spec["podSelector"] = map[string]any{"matchLabels": map[string]string{"kflare-e2e": probe}}
		copied, err := json.Marshal(map[string]any{
			"apiVersion": "networking.k8s.io/v1",
			"kind":       "NetworkPolicy",
			"metadata":   map[string]string{"name": probe, "namespace": managerNamespace},
			"spec":       chartPolicy.Spec,
		})
		Expect(err).NotTo(HaveOccurred())
		apply := exec.Command("kubectl", "apply", "-f", "-")
		apply.Stdin = strings.NewReader(string(copied))
		_, err = utils.Run(apply)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_, _ = utils.Run(exec.Command("kubectl", "delete", "networkpolicy", probe, "-n", managerNamespace,
				"--ignore-not-found"))
		})

		// agnhost is the image Kubernetes' own NetworkPolicy tests use.
		_, err = utils.Run(exec.Command("kubectl", "run", probe, "-n", managerNamespace,
			"--image=registry.k8s.io/e2e-test-images/agnhost:2.53", "--labels=kflare-e2e="+probe,
			"--restart=Never", "--command", "--", "/agnhost", "pause"))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_, _ = utils.Run(exec.Command("kubectl", "delete", "pod", probe, "-n", managerNamespace,
				"--ignore-not-found", "--wait=false"))
		})
		_, err = utils.Run(exec.Command("kubectl", "wait", "pod/"+probe, "-n", managerNamespace,
			"--for=condition=Ready", "--timeout=2m"))
		Expect(err).NotTo(HaveOccurred())

		clusterIP := func(namespace, service string) string {
			out, err := utils.Run(exec.Command("kubectl", "get", "service", service, "-n", namespace,
				"-o", "jsonpath={.spec.clusterIP}"))
			Expect(err).NotTo(HaveOccurred())
			return strings.TrimSpace(string(out))
		}
		connect := func(target string) func() error {
			return func() error {
				_, err := utils.Run(exec.Command("kubectl", "exec", probe, "-n", managerNamespace, "--",
					"/agnhost", "connect", target, "--timeout=5s"))
				return err
			}
		}

		By("blocking a port the policy does not open: the metrics Service's, from inside")
		Eventually(connect(clusterIP(managerNamespace, releaseName+"-metrics")+":8080"),
			time.Minute, 2*time.Second).ShouldNot(Succeed())
		By("allowing the Kubernetes API server, through the kubernetes Service")
		Eventually(connect(clusterIP("default", "kubernetes")+":443"), 30*time.Second, 2*time.Second).Should(Succeed())
		By("allowing DNS, over TCP to the cluster DNS Service")
		Eventually(connect(clusterIP("kube-system", "kube-dns")+":53"), 30*time.Second, 2*time.Second).Should(Succeed())
		By("allowing api.cloudflare.com on port 443, looked up through the cluster DNS")
		Eventually(connect("api.cloudflare.com:443"), 30*time.Second, 2*time.Second).Should(Succeed())
	})
})
