/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"errors"
	"fmt"
	"go/types"
	"slices"
	"strings"
	"sync"
	"testing"

	"golang.org/x/tools/go/packages"
)

// testServices caches what the tests load: loading a service type-checks its
// package.
var (
	testServicesMu sync.Mutex
	testServices   = map[string]*service{}
)

// loadTestService loads a cloudflare-go v7 service, at the version go.mod
// pins.
func loadTestService(t *testing.T, name string) *service {
	t.Helper()
	testServicesMu.Lock()
	defer testServicesMu.Unlock()
	if svc, ok := testServices[name]; ok {
		return svc
	}
	svc, err := sdk{dir: "."}.service(name)
	if err != nil {
		t.Fatalf("loading %s: %v", name, err)
	}
	testServices[name] = svc
	return svc
}

var (
	testRulesMu sync.Mutex
	testRules   = map[string]*sdkRule{}
)

// loadTestRule loads a cloudflare-go v7 rule type once per test binary.
func loadTestRule(t *testing.T, name string) *sdkRule {
	t.Helper()
	testRulesMu.Lock()
	defer testRulesMu.Unlock()
	if rule, ok := testRules[name]; ok {
		return rule
	}
	rule, err := sdk{dir: "."}.rule(name)
	if err != nil {
		t.Fatalf("loading %s: %v", name, err)
	}
	testRules[name] = rule
	return rule
}

// testRuleLoader returns a loader of rule types for buildRuleKind, through
// loadTestRule.
func testRuleLoader(t *testing.T) func(name string) (*sdkRule, error) {
	t.Helper()
	return func(name string) (*sdkRule, error) { return loadTestRule(t, name), nil }
}

var (
	testSettingsMu sync.Mutex
	testSettings   = map[string]*settingsService{}
)

// loadTestSettings loads a cloudflare-go v7 service of settings once per
// test binary.
func loadTestSettings(t *testing.T, name string) *settingsService {
	t.Helper()
	testSettingsMu.Lock()
	defer testSettingsMu.Unlock()
	if svc, ok := testSettings[name]; ok {
		return svc
	}
	svc, err := sdk{dir: "."}.settings(name)
	if err != nil {
		t.Fatalf("loading %s: %v", name, err)
	}
	testSettings[name] = svc
	return svc
}

// summary describes fields compactly, one string each.
func summary(fields []sdkField) []string {
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		s := fmt.Sprintf("%s %s %s", f.in, f.apiName, types.TypeString(f.typ, (*types.Package).Name))
		if f.required {
			s += " required"
		}
		if len(f.enum) > 0 {
			s += " enum=" + strings.Join(f.enum, ",")
		}
		out = append(out, s)
	}
	return out
}

func TestServiceReadsTheKVNamespaceService(t *testing.T) {
	svc := loadTestService(t, "kv.NamespaceService")
	check := func(what string, got, want []string) {
		t.Helper()
		if !slices.Equal(got, want) {
			t.Errorf("%s = %q, want %q", what, got, want)
		}
	}
	check("create", summary(svc.create), []string{
		"path account_id string required",
		"body title string required",
		"body jurisdiction kv.NamespaceNewParamsJurisdiction enum=eu,fedramp,us",
		"body mode kv.NamespaceNewParamsMode enum=instant",
	})
	check("update", summary(svc.update), []string{"path account_id string required", "body title string required"})
	check("object", summary(svc.object), []string{
		"body id string required",
		"body title string required",
		"body jurisdiction kv.NamespaceJurisdiction enum=eu,fedramp,us",
		"body mode kv.NamespaceMode enum=instant",
		"body supports_url_encoding bool",
	})
	if svc.create[1].doc != "Human-readable string name for a Workers KV namespace." {
		t.Errorf("title's doc = %q", svc.create[1].doc)
	}
}

func TestServiceTakesEditWithoutUpdate(t *testing.T) {
	svc := loadTestService(t, "r2.BucketService")
	if got, want := summary(svc.update), []string{
		"path account_id string required",
		"header cf-r2-storage-class r2.BucketEditParamsCfR2StorageClass required enum=Standard,InfrequentAccess",
		"header cf-r2-jurisdiction r2.BucketEditParamsCfR2Jurisdiction enum=default,eu,us,fedramp,fedramp-high",
	}; !slices.Equal(got, want) {
		t.Errorf("update = %q, want %q", got, want)
	}
}

func TestServiceRefuses(t *testing.T) {
	for name, want := range map[string]string{
		"kv":                    "not <package>.<type>",
		"kv.":                   "not <package>.<type>",
		"kv.NoSuchService":      "has no type NoSuchService",
		"nosuchpackage.Service": "loading " + sdkModule + "/nosuchpackage",
		"dns.RecordService":     "kflare-gen cannot map unions yet",
	} {
		_, err := sdk{dir: "."}.service(name)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("service(%q): err = %v, want one containing %q", name, err, want)
		}
	}
}

func TestCallRefusesWhatIsNotACall(t *testing.T) {
	pkgs, err := packages.Load(&packages.Config{
		Mode: packages.NeedName | packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo,
	}, sdkModule+"/kv")
	if err != nil || len(pkgs) != 1 || len(pkgs[0].Errors) > 0 {
		t.Fatalf("loading kv: %v %v", err, pkgs)
	}
	r := newSDKReader(pkgs[0])
	typ := pkgs[0].Types.Scope().Lookup("NamespaceService").(*types.TypeName)
	methods := types.NewMethodSet(types.NewPointer(typ.Type()))

	if _, _, err := r.call(methods, "Rename"); !errors.Is(err, errNoMethod) {
		t.Errorf("call(Rename): err = %v, want errNoMethod", err)
	}
	if _, _, err := r.call(methods, "ListAutoPaging"); err == nil || !strings.Contains(err.Error(), "is not shaped") {
		t.Errorf("call(ListAutoPaging), which returns one value: err = %v", err)
	}
	if _, _, err := r.call(methods, "List"); err == nil || !strings.Contains(err.Error(), "is not a type of") {
		t.Errorf("call(List), which returns a pagination type: err = %v", err)
	}
	if _, err := r.fields(types.Typ[types.String], false); err == nil {
		t.Error("fields(string): err = nil")
	}
	if docs := r.fieldDocs("NoSuchType"); len(docs) != 0 {
		t.Errorf("fieldDocs(NoSuchType) = %v", docs)
	}
}

func TestServiceReadsRoutes(t *testing.T) {
	kv := loadTestService(t, "kv.NamespaceService")
	for name, got := range map[string]route{"New": kv.newRoute, "Get": kv.getRoute, "Update": kv.updateRoute} {
		want := map[string]string{
			"New":    "post /accounts/{account_id}/storage/kv/namespaces",
			"Get":    "get /accounts/{account_id}/storage/kv/namespaces/{namespace_id}",
			"Update": "put /accounts/{account_id}/storage/kv/namespaces/{namespace_id}",
		}[name]
		if got.String() != want {
			t.Errorf("%s's route = %s, want %s", name, got, want)
		}
	}
	if got := kv.listRoute.String(); got != "get /accounts/{account_id}/storage/kv/namespaces" {
		t.Errorf("List's route = %s", got)
	}
	r2 := loadTestService(t, "r2.BucketService")
	if got := r2.updateRoute.String(); got != "patch /accounts/{account_id}/r2/buckets/{bucket_name}" {
		t.Errorf("R2's Edit route = %s", got)
	}
}

func TestServiceReadsTheCallsAControllerMakes(t *testing.T) {
	kv := loadTestService(t, "kv.NamespaceService")
	if kv.pkg.Path() != sdkModule+"/kv" || kv.typeName != "NamespaceService" || kv.updateMethod != "Update" {
		t.Errorf("package %s, type %s, update %s", kv.pkg.Path(), kv.typeName, kv.updateMethod)
	}
	for name, params := range map[string]string{
		"New": "kv.NamespaceNewParams", "Get": "kv.NamespaceGetParams", "List": "kv.NamespaceListParams",
		"Update": "kv.NamespaceUpdateParams", "Delete": "kv.NamespaceDeleteParams",
	} {
		call := kv.calls[name]
		if call == nil || call.name != name || typeName(call.params) != params {
			t.Errorf("%s: %+v, want params %s", name, call, params)
		}
	}
	if got := summary(kv.calls["List"].paramFields); !slices.Equal(got, []string{
		"path account_id string required",
		"query direction kv.NamespaceListParamsDirection enum=asc,desc",
		"query order kv.NamespaceListParamsOrder enum=id,title",
		"query page float64",
		"query per_page float64",
	}) {
		t.Errorf("List's params = %q", got)
	}
	r2 := loadTestService(t, "r2.BucketService")
	if r2.updateMethod != "Edit" || r2.calls["Edit"] == nil || r2.calls["Update"] != nil {
		t.Errorf("R2's update is Edit: %s", r2.updateMethod)
	}
}

func TestParseRoutes(t *testing.T) {
	apiMD := `- <code title="post /accounts/{account_id}/things">client.Things.<a href="https://pkg.go.dev/` + sdkModule +
		`/things#ThingService.New">New</a>(ctx)</code>
- <code title="get /zones/{zone_id}/other">client.Other.<a href="https://pkg.go.dev/` + sdkModule +
		`/other#OtherService.Get">Get</a>(ctx)</code>`
	got := parseRoutes(apiMD, sdkModule+"/things")
	if len(got) != 1 || got["ThingService.New"] != (route{verb: "post", path: "/accounts/{account_id}/things"}) {
		t.Errorf("parseRoutes = %v, want only the package's own call", got)
	}
}
