// Package controller holds the CanaryRollout reconciler (DESIGN.md, "The rollout").
package controller

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	rolloutsv1alpha1 "github.com/vivekdavara/canary/api/v1alpha1"
	"github.com/vivekdavara/canary/internal/revision"
)

// CanaryRolloutReconciler reconciles CanaryRollouts. On day 1 it only observes: it records which generation it saw
// and the template's revision. Day 2 adds the Deployments and the steps.
type CanaryRolloutReconciler struct {
	client.Client
}

// The permissions the operator needs, generated into config/rbac/role.yaml by make manifests. Watching Deployments
// needs only reading them; day 2 adds writing them, and Events.
//
// +kubebuilder:rbac:groups=rollouts.vivekdavara.github.io,resources=canaryrollouts,verbs=get;list;watch
// +kubebuilder:rbac:groups=rollouts.vivekdavara.github.io,resources=canaryrollouts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch

// Reconcile brings one rollout's status up to date with its spec.
func (r *CanaryRolloutReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ro rolloutsv1alpha1.CanaryRollout
	if err := r.Get(ctx, req.NamespacedName, &ro); err != nil {
		// Deleted since the event: its Deployments go with it (owner references), nothing to do.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	want := ro.Status.DeepCopy()
	want.ObservedGeneration = ro.Generation
	// The template as the API server stored it, defaults included (internal/revision says why).
	want.UpdateRevision = revision.Of(&ro.Spec.Template)
	if equality.Semantic.DeepEqual(*want, ro.Status) {
		return ctrl.Result{}, nil
	}

	// The patch carries the resourceVersion read above, so it fails if the object changed since: a status computed
	// from an older spec is never written over a newer one.
	patch := client.MergeFromWithOptions(ro.DeepCopy(), client.MergeFromWithOptimisticLock{})
	ro.Status = *want
	if err := r.Status().Patch(ctx, &ro, patch); err != nil {
		if apierrors.IsConflict(err) {
			// The change that caused the conflict is an event of its own and brings another reconcile.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers the reconciler: it runs for each change to a CanaryRollout and to a Deployment one
// owns.
func (r *CanaryRolloutReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&rolloutsv1alpha1.CanaryRollout{}).
		Owns(&appsv1.Deployment{}).
		Named("canaryrollout").
		Complete(r)
}
