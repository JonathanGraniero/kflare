/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"fmt"
	"time"

	cf "github.com/cloudflare/cloudflare-go"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	cfpkg "github.com/JonathanGraniero/kflare/pkg/cloudflare"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

// r2BucketNotEmptyCode is the Cloudflare error code (with HTTP 409) for
// deleting a bucket that still holds objects.
const r2BucketNotEmptyCode = 10008

// R2BucketAPI is the subset of the Cloudflare API used by this controller.
// Declaring a narrow interface keeps unit tests simple: tests inject a fake
// that implements only these methods. In production, *cfpkg.Client satisfies
// this interface because it embeds *cf.API.
//
// A bucket has no ID: its name identifies it within the account. These calls
// reach the default jurisdiction only; cloudflare-go v0.89 cannot send the
// header that selects another one.
type R2BucketAPI interface {
	GetR2Bucket(ctx context.Context, rc *cf.ResourceContainer, bucketName string) (cf.R2Bucket, error)
	CreateR2Bucket(ctx context.Context, rc *cf.ResourceContainer, params cf.CreateR2BucketParameters) (cf.R2Bucket, error)
	DeleteR2Bucket(ctx context.Context, rc *cf.ResourceContainer, bucketName string) error
}

// R2BucketReconciler reconciles an R2Bucket object.
type R2BucketReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// NewR2BucketAPI constructs an R2BucketAPI from a raw API token.
	// Defaults to defaultR2BucketAPI; overridden in tests to inject a fake.
	NewR2BucketAPI func(token string) (R2BucketAPI, error)
}

//+kubebuilder:rbac:groups=kflare.dev,resources=r2buckets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=kflare.dev,resources=r2buckets/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=kflare.dev,resources=r2buckets/finalizers,verbs=update
//+kubebuilder:rbac:groups=kflare.dev,resources=cloudflareaccounts,verbs=get;list;watch
//+kubebuilder:rbac:groups=kflare.dev,resources=workerscripts,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *R2BucketReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	bucket := &cloudflarev1alpha1.R2Bucket{}
	if err := r.Get(ctx, req.NamespacedName, bucket); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion before anything else.
	if !bucket.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, bucket)
	}

	// Ensure the finalizer is present before doing any work.
	// On the very first reconcile the finalizer is absent — add it and return.
	// controller-runtime will re-enqueue the object once the update is visible.
	added, err := reconciler.EnsureFinalizer(ctx, r.Client, bucket)
	if err != nil {
		return ctrl.Result{}, err
	}
	if added {
		logger.Info("Added finalizer", "r2bucket", req.NamespacedName)
		return ctrl.Result{}, nil
	}

	account, token, credErr := resolveAccountToken(ctx, r.Client, bucket.Spec.AccountRef.Name, true)
	if credErr != nil {
		return notReadyRetryAfter(ctx, r.Client, bucket, credErr)
	}

	cfAPI, err := r.newR2BucketAPI(token)
	if err != nil {
		return invalidToken(ctx, r.Client, bucket, err)
	}

	return r.syncBucket(ctx, bucket, account, cfAPI)
}

// syncBucket drives the desired→observed→delta→reconcile loop for an
// R2Bucket. A bucket has nothing kflare can change in place: its name is
// immutable and its location is set at creation. So the loop makes sure the
// bucket exists and is this resource's to manage.
func (r *R2BucketReconciler) syncBucket(
	ctx context.Context,
	bucket *cloudflarev1alpha1.R2Bucket,
	account *cloudflarev1alpha1.CloudflareAccount,
	cfAPI R2BucketAPI,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	rc := cf.AccountIdentifier(account.Spec.AccountID)
	name := bucket.Spec.Name
	claimed := managesBucket(bucket)

	current, err := cfAPI.GetR2Bucket(ctx, rc, name)
	found := err == nil
	if err != nil && !cfpkg.IsNotFound(err) {
		return handleCloudflareError(ctx, r.Client, bucket, err)
	}

	// Until it holds the claim, a bucket name another R2Bucket manages in
	// the same account is off limits, whether or not the bucket exists now:
	// adopting it would let either resource delete it under the other.
	if !claimed {
		owner, err := r.bucketClaimant(ctx, bucket, account.Spec.AccountID)
		if err != nil {
			return ctrl.Result{}, err
		}
		if owner != "" {
			return notReadyRetryAfter(ctx, r.Client, bucket, &conditionError{
				Reason:  "NameConflict",
				Message: fmt.Sprintf("R2 bucket %q is already managed by R2Bucket %s", name, owner),
			})
		}
	}

	switch {
	case !found:
		// Missing: never created, or deleted outside kflare together with
		// its objects. A new, empty bucket is created either way.
		if claimed {
			logger.Info("R2 bucket deleted outside kflare, recreating", "bucket", name)
		}
		current, err = cfAPI.CreateR2Bucket(ctx, rc, cf.CreateR2BucketParameters{
			Name:         name,
			LocationHint: bucket.Spec.LocationHint,
		})
		if err != nil {
			return handleCloudflareError(ctx, r.Client, bucket, err)
		}
		logger.Info("Created R2 bucket", "bucket", name, "location", current.Location)
	case !claimed:
		logger.Info("Adopted existing R2 bucket", "bucket", name, "location", current.Location)
	}

	// Claim the bucket before anything else can fail, so no other R2Bucket
	// adopts it in the meantime.
	if err := claimID(ctx, r.Client, bucket, cloudflarev1alpha1.R2BucketNameLabel, name); err != nil {
		return ctrl.Result{}, err
	}

	bucket.Status.CloudflareMetadata.Location = current.Location
	bucket.Status.CloudflareMetadata.CreationDate = metaTime(current.CreationDate)
	return ctrl.Result{}, updateReady(ctx, r.Client, bucket, "Synced", "R2 bucket exists in Cloudflare")
}

// managesBucket reports whether bucket has claimed the Cloudflare bucket its
// spec names, that is, created or adopted it.
func managesBucket(bucket *cloudflarev1alpha1.R2Bucket) bool {
	return bucket.Labels[cloudflarev1alpha1.R2BucketNameLabel] == bucket.Spec.Name
}

// bucketClaimant returns the "namespace/name" of another R2Bucket that
// claims the bucket name bucket wants in the Cloudflare account accountID,
// or "" if there is none.
//
// Bucket names are only unique within an account, so a claim counts when the
// other R2Bucket uses the same CloudflareAccount, or a different one that
// points at the same Cloudflare account. Claims are searched across all
// namespaces: R2Buckets in different namespaces can share an account.
func (r *R2BucketReconciler) bucketClaimant(
	ctx context.Context,
	bucket *cloudflarev1alpha1.R2Bucket,
	accountID string,
) (string, error) {
	list := &cloudflarev1alpha1.R2BucketList{}
	if err := r.List(ctx, list,
		client.MatchingLabels{cloudflarev1alpha1.R2BucketNameLabel: bucket.Spec.Name}); err != nil {
		return "", err
	}
	for i := range list.Items {
		other := &list.Items[i]
		if other.UID == bucket.UID {
			continue
		}
		owner := other.Namespace + "/" + other.Name
		if other.Spec.AccountRef.Name == bucket.Spec.AccountRef.Name {
			return owner, nil
		}
		otherAccount := &cloudflarev1alpha1.CloudflareAccount{}
		err := r.Get(ctx, types.NamespacedName{Name: other.Spec.AccountRef.Name}, otherAccount)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		if otherAccount.Spec.AccountID == accountID {
			return owner, nil
		}
	}
	return "", nil
}

// metaTime converts an optional timestamp from the Cloudflare API.
func metaTime(t *time.Time) *metav1.Time {
	if t == nil {
		return nil
	}
	mt := metav1.NewTime(*t)
	return &mt
}

// reconcileDelete handles the deletion lifecycle. Once no WorkerScript binds
// the bucket, it deletes the bucket from Cloudflare (unless the retain policy
// is set or the bucket was never this resource's), then removes the finalizer.
//
// Cloudflare deletes a bucket that a Worker is still bound to, but every later
// upload of that Worker then fails, so bound WorkerScripts go first. kflare
// never deletes objects: Cloudflare refuses to delete a bucket that holds
// any, and the resource then reports BucketNotEmpty until the bucket is
// emptied or the retain policy is set.
func (r *R2BucketReconciler) reconcileDelete(ctx context.Context, bucket *cloudflarev1alpha1.R2Bucket) (ctrl.Result, error) {
	// Safety check: if the finalizer is already gone, there is nothing to do.
	if !controllerutil.ContainsFinalizer(bucket, reconciler.Finalizer) {
		return ctrl.Result{}, nil
	}

	dependents, err := dependentsOf(ctx, r.Client, bucket.Namespace, bucket.Name, r2BucketDependents)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(dependents) > 0 {
		return ctrl.Result{}, reportInUse(ctx, r.Client, bucket, dependents)
	}

	if managesBucket(bucket) && !reconciler.RetainOnDelete(bucket) {
		account, token, credErr := resolveAccountToken(ctx, r.Client, bucket.Spec.AccountRef.Name, false)
		if credErr != nil {
			return ctrl.Result{}, credErr
		}
		cfAPI, err := r.newR2BucketAPI(token)
		if err != nil {
			return ctrl.Result{}, err
		}
		err = cfAPI.DeleteR2Bucket(ctx, cf.AccountIdentifier(account.Spec.AccountID), bucket.Spec.Name)
		if cfpkg.HasErrorCode(err, r2BucketNotEmptyCode) {
			// Objects are not watched, so check again periodically.
			return notReadyRetryAfter(ctx, r.Client, bucket, &conditionError{
				Reason: "BucketNotEmpty",
				Message: fmt.Sprintf("R2 bucket %q still holds objects and kflare does not delete them: empty it, "+
					"or set the %s: %s annotation to keep it", bucket.Spec.Name,
					reconciler.DeletionPolicyAnnotation, reconciler.DeletionPolicyRetain),
			})
		}
		if err != nil && !cfpkg.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	// Remove the finalizer to allow Kubernetes to complete the deletion.
	_, err = reconciler.RemoveFinalizer(ctx, r.Client, bucket)
	return ctrl.Result{}, err
}

// SetupWithManager registers R2BucketReconciler with the manager. It watches
// CloudflareAccounts, so a bucket waiting for its account is reconciled once
// the account is ready, and deleted WorkerScripts, so a bucket waiting for the
// Workers bound to it is released.
func (r *R2BucketReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cloudflarev1alpha1.R2Bucket{}).
		Watches(&cloudflarev1alpha1.CloudflareAccount{}, handler.EnqueueRequestsFromMapFunc(r.bucketsForAccount)).
		Watches(&cloudflarev1alpha1.WorkerScript{},
			handler.EnqueueRequestsFromMapFunc(r.deletingR2BucketsOf), builder.WithPredicates(deletesOnly)).
		Complete(r)
}

// bucketsForAccount maps a CloudflareAccount event to the R2Buckets that use it.
func (r *R2BucketReconciler) bucketsForAccount(ctx context.Context, obj client.Object) []reconcile.Request {
	list := &cloudflarev1alpha1.R2BucketList{}
	if err := r.List(ctx, list); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, b := range list.Items {
		if b.Spec.AccountRef.Name == obj.GetName() {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: b.Name, Namespace: b.Namespace}})
		}
	}
	return reqs
}

// deletingR2BucketsOf maps the deletion of a WorkerScript to the R2Buckets it
// binds that are themselves being deleted.
func (r *R2BucketReconciler) deletingR2BucketsOf(ctx context.Context, obj client.Object) []reconcile.Request {
	return deletingParentOf(ctx, r.Client, obj, func() client.Object { return &cloudflarev1alpha1.R2Bucket{} },
		true, r2BucketDependents)
}

// newR2BucketAPI builds an R2BucketAPI with the injected factory, falling
// back to the production client.
func (r *R2BucketReconciler) newR2BucketAPI(token string) (R2BucketAPI, error) {
	if r.NewR2BucketAPI != nil {
		return r.NewR2BucketAPI(token)
	}
	return defaultR2BucketAPI(token)
}

// defaultR2BucketAPI is the production factory: it delegates to cfpkg.New so
// that the returned *cfpkg.Client (which embeds *cf.API) satisfies
// R2BucketAPI.
func defaultR2BucketAPI(token string) (R2BucketAPI, error) {
	return cfpkg.New(token)
}
