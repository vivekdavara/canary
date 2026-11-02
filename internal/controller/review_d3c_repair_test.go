package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	rolloutsv1alpha1 "github.com/vivekdavara/canary/api/v1alpha1"
	"github.com/vivekdavara/canary/internal/testenv"
)

// reviewD3cStore creates an analysed rollout and stores lastCheckAt as given, past the typed client.
func reviewD3cStore(t *testing.T, stored string) (*CanaryRolloutReconciler, *rolloutsv1alpha1.CanaryRollout, *unstructured.Unstructured) {
	t.Helper()
	env := testenv.Get(t)
	r := direct(t, interceptor.Funcs{})
	now := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	r.Now = func() time.Time { return now }
	ns := env.Namespace(t)
	ro := analysedRollout(ns, time.Hour, 5)
	if err := env.Client.Create(context.Background(), ro); err != nil {
		t.Fatal(err)
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(rolloutsv1alpha1.GroupVersion.WithKind("CanaryRollout"))
	u.SetNamespace(ns)
	u.SetName(ro.Name)
	patch := fmt.Sprintf(`{"status":{"analysis":{"checks":1,"lastCheckAt":%q}}}`, stored)
	if err := env.Client.Status().Patch(context.Background(), u, client.RawPatch(types.MergePatchType, []byte(patch))); err != nil {
		t.Fatalf("the API server refused lastCheckAt %s: %v", stored, err)
	}
	return r, ro, u
}

// Review of day 3, third round (69325da). RFC 3339 (5.6, NOTE) allows a lower-case "t" and "z", and the CRD's
// date-time format accepts them, but neither of the repair's layouts parses them, so the repair removes the time as
// if it didn't parse: the instant is lost, and the next check is due an interval after the pause's start instead of
// after the last check.
func TestReviewD3cALowerCaseCheckTimeKeepsItsInstant(t *testing.T) {
	r, ro, u := reviewD3cStore(t, "2026-11-02t10:00:02.5z")
	reconcileOnce(t, r, ro)
	if err := testenv.Get(t).Client.Get(context.Background(), client.ObjectKeyFromObject(ro), u); err != nil {
		t.Fatal(err)
	}
	got, found, _ := unstructured.NestedString(u.Object, "status", "analysis", "lastCheckAt")
	if want := "2026-11-02T10:00:02.500000Z"; got != want {
		t.Fatalf("stored 2026-11-02t10:00:02.5z: lastCheckAt %q (found %v) after the repair; want %q", got, found, want)
	}
}

// Review of day 3, third round (69325da). A time the CRD accepts whose instant in UTC falls outside years 0000-9999
// ("9999-12-31T23:00:00-05:00" is 10000-01-01T04:00Z) is rewritten to a form the CRD's date-time format refuses
// ("10000-01-01T04:00:00.000000Z"), so the patch fails with 422 at every reconcile: the repair never converges, the
// reconcile errors forever (backoff), and the rollout is never even reported InvalidSpec, as it was before the repair.
func TestReviewD3cARepairTheAPIServerRefusesConverges(t *testing.T) {
	r, ro, _ := reviewD3cStore(t, "9999-12-31T23:00:00-05:00")
	for i := range 3 {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ro)}); err != nil {
			t.Fatalf("reconcile %d: %v", i+1, err)
		}
	}
}
