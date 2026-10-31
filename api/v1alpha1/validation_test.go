package v1alpha1_test

// The CRD's rules (DESIGN.md, "Spec") checked against a real API server: each refusal names its rule, and each
// boundary value is accepted, so a rule that refuses too much fails here as well as one that refuses too little.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	v1 "github.com/vivekdavara/canary/api/v1alpha1"
	"github.com/vivekdavara/canary/internal/revision"
	"github.com/vivekdavara/canary/internal/testenv"
)

func TestMain(m *testing.M) { os.Exit(testenv.Main(m)) }

// valid is the smallest rollout the API server accepts, with an analysis so its rules can be varied too.
func valid(name string) *v1.CanaryRollout {
	return &v1.CanaryRollout{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1.CanaryRolloutSpec{
			Replicas: 10,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "web"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: "example.com/web:1"}}},
			},
			Steps: []v1.Step{{Weight: 10}, {Weight: 50}},
			Analysis: &v1.Analysis{Metrics: []v1.Metric{
				{Name: "error-rate", Query: "vector(0)", Max: ptr.To(0.01)},
			}},
		},
	}
}

func create(t *testing.T, obj client.Object) error {
	t.Helper()
	env := testenv.Get(t)
	obj.SetNamespace(env.Namespace(t))
	return env.Client.Create(context.Background(), obj)
}

// refused checks that err is the API server's Invalid refusal and says want.
func refused(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("accepted; want a refusal saying %q", want)
	}
	if !apierrors.IsInvalid(err) {
		t.Fatalf("got %v; want an Invalid refusal saying %q", err, want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("refused with %q; want it to say %q", err.Error(), want)
	}
}

func TestRulesRefuse(t *testing.T) {
	steps := func(weights ...int32) []v1.Step {
		s := make([]v1.Step, len(weights))
		for i, w := range weights {
			s[i] = v1.Step{Weight: w}
		}
		return s
	}
	metric := func(name string) v1.Metric {
		return v1.Metric{Name: name, Query: "vector(0)", Max: ptr.To(1.0)}
	}
	cases := []struct {
		name   string
		change func(r *v1.CanaryRollout)
		want   string
	}{
		{"no replicas", func(r *v1.CanaryRollout) { r.Spec.Replicas = 0 }, "spec.replicas: Invalid value: 0: spec.replicas in body should be greater than or equal to 1"},
		{"too many replicas", func(r *v1.CanaryRollout) { r.Spec.Replicas = 1001 }, "spec.replicas: Invalid value: 1001: spec.replicas in body should be less than or equal to 1000"},
		{"no steps", func(r *v1.CanaryRollout) { r.Spec.Steps = nil }, "spec.steps: Required value"},
		{"21 steps", func(r *v1.CanaryRollout) {
			r.Spec.Steps = steps(1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21)
		}, "spec.steps: Too many: 21: must have at most 20 items"},
		{"weight 0", func(r *v1.CanaryRollout) { r.Spec.Steps = steps(0, 50) }, "spec.steps[0].weight: Invalid value: 0: spec.steps[0].weight in body should be greater than or equal to 1"},
		{"weight 101", func(r *v1.CanaryRollout) { r.Spec.Steps = steps(50, 101) }, "spec.steps[1].weight: Invalid value: 101: spec.steps[1].weight in body should be less than or equal to 100"},
		{"weights decreasing", func(r *v1.CanaryRollout) { r.Spec.Steps = steps(50, 10) }, "step weights must be strictly increasing"},
		{"weights repeated", func(r *v1.CanaryRollout) { r.Spec.Steps = steps(10, 10, 50) }, "step weights must be strictly increasing"},
		{"weights repeated apart", func(r *v1.CanaryRollout) { r.Spec.Steps = steps(10, 20, 10) }, "step weights must be strictly increasing"},
		{"negative pause", func(r *v1.CanaryRollout) {
			r.Spec.Steps[0].Pause = &metav1.Duration{Duration: -time.Second}
		}, "pause must be from 0s to 24h"},
		{"pause over a day", func(r *v1.CanaryRollout) {
			r.Spec.Steps[0].Pause = &metav1.Duration{Duration: 24*time.Hour + time.Second}
		}, "pause must be from 0s to 24h"},
		{"no deadline", func(r *v1.CanaryRollout) { r.Spec.ProgressDeadlineSeconds = ptr.To[int32](0) }, "spec.progressDeadlineSeconds: Invalid value: 0"},
		{"deadline over a day", func(r *v1.CanaryRollout) { r.Spec.ProgressDeadlineSeconds = ptr.To[int32](86401) }, "spec.progressDeadlineSeconds: Invalid value: 86401"},
		{"interval under 1s", func(r *v1.CanaryRollout) {
			r.Spec.Analysis.Interval = &metav1.Duration{Duration: 999 * time.Millisecond}
		}, "interval must be from 1s to 1h"},
		{"interval over 1h", func(r *v1.CanaryRollout) {
			r.Spec.Analysis.Interval = &metav1.Duration{Duration: time.Hour + time.Second}
		}, "interval must be from 1s to 1h"},
		{"negative failure limit", func(r *v1.CanaryRollout) { r.Spec.Analysis.FailureLimit = ptr.To[int32](-1) }, "spec.analysis.failureLimit: Invalid value: -1"},
		{"failure limit over 100", func(r *v1.CanaryRollout) { r.Spec.Analysis.FailureLimit = ptr.To[int32](101) }, "spec.analysis.failureLimit: Invalid value: 101"},
		{"no metrics", func(r *v1.CanaryRollout) { r.Spec.Analysis.Metrics = nil }, "spec.analysis.metrics: Required value"},
		{"11 metrics", func(r *v1.CanaryRollout) {
			r.Spec.Analysis.Metrics = nil
			for _, n := range "abcdefghijk" {
				r.Spec.Analysis.Metrics = append(r.Spec.Analysis.Metrics, metric(string(n)))
			}
		}, "spec.analysis.metrics: Too many: 11: must have at most 10 items"},
		{"metric names repeated", func(r *v1.CanaryRollout) {
			r.Spec.Analysis.Metrics = []v1.Metric{metric("errors"), metric("errors")}
		}, `spec.analysis.metrics[1]: Duplicate value: {"name":"errors"}`},
		{"metric name not a DNS label", func(r *v1.CanaryRollout) {
			r.Spec.Analysis.Metrics = []v1.Metric{metric("Error_Rate")}
		}, `spec.analysis.metrics[0].name: Invalid value: "Error_Rate": spec.analysis.metrics[0].name in body should match`},
		{"empty query", func(r *v1.CanaryRollout) { r.Spec.Analysis.Metrics[0].Query = "" }, "spec.analysis.metrics[0].query: Invalid value: \"\": spec.analysis.metrics[0].query in body should be at least 1 chars long"},
		{"query over 4096", func(r *v1.CanaryRollout) {
			r.Spec.Analysis.Metrics[0].Query = strings.Repeat("x", 4097)
		}, "spec.analysis.metrics[0].query: Too long: may not be more than 4096 bytes"},
		{"no bounds", func(r *v1.CanaryRollout) { r.Spec.Analysis.Metrics[0].Max = nil }, "a metric needs min, max or both"},
		{"min above max", func(r *v1.CanaryRollout) {
			r.Spec.Analysis.Metrics[0].Min = ptr.To(2.0)
			r.Spec.Analysis.Metrics[0].Max = ptr.To(1.5)
		}, "min must not be greater than max"},
		{"reserved template label", func(r *v1.CanaryRollout) {
			r.Spec.Template.Labels[v1.LabelPrefix+"track"] = "canary"
		}, "template labels may not start with rollouts.vivekdavara.github.io/"},
		{"no containers", func(r *v1.CanaryRollout) { r.Spec.Template.Spec.Containers = nil }, "spec.template.spec.containers: Required value"},
		{"name over 50", func(r *v1.CanaryRollout) { r.Name = strings.Repeat("a", 51) }, "the name may be at most 50 characters"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := valid("web")
			tc.change(r)
			refused(t, create(t, r), tc.want)
		})
	}
}

func TestRulesAcceptBoundaries(t *testing.T) {
	cases := []struct {
		name   string
		change func(r *v1.CanaryRollout)
	}{
		{"the base case", func(r *v1.CanaryRollout) {}},
		{"one replica", func(r *v1.CanaryRollout) { r.Spec.Replicas = 1 }},
		{"1000 replicas", func(r *v1.CanaryRollout) { r.Spec.Replicas = 1000 }},
		{"one step at 100", func(r *v1.CanaryRollout) { r.Spec.Steps = []v1.Step{{Weight: 100}} }},
		{"one step at 1", func(r *v1.CanaryRollout) { r.Spec.Steps = []v1.Step{{Weight: 1}} }},
		{"20 steps", func(r *v1.CanaryRollout) {
			r.Spec.Steps = nil
			for w := int32(5); w <= 100; w += 5 {
				r.Spec.Steps = append(r.Spec.Steps, v1.Step{Weight: w})
			}
		}},
		{"pauses at 0s and 24h", func(r *v1.CanaryRollout) {
			r.Spec.Steps[0].Pause = &metav1.Duration{}
			r.Spec.Steps[1].Pause = &metav1.Duration{Duration: 24 * time.Hour}
		}},
		{"deadline 1s", func(r *v1.CanaryRollout) { r.Spec.ProgressDeadlineSeconds = ptr.To[int32](1) }},
		{"deadline a day", func(r *v1.CanaryRollout) { r.Spec.ProgressDeadlineSeconds = ptr.To[int32](86400) }},
		{"interval 1s", func(r *v1.CanaryRollout) { r.Spec.Analysis.Interval = &metav1.Duration{Duration: time.Second} }},
		{"interval 1h", func(r *v1.CanaryRollout) { r.Spec.Analysis.Interval = &metav1.Duration{Duration: time.Hour} }},
		{"failure limit 100", func(r *v1.CanaryRollout) { r.Spec.Analysis.FailureLimit = ptr.To[int32](100) }},
		{"no analysis", func(r *v1.CanaryRollout) { r.Spec.Analysis = nil }},
		{"min only", func(r *v1.CanaryRollout) {
			r.Spec.Analysis.Metrics[0].Min, r.Spec.Analysis.Metrics[0].Max = ptr.To(0.99), nil
		}},
		{"min equal to max", func(r *v1.CanaryRollout) {
			r.Spec.Analysis.Metrics[0].Min = ptr.To(0.01)
			r.Spec.Analysis.Metrics[0].Max = ptr.To(0.01)
		}},
		{"negative bounds", func(r *v1.CanaryRollout) {
			r.Spec.Analysis.Metrics[0].Min = ptr.To(-2.5)
			r.Spec.Analysis.Metrics[0].Max = ptr.To(-0.5)
		}},
		{"10 metrics", func(r *v1.CanaryRollout) {
			r.Spec.Analysis.Metrics = nil
			for _, n := range "abcdefghij" {
				r.Spec.Analysis.Metrics = append(r.Spec.Analysis.Metrics,
					v1.Metric{Name: string(n), Query: "vector(0)", Max: ptr.To(1.0)})
			}
		}},
		{"query of 4096", func(r *v1.CanaryRollout) { r.Spec.Analysis.Metrics[0].Query = strings.Repeat("x", 4096) }},
		{"a label merely containing the prefix", func(r *v1.CanaryRollout) {
			r.Spec.Template.Labels["example.com/rollouts.vivekdavara.github.io"] = "x"
		}},
		{"name of 50", func(r *v1.CanaryRollout) { r.Name = strings.Repeat("a", 50) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := valid("web")
			tc.change(r)
			if err := create(t, r); err != nil {
				t.Fatalf("refused: %v", err)
			}
		})
	}
}

// unstructuredValid is valid("web") as a map, for what the typed struct can't express (strings that aren't
// durations, floating-point numbers, missing required fields).
func unstructuredValid(t *testing.T) *unstructured.Unstructured {
	t.Helper()
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(valid("web"))
	if err != nil {
		t.Fatal(err)
	}
	u := &unstructured.Unstructured{Object: m}
	u.SetGroupVersionKind(v1.GroupVersion.WithKind("CanaryRollout"))
	return u
}

func TestRulesRefuseRawInput(t *testing.T) {
	cases := []struct {
		name  string
		path  []string
		value any
		want  string
	}{
		// A day isn't a Go duration unit: had the schema let it in, the operator couldn't decode the object at all.
		{"pause in days", []string{"spec", "steps"}, []any{map[string]any{"weight": int64(10), "pause": "1d"}}, "spec.steps[0].pause"},
		{"pause not a duration", []string{"spec", "steps"}, []any{map[string]any{"weight": int64(10), "pause": "soon"}}, "spec.steps[0].pause"},
		{"interval not a duration", []string{"spec", "analysis", "interval"}, "30", "spec.analysis.interval"},
		// A quoted number is a string, not a number: `max: "0.01"` is refused rather than compared as text.
		{"max as a string", []string{"spec", "analysis", "metrics"}, []any{map[string]any{"name": "e", "query": "q", "max": "0.01"}}, "spec.analysis.metrics[0].max"},
		{"min above max as integers", []string{"spec", "analysis", "metrics"}, []any{map[string]any{"name": "e", "query": "q", "min": int64(2), "max": int64(1)}}, "min must not be greater than max"},
		{"no template", []string{"spec", "template"}, nil, "spec.template: Required value"},
		{"no replicas field", []string{"spec", "replicas"}, nil, "spec.replicas: Required value"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := unstructuredValid(t)
			if tc.value == nil {
				unstructured.RemoveNestedField(u.Object, tc.path...)
			} else if err := unstructured.SetNestedField(u.Object, tc.value, tc.path...); err != nil {
				t.Fatal(err)
			}
			refused(t, create(t, u), tc.want)
		})
	}
}

// The API server fills the defaults DESIGN.md names, keeps the template's labels (generateEmbeddedObjectMeta), and
// stores a threshold as written.
func TestDefaultsAndTemplateLabelsSurvive(t *testing.T) {
	env := testenv.Get(t)
	r := valid("web")
	r.Spec.Template.Annotations = map[string]string{"example.com/note": "kept"}
	if err := create(t, r); err != nil {
		t.Fatal(err)
	}
	var got v1.CanaryRollout
	if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(r), &got); err != nil {
		t.Fatal(err)
	}
	if d := got.Spec.ProgressDeadlineSeconds; d == nil || *d != 600 {
		t.Errorf("progressDeadlineSeconds = %v; want the default 600", d)
	}
	for i, s := range got.Spec.Steps {
		if s.Pause == nil || s.Pause.Duration != 0 {
			t.Errorf("steps[%d].pause = %v; want the default 0s", i, s.Pause)
		}
	}
	if a := got.Spec.Analysis; a.Interval == nil || a.Interval.Duration != 30*time.Second {
		t.Errorf("analysis.interval = %v; want the default 30s", a.Interval)
	}
	if a := got.Spec.Analysis; a.FailureLimit == nil || *a.FailureLimit != 0 {
		t.Errorf("analysis.failureLimit = %v; want the default 0", a.FailureLimit)
	}
	if got.Spec.Template.Labels["app"] != "web" {
		t.Errorf("template labels = %v; want app=web kept", got.Spec.Template.Labels)
	}
	if got.Spec.Template.Annotations["example.com/note"] != "kept" {
		t.Errorf("template annotations = %v; want example.com/note kept", got.Spec.Template.Annotations)
	}
	if m := got.Spec.Analysis.Metrics[0].Max; m == nil || *m != 0.01 {
		t.Errorf("max = %v; want 0.01", m)
	}
}

// The API server fills defaults inside the pod template too (the CRD carries the pod schema's own defaults), so the
// stored template can differ from the one a client sent: revisions are taken from the stored one.
func TestServerDefaultsInsideTheTemplate(t *testing.T) {
	env := testenv.Get(t)
	r := valid("web")
	r.Spec.Template.Spec.Containers[0].Ports = []corev1.ContainerPort{{ContainerPort: 8080}}
	sent := r.Spec.Template.DeepCopy()
	if err := create(t, r); err != nil {
		t.Fatal(err)
	}
	var got v1.CanaryRollout
	if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(r), &got); err != nil {
		t.Fatal(err)
	}
	if p := got.Spec.Template.Spec.Containers[0].Ports[0].Protocol; p != corev1.ProtocolTCP {
		t.Fatalf("stored port protocol %q; want the default TCP", p)
	}
	if revision.Of(sent) == revision.Of(&got.Spec.Template) {
		t.Errorf("the sent and stored templates have the same revision; the default should have changed it")
	}
	// Reading it again gives the same revision: the defaults are stored, not applied on each read.
	var again v1.CanaryRollout
	if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(r), &again); err != nil {
		t.Fatal(err)
	}
	if revision.Of(&again.Spec.Template) != revision.Of(&got.Spec.Template) {
		t.Errorf("two reads of one object gave different revisions")
	}
}

// Status is a subresource: a create or an update of the object ignores it, and only a spec change moves the
// generation (what status.observedGeneration is compared with).
func TestStatusIsASubresource(t *testing.T) {
	env := testenv.Get(t)
	ctx := context.Background()
	r := valid("web")
	r.Status.Phase = v1.PhaseRolledBack
	if err := create(t, r); err != nil {
		t.Fatal(err)
	}
	if r.Status.Phase != "" {
		t.Errorf("created with status.phase %q; want it dropped", r.Status.Phase)
	}
	if r.Generation != 1 {
		t.Fatalf("generation = %d; want 1", r.Generation)
	}
	r.Status.Phase = v1.PhaseHealthy
	if err := env.Client.Status().Update(ctx, r); err != nil {
		t.Fatal(err)
	}
	if r.Generation != 1 || r.Status.Phase != v1.PhaseHealthy {
		t.Errorf("after a status update: generation %d, phase %q; want 1, Healthy", r.Generation, r.Status.Phase)
	}
	r.Spec.Replicas = 3
	if err := env.Client.Update(ctx, r); err != nil {
		t.Fatal(err)
	}
	if r.Generation != 2 {
		t.Errorf("after a spec update: generation %d; want 2", r.Generation)
	}
	// A status update can't be smuggled into an update of the object.
	r.Status.Phase = v1.PhaseRolledBack
	if err := env.Client.Update(ctx, r); err != nil {
		t.Fatal(err)
	}
	if r.Status.Phase != v1.PhaseHealthy {
		t.Errorf("an update of the object set status.phase to %q; want Healthy kept", r.Status.Phase)
	}
}

// Every sample in config/samples decodes with no unknown field (a typo would be dropped silently by a lenient
// decoder, and pruned by the API server) and is accepted as written.
func TestSamplesAreAccepted(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "..", "config", "samples", "*.yaml"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no samples found: %v", err)
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var r v1.CanaryRollout
			if err := yaml.UnmarshalStrict(data, &r); err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			if err := create(t, &r); err != nil {
				t.Fatalf("%s refused: %v", path, err)
			}
		})
	}
}
