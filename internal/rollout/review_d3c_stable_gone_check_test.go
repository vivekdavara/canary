package rollout

import (
	"slices"
	"testing"
	"time"

	v1 "github.com/vivekdavara/canary/api/v1alpha1"
	"github.com/vivekdavara/canary/internal/analysis"
)

// Review of day 3, third round (237ba4f). With the stable gone mid-pause and a check due, the decision runs the check
// first. When it fails within the limit, day 2's rule follows (the stable is recreated from the template), but the
// check's result is thrown away: no Warning AnalysisFailed Event and no AnalysisPassing condition (Plan.Checked nil),
// though DESIGN.md says each failed check records one and AnalysisPassing is written whenever a check runs.
func TestReviewD3cAFailedCheckBeforeAStableIsRecreatedIsReported(t *testing.T) {
	w := newWorld(t, 4, step(25, time.Hour)).withAnalysis(time.Minute, 1).started()
	w.checks = func() analysis.Check { return failing }
	w.settle()
	w.run() // the pause starts
	w.now = w.state.StepStartedAt.Add(time.Minute)
	w.stable = nil // deleted, with a check due
	checked := w.checked
	p := w.run()
	if w.checked != checked+1 {
		t.Fatalf("%d checks ran; want the due one", w.checked-checked)
	}
	if !slices.Contains(w.warnings, EventAnalysisFailed) || p.Checked == nil || p.Checked.Passed {
		t.Fatalf("a check ran and failed (1 of 1 allowed), then the stable was recreated: warnings %v, Checked %+v; want an AnalysisFailed Warning and the AnalysisPassing result",
			w.warnings, p.Checked)
	}
}

// The same check run on the normal path and with the stable gone records the same analysis status. With the stable
// gone, a failed check that rolls back counts the failure but not the check, and leaves lastCheckAt as it was: the
// status says failedChecks 1 of checks 0 (DESIGN.md: the second decision makes checks go up and sets lastCheckAt).
func TestReviewD3cARollbackByCheckWithTheStableGoneCountsTheCheck(t *testing.T) {
	mk := func() *world {
		w := newWorld(t, 4, step(25, time.Hour)).withAnalysis(time.Second, 0).started()
		w.checks = func() analysis.Check { return failing }
		w.settle()
		w.run()
		w.now = w.state.StepStartedAt.Add(time.Second)
		return w
	}
	normal, gone := mk(), mk()
	gone.stable = nil
	normal.run()
	gone.run()
	if normal.state.Phase != v1.PhaseRolledBack || gone.state.Phase != v1.PhaseRolledBack {
		t.Fatalf("phases %s, %s; want both rolled back", normal.state.Phase, gone.state.Phase)
	}
	n, g := normal.state.Analysis, gone.state.Analysis
	if n.Checks != g.Checks || n.FailedChecks != g.FailedChecks || (n.LastCheckAt == nil) != (g.LastCheckAt == nil) ||
		(n.LastCheckAt != nil && !n.LastCheckAt.Equal(g.LastCheckAt)) {
		t.Fatalf("analysis with the stable: %+v; with it gone: %+v (checks %d, failedChecks %d); want the same", n, g, g.Checks, g.FailedChecks)
	}
}
