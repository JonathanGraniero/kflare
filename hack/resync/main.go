/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

// Command resync measures how long the controller takes to reconcile every
// resource once, as it does after a restart or a change of leader, with one
// reconcile at a time per controller and with several
// (--max-concurrent-reconciles). It runs the manager binary against an envtest
// API server with kflare's CRDs, and against a stand-in for Cloudflare's API
// that answers every call after a fixed latency:
//
//  1. setup: -accounts CloudflareAccounts, each with an API token of its own,
//     and -resources KVNamespaces spread evenly over them
//  2. create: the manager, with the most -workers, creates every namespace in
//     the stand-in, and the resources turn Ready
//  3. resync, for each value of -workers: the manager starts again, and the
//     time until it has read every namespace back is the resync's
//
// The manager reaches the stand-in as it would reach Cloudflare through a
// proxy that inspects TLS: HTTPS_PROXY names a proxy on this machine that
// answers for api.cloudflare.com with a certificate from a CA of its own, which
// the manager trusts through SSL_CERT_DIR, as the chart's trustedCA sets it. So
// the manager's own HTTP client, rate limiter and metrics run as they do in a
// cluster: each token's budget (--cloudflare-rate-limit) still paces its calls.
//
// Run it through make, from the repository's root, which builds the manager
// and sets KUBEBUILDER_ASSETS to the envtest binaries:
//
//	make resync RESYNC_ARGS="-accounts 20 -resources 1000 -workers 1,4,16"
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
)

// cloudflareHost is the host the manager calls, which the proxy answers for.
const cloudflareHost = "api.cloudflare.com"

// accountsPath starts the path of every call the stand-in answers.
const accountsPath = "accounts"

// namespace holds the measurement's Secrets and KVNamespaces.
const namespace = "resync"

type options struct {
	manager   string
	accounts  int
	resources int
	latency   time.Duration
	workers   []int
	timeout   time.Duration
	logDir    string
}

func main() {
	var o options
	var workers string
	flag.StringVar(&o.manager, "manager", filepath.Join("bin", "manager"), "The manager binary (make build).")
	flag.IntVar(&o.accounts, "accounts", 20, "CloudflareAccounts, each with an API token of its own.")
	flag.IntVar(&o.resources, "resources", 1000, "KVNamespaces, spread evenly over the accounts.")
	flag.DurationVar(&o.latency, "latency", 200*time.Millisecond, "How long the stand-in for Cloudflare takes to "+
		"answer each call.")
	flag.StringVar(&workers, "workers", "1,2,4,8,16", "The --max-concurrent-reconciles values to time a resync "+
		"with, separated by commas.")
	flag.DurationVar(&o.timeout, "timeout", 30*time.Minute, "How long one phase may take.")
	flag.StringVar(&o.logDir, "log-dir", "", "A directory to keep the manager's logs in, one file per phase.")
	flag.Parse()

	var err error
	if o.workers, err = parseWorkers(workers); err == nil {
		err = o.validate()
	}
	if err == nil {
		err = run(context.Background(), o, os.Stdout)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "resync:", err)
		os.Exit(1)
	}
}

// parseWorkers reads -workers.
func parseWorkers(s string) ([]int, error) {
	var out []int
	for field := range strings.SplitSeq(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(field))
		if err != nil || n < 1 {
			return nil, fmt.Errorf("-workers: %q is not a number of 1 or more", field)
		}
		out = append(out, n)
	}
	return out, nil
}

func (o options) validate() error {
	switch {
	case o.accounts < 1:
		return errors.New("-accounts must be 1 or more")
	case o.resources < o.accounts:
		return errors.New("-resources must be at least -accounts")
	case o.latency < 0:
		return errors.New("-latency must not be negative")
	case len(o.workers) == 0:
		return errors.New("-workers names no value")
	case os.Getenv("KUBEBUILDER_ASSETS") == "":
		return errors.New("KUBEBUILDER_ASSETS is not set: run it through make resync")
	}
	if _, err := os.Stat(o.manager); err != nil {
		return fmt.Errorf("-manager: %w (make build)", err)
	}
	return nil
}

// result is what one phase measured.
type result struct {
	phase    string
	workers  int
	elapsed  time.Duration
	calls    int
	cpu      time.Duration
	maxRSSMi float64
}

func run(ctx context.Context, o options, out io.Writer) error {
	dir, err := os.MkdirTemp("", "kflare-resync-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	cf := newFakeCloudflare(o.latency)
	ca, err := newCA()
	if err != nil {
		return err
	}
	certDir := filepath.Join(dir, "certs")
	if err := os.Mkdir(certDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(certDir, "ca.pem"), ca.certPEM, 0o644); err != nil {
		return err
	}
	proxyAddr, stopProxy, err := serveProxy(cf, ca)
	if err != nil {
		return err
	}
	defer stopProxy()

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		return fmt.Errorf("starting envtest: %w", err)
	}
	defer func() { _ = env.Stop() }()
	user, err := env.AddUser(envtest.User{Name: "kflare-resync", Groups: []string{"system:masters"}}, nil)
	if err != nil {
		return err
	}
	kubeconfig, err := user.KubeConfig()
	if err != nil {
		return err
	}
	kubeconfigPath := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(kubeconfigPath, kubeconfig, 0o600); err != nil {
		return err
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := cloudflarev1alpha1.AddToScheme(scheme); err != nil {
		return err
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	if err := setup(ctx, c, o); err != nil {
		return err
	}

	m := managerRun{binary: o.manager, kubeconfig: kubeconfigPath, proxy: proxyAddr, certDir: certDir,
		logDir: o.logDir}
	var results []result

	most := slices.Max(o.workers)
	if _, err := fmt.Fprintf(out, "Creating %d KVNamespaces over %d accounts, %s a call, %d at once\n",
		o.resources, o.accounts, o.latency, most); err != nil {
		return err
	}
	r, err := m.measure(ctx, "create", most, o.timeout, func() (bool, error) {
		return allReady(ctx, c, o.resources)
	}, cf)
	if err != nil {
		return err
	}
	results = append(results, r)
	if held := cf.namespaces(); held != o.resources {
		return fmt.Errorf("the stand-in holds %d namespaces, want %d", held, o.resources)
	}

	for _, workers := range o.workers {
		if _, err := fmt.Fprintf(out, "Resyncing with %d at once\n", workers); err != nil {
			return err
		}
		cf.startReading()
		r, err := m.measure(ctx, "resync", workers, o.timeout, func() (bool, error) {
			return cf.readAll(), nil
		}, cf)
		if err != nil {
			return err
		}
		results = append(results, r)
	}
	if unknown := cf.unknownCalls(); len(unknown) > 0 {
		return fmt.Errorf("the stand-in got calls it does not answer: %v", unknown)
	}
	return writeTable(out, o, results)
}

// setup creates the accounts, their Secrets and the KVNamespaces.
func setup(ctx context.Context, c client.Client, o options) error {
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
		return err
	}
	for i := range o.accounts {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: accountName(i), Namespace: namespace},
			StringData: map[string]string{"CF_API_TOKEN": fmt.Sprintf("resync-token-%d", i)},
		}
		if err := c.Create(ctx, secret); err != nil {
			return err
		}
		account := &cloudflarev1alpha1.CloudflareAccount{
			ObjectMeta: metav1.ObjectMeta{Name: accountName(i)},
			Spec: cloudflarev1alpha1.CloudflareAccountSpec{
				AccountID: accountID(i),
				TokenSecretRef: cloudflarev1alpha1.SecretReference{
					Name: accountName(i), Namespace: namespace, Key: "CF_API_TOKEN",
				},
			},
		}
		if err := c.Create(ctx, account); err != nil {
			return err
		}
	}
	for i := range o.resources {
		kv := &cloudflarev1alpha1.KVNamespace{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("kv-%d", i), Namespace: namespace},
		}
		kv.Spec.AccountRef.Name = accountName(i % o.accounts)
		kv.Spec.Title = fmt.Sprintf("resync-%d", i)
		if err := c.Create(ctx, kv); err != nil {
			return err
		}
	}
	return nil
}

func accountName(i int) string { return fmt.Sprintf("resync-%d", i) }
func accountID(i int) string   { return fmt.Sprintf("%032x", i+1) }

// allReady reports whether want KVNamespaces are Ready.
func allReady(ctx context.Context, c client.Client, want int) (bool, error) {
	list := &cloudflarev1alpha1.KVNamespaceList{}
	if err := c.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return false, err
	}
	ready := 0
	for i := range list.Items {
		if meta.IsStatusConditionTrue(list.Items[i].Status.Conditions, cloudflarev1alpha1.ConditionReady) {
			ready++
		}
	}
	return ready == want, nil
}

// managerRun runs the manager binary for a phase.
type managerRun struct {
	binary, kubeconfig, proxy, certDir, logDir string
}

// measure runs the manager with workers reconciles at once until done reports
// true, and returns how long that took from the manager's start, the calls
// the stand-in answered meanwhile, and the CPU and memory the manager used.
func (m managerRun) measure(ctx context.Context, phase string, workers int, timeout time.Duration,
	done func() (bool, error), cf *fakeCloudflare) (result, error) {
	logs := io.Discard
	if m.logDir != "" {
		f, err := os.Create(filepath.Join(m.logDir, fmt.Sprintf("%s-%d.log", phase, workers)))
		if err != nil {
			return result{}, err
		}
		defer func() { _ = f.Close() }()
		logs = f
	}
	cmd := exec.CommandContext(ctx, m.binary,
		"--leader-elect=false", "--metrics-bind-address=0", "--health-probe-bind-address=0",
		"--max-concurrent-reconciles="+strconv.Itoa(workers), "--zap-devel=false", "--zap-log-level=info")
	cmd.Env = append(os.Environ(),
		"KUBECONFIG="+m.kubeconfig,
		"HTTPS_PROXY=http://"+m.proxy, "NO_PROXY=127.0.0.1,localhost",
		"SSL_CERT_DIR="+m.certDir)
	cmd.Stdout, cmd.Stderr = logs, logs

	callsBefore := cf.callCount()
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return result{}, err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	var elapsed time.Duration
	deadline := time.After(timeout)
	for elapsed == 0 {
		select {
		case err := <-exited:
			return result{}, fmt.Errorf("%s with %d at once: the manager exited: %v", phase, workers, err)
		case <-deadline:
			_ = cmd.Process.Kill()
			<-exited
			return result{}, fmt.Errorf("%s with %d at once: not done after %s", phase, workers, timeout)
		case <-time.After(100 * time.Millisecond):
		}
		ok, err := done()
		if err != nil {
			_ = cmd.Process.Kill()
			<-exited
			return result{}, err
		}
		if ok {
			elapsed = time.Since(start)
		}
	}
	calls := cf.callCount() - callsBefore
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		return result{}, err
	}
	select {
	case <-exited:
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		<-exited
	}
	r := result{phase: phase, workers: workers, elapsed: elapsed, calls: calls,
		cpu: cmd.ProcessState.UserTime() + cmd.ProcessState.SystemTime()}
	if usage, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok {
		r.maxRSSMi = float64(usage.Maxrss) / 1024 // kilobytes on Linux
	}
	return r, nil
}

func writeTable(out io.Writer, o options, results []result) error {
	if _, err := fmt.Fprintf(out, "\n%d KVNamespaces over %d accounts, %d each, one API token each; %s a call\n",
		o.resources, o.accounts, o.resources/o.accounts, o.latency); err != nil {
		return err
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "phase\tat once\ttime\tCloudflare calls\tcalls/s\tCPU (s)\tmax RSS (MiB)"); err != nil {
		return err
	}
	for _, r := range results {
		if _, err := fmt.Fprintf(w, "%s\t%d\t%s\t%d\t%.1f\t%.1f\t%.0f\n", r.phase, r.workers,
			r.elapsed.Round(100*time.Millisecond), r.calls, float64(r.calls)/r.elapsed.Seconds(), r.cpu.Seconds(),
			r.maxRSSMi); err != nil {
			return err
		}
	}
	return w.Flush()
}

// fakeCloudflare stands in for the parts of Cloudflare's API that
// CloudflareAccounts and KVNamespaces call: verifying a token, reading an
// account, and creating, reading, listing, updating and deleting KV
// namespaces, in Cloudflare's envelope. Each call is answered after latency.
type fakeCloudflare struct {
	latency time.Duration

	mu sync.Mutex
	// kv holds each account's namespaces by ID; next numbers new ones.
	kv   map[string]map[string]string
	next int
	// calls counts every call; read holds the namespaces read by ID since
	// startReading; unknown the calls the stand-in does not answer.
	calls   int
	read    map[string]bool
	unknown []string
}

func newFakeCloudflare(latency time.Duration) *fakeCloudflare {
	return &fakeCloudflare{latency: latency, kv: map[string]map[string]string{}}
}

// envelope is Cloudflare's v4 answer.
type envelope struct {
	Success    bool       `json:"success"`
	Errors     []apiError `json:"errors"`
	Messages   []apiError `json:"messages"`
	Result     any        `json:"result"`
	ResultInfo *pageInfo  `json:"result_info,omitempty"`
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type pageInfo struct {
	Page       int `json:"page"`
	PerPage    int `json:"per_page"`
	Count      int `json:"count"`
	TotalCount int `json:"total_count"`
}

type kvNamespace struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

func (f *fakeCloudflare) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	select {
	case <-time.After(f.latency):
	case <-r.Context().Done():
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++

	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/client/v4"), "/"), "/")
	switch {
	case len(parts) == 2 && parts[0] == accountsPath && r.Method == http.MethodGet:
		answer(w, map[string]any{"id": parts[1], "name": "resync " + parts[1], "type": "standard"}, nil)
	case len(parts) == 4 && parts[0] == accountsPath && parts[2] == "tokens" && parts[3] == "verify" &&
		r.Method == http.MethodGet:
		answer(w, map[string]any{"id": "token-" + parts[1], "status": "active"}, nil)
	case len(parts) >= 5 && parts[0] == accountsPath && parts[2] == "storage" && parts[3] == "kv" &&
		parts[4] == "namespaces":
		f.serveKV(w, r, parts[1], parts[5:])
	default:
		f.unknown = append(f.unknown, r.Method+" "+r.URL.Path)
		fail(w, http.StatusNotFound, 7003, "No route for that URI")
	}
}

// serveKV answers the KV namespace calls of account, for the path after
// /namespaces.
func (f *fakeCloudflare) serveKV(w http.ResponseWriter, r *http.Request, account string, rest []string) {
	namespaces := f.kv[account]
	if namespaces == nil {
		namespaces = map[string]string{}
		f.kv[account] = namespaces
	}
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		page, perPage := atoiOr(r.URL.Query().Get("page"), 1), atoiOr(r.URL.Query().Get("per_page"), 20)
		ids := slices.Sorted(func(yield func(string) bool) {
			for id := range namespaces {
				if !yield(id) {
					return
				}
			}
		})
		start := min(max(page-1, 0)*perPage, len(ids))
		end := min(start+perPage, len(ids))
		items := make([]kvNamespace, 0, end-start)
		for _, id := range ids[start:end] {
			items = append(items, kvNamespace{ID: id, Title: namespaces[id]})
		}
		answer(w, items,
			&pageInfo{Page: page, PerPage: perPage, Count: len(items), TotalCount: len(ids)})
	case len(rest) == 0 && r.Method == http.MethodPost:
		var body kvNamespace
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Title == "" {
			fail(w, http.StatusBadRequest, 10019, "a title is required")
			return
		}
		for _, title := range namespaces {
			if title == body.Title {
				fail(w, http.StatusBadRequest, 10014, "a namespace with this account ID and title already exists")
				return
			}
		}
		f.next++
		id := fmt.Sprintf("%032x", f.next)
		namespaces[id] = body.Title
		answer(w, kvNamespace{ID: id, Title: body.Title}, nil)
	case len(rest) == 1:
		title, ok := namespaces[rest[0]]
		if !ok {
			fail(w, http.StatusNotFound, 10013, "namespace not found")
			return
		}
		switch r.Method {
		case http.MethodGet:
			if f.read != nil {
				f.read[rest[0]] = true
			}
			answer(w, kvNamespace{ID: rest[0], Title: title}, nil)
		case http.MethodPut:
			var body kvNamespace
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				fail(w, http.StatusBadRequest, 10019, "unreadable body")
				return
			}
			namespaces[rest[0]] = body.Title
			answer(w, kvNamespace{ID: rest[0], Title: body.Title}, nil)
		case http.MethodDelete:
			delete(namespaces, rest[0])
			answer(w, nil, nil)
		default:
			f.unknown = append(f.unknown, r.Method+" "+r.URL.Path)
			fail(w, http.StatusMethodNotAllowed, 10000, "method not allowed")
		}
	default:
		f.unknown = append(f.unknown, r.Method+" "+r.URL.Path)
		fail(w, http.StatusNotFound, 7003, "No route for that URI")
	}
}

func atoiOr(s string, fallback int) int {
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return n
	}
	return fallback
}

// answer writes a successful call's envelope.
func answer(w http.ResponseWriter, result any, info *pageInfo) {
	write(w, http.StatusOK, envelope{Success: true, Errors: []apiError{}, Messages: []apiError{}, Result: result,
		ResultInfo: info})
}

// fail writes a refused call's envelope.
func fail(w http.ResponseWriter, status, code int, message string) {
	write(w, status, envelope{Errors: []apiError{{Code: code, Message: message}}, Messages: []apiError{}})
}

func write(w http.ResponseWriter, status int, body envelope) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// startReading starts recording the namespaces read by ID.
func (f *fakeCloudflare) startReading() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.read = map[string]bool{}
}

// readAll reports whether every namespace has been read since startReading.
func (f *fakeCloudflare) readAll() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	held := 0
	for _, namespaces := range f.kv {
		held += len(namespaces)
	}
	return len(f.read) == held
}

func (f *fakeCloudflare) namespaces() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	held := 0
	for _, namespaces := range f.kv {
		held += len(namespaces)
	}
	return held
}

func (f *fakeCloudflare) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeCloudflare) unknownCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.unknown)
}

// ca is a certificate authority of the measurement's own, and a certificate it
// issued for cloudflareHost.
type ca struct {
	certPEM []byte
	leaf    tls.Certificate
}

func newCA() (ca, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return ca{}, err
	}
	now := time.Now()
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "kflare resync CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return ca{}, err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return ca{}, err
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return ca{}, err
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: cloudflareHost},
		DNSNames:     []string{cloudflareHost},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return ca{}, err
	}
	return ca{
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		leaf:    tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey},
	}, nil
}

// serveProxy serves, on a port of this machine, a proxy that answers CONNECT
// for cloudflareHost:443 with handler, over TLS with the CA's certificate, and
// refuses any other. It returns the proxy's address and a function that stops
// it.
func serveProxy(handler http.Handler, authority ca) (string, func(), error) {
	tunnels := newConnListener()
	api := &http.Server{Handler: handler, TLSConfig: &tls.Config{
		Certificates: []tls.Certificate{authority.leaf}, MinVersion: tls.VersionTLS12,
	}, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = api.ServeTLS(tunnels, "", "") }()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	proxy := &http.Server{ReadHeaderTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodConnect || r.Host != cloudflareHost+":443" {
				http.Error(w, "this proxy only reaches "+cloudflareHost, http.StatusForbidden)
				return
			}
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "cannot hijack the connection", http.StatusInternalServerError)
				return
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				return
			}
			if _, err := conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
				_ = conn.Close()
				return
			}
			tunnels.push(conn)
		})}
	go func() { _ = proxy.Serve(listener) }()
	return listener.Addr().String(), func() {
		_ = proxy.Close()
		_ = api.Close()
	}, nil
}

// connListener is a net.Listener whose connections are the tunnels the proxy
// opens.
type connListener struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newConnListener() *connListener {
	return &connListener{conns: make(chan net.Conn), closed: make(chan struct{})}
}

func (l *connListener) push(conn net.Conn) {
	select {
	case l.conns <- conn:
	case <-l.closed:
		_ = conn.Close()
	}
}

func (l *connListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *connListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *connListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }
