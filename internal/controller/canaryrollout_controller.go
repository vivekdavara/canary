// Package controller holds the CanaryRollout reconciler (DESIGN.md, "The rollout").
package controller

import (
	"context"
	"encoding/json"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"

	rolloutsv1alpha1 "github.com/vivekdavara/canary/api/v1alpha1"
	"github.com/vivekdavara/canary/internal/revision"
)

// Condition types and the reasons the operator gives (DESIGN.md, "Status").
const (
	ConditionProgressing = "Progressing"

	// ReasonInvalidSpec: the stored rollout can't be decoded, or its template can't be run.
	ReasonInvalidSpec = "InvalidSpec"
)

// CanaryRolloutReconciler reconciles CanaryRollouts.
type CanaryRolloutReconciler struct {
	client.Client
	// APIReader reads each rollout straight from the API server (see Reconcile for why not from the cache).
	APIReader client.Reader
}

// New returns the reconciler for mgr.
func New(mgr ctrl.Manager) *CanaryRolloutReconciler {
	return &CanaryRolloutReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader()}
}

// The permissions the operator needs, generated into config/rbac/role.yaml by make manifests: no more than the code
// uses (the status is only patched). Watching Deployments needs only reading them; day 2 adds writing them, and
// Events. --leader-elect's Lease is in config/rbac/leader_election_role.yaml, a namespaced Role.
//
// +kubebuilder:rbac:groups=rollouts.vivekdavara.github.io,resources=canaryrollouts,verbs=get;list;watch
// +kubebuilder:rbac:groups=rollouts.vivekdavara.github.io,resources=canaryrollouts/status,verbs=patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch

// Reconcile brings one rollout's status up to date with its spec.
//
// The rollout is read from the API server as unstructured JSON and decoded here, one object at a time. The API server
// can accept a rollout the typed client can't decode (a probe port beyond int32: the CRD's copy of the pod schema
// doesn't bound int-or-string fields), and a typed cache decodes every rollout in one list, so one such object would
// stop the operator for all of them. The watch is metadata-only for the same reason; the price is one GET per
// reconcile instead of a cache read.
func (r *CanaryRolloutReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(rolloutsv1alpha1.GroupVersion.WithKind("CanaryRollout"))
	if err := r.APIReader.Get(ctx, req.NamespacedName, u); err != nil {
		// Deleted since the event: its Deployments go with it (owner references), nothing to do.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	var ro rolloutsv1alpha1.CanaryRollout
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &ro); err != nil {
		return ctrl.Result{}, r.reportUndecodable(ctx, u, err)
	}

	want := ro.Status.DeepCopy()
	want.ObservedGeneration = ro.Generation
	// The template as the API server stored it, defaults included (internal/revision says why).
	want.UpdateRevision = revision.Of(&ro.Spec.Template)
	// A rollout that was undecodable and has been fixed loses the condition that said so.
	if c := meta.FindStatusCondition(want.Conditions, ConditionProgressing); c != nil && c.Reason == ReasonInvalidSpec {
		meta.RemoveStatusCondition(&want.Conditions, ConditionProgressing)
	}
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

// maxMessage bounds a condition message built from an error (the API allows 32768 bytes).
const maxMessage = 1024

// reportUndecodable records on the rollout's own status that it can't be decoded, without decoding the rest of it:
// observedGeneration and a Progressing condition, False with reason InvalidSpec. The operator then leaves the rollout
// alone until it changes (a fix is a new event).
func (r *CanaryRolloutReconciler) reportUndecodable(ctx context.Context, u *unstructured.Unstructured, decodeErr error) error {
	// The conditions are the operator's own writes, so they decode even when the spec doesn't.
	var st struct {
		ObservedGeneration int64              `json:"observedGeneration"`
		Conditions         []metav1.Condition `json:"conditions"`
	}
	if raw, ok := u.Object["status"].(map[string]any); ok {
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &st); err != nil {
			return fmt.Errorf("decoding the status of an undecodable rollout: %w", err)
		}
	}
	msg := "the rollout can't be decoded: " + decodeErr.Error()
	if len(msg) > maxMessage {
		msg = msg[:maxMessage]
	}
	changed := meta.SetStatusCondition(&st.Conditions, metav1.Condition{
		Type:               ConditionProgressing,
		Status:             metav1.ConditionFalse,
		Reason:             ReasonInvalidSpec,
		Message:            msg,
		ObservedGeneration: u.GetGeneration(),
	})
	if !changed && st.ObservedGeneration == u.GetGeneration() {
		return nil
	}
	// A JSON merge patch replaces the conditions list whole, and the resourceVersion makes it fail if the object
	// changed since it was read, like the typed path's optimistic lock.
	data, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"resourceVersion": u.GetResourceVersion()},
		"status":   map[string]any{"observedGeneration": u.GetGeneration(), "conditions": st.Conditions},
	})
	if err != nil {
		return err
	}
	if err := r.Status().Patch(ctx, u, client.RawPatch(types.MergePatchType, data)); err != nil && !apierrors.IsConflict(err) {
		return err
	}
	return nil
}

// SetupWithManager registers the reconciler: it runs for each change to a CanaryRollout and to a Deployment one
// owns. Rollouts are watched metadata-only (Reconcile says why).
func (r *CanaryRolloutReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&rolloutsv1alpha1.CanaryRollout{}, builder.OnlyMetadata).
		Owns(&appsv1.Deployment{}).
		Named("canaryrollout").
		Complete(r)
}
