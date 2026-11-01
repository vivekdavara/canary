package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	rolloutsv1alpha1 "github.com/vivekdavara/canary/api/v1alpha1"
	"github.com/vivekdavara/canary/internal/testenv"
)

// Review (day 2): stepStartedAt is set to the reconcile's clock (nanoseconds) and written; the API server stores a
// metav1.Time in RFC 3339 seconds, so the next reconcile (the one the status write itself causes, at once) reads it
// back up to 1s earlier and computes the pause's end, and its requeue, from that. Every pause then ends up to a second
// before "pause after the step's pods were ready". TestAPauseEndsOnItsOwn compares with the stored (truncated) start,
// so it can't see it. Driven by Reconcile calls with a fixed clock, so it is deterministic.
func TestReviewD2PauseRunsItsFullLength(t *testing.T) {
	env := testenv.Get(t)
	ctx := context.Background()
	ns := env.Namespace(t)
	clock := time.Date(2026, 11, 1, 10, 0, 0, 900_000_000, time.UTC)
	r := &CanaryRolloutReconciler{
		Client: env.Client, APIReader: env.Client, Recorder: events.NewFakeRecorder(100),
		Now: func() time.Time { return clock },
	}
	ro := newRollout(ns, "example.com/web:1")
	ro.Spec.Replicas = 2
	ro.Spec.Steps = []rolloutsv1alpha1.Step{{Weight: 50, Pause: &metav1.Duration{Duration: 2 * time.Minute}}}
	if err := env.Client.Create(ctx, ro); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKeyFromObject(ro)
	reconcile := func() ctrl.Result {
		t.Helper()
		res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	get := func() *rolloutsv1alpha1.CanaryRollout {
		t.Helper()
		var got rolloutsv1alpha1.CanaryRollout
		if err := env.Client.Get(ctx, key, &got); err != nil {
			t.Fatal(err)
		}
		return &got
	}

	reconcile() // the stable
	markReady(t, env.Client, ns, "web-stable")
	got := get()
	got.Spec.Template.Spec.Containers[0].Image = "example.com/web:2"
	if err := env.Client.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	reconcile() // the canary's pod
	markReady(t, env.Client, ns, "web-canary")
	ready := clock // the step's pods are ready now: the pause runs until ready + 2m
	// The start is rounded up to a whole second (what the API server stores), so the pause runs 2m to 2m1s.
	if res := reconcile(); res.RequeueAfter < 2*time.Minute || res.RequeueAfter > 2*time.Minute+time.Second {
		t.Fatalf("requeue %v; want 2m to 2m1s", res.RequeueAfter)
	}
	if res := reconcile(); res.RequeueAfter < 2*time.Minute {
		t.Errorf("the next reconcile (the status write's own event) asks to be woken after %v, at %v; the pause ends at %v "+
			"(stored stepStartedAt %v)", res.RequeueAfter, clock.Add(res.RequeueAfter), ready.Add(2*time.Minute), get().Status.StepStartedAt)
	}
	clock = ready.Add(2*time.Minute - 500*time.Millisecond)
	reconcile()
	if p := get().Status.Phase; p != rolloutsv1alpha1.PhaseProgressing {
		t.Errorf("phase %s at %v, 500ms before the pause's end %v", p, clock, ready.Add(2*time.Minute))
	}
}
