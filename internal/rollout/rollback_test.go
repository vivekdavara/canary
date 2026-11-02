package rollout

import (
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/vivekdavara/canary/api/v1alpha1"
	"github.com/vivekdavara/canary/internal/analysis"
)

// withDeadline sets the progress deadline in seconds.
func (w *world) withDeadline(seconds int32) *world {
	w.spec.ProgressDeadlineSeconds = ptr(seconds)
	return w
}

// withAnalysis gives the rollout one metric checked every interval, tolerating limit failed checks per step.
func (w *world) withAnalysis(interval time.Duration, limit int32) *world {
	max := 0.01
	w.spec.Analysis = &v1.Analysis{
		Interval:     &metav1.Duration{Duration: interval},
		FailureLimit: ptr(limit),
		Metrics:      []v1.Metric{{Name: "error-rate", Query: "q", Max: &max}},
	}
	return w
}

// started is a world with r1 stable and settled, then the template at r2: a rollout at step 0, nothing ready.
func (w *world) started() *world {
	w.run()
	w.settle()
	w.rev = "r2"
	w.run()
	return w
}

// A step whose canary pods aren't ready by the deadline rolls back: the stable, which never went down, keeps its
// pods; the canary goes to 0 at once since the stable is ready; the rollout then holds until the template changes.
func TestTheDeadlineRollsBack(t *testing.T) {
	w := newWorld(t, 10, step(10, time.Minute), step(50, 0)).withDeadline(300).started()
	w.expect(v1.PhaseProgressing, 10, 1)
	start := w.state.StepWaitingSince.Time

	w.now = start.Add(299 * time.Second)
	if p := w.run(); p.Reason != ReasonWaitingForCanary || p.RequeueAfter != time.Second {
		t.Fatalf("plan %+v; want waiting with 1s to the deadline", p)
	}
	w.now = start.Add(300 * time.Second)
	p := w.run()
	w.expect(v1.PhaseRolledBack, 10, 0)
	if p.Progressing() || p.Reason != ReasonRolledBack || w.state.RolledBackRevision != "r2" || w.state.CanaryRevision != "" ||
		w.state.StableRevision != "r1" || w.state.StepWaitingSince != nil || *w.state.CurrentStep != 0 {
		t.Fatalf("plan %+v, state %+v", p, w.state)
	}
	if want := []string{EventRolloutStarted, EventRolledBack}; !slices.Equal(w.events, want) || !slices.Equal(w.warnings, []string{EventRolledBack}) {
		t.Fatalf("events %v, warnings %v; want %v with RolledBack a Warning", w.events, w.warnings, want)
	}
	if !strings.Contains(p.Message, "step 1 of 2 (10%) wasn't ready within the progress deadline of 5m0s") {
		t.Fatalf("message %q", p.Message)
	}

	// The hold: pods becoming ready and time passing change nothing, and nothing is timed.
	for range 3 {
		w.settle()
		w.now = w.now.Add(time.Hour)
		if p = w.run(); p.RequeueAfter != 0 || p.Reason != ReasonRolledBack {
			t.Fatalf("plan %+v; want the hold", p)
		}
	}
	w.expect(v1.PhaseRolledBack, 10, 0)
	if len(w.events) != 2 || w.canary.Revision != "r2" {
		t.Fatalf("events %v, canary %+v; want nothing new", w.events, w.canary)
	}

	// A new template starts a new rollout; the rolled-back revision stays recorded.
	w.rev = "r3"
	w.run()
	w.expect(v1.PhaseProgressing, 10, 1)
	if w.state.CanaryRevision != "r3" || w.state.RolledBackRevision != "r2" || w.canary.Revision != "r3" {
		t.Fatalf("state %+v, canary %+v", w.state, w.canary)
	}
}

// The deadline runs per step, from when the step began waiting: step 2's own 300s start when step 1's pause ends.
func TestTheDeadlineIsPerStep(t *testing.T) {
	w := newWorld(t, 10, step(10, 0), step(50, 0)).withDeadline(300).started()
	w.now = w.now.Add(200 * time.Second)
	w.settle()
	w.run() // step 1 ready at 200s; its 0s pause passes; step 2 waits from here
	w.expect(v1.PhaseProgressing, 10, 5)
	w.now = w.now.Add(299 * time.Second)
	if p := w.run(); p.Reason != ReasonWaitingForCanary {
		t.Fatalf("plan %+v; want step 2 still waiting at 499s", p)
	}
	w.now = w.now.Add(time.Second)
	w.run()
	if w.state.Phase != v1.PhaseRolledBack {
		t.Fatalf("state %+v; want rolled back at step 2's deadline", w.state)
	}
}

// A rollback with the stable down to the step's count: the stable goes back to all replicas, and the canary keeps its
// pods until the stable's are ready, so capacity never drops.
func TestARollbackKeepsTheCanaryUntilTheStableIsBack(t *testing.T) {
	w := newWorld(t, 4, step(50, time.Hour)).withAnalysis(30*time.Second, 0).started()
	w.checks = func() analysis.Check { return failing }
	w.settle()
	w.run() // step 1's pause starts; the stable goes down to 2
	w.settle()
	w.expect(v1.PhaseProgressing, 2, 2)
	w.now = w.now.Add(30 * time.Second)
	p := w.run() // the first check fails: no failure allowed
	w.expect(v1.PhaseRolledBack, 4, 2)
	if p.Checked == nil || p.Checked.Passed || !strings.Contains(p.Checked.Message, "step 1 of 1 (50%), check 1 failed (1 failed, 0 allowed): error-rate 0.2 (above max 0.01)") {
		t.Fatalf("checked %+v", p.Checked)
	}
	if want := []string{EventAnalysisFailed, EventRolledBack}; !slices.Equal(w.warnings, want) {
		t.Fatalf("warnings %v; want %v", w.warnings, want)
	}
	if w.state.Analysis.Checks != 1 || w.state.Analysis.FailedChecks != 1 {
		t.Fatalf("analysis %+v; want the failed step's counts kept", w.state.Analysis)
	}
	if p = w.run(); !strings.Contains(p.Message, "the canary keeps its 2 pods until the stable's 4 are ready") {
		t.Fatalf("message %q", p.Message)
	}
	w.expect(v1.PhaseRolledBack, 4, 2)
	w.settle()
	w.run()
	w.expect(v1.PhaseRolledBack, 4, 0)
}

// Checks run every interval from the pause's start: a 2m pause with a 30s interval has four, the last at the pause's
// end, and the step passes only then. The requeues land on each check.
func TestChecksRunEveryIntervalDuringThePause(t *testing.T) {
	w := newWorld(t, 10, step(10, 2*time.Minute), step(50, time.Hour)).withAnalysis(30*time.Second, 0).started()
	w.settle()
	ready := w.now
	p := w.run()
	if p.RequeueAfter != 30*time.Second || p.AnalysisDue || w.checked != 0 {
		t.Fatalf("plan %+v; want the first check in 30s", p)
	}
	for k := 1; k <= 4; k++ {
		w.now = ready.Add(time.Duration(k) * 30 * time.Second)
		p = w.run()
		if w.checked != k || p.Checked == nil || !p.Checked.Passed || (k < 4 && w.state.Analysis.Checks != int32(k)) {
			t.Fatalf("at %ds: %d checks run, state %+v, checked %+v", k*30, w.checked, w.state.Analysis, p.Checked)
		}
		if k < 4 && (p.RequeueAfter != 30*time.Second || *w.state.CurrentStep != 0) {
			t.Fatalf("at %ds: plan %+v; want the next check in 30s, still step 0", k*30, p)
		}
	}
	// The fourth check, at the pause's end, passed the step.
	if *w.state.CurrentStep != 1 || w.state.Analysis != (Analysis{}) {
		t.Fatalf("state %+v; want step 1 with no checks yet", w.state)
	}
	// Between checks, nothing is due and nothing runs.
	w.settle()
	w.now = w.now.Add(10 * time.Second)
	w.run()
	w.now = w.now.Add(10 * time.Second)
	w.run()
	if w.checked != 4 {
		t.Fatalf("%d checks; want 4", w.checked)
	}
}

// With analysis, a step passes only after a check: a pause shorter than the interval (here 0s) lasts until the first.
func TestAPauseShorterThanTheIntervalWaitsForACheck(t *testing.T) {
	w := newWorld(t, 10, step(10, 0), step(50, 0)).withAnalysis(45*time.Second, 0).started()
	w.settle()
	ready := w.now
	p := w.run()
	if p.Reason != ReasonStepPaused || p.RequeueAfter != 45*time.Second || *w.state.CurrentStep != 0 ||
		!strings.Contains(p.Message, "pause over, waiting for a check; checks: 0, 0 failed, the next at 2026-11-01T10:00:45Z") {
		t.Fatalf("plan %+v; want held for the first check in 45s", p)
	}
	w.expect(v1.PhaseProgressing, 9, 1)
	w.now = ready.Add(44 * time.Second)
	if w.run(); *w.state.CurrentStep != 0 || w.checked != 0 {
		t.Fatalf("state %+v; want still step 0, no check", w.state)
	}
	w.now = ready.Add(45 * time.Second)
	w.run()
	if *w.state.CurrentStep != 1 || w.checked != 1 {
		t.Fatalf("state %+v, %d checks; want step 1 after one", w.state, w.checked)
	}
}

// failureLimit failed checks are tolerated per step; one more rolls back. Each failure is a Warning Event.
func TestTheFailureLimit(t *testing.T) {
	w := newWorld(t, 10, step(10, 10*time.Minute)).withAnalysis(time.Minute, 2).started()
	results := []analysis.Check{failing, passing, failing, passing, failing}
	w.checks = func() analysis.Check { c := results[0]; results = results[1:]; return c }
	w.settle()
	ready := w.now
	w.run()
	for k := 1; k <= 4; k++ {
		w.now = ready.Add(time.Duration(k) * time.Minute)
		if w.run(); w.state.Phase != v1.PhaseProgressing {
			t.Fatalf("check %d: %+v; want still progressing", k, w.state)
		}
	}
	if w.state.Analysis.Checks != 4 || w.state.Analysis.FailedChecks != 2 {
		t.Fatalf("analysis %+v; want 4 checks, 2 failed", w.state.Analysis)
	}
	w.now = ready.Add(5 * time.Minute)
	p := w.run()
	if w.state.Phase != v1.PhaseRolledBack || !strings.Contains(w.lastEvent(p, EventRolledBack), "3 of step 1 of 1 (10%)'s checks failed, more than the 2 allowed") {
		t.Fatalf("state %+v, events %+v", w.state, p.Events)
	}
	if want := []string{EventAnalysisFailed, EventAnalysisFailed, EventAnalysisFailed, EventRolledBack}; !slices.Equal(w.warnings, want) {
		t.Fatalf("warnings %v; want %v", w.warnings, want)
	}
}

func (w *world) lastEvent(p Plan, reason string) string {
	for _, e := range p.Events {
		if e.Reason == reason {
			return e.Message
		}
	}
	return ""
}

// The checks belong to the pause: an edit that restarts it (here replicas, so the step's count changes) starts the
// count of failed checks over too.
func TestChecksStartOverWithThePause(t *testing.T) {
	w := newWorld(t, 10, step(10, 10*time.Minute)).withAnalysis(time.Minute, 1).started()
	w.checks = func() analysis.Check { return failing }
	w.settle()
	w.run()
	w.now = w.now.Add(time.Minute)
	w.run()
	if w.state.Analysis.FailedChecks != 1 || w.state.Phase != v1.PhaseProgressing {
		t.Fatalf("state %+v; want one failure tolerated", w.state)
	}
	w.spec.Replicas = 20 // 10% is now 2 canary pods: the pause waits for them again
	w.run()
	if w.state.StepStartedAt != nil || w.state.Analysis != (Analysis{}) {
		t.Fatalf("state %+v; want the pause and its checks cleared", w.state)
	}
	w.settle()
	w.run()
	w.now = w.now.Add(time.Minute)
	w.run()
	if w.state.Phase != v1.PhaseProgressing || w.state.Analysis.FailedChecks != 1 {
		t.Fatalf("state %+v; want one failure again, tolerated", w.state)
	}
}

// After a rollback the template is the rolled-back revision, so a deleted stable isn't recreated from it: that would
// run the failed version at all replicas. Nothing is created until the template changes; then the new template
// becomes the stable, as for any deleted stable.
func TestADeletedStableAfterARollback(t *testing.T) {
	w := newWorld(t, 4, step(25, 0)).withDeadline(60).started()
	w.now = w.now.Add(time.Minute)
	w.run()
	w.expect(v1.PhaseRolledBack, 4, 0)
	w.stable = nil
	p := w.run()
	if w.stable != nil || p.Reason != ReasonRolledBack || !strings.Contains(p.Message, "the stable Deployment is gone") || w.state.Phase != v1.PhaseRolledBack {
		t.Fatalf("stable %+v, plan %+v; want nothing created", w.stable, p)
	}
	w.rev = "r3"
	w.run()
	w.expect(v1.PhaseHealthy, 4, 0)
	if w.stable.Revision != "r3" || w.state.RolledBackRevision != "r2" {
		t.Fatalf("stable %+v, state %+v", w.stable, w.state)
	}
}

// After a rollback, the template set back to the stable's revision is simply Healthy (no RolloutReverted: no rollout
// was running), and setting it to the rolled-back revision after another rollout started tries it again.
func TestAfterARollbackTheTemplateChanges(t *testing.T) {
	w := newWorld(t, 4, step(25, 0)).withDeadline(60).started()
	w.now = w.now.Add(time.Minute)
	w.run()
	w.settle()
	w.rev = "r1"
	w.events = nil
	w.run()
	w.expect(v1.PhaseHealthy, 4, 0)
	if len(w.events) != 0 || w.state.RolledBackRevision != "r2" {
		t.Fatalf("events %v, state %+v", w.events, w.state)
	}

	w.rev = "r3"
	w.run()
	w.rev = "r2" // another rollout ran since: r2 is a template change like any other
	w.run()
	if w.state.Phase != v1.PhaseProgressing || w.state.CanaryRevision != "r2" {
		t.Fatalf("state %+v; want r2 rolled out again", w.state)
	}
}

// A rollback whose status write is lost is decided again (the deadline is still past), and its Events go out once,
// with the write that succeeds.
func TestARollbackWhoseStatusWasLost(t *testing.T) {
	w := newWorld(t, 4, step(25, 0)).withDeadline(60).started()
	w.now = w.now.Add(time.Minute)
	last := w.state
	w.run()
	w.state, w.events, w.warnings = last, nil, nil
	w.run()
	w.expect(v1.PhaseRolledBack, 4, 0)
	if !slices.Equal(w.warnings, []string{EventRolledBack}) {
		t.Fatalf("warnings %v", w.warnings)
	}
}

// A check that a decision says is due must run before anything else is decided: the plan without it holds the step.
func TestADueCheckHoldsTheStep(t *testing.T) {
	w := newWorld(t, 10, step(10, 0), step(50, 0)).withAnalysis(30*time.Second, 0).started()
	w.settle()
	w.run()
	w.now = w.now.Add(30 * time.Second)
	p := Decide(Input{Spec: &w.spec, Revision: w.rev, Status: w.state, Stable: w.stable, Canary: w.canary, Now: w.now})
	if !p.AnalysisDue || p.Reason != ReasonStepPaused || *p.State.CurrentStep != 0 || p.Checked != nil || p.State.Analysis.Checks != 0 {
		t.Fatalf("plan %+v; want the step held with a check due", p)
	}
}

// A 1s interval checks about every second even when reconciles come a fraction late: each check's time is kept rounded
// down to the second (as the API server stores it), so the next is never pushed later. A 3s pause gets three checks.
func TestASecondIntervalChecksEverySecond(t *testing.T) {
	w := newWorld(t, 2, step(50, 3*time.Second)).withAnalysis(time.Second, 0).started()
	w.now = w.now.Add(300 * time.Millisecond)
	w.settle()
	w.run() // the pause starts at the next whole second
	start := w.state.StepStartedAt.Time
	for _, late := range []time.Duration{1300, 2050, 3000} {
		w.now = start.Add(late * time.Millisecond)
		w.run()
	}
	if w.checked != 3 || w.state.Phase != v1.PhasePromoting {
		t.Fatalf("%d checks, state %+v; want 3 and the step passed", w.checked, w.state)
	}
}

// While the rollout holds after a rollback, its plan asks to keep the Progressing message, which says why it rolled
// back; the rollback's own plan carries that message.
func TestTheHoldKeepsTheRollbacksMessage(t *testing.T) {
	w := newWorld(t, 4, step(25, 0)).withDeadline(60).started()
	w.now = w.now.Add(time.Minute)
	p := w.run()
	if p.KeepMessage || !strings.Contains(p.Message, "wasn't ready within the progress deadline of 1m0s") {
		t.Fatalf("rollback plan %+v; want the deadline in its message", p)
	}
	if p = w.run(); !p.KeepMessage || p.Reason != ReasonRolledBack {
		t.Fatalf("hold plan %+v; want KeepMessage", p)
	}
}

// Found by TestRandomWalksWithAnalysis: a step held past its 0s pause for its first check, after a replicas decrease
// left the canary with more pods than the step's (11 -> 10 at 46%: 6 -> 5) while a stable pod was down. The hold
// scaled the canary down at once, taking the ready pods from 10 to 9; it keeps them now until the stable is settled.
func TestAStepHeldForItsCheckKeepsTheCanaryWhileTheStableIsShort(t *testing.T) {
	w := newWorld(t, 11, step(46, 0), step(56, 0)).withAnalysis(30*time.Second, 0).started()
	w.settle()
	w.run() // the 0s pause is over at once, but the step waits for its first check
	w.settle()
	w.expect(v1.PhaseProgressing, 5, 6)
	w.stable.ReadyReplicas, w.stable.Settled = 4, false
	w.spec.Replicas = 10
	if p := w.run(); p.Reason != ReasonStepPaused {
		t.Fatalf("plan %+v; want still held for the check", p)
	}
	w.expect(v1.PhaseProgressing, 5, 6)
	if ready := w.stable.ReadyReplicas + w.canary.ReadyReplicas; ready < 10 {
		t.Fatalf("%d pods ready; want 10", ready)
	}
	w.settle()
	w.run()
	w.expect(v1.PhaseProgressing, 5, 5) // the stable settled: now the canary goes down to the step's count
}

// A failure limit lowered mid-step below the failures so far rolls back at the next decision, check or not: a step
// never passes with more failed checks than the spec allows.
func TestALoweredFailureLimitRollsBack(t *testing.T) {
	w := newWorld(t, 10, step(10, 10*time.Minute)).withAnalysis(time.Minute, 2).started()
	w.checks = func() analysis.Check { return failing }
	w.settle()
	w.run()
	w.now = w.now.Add(time.Minute)
	w.run()
	if w.state.Phase != v1.PhaseProgressing || w.state.Analysis.FailedChecks != 1 {
		t.Fatalf("state %+v; want one failure tolerated", w.state)
	}
	w.spec.Analysis.FailureLimit = ptr(int32(0))
	w.now = w.now.Add(time.Second) // no check due
	p := w.run()
	if w.state.Phase != v1.PhaseRolledBack || w.checked != 1 || !strings.Contains(w.lastEvent(p, EventRolledBack), "1 of step 1 of 1 (10%)'s checks failed, more than the 0 allowed") {
		t.Fatalf("state %+v, %d checks, events %+v", w.state, w.checked, p.Events)
	}
}
