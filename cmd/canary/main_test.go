package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	rolloutsv1alpha1 "github.com/vivekdavara/canary/api/v1alpha1"
	"github.com/vivekdavara/canary/internal/testenv"
)

func TestMain(m *testing.M) { os.Exit(testenv.Main(m)) }

// freeAddr is a loopback address with a port nothing listened on a moment ago.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func get(url string) (int, string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), err
}

// The binary's own wiring, which the controller's tests don't see: flags, the scheme, the controller registered, the
// probes and metrics served, a clean stop on cancel. It runs as the operator's user, from a kubeconfig file.
func TestRunServesAndReconciles(t *testing.T) {
	env := testenv.Get(t)
	metrics, probes := freeAddr(t), freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{
			"--kubeconfig", env.OperatorKubeconfig(t),
			"--metrics-bind-address", metrics,
			"--health-probe-bind-address", probes,
		})
	}()
	stopped := false
	defer func() {
		if !stopped {
			cancel()
			<-done
		}
	}()

	deadline := time.Now().Add(15 * time.Second)
	for {
		code, _, err := get("http://" + probes + "/readyz")
		if err == nil && code == http.StatusOK {
			break
		}
		select {
		case err := <-done:
			stopped = true
			t.Fatalf("run returned early: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("/readyz: %d, %v after 15s", code, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if code, _, err := get("http://" + probes + "/healthz"); err != nil || code != http.StatusOK {
		t.Errorf("/healthz: %d, %v; want 200", code, err)
	}

	ns := env.Namespace(t)
	ro := &rolloutsv1alpha1.CanaryRollout{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "web"},
		Spec: rolloutsv1alpha1.CanaryRolloutSpec{
			Replicas: 2,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "web"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: "example.com/web:1"}}},
			},
			Steps: []rolloutsv1alpha1.Step{{Weight: 50}},
		},
	}
	if err := env.Client.Create(context.Background(), ro); err != nil {
		t.Fatal(err)
	}
	for {
		var got rolloutsv1alpha1.CanaryRollout
		err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(ro), &got)
		if err == nil && got.Status.ObservedGeneration == 1 && got.Status.UpdateRevision != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not reconciled after 15s: %v, status %+v", err, got.Status)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// The reconcile is counted, under the controller's name.
	code, body, err := get("http://" + metrics + "/metrics")
	if err != nil || code != http.StatusOK {
		t.Fatalf("/metrics: %d, %v", code, err)
	}
	if want := `controller_runtime_reconcile_total{controller="canaryrollout",result="success"}`; !strings.Contains(body, want) {
		t.Errorf("/metrics has no %s", want)
	}

	cancel()
	stopped = true
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run returned %v after cancel; want nil", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("run didn't return within 30s of cancel")
	}
}

func TestRunRefusesUnknownFlags(t *testing.T) {
	err := run(context.Background(), []string{"--no-such-flag"})
	if err == nil || !strings.Contains(err.Error(), "no-such-flag") {
		t.Errorf("run = %v; want an error naming the flag", err)
	}
}

func TestRunHelpIsNotAFailure(t *testing.T) {
	if err := run(context.Background(), []string{"--help"}); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("run(--help) = %v; want flag.ErrHelp, which main exits 0 on", err)
	}
}
