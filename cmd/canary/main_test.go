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

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	rolloutsv1alpha1 "github.com/vivekdavara/canary/api/v1alpha1"
	"github.com/vivekdavara/canary/internal/testenv"
)

func TestMain(m *testing.M) {
	skipNameValidation = true
	os.Exit(testenv.Main(m))
}

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

// httpClient has a timeout: the manager binds its ports when it is created, before it serves them, so a request can
// connect and then wait for an answer that never comes.
var httpClient = &http.Client{Timeout: 2 * time.Second}

func get(url string) (int, string, error) {
	resp, err := httpClient.Get(url)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), err
}

// operator is the binary's run() going in the background.
type operator struct {
	metrics, probes string
	cancel          context.CancelFunc
	done            chan error
	stopped         bool
}

// startRun starts run() as the operator's user, from a kubeconfig file, with the probes and metrics on free loopback
// ports and the given extra flags, and waits until /readyz answers. The test's end stops it if the test hasn't.
func startRun(t *testing.T, extra ...string) *operator {
	t.Helper()
	env := testenv.Get(t)
	op := &operator{metrics: freeAddr(t), probes: freeAddr(t), done: make(chan error, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	op.cancel = cancel
	args := append([]string{
		"--kubeconfig", env.OperatorKubeconfig(t),
		"--metrics-bind-address", op.metrics,
		"--health-probe-bind-address", op.probes,
	}, extra...)
	go func() { op.done <- run(ctx, args) }()
	t.Cleanup(func() {
		if !op.stopped {
			cancel()
			<-op.done
		}
	})
	deadline := time.Now().Add(15 * time.Second)
	for {
		code, _, err := get("http://" + op.probes + "/readyz")
		if err == nil && code == http.StatusOK {
			return op
		}
		select {
		case err := <-op.done:
			op.stopped = true
			t.Fatalf("run returned early: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("/readyz: %d, %v after 15s", code, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// stop cancels run() and returns what it returned.
func (op *operator) stop(t *testing.T) error {
	t.Helper()
	op.cancel()
	op.stopped = true
	select {
	case err := <-op.done:
		return err
	case <-time.After(30 * time.Second):
		t.Fatal("run didn't return within 30s of cancel")
		return nil
	}
}

// createAndAwait creates a small rollout in ns and waits until the operator has reconciled it.
func createAndAwait(t *testing.T, ns string) {
	t.Helper()
	env := testenv.Get(t)
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
	deadline := time.Now().Add(15 * time.Second)
	for {
		var got rolloutsv1alpha1.CanaryRollout
		err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(ro), &got)
		if err == nil && got.Status.ObservedGeneration == 1 && got.Status.UpdateRevision != "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("not reconciled after 15s: %v, status %+v", err, got.Status)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// The binary's own wiring, which the controller's tests don't see: flags, the scheme, the controller registered, the
// probes and metrics served, a clean stop on cancel.
func TestRunServesAndReconciles(t *testing.T) {
	op := startRun(t)
	if code, _, err := get("http://" + op.probes + "/healthz"); err != nil || code != http.StatusOK {
		t.Errorf("/healthz: %d, %v; want 200", code, err)
	}
	createAndAwait(t, testenv.Get(t).Namespace(t))

	// The reconcile is counted, under the controller's name.
	code, body, err := get("http://" + op.metrics + "/metrics")
	if err != nil || code != http.StatusOK {
		t.Fatalf("/metrics: %d, %v", code, err)
	}
	if want := `controller_runtime_reconcile_total{controller="canaryrollout",result="success"}`; !strings.Contains(body, want) {
		t.Errorf("/metrics has no %s", want)
	}
	if err := op.stop(t); err != nil {
		t.Errorf("run returned %v after cancel; want nil", err)
	}
}

// With --leader-elect the operator takes the Lease in the namespace given, with only the permissions of
// config/rbac/leader_election_role.yaml there, and reconciles once it leads.
func TestRunElectsALeader(t *testing.T) {
	env := testenv.Get(t)
	lockNS := env.Namespace(t)
	env.BindLeaderElection(t, lockNS)
	op := startRun(t, "--leader-elect", "--leader-election-namespace", lockNS)
	createAndAwait(t, env.Namespace(t))

	var lease coordinationv1.Lease
	key := client.ObjectKey{Namespace: lockNS, Name: "canary.rollouts.vivekdavara.github.io"}
	if err := env.Client.Get(context.Background(), key, &lease); err != nil {
		t.Fatalf("the Lease: %v", err)
	}
	if h := lease.Spec.HolderIdentity; h == nil || *h == "" {
		t.Errorf("the Lease has no holder: %+v", lease.Spec)
	}
	if err := op.stop(t); err != nil {
		t.Errorf("run returned %v after cancel; want nil", err)
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
