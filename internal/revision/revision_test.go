package revision

import (
	"math/rand/v2"
	"regexp"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

func template(image string) *corev1.PodTemplateSpec {
	return &corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "web", "tier": "frontend"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:  "web",
			Image: image,
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("0.5")},
			},
		}}},
	}
}

// The value is pinned: a change of encoding (a new k8s.io/api adding a field that isn't omitempty, say) would give
// every running template a new revision, and so start a rollout of every application the operator manages. This
// test makes such an upgrade visible instead of silent.
func TestRevisionIsPinned(t *testing.T) {
	if got, want := Of(template("example.com/web:1")), "f8c1c043400d9ddb"; got != want {
		t.Errorf("Of = %s; want %s (see the comment before changing it)", got, want)
	}
}

func TestRevisionIsALabelValue(t *testing.T) {
	got := Of(template("example.com/web:1"))
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(got) {
		t.Errorf("Of = %q; want 16 lowercase hex digits", got)
	}
	if errs := validation.IsValidLabelValue(got); len(errs) > 0 {
		t.Errorf("Of = %q is not a label value: %v", got, errs)
	}
}

func TestRevisionChangesWithTheTemplate(t *testing.T) {
	base := Of(template("example.com/web:1"))
	changes := map[string]func(*corev1.PodTemplateSpec){
		"image":      func(p *corev1.PodTemplateSpec) { p.Spec.Containers[0].Image = "example.com/web:2" },
		"label":      func(p *corev1.PodTemplateSpec) { p.Labels["tier"] = "backend" },
		"annotation": func(p *corev1.PodTemplateSpec) { p.Annotations = map[string]string{"a": "b"} },
		"env":        func(p *corev1.PodTemplateSpec) { p.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "A", Value: "1"}} },
		"cpu": func(p *corev1.PodTemplateSpec) {
			p.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("1")
		},
	}
	for name, change := range changes {
		p := template("example.com/web:1")
		change(p)
		if Of(p) == base {
			t.Errorf("changing the %s kept the revision %s", name, base)
		}
	}
}

// Equal templates get equal revisions however they were built: maps filled in any order, quantities spelled either
// way.
func TestEqualTemplatesGetEqualRevisions(t *testing.T) {
	want := Of(template("example.com/web:1"))
	p := template("example.com/web:1")
	p.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("500m")
	if got := Of(p); got != want {
		t.Errorf("cpu 500m: %s; want %s as for 0.5", got, want)
	}

	keys := make([]string, 50)
	for i := range keys {
		keys[i] = string(rune('a'+i%26)) + string(rune('a'+i/26))
	}
	r := rand.New(rand.NewPCG(1, 2))
	var first string
	for trial := range 100 {
		r.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
		p := template("example.com/web:1")
		p.Labels = map[string]string{}
		for _, k := range keys {
			p.Labels[k] = k + "-value"
		}
		got := Of(p)
		if trial == 0 {
			first = got
		} else if got != first {
			t.Fatalf("trial %d: %s; want %s (same labels, another insertion order)", trial, got, first)
		}
	}
}

// A quantity's format is part of the revision (found by day 1's review): canonical within a format, so 0.5 and 500m
// agree, but 1Gi and 1073741824, though equal, don't. Rewriting a quantity in another format starts a rollout.
func TestQuantityFormatIsPartOfTheRevision(t *testing.T) {
	memory := func(q string) string {
		p := template("example.com/web:1")
		p.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory] = resource.MustParse(q)
		return Of(p)
	}
	if memory("1Gi") == memory("1073741824") {
		t.Error("1Gi and 1073741824 have the same revision; the docs say they don't")
	}
	if memory("1500m") != memory("1.5") {
		t.Error("1500m and 1.5 have different revisions; both are decimal SI and should agree")
	}
}
