package rollout

import (
	"testing"
	"time"

	v1 "github.com/vivekdavara/canary/api/v1alpha1"
)

// Review d2c: bdc3c33 keeps a pause that is "over" whatever the canary's count. "Over" is measured with the step's
// pause as it is now, so a steps edit mid-pause that raises the step's weight and shortens its pause makes a running
// pause over at once, with the canary at the old step's count: the decision advances (or promotes) without the edited
// step's canary pods ever being ready, so the new weight is never observed. Before bdc3c33 the pause was cleared (the
// canary isn't set to the step's count) and the step waited for its pods and paused again.
func TestReviewD2cStepsEditMidPauseSkipsTheEditedWeight(t *testing.T) {
	for name, steps := range map[string][]v1.Step{
		"advance": {step(25, time.Hour), step(100, 0)}, // to 100% straight from 25%
		"promote": {step(25, time.Hour)},               // promoted after 25%
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t, 4, steps...)
			w.run()
			w.settle()
			w.rev = "r2"
			w.run() // step 0: canary 1
			w.settle()
			w.run() // the 25% pause starts (1 hour)
			w.settle()
			if w.state.StepStartedAt == nil || w.canary.Replicas != 1 {
				t.Fatalf("setup: %+v, canary %+v", w.state, w.canary)
			}
			w.now = w.now.Add(10 * time.Minute)
			// The user makes step 0 a 50% step with a 5-minute pause.
			w.spec.Steps[0] = step(50, 5*time.Minute)
			w.events = nil
			p := w.run()
			if w.state.CurrentStep == nil || *w.state.CurrentStep != 0 || w.state.Phase != v1.PhaseProgressing {
				t.Fatalf("10 minutes into a 25%% pause (canary at 1 of 4), step 0 was edited to 50%% / 5m: the decision went to %s step %v (%s: %s, events %v) without the 50%% step's 2 canary pods ever being ready",
					w.state.Phase, *w.state.CurrentStep, p.Reason, p.Message, w.events)
			}
		})
	}
}
