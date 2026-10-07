/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package e2e

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"time"

	cf "github.com/cloudflare/cloudflare-go"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	cfpkg "github.com/JonathanGraniero/kflare/pkg/cloudflare"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

const (
	// e2eNamespace holds the namespaced resources the Cloudflare specs create.
	e2eNamespace = "kflare-e2e"

	// e2eTimeout bounds each wait for the manager or Cloudflare to converge.
	e2eTimeout = 2 * time.Minute
	e2ePoll    = 2 * time.Second
)

// These specs drive the deployed manager against the real Cloudflare API.
// They run only when CF_API_TOKEN and CF_ACCOUNT_ID are set. The Zone and
// DNSRecord specs also need CF_E2E_ZONE: an existing zone the token can
// edit. It is adopted with deletion-policy retain, so it is never deleted or
// changed; only records named kflare-e2e-<run>.<zone> are created in it.
//
// Every Cloudflare object is named after a random run ID and removed again
// by the specs, or by the cleanup below if a spec fails part-way.
var _ = Describe("Cloudflare", Ordered, Label("cloudflare"), func() {
	var (
		ctx       = context.Background()
		k8s       client.Client
		cfAPI     *cf.API
		accountRC *cf.ResourceContainer
		run       string
		zoneName  string

		accountKey, tunnelKey, tunnelConfigKey, workerKey, zoneKey, recordKey client.ObjectKey
		routeKey, exclusionKey, kvKey                                         client.ObjectKey

		tunnelID, zoneID, recordID, routeID, exclusionID, kvNamespaceID string
	)

	// resourceName is the name of every Cloudflare object this run creates.
	resourceName := func() string { return "kflare-e2e-" + run }
	recordName := func() string { return resourceName() + "." + zoneName }

	// expectReady waits until obj, re-read through key, reports Ready=True.
	expectReady := func(key client.ObjectKey, obj client.Object, conditions func() []metav1.Condition) {
		Eventually(func(g Gomega) {
			g.Expect(k8s.Get(ctx, key, obj)).To(Succeed())
			ready := meta.FindStatusCondition(conditions(), cloudflarev1alpha1.ConditionReady)
			g.Expect(ready).NotTo(BeNil(), "no Ready condition yet")
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue), "%s: %s", ready.Reason, ready.Message)
		}, e2eTimeout, e2ePoll).Should(Succeed())
	}

	// expectGone waits until the object at key no longer exists.
	expectGone := func(key client.ObjectKey, obj client.Object) {
		Eventually(func() bool {
			return apierrors.IsNotFound(k8s.Get(ctx, key, obj))
		}, e2eTimeout, e2ePoll).Should(BeTrue(), "%s still exists", key)
	}

	BeforeAll(func() {
		token, accountID := os.Getenv("CF_API_TOKEN"), os.Getenv("CF_ACCOUNT_ID")
		if token == "" || accountID == "" {
			Skip("CF_API_TOKEN and CF_ACCOUNT_ID are not set")
		}
		zoneName = os.Getenv("CF_E2E_ZONE")

		b := make([]byte, 3)
		_, err := rand.Read(b)
		Expect(err).NotTo(HaveOccurred())
		run = hex.EncodeToString(b)
		By("using run ID " + run)

		scheme := runtime.NewScheme()
		Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
		Expect(cloudflarev1alpha1.AddToScheme(scheme)).To(Succeed())
		k8s, err = client.New(config.GetConfigOrDie(), client.Options{Scheme: scheme})
		Expect(err).NotTo(HaveOccurred())

		cfAPI, err = cf.NewWithAPIToken(token)
		Expect(err).NotTo(HaveOccurred())
		accountRC = cf.AccountIdentifier(accountID)

		accountKey = client.ObjectKey{Name: resourceName()}
		tunnelKey = client.ObjectKey{Namespace: e2eNamespace, Name: "tunnel"}
		tunnelConfigKey = client.ObjectKey{Namespace: e2eNamespace, Name: "tunnel-config"}
		workerKey = client.ObjectKey{Namespace: e2eNamespace, Name: "worker"}
		zoneKey = client.ObjectKey{Namespace: e2eNamespace, Name: "zone"}
		recordKey = client.ObjectKey{Namespace: e2eNamespace, Name: "record"}
		routeKey = client.ObjectKey{Namespace: e2eNamespace, Name: "route"}
		exclusionKey = client.ObjectKey{Namespace: e2eNamespace, Name: "route-exclusion"}
		kvKey = client.ObjectKey{Namespace: e2eNamespace, Name: "kv"}

		DeferCleanup(func() {
			By("removing anything this run left behind in Cloudflare")
			cleanupCloudflare(ctx, cfAPI, accountRC, resourceName(), zoneID, recordName(), routeIDs(routeID, exclusionID))
			if kvNamespaceID != "" {
				_, err := cfAPI.DeleteWorkersKVNamespace(ctx, accountRC, kvNamespaceID)
				if err != nil && !cfpkg.IsNotFound(err) {
					GinkgoWriter.Printf("cleanup: deleting KV namespace %s: %v\n", kvNamespaceID, err)
				}
			}
		})

		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: e2eNamespace}}
		Expect(client.IgnoreAlreadyExists(k8s.Create(ctx, ns))).To(Succeed())
		Expect(k8s.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-token", Namespace: e2eNamespace},
			StringData: map[string]string{"CF_API_TOKEN": token},
		})).To(Succeed())
		Expect(k8s.Create(ctx, &cloudflarev1alpha1.CloudflareAccount{
			ObjectMeta: metav1.ObjectMeta{Name: accountKey.Name},
			Spec: cloudflarev1alpha1.CloudflareAccountSpec{
				AccountID: accountID,
				TokenSecretRef: cloudflarev1alpha1.SecretReference{
					Name: "cloudflare-token", Namespace: e2eNamespace, Key: "CF_API_TOKEN",
				},
			},
		})).To(Succeed())
	})

	It("validates the account", func() {
		account := &cloudflarev1alpha1.CloudflareAccount{}
		expectReady(accountKey, account, func() []metav1.Condition { return account.Status.Conditions })
		Expect(account.Status.AccountName).NotTo(BeEmpty())
	})

	It("creates a Tunnel and writes its token Secret", func() {
		tunnel := &cloudflarev1alpha1.Tunnel{
			ObjectMeta: metav1.ObjectMeta{Name: tunnelKey.Name, Namespace: e2eNamespace},
			Spec: cloudflarev1alpha1.TunnelSpec{
				Name:                 resourceName(),
				AccountRef:           corev1.LocalObjectReference{Name: accountKey.Name},
				CredentialsSecretRef: cloudflarev1alpha1.TunnelCredentialsSecretReference{Name: "tunnel-token"},
			},
		}
		Expect(k8s.Create(ctx, tunnel)).To(Succeed())
		expectReady(tunnelKey, tunnel, func() []metav1.Condition { return tunnel.Status.Conditions })
		tunnelID = tunnel.Status.CloudflareMetadata.TunnelID

		secret := &corev1.Secret{}
		Expect(k8s.Get(ctx, client.ObjectKey{Namespace: e2eNamespace, Name: "tunnel-token"}, secret)).To(Succeed())
		Expect(secret.Data).To(HaveKey(cloudflarev1alpha1.TunnelTokenKey))

		cfTunnel, err := cfAPI.GetTunnel(ctx, accountRC, tunnelID)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfTunnel.Name).To(Equal(resourceName()))
		Expect(cfTunnel.DeletedAt).To(BeNil())
	})

	It("pushes a TunnelConfiguration's ingress rules", func() {
		hostname := resourceName() + ".example.com"
		tc := &cloudflarev1alpha1.TunnelConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: tunnelConfigKey.Name, Namespace: e2eNamespace},
			Spec: cloudflarev1alpha1.TunnelConfigurationSpec{
				TunnelRef: corev1.LocalObjectReference{Name: tunnelKey.Name},
				Ingress: []cloudflarev1alpha1.TunnelIngressRule{
					{Hostname: hostname, Service: "http://localhost:8080"},
				},
			},
		}
		Expect(k8s.Create(ctx, tc)).To(Succeed())
		expectReady(tunnelConfigKey, tc, func() []metav1.Condition { return tc.Status.Conditions })
		Expect(tc.Status.CloudflareMetadata.Version).To(BeNumerically(">", 0))

		cfConfig, err := cfAPI.GetTunnelConfiguration(ctx, accountRC, tunnelID)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfConfig.Config.Ingress).To(HaveLen(2))
		Expect(cfConfig.Config.Ingress[0].Hostname).To(Equal(hostname))
		Expect(cfConfig.Config.Ingress[1].Service).To(Equal("http_status:404"))
	})

	It("creates a KVNamespace", func() {
		kv := &cloudflarev1alpha1.KVNamespace{
			ObjectMeta: objectMeta(kvKey),
			Spec: cloudflarev1alpha1.KVNamespaceSpec{
				AccountRef: corev1.LocalObjectReference{Name: accountKey.Name},
				Title:      resourceName(),
			},
		}
		Expect(k8s.Create(ctx, kv)).To(Succeed())
		expectReady(kvKey, kv, func() []metav1.Condition { return kv.Status.Conditions })
		kvNamespaceID = kv.Status.CloudflareMetadata.NamespaceID

		Expect(kvNamespaces(ctx, cfAPI, accountRC)).To(HaveKeyWithValue(kvNamespaceID, resourceName()))
	})

	It("uploads a WorkerScript bound to the KVNamespace", func() {
		script := `export default { async fetch(request, env) { return new Response(env.GREETING); } };`
		greeting := "hello from kflare e2e"
		worker := &cloudflarev1alpha1.WorkerScript{
			ObjectMeta: metav1.ObjectMeta{Name: workerKey.Name, Namespace: e2eNamespace},
			Spec: cloudflarev1alpha1.WorkerScriptSpec{
				Name:              resourceName(),
				AccountRef:        corev1.LocalObjectReference{Name: accountKey.Name},
				Script:            &script,
				CompatibilityDate: "2024-09-23",
				Bindings: []cloudflarev1alpha1.WorkerBinding{
					{Name: "GREETING", PlainText: &greeting},
					{Name: "CACHE", KVNamespaceRef: &corev1.LocalObjectReference{Name: kvKey.Name}},
				},
			},
		}
		Expect(k8s.Create(ctx, worker)).To(Succeed())
		expectReady(workerKey, worker, func() []metav1.Condition { return worker.Status.Conditions })
		Expect(worker.Status.CloudflareMetadata.ModifiedOn).NotTo(BeEmpty())

		Expect(workerNames(ctx, cfAPI, accountRC)).To(ContainElement(resourceName()))
		bindings, err := cfAPI.ListWorkerBindings(ctx, accountRC, cf.ListWorkerBindingsParams{ScriptName: resourceName()})
		Expect(err).NotTo(HaveOccurred())
		Expect(bindings.BindingList).To(ContainElement(cf.WorkerBindingListItem{
			Name: "CACHE", Binding: cf.WorkerKvNamespaceBinding{NamespaceID: kvNamespaceID},
		}))
	})

	It("adopts the test zone without changing it", func() {
		if zoneName == "" {
			Skip("CF_E2E_ZONE is not set")
		}
		zone := &cloudflarev1alpha1.Zone{
			ObjectMeta: metav1.ObjectMeta{
				Name: zoneKey.Name, Namespace: e2eNamespace,
				// Never delete the shared test zone.
				Annotations: map[string]string{reconciler.DeletionPolicyAnnotation: reconciler.DeletionPolicyRetain},
			},
			Spec: cloudflarev1alpha1.ZoneSpec{
				Name:       zoneName,
				AccountRef: corev1.LocalObjectReference{Name: accountKey.Name},
			},
		}
		Expect(k8s.Create(ctx, zone)).To(Succeed())
		expectReady(zoneKey, zone, func() []metav1.Condition { return zone.Status.Conditions })
		zoneID = zone.Status.CloudflareMetadata.ZoneID
		Expect(zoneID).NotTo(BeEmpty())
	})

	It("creates a DNSRecord and leaves it alone once it is in sync", func() {
		if zoneName == "" {
			Skip("CF_E2E_ZONE is not set")
		}
		// ttl and proxied are left unset on purpose: Cloudflare reports them
		// anyway, and the controller must not keep "correcting" them.
		record := &cloudflarev1alpha1.DNSRecord{
			ObjectMeta: metav1.ObjectMeta{Name: recordKey.Name, Namespace: e2eNamespace},
			Spec: cloudflarev1alpha1.DNSRecordSpec{
				ZoneRef: corev1.LocalObjectReference{Name: zoneKey.Name},
				Name:    recordName(),
				Type:    "A",
				Content: "192.0.2.10",
				Comment: "kflare e2e " + run,
			},
		}
		Expect(k8s.Create(ctx, record)).To(Succeed())
		expectReady(recordKey, record, func() []metav1.Condition { return record.Status.Conditions })
		recordID = record.Status.CloudflareMetadata.RecordID

		cfRecord, err := cfAPI.GetDNSRecord(ctx, cf.ZoneIdentifier(zoneID), recordID)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfRecord.Content).To(Equal("192.0.2.10"))
		Expect(cfRecord.TTL).To(Equal(1))

		By("re-triggering the reconcile and checking Cloudflare is not written to again")
		modifiedOn := cfRecord.ModifiedOn
		patch := client.MergeFrom(record.DeepCopy())
		record.Annotations = map[string]string{"e2e.kflare.dev/touched": time.Now().Format(time.RFC3339)}
		Expect(k8s.Patch(ctx, record, patch)).To(Succeed())
		Consistently(func(g Gomega) {
			current, err := cfAPI.GetDNSRecord(ctx, cf.ZoneIdentifier(zoneID), recordID)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(current.ModifiedOn).To(Equal(modifiedOn))
		}, 15*time.Second, 3*time.Second).Should(Succeed())
	})

	It("routes a pattern to the WorkerScript and excludes a sub-path", func() {
		if zoneName == "" {
			Skip("CF_E2E_ZONE is not set")
		}
		pattern := recordName() + "/*"
		route := &cloudflarev1alpha1.WorkerRoute{
			ObjectMeta: objectMeta(routeKey),
			Spec: cloudflarev1alpha1.WorkerRouteSpec{
				ZoneRef:         corev1.LocalObjectReference{Name: zoneKey.Name},
				Pattern:         pattern,
				WorkerScriptRef: &corev1.LocalObjectReference{Name: workerKey.Name},
			},
		}
		exclusion := &cloudflarev1alpha1.WorkerRoute{
			ObjectMeta: objectMeta(exclusionKey),
			Spec: cloudflarev1alpha1.WorkerRouteSpec{
				ZoneRef: corev1.LocalObjectReference{Name: zoneKey.Name},
				Pattern: recordName() + "/static/*",
			},
		}
		Expect(k8s.Create(ctx, route)).To(Succeed())
		Expect(k8s.Create(ctx, exclusion)).To(Succeed())
		expectReady(routeKey, route, func() []metav1.Condition { return route.Status.Conditions })
		expectReady(exclusionKey, exclusion, func() []metav1.Condition { return exclusion.Status.Conditions })
		routeID = route.Status.CloudflareMetadata.RouteID
		exclusionID = exclusion.Status.CloudflareMetadata.RouteID

		cfRoute, err := cfAPI.GetWorkerRoute(ctx, cf.ZoneIdentifier(zoneID), routeID)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfRoute.Pattern).To(Equal(pattern))
		Expect(cfRoute.ScriptName).To(Equal(resourceName()))
		cfExclusion, err := cfAPI.GetWorkerRoute(ctx, cf.ZoneIdentifier(zoneID), exclusionID)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfExclusion.ScriptName).To(BeEmpty())
	})

	It("removes everything from Cloudflare when everything is deleted at once", func() {

		// One delete for everything, in no particular order, like `kubectl delete
		// -f` on a directory: parents must wait for the children that need them
		// (Zone, Tunnel, CloudflareAccount and its token Secret).
		By("deleting every resource at once")
		everything := []client.Object{
			&cloudflarev1alpha1.WorkerRoute{ObjectMeta: objectMeta(routeKey)},
			&cloudflarev1alpha1.WorkerRoute{ObjectMeta: objectMeta(exclusionKey)},
			&cloudflarev1alpha1.DNSRecord{ObjectMeta: objectMeta(recordKey)},
			&cloudflarev1alpha1.TunnelConfiguration{ObjectMeta: objectMeta(tunnelConfigKey)},
			&cloudflarev1alpha1.WorkerScript{ObjectMeta: objectMeta(workerKey)},
			&cloudflarev1alpha1.KVNamespace{ObjectMeta: objectMeta(kvKey)},
			&cloudflarev1alpha1.Tunnel{ObjectMeta: objectMeta(tunnelKey)},
			&cloudflarev1alpha1.Zone{ObjectMeta: objectMeta(zoneKey)},
			&cloudflarev1alpha1.CloudflareAccount{ObjectMeta: objectMeta(accountKey)},
			&corev1.Secret{ObjectMeta: objectMeta(client.ObjectKey{Namespace: e2eNamespace, Name: "cloudflare-token"})},
		}
		for _, obj := range everything {
			Expect(client.IgnoreNotFound(k8s.Delete(ctx, obj))).To(Succeed())
		}
		for _, obj := range everything {
			expectGone(client.ObjectKeyFromObject(obj), obj)
		}

		By("checking Cloudflare")
		cfTunnel, err := cfAPI.GetTunnel(ctx, accountRC, tunnelID)
		if err == nil {
			Expect(cfTunnel.DeletedAt).NotTo(BeNil(), "tunnel was not deleted")
		} else {
			Expect(cfpkg.IsNotFound(err)).To(BeTrue(), "unexpected error: %v", err)
		}
		Expect(workerNames(ctx, cfAPI, accountRC)).NotTo(ContainElement(resourceName()))
		Expect(kvNamespaces(ctx, cfAPI, accountRC)).NotTo(HaveKey(kvNamespaceID))
		if zoneName != "" {
			_, err := cfAPI.GetDNSRecord(ctx, cf.ZoneIdentifier(zoneID), recordID)
			Expect(cfpkg.IsNotFound(err)).To(BeTrue(), "record was not deleted: %v", err)
			for _, id := range routeIDs(routeID, exclusionID) {
				_, err = cfAPI.GetWorkerRoute(ctx, cf.ZoneIdentifier(zoneID), id)
				Expect(cfpkg.IsNotFound(err)).To(BeTrue(), "route %s was not deleted: %v", id, err)
			}
			_, err = cfAPI.ZoneDetails(ctx, zoneID)
			Expect(err).NotTo(HaveOccurred(), "the retained zone must still exist")
		}
	})
})

// kvNamespaces maps the account's KV namespace IDs to their titles.
func kvNamespaces(ctx context.Context, api *cf.API, rc *cf.ResourceContainer) map[string]string {
	namespaces, _, err := api.ListWorkersKVNamespaces(ctx, rc, cf.ListWorkersKVNamespacesParams{})
	Expect(err).NotTo(HaveOccurred())
	out := make(map[string]string, len(namespaces))
	for _, ns := range namespaces {
		out[ns.ID] = ns.Title
	}
	return out
}

// workerNames lists the Worker script names in the account.
func workerNames(ctx context.Context, api *cf.API, rc *cf.ResourceContainer) []string {
	workers, _, err := api.ListWorkers(ctx, rc, cf.ListWorkersParams{})
	Expect(err).NotTo(HaveOccurred())
	names := make([]string, 0, len(workers.WorkerList))
	for _, w := range workers.WorkerList {
		names = append(names, w.ID)
	}
	return names
}

// cleanupCloudflare deletes the tunnel, Worker, DNS record and Worker routes
// this run may have created. It only reports problems: it runs after the specs, whether
// or not they passed.
func cleanupCloudflare(
	ctx context.Context,
	api *cf.API,
	rc *cf.ResourceContainer,
	name, zoneID, record string,
	routes []string,
) {
	report := func(what string, err error) {
		if err != nil && !cfpkg.IsNotFound(err) {
			GinkgoWriter.Printf("cleanup: %s: %v\n", what, err)
		}
	}

	tunnels, _, err := api.ListTunnels(ctx, rc, cf.TunnelListParams{Name: name, IsDeleted: cf.BoolPtr(false)})
	report("listing tunnels", err)
	for _, t := range tunnels {
		report("removing tunnel connections", api.CleanupTunnelConnections(ctx, rc, t.ID))
		report("deleting tunnel "+t.ID, api.DeleteTunnel(ctx, rc, t.ID))
	}

	report("deleting Worker", api.DeleteWorker(ctx, rc, cf.DeleteWorkerParams{ScriptName: name}))

	if zoneID != "" {
		zrc := cf.ZoneIdentifier(zoneID)
		records, _, err := api.ListDNSRecords(ctx, zrc, cf.ListDNSRecordsParams{Name: record})
		report("listing DNS records", err)
		for _, r := range records {
			report("deleting DNS record "+r.ID, api.DeleteDNSRecord(ctx, zrc, r.ID))
		}
		for _, id := range routes {
			_, err := api.DeleteWorkerRoute(ctx, zrc, id)
			report("deleting Worker route "+id, err)
		}
	}
}

// objectMeta names the object at key.
func objectMeta(key client.ObjectKey) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}
}

// routeIDs returns the non-empty route IDs among ids.
func routeIDs(ids ...string) []string {
	var out []string
	for _, id := range ids {
		if id != "" {
			out = append(out, id)
		}
	}
	return out
}
