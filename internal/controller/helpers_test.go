/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	cf "github.com/cloudflare/cloudflare-go"
	"github.com/cloudflare/cloudflare-go/v7/kv"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// forceDelete clears the finalizers of the object at key and deletes it, so a
// test leaves nothing stuck in Terminating: no controller runs in the envtest
// suite to remove them.
func forceDelete(ctx context.Context, obj client.Object, key types.NamespacedName) {
	if err := k8sClient.Get(ctx, key, obj); err != nil {
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
		return
	}
	if len(obj.GetFinalizers()) > 0 {
		obj.SetFinalizers(nil)
		Expect(client.IgnoreNotFound(k8sClient.Update(ctx, obj))).To(Succeed())
	}
	Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
}

// meeting returns an arrive hook for a fake, under which the first n calls
// named call each wait, up to wait, until all n are under way at once, and a
// function that reports whether they were. It tells reconciles that call
// Cloudflare at once from reconciles that take turns: taking turns, the first
// call waits in vain for the others.
func meeting(n int, call string, wait time.Duration) (arrive func(string), met func() bool) {
	var mu sync.Mutex
	arrived, timedOut := 0, false
	all := make(chan struct{})
	arrive = func(c string) {
		if c != call {
			return
		}
		mu.Lock()
		arrived++
		if arrived == n {
			close(all)
		}
		waits := arrived < n
		mu.Unlock()
		if !waits {
			return
		}
		select {
		case <-all:
		case <-time.After(wait):
			mu.Lock()
			timedOut = true
			mu.Unlock()
		}
	}
	met = func() bool {
		mu.Lock()
		defer mu.Unlock()
		return arrived >= n && !timedOut
	}
	return arrive, met
}

// cloudflareError builds the error cloudflare-go returns for an API failure
// with an HTTP status (404, or another 4xx) and a Cloudflare error code.
func cloudflareError(status, code int) error {
	e := &cf.Error{StatusCode: status, ErrorCodes: []int{code}}
	if status == 404 {
		e.Type = cf.ErrorTypeNotFound
		v := cf.NewNotFoundError(e)
		return &v
	}
	e.Type = cf.ErrorTypeRequest
	v := cf.NewRequestError(e)
	return &v
}

// v7APIError builds the error cloudflare-go v7 returns when Cloudflare answers
// a request for path, relative to the API's base URL, with an HTTP status and
// a Cloudflare error. The type is the same in every v7 package; kv's alias
// names it here. Its Error method reads the request and the response, so both
// are set.
func v7APIError(method, path string, status, code int, message string) error {
	req := httptest.NewRequest(method, "https://api.cloudflare.com/client/v4/"+path, nil)
	return &kv.Error{
		StatusCode: status,
		Errors:     []kv.ErrorData{{Code: int64(code), Message: message}},
		Request:    req,
		Response: &http.Response{
			StatusCode: status,
			Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
			Request:    req,
		},
	}
}
