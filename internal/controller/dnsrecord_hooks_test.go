/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"encoding/json"

	"github.com/cloudflare/cloudflare-go/v7/dns"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

// fakeDNSRecordWrite is what a body sets that cloudflare-go v7 types by the
// record's type: nil where it sets nothing.
type fakeDNSRecordWrite struct {
	ttl        *dns.TTL
	proxied    *bool
	priority   *float64
	tags, data any
}

// fakeDNSRecordStored writes what a body holds to the record, as Cloudflare
// would send it: kflare reads its tags and data from the JSON of the response.
func fakeDNSRecordStored(object *dns.RecordResponse, write fakeDNSRecordWrite) {
	stored := map[string]any{}
	if raw := object.JSON.RawJSON(); raw != "" {
		ExpectWithOffset(1, json.Unmarshal([]byte(raw), &stored)).To(Succeed())
	}
	stored["id"], stored["name"], stored["type"] = object.ID, object.Name, object.Type
	stored["content"], stored["comment"] = object.Content, object.Comment
	if write.tags != nil {
		stored["tags"] = write.tags
	}
	switch {
	case write.data != nil:
		stored["data"] = write.data
	case object.Content != "":
		delete(stored, "data")
	}
	encoded, err := json.Marshal(stored)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	current := *object
	ExpectWithOffset(1, object.UnmarshalJSON(encoded)).To(Succeed())
	object.Proxiable, object.TTL, object.Proxied, object.Priority = current.Proxiable, current.TTL, current.Proxied,
		current.Priority
	if write.ttl != nil {
		object.TTL = *write.ttl
	}
	if write.proxied != nil {
		object.Proxied = *write.proxied
	}
	if write.priority != nil {
		object.Priority = *write.priority
	}
}

// fakeDNSRecordSeed gives a seeded record what Cloudflare fills in: an
// automatic TTL.
func fakeDNSRecordSeed(object *dns.RecordResponse) {
	object.TTL = automaticTTL
}

func fakeDNSRecordNewBody(object *dns.RecordResponse, body dns.RecordNewParamsBody) {
	write := fakeDNSRecordWrite{tags: body.Tags.Value, data: body.Data.Value}
	if body.TTL.Present {
		write.ttl = &body.TTL.Value
	}
	if body.Proxied.Present {
		write.proxied = &body.Proxied.Value
	}
	if body.Priority.Present {
		write.priority = &body.Priority.Value
	}
	fakeDNSRecordStored(object, write)
}

func fakeDNSRecordEditBody(object *dns.RecordResponse, body dns.RecordEditParamsBody) {
	write := fakeDNSRecordWrite{tags: body.Tags.Value, data: body.Data.Value}
	if body.TTL.Present {
		write.ttl = &body.TTL.Value
	}
	if body.Proxied.Present {
		write.proxied = &body.Proxied.Value
	}
	if body.Priority.Present {
		write.priority = &body.Priority.Value
	}
	fakeDNSRecordStored(object, write)
}

var _ = Describe("DNSRecord hooks", func() {
	const (
		ns          = "default"
		accountName = "dns-hooks-account"
		secretName  = "dns-hooks-token"
		zoneName    = "dns-hooks-zone"
		zoneID      = "dns-hooks-zone-id"
		tunnelName  = "dns-hooks-tunnel"
	)
	ctx := context.Background()
	key := types.NamespacedName{Name: "www", Namespace: ns}
	var store *fakeCRUDStore[dns.RecordResponse]
	var r *DNSRecordReconciler

	create := func(obj client.Object) {
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		DeferCleanup(forceDelete, ctx, obj.DeepCopyObject().(client.Object), client.ObjectKeyFromObject(obj))
	}
	setReady := func(obj conditionedObject) {
		obj.SetConditions([]metav1.Condition{{Type: cloudflarev1alpha1.ConditionReady, Status: metav1.ConditionTrue,
			Reason: "Synced", LastTransitionTime: metav1.Now()}})
		Expect(k8sClient.Status().Update(ctx, obj)).To(Succeed())
	}
	createRecord := func(at types.NamespacedName, change func(*cloudflarev1alpha1.DNSRecord)) {
		record := &cloudflarev1alpha1.DNSRecord{
			ObjectMeta: metav1.ObjectMeta{Name: at.Name, Namespace: at.Namespace, Finalizers: []string{reconciler.Finalizer}},
			Spec: cloudflarev1alpha1.DNSRecordSpec{
				ZoneRef: corev1.LocalObjectReference{Name: zoneName}, Name: "www.example.com", Type: "A",
				Content: "192.0.2.1",
			},
		}
		if change != nil {
			change(record)
		}
		create(record)
	}
	adoptOrCreate := func(record *cloudflarev1alpha1.DNSRecord) {
		record.Annotations = map[string]string{reconciler.AdoptionPolicyAnnotation: reconciler.AdoptionPolicyAdoptOrCreate}
	}
	getRecord := func() *cloudflarev1alpha1.DNSRecord {
		record := &cloudflarev1alpha1.DNSRecord{}
		Expect(k8sClient.Get(ctx, key, record)).To(Succeed())
		return record
	}
	condition := func() metav1.Condition {
		cond := meta.FindStatusCondition(getRecord().Status.Conditions, cloudflarev1alpha1.ConditionReady)
		Expect(cond).NotTo(BeNil(), "no Ready condition")
		return *cond
	}
	reconcileRecord := func() (ctrl.Result, error) {
		return r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	}
	sync := func() {
		_, err := reconcileRecord()
		Expect(err).NotTo(HaveOccurred())
		Expect(condition().Reason).To(Equal("Synced"))
	}
	// update changes the DNSRecord's spec.
	update := func(change func(*cloudflarev1alpha1.DNSRecord)) {
		record := getRecord()
		change(record)
		Expect(k8sClient.Update(ctx, record)).To(Succeed())
	}
	// seed stores a record of Cloudflare's own, as it would send it.
	seed := func(id, content string, change func(map[string]any)) {
		raw := map[string]any{"id": id, "name": "www.example.com", "type": "A", "content": content, "ttl": 1}
		if change != nil {
			change(raw)
		}
		encoded, err := json.Marshal(raw)
		Expect(err).NotTo(HaveOccurred())
		var object dns.RecordResponse
		Expect(object.UnmarshalJSON(encoded)).To(Succeed())
		store.objects[id] = object
	}
	current := func() dns.RecordResponse { return store.objects[getRecord().Status.CloudflareMetadata.RecordID] }
	sentBody := func() dns.RecordNewParamsBody {
		return store.sent[len(store.sent)-1].(dns.RecordNewParams).Body.(dns.RecordNewParamsBody)
	}

	BeforeEach(func() {
		store = newFakeCRUDStore[dns.RecordResponse](dnsRecordListPageSize, "")
		r = &DNSRecordReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(),
			NewDNSRecordAPI: func(string) (DNSRecordAPI, error) { return fakeDNSRecordAPI{store}, nil }}
		create(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns},
			Data:       map[string][]byte{"CF_API_TOKEN": []byte("token")},
		})
		account := &cloudflarev1alpha1.CloudflareAccount{
			ObjectMeta: metav1.ObjectMeta{Name: accountName},
			Spec: cloudflarev1alpha1.CloudflareAccountSpec{
				AccountID:      "account-id",
				TokenSecretRef: cloudflarev1alpha1.SecretReference{Name: secretName, Namespace: ns, Key: "CF_API_TOKEN"},
			},
		}
		create(account)
		setReady(account)
		zone := &cloudflarev1alpha1.Zone{
			ObjectMeta: metav1.ObjectMeta{Name: zoneName, Namespace: ns},
			Spec: cloudflarev1alpha1.ZoneSpec{
				AccountRef: corev1.LocalObjectReference{Name: accountName},
				ZoneSpecExtras: cloudflarev1alpha1.ZoneSpecExtras{
					Name: "example.com",
				},
			},
		}
		create(zone)
		zone.Status.CloudflareMetadata.ZoneID = zoneID
		setReady(zone)
	})

	Describe("what it writes", func() {
		It("sends an automatic ttl for an unset one, and only the other values the spec sets", func() {
			createRecord(key, nil)
			sync()
			body := sentBody()
			Expect(float64(body.TTL.Value)).To(Equal(float64(automaticTTL)))
			Expect(body.Proxied.Present).To(BeFalse())
			Expect(body.Priority.Present).To(BeFalse())
			Expect(body.Tags.Present).To(BeFalse())
			Expect(body.Data.Present).To(BeFalse())
		})

		It("sends the values the spec sets", func() {
			createRecord(key, func(record *cloudflarev1alpha1.DNSRecord) {
				record.Spec.Type, record.Spec.Content, record.Spec.TTL = "MX", "mail.example.com", 300
				record.Spec.Proxied, record.Spec.Priority = new(false), new(uint16(10))
				record.Spec.Tags = []string{"a", "b"}
			})
			sync()
			body := sentBody()
			Expect(float64(body.TTL.Value)).To(Equal(300.0))
			Expect(body.Proxied.Present).To(BeTrue())
			Expect(body.Proxied.Value).To(BeFalse())
			Expect(body.Priority.Value).To(Equal(10.0))
			Expect(body.Tags.Value).To(Equal([]string{"a", "b"}))
			Expect(store.writes).To(Equal(1), "the new record is what the spec asks for")
		})

		It("sends data, which a record of its type holds instead of content", func() {
			createRecord(key, func(record *cloudflarev1alpha1.DNSRecord) {
				record.Spec.Type, record.Spec.Content = "SRV", ""
				record.Spec.Data = &apiextensionsv1.JSON{Raw: []byte(`{"port":443,"target":"sip.example.com"}`)}
			})
			sync()
			Expect(sentBody().Data.Value).To(Equal(map[string]any{"port": 443.0, "target": "sip.example.com"}))
			Expect(store.writes).To(Equal(1))
		})
	})

	Describe("drift", func() {
		// expectCorrected changes Cloudflare's copy of a synced record, and
		// expects the next reconcile to write the record back.
		expectCorrected := func(setup func(*cloudflarev1alpha1.DNSRecord), drift func(*dns.RecordResponse)) {
			createRecord(key, setup)
			sync()
			id := getRecord().Status.CloudflareMetadata.RecordID
			object := store.objects[id]
			drift(&object)
			store.objects[id] = object
			writes := store.writes
			sync()
			Expect(store.writes).To(Equal(writes+1), "the record is written back")
			writes = store.writes
			sync()
			Expect(store.writes).To(Equal(writes), "and then left alone")
		}

		It("writes nothing for a record that leaves ttl and proxied unset", func() {
			createRecord(key, nil)
			sync()
			sync()
			Expect(store.writes).To(Equal(1))
		})

		It("leaves a priority the spec does not set", func() {
			createRecord(key, nil)
			sync()
			id := getRecord().Status.CloudflareMetadata.RecordID
			object := store.objects[id]
			object.Priority = 20
			store.objects[id] = object
			sync()
			Expect(store.writes).To(Equal(1))
		})

		It("corrects the content", func() {
			expectCorrected(nil, func(o *dns.RecordResponse) { o.Content = "203.0.113.1" })
			Expect(current().Content).To(Equal("192.0.2.1"))
		})

		It("corrects the ttl, an unset one to automatic", func() {
			expectCorrected(nil, func(o *dns.RecordResponse) { o.TTL = 300 })
			Expect(float64(current().TTL)).To(Equal(float64(automaticTTL)))
		})

		It("turns proxying off when the spec leaves it unset", func() {
			expectCorrected(nil, func(o *dns.RecordResponse) { o.Proxied = true })
			Expect(current().Proxied).To(BeFalse())
		})

		It("corrects a priority the spec sets", func() {
			expectCorrected(func(record *cloudflarev1alpha1.DNSRecord) {
				record.Spec.Type, record.Spec.Priority = "MX", new(uint16(10))
			}, func(o *dns.RecordResponse) { o.Priority = 20 })
			Expect(current().Priority).To(Equal(10.0))
		})

		It("clears a comment the spec leaves unset", func() {
			expectCorrected(nil, func(o *dns.RecordResponse) { o.Comment = "added by hand" })
			Expect(current().Comment).To(BeEmpty())
		})

		It("compares tags in any order, and clears those the spec drops", func() {
			createRecord(key, func(record *cloudflarev1alpha1.DNSRecord) { record.Spec.Tags = []string{"a", "b"} })
			sync()
			update(func(record *cloudflarev1alpha1.DNSRecord) { record.Spec.Tags = []string{"b", "a"} })
			sync()
			Expect(store.writes).To(Equal(1), "the same tags, in another order")
			update(func(record *cloudflarev1alpha1.DNSRecord) { record.Spec.Tags = nil })
			sync()
			Expect(store.writes).To(Equal(2))
			Expect(readDNSRecord(new(current())).Tags).To(BeEmpty())
		})

		It("corrects data, and does not compare the content Cloudflare derives from it", func() {
			createRecord(key, func(record *cloudflarev1alpha1.DNSRecord) {
				record.Spec.Type, record.Spec.Content = "SRV", ""
				record.Spec.Data = &apiextensionsv1.JSON{Raw: []byte(`{"port":443}`)}
			})
			sync()
			id := getRecord().Status.CloudflareMetadata.RecordID
			object := store.objects[id]
			object.Content = "0 443 derived.example.com"
			store.objects[id] = object
			sync()
			Expect(store.writes).To(Equal(1), "Cloudflare derives an SRV record's content")
			update(func(record *cloudflarev1alpha1.DNSRecord) {
				record.Spec.Data = &apiextensionsv1.JSON{Raw: []byte(`{"port":8443}`)}
			})
			sync()
			Expect(store.writes).To(Equal(2))
			Expect(readDNSRecord(new(current())).Data).To(Equal(map[string]any{"port": 8443.0}))
		})
	})

	Describe("the records of a set", func() {
		It("adopts the member whose content it asks for, and leaves the others", func() {
			seed("other", "192.0.2.9", nil)
			seed("mine", "192.0.2.1", nil)
			createRecord(key, adoptOrCreate)
			sync()
			Expect(getRecord().Status.CloudflareMetadata.RecordID).To(Equal("mine"))
			Expect(getRecord().Annotations).To(HaveKey(reconciler.AdoptedAnnotation))
			Expect(store.objects["other"].Content).To(Equal("192.0.2.9"))
		})

		It("adopts a member by its data", func() {
			seed("other", "", func(raw map[string]any) { raw["type"], raw["data"] = "SRV", map[string]any{"port": 25} })
			seed("mine", "", func(raw map[string]any) { raw["type"], raw["data"] = "SRV", map[string]any{"port": 443} })
			createRecord(key, func(record *cloudflarev1alpha1.DNSRecord) {
				adoptOrCreate(record)
				record.Spec.Type, record.Spec.Content = "SRV", ""
				record.Spec.Data = &apiextensionsv1.JSON{Raw: []byte(`{"port":443}`)}
			})
			sync()
			Expect(getRecord().Status.CloudflareMetadata.RecordID).To(Equal("mine"))
		})

		It("reports AlreadyExists for an identical record without an adoption policy, and leaves it", func() {
			seed("existing", "192.0.2.1", nil)
			createRecord(key, nil)
			result, err := reconcileRecord()
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(credentialsRetryInterval))
			Expect(condition().Reason).To(Equal("AlreadyExists"))
			Expect(store.writes).To(BeZero())
		})

		It("joins the set without an adoption policy when the record there differs", func() {
			seed("existing", "192.0.2.9", nil)
			createRecord(key, nil)
			sync()
			Expect(getRecord().Status.CloudflareMetadata.RecordID).NotTo(Equal("existing"))
			Expect(store.objects).To(HaveLen(2))
		})

		It("adopts and corrects the only record with the name and type with adopt-or-create", func() {
			seed("existing", "192.0.2.9", nil)
			createRecord(key, adoptOrCreate)
			sync()
			Expect(getRecord().Status.CloudflareMetadata.RecordID).To(Equal("existing"))
			Expect(store.objects["existing"].Content).To(Equal("192.0.2.1"))
		})

		It("does not take a lone record for its own when two differ", func() {
			seed("one", "192.0.2.8", nil)
			seed("two", "192.0.2.9", nil)
			createRecord(key, adoptOrCreate)
			sync()
			Expect(store.objects).To(HaveLen(3))
		})

		It("ignores records of another name or type", func() {
			seed("other-name", "192.0.2.1", func(raw map[string]any) { raw["name"] = "api.example.com" })
			seed("other-type", "192.0.2.1", func(raw map[string]any) { raw["type"] = "AAAA" })
			createRecord(key, adoptOrCreate)
			sync()
			Expect(store.objects).To(HaveLen(3))
		})

		It("takes a name that differs only in case for the same record", func() {
			seed("existing", "192.0.2.1", func(raw map[string]any) { raw["name"] = "WWW.example.com" })
			createRecord(key, adoptOrCreate)
			sync()
			Expect(getRecord().Status.CloudflareMetadata.RecordID).To(Equal("existing"))
		})

		It("joins the set rather than adopt a lone record another DNSRecord manages", func() {
			createRecord(types.NamespacedName{Name: "owner", Namespace: ns}, func(record *cloudflarev1alpha1.DNSRecord) {
				record.Spec.Content = "192.0.2.9"
				record.Labels = map[string]string{cloudflarev1alpha1.DNSRecordIDLabel: "taken"}
			})
			seed("taken", "192.0.2.9", nil)
			createRecord(key, adoptOrCreate)
			sync()
			Expect(getRecord().Status.CloudflareMetadata.RecordID).NotTo(Equal("taken"))
			Expect(store.objects["taken"].Content).To(Equal("192.0.2.9"))
			Expect(store.objects).To(HaveLen(2))
		})

		It("reports the conflict for an identical record another DNSRecord manages", func() {
			createRecord(types.NamespacedName{Name: "owner", Namespace: ns}, func(record *cloudflarev1alpha1.DNSRecord) {
				record.Labels = map[string]string{cloudflarev1alpha1.DNSRecordIDLabel: "taken"}
			})
			seed("taken", "192.0.2.1", nil)
			createRecord(key, adoptOrCreate)
			result, err := reconcileRecord()
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(credentialsRetryInterval))
			Expect(condition().Reason).To(Equal(reasonNameConflict))
			Expect(store.objects).To(HaveLen(1))
		})

		It("finds the record it claimed when its status was not recorded", func() {
			seed("mine", "192.0.2.9", nil)
			seed("other", "192.0.2.1", nil)
			createRecord(key, func(record *cloudflarev1alpha1.DNSRecord) {
				record.Labels = map[string]string{cloudflarev1alpha1.DNSRecordIDLabel: "mine"}
			})
			sync()
			Expect(getRecord().Status.CloudflareMetadata.RecordID).To(Equal("mine"))
		})

		It("finds a record in a later page", func() {
			store.pageCap = 2
			seed("a", "192.0.2.7", nil)
			seed("b", "192.0.2.8", nil)
			seed("c", "192.0.2.1", nil)
			createRecord(key, adoptOrCreate)
			sync()
			Expect(getRecord().Status.CloudflareMetadata.RecordID).To(Equal("c"))
			Expect(store.listPages).To(Equal([]int64{1, 2}))
		})
	})

	Describe("with a tunnelRef", func() {
		createTunnel := func(account, id string, ready bool) {
			tunnel := &cloudflarev1alpha1.Tunnel{
				ObjectMeta: metav1.ObjectMeta{Name: tunnelName, Namespace: ns},
				Spec: cloudflarev1alpha1.TunnelSpec{
					AccountRef: corev1.LocalObjectReference{Name: account}, Name: "edge",
					TunnelSpecExtras: cloudflarev1alpha1.TunnelSpecExtras{
						CredentialsSecretRef: cloudflarev1alpha1.TunnelCredentialsSecretReference{Name: "t"},
					},
				},
			}
			create(tunnel)
			tunnel.Status.CloudflareMetadata.TunnelID = id
			if ready {
				setReady(tunnel)
			} else {
				Expect(k8sClient.Status().Update(ctx, tunnel)).To(Succeed())
			}
		}
		pointAtTunnel := func(record *cloudflarev1alpha1.DNSRecord) {
			record.Spec.Type, record.Spec.Name, record.Spec.Content = "CNAME", "app.example.com", ""
			record.Spec.TunnelRef = &corev1.LocalObjectReference{Name: tunnelName}
		}

		It("waits for the Tunnel while it does not exist", func() {
			createRecord(key, pointAtTunnel)
			_, err := reconcileRecord()
			Expect(err).NotTo(HaveOccurred())
			Expect(condition().Reason).To(Equal("TunnelNotFound"))
		})

		It("waits for the Tunnel until it is ready", func() {
			createTunnel(accountName, "tunnel-1", false)
			createRecord(key, pointAtTunnel)
			_, err := reconcileRecord()
			Expect(err).NotTo(HaveOccurred())
			Expect(condition().Reason).To(Equal("TunnelNotReady"))
		})

		It("refuses a Tunnel that uses another CloudflareAccount than the Zone", func() {
			createTunnel("another-account", "tunnel-1", true)
			createRecord(key, pointAtTunnel)
			_, err := reconcileRecord()
			Expect(err).NotTo(HaveOccurred())
			Expect(condition().Reason).To(Equal(reasonAccountMismatch))
		})

		It("creates a proxied CNAME to the tunnel's hostname, and then leaves it alone", func() {
			createTunnel(accountName, "tunnel-1", true)
			createRecord(key, pointAtTunnel)
			sync()
			Expect(current().Content).To(Equal("tunnel-1" + cloudflarev1alpha1.TunnelHostnameSuffix))
			Expect(current().Proxied).To(BeTrue())
			sync()
			Expect(store.writes).To(Equal(1))
		})

		It("follows the tunnel to its new ID, and turns proxying back on", func() {
			createTunnel(accountName, "tunnel-1", true)
			createRecord(key, pointAtTunnel)
			sync()
			tunnel := &cloudflarev1alpha1.Tunnel{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: tunnelName, Namespace: ns}, tunnel)).To(Succeed())
			tunnel.Status.CloudflareMetadata.TunnelID = "tunnel-2"
			Expect(k8sClient.Status().Update(ctx, tunnel)).To(Succeed())
			sync()
			Expect(current().Content).To(Equal("tunnel-2" + cloudflarev1alpha1.TunnelHostnameSuffix))

			id := getRecord().Status.CloudflareMetadata.RecordID
			object := store.objects[id]
			object.Proxied = false
			store.objects[id] = object
			sync()
			Expect(current().Proxied).To(BeTrue())
		})

		It("reports AlreadyExists for a CNAME to the tunnel it did not create", func() {
			createTunnel(accountName, "tunnel-1", true)
			seed("existing", "tunnel-1"+cloudflarev1alpha1.TunnelHostnameSuffix, func(raw map[string]any) {
				raw["name"], raw["type"] = "app.example.com", "CNAME"
			})
			createRecord(key, pointAtTunnel)
			_, err := reconcileRecord()
			Expect(err).NotTo(HaveOccurred())
			Expect(condition().Reason).To(Equal("AlreadyExists"))
		})

		It("finds the record it was creating by the tunnel's hostname", func() {
			createTunnel(accountName, "tunnel-1", true)
			seed("mine", "tunnel-1"+cloudflarev1alpha1.TunnelHostnameSuffix, func(raw map[string]any) {
				raw["name"], raw["type"] = "app.example.com", "CNAME"
			})
			createRecord(key, func(record *cloudflarev1alpha1.DNSRecord) {
				pointAtTunnel(record)
				record.Annotations = map[string]string{reconciler.CreatingAnnotation: "app.example.com"}
			})
			sync()
			Expect(getRecord().Status.CloudflareMetadata.RecordID).To(Equal("mine"))
			Expect(getRecord().Annotations).NotTo(HaveKey(reconciler.AdoptedAnnotation))
		})

		It("maps a Tunnel to the records in its namespace that point at it", func() {
			createRecord(key, pointAtTunnel)
			createRecord(types.NamespacedName{Name: "plain", Namespace: ns}, nil)
			ref := r.crud().references()[0]
			tunnel := &cloudflarev1alpha1.Tunnel{ObjectMeta: metav1.ObjectMeta{Name: tunnelName, Namespace: ns}}
			Expect(r.crud().resourcesReferencing(ref)(ctx, tunnel)).To(ConsistOf(reconcile.Request{NamespacedName: key}))
		})
	})

	Describe("validation", func() {
		tunnelRef := &corev1.LocalObjectReference{Name: "tunnel"}
		srvData := &apiextensionsv1.JSON{Raw: []byte(`{"port":443}`)}
		try := func(extras cloudflarev1alpha1.DNSRecordSpecExtras, recordType, content string) error {
			record := &cloudflarev1alpha1.DNSRecord{
				ObjectMeta: metav1.ObjectMeta{Name: "validated", Namespace: ns},
				Spec: cloudflarev1alpha1.DNSRecordSpec{
					ZoneRef: corev1.LocalObjectReference{Name: zoneName}, Name: "x.example.com", Type: recordType,
					Content: content, DNSRecordSpecExtras: extras,
				},
			}
			err := k8sClient.Create(ctx, record)
			forceDelete(ctx, &cloudflarev1alpha1.DNSRecord{}, client.ObjectKeyFromObject(record))
			return err
		}

		DescribeTable("refuses",
			func(extras cloudflarev1alpha1.DNSRecordSpecExtras, recordType, content, message string) {
				Expect(try(extras, recordType, content)).To(MatchError(ContainSubstring(message)))
			},
			Entry("content and a tunnelRef", cloudflarev1alpha1.DNSRecordSpecExtras{TunnelRef: tunnelRef}, "CNAME", "a.b",
				"set exactly one of content, data and tunnelRef"),
			Entry("data and a tunnelRef", cloudflarev1alpha1.DNSRecordSpecExtras{TunnelRef: tunnelRef, Data: srvData},
				"CNAME", "", "set exactly one of content, data and tunnelRef"),
			Entry("content and data", cloudflarev1alpha1.DNSRecordSpecExtras{Data: srvData}, "SRV", "a.b",
				"set exactly one of content, data and tunnelRef"),
			Entry("none of them", cloudflarev1alpha1.DNSRecordSpecExtras{}, "A", "",
				"set exactly one of content, data and tunnelRef"),
			Entry("a tunnelRef on another type than CNAME", cloudflarev1alpha1.DNSRecordSpecExtras{TunnelRef: tunnelRef},
				"A", "", "tunnelRef needs type CNAME"),
			Entry("an unproxied record with a tunnelRef",
				cloudflarev1alpha1.DNSRecordSpecExtras{TunnelRef: tunnelRef, Proxied: new(false)}, "CNAME", "",
				"must be proxied"),
			Entry("a ttl on a record with a tunnelRef",
				cloudflarev1alpha1.DNSRecordSpecExtras{TunnelRef: tunnelRef, TTL: 300}, "CNAME", "",
				"takes ttl 1 (automatic)"),
			Entry("a ttl on a proxied record", cloudflarev1alpha1.DNSRecordSpecExtras{Proxied: new(true), TTL: 300}, "A",
				"192.0.2.1", "takes ttl 1 (automatic)"),
			Entry("a ttl Cloudflare cannot accept", cloudflarev1alpha1.DNSRecordSpecExtras{TTL: 20}, "A", "192.0.2.1",
				"ttl must be 1 (automatic) or between 30 and 86400"),
			Entry("data that is not an object",
				cloudflarev1alpha1.DNSRecordSpecExtras{Data: &apiextensionsv1.JSON{Raw: []byte(`"text"`)}}, "SRV", "",
				"spec.data"),
		)

		DescribeTable("accepts",
			func(extras cloudflarev1alpha1.DNSRecordSpecExtras, recordType, content string) {
				Expect(try(extras, recordType, content)).To(Succeed())
			},
			Entry("a tunnelRef on a CNAME", cloudflarev1alpha1.DNSRecordSpecExtras{TunnelRef: tunnelRef}, "CNAME", ""),
			Entry("a tunnelRef, proxied, with an automatic ttl",
				cloudflarev1alpha1.DNSRecordSpecExtras{TunnelRef: tunnelRef, Proxied: new(true), TTL: 1}, "CNAME", ""),
			Entry("a ttl on an unproxied record", cloudflarev1alpha1.DNSRecordSpecExtras{TTL: 300}, "A", "192.0.2.1"),
			Entry("data alone", cloudflarev1alpha1.DNSRecordSpecExtras{Data: srvData}, "SRV", ""),
		)
	})

	It("reads a record's data and tags from a response cloudflare-go decoded", func() {
		var object dns.RecordResponse
		Expect(object.UnmarshalJSON([]byte(`{"id":"r","type":"SRV","name":"a","tags":["x:1"],"data":{"port":443}}`))).
			To(Succeed())
		stored := readDNSRecord(&object)
		Expect(stored.Tags).To(Equal([]string{"x:1"}))
		Expect(stored.Data).To(Equal(map[string]any{"port": 443.0}))
	})
})

var _ = Describe("isZoneReady", func() {
	It("needs both the Ready condition and a zone ID", func() {
		zone := &cloudflarev1alpha1.Zone{}
		Expect(isZoneReady(zone)).To(BeFalse())
		zone.Status.CloudflareMetadata.ZoneID = "zone"
		Expect(isZoneReady(zone)).To(BeFalse(), "no condition")
		zone.Status.Conditions = []metav1.Condition{{Type: cloudflarev1alpha1.ConditionReady, Status: metav1.ConditionFalse}}
		Expect(isZoneReady(zone)).To(BeFalse(), "not ready")
		zone.Status.Conditions[0].Status = metav1.ConditionTrue
		Expect(isZoneReady(zone)).To(BeTrue())
		zone.Status.CloudflareMetadata.ZoneID = ""
		Expect(isZoneReady(zone)).To(BeFalse(), "no zone ID")
	})
})

var _ = Describe("tagsEqual", func() {
	DescribeTable("compares tags in any order", func(a, b []string, equal bool) {
		Expect(tagsEqual(a, b)).To(Equal(equal))
	},
		Entry("nil and empty", nil, []string{}, true),
		Entry("another order", []string{"a", "b"}, []string{"b", "a"}, true),
		Entry("another length", []string{"a"}, []string{"a", "b"}, false),
		Entry("another tag", []string{"a", "b"}, []string{"a", "c"}, false),
	)
})
