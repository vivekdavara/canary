package rollout

import (
	"testing"
	"time"
)

// Review d2b: the new "a pause belongs to the pods it started with" rule clears stepStartedAt whenever the canary isn't
// set to the step's count. A step advance writes the canary at the next step's count before the status; if that status
// write is lost (a conflict with any concurrent edit of the rollout), the next decision reads the old step with its
// finished pause, finds the canary at the next step's count, and clears the pause: the canary is scaled back down to
// the old step's count and the old step's whole pause runs again (an hour here). Before the fix the finished pause was
// kept and the step advanced again at once.
func TestReviewD2bLostStatusWriteOnAStepAdvanceDoesNotRepeatThePause(t *testing.T) {
	w := newWorld(t, 4, step(25, time.Hour), step(50, time.Hour))
	w.run()
	w.settle()
	w.rev = "r2"
	w.run() // step 1 of 2: canary 1, stable kept at 4
	w.settle()
	w.run() // the pause starts, stable to 3
	w.settle()
	w.now = w.now.Add(time.Hour + time.Second)
	last := w.state
	w.run() // the pause is over: step 2, canary to 2 ... and this status write is lost
	if w.canary.Replicas != 2 {
		t.Fatalf("setup: the advance set the canary to %d; want 2", w.canary.Replicas)
	}
	w.state = last
	for range 4 { // pods get ready, a few reconciles, no time passes
		p := w.run()
		w.settle()
		if w.canary.Replicas < 2 {
			t.Fatalf("after the lost advance the canary went back to %d pods (%s: %s)", w.canary.Replicas, p.Reason, p.Message)
		}
	}
	if w.state.CurrentStep == nil || *w.state.CurrentStep != 1 {
		t.Fatalf("after the lost advance the rollout is at %+v; want step index 1 (the finished pause of step 0 ran again)", w.state)
	}
}
