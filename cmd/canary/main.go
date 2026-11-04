// Command canary runs the CanaryRollout operator: one controller-runtime manager with one controller, watching
// CanaryRollouts and the Deployments they own. DESIGN.md is the specification.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	rolloutsv1alpha1 "github.com/vivekdavara/canary/api/v1alpha1"
	"github.com/vivekdavara/canary/internal/analysis"
	"github.com/vivekdavara/canary/internal/controller"
)

// skipNameValidation is set by tests, which call run more than once in one process: controller-runtime refuses a
// second controller of the same name in a process (their metrics would mix).
var skipNameValidation = false

func main() {
	err := run(ctrl.SetupSignalHandler(), os.Args[1:])
	switch {
	case err == nil:
	case errors.Is(err, flag.ErrHelp):
		// -h or --help: the flag package has printed the usage.
	default:
		fmt.Fprintln(os.Stderr, "canary:", err)
		os.Exit(1)
	}
}

// run runs the operator until ctx is done (SIGTERM or SIGINT, from main).
func run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("canary", flag.ContinueOnError)
	// --kubeconfig; without it, $KUBECONFIG, then the in-cluster service account, then ~/.kube/config.
	ctrl.RegisterFlags(fs)
	// Ports from the portfolio's port map: metrics on 8111; the probes on 8121, which nothing else here uses.
	metricsAddr := fs.String("metrics-bind-address", ":8111", "where the Prometheus metrics are served; 0 turns them off")
	probeAddr := fs.String("health-probe-bind-address", ":8121", "where /healthz and /readyz are served")
	leaderElect := fs.Bool("leader-elect", false, "elect a leader, so only one of several replicas reconciles")
	leaderNamespace := fs.String("leader-election-namespace", "",
		"the namespace of the leader's Lease; in a cluster, the operator's own by default (outside one, required)")
	prometheusURL := fs.String("prometheus-url", "",
		"the Prometheus that answers rollouts' analysis queries (http:// or https://); without it, every check fails")
	analysisTimeout := fs.Duration("analysis-timeout", controller.DefaultAnalysisTimeout, "how long one analysis query may take")
	zapOpts := zap.Options{}
	zapOpts.BindFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))
	if *analysisTimeout <= 0 {
		return fmt.Errorf("--analysis-timeout must be positive, not %v", *analysisTimeout)
	}
	// A rollout's analysis goes only to this address, never one a rollout names (DESIGN.md, "The pieces").
	var querier analysis.Querier
	if *prometheusURL != "" {
		p, err := analysis.NewPrometheus(*prometheusURL, &http.Client{})
		if err != nil {
			return fmt.Errorf("--prometheus-url: %w", err)
		}
		querier = p
	}

	scheme, err := newScheme()
	if err != nil {
		return err
	}
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("no Kubernetes configuration: %w", err)
	}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: *metricsAddr},
		HealthProbeBindAddress:  *probeAddr,
		LeaderElection:          *leaderElect,
		LeaderElectionID:        "canary.rollouts.vivekdavara.github.io",
		LeaderElectionNamespace: *leaderNamespace,
		// A stopped leader (an upgrade, a drain: SIGTERM) gives the Lease up once its reconciles have ended, so another
		// replica leads at once rather than after the Lease's 15 seconds. Safe because run returns, and the process
		// ends, as soon as the manager has stopped: nothing reconciles after the release.
		LeaderElectionReleaseOnCancel: true,
		Controller:                    config.Controller{SkipNameValidation: &skipNameValidation},
	})
	if err != nil {
		return fmt.Errorf("creating the manager: %w", err)
	}
	reconciler := controller.New(mgr)
	reconciler.Querier, reconciler.AnalysisTimeout = querier, *analysisTimeout
	if err := reconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setting up the controller: %w", err)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}
	ctrl.Log.WithName("canary").Info("starting", "metrics", *metricsAddr, "probes", *probeAddr, "prometheus", *prometheusURL)
	if querier == nil {
		ctrl.Log.WithName("canary").Info("no --prometheus-url: rollouts with analysis will fail their checks and roll back")
	}
	return mgr.Start(ctx)
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
