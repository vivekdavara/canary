package rollout

import (
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/vivekdavara/canary/api/v1alpha1"
	"github.com/vivekdavara/canary/internal/analysis"
)

// Deployment is what the operator read of one of its two Deployments.
type Deployment struct {
	// Revision is the revision label on its pod template: the version its pods run, or are being replaced with.
	Revision string
	// Replicas is its spec.replicas.
	Replicas int32
	// ReadyReplicas is its status.readyReplicas.
	ReadyReplicas int32
	// Settled says the Deployment controller has caught up with the spec: status.observedGeneration is current, and
	// all Replicas pods run the current template and are ready, with no others left.
	Settled bool
	// PromotedRevision is the revision a promotion gave the stable, written with its template; empty for a stable
	// created from the template (a first apply, or a deleted stable recreated).
	PromotedRevision string
}

// readyAt says d exists and is settled at n pods of revision rev.
func (d *Deployment) readyAt(rev string, n int32) bool {
	return d != nil && d.Settled && d.Revision == rev && d.Replicas == n
}

// State is the part of the status the state machine owns.
type State struct {
	Phase          v1.Phase
	StableRevision string
	CanaryRevision string
	// RolledBackRevision is the last revision rolled back. It is kept until another rollback replaces it; the
	// rollout holds while the template is that revision and the phase is RolledBack.
	RolledBackRevision string
	CurrentStep        *int32
	StepStartedAt      *metav1.Time
	// StepWaitingSince is when the current step began waiting for its canary pods (the progress deadline runs from
	// it); nil once its pause has started.
	StepWaitingSince *metav1.Time
	// Analysis counts the current pause's checks.
	Analysis Analysis
}

// Analysis counts one step's checks: they begin with its pause, and start over when the pause does.
type Analysis struct {
	Checks, FailedChecks int32
	LastCheckAt          *metav1.Time
}

// Input is everything a decision depends on.
type Input struct {
	Spec *v1.CanaryRolloutSpec
	// Revision is the revision of Spec.Template.
	Revision string
	// Status is the state the last reconcile wrote.
	Status State
	// Stable and Canary are the two Deployments, nil when missing.
	Stable, Canary *Deployment
	Now            time.Time
	// Check is the check the operator ran because a decision from this same input said one was due
	// (Plan.AnalysisDue); nil otherwise.
	Check *analysis.Check
}

// Target is what one Deployment should become.
type Target struct {
	// Replicas is the spec.replicas to set.
	Replicas int32
	// FromTemplate says its pod template should be the rollout's spec.template: it is created if missing, and its
	// template replaced if its revision differs. Otherwise its template is left as it is, and a missing one isn't
	// created.
	FromTemplate bool
	// Promote marks the stable as promoted to the template's revision (Deployment.PromotedRevision), in the same
	// write as its template.
	Promote bool
}

// Event is an Event to record on the rollout.
type Event struct {
	Reason, Message string
	// Warning makes it a Warning Event; otherwise it is Normal.
	Warning bool
}

// Reasons of the Progressing condition and of Events.
const (
	ReasonIdle             = "Idle"             // no rollout in progress
	ReasonWaitingForCanary = "WaitingForCanary" // a step's canary pods aren't all ready yet
	ReasonStepPaused       = "StepPaused"       // a step's pods are ready and its pause is running
	ReasonPromoting        = "Promoting"        // the stable is taking the canary's version
	ReasonRolledBack       = "RolledBack"       // the rollout failed; the stable runs the old version

	EventRolloutStarted  = "RolloutStarted"
	EventStepAdvanced    = "StepAdvanced"
	EventPromoting       = "Promoting"
	EventPromoted        = "Promoted"
	EventRolloutReverted = "RolloutReverted"
	EventAnalysisFailed  = "AnalysisFailed" // Warning: a check failed
	EventRolledBack      = "RolledBack"     // Warning

	// Reasons of the AnalysisPassing condition.
	ReasonChecksPassed = "ChecksPassed"
	ReasonCheckFailed  = "CheckFailed"
)

// Defaults of the spec's optional fields, which the API server fills in; the operator falls back to them for a spec
// that didn't go through it (the pure tests).
const (
	DefaultProgressDeadline = 600 * time.Second
	DefaultInterval         = 30 * time.Second
)

// Plan is what the operator does next.
type Plan struct {
	Stable, Canary Target
	// State is the status afterwards; Reason and Message are for the Progressing condition (True while a rollout is
	// in progress, False when Idle).
	State           State
	Reason, Message string
	// RequeueAfter is when the next timed decision is due (the end of a pause); 0 for none, since changes to the
	// rollout and its Deployments bring reconciles of their own.
	RequeueAfter time.Duration
	Events       []Event
	// KeepMessage says the Progressing condition keeps the message it has if its reason is already Reason: a rollout
	// that rolled back keeps the message that says why while it holds, and Message is for when it has none.
	KeepMessage bool
	// AnalysisDue says a check is due: the operator runs one and decides again from the same input with its result
	// (Input.Check). The rest of such a plan is safe to carry out as it is (the step holds), but it isn't meant to be.
	AnalysisDue bool
	// Checked is the check this decision recorded, for the AnalysisPassing condition; nil when none was.
	Checked *CheckResult
}

// CheckResult is the AnalysisPassing condition after a check.
type CheckResult struct {
	Passed  bool
	Message string
}

// Progressing says whether the plan's Progressing condition is True.
func (p *Plan) Progressing() bool { return p.Reason != ReasonIdle && p.Reason != ReasonRolledBack }

// Decide is the state machine of DESIGN.md, "The rollout": from the spec, the last status and the two Deployments, the
// Deployments' targets and the next status. The status is the rollout's memory between reconciles; the stable
// Deployment's template is the only copy of the stable version, so the stable's template is written only when it is
// created and when a canary is promoted.
//
// Rollback (DESIGN.md, "Rollback"): a step whose canary pods aren't ready within the progress deadline, or whose
// checks fail more than the failure limit allows, rolls the rollout back. The stable goes back to all replicas with
// the template it has, the canary keeps its pods until the stable's are ready, and the rollout holds (phase
// RolledBack) until the template changes.
func Decide(in Input) Plan {
	p := decide(in)
	if p.State.RolledBackRevision == "" {
		p.State.RolledBackRevision = in.Status.RolledBackRevision
	}
	return p
}

func decide(in Input) Plan {
	r := in.Spec.Replicas
	rev := in.Revision
	stable, canary := in.Stable, in.Canary
	last := in.Status

	heldBack := last.Phase == v1.PhaseRolledBack && rev == last.RolledBackRevision

	// No stable Deployment: create it from the template, at all replicas. On a first apply there is nothing to
	// compare the template with; if the stable was deleted, the version it ran is gone with it, so the template
	// becomes the stable version either way. A canary keeps its pods (the next decision takes it to 0 once the new
	// stable is ready), since they may be the only ones serving.
	//
	// Except after a rollback, while the template is still the revision rolled back: creating the stable from it would
	// run that revision at all replicas, with no canary and no analysis. The operator holds no other copy of a
	// version, so it creates nothing and says what to do.
	if stable == nil && heldBack {
		return Plan{
			Canary:  Target{Replicas: replicasOf(canary)},
			State:   last,
			Reason:  ReasonRolledBack,
			Message: fmt.Sprintf("the stable Deployment is gone, and the template is revision %s, which was rolled back: set the template to the version to run", rev),
		}
	}
	if stable == nil {
		return Plan{
			Stable:  Target{Replicas: r, FromTemplate: true},
			Canary:  Target{Replicas: replicasOf(canary)},
			State:   State{Phase: v1.PhaseHealthy, StableRevision: rev},
			Reason:  ReasonIdle,
			Message: fmt.Sprintf("creating the stable Deployment at revision %s", rev),
		}
	}

	inRollout := last.CanaryRevision == rev && (last.Phase == v1.PhaseProgressing || last.Phase == v1.PhasePromoting)

	// Promotion: the stable takes the template at all replicas, and the canary keeps its pods until the stable's
	// are ready, so capacity never drops. A stable marked as promoted to the revision under test means a promotion
	// too: the operator writes the Deployments before the status, so a status write that failed after the stable
	// took the template leaves the phase at Progressing. The mark, not a guess from the step, tells such a stable from
	// one recreated from the template after being deleted (the next branch ends that rollout).
	if inRollout && (last.Phase == v1.PhasePromoting || stable.PromotedRevision == rev) {
		// The steps may have changed since (they aren't part of the template): the index stays in range.
		step := ptr(stepIndex(last.CurrentStep, in.Spec))
		var events []Event
		if last.Phase != v1.PhasePromoting {
			// The status write that recorded the promotion's start was lost, and its Event with it.
			events = append(events, Event{Reason: EventPromoting, Message: fmt.Sprintf("the last step passed; promoting revision %s", rev)})
		}
		if stable.readyAt(rev, r) {
			return Plan{
				Stable:  Target{Replicas: r},
				Canary:  Target{Replicas: 0},
				State:   State{Phase: v1.PhaseHealthy, StableRevision: rev},
				Reason:  ReasonIdle,
				Message: fmt.Sprintf("revision %s promoted: the stable runs it", rev),
				Events:  append(events, Event{Reason: EventPromoted, Message: fmt.Sprintf("revision %s promoted: the stable runs it at %d pods", rev, r)}),
			}
		}
		return Plan{
			Stable:  Target{Replicas: r, FromTemplate: true, Promote: true},
			Canary:  Target{Replicas: replicasOf(canary)},
			State:   State{Phase: v1.PhasePromoting, StableRevision: rev, CanaryRevision: rev, CurrentStep: step},
			Reason:  ReasonPromoting,
			Message: fmt.Sprintf("promoting revision %s: waiting for the stable's %d pods to be ready", rev, r),
			Events:  events,
		}
	}

	// Nothing to roll out: the template is the stable's version. After a first apply or a promotion, or when the
	// template went back to the stable's version mid-rollout. A canary still running keeps its pods until the
	// stable is ready at all replicas.
	if rev == stable.Revision {
		keep := int32(0)
		if !stable.readyAt(rev, r) {
			keep = replicasOf(canary)
		}
		p := Plan{
			Stable:  Target{Replicas: r},
			Canary:  Target{Replicas: keep},
			State:   State{Phase: v1.PhaseHealthy, StableRevision: rev},
			Reason:  ReasonIdle,
			Message: fmt.Sprintf("no rollout in progress: the stable runs revision %s", rev),
		}
		// The template went back to the stable's version only if the rollout was of another one (a stable recreated
		// from the template mid-rollout runs the revision that was under test).
		if (last.Phase == v1.PhaseProgressing || last.Phase == v1.PhasePromoting) && last.CanaryRevision != rev {
			p.Events = []Event{{Reason: EventRolloutReverted, Message: fmt.Sprintf("the template went back to the stable's revision %s; revision %s's rollout ended", rev, last.CanaryRevision)}}
		}
		return p
	}

	// Rolled back: hold until the template changes. The stable runs its own version at all replicas, and the canary
	// keeps its pods until the stable's are all ready.
	if heldBack {
		return holdBack(in, last, "")
	}

	// A version other than the stable's: start a rollout at step 0, or restart one in progress for another version.
	var events []Event
	st := last
	if !inRollout {
		st = State{Phase: v1.PhaseProgressing, StableRevision: stable.Revision, CanaryRevision: rev, CurrentStep: ptr(int32(0))}
		msg := fmt.Sprintf("rolling out revision %s over the stable's %s in %d steps", rev, stable.Revision, len(in.Spec.Steps))
		if last.CanaryRevision != "" && (last.Phase == v1.PhaseProgressing || last.Phase == v1.PhasePromoting) {
			msg += fmt.Sprintf(" (replacing revision %s's rollout)", last.CanaryRevision)
		}
		events = append(events, Event{Reason: EventRolloutStarted, Message: msg})
	}
	st.StableRevision = stable.Revision

	// The step. Steps can change mid-rollout (they aren't part of the template), so the index is kept in range.
	i := stepIndex(st.CurrentStep, in.Spec)
	st.CurrentStep = ptr(i)
	c, s := Split(r, in.Spec.Steps[i].Weight)
	// A pause belongs to the pods it started with. If the canary no longer runs the template's revision (a status
	// write was lost while the template changed and changed back), or is set to another count than the step's (the
	// steps or replicas were edited), the step waits for its pods again, under the capacity rules, and its pause
	// starts over. One exception: a pause already over, with the canary set to *more* pods than the step's. That is
	// what a status write lost at a step advance leaves (weights strictly increase, so the next step's count is never
	// lower), and the step mustn't run again. A canary set to fewer pods usually means a raised weight or replicas
	// edited in during the pause (an edit can also make a running pause over at once), so the new share waits to be
	// seen. Not always: a lost advance followed by a replicas increase also leaves fewer pods than the step now needs,
	// and that step's pause then runs again (README, limitations; recording the step on the canary would tell them
	// apart).
	if st.StepStartedAt != nil {
		over := pauseOver(st.StepStartedAt, in.Spec.Steps[i], in.Now)
		if canary == nil || canary.Revision != rev || (canary.Replicas != c && !(over && canary.Replicas > c)) {
			st.StepStartedAt = nil
		}
	}
	if st.StepStartedAt == nil && canary.readyAt(rev, c) {
		st.StepStartedAt = &metav1.Time{Time: ceilSecond(in.Now)}
	}
	// The checks belong to the pause: they start over with it.
	if !sameTime(st.StepStartedAt, last.StepStartedAt) {
		st.Analysis = Analysis{}
	}
	if st.StepStartedAt == nil {
		return waitForCanary(in, st, i, events)
	}
	st.StepWaitingSince = nil
	end := st.StepStartedAt.Add(pauseOf(in.Spec.Steps[i]))
	holding := !pauseOver(st.StepStartedAt, in.Spec.Steps[i], in.Now)

	// The analysis: a check every interval from the pause's start, while the pause runs and after it until the step
	// has had one (a step with analysis passes only after a check, so a pause shorter than the interval lasts an
	// interval). A check that fails more often than the failure limit allows rolls the rollout back.
	var checked *CheckResult
	var nextCheck time.Time
	due := false
	if a := in.Spec.Analysis; a != nil {
		nextCheck = st.StepStartedAt.Add(intervalOf(a))
		if st.Analysis.LastCheckAt != nil {
			nextCheck = st.Analysis.LastCheckAt.Add(intervalOf(a))
		}
		if !in.Now.Before(nextCheck) {
			if in.Check == nil {
				due = true
			} else {
				st.Analysis.Checks++
				// Rounded down, to the second the API server stores: the next check is then due an interval after it,
				// never later. Rounded up, every check would be up to a second late, and a 1s interval would check
				// every 2s.
				st.Analysis.LastCheckAt = &metav1.Time{Time: in.Now.Truncate(time.Second)}
				nextCheck = st.Analysis.LastCheckAt.Add(intervalOf(a))
				msg := fmt.Sprintf("%s, check %d", stepName(in.Spec, i), st.Analysis.Checks)
				if in.Check.Passed() {
					checked = &CheckResult{Passed: true, Message: msg + " passed: " + in.Check.String()}
				} else {
					st.Analysis.FailedChecks++
					limit := failureLimitOf(a)
					msg += fmt.Sprintf(" failed (%d failed, %d allowed): %s", st.Analysis.FailedChecks, limit, in.Check.String())
					checked = &CheckResult{Message: msg}
					events = append(events, Event{Reason: EventAnalysisFailed, Message: msg, Warning: true})
					if st.Analysis.FailedChecks > limit {
						p := rollBack(in, st, fmt.Sprintf("%d of %s's checks failed, more than the %d allowed", st.Analysis.FailedChecks, stepName(in.Spec, i), limit), events)
						p.Checked = checked
						return p
					}
				}
			}
		}
		holding = holding || due || st.Analysis.Checks == 0
	}
	if holding {
		// The stable goes down to the step's count only while the step's canary pods are all ready: one lost
		// mid-pause doesn't restart the pause, but a replicas edit meanwhile mustn't take the stable below what the
		// ready pods need.
		stableWant := s
		if !canary.readyAt(rev, c) {
			stableWant = max(s, stable.Replicas)
		}
		// A step held past its pause for its first check can have a canary with more pods than the step's (a pause
		// over with more is kept, above), and the capacity rule of waitForCanary holds here too: the canary isn't
		// scaled down until the stable is settled at its count.
		canaryWant := c
		if canary.Replicas > c && !(stable.Settled && stable.Replicas == stableWant) {
			canaryWant = canary.Replicas
		}
		msg := fmt.Sprintf("%s: pods ready, paused until %s", stepName(in.Spec, i), end.UTC().Format(time.RFC3339))
		wake := end
		if a := in.Spec.Analysis; a != nil {
			msg += fmt.Sprintf("; checks: %d, %d failed, the next at %s", st.Analysis.Checks, st.Analysis.FailedChecks, nextCheck.UTC().Format(time.RFC3339))
			if !in.Now.Before(end) || nextCheck.Before(end) {
				wake = nextCheck
			}
		}
		return Plan{
			Canary:       Target{Replicas: canaryWant, FromTemplate: true},
			Stable:       Target{Replicas: stableWant},
			State:        st,
			Reason:       ReasonStepPaused,
			Message:      msg,
			RequeueAfter: wake.Sub(in.Now),
			Events:       events,
			AnalysisDue:  due,
			Checked:      checked,
		}
	}

	// The pause is over (and the step has had a check, if it has analysis): the next step, or after the last one,
	// promotion.
	if int(i) == len(in.Spec.Steps)-1 {
		st.Phase = v1.PhasePromoting
		st.StableRevision = rev
		st.StepStartedAt = nil // the step is over; currentStep stays at the last one
		st.Analysis = Analysis{}
		return Plan{
			Stable:  Target{Replicas: r, FromTemplate: true, Promote: true},
			Canary:  Target{Replicas: replicasOf(canary)},
			State:   st,
			Reason:  ReasonPromoting,
			Message: fmt.Sprintf("promoting revision %s: waiting for the stable's %d pods to be ready", rev, r),
			Events:  append(events, Event{Reason: EventPromoting, Message: fmt.Sprintf("the last step passed; promoting revision %s", rev)}),
			Checked: checked,
		}
	}
	st.CurrentStep = ptr(i + 1)
	st.StepStartedAt = nil
	st.Analysis = Analysis{}
	c2, s2 := Split(r, in.Spec.Steps[i+1].Weight)
	events = append(events, Event{Reason: EventStepAdvanced, Message: fmt.Sprintf("%s: %d canary and %d stable pods", stepName(in.Spec, i+1), c2, s2)})
	p := waitForCanary(in, st, i+1, events)
	p.Checked = checked
	return p
}

// waitForCanary is the plan while step i's canary pods aren't all ready: the canary is scaled up first, and the
// stable keeps the pods it has until the step's canary pods are ready, so capacity doesn't drop while the new pods
// start. A stable with fewer pods than the step's count (a rollout restarted from a later step) gets them back now.
//
// A canary with more pods than the step's (a restart, or a new rollout while the last one's canary still drains)
// isn't scaled down, nor its pods replaced, until the stable is settled at its count: scaling it down first would
// drop capacity, and replacing its pods at their old count would send the new version more than the step's share.
//
// The wait is bounded by the progress deadline, from when the step began waiting: past it, the rollout rolls back.
func waitForCanary(in Input, st State, i int32, events []Event) Plan {
	c, s := Split(in.Spec.Replicas, in.Spec.Steps[i].Weight)
	stable := Target{Replicas: max(s, in.Stable.Replicas)}
	canary := Target{Replicas: c, FromTemplate: true}
	msg := fmt.Sprintf("waiting for %d canary pods of revision %s to be ready", c, in.Revision)
	if in.Canary != nil && in.Canary.Replicas > c && !(in.Stable.Settled && in.Stable.Replicas == stable.Replicas) {
		canary = Target{Replicas: in.Canary.Replicas}
		msg = fmt.Sprintf("waiting for the stable's %d pods to be ready before the canary goes down to %d", stable.Replicas, c)
	}
	if st.StepWaitingSince == nil {
		st.StepWaitingSince = &metav1.Time{Time: ceilSecond(in.Now)}
	}
	deadline := st.StepWaitingSince.Add(progressDeadlineOf(in.Spec))
	if !in.Now.Before(deadline) {
		return rollBack(in, st, fmt.Sprintf("%s wasn't ready within the progress deadline of %v (%s)", stepName(in.Spec, i), progressDeadlineOf(in.Spec), msg), events)
	}
	return Plan{
		Canary:       canary,
		Stable:       stable,
		State:        st,
		Reason:       ReasonWaitingForCanary,
		Message:      fmt.Sprintf("%s: %s, until %s", stepName(in.Spec, i), msg, deadline.UTC().Format(time.RFC3339)),
		RequeueAfter: deadline.Sub(in.Now),
		Events:       events,
	}
}

// rollBack ends the rollout of the template's revision: the stable goes back to all replicas with the template it has
// (the old version), the canary keeps its pods until the stable's are ready, and the rollout holds (holdBack) until
// the template changes. why says what failed.
func rollBack(in Input, st State, why string, events []Event) Plan {
	st.Phase = v1.PhaseRolledBack
	st.RolledBackRevision = in.Revision
	st.CanaryRevision = ""
	st.StepStartedAt, st.StepWaitingSince = nil, nil
	p := holdBack(in, st, why)
	p.KeepMessage = false // this message says why; the hold keeps it
	p.Events = append(events, Event{
		Reason:  EventRolledBack,
		Message: fmt.Sprintf("revision %s rolled back: %s; the stable runs revision %s", in.Revision, why, in.Stable.Revision),
		Warning: true,
	})
	return p
}

// holdBack is the plan after a rollback, while the template is the revision rolled back: the stable at all replicas
// with its own template, and the canary kept until the stable's pods are all ready, then 0.
func holdBack(in Input, st State, why string) Plan {
	r := in.Spec.Replicas
	keep := int32(0)
	if !in.Stable.readyAt(in.Stable.Revision, r) {
		keep = replicasOf(in.Canary)
	}
	st.StableRevision = in.Stable.Revision
	if why == "" {
		why = "it failed"
		if st.CurrentStep != nil {
			why = fmt.Sprintf("it failed at step %d", *st.CurrentStep+1)
		}
	}
	msg := fmt.Sprintf("revision %s was rolled back (%s); the stable runs revision %s", st.RolledBackRevision, why, in.Stable.Revision)
	if keep > 0 {
		msg += fmt.Sprintf(", and the canary keeps its %d pods until the stable's %d are ready", keep, r)
	}
	return Plan{
		Stable:      Target{Replicas: r},
		Canary:      Target{Replicas: keep},
		State:       st,
		Reason:      ReasonRolledBack,
		Message:     msg + "; change the template to roll out another version",
		KeepMessage: true,
	}
}

// pauseOver says a step's pause that started at start has ended by now. A 0s pause is over at once (its start, rounded
// up to the second, may be a moment ahead of now).
func pauseOver(start *metav1.Time, s v1.Step, now time.Time) bool {
	pause := pauseOf(s)
	return pause == 0 || !now.Before(start.Add(pause))
}

// stepName is "step 2 of 3 (50%)" for index 1 of three steps.
func stepName(spec *v1.CanaryRolloutSpec, i int32) string {
	return fmt.Sprintf("step %d of %d (%d%%)", i+1, len(spec.Steps), spec.Steps[i].Weight)
}

func pauseOf(s v1.Step) time.Duration {
	if s.Pause == nil {
		return 0
	}
	return s.Pause.Duration
}

func replicasOf(d *Deployment) int32 {
	if d == nil {
		return 0
	}
	return d.Replicas
}

// stepIndex is the status's step index kept within the spec's steps (0 when there is none).
func stepIndex(p *int32, spec *v1.CanaryRolloutSpec) int32 {
	i := int32(0)
	if p != nil {
		i = *p
	}
	return min(max(i, 0), int32(len(spec.Steps)-1))
}

// ceilSecond rounds t up to a whole second. The API server stores status times to the second, so a pause start
// written with its fraction would be read back up to a second earlier, and the pause would end that much early;
// rounded up, it runs at least its full length.
func ceilSecond(t time.Time) time.Time {
	if s := t.Truncate(time.Second); s.Before(t) {
		return s.Add(time.Second)
	}
	return t
}

func ptr[T any](v T) *T { return &v }

// sameTime says a and b are both nil or the same instant.
func sameTime(a, b *metav1.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(b)
}

func progressDeadlineOf(spec *v1.CanaryRolloutSpec) time.Duration {
	if spec.ProgressDeadlineSeconds == nil {
		return DefaultProgressDeadline
	}
	return time.Duration(*spec.ProgressDeadlineSeconds) * time.Second
}

func intervalOf(a *v1.Analysis) time.Duration {
	if a.Interval == nil {
		return DefaultInterval
	}
	return a.Interval.Duration
}

func failureLimitOf(a *v1.Analysis) int32 {
	if a.FailureLimit == nil {
		return 0
	}
	return *a.FailureLimit
}
