package charts_test

import (
	"strings"
	"testing"

	"github.com/vivekdavara/canary/internal/testenv"
)

// Review (day 5): the chart refuses replicaCount above 1 without leader election because "every replica reconciles,
// and they overwrite each other's decisions". With replicaCount=1 and leaderElection.enabled=false it renders no
// strategy, so the API server defaults it to RollingUpdate with maxSurge 25% (rounded up: 1 pod) and maxUnavailable
// 25% (rounded down: 0): every helm upgrade that changes the pod template (a new image, a new prometheus.url) starts
// the new operator and waits for it to be ready before stopping the old one, so for that window two operators
// without leader election both reconcile, the case the chart's own fail message forbids. It needs
// strategy Recreate (or maxSurge 0) when leader election is off. (envtest runs no Deployment controller, so this
// shows the stored strategy, not the overlap itself.) Fixed: the chart sets strategy Recreate without leader election;
// the Deployment is named after the release since the naming fix (it was solo-canary).
func TestReviewD5WithoutLeaderElectionAnUpgradeRunsTwoOperators(t *testing.T) {
	env := testenv.Get(t)
	kubeconfig := env.AdminKubeconfig(t)
	ns := env.Namespace(t)
	if _, stderr, err := helm(t, "install", "solo", "./canary", "-n", ns, "--kubeconfig", kubeconfig,
		"--set", "replicaCount=1", "--set", "leaderElection.enabled=false"); err != nil {
		t.Fatalf("helm install: %v: %s", err, stderr)
	}
	t.Cleanup(func() { helm(t, "uninstall", "solo", "-n", ns, "--kubeconfig", kubeconfig) })
	stdout, stderr, err := env.Kubectl("get", "deployment", "solo", "-n", ns, "-o",
		"jsonpath={.spec.strategy.type} {.spec.strategy.rollingUpdate.maxSurge}")
	if err != nil {
		t.Fatalf("kubectl get: %v: %s", err, stderr)
	}
	if typ, surge, _ := strings.Cut(stdout, " "); typ != "Recreate" && surge != "0" {
		t.Errorf("one replica without leader election, strategy %s with maxSurge %s: an upgrade runs the old and the new "+
			"operator at once, both reconciling", typ, surge)
	}
}
