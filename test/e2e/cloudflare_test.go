/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package e2e

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"slices"
	"strings"
	"time"

	cf "github.com/cloudflare/cloudflare-go"
	"github.com/cloudflare/cloudflare-go/v7/d1"
	"github.com/cloudflare/cloudflare-go/v7/rulesets"
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

	// customRulesPhase, rateLimitPhase and managedPhase are the Rulesets
	// phases of WAF custom rules, rate limiting rules and managed ruleset
	// deployments.
	customRulesPhase = "http_request_firewall_custom"
	rateLimitPhase   = "http_ratelimit"
	managedPhase     = "http_request_firewall_managed"

	// urlRewritePhase, requestHeadersPhase and responseHeadersPhase are the
	// Rulesets phases of TransformRules of type urlRewrite, requestHeaders and
	// responseHeaders.
	urlRewritePhase      = "http_request_transform"
	requestHeadersPhase  = "http_request_late_transform"
	responseHeadersPhase = "http_response_headers_transform"

	// freeManagedRulesetID is the Cloudflare Free Managed Ruleset, the
	// managed ruleset a Free zone can deploy.
	freeManagedRulesetID = "77454fe2d30c4220b5701f6fdfb893ba"

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
// by the specs, or by the cleanup below if a spec fails part-way. The rule
// specs need room in CF_E2E_ZONE: a Free zone allows one rate limiting rule,
// so the zone must hold no other, and two runs cannot share the zone at once.
// Likewise a Free account holds one IP list, so it must hold no other, and a
// zone deploys a managed ruleset at most once, so no rule in the zone may
// deploy the Free Managed Ruleset already.
var _ = Describe("Cloudflare", Ordered, Label("cloudflare"), func() {
	var (
		ctx       = context.Background()
		k8s       client.Client
		cfAPI     *cf.API
		accountRC *cf.ResourceContainer
		accountID string
		run       string
		zoneName  string
		// databases is cloudflare-go v7's D1 service: v0 does not report a
		// database's jurisdiction.
		databases *d1.DatabaseService
		// ruleService is cloudflare-go v7's ruleset rule service: v0's rule
		// delete fails on the API's answer.
		ruleService *rulesets.RuleService

		accountKey, tunnelKey, tunnelConfigKey, workerKey, zoneKey, recordKey client.ObjectKey
		routeKey, exclusionKey, kvKey, bucketKey, wafKey, wafSkipKey          client.ObjectKey
		rateLimitKey, listKey, listRuleKey, managedKey, d1Key                 client.ObjectKey
		urlRewriteKey, requestHeadersKey, responseHeadersKey                  client.ObjectKey

		tunnelID, zoneID, recordID, routeID, exclusionID, kvNamespaceID, listID, databaseID string
	)

	// resourceName is the name of every Cloudflare object this run creates.
	resourceName := func() string { return "kflare-e2e-" + run }
	recordName := func() string { return resourceName() + "." + zoneName }
	// listName is the name of the run's IP list: list names allow no hyphens.
	listName := func() string { return "kflare_e2e_" + run }

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
		token := os.Getenv("CF_API_TOKEN")
		accountID = os.Getenv("CF_ACCOUNT_ID")
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
		cfClient, err := cfpkg.New(token)
		Expect(err).NotTo(HaveOccurred())
		databases = d1.NewDatabaseService(cfClient.V7Options()...)
		ruleService = rulesets.NewRuleService(cfClient.V7Options()...)

		accountKey = client.ObjectKey{Name: resourceName()}
		tunnelKey = client.ObjectKey{Namespace: e2eNamespace, Name: "tunnel"}
		tunnelConfigKey = client.ObjectKey{Namespace: e2eNamespace, Name: "tunnel-config"}
		workerKey = client.ObjectKey{Namespace: e2eNamespace, Name: "worker"}
		zoneKey = client.ObjectKey{Namespace: e2eNamespace, Name: "zone"}
		recordKey = client.ObjectKey{Namespace: e2eNamespace, Name: "record"}
		routeKey = client.ObjectKey{Namespace: e2eNamespace, Name: "route"}
		exclusionKey = client.ObjectKey{Namespace: e2eNamespace, Name: "route-exclusion"}
		kvKey = client.ObjectKey{Namespace: e2eNamespace, Name: "kv"}
		bucketKey = client.ObjectKey{Namespace: e2eNamespace, Name: "bucket"}
		wafKey = client.ObjectKey{Namespace: e2eNamespace, Name: "waf-block"}
		wafSkipKey = client.ObjectKey{Namespace: e2eNamespace, Name: "waf-skip"}
		rateLimitKey = client.ObjectKey{Namespace: e2eNamespace, Name: "rate-limit"}
		listKey = client.ObjectKey{Namespace: e2eNamespace, Name: "ip-list"}
		listRuleKey = client.ObjectKey{Namespace: e2eNamespace, Name: "waf-list"}
		managedKey = client.ObjectKey{Namespace: e2eNamespace, Name: "managed-ruleset"}
		d1Key = client.ObjectKey{Namespace: e2eNamespace, Name: "d1"}
		urlRewriteKey = client.ObjectKey{Namespace: e2eNamespace, Name: "url-rewrite"}
		requestHeadersKey = client.ObjectKey{Namespace: e2eNamespace, Name: "request-headers"}
		responseHeadersKey = client.ObjectKey{Namespace: e2eNamespace, Name: "response-headers"}

		DeferCleanup(func() {
			By("removing anything this run left behind in Cloudflare")
			cleanupCloudflare(ctx, cfAPI, accountRC, resourceName(), zoneID, recordName(), routeIDs(routeID, exclusionID))
			if kvNamespaceID != "" {
				_, err := cfAPI.DeleteWorkersKVNamespace(ctx, accountRC, kvNamespaceID)
				if err != nil && !cfpkg.IsNotFound(err) {
					GinkgoWriter.Printf("cleanup: deleting KV namespace %s: %v\n", kvNamespaceID, err)
				}
			}
			if err := cfAPI.DeleteR2Bucket(ctx, accountRC, resourceName()); err != nil && !cfpkg.IsNotFound(err) {
				GinkgoWriter.Printf("cleanup: deleting R2 bucket %s: %v\n", resourceName(), err)
			}
			cleanupD1Database(ctx, cfAPI, accountRC, resourceName())
			if zoneID != "" {
				cleanupRules(ctx, cfAPI, ruleService, zoneID, customRulesPhase, ruleRef("WAFCustomRule", wafKey),
					ruleRef("WAFCustomRule", wafSkipKey), ruleRef("WAFCustomRule", listRuleKey))
				cleanupRules(ctx, cfAPI, ruleService, zoneID, rateLimitPhase, ruleRef("RateLimitRule", rateLimitKey))
				cleanupRules(ctx, cfAPI, ruleService, zoneID, managedPhase, ruleRef("ManagedRuleset", managedKey))
				cleanupRules(ctx, cfAPI, ruleService, zoneID, urlRewritePhase, ruleRef("TransformRule", urlRewriteKey))
				cleanupRules(ctx, cfAPI, ruleService, zoneID, requestHeadersPhase,
					ruleRef("TransformRule", requestHeadersKey))
				cleanupRules(ctx, cfAPI, ruleService, zoneID, responseHeadersPhase,
					ruleRef("TransformRule", responseHeadersKey))
				// An entry point in the managed phase, even an empty one,
				// stops Cloudflare's default deployment of the Free Managed
				// Ruleset; deleting it restores the zone as found.
				cleanupEmptyEntrypoint(ctx, cfAPI, zoneID, managedPhase)
			}
			// After the rules: Cloudflare does not delete a list a rule uses.
			cleanupList(ctx, cfAPI, accountRC, listName())
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

	It("creates a KVNamespace and renames it in place", func() {
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

		By("changing the title, which renames the namespace and keeps its ID")
		patch := client.MergeFrom(kv.DeepCopy())
		kv.Spec.Title = resourceName() + "-renamed"
		Expect(k8s.Patch(ctx, kv, patch)).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(k8s.Get(ctx, kvKey, kv)).To(Succeed())
			ready := meta.FindStatusCondition(kv.Status.Conditions, cloudflarev1alpha1.ConditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.ObservedGeneration).To(Equal(kv.Generation), "the rename is not reconciled yet")
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue), "%s: %s", ready.Reason, ready.Message)
		}, e2eTimeout, e2ePoll).Should(Succeed())
		Expect(kv.Status.CloudflareMetadata.NamespaceID).To(Equal(kvNamespaceID))
		Expect(kvNamespaces(ctx, cfAPI, accountRC)).To(HaveKeyWithValue(kvNamespaceID, resourceName()+"-renamed"))
	})

	It("creates an R2Bucket", func() {
		bucket := &cloudflarev1alpha1.R2Bucket{
			ObjectMeta: objectMeta(bucketKey),
			Spec: cloudflarev1alpha1.R2BucketSpec{
				AccountRef:   corev1.LocalObjectReference{Name: accountKey.Name},
				Name:         resourceName(),
				LocationHint: "weur",
			},
		}
		Expect(k8s.Create(ctx, bucket)).To(Succeed())
		expectReady(bucketKey, bucket, func() []metav1.Condition { return bucket.Status.Conditions })
		Expect(bucket.Status.CloudflareMetadata.Location).NotTo(BeEmpty())

		cfBucket, err := cfAPI.GetR2Bucket(ctx, accountRC, resourceName())
		Expect(err).NotTo(HaveOccurred())
		Expect(cfBucket.Location).To(Equal(bucket.Status.CloudflareMetadata.Location))
	})

	It("creates a D1Database in a jurisdiction, and adopts the database by name once it is retained", func() {
		newDatabase := func() *cloudflarev1alpha1.D1Database {
			return &cloudflarev1alpha1.D1Database{
				ObjectMeta: objectMeta(d1Key),
				Spec: cloudflarev1alpha1.D1DatabaseSpec{
					AccountRef:   corev1.LocalObjectReference{Name: accountKey.Name},
					Name:         resourceName(),
					Jurisdiction: "eu",
				},
			}
		}
		getDatabase := func(id string) (*d1.D1, error) {
			var params d1.DatabaseGetParams
			params.AccountID.Value, params.AccountID.Present = accountID, true
			return databases.Get(ctx, id, params)
		}

		db := newDatabase()
		Expect(k8s.Create(ctx, db)).To(Succeed())
		expectReady(d1Key, db, func() []metav1.Condition { return db.Status.Conditions })
		databaseID = db.Status.CloudflareMetadata.DatabaseID
		Expect(databaseID).NotTo(BeEmpty())
		Expect(db.Labels).To(HaveKeyWithValue(cloudflarev1alpha1.D1DatabaseIDLabel, databaseID))
		Expect(db.Status.CloudflareMetadata.Version).To(Equal("production"))

		cfDatabase, err := getDatabase(databaseID)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfDatabase.Name).To(Equal(resourceName()))
		Expect(string(cfDatabase.Jurisdiction)).To(Equal("eu"))

		By("deleting it with the retain policy, which keeps the database")
		patch := client.MergeFrom(db.DeepCopy())
		db.Annotations = map[string]string{reconciler.DeletionPolicyAnnotation: reconciler.DeletionPolicyRetain}
		Expect(k8s.Patch(ctx, db, patch)).To(Succeed())
		Expect(k8s.Delete(ctx, db)).To(Succeed())
		expectGone(d1Key, &cloudflarev1alpha1.D1Database{})
		_, err = getDatabase(databaseID)
		Expect(err).NotTo(HaveOccurred(), "the retained database was deleted")

		By("creating a D1Database with the same name, which adopts the database through the list")
		db = newDatabase()
		Expect(k8s.Create(ctx, db)).To(Succeed())
		expectReady(d1Key, db, func() []metav1.Condition { return db.Status.Conditions })
		Expect(db.Status.CloudflareMetadata.DatabaseID).To(Equal(databaseID), "a new database was created")
		Expect(d1DatabaseIDs(ctx, cfAPI, accountRC, resourceName())).To(ConsistOf(databaseID))
	})

	It("uploads a WorkerScript bound to the KVNamespace and R2Bucket", func() {
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
					{Name: "ASSETS", R2BucketRef: &corev1.LocalObjectReference{Name: bucketKey.Name}},
				},
			},
		}
		Expect(k8s.Create(ctx, worker)).To(Succeed())
		expectReady(workerKey, worker, func() []metav1.Condition { return worker.Status.Conditions })
		Expect(worker.Status.CloudflareMetadata.ModifiedOn).NotTo(BeEmpty())

		Expect(workerNames(ctx, cfAPI, accountRC)).To(ContainElement(resourceName()))
		bindings, err := cfAPI.ListWorkerBindings(ctx, accountRC, cf.ListWorkerBindingsParams{ScriptName: resourceName()})
		Expect(err).NotTo(HaveOccurred())
		Expect(bindings.BindingList).To(ContainElements(
			cf.WorkerBindingListItem{Name: "CACHE", Binding: cf.WorkerKvNamespaceBinding{NamespaceID: kvNamespaceID}},
			cf.WorkerBindingListItem{Name: "ASSETS", Binding: cf.WorkerR2BucketBinding{BucketName: resourceName()}},
		))
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

	It("writes WAF custom rules in priority order and leaves them alone once they are in sync", func() {
		if zoneName == "" {
			Skip("CF_E2E_ZONE is not set")
		}
		path := "/" + resourceName()
		block := &cloudflarev1alpha1.WAFCustomRule{
			ObjectMeta: objectMeta(wafKey),
			Spec: cloudflarev1alpha1.WAFCustomRuleSpec{
				RulesetRuleSpec: cloudflarev1alpha1.RulesetRuleSpec{
					ZoneRef:     corev1.LocalObjectReference{Name: zoneKey.Name},
					Expression:  `starts_with(http.request.uri.path, "` + path + `")`,
					Description: "kflare e2e " + run,
					Priority:    10,
				},
				Action: "block",
			},
		}
		Expect(k8s.Create(ctx, block)).To(Succeed())
		expectReady(wafKey, block, func() []metav1.Condition { return block.Status.Conditions })
		Expect(block.Status.CloudflareMetadata.Ref).To(Equal(ruleRef("WAFCustomRule", wafKey)))

		By("adding a skip rule with a lower priority, which kflare places before the block rule")
		skip := &cloudflarev1alpha1.WAFCustomRule{
			ObjectMeta: objectMeta(wafSkipKey),
			Spec: cloudflarev1alpha1.WAFCustomRuleSpec{
				RulesetRuleSpec: cloudflarev1alpha1.RulesetRuleSpec{
					ZoneRef:    corev1.LocalObjectReference{Name: zoneKey.Name},
					Expression: `http.request.uri.path eq "` + path + `/health"`,
				},
				Action: "skip",
				Skip:   &cloudflarev1alpha1.WAFCustomRuleSkip{RemainingRules: true},
			},
		}
		Expect(k8s.Create(ctx, skip)).To(Succeed())
		expectReady(wafSkipKey, skip, func() []metav1.Condition { return skip.Status.Conditions })

		rules := phaseRules(ctx, cfAPI, zoneID, customRulesPhase)
		refs := make([]string, len(rules))
		for i, rule := range rules {
			refs[i] = rule.Ref
		}
		blockRef, skipRef := ruleRef("WAFCustomRule", wafKey), ruleRef("WAFCustomRule", wafSkipKey)
		Expect(slices.Index(refs, skipRef)).To(BeNumerically(">=", 0), "the skip rule is missing: %v", refs)
		Expect(slices.Index(refs, skipRef)).To(BeNumerically("<", slices.Index(refs, blockRef)))
		blockRule := rules[slices.Index(refs, blockRef)]
		Expect(blockRule.ID).To(Equal(block.Status.CloudflareMetadata.RuleID))
		Expect(blockRule.Action).To(Equal("block"))
		Expect(blockRule.Expression).To(Equal(block.Spec.Expression))
		Expect(blockRule.Description).To(Equal(block.Spec.Description))

		By("re-triggering the reconcile and checking the ruleset is not written to again")
		expectIdle(ctx, k8s, cfAPI, zoneID, customRulesPhase, block)
	})

	It("writes a rate limiting rule, updates it in place and then leaves it alone", func() {
		if zoneName == "" {
			Skip("CF_E2E_ZONE is not set")
		}
		rule := &cloudflarev1alpha1.RateLimitRule{
			ObjectMeta: objectMeta(rateLimitKey),
			Spec: cloudflarev1alpha1.RateLimitRuleSpec{
				RulesetRuleSpec: cloudflarev1alpha1.RulesetRuleSpec{
					ZoneRef:     corev1.LocalObjectReference{Name: zoneKey.Name},
					Expression:  `starts_with(http.request.uri.path, "/` + resourceName() + `")`,
					Description: "kflare e2e " + run,
				},
				RequestsPerPeriod: 50,
				BlockResponse: &cloudflarev1alpha1.RateLimitRuleBlockResponse{
					ContentType: "application/json", Content: `{"error":"rate limited"}`,
				},
			},
		}
		Expect(k8s.Create(ctx, rule)).To(Succeed())
		expectReady(rateLimitKey, rule, func() []metav1.Condition { return rule.Status.Conditions })
		ref := ruleRef("RateLimitRule", rateLimitKey)
		Expect(rule.Status.CloudflareMetadata.Ref).To(Equal(ref))

		cfRule, ok := ruleByRef(phaseRules(ctx, cfAPI, zoneID, rateLimitPhase), ref)
		Expect(ok).To(BeTrue(), "Cloudflare has no rule with ref %s", ref)
		Expect(cfRule.ID).To(Equal(rule.Status.CloudflareMetadata.RuleID))
		Expect(cfRule.Action).To(Equal("block"))
		Expect(cfRule.Expression).To(Equal(rule.Spec.Expression))
		Expect(cfRule.Description).To(Equal(rule.Spec.Description))
		Expect(cfRule.RateLimit).To(Equal(&cf.RulesetRuleRateLimit{
			Characteristics: []string{"cf.colo.id", "ip.src"}, Period: 10, RequestsPerPeriod: 50, MitigationTimeout: 10,
		}))
		Expect(cfRule.ActionParameters).NotTo(BeNil())
		Expect(cfRule.ActionParameters.Response).To(Equal(&cf.RulesetRuleActionParametersBlockResponse{
			StatusCode: 429, ContentType: "application/json", Content: `{"error":"rate limited"}`,
		}))

		By("raising the limit, which updates the rule in place")
		patch := client.MergeFrom(rule.DeepCopy())
		rule.Spec.RequestsPerPeriod = 100
		Expect(k8s.Patch(ctx, rule, patch)).To(Succeed())
		Eventually(func(g Gomega) {
			updated, ok := ruleByRef(phaseRules(ctx, cfAPI, zoneID, rateLimitPhase), ref)
			g.Expect(ok).To(BeTrue(), "Cloudflare has no rule with ref %s", ref)
			g.Expect(updated.ID).To(Equal(cfRule.ID))
			g.Expect(updated.RateLimit.RequestsPerPeriod).To(Equal(100))
		}, e2eTimeout, e2ePoll).Should(Succeed())

		expectIdle(ctx, k8s, cfAPI, zoneID, rateLimitPhase, rule)
	})

	It("writes a transform rule of each type to its phase, updates one in place and then leaves them alone", func() {
		if zoneName == "" {
			Skip("CF_E2E_ZONE is not set")
		}
		path := "/" + resourceName()
		transformRule := func(key client.ObjectKey, typ string) *cloudflarev1alpha1.TransformRule {
			return &cloudflarev1alpha1.TransformRule{
				ObjectMeta: objectMeta(key),
				Spec: cloudflarev1alpha1.TransformRuleSpec{
					RulesetRuleSpec: cloudflarev1alpha1.RulesetRuleSpec{
						ZoneRef:     corev1.LocalObjectReference{Name: zoneKey.Name},
						Expression:  `starts_with(http.request.uri.path, "` + path + `")`,
						Description: "kflare e2e " + run,
					},
					Type: typ,
				},
			}
		}
		urlRewrite := transformRule(urlRewriteKey, "urlRewrite")
		urlRewrite.Spec.URI = &cloudflarev1alpha1.TransformRuleURI{
			Path:  &cloudflarev1alpha1.TransformRuleURIPath{Value: path + "/rewritten"},
			Query: &cloudflarev1alpha1.TransformRuleURIQuery{Value: new("")},
		}
		requestHeaders := transformRule(requestHeadersKey, "requestHeaders")
		requestHeaders.Spec.Headers = []cloudflarev1alpha1.TransformRuleHeader{
			{Name: "x-kflare-e2e", Operation: "set", Expression: "ip.src.country"},
			{Name: "x-kflare-e2e-removed", Operation: "remove"},
		}
		responseHeaders := transformRule(responseHeadersKey, "responseHeaders")
		responseHeaders.Spec.Headers = []cloudflarev1alpha1.TransformRuleHeader{
			{Name: "x-kflare-e2e", Operation: "add", Value: run},
			{Name: "x-frame-options", Operation: "set", Value: "DENY"},
		}
		for _, rule := range []*cloudflarev1alpha1.TransformRule{urlRewrite, requestHeaders, responseHeaders} {
			Expect(k8s.Create(ctx, rule)).To(Succeed())
		}
		phases := map[string]string{"urlRewrite": urlRewritePhase, "requestHeaders": requestHeadersPhase,
			"responseHeaders": responseHeadersPhase}
		cfRules := map[string]cf.RulesetRule{}
		for _, rule := range []*cloudflarev1alpha1.TransformRule{urlRewrite, requestHeaders, responseHeaders} {
			key := client.ObjectKeyFromObject(rule)
			expectReady(key, rule, func() []metav1.Condition { return rule.Status.Conditions })
			ref := ruleRef("TransformRule", key)
			Expect(rule.Status.CloudflareMetadata.Ref).To(Equal(ref))
			cfRule, ok := ruleByRef(phaseRules(ctx, cfAPI, zoneID, phases[rule.Spec.Type]), ref)
			Expect(ok).To(BeTrue(), "the %s phase has no rule with ref %s", phases[rule.Spec.Type], ref)
			Expect(cfRule.ID).To(Equal(rule.Status.CloudflareMetadata.RuleID))
			Expect(cfRule.Action).To(Equal("rewrite"))
			Expect(cfRule.Expression).To(Equal(rule.Spec.Expression))
			Expect(cfRule.Description).To(Equal(rule.Spec.Description))
			Expect(cfRule.ActionParameters).NotTo(BeNil())
			cfRules[rule.Spec.Type] = cfRule
		}
		Expect(cfRules["urlRewrite"].ActionParameters.URI).To(Equal(&cf.RulesetRuleActionParametersURI{
			Path:  &cf.RulesetRuleActionParametersURIPath{Value: path + "/rewritten"},
			Query: &cf.RulesetRuleActionParametersURIQuery{Value: new("")},
		}))
		type headers = map[string]cf.RulesetRuleActionParametersHTTPHeader
		Expect(cfRules["requestHeaders"].ActionParameters.Headers).To(Equal(headers{
			"x-kflare-e2e":         {Operation: "set", Expression: "ip.src.country"},
			"x-kflare-e2e-removed": {Operation: "remove"},
		}))
		Expect(cfRules["responseHeaders"].ActionParameters.Headers).To(Equal(headers{
			"x-kflare-e2e":    {Operation: "add", Value: run},
			"x-frame-options": {Operation: "set", Value: "DENY"},
		}))

		By("changing a response header's value, which updates the rule in place")
		patch := client.MergeFrom(responseHeaders.DeepCopy())
		responseHeaders.Spec.Headers[0].Value = run + "-changed"
		Expect(k8s.Patch(ctx, responseHeaders, patch)).To(Succeed())
		ref := ruleRef("TransformRule", responseHeadersKey)
		Eventually(func(g Gomega) {
			updated, ok := ruleByRef(phaseRules(ctx, cfAPI, zoneID, responseHeadersPhase), ref)
			g.Expect(ok).To(BeTrue(), "Cloudflare has no rule with ref %s", ref)
			g.Expect(updated.ID).To(Equal(cfRules["responseHeaders"].ID))
			g.Expect(updated.ActionParameters.Headers["x-kflare-e2e"].Value).To(Equal(run + "-changed"))
		}, e2eTimeout, e2ePoll).Should(Succeed())

		for _, rule := range []*cloudflarev1alpha1.TransformRule{urlRewrite, requestHeaders, responseHeaders} {
			expectIdle(ctx, k8s, cfAPI, zoneID, phases[rule.Spec.Type], rule)
		}
	})

	It("deploys the Free Managed Ruleset, updates its overrides in place and then leaves it alone", func() {
		if zoneName == "" {
			Skip("CF_E2E_ZONE is not set")
		}
		deployment := &cloudflarev1alpha1.ManagedRuleset{
			ObjectMeta: objectMeta(managedKey),
			Spec: cloudflarev1alpha1.ManagedRulesetSpec{
				RulesetRuleSpec: cloudflarev1alpha1.RulesetRuleSpec{
					ZoneRef:     corev1.LocalObjectReference{Name: zoneKey.Name},
					Expression:  "true",
					Description: "kflare e2e " + run,
				},
				RulesetID: freeManagedRulesetID,
				Overrides: &cloudflarev1alpha1.ManagedRulesetOverrides{
					Categories: []cloudflarev1alpha1.ManagedRulesetCategoryOverride{
						{Category: "wordpress", Action: "log"},
						{Category: "log4j", Action: "managed_challenge"},
					},
				},
			},
		}
		Expect(k8s.Create(ctx, deployment)).To(Succeed())
		expectReady(managedKey, deployment, func() []metav1.Condition { return deployment.Status.Conditions })
		ref := ruleRef("ManagedRuleset", managedKey)
		Expect(deployment.Status.CloudflareMetadata.Ref).To(Equal(ref))

		cfRule, ok := ruleByRef(phaseRules(ctx, cfAPI, zoneID, managedPhase), ref)
		Expect(ok).To(BeTrue(), "Cloudflare has no rule with ref %s", ref)
		Expect(cfRule.ID).To(Equal(deployment.Status.CloudflareMetadata.RuleID))
		Expect(cfRule.Action).To(Equal("execute"))
		Expect(cfRule.Expression).To(Equal("true"))
		Expect(cfRule.Description).To(Equal(deployment.Spec.Description))
		Expect(cfRule.ActionParameters).NotTo(BeNil())
		Expect(cfRule.ActionParameters.ID).To(Equal(freeManagedRulesetID))
		Expect(cfRule.ActionParameters.Version).To(Equal(new("latest")))
		Expect(cfRule.ActionParameters.Overrides).To(Equal(&cf.RulesetRuleActionParametersOverrides{
			Categories: []cf.RulesetRuleActionParametersCategories{
				{Category: "log4j", Action: "managed_challenge"},
				{Category: "wordpress", Action: "log"},
			},
		}))

		By("turning a rule off, which updates the deployment in place")
		patch := client.MergeFrom(deployment.DeepCopy())
		deployment.Spec.Overrides.Rules = []cloudflarev1alpha1.ManagedRulesetRuleOverride{
			{ID: "9f7e454e4cad45abbcc40696037f70df", Enabled: new(false)}, // Shellshock
		}
		Expect(k8s.Patch(ctx, deployment, patch)).To(Succeed())
		Eventually(func(g Gomega) {
			updated, ok := ruleByRef(phaseRules(ctx, cfAPI, zoneID, managedPhase), ref)
			g.Expect(ok).To(BeTrue(), "Cloudflare has no rule with ref %s", ref)
			g.Expect(updated.ID).To(Equal(cfRule.ID))
			g.Expect(updated.ActionParameters.Overrides.Rules).To(Equal([]cf.RulesetRuleActionParametersRules{
				{ID: "9f7e454e4cad45abbcc40696037f70df", Enabled: new(false)},
			}))
		}, e2eTimeout, e2ePoll).Should(Succeed())

		expectIdle(ctx, k8s, cfAPI, zoneID, managedPhase, deployment)
	})

	It("manages an IP list, and writes a rule that uses it once the list exists", func() {
		var rule *cloudflarev1alpha1.WAFCustomRule
		if zoneName != "" {
			By("creating a WAF custom rule that uses the list, which Cloudflare refuses while the list does not exist")
			rule = &cloudflarev1alpha1.WAFCustomRule{
				ObjectMeta: objectMeta(listRuleKey),
				Spec: cloudflarev1alpha1.WAFCustomRuleSpec{
					RulesetRuleSpec: cloudflarev1alpha1.RulesetRuleSpec{
						ZoneRef: corev1.LocalObjectReference{Name: zoneKey.Name},
						Expression: `ip.src in $` + listName() +
							` and starts_with(http.request.uri.path, "/` + resourceName() + `/office")`,
						Description: "kflare e2e " + run,
					},
					Action: "block",
				},
			}
			Expect(k8s.Create(ctx, rule)).To(Succeed())
			Eventually(func(g Gomega) {
				g.Expect(k8s.Get(ctx, listRuleKey, rule)).To(Succeed())
				ready := meta.FindStatusCondition(rule.Status.Conditions, cloudflarev1alpha1.ConditionReady)
				g.Expect(ready).NotTo(BeNil(), "no Ready condition yet")
				g.Expect(ready.Reason).To(Equal("TerminalError"))
				g.Expect(ready.Message).To(ContainSubstring("is unknown"))
			}, e2eTimeout, e2ePoll).Should(Succeed())
		}

		list := &cloudflarev1alpha1.IPList{
			ObjectMeta: objectMeta(listKey),
			Spec: cloudflarev1alpha1.IPListSpec{
				AccountRef:  corev1.LocalObjectReference{Name: accountKey.Name},
				Name:        listName(),
				Description: "kflare e2e " + run,
				Items: []cloudflarev1alpha1.IPListItem{
					{IP: "192.0.2.1", Comment: "documentation address"},
					{IP: "198.51.100.0/24"},
					{IP: "2001:DB8::/48"},
				},
			},
		}
		Expect(k8s.Create(ctx, list)).To(Succeed())
		expectReady(listKey, list, func() []metav1.Condition { return list.Status.Conditions })
		listID = list.Status.CloudflareMetadata.ListID
		Expect(list.Labels).To(HaveKeyWithValue(cloudflarev1alpha1.IPListIDLabel, listID))
		Expect(list.Status.CloudflareMetadata.NumItems).To(BeEquivalentTo(3))
		cfList, err := cfAPI.GetList(ctx, accountRC, listID)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfList.Name).To(Equal(listName()))
		Expect(cfList.Kind).To(Equal(cf.ListTypeIP))
		Expect(cfList.Description).To(Equal(list.Spec.Description))
		Expect(listItems(ctx, cfAPI, accountRC, listID)).To(
			ConsistOf("192.0.2.1 documentation address", "198.51.100.0/24", "2001:db8::/48"))

		if rule != nil {
			By("writing the rule once the list exists")
			expectReady(listRuleKey, rule, func() []metav1.Condition { return rule.Status.Conditions })
			cfRule, ok := ruleByRef(phaseRules(ctx, cfAPI, zoneID, customRulesPhase), ruleRef("WAFCustomRule", listRuleKey))
			Expect(ok).To(BeTrue(), "the rule that uses the list was not written")
			Expect(cfRule.Expression).To(Equal(rule.Spec.Expression))
		}

		By("changing the items, which Cloudflare replaces in the same list")
		patch := client.MergeFrom(list.DeepCopy())
		list.Spec.Items = []cloudflarev1alpha1.IPListItem{
			{IP: "192.0.2.1", Comment: "front door"},
			{IP: "203.0.113.7/32"},
		}
		Expect(k8s.Patch(ctx, list, patch)).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(k8s.Get(ctx, listKey, list)).To(Succeed())
			ready := meta.FindStatusCondition(list.Status.Conditions, cloudflarev1alpha1.ConditionReady)
			g.Expect(ready).NotTo(BeNil(), "no Ready condition yet")
			g.Expect(ready.ObservedGeneration).To(Equal(list.Generation))
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue), "%s: %s", ready.Reason, ready.Message)
		}, e2eTimeout, e2ePoll).Should(Succeed())
		Expect(list.Status.CloudflareMetadata.ListID).To(Equal(listID))
		Expect(listItems(ctx, cfAPI, accountRC, listID)).To(ConsistOf("192.0.2.1 front door", "203.0.113.7"))

		By("re-triggering the reconcile and checking the list is not written to again")
		modified := func() *time.Time {
			current, err := cfAPI.GetList(ctx, accountRC, listID)
			Expect(err).NotTo(HaveOccurred())
			return current.ModifiedOn
		}
		before := modified()
		patch = client.MergeFrom(list.DeepCopy())
		list.Annotations = map[string]string{"e2e.kflare.dev/touched": time.Now().Format(time.RFC3339)}
		Expect(k8s.Patch(ctx, list, patch)).To(Succeed())
		Consistently(modified, 15*time.Second, 3*time.Second).Should(Equal(before))
	})

	It("removes everything from Cloudflare when everything is deleted at once", func() {

		// One delete for everything, in no particular order, like `kubectl delete
		// -f` on a directory: parents must wait for the children that need them
		// (Zone, Tunnel, KVNamespace, R2Bucket, D1Database, CloudflareAccount and its token Secret),
		// and Cloudflare deletes the IP list only once the rule using it is gone.
		By("deleting every resource at once")
		everything := []client.Object{
			&cloudflarev1alpha1.WorkerRoute{ObjectMeta: objectMeta(routeKey)},
			&cloudflarev1alpha1.WorkerRoute{ObjectMeta: objectMeta(exclusionKey)},
			&cloudflarev1alpha1.WAFCustomRule{ObjectMeta: objectMeta(wafKey)},
			&cloudflarev1alpha1.WAFCustomRule{ObjectMeta: objectMeta(wafSkipKey)},
			&cloudflarev1alpha1.RateLimitRule{ObjectMeta: objectMeta(rateLimitKey)},
			&cloudflarev1alpha1.ManagedRuleset{ObjectMeta: objectMeta(managedKey)},
			&cloudflarev1alpha1.TransformRule{ObjectMeta: objectMeta(urlRewriteKey)},
			&cloudflarev1alpha1.TransformRule{ObjectMeta: objectMeta(requestHeadersKey)},
			&cloudflarev1alpha1.TransformRule{ObjectMeta: objectMeta(responseHeadersKey)},
			&cloudflarev1alpha1.WAFCustomRule{ObjectMeta: objectMeta(listRuleKey)},
			&cloudflarev1alpha1.IPList{ObjectMeta: objectMeta(listKey)},
			&cloudflarev1alpha1.DNSRecord{ObjectMeta: objectMeta(recordKey)},
			&cloudflarev1alpha1.TunnelConfiguration{ObjectMeta: objectMeta(tunnelConfigKey)},
			&cloudflarev1alpha1.WorkerScript{ObjectMeta: objectMeta(workerKey)},
			&cloudflarev1alpha1.KVNamespace{ObjectMeta: objectMeta(kvKey)},
			&cloudflarev1alpha1.R2Bucket{ObjectMeta: objectMeta(bucketKey)},
			&cloudflarev1alpha1.D1Database{ObjectMeta: objectMeta(d1Key)},
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
		_, err = cfAPI.GetR2Bucket(ctx, accountRC, resourceName())
		Expect(cfpkg.IsNotFound(err)).To(BeTrue(), "bucket was not deleted: %v", err)
		Expect(d1DatabaseIDs(ctx, cfAPI, accountRC, resourceName())).To(BeEmpty(), "the D1 database was not deleted")
		if listID != "" {
			_, err = cfAPI.GetList(ctx, accountRC, listID)
			Expect(cfpkg.IsNotFound(err)).To(BeTrue(), "IP list was not deleted: %v", err)
		}
		if zoneName != "" {
			_, err := cfAPI.GetDNSRecord(ctx, cf.ZoneIdentifier(zoneID), recordID)
			Expect(cfpkg.IsNotFound(err)).To(BeTrue(), "record was not deleted: %v", err)
			for _, id := range routeIDs(routeID, exclusionID) {
				_, err = cfAPI.GetWorkerRoute(ctx, cf.ZoneIdentifier(zoneID), id)
				Expect(cfpkg.IsNotFound(err)).To(BeTrue(), "route %s was not deleted: %v", id, err)
			}
			for _, rule := range phaseRules(ctx, cfAPI, zoneID, customRulesPhase) {
				Expect(rule.Ref).NotTo(BeElementOf(ruleRef("WAFCustomRule", wafKey), ruleRef("WAFCustomRule", wafSkipKey),
					ruleRef("WAFCustomRule", listRuleKey)), "rule %s was not deleted", rule.ID)
			}
			_, ok := ruleByRef(phaseRules(ctx, cfAPI, zoneID, rateLimitPhase), ruleRef("RateLimitRule", rateLimitKey))
			Expect(ok).To(BeFalse(), "the rate limiting rule was not deleted")
			for phase, key := range map[string]client.ObjectKey{urlRewritePhase: urlRewriteKey,
				requestHeadersPhase: requestHeadersKey, responseHeadersPhase: responseHeadersKey} {
				_, ok := ruleByRef(phaseRules(ctx, cfAPI, zoneID, phase), ruleRef("TransformRule", key))
				Expect(ok).To(BeFalse(), "the transform rule in %s was not deleted", phase)
			}
			_, err = cfAPI.GetEntrypointRuleset(ctx, cf.ZoneIdentifier(zoneID), managedPhase)
			Expect(cfpkg.IsNotFound(err)).To(BeTrue(),
				"the managed phase's emptied entry point was not deleted, which leaves Cloudflare's default off: %v", err)
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

// d1DatabaseIDs returns the IDs of the account's D1 databases named name.
func d1DatabaseIDs(ctx context.Context, api *cf.API, rc *cf.ResourceContainer, name string) []string {
	databases, _, err := api.ListD1Databases(ctx, rc, cf.ListD1DatabasesParams{Name: name})
	Expect(err).NotTo(HaveOccurred())
	var ids []string
	for _, db := range databases {
		// The name filter matches part of a name: compare it in full.
		if db.Name == name {
			ids = append(ids, db.UUID)
		}
	}
	return ids
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

	tunnels, _, err := api.ListTunnels(ctx, rc, cf.TunnelListParams{Name: name, IsDeleted: new(false)})
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

// ruleRef is the ref of the rule the resource of kind at key manages:
// "kflare-" and the first 32 hex characters of the SHA-256 of
// kind/namespace/name, as ARCHITECTURE.md documents it. It is computed here
// independently, to check the controller keeps to it.
func ruleRef(kind string, key client.ObjectKey) string {
	sum := sha256.Sum256([]byte(kind + "/" + key.Namespace + "/" + key.Name))
	return "kflare-" + hex.EncodeToString(sum[:16])
}

// ruleByRef returns the rule with ref among rules.
func ruleByRef(rules []cf.RulesetRule, ref string) (cf.RulesetRule, bool) {
	i := slices.IndexFunc(rules, func(rule cf.RulesetRule) bool { return rule.Ref == ref })
	if i < 0 {
		return cf.RulesetRule{}, false
	}
	return rules[i], true
}

// phaseRules returns the zone's rules in phase, in order, or none when the
// zone has no entry point in the phase.
func phaseRules(ctx context.Context, api *cf.API, zoneID, phase string) []cf.RulesetRule {
	ruleset, err := api.GetEntrypointRuleset(ctx, cf.ZoneIdentifier(zoneID), phase)
	if cfpkg.IsNotFound(err) {
		return nil
	}
	Expect(err).NotTo(HaveOccurred())
	return ruleset.Rules
}

// phaseVersion returns the version of the zone's entry point in phase, which
// Cloudflare bumps on every write.
func phaseVersion(ctx context.Context, api *cf.API, zoneID, phase string) string {
	ruleset, err := api.GetEntrypointRuleset(ctx, cf.ZoneIdentifier(zoneID), phase)
	Expect(err).NotTo(HaveOccurred())
	Expect(ruleset.Version).NotTo(BeNil())
	return *ruleset.Version
}

// expectIdle re-triggers the reconcile of rule, a resource whose rule is in
// sync, and checks that its phase's entry point is not written to again.
func expectIdle(ctx context.Context, k8s client.Client, api *cf.API, zoneID, phase string, rule client.Object) {
	By("re-triggering the reconcile and checking the ruleset is not written to again")
	version := phaseVersion(ctx, api, zoneID, phase)
	patch := client.MergeFrom(rule.DeepCopyObject().(client.Object))
	rule.SetAnnotations(map[string]string{"e2e.kflare.dev/touched": time.Now().Format(time.RFC3339)})
	Expect(k8s.Patch(ctx, rule, patch)).To(Succeed())
	Consistently(func() string {
		return phaseVersion(ctx, api, zoneID, phase)
	}, 15*time.Second, 3*time.Second).Should(Equal(version))
}

// listItems returns the items of the account's list with listID, as "ip
// comment" strings.
func listItems(ctx context.Context, api *cf.API, rc *cf.ResourceContainer, listID string) []string {
	items, err := api.ListListItems(ctx, rc, cf.ListListItemsParams{ID: listID, PerPage: 500})
	Expect(err).NotTo(HaveOccurred())
	out := make([]string, 0, len(items))
	for _, item := range items {
		Expect(item.IP).NotTo(BeNil(), "item %s is not an address", item.ID)
		out = append(out, strings.TrimSpace(*item.IP+" "+item.Comment))
	}
	return out
}

// cleanupList deletes the account's list named name, if there is one. It
// only reports problems: it runs after the specs, whether or not they passed.
func cleanupList(ctx context.Context, api *cf.API, rc *cf.ResourceContainer, name string) {
	lists, err := api.ListLists(ctx, rc, cf.ListListsParams{})
	if err != nil {
		GinkgoWriter.Printf("cleanup: listing IP lists: %v\n", err)
		return
	}
	for _, list := range lists {
		if list.Name != name {
			continue
		}
		if _, err := api.DeleteList(ctx, rc, list.ID); err != nil {
			GinkgoWriter.Printf("cleanup: deleting IP list %s: %v\n", list.ID, err)
		}
	}
}

// cleanupD1Database deletes the account's D1 databases named name. It only
// reports problems: it runs after the specs, whether or not they passed.
func cleanupD1Database(ctx context.Context, api *cf.API, rc *cf.ResourceContainer, name string) {
	databases, _, err := api.ListD1Databases(ctx, rc, cf.ListD1DatabasesParams{Name: name})
	if err != nil {
		GinkgoWriter.Printf("cleanup: listing D1 databases: %v\n", err)
		return
	}
	for _, db := range databases {
		if db.Name != name {
			continue
		}
		if err := api.DeleteD1Database(ctx, rc, db.UUID); err != nil && !cfpkg.IsNotFound(err) {
			GinkgoWriter.Printf("cleanup: deleting D1 database %s: %v\n", db.UUID, err)
		}
	}
}

// cleanupEmptyEntrypoint deletes the zone's entry point in phase if it holds
// no rules. It only reports problems: it runs after the specs, whether or not
// they passed.
func cleanupEmptyEntrypoint(ctx context.Context, api *cf.API, zoneID, phase string) {
	zrc := cf.ZoneIdentifier(zoneID)
	ruleset, err := api.GetEntrypointRuleset(ctx, zrc, phase)
	if err != nil {
		if !cfpkg.IsNotFound(err) {
			GinkgoWriter.Printf("cleanup: reading the %s entry point: %v\n", phase, err)
		}
		return
	}
	if len(ruleset.Rules) > 0 {
		return
	}
	if err := api.DeleteRuleset(ctx, zrc, ruleset.ID); err != nil && !cfpkg.IsNotFound(err) {
		GinkgoWriter.Printf("cleanup: deleting the empty %s entry point: %v\n", phase, err)
	}
}

// cleanupRules deletes the rules with refs from the zone's entry point in
// phase. It deletes through cloudflare-go v7, as kflare does: v0's
// DeleteRulesetRule reports an error even when the delete succeeds.
func cleanupRules(
	ctx context.Context, api *cf.API, ruleService *rulesets.RuleService, zoneID, phase string, refs ...string,
) {
	zrc := cf.ZoneIdentifier(zoneID)
	ruleset, err := api.GetEntrypointRuleset(ctx, zrc, phase)
	if err != nil {
		if !cfpkg.IsNotFound(err) {
			GinkgoWriter.Printf("cleanup: reading the %s rules: %v\n", phase, err)
		}
		return
	}
	for _, rule := range ruleset.Rules {
		if !slices.Contains(refs, rule.Ref) {
			continue
		}
		var params rulesets.RuleDeleteParams
		params.ZoneID.Value, params.ZoneID.Present = zoneID, true
		if _, err := ruleService.Delete(ctx, ruleset.ID, rule.ID, params); err != nil && !cfpkg.IsNotFound(err) {
			GinkgoWriter.Printf("cleanup: deleting %s rule %s: %v\n", phase, rule.ID, err)
		}
	}
}
