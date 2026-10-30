/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package e2e

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/JonathanGraniero/kflare/test/utils"
)

const (
	// releaseName and managerNamespace are the Helm release the suite installs
	// the chart as, and its namespace.
	releaseName      = "kflare"
	managerNamespace = "kflare-system"

	// leaderElectionID is the manager's lease (cmd/main.go).
	leaderElectionID = "a634e97e.kflare.dev"

	// managerSelector selects the controller pods the chart runs.
	managerSelector = "app.kubernetes.io/name=kflare,app.kubernetes.io/instance=" + releaseName

	// The image built from this checkout and loaded into kind.
	managerImageRepository = "example.com/kflare"
	managerImageTag        = "e2e"

	// metricsCertSecret holds the metrics server's certificate (metrics.certSecret).
	metricsCertSecret = "kflare-metrics-tls"
	// metricsServerName is the metrics Service's name, which the certificate is for.
	metricsServerName = releaseName + "-metrics." + managerNamespace + ".svc"

	// metricsServerManifest installs metrics-server, which serves the metrics
	// API the chart's HorizontalPodAutoscaler reads. kind does not run it.
	metricsServerManifest = "https://github.com/kubernetes-sigs/metrics-server/releases/download/v0.9.0/" +
		"components.yaml"
)

// metricsCA trusts the CA that signed the metrics server's certificate.
var metricsCA *x509.CertPool

// installedMetricsServer is whether the suite installed metrics-server, and so
// removes it afterwards; one the cluster already ran is left alone.
var installedMetricsServer bool

// externalManager reports whether KFLARE_E2E_MANAGER=external asks the suite
// to use a manager that is already running (for example `make run` against
// the current cluster) instead of building and deploying one. The suite then
// only installs the CRDs, and the deployment smoke specs are skipped.
func externalManager() bool {
	return os.Getenv("KFLARE_E2E_MANAGER") == "external"
}

// TestE2E runs the e2e suite against the kind cluster in the current
// kubeconfig context ($KIND_CLUSTER names it for image loading).
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	logf.SetLogger(GinkgoLogr)
	GinkgoWriter.Println("Starting kflare e2e suite")
	RunSpecs(t, "e2e suite")
}

// The manager is deployed once for the whole suite, with the Helm chart as
// users install it: Ginkgo shuffles top-level containers, and every spec needs
// it running. The chart's NetworkPolicy is on: the live specs reach Cloudflare
// under it, and a smoke spec checks its rules from a probe pod. The metrics
// server's certificate comes from a Secret, signed by a CA the suite makes, so
// that the metrics spec verifies it.
var _ = BeforeSuite(func() {
	if externalManager() {
		By("installing CRDs for the externally running manager")
		_, err := utils.Run(exec.Command("make", "install"))
		Expect(err).NotTo(HaveOccurred())
		return
	}

	managerImage := managerImageRepository + ":" + managerImageTag
	By("building the manager image")
	_, err := utils.Run(exec.Command("make", "docker-build", "IMG="+managerImage))
	Expect(err).NotTo(HaveOccurred())

	By("loading the manager image on kind")
	Expect(utils.LoadImageToKindClusterWithName(managerImage)).To(Succeed())

	By("creating the metrics server's certificate")
	_, _ = utils.Run(exec.Command("kubectl", "create", "namespace", managerNamespace))
	secret, err := newMetricsCertSecret()
	Expect(err).NotTo(HaveOccurred())
	apply := exec.Command("kubectl", "apply", "-f", "-")
	apply.Stdin = strings.NewReader(secret)
	_, err = utils.Run(apply)
	Expect(err).NotTo(HaveOccurred())

	if _, err := utils.Run(exec.Command("kubectl", "get", "apiservice", "v1beta1.metrics.k8s.io")); err != nil {
		By("installing metrics-server, for the chart's HorizontalPodAutoscaler")
		installedMetricsServer = true
		_, err = utils.Run(exec.Command("kubectl", "apply", "-f", metricsServerManifest))
		Expect(err).NotTo(HaveOccurred())
		// kind's kubelets serve self-signed certificates.
		_, err = utils.Run(exec.Command("kubectl", "patch", "deployment", "metrics-server", "-n", "kube-system",
			"--type=json", "-p",
			`[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]`))
		Expect(err).NotTo(HaveOccurred())
	}

	By("installing the chart, which installs the CRDs, and waiting for the controller")
	_, err = utils.Run(exec.Command("helm", "install", releaseName, "helm/kflare",
		"--namespace", managerNamespace,
		"--set", "image.repository="+managerImageRepository+",image.tag="+managerImageTag,
		// Uninstalling removes the CRDs too, so that the cluster is left as it was.
		"--set", "crds.keep=false,networkPolicy.enabled=true,metrics.certSecret="+metricsCertSecret,
		"--wait", "--timeout", "3m"))
	Expect(err).NotTo(HaveOccurred())
})

// ReportAfterEach prints the manager's recent log after a failed spec, before
// AfterSuite undeploys it.
var _ = ReportAfterEach(func(report SpecReport) {
	if !report.Failed() || externalManager() {
		return
	}
	out, _ := utils.Run(exec.Command("kubectl", "logs", "-n", managerNamespace,
		"-l", managerSelector, "-c", "manager", "--tail=100"))
	GinkgoWriter.Printf("controller-manager log:\n%s\n", out)
})

var _ = AfterSuite(func() {
	if externalManager() {
		return
	}
	By("uninstalling the chart")
	_, _ = utils.Run(exec.Command("helm", "uninstall", releaseName, "--namespace", managerNamespace,
		"--ignore-not-found", "--wait"))

	By("removing manager namespace")
	_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", managerNamespace, "--ignore-not-found"))

	if installedMetricsServer {
		By("removing metrics-server")
		_, _ = utils.Run(exec.Command("kubectl", "delete", "-f", metricsServerManifest, "--ignore-not-found"))
	}
})

// newMetricsCertSecret makes a CA, trusted from then on by metricsCA, and a
// certificate it signs for metricsServerName. It returns the Secret that holds
// them for metrics.certSecret, as a manifest.
func newMetricsCertSecret() (string, error) {
	newKey := func() (*ecdsa.PrivateKey, error) { return ecdsa.GenerateKey(elliptic.P256(), rand.Reader) }
	caKey, err := newKey()
	if err != nil {
		return "", err
	}
	now := time.Now()
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "kflare e2e metrics CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return "", err
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return "", err
	}

	key, err := newKey()
	if err != nil {
		return "", err
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: metricsServerName},
		DNSNames:     []string{metricsServerName},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
	if err != nil {
		return "", err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", err
	}

	metricsCA = x509.NewCertPool()
	metricsCA.AddCert(ca)
	encode := func(kind string, der []byte) string {
		return string(pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}))
	}
	manifest, err := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"type":       "kubernetes.io/tls",
		"metadata":   map[string]string{"name": metricsCertSecret, "namespace": managerNamespace},
		"stringData": map[string]string{
			"tls.crt": encode("CERTIFICATE", leafDER),
			"tls.key": encode("PRIVATE KEY", keyDER),
			"ca.crt":  encode("CERTIFICATE", caDER),
		},
	})
	return string(manifest), err
}
