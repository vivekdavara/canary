package charts_test

// Review (day 5): values.schema.json accepts each of these values, helm renders them, and the operator started
// with the rendered Deployment's arguments exits at once (in a cluster: a crash loop). DESIGN.md says the schema
// refuses a Prometheus URL "the operator would refuse at start", and chart_test.go says "Values that would make an
// operator that can't work are refused at install"; these show both are only partly true: a URL that url.Parse
// refuses (a typo in the port, a space, an unclosed IPv6 bracket, a bad escape), a timeout of 0s or one that
// overflows time.Duration, and metricsPort equal to probesPort all pass the schema. The control case (the defaults)
// stays up, so the harness itself works.

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vivekdavara/canary/internal/testenv"
)

func reviewFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// reviewRunOperator starts the operator binary with args for up to d and returns whether it was still running.
func reviewRunOperator(t *testing.T, bin string, args []string, d time.Duration) (bool, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	out, _ := cmd.CombinedOutput()
	return ctx.Err() != nil, string(out)
}

// Fixed: the schema's URL pattern is now a subset of what url.Parse accepts (a host name or a bracketed IPv6 address,
// a port of digits, a path of unreserved characters and well-formed %-escapes), the duration pattern refuses zero
// and keeps each number to five digits (at most seven of them, inside time.Duration), and equal ports fail the
// render. So each case is now refused at render, and TestValuesTheSchemaAcceptsStartTheOperator checks the other
// direction: values the schema accepts start the operator. (The control case moved there.)
func TestReviewD5SchemaAcceptsValuesTheOperatorRefuses(t *testing.T) {
	for _, c := range []struct {
		name string
		set  []string
		want string
	}{
		{"a letter O in the port", []string{"prometheus.url=http://prometheus.monitoring.svc:9O90"}, "at '/prometheus/url'"},
		{"a space in the host", []string{"prometheus.url=http://prometheus monitoring.svc:9090"}, "at '/prometheus/url'"},
		{"an unclosed IPv6 bracket", []string{"prometheus.url=http://[fd00::1:9090"}, "at '/prometheus/url'"},
		{"a bad escape in the path", []string{"prometheus.url=http://prometheus:9090/%zz"}, "at '/prometheus/url'"},
		{"a zero timeout", []string{"analysisTimeout=0s"}, "at '/analysisTimeout'"},
		{"a timeout over time.Duration's range", []string{"analysisTimeout=9999999h"}, "at '/analysisTimeout'"},
		{"the same port twice", []string{"metricsPort=9000", "probesPort=9000"}, "metricsPort and probesPort must differ"},
	} {
		args := []string{"template", "canary", "./canary"}
		for _, s := range c.set {
			args = append(args, "--set", s)
		}
		if _, stderr, err := helm(t, args...); err == nil {
			t.Errorf("%s (--set %v): rendered; the operator would exit at start", c.name, c.set)
		} else if !strings.Contains(stderr, c.want) {
			t.Errorf("%s (--set %v): refused without %q: %s", c.name, c.set, c.want, lastLines(stderr, 2))
		}
	}
}

// The other direction: values the schema accepts, the defaults and some unusual ones, give arguments the operator
// starts with (against envtest, for 3 seconds: flags are checked, and the ports bound, at once).
func TestValuesTheSchemaAcceptsStartTheOperator(t *testing.T) {
	env := testenv.Get(t)
	kubeconfig := env.AdminKubeconfig(t)
	bin := filepath.Join(t.TempDir(), "canary")
	if out, err := exec.Command("go", "build", "-o", bin, "github.com/vivekdavara/canary/cmd/canary").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v: %s", err, out)
	}
	for _, c := range []struct {
		name string
		set  []string
	}{
		{"the defaults", nil},
		{"a service URL with a port", []string{"prometheus.url=http://prometheus.monitoring.svc.cluster.local:9090"}},
		{"https with a path prefix", []string{"prometheus.url=https://monitoring.example.com/prometheus/"}},
		{"a bracketed IPv6 address", []string{"prometheus.url=http://[fd00::1]:9090"}},
		{"an escape in the path", []string{"prometheus.url=http://p:9090/a%2Fb"}},
		{"a timeout in two units", []string{"analysisTimeout=1m30s"}},
		{"a fractional timeout", []string{"analysisTimeout=0.5s"}},
		{"a timeout with a microsecond part", []string{"analysisTimeout=1s25000µs"}},
		{"the largest timeout the pattern allows", []string{"analysisTimeout=99999h99999h99999h99999h99999h99999h99999h"}},
		{"one replica without leader election", []string{"replicaCount=1", "leaderElection.enabled=false"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			set := []string{"--set", fmt.Sprintf("metricsPort=%d", reviewFreePort(t)), "--set", fmt.Sprintf("probesPort=%d", reviewFreePort(t))}
			for _, s := range c.set {
				set = append(set, "--set", s)
			}
			dep := byKey(render(t, "canary", set...))["Deployment/canary-system/canary"]
			var args []string
			for _, a := range field(t, dep, "spec", "template", "spec", "containers").([]any)[0].(map[string]any)["args"].([]any) {
				args = append(args, a.(string))
			}
			chartArgs := args
			args = append(args, "--kubeconfig", kubeconfig, "--leader-election-namespace", env.Namespace(t))
			if up, out := reviewRunOperator(t, bin, args, 3*time.Second); !up {
				t.Errorf("the schema accepted --set %v, and the operator exited with the chart's arguments %v:\n%s",
					c.set, chartArgs, lastLines(out, 3))
			}
		})
	}
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
