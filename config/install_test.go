// Package config_test checks the manifests in this directory with the kubectl a user would run.
package config_test

import (
	"os"
	"strings"
	"testing"

	"github.com/vivekdavara/canary/internal/testenv"
)

func TestMain(m *testing.M) { os.Exit(testenv.Main(m)) }

// README, "Run it": the CRD has to be applied with --server-side. A client-side apply copies the whole object into an
// annotation, and the CRD is bigger than annotations may be.
func TestTheCRDNeedsServerSideApply(t *testing.T) {
	env := testenv.Get(t)
	_, stderr, err := env.Kubectl("apply", "-f", "crd/bases")
	if err == nil {
		t.Fatal("a client-side apply of the CRD succeeded; README says it can't (and should say otherwise now)")
	}
	if !strings.Contains(stderr, "Too long") {
		t.Errorf("the client-side apply failed, but not on the annotation's size: %s", stderr)
	}
	stdout, stderr, err := env.Kubectl("apply", "--server-side", "-f", "crd/bases")
	if err != nil {
		t.Fatalf("kubectl apply --server-side: %v: %s", err, stderr)
	}
	if !strings.Contains(stdout, "canaryrollouts.rollouts.vivekdavara.github.io serverside-applied") {
		t.Errorf("kubectl apply --server-side said %q", stdout)
	}
}

// The sample applies with a plain kubectl apply, and kubectl get shows the CRD's columns.
func TestTheSampleAppliesWithKubectl(t *testing.T) {
	env := testenv.Get(t)
	ns := env.Namespace(t)
	stdout, stderr, err := env.Kubectl("apply", "-n", ns, "-f", "samples/web.yaml")
	if err != nil {
		t.Fatalf("kubectl apply: %v: %s", err, stderr)
	}
	if !strings.Contains(stdout, "canaryrollout.rollouts.vivekdavara.github.io/web created") {
		t.Errorf("kubectl apply said %q", stdout)
	}
	stdout, stderr, err = env.Kubectl("get", "crl", "-n", ns)
	if err != nil {
		t.Fatalf("kubectl get crl: %v: %s", err, stderr)
	}
	header := strings.Fields(strings.SplitN(stdout, "\n", 2)[0])
	want := []string{"NAME", "PHASE", "STEP", "CANARY", "%", "REVISION", "AGE"}
	if strings.Join(header, " ") != strings.Join(want, " ") {
		t.Errorf("kubectl get crl header %q; want %q", header, want)
	}
}
