package rollout

import (
	"slices"
	"testing"
	"time"
)

// Review d2b: the promotion is now also entered when the stable already runs the revision under test, on the
// assumption that only a promotion whose status write was lost gets it there, and the "lost" Promoting Event ("the
// last step passed") is sent. A stable deleted mid-rollout and recreated from the template gets there too: with its
// status write lost (a conflict with a concurrent spec edit is enough), a rollout still at step 1 of 2, with its pause
// not even started, reports Promoting ("the last step passed") and Promoted. With the write kept, the same decision
// ends the rollout with no Event at all, so the Events depend on whether one status write was lost.
func TestReviewD2bStableRecreatedMidRolloutIsNotAPassedLastStep(t *testing.T) {
	w := newWorld(t, 4, step(25, time.Hour), step(50, time.Hour))
	w.run()
	w.settle()
	w.rev = "r2"
	w.run() // step 1 of 2: canary 1, pause not started
	if w.state.CurrentStep == nil || *w.state.CurrentStep != 0 || w.state.StepStartedAt != nil {
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
		t.Fatalf("Events %v for a rollout whose first step's pause never started (its stable was deleted and recreated, and one status write lost)", w.events)
	}
}
