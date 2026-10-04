/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"encoding/json"

	cf "github.com/cloudflare/cloudflare-go"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	cfpkg "github.com/JonathanGraniero/kflare/pkg/cloudflare"
	corev1 "k8s.io/api/core/v1"
)

var _ = Describe("defaultDNSRecordAPI", func() {
	It("returns an error for an empty token", func() {
		_, err := defaultDNSRecordAPI("")
		Expect(err).To(HaveOccurred())
	})

	It("returns a non-nil client for a non-empty token", func() {
		api, err := defaultDNSRecordAPI("any-token-value")
		Expect(err).NotTo(HaveOccurred())
		Expect(api).NotTo(BeNil())
	})

	It("satisfies the DNSRecordAPI interface from *cfpkg.Client", func() {
		client, err := cfpkg.New("any-token")
		Expect(err).NotTo(HaveOccurred())
		var _ DNSRecordAPI = client
	})
})

var _ = Describe("DNSRecordReconciler SetupWithManager", func() {
	It("registers without error", func() {
		mgr, err := manager.New(cfg, manager.Options{})
		Expect(err).NotTo(HaveOccurred())

		r := &DNSRecordReconciler{
			Client: mgr.GetClient(),
			Scheme: mgr.GetScheme(),
		}
		Expect(r.SetupWithManager(mgr)).To(Succeed())
	})
})

var _ = Describe("DNSRecordReconciler recordsForZone", func() {
	It("returns empty when no records reference the zone", func() {
		r := &DNSRecordReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
		reqs := r.recordsForZone(context.Background(), &cloudflarev1alpha1.Zone{
			ObjectMeta: metav1.ObjectMeta{Name: "some-zone", Namespace: "default"},
		})
		Expect(reqs).To(Or(BeNil(), BeEmpty()))
	})

	It("returns nil when the record list fails", func() {
		r := &DNSRecordReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
		cancelCtx, cancel := context.WithCancel(context.Background())
		cancel()
		reqs := r.recordsForZone(cancelCtx, &cloudflarev1alpha1.Zone{
			ObjectMeta: metav1.ObjectMeta{Name: "some-zone", Namespace: "default"},
		})
		Expect(reqs).To(BeNil())
	})

	Context("when DNS records reference the zone", func() {
		const (
			rfzZoneName   = "rfz-test-zone"
			rfzRecordName = "rfz-test-record"
			rfzNS         = "default"
		)
		ctx := context.Background()

		BeforeEach(func() {
			rec := &cloudflarev1alpha1.DNSRecord{
				ObjectMeta: metav1.ObjectMeta{Name: rfzRecordName, Namespace: rfzNS},
				Spec: cloudflarev1alpha1.DNSRecordSpec{
					ZoneRef: corev1.LocalObjectReference{Name: rfzZoneName},
					Name:    "www.rfz-example.com",
					Type:    "A",
					Content: "1.2.3.4",
				},
			}
			Expect(k8sClient.Create(ctx, rec)).To(Succeed())
		})

		AfterEach(func() {
			rec := &cloudflarev1alpha1.DNSRecord{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: rfzRecordName, Namespace: rfzNS}, rec); err == nil {
				rec.Finalizers = nil
				_ = k8sClient.Update(ctx, rec)
				_ = k8sClient.Delete(ctx, rec)
			}
		})

		It("returns a reconcile.Request for each record referencing the zone", func() {
			r := &DNSRecordReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			zone := &cloudflarev1alpha1.Zone{
				ObjectMeta: metav1.ObjectMeta{Name: rfzZoneName, Namespace: rfzNS},
			}
			reqs := r.recordsForZone(ctx, zone)
			Expect(reqs).To(HaveLen(1))
			Expect(reqs[0].NamespacedName.Name).To(Equal(rfzRecordName))
			Expect(reqs[0].NamespacedName.Namespace).To(Equal(rfzNS))
		})
	})
})

var _ = Describe("isZoneReady", func() {
	It("returns false when the zone has no conditions", func() {
		zone := &cloudflarev1alpha1.Zone{
			Status: cloudflarev1alpha1.ZoneStatus{
				CloudflareMetadata: cloudflarev1alpha1.ZoneCloudflareMetadata{ZoneID: "zone-123"},
			},
		}
		Expect(isZoneReady(zone)).To(BeFalse())
	})

	It("returns false when Ready=False", func() {
		zone := &cloudflarev1alpha1.Zone{
			Status: cloudflarev1alpha1.ZoneStatus{
				CloudflareMetadata: cloudflarev1alpha1.ZoneCloudflareMetadata{ZoneID: "zone-123"},
				Conditions: []metav1.Condition{
					{Type: cloudflarev1alpha1.ConditionReady, Status: metav1.ConditionFalse},
				},
			},
		}
		Expect(isZoneReady(zone)).To(BeFalse())
	})

	It("returns false when Ready=True but zoneID is empty", func() {
		zone := &cloudflarev1alpha1.Zone{
			Status: cloudflarev1alpha1.ZoneStatus{
				Conditions: []metav1.Condition{
					{Type: cloudflarev1alpha1.ConditionReady, Status: metav1.ConditionTrue},
				},
			},
		}
		Expect(isZoneReady(zone)).To(BeFalse())
	})

	It("returns true when Ready=True and zoneID is set", func() {
		zone := &cloudflarev1alpha1.Zone{
			Status: cloudflarev1alpha1.ZoneStatus{
				CloudflareMetadata: cloudflarev1alpha1.ZoneCloudflareMetadata{ZoneID: "zone-123"},
				Conditions: []metav1.Condition{
					{Type: cloudflarev1alpha1.ConditionReady, Status: metav1.ConditionTrue},
				},
			},
		}
		Expect(isZoneReady(zone)).To(BeTrue())
	})
})

var _ = Describe("tagsEqual", func() {
	It("returns true for two nil slices", func() {
		Expect(tagsEqual(nil, nil)).To(BeTrue())
	})

	It("returns true for two empty slices", func() {
		Expect(tagsEqual([]string{}, []string{})).To(BeTrue())
	})

	It("returns true for slices with the same elements in different order", func() {
		Expect(tagsEqual([]string{"b", "a"}, []string{"a", "b"})).To(BeTrue())
	})

	It("returns false for slices with different lengths", func() {
		Expect(tagsEqual([]string{"a"}, []string{"a", "b"})).To(BeFalse())
	})

	It("returns false for slices with different elements", func() {
		Expect(tagsEqual([]string{"a", "c"}, []string{"a", "b"})).To(BeFalse())
	})
})

var _ = Describe("dataDrifted", func() {
	makeRecord := func(rawJSON []byte) *cloudflarev1alpha1.DNSRecord {
		rec := &cloudflarev1alpha1.DNSRecord{}
		if rawJSON != nil {
			rec.Spec.Data = &apiextensionsv1.JSON{Raw: rawJSON}
		}
		return rec
	}

	It("returns false when both spec and CF data are nil", func() {
		Expect(dataDrifted(makeRecord(nil), cf.DNSRecord{})).To(BeFalse())
	})

	It("returns true when spec data is nil but CF data is set", func() {
		Expect(dataDrifted(makeRecord(nil), cf.DNSRecord{Data: map[string]interface{}{"port": float64(80)}})).To(BeTrue())
	})

	It("returns true when spec data is set but CF data is nil", func() {
		Expect(dataDrifted(makeRecord([]byte(`{"port":80}`)), cf.DNSRecord{})).To(BeTrue())
	})

	It("returns false when both have equal data", func() {
		cfData := map[string]interface{}{"port": float64(80), "weight": float64(1)}
		cfJSON, _ := json.Marshal(cfData)
		Expect(dataDrifted(makeRecord(cfJSON), cf.DNSRecord{Data: cfData})).To(BeFalse())
	})

	It("returns true when data values differ", func() {
		specJSON := []byte(`{"port":80}`)
		cfData := map[string]interface{}{"port": float64(9000)}
		Expect(dataDrifted(makeRecord(specJSON), cf.DNSRecord{Data: cfData})).To(BeTrue())
	})

	It("returns true for invalid spec JSON", func() {
		Expect(dataDrifted(makeRecord([]byte(`not-valid-json`)), cf.DNSRecord{Data: "something"})).To(BeTrue())
	})
})

var _ = Describe("buildCreateParams", func() {
	It("sends an automatic TTL when spec.ttl is unset", func() {
		rec := &cloudflarev1alpha1.DNSRecord{
			Spec: cloudflarev1alpha1.DNSRecordSpec{Type: "A", Name: "www.example.com", Content: "1.2.3.4"},
		}
		params, err := buildCreateParams(rec)
		Expect(err).NotTo(HaveOccurred())
		Expect(params.TTL).To(Equal(1))
	})

	It("includes Data when spec.Data is valid JSON", func() {
		rec := &cloudflarev1alpha1.DNSRecord{
			Spec: cloudflarev1alpha1.DNSRecordSpec{
				Type:    "SRV",
				Name:    "_http._tcp.example.com",
				Content: "",
				TTL:     300,
				Data:    &apiextensionsv1.JSON{Raw: []byte(`{"port":8080,"weight":1}`)},
			},
		}
		params, err := buildCreateParams(rec)
		Expect(err).NotTo(HaveOccurred())
		Expect(params.Data).NotTo(BeNil())
	})

	It("returns error when spec.Data contains invalid JSON", func() {
		rec := &cloudflarev1alpha1.DNSRecord{
			Spec: cloudflarev1alpha1.DNSRecordSpec{
				Type: "SRV",
				Name: "_http._tcp.example.com",
				Data: &apiextensionsv1.JSON{Raw: []byte(`not-valid-json`)},
			},
		}
		_, err := buildCreateParams(rec)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("invalid data field"))
	})
})

var _ = Describe("driftDetect", func() {
	makeProxy := func(b bool) *bool { return &b }
	makePriority := func(n uint16) *uint16 { return &n }

	It("returns false with zero params when nothing has drifted", func() {
		proxied := true
		priority := uint16(10)
		spec := &cloudflarev1alpha1.DNSRecord{
			Spec: cloudflarev1alpha1.DNSRecordSpec{
				Type:     "A",
				Content:  "1.2.3.4",
				TTL:      300,
				Proxied:  &proxied,
				Priority: &priority,
				Comment:  "my record",
				Tags:     []string{"env:prod"},
			},
		}
		cfRecord := cf.DNSRecord{
			Type:     "A",
			Content:  "1.2.3.4",
			TTL:      300,
			Proxied:  makeProxy(true),
			Priority: makePriority(10),
			Comment:  "my record",
			Tags:     []string{"env:prod"},
		}
		drifted, params := driftDetect(spec, cfRecord)
		Expect(drifted).To(BeFalse())
		Expect(params).To(Equal(cf.UpdateDNSRecordParams{}))
	})

	It("treats unset ttl and proxied as Cloudflare's defaults (automatic, not proxied)", func() {
		spec := &cloudflarev1alpha1.DNSRecord{
			Spec: cloudflarev1alpha1.DNSRecordSpec{Type: "A", Content: "1.2.3.4"},
		}
		// Cloudflare always reports ttl and proxied, even when they were never set.
		cfRecord := cf.DNSRecord{Type: "A", Content: "1.2.3.4", TTL: 1, Proxied: makeProxy(false), Tags: []string{}}
		drifted, _ := driftDetect(spec, cfRecord)
		Expect(drifted).To(BeFalse())
	})

	It("sends proxied=false explicitly when an unset proxied was enabled outside kflare", func() {
		spec := &cloudflarev1alpha1.DNSRecord{
			Spec: cloudflarev1alpha1.DNSRecordSpec{Type: "A", Content: "1.2.3.4"},
		}
		cfRecord := cf.DNSRecord{Type: "A", Content: "1.2.3.4", TTL: 1, Proxied: makeProxy(true)}
		drifted, params := driftDetect(spec, cfRecord)
		Expect(drifted).To(BeTrue())
		// A nil Proxied would be omitted from the PATCH and leave proxying on.
		Expect(params.Proxied).To(Equal(makeProxy(false)))
	})

	It("sends ttl=1 explicitly when an unset ttl was changed outside kflare", func() {
		spec := &cloudflarev1alpha1.DNSRecord{
			Spec: cloudflarev1alpha1.DNSRecordSpec{Type: "A", Content: "1.2.3.4"},
		}
		cfRecord := cf.DNSRecord{Type: "A", Content: "1.2.3.4", TTL: 300, Proxied: makeProxy(false)}
		drifted, params := driftDetect(spec, cfRecord)
		Expect(drifted).To(BeTrue())
		Expect(params.TTL).To(Equal(1))
	})

	It("leaves priority to Cloudflare when the spec does not set it", func() {
		spec := &cloudflarev1alpha1.DNSRecord{
			Spec: cloudflarev1alpha1.DNSRecordSpec{Type: "MX", Content: "mail.example.com", TTL: 300},
		}
		cfRecord := cf.DNSRecord{Type: "MX", Content: "mail.example.com", TTL: 300, Proxied: makeProxy(false),
			Priority: makePriority(10)}
		drifted, _ := driftDetect(spec, cfRecord)
		Expect(drifted).To(BeFalse())
	})

	It("skips content drift when the record is described by data (CAA)", func() {
		spec := &cloudflarev1alpha1.DNSRecord{
			Spec: cloudflarev1alpha1.DNSRecordSpec{
				Type: "CAA",
				TTL:  300,
				Data: &apiextensionsv1.JSON{Raw: []byte(`{"flags":0,"tag":"issue","value":"letsencrypt.org"}`)},
			},
		}
		cfRecord := cf.DNSRecord{
			Type:    "CAA",
			Content: `0 issue "letsencrypt.org"`, // derived from data by Cloudflare
			TTL:     300,
			Proxied: makeProxy(false),
			Data:    map[string]interface{}{"flags": float64(0), "tag": "issue", "value": "letsencrypt.org"},
		}
		drifted, _ := driftDetect(spec, cfRecord)
		Expect(drifted).To(BeFalse())
	})

	It("sends an empty tag list rather than null when the spec removes all tags", func() {
		spec := &cloudflarev1alpha1.DNSRecord{
			Spec: cloudflarev1alpha1.DNSRecordSpec{Type: "A", Content: "1.2.3.4", TTL: 300},
		}
		cfRecord := cf.DNSRecord{Type: "A", Content: "1.2.3.4", TTL: 300, Tags: []string{"env:prod"}}
		drifted, params := driftDetect(spec, cfRecord)
		Expect(drifted).To(BeTrue())
		Expect(params.Tags).NotTo(BeNil())
		Expect(params.Tags).To(BeEmpty())
	})

	It("skips content drift for SRV records", func() {
		spec := &cloudflarev1alpha1.DNSRecord{
			Spec: cloudflarev1alpha1.DNSRecordSpec{
				Type:    "SRV",
				Content: "something-different", // would drift for other types
				TTL:     300,
			},
		}
		cfRecord := cf.DNSRecord{
			Type:    "SRV",
			Content: "auto-formatted-by-cf",
			TTL:     300,
		}
		drifted, _ := driftDetect(spec, cfRecord)
		// Only content could have drifted, but it's skipped for SRV.
		Expect(drifted).To(BeFalse())
	})
})
