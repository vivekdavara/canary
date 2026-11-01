package controller

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	rolloutsv1alpha1 "github.com/vivekdavara/canary/api/v1alpha1"
	"github.com/vivekdavara/canary/internal/testenv"
)

// Review (day 2): Reconcile doesn't look at deletionTimestamp. With a foreground deletion (`kubectl delete
// canaryrollout web --cascade=foreground`), the rollout stays, terminating, while the garbage collector deletes its
// Deployments; each deletion is an event for the rollout, and Decide's "no stable: create it from the template at all
// replicas" recreates what the garbage collector just deleted (a full set of pods), so the operator fights the
// deletion. envtest has no garbage collector, so the test deletes web-stable itself, as the collector would.
func TestReviewD2ATerminatingRolloutIsLeftAlone(t *testing.T) {
	c := startOperator(t)
	ctx := context.Background()
	ns := testenv.Get(t).Namespace(t)
	ro := newRollout(ns, "example.com/web:1")
	if err := c.Create(ctx, ro); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKeyFromObject(ro)
	eventually(t, c, key, phase(rolloutsv1alpha1.PhaseHealthy))
	stable := deploymentWhen(t, c, ns, "web-stable", func(d *appsv1.Deployment) bool { return d != nil })

	if err := c.Delete(ctx, ro, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil {
		t.Fatal(err)
	}
	eventually(t, c, key, func(r *rolloutsv1alpha1.CanaryRollout) bool { return r.DeletionTimestamp != nil })
	if err := c.Delete(ctx, stable); err != nil { // the garbage collector's part
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var d appsv1.Deployment
		err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "web-stable"}, &d)
		if err == nil && d.UID != stable.UID {
			t.Fatalf("the operator recreated web-stable (%d replicas) for a rollout being deleted (created %v)",
				*d.Spec.Replicas, d.CreationTimestamp)
		}
		if err != nil && !apierrors.IsNotFound(err) {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
