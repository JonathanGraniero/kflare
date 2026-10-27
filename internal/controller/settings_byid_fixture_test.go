/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"

	"github.com/cloudflare/cloudflare-go/v7/option"
	"github.com/cloudflare/cloudflare-go/v7/shared"
	"github.com/cloudflare/cloudflare-go/v7/zones"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	cfpkg "github.com/JonathanGraniero/kflare/pkg/cloudflare"
)

// SettingsByIDFixture is a settings kind of a service whose settings go by
// their IDs that only the tests know: the shared settings kind specs run on
// its table, so that the shared settings reconciler's settings by ID
// (settings_byid.go) are tested on their own, whatever settings kinds
// kflare-gen generates. It is kflare-gen's golden TestZoneSettings
// (generator/testdata) by another name, a zone's settings, each by its ID:
// the types, controller, fake and table kflare-gen writes for it under that
// name, and the deepcopy methods controller-gen writes for its types. Its CRD,
// testdata/crd/kflare.dev_settingsbyidfixtures.yaml, is controller-gen's too.

// SettingsByIDFixtureSpec defines the desired state of SettingsByIDFixture: its
// zone, and the zone settings it configures there.
type SettingsByIDFixtureSpec struct {
	cloudflarev1alpha1.SettingsSpec `json:",inline"`

	// Reply to all requests for URLs that use "http" with a 301 redirect to the
	// equivalent "https" URL. If you only want to redirect for a subset of requests,
	// consider creating an "Always use HTTPS" page rule.
	AlwaysUseHTTPS string `json:"alwaysUseHTTPS,omitempty"`

	// Browser Cache TTL (in seconds) specifies how long Cloudflare-cached resources
	// will remain on your visitors' computers. Cloudflare will honor any larger times
	// specified by your server.
	// (https://support.cloudflare.com/hc/en-us/articles/200168276).
	BrowserCacheTTL *int64 `json:"browserCacheTTL,omitempty"`

	// Cloudflare security header for a zone.
	SecurityHeader *SettingsByIDFixtureSecurityHeader `json:"securityHeader,omitempty"`

	// SSL encrypts your visitor's connection and safeguards credit card numbers and
	// other personal data to and from your website. SSL can take up to 5 minutes to
	// fully activate. Requires Cloudflare active on your root domain or www domain.
	// Off: no SSL between the visitor and Cloudflare, and no SSL between Cloudflare
	// and your web server (all HTTP traffic). Flexible: SSL between the visitor and
	// Cloudflare -- visitor sees HTTPS on your site, but no SSL between Cloudflare and
	// your web server. You don't need to have an SSL cert on your web server, but your
	// visitors will still see the site as being HTTPS enabled. Full: SSL between the
	// visitor and Cloudflare -- visitor sees HTTPS on your site, and SSL between
	// Cloudflare and your web server. You'll need to have your own SSL cert or
	// self-signed cert at the very least. Full (Strict): SSL between the visitor and
	// Cloudflare -- visitor sees HTTPS on your site, and SSL between Cloudflare and
	// your web server. You'll need to have a valid SSL certificate installed on your
	// web server. This certificate must be signed by a certificate authority, have an
	// expiration date in the future, and respond for the request domain name
	// (hostname). (https://support.cloudflare.com/hc/en-us/articles/200170416).
	SSL string `json:"ssl,omitempty"`
}

// SettingsByIDFixtureSecurityHeader is security_header in a
// SettingsByIDFixture.
type SettingsByIDFixtureSecurityHeader struct {
	// Strict Transport Security.
	StrictTransportSecurity *SettingsByIDFixtureSecurityHeaderStrictTransportSecurity `json:"strictTransportSecurity,omitempty"`
}

// SettingsByIDFixtureSecurityHeaderStrictTransportSecurity is
// security_header.strict_transport_security in a SettingsByIDFixture.
type SettingsByIDFixtureSecurityHeaderStrictTransportSecurity struct {
	// Whether or not strict transport security is enabled.
	Enabled *bool `json:"enabled,omitempty"`

	// Include all subdomains for strict transport security.
	IncludeSubdomains *bool `json:"includeSubdomains,omitempty"`

	// Max age in seconds of the strict transport security.
	MaxAge *int64 `json:"maxAge,omitempty"`

	// Whether or not to include 'X-Content-Type-Options: nosniff' header.
	Nosniff *bool `json:"nosniff,omitempty"`

	// Enable automatic preload of the HSTS configuration.
	Preload *bool `json:"preload,omitempty"`
}

// SettingsByIDFixture configures a zone's settings, each by its ID.
type SettingsByIDFixture struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SettingsByIDFixtureSpec           `json:"spec,omitempty"`
	Status cloudflarev1alpha1.SettingsStatus `json:"status,omitempty"`
}

// SettingsByIDFixtureList contains a list of SettingsByIDFixture.
type SettingsByIDFixtureList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SettingsByIDFixture `json:"items"`
}

// The fixture's deepcopy methods, as controller-gen writes them for a kind.

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *SettingsByIDFixture) DeepCopyInto(out *SettingsByIDFixture) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new SettingsByIDFixture.
func (in *SettingsByIDFixture) DeepCopy() *SettingsByIDFixture {
	if in == nil {
		return nil
	}
	out := new(SettingsByIDFixture)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject is an autogenerated deepcopy function, copying the receiver, creating a new runtime.Object.
func (in *SettingsByIDFixture) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *SettingsByIDFixtureList) DeepCopyInto(out *SettingsByIDFixtureList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		in, out := &in.Items, &out.Items
		*out = make([]SettingsByIDFixture, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new SettingsByIDFixtureList.
func (in *SettingsByIDFixtureList) DeepCopy() *SettingsByIDFixtureList {
	if in == nil {
		return nil
	}
	out := new(SettingsByIDFixtureList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject is an autogenerated deepcopy function, copying the receiver, creating a new runtime.Object.
func (in *SettingsByIDFixtureList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *SettingsByIDFixtureSecurityHeader) DeepCopyInto(out *SettingsByIDFixtureSecurityHeader) {
	*out = *in
	if in.StrictTransportSecurity != nil {
		in, out := &in.StrictTransportSecurity, &out.StrictTransportSecurity
		*out = new(SettingsByIDFixtureSecurityHeaderStrictTransportSecurity)
		(*in).DeepCopyInto(*out)
	}
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new SettingsByIDFixtureSecurityHeader.
func (in *SettingsByIDFixtureSecurityHeader) DeepCopy() *SettingsByIDFixtureSecurityHeader {
	if in == nil {
		return nil
	}
	out := new(SettingsByIDFixtureSecurityHeader)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *SettingsByIDFixtureSecurityHeaderStrictTransportSecurity) DeepCopyInto(out *SettingsByIDFixtureSecurityHeaderStrictTransportSecurity) {
	*out = *in
	if in.Enabled != nil {
		in, out := &in.Enabled, &out.Enabled
		*out = new(bool)
		**out = **in
	}
	if in.IncludeSubdomains != nil {
		in, out := &in.IncludeSubdomains, &out.IncludeSubdomains
		*out = new(bool)
		**out = **in
	}
	if in.MaxAge != nil {
		in, out := &in.MaxAge, &out.MaxAge
		*out = new(int64)
		**out = **in
	}
	if in.Nosniff != nil {
		in, out := &in.Nosniff, &out.Nosniff
		*out = new(bool)
		**out = **in
	}
	if in.Preload != nil {
		in, out := &in.Preload, &out.Preload
		*out = new(bool)
		**out = **in
	}
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new SettingsByIDFixtureSecurityHeaderStrictTransportSecurity.
func (in *SettingsByIDFixtureSecurityHeaderStrictTransportSecurity) DeepCopy() *SettingsByIDFixtureSecurityHeaderStrictTransportSecurity {
	if in == nil {
		return nil
	}
	out := new(SettingsByIDFixtureSecurityHeaderStrictTransportSecurity)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *SettingsByIDFixtureSpec) DeepCopyInto(out *SettingsByIDFixtureSpec) {
	*out = *in
	out.SettingsSpec = in.SettingsSpec
	if in.BrowserCacheTTL != nil {
		in, out := &in.BrowserCacheTTL, &out.BrowserCacheTTL
		*out = new(int64)
		**out = **in
	}
	if in.SecurityHeader != nil {
		in, out := &in.SecurityHeader, &out.SecurityHeader
		*out = new(SettingsByIDFixtureSecurityHeader)
		(*in).DeepCopyInto(*out)
	}
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new SettingsByIDFixtureSpec.
func (in *SettingsByIDFixtureSpec) DeepCopy() *SettingsByIDFixtureSpec {
	if in == nil {
		return nil
	}
	out := new(SettingsByIDFixtureSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *SettingsByIDFixture) GetConditions() []metav1.Condition { return in.Status.Conditions }

func (in *SettingsByIDFixture) SetConditions(c []metav1.Condition) { in.Status.Conditions = c }

func (in *SettingsByIDFixture) GetSettingsSpec() *cloudflarev1alpha1.SettingsSpec {
	return &in.Spec.SettingsSpec
}

func (in *SettingsByIDFixture) GetSettingsStatus() *cloudflarev1alpha1.SettingsStatus {
	return &in.Status
}

// The scheme the suite's client uses knows the fixture, and a Zone waits for
// its resources, as it does for every generated settings kind's.
func init() {
	scheme.Scheme.AddKnownTypes(cloudflarev1alpha1.GroupVersion, &SettingsByIDFixture{}, &SettingsByIDFixtureList{})
	zoneDependents = append(zoneDependents,
		dependents("SettingsByIDFixture", func() client.ObjectList { return &SettingsByIDFixtureList{} },
			func(obj *SettingsByIDFixture) []string { return []string{obj.Spec.ZoneRef.Name} }))
}

// SettingsByIDFixtureAPI is the part of cloudflare-go v7's zones.SettingService
// that the SettingsByIDFixture reconciler calls. Tests inject a fake; in
// production it is a *zones.SettingService.
type SettingsByIDFixtureAPI interface {
	Get(ctx context.Context, settingID string, query zones.SettingGetParams,
		opts ...option.RequestOption) (*zones.SettingGetResponse, error)
	Edit(ctx context.Context, settingID string, params zones.SettingEditParams,
		opts ...option.RequestOption) (*zones.SettingEditResponse, error)
}

var _ SettingsByIDFixtureAPI = (*zones.SettingService)(nil)

// SettingsByIDFixtureReconciler reconciles a SettingsByIDFixture object, with
// the shared settings controller (settings_controller.go).
type SettingsByIDFixtureReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// NewSettingsByIDFixtureAPI constructs a SettingsByIDFixtureAPI from a raw
	// API token. Defaults to defaultSettingsByIDFixtureAPI; tests inject a
	// fake.
	NewSettingsByIDFixtureAPI func(token string) (SettingsByIDFixtureAPI, error)
}

// +kubebuilder:rbac:groups=kflare.dev,resources=settingsbyidfixtures,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kflare.dev,resources=settingsbyidfixtures/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kflare.dev,resources=settingsbyidfixtures/finalizers,verbs=update
// +kubebuilder:rbac:groups=kflare.dev,resources=zones,verbs=get;list;watch
// +kubebuilder:rbac:groups=kflare.dev,resources=cloudflareaccounts,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// settingsByIDFixtureKind describes SettingsByIDFixtures to the shared settings
// reconciler. A SettingsByIDFixture configures the zone settings of its zone,
// each by its ID: it writes the settings its spec sets, and resets them when it
// is deleted. A zone nobody has configured holds always_use_https "off",
// browser_cache_ttl float64(14400),
// security_header.strict_transport_security.enabled false,
// security_header.strict_transport_security.include_subdomains false,
// security_header.strict_transport_security.max_age float64(0),
// security_header.strict_transport_security.nosniff false,
// security_header.strict_transport_security.preload false and
// ssl_automatic_mode "auto". Cloudflare picks ssl itself while
// ssl_automatic_mode is "auto".
var settingsByIDFixtureKind = settingsKind[SettingsByIDFixtureAPI]{
	name:      "SettingsByIDFixture",
	noun:      "zone settings",
	newObject: func() settingsObject { return &SettingsByIDFixture{} },
	newList:   func() client.ObjectList { return &SettingsByIDFixtureList{} },

	byID: true,

	read: func(ctx context.Context, api SettingsByIDFixtureAPI, zoneID string, obj settingsObject) (string, error) {
		spec := obj.(*SettingsByIDFixture).Spec
		return readSettingsByID(ctx, settingIDs(settingsByIDFixtureWrites(spec), settingsByIDFixtureResets(spec)), func(ctx context.Context, id string) (string, error) {
			var params zones.SettingGetParams
			params.ZoneID.Value, params.ZoneID.Present = zoneID, true
			setting, err := api.Get(ctx, id, params)
			if err != nil {
				return "", err
			}
			return setting.JSON.RawJSON(), nil
		})
	},
	write: func(ctx context.Context, api SettingsByIDFixtureAPI, zoneID string, obj settingsObject, ids []string) error {
		return writeSettingsByID(ctx, settingsNamed(settingsByIDFixtureWrites(obj.(*SettingsByIDFixture).Spec), ids), settingsByIDFixtureEdit(api, zoneID))
	},
	reset: func(ctx context.Context, api SettingsByIDFixtureAPI, zoneID string, obj settingsObject) error {
		return writeSettingsByID(ctx, settingsByIDFixtureResets(obj.(*SettingsByIDFixture).Spec), settingsByIDFixtureEdit(api, zoneID))
	},
	desired: func(obj settingsObject) ([]byte, error) {
		return settingsByIDJSON(settingsByIDFixtureWrites(obj.(*SettingsByIDFixture).Spec), settingsByIDFixtureBody)
	},

	shape: settingsShape{},
	unconfigured: []settingsValue{
		{path: "always_use_https", value: "off"},
		{path: "browser_cache_ttl", value: float64(14400)},
		{path: "security_header.strict_transport_security.enabled", value: false},
		{path: "security_header.strict_transport_security.include_subdomains", value: false},
		{path: "security_header.strict_transport_security.max_age", value: float64(0)},
		{path: "security_header.strict_transport_security.nosniff", value: false},
		{path: "security_header.strict_transport_security.preload", value: false},
		{path: "ssl_automatic_mode", value: "auto"},
	},
}

func (r *SettingsByIDFixtureReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	return r.settings().reconcile(ctx, req)
}

// SetupWithManager registers SettingsByIDFixtureReconciler with the manager. It
// watches Zones, so a SettingsByIDFixture waiting for its Zone is reconciled
// once the Zone is ready, and the deletion of SettingsByIDFixtures, so another
// one takes over the zone's zone settings at once.
func (r *SettingsByIDFixtureReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return r.settings().setup(mgr, r)
}

// settings returns the shared settings reconciler for SettingsByIDFixtures,
// with the injected factory or the production one.
func (r *SettingsByIDFixtureReconciler) settings() *settingsReconciler[SettingsByIDFixtureAPI] {
	newAPI := r.NewSettingsByIDFixtureAPI
	if newAPI == nil {
		newAPI = defaultSettingsByIDFixtureAPI
	}
	return &settingsReconciler[SettingsByIDFixtureAPI]{Client: r.Client, kind: settingsByIDFixtureKind, newAPI: newAPI}
}

// defaultSettingsByIDFixtureAPI is the production factory: cloudflare-go v7's
// zones.SettingService, built from cfpkg.New's options for the token.
func defaultSettingsByIDFixtureAPI(token string) (SettingsByIDFixtureAPI, error) {
	client, err := cfpkg.New(token)
	if err != nil {
		return nil, err
	}
	return zones.NewSettingService(client.V7Options()...), nil
}

// settingsByIDFixtureWrite is a write of one setting of a SettingsByIDFixture:
// the setting's ID, and its value as cloudflare-go v7's Edit takes it.
type settingsByIDFixtureWrite = settingValue[zones.SettingEditParamsBodyValueValueUnion]

// settingsByIDFixtureEdit returns the write of one setting of the zone zoneID,
// with cloudflare-go v7's Edit.
func settingsByIDFixtureEdit(api SettingsByIDFixtureAPI, zoneID string) func(context.Context, string, zones.SettingEditParamsBodyValueValueUnion) error {
	return func(ctx context.Context, id string, value zones.SettingEditParamsBodyValueValueUnion) error {
		var params zones.SettingEditParams
		params.ZoneID.Value, params.ZoneID.Present = zoneID, true
		params.Body = settingsByIDFixtureBody(value)
		_, err := api.Edit(ctx, id, params)
		return err
	}
}

// settingsByIDFixtureBody returns the body with which cloudflare-go v7's Edit
// writes a setting's value.
func settingsByIDFixtureBody(value zones.SettingEditParamsBodyValueValueUnion) zones.SettingEditParamsBodyValue {
	var body zones.SettingEditParamsBodyValue
	body.Value.Value, body.Value.Present = value, true
	return body
}

// settingsByIDFixtureWrites returns the settings a SettingsByIDFixture asks
// for, each by its ID, as cloudflare-go v7's Edit writes them.
func settingsByIDFixtureWrites(in SettingsByIDFixtureSpec) []settingsByIDFixtureWrite {
	var out []settingsByIDFixtureWrite
	if in.AlwaysUseHTTPS != "" {
		out = append(out, settingsByIDFixtureWrite{id: "always_use_https", value: shared.UnionString(in.AlwaysUseHTTPS)})
	}
	if in.BrowserCacheTTL != nil {
		out = append(out, settingsByIDFixtureWrite{id: "browser_cache_ttl", value: shared.UnionFloat(*in.BrowserCacheTTL)})
	}
	if in.SecurityHeader != nil {
		out = append(out, settingsByIDFixtureWrite{id: "security_header", value: settingsByIDFixtureSecurityHeaderParam(*in.SecurityHeader)})
	}
	if in.SSL != "" {
		out = append(out, settingsByIDFixtureWrite{id: "ssl", value: shared.UnionString(in.SSL)})
	}
	return out
}

// settingsByIDFixtureResets returns the writes that reset the settings a
// SettingsByIDFixture asks for, each to the value a zone nobody has configured
// holds; ssl through ssl_automatic_mode "auto", which hands it back to
// Cloudflare.
func settingsByIDFixtureResets(in SettingsByIDFixtureSpec) []settingsByIDFixtureWrite {
	var out []settingsByIDFixtureWrite
	if in.AlwaysUseHTTPS != "" {
		out = append(out, settingsByIDFixtureWrite{id: "always_use_https", value: shared.UnionString("off")})
	}
	if in.BrowserCacheTTL != nil {
		out = append(out, settingsByIDFixtureWrite{id: "browser_cache_ttl", value: shared.UnionFloat(14400)})
	}
	if in.SecurityHeader != nil {
		out = append(out, settingsByIDFixtureWrite{id: "security_header", value: settingsByIDFixtureSecurityHeaderParam(SettingsByIDFixtureSecurityHeader{StrictTransportSecurity: &SettingsByIDFixtureSecurityHeaderStrictTransportSecurity{Enabled: new(false), IncludeSubdomains: new(false), MaxAge: new(int64(0)), Nosniff: new(false), Preload: new(false)}})})
	}
	if in.SSL != "" {
		out = append(out, settingsByIDFixtureWrite{id: "ssl_automatic_mode", value: shared.UnionString("auto")})
	}
	return out
}

// settingsByIDFixtureSecurityHeaderParam returns security_header of a
// SettingsByIDFixture as cloudflare-go v7 sends it.
func settingsByIDFixtureSecurityHeaderParam(in SettingsByIDFixtureSecurityHeader) zones.SettingEditParamsBodyValueValueZonesSecurityHeaderValue {
	var out zones.SettingEditParamsBodyValueValueZonesSecurityHeaderValue
	if in.StrictTransportSecurity != nil {
		out.StrictTransportSecurity.Value, out.StrictTransportSecurity.Present = settingsByIDFixtureSecurityHeaderStrictTransportSecurityParam(*in.StrictTransportSecurity), true
	}
	return out
}

// settingsByIDFixtureSecurityHeaderStrictTransportSecurityParam returns
// security_header.strict_transport_security of a SettingsByIDFixture as
// cloudflare-go v7 sends it.
func settingsByIDFixtureSecurityHeaderStrictTransportSecurityParam(in SettingsByIDFixtureSecurityHeaderStrictTransportSecurity) zones.SettingEditParamsBodyValueValueZonesSecurityHeaderValueStrictTransportSecurity {
	var out zones.SettingEditParamsBodyValueValueZonesSecurityHeaderValueStrictTransportSecurity
	if in.Enabled != nil {
		out.Enabled.Value, out.Enabled.Present = *in.Enabled, true
	}
	if in.IncludeSubdomains != nil {
		out.IncludeSubdomains.Value, out.IncludeSubdomains.Present = *in.IncludeSubdomains, true
	}
	if in.MaxAge != nil {
		out.MaxAge.Value, out.MaxAge.Present = float64(*in.MaxAge), true
	}
	if in.Nosniff != nil {
		out.Nosniff.Value, out.Nosniff.Present = *in.Nosniff, true
	}
	if in.Preload != nil {
		out.Preload.Value, out.Preload.Present = *in.Preload, true
	}
	return out
}

// fakeSettingsByIDFixtureAPI is an in-memory zones.SettingService for
// SettingsByIDFixture's generated specs: it keeps each zone's zone settings in
// a fakeSettingsStore, by their IDs.
type fakeSettingsByIDFixtureAPI struct {
	*fakeSettingsStore
}

var _ SettingsByIDFixtureAPI = fakeSettingsByIDFixtureAPI{}

func (f fakeSettingsByIDFixtureAPI) Get(_ context.Context, settingID string, query zones.SettingGetParams, _ ...option.RequestOption) (*zones.SettingGetResponse, error) {
	return fakeSettingReadByID[zones.SettingGetResponse](f.fakeSettingsStore, query.ZoneID.Value, settingID)
}

func (f fakeSettingsByIDFixtureAPI) Edit(_ context.Context, settingID string, params zones.SettingEditParams, _ ...option.RequestOption) (*zones.SettingEditResponse, error) {
	return fakeSettingWriteByID[zones.SettingEditResponse](f.fakeSettingsStore, params.ZoneID.Value, settingID, params)
}

// The shared settings kind specs, on SettingsByIDFixture's sample and tests,
// the edits of them, and specs that set every field.
var _ = describeSettingsKind(settingsKindSpecs[*SettingsByIDFixture, SettingsByIDFixtureAPI]{
	kind:      settingsByIDFixtureKind,
	newObject: func() *SettingsByIDFixture { return &SettingsByIDFixture{} },
	newReconciler: func(c client.Client, newAPI func(string) (SettingsByIDFixtureAPI, error)) settingsKindReconciler[SettingsByIDFixtureAPI] {
		return &SettingsByIDFixtureReconciler{Client: c, Scheme: c.Scheme(), NewSettingsByIDFixtureAPI: newAPI}
	},
	newFake:    func(store *fakeSettingsStore) SettingsByIDFixtureAPI { return fakeSettingsByIDFixtureAPI{store} },
	defaultAPI: defaultSettingsByIDFixtureAPI,
	picks: map[string]settingsValue{
		"ssl": {path: "ssl_automatic_mode", value: "custom"},
	},
	unconfigured: `{"always_use_https":"off","browser_cache_ttl":14400,"security_header":{"strict_transport_security":{"enabled":false,"include_subdomains":false,"max_age":0,"nosniff":false,"preload":false}},"ssl":"off","ssl_automatic_mode":"auto"}`,
	configured: []settingsSpecCase{
		{
			what:     "always_use_https \"on\"",
			settings: `{"always_use_https":"on","browser_cache_ttl":14400,"security_header":{"strict_transport_security":{"enabled":false,"include_subdomains":false,"max_age":0,"nosniff":false,"preload":false}},"ssl":"off","ssl_automatic_mode":"auto"}`,
		},
		{
			what:     "browser_cache_ttl 14401",
			settings: `{"always_use_https":"off","browser_cache_ttl":14401,"security_header":{"strict_transport_security":{"enabled":false,"include_subdomains":false,"max_age":0,"nosniff":false,"preload":false}},"ssl":"off","ssl_automatic_mode":"auto"}`,
		},
		{
			what:     "security_header.strict_transport_security.enabled true",
			settings: `{"always_use_https":"off","browser_cache_ttl":14400,"security_header":{"strict_transport_security":{"enabled":true,"include_subdomains":false,"max_age":0,"nosniff":false,"preload":false}},"ssl":"off","ssl_automatic_mode":"auto"}`,
		},
		{
			what:     "security_header.strict_transport_security.include_subdomains true",
			settings: `{"always_use_https":"off","browser_cache_ttl":14400,"security_header":{"strict_transport_security":{"enabled":false,"include_subdomains":true,"max_age":0,"nosniff":false,"preload":false}},"ssl":"off","ssl_automatic_mode":"auto"}`,
		},
		{
			what:     "security_header.strict_transport_security.max_age 1",
			settings: `{"always_use_https":"off","browser_cache_ttl":14400,"security_header":{"strict_transport_security":{"enabled":false,"include_subdomains":false,"max_age":1,"nosniff":false,"preload":false}},"ssl":"off","ssl_automatic_mode":"auto"}`,
		},
		{
			what:     "security_header.strict_transport_security.nosniff true",
			settings: `{"always_use_https":"off","browser_cache_ttl":14400,"security_header":{"strict_transport_security":{"enabled":false,"include_subdomains":false,"max_age":0,"nosniff":true,"preload":false}},"ssl":"off","ssl_automatic_mode":"auto"}`,
		},
		{
			what:     "security_header.strict_transport_security.preload true",
			settings: `{"always_use_https":"off","browser_cache_ttl":14400,"security_header":{"strict_transport_security":{"enabled":false,"include_subdomains":false,"max_age":0,"nosniff":false,"preload":true}},"ssl":"off","ssl_automatic_mode":"auto"}`,
		},
		{
			what:     "ssl_automatic_mode \"custom\"",
			settings: `{"always_use_https":"off","browser_cache_ttl":14400,"security_header":{"strict_transport_security":{"enabled":false,"include_subdomains":false,"max_age":0,"nosniff":false,"preload":false}},"ssl":"off","ssl_automatic_mode":"custom"}`,
		},
	},
	samples: []settingsSpecCase{
		{
			what:     "the sample",
			spec:     `{"alwaysUseHTTPS":"on","browserCacheTTL":7200,"securityHeader":{"strictTransportSecurity":{"enabled":true,"maxAge":31536000}},"ssl":"strict"}`,
			settings: `{"always_use_https":"on","browser_cache_ttl":7200,"security_header":{"strict_transport_security":{"enabled":true,"max_age":31536000}},"ssl":"strict"}`,
		},
		{
			what:     "test 1",
			spec:     `{"securityHeader":{"strictTransportSecurity":{"includeSubdomains":true,"nosniff":true,"preload":true}},"ssl":"flexible"}`,
			settings: `{"security_header":{"strict_transport_security":{"include_subdomains":true,"nosniff":true,"preload":true}},"ssl":"flexible"}`,
		},
	},
	changes: []settingsSpecCase{
		{
			what:     "a new alwaysUseHTTPS",
			spec:     `{"alwaysUseHTTPS":"off","browserCacheTTL":7200,"securityHeader":{"strictTransportSecurity":{"enabled":true,"maxAge":31536000}},"ssl":"strict"}`,
			settings: `{"always_use_https":"off","browser_cache_ttl":7200,"security_header":{"strict_transport_security":{"enabled":true,"max_age":31536000}},"ssl":"strict"}`,
		},
		{
			what:     "a new browserCacheTTL",
			spec:     `{"alwaysUseHTTPS":"on","browserCacheTTL":7201,"securityHeader":{"strictTransportSecurity":{"enabled":true,"maxAge":31536000}},"ssl":"strict"}`,
			settings: `{"always_use_https":"on","browser_cache_ttl":7201,"security_header":{"strict_transport_security":{"enabled":true,"max_age":31536000}},"ssl":"strict"}`,
		},
		{
			what:     "a new securityHeader.strictTransportSecurity.enabled",
			spec:     `{"alwaysUseHTTPS":"on","browserCacheTTL":7200,"securityHeader":{"strictTransportSecurity":{"enabled":false,"maxAge":31536000}},"ssl":"strict"}`,
			settings: `{"always_use_https":"on","browser_cache_ttl":7200,"security_header":{"strict_transport_security":{"enabled":false,"max_age":31536000}},"ssl":"strict"}`,
		},
		{
			what:     "a new securityHeader.strictTransportSecurity.maxAge",
			spec:     `{"alwaysUseHTTPS":"on","browserCacheTTL":7200,"securityHeader":{"strictTransportSecurity":{"enabled":true,"maxAge":31536001}},"ssl":"strict"}`,
			settings: `{"always_use_https":"on","browser_cache_ttl":7200,"security_header":{"strict_transport_security":{"enabled":true,"max_age":31536001}},"ssl":"strict"}`,
		},
		{
			what:     "a new ssl",
			spec:     `{"alwaysUseHTTPS":"on","browserCacheTTL":7200,"securityHeader":{"strictTransportSecurity":{"enabled":true,"maxAge":31536000}},"ssl":"off"}`,
			settings: `{"always_use_https":"on","browser_cache_ttl":7200,"security_header":{"strict_transport_security":{"enabled":true,"max_age":31536000}},"ssl":"off"}`,
		},
		{
			what:     "a new securityHeader.strictTransportSecurity.includeSubdomains",
			spec:     `{"securityHeader":{"strictTransportSecurity":{"includeSubdomains":false,"nosniff":true,"preload":true}},"ssl":"flexible"}`,
			sample:   1,
			settings: `{"security_header":{"strict_transport_security":{"include_subdomains":false,"nosniff":true,"preload":true}},"ssl":"flexible"}`,
		},
		{
			what:     "a new securityHeader.strictTransportSecurity.nosniff",
			spec:     `{"securityHeader":{"strictTransportSecurity":{"includeSubdomains":true,"nosniff":false,"preload":true}},"ssl":"flexible"}`,
			sample:   1,
			settings: `{"security_header":{"strict_transport_security":{"include_subdomains":true,"nosniff":false,"preload":true}},"ssl":"flexible"}`,
		},
		{
			what:     "a new securityHeader.strictTransportSecurity.preload",
			spec:     `{"securityHeader":{"strictTransportSecurity":{"includeSubdomains":true,"nosniff":true,"preload":false}},"ssl":"flexible"}`,
			sample:   1,
			settings: `{"security_header":{"strict_transport_security":{"include_subdomains":true,"nosniff":true,"preload":false}},"ssl":"flexible"}`,
		},
		{
			what:     "a new ssl",
			spec:     `{"securityHeader":{"strictTransportSecurity":{"includeSubdomains":true,"nosniff":true,"preload":true}},"ssl":"off"}`,
			sample:   1,
			settings: `{"security_header":{"strict_transport_security":{"include_subdomains":true,"nosniff":true,"preload":true}},"ssl":"off"}`,
		},
	},
	invalid: []settingsSpecCase{
		{
			what:    "alwaysUseHTTPS outside its enum",
			spec:    `{"alwaysUseHTTPS":"kflare-not-in-enum","browserCacheTTL":7200,"securityHeader":{"strictTransportSecurity":{"enabled":true,"maxAge":31536000}},"ssl":"strict"}`,
			message: "spec.alwaysUseHTTPS: Unsupported value",
		},
		{
			what:    "browserCacheTTL below its minimum",
			spec:    `{"alwaysUseHTTPS":"on","browserCacheTTL":-1,"securityHeader":{"strictTransportSecurity":{"enabled":true,"maxAge":31536000}},"ssl":"strict"}`,
			message: "spec.browserCacheTTL",
		},
		{
			what:    "browserCacheTTL above its maximum",
			spec:    `{"alwaysUseHTTPS":"on","browserCacheTTL":31536001,"securityHeader":{"strictTransportSecurity":{"enabled":true,"maxAge":31536000}},"ssl":"strict"}`,
			message: "spec.browserCacheTTL",
		},
		{
			what:    "securityHeader.strictTransportSecurity.maxAge below its minimum",
			spec:    `{"alwaysUseHTTPS":"on","browserCacheTTL":7200,"securityHeader":{"strictTransportSecurity":{"enabled":true,"maxAge":-1}},"ssl":"strict"}`,
			message: "spec.securityHeader.strictTransportSecurity.maxAge",
		},
		{
			what:    "ssl outside its enum",
			spec:    `{"alwaysUseHTTPS":"on","browserCacheTTL":7200,"securityHeader":{"strictTransportSecurity":{"enabled":true,"maxAge":31536000}},"ssl":"kflare-not-in-enum"}`,
			message: "spec.ssl: Unsupported value",
		},
	},
	full: []settingsSpecCase{
		{
			what:     "every field set",
			spec:     `{"alwaysUseHTTPS":"off","browserCacheTTL":7200,"securityHeader":{"strictTransportSecurity":{"enabled":true,"includeSubdomains":true,"maxAge":31536000,"nosniff":true,"preload":true}},"ssl":"off"}`,
			settings: `{"always_use_https":"off","browser_cache_ttl":7200,"security_header":{"strict_transport_security":{"enabled":true,"include_subdomains":true,"max_age":31536000,"nosniff":true,"preload":true}},"ssl":"off"}`,
		},
		{
			what:     "every field set, optional scalars to their zero values where allowed",
			spec:     `{"alwaysUseHTTPS":"off","browserCacheTTL":0,"securityHeader":{"strictTransportSecurity":{"enabled":false,"includeSubdomains":false,"maxAge":0,"nosniff":false,"preload":false}},"ssl":"off"}`,
			settings: `{"always_use_https":"off","browser_cache_ttl":0,"security_header":{"strict_transport_security":{"enabled":false,"include_subdomains":false,"max_age":0,"nosniff":false,"preload":false}},"ssl":"off"}`,
		},
	},
	picked: []settingsSpecCase{
		{
			what:     "ssl alone",
			spec:     `{"ssl":"flexible"}`,
			settings: `{"ssl":"flexible"}`,
		},
	},
})
