/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

// The specs every settings kind runs. kflare-gen writes a table per settings
// kind (zz_generated.<kind>_test.go): a fake of its service on
// fakeSettingsStore, with the lists Cloudflare keys, the settings of a zone
// nobody has configured and those settings configured in each one respect,
// its sample and tests, each with the settings it asks for, edits of them, and
// specs that set every field. describeSettingsKind runs the shared settings reconciler's
// lifecycle against them: the finalizer, waiting for the Zone and the
// credentials, the sample and each test written to an unconfigured zone, an
// idle reconcile that writes nothing, drift corrected, each change written in
// place, settings configured outside kflare taken over only as the adoption
// policy allows, one resource per zone, the reset on deletion and what keeps
// the settings, Cloudflare's errors, the Zone waiting, setup, and the CRD:
// the immutable zoneRef and the specs it refuses.
//
// A kind whose settings go by their IDs reads and writes each setting on its
// own (settings_byid.go), and resets those its resource sets by writing them:
// its fake keeps a zone's settings as one JSON object whose fields are their
// IDs, and the writes it makes while the specs reconcile a resource's
// deletion are that resource's reset.

// settingsKindReconciler is a settings kind's reconciler.
type settingsKindReconciler[A any] interface {
	reconcile.Reconciler
	SetupWithManager(mgr ctrl.Manager) error
	settings() *settingsReconciler[A]
}

// settingsSpecCase is one spec of a settings kind's table.
type settingsSpecCase struct {
	what string
	// spec is the spec as JSON, zoneRef aside, which the specs set.
	spec string
	// sample is the spec of samples whose resource a change edits.
	sample int
	// settings are the settings the spec asks for, as cloudflare-go v7 sends
	// them, and message what the CRD refuses an invalid spec with.
	settings, message string
}

// settingsKindSpecs is a settings kind's table.
type settingsKindSpecs[O settingsObject, A any] struct {
	kind          settingsKind[A]
	newObject     func() O
	newReconciler func(c client.Client, newAPI func(string) (A, error)) settingsKindReconciler[A]
	// newFake returns the kind's fake service on a store; defaultAPI is its
	// production factory.
	newFake    func(store *fakeSettingsStore) A
	defaultAPI func(token string) (A, error)
	// keyedLists are the lists Cloudflare writes item by item, each by the
	// key given, keeping the items a write leaves out. The fake writes them
	// so, whatever the kind compares.
	keyedLists map[string]string
	// picks are, for a kind whose settings go by their IDs, the settings
	// Cloudflare picks the value of itself, each with the value the setting it
	// is picked by takes once the setting is written: the fake writes it so.
	picks map[string]settingsValue
	// unconfigured are the settings of a zone nobody has configured, as JSON,
	// which every zone of the fake starts with: with an item no spec lists in
	// each keyed list, as Cloudflare's hold items a spec leaves out.
	// configured are such settings with one of the values of the kind's
	// unconfigured changed each, which count as configured outside kflare,
	// each read by the resource of the spec of samples sample names.
	unconfigured string
	configured   []settingsSpecCase
	// samples are valid specs: the entry's sample, then its tests; changes
	// edit them; invalid are specs the CRD refuses; full are specs that set
	// every field, which the specs convert without the API server; picked are,
	// for a kind whose settings go by their IDs, specs that each set one
	// setting of picks alone, at another value than an unconfigured zone's.
	samples, changes, invalid, full, picked []settingsSpecCase
}

// fakeSettingsStore is an in-memory Cloudflare's settings of one product, in
// every zone. Each generated fake converts its service's params into calls on
// it. Every zone holds the settings of a zone nobody has configured until it
// is written; a write changes what its body sets, as Cloudflare does, and a
// reset restores the unconfigured settings. Like cloudflare-go, it refuses a
// call without a zone ID.
type fakeSettingsStore struct {
	shape        settingsShape
	unconfigured any
	zones        map[string]any
	// picks are the settings Cloudflare picks the value of itself, of a kind
	// whose settings go by their IDs: a write of one writes, too, the value
	// the setting it is picked by takes.
	picks map[string]settingsValue

	readErr, writeErr, resetErr error

	zoneIDs []string
	// writes holds the body of each write, as cloudflare-go v7 sent it, and
	// resets counts the resets. A kind whose settings go by their IDs writes
	// one setting each time, recorded as an object of its ID and value.
	writes [][]byte
	resets int
	// deleting is set while the specs reconcile the deletion of a resource:
	// the writes of a kind whose settings go by their IDs are then its reset,
	// counted once (resetting), and resetErr refuses them.
	deleting, resetting bool
}

func newFakeSettingsStore(shape settingsShape, unconfigured string) *fakeSettingsStore {
	var settings any
	Expect(json.Unmarshal([]byte(unconfigured), &settings)).To(Succeed())
	return &fakeSettingsStore{shape: shape, unconfigured: settings, zones: map[string]any{}}
}

// zone records the zone ID a call names.
func (s *fakeSettingsStore) zone(id string) error {
	s.zoneIDs = append(s.zoneIDs, id)
	if id == "" {
		return errors.New("missing required zone_id parameter")
	}
	return nil
}

// settings returns the settings of the zone id.
func (s *fakeSettingsStore) settings(id string) any {
	settings, ok := s.zones[id]
	if !ok {
		return s.unconfigured
	}
	return settings
}

// seed sets the settings of the zone id to JSON, as if written outside kflare.
func (s *fakeSettingsStore) seed(id, settings string) {
	var value any
	Expect(json.Unmarshal([]byte(settings), &value)).To(Succeed())
	s.zones[id] = value
}

func (s *fakeSettingsStore) reset(zoneID string) error {
	if err := s.zone(zoneID); err != nil {
		return err
	}
	s.resets++
	if s.resetErr != nil {
		return s.resetErr
	}
	delete(s.zones, zoneID)
	return nil
}

// fakeSettingsRead returns the settings of the zone zoneID, as cloudflare-go
// v7 decodes them into T.
func fakeSettingsRead[T any](s *fakeSettingsStore, zoneID string) (*T, error) {
	if err := s.zone(zoneID); err != nil {
		return nil, err
	}
	if s.readErr != nil {
		return nil, s.readErr
	}
	return decodeSettings[T](s.settings(zoneID))
}

// fakeSettingsWrite writes the settings params set, in the zone zoneID, and
// returns the zone's settings as cloudflare-go v7 decodes them into T.
func fakeSettingsWrite[T any](s *fakeSettingsStore, zoneID string, params json.Marshaler) (*T, error) {
	if err := s.zone(zoneID); err != nil {
		return nil, err
	}
	body, err := params.MarshalJSON()
	if err != nil {
		return nil, err
	}
	s.writes = append(s.writes, body)
	if s.writeErr != nil {
		return nil, s.writeErr
	}
	var written any
	if err := json.Unmarshal(body, &written); err != nil {
		return nil, err
	}
	s.zones[zoneID] = s.shape.written(s.settings(zoneID), written, "")
	return decodeSettings[T](s.zones[zoneID])
}

// fakeSettingReadByID returns the setting id of the zone zoneID, as
// cloudflare-go v7 decodes it into T: its ID and value, as Cloudflare returns
// one of a zone's settings. Like Cloudflare, it refuses a setting it does not
// know.
func fakeSettingReadByID[T any](s *fakeSettingsStore, zoneID, id string) (*T, error) {
	if err := s.zone(zoneID); err != nil {
		return nil, err
	}
	if s.readErr != nil {
		return nil, s.readErr
	}
	settings, _ := s.settings(zoneID).(map[string]any)
	value, ok := settings[id]
	if !ok {
		return nil, v7APIError(http.MethodGet, "zones/"+zoneID+"/settings/"+id, http.StatusBadRequest, 1003,
			"Undefined zone setting: "+id)
	}
	return decodeSettings[T](map[string]any{"id": id, "value": value, "editable": true})
}

// fakeSettingWriteByID writes the value params carry to the setting id of the
// zone zoneID, as Cloudflare writes one of a zone's settings: an object field
// by field, any other value replaced; a setting Cloudflare picks the value of
// itself writes the value of the setting it is picked by too. It returns the
// setting as cloudflare-go v7 decodes it into T. Like Cloudflare, it refuses a
// setting it does not know.
func fakeSettingWriteByID[T any](s *fakeSettingsStore, zoneID, id string, params json.Marshaler) (*T, error) {
	if err := s.zone(zoneID); err != nil {
		return nil, err
	}
	body, err := params.MarshalJSON()
	if err != nil {
		return nil, err
	}
	var sent struct {
		Value any `json:"value"`
	}
	if err := json.Unmarshal(body, &sent); err != nil {
		return nil, err
	}
	written := map[string]any{id: sent.Value}
	if s.deleting {
		if !s.resetting {
			s.resets++
			s.resetting = true
		}
		if s.resetErr != nil {
			return nil, s.resetErr
		}
	} else {
		data, err := json.Marshal(written)
		if err != nil {
			return nil, err
		}
		s.writes = append(s.writes, data)
		if s.writeErr != nil {
			return nil, s.writeErr
		}
	}
	settings, _ := s.settings(zoneID).(map[string]any)
	if _, ok := settings[id]; !ok {
		return nil, v7APIError(http.MethodPatch, "zones/"+zoneID+"/settings/"+id, http.StatusBadRequest, 1006,
			"Unrecognized zone setting name: "+id)
	}
	if pick, ok := s.picks[id]; ok {
		written[pick.path] = pick.value
	}
	updated := s.shape.written(settings, written, "").(map[string]any)
	s.zones[zoneID] = updated
	return decodeSettings[T](map[string]any{"id": id, "value": updated[id], "editable": true})
}

// decodeSettings decodes settings into T as cloudflare-go v7 decodes a
// response, which keeps the JSON it read.
func decodeSettings[T any](settings any) (*T, error) {
	data, err := json.Marshal(settings)
	if err != nil {
		return nil, err
	}
	out := new(T)
	if err := json.Unmarshal(data, out); err != nil {
		return nil, err
	}
	return out, nil
}

// written returns the settings current with body written over them, at
// path: an object's fields one by one, a keyed list's items by their key
// (each item body holds that current does not is added), and any other value
// replaced.
func (s settingsShape) written(current, body any, path string) any {
	switch b := body.(type) {
	case map[string]any:
		c, ok := current.(map[string]any)
		if !ok {
			c = map[string]any{}
		}
		out := map[string]any{}
		maps.Copy(out, c)
		for name, value := range b {
			out[name] = s.written(c[name], value, settingsPath(path, name))
		}
		return out
	case []any:
		key, keyed := s.listKeys[path]
		c, ok := current.([]any)
		if !keyed || !ok {
			return b
		}
		out := slices.Clone(c)
		for _, item := range b {
			i := slices.IndexFunc(out, func(other any) bool { return sameKey(item, other, key) })
			if i < 0 {
				out = append(out, item)
				continue
			}
			out[i] = s.written(out[i], item, path+"[]")
		}
		return out
	}
	return body
}

// comparableSettings is settings, as JSON, without the nulls and the empty
// objects and lists, which Cloudflare treats as unset.
func comparableSettings(settings string) any {
	var out any
	Expect(json.Unmarshal([]byte(settings), &out)).To(Succeed())
	return withoutEmpty(out)
}

// snapshot returns a copy of the settings of the zone id.
func (s *fakeSettingsStore) snapshot(id string) any {
	data, err := json.Marshal(s.settings(id))
	Expect(err).NotTo(HaveOccurred())
	var out any
	Expect(json.Unmarshal(data, &out)).To(Succeed())
	return out
}

// expectWritten checks what the writes after the first n wrote: settings, as
// a table holds them, at once; or, for a kind whose settings go by their IDs
// (byID), each of them that before, the zone's settings before the writes,
// did not hold, on its own.
func (s *fakeSettingsStore) expectWritten(byID bool, n int, before any, settings string) {
	GinkgoHelper()
	if !byID {
		Expect(s.writes).To(HaveLen(n+1), "one write of the settings")
		Expect(comparableSettings(string(s.writes[n]))).To(Equal(comparableSettings(settings)))
		return
	}
	asked, _ := comparableSettings(settings).(map[string]any)
	held, _ := before.(map[string]any)
	want := map[string]any{}
	for id, value := range asked {
		if !s.shape.holds(value, held[id], id) {
			want[id] = value
		}
	}
	got := map[string]any{}
	for _, write := range s.writes[n:] {
		var setting map[string]any
		Expect(json.Unmarshal(write, &setting)).To(Succeed())
		Expect(setting).To(HaveLen(1), "a write of one setting")
		for id, value := range setting {
			Expect(got).NotTo(HaveKey(id), "%s was written twice", id)
			got[id] = value
		}
	}
	Expect(withoutEmpty(got)).To(Equal(withoutEmpty(want)))
}

// expectUnconfigured checks that the zone id holds the settings of a zone
// nobody has configured, unconfigured as JSON, but for those Cloudflare picks
// the value of, which a reset hands back to Cloudflare without writing them.
func (s *fakeSettingsStore) expectUnconfigured(id, unconfigured string) {
	GinkgoHelper()
	got, _ := s.snapshot(id).(map[string]any)
	var want map[string]any
	Expect(json.Unmarshal([]byte(unconfigured), &want)).To(Succeed())
	for picked := range s.picks {
		delete(got, picked)
		delete(want, picked)
	}
	Expect(got).To(Equal(want))
}

// settingsTestError builds the error cloudflare-go v7 returns when
// Cloudflare refuses a call on a zone's settings with an HTTP status and a
// Cloudflare error code.
func settingsTestError(status, code int) error {
	return v7APIError(http.MethodPatch, "zones/zone/settings", status, code, "refused")
}

func describeSettingsKind[O settingsObject, A any](specs settingsKindSpecs[O, A]) bool {
	// The kind as its API type names it; one spec checks that the settings
	// kind goes by it too.
	kind := reflect.TypeOf(specs.newObject()).Elem().Name()
	return Describe(kind+" (settings kind)", func() {
		const ns = "default"
		ctx := context.Background()
		prefix := strings.ToLower(kind)
		zoneName, accountName, secretName := prefix+"-zone", prefix+"-account", prefix+"-api-token"
		zoneID := "zone-" + prefix
		var (
			fake *fakeSettingsStore
			r    settingsKindReconciler[A]
		)

		// create creates obj and deletes it again after the spec.
		create := func(obj client.Object) {
			Expect(k8sClient.Create(ctx, obj)).To(Succeed())
			DeferCleanup(forceDelete, ctx, obj.DeepCopyObject().(client.Object), client.ObjectKeyFromObject(obj))
		}
		// readyZone creates a ready Zone named name for the Cloudflare zone id.
		readyZone := func(name, id string) {
			zone := &cloudflarev1alpha1.Zone{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Spec: cloudflarev1alpha1.ZoneSpec{
					AccountRef: corev1.LocalObjectReference{Name: accountName},
					ZoneSpecExtras: cloudflarev1alpha1.ZoneSpecExtras{
						Name: prefix + ".example.com",
					},
				},
			}
			create(zone)
			zone.Status.CloudflareMetadata.ZoneID = id
			zone.Status.Conditions = []metav1.Condition{{
				Type: cloudflarev1alpha1.ConditionReady, Status: metav1.ConditionTrue,
				Reason: "Synced", LastTransitionTime: metav1.Now(),
			}}
			Expect(k8sClient.Status().Update(ctx, zone)).To(Succeed())
		}

		BeforeEach(func() {
			fake = newFakeSettingsStore(settingsShape{listKeys: specs.keyedLists}, specs.unconfigured)
			fake.picks = specs.picks
			r = specs.newReconciler(k8sClient, func(string) (A, error) { return specs.newFake(fake), nil })

			create(&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns},
				Data:       map[string][]byte{"CF_API_TOKEN": []byte("token")},
			})
			account := &cloudflarev1alpha1.CloudflareAccount{
				ObjectMeta: metav1.ObjectMeta{Name: accountName},
				Spec: cloudflarev1alpha1.CloudflareAccountSpec{
					AccountID:      "acct-" + prefix,
					TokenSecretRef: cloudflarev1alpha1.SecretReference{Name: secretName, Namespace: ns, Key: "CF_API_TOKEN"},
				},
			}
			create(account)
			account.Status.Conditions = []metav1.Condition{{
				Type: cloudflarev1alpha1.ConditionReady, Status: metav1.ConditionTrue,
				Reason: "Validated", LastTransitionTime: metav1.Now(),
			}}
			Expect(k8sClient.Status().Update(ctx, account)).To(Succeed())
			readyZone(zoneName, zoneID)
		})

		key := func(name string) types.NamespacedName { return types.NamespacedName{Name: name, Namespace: ns} }

		// object returns a resource named name, for the Zone, with the spec a
		// table holds as JSON. An unknown field in the JSON fails the spec:
		// the table must name the API types' fields.
		object := func(name, spec string) O {
			obj := specs.newObject()
			decoder := json.NewDecoder(strings.NewReader(`{"spec":` + spec + `}`))
			decoder.DisallowUnknownFields()
			Expect(decoder.Decode(obj)).To(Succeed(), "the table's spec does not fit the API types")
			obj.SetName(name)
			obj.SetNamespace(ns)
			obj.GetSettingsSpec().ZoneRef.Name = zoneName
			return obj
		}
		// rawObject returns the resource that object returns as JSON, which the
		// API server takes unchanged: an empty value or list that the API types
		// leave out stays in.
		rawObject := func(name, spec string) *unstructured.Unstructured {
			object(name, spec)
			raw := &unstructured.Unstructured{}
			Expect(raw.UnmarshalJSON(fmt.Appendf(nil,
				`{"apiVersion":%q,"kind":%q,"metadata":{"name":%q,"namespace":%q},"spec":%s}`,
				cloudflarev1alpha1.GroupVersion.String(), kind, name, ns, spec))).To(Succeed())
			Expect(unstructured.SetNestedField(raw.Object, zoneName, "spec", "zoneRef", "name")).To(Succeed())
			return raw
		}
		get := func(name string) O {
			obj := specs.newObject()
			Expect(k8sClient.Get(ctx, key(name), obj)).To(Succeed())
			return obj
		}
		// reconcileOnce reconciles the resource name, telling the fake whether
		// the resource is being deleted.
		reconcileOnce := func(name string) (ctrl.Result, error) {
			obj := specs.newObject()
			deleting := k8sClient.Get(ctx, key(name), obj) == nil && !obj.GetDeletionTimestamp().IsZero()
			fake.deleting, fake.resetting = deleting, false
			defer func() { fake.deleting = false }()
			return r.Reconcile(ctx, reconcile.Request{NamespacedName: key(name)})
		}
		// ready returns the Ready condition of the resource name.
		ready := func(name string) *metav1.Condition {
			condition := meta.FindStatusCondition(get(name).GetConditions(), cloudflarev1alpha1.ConditionReady)
			Expect(condition).NotTo(BeNil())
			return condition
		}
		// withFinalizer creates a resource named name with spec, carrying the
		// finalizer, and annotations.
		withFinalizer := func(name, spec string, annotations map[string]string) {
			obj := object(name, spec)
			obj.SetFinalizers([]string{reconciler.Finalizer})
			obj.SetAnnotations(annotations)
			create(obj)
		}
		// createSynced creates a resource named name with spec, carrying the
		// finalizer, and reconciles it once.
		createSynced := func(name, spec string) O {
			withFinalizer(name, spec, nil)
			result, err := reconcileOnce(name)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeTrue())
			Expect(ready(name).Reason).To(Equal("Synced"))
			return get(name)
		}
		// deleteAndReconcile deletes the resource name and reconciles its
		// deletion, which removes its finalizer.
		deleteAndReconcile := func(name string) {
			Expect(k8sClient.Delete(ctx, get(name))).To(Succeed())
			_, err := reconcileOnce(name)
			Expect(err).NotTo(HaveOccurred())
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key(name), specs.newObject()))).To(BeTrue())
		}
		annotate := func(name string, annotations map[string]string) {
			obj := get(name)
			patch := client.MergeFrom(obj.DeepCopyObject().(client.Object))
			current := obj.GetAnnotations()
			if current == nil {
				current = map[string]string{}
			}
			maps.Copy(current, annotations)
			obj.SetAnnotations(current)
			Expect(k8sClient.Patch(ctx, obj, patch)).To(Succeed())
		}
		replaceSpec := func(name, spec string) error {
			current := get(name)
			updated := object(name, spec)
			updated.SetResourceVersion(current.GetResourceVersion())
			updated.SetFinalizers(current.GetFinalizers())
			updated.SetAnnotations(current.GetAnnotations())
			return k8sClient.Update(ctx, updated)
		}
		// holds reports whether the fake zone's settings hold what settings,
		// as a table holds them, ask for, as Cloudflare keys its lists.
		holds := func(settings string) bool {
			stored, err := json.Marshal(fake.settings(zoneID))
			Expect(err).NotTo(HaveOccurred())
			matches, err := fake.shape.matches([]byte(settings), stored)
			Expect(err).NotTo(HaveOccurred())
			return matches
		}
		// configuredOutside returns settings configured outside kflare: those of a
		// change written over an unconfigured zone, when they count as configured
		// and do not already hold the sample's settings, else the sample's own.
		// A kind whose settings go by their IDs reads only those the resource
		// sets, so only the sample's changes count, as the sample's resource
		// reads them.
		configuredOutside := func() string {
			var unconfigured any
			Expect(json.Unmarshal([]byte(specs.unconfigured), &unconfigured)).To(Succeed())
			for _, c := range append(slices.Clone(specs.changes), specs.samples[0]) {
				if specs.kind.byID && c.sample != 0 {
					continue
				}
				var body any
				Expect(json.Unmarshal([]byte(c.settings), &body)).To(Succeed())
				data, err := json.Marshal(fake.shape.written(unconfigured, body, ""))
				Expect(err).NotTo(HaveOccurred())
				read := data
				if specs.kind.byID {
					zone := newFakeSettingsStore(fake.shape, string(data))
					got, err := specs.kind.read(ctx, specs.newFake(zone), zoneID,
						object("configured-outside", specs.samples[0].spec))
					Expect(err).NotTo(HaveOccurred())
					read = []byte(got)
				}
				configured, err := settingsConfigured(read, specs.kind.unconfigured)
				Expect(err).NotTo(HaveOccurred())
				matches, err := fake.shape.matches([]byte(specs.samples[0].settings), read)
				Expect(err).NotTo(HaveOccurred())
				if configured && (!matches || c == specs.samples[0]) {
					return string(data)
				}
			}
			Fail("the sample's settings are not configured")
			return ""
		}

		It("adds its finalizer first, and calls Cloudflare only in its Zone", func() {
			create(object("finalizer", specs.samples[0].spec))
			result, err := reconcileOnce("finalizer")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeTrue())
			Expect(get("finalizer").GetFinalizers()).To(ContainElement(reconciler.Finalizer))
			Expect(fake.zoneIDs).To(BeEmpty())

			_, err = reconcileOnce("finalizer")
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.zoneIDs).NotTo(BeEmpty())
			for _, id := range fake.zoneIDs {
				Expect(id).To(Equal(zoneID))
			}
			result, err = reconcileOnce("missing")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeTrue())
		})

		It("writes the sample and each test to an unconfigured zone, and records the zone", func() {
			for i, sample := range specs.samples {
				By(sample.what)
				name := fmt.Sprintf("sample-%d", i)
				writes, before := len(fake.writes), fake.snapshot(zoneID)
				obj := createSynced(name, sample.spec)
				fake.expectWritten(specs.kind.byID, writes, before, sample.settings)
				Expect(holds(sample.settings)).To(BeTrue())
				Expect(obj.GetSettingsStatus().CloudflareMetadata.ZoneID).To(Equal(zoneID))
				Expect(obj.GetAnnotations()).To(HaveKeyWithValue(reconciler.CreatingAnnotation, zoneID))
				Expect(obj.GetAnnotations()).NotTo(HaveKey(reconciler.AdoptedAnnotation))

				By("dropping the creating annotation once status records the zone")
				_, err := reconcileOnce(name)
				Expect(err).NotTo(HaveOccurred())
				Expect(get(name).GetAnnotations()).NotTo(HaveKey(reconciler.CreatingAnnotation))

				By("resetting the settings with the resource, for the next one")
				deleteAndReconcile(name)
				fake.expectUnconfigured(zoneID, specs.unconfigured)
			}
		})

		It("writes nothing to Cloudflare once its settings are in sync", func() {
			for i, sample := range specs.samples {
				name := fmt.Sprintf("idle-%d", i)
				createSynced(name, sample.spec)
				writes := len(fake.writes)
				_, err := reconcileOnce(name)
				Expect(err).NotTo(HaveOccurred())
				Expect(fake.writes).To(HaveLen(writes), "%s: an idle reconcile wrote to Cloudflare", sample.what)
				deleteAndReconcile(name)
			}
		})

		It("schedules a drift check after a sync, once the manager sets an interval", func() {
			createSynced("drift-check", specs.samples[0].spec)
			writes := len(fake.writes)
			expectDriftCheck(func() (reconcile.Result, error) { return reconcileOnce("drift-check") })
			Expect(fake.writes).To(HaveLen(writes))
			deleteAndReconcile("drift-check")
		})

		It("compares its keyed lists by the keys Cloudflare writes them by", func() {
			Expect(maps.Equal(specs.kind.shape.listKeys, specs.keyedLists)).To(BeTrue(),
				"the kind keys %v, Cloudflare %v", specs.kind.shape.listKeys, specs.keyedLists)
		})

		It("writes its settings again when they change outside kflare", func() {
			createSynced("drift", specs.samples[0].spec)
			fake.seed(zoneID, specs.unconfigured)
			writes, before := len(fake.writes), fake.snapshot(zoneID)
			_, err := reconcileOnce("drift")
			Expect(err).NotTo(HaveOccurred())
			fake.expectWritten(specs.kind.byID, writes, before, specs.samples[0].settings)
			Expect(holds(specs.samples[0].settings)).To(BeTrue())
		})

		It("writes the settings again when the spec changes", func() {
			for i, change := range specs.changes {
				By(change.what)
				name := fmt.Sprintf("change-%d", i)
				createSynced(name, specs.samples[change.sample].spec)
				Expect(replaceSpec(name, change.spec)).To(Succeed())
				writes, before := len(fake.writes), fake.snapshot(zoneID)
				_, err := reconcileOnce(name)
				Expect(err).NotTo(HaveOccurred())
				fake.expectWritten(specs.kind.byID, writes, before, change.settings)
				Expect(holds(change.settings)).To(BeTrue())
				deleteAndReconcile(name)
			}
		})

		It("takes over settings configured outside kflare only as its adoption policy allows", func() {
			configured := configuredOutside()
			fake.seed(zoneID, configured)

			By("refusing them without an adoption policy")
			withFinalizer("adopting", specs.samples[0].spec, nil)
			result, err := reconcileOnce("adopting")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(credentialsRetryInterval))
			Expect(ready("adopting").Reason).To(Equal(reasonAlreadyExists))
			Expect(fake.writes).To(BeEmpty())

			By("taking them over with the adopt policy")
			annotate("adopting", map[string]string{reconciler.AdoptionPolicyAnnotation: reconciler.AdoptionPolicyAdopt})
			_, err = reconcileOnce("adopting")
			Expect(err).NotTo(HaveOccurred())
			Expect(ready("adopting").Reason).To(Equal("Synced"))
			Expect(get("adopting").GetAnnotations()).To(HaveKeyWithValue(reconciler.AdoptedAnnotation, "true"))
			Expect(holds(specs.samples[0].settings)).To(BeTrue())

			By("leaving them as they are when it is deleted")
			deleteAndReconcile("adopting")
			Expect(fake.resets).To(BeZero())
			Expect(holds(specs.samples[0].settings)).To(BeTrue())

			By("resetting adopted settings when the deletion policy says delete")
			fake.seed(zoneID, configured)
			withFinalizer("adopting-deleted", specs.samples[0].spec, map[string]string{
				reconciler.AdoptionPolicyAnnotation: reconciler.AdoptionPolicyAdopt,
				reconciler.DeletionPolicyAnnotation: reconciler.DeletionPolicyDelete,
			})
			_, err = reconcileOnce("adopting-deleted")
			Expect(err).NotTo(HaveOccurred())
			Expect(get("adopting-deleted").GetAnnotations()).To(HaveKeyWithValue(reconciler.AdoptedAnnotation, "true"))
			deleteAndReconcile("adopting-deleted")
			Expect(fake.resets).To(Equal(1))
			fake.expectUnconfigured(zoneID, specs.unconfigured)
		})

		It("takes settings that differ from an unconfigured zone's in any one value for configured outside kflare", func() {
			Expect(specs.configured).To(HaveLen(len(specs.kind.unconfigured)), "a configured case per unconfigured value")
			for i, c := range specs.configured {
				By(c.what)
				name := fmt.Sprintf("configured-%d", i)
				fake.seed(zoneID, c.settings)
				withFinalizer(name, specs.samples[c.sample].spec, nil)
				_, err := reconcileOnce(name)
				Expect(err).NotTo(HaveOccurred())
				Expect(ready(name).Reason).To(Equal(reasonAlreadyExists))
				deleteAndReconcile(name)
			}
			Expect(fake.writes).To(BeEmpty())
			Expect(fake.resets).To(BeZero())
		})

		It("takes a setting Cloudflare picks for configured once written, until its resource resets it", func() {
			for i, c := range specs.picked {
				By(c.what)
				name := fmt.Sprintf("picked-%d", i)
				createSynced(name, c.spec)
				Expect(holds(c.settings)).To(BeTrue())
				annotate(name, map[string]string{reconciler.DeletionPolicyAnnotation: reconciler.DeletionPolicyRetain})
				deleteAndReconcile(name)

				By("refusing it to another resource, once its resource kept it")
				withFinalizer(name+"-kept", c.spec, nil)
				_, err := reconcileOnce(name + "-kept")
				Expect(err).NotTo(HaveOccurred())
				Expect(ready(name + "-kept").Reason).To(Equal(reasonAlreadyExists))
				deleteAndReconcile(name + "-kept")

				By("handing it back to Cloudflare with the reset of the resource that wrote it")
				fake.seed(zoneID, specs.unconfigured)
				createSynced(name+"-reset", c.spec)
				deleteAndReconcile(name + "-reset")
				createSynced(name+"-after", c.spec)
				deleteAndReconcile(name + "-after")
			}
		})

		It("takes settings it wrote and did not record for its own", func() {
			fake.seed(zoneID, configuredOutside())
			withFinalizer("unrecorded", specs.samples[0].spec,
				map[string]string{reconciler.CreatingAnnotation: zoneID})
			_, err := reconcileOnce("unrecorded")
			Expect(err).NotTo(HaveOccurred())
			Expect(ready("unrecorded").Reason).To(Equal("Synced"))
			Expect(get("unrecorded").GetAnnotations()).NotTo(HaveKey(reconciler.AdoptedAnnotation))

			By("resetting them with the resource")
			deleteAndReconcile("unrecorded")
			Expect(fake.resets).To(Equal(1))
		})

		It("reports NotFound with the adopt policy in an unconfigured zone, and writes with adopt-or-create", func() {
			withFinalizer("adopt-only", specs.samples[0].spec,
				map[string]string{reconciler.AdoptionPolicyAnnotation: reconciler.AdoptionPolicyAdopt})
			_, err := reconcileOnce("adopt-only")
			Expect(err).NotTo(HaveOccurred())
			Expect(ready("adopt-only").Reason).To(Equal(reasonNotFound))
			Expect(fake.writes).To(BeEmpty())
			deleteAndReconcile("adopt-only")

			withFinalizer("adopt-or-create", specs.samples[0].spec,
				map[string]string{reconciler.AdoptionPolicyAnnotation: reconciler.AdoptionPolicyAdoptOrCreate})
			_, err = reconcileOnce("adopt-or-create")
			Expect(err).NotTo(HaveOccurred())
			Expect(ready("adopt-or-create").Reason).To(Equal("Synced"))
			Expect(get("adopt-or-create").GetAnnotations()).NotTo(HaveKey(reconciler.AdoptedAnnotation))
			Expect(holds(specs.samples[0].settings)).To(BeTrue())
		})

		It("lets the oldest resource configure a zone, and hands the zone on once it is gone", func() {
			createSynced("oldest", specs.samples[0].spec)
			// The others are created a second later, the granularity of a
			// creation time, under names that sort before the oldest one's.
			time.Sleep(time.Second)

			By("refusing a younger resource for the zone, through another Zone")
			readyZone(zoneName+"-again", zoneID)
			younger := object("a-younger", specs.samples[len(specs.samples)-1].spec)
			younger.GetSettingsSpec().ZoneRef.Name = zoneName + "-again"
			younger.SetFinalizers([]string{reconciler.Finalizer})
			create(younger)
			writes := len(fake.writes)
			result, err := reconcileOnce("a-younger")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(settingsConflictRetryInterval))
			condition := ready("a-younger")
			Expect(condition.Reason).To(Equal("AlreadyConfigured"))
			Expect(condition.Message).To(ContainSubstring(ns + "/oldest"))
			Expect(get("a-younger").GetSettingsStatus().CloudflareMetadata.ZoneID).To(BeEmpty())
			Expect(fake.writes).To(HaveLen(writes))

			By("hearing of the oldest one's deletion, and of nothing else's")
			reqs := r.settings().othersInZoneOf(ctx, get("oldest"))
			Expect(reqs).To(ContainElement(reconcile.Request{NamespacedName: key("a-younger")}))
			Expect(r.settings().othersInZoneOf(ctx, &cloudflarev1alpha1.Zone{})).To(BeEmpty())

			By("deleting the younger one, which resets nothing, even if it was about to write the zone's settings")
			annotate("a-younger", map[string]string{reconciler.CreatingAnnotation: zoneID})
			Expect(k8sClient.Delete(ctx, get("a-younger"))).To(Succeed())
			_, err = reconcileOnce("a-younger")
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.resets).To(BeZero())

			By("handing the zone to another one once the oldest is gone")
			next := object("b-next", specs.samples[0].spec)
			next.SetFinalizers([]string{reconciler.Finalizer})
			create(next)
			_, err = reconcileOnce("b-next")
			Expect(err).NotTo(HaveOccurred())
			Expect(ready("b-next").Reason).To(Equal("AlreadyConfigured"))
			deleteAndReconcile("oldest")
			Expect(fake.resets).To(Equal(1))
			_, err = reconcileOnce("b-next")
			Expect(err).NotTo(HaveOccurred())
			Expect(ready("b-next").Reason).To(Equal("Synced"))
			Expect(get("b-next").GetSettingsStatus().CloudflareMetadata.ZoneID).To(Equal(zoneID))
		})

		It("is not held up by a resource that configures another zone", func() {
			readyZone(zoneName+"-elsewhere", zoneID+"-elsewhere")
			elsewhere := object("a-elsewhere", specs.samples[0].spec)
			elsewhere.GetSettingsSpec().ZoneRef.Name = zoneName + "-elsewhere"
			elsewhere.SetFinalizers([]string{reconciler.Finalizer})
			create(elsewhere)
			_, err := reconcileOnce("a-elsewhere")
			Expect(err).NotTo(HaveOccurred())
			Expect(ready("a-elsewhere").Reason).To(Equal("Synced"))
			createSynced("b-here", specs.samples[0].spec)
			Expect(get("b-here").GetSettingsStatus().CloudflareMetadata.ZoneID).To(Equal(zoneID))

			By("not hearing of a deletion in another zone")
			Expect(r.settings().othersInZoneOf(ctx, get("b-here"))).NotTo(
				ContainElement(reconcile.Request{NamespacedName: key("a-elsewhere")}))
		})

		It("leaves a zone with the resource that configures it, when an older one's Zone becomes ready", func() {
			late := &cloudflarev1alpha1.Zone{
				ObjectMeta: metav1.ObjectMeta{Name: zoneName + "-late", Namespace: ns},
				Spec: cloudflarev1alpha1.ZoneSpec{
					AccountRef: corev1.LocalObjectReference{Name: accountName},
					ZoneSpecExtras: cloudflarev1alpha1.ZoneSpecExtras{
						Name: prefix + ".example.com",
					},
				},
			}
			create(late)
			waiting := object("a-waiting", specs.samples[0].spec)
			waiting.GetSettingsSpec().ZoneRef.Name = late.Name
			waiting.SetFinalizers([]string{reconciler.Finalizer})
			create(waiting)
			_, err := reconcileOnce("a-waiting")
			Expect(err).NotTo(HaveOccurred())
			Expect(ready("a-waiting").Reason).To(Equal("ZoneNotReady"))
			createSynced("b-configures", specs.samples[0].spec)

			By("making the older one's Zone ready, for the same Cloudflare zone")
			Expect(k8sClient.Get(ctx, key(late.Name), late)).To(Succeed())
			late.Status.CloudflareMetadata.ZoneID = zoneID
			late.Status.Conditions = []metav1.Condition{{
				Type: cloudflarev1alpha1.ConditionReady, Status: metav1.ConditionTrue,
				Reason: "Synced", LastTransitionTime: metav1.Now(),
			}}
			Expect(k8sClient.Status().Update(ctx, late)).To(Succeed())
			writes := len(fake.writes)
			_, err = reconcileOnce("a-waiting")
			Expect(err).NotTo(HaveOccurred())
			condition := ready("a-waiting")
			Expect(condition.Reason).To(Equal("AlreadyConfigured"))
			Expect(condition.Message).To(ContainSubstring(ns + "/b-configures"))
			_, err = reconcileOnce("b-configures")
			Expect(err).NotTo(HaveOccurred())
			Expect(ready("b-configures").Reason).To(Equal("Synced"))
			Expect(fake.writes).To(HaveLen(writes))
		})

		It("leaves a zone two resources recorded with the older one, the younger forgetting it", func() {
			createSynced("a-recorded", specs.samples[0].spec)
			withFinalizer("b-recorded", specs.samples[0].spec, map[string]string{reconciler.CreatingAnnotation: zoneID})
			younger := get("b-recorded")
			younger.GetSettingsStatus().CloudflareMetadata.ZoneID = zoneID
			Expect(k8sClient.Status().Update(ctx, younger)).To(Succeed())

			_, err := reconcileOnce("b-recorded")
			Expect(err).NotTo(HaveOccurred())
			Expect(ready("b-recorded").Reason).To(Equal("AlreadyConfigured"))
			Expect(get("b-recorded").GetSettingsStatus().CloudflareMetadata.ZoneID).To(BeEmpty())
			Expect(get("b-recorded").GetAnnotations()).NotTo(HaveKey(reconciler.CreatingAnnotation))
			_, err = reconcileOnce("a-recorded")
			Expect(err).NotTo(HaveOccurred())
			Expect(ready("a-recorded").Reason).To(Equal("Synced"))

			By("resetting nothing when the younger one goes")
			deleteAndReconcile("b-recorded")
			Expect(fake.resets).To(BeZero())
		})

		It("goes by the resource it reconciles, not by the list's copy of it", func() {
			createSynced("listed", specs.samples[0].spec)
			// A copy as a cache may hold it, before its status forgot the zone.
			reconciled := get("listed")
			reconciled.GetSettingsStatus().CloudflareMetadata.ZoneID = ""
			owner, err := r.settings().owner(ctx, reconciled, zoneID)
			Expect(err).NotTo(HaveOccurred())
			Expect(owner).To(BeIdenticalTo(settingsObject(reconciled)))
		})

		It("resets the settings with the resource, and keeps them with the retain policy", func() {
			for _, retain := range []bool{false, true} {
				name := fmt.Sprintf("deleted-%t", retain)
				createSynced(name, specs.samples[0].spec)
				if retain {
					annotate(name, map[string]string{reconciler.DeletionPolicyAnnotation: reconciler.DeletionPolicyRetain})
				}
				resets := fake.resets
				deleteAndReconcile(name)
				Expect(fake.resets).To(Equal(resets + map[bool]int{false: 1, true: 0}[retain]))
				Expect(holds(specs.samples[0].settings)).To(Equal(retain))
				fake.seed(zoneID, specs.unconfigured)
			}

			By("resetting the zone a sync wrote but did not record")
			withFinalizer("unrecorded", specs.samples[0].spec, map[string]string{reconciler.CreatingAnnotation: zoneID})
			deleteAndReconcile("unrecorded")
			Expect(fake.resets).To(Equal(2))

			By("resetting nothing for a resource that never synced")
			withFinalizer("never-synced", specs.samples[0].spec, nil)
			deleteAndReconcile("never-synced")
			Expect(fake.resets).To(Equal(2))
		})

		// reconcilePaused reconciles the resource name, and checks that it made
		// no Cloudflare call, which the fake records the zone of, and scheduled
		// nothing.
		reconcilePaused := func(name string) {
			GinkgoHelper()
			calls := len(fake.zoneIDs)
			result, err := reconcileOnce(name)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeTrue())
			Expect(fake.zoneIDs).To(HaveLen(calls), "a paused resource called Cloudflare")
			Expect(ready(name).Reason).To(Equal(reasonPaused))
		}

		It("calls Cloudflare for nothing while paused, and writes its changed settings once unpaused", func() {
			sample, spec, settings := specs.samples[0], specs.samples[0].spec, specs.samples[0].settings
			if len(specs.changes) > 0 {
				change := specs.changes[0]
				sample, spec, settings = specs.samples[change.sample], change.spec, change.settings
			}
			createSynced("paused", sample.spec)
			observed := ready("paused").ObservedGeneration
			Expect(replaceSpec("paused", spec)).To(Succeed())
			setPaused(ctx, get("paused"), true)
			reconcilePaused("paused")
			Expect(ready("paused").Status).To(Equal(metav1.ConditionTrue))
			Expect(ready("paused").ObservedGeneration).To(Equal(observed), "the spec changed while paused is not observed")
			Expect(holds(sample.settings)).To(BeTrue(), "the settings are as the last sync left them")

			By("writing the changed settings once unpaused")
			setPaused(ctx, get("paused"), false)
			_, err := reconcileOnce("paused")
			Expect(err).NotTo(HaveOccurred())
			Expect(holds(settings)).To(BeTrue())
			Expect(ready("paused").Reason).To(Equal("Synced"))
			deleteAndReconcile("paused")
		})

		It("keeps a paused resource's settings and finalizer when it is deleted, and resets them once unpaused", func() {
			createSynced("paused-deleted", specs.samples[0].spec)
			setPaused(ctx, get("paused-deleted"), true)
			Expect(k8sClient.Delete(ctx, get("paused-deleted"))).To(Succeed())
			resets := fake.resets
			reconcilePaused("paused-deleted")
			Expect(fake.resets).To(Equal(resets))
			Expect(holds(specs.samples[0].settings)).To(BeTrue())
			Expect(get("paused-deleted").GetFinalizers()).To(ContainElement(reconciler.Finalizer))
			Expect(ready("paused-deleted").Message).To(Equal(pausedDeletionMessage))

			By("resetting them once unpaused")
			setPaused(ctx, get("paused-deleted"), false)
			_, err := reconcileOnce("paused-deleted")
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.resets).To(Equal(resets + 1))
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key("paused-deleted"), specs.newObject()))).To(BeTrue())
		})

		It("adds no finalizer to a resource created paused", func() {
			obj := object("created-paused", specs.samples[0].spec)
			obj.SetAnnotations(map[string]string{reconciler.PausedAnnotation: "true"})
			create(obj)
			reconcilePaused("created-paused")
			Expect(get("created-paused").GetFinalizers()).To(BeEmpty())
			Expect(ready("created-paused").Status).To(Equal(metav1.ConditionUnknown))
		})

		It("has nothing to do for a resource whose finalizer is gone", func() {
			result, err := r.settings().reconcileDelete(ctx, object("no-finalizer", specs.samples[0].spec))
			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeTrue())
			Expect(fake.zoneIDs).To(BeEmpty())
		})

		It("retries a failed reset, and takes a zone Cloudflare no longer has as reset", func() {
			createSynced("reset-fails", specs.samples[0].spec)
			fake.resetErr = errors.New("connection reset")
			Expect(k8sClient.Delete(ctx, get("reset-fails"))).To(Succeed())
			_, err := reconcileOnce("reset-fails")
			Expect(err).To(MatchError(ContainSubstring("connection reset")))
			Expect(get("reset-fails").GetFinalizers()).To(ContainElement(reconciler.Finalizer))

			fake.resetErr = v7APIError(http.MethodDelete, "zones/"+zoneID, http.StatusNotFound, 1001, "not found")
			_, err = reconcileOnce("reset-fails")
			Expect(err).NotTo(HaveOccurred())
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key("reset-fails"), specs.newObject()))).To(BeTrue())
		})

		It("leaves the settings as they are when its Zone is gone, or it cannot reach Cloudflare", func() {
			createSynced("zone-gone", specs.samples[0].spec)
			zone := &cloudflarev1alpha1.Zone{}
			Expect(k8sClient.Get(ctx, key(zoneName), zone)).To(Succeed())
			Expect(k8sClient.Delete(ctx, zone)).To(Succeed())
			deleteAndReconcile("zone-gone")
			Expect(fake.resets).To(BeZero())

			By("retrying while the account's credentials are missing")
			readyZone(zoneName, zoneID)
			fake.seed(zoneID, specs.unconfigured)
			createSynced("no-credentials", specs.samples[0].spec)
			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, key(secretName), secret)).To(Succeed())
			Expect(k8sClient.Delete(ctx, secret)).To(Succeed())
			Expect(k8sClient.Delete(ctx, get("no-credentials"))).To(Succeed())
			_, err := reconcileOnce("no-credentials")
			Expect(err).To(HaveOccurred())
			Expect(get("no-credentials").GetFinalizers()).To(ContainElement(reconciler.Finalizer))

			By("retrying while no Cloudflare client can be built")
			r = specs.newReconciler(k8sClient, func(string) (A, error) {
				var none A
				return none, errors.New("no client")
			})
			create(&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns},
				Data:       map[string][]byte{"CF_API_TOKEN": []byte("token")},
			})
			_, err = reconcileOnce("no-credentials")
			Expect(err).To(MatchError("no client"))
			Expect(fake.resets).To(BeZero())
		})

		It("waits for its Zone and its account", func() {
			missing := object("zone-missing", specs.samples[0].spec)
			missing.GetSettingsSpec().ZoneRef.Name = "no-such-zone"
			missing.SetFinalizers([]string{reconciler.Finalizer})
			create(missing)
			_, err := reconcileOnce("zone-missing")
			Expect(err).NotTo(HaveOccurred())
			Expect(ready("zone-missing").Reason).To(Equal("ZoneNotFound"))

			zone := &cloudflarev1alpha1.Zone{}
			Expect(k8sClient.Get(ctx, key(zoneName), zone)).To(Succeed())
			conditions := zone.Status.Conditions
			zone.Status.Conditions = nil
			Expect(k8sClient.Status().Update(ctx, zone)).To(Succeed())
			withFinalizer("zone-not-ready", specs.samples[0].spec, nil)
			_, err = reconcileOnce("zone-not-ready")
			Expect(err).NotTo(HaveOccurred())
			Expect(ready("zone-not-ready").Reason).To(Equal("ZoneNotReady"))

			By("waiting for the account once the Zone is ready")
			zone.Status.Conditions = conditions
			Expect(k8sClient.Status().Update(ctx, zone)).To(Succeed())
			account := &cloudflarev1alpha1.CloudflareAccount{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: accountName}, account)).To(Succeed())
			account.Status.Conditions = nil
			Expect(k8sClient.Status().Update(ctx, account)).To(Succeed())
			result, err := reconcileOnce("zone-not-ready")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(credentialsRetryInterval))
			Expect(ready("zone-not-ready").Reason).To(Equal("AccountNotReady"))
			Expect(fake.zoneIDs).To(BeEmpty())
		})

		It("reports an invalid token", func() {
			r = specs.newReconciler(k8sClient, func(string) (A, error) {
				var none A
				return none, errors.New("bad token")
			})
			withFinalizer("invalid-token", specs.samples[0].spec, nil)
			_, err := reconcileOnce("invalid-token")
			Expect(err).NotTo(HaveOccurred())
			condition := ready("invalid-token")
			Expect(condition.Reason).To(Equal("InvalidToken"))
			Expect(condition.Message).To(ContainSubstring("bad token"))
		})

		It("reports Cloudflare's errors, retrying those that may pass", func() {
			withFinalizer("errors", specs.samples[0].spec, nil)
			for _, c := range []struct {
				what     string
				set      func(error)
				terminal bool
			}{
				{"a refused read", func(err error) { fake.readErr = err }, true},
				{"a read that may pass", func(err error) { fake.readErr = err }, false},
				{"a refused write", func(err error) { fake.writeErr = err }, true},
				{"a write that may pass", func(err error) { fake.writeErr = err }, false},
			} {
				By(c.what)
				fake.readErr, fake.writeErr = nil, nil
				status, reason := http.StatusInternalServerError, "APIError"
				if c.terminal {
					status, reason = http.StatusBadRequest, "TerminalError"
				}
				c.set(settingsTestError(status, 1004))
				_, err := reconcileOnce("errors")
				if c.terminal {
					Expect(err).NotTo(HaveOccurred())
				} else {
					Expect(err).To(HaveOccurred())
				}
				Expect(ready("errors").Reason).To(Equal(reason))
			}
		})

		It("makes its Zone wait for it before the Zone is deleted, and releases the Zone once it is gone", func() {
			create(object("dependent", specs.samples[0].spec))
			dependents, err := dependentsOf(ctx, k8sClient, ns, zoneName, zoneDependents)
			Expect(err).NotTo(HaveOccurred())
			Expect(dependents).To(ContainElement(kind + " " + ns + "/dependent"))

			By("running the Zone controller, which must hear of the " + kind + "'s deletion")
			mgr, err := manager.New(cfg, manager.Options{
				Controller: config.Controller{SkipNameValidation: new(true)},
				Metrics:    metricsserver.Options{BindAddress: "0"},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect((&ZoneReconciler{
				Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
				NewZoneAPI: func(string) (ZoneAPI, error) { return nil, errors.New("this spec calls no Cloudflare API") },
			}).SetupWithManager(mgr)).To(Succeed())
			mgrCtx, stop := context.WithCancel(ctx)
			stopped := make(chan struct{})
			go func() {
				defer GinkgoRecover()
				defer close(stopped)
				Expect(mgr.Start(mgrCtx)).To(Succeed())
			}()
			// Stop it before the next spec, which may reconcile Zones itself.
			DeferCleanup(func() {
				stop()
				Eventually(stopped).Should(BeClosed())
			})

			By("deleting the Zone, retained, which waits for the " + kind)
			zone := &cloudflarev1alpha1.Zone{}
			Expect(k8sClient.Get(ctx, key(zoneName), zone)).To(Succeed())
			patch := client.MergeFrom(zone.DeepCopy())
			zone.Finalizers = append(zone.Finalizers, reconciler.Finalizer)
			zone.Annotations = map[string]string{reconciler.DeletionPolicyAnnotation: reconciler.DeletionPolicyRetain}
			Expect(k8sClient.Patch(ctx, zone, patch)).To(Succeed())
			Expect(k8sClient.Delete(ctx, zone)).To(Succeed())
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, key(zoneName), zone)).To(Succeed())
				condition := meta.FindStatusCondition(zone.Status.Conditions, cloudflarev1alpha1.ConditionReady)
				g.Expect(condition).NotTo(BeNil())
				g.Expect(condition.Reason).To(Equal("InUse"))
			}).WithTimeout(10 * time.Second).Should(Succeed())

			By("deleting the " + kind + ", which releases the Zone")
			Expect(k8sClient.Delete(ctx, get("dependent"))).To(Succeed())
			Eventually(func() bool {
				return apierrors.IsNotFound(k8sClient.Get(ctx, key(zoneName), &cloudflarev1alpha1.Zone{}))
			}).WithTimeout(10*time.Second).Should(BeTrue(), "the Zone was not re-triggered by the %s's deletion", kind)
		})

		It("is reconciled when its Zone becomes ready, and when the resource configuring its zone goes", func() {
			mgr, err := manager.New(cfg, manager.Options{
				Controller: config.Controller{SkipNameValidation: new(true)},
				Metrics:    metricsserver.Options{BindAddress: "0"},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(specs.newReconciler(mgr.GetClient(), func(string) (A, error) { return specs.newFake(fake), nil }).
				SetupWithManager(mgr)).To(Succeed())
			mgrCtx, stop := context.WithCancel(ctx)
			stopped := make(chan struct{})
			go func() {
				defer GinkgoRecover()
				defer close(stopped)
				Expect(mgr.Start(mgrCtx)).To(Succeed())
			}()
			stopManager := func() {
				stop()
				Eventually(stopped).Should(BeClosed())
			}
			DeferCleanup(stopManager)
			reason := func(name string) func() string {
				return func() string {
					obj := specs.newObject()
					if err := k8sClient.Get(ctx, key(name), obj); err != nil {
						return err.Error()
					}
					if condition := meta.FindStatusCondition(obj.GetConditions(),
						cloudflarev1alpha1.ConditionReady); condition != nil {
						return condition.Reason
					}
					return ""
				}
			}

			By("writing the settings once the Zone is ready")
			late := &cloudflarev1alpha1.Zone{
				ObjectMeta: metav1.ObjectMeta{Name: zoneName + "-watched", Namespace: ns},
				Spec: cloudflarev1alpha1.ZoneSpec{
					AccountRef: corev1.LocalObjectReference{Name: accountName},
					ZoneSpecExtras: cloudflarev1alpha1.ZoneSpecExtras{
						Name: prefix + ".example.com",
					},
				},
			}
			create(late)
			first := object("watched-first", specs.samples[0].spec)
			first.GetSettingsSpec().ZoneRef.Name = late.Name
			create(first)
			Eventually(reason("watched-first")).WithTimeout(20 * time.Second).Should(Equal("ZoneNotReady"))
			Expect(k8sClient.Get(ctx, key(late.Name), late)).To(Succeed())
			late.Status.CloudflareMetadata.ZoneID = zoneID
			late.Status.Conditions = []metav1.Condition{{
				Type: cloudflarev1alpha1.ConditionReady, Status: metav1.ConditionTrue,
				Reason: "Synced", LastTransitionTime: metav1.Now(),
			}}
			Expect(k8sClient.Status().Update(ctx, late)).To(Succeed())
			Eventually(reason("watched-first")).WithTimeout(20 * time.Second).Should(Equal("Synced"))

			By("handing the zone on at once, not at the next retry, when the resource configuring it goes")
			create(object("watched-second", specs.samples[0].spec))
			Eventually(reason("watched-second")).WithTimeout(20 * time.Second).Should(Equal("AlreadyConfigured"))
			Expect(k8sClient.Delete(ctx, get("watched-first"))).To(Succeed())
			Eventually(reason("watched-second")).WithTimeout(20 * time.Second).Should(Equal("Synced"))
			// Stopped before the cleanups delete what it reconciles.
			stopManager()
		})

		It("reconciles the resources of a Zone when it changes", func() {
			create(object("for-zone", specs.samples[0].spec))
			other := object("other-zone", specs.samples[0].spec)
			other.GetSettingsSpec().ZoneRef.Name = "another-zone"
			create(other)
			zone := &cloudflarev1alpha1.Zone{}
			Expect(k8sClient.Get(ctx, key(zoneName), zone)).To(Succeed())
			Expect(r.settings().resourcesForZone(ctx, zone)).To(ConsistOf(
				reconcile.Request{NamespacedName: key("for-zone")}))
		})

		It("converts specs that set every field into the settings they ask for", func() {
			Expect(specs.full).NotTo(BeEmpty())
			for i, full := range specs.full {
				By(full.what)
				desired, err := specs.kind.desired(object(fmt.Sprintf("full-%d", i), full.spec))
				Expect(err).NotTo(HaveOccurred())
				Expect(comparableSettings(string(desired))).To(Equal(comparableSettings(full.settings)))
			}
		})

		It("keeps zoneRef immutable", func() {
			obj := object("immutable", specs.samples[0].spec)
			create(obj)
			obj.GetSettingsSpec().ZoneRef.Name = "another-zone"
			Expect(k8sClient.Update(ctx, obj)).To(MatchError(ContainSubstring("zoneRef is immutable")))
		})

		It("refuses the specs its validation does not allow", func() {
			for i, invalid := range specs.invalid {
				By(invalid.what)
				err := k8sClient.Create(ctx, rawObject(fmt.Sprintf("invalid-%d", i), invalid.spec))
				Expect(err).To(MatchError(ContainSubstring(invalid.message)))
			}
		})

		It("registers with a manager, and falls back to the production client", func() {
			mgr, err := manager.New(cfg, manager.Options{
				Controller: config.Controller{SkipNameValidation: new(true)},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(specs.newReconciler(mgr.GetClient(), nil).SetupWithManager(mgr)).To(Succeed())
			_, err = specs.newReconciler(k8sClient, nil).settings().newAPI("")
			Expect(err).To(HaveOccurred(), "the production client rejects an empty token")
			_, err = specs.defaultAPI("")
			Expect(err).To(HaveOccurred())
		})

		It("is named after its API type", func() {
			gvk, err := apiutil.GVKForObject(specs.newObject(), k8sClient.Scheme())
			Expect(err).NotTo(HaveOccurred())
			Expect(gvk.Kind).To(Equal(kind))
			Expect(specs.kind.name).To(Equal(kind))
		})
	})
}
