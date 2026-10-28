/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package metricsauth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/testr"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// The API server the tests review requests against: envtest's, which
// authenticates ServiceAccount tokens and authorizes with RBAC, as a cluster
// does. `make test` points KUBEBUILDER_ASSETS at its binaries. It starts with
// the first test that asks for it (apiServer), so that a fuzz worker, which
// runs TestMain too, starts none.
var (
	testEnv      envtest.Environment
	startOnce    sync.Once
	startErr     error
	apiConfig    *rest.Config
	apiClientset *kubernetes.Clientset
)

// apiServer starts the API server if no test has yet, and returns its config
// and a clientset for it.
func apiServer(t *testing.T) (*rest.Config, *kubernetes.Clientset) {
	t.Helper()
	startOnce.Do(func() {
		if apiConfig, startErr = testEnv.Start(); startErr != nil {
			return
		}
		apiClientset, startErr = kubernetes.NewForConfig(apiConfig)
	})
	if startErr != nil {
		t.Fatalf("starting envtest: %v", startErr)
	}
	return apiConfig, apiClientset
}

func TestMain(m *testing.M) {
	code := m.Run()
	if apiConfig != nil {
		if err := testEnv.Stop(); err != nil {
			fmt.Fprintln(os.Stderr, "stopping envtest:", err)
		}
	}
	os.Exit(code)
}

// serve starts a server that answers every path with 200 and "ok" behind
// filter.
func serve(t *testing.T, filter func(t *testing.T, h http.Handler) http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(filter(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})))
	t.Cleanup(srv.Close)
	return srv
}

// filterOf wraps a handler with f, as the metrics server does.
func filterOf(f func(logr.Logger, http.Handler) (http.Handler, error)) func(*testing.T, http.Handler) http.Handler {
	return func(t *testing.T, h http.Handler) http.Handler {
		t.Helper()
		wrapped, err := f(testr.New(t), h)
		if err != nil {
			t.Fatalf("wrapping the handler: %v", err)
		}
		return wrapped
	}
}

// response is what a request got back.
type response struct {
	code            int
	body            string
	wwwAuthenticate string
}

// do sends method to srv's path with the Authorization header auth (none when
// empty).
func do(t *testing.T, srv *httptest.Server, method, path, auth string) response {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response{
		code:            resp.StatusCode,
		body:            strings.TrimSpace(string(body)),
		wwwAuthenticate: resp.Header.Get("WWW-Authenticate"),
	}
}

// serviceAccountToken creates a ServiceAccount named name in the default
// namespace and returns a token for it.
func serviceAccountToken(t *testing.T, name string) string {
	t.Helper()
	_, clientset := apiServer(t)
	ctx := context.Background()
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}
	if _, err := clientset.CoreV1().ServiceAccounts("default").Create(ctx, sa, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating ServiceAccount %s: %v", name, err)
	}
	t.Cleanup(func() {
		_ = clientset.CoreV1().ServiceAccounts("default").Delete(ctx, name, metav1.DeleteOptions{})
	})
	tr, err := clientset.CoreV1().ServiceAccounts("default").CreateToken(ctx, name,
		&authenticationv1.TokenRequest{}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("requesting a token for %s: %v", name, err)
	}
	return tr.Status.Token
}

// grantGet binds the ServiceAccount name to a new ClusterRole that grants get
// on path, as the chart's metrics-reader role does for /metrics. It returns a
// function that deletes the binding.
func grantGet(t *testing.T, name, path string) (revoke func()) {
	t.Helper()
	_, clientset := apiServer(t)
	ctx := context.Background()
	role := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Rules:      []rbacv1.PolicyRule{{NonResourceURLs: []string{path}, Verbs: []string{"get"}}},
	}
	if _, err := clientset.RbacV1().ClusterRoles().Create(ctx, role, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating ClusterRole %s: %v", name, err)
	}
	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: name},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: name, Namespace: "default"}},
	}
	if _, err := clientset.RbacV1().ClusterRoleBindings().Create(ctx, binding, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating ClusterRoleBinding %s: %v", name, err)
	}
	revoke = func() {
		_ = clientset.RbacV1().ClusterRoleBindings().Delete(ctx, name, metav1.DeleteOptions{})
	}
	t.Cleanup(func() {
		revoke()
		_ = clientset.RbacV1().ClusterRoles().Delete(ctx, name, metav1.DeleteOptions{})
	})
	return revoke
}

// eventually retries check for up to ten seconds, since RBAC changes reach the
// API server's authorizer through its informers.
func eventually(t *testing.T, what string, check func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !check(); time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
	}
}

func TestWithAuthenticationAndAuthorization(t *testing.T) {
	cfg, _ := apiServer(t)
	httpClient, err := rest.HTTPClientFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	filter, err := WithAuthenticationAndAuthorization(cfg, httpClient)
	if err != nil {
		t.Fatal(err)
	}
	srv := serve(t, filterOf(filter))

	reader := serviceAccountToken(t, "metrics-reader")
	grantGet(t, "metrics-reader", "/metrics")
	other := serviceAccountToken(t, "not-a-reader")
	eventually(t, "the reader's role binding applies", func() bool {
		return do(t, srv, http.MethodGet, "/metrics", "Bearer "+reader).code == http.StatusOK
	})

	tests := []struct {
		name   string
		method string
		path   string
		auth   string
		want   response
	}{
		{"no token", http.MethodGet, "/metrics", "",
			response{http.StatusUnauthorized, "Unauthorized", `Bearer realm="metrics"`}},
		{"another scheme", http.MethodGet, "/metrics", "Basic " + reader,
			response{http.StatusUnauthorized, "Unauthorized", `Bearer realm="metrics"`}},
		{"an empty bearer token", http.MethodGet, "/metrics", "Bearer   ",
			response{http.StatusUnauthorized, "Unauthorized", `Bearer realm="metrics"`}},
		{"a token the API server does not accept", http.MethodGet, "/metrics", "Bearer not-a-token",
			response{http.StatusUnauthorized, "Unauthorized", `Bearer realm="metrics"`}},
		{"a ServiceAccount without the role", http.MethodGet, "/metrics", "Bearer " + other,
			response{code: http.StatusForbidden, body: "Forbidden"}},
		{"a ServiceAccount with the role", http.MethodGet, "/metrics", "Bearer " + reader,
			response{code: http.StatusOK, body: "ok"}},
		{"the scheme in lower case", http.MethodGet, "/metrics", "bearer " + reader,
			response{code: http.StatusOK, body: "ok"}},
		{"the role, on another path", http.MethodGet, "/debug", "Bearer " + reader,
			response{code: http.StatusForbidden, body: "Forbidden"}},
		{"the role, with another verb", http.MethodPost, "/metrics", "Bearer " + reader,
			response{code: http.StatusForbidden, body: "Forbidden"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := do(t, srv, tt.method, tt.path, tt.auth); got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestWithAuthenticationAndAuthorizationRefusesABadConfig(t *testing.T) {
	_, err := WithAuthenticationAndAuthorization(&rest.Config{Host: "http://[::1"}, http.DefaultClient)
	if err == nil {
		t.Fatal("expected an error for a host that is not a URL")
	}
}

func TestReviewsAreRateLimited(t *testing.T) {
	cfg, _ := apiServer(t)
	// A config whose own limit would let every review through at once.
	unlimited := rest.CopyConfig(cfg)
	unlimited.QPS, unlimited.Burst = 1000, 1000
	httpClient, err := rest.HTTPClientFor(unlimited)
	if err != nil {
		t.Fatal(err)
	}
	filter, err := WithAuthenticationAndAuthorization(unlimited, httpClient)
	if err != nil {
		t.Fatal(err)
	}
	srv := serve(t, filterOf(filter))

	// Each request sends one TokenReview: a burst of ten goes through at once,
	// and the other five wait a fifth of a second each.
	const requests = 15
	start := time.Now()
	var wg sync.WaitGroup
	for i := range requests {
		wg.Go(func() {
			if got := do(t, srv, http.MethodGet, "/metrics", fmt.Sprintf("Bearer not-a-token-%d", i)); got.code !=
				http.StatusUnauthorized {
				t.Errorf("request %d: got %d, want %d", i, got.code, http.StatusUnauthorized)
			}
		})
	}
	wg.Wait()
	if elapsed := time.Since(start); elapsed < 800*time.Millisecond {
		t.Errorf("%d reviews took %v, want about a second: 5 a second after a burst of 10", requests, elapsed)
	}
}

// clock is a time source the test moves by hand.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestAnAllowedRequestIsRememberedForAMinute(t *testing.T) {
	_, clientset := apiServer(t)
	newClocked := func() (*reviewer, *clock) {
		c := &clock{now: time.Now()}
		return newReviewer(clientset.AuthenticationV1().TokenReviews(),
			clientset.AuthorizationV1().SubjectAccessReviews(), c.Now), c
	}
	r, c := newClocked()
	srv := serve(t, filterOf(r.filter))

	token := serviceAccountToken(t, "revoked-reader")
	revoke := grantGet(t, "revoked-reader", "/metrics")
	eventually(t, "the role binding applies", func() bool {
		return do(t, srv, http.MethodGet, "/metrics", "Bearer "+token).code == http.StatusOK
	})

	revoke()
	// Wait until the API server itself refuses the token, through a reviewer
	// that remembers nothing yet.
	fresh, _ := newClocked()
	freshSrv := serve(t, filterOf(fresh.filter))
	eventually(t, "the API server sees the binding is gone", func() bool {
		return do(t, freshSrv, http.MethodGet, "/metrics", "Bearer "+token).code == http.StatusForbidden
	})

	c.Advance(59 * time.Second)
	if got := do(t, srv, http.MethodGet, "/metrics", "Bearer "+token).code; got != http.StatusOK {
		t.Errorf("within a minute of being allowed: got %d, want %d (remembered)", got, http.StatusOK)
	}
	c.Advance(time.Second)
	if got := do(t, srv, http.MethodGet, "/metrics", "Bearer "+token).code; got != http.StatusForbidden {
		t.Errorf("a minute after being allowed: got %d, want %d (reviewed again)", got, http.StatusForbidden)
	}
}

func TestExpiredEntriesAreForgotten(t *testing.T) {
	c := &clock{now: time.Now()}
	r := newReviewer(nil, nil, c.Now)
	first := allowKey{verb: "get", path: "/metrics", token: [32]byte{1}}
	second := allowKey{verb: "get", path: "/metrics", token: [32]byte{2}}

	r.allow(first)
	c.Advance(time.Minute)
	if r.isAllowed(first) {
		t.Error("an entry is still allowed after allowTTL")
	}
	r.allow(second)
	if _, ok := r.allowed[first]; ok {
		t.Error("allowing a key kept an expired one")
	}
	if !r.isAllowed(second) {
		t.Error("a key allowed just now is not allowed")
	}
}

// fakeTokenReviews answers TokenReviews with status, or fails with err.
type fakeTokenReviews struct {
	status authenticationv1.TokenReviewStatus
	err    error
	tokens []string
}

func (f *fakeTokenReviews) Create(_ context.Context, tr *authenticationv1.TokenReview,
	_ metav1.CreateOptions) (*authenticationv1.TokenReview, error) {
	f.tokens = append(f.tokens, tr.Spec.Token)
	if f.err != nil {
		return nil, f.err
	}
	return &authenticationv1.TokenReview{Spec: tr.Spec, Status: f.status}, nil
}

// fakeAccessReviews answers SubjectAccessReviews with allowed, or fails with
// err, and records the specs it was sent.
type fakeAccessReviews struct {
	allowed bool
	err     error
	specs   []authorizationv1.SubjectAccessReviewSpec
}

func (f *fakeAccessReviews) Create(_ context.Context, sar *authorizationv1.SubjectAccessReview,
	_ metav1.CreateOptions) (*authorizationv1.SubjectAccessReview, error) {
	f.specs = append(f.specs, sar.Spec)
	if f.err != nil {
		return nil, f.err
	}
	return &authorizationv1.SubjectAccessReview{
		Spec:   sar.Spec,
		Status: authorizationv1.SubjectAccessReviewStatus{Allowed: f.allowed},
	}, nil
}

var prometheus = authenticationv1.UserInfo{
	Username: "system:serviceaccount:monitoring:prometheus",
	UID:      "1234",
	Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:monitoring"},
	Extra:    map[string]authenticationv1.ExtraValue{"authentication.kubernetes.io/credential-id": {"JTI=abc"}},
}

func TestTheAccessReviewDescribesTheRequest(t *testing.T) {
	tokens := &fakeTokenReviews{status: authenticationv1.TokenReviewStatus{Authenticated: true, User: prometheus}}
	access := &fakeAccessReviews{allowed: true}
	srv := serve(t, filterOf(newReviewer(tokens, access, time.Now).filter))

	if got := do(t, srv, http.MethodGet, "/metrics", "Bearer  the-token "); got.code != http.StatusOK {
		t.Fatalf("got %d, want %d", got.code, http.StatusOK)
	}
	if len(tokens.tokens) != 1 || tokens.tokens[0] != "the-token" {
		t.Errorf("reviewed tokens %q, want [the-token]", tokens.tokens)
	}
	want := authorizationv1.SubjectAccessReviewSpec{
		User:   prometheus.Username,
		UID:    prometheus.UID,
		Groups: prometheus.Groups,
		Extra:  map[string]authorizationv1.ExtraValue{"authentication.kubernetes.io/credential-id": {"JTI=abc"}},
		NonResourceAttributes: &authorizationv1.NonResourceAttributes{
			Path: "/metrics",
			Verb: "get",
		},
	}
	if len(access.specs) != 1 || !reflect.DeepEqual(access.specs[0], want) {
		t.Errorf("access reviews %+v, want [%+v]", access.specs, want)
	}

	if got := do(t, srv, http.MethodGet, "/metrics", "Bearer the-token"); got.code != http.StatusOK {
		t.Fatalf("again: got %d, want %d", got.code, http.StatusOK)
	}
	if len(tokens.tokens) != 1 || len(access.specs) != 1 {
		t.Errorf("an allowed token was reviewed again within a minute: %d token and %d access reviews",
			len(tokens.tokens), len(access.specs))
	}
}

func TestAUserWithoutExtraAttributes(t *testing.T) {
	user := prometheus
	user.Extra = nil
	tokens := &fakeTokenReviews{status: authenticationv1.TokenReviewStatus{Authenticated: true, User: user}}
	access := &fakeAccessReviews{allowed: true}
	srv := serve(t, filterOf(newReviewer(tokens, access, time.Now).filter))

	if got := do(t, srv, http.MethodGet, "/metrics", "Bearer the-token"); got.code != http.StatusOK {
		t.Fatalf("got %d, want %d", got.code, http.StatusOK)
	}
	if len(access.specs) != 1 || access.specs[0].Extra != nil {
		t.Errorf("access reviews %+v, want one without extra attributes", access.specs)
	}
}

func TestFailedReviews(t *testing.T) {
	authenticated := authenticationv1.TokenReviewStatus{Authenticated: true, User: prometheus}
	tests := []struct {
		name   string
		tokens *fakeTokenReviews
		access *fakeAccessReviews
		want   response
	}{
		{"the token review fails", &fakeTokenReviews{err: errors.New("connection refused")}, &fakeAccessReviews{},
			response{code: http.StatusInternalServerError, body: "Authentication failed"}},
		{"the access review fails", &fakeTokenReviews{status: authenticated},
			&fakeAccessReviews{err: errors.New("connection refused")},
			response{code: http.StatusInternalServerError, body: "Authorization failed"}},
		{"the token is not authenticated",
			&fakeTokenReviews{status: authenticationv1.TokenReviewStatus{Error: "token expired"}},
			&fakeAccessReviews{allowed: true},
			response{http.StatusUnauthorized, "Unauthorized", `Bearer realm="metrics"`}},
		{"the access review refuses", &fakeTokenReviews{status: authenticated}, &fakeAccessReviews{},
			response{code: http.StatusForbidden, body: "Forbidden"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newReviewer(tt.tokens, tt.access, time.Now)
			srv := serve(t, filterOf(r.filter))
			for attempt := range 2 {
				if got := do(t, srv, http.MethodGet, "/metrics", "Bearer the-token"); got != tt.want {
					t.Errorf("attempt %d: got %+v, want %+v", attempt+1, got, tt.want)
				}
			}
			if len(tt.tokens.tokens) != 2 {
				t.Errorf("reviewed the token %d times in 2 attempts: a refusal was remembered",
					len(tt.tokens.tokens))
			}
			if len(r.allowed) != 0 {
				t.Errorf("remembered %d refused requests", len(r.allowed))
			}
		})
	}
}

func TestRequestsWithoutABearerTokenAreNotReviewed(t *testing.T) {
	tokens := &fakeTokenReviews{}
	srv := serve(t, filterOf(newReviewer(tokens, &fakeAccessReviews{}, time.Now).filter))
	for _, auth := range []string{"", "Bearer", "Bearer ", "Basic dXNlcjpwYXNz", "Token abc"} {
		want := response{http.StatusUnauthorized, "Unauthorized", `Bearer realm="metrics"`}
		if got := do(t, srv, http.MethodGet, "/metrics", auth); got != want {
			t.Errorf("Authorization %q: got %+v, want %+v", auth, got, want)
		}
	}
	if len(tokens.tokens) != 0 {
		t.Errorf("sent %d token reviews for requests without a bearer token", len(tokens.tokens))
	}
}

// usersByToken authenticates the tokens it maps to a user, as the API server's
// TokenReview does, refuses any other, and records the tokens it was sent and
// how many it refused.
type usersByToken struct {
	users   map[string]authenticationv1.UserInfo
	tokens  []string
	refused int
}

func (u *usersByToken) Create(_ context.Context, tr *authenticationv1.TokenReview,
	_ metav1.CreateOptions) (*authenticationv1.TokenReview, error) {
	u.tokens = append(u.tokens, tr.Spec.Token)
	user, ok := u.users[tr.Spec.Token]
	status := authenticationv1.TokenReviewStatus{Authenticated: ok, User: user}
	if !ok {
		status.Error = "invalid bearer token"
		u.refused++
	}
	return &authenticationv1.TokenReview{Spec: tr.Spec, Status: status}, nil
}

// getGrants allows each user it maps get on one path, as a ClusterRole for a
// non-resource URL does, refuses everything else, and records what it was
// asked about and how much it allowed.
type getGrants struct {
	paths   map[string]string
	asked   []authorizationv1.NonResourceAttributes
	allowed int
}

func (g *getGrants) Create(_ context.Context, sar *authorizationv1.SubjectAccessReview,
	_ metav1.CreateOptions) (*authorizationv1.SubjectAccessReview, error) {
	attrs := sar.Spec.NonResourceAttributes
	if attrs == nil {
		return nil, errors.New("not a review of a non-resource URL")
	}
	g.asked = append(g.asked, *attrs)
	path, ok := g.paths[sar.Spec.User]
	allowed := ok && attrs.Verb == "get" && attrs.Path == path
	if allowed {
		g.allowed++
	}
	return &authorizationv1.SubjectAccessReview{
		Spec:   sar.Spec,
		Status: authorizationv1.SubjectAccessReviewStatus{Allowed: allowed},
	}, nil
}

// fuzzedFilter is the filter in front of a handler, reviewing with
// usersByToken and getGrants.
type fuzzedFilter struct {
	handler http.Handler
	tokens  *usersByToken
	grants  *getGrants
	reached bool
}

func newFuzzedFilter(t *testing.T, users map[string]authenticationv1.UserInfo, paths map[string]string) *fuzzedFilter {
	t.Helper()
	f := &fuzzedFilter{tokens: &usersByToken{users: users}, grants: &getGrants{paths: paths}}
	handler, err := newReviewer(f.tokens, f.grants, time.Now).filter(logr.Discard(),
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			f.reached = true
			_, _ = io.WriteString(w, "ok")
		}))
	if err != nil {
		t.Fatal(err)
	}
	f.handler = handler
	return f
}

// outcome is what a request got, and what the reviews did meanwhile.
type outcome struct {
	code            int
	wwwAuthenticate string
	reached         bool
	tokenReviews    int
	refusedTokens   int
	accessReviews   int
	allowed         int
}

// send serves a request with method, path and the Authorization header auth
// (none when empty), as the metrics server does with what a client sent.
func (f *fuzzedFilter) send(method, path, auth string) outcome {
	req := &http.Request{Method: method, URL: &url.URL{Path: path}, Header: http.Header{}}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	tokenReviews, refused := len(f.tokens.tokens), f.tokens.refused
	accessReviews, allowed := len(f.grants.asked), f.grants.allowed
	f.reached = false
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, req)
	return outcome{
		code:            w.Code,
		wwwAuthenticate: w.Header().Get("WWW-Authenticate"),
		reached:         f.reached,
		tokenReviews:    len(f.tokens.tokens) - tokenReviews,
		refusedTokens:   f.tokens.refused - refused,
		accessReviews:   len(f.grants.asked) - accessReviews,
		allowed:         f.grants.allowed - allowed,
	}
}

// FuzzReview sends the filter requests with any method, path and
// Authorization header, as any client that can reach the metrics port may.
// The reviews know two tokens: the reader's, whose user may get /metrics, and
// another user's, who may get nothing. Whatever the request:
//   - it reaches the handler only when the access review allowed it, and gets
//     a 401 asking for a bearer token unless it had one the token review
//     authenticated, without an access review
//   - the token reviewed is the header's bearer token, trimmed, and the access
//     review asks about the request's verb, in lower case, and path
//   - a reviewer that has already allowed the reader's scrape answers it as a
//     new reviewer does: remembering an allowed request lets in no other
func FuzzReview(f *testing.F) {
	for _, seed := range []struct{ method, path, auth string }{
		{http.MethodGet, "/metrics", "Bearer reader-token"},
		{http.MethodGet, "/metrics", "bearer reader-token"},
		{http.MethodGet, "/metrics", "Bearer  reader-token "},
		{"get", "/metrics", "BEARER reader-token"},
		{http.MethodPost, "/metrics", "Bearer reader-token"},
		{http.MethodGet, "/metrics/", "Bearer reader-token"},
		{http.MethodGet, "/debug/pprof", "Bearer reader-token"},
		{http.MethodGet, "/metrics", "Bearer other-token"},
		{http.MethodGet, "/metrics", "Bearer reader-token other-token"},
		{http.MethodGet, "/metrics", "Bearer\treader-token"},
		{http.MethodGet, "/metrics", "Basic cmVhZGVyOnRva2Vu"},
		{http.MethodGet, "/metrics", "Bearer"},
		{http.MethodGet, "/metrics", ""},
	} {
		f.Add(seed.method, seed.path, seed.auth)
	}

	users := map[string]authenticationv1.UserInfo{
		"reader-token": {Username: "system:serviceaccount:monitoring:prometheus"},
		"other-token":  {Username: "system:serviceaccount:default:other"},
	}
	paths := map[string]string{"system:serviceaccount:monitoring:prometheus": "/metrics"}

	f.Fuzz(func(t *testing.T, method, path, auth string) {
		fresh := newFuzzedFilter(t, users, paths)
		got := fresh.send(method, path, auth)
		if got.reached != (got.code == http.StatusOK) {
			t.Errorf("%+v: reached the handler, or not, with %d", got, got.code)
		}
		if got.reached && got.allowed != 1 {
			t.Errorf("%+v: reached the handler without an access review allowing it", got)
		}
		if (got.tokenReviews == 0 || got.refusedTokens > 0) && (got.code != http.StatusUnauthorized ||
			got.wwwAuthenticate != `Bearer realm="metrics"` || got.accessReviews != 0) {
			t.Errorf("%+v: a request without a token the API server authenticates must get a 401 asking for one",
				got)
		}
		if got.code != http.StatusOK && got.code != http.StatusUnauthorized && got.code != http.StatusForbidden {
			t.Errorf("%+v: reviews that do not fail must not give %d", got, got.code)
		}
		for _, token := range fresh.tokens.tokens {
			scheme, rest, _ := strings.Cut(auth, " ")
			if !strings.EqualFold(scheme, "Bearer") || token == "" || token != strings.TrimSpace(rest) {
				t.Errorf("reviewed token %q, which is not the trimmed bearer token of %q", token, auth)
			}
		}
		for _, attrs := range fresh.grants.asked {
			if attrs.Verb != strings.ToLower(method) || attrs.Path != path {
				t.Errorf("asked about %s %q for a %s request to %q", attrs.Verb, attrs.Path, method, path)
			}
		}

		warm := newFuzzedFilter(t, users, paths)
		if scrape := warm.send(http.MethodGet, "/metrics", "Bearer reader-token"); !scrape.reached {
			t.Fatalf("the reader's scrape was refused: %+v", scrape)
		}
		if again := warm.send(method, path, auth); again.code != got.code || again.reached != got.reached {
			t.Errorf("after allowing the reader's scrape: %+v; from a new reviewer: %+v", again, got)
		}
	})
}
