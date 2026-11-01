// Package controller holds the CanaryRollout reconciler (DESIGN.md, "The rollout").
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	rolloutsv1alpha1 "github.com/vivekdavara/canary/api/v1alpha1"
	"github.com/vivekdavara/canary/internal/revision"
	"github.com/vivekdavara/canary/internal/rollout"
)

// Condition types and the reasons the operator gives (DESIGN.md, "Status"). The Progressing condition's other
// reasons are the state machine's (internal/rollout).
const (
	ConditionProgressing = "Progressing"
	ConditionAvailable   = "Available"

	// ReasonInvalidSpec: the stored rollout can't be decoded, or the API server refused a Deployment built from its
	// template.
	ReasonInvalidSpec = "InvalidSpec"
	// ReasonDeploymentConflict: a Deployment with one of the rollout's names belongs to someone else.
	ReasonDeploymentConflict = "DeploymentConflict"

	ReasonEnoughPods      = "EnoughPodsReady"
	ReasonNotEnoughPods   = "NotEnoughPodsReady"
	eventActionRollout    = "Rollout"
	eventActionValidation = "Validate"
)

// CanaryRolloutReconciler reconciles CanaryRollouts.
type CanaryRolloutReconciler struct {
	client.Client
	// APIReader reads each rollout straight from the API server (see Reconcile for why not from the cache).
	APIReader client.Reader
	// Recorder records Events on rollouts (events.k8s.io/v1).
	Recorder events.EventRecorder
	// Now is the clock; time.Now when nil. Tests set it.
	Now func() time.Time
}

// New returns the reconciler for mgr.
func New(mgr ctrl.Manager) *CanaryRolloutReconciler {
	return &CanaryRolloutReconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Recorder:  mgr.GetEventRecorder("canary"),
	}
}

// The permissions the operator needs, generated into config/rbac/role.yaml by make manifests: no more than the code
// uses. Rollouts are read with a GET and watched; the status is only patched. Deployments are read from the cache
// (list and watch, no get), created and updated, never deleted (owner references let the garbage collector do that).
// A controller reference sets blockOwnerDeletion, which clusters running the OwnerReferencesPermissionEnforcement
// admission plugin (OpenShift does) allow only to users who may update the owner's finalizers. Events are created,
// and patched when one repeats. --leader-elect's Lease is in config/rbac/leader_election_role.yaml, a namespaced Role.
//
// +kubebuilder:rbac:groups=rollouts.vivekdavara.github.io,resources=canaryrollouts,verbs=get;list;watch
// +kubebuilder:rbac:groups=rollouts.vivekdavara.github.io,resources=canaryrollouts/status,verbs=patch
// +kubebuilder:rbac:groups=rollouts.vivekdavara.github.io,resources=canaryrollouts/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=list;watch;create;update
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile moves one rollout a step on: it reads the rollout and its two Deployments, asks the state machine
// (rollout.Decide) what comes next, makes the Deployments match, then writes the status and records Events.
//
// The rollout is read from the API server as unstructured JSON and decoded here, one object at a time. The API server
// can accept a rollout the typed client can't decode (a probe port beyond int32: the CRD's copy of the pod schema
// doesn't bound int-or-string fields), and a typed cache decodes every rollout in one list, so one such object would
// stop the operator for all of them. The watch is metadata-only for the same reason; the price is one GET per
// reconcile instead of a cache read.
//
// The Deployments are written before the status: a failed Deployment write (a template the API server refuses, a
// conflict) then leaves the status describing what is really running. The state machine copes with the other
// order of failure (rollout.Decide says how).
func (r *CanaryRolloutReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(rolloutsv1alpha1.GroupVersion.WithKind("CanaryRollout"))
	if err := r.APIReader.Get(ctx, req.NamespacedName, u); err != nil {
		// Deleted since the event: its Deployments go with it (owner references), nothing to do.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if u.GetDeletionTimestamp() != nil {
		// Being deleted (a foreground delete waits for the garbage collector to remove the Deployments first):
		// recreating them would fight the deletion.
		return ctrl.Result{}, nil
	}
	var ro rolloutsv1alpha1.CanaryRollout
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &ro); err != nil {
		return ctrl.Result{}, r.reportUndecodable(ctx, u, err)
	}
	// The template as the API server stored it, defaults included (internal/revision says why).
	rev := revision.Of(&ro.Spec.Template)

	stable, err := r.readDeployment(ctx, &ro, TrackStable)
	if err == nil {
		var canary *appsv1.Deployment
		canary, err = r.readDeployment(ctx, &ro, TrackCanary)
		if err == nil {
			return r.reconcile(ctx, &ro, rev, stable, canary)
		}
	}
	var notOurs errNotOurs
	if errors.As(err, &notOurs) {
		return ctrl.Result{}, r.reportStuck(ctx, &ro, rev, ReasonDeploymentConflict, notOurs.Error())
	}
	return ctrl.Result{}, err
}

func (r *CanaryRolloutReconciler) reconcile(ctx context.Context, ro *rolloutsv1alpha1.CanaryRollout, rev string, stable, canary *appsv1.Deployment) (ctrl.Result, error) {
	plan := rollout.Decide(rollout.Input{
		Spec:     &ro.Spec,
		Revision: rev,
		Status: rollout.State{
			Phase:          ro.Status.Phase,
			StableRevision: ro.Status.StableRevision,
			CanaryRevision: ro.Status.CanaryRevision,
			CurrentStep:    ro.Status.CurrentStep,
			StepStartedAt:  ro.Status.StepStartedAt,
		},
		Stable: observe(stable),
		Canary: observe(canary),
		Now:    r.now(),
	})
	for _, w := range []struct {
		track  string
		cur    *appsv1.Deployment
		target rollout.Target
	}{{TrackStable, stable, plan.Stable}, {TrackCanary, canary, plan.Canary}} {
		err := r.apply(ctx, ro, w.track, rev, w.cur, w.target)
		switch {
		case err == nil:
		case apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err):
			// The cache was behind: the newer Deployment is an event of its own and brings another reconcile.
			return ctrl.Result{}, nil
		case apierrors.IsInvalid(err):
			// The template passed the CRD's schema but not the Deployment's validation (a label's syntax, a
			// container name). Nothing changes until the template does.
			msg := fmt.Sprintf("the API server refused Deployment %s: %v", DeploymentName(ro.Name, w.track), err)
			return ctrl.Result{}, r.reportStuck(ctx, ro, rev, ReasonInvalidSpec, msg)
		default:
			return ctrl.Result{}, err
		}
	}

	want := ro.Status.DeepCopy()
	want.ObservedGeneration = ro.Generation
	want.UpdateRevision = rev
	want.Phase = plan.State.Phase
	want.StableRevision = plan.State.StableRevision
	want.CanaryRevision = plan.State.CanaryRevision
	want.CurrentStep = plan.State.CurrentStep
	want.StepStartedAt = plan.State.StepStartedAt
	// Ready pods as read, before this reconcile's writes: the writes' effects come back as Deployment events.
	want.StableReplicas, want.CanaryReplicas = readyOf(stable), readyOf(canary)
	want.CanaryWeight = rollout.Weight(want.CanaryReplicas, want.StableReplicas)
	progressing := metav1.ConditionFalse
	if plan.Progressing() {
		progressing = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&want.Conditions, metav1.Condition{
		Type: ConditionProgressing, Status: progressing, Reason: plan.Reason, Message: plan.Message, ObservedGeneration: ro.Generation,
	})
	meta.SetStatusCondition(&want.Conditions, available(ro, want))
	written, err := r.writeStatus(ctx, ro, want)
	if err != nil {
		return ctrl.Result{}, err
	}
	// Events go out once their transition is recorded: a reconcile whose status write lost a conflict decides again
	// from the same last status and would repeat them.
	if written {
		for _, e := range plan.Events {
			r.Recorder.Eventf(ro, nil, corev1.EventTypeNormal, e.Reason, eventActionRollout, "%s", e.Message)
		}
	}
	// The requeue goes back even when nothing was written: the reconcile the status write itself causes finds nothing
	// to write, and it must still ask to be woken at the pause's end.
	return ctrl.Result{RequeueAfter: plan.RequeueAfter}, nil
}

// available is True when the ready pods of both Deployments add up to spec.replicas.
func available(ro *rolloutsv1alpha1.CanaryRollout, st *rolloutsv1alpha1.CanaryRolloutStatus) metav1.Condition {
	ready := st.StableReplicas + st.CanaryReplicas
	c := metav1.Condition{
		Type: ConditionAvailable, Status: metav1.ConditionTrue, Reason: ReasonEnoughPods, ObservedGeneration: ro.Generation,
		Message: fmt.Sprintf("%d of %d pods ready (%d stable, %d canary)", ready, ro.Spec.Replicas, st.StableReplicas, st.CanaryReplicas),
	}
	if ready < ro.Spec.Replicas {
		c.Status, c.Reason = metav1.ConditionFalse, ReasonNotEnoughPods
	}
	return c
}

// reportStuck records why the rollout can't go on (Progressing False with reason and message) and a Warning Event,
// and leaves the rest of the status, and the Deployments, as they were.
func (r *CanaryRolloutReconciler) reportStuck(ctx context.Context, ro *rolloutsv1alpha1.CanaryRollout, rev, reason, msg string) error {
	if len(msg) > maxMessage {
		msg = msg[:maxMessage]
	}
	want := ro.Status.DeepCopy()
	want.ObservedGeneration = ro.Generation
	want.UpdateRevision = rev
	meta.SetStatusCondition(&want.Conditions, metav1.Condition{
		Type: ConditionProgressing, Status: metav1.ConditionFalse, Reason: reason, Message: msg, ObservedGeneration: ro.Generation,
	})
	written, err := r.writeStatus(ctx, ro, want)
	if written {
		r.Recorder.Eventf(ro, nil, corev1.EventTypeWarning, reason, eventActionValidation, "%s", msg)
	}
	return err
}

// writeStatus patches the rollout's status to want, if it differs, and says whether it wrote. The patch carries the
// resourceVersion read, so it fails if the object changed since: a status computed from an older spec is never
// written over a newer one. A conflict is no error: the change that caused it is an event of its own.
func (r *CanaryRolloutReconciler) writeStatus(ctx context.Context, ro *rolloutsv1alpha1.CanaryRollout, want *rolloutsv1alpha1.CanaryRolloutStatus) (bool, error) {
	if equality.Semantic.DeepEqual(*want, ro.Status) {
		return false, nil
	}
	patch := client.MergeFromWithOptions(ro.DeepCopy(), client.MergeFromWithOptimisticLock{})
	ro.Status = *want
	if err := r.Status().Patch(ctx, ro, patch); err != nil {
		if apierrors.IsConflict(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (r *CanaryRolloutReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func readyOf(d *appsv1.Deployment) int32 {
	if d == nil {
		return 0
	}
	return d.Status.ReadyReplicas
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

// SetupWithManager registers the reconciler: it runs for each change to a CanaryRollout, to a Deployment one owns, and
// to a Deployment that has one of a rollout's names without being its own (so a DeploymentConflict clears itself when
// that Deployment goes). Rollouts are watched metadata-only (Reconcile says why).
func (r *CanaryRolloutReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&rolloutsv1alpha1.CanaryRollout{}, builder.OnlyMetadata).
		Owns(&appsv1.Deployment{}).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(r.rolloutNamedFor)).
		Named("canaryrollout").
		Complete(r)
}

// rolloutNamedFor maps a Deployment called <name>-stable or <name>-canary that no CanaryRollout controls to rollout
// <name> in its namespace, if one exists (checked in the metadata cache the rollout watch already keeps, so other
// Deployments cost no request).
func (r *CanaryRolloutReconciler) rolloutNamedFor(ctx context.Context, d client.Object) []reconcile.Request {
	if ref := metav1.GetControllerOf(d); ref != nil && ref.Kind == "CanaryRollout" && strings.HasPrefix(ref.APIVersion, rolloutsv1alpha1.GroupVersion.Group+"/") {
		return nil // Owns handles it
	}
	name, ok := strings.CutSuffix(d.GetName(), "-"+TrackStable)
	if !ok {
		if name, ok = strings.CutSuffix(d.GetName(), "-"+TrackCanary); !ok {
			return nil
		}
	}
	key := client.ObjectKey{Namespace: d.GetNamespace(), Name: name}
	ro := &metav1.PartialObjectMetadata{}
	ro.SetGroupVersionKind(rolloutsv1alpha1.GroupVersion.WithKind("CanaryRollout"))
	if err := r.Get(ctx, key, ro); err != nil {
		return nil
	}
	return []reconcile.Request{{NamespacedName: key}}
}
