package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	rolloutsv1alpha1 "github.com/vivekdavara/canary/api/v1alpha1"
	"github.com/vivekdavara/canary/internal/testenv"
)

// createRaw sends obj to the API server as JSON exactly as given (the typed client can't send a port beyond int32),
// and fails the test unless the server stores it.
func createRaw(t *testing.T, ns string, obj map[string]any) {
	t.Helper()
	env := testenv.Get(t)
	body, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	hc, err := rest.HTTPClientFor(env.Config)
	if err != nil {
		t.Fatal(err)
	}
	url := fmt.Sprintf("%s/apis/%s/namespaces/%s/canaryrollouts", strings.TrimSuffix(env.Config.Host, "/"), rolloutsv1alpha1.GroupVersion, ns)
	resp, err := hc.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("the API server answered %d, not 201: %.300s", resp.StatusCode, respBody)
	}
}

// undecodable is a rollout the API server accepts and the typed client can't decode: a readiness probe's port of
// 99999999999 (an int-or-string field the CRD's pod schema doesn't bound; DESIGN.md, "What the API server does not
// catch").
func undecodable(t *testing.T, ns string) map[string]any {
	t.Helper()
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(newRollout(ns, "example.com/web:1"))
	if err != nil {
		t.Fatal(err)
	}
	m["apiVersion"], m["kind"] = rolloutsv1alpha1.GroupVersion.String(), "CanaryRollout"
	c0 := m["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	c0["readinessProbe"] = map[string]any{"httpGet": map[string]any{"port": json.RawMessage("99999999999")}}
	return m
}

// Day 1's review: one rollout the typed client can't decode, in any namespace, stopped the operator from
// reconciling every rollout. Now the others carry on, and the bad one says what's wrong on its own status.
func TestOneUndecodableRolloutDoesNotStopTheOthers(t *testing.T) {
	c := startOperator(t)
	env := testenv.Get(t)
	ctx := context.Background()

	badNS := env.Namespace(t)
	createRaw(t, badNS, undecodable(t, badNS))
	// The typed client fails on it, as the operator's cache did.
	if err := c.Get(ctx, client.ObjectKey{Namespace: badNS, Name: "web"}, &rolloutsv1alpha1.CanaryRollout{}); err == nil {
		t.Fatal("the typed client decoded the bad rollout; the test no longer makes one it can't")
	}

	ro := newRollout(env.Namespace(t), "example.com/web:1")
	if err := c.Create(ctx, ro); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKeyFromObject(ro)
	eventually(t, c, key, func(r *rolloutsv1alpha1.CanaryRollout) bool { return r.Status.ObservedGeneration == 1 })
	edit(t, c, key, func(r *rolloutsv1alpha1.CanaryRollout) {
		r.Spec.Template.Spec.Containers[0].Image = "example.com/web:2"
	})
	eventually(t, c, key, func(r *rolloutsv1alpha1.CanaryRollout) bool { return r.Status.ObservedGeneration == 2 })

	cond := eventuallyCondition(t, c, client.ObjectKey{Namespace: badNS, Name: "web"}, func(st map[string]any, cond *metav1.Condition) bool {
		return cond != nil && cond.Reason == ReasonInvalidSpec
	})
	if cond.Status != metav1.ConditionFalse || !strings.Contains(cond.Message, "can't be decoded") || cond.ObservedGeneration != 1 {
		t.Errorf("the bad rollout's condition = %+v; want Progressing False, InvalidSpec, a decode message, generation 1", cond)
	}
}

// eventuallyCondition polls the rollout at key as unstructured (it may not decode) until ok accepts its status and
// Progressing condition, for up to 10 seconds.
func eventuallyCondition(t *testing.T, c client.Client, key client.ObjectKey, ok func(map[string]any, *metav1.Condition) bool) *metav1.Condition {
	t.Helper()
	var last map[string]any
	for range poll(t) {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(rolloutsv1alpha1.GroupVersion.WithKind("CanaryRollout"))
		if err := c.Get(context.Background(), key, u); err != nil {
			t.Fatal(err)
		}
		st, _ := u.Object["status"].(map[string]any)
		last = st
		var decoded struct {
			Conditions []metav1.Condition `json:"conditions"`
		}
		if st != nil {
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(st, &decoded); err != nil {
				t.Fatal(err)
			}
		}
		cond := meta.FindStatusCondition(decoded.Conditions, ConditionProgressing)
		if ok(st, cond) {
			return cond
		}
	}
	t.Fatalf("still not there after 10s: status %v", last)
	return nil
}
