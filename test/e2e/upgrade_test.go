/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"text/template"
	"time"

	cf "github.com/cloudflare/cloudflare-go"
	"github.com/cloudflare/cloudflare-go/v7/managed_transforms"
	"github.com/cloudflare/cloudflare-go/v7/r2"
	"github.com/cloudflare/cloudflare-go/v7/rulesets"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/yaml"

	cfpkg "github.com/JonathanGraniero/kflare/pkg/cloudflare"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
	"github.com/JonathanGraniero/kflare/test/utils"
)

const (
	// upgradeManifests holds the resources the upgrade specs apply, from the
	// repository's root. Its header says how a release's copy is used.
	upgradeManifests = "test/e2e/testdata/upgrade.yaml"

	// upgradeNamespace holds the upgrade specs' namespaced resources.
	upgradeNamespace = "kflare-e2e-upgrade"

	// upgradeFieldManager applies the manifests, as a GitOps tool's field
	// manager does with server-side apply.
	upgradeFieldManager = "kflare-e2e-upgrade"

	// Without Cloudflare credentials the manifests name this zone and account,
	// and the resources wait for credentials without calling Cloudflare.
	placeholderZone      = "example.com"
	placeholderAccountID = "0123456789abcdef0123456789abcdef"

	// upgradeTimeout bounds each wait for every resource at once.
	upgradeTimeout = 5 * time.Minute
)

// The upgrade specs install the last release's chart (BeforeSuite, with
// KFLARE_E2E_UPGRADE_FROM), apply that release's manifests and let its manager
// reconcile them, then upgrade the chart to this checkout's, as a user would.
// They check that the CRDs and the manager are this checkout's, that the
// stored resources still apply as GitOps would sync them, and that the new
// manager reconciles each resource again and keeps what the release stored.
// With Cloudflare credentials, the release's manager creates the resources'
// objects, and the new manager must find each one in sync and write nothing
// to Cloudflare, then delete it with its resource.
//
// The release's manifests are its copy of upgradeManifests, and this
// checkout's copy is applied after the upgrade: a resource that differs is the
// migration the release notes describe, so it is only checked to reconcile,
// and its kind's controller may write.
var _ = Describe("Upgrade from the last release", Ordered, Label("upgrade"), func() {
	var (
		ctx     = context.Background()
		k8s     client.Client
		release string
		live    bool
		data    manifestData

		// released and current are the manifests of the release and of this
		// checkout; changed holds the keys of the resources they do not agree
		// on, the new ones included.
		released, current manifests
		changed           map[string]bool
		// stored holds each resource as the release's manager left it.
		stored map[string]*unstructured.Unstructured
		// leader is this checkout's controller pod.
		leader string

		// The Cloudflare clients, with credentials.
		cfAPI       *cf.API
		accountRC   *cf.ResourceContainer
		buckets     *r2.BucketService
		ruleService *rulesets.RuleService
		transforms  *managed_transforms.ManagedTransformService
		zoneID      string
		// newEntrypointPhases are the phases in which the zone had no entry
		// point before the run.
		newEntrypointPhases []string
	)

	BeforeAll(func() {
		release = upgradeFrom()
		if release == "" {
			Skip("KFLARE_E2E_UPGRADE_FROM is not set: make test-e2e-upgrade runs these specs")
		}
		scheme := runtime.NewScheme()
		Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
		Expect(apiextensionsv1.AddToScheme(scheme)).To(Succeed())
		var err error
		k8s, err = client.New(config.GetConfigOrDie(), client.Options{Scheme: scheme})
		Expect(err).NotTo(HaveOccurred())

		token, accountID, zoneName := os.Getenv("CF_API_TOKEN"), os.Getenv("CF_ACCOUNT_ID"), os.Getenv("CF_E2E_ZONE")
		live = token != "" && accountID != ""
		run := newRunID()
		By("using run ID " + run)
		if live {
			Expect(zoneName).NotTo(BeEmpty(), "CF_E2E_ZONE is not set: the upgrade specs need a zone, as their "+
				"manifests hold a resource of each kind")
			data = newManifestData(run, upgradeNamespace, zoneName, accountID)
		} else {
			By("without Cloudflare credentials: the resources wait for them, and nothing calls Cloudflare")
			data = newManifestData(run, upgradeNamespace, placeholderZone, placeholderAccountID)
		}

		root, err := utils.GetProjectDir()
		Expect(err).NotTo(HaveOccurred())
		ours, err := os.ReadFile(filepath.Join(root, upgradeManifests))
		Expect(err).NotTo(HaveOccurred())
		theirs, found, err := releasedTemplate(root, release)
		Expect(err).NotTo(HaveOccurred())
		if !found {
			By(release + " predates " + upgradeManifests + ": applying this checkout's copy to it")
			theirs = string(ours)
		}
		released, err = renderManifests(theirs, data)
		Expect(err).NotTo(HaveOccurred(), "%s's %s", release, upgradeManifests)
		current, err = renderManifests(string(ours), data)
		Expect(err).NotTo(HaveOccurred(), upgradeManifests)
		changed = changedObjects(released, current)
		for _, key := range slices.Sorted(maps.Keys(changed)) {
			By("this checkout changes or adds " + key + ", which it applies after the upgrade")
		}

		if live {
			cfAPI, err = cf.NewWithAPIToken(token)
			Expect(err).NotTo(HaveOccurred())
			accountRC = cf.AccountIdentifier(accountID)
			cfClient, err := cfpkg.New(token)
			Expect(err).NotTo(HaveOccurred())
			buckets = r2.NewBucketService(cfClient.V7Options()...)
			ruleService = rulesets.NewRuleService(cfClient.V7Options()...)
			transforms = managed_transforms.NewManagedTransformService(cfClient.V7Options()...)
			zoneID, err = cfAPI.ZoneIDByName(zoneName)
			Expect(err).NotTo(HaveOccurred())
			Expect(enabledTransforms(ctx, transforms, zoneID)).To(BeEmpty(),
				"the zone's managed transforms must all be off: the run turns them all off at the end")
			newEntrypointPhases = phasesWithoutEntrypoint(ctx, cfAPI, zoneID)

			// Runs last, after the resources are deleted.
			DeferCleanup(func() {
				By("removing anything this run left behind in Cloudflare")
				sweepUpgrade(ctx, cfAPI, accountRC, buckets, ruleService, transforms, zoneID, newEntrypointPhases, data,
					current, released)
			})
		}

		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: upgradeNamespace}}
		Expect(client.IgnoreAlreadyExists(k8s.Create(ctx, ns))).To(Succeed())
		DeferCleanup(func() {
			By("deleting the upgrade specs' resources, if a spec left them")
			deleteResources(ctx, k8s, released, current)
			Expect(client.IgnoreNotFound(k8s.Delete(ctx, ns))).To(Succeed())
		})
		if live {
			Expect(k8s.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-token", Namespace: upgradeNamespace},
				StringData: map[string]string{"CF_API_TOKEN": token},
			})).To(Succeed())
		}
	})

	It("applies the release's manifests, which its manager reconciles", func() {
		Expect(applyManifests(released, false)).To(Succeed())
		By("waiting for the release's manager to report on every resource")
		expectEvery(ctx, k8s, released.objects, func(obj *unstructured.Unstructured) error {
			return expectReady(obj, live)
		})
		stored = settled(ctx, k8s, released.objects)
	})

	It("upgrades the chart to this checkout's", func() {
		_, err := utils.Run(exec.Command("helm", "upgrade", releaseName, "helm/kflare",
			"--namespace", managerNamespace,
			"--set", "image.repository="+managerImageRepository+",image.tag="+managerImageTag,
			"--set", chartValues,
			"--wait", "--timeout", "3m"))
		Expect(err).NotTo(HaveOccurred())
	})

	It("serves this checkout's CRDs", func() {
		root, err := utils.GetProjectDir()
		Expect(err).NotTo(HaveOccurred())
		files, err := filepath.Glob(filepath.Join(root, "config", "crd", "bases", "*.yaml"))
		Expect(err).NotTo(HaveOccurred())
		Expect(files).NotTo(BeEmpty())
		for _, file := range files {
			raw, err := os.ReadFile(file)
			Expect(err).NotTo(HaveOccurred())
			want := &apiextensionsv1.CustomResourceDefinition{}
			Expect(yaml.Unmarshal(raw, want)).To(Succeed())
			got := &apiextensionsv1.CustomResourceDefinition{}
			Expect(k8s.Get(ctx, client.ObjectKey{Name: want.Name}, got)).To(Succeed())
			Expect(crdDifferences(want, got)).To(BeEmpty(), "%s is not this checkout's", want.Name)
		}
	})

	It("runs this checkout's manager, which holds the lease", func() {
		Eventually(func() error {
			pod, err := runningController()
			if err != nil {
				return err
			}
			out, err := utils.Run(exec.Command("kubectl", "get", "pod", pod, "-n", managerNamespace,
				"-o", `jsonpath={.spec.containers[?(@.name=="manager")].image}`))
			if err != nil {
				return err
			}
			if image := string(out); image != managerImageRepository+":"+managerImageTag {
				return fmt.Errorf("pod %s runs %s, not this checkout's image", pod, image)
			}
			if err := holdsLease(pod); err != nil {
				return err
			}
			leader = pod
			return nil
		}, 2*time.Minute, 2*time.Second).Should(Succeed())
	})

	It("applies this checkout's manifests, as a GitOps sync after the upgrade would", func() {
		// A resource whose manifest did not change reapplies as it is: the
		// API server refuses a field the new CRDs no longer have.
		Expect(applyManifests(current, false)).To(Succeed())
	})

	It("accepts the same manifests as new resources, as on a new cluster", func() {
		// An update to a resource passes validation the stored value already
		// failed (validation ratcheting); a create does not.
		namespace := upgradeNamespace + "-new"
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		Expect(client.IgnoreAlreadyExists(k8s.Create(ctx, ns))).To(Succeed())
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8s.Delete(ctx, ns))).To(Succeed())
		})
		root, err := utils.GetProjectDir()
		Expect(err).NotTo(HaveOccurred())
		ours, err := os.ReadFile(filepath.Join(root, upgradeManifests))
		Expect(err).NotTo(HaveOccurred())
		fresh, err := renderManifests(string(ours), newManifestData(newRunID(), namespace, data.Zone, data.AccountID))
		Expect(err).NotTo(HaveOccurred())
		Expect(applyManifests(fresh, true)).To(Succeed())
	})

	It("reconciles every resource again, keeping what the release stored", func() {
		// A paused resource reports Paused, and only a full reconcile replaces
		// that reason once the annotation goes, so every resource has been
		// through one of the new manager's.
		By("pausing every resource")
		for _, obj := range current.objects {
			Expect(annotate(ctx, k8s, obj, reconciler.PausedAnnotation, new("true"))).To(Succeed())
		}
		expectEvery(ctx, k8s, current.objects, func(obj *unstructured.Unstructured) error {
			ready, ok := readyCondition(obj)
			if !ok || ready.Reason != "Paused" {
				return fmt.Errorf("not paused yet: %+v", ready)
			}
			return nil
		})
		By("resuming every resource")
		for _, obj := range current.objects {
			Expect(annotate(ctx, k8s, obj, reconciler.PausedAnnotation, nil)).To(Succeed())
		}
		expectEvery(ctx, k8s, current.objects, func(obj *unstructured.Unstructured) error {
			return expectReady(obj, live)
		})

		By("checking that each resource keeps what the release stored")
		// Every resource that lost something, so that one run shows them all.
		lost := map[string][]string{}
		for key, now := range settled(ctx, k8s, current.objects) {
			was, ok := stored[key]
			if !ok {
				continue
			}
			if changed[key] {
				if now.GetUID() != was.GetUID() {
					lost[key] = []string{"it was recreated"}
				}
				continue
			}
			if l := lostSince(was, now); len(l) > 0 {
				lost[key] = l
			}
		}
		Expect(lost).To(BeEmpty(), "these resources lost what the release stored")
	})

	It("writes nothing to Cloudflare for a resource whose manifest did not change", func() {
		if !live {
			Skip("no Cloudflare credentials: the manager has not called Cloudflare")
		}
		pod, err := runningController()
		Expect(err).NotTo(HaveOccurred())
		Expect(pod).To(Equal(leader), "a new controller pod has new metrics")
		// A migration may write, and so may a new kind's controller.
		allowed := map[string]bool{}
		for key := range changed {
			allowed[strings.ToLower(strings.Fields(key)[0])] = true
		}
		reads, writes := cloudflareRequests(pod)
		Expect(reads).To(BeNumerically(">", 0), "the manager's metrics count no read of Cloudflare's")
		maps.DeleteFunc(writes, func(controller string, _ []string) bool { return allowed[controller] })
		Expect(writes).To(BeEmpty(), "these controllers wrote to Cloudflare after the upgrade")
	})

	It("deletes every resource, and with it what it holds in Cloudflare", func() {
		var ids cloudflareIDs
		if live {
			ids = collectIDs(ctx, k8s, current)
		}
		deleteResources(ctx, k8s, released, current)
		if live {
			Expect(cloudflareLeftovers(ctx, cfAPI, accountRC, buckets, transforms, zoneID, data, current, ids)).
				To(BeEmpty(), "Cloudflare still holds them")
		}
	})
})

// TestUpgradeManifests checks the upgrade manifests without a cluster, as part
// of every run of the suite: that they render, hold each resource once, and
// hold one of every kind the CRDs define.
func TestUpgradeManifests(t *testing.T) {
	// The suite's commands may have moved to the repository's root already.
	root, err := utils.GetProjectDir()
	if err != nil {
		t.Fatal(err)
	}
	text, err := os.ReadFile(filepath.Join(root, upgradeManifests))
	if err != nil {
		t.Fatal(err)
	}
	m, err := renderManifests(string(text), newManifestData("abc123", upgradeNamespace, placeholderZone,
		placeholderAccountID))
	if err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(root, "config", "crd", "bases", "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no CRDs: %v", err)
	}
	kinds := make([]string, 0, len(files))
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := yaml.Unmarshal(raw, crd); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, crd.Spec.Names.Kind)
	}
	for _, problem := range manifestProblems(m, kinds) {
		t.Errorf("%s %s", upgradeManifests, problem)
	}

	// And manifests the checks refuse.
	zone, record := object(t, "Zone", "zone", `{}`), object(t, "DNSRecord", "record", `{}`)
	other := object(t, "Zone", "other", `{}`)
	other.SetAPIVersion("v1")
	unnamed := object(t, "Zone", "", `{}`)
	got := manifestProblems(manifests{objects: []*unstructured.Unstructured{zone, record, record, other, unnamed}},
		[]string{"Zone", "DNSRecord", "Tunnel"})
	want := []string{
		"holds DNSRecord " + upgradeNamespace + "/record twice",
		fmt.Sprintf("holds an object that is not a named kflare resource: %v", other.Object),
		fmt.Sprintf("holds an object that is not a named kflare resource: %v", unnamed.Object),
		"has no Tunnel: add one, so that upgrades are tested with it",
	}
	if !slices.Equal(got, want) {
		t.Errorf("problems\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// manifestProblems returns what is wrong with m, the upgrade manifests, for
// CRDs of kinds: each object must be a named kflare resource, held once, and
// every kind must have one.
func manifestProblems(m manifests, kinds []string) []string {
	var problems []string
	held, keys := map[string]bool{}, map[string]bool{}
	for _, obj := range m.objects {
		switch {
		case obj.GetAPIVersion() != "kflare.dev/v1alpha1" || obj.GetName() == "":
			problems = append(problems, fmt.Sprintf("holds an object that is not a named kflare resource: %v",
				obj.Object))
		case keys[keyOf(obj)]:
			problems = append(problems, "holds "+keyOf(obj)+" twice")
		}
		held[obj.GetKind()], keys[keyOf(obj)] = true, true
	}
	for _, kind := range kinds {
		if !held[kind] {
			problems = append(problems, "has no "+kind+": add one, so that upgrades are tested with it")
		}
	}
	return problems
}

// object returns an object of kind named name in the upgrade namespace, with
// the spec given as JSON.
func object(t *testing.T, kind, name, spec string) *unstructured.Unstructured {
	t.Helper()
	obj := &unstructured.Unstructured{}
	raw := `{"apiVersion": "kflare.dev/v1alpha1", "kind": "` + kind + `", "metadata": {"name": "` + name + `", ` +
		`"namespace": "` + upgradeNamespace + `"}, "spec": ` + spec + `}`
	if err := json.Unmarshal([]byte(raw), &obj.Object); err != nil {
		t.Fatal(err)
	}
	return obj
}

func TestChangedObjects(t *testing.T) {
	released := manifests{objects: []*unstructured.Unstructured{
		object(t, "Zone", "zone", `{"name": "example.com"}`),
		object(t, "DNSRecord", "record", `{"name": "a.example.com", "ttl": 60}`),
	}}
	current := manifests{objects: []*unstructured.Unstructured{
		object(t, "Zone", "zone", `{"name": "example.com"}`),
		object(t, "DNSRecord", "record", `{"name": "a.example.com", "ttl": 120}`),
		object(t, "Tunnel", "tunnel", `{"name": "t"}`),
	}}
	got := slices.Sorted(maps.Keys(changedObjects(released, current)))
	want := []string{"DNSRecord " + upgradeNamespace + "/record", "Tunnel " + upgradeNamespace + "/tunnel"}
	if !slices.Equal(got, want) {
		t.Errorf("changed %v, want %v", got, want)
	}
	if changed := changedObjects(current, current); len(changed) > 0 {
		t.Errorf("unchanged manifests: %v", changed)
	}
}

func TestLostSince(t *testing.T) {
	was := object(t, "DNSRecord", "record", `{"name": "a.example.com", "tags": ["x", "y"], "ref": {"name": "zone"}}`)
	was.SetUID("1")
	was.SetLabels(map[string]string{"kflare.dev/record-id": "r1"})
	was.SetAnnotations(map[string]string{"kflare.dev/adopted": "true"})
	was.SetFinalizers([]string{"kflare.dev/finalizer"})
	was.Object["status"] = map[string]any{
		"conditions":         []any{map[string]any{"type": "Ready", "status": "True", "reason": "Synced"}},
		"cloudflareMetadata": map[string]any{"recordID": "r1"},
	}
	same := func() *unstructured.Unstructured { return was.DeepCopy() }

	for _, tc := range []struct {
		name   string
		change func(*unstructured.Unstructured)
		want   []string
		// annotations replaces was's, when set.
		annotations map[string]string
	}{
		{name: "nothing changed", change: func(*unstructured.Unstructured) {}},
		{name: "fields gained, and other conditions", change: func(now *unstructured.Unstructured) {
			now.Object["spec"].(map[string]any)["ttl"] = int64(1)
			now.SetLabels(map[string]string{"kflare.dev/record-id": "r1", "kflare.dev/new": "x"})
			now.Object["status"].(map[string]any)["conditions"] = []any{}
		}},
		{name: "a label lost", change: func(now *unstructured.Unstructured) { now.SetLabels(nil) },
			want: []string{".metadata.labels: map[kflare.dev/record-id:r1] becomes <nil>"}},
		{name: "an annotation changed", change: func(now *unstructured.Unstructured) {
			now.SetAnnotations(map[string]string{"kflare.dev/adopted": "false"})
		}, want: []string{".metadata.annotations.kflare.dev/adopted: true becomes false"}},
		{name: "a finalizer lost", change: func(now *unstructured.Unstructured) { now.SetFinalizers(nil) },
			want: []string{".metadata.finalizers: [kflare.dev/finalizer] becomes <nil>"}},
		{name: "a finalizer gained", change: func(now *unstructured.Unstructured) {
			now.SetFinalizers([]string{"kflare.dev/finalizer", "kflare.dev/other"})
		}},
		{name: "a finalizer replaced", change: func(now *unstructured.Unstructured) {
			now.SetFinalizers([]string{"kflare.dev/other"})
		}, want: []string{".metadata.finalizers[0]: kflare.dev/finalizer becomes kflare.dev/other"}},
		{name: "a spec field lost, a list shortened, a value changed", change: func(now *unstructured.Unstructured) {
			spec := now.Object["spec"].(map[string]any)
			delete(spec, "name")
			spec["tags"] = []any{"x"}
			spec["ref"] = map[string]any{"name": "other"}
		}, want: []string{".spec.name: a.example.com is gone", ".spec.ref.name: zone becomes other",
			".spec.tags: [x y] becomes [x]"}},
		{name: "a status ID changed", change: func(now *unstructured.Unstructured) {
			now.Object["status"].(map[string]any)["cloudflareMetadata"] = map[string]any{"recordID": "r2"}
		}, want: []string{".status.cloudflareMetadata.recordID: r1 becomes r2"}},
		{name: "recreated", change: func(now *unstructured.Unstructured) { now.SetUID("2") },
			want: []string{"it was recreated"}},
		{name: "the creating annotation dropped", change: func(now *unstructured.Unstructured) {
			now.SetAnnotations(map[string]string{"kflare.dev/adopted": "true"})
		}, annotations: map[string]string{"kflare.dev/adopted": "true", reconciler.CreatingAnnotation: "z1"}},
		{name: "the only annotation, the creating one, dropped", change: func(now *unstructured.Unstructured) {
			now.SetAnnotations(nil)
		}, annotations: map[string]string{reconciler.CreatingAnnotation: "z1"}},
		{name: "another annotation lost along with the creating one", change: func(now *unstructured.Unstructured) {
			now.SetAnnotations(nil)
		}, annotations: map[string]string{"kflare.dev/adopted": "true", reconciler.CreatingAnnotation: "z1"},
			want: []string{".metadata.annotations: map[kflare.dev/adopted:true] becomes <nil>"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			was := same()
			if tc.annotations != nil {
				was.SetAnnotations(tc.annotations)
			}
			now := was.DeepCopy()
			tc.change(now)
			if got := lostSince(was, now); !slices.Equal(got, tc.want) {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// newRunID returns a random ID for a run's names.
func newRunID() string {
	b := make([]byte, 3)
	_, err := rand.Read(b)
	Expect(err).NotTo(HaveOccurred())
	return hex.EncodeToString(b)
}

// manifestData fills in the upgrade manifests' template, whose header says
// what each field holds. A release's copy of the template may use any of them.
type manifestData struct {
	Name, ListName, Namespace, Zone, AccountID, Run string
}

func newManifestData(run, namespace, zone, accountID string) manifestData {
	return manifestData{Name: "kflare-e2e-" + run, ListName: "kflare_e2e_" + run, Namespace: namespace, Zone: zone,
		AccountID: accountID, Run: run}
}

// manifests are rendered upgrade manifests: the YAML to apply, and its
// objects, in order.
type manifests struct {
	yaml    string
	objects []*unstructured.Unstructured
}

// renderManifests renders the template text with data.
func renderManifests(text string, data manifestData) (manifests, error) {
	tmpl, err := template.New(upgradeManifests).Parse(text)
	if err != nil {
		return manifests{}, err
	}
	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, data); err != nil {
		return manifests{}, err
	}
	m := manifests{yaml: rendered.String()}
	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(rendered.Bytes())))
	for {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return m, nil
		}
		if err != nil {
			return manifests{}, err
		}
		var content map[string]any
		if err := yaml.Unmarshal(doc, &content); err != nil {
			return manifests{}, err
		}
		if len(content) > 0 {
			m.objects = append(m.objects, &unstructured.Unstructured{Object: content})
		}
	}
}

// releasedTemplate returns the upgrade manifests' template as the release tag
// has it, or false when the release predates the file. The tag must be in the
// repository at root.
func releasedTemplate(root, tag string) (string, bool, error) {
	git := func(args ...string) *exec.Cmd {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		return cmd
	}
	if err := git("rev-parse", "--verify", "--quiet", tag+"^{commit}").Run(); err != nil {
		return "", false, fmt.Errorf("release %s is not in the repository's tags (git fetch --tags): %w", tag, err)
	}
	if err := git("cat-file", "-e", tag+":"+upgradeManifests).Run(); err != nil {
		return "", false, nil
	}
	out, err := git("show", tag+":"+upgradeManifests).Output()
	return string(out), true, err
}

// keyOf names obj by its kind, namespace and name.
func keyOf(obj *unstructured.Unstructured) string {
	return obj.GetKind() + " " + client.ObjectKeyFromObject(obj).String()
}

// changedObjects returns the keys of the objects of current that released
// does not hold as they are.
func changedObjects(released, current manifests) map[string]bool {
	before := map[string]*unstructured.Unstructured{}
	for _, obj := range released.objects {
		before[keyOf(obj)] = obj
	}
	changed := map[string]bool{}
	for _, obj := range current.objects {
		if was, ok := before[keyOf(obj)]; !ok || !reflect.DeepEqual(was.Object, obj.Object) {
			changed[keyOf(obj)] = true
		}
	}
	return changed
}

// applyManifests applies m with server-side apply, as a GitOps tool does, and
// strict field validation, which refuses a field the CRDs do not have. dryRun
// has the API server check them without storing anything.
func applyManifests(m manifests, dryRun bool) error {
	args := []string{"apply", "--server-side", "--field-manager=" + upgradeFieldManager, "--validate=strict", "-f", "-"}
	if dryRun {
		args = append(args, "--dry-run=server")
	}
	cmd := exec.Command("kubectl", args...)
	cmd.Stdin = strings.NewReader(m.yaml)
	_, err := utils.Run(cmd)
	return err
}

// fetch returns obj as the API server has it.
func fetch(ctx context.Context, k8s client.Client, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(obj.GroupVersionKind())
	return current, k8s.Get(ctx, client.ObjectKeyFromObject(obj), current)
}

// expectEvery waits until check passes for every object, as the API server
// has it. check returns why an object does not pass yet.
func expectEvery(ctx context.Context, k8s client.Client, objects []*unstructured.Unstructured,
	check func(*unstructured.Unstructured) error) {
	GinkgoHelper()
	Eventually(func() error {
		var errs []error
		for _, obj := range objects {
			current, err := fetch(ctx, k8s, obj)
			if err == nil {
				err = check(current)
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", keyOf(obj), err))
			}
		}
		return errors.Join(errs...)
	}, upgradeTimeout, e2ePoll).Should(Succeed())
}

// settled returns the objects, by key, as the API server has them once their
// manager is done with them: none changed between two reads five seconds
// apart, as a status write starts another reconcile.
func settled(
	ctx context.Context, k8s client.Client, objects []*unstructured.Unstructured,
) map[string]*unstructured.Unstructured {
	GinkgoHelper()
	var last, read map[string]*unstructured.Unstructured
	versions := func(m map[string]*unstructured.Unstructured) map[string]string {
		out := map[string]string{}
		for key, obj := range m {
			out[key] = obj.GetResourceVersion()
		}
		return out
	}
	Eventually(func() (bool, error) {
		read = map[string]*unstructured.Unstructured{}
		for _, obj := range objects {
			current, err := fetch(ctx, k8s, obj)
			if err != nil {
				return false, err
			}
			read[keyOf(obj)] = current
		}
		done := last != nil && maps.Equal(versions(read), versions(last))
		last = read
		return done, nil
	}, upgradeTimeout, 5*time.Second).Should(BeTrue(), "the resources keep changing")
	return read
}

// readyCondition returns obj's Ready condition, if it has one.
func readyCondition(obj *unstructured.Unstructured) (metav1.Condition, bool) {
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, c := range conditions {
		condition, ok := c.(map[string]any)
		if !ok || condition["type"] != "Ready" {
			continue
		}
		var ready metav1.Condition
		raw, err := json.Marshal(condition)
		if err == nil {
			err = json.Unmarshal(raw, &ready)
		}
		return ready, err == nil
	}
	return metav1.Condition{}, false
}

// expectReady returns why obj does not yet report what a manager that has
// reconciled it reports: Ready with credentials (live), and, without them,
// Ready False for a reason it waits on. A paused resource is not done.
func expectReady(obj *unstructured.Unstructured, live bool) error {
	ready, ok := readyCondition(obj)
	want := metav1.ConditionFalse
	if live {
		want = metav1.ConditionTrue
	}
	switch {
	case !ok:
		return errors.New("no Ready condition yet")
	case ready.Reason == "Paused" || ready.Status != want:
		return fmt.Errorf("Ready is %s, not %s: %s: %s", ready.Status, want, ready.Reason, ready.Message)
	}
	return nil
}

// annotate sets obj's annotation key to value, or removes it when value is
// nil, with a merge patch.
func annotate(ctx context.Context, k8s client.Client, obj *unstructured.Unstructured, key string, value *string) error {
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]*string{key: value}}})
	if err != nil {
		return err
	}
	target := &unstructured.Unstructured{}
	target.SetGroupVersionKind(obj.GroupVersionKind())
	target.SetNamespace(obj.GetNamespace())
	target.SetName(obj.GetName())
	return k8s.Patch(ctx, target, client.RawPatch(types.MergePatchType, patch))
}

// lostSince returns what now, a resource after the upgrade, lost of was, the
// resource as the release's manager left it: in its spec, labels,
// annotations, finalizers and status but for its conditions. It may have
// gained fields, such as a new field's default, but must keep the ones it had.
func lostSince(was, now *unstructured.Unstructured) []string {
	var lost []string
	if was.GetUID() != now.GetUID() {
		lost = append(lost, "it was recreated")
	}
	for _, field := range [][]string{
		{"metadata", "labels"}, {"metadata", "annotations"}, {"metadata", "finalizers"}, {"spec"}, {"status"},
	} {
		before, found, _ := unstructured.NestedFieldNoCopy(was.Object, field...)
		if !found {
			continue
		}
		after, _, _ := unstructured.NestedFieldNoCopy(now.Object, field...)
		switch field[len(field)-1] {
		case "status":
			before, after = withoutConditions(before), withoutConditions(after)
		case "annotations":
			if before = withoutCreating(before); before == nil {
				continue
			}
		}
		lost = append(lost, missing(before, after, "."+strings.Join(field, "."))...)
	}
	return lost
}

// withoutCreating returns a copy of annotations without kflare.dev/creating,
// or nil when no other is left. That annotation marks a create about to
// happen, and a reconcile drops it once status records what it created: a
// kind with a claim label in the same patch, one without on a later reconcile
// (adoption.go), which may be the new manager's.
func withoutCreating(annotations any) any {
	fields, ok := annotations.(map[string]any)
	if !ok {
		return annotations
	}
	fields = maps.Clone(fields)
	delete(fields, reconciler.CreatingAnnotation)
	if len(fields) == 0 {
		return nil
	}
	return fields
}

// withoutConditions returns a copy of status without its conditions, which
// each reconcile writes.
func withoutConditions(status any) any {
	fields, ok := status.(map[string]any)
	if !ok {
		return status
	}
	fields = maps.Clone(fields)
	delete(fields, "conditions")
	return fields
}

// missing returns, by path, where got lacks a value want holds, or holds
// another value. A map may gain keys, and a list items after those it had,
// such as another finalizer.
func missing(want, got any, path string) []string {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return []string{fmt.Sprintf("%s: %v becomes %v", path, want, got)}
		}
		var out []string
		for _, key := range slices.Sorted(maps.Keys(w)) {
			value, ok := g[key]
			if !ok {
				out = append(out, fmt.Sprintf("%s.%s: %v is gone", path, key, w[key]))
				continue
			}
			out = append(out, missing(w[key], value, path+"."+key)...)
		}
		return out
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) < len(w) {
			return []string{fmt.Sprintf("%s: %v becomes %v", path, want, got)}
		}
		var out []string
		for i := range w {
			out = append(out, missing(w[i], g[i], fmt.Sprintf("%s[%d]", path, i))...)
		}
		return out
	default:
		if !reflect.DeepEqual(want, got) {
			return []string{fmt.Sprintf("%s: %v becomes %v", path, want, got)}
		}
		return nil
	}
}

// crdDifferences returns how got, a CRD the API server serves, differs from
// want, the CRD in config/crd/bases: in its kind, scope, versions or schemas.
func crdDifferences(want, got *apiextensionsv1.CustomResourceDefinition) []string {
	var diffs []string
	if got.Spec.Names.Kind != want.Spec.Names.Kind || got.Spec.Scope != want.Spec.Scope {
		diffs = append(diffs, fmt.Sprintf("kind %s, scope %s", got.Spec.Names.Kind, got.Spec.Scope))
	}
	if len(got.Spec.Versions) != len(want.Spec.Versions) {
		diffs = append(diffs, fmt.Sprintf("%d versions, not %d", len(got.Spec.Versions), len(want.Spec.Versions)))
	}
	for _, version := range want.Spec.Versions {
		i := slices.IndexFunc(got.Spec.Versions, func(v apiextensionsv1.CustomResourceDefinitionVersion) bool {
			return v.Name == version.Name
		})
		if i < 0 {
			diffs = append(diffs, "no version "+version.Name)
			continue
		}
		served := got.Spec.Versions[i]
		if served.Served != version.Served || served.Storage != version.Storage {
			diffs = append(diffs, fmt.Sprintf("%s: served %t, storage %t", version.Name, served.Served, served.Storage))
		}
		wantSchema, err := json.Marshal(version.Schema)
		Expect(err).NotTo(HaveOccurred())
		gotSchema, err := json.Marshal(served.Schema)
		Expect(err).NotTo(HaveOccurred())
		if !bytes.Equal(wantSchema, gotSchema) {
			diffs = append(diffs, version.Name+": another schema")
		}
	}
	return diffs
}

// The samples of kflare_cloudflare_api_requests_total in the metrics' text
// format, and their labels.
var (
	apiRequestSample = regexp.MustCompile(`^kflare_cloudflare_api_requests_total\{([^}]*)\} (\S+)$`)
	sampleLabel      = regexp.MustCompile(`(\w+)="([^"]*)"`)
)

// cloudflareRequests returns how many reads the manager in pod has sent to
// Cloudflare since it started, and its other requests by controller, each as
// method, response code and count.
func cloudflareRequests(pod string) (float64, map[string][]string) {
	GinkgoHelper()
	By("reading the metrics of " + pod + ", with a token allowed to")
	const sa = "e2e-upgrade-metrics"
	_, err := utils.Run(exec.Command("kubectl", "create", "serviceaccount", sa, "-n", managerNamespace))
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() {
		_, _ = utils.Run(exec.Command("kubectl", "delete", "serviceaccount", sa, "-n", managerNamespace,
			"--ignore-not-found"))
	})
	_, err = utils.Run(exec.Command("kubectl", "create", "clusterrolebinding", sa,
		"--clusterrole="+releaseName+"-metrics-reader", "--serviceaccount="+managerNamespace+":"+sa))
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() {
		_, _ = utils.Run(exec.Command("kubectl", "delete", "clusterrolebinding", sa, "--ignore-not-found"))
	})
	out, err := utils.Run(exec.Command("kubectl", "create", "token", sa, "-n", managerNamespace))
	Expect(err).NotTo(HaveOccurred())
	token := strings.TrimSpace(string(out))

	port, stop, err := utils.PortForward(managerNamespace, "pod/"+pod, 8080)
	Expect(err).NotTo(HaveOccurred())
	defer stop()
	httpClient := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: metricsCA, ServerName: metricsServerName},
	}}
	var body string
	// The binding reaches the API server's authorizer within seconds.
	Eventually(func(g Gomega) {
		req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("https://127.0.0.1:%d/metrics", port), nil)
		g.Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := httpClient.Do(req)
		g.Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()
		raw, err := io.ReadAll(resp.Body)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(resp.StatusCode).To(Equal(http.StatusOK), string(raw))
		body = string(raw)
	}, time.Minute, time.Second).Should(Succeed())

	var reads float64
	writes := map[string][]string{}
	for line := range strings.Lines(body) {
		sample := apiRequestSample.FindStringSubmatch(strings.TrimSpace(line))
		if sample == nil {
			continue
		}
		labels := map[string]string{}
		for _, label := range sampleLabel.FindAllStringSubmatch(sample[1], -1) {
			labels[label[1]] = label[2]
		}
		count, err := strconv.ParseFloat(sample[2], 64)
		Expect(err).NotTo(HaveOccurred(), line)
		switch {
		case count == 0:
		case labels["method"] == http.MethodGet:
			reads += count
		default:
			writes[labels["controller"]] = append(writes[labels["controller"]],
				fmt.Sprintf("%s %s (%s)", labels["method"], labels["code"], sample[2]))
		}
	}
	return reads, writes
}

// deleteResources deletes the resources of every manifest at once, as
// `kubectl delete -f` does, the CloudflareAccount last, and waits until they
// are gone: their manager deletes their Cloudflare objects first.
func deleteResources(ctx context.Context, k8s client.Client, all ...manifests) {
	GinkgoHelper()
	var namespaced, cluster []*unstructured.Unstructured
	seen := map[string]bool{}
	for _, m := range all {
		for _, obj := range m.objects {
			if seen[keyOf(obj)] {
				continue
			}
			seen[keyOf(obj)] = true
			if obj.GetNamespace() == "" {
				cluster = append(cluster, obj)
			} else {
				namespaced = append(namespaced, obj)
			}
		}
	}
	for _, objects := range [][]*unstructured.Unstructured{namespaced, cluster} {
		for _, obj := range objects {
			Expect(client.IgnoreNotFound(k8s.Delete(ctx, obj.DeepCopy()))).To(Succeed())
		}
		Eventually(func() error {
			var errs []error
			for _, obj := range objects {
				current, err := fetch(ctx, k8s, obj)
				switch {
				case apierrors.IsNotFound(err):
				case err != nil:
					errs = append(errs, fmt.Errorf("%s: %w", keyOf(obj), err))
				default:
					ready, _ := readyCondition(current)
					errs = append(errs, fmt.Errorf("%s: not deleted yet: %s: %s", keyOf(obj), ready.Reason,
						ready.Message))
				}
			}
			return errors.Join(errs...)
		}, upgradeTimeout, e2ePoll).Should(Succeed())
	}
}

// cloudflareIDs are the IDs of the Cloudflare objects of the upgrade specs'
// resources, from their status.
type cloudflareIDs struct {
	tunnel, kvNamespace, list string
	records, routes           []string
}

// collectIDs reads the IDs of the resources' Cloudflare objects from their
// status.
func collectIDs(ctx context.Context, k8s client.Client, m manifests) cloudflareIDs {
	GinkgoHelper()
	var ids cloudflareIDs
	for _, obj := range m.objects {
		current, err := fetch(ctx, k8s, obj)
		Expect(err).NotTo(HaveOccurred())
		id := func(field string) string {
			value, _, _ := unstructured.NestedString(current.Object, "status", "cloudflareMetadata", field)
			return value
		}
		switch obj.GetKind() {
		case "Tunnel":
			ids.tunnel = id("tunnelID")
		case "KVNamespace":
			ids.kvNamespace = id("namespaceID")
		case "IPList":
			ids.list = id("listID")
		case "DNSRecord":
			ids.records = append(ids.records, id("recordID"))
		case "WorkerRoute":
			ids.routes = append(ids.routes, id("routeID"))
		}
	}
	return ids
}

// rulePhase returns the Rulesets phase of a rule kind's resource, or false
// for a resource of another kind.
func rulePhase(obj *unstructured.Unstructured) (string, bool) {
	phases := map[string]string{
		"WAFCustomRule": customRulesPhase, "RateLimitRule": rateLimitPhase, "ManagedRuleset": managedPhase,
		"RedirectRule": redirectPhase, "CacheRule": cachePhase, "ConfigurationRule": configurationPhase,
		"OriginRule": originPhase,
	}
	if obj.GetKind() == "TransformRule" {
		typ, _, _ := unstructured.NestedString(obj.Object, "spec", "type")
		phases["TransformRule"] = map[string]string{
			"urlRewrite": urlRewritePhase, "requestHeaders": requestHeadersPhase,
			"responseHeaders": responseHeadersPhase,
		}[typ]
	}
	phase, ok := phases[obj.GetKind()]
	return phase, ok
}

// cloudflareLeftovers returns what Cloudflare still holds of the run's
// objects: the zone excepted, which the run adopts and keeps.
func cloudflareLeftovers(ctx context.Context, cfAPI *cf.API, rc *cf.ResourceContainer, buckets *r2.BucketService,
	transforms *managed_transforms.ManagedTransformService, zoneID string, data manifestData, m manifests,
	ids cloudflareIDs) []string {
	GinkgoHelper()
	var left []string
	if ids.tunnel != "" {
		tunnel, err := cfAPI.GetTunnel(ctx, rc, ids.tunnel)
		if err == nil && tunnel.DeletedAt == nil {
			left = append(left, "tunnel "+ids.tunnel)
		} else if err != nil && !cfpkg.IsNotFound(err) {
			Expect(err).NotTo(HaveOccurred())
		}
	}
	if slices.Contains(workerNames(ctx, cfAPI, rc), data.Name) {
		left = append(left, "Worker "+data.Name)
	}
	if _, ok := kvNamespaces(ctx, cfAPI, rc)[ids.kvNamespace]; ok {
		left = append(left, "KV namespace "+ids.kvNamespace)
	}
	var bucket r2.BucketGetParams
	bucket.AccountID.Value, bucket.AccountID.Present = rc.Identifier, true
	if _, err := buckets.Get(ctx, data.Name, bucket); !cfpkg.IsNotFound(err) {
		left = append(left, fmt.Sprintf("R2 bucket %s (%v)", data.Name, err))
	}
	if found := d1DatabaseIDs(ctx, cfAPI, rc, data.Name); len(found) > 0 {
		left = append(left, fmt.Sprintf("D1 databases %v", found))
	}
	if ids.list != "" {
		if _, err := cfAPI.GetList(ctx, rc, ids.list); !cfpkg.IsNotFound(err) {
			left = append(left, fmt.Sprintf("IP list %s (%v)", ids.list, err))
		}
	}
	zrc := cf.ZoneIdentifier(zoneID)
	for _, id := range ids.records {
		if _, err := cfAPI.GetDNSRecord(ctx, zrc, id); !cfpkg.IsNotFound(err) {
			left = append(left, fmt.Sprintf("DNS record %s (%v)", id, err))
		}
	}
	for _, id := range ids.routes {
		if _, err := cfAPI.GetWorkerRoute(ctx, zrc, id); !cfpkg.IsNotFound(err) {
			left = append(left, fmt.Sprintf("Worker route %s (%v)", id, err))
		}
	}
	for _, obj := range m.objects {
		phase, ok := rulePhase(obj)
		if !ok {
			continue
		}
		key := client.ObjectKeyFromObject(obj)
		if _, found := ruleByRef(phaseRules(ctx, cfAPI, zoneID, phase), ruleRef(obj.GetKind(), key)); found {
			left = append(left, fmt.Sprintf("the %s rule of %s", phase, keyOf(obj)))
		}
	}
	if on := enabledTransforms(ctx, transforms, zoneID); len(on) > 0 {
		left = append(left, fmt.Sprintf("managed transforms %v", on))
	}
	// An entry point in the managed phase, even an empty one, turns off
	// Cloudflare's default deployment of the Free Managed Ruleset.
	if _, err := cfAPI.GetEntrypointRuleset(ctx, zrc, managedPhase); !cfpkg.IsNotFound(err) {
		left = append(left, fmt.Sprintf("the %s entry point (%v)", managedPhase, err))
	}
	if _, err := cfAPI.ZoneDetails(ctx, zoneID); err != nil {
		left = append(left, fmt.Sprintf("the zone is gone: %v", err))
	}
	return left
}

// sweepUpgrade deletes whatever the run's objects left in Cloudflare, found
// by their names and refs, and the entry points of newEntrypointPhases, the
// phases that had none before the run, if they are empty. It only reports problems: it runs after the specs,
// whether or not they passed.
func sweepUpgrade(ctx context.Context, cfAPI *cf.API, rc *cf.ResourceContainer, buckets *r2.BucketService,
	ruleService *rulesets.RuleService, transforms *managed_transforms.ManagedTransformService, zoneID string,
	newEntrypointPhases []string, data manifestData, all ...manifests) {
	zrc := cf.ZoneIdentifier(zoneID)
	var routes []string
	found, err := cfAPI.ListWorkerRoutes(ctx, zrc, cf.ListWorkerRoutesParams{})
	if err != nil {
		GinkgoWriter.Printf("cleanup: listing Worker routes: %v\n", err)
	}
	for _, route := range found.Routes {
		if strings.HasPrefix(route.Pattern, data.Name+".") {
			routes = append(routes, route.ID)
		}
	}
	cleanupCloudflare(ctx, cfAPI, rc, data.Name, zoneID,
		[]string{data.Name + "." + data.Zone, data.Name + "-tunnel." + data.Zone}, routes)
	namespaces, _, err := cfAPI.ListWorkersKVNamespaces(ctx, rc, cf.ListWorkersKVNamespacesParams{})
	if err != nil {
		GinkgoWriter.Printf("cleanup: listing KV namespaces: %v\n", err)
	}
	for _, ns := range namespaces {
		if ns.Title != data.Name {
			continue
		}
		if _, err := cfAPI.DeleteWorkersKVNamespace(ctx, rc, ns.ID); err != nil && !cfpkg.IsNotFound(err) {
			GinkgoWriter.Printf("cleanup: deleting KV namespace %s: %v\n", ns.ID, err)
		}
	}
	var bucket r2.BucketDeleteParams
	bucket.AccountID.Value, bucket.AccountID.Present = rc.Identifier, true
	if _, err := buckets.Delete(ctx, data.Name, bucket); err != nil && !cfpkg.IsNotFound(err) {
		GinkgoWriter.Printf("cleanup: deleting R2 bucket %s: %v\n", data.Name, err)
	}
	cleanupD1Database(ctx, cfAPI, rc, data.Name)
	refs := map[string][]string{}
	for _, m := range all {
		for _, obj := range m.objects {
			if phase, ok := rulePhase(obj); ok {
				refs[phase] = append(refs[phase], ruleRef(obj.GetKind(), client.ObjectKeyFromObject(obj)))
			}
		}
	}
	for phase, phaseRefs := range refs {
		cleanupRules(ctx, cfAPI, ruleService, zoneID, phase, phaseRefs...)
	}
	cleanupEmptyEntrypoint(ctx, cfAPI, zoneID, managedPhase)
	for _, phase := range newEntrypointPhases {
		cleanupEmptyEntrypoint(ctx, cfAPI, zoneID, phase)
	}
	// The run found them all off.
	cleanupManagedTransforms(ctx, transforms, zoneID)
	// After the rules: Cloudflare does not delete a list a rule uses.
	cleanupList(ctx, cfAPI, rc, data.ListName)
}
