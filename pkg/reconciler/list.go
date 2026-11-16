/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package reconciler

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// List lists list's kind with opts into list and returns its items as T.
func List[T client.Object](ctx context.Context, c client.Reader, list client.ObjectList,
	opts ...client.ListOption) ([]T, error) {
	if err := c.List(ctx, list, opts...); err != nil {
		return nil, err
	}
	items, err := meta.ExtractList(list)
	if err != nil {
		return nil, err
	}
	objs := make([]T, 0, len(items))
	for _, item := range items {
		obj, ok := item.(T)
		if !ok {
			return nil, fmt.Errorf("unexpected %T in a %T", item, list)
		}
		objs = append(objs, obj)
	}
	return objs, nil
}

// RequestsWhere returns a request for each object of list's kind, listed with
// opts, that match accepts. It is a watch's mapping, which has nowhere to
// report a failed list, so it then returns none.
func RequestsWhere[T client.Object](ctx context.Context, c client.Reader, list client.ObjectList,
	opts []client.ListOption, match func(T) bool) []reconcile.Request {
	objs, err := List[T](ctx, c, list, opts...)
	if err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, obj := range objs {
		if match(obj) {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(obj)})
		}
	}
	return reqs
}
