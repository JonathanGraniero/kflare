/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"

	"github.com/cloudflare/cloudflare-go/v7/option"
	"github.com/cloudflare/cloudflare-go/v7/url_normalization"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	cfpkg "github.com/JonathanGraniero/kflare/pkg/cloudflare"
)

// SettingsFixture is a settings kind that only the tests know: the shared
// settings kind specs run on its table, so that the shared settings
// reconciler is tested on its own, whatever settings kinds kflare-gen
// generates. It is kflare-gen's golden TestURLNormalization
// (generator/testdata) by another name, a zone's URL normalization: the same
// types, controller, fake and table. Its CRD,
// testdata/crd/kflare.dev_settingsfixtures.yaml, is what controller-gen writes
// for its types.

// SettingsFixtureSpec defines the desired state of SettingsFixture: its zone,
// and the URL normalization it configures there.
type SettingsFixtureSpec struct {
	cloudflarev1alpha1.SettingsSpec `json:",inline"`

	// The scope of the URL normalization.
	Scope string `json:"scope"`

	// The type of URL normalization performed by Cloudflare.
	Type string `json:"type"`
}

// SettingsFixture configures how Cloudflare normalizes the URLs of a zone's
// requests.
type SettingsFixture struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SettingsFixtureSpec               `json:"spec,omitempty"`
	Status cloudflarev1alpha1.SettingsStatus `json:"status,omitempty"`
}

// SettingsFixtureList contains a list of SettingsFixture.
type SettingsFixtureList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SettingsFixture `json:"items"`
}

// The fixture's deepcopy methods, as controller-gen writes them for a kind.

func (in *SettingsFixture) DeepCopyInto(out *SettingsFixture) {
	*out = *in
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *SettingsFixture) DeepCopyObject() runtime.Object {
	out := new(SettingsFixture)
	in.DeepCopyInto(out)
	return out
}

func (in *SettingsFixtureList) DeepCopyInto(out *SettingsFixtureList) {
	*out = *in
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	out.Items = make([]SettingsFixture, len(in.Items))
	for i := range in.Items {
		in.Items[i].DeepCopyInto(&out.Items[i])
	}
}

func (in *SettingsFixtureList) DeepCopyObject() runtime.Object {
	out := new(SettingsFixtureList)
	in.DeepCopyInto(out)
	return out
}

func (in *SettingsFixture) GetConditions() []metav1.Condition { return in.Status.Conditions }

func (in *SettingsFixture) SetConditions(c []metav1.Condition) { in.Status.Conditions = c }

func (in *SettingsFixture) GetSettingsSpec() *cloudflarev1alpha1.SettingsSpec {
	return &in.Spec.SettingsSpec
}

func (in *SettingsFixture) GetSettingsStatus() *cloudflarev1alpha1.SettingsStatus { return &in.Status }

// The scheme the suite's client uses knows the fixture, and a Zone waits for
// its resources, as it does for every generated settings kind's.
func init() {
	scheme.Scheme.AddKnownTypes(cloudflarev1alpha1.GroupVersion, &SettingsFixture{}, &SettingsFixtureList{})
	zoneDependents = append(zoneDependents,
		dependents("SettingsFixture", func() client.ObjectList { return &SettingsFixtureList{} },
			func(obj *SettingsFixture) []string { return []string{obj.Spec.ZoneRef.Name} }))
}

// SettingsFixtureAPI is the part of cloudflare-go v7's
// url_normalization.URLNormalizationService that the SettingsFixture
// reconciler calls.
type SettingsFixtureAPI interface {
	Get(ctx context.Context, query url_normalization.URLNormalizationGetParams,
		opts ...option.RequestOption) (*url_normalization.URLNormalizationGetResponse, error)
	Update(ctx context.Context, params url_normalization.URLNormalizationUpdateParams,
		opts ...option.RequestOption) (*url_normalization.URLNormalizationUpdateResponse, error)
	Delete(ctx context.Context, body url_normalization.URLNormalizationDeleteParams,
		opts ...option.RequestOption) error
}

var _ SettingsFixtureAPI = (*url_normalization.URLNormalizationService)(nil)

// SettingsFixtureReconciler reconciles a SettingsFixture object, with the
// shared settings controller.
type SettingsFixtureReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// NewSettingsFixtureAPI constructs a SettingsFixtureAPI from a raw API
	// token. Defaults to defaultSettingsFixtureAPI; the specs inject a fake.
	NewSettingsFixtureAPI func(token string) (SettingsFixtureAPI, error)
}

// settingsFixtureKind describes SettingsFixtures to the shared settings
// reconciler. A zone nobody has configured holds scope "incoming" and type
// "cloudflare".
var settingsFixtureKind = settingsKind[SettingsFixtureAPI]{
	name:      "SettingsFixture",
	noun:      "URL normalization",
	newObject: func() settingsObject { return &SettingsFixture{} },
	newList:   func() client.ObjectList { return &SettingsFixtureList{} },

	read: func(ctx context.Context, api SettingsFixtureAPI, zoneID string) (string, error) {
		var params url_normalization.URLNormalizationGetParams
		params.ZoneID.Value, params.ZoneID.Present = zoneID, true
		settings, err := api.Get(ctx, params)
		if err != nil {
			return "", err
		}
		return settings.JSON.RawJSON(), nil
	},
	write: func(ctx context.Context, api SettingsFixtureAPI, zoneID string, obj settingsObject) error {
		params := settingsFixtureParams(obj.(*SettingsFixture).Spec)
		params.ZoneID.Value, params.ZoneID.Present = zoneID, true
		_, err := api.Update(ctx, params)
		return err
	},
	reset: func(ctx context.Context, api SettingsFixtureAPI, zoneID string) error {
		var params url_normalization.URLNormalizationDeleteParams
		params.ZoneID.Value, params.ZoneID.Present = zoneID, true
		return api.Delete(ctx, params)
	},
	desired: func(obj settingsObject) ([]byte, error) {
		return settingsFixtureParams(obj.(*SettingsFixture).Spec).MarshalJSON()
	},

	shape: settingsShape{},
	unconfigured: []settingsValue{
		{path: "scope", value: "incoming"},
		{path: "type", value: "cloudflare"},
	},
}

func (r *SettingsFixtureReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	return r.settings().reconcile(ctx, req)
}

func (r *SettingsFixtureReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return r.settings().setup(mgr, r)
}

func (r *SettingsFixtureReconciler) settings() *settingsReconciler[SettingsFixtureAPI] {
	newAPI := r.NewSettingsFixtureAPI
	if newAPI == nil {
		newAPI = defaultSettingsFixtureAPI
	}
	return &settingsReconciler[SettingsFixtureAPI]{Client: r.Client, kind: settingsFixtureKind, newAPI: newAPI}
}

func defaultSettingsFixtureAPI(token string) (SettingsFixtureAPI, error) {
	client, err := cfpkg.New(token)
	if err != nil {
		return nil, err
	}
	return url_normalization.NewURLNormalizationService(client.V7Options()...), nil
}

// settingsFixtureParams returns the settings a SettingsFixture asks for, as
// cloudflare-go v7's Update sends them, but for the zone.
func settingsFixtureParams(in SettingsFixtureSpec) url_normalization.URLNormalizationUpdateParams {
	var out url_normalization.URLNormalizationUpdateParams
	out.Scope.Value, out.Scope.Present = url_normalization.URLNormalizationUpdateParamsScope(in.Scope), true
	out.Type.Value, out.Type.Present = url_normalization.URLNormalizationUpdateParamsType(in.Type), true
	return out
}

// fakeSettingsFixtureAPI keeps each zone's URL normalization in a
// fakeSettingsStore.
type fakeSettingsFixtureAPI struct {
	*fakeSettingsStore
}

var _ SettingsFixtureAPI = fakeSettingsFixtureAPI{}

func (f fakeSettingsFixtureAPI) Get(_ context.Context, query url_normalization.URLNormalizationGetParams,
	_ ...option.RequestOption) (*url_normalization.URLNormalizationGetResponse, error) {
	return fakeSettingsRead[url_normalization.URLNormalizationGetResponse](f.fakeSettingsStore, query.ZoneID.Value)
}

func (f fakeSettingsFixtureAPI) Update(_ context.Context, params url_normalization.URLNormalizationUpdateParams,
	_ ...option.RequestOption) (*url_normalization.URLNormalizationUpdateResponse, error) {
	return fakeSettingsWrite[url_normalization.URLNormalizationUpdateResponse](f.fakeSettingsStore,
		params.ZoneID.Value, params)
}

func (f fakeSettingsFixtureAPI) Delete(_ context.Context, body url_normalization.URLNormalizationDeleteParams,
	_ ...option.RequestOption) error {
	return f.reset(body.ZoneID.Value)
}

var _ = describeSettingsKind(settingsKindSpecs[*SettingsFixture, SettingsFixtureAPI]{
	kind:      settingsFixtureKind,
	newObject: func() *SettingsFixture { return &SettingsFixture{} },
	newReconciler: func(c client.Client,
		newAPI func(string) (SettingsFixtureAPI, error)) settingsKindReconciler[SettingsFixtureAPI] {
		return &SettingsFixtureReconciler{Client: c, Scheme: c.Scheme(), NewSettingsFixtureAPI: newAPI}
	},
	newFake:      func(store *fakeSettingsStore) SettingsFixtureAPI { return fakeSettingsFixtureAPI{store} },
	defaultAPI:   defaultSettingsFixtureAPI,
	unconfigured: `{"scope":"incoming","type":"cloudflare"}`,
	samples: []settingsSpecCase{
		{what: "the sample", spec: `{"scope":"both","type":"rfc3986"}`, settings: `{"scope":"both","type":"rfc3986"}`},
	},
	changes: []settingsSpecCase{
		{what: "a new scope", spec: `{"scope":"incoming","type":"rfc3986"}`,
			settings: `{"scope":"incoming","type":"rfc3986"}`},
		{what: "a new type", spec: `{"scope":"both","type":"cloudflare"}`,
			settings: `{"scope":"both","type":"cloudflare"}`},
	},
	invalid: []settingsSpecCase{
		{what: "scope left out", spec: `{"type":"rfc3986"}`, message: "spec.scope: Required value"},
		{what: "type left out", spec: `{"scope":"both"}`, message: "spec.type: Required value"},
		{what: "scope outside its enum", spec: `{"scope":"kflare-not-in-enum","type":"rfc3986"}`,
			message: "spec.scope: Unsupported value"},
		{what: "type outside its enum", spec: `{"scope":"both","type":"kflare-not-in-enum"}`,
			message: "spec.type: Unsupported value"},
	},
	full: []settingsSpecCase{
		{what: "every field set", spec: `{"scope":"incoming","type":"cloudflare"}`,
			settings: `{"scope":"incoming","type":"cloudflare"}`},
	},
})
