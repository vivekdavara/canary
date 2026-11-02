package controller

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	rolloutsv1alpha1 "github.com/vivekdavara/canary/api/v1alpha1"
	"github.com/vivekdavara/canary/internal/testenv"
)

// The operator before a005015 wrote status.analysis.lastCheckAt as a metav1.Time: RFC 3339 to the second, with no
// fraction ("2026-11-02T10:01:00Z"). The CRD's schema (format: date-time) accepts that, and MicroTime.UnmarshalJSON
// parses with exactly six fractional digits, so after an upgrade mid-pause the rollout can't be decoded: it is
// reported InvalidSpec and left alone for good (the operator never rewrites the status it can't read).
func TestReviewD3bASecondPrecisionLastCheckAtStillDecodes(t *testing.T) {
	env := testenv.Get(t)
	r := direct(t, *(&writeCounter{}).funcs())
	now := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	r.Now = func() time.Time { return now }
	r.Querier = nil
	ns := env.Namespace(t)
	ro := analysedRollout(ns, time.Hour, 5)
	if err := env.Client.Create(context.Background(), ro); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKeyFromObject(ro)
	reconcileOnce(t, r, ro) // the stable
	markReady(t, env.Client, ns, "web-stable")
	edit(t, env.Client, key, func(r *rolloutsv1alpha1.CanaryRollout) {
		r.Spec.Template.Spec.Containers[0].Image = "example.com/web:2"
	})
	reconcileOnce(t, r, ro) // the canary
	markReady(t, env.Client, ns, "web-canary")
	reconcileOnce(t, r, ro) // the pause starts

	// What the previous operator left in the status: one check, its time to the second.
	// (Patched as unstructured: the typed client can't decode the API server's answer either.)
	cur := &unstructured.Unstructured{}
	cur.SetGroupVersionKind(rolloutsv1alpha1.GroupVersion.WithKind("CanaryRollout"))
	cur.SetNamespace(ns)
	cur.SetName(ro.Name)
	patch := []byte(`{"status":{"analysis":{"checks":1,"lastCheckAt":"2026-11-02T10:00:02Z"}}}`)
	if err := env.Client.Status().Patch(context.Background(), cur, client.RawPatch(types.MergePatchType, patch)); err != nil {
		t.Fatalf("the API server refused a second-precision lastCheckAt: %v", err)
	}

	now = now.Add(3 * time.Second)
	reconcileOnce(t, r, ro)
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(rolloutsv1alpha1.GroupVersion.WithKind("CanaryRollout"))
	if err := env.Client.Get(context.Background(), key, u); err != nil {
		t.Fatal(err)
	}
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		if m, _ := c.(map[string]any); m["type"] == ConditionProgressing && m["reason"] == ReasonInvalidSpec {
			t.Fatalf("Progressing %v after a status the previous operator wrote (lastCheckAt to the second); want the rollout carried on", m)
		}
	}
}
