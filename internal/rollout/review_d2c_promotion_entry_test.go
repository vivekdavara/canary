package rollout

import (
	"slices"
	"testing"
	"time"
)

// Review d2c: bdc3c33 makes "a stable at the revision under test" a lost promotion only at the last step, so round 2's
// false Promoting Event (a stable deleted mid-rollout and recreated from the template, that status write lost) is
// gone at step 1 of 2 but not at step 2 of 2: there the last step's pause hasn't even started, and the rollout reports
// "the last step passed; promoting" and Promoted. Any rollout with one step is always at its last step.
func TestReviewD2cStableRecreatedAtTheLastStepIsNotAPassedLastStep(t *testing.T) {
	w := newWorld(t, 4, step(25, 0), step(50, time.Hour))
	w.run()
	w.settle()
	w.rev = "r2"
	w.run() // step 1 of 2: canary 1
	w.settle()
	w.run() // a 0s pause: on to step 2 of 2, canary 2, its pause not started
	if w.state.CurrentStep == nil || *w.state.CurrentStep != 1 || w.state.StepStartedAt != nil {
		t.Fatalf("setup: %+v", w.state)
	}
	w.events = nil
	last := w.state
	w.stable = nil // deleted by hand
	w.run()        // recreates the stable from the template; this status write is lost
	w.state = last
	w.run()
	w.settle()
	w.run()
	if slices.Contains(w.events, EventPromoting) || slices.Contains(w.events, EventPromoted) {
		t.Fatalf("Events %v for a rollout whose last step's pause never started (its stable was deleted and recreated, and one status write lost)", w.events)
	}
}

// Review d2c: a promotion whose status write was lost (the stable already took the template) is no longer recognized
// once a step is added before the next reconcile: the status's step isn't the last any more, so the decision ends the
// rollout through the "nothing to roll out" branch, with no Promoting and no Promoted Event (Events go out only with
// the status write), and no RolloutReverted either. The rollout's Events stop at "step 2 of 2" and its status says
// Healthy: the promotion is never reported. Before bdc3c33 the same decision sent Promoting and Promoted.
func TestReviewD2cLostPromotionThenAStepAddedIsStillReported(t *testing.T) {
	w := newWorld(t, 4, step(25, 0), step(50, 0))
	w.run()
	w.settle()
	w.rev = "r2"
	w.run() // step 1 of 2
	w.settle()
	w.run() // step 2 of 2
	w.settle()
	w.events = nil
	last := w.state
	w.run() // the last step passes: the stable takes r2 ... and this status write is lost, with its Promoting Event
	if w.stable.Revision != "r2" || w.state.Phase != "Promoting" {
		t.Fatalf("setup: %+v, stable %+v", w.state, w.stable)
	}
	w.state, w.events = last, nil
	w.spec.Steps = append(w.spec.Steps, step(75, time.Hour)) // a step added meanwhile
	for range 3 {
		w.run()
		w.settle()
	}
	if w.state.Phase != "Healthy" || w.stable.Revision != "r2" {
		t.Fatalf("setup: ended %+v, stable %+v", w.state, w.stable)
	}
	if !slices.Contains(w.events, EventPromoted) {
		t.Fatalf("revision r2 was promoted (the stable runs it, the rollout is Healthy) but the Events sent are %v: no Promoting or Promoted", w.events)
	}
}
