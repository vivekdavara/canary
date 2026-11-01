package rollout

import (
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/vivekdavara/canary/api/v1alpha1"
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
	CurrentStep    *int32
	StepStartedAt  *metav1.Time
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
}

// Reasons of the Progressing condition and of Events.
const (
	ReasonIdle             = "Idle"             // no rollout in progress
	ReasonWaitingForCanary = "WaitingForCanary" // a step's canary pods aren't all ready yet
	ReasonStepPaused       = "StepPaused"       // a step's pods are ready and its pause is running
	ReasonPromoting        = "Promoting"        // the stable is taking the canary's version

	EventRolloutStarted  = "RolloutStarted"
	EventStepAdvanced    = "StepAdvanced"
	EventPromoting       = "Promoting"
	EventPromoted        = "Promoted"
	EventRolloutReverted = "RolloutReverted"
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
}

// Progressing says whether the plan's Progressing condition is True.
func (p *Plan) Progressing() bool { return p.Reason != ReasonIdle }

// Decide is the state machine of DESIGN.md, "The rollout": from the spec, the last status and the two Deployments, the
// Deployments' targets and the next status. The status is the rollout's memory between reconciles; the stable
// Deployment's template is the only copy of the stable version, so the stable's template is written only when it is
// created and when a canary is promoted.
func Decide(in Input) Plan {
	r := in.Spec.Replicas
	rev := in.Revision
	stable, canary := in.Stable, in.Canary
	last := in.Status

	// No stable Deployment: create it from the template, at all replicas. On a first apply there is nothing to
	// compare the template with; if the stable was deleted, the version it ran is gone with it, so the template
	// becomes the stable version either way. A canary keeps its pods (the next decision takes it to 0 once the new
	// stable is ready), since they may be the only ones serving.
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
			events = append(events, Event{EventPromoting, fmt.Sprintf("the last step passed; promoting revision %s", rev)})
		}
		if stable.readyAt(rev, r) {
			return Plan{
				Stable:  Target{Replicas: r},
				Canary:  Target{Replicas: 0},
				State:   State{Phase: v1.PhaseHealthy, StableRevision: rev},
				Reason:  ReasonIdle,
				Message: fmt.Sprintf("revision %s promoted: the stable runs it", rev),
				Events:  append(events, Event{EventPromoted, fmt.Sprintf("revision %s promoted: the stable runs it at %d pods", rev, r)}),
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
			p.Events = []Event{{EventRolloutReverted, fmt.Sprintf("the template went back to the stable's revision %s; revision %s's rollout ended", rev, last.CanaryRevision)}}
		}
		return p
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
		events = append(events, Event{EventRolloutStarted, msg})
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
	// a status write lost at a step advance (weights strictly increase, so the next step's count is never lower),
	// and the step mustn't run again. A canary set to fewer pods is never that: a raised weight or replicas edited
	// in during the pause (an edit can also make a running pause over at once), so the new share waits to be seen.
	if st.StepStartedAt != nil {
		over := pauseOver(st.StepStartedAt, in.Spec.Steps[i], in.Now)
		if canary == nil || canary.Revision != rev || (canary.Replicas != c && !(over && canary.Replicas > c)) {
			st.StepStartedAt = nil
		}
	}
	if st.StepStartedAt == nil && canary.readyAt(rev, c) {
		st.StepStartedAt = &metav1.Time{Time: ceilSecond(in.Now)}
	}
	if st.StepStartedAt == nil {
		return waitForCanary(in, st, i, events)
	}
	end := st.StepStartedAt.Add(pauseOf(in.Spec.Steps[i]))
	if !pauseOver(st.StepStartedAt, in.Spec.Steps[i], in.Now) {
		// The stable goes down to the step's count only while the step's canary pods are all ready: one lost
		// mid-pause doesn't restart the pause, but a replicas edit meanwhile mustn't take the stable below what the
		// ready pods need.
		stableWant := s
		if !canary.readyAt(rev, c) {
			stableWant = max(s, stable.Replicas)
		}
		return Plan{
			Canary:       Target{Replicas: c, FromTemplate: true},
			Stable:       Target{Replicas: stableWant},
			State:        st,
			Reason:       ReasonStepPaused,
			Message:      fmt.Sprintf("%s: pods ready, paused until %s", stepName(in.Spec, i), end.UTC().Format(time.RFC3339)),
			RequeueAfter: end.Sub(in.Now),
			Events:       events,
		}
	}

	// The pause is over: the next step, or after the last one, promotion.
	if int(i) == len(in.Spec.Steps)-1 {
		st.Phase = v1.PhasePromoting
		st.StableRevision = rev
		st.StepStartedAt = nil // the step is over; currentStep stays at the last one
		return Plan{
			Stable:  Target{Replicas: r, FromTemplate: true, Promote: true},
			Canary:  Target{Replicas: replicasOf(canary)},
			State:   st,
			Reason:  ReasonPromoting,
			Message: fmt.Sprintf("promoting revision %s: waiting for the stable's %d pods to be ready", rev, r),
			Events:  append(events, Event{EventPromoting, fmt.Sprintf("the last step passed; promoting revision %s", rev)}),
		}
	}
	st.CurrentStep = ptr(i + 1)
	st.StepStartedAt = nil
	c2, s2 := Split(r, in.Spec.Steps[i+1].Weight)
	events = append(events, Event{EventStepAdvanced, fmt.Sprintf("%s: %d canary and %d stable pods", stepName(in.Spec, i+1), c2, s2)})
	return waitForCanary(in, st, i+1, events)
}

// waitForCanary is the plan while step i's canary pods aren't all ready: the canary is scaled up first, and the
// stable keeps the pods it has until the step's canary pods are ready, so capacity doesn't drop while the new pods
// start. A stable with fewer pods than the step's count (a rollout restarted from a later step) gets them back now.
//
// A canary with more pods than the step's (a restart, or a new rollout while the last one's canary still drains)
// isn't scaled down, nor its pods replaced, until the stable is settled at its count: scaling it down first would
// drop capacity, and replacing its pods at their old count would send the new version more than the step's share.
func waitForCanary(in Input, st State, i int32, events []Event) Plan {
	c, s := Split(in.Spec.Replicas, in.Spec.Steps[i].Weight)
	stable := Target{Replicas: max(s, in.Stable.Replicas)}
	canary := Target{Replicas: c, FromTemplate: true}
	msg := fmt.Sprintf("%s: waiting for %d canary pods of revision %s to be ready", stepName(in.Spec, i), c, in.Revision)
	if in.Canary != nil && in.Canary.Replicas > c && !(in.Stable.Settled && in.Stable.Replicas == stable.Replicas) {
		canary = Target{Replicas: in.Canary.Replicas}
		msg = fmt.Sprintf("%s: waiting for the stable's %d pods to be ready before the canary goes down to %d", stepName(in.Spec, i), stable.Replicas, c)
	}
	return Plan{
		Canary:  canary,
		Stable:  stable,
		State:   st,
		Reason:  ReasonWaitingForCanary,
		Message: msg,
		Events:  events,
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
