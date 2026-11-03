package rollout

import (
	"testing"
	"time"

	v1 "github.com/vivekdavara/canary/api/v1alpha1"
	"github.com/vivekdavara/canary/internal/analysis"
)

// Review of b6b6df8 (day 3, round 4). A pause belongs to the pods it started with: raising replicas mid-pause (the
// canary is set to 1 pod, the step now needs 2) restarts the pause and clears its checks and failures (DESIGN.md,
// "The state machine" 3 and "When checks run"), so on the normal path no check runs and nothing rolls back. With the
// stable gone, the branch runs the due check of the old pause anyway, adds its failure to the old pause's, and rolls
// back: the stable isn't recreated (day 2's rule) and the rollout holds with only the canary's pod serving, on a
// failure count the normal path would have cleared.
func TestReviewD4ARestartedPauseWithTheStableGoneDoesNotRollBack(t *testing.T) {
	mk := func() *world {
		w := newWorld(t, 4, step(25, time.Hour)).withAnalysis(time.Second, 1).started()
		w.checks = func() analysis.Check { return failing }
		w.settle()
		w.run() // the pause starts: 1 canary pod
		w.now = w.state.StepStartedAt.Add(time.Second)
		w.run()                        // check 1 fails: 1 failed, 1 allowed
		w.now = w.now.Add(time.Second) // check 2 due
		w.spec.Replicas = 8            // the step now needs 2 canary pods: the pause starts over
		return w
	}
	normal, gone := mk(), mk()
	gone.stable = nil
	nc, gc := normal.checked, gone.checked
	normal.run()
	gone.run()
	if normal.state.Phase != v1.PhaseProgressing || normal.checked != nc || normal.state.Analysis.FailedChecks != 0 {
		t.Fatalf("with the stable: %+v, %d checks; want the pause restarted, its failures cleared, no check", normal.state, normal.checked-nc)
	}
	if gone.state.Phase == v1.PhaseRolledBack || gone.checked != gc {
		t.Fatalf("with the stable gone: phase %s, %d checks run, analysis %+v, stable %+v; want no check of the restarted pause and no rollback (day 2's rule: the stable recreated)",
			gone.state.Phase, gone.checked-gc, gone.state.Analysis, gone.stable)
	}
}

// The same with the limit lowered below the old pause's failures instead of a check: alreadyFailed must not count
// the failures of a pause the normal path restarts (the check condition alone doesn't cover it: no check runs here).
func TestALoweredLimitDoesNotFailARestartedPauseWithTheStableGone(t *testing.T) {
	mk := func() *world {
		w := newWorld(t, 4, step(25, time.Hour)).withAnalysis(time.Second, 1).started()
		w.checks = func() analysis.Check { return failing }
		w.settle()
		w.run() // the pause starts: 1 canary pod
		w.now = w.state.StepStartedAt.Add(time.Second)
		w.run()                                      // check 1 fails: 1 failed, 1 allowed
		w.now = w.now.Add(500 * time.Millisecond)    // no check due
		w.spec.Replicas = 8                          // the pause starts over
		w.spec.Analysis.FailureLimit = ptr(int32(0)) // 1 failed, 0 allowed: only for the old pause
		return w
	}
	normal, gone := mk(), mk()
	gone.stable = nil
	normal.run()
	gone.run()
	if normal.state.Phase != v1.PhaseProgressing || normal.state.Analysis.FailedChecks != 0 {
		t.Fatalf("with the stable: %+v; want the pause restarted and its failures cleared", normal.state)
	}
	if gone.state.Phase == v1.PhaseRolledBack {
		t.Fatalf("with the stable gone: rolled back on the restarted pause's failures (%+v); want day 2's rule (the stable recreated)", gone.state.Analysis)
	}
}
