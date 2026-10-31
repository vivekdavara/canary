// Command canary runs the CanaryRollout operator: one controller-runtime manager watching CanaryRollouts and the
// Deployments they own. DESIGN.md is the specification.
package main

import (
	"flag"
	"fmt"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	rolloutsv1alpha1 "github.com/vivekdavara/canary/api/v1alpha1"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "canary:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("canary", flag.ContinueOnError)
	// Ports from the portfolio's port map: metrics on 8111; the probes on 8121, which nothing else here uses.
	metricsAddr := fs.String("metrics-bind-address", ":8111", "where the Prometheus metrics are served; 0 turns them off")
	probeAddr := fs.String("health-probe-bind-address", ":8121", "where /healthz and /readyz are served")
	leaderElect := fs.Bool("leader-elect", false, "elect a leader, so only one of several replicas reconciles")
	zapOpts := zap.Options{}
	zapOpts.BindFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))

	scheme, err := newScheme()
	if err != nil {
		return err
	}
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("no Kubernetes configuration: %w", err)
	}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: *metricsAddr},
		HealthProbeBindAddress: *probeAddr,
		LeaderElection:         *leaderElect,
		LeaderElectionID:       "canary.rollouts.vivekdavara.github.io",
	})
	if err != nil {
		return fmt.Errorf("creating the manager: %w", err)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}
	ctrl.Log.WithName("canary").Info("starting", "metrics", *metricsAddr, "probes", *probeAddr)
	return mgr.Start(ctrl.SetupSignalHandler())
}

// newScheme holds every type the manager reads or writes: the built-in ones (Deployments, Events) and CanaryRollout.
func newScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := rolloutsv1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	return scheme, nil
}
