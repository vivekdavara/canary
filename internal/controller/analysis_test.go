package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	eventsv1 "k8s.io/api/events/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	rolloutsv1alpha1 "github.com/vivekdavara/canary/api/v1alpha1"
	"github.com/vivekdavara/canary/internal/analysis"
	"github.com/vivekdavara/canary/internal/rollout"
	"github.com/vivekdavara/canary/internal/testenv"
)

const errorRateQuery = `sum(rate(http_requests_total{app="web",track="canary",code=~"5.."}[1m])) / sum(rate(http_requests_total{app="web",track="canary"}[1m]))`

// fakePrometheus is a Prometheus HTTP API serving one value for every query, and the operator's real client for it,
// so these tests go through the HTTP client too. It records the queries it was sent.
type fakePrometheus struct {
	mu      sync.Mutex
	value   string
	queries []string
}

func newFakePrometheus(t *testing.T, value string) (*fakePrometheus, analysis.Querier) {
	t.Helper()
	f := &fakePrometheus{value: value}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" || r.ParseForm() != nil {
			http.NotFound(w, r)
			return
		}
		f.mu.Lock()
		f.queries = append(f.queries, r.PostForm.Get("query"))
		v := f.value
		f.mu.Unlock()
		fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[%d,%q]}]}}`, time.Now().Unix(), v)
	}))
	t.Cleanup(srv.Close)
	q, err := analysis.NewPrometheus(srv.URL, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return f, q
}

func (f *fakePrometheus) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.queries)
}

// analysedRollout is a rollout of 2 pods in one step of 50% with a pause, checked every second.
func analysedRollout(ns string, pause time.Duration, failureLimit int32) *rolloutsv1alpha1.CanaryRollout {
	ro := newRollout(ns, "example.com/web:1")
	ro.Spec.Replicas = 2
	ro.Spec.Steps = []rolloutsv1alpha1.Step{{Weight: 50, Pause: &metav1.Duration{Duration: pause}}}
	maxRate := 0.01
	ro.Spec.Analysis = &rolloutsv1alpha1.Analysis{
		Interval:     &metav1.Duration{Duration: time.Second},
		FailureLimit: &failureLimit,
		Metrics:      []rolloutsv1alpha1.Metric{{Name: "error-rate", Query: errorRateQuery, Max: &maxRate}},
	}
	return ro
}

// startCanary creates ro, makes its stable ready, changes the image and makes the canary's pod ready: the step's
// pause starts. It returns the rollout's key and the stable's and canary's revisions.
func startCanary(t *testing.T, c client.Client, ro *rolloutsv1alpha1.CanaryRollout) (key client.ObjectKey, stableRev, canaryRev string) {
	t.Helper()
	ns := ro.Namespace
	if err := c.Create(context.Background(), ro); err != nil {
		t.Fatal(err)
	}
	key = client.ObjectKeyFromObject(ro)
	stableRev = eventually(t, c, key, func(r *rolloutsv1alpha1.CanaryRollout) bool { return r.Status.StableRevision != "" }).Status.StableRevision
	markReady(t, c, ns, "web-stable")
	edit(t, c, key, func(r *rolloutsv1alpha1.CanaryRollout) {
		r.Spec.Template.Spec.Containers[0].Image = "example.com/web:2"
	})
	canaryRev = eventually(t, c, key, func(r *rolloutsv1alpha1.CanaryRollout) bool { return r.Status.CanaryRevision != "" }).Status.CanaryRevision
	deploymentWhen(t, c, ns, "web-canary", at(canaryRev, 1))
	markReady(t, c, ns, "web-canary")
	return key, stableRev, canaryRev
}

func condition(r *rolloutsv1alpha1.CanaryRollout, typ string) *metav1.Condition {
	return meta.FindStatusCondition(r.Status.Conditions, typ)
}

// Checks every second through a 3s pause, each passing, against a Prometheus HTTP API: the status counts them, the
// AnalysisPassing condition shows the last, and the step passes on to the promotion.
func TestAPassingAnalysisPromotes(t *testing.T) {
	prom, q := newFakePrometheus(t, "0.002")
	runOperatorWith(t, operatorOptions{querier: q})
	c := testenv.Get(t).Client
	ns := testenv.Get(t).Namespace(t)
	key, _, canaryRev := startCanary(t, c, analysedRollout(ns, 3*time.Second, 0))

	got := eventually(t, c, key, func(r *rolloutsv1alpha1.CanaryRollout) bool {
		return r.Status.Analysis != nil && r.Status.Analysis.Checks >= 1
	})
	cond := condition(got, ConditionAnalysisPassing)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != rollout.ReasonChecksPassed ||
		!strings.Contains(cond.Message, "error-rate 0.002 (max 0.01)") || got.Status.Analysis.FailedChecks != 0 || got.Status.Analysis.LastCheckAt == nil {
		t.Fatalf("AnalysisPassing %+v, analysis %+v", cond, got.Status.Analysis)
	}
	if p := condition(got, ConditionProgressing); p == nil || p.Reason != rollout.ReasonStepPaused || !strings.Contains(p.Message, "checks: ") {
		t.Fatalf("Progressing %+v; want StepPaused with the checks", p)
	}

	got = eventually(t, c, key, phase(rolloutsv1alpha1.PhasePromoting))
	deploymentWhen(t, c, ns, "web-stable", at(canaryRev, 2))
	if n := len(prom.sent()); n < 3 || n > 5 {
		t.Errorf("%d queries for a 3s pause checked every second; want 3 (a few more if reconciles were slow)", n)
	}
	if prom.sent()[0] != errorRateQuery {
		t.Errorf("query %q; want the metric's, as written", prom.sent()[0])
	}
	if got.Status.Analysis != nil {
		t.Errorf("analysis %+v during the promotion; want it cleared with the step", got.Status.Analysis)
	}
}

// A check above the threshold, with no failure allowed, rolls back at the first check: Warning Events for the check
// and the rollback, the conditions, the stable back to both pods with its own template, and the canary at 0 once the
// stable's pods are ready. Then the rollout holds: more checks aren't run.
func TestAFailingAnalysisRollsBack(t *testing.T) {
	prom, q := newFakePrometheus(t, "0.2")
	runOperatorWith(t, operatorOptions{querier: q})
	c := testenv.Get(t).Client
	ns := testenv.Get(t).Namespace(t)
	key, stableRev, canaryRev := startCanary(t, c, analysedRollout(ns, time.Hour, 0))
	deploymentWhen(t, c, ns, "web-stable", at(stableRev, 1)) // the pause began: the stable went down to 1

	got := eventually(t, c, key, phase(rolloutsv1alpha1.PhaseRolledBack))
	if got.Status.RolledBackRevision != canaryRev || got.Status.CanaryRevision != "" || got.Status.StableRevision != stableRev {
		t.Fatalf("status %+v", got.Status)
	}
	if a := got.Status.Analysis; a == nil || a.Checks != 1 || a.FailedChecks != 1 {
		t.Fatalf("analysis %+v; want the failed step's one failed check", a)
	}
	ap := condition(got, ConditionAnalysisPassing)
	if ap == nil || ap.Status != metav1.ConditionFalse || ap.Reason != rollout.ReasonCheckFailed || !strings.Contains(ap.Message, "error-rate 0.2 (above max 0.01)") {
		t.Fatalf("AnalysisPassing %+v", ap)
	}
	if p := condition(got, ConditionProgressing); p == nil || p.Status != metav1.ConditionFalse || p.Reason != rollout.ReasonRolledBack {
		t.Fatalf("Progressing %+v; want False, RolledBack", p)
	}
	for _, reason := range []string{rollout.EventAnalysisFailed, rollout.EventRolledBack} {
		if typ := eventType(t, c, ns, "web", reason); typ != "Warning" {
			t.Errorf("%s Event of type %q; want Warning", reason, typ)
		}
	}

	// The stable back to 2 with its own template; the canary keeps its pod until those are ready.
	deploymentWhen(t, c, ns, "web-stable", at(stableRev, 2))
	deploymentWhen(t, c, ns, "web-canary", at(canaryRev, 1))
	markReady(t, c, ns, "web-stable")
	deploymentWhen(t, c, ns, "web-canary", at(canaryRev, 0))

	n := len(prom.sent())
	time.Sleep(2 * time.Second)
	if m := len(prom.sent()); m != n || n != 1 {
		t.Errorf("%d queries, then %d after 2s of the hold; want 1 and no more", n, m)
	}
}

// A canary whose pods are never ready rolls back at the progress deadline, on a requeue (nothing else happens to
// wake the operator).
func TestTheProgressDeadlineRollsBack(t *testing.T) {
	c := startOperator(t)
	ns := testenv.Get(t).Namespace(t)
	ro := newRollout(ns, "example.com/web:1")
	ro.Spec.ProgressDeadlineSeconds = ptrTo(int32(2))
	if err := c.Create(context.Background(), ro); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKeyFromObject(ro)
	stableRev := eventually(t, c, key, func(r *rolloutsv1alpha1.CanaryRollout) bool { return r.Status.StableRevision != "" }).Status.StableRevision
	markReady(t, c, ns, "web-stable")
	edit(t, c, key, func(r *rolloutsv1alpha1.CanaryRollout) {
		r.Spec.Template.Spec.Containers[0].Image = "example.com/web:2"
	})
	waiting := eventually(t, c, key, func(r *rolloutsv1alpha1.CanaryRollout) bool { return r.Status.StepWaitingSince != nil })
	since := waiting.Status.StepWaitingSince.Time

	got := eventually(t, c, key, phase(rolloutsv1alpha1.PhaseRolledBack))
	if now := time.Now(); now.Before(since.Add(2 * time.Second)) {
		t.Errorf("rolled back at %v, before the deadline %v", now, since.Add(2*time.Second))
	}
	if p := condition(got, ConditionProgressing); p == nil || !strings.Contains(p.Message, "progress deadline of 2s") {
		t.Errorf("Progressing %+v; want the deadline in the message", p)
	}
	if got.Status.StepWaitingSince != nil {
		t.Errorf("stepWaitingSince %v after the rollback; want cleared", got.Status.StepWaitingSince)
	}
	// The stable never went down, so the canary goes to 0 at once.
	deploymentWhen(t, c, ns, "web-canary", func(d *appsv1.Deployment) bool { return d != nil && *d.Spec.Replicas == 0 })
	deploymentWhen(t, c, ns, "web-stable", at(stableRev, 4))
	if typ := eventType(t, c, ns, "web", rollout.EventRolledBack); typ != "Warning" {
		t.Errorf("RolledBack Event of type %q; want Warning", typ)
	}
}

// Without --prometheus-url, a rollout with analysis can't be measured, so its first check fails and it rolls back;
// the condition says why.
func TestWithoutPrometheusAnAnalysisFailsClosed(t *testing.T) {
	c := startOperator(t)
	ns := testenv.Get(t).Namespace(t)
	key, _, _ := startCanary(t, c, analysedRollout(ns, time.Hour, 0))
	got := eventually(t, c, key, phase(rolloutsv1alpha1.PhaseRolledBack))
	if ap := condition(got, ConditionAnalysisPassing); ap == nil || !strings.Contains(ap.Message, "--prometheus-url") {
		t.Fatalf("AnalysisPassing %+v; want the missing flag named", ap)
	}
}

// A Prometheus that doesn't answer fails the check at the operator's analysis timeout, not later.
func TestASlowPrometheusFailsTheCheckAtTheTimeout(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(block) })
	q, err := analysis.NewPrometheus(srv.URL, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	runOperatorWith(t, operatorOptions{querier: q, analysisTimeout: 200 * time.Millisecond})
	c := testenv.Get(t).Client
	ns := testenv.Get(t).Namespace(t)
	key, _, _ := startCanary(t, c, analysedRollout(ns, time.Hour, 0))
	got := eventually(t, c, key, phase(rolloutsv1alpha1.PhaseRolledBack))
	if ap := condition(got, ConditionAnalysisPassing); ap == nil || !strings.Contains(ap.Message, "deadline exceeded") {
		t.Fatalf("AnalysisPassing %+v; want the query's deadline", ap)
	}
}

func ptrTo[T any](v T) *T { return &v }

// A rollout that fails its analysis, counted at the operator's own client: the numbers the README quotes. Three
// failed checks a second apart (the limit allows two), the rollback, the drain; then the hold writes nothing and
// queries nothing.
func TestARolledBackRolloutsWrites(t *testing.T) {
	prom, q := newFakePrometheus(t, "0.2")
	var writes writeCounter
	runOperatorWith(t, operatorOptions{querier: q, funcs: writes.funcs()})
	c := testenv.Get(t).Client
	ns := testenv.Get(t).Namespace(t)
	key, stableRev, canaryRev := startCanary(t, c, analysedRollout(ns, time.Hour, 2))
	deploymentWhen(t, c, ns, "web-stable", at(stableRev, 1))
	markReady(t, c, ns, "web-stable")
	got := eventually(t, c, key, phase(rolloutsv1alpha1.PhaseRolledBack))
	if a := got.Status.Analysis; a == nil || a.Checks != 3 || a.FailedChecks != 3 {
		t.Fatalf("analysis %+v; want 3 checks, all failed", a)
	}
	deploymentWhen(t, c, ns, "web-stable", at(stableRev, 2))
	markReady(t, c, ns, "web-stable")
	deploymentWhen(t, c, ns, "web-canary", at(canaryRev, 0))
	markReady(t, c, ns, "web-canary")
	eventually(t, c, key, func(r *rolloutsv1alpha1.CanaryRollout) bool {
		return r.Status.CanaryReplicas == 0 && r.Status.StableReplicas == 2
	})

	queries := len(prom.sent())
	counts := writes.quiet(t, 2*time.Second)
	if n := len(prom.sent()); n != queries || n != 3 {
		t.Errorf("%d queries, %d after two quiet seconds; want 3 and no more", queries, n)
	}
	t.Logf("a rolled-back rollout (first apply, one step, 3 failed checks, rollback, drain): %d creates, %d Deployment updates, %d status patches, %d queries",
		counts[0], counts[1], counts[2], queries)
	if counts[0] != 2 || counts[1] > 6 || counts[2] > 20 {
		t.Errorf("(creates, updates, status patches) = %v; want 2, at most 6, at most 20", counts)
	}
}

// cancellingQuerier stands for an operator stopping mid-check: the query cancels the reconcile's context and fails
// with its error, as every query would at a shutdown.
type cancellingQuerier struct{ cancel context.CancelFunc }

func (q cancellingQuerier) Query(ctx context.Context, _ string) (analysis.Result, error) {
	q.cancel()
	<-ctx.Done()
	return analysis.Result{}, ctx.Err()
}

// A check cut short because the operator is stopping is no verdict: the reconcile returns the context's error and
// writes nothing, rather than counting a failure (with no failure allowed, that would have rolled back).
func TestACheckCutShortByAShutdownIsNoVerdict(t *testing.T) {
	env := testenv.Get(t)
	var writes writeCounter
	r := direct(t, *writes.funcs())
	now := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	r.Now = func() time.Time { return now }
	ns := env.Namespace(t)
	ro := analysedRollout(ns, time.Hour, 0)
	if err := env.Client.Create(context.Background(), ro); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, ro) // the stable
	markReady(t, env.Client, ns, "web-stable")
	edit(t, env.Client, client.ObjectKeyFromObject(ro), func(r *rolloutsv1alpha1.CanaryRollout) {
		r.Spec.Template.Spec.Containers[0].Image = "example.com/web:2"
	})
	reconcileOnce(t, r, ro) // the canary
	markReady(t, env.Client, ns, "web-canary")
	reconcileOnce(t, r, ro) // the pause starts
	var before rolloutsv1alpha1.CanaryRollout
	if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(ro), &before); err != nil || before.Status.StepStartedAt == nil {
		t.Fatalf("status %+v, %v; want a pause running", before.Status, err)
	}

	now = now.Add(time.Minute) // a check is due
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Querier = cancellingQuerier{cancel}
	counts := writes.load()
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ro)}); err == nil {
		t.Fatal("Reconcile = nil; want the context's error")
	}
	var after rolloutsv1alpha1.CanaryRollout
	if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(ro), &after); err != nil {
		t.Fatal(err)
	}
	if writes.load() != counts || after.Status.Phase != rolloutsv1alpha1.PhaseProgressing || after.Status.Analysis != nil {
		t.Fatalf("writes %v -> %v, status %+v; want nothing written", counts, writes.load(), after.Status)
	}
}

// An AnalysisFailed note with ten long metric errors is over the 1 KiB the API allows an Event's note: it is cut, so
// the Event is recorded (uncut, the API server refuses it and the Event is lost), with the action Analyze; the
// AnalysisPassing message is cut to the same length.
func TestALongCheckReportIsCut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `{"status":"error","errorType":"bad_data","error":%q}`, strings.Repeat("unexpected character in the query ", 12))
	}))
	t.Cleanup(srv.Close)
	q, err := analysis.NewPrometheus(srv.URL, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	runOperatorWith(t, operatorOptions{querier: q})
	c := testenv.Get(t).Client
	ns := testenv.Get(t).Namespace(t)
	ro := analysedRollout(ns, time.Hour, 0)
	maxRate := 0.01
	ro.Spec.Analysis.Metrics = nil
	for i := range 10 {
		ro.Spec.Analysis.Metrics = append(ro.Spec.Analysis.Metrics, rolloutsv1alpha1.Metric{
			Name: fmt.Sprintf("metric-%d-%s", i, strings.Repeat("x", 50)), Query: "up", Max: &maxRate,
		})
	}
	key, _, _ := startCanary(t, c, ro)
	got := eventually(t, c, key, phase(rolloutsv1alpha1.PhaseRolledBack))
	if ap := condition(got, ConditionAnalysisPassing); ap == nil || len(ap.Message) > maxMessage || len(ap.Message) < maxMessage-300 {
		t.Fatalf("AnalysisPassing %+v; want the message cut to at most %d bytes", ap, maxMessage)
	}
	var list eventsv1.EventList
	for range poll(t) {
		if err := c.List(context.Background(), &list, client.InNamespace(ns)); err != nil {
			t.Fatal(err)
		}
		for _, e := range list.Items {
			if e.Reason == rollout.EventAnalysisFailed {
				if len(e.Note) > maxMessage || !strings.Contains(e.Note, "the query failed: Prometheus: bad_data") || e.Action != "Analyze" || e.Type != "Warning" {
					t.Fatalf("AnalysisFailed: action %q, type %q, note of %d bytes: %q", e.Action, e.Type, len(e.Note), e.Note)
				}
				return
			}
		}
	}
	t.Fatalf("no AnalysisFailed Event: %d Events", len(list.Items))
}
