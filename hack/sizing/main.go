/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

// Command sizing measures the controller's CPU and memory, the numbers behind
// the Helm chart's resources and HorizontalPodAutoscaler targets. It starts an
// envtest API server with kflare's CRDs, runs the controller image in Docker
// containers against it with leader election (one leader, the rest standbys),
// and samples every container with docker stats while it loads the API server
// in phases:
//
//  1. idle: kflare's CRDs and nothing else
//  2. secrets: -secrets Secrets of -secret-bytes each, all of which the leader
//     caches (the CloudflareAccount controller watches every Secret)
//  3. resources: -resources kflare resources copied from config/samples, whose
//     CloudflareAccount does not exist, so that every reconcile ends with
//     AccountNotFound and is requeued a minute later without calling Cloudflare
//  4. failover: the leader is stopped, and a standby takes the lease and then
//     lists and reconciles everything at once
//
// The loading phases are sampled while the objects are created, and each
// phase again once it has settled. docker stats reports what the kubelet, and
// so an HPA, sees: CPU in cores, and memory as the working set (usage less
// inactive file pages).
//
// Run it through make, from the repository's root, which also sets
// KUBEBUILDER_ASSETS to the envtest binaries:
//
//	make docker-build sizing IMG=controller:latest SIZING_ARGS="-resources 5000"
//
// It needs Docker, whose containers reach the API server on the host network.
// Calls to Cloudflare are left out on purpose: Cloudflare's rate limit of
// 1,200 calls per five minutes bounds them to 4 a second, while the API
// server's watches and the reconciles they cause have no such bound.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

// namespaces is how many namespaces the Secrets and resources are spread over.
const namespaces = 10

// leaseNamespace holds the leader election Lease. The controller reads its
// namespace from the file a ServiceAccount token mount would provide.
const leaseNamespace = "kflare-system"

type options struct {
	image       string
	assets      string
	replicas    int
	secrets     int
	secretBytes int
	resources   int
	settle      time.Duration
	window      time.Duration
	cpus        string
	memory      string
	samples     string
}

func main() {
	var o options
	flag.StringVar(&o.image, "image", "", "The controller image to measure, such as one make docker-build built.")
	flag.StringVar(&o.assets, "assets", os.Getenv("KUBEBUILDER_ASSETS"),
		"The directory holding the envtest binaries (etcd, kube-apiserver).")
	flag.IntVar(&o.replicas, "replicas", 2, "Controller containers to run: one leader, the rest standbys.")
	flag.IntVar(&o.secrets, "secrets", 1000, "Secrets to create in the secrets phase.")
	flag.IntVar(&o.secretBytes, "secret-bytes", 4096, "Bytes of data in each Secret.")
	flag.IntVar(&o.resources, "resources", 1000, "kflare resources to create in the resources phase.")
	flag.DurationVar(&o.settle, "settle", 30*time.Second, "How long a phase settles before it is sampled again.")
	flag.DurationVar(&o.window, "window", 2*time.Minute,
		"How long a settled phase is sampled; longer than a minute, so that the requeues fall in it.")
	flag.StringVar(&o.cpus, "cpus", "", "A CPU limit for each container (docker run --cpus), such as 0.5.")
	flag.StringVar(&o.memory, "memory", "", "A memory limit for each container (docker run --memory), such as 256m.")
	flag.StringVar(&o.samples, "samples", "", "A CSV file to write every sample to, to see a phase over time.")
	flag.Parse()

	if err := o.validate(); err != nil {
		fmt.Fprintln(os.Stderr, "sizing:", err)
		flag.Usage()
		os.Exit(2)
	}
	if err := run(context.Background(), o, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "sizing:", err)
		os.Exit(1)
	}
}

func (o options) validate() error {
	switch {
	case o.image == "":
		return errors.New("-image is required")
	case o.assets == "":
		return errors.New("-assets or KUBEBUILDER_ASSETS is required")
	case o.replicas < 1:
		return errors.New("-replicas must be at least 1")
	case o.secrets < 0 || o.secretBytes < 0 || o.resources < 0:
		return errors.New("-secrets, -secret-bytes and -resources cannot be negative")
	case o.window <= 0:
		return errors.New("-window must be positive")
	}
	return nil
}

func run(ctx context.Context, o options, out io.Writer) error {
	templates, err := sampleTemplates(filepath.Join("config", "samples"))
	if err != nil {
		return err
	}

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: o.assets,
	}
	cfg, err := env.Start()
	if err != nil {
		return fmt.Errorf("starting envtest: %w", err)
	}
	defer func() {
		if err := env.Stop(); err != nil {
			fmt.Fprintln(os.Stderr, "sizing: stopping envtest:", err)
		}
	}()
	// The loader creates thousands of objects: client-go's default of 5
	// requests a second would take minutes.
	cfg.QPS, cfg.Burst = 500, 1000
	c, err := client.New(cfg, client.Options{})
	if err != nil {
		return err
	}

	dir, err := os.MkdirTemp("", "kflare-sizing-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	mounts, err := writeMounts(env, dir)
	if err != nil {
		return err
	}
	if err := createNamespaces(ctx, c); err != nil {
		return err
	}

	m := &meter{}
	defer m.removeAll()
	for i := range o.replicas {
		if err := m.start(ctx, o, i, mounts); err != nil {
			return err
		}
	}
	leader, err := m.waitForLeader(ctx, 2*time.Minute)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "%s holds the lease\n", leader); err != nil {
		return err
	}

	var results []result
	measure := func(phase string, load func() error) error {
		r, err := m.sample(ctx, phase, load, o.window)
		results = append(results, r...)
		return err
	}
	settled := func(phase string) error {
		time.Sleep(o.settle)
		return measure(phase, nil)
	}

	if err := settled("idle"); err != nil {
		return err
	}
	if err := measure("secrets: loading", func() error {
		return createSecrets(ctx, c, o.secrets, o.secretBytes)
	}); err != nil {
		return err
	}
	if err := settled("secrets: settled"); err != nil {
		return err
	}
	if err := measure("resources: loading", func() error {
		return createResources(ctx, c, templates, o.resources)
	}); err != nil {
		return err
	}
	if err := settled("resources: settled"); err != nil {
		return err
	}
	if o.replicas > 1 {
		if err := measure("failover", func() error { return m.stop(ctx, leader) }); err != nil {
			return err
		}
	}

	if _, err := fmt.Fprintf(out, "\n%d Secrets of %d bytes, %d resources; limits: cpus %q, memory %q\n\n",
		o.secrets, o.secretBytes, o.resources, o.cpus, o.memory); err != nil {
		return err
	}
	if err := writeTable(out, results); err != nil {
		return err
	}
	if o.samples == "" {
		return nil
	}
	f, err := os.Create(o.samples)
	if err != nil {
		return err
	}
	if err := writeSamples(f, results); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// mounts are the host files the controller containers mount.
type mounts struct {
	kubeconfig string
	namespace  string
}

// writeMounts writes a kubeconfig for an envtest user in system:masters, and
// the namespace file that tells controller-runtime where its Lease goes. Both
// are world-readable: the image runs as a non-root user.
func writeMounts(env *envtest.Environment, dir string) (mounts, error) {
	user, err := env.AddUser(envtest.User{Name: "kflare-sizing", Groups: []string{"system:masters"}}, nil)
	if err != nil {
		return mounts{}, err
	}
	kubeconfig, err := user.KubeConfig()
	if err != nil {
		return mounts{}, err
	}
	m := mounts{kubeconfig: filepath.Join(dir, "kubeconfig"), namespace: filepath.Join(dir, "namespace")}
	if err := os.WriteFile(m.kubeconfig, kubeconfig, 0o644); err != nil {
		return mounts{}, err
	}
	if err := os.WriteFile(m.namespace, []byte(leaseNamespace), 0o644); err != nil {
		return mounts{}, err
	}
	return m, nil
}

func createNamespaces(ctx context.Context, c client.Client) error {
	names := make([]string, 0, namespaces+1)
	names = append(names, leaseNamespace)
	for i := range namespaces {
		names = append(names, loadNamespace(i))
	}
	for _, name := range names {
		if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
			return err
		}
	}
	return nil
}

func loadNamespace(i int) string { return fmt.Sprintf("sizing-%d", i%namespaces) }

func createSecrets(ctx context.Context, c client.Client, count, size int) error {
	return parallel(count, func(i int) error {
		data := make([]byte, size)
		if _, err := rand.Read(data); err != nil {
			return err
		}
		return c.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("secret-%d", i), Namespace: loadNamespace(i)},
			Data:       map[string][]byte{"data": data},
		})
	})
}

func createResources(ctx context.Context, c client.Client, templates []*unstructured.Unstructured, count int) error {
	return parallel(count, func(i int) error {
		obj := templates[i%len(templates)].DeepCopy()
		obj.SetName(fmt.Sprintf("%s-%d", obj.GetName(), i))
		namespaced, err := c.IsObjectNamespaced(obj)
		if err != nil {
			return err
		}
		if namespaced {
			obj.SetNamespace(loadNamespace(i))
		}
		return c.Create(ctx, obj)
	})
}

// parallel runs create for 0 to count-1 on a few workers, and returns the
// first error.
func parallel(count int, create func(i int) error) error {
	const workers = 16
	next := make(chan int)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for i := range next {
				if err := create(i); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	var err error
feed:
	for i := range count {
		select {
		case next <- i:
		case err = <-errs:
			break feed
		}
	}
	close(next)
	go func() { wg.Wait(); close(errs) }()
	for e := range errs {
		err = errors.Join(err, e)
	}
	return err
}

// sampleTemplates reads every namespaced and cluster-scoped kind's samples
// but CloudflareAccount's: an account would call Cloudflare to check its token.
func sampleTemplates(dir string) ([]*unstructured.Unstructured, error) {
	files, err := filepath.Glob(filepath.Join(dir, "cloudflare_v1alpha1_*.yaml"))
	if err != nil {
		return nil, err
	}
	var templates []*unstructured.Unstructured
	for _, file := range files {
		content, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		for _, doc := range splitYAML(content) {
			obj := &unstructured.Unstructured{}
			if err := yaml.Unmarshal(doc, &obj.Object); err != nil {
				return nil, fmt.Errorf("%s: %w", file, err)
			}
			if obj.GetKind() == "" || obj.GetKind() == "CloudflareAccount" {
				continue
			}
			templates = append(templates, obj)
		}
	}
	if len(templates) == 0 {
		return nil, fmt.Errorf("no samples in %s", dir)
	}
	return templates, nil
}

var documentSeparator = regexp.MustCompile(`(?m)^---\s*$`)

// splitYAML splits a multi-document YAML file, dropping documents with no
// content but comments.
func splitYAML(content []byte) [][]byte {
	var docs [][]byte
	for _, doc := range documentSeparator.Split(string(content), -1) {
		for line := range strings.SplitSeq(doc, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
				docs = append(docs, []byte(doc))
				break
			}
		}
	}
	return docs
}

// meter runs the controller containers and samples them.
type meter struct {
	mu         sync.Mutex
	containers []string
	running    []string
}

func (m *meter) start(ctx context.Context, o options, i int, mnt mounts) error {
	name := fmt.Sprintf("kflare-sizing-%d", i)
	args := []string{"run", "--detach", "--name", name, "--network", "host",
		"--volume", mnt.kubeconfig + ":/etc/kflare/kubeconfig:ro",
		"--volume", mnt.namespace + ":/var/run/secrets/kubernetes.io/serviceaccount/namespace:ro",
		"--env", "KUBECONFIG=/etc/kflare/kubeconfig"}
	if o.cpus != "" {
		args = append(args, "--cpus", o.cpus)
	}
	if o.memory != "" {
		args = append(args, "--memory", o.memory)
	}
	// The chart's arguments, with ports apart: the containers share the
	// host's network.
	args = append(args, o.image, "--leader-elect", "--metrics-secure",
		fmt.Sprintf("--metrics-bind-address=:%d", 18443+i),
		fmt.Sprintf("--health-probe-bind-address=:%d", 18081+i),
		"--zap-encoder=json", "--zap-log-level=info")
	if _, err := docker(ctx, args...); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.containers = append(m.containers, name)
	m.running = append(m.running, name)
	return nil
}

func (m *meter) stop(ctx context.Context, name string) error {
	if _, err := docker(ctx, "stop", name); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.running = slices.DeleteFunc(m.running, func(n string) bool { return n == name })
	return nil
}

func (m *meter) removeAll() {
	for _, name := range m.containers {
		if _, err := docker(context.Background(), "rm", "--force", name); err != nil {
			fmt.Fprintln(os.Stderr, "sizing:", err)
		}
	}
}

func (m *meter) runningContainers() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.running)
}

// leader is the running container whose log says it acquired the lease
// last, or "" if none has.
func (m *meter) leader(ctx context.Context) (string, error) {
	var leader string
	var latest time.Time
	for _, name := range m.runningContainers() {
		logs, err := docker(ctx, "logs", "--timestamps", name)
		if err != nil {
			return "", err
		}
		if at, ok := acquiredAt(logs); ok && at.After(latest) {
			leader, latest = name, at
		}
	}
	return leader, nil
}

// acquiredAt returns when a docker logs --timestamps output last says that
// the lease was acquired.
func acquiredAt(logs []byte) (time.Time, bool) {
	var at time.Time
	found := false
	scanner := bufio.NewScanner(bytes.NewReader(logs))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.Contains(strings.ToLower(line), "successfully acquired lease") {
			continue
		}
		stamp, _, _ := strings.Cut(line, " ")
		t, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			continue
		}
		at, found = t, true
	}
	return at, found
}

func (m *meter) waitForLeader(ctx context.Context, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		leader, err := m.leader(ctx)
		if err != nil {
			return "", err
		}
		if leader != "" {
			return leader, nil
		}
		time.Sleep(time.Second)
	}
	return "", fmt.Errorf("no container acquired the lease within %s", timeout)
}

// sample samples the running containers while load runs and for window after
// it returns, so that the controller's catching up is in the phase, or for
// window when load is nil. It returns a result per container, with the role
// the container ended the phase in.
func (m *meter) sample(ctx context.Context, phase string, load func() error, window time.Duration) ([]result, error) {
	samples := map[string][]stat{}
	done := make(chan error, 1)
	if load == nil {
		go func() { time.Sleep(window); done <- nil }()
	} else {
		go func() {
			err := load()
			if err == nil {
				// Keep sampling while the controller catches up.
				time.Sleep(window)
			}
			done <- err
		}()
	}

	var loadErr error
	start := time.Now()
loop:
	for {
		select {
		case loadErr = <-done:
			break loop
		default:
		}
		out, err := docker(ctx, append([]string{"stats", "--no-stream", "--format", "{{json .}}"},
			m.runningContainers()...)...)
		if err != nil {
			return nil, err
		}
		stats, err := parseStats(out)
		if err != nil {
			return nil, err
		}
		at := time.Since(start)
		for _, s := range stats {
			s.at = at
			samples[s.name] = append(samples[s.name], s)
		}
	}
	elapsed := time.Since(start)

	leader, err := m.leader(ctx)
	if err != nil {
		return nil, err
	}
	var results []result
	for _, name := range m.runningContainers() {
		role := "standby"
		if name == leader {
			role = "leader"
		}
		results = append(results, summarize(phase, name, role, elapsed, samples[name]))
	}
	return results, loadErr
}

func docker(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	if args[0] == "logs" {
		// docker logs writes the container's stderr, where the controller logs, to its own.
		out = append(out, stderr.Bytes()...)
	}
	return out, nil
}

// stat is one docker stats sample of a container, taken at in its phase.
type stat struct {
	name      string
	at        time.Duration
	millicpu  float64
	workingMi float64
}

// parseStats reads docker stats --format '{{json .}}' output: a JSON object a
// line, whose CPUPerc is a percentage of one core ("1.25%") and whose
// MemUsage is the working set and the limit ("21.3MiB / 15.7GiB").
func parseStats(out []byte) ([]stat, error) {
	var stats []stat
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		var raw struct {
			Name     string `json:"Name"`
			CPUPerc  string `json:"CPUPerc"`
			MemUsage string `json:"MemUsage"`
		}
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			return nil, fmt.Errorf("docker stats: %w: %q", err, line)
		}
		percent, err := strconv.ParseFloat(strings.TrimSuffix(raw.CPUPerc, "%"), 64)
		if err != nil {
			return nil, fmt.Errorf("docker stats CPUPerc %q: %w", raw.CPUPerc, err)
		}
		usage, _, _ := strings.Cut(raw.MemUsage, " / ")
		used, err := parseBytes(usage)
		if err != nil {
			return nil, fmt.Errorf("docker stats MemUsage %q: %w", raw.MemUsage, err)
		}
		stats = append(stats, stat{name: raw.Name, millicpu: percent * 10, workingMi: used / (1 << 20)})
	}
	return stats, nil
}

// parseBytes reads a size as docker prints it, in binary units ("21.3MiB").
func parseBytes(s string) (float64, error) {
	units := []struct {
		suffix string
		factor float64
	}{{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"TiB", 1 << 40}, {"B", 1}}
	s = strings.TrimSpace(s)
	for _, u := range units {
		if number, ok := strings.CutSuffix(s, u.suffix); ok {
			v, err := strconv.ParseFloat(number, 64)
			if err != nil {
				return 0, err
			}
			return v * u.factor, nil
		}
	}
	return 0, fmt.Errorf("no unit in %q", s)
}

// result is what a container did during a phase.
type result struct {
	phase, container, role string
	duration               time.Duration
	samples                int
	cpuMean, cpuMax        float64
	memMean, memMax        float64
	stats                  []stat
}

func summarize(phase, container, role string, duration time.Duration, stats []stat) result {
	r := result{phase: phase, container: container, role: role, duration: duration, samples: len(stats),
		stats: stats}
	for _, s := range stats {
		r.cpuMean += s.millicpu
		r.memMean += s.workingMi
		r.cpuMax = max(r.cpuMax, s.millicpu)
		r.memMax = max(r.memMax, s.workingMi)
	}
	if len(stats) > 0 {
		r.cpuMean /= float64(len(stats))
		r.memMean /= float64(len(stats))
	}
	return r
}

func writeTable(out io.Writer, results []result) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', tabwriter.AlignRight)
	if _, err := fmt.Fprintln(w, "phase\tcontainer\trole\tduration\tsamples\tCPU mean (m)\tCPU max (m)\t"+
		"memory mean (Mi)\tmemory max (Mi)\t"); err != nil {
		return err
	}
	for _, r := range results {
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%.0f\t%.0f\t%.1f\t%.1f\t\n", r.phase, r.container, r.role,
			r.duration.Round(time.Second), r.samples, r.cpuMean, r.cpuMax, r.memMean, r.memMax); err != nil {
			return err
		}
	}
	return w.Flush()
}

// writeSamples writes every sample as CSV, with its offset in its phase.
func writeSamples(out io.Writer, results []result) error {
	w := csv.NewWriter(out)
	if err := w.Write([]string{"phase", "container", "role", "seconds", "cpu_millicores", "working_set_mib"}); err != nil {
		return err
	}
	for _, r := range results {
		for _, s := range r.stats {
			if err := w.Write([]string{r.phase, r.container, r.role, strconv.FormatFloat(s.at.Seconds(), 'f', 1, 64),
				strconv.FormatFloat(s.millicpu, 'f', 1, 64), strconv.FormatFloat(s.workingMi, 'f', 2, 64)}); err != nil {
				return err
			}
		}
	}
	w.Flush()
	return w.Error()
}
