package rollout

import (
	"testing"
	"time"

	v1 "github.com/vivekdavara/canary/api/v1alpha1"
	"github.com/vivekdavara/canary/internal/analysis"
)

// Review of b6b6df8 (day 3, round 4). failureLimit is lowered below the failures so far while a check is due. On the
// normal path the decision rolls back at once, without the check (DESIGN.md: "as soon as the limit is lowered below
// the failures so far"): the rollback in decide returns before AnalysisDue is acted on. With the stable gone, the
// branch asks for the check first, so the operator runs a Prometheus check it doesn't need (up to --analysis-timeout),
// counts it (checks 2, failedChecks 2 instead of 1 and 1), records an extra AnalysisFailed Warning, and the rollback
// message says "2 of ... failed" where the normal path says 1. b6b6df8's own test says the two paths record "the
// same analysis status"; here they don't.
func TestReviewD4ALoweredLimitWithTheStableGoneRollsBackWithoutACheck(t *testing.T) {
	mk := func() *world {
		w := newWorld(t, 4, step(25, time.Hour)).withAnalysis(time.Second, 1).started()
		w.checks = func() analysis.Check { return failing }
		w.settle()
		w.run() // the pause starts
		w.now = w.state.StepStartedAt.Add(time.Second)
		w.run() // check 1 fails: 1 failed, 1 allowed
		if w.state.Phase != v1.PhaseProgressing || w.state.Analysis.FailedChecks != 1 {
			t.Fatalf("after one failed check: %+v; want Progressing with 1 failed", w.state)
		}
		w.now = w.now.Add(time.Second) // check 2 due
		w.spec.Analysis.FailureLimit = ptr(int32(0))
		return w
	}
	normal, gone := mk(), mk()
	gone.stable = nil
	nc, gc := normal.checked, gone.checked
	normal.run()
	gone.run()
	if normal.state.Phase != v1.PhaseRolledBack || gone.state.Phase != v1.PhaseRolledBack {
		t.Fatalf("phases %s, %s; want both rolled back", normal.state.Phase, gone.state.Phase)
	}
	if normal.checked-nc != gone.checked-gc || normal.state.Analysis.Checks != gone.state.Analysis.Checks ||
		normal.state.Analysis.FailedChecks != gone.state.Analysis.FailedChecks || len(normal.warnings) != len(gone.warnings) {
		t.Fatalf("limit lowered to 0 with 1 failure and a check due: with the stable, %d checks run, analysis %+v, warnings %v; with it gone, %d checks run, analysis %+v, warnings %v; want the same (no check: the status alone already says it failed)",
			normal.checked-nc, normal.state.Analysis, normal.warnings, gone.checked-gc, gone.state.Analysis, gone.warnings)
	}
}
