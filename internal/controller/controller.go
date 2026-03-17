package controller

import (
	"context"
	"fmt"
	"log"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	bgpv1alpha1 "go.miloapis.com/bgp/api/v1alpha1"
)

// ControllerOptions holds runtime configuration for the BGP CRD controller.
type ControllerOptions struct {
	// LocalEndpoint is the name of the BGPEndpoint resource representing this instance.
	// Used for router ID resolution, next-hop address lookup, and session ownership.
	LocalEndpoint string

	// SRv6Net is the node's SRv6 prefix (e.g. a /48).
	// The route watcher skips routes matching this prefix to avoid self-routing.
	SRv6Net string

	// GoBGPAddr is the gRPC address of the local GoBGP sidecar (e.g. "127.0.0.1:50051").
	// Defaults to "127.0.0.1:50051" when empty.
	GoBGPAddr string

	// MetricsAddr is the address to serve Prometheus metrics on (e.g. ":8082").
	// Defaults to ":8082" when empty.
	MetricsAddr string

	// HealthAddr is the address to serve health/readiness probes on (e.g. ":8083").
	// Defaults to ":8083" when empty.
	HealthAddr string

	// LeaderElectionNamespace is the namespace used for leader election ConfigMap/Lease.
	// Defaults to "bgp-system" when empty.
	LeaderElectionNamespace string
}

// scheme holds the runtime.Scheme for all types used by the manager.
var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(bgpv1alpha1.AddToScheme(scheme))
}

// Run starts the BGP CRD controller and blocks until ctx is cancelled.
//
// Two controller-runtime managers are used:
//
//  1. Per-node manager (no leader election): runs ConfigReconciler,
//     SessionReconciler, AdvertisementReconciler, and RoutePolicyReconciler.
//     Every DaemonSet pod runs these reconcilers because each node must
//     independently configure its local GoBGP sidecar.
//
//  2. Leader-elected manager: runs PeeringPolicyReconciler exclusively.
//     Only one pod at a time creates/deletes BGPSession cluster-scoped
//     resources.  Without this, every DaemonSet pod would race to create
//     and garbage-collect the same sessions, producing flapping state.
func Run(ctx context.Context, opts ControllerOptions, routeWatcher func(ctx context.Context, gobgp *GoBGPClient, srv6Net string)) error {
	if opts.GoBGPAddr == "" {
		opts.GoBGPAddr = gobgpDefaultAddr
	}
	if opts.MetricsAddr == "" {
		opts.MetricsAddr = ":8082"
	}
	if opts.HealthAddr == "" {
		opts.HealthAddr = ":8083"
	}
	if opts.LeaderElectionNamespace == "" {
		opts.LeaderElectionNamespace = "bgp-system"
	}

	// Connect to GoBGP first — reconcilers depend on this connection.
	gobgp := NewGoBGPClientWithAddr(opts.GoBGPAddr)
	if err := gobgp.Connect(ctx); err != nil {
		return fmt.Errorf("connect GoBGP: %w", err)
	}
	log.Printf("bgp/controller: GoBGP connected")

	// Build the REST config used by both managers.
	restCfg, err := config.GetConfig()
	if err != nil {
		return fmt.Errorf("get k8s config: %w", err)
	}

	// ── Per-node manager ────────────────────────────────────────────────────
	// Runs on every DaemonSet pod. Configures the local GoBGP sidecar.
	// Metrics and health probes are served only from this manager.
	nodeMgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: opts.HealthAddr,
		Metrics:                metricsserver.Options{BindAddress: opts.MetricsAddr},
	})
	if err != nil {
		return fmt.Errorf("new per-node manager: %w", err)
	}

	// BGPConfiguration: reads global config, applies to GoBGP global settings.
	if err := (&ConfigReconciler{
		Client:        nodeMgr.GetClient(),
		GoBGP:         gobgp,
		LocalEndpoint: opts.LocalEndpoint,
	}).SetupWithManager(nodeMgr); err != nil {
		return fmt.Errorf("setup BGPConfiguration reconciler: %w", err)
	}

	// BGPSession: each node configures its own GoBGP peer for sessions that
	// involve its LocalEndpoint.
	if err := (&SessionReconciler{
		Client:        nodeMgr.GetClient(),
		GoBGP:         gobgp,
		LocalEndpoint: opts.LocalEndpoint,
	}).SetupWithManager(nodeMgr); err != nil {
		return fmt.Errorf("setup BGPSession reconciler: %w", err)
	}

	// BGPAdvertisement: each node advertises prefixes into its local GoBGP.
	if err := (&AdvertisementReconciler{
		Client:        nodeMgr.GetClient(),
		GoBGP:         gobgp,
		LocalEndpoint: opts.LocalEndpoint,
	}).SetupWithManager(nodeMgr); err != nil {
		return fmt.Errorf("setup BGPAdvertisement reconciler: %w", err)
	}

	// BGPRoutePolicy: each node programs route policies into its local GoBGP.
	if err := (&RoutePolicyReconciler{
		Client: nodeMgr.GetClient(),
		GoBGP:  gobgp,
	}).SetupWithManager(nodeMgr); err != nil {
		return fmt.Errorf("setup BGPRoutePolicy reconciler: %w", err)
	}

	// Health and readiness probes.
	if err := nodeMgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("add healthz check: %w", err)
	}
	if err := nodeMgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("add readyz check: %w", err)
	}

	// ── Leader-elected manager ──────────────────────────────────────────────
	// Only the elected leader runs the PeeringPolicyReconciler, which creates
	// and garbage-collects cluster-scoped BGPSession resources.  Running this
	// on every node would cause N pods to race over the same sessions.
	//
	// Metrics and health are disabled on this manager (served by nodeMgr).
	leaderMgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme:                        scheme,
		LeaderElection:                true,
		LeaderElectionID:              "bgp-operator-leader",
		LeaderElectionNamespace:       opts.LeaderElectionNamespace,
		LeaderElectionReleaseOnCancel: true,
		Metrics:                       metricsserver.Options{BindAddress: "0"}, // disabled
		HealthProbeBindAddress:        "0",                                     // disabled
	})
	if err != nil {
		return fmt.Errorf("new leader manager: %w", err)
	}

	// BGPPeeringPolicy: creates/deletes BGPSession objects for matching endpoint pairs.
	if err := (&PeeringPolicyReconciler{
		Client: leaderMgr.GetClient(),
	}).SetupWithManager(leaderMgr); err != nil {
		return fmt.Errorf("setup BGPPeeringPolicy reconciler: %w", err)
	}

	// Start background goroutines shared across both managers.
	// The status poller and route watcher use the per-node client.
	go gobgp.WatchHealth(ctx, nodeMgr.GetClient())
	go RunStatusPoller(ctx, nodeMgr.GetClient(), gobgp)
	if routeWatcher != nil {
		go routeWatcher(ctx, gobgp, opts.SRv6Net)
	}

	// Start both managers concurrently. Either one failing cancels the context
	// via the errgroup pattern below. We use a simple goroutine + channel here
	// to avoid importing golang.org/x/sync.
	errCh := make(chan error, 2)

	go func() {
		log.Printf("bgp/controller: starting leader-elected manager")
		errCh <- leaderMgr.Start(ctx)
	}()

	go func() {
		log.Printf("bgp/controller: starting per-node manager")
		errCh <- nodeMgr.Start(ctx)
	}()

	// Wait for the first manager to exit (either error or context cancellation).
	return <-errCh
}
