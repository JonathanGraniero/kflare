/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"

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
