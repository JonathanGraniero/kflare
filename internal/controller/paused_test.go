/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"errors"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

// setPaused sets obj's kflare.dev/paused annotation to "true", or removes it,
// and updates obj, which it reads again first.
func setPaused(ctx context.Context, obj client.Object, paused bool) {
	GinkgoHelper()
	Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj)).To(Succeed())
	annotations := obj.GetAnnotations()
	if paused {
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[reconciler.PausedAnnotation] = "true"
	} else {
		delete(annotations, reconciler.PausedAnnotation)
	}
	obj.SetAnnotations(annotations)
	Expect(k8sClient.Update(ctx, obj)).To(Succeed())
}

// These specs cover updatePaused, which every controller reports a paused
// resource with. Every kind reports through it, so a CloudflareAccount
// stands in for all of them.
var _ = Describe("updatePaused", func() {
	const name = "paused-status"
	ctx := context.Background()
	var recorder *events.FakeRecorder
	var account *cloudflarev1alpha1.CloudflareAccount

	BeforeEach(func() {
		recorder = events.NewFakeRecorder(10)
		SetEventRecorder(recorder)
		DeferCleanup(func() { SetEventRecorder(nil) })
		account = &cloudflarev1alpha1.CloudflareAccount{
			ObjectMeta: metav1.ObjectMeta{Name: name, Finalizers: []string{reconciler.Finalizer}},
			Spec: cloudflarev1alpha1.CloudflareAccountSpec{
				AccountID:      "acct-" + name,
				TokenSecretRef: cloudflarev1alpha1.SecretReference{Name: name, Namespace: "default", Key: "CF_API_TOKEN"},
			},
		}
		Expect(k8sClient.Create(ctx, account)).To(Succeed())
		DeferCleanup(func() {
			forceDelete(ctx, &cloudflarev1alpha1.CloudflareAccount{}, types.NamespacedName{Name: name})
		})
	})

	stored := func() *cloudflarev1alpha1.CloudflareAccount {
		obj := &cloudflarev1alpha1.CloudflareAccount{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, obj)).To(Succeed())
		return obj
	}
	ready := func() *metav1.Condition {
		return meta.FindStatusCondition(stored().Status.Conditions, cloudflarev1alpha1.ConditionReady)
	}

	It("keeps Ready's status and observed generation, and records a Normal Event once", func() {
		Expect(updateReady(ctx, k8sClient, account, "Validated", "valid")).To(Succeed())
		Expect(recorder.Events).To(Receive())
		synced := *ready()

		By("leaving a spec changed while paused unobserved")
		account.Spec.AccountID = "acct-changed"
		Expect(k8sClient.Update(ctx, account)).To(Succeed())
		Expect(account.Generation).To(BeNumerically(">", synced.ObservedGeneration))
		Expect(updatePaused(ctx, k8sClient, account)).To(Succeed())
		cond := ready()
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal(reasonPaused))
		Expect(cond.Message).To(Equal(pausedMessage))
		Expect(cond.ObservedGeneration).To(Equal(synced.ObservedGeneration))
		Expect(cond.LastTransitionTime).To(Equal(synced.LastTransitionTime))
		Expect(recorder.Events).To(Receive(Equal("Normal Paused " + pausedMessage)))

		Expect(updatePaused(ctx, k8sClient, account)).To(Succeed())
		Expect(recorder.Events).To(BeEmpty(), "a resource that stays paused records nothing")
	})

	It("keeps Ready=False, and records the pause as Normal all the same", func() {
		Expect(updateNotReady(ctx, k8sClient, account, "TerminalError", "refused")).To(Succeed())
		Expect(recorder.Events).To(Receive())
		Expect(updatePaused(ctx, k8sClient, account)).To(Succeed())
		cond := ready()
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(reasonPaused))
		Expect(recorder.Events).To(Receive(Equal("Normal Paused " + pausedMessage)))
	})

	It("reports Ready=Unknown for a resource never reconciled", func() {
		Expect(updatePaused(ctx, k8sClient, account)).To(Succeed())
		cond := ready()
		Expect(cond.Status).To(Equal(metav1.ConditionUnknown))
		Expect(cond.Reason).To(Equal(reasonPaused))
		Expect(cond.ObservedGeneration).To(Equal(account.Generation))
		Expect(recorder.Events).To(Receive(Equal("Normal Paused " + pausedMessage)))
	})

	It("says that the deletion of a paused resource waits for the annotation to go", func() {
		Expect(updatePaused(ctx, k8sClient, account)).To(Succeed())
		Expect(recorder.Events).To(Receive())
		Expect(k8sClient.Delete(ctx, account)).To(Succeed())
		deleting := stored()
		Expect(updatePaused(ctx, k8sClient, deleting)).To(Succeed())
		Expect(ready().Message).To(Equal(pausedDeletionMessage))
		Expect(stored().Finalizers).To(ContainElement(reconciler.Finalizer))
		Expect(recorder.Events).To(BeEmpty(), "the reason stays Paused")
	})

	It("writes nothing for a resource being deleted without kflare's finalizer", func() {
		account.Finalizers = []string{"example.com/other"}
		Expect(k8sClient.Update(ctx, account)).To(Succeed())
		Expect(k8sClient.Delete(ctx, account)).To(Succeed())
		Expect(updatePaused(ctx, k8sClient, stored())).To(Succeed())
		Expect(ready()).To(BeNil())
		Expect(recorder.Events).To(BeEmpty())
	})

	It("returns the status write's error, and records nothing", func() {
		stale := account.DeepCopy()
		Expect(updateNotReady(ctx, k8sClient, account, "APIError", "first")).To(Succeed())
		Expect(recorder.Events).To(Receive())
		err := updatePaused(ctx, k8sClient, stale)
		Expect(apierrors.IsConflict(err)).To(BeTrue(), "got %v", err)
		Expect(recorder.Events).To(BeEmpty())
	})
})

// errPausedSpecClient is the error the factories of the specs below fail
// with, so that a reconcile that builds a Cloudflare client reports
// InvalidToken and calls nothing.
var errPausedSpecClient = errors.New("this spec calls no Cloudflare API")

// pausedKind is a hand-written controller's part in the specs every one of
// them runs for a paused resource. The kinds that run on a shared reconciler
// (the generated kinds, every rule kind among them) have specs of their own
// in the shared reconcilers' spec files.
type pausedKind struct {
	name string
	// newObject returns a valid resource of the kind named name, whose
	// references name the environment's account, Zone and Tunnel.
	newObject func(name string) conditionedObject
	// newReconciler returns the kind's reconciler, whose Cloudflare client
	// factory calls built and fails with errPausedSpecClient.
	newReconciler func(built func()) reconcile.Reconciler
}

// The environment the specs below set up: a ready account with its token
// Secret, a ready Zone and a ready Tunnel. With it, every kind's reconcile
// gets as far as building a Cloudflare client, unless the resource is paused.
const (
	pausedSpecNS      = "default"
	pausedSpecAccount = "paused-spec-account"
	pausedSpecSecret  = "paused-spec-token"
	pausedSpecZone    = "paused-spec-zone"
	pausedSpecDomain  = "paused.example.com"
	pausedSpecTunnel  = "paused-spec-tunnel"
)

var pausedSpecTokenRef = cloudflarev1alpha1.SecretReference{
	Name: pausedSpecSecret, Namespace: pausedSpecNS, Key: "CF_API_TOKEN",
}

var pausedKinds = []pausedKind{
	{
		name: "CloudflareAccount",
		newObject: func(name string) conditionedObject {
			return &cloudflarev1alpha1.CloudflareAccount{
				ObjectMeta: metav1.ObjectMeta{Name: name},
				Spec: cloudflarev1alpha1.CloudflareAccountSpec{
					AccountID: "acct-" + name, TokenSecretRef: pausedSpecTokenRef,
				},
			}
		},
		newReconciler: func(built func()) reconcile.Reconciler {
			return &CloudflareAccountReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(),
				NewCFClient: func(string) (CloudflareAccountAPI, error) { built(); return nil, errPausedSpecClient }}
		},
	},
	{
		name: "Zone",
		newObject: func(name string) conditionedObject {
			return &cloudflarev1alpha1.Zone{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: pausedSpecNS},
				Spec: cloudflarev1alpha1.ZoneSpec{
					Name: name + ".example.com", AccountRef: corev1.LocalObjectReference{Name: pausedSpecAccount},
				},
			}
		},
		newReconciler: func(built func()) reconcile.Reconciler {
			return &ZoneReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(),
				NewZoneAPI: func(string) (ZoneAPI, error) { built(); return nil, errPausedSpecClient }}
		},
	},
	{
		name: "DNSRecord",
		newObject: func(name string) conditionedObject {
			return &cloudflarev1alpha1.DNSRecord{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: pausedSpecNS},
				Spec: cloudflarev1alpha1.DNSRecordSpec{
					ZoneRef: corev1.LocalObjectReference{Name: pausedSpecZone},
					Name:    "www." + pausedSpecDomain, Type: "A", Content: "192.0.2.1",
				},
			}
		},
		newReconciler: func(built func()) reconcile.Reconciler {
			return &DNSRecordReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(),
				NewDNSRecordAPI: func(string) (DNSRecordAPI, error) { built(); return nil, errPausedSpecClient }}
		},
	},
	{
		name: "TunnelConfiguration",
		newObject: func(name string) conditionedObject {
			return &cloudflarev1alpha1.TunnelConfiguration{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: pausedSpecNS},
				Spec: cloudflarev1alpha1.TunnelConfigurationSpec{
					TunnelRef: corev1.LocalObjectReference{Name: pausedSpecTunnel},
					Ingress: []cloudflarev1alpha1.TunnelIngressRule{
						{Hostname: "app." + pausedSpecDomain, Service: "http://app.default.svc:80"},
					},
				},
			}
		},
		newReconciler: func(built func()) reconcile.Reconciler {
			return &TunnelConfigurationReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(),
				NewTunnelConfigurationAPI: func(string) (TunnelConfigurationAPI, error) {
					built()
					return nil, errPausedSpecClient
				}}
		},
	},
	{
		name: "WorkerScript",
		newObject: func(name string) conditionedObject {
			return &cloudflarev1alpha1.WorkerScript{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: pausedSpecNS},
				Spec: cloudflarev1alpha1.WorkerScriptSpec{
					Name:       name,
					AccountRef: corev1.LocalObjectReference{Name: pausedSpecAccount},
					Script:     new("export default { fetch() { return new Response('paused') } }"),
				},
			}
		},
		newReconciler: func(built func()) reconcile.Reconciler {
			return &WorkerScriptReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(),
				NewWorkerScriptAPI: func(string) (WorkerScriptAPI, error) { built(); return nil, errPausedSpecClient }}
		},
	},
}

var _ = Describe("Every hand-written controller, for a paused resource", func() {
	ctx := context.Background()

	// create creates obj and deletes it again after the spec. With status,
	// which fills in obj's status, it then writes that status, Ready=True.
	create := func(obj conditionedObject, status func()) {
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		DeferCleanup(forceDelete, ctx, obj.DeepCopyObject().(client.Object), client.ObjectKeyFromObject(obj))
		if status != nil {
			status()
			obj.SetConditions([]metav1.Condition{{
				Type: cloudflarev1alpha1.ConditionReady, Status: metav1.ConditionTrue,
				Reason: "Synced", LastTransitionTime: metav1.Now(),
			}})
			Expect(k8sClient.Status().Update(ctx, obj)).To(Succeed())
		}
	}

	BeforeEach(func() {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: pausedSpecSecret, Namespace: pausedSpecNS},
			Data:       map[string][]byte{"CF_API_TOKEN": []byte("token")},
		}
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())
		DeferCleanup(forceDelete, ctx, &corev1.Secret{}, client.ObjectKeyFromObject(secret))
		create(&cloudflarev1alpha1.CloudflareAccount{
			ObjectMeta: metav1.ObjectMeta{Name: pausedSpecAccount},
			Spec:       cloudflarev1alpha1.CloudflareAccountSpec{AccountID: "acct-paused", TokenSecretRef: pausedSpecTokenRef},
		}, func() {})
		zone := &cloudflarev1alpha1.Zone{
			ObjectMeta: metav1.ObjectMeta{Name: pausedSpecZone, Namespace: pausedSpecNS},
			Spec: cloudflarev1alpha1.ZoneSpec{
				Name: pausedSpecDomain, AccountRef: corev1.LocalObjectReference{Name: pausedSpecAccount},
			},
		}
		create(zone, func() { zone.Status.CloudflareMetadata.ZoneID = "zone-paused" })
		tunnel := &cloudflarev1alpha1.Tunnel{
			ObjectMeta: metav1.ObjectMeta{Name: pausedSpecTunnel, Namespace: pausedSpecNS},
			Spec: cloudflarev1alpha1.TunnelSpec{
				Name:             pausedSpecTunnel,
				AccountRef:       corev1.LocalObjectReference{Name: pausedSpecAccount},
				TunnelSpecExtras: cloudflarev1alpha1.TunnelSpecExtras{CredentialsSecretRef: cloudflarev1alpha1.TunnelCredentialsSecretReference{Name: pausedSpecTunnel + "-token"}},
			},
		}
		create(tunnel, func() { tunnel.Status.CloudflareMetadata.TunnelID = "tunnel-paused" })
	})

	for _, k := range pausedKinds {
		Describe(k.name, func() {
			name := "paused-" + strings.ToLower(k.name)
			var (
				r      reconcile.Reconciler
				builds int
			)
			BeforeEach(func() {
				builds = 0
				r = k.newReconciler(func() { builds++ })
			})

			// paused creates a paused resource, with kflare's finalizer when
			// withFinalizer is set.
			paused := func(withFinalizer bool) conditionedObject {
				obj := k.newObject(name)
				obj.SetAnnotations(map[string]string{reconciler.PausedAnnotation: "true"})
				if withFinalizer {
					obj.SetFinalizers([]string{reconciler.Finalizer})
				}
				create(obj, nil)
				return obj
			}
			reconcileIt := func(obj client.Object) {
				GinkgoHelper()
				result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(obj)})
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(BeZero())
			}
			stored := func(obj conditionedObject) conditionedObject {
				GinkgoHelper()
				current := k.newObject(name)
				Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), current)).To(Succeed())
				return current
			}
			ready := func(obj conditionedObject) metav1.Condition {
				GinkgoHelper()
				cond := meta.FindStatusCondition(stored(obj).GetConditions(), cloudflarev1alpha1.ConditionReady)
				Expect(cond).NotTo(BeNil())
				return *cond
			}

			It("builds no Cloudflare client for it, and reports it, keeping Ready's status", func() {
				obj := paused(true)
				Expect(updateReady(ctx, k8sClient, obj, "Synced", "synced")).To(Succeed())
				reconcileIt(obj)
				Expect(builds).To(BeZero(), "a paused resource built a Cloudflare client")
				Expect(ready(obj).Status).To(Equal(metav1.ConditionTrue))
				Expect(ready(obj).Reason).To(Equal(reasonPaused))
				Expect(ready(obj).Message).To(Equal(pausedMessage))

				By("building one once unpaused, as the environment lets it")
				setPaused(ctx, obj, false)
				_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(obj)})
				Expect(err).NotTo(HaveOccurred())
				Expect(builds).To(Equal(1))
				Expect(ready(obj).Reason).To(Equal("InvalidToken"))
			})

			It("keeps its finalizer when it is deleted, building no Cloudflare client", func() {
				obj := paused(true)
				Expect(k8sClient.Delete(ctx, obj)).To(Succeed())
				reconcileIt(obj)
				Expect(builds).To(BeZero())
				Expect(stored(obj).GetFinalizers()).To(ConsistOf(reconciler.Finalizer))
				Expect(ready(obj).Reason).To(Equal(reasonPaused))
				Expect(ready(obj).Message).To(Equal(pausedDeletionMessage))
			})

			It("adds no finalizer to a resource created paused", func() {
				obj := paused(false)
				reconcileIt(obj)
				Expect(builds).To(BeZero())
				Expect(stored(obj).GetFinalizers()).To(BeEmpty())
				Expect(ready(obj).Status).To(Equal(metav1.ConditionUnknown))
				Expect(ready(obj).Reason).To(Equal(reasonPaused))
			})
		})
	}
})
