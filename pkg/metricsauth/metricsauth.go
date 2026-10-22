/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

// Package metricsauth protects the metrics endpoint: a request reaches it only
// with a bearer token that the Kubernetes API server authenticates
// (TokenReview) and authorizes for the request's verb on its path, as a
// non-resource URL (SubjectAccessReview). A ClusterRole granting get on
// /metrics lets a ServiceAccount, such as Prometheus', scrape it.
//
// It does what controller-runtime's metrics/filters package does, but on
// client-go's typed clients, which the manager links already: that package
// pulls in k8s.io/apiserver, which grows the manager by more than half.
package metricsauth

import (
	"crypto/sha256"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	authenticationv1client "k8s.io/client-go/kubernetes/typed/authentication/v1"
	authorizationv1client "k8s.io/client-go/kubernetes/typed/authorization/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

const (
	// allowTTL is how long an allowed token is trusted for a verb and path
	// without asking the API server again, so that a scrape every 30 seconds
	// costs two reviews a minute. A revoked token or role binding stops
	// working within it.
	allowTTL = time.Minute

	// The reviews' own rate limit, shared by both clients: however many
	// requests arrive, they send the API server at most this many reviews a
	// second, and they never take from the controllers' client's limit.
	reviewQPS   = 5
	reviewBurst = 10
)

// WithAuthenticationAndAuthorization is a metrics server FilterProvider
// (metricsserver.Options.FilterProvider) that sends its reviews to the API
// server in config, through httpClient.
func WithAuthenticationAndAuthorization(config *rest.Config, httpClient *http.Client) (metricsserver.Filter, error) {
	config = rest.CopyConfig(config)
	config.RateLimiter = flowcontrol.NewTokenBucketRateLimiter(reviewQPS, reviewBurst)
	authn, err := authenticationv1client.NewForConfigAndClient(config, httpClient)
	if err != nil {
		return nil, err
	}
	authz, err := authorizationv1client.NewForConfigAndClient(config, httpClient)
	if err != nil {
		return nil, err
	}
	return newReviewer(authn.TokenReviews(), authz.SubjectAccessReviews(), time.Now).filter, nil
}

// newReviewer returns a reviewer that reviews requests with these clients, and
// reads the time from now.
func newReviewer(
	tokenReviews authenticationv1client.TokenReviewInterface,
	accessReviews authorizationv1client.SubjectAccessReviewInterface,
	now func() time.Time,
) *reviewer {
	return &reviewer{
		tokenReviews:  tokenReviews,
		accessReviews: accessReviews,
		now:           now,
		allowed:       map[allowKey]time.Time{},
	}
}

// reviewer authenticates and authorizes requests, and remembers the ones it
// allowed for allowTTL.
type reviewer struct {
	tokenReviews  authenticationv1client.TokenReviewInterface
	accessReviews authorizationv1client.SubjectAccessReviewInterface
	now           func() time.Time

	mu sync.Mutex
	// allowed maps each allowed token, verb and path to when it expires. Only
	// allowed requests are remembered, so tokens that fail cannot grow it.
	allowed map[allowKey]time.Time
}

// filter is a metricsserver.Filter: it passes a request on to handler only
// once review allows it.
func (r *reviewer) filter(log logr.Logger, handler http.Handler) (http.Handler, error) {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if r.review(log, w, req) {
			handler.ServeHTTP(w, req)
		}
	}), nil
}

// allowKey identifies an allowed request. It holds the token's hash, not the
// token.
type allowKey struct {
	token      [sha256.Size]byte
	verb, path string
}

// review reports whether req may go on to the handler. When it may not, it has
// written the response: 401 without a token the API server authenticates, 403
// when the token's user may not use the verb on the path, and 500 when a
// review fails.
func (r *reviewer) review(log logr.Logger, w http.ResponseWriter, req *http.Request) bool {
	token, ok := bearerToken(req)
	if !ok {
		unauthorized(w)
		return false
	}
	key := allowKey{
		token: sha256.Sum256([]byte(token)),
		verb:  strings.ToLower(req.Method),
		path:  req.URL.Path,
	}
	if r.isAllowed(key) {
		return true
	}

	ctx := req.Context()
	tokenReview, err := r.tokenReviews.Create(ctx, &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{Token: token},
	}, metav1.CreateOptions{})
	if err != nil {
		log.Error(err, "Unable to authenticate a metrics request")
		http.Error(w, "Authentication failed", http.StatusInternalServerError)
		return false
	}
	if !tokenReview.Status.Authenticated {
		log.V(1).Info("Refused a metrics request without a valid token", "error", tokenReview.Status.Error)
		unauthorized(w)
		return false
	}

	user := tokenReview.Status.User
	accessReview, err := r.accessReviews.Create(ctx, &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User:   user.Username,
			UID:    user.UID,
			Groups: user.Groups,
			Extra:  extra(user.Extra),
			NonResourceAttributes: &authorizationv1.NonResourceAttributes{
				Path: key.path,
				Verb: key.verb,
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		log.Error(err, "Unable to authorize a metrics request", "user", user.Username)
		http.Error(w, "Authorization failed", http.StatusInternalServerError)
		return false
	}
	if !accessReview.Status.Allowed {
		log.V(1).Info("Refused a metrics request", "user", user.Username, "verb", key.verb, "path", key.path,
			"reason", accessReview.Status.Reason)
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return false
	}

	r.allow(key)
	return true
}

// isAllowed reports whether key was allowed less than allowTTL ago.
func (r *reviewer) isAllowed(key allowKey) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	expiry, ok := r.allowed[key]
	return ok && r.now().Before(expiry)
}

// allow remembers key for allowTTL, and forgets the keys that expired.
func (r *reviewer) allow(key allowKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	for k, expiry := range r.allowed {
		if !now.Before(expiry) {
			delete(r.allowed, k)
		}
	}
	r.allowed[key] = now.Add(allowTTL)
}

// bearerToken returns the token of req's "Authorization: Bearer" header.
func bearerToken(req *http.Request) (string, bool) {
	scheme, token, _ := strings.Cut(req.Header.Get("Authorization"), " ")
	token = strings.TrimSpace(token)
	return token, strings.EqualFold(scheme, "Bearer") && token != ""
}

// unauthorized writes a 401 that names the scheme the endpoint takes.
func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="metrics"`)
	http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
}

// extra converts the user's extra attributes from the TokenReview's type to
// the SubjectAccessReview's.
func extra(in map[string]authenticationv1.ExtraValue) map[string]authorizationv1.ExtraValue {
	if in == nil {
		return nil
	}
	out := make(map[string]authorizationv1.ExtraValue, len(in))
	for k, v := range in {
		out[k] = authorizationv1.ExtraValue(v)
	}
	return out
}
