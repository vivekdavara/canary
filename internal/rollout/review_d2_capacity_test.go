package rollout

import (
	"testing"
	"time"

	v1 "github.com/vivekdavara/canary/api/v1alpha1"
)

// Review (day 2): the steps aren't part of the template, so a user can edit them mid-rollout (DESIGN.md says
// Decide keeps the index in range for that). While a step's pause runs, the pause branch applies the step's split
// to both Deployments in one plan, with no "canary up before the stable down" ordering and no "hold an oversized
// canary until the stable is settled" rule. A changed weight at the current index then drops ready pods below
// spec.replicas: a higher weight scales the stable down before the canary's new pods are ready; a lower one scales
// the canary down before the stable's new pods are ready.
func TestReviewD2StepsEditedMidPauseKeepCapacity(t *testing.T) {
	for _, tc := range []struct {
		name  string
		edit  []v1.Step // the steps after the edit; the rollout is at index 1, paused
		wantC int32     // step 1's canary count after the edit (r = 10)
	}{
		{"current step's weight raised 50 -> 80", []v1.Step{step(10, 0), step(80, time.Hour)}, 8},
		{"current step's weight lowered 50 -> 20", []v1.Step{step(10, 0), step(20, time.Hour)}, 2},
		{"a step inserted before the current one", []v1.Step{step(10, 0), step(20, time.Hour), step(50, time.Hour)}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t, 10, step(10, 0), step(50, time.Hour))
			w.run()
			w.settle()
			w.rev = "r2"
			for range 3 {
				w.run()
				w.settle()
			}
			p := w.run()
			if p.Reason != ReasonStepPaused || *w.state.CurrentStep != 1 {
				t.Fatalf("setup: plan %+v, state %+v; want step 1 paused", p, w.state)
			}
			w.expect(v1.PhaseProgressing, 5, 5)
			if w.stable.ReadyReplicas+w.canary.ReadyReplicas != 10 {
				t.Fatalf("setup: not all ready")
			}

			w.spec.Steps = tc.edit
			p = w.run()
			ready := w.stable.ReadyReplicas + w.canary.ReadyReplicas
			if ready < w.spec.Replicas {
				t.Errorf("after editing the steps mid-pause: %d ready pods of %d (stable %+v, canary %+v; plan %s %q): "+
					"the operator scaled one Deployment down before the other's new pods were ready",
					ready, w.spec.Replicas, *w.stable, *w.canary, p.Reason, p.Message)
			}
		})
	}
}

// Review (day 2): Steps shrinking mid-pause clamps the index, but StepStartedAt, the start of the old step's pause,
// is carried over to a step whose pods were never ready at its count. With the clamped step being the last one and a
// zero pause, the very next decision promotes, and the promotion's canary target is the clamped step's split (not
// the canary's pods): the canary goes down while the stable is still being replaced.
func TestReviewD2FewerStepsMidPauseKeepCapacity(t *testing.T) {
	w := newWorld(t, 10, step(10, 0), step(20, 0), step(30, time.Hour))
	w.run()
	w.settle()
	w.rev = "r2"
	for range 4 {
		w.run()
		w.settle()
	}
	p := w.run()
	if p.Reason != ReasonStepPaused || *w.state.CurrentStep != 2 {
		t.Fatalf("setup: plan %+v, state %+v; want step 2 paused", p, w.state)
	}
	w.expect(v1.PhaseProgressing, 7, 3)

	w.spec.Steps = []v1.Step{step(10, 0)}
	p = w.run()
	if ready := w.stable.ReadyReplicas + w.canary.ReadyReplicas; ready < w.spec.Replicas {
		t.Errorf("after cutting the steps to one mid-pause: %d ready pods of %d (stable %+v, canary %+v; plan %s %q)",
			ready, w.spec.Replicas, *w.stable, *w.canary, p.Reason, p.Message)
	}
}

// Review (day 2): the stable deleted mid-rollout (by a user, or `kubectl delete deploy web-stable` to "reset"). Its
// own pods go with it; that drop isn't the operator's. But the operator's decision then scales the canary, whose pods
// are ready and already run the template the new stable is created from, to 0 at once, before any of the new stable's
// pods are ready: the rollout goes from 2 ready pods to none. DESIGN's rule elsewhere ("the canary's pods stay up until
// the stable's new pods are ready") would keep them.
func TestReviewD2DeletedStableKeepsTheCanaryUntilTheNewStableIsReady(t *testing.T) {
	w := newWorld(t, 4, step(50, time.Hour))
	w.run()
	w.settle()
	w.rev = "r2"
	w.run()
	w.settle()
	w.run()
	w.expect(v1.PhaseProgressing, 2, 2)

	w.stable = nil // deleted, with its pods
	before := readyOf(w.canary)
	p := w.run()
	if after := w.stable.ReadyReplicas + readyOf(w.canary); after < before {
		t.Errorf("the operator took the ready pods from %d to %d (canary target %+v) while the new stable has none ready",
			before, after, p.Canary)
	}
}
