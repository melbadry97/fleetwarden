// Command manager runs the fleetwarden controller manager.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/prometheus/client_golang/prometheus"

	fleetv1alpha1 "github.com/melbadry97/fleetwarden/api/v1alpha1"
	"github.com/melbadry97/fleetwarden/controllers"
	"github.com/melbadry97/fleetwarden/internal/audit"
	"github.com/melbadry97/fleetwarden/internal/budget"
	"github.com/melbadry97/fleetwarden/internal/fleet"
	"github.com/melbadry97/fleetwarden/internal/policy"
	"github.com/melbadry97/fleetwarden/internal/signal"
	validating "github.com/melbadry97/fleetwarden/webhooks/validating"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

// Metrics from the design document's observability section.
var (
	nodesByState = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "fleetwarden_nodes_by_state",
		Help: "Number of nodes in each lifecycle state, by class.",
	}, []string{"class", "state"})

	budgetConsumed = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "fleetwarden_budget_consumed_ratio",
		Help: "Fraction of the disruption budget currently consumed, by class. Primary alerting signal.",
	}, []string{"class"})

	intentsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "fleetwarden_intents_total",
		Help: "Disruption intents by origin and admission decision.",
	}, []string{"origin", "decision"})

	signalToAction = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "fleetwarden_signal_to_action_seconds",
		Help:    "Latency from first signal breach to applied action.",
		Buckets: prometheus.ExponentialBuckets(1, 2, 12),
	}, []string{"class", "action"})
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(fleetv1alpha1.AddToScheme(scheme))

	metrics.Registry.MustRegister(nodesByState, budgetConsumed, intentsTotal, signalToAction)
}

func main() {
	var (
		metricsAddr    string
		probeAddr      string
		enableLeader   bool
		enableWebhooks bool
		staleTimeout   time.Duration
		retention      time.Duration
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "metrics endpoint address")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "health probe address")
	flag.BoolVar(&enableLeader, "leader-elect", false,
		"enable leader election; required for HA, since two active managers would double-spend the budget")
	flag.BoolVar(&enableWebhooks, "enable-webhooks", true, "serve admission webhooks")
	flag.DurationVar(&staleTimeout, "budget-stale-timeout", 30*time.Minute,
		"how long an in-flight budget slot survives without release before being reclaimed")
	flag.DurationVar(&retention, "signal-retention", time.Hour,
		"how long signal observations are retained for windowed evaluation")

	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeader,
		LeaderElectionID:       "fleetwarden.fleet.elbadry.dev",
	})
	if err != nil {
		setupLog.Error(err, "unable to build manager")
		os.Exit(1)
	}

	// The accountant is constructed once and shared, because a per-reconciler
	// budget would not be fleet-wide and therefore would not be a budget.
	accountant := budget.New(
		map[fleet.ClassName]budget.Quota{},
		budget.WithStaleTimeout(staleTimeout),
	)

	auditLog := audit.NewLog(
		audit.WithSink(audit.SinkFunc(func(r audit.Record) {
			intentsTotal.WithLabelValues(string(r.Origin), string(r.Outcome)).Inc()
			setupLog.Info("audit",
				"seq", r.Seq, "node", r.Node, "action", r.Action,
				"decision", r.Decision, "outcome", r.Outcome, "hash", r.Hash[:8])
		})),
	)

	reconciler := &controllers.NodeHealthReconciler{
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		Recorder:   mgr.GetEventRecorderFor("fleetwarden"),
		Accountant: accountant,
		Audit:      auditLog,
		Evaluator:  signal.NewEvaluator(retention),
		Policy:     policy.New(),
		Sources:    buildSources(mgr),
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to register NodeHealth controller")
		os.Exit(1)
	}

	if enableWebhooks {
		if err := (&validating.PolicyValidator{}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to register policy webhook")
			os.Exit(1)
		}
		if err := (&validating.ClassValidator{}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to register class webhook")
			os.Exit(1)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to add health check")
		os.Exit(1)
	}
	// Readiness additionally verifies the audit chain: a manager that cannot
	// produce a verifiable audit trail should not be taking disruptive actions.
	if err := mgr.AddReadyzCheck("readyz", func(_ *http.Request) error {
		return auditLog.Verify()
	}); err != nil {
		setupLog.Error(err, "unable to add ready check")
		os.Exit(1)
	}

	setupLog.Info("starting fleetwarden",
		"leaderElection", enableLeader, "webhooks", enableWebhooks)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager exited with error")
		os.Exit(1)
	}
}

// buildSources wires the signal source implementations. NodeCondition and
// NodeLabel read from the cached client; Prometheus and OutOfBand are
// configured from environment.
func buildSources(mgr ctrl.Manager) map[signal.Kind]signal.Source {
	srcs := map[signal.Kind]signal.Source{
		signal.KindNodeCondition: &nodeConditionSource{mgr: mgr},
		signal.KindNodeLabel:     &nodeLabelSource{mgr: mgr},
	}
	if addr := os.Getenv("PROMETHEUS_URL"); addr != "" {
		srcs[signal.KindPrometheus] = &prometheusSource{addr: addr, timeout: 10 * time.Second}
	}
	if addr := os.Getenv("REDFISH_BROKER_URL"); addr != "" {
		srcs[signal.KindOutOfBand] = &oobSource{broker: addr, timeout: 15 * time.Second}
	}
	return srcs
}

// --- source implementations -------------------------------------------------

type nodeConditionSource struct{ mgr ctrl.Manager }

func (s *nodeConditionSource) Fetch(spec signal.Spec) ([]signal.Sample, error) {
	var list corev1.NodeList
	if err := s.mgr.GetClient().List(context.Background(), &list); err != nil {
		return nil, err
	}
	now := time.Now()
	out := make([]signal.Sample, 0, len(list.Items))
	for i := range list.Items {
		n := &list.Items[i]
		for _, c := range n.Status.Conditions {
			if string(c.Type) == spec.Key {
				out = append(out, signal.Sample{
					Node: fleet.NodeID(n.Name), Value: string(c.Status), At: now,
				})
			}
		}
	}
	return out, nil
}

type nodeLabelSource struct{ mgr ctrl.Manager }

func (s *nodeLabelSource) Fetch(spec signal.Spec) ([]signal.Sample, error) {
	var list corev1.NodeList
	if err := s.mgr.GetClient().List(context.Background(), &list); err != nil {
		return nil, err
	}
	now := time.Now()
	out := make([]signal.Sample, 0, len(list.Items))
	for i := range list.Items {
		n := &list.Items[i]
		v, ok := n.Labels[spec.Key]
		if !ok {
			// A missing label is drift from the baseline, not an absent
			// observation. Reporting it as empty lets NotEquals fire.
			v = ""
		}
		out = append(out, signal.Sample{Node: fleet.NodeID(n.Name), Value: v, At: now})
	}
	return out, nil
}

type prometheusSource struct {
	addr    string
	timeout time.Duration
}

func (s *prometheusSource) Fetch(spec signal.Spec) ([]signal.Sample, error) {
	// Implemented against the Prometheus HTTP API; see docs/RUNBOOK.md for the
	// expected label convention (every series must carry a `node` label).
	return nil, fmt.Errorf("prometheus source: not implemented in the M1 slice (query=%q)", spec.Query)
}

type oobSource struct {
	broker  string
	timeout time.Duration
}

func (s *oobSource) Fetch(spec signal.Spec) ([]signal.Sample, error) {
	// Talks to the OOB broker, never directly to BMCs, and never holds raw
	// credentials. See design doc section 8.3.
	return nil, fmt.Errorf("out-of-band source: not implemented in the M1 slice (probe=%q)", spec.Key)
}
