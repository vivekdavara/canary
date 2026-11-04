//go:build e2e

// Package e2e runs CanaryRollouts on a real cluster (kind, in CI; make e2e): a real Deployment controller starting
// real pods of test/e2e/demoapp, a real Prometheus scraping them (test/e2e/config), and the operator from its image,
// installed by its Helm chart (charts/canary). DESIGN.md, "End to end", says what each test shows.
//
// It needs the cluster set up by make e2e-install (the operator and Prometheus running, the images loaded) and
// Prometheus's API on CANARY_E2E_PROMETHEUS (http://127.0.0.1:9090, where test/e2e/kind.yaml publishes it).
package e2e

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/vivekdavara/canary/api/v1alpha1"
	"github.com/vivekdavara/canary/internal/analysis"
	"github.com/vivekdavara/canary/internal/controller"
	"github.com/vivekdavara/canary/internal/rollout"
)

var (
	k8s       client.Client
	prom      *analysis.Prometheus
	promURL   = envOr("CANARY_E2E_PROMETHEUS", "http://127.0.0.1:9090")
	demoImage = envOr("CANARY_E2E_DEMO_IMAGE", "canary-demoapp:dev")
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func TestMain(m *testing.M) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		panic(err)
	}
	if err := v1.AddToScheme(scheme); err != nil {
		panic(err)
	}
	cfg, err := ctrl.GetConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e: no cluster (make e2e-cluster, or KUBECONFIG):", err)
		os.Exit(1)
	}
	if k8s, err = client.New(cfg, client.Options{Scheme: scheme}); err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		os.Exit(1)
	}
	if prom, err = analysis.NewPrometheus(promURL, &http.Client{Timeout: 5 * time.Second}); err != nil {
		fmt.Fprintln(os.Stderr, "e2e: CANARY_E2E_PROMETHEUS:", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// The rollouts here run four pods (two for the deadline), and analysis checks every 5 seconds whether the canary's
// pods fail more than 5% of their requests over the last 20 seconds. Two failed checks a step are tolerated: the
// first may come before Prometheus has scraped a new pod twice, which a rate needs.
const (
	interval     = 5 * time.Second
	failureLimit = 2
	maxErrorRate = 0.05
	loadRPS      = "40"
)

// A version of the demo app: its VERSION, the share of requests it fails, and whether it ever becomes ready.
type version struct {
	name       string
	errorRatio float64
	ready      bool
}

var (
	v1Good = version{"v1", 0, true}
	v2Good = version{"v2", 0, true}
	v2Bad  = version{"v2", 0.5, true}
	v2Hung = version{"v2", 0, false}
)

// A test's rollout, in a namespace of its own, with its Service and a load generator sending it requests.
type world struct {
	t     *testing.T
	ns    string
	name  string
	start time.Time

	mu       sync.Mutex
	minReady int32
}

// newWorld creates the namespace, then first (objects of the test's own, such as a ResourceQuota, put in it), then
// the Service, the rollout (changed by edit, if given) and the load generator.
func newWorld(t *testing.T, name string, replicas int32, steps []v1.Step, deadline int32, edit func(*v1.CanaryRollout), first ...client.Object) *world {
	t.Helper()
	ctx := context.Background()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-" + name + "-"}}
	if err := k8s.Create(ctx, ns); err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, ns: ns.Name, name: name, start: time.Now(), minReady: -1}
	t.Cleanup(func() {
		// A failed test's namespace stays, for CI's diagnostics step.
		if !t.Failed() {
			_ = k8s.Delete(context.Background(), ns)
		}
	})
	t.Logf("namespace %s", w.ns)

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: w.ns, Name: name},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": name},
			Ports:    []corev1.ServicePort{{Port: 80, TargetPort: intstr.FromString("metrics")}},
		},
	}
	ro := &v1.CanaryRollout{
		ObjectMeta: metav1.ObjectMeta{Namespace: w.ns, Name: name},
		Spec: v1.CanaryRolloutSpec{
			Replicas:                replicas,
			Template:                w.template(v1Good),
			Steps:                   steps,
			ProgressDeadlineSeconds: &deadline,
			Analysis: &v1.Analysis{
				Interval:     &metav1.Duration{Duration: interval},
				FailureLimit: ptr(int32(failureLimit)),
				Metrics: []v1.Metric{{
					Name:  "error-rate",
					Query: w.errorRateQuery(),
					Max:   ptr(maxErrorRate),
				}},
			},
		},
	}
	one := int32(1)
	load := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: w.ns, Name: name + "-load"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &one,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name + "-load"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name + "-load"}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:            "load",
					Image:           demoImage,
					ImagePullPolicy: corev1.PullIfNotPresent,
					Args:            []string{"load", "--url", "http://" + name + "/", "--rps", loadRPS},
					Resources:       small(),
				}}},
			},
		},
	}
	if edit != nil {
		edit(ro)
	}
	for _, obj := range first {
		obj.SetNamespace(w.ns)
	}
	for _, obj := range append(first, svc, ro, load) {
		if err := k8s.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	return w
}

func (w *world) template(v version) corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": w.name}},
		Spec: corev1.PodSpec{
			TerminationGracePeriodSeconds: ptr(int64(5)),
			Containers: []corev1.Container{{
				Name:            "app",
				Image:           demoImage,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Args:            []string{"serve"},
				Env: []corev1.EnvVar{
					{Name: "VERSION", Value: v.name},
					{Name: "ERROR_RATIO", Value: strconv.FormatFloat(v.errorRatio, 'g', -1, 64)},
					{Name: "READY", Value: strconv.FormatBool(v.ready)},
				},
				Ports: []corev1.ContainerPort{{Name: "metrics", ContainerPort: 8080}},
				ReadinessProbe: &corev1.Probe{
					ProbeHandler:  corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/readyz", Port: intstr.FromString("metrics")}},
					PeriodSeconds: 1,
				},
				Resources: small(),
			}},
		},
	}
}

func small() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{Requests: corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("10m"),
		corev1.ResourceMemory: resource.MustParse("16Mi"),
	}}
}

// errorRateQuery is the share of the canary's requests that failed over the last 20 seconds, as the sample's
// error-rate metric (config/samples/web.yaml) but over the demo app's counter.
func (w *world) errorRateQuery() string {
	sel := fmt.Sprintf(`namespace=%q,rollout=%q,track="canary"`, w.ns, w.name)
	return fmt.Sprintf(`sum(rate(demo_requests_total{%s,code="500"}[20s])) / sum(rate(demo_requests_total{%s}[20s]))`, sel, sel)
}

// canaryShare is the canary's share of all the rollout's requests over the last 10 seconds, measured from the
// pods' own counters: how well the replica ratio splits real traffic.
func (w *world) canaryShare() (float64, error) {
	sel := fmt.Sprintf(`namespace=%q,rollout=%q`, w.ns, w.name)
	q := fmt.Sprintf(`sum(rate(demo_requests_total{%s,track="canary"}[10s])) / sum(rate(demo_requests_total{%s}[10s]))`, sel, sel)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := prom.Query(ctx, q)
	if err != nil {
		return 0, err
	}
	if len(r.Values) != 1 {
		return 0, fmt.Errorf("%s: %d values", q, len(r.Values))
	}
	return r.Values[0], nil
}

func (w *world) get() *v1.CanaryRollout {
	w.t.Helper()
	var ro v1.CanaryRollout
	if err := k8s.Get(context.Background(), client.ObjectKey{Namespace: w.ns, Name: w.name}, &ro); err != nil {
		w.t.Fatal(err)
	}
	return &ro
}

// setVersion changes the rollout's template and returns the new revision, once the operator has seen it.
func (w *world) setVersion(v version) string {
	w.t.Helper()
	var gen int64
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		ro := w.get()
		ro.Spec.Template = w.template(v)
		if err := k8s.Update(context.Background(), ro); err != nil {
			return err
		}
		gen = ro.Generation
		return nil
	})
	if err != nil {
		w.t.Fatal(err)
	}
	ro := w.waitFor(30*time.Second, "the operator to see the new template", func(ro *v1.CanaryRollout) bool {
		return ro.Status.ObservedGeneration >= gen
	})
	return ro.Status.UpdateRevision
}

// waitFor polls the rollout every 250 ms until cond holds, logging each change of its status, and fails the test
// after timeout.
func (w *world) waitFor(timeout time.Duration, what string, cond func(*v1.CanaryRollout) bool) *v1.CanaryRollout {
	w.t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		ro := w.get()
		if line := describe(ro); line != last {
			w.t.Logf("+%5.1fs %s", time.Since(w.start).Seconds(), line)
			last = line
		}
		if cond(ro) {
			return ro
		}
		if time.Now().After(deadline) {
			w.t.Fatalf("waited %v for %s", timeout, what)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func describe(ro *v1.CanaryRollout) string {
	s := ro.Status
	step := "-"
	if s.CurrentStep != nil {
		step = strconv.Itoa(int(*s.CurrentStep))
	}
	line := fmt.Sprintf("%s step %s stable %d canary %d (%d%%)", s.Phase, step, s.StableReplicas, s.CanaryReplicas, s.CanaryWeight)
	if s.Analysis != nil {
		line += fmt.Sprintf(" checks %d failed %d", s.Analysis.Checks, s.Analysis.FailedChecks)
	}
	if c := meta.FindStatusCondition(s.Conditions, controller.ConditionProgressing); c != nil {
		line += " | " + c.Reason
	}
	if c := meta.FindStatusCondition(s.Conditions, controller.ConditionAnalysisPassing); c != nil {
		line += " | " + c.Message
	}
	return line
}

func (w *world) deployment(track string) *appsv1.Deployment {
	w.t.Helper()
	var d appsv1.Deployment
	err := k8s.Get(context.Background(), client.ObjectKey{Namespace: w.ns, Name: w.name + "-" + track}, &d)
	if err != nil {
		return nil
	}
	return &d
}

// watchCapacity records, every 100 ms until the test ends, the fewest ready pods of the two Deployments together:
// the capacity the rollout kept (DESIGN.md, "The state machine": no decision takes the ready pods below replicas).
func (w *world) watchCapacity() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	w.t.Cleanup(func() { cancel(); <-done })
	go func() {
		defer close(done)
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			var ready int32
			for _, track := range []string{"stable", "canary"} {
				var d appsv1.Deployment
				if err := k8s.Get(ctx, client.ObjectKey{Namespace: w.ns, Name: w.name + "-" + track}, &d); err == nil {
					ready += d.Status.ReadyReplicas
				}
			}
			if ctx.Err() != nil {
				return
			}
			w.mu.Lock()
			if w.minReady < 0 || ready < w.minReady {
				w.minReady = ready
			}
			w.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
}

func (w *world) fewestReady() int32 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.minReady
}

// waitStable waits for the rollout to be Healthy on its first version, with all its pods ready.
func (w *world) waitStable(replicas int32) string {
	w.t.Helper()
	ro := w.waitFor(3*time.Minute, "the first version to be stable", func(ro *v1.CanaryRollout) bool {
		available := meta.IsStatusConditionTrue(ro.Status.Conditions, controller.ConditionAvailable)
		return ro.Status.Phase == v1.PhaseHealthy && ro.Status.StableRevision != "" && available &&
			ro.Status.StableReplicas == replicas
	})
	return ro.Status.StableRevision
}

// waitSettled waits until only the pods of revision rev are left of the rollout, replicas of them, all ready, and
// the canary Deployment is at 0.
func (w *world) waitSettled(rev string, replicas int32) {
	w.t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		var pods corev1.PodList
		err := k8s.List(context.Background(), &pods, client.InNamespace(w.ns), client.MatchingLabels{controller.LabelRollout: w.name})
		if err != nil {
			w.t.Fatal(err)
		}
		other, ready := 0, int32(0)
		for _, p := range pods.Items {
			if p.Labels[controller.LabelRevision] != rev {
				other++
			} else if p.DeletionTimestamp == nil && podReady(&p) {
				ready++
			}
		}
		canary := w.deployment("canary")
		canaryZero := canary == nil || (canary.Spec.Replicas != nil && *canary.Spec.Replicas == 0)
		if other == 0 && ready == replicas && canaryZero {
			return
		}
		if time.Now().After(deadline) {
			w.t.Fatalf("pods didn't settle on %s: %d of other revisions, %d ready of %d, canary at 0: %t", rev, other, ready, replicas, canaryZero)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// events returns the rollout's Events by reason, and their notes.
func (w *world) events() (map[string]int, []string) {
	w.t.Helper()
	var list eventsv1.EventList
	if err := k8s.List(context.Background(), &list, client.InNamespace(w.ns)); err != nil {
		w.t.Fatal(err)
	}
	reasons := map[string]int{}
	var notes []string
	for _, e := range list.Items {
		if e.Regarding.Kind != "CanaryRollout" || e.Regarding.Name != w.name {
			continue
		}
		n := 1
		if e.Series != nil {
			n = int(e.Series.Count)
		}
		reasons[e.Reason] += n
		notes = append(notes, e.Reason+": "+e.Note)
	}
	return reasons, notes
}

func (w *world) result(format string, args ...any) {
	w.t.Logf("RESULT %s: "+format, append([]any{w.t.Name()}, args...)...)
}

func ptr[T any](v T) *T { return &v }

func steps(weights ...int32) []v1.Step {
	var s []v1.Step
	for _, wt := range weights {
		s = append(s, v1.Step{Weight: wt, Pause: &metav1.Duration{Duration: 15 * time.Second}})
	}
	return s
}

// A version that fails none of its requests goes through both steps, its checks measuring an error rate of 0 on
// real traffic, and is promoted; the pods' own counters show the canary's share of the requests at each step.
func TestAGoodVersionIsPromoted(t *testing.T) {
	t.Parallel()
	const replicas = 4
	w := newWorld(t, "good", replicas, steps(25, 50), 120, nil)
	old := w.waitStable(replicas)
	w.watchCapacity()

	changed := time.Now()
	rev := w.setVersion(v2Good)
	if rev == old || rev == "" {
		t.Fatalf("the new template's revision is %q (the old one %q)", rev, old)
	}
	// At each step's end, the canary's share of the requests over the step's last 10 seconds.
	var shares []string
	step := int32(-1)
	// The most failed checks seen in a step (a promotion clears the counts).
	var mostFailed int32
	final := w.waitFor(4*time.Minute, "the promotion", func(ro *v1.CanaryRollout) bool {
		s := ro.Status
		if s.Phase == v1.PhaseRolledBack {
			t.Fatalf("rolled back: %s", describe(ro))
		}
		if s.Analysis != nil && s.Analysis.FailedChecks > mostFailed {
			mostFailed = s.Analysis.FailedChecks
		}
		at := int32(-1)
		if s.CurrentStep != nil {
			at = *s.CurrentStep
		}
		if s.Phase == v1.PhasePromoting {
			at = int32(len(ro.Spec.Steps))
		}
		if at > step && step >= 0 {
			share, err := w.canaryShare()
			if err != nil {
				shares = append(shares, fmt.Sprintf("step %d: %v", step, err))
			} else {
				shares = append(shares, fmt.Sprintf("step %d (weight %d%%): %.1f%% of requests", step, ro.Spec.Steps[step].Weight, 100*share))
			}
		}
		if at > step {
			step = at
		}
		return s.Phase == v1.PhaseHealthy && s.StableRevision == rev
	})
	took := time.Since(changed)
	w.waitSettled(rev, replicas)

	if c := meta.FindStatusCondition(final.Status.Conditions, controller.ConditionAnalysisPassing); c == nil || c.Status != metav1.ConditionTrue ||
		!strings.Contains(c.Message, "error-rate 0 ") {
		t.Errorf("AnalysisPassing after the promotion: %+v; want True, with an error rate of 0 measured", c)
	}
	reasons, notes := w.events()
	for reason, want := range map[string]int{rollout.EventRolloutStarted: 1, rollout.EventStepAdvanced: 1, rollout.EventPromoting: 1, rollout.EventPromoted: 1} {
		if reasons[reason] < want {
			t.Errorf("%d %s Events, want at least %d; all: %q", reasons[reason], reason, want, notes)
		}
	}
	if reasons[rollout.EventRolledBack] != 0 {
		t.Errorf("a RolledBack Event: %q", notes)
	}
	if fewest := w.fewestReady(); fewest < replicas {
		t.Errorf("the ready pods went down to %d of %d during the rollout", fewest, replicas)
	}
	w.result("promoted %.1fs after the template changed; fewest ready pods %d of %d; at most %d failed checks tolerated in a step; %s",
		took.Seconds(), w.fewestReady(), replicas, mostFailed, strings.Join(shares, "; "))
}

// A version that fails half its requests fails its checks on measured error rates (not on missing data) and is
// rolled back: the stable is back at all its pods on the old version, the canary at 0.
func TestABadVersionIsRolledBack(t *testing.T) {
	t.Parallel()
	const replicas = 4
	w := newWorld(t, "bad", replicas, []v1.Step{{Weight: 25, Pause: &metav1.Duration{Duration: time.Minute}}, {Weight: 50}}, 120, nil)
	old := w.waitStable(replicas)
	w.watchCapacity()

	changed := time.Now()
	rev := w.setVersion(v2Bad)
	final := w.waitFor(3*time.Minute, "the rollback", func(ro *v1.CanaryRollout) bool {
		if ro.Status.Phase == v1.PhaseHealthy && ro.Status.StableRevision == rev {
			t.Fatalf("the bad version was promoted: %s", describe(ro))
		}
		return ro.Status.Phase == v1.PhaseRolledBack
	})
	took := time.Since(changed)
	w.waitSettled(old, replicas)

	s := final.Status
	if s.RolledBackRevision != rev || s.StableRevision != old || s.CanaryRevision != "" {
		t.Errorf("status after the rollback: rolledBack %q stable %q canary %q; want %q, %q, none", s.RolledBackRevision, s.StableRevision, s.CanaryRevision, rev, old)
	}
	if s.Analysis == nil || s.Analysis.FailedChecks != failureLimit+1 {
		t.Errorf("analysis status %+v; want %d failed checks", s.Analysis, failureLimit+1)
	}
	if c := meta.FindStatusCondition(s.Conditions, controller.ConditionProgressing); c == nil || c.Reason != rollout.ReasonRolledBack {
		t.Errorf("Progressing %+v; want reason RolledBack", c)
	}
	reasons, notes := w.events()
	if reasons[rollout.EventAnalysisFailed] != failureLimit+1 || reasons[rollout.EventRolledBack] != 1 {
		t.Errorf("Events %v; want %d AnalysisFailed and 1 RolledBack; all: %q", reasons, failureLimit+1, notes)
	}
	// The rollback must come from measuring the error rate, not from a query that found nothing.
	measured := 0
	for _, n := range notes {
		if strings.HasPrefix(n, rollout.EventAnalysisFailed) && strings.Contains(n, "above max 0.05") {
			measured++
		}
	}
	if measured == 0 {
		t.Errorf("no failed check measured an error rate above the max; the Events: %q", notes)
	}
	if fewest := w.fewestReady(); fewest < replicas {
		t.Errorf("the ready pods went down to %d of %d during the rollout", fewest, replicas)
	}
	w.result("rolled back %.1fs after the template changed; %d of %d failed checks measured the error rate; fewest ready pods %d of %d",
		took.Seconds(), measured, failureLimit+1, w.fewestReady(), replicas)
}

// A version whose pods never become ready is rolled back when the progress deadline passes, with no other event
// to wake the operator, and never before it: the stable keeps all its pods throughout.
func TestAVersionThatNeverBecomesReadyIsRolledBackAtTheDeadline(t *testing.T) {
	t.Parallel()
	const replicas, deadline = 2, 20
	w := newWorld(t, "hung", replicas, steps(50), deadline, nil)
	old := w.waitStable(replicas)
	w.watchCapacity()

	changed := time.Now()
	rev := w.setVersion(v2Hung)
	final := w.waitFor(2*time.Minute, "the deadline's rollback", func(ro *v1.CanaryRollout) bool {
		return ro.Status.Phase == v1.PhaseRolledBack
	})
	took := time.Since(changed)
	w.waitSettled(old, replicas)

	if took < deadline*time.Second {
		t.Errorf("rolled back after %v, before the %ds deadline", took, deadline)
	}
	s := final.Status
	if s.RolledBackRevision != rev {
		t.Errorf("rolledBackRevision %q, want %q", s.RolledBackRevision, rev)
	}
	if c := meta.FindStatusCondition(s.Conditions, controller.ConditionProgressing); c == nil || c.Reason != rollout.ReasonRolledBack ||
		!strings.Contains(c.Message, "progress deadline") {
		t.Errorf("Progressing %+v; want RolledBack for the progress deadline", c)
	}
	if s.Analysis != nil && s.Analysis.Checks != 0 {
		t.Errorf("%d checks ran for a step whose pods never became ready", s.Analysis.Checks)
	}
	if reasons, notes := w.events(); reasons[rollout.EventRolledBack] != 1 {
		t.Errorf("Events %v; want 1 RolledBack; all: %q", reasons, notes)
	}
	if fewest := w.fewestReady(); fewest < replicas {
		t.Errorf("the ready pods went down to %d of %d", fewest, replicas)
	}
	w.result("rolled back %.1fs after the template changed (deadline %ds); fewest ready pods %d of %d", took.Seconds(), deadline, w.fewestReady(), replicas)
}

// leaderIdentity is the operator's Lease's holder: the pod's name (the hostname), "_" and a UUID each process makes
// (controller-runtime's leader election); "" when nobody holds it.
func leaderIdentity(t *testing.T) string {
	t.Helper()
	var lease coordinationv1.Lease
	err := k8s.Get(context.Background(), client.ObjectKey{Namespace: "canary-system", Name: "canary.rollouts.vivekdavara.github.io"}, &lease)
	if err != nil || lease.Spec.HolderIdentity == nil {
		return ""
	}
	return *lease.Spec.HolderIdentity
}

// leaderPod is the pod holding the operator's Lease.
func leaderPod(t *testing.T) string {
	t.Helper()
	pod, _, _ := strings.Cut(leaderIdentity(t), "_")
	return pod
}

// kindNode is the kind cluster's node container (make e2e-test sets it from the cluster's name).
var kindNode = envOr("CANARY_E2E_NODE", "canary-e2e-control-plane")

// The leader crashes mid-step: its process is killed with SIGKILL through the node's container runtime (crictl), so
// it can't give the Lease up. (Day 4 deleted its pod with no grace period, but the kubelet still sends SIGTERM first,
// and since day 5 the operator answers that by releasing the Lease: that's the upgrade test's case now.) Another
// process takes the Lease when it expires (the other replica, or the killed container restarted: either has a new
// identity), carries the step on from the status (the pause isn't restarted) and promotes, and the ready pods never go
// below replicas. The killed container restarts in the same pod, so the holder
// is compared by identity (a new process has a new UUID), not by pod. Not parallel, so the handover doesn't hold up
// the other tests' rollouts and timings.
func TestARolloutSurvivesTheLeaderCrashing(t *testing.T) {
	const replicas = 4
	w := newWorld(t, "failover", replicas, []v1.Step{
		{Weight: 25, Pause: &metav1.Duration{Duration: 40 * time.Second}},
		{Weight: 50, Pause: &metav1.Duration{Duration: 10 * time.Second}},
	}, 120, nil)
	w.waitStable(replicas)
	w.watchCapacity()

	changed := time.Now()
	rev := w.setVersion(v2Good)
	paused := w.waitFor(time.Minute, "the first step's pause", func(ro *v1.CanaryRollout) bool {
		c := meta.FindStatusCondition(ro.Status.Conditions, controller.ConditionProgressing)
		return ro.Status.StepStartedAt != nil && c != nil && c.Reason == rollout.ReasonStepPaused
	})
	started := paused.Status.StepStartedAt.Time
	leader := leaderIdentity(t)
	if leader == "" {
		t.Fatal("no operator holds the Lease")
	}
	podName, _, _ := strings.Cut(leader, "_")
	var pod corev1.Pod
	if err := k8s.Get(context.Background(), client.ObjectKey{Namespace: "canary-system", Name: podName}, &pod); err != nil {
		t.Fatal(err)
	}
	if len(pod.Status.ContainerStatuses) != 1 {
		t.Fatalf("the leader's pod has %d containers", len(pod.Status.ContainerStatuses))
	}
	_, container, ok := strings.Cut(pod.Status.ContainerStatuses[0].ContainerID, "://")
	if !ok {
		t.Fatalf("the leader's container ID %q", pod.Status.ContainerStatuses[0].ContainerID)
	}
	if out, err := exec.Command("docker", "exec", kindNode, "crictl", "stop", "--timeout", "0", container).CombinedOutput(); err != nil {
		t.Fatalf("killing the leader's container: %v: %s", err, out)
	}
	killed := time.Now()
	t.Logf("+%5.1fs killed the leader, %s", time.Since(w.start).Seconds(), leader)
	var next string
	for next = leaderIdentity(t); next == "" || next == leader; next = leaderIdentity(t) {
		if time.Since(killed) > time.Minute {
			t.Fatal("no new leader a minute after the old one was killed")
		}
		time.Sleep(100 * time.Millisecond)
	}
	takeover := time.Since(killed)
	t.Logf("+%5.1fs %s took the Lease", time.Since(w.start).Seconds(), next)

	final := w.waitFor(3*time.Minute, "the promotion", func(ro *v1.CanaryRollout) bool {
		s := ro.Status
		if s.Phase == v1.PhaseRolledBack {
			t.Fatalf("rolled back: %s", describe(ro))
		}
		// While the first step runs, its pause is the one the old leader started.
		if s.CurrentStep != nil && *s.CurrentStep == 0 && s.StepStartedAt != nil && !s.StepStartedAt.Time.Equal(started) {
			t.Fatalf("the new leader restarted the pause: it started at %v, now %v", started, s.StepStartedAt.Time)
		}
		return s.Phase == v1.PhaseHealthy && s.StableRevision == rev
	})
	took := time.Since(changed)
	w.waitSettled(rev, replicas)

	reasons, notes := w.events()
	for reason, want := range map[string]int{rollout.EventRolloutStarted: 1, rollout.EventPromoting: 1, rollout.EventPromoted: 1, rollout.EventRolledBack: 0} {
		if reasons[reason] != want {
			t.Errorf("%d %s Events, want %d; all: %q", reasons[reason], reason, want, notes)
		}
	}
	if fewest := w.fewestReady(); fewest < replicas {
		t.Errorf("the ready pods went down to %d of %d", fewest, replicas)
	}
	if final.Status.StableRevision != rev {
		t.Errorf("stable revision %q, want %q", final.Status.StableRevision, rev)
	}
	w.result("the new leader took the Lease %.1fs after the old one was killed; promoted %.1fs after the template changed, the first pause kept; fewest ready pods %d of %d",
		takeover.Seconds(), took.Seconds(), w.fewestReady(), replicas)
}

// The operator is upgraded mid-step: helm upgrade changes its pod template (an annotation), so its Deployment
// replaces both pods one at a time, each told to stop (SIGTERM) and given its grace period, as any upgrade or node
// drain does. The rollout carries on from the status as after a crash (the pause kept, one of each Event, capacity),
// and the result is how long nothing reconciled: from the old leader's pod being told to stop to another pod holding
// the Lease. Not parallel, like the crash.
func TestARolloutSurvivesAnOperatorUpgrade(t *testing.T) {
	helm := os.Getenv("CANARY_HELM")
	if helm == "" {
		t.Fatal("CANARY_HELM isn't set (make e2e-test sets it to the pinned helm)")
	}
	const replicas = 4
	w := newWorld(t, "upgrade", replicas, []v1.Step{
		{Weight: 25, Pause: &metav1.Duration{Duration: 40 * time.Second}},
		{Weight: 50, Pause: &metav1.Duration{Duration: 10 * time.Second}},
	}, 120, nil)
	w.waitStable(replicas)
	w.watchCapacity()

	changed := time.Now()
	rev := w.setVersion(v2Good)
	paused := w.waitFor(time.Minute, "the first step's pause", func(ro *v1.CanaryRollout) bool {
		c := meta.FindStatusCondition(ro.Status.Conditions, controller.ConditionProgressing)
		return ro.Status.StepStartedAt != nil && c != nil && c.Reason == rollout.ReasonStepPaused
	})
	started := paused.Status.StepStartedAt.Time
	leader := leaderPod(t)
	if leader == "" {
		t.Fatal("no operator holds the Lease")
	}
	out, err := exec.Command(helm, "upgrade", "canary", "../../charts/canary", "-n", "canary-system", "--reuse-values",
		"--set-string", "podAnnotations.e2e-upgraded-at="+time.Now().UTC().Format(time.RFC3339Nano)).CombinedOutput()
	if err != nil {
		t.Fatalf("helm upgrade: %v: %s", err, out)
	}
	upgraded := time.Now()
	t.Logf("+%5.1fs upgraded the operator; the leader is %s", time.Since(w.start).Seconds(), leader)

	// The leader's pod is told to stop when its deletion timestamp is set (or it is already gone).
	var stopped time.Time
	for {
		var pod corev1.Pod
		err := k8s.Get(context.Background(), client.ObjectKey{Namespace: "canary-system", Name: leader}, &pod)
		if apierrors.IsNotFound(err) || (err == nil && pod.DeletionTimestamp != nil) {
			stopped = time.Now()
			break
		}
		if time.Since(upgraded) > 3*time.Minute {
			t.Fatalf("the old leader's pod wasn't stopped within 3 minutes of the upgrade (%v)", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("+%5.1fs the old leader's pod is stopping", time.Since(w.start).Seconds())
	var next string
	for next = leaderPod(t); next == "" || next == leader; next = leaderPod(t) {
		if time.Since(stopped) > time.Minute {
			t.Fatal("no new leader a minute after the old one was told to stop")
		}
		time.Sleep(100 * time.Millisecond)
	}
	handover := time.Since(stopped)
	t.Logf("+%5.1fs %s took the Lease", time.Since(w.start).Seconds(), next)

	final := w.waitFor(3*time.Minute, "the promotion", func(ro *v1.CanaryRollout) bool {
		s := ro.Status
		if s.Phase == v1.PhaseRolledBack {
			t.Fatalf("rolled back: %s", describe(ro))
		}
		if s.CurrentStep != nil && *s.CurrentStep == 0 && s.StepStartedAt != nil && !s.StepStartedAt.Time.Equal(started) {
			t.Fatalf("the new leader restarted the pause: it started at %v, now %v", started, s.StepStartedAt.Time)
		}
		return s.Phase == v1.PhaseHealthy && s.StableRevision == rev
	})
	took := time.Since(changed)
	w.waitSettled(rev, replicas)

	reasons, notes := w.events()
	for reason, want := range map[string]int{rollout.EventRolloutStarted: 1, rollout.EventPromoting: 1, rollout.EventPromoted: 1, rollout.EventRolledBack: 0} {
		if reasons[reason] != want {
			t.Errorf("%d %s Events, want %d; all: %q", reasons[reason], reason, want, notes)
		}
	}
	if fewest := w.fewestReady(); fewest < replicas {
		t.Errorf("the ready pods went down to %d of %d", fewest, replicas)
	}
	if final.Status.StableRevision != rev {
		t.Errorf("stable revision %q, want %q", final.Status.StableRevision, rev)
	}
	w.result("the old leader's pod was told to stop %.1fs after helm upgrade, and another took the Lease %.1fs after that; promoted %.1fs after the template changed, the first pause kept; fewest ready pods %d of %d",
		stopped.Sub(upgraded).Seconds(), handover.Seconds(), took.Seconds(), w.fewestReady(), replicas)
}

// A limitation (README "Limitations"), reproduced: a namespace whose ResourceQuota leaves room for one pod beyond
// the rollout's and the load generator's. A rollback from the 50% step needs the stable's pods back before the
// canary's go (capacity), the quota has room for only one of them while the canary's exist, and the rollback never
// finishes: the rolled-back version keeps serving its share. The metric fails once the canary has two pods, so the
// rollback comes at the second step.
func TestATightQuotaHoldsARollbackWithTheCanaryServing(t *testing.T) {
	t.Parallel()
	const replicas = 4
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "pods"},
		Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
			// The rollout's pods, one more (a step's canary pod starts before a stable pod goes), and the load generator.
			corev1.ResourcePods: *resource.NewQuantity(replicas+2, resource.DecimalSI),
		}},
	}
	w := newWorld(t, "quota", replicas, []v1.Step{
		{Weight: 25, Pause: &metav1.Duration{Duration: 10 * time.Second}},
		{Weight: 50, Pause: &metav1.Duration{Duration: time.Hour}},
	}, 120, func(ro *v1.CanaryRollout) {
		ro.Spec.Analysis.Metrics = []v1.Metric{{
			Name:  "canary-pods",
			Query: fmt.Sprintf(`count(up{namespace=%q,rollout="quota",track="canary"})`, ro.Namespace),
			Max:   ptr(1.0),
		}}
	}, quota)
	old := w.waitStable(replicas)
	w.watchCapacity()

	rev := w.setVersion(v2Good)
	w.waitFor(3*time.Minute, "the rollback at the second step", func(ro *v1.CanaryRollout) bool {
		if ro.Status.Phase == v1.PhaseHealthy && ro.Status.StableRevision == rev {
			t.Fatalf("promoted: %s", describe(ro))
		}
		return ro.Status.Phase == v1.PhaseRolledBack
	})
	rolledBack := time.Now()
	// Give it 30 seconds to finish, which it can't.
	time.Sleep(30 * time.Second)
	held := time.Since(rolledBack)
	stable, canary := w.deployment("stable"), w.deployment("canary")
	if stable == nil || canary == nil {
		t.Fatalf("a Deployment is gone: stable %v, canary %v", stable != nil, canary != nil)
	}
	ro := w.get()
	if ro.Status.Phase != v1.PhaseRolledBack || ro.Status.StableRevision != old {
		t.Fatalf("after the hold: %s", describe(ro))
	}
	if stable.Status.ReadyReplicas >= replicas || canary.Status.ReadyReplicas == 0 {
		t.Fatalf("the rollback finished under the quota (stable %d ready, canary %d): the limitation is gone; update README",
			stable.Status.ReadyReplicas, canary.Status.ReadyReplicas)
	}
	// The stable's ReplicaSet can't create the pods it is short: the quota refuses them.
	var sets appsv1.ReplicaSetList
	if err := k8s.List(context.Background(), &sets, client.InNamespace(w.ns), client.MatchingLabels{controller.LabelRollout: "quota", controller.LabelTrack: "stable"}); err != nil {
		t.Fatal(err)
	}
	why := ""
	for _, rs := range sets.Items {
		for _, c := range rs.Status.Conditions {
			if c.Type == appsv1.ReplicaSetReplicaFailure && c.Status == corev1.ConditionTrue {
				why = c.Message
			}
		}
	}
	if !strings.Contains(why, "exceeded quota") {
		t.Errorf("the stable's ReplicaSets show no quota failure: %q", why)
	}
	// Whether the Deployment itself says so (the Deployment controller copies a ReplicaSet's ReplicaFailure onto its
	// Deployment when it syncs a rollout's status, not when it only scales): the signal a fix would read without
	// watching ReplicaSets (README, "Limitations"). Reported, not asserted.
	onDeployment := "no ReplicaFailure condition"
	for _, c := range stable.Status.Conditions {
		if c.Type == appsv1.DeploymentReplicaFailure {
			onDeployment = fmt.Sprintf("ReplicaFailure=%s (%s)", c.Status, c.Reason)
		}
	}
	share, err := w.canaryShare()
	if err != nil {
		t.Fatal(err)
	}
	if fewest := w.fewestReady(); fewest < replicas {
		t.Errorf("the ready pods went down to %d of %d", fewest, replicas)
	}
	w.result("%.0fs after the rollback: stable %d of %d ready, the rolled-back canary still at %d ready pods serving %.1f%% of the requests; the stable's ReplicaSet: %.120s; the stable Deployment: %s",
		held.Seconds(), stable.Status.ReadyReplicas, replicas, canary.Status.ReadyReplicas, 100*share, why, onDeployment)
}

// The analysis client (internal/analysis/prometheus.go) against a real Prometheus: the unit tests use canned
// answers, written from the API's documentation; these are the answers Prometheus actually gives.
func TestThePrometheusClientAgainstPrometheus(t *testing.T) {
	ctx := context.Background()
	// Prometheus scrapes the operator's two pods like any others; its own up series of them (1 per scrape, one series
	// per pod) are real series to query. Wait for three scrapes of each (a rate needs two).
	const up = `up{namespace="canary-system"}`
	deadline := time.Now().Add(time.Minute)
	for {
		r, err := prom.Query(ctx, `min(count_over_time(`+up+`[1m]))`)
		if err == nil && len(r.Values) == 1 && r.Values[0] >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Prometheus hasn't scraped the operator three times in a minute: %+v, %v", r, err)
		}
		time.Sleep(time.Second)
	}
	if r, err := prom.Query(ctx, `count(count by (__name__) ({namespace="canary-system"}))`); err == nil && len(r.Values) == 1 {
		t.Logf("the operator's pods export %g metric names", r.Values[0])
	}
	m := v1.Metric{Name: "m", Max: ptr(1.0)}
	for _, c := range []struct {
		query string
		typ   string
		// The verdict's Why, or the start of the client's error.
		why string
		err string
	}{
		{query: `vector(0.25)`, typ: "vector", why: "max 1"},
		{query: `scalar(vector(0.5))`, typ: "scalar", why: "max 1"},
		{query: `2`, typ: "scalar", why: "above max 1"},
		{query: `vector(0) / 0`, typ: "vector", why: "not a finite number"},
		{query: `vector(1) / 0`, typ: "vector", why: "not a finite number"},
		{query: `-vector(1) / 0`, typ: "vector", why: "not a finite number"},
		{query: `vector(4.9e-324)`, typ: "vector", why: "max 1"},
		{query: `no_such_metric_anywhere`, typ: "vector", why: "the query returned no samples"},
		{query: `vector(1) or label_replace(vector(2), "x", "y", "", "")`, typ: "vector", why: "the query returned 2 samples, not one"},
		{query: up, typ: "vector", why: "the query returned 2 samples, not one"},
		{query: up + `[1m]`, typ: "matrix", why: "the query returned a matrix, not one number"},
		{query: `"text"`, typ: "string", why: "the query returned a string, not one number"},
		// An info annotation (a rate over a gauge) says nothing about the data and passes.
		{query: `sum(rate(` + up + `[1m])) * 0`, typ: "vector", why: "max 1"},
		// A warning means the answer may be wrong or partial: here, buckets with no le label.
		{query: `histogram_quantile(0.9, ` + up + `)`, err: "Prometheus answered with a warning"},
		{query: `sum(`, err: "Prometheus: bad_data: "},
	} {
		r, err := prom.Query(ctx, c.query)
		got := analysis.Evaluate(m, r, err)
		switch {
		case c.err != "":
			if err == nil || !strings.HasPrefix(err.Error(), c.err) {
				t.Errorf("%s: %+v, %v; want an error starting %q", c.query, r, err, c.err)
			}
		case err != nil:
			t.Errorf("%s: %v", c.query, err)
		case r.Type != c.typ || got.Why != c.why:
			t.Errorf("%s: %s %v, %q; want %s, %q", c.query, r.Type, r.Values, got.Why, c.typ, c.why)
		}
	}
}
