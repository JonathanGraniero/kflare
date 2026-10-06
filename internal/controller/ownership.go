/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Several kinds manage Cloudflare objects that they may adopt rather than
// create (DNS records, Worker routes). Each such resource records the ID of
// the Cloudflare object it manages in a kind-specific label, and adoption
// skips any object another resource of the same kind already carries in that
// label. The label is a claim: it is set as soon as the object is created or
// adopted, before anything else can fail.

// claimedIDs returns, for every Cloudflare ID that a resource other than self
// carries in label, the "namespace/name" of that resource. list selects the
// kind (for example &DNSRecordList{}) and is searched across all namespaces,
// because two parent resources in different namespaces can point at the same
// Cloudflare zone.
func claimedIDs(
	ctx context.Context,
	c client.Reader,
	list client.ObjectList,
	label string,
	self client.Object,
) (map[string]string, error) {
	if err := c.List(ctx, list, client.HasLabels{label}); err != nil {
		return nil, err
	}
	items, err := meta.ExtractList(list)
	if err != nil {
		return nil, err
	}
	claimed := make(map[string]string, len(items))
	for _, item := range items {
		other, ok := item.(client.Object)
		if !ok {
			return nil, fmt.Errorf("unexpected list item %T", item)
		}
		if other.GetUID() != self.GetUID() {
			claimed[other.GetLabels()[label]] = other.GetNamespace() + "/" + other.GetName()
		}
	}
	return claimed, nil
}

// claimID records id in obj's label, so other resources of its kind treat the
// Cloudflare object as taken. It patches only the label.
func claimID(ctx context.Context, c client.Writer, obj client.Object, label, id string) error {
	if obj.GetLabels()[label] == id {
		return nil
	}
	patch := client.MergeFrom(obj.DeepCopyObject().(client.Object))
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[label] = id
	obj.SetLabels(labels)
	return c.Patch(ctx, obj, patch)
}
