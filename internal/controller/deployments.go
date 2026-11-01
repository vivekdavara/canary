package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	rolloutsv1alpha1 "github.com/vivekdavara/canary/api/v1alpha1"
	"github.com/vivekdavara/canary/internal/rollout"
)

// The labels the operator sets (DESIGN.md, "The objects the operator writes"). The rollout and track labels are the
// Deployments' selectors, so the two never select each other's pods; the revision label goes on the pod template
// only, since selectors can't change.
const (
	LabelRollout  = rolloutsv1alpha1.LabelPrefix + "rollout"
	LabelTrack    = rolloutsv1alpha1.LabelPrefix + "track"
	LabelRevision = rolloutsv1alpha1.LabelPrefix + "revision"
	// AnnotationPromotedRevision on the stable Deployment names the revision a promotion gave it, written in the same
	// update as its template, so a promotion whose status write was lost is told apart from a stable recreated from
	// the template (rollout.Deployment.PromotedRevision).
	AnnotationPromotedRevision = rolloutsv1alpha1.LabelPrefix + "promoted-revision"

	TrackStable = "stable"
	TrackCanary = "canary"
)

// DeploymentName is the name of a rollout's Deployment of the given track: web-stable, web-canary.
func DeploymentName(rolloutName, track string) string { return rolloutName + "-" + track }

// errNotOurs is a Deployment with one of the rollout's names that the rollout doesn't control.
type errNotOurs struct{ name string }

func (e errNotOurs) Error() string {
	return fmt.Sprintf("Deployment %s exists and isn't controlled by this rollout; rename or delete it", e.name)
}

// readDeployment reads one of the rollout's Deployments from the cache: nil when it doesn't exist, errNotOurs when
// one of that name belongs to someone else.
func (r *CanaryRolloutReconciler) readDeployment(ctx context.Context, ro *rolloutsv1alpha1.CanaryRollout, track string) (*appsv1.Deployment, error) {
	var d appsv1.Deployment
	key := client.ObjectKey{Namespace: ro.Namespace, Name: DeploymentName(ro.Name, track)}
	if err := r.Get(ctx, key, &d); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if !metav1.IsControlledBy(&d, ro) {
		return nil, errNotOurs{key.Name}
	}
	return &d, nil
}

// observe is what the state machine needs to know of a Deployment.
func observe(d *appsv1.Deployment) *rollout.Deployment {
	if d == nil {
		return nil
	}
	want := ptr.Deref(d.Spec.Replicas, 1)
	return &rollout.Deployment{
		Revision:         d.Spec.Template.Labels[LabelRevision],
		PromotedRevision: d.Annotations[AnnotationPromotedRevision],
		Replicas:         want,
		ReadyReplicas:    d.Status.ReadyReplicas,
		// What `kubectl rollout status` waits for, with ready pods: the controller has seen the latest spec, and every
		// pod it counts runs the current template and is ready.
		Settled: d.Status.ObservedGeneration >= d.Generation &&
			d.Status.UpdatedReplicas == want && d.Status.ReadyReplicas == want && d.Status.Replicas == want,
	}
}

// podTemplate is the rollout's template with the operator's labels added.
func podTemplate(ro *rolloutsv1alpha1.CanaryRollout, track, rev string) corev1.PodTemplateSpec {
	t := *ro.Spec.Template.DeepCopy()
	labels := make(map[string]string, len(t.Labels)+3)
	for k, v := range t.Labels {
		labels[k] = v
	}
	labels[LabelRollout], labels[LabelTrack], labels[LabelRevision] = ro.Name, track, rev
	t.Labels = labels
	return t
}

// newDeployment is a rollout's Deployment of the given track, running its template at n pods. A template change
// replaces pods with none unavailable (maxUnavailable 0), so a promotion doesn't drop capacity.
func (r *CanaryRolloutReconciler) newDeployment(ro *rolloutsv1alpha1.CanaryRollout, track, rev string, n int32) (*appsv1.Deployment, error) {
	selector := map[string]string{LabelRollout: ro.Name, LabelTrack: track}
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ro.Namespace,
			Name:      DeploymentName(ro.Name, track),
			Labels:    map[string]string{LabelRollout: ro.Name, LabelTrack: track},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(n),
			Selector: &metav1.LabelSelector{MatchLabels: selector},
			Template: podTemplate(ro, track, rev),
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxUnavailable: ptr.To(intstr.FromInt32(0)),
					MaxSurge:       ptr.To(intstr.FromString("25%")),
				},
			},
		},
	}
	if err := controllerutil.SetControllerReference(ro, d, r.Scheme()); err != nil {
		return nil, err
	}
	return d, nil
}

// apply makes one Deployment match its target: created if missing and the target takes the template, otherwise its
// replicas set and, when the target takes the template and the revision differs, its pod template replaced. The
// update carries the resourceVersion read, so a stale read fails with a conflict rather than undoing a newer write.
func (r *CanaryRolloutReconciler) apply(ctx context.Context, ro *rolloutsv1alpha1.CanaryRollout, track, rev string, cur *appsv1.Deployment, t rollout.Target) error {
	if cur == nil {
		if !t.FromTemplate {
			return nil
		}
		d, err := r.newDeployment(ro, track, rev, t.Replicas)
		if err != nil {
			return err
		}
		return r.Create(ctx, d)
	}
	next := cur.DeepCopy()
	changed := false
	if ptr.Deref(cur.Spec.Replicas, 1) != t.Replicas {
		next.Spec.Replicas, changed = ptr.To(t.Replicas), true
	}
	if t.FromTemplate && cur.Spec.Template.Labels[LabelRevision] != rev {
		next.Spec.Template, changed = podTemplate(ro, track, rev), true
	}
	if t.Promote && cur.Annotations[AnnotationPromotedRevision] != rev {
		if next.Annotations == nil {
			next.Annotations = map[string]string{}
		}
		next.Annotations[AnnotationPromotedRevision], changed = rev, true
	}
	if !changed {
		return nil
	}
	return r.Update(ctx, next)
}
