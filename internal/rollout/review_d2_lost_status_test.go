package rollout

import (
	"slices"
	"testing"
	"time"

	v1 "github.com/vivekdavara/canary/api/v1alpha1"
)

// runLost decides and applies the plan's Deployment targets, but the status write fails (a conflict: the rollout
// changed since it was read), so the status stays as it was and the plan's Events aren't recorded (the reconciler
// records them only after a status write succeeds).
func (w *world) runLost() Plan {
	w.t.Helper()
	before, n := w.state, len(w.events)
	p := w.run()
	w.state, w.events = before, w.events[:n]
	return p
}

// Review (day 2): the operator writes the Deployments, then the status. When the status write of the promotion's
// first decision fails (the stable already took the template), the next decision enters the promotion through
// "stable.Revision == rev" and returns the Promoting state with no Event, so the Promoting Event is never recorded.
// TestAPromotionWhoseStatusWasLost codifies the loss (it wants exactly [Promoted] after the lost write).
func TestReviewD2PromotingEventIsNotLostWithTheStatusWrite(t *testing.T) {
	w := newWorld(t, 4, step(50, 0))
	w.run()
	w.settle()
	w.rev = "r2"
	w.run()
	w.settle()
	w.runLost() // the last step passed: the stable takes r2, the status write fails
	w.run()     // decided again: Promoting is written now
	w.expect(v1.PhasePromoting, 4, 2)
	w.settle()
	w.run()
	w.expect(v1.PhaseHealthy, 4, 0)
	want := []string{EventRolloutStarted, EventPromoting, EventPromoted}
	if !slices.Equal(w.events, want) {
		t.Errorf("events recorded %v; want %v: the promotion's start is a transition the status records, with no Event", w.events, want)
	}
}

// Review (day 2): a template flipped to r3 and quickly back to r2 mid-step. The reconcile that read r3 restarts at
// step 0 and gives the canary r3's template (its count isn't above step 0's, so it isn't held), then its status write
// loses the conflict with the user's flip back. The next decision reads r2 with a status still saying "r2, step 1,
// pods ready since 10:00" and carries on that step's pause: the canary runs (or is going back from) r3 pods, the
// status says r2's step-1 pods have been ready all along, and when the pause ends the rollout advances or, here,
// promotes r2 with no r2 canary pod ready since the flip. Decide trusts StepStartedAt without checking that the
// canary is still at the step's revision and count.
func TestReviewD2LostRestartThenTemplateBackRestartsThePause(t *testing.T) {
	w := newWorld(t, 4, step(10, 0), step(25, time.Hour)) // both steps run 1 canary and 3 stable pods
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
	w.expect(v1.PhaseProgressing, 3, 1)
	started := w.state.StepStartedAt.Time

	w.now = w.now.Add(10 * time.Minute)
	w.rev = "r3"
	w.runLost() // the canary takes r3's template; the status write loses to the flip back
	if w.canary.Revision != "r3" {
		t.Fatalf("setup: canary %+v; want r3's template", *w.canary)
	}
	w.settle() // r3's pod is ready: the canary runs r3

	w.rev = "r2"
	canaryBefore := *w.canary
	p = w.run()
	if p.Reason == ReasonStepPaused {
		t.Errorf("template back to r2 with the canary running %s: plan %s %q, step %d started %v (before the flip); "+
			"want the step to wait for r2's canary pods again", canaryBefore.Revision, p.Reason, p.Message,
			*w.state.CurrentStep, w.state.StepStartedAt.Time)
	}
	// And the pause's end, with r2's pod never ready since the flip, promotes.
	w.now = started.Add(time.Hour)
	p = w.run()
	if w.state.Phase == v1.PhasePromoting {
		t.Errorf("promoted r2 at %v with the canary %+v: no r2 canary pod has been ready since the flip", w.now, *w.canary)
	}
}

// Review (day 2): steps shrinking during the promotion leave status.currentStep out of range (the promotion branch
// copies last.CurrentStep without the clamp the step branch applies), so the status names a step that doesn't exist.
func TestReviewD2CurrentStepInRangeDuringPromotion(t *testing.T) {
	w := newWorld(t, 4, step(25, 0), step(50, 0))
	w.run()
	w.settle()
	w.rev = "r2"
	for range 2 {
		w.run()
		w.settle()
	}
	w.run()
	w.expect(v1.PhasePromoting, 4, 2)
	w.spec.Steps = w.spec.Steps[:1]
	w.run()
	if got := *w.state.CurrentStep; int(got) >= len(w.spec.Steps) {
		t.Errorf("Promoting with currentStep %d of %d steps", got, len(w.spec.Steps))
	}
}
