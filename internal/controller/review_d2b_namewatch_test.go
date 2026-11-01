package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	rolloutsv1alpha1 "github.com/vivekdavara/canary/api/v1alpha1"
	"github.com/vivekdavara/canary/internal/testenv"
)

// Review d2b: rolloutNamedFor skips every Deployment controlled by a CanaryRollout ("Owns handles it"), but Owns
// enqueues the controller's name, not the rollout whose name the Deployment has. A Deployment web-stable controlled by
// rollout "other" (a manifest copied from other-stable and renamed keeps its ownerReferences) puts rollout web in
// DeploymentConflict; deleting it enqueues only "other", so web stays stuck until something else touches it (the
// resync, hours later). TestSomeoneElsesDeploymentWithTheName is the same with no owner reference, and passes.
func TestReviewD2bConflictWithAnotherRolloutsDeploymentClears(t *testing.T) {
	c := startOperator(t)
	ctx := context.Background()
	ns := testenv.Get(t).Namespace(t)
	theirs := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "web-stable", OwnerReferences: []metav1.OwnerReference{{
			APIVersion: rolloutsv1alpha1.GroupVersion.String(), Kind: "CanaryRollout", Name: "other",
			UID: types.UID("00000000-0000-0000-0000-00000000beef"), Controller: ptr.To(true),
		}}},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](7),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "theirs"}},
			Template: newRollout(ns, "example.com/theirs:1").Spec.Template,
		},
	}
	theirs.Spec.Template.Labels = map[string]string{"app": "theirs"}
	if err := c.Create(ctx, theirs); err != nil {
		t.Fatal(err)
	}
	ro := newRollout(ns, "example.com/web:1")
	if err := c.Create(ctx, ro); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKeyFromObject(ro)
	eventually(t, c, key, func(r *rolloutsv1alpha1.CanaryRollout) bool {
		cond := meta.FindStatusCondition(r.Status.Conditions, ConditionProgressing)
		return cond != nil && cond.Reason == ReasonDeploymentConflict
	})
	if err := c.Delete(ctx, theirs); err != nil {
		t.Fatal(err)
	}
	got := eventually(t, c, key, phase(rolloutsv1alpha1.PhaseHealthy))
	deploymentWhen(t, c, ns, "web-stable", at(got.Status.StableRevision, 4))
}
