/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"

	cf "github.com/cloudflare/cloudflare-go"
	"github.com/cloudflare/cloudflare-go/v7/accounts"
	"github.com/cloudflare/cloudflare-go/v7/kv"
	"github.com/cloudflare/cloudflare-go/v7/option"

	cfpkg "github.com/JonathanGraniero/kflare/pkg/cloudflare"
)

// cloudflareBaseURL is where cloudflare-go v7 sends calls, as the manager's
// clients set it.
const cloudflareBaseURL = "https://" + cloudflareHost + "/client/v4/"

// proxiedClient returns an HTTP client that reaches the stand-in as the
// manager does: through the proxy, trusting the measurement's CA.
func proxiedClient(t *testing.T, f *fakeCloudflare) *http.Client {
	t.Helper()
	authority, err := newCA()
	if err != nil {
		t.Fatal(err)
	}
	addr, stop, err := serveProxy(f, authority)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(authority.certPEM) {
		t.Fatal("the CA's PEM holds no certificate")
	}
	proxyURL := &url.URL{Scheme: "http", Host: addr}
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		Proxy:             http.ProxyURL(proxyURL),
		TLSClientConfig:   &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2: true,
	}}
}

func TestStandInAnswersTheSDKsThroughTheProxy(t *testing.T) {
	f := newFakeCloudflare(0)
	hc := proxiedClient(t, f)
	ctx := context.Background()
	opts := []option.RequestOption{option.WithHTTPClient(hc), option.WithBaseURL(cloudflareBaseURL),
		option.WithAPIToken("resync-token-0"), option.WithMaxRetries(0)}
	namespaces := kv.NewNamespaceService(opts...)
	account := accountID(0)

	var verify accounts.TokenVerifyParams
	verify.AccountID.Value, verify.AccountID.Present = account, true
	verified, err := accounts.NewTokenService(opts...).Verify(ctx, verify)
	if err != nil || verified.Status != accounts.TokenVerifyResponseStatusActive {
		t.Fatalf("Verify() = %+v, %v; want an active token", verified, err)
	}
	api, err := cf.NewWithAPIToken("resync-token-0", cf.HTTPClient(hc))
	if err != nil {
		t.Fatal(err)
	}
	read, _, err := api.Account(ctx, account)
	if err != nil || read.ID != account {
		t.Fatalf("Account() = %+v, %v; want account %s", read, err, account)
	}

	create := func(title string) (*kv.Namespace, error) {
		var params kv.NamespaceNewParams
		params.AccountID.Value, params.AccountID.Present = account, true
		params.Title.Value, params.Title.Present = title, true
		return namespaces.New(ctx, params)
	}
	var get kv.NamespaceGetParams
	get.AccountID.Value, get.AccountID.Present = account, true

	ids := make([]string, 0, 3)
	for _, title := range []string{"a", "b", "c"} {
		created, err := create(title)
		if err != nil {
			t.Fatalf("New(%s): %v", title, err)
		}
		ids = append(ids, created.ID)
	}
	if _, err = create("a"); !cfpkg.HasErrorCode(err, 10014) {
		t.Fatalf("New() of a title taken = %v, want code 10014", err)
	}

	page := func(n float64) []string {
		t.Helper()
		var params kv.NamespaceListParams
		params.AccountID.Value, params.AccountID.Present = account, true
		params.Page.Value, params.Page.Present = n, true
		params.PerPage.Value, params.PerPage.Present = 2, true
		listed, err := namespaces.List(ctx, params)
		if err != nil {
			t.Fatal(err)
		}
		if listed.ResultInfo.PerPage != 2 || listed.ResultInfo.Page != int64(n) {
			t.Fatalf("page %v reports %+v", n, listed.ResultInfo)
		}
		titles := make([]string, 0, len(listed.Result))
		for _, ns := range listed.Result {
			titles = append(titles, ns.Title)
		}
		return titles
	}
	if got := append(page(1), page(2)...); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Errorf("listed %v, want a, b and c", got)
	}

	f.startReading()
	for i, id := range ids[:2] {
		got, err := namespaces.Get(ctx, id, get)
		if err != nil || got.Title != []string{"a", "b"}[i] {
			t.Fatalf("Get(%s) = %+v, %v", id, got, err)
		}
	}
	if f.readAll() {
		t.Error("readAll() before the third namespace was read")
	}
	if _, err := namespaces.Get(ctx, ids[2], get); err != nil {
		t.Fatal(err)
	}
	if !f.readAll() {
		t.Error("readAll() after every namespace was read")
	}

	var update kv.NamespaceUpdateParams
	update.AccountID.Value, update.AccountID.Present = account, true
	update.Title.Value, update.Title.Present = "renamed", true
	updated, err := namespaces.Update(ctx, ids[0], update)
	if err != nil || updated.Title != "renamed" {
		t.Fatalf("Update() = %+v, %v", updated, err)
	}
	var del kv.NamespaceDeleteParams
	del.AccountID.Value, del.AccountID.Present = account, true
	if _, err := namespaces.Delete(ctx, ids[1], del); err != nil {
		t.Fatal(err)
	}
	if _, err := namespaces.Get(ctx, ids[1], get); !cfpkg.IsNotFound(err) {
		t.Fatalf("Get() of a deleted namespace = %v, want a 404", err)
	}
	if got := f.namespaces(); got != 2 {
		t.Errorf("namespaces() = %d, want 2", got)
	}
	if got := f.callCount(); got != 14 {
		t.Errorf("callCount() = %d, want 14", got)
	}
	if unknown := f.unknownCalls(); len(unknown) > 0 {
		t.Errorf("unknownCalls() = %v", unknown)
	}

	if _, err := api.ListZones(ctx); err == nil {
		t.Error("ListZones() succeeded against a stand-in that holds no zones")
	}
	if unknown := f.unknownCalls(); len(unknown) != 1 {
		t.Errorf("unknownCalls() = %v, want the zones list", unknown)
	}
}

func TestStandInWaitsItsLatency(t *testing.T) {
	f := newFakeCloudflare(150 * time.Millisecond)
	hc := proxiedClient(t, f)
	var verify accounts.TokenVerifyParams
	verify.AccountID.Value, verify.AccountID.Present = accountID(0), true
	opts := []option.RequestOption{option.WithHTTPClient(hc), option.WithBaseURL(cloudflareBaseURL),
		option.WithAPIToken("t")}
	start := time.Now()
	if _, err := accounts.NewTokenService(opts...).Verify(context.Background(), verify); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Errorf("a call took %s, want at least the latency", elapsed)
	}
}

func TestProxyReachesOnlyCloudflare(t *testing.T) {
	hc := proxiedClient(t, newFakeCloudflare(0))
	if _, err := hc.Get("https://example.com/"); err == nil {
		t.Error("the proxy reached example.com")
	}
}

func TestParseWorkers(t *testing.T) {
	got, err := parseWorkers("1, 4,16")
	if err != nil || !slices.Equal(got, []int{1, 4, 16}) {
		t.Errorf("parseWorkers() = %v, %v", got, err)
	}
	for _, bad := range []string{"", "0", "2,x", "-1"} {
		if _, err := parseWorkers(bad); err == nil {
			t.Errorf("parseWorkers(%q) succeeded", bad)
		}
	}
}

func TestValidate(t *testing.T) {
	t.Setenv("KUBEBUILDER_ASSETS", "/assets")
	good := options{manager: "main.go", accounts: 2, resources: 4, workers: []int{1}}
	if err := good.validate(); err != nil {
		t.Fatalf("validate() = %v", err)
	}
	for name, change := range map[string]func(*options){
		"no accounts":            func(o *options) { o.accounts = 0 },
		"fewer resources":        func(o *options) { o.resources = 1 },
		"negative latency":       func(o *options) { o.latency = -time.Second },
		"no workers":             func(o *options) { o.workers = nil },
		"missing manager binary": func(o *options) { o.manager = "no-such-manager" },
	} {
		o := good
		change(&o)
		if err := o.validate(); err == nil {
			t.Errorf("%s: validate() succeeded", name)
		}
	}
	t.Setenv("KUBEBUILDER_ASSETS", "")
	if err := good.validate(); err == nil {
		t.Error("validate() succeeded without KUBEBUILDER_ASSETS")
	}
}
