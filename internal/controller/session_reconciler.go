package controller

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gobgpapi "github.com/osrg/gobgp/v3/api"

	bgpv1alpha1 "go.miloapis.com/bgp/api/v1alpha1"
)

const (
	// SessionFinalizer is the finalizer added to BGPSession resources to ensure
	// GoBGP DeletePeer is called before the resource is removed from etcd.
	SessionFinalizer = "bgp.miloapis.com/session-cleanup"

	// sessionNotEstablishedRequeue is how long to wait before re-checking peer
	// state when a session is not yet Established. Once Established, no requeue
	// is needed — BGPEndpoint and BGPSession watches drive re-reconciliation.
	sessionNotEstablishedRequeue = 10 * time.Second
)

// SessionReconciler reconciles BGPSession resources into GoBGP AddPeer/DeletePeer calls.
// Each node's BGP agent reconciles ALL BGPSession resources — not just sessions involving
// its own endpoint — because iBGP full mesh requires every node to know about every peer.
type SessionReconciler struct {
	client.Client
	GoBGP         *GoBGPClient
	LocalEndpoint string

	// lastGoBGPAddr is an in-memory cache of the last GoBGP peer address used per
	// session. Key is session name; value is the remote endpoint address last applied
	// to GoBGP. This replaces the per-node annotation pattern, eliminating two API
	// calls per reconcile (Patch + re-Get).
	lastGoBGPAddrMu sync.RWMutex
	lastGoBGPAddr   map[string]string
}

// Reconcile ensures the GoBGP peer state matches the BGPSession spec.
// It resolves the LocalEndpoint and RemoteEndpoint BGPEndpoint resources to
// obtain addresses and AS numbers, then programs GoBGP accordingly.
// After configuring the peer it performs a one-shot ListPeer to set the
// SessionEstablished condition and emit Prometheus metrics. A RequeueAfter
// of 30 seconds ensures the condition is refreshed periodically without a
// dedicated polling goroutine.
func (r *SessionReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	var session bgpv1alpha1.BGPSession
	if err := r.Get(ctx, req.NamespacedName, &session); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}
		// Resource fully deleted — finalizer handled the delete path.
		return ctrl.Result{}, nil
	}

	c := r.GoBGP.Client()
	if c == nil {
		return ctrl.Result{}, fmt.Errorf("GoBGP not connected")
	}

	// Handle deletion.
	if !session.DeletionTimestamp.IsZero() {
		if err := r.handleDelete(ctx, c, &session); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Ensure finalizer is present before making any GoBGP calls.
	if !controllerutil.ContainsFinalizer(&session, SessionFinalizer) {
		patch := client.MergeFrom(session.DeepCopy())
		controllerutil.AddFinalizer(&session, SessionFinalizer)
		if err := r.Patch(ctx, &session, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		// Re-read after patch to get updated resource version.
		if err := r.Get(ctx, req.NamespacedName, &session); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}

	// Resolve both endpoint references.
	ep1, ep2, err := r.resolveEndpoints(ctx, &session)
	if err != nil {
		return r.setConfiguredFalse(ctx, &session, fmt.Sprintf("endpoint resolution failed: %v", err))
	}

	// Fetch BGPConfiguration to get the listen port.
	var bgpCfg bgpv1alpha1.BGPConfiguration
	listenPort := int32(1790) // default
	if err := r.Get(ctx, types.NamespacedName{Name: "default"}, &bgpCfg); err == nil {
		listenPort = bgpCfg.Spec.ListenPort
	}

	// Orient local/remote from this node's perspective.
	// The BGPSession has a LocalEndpoint and RemoteEndpoint, but GoBGP on this node
	// can only bind to addresses that are local to this node. Determine which endpoint
	// belongs to this node by comparing against the expected endpoint name pattern.
	thisNodeEndpoint := r.LocalEndpoint
	var localEP, remoteEP *bgpv1alpha1.BGPEndpoint
	switch {
	case ep1.Name == thisNodeEndpoint:
		localEP, remoteEP = ep1, ep2
	case ep2.Name == thisNodeEndpoint:
		localEP, remoteEP = ep2, ep1
	default:
		// This session does not involve this node — skip without error.
		// The node on either endpoint will handle its own GoBGP configuration.
		return ctrl.Result{}, nil
	}

	// Detect remote address change using the in-memory last-address cache.
	// This avoids annotation Patch + re-Get API calls on every reconcile.
	r.lastGoBGPAddrMu.RLock()
	lastAddr, hasLastAddr := r.lastGoBGPAddr[session.Name]
	r.lastGoBGPAddrMu.RUnlock()
	if hasLastAddr && lastAddr != remoteEP.Spec.Address {
		log.Printf("bgp/session: %s remote address changed from %s to %s — deleting old peer",
			session.Name, lastAddr, remoteEP.Spec.Address)
		if _, err := c.DeletePeer(ctx, &gobgpapi.DeletePeerRequest{Address: lastAddr}); err != nil && !isNotFound(err) {
			log.Printf("bgp/session: delete old peer %s: %v", lastAddr, err)
		}
	}

	// Add or update the peer in GoBGP.
	gobgpPeer := buildGoBGPPeer(&session, localEP, remoteEP, listenPort)
	if err := addOrUpdatePeer(ctx, c, gobgpPeer); err != nil {
		return r.setConfiguredFalse(ctx, &session, fmt.Sprintf("GoBGP error: %v", err))
	}

	// Record the last known remote address in-memory for change detection on future reconciles.
	r.lastGoBGPAddrMu.Lock()
	if r.lastGoBGPAddr == nil {
		r.lastGoBGPAddr = make(map[string]string)
	}
	r.lastGoBGPAddr[session.Name] = remoteEP.Spec.Address
	r.lastGoBGPAddrMu.Unlock()

	// Query the live peer state from GoBGP and update both the SessionEstablished
	// condition and Prometheus metrics.
	sessionState, isEstablished, rxPrefixes := r.queryPeerState(ctx, c, remoteEP.Spec.Address)

	// Save the previous established state BEFORE mutating conditions so flap
	// detection compares the old persisted state against the new observed state.
	prevEstablished := apimeta.FindStatusCondition(session.Status.Conditions, bgpv1alpha1.BGPSessionEstablished)
	wasEstablished := prevEstablished != nil && prevEstablished.Status == metav1.ConditionTrue

	// Build the full status patch: Configured=True + SessionEstablished.
	statusPatch := client.MergeFrom(session.DeepCopy())

	apimeta.SetStatusCondition(&session.Status.Conditions, metav1.Condition{
		Type:               bgpv1alpha1.BGPSessionConfigured,
		Status:             metav1.ConditionTrue,
		Reason:             "Configured",
		Message:            fmt.Sprintf("peer %s added to GoBGP", remoteEP.Spec.Address),
		ObservedGeneration: session.Generation,
	})

	establishedStatus := metav1.ConditionFalse
	if isEstablished {
		establishedStatus = metav1.ConditionTrue
	}
	apimeta.SetStatusCondition(&session.Status.Conditions, metav1.Condition{
		Type:    bgpv1alpha1.BGPSessionEstablished,
		Status:  establishedStatus,
		Reason:  sessionState,
		Message: "GoBGP session state: " + sessionState,
	})

	if err := r.Status().Patch(ctx, &session, statusPatch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch session status: %w", err)
	}

	// Emit Prometheus metrics. Flap detection uses the saved pre-mutation state
	// so we correctly detect a True→False transition this reconcile cycle.
	RecordSessionState(session.Name, sessionState)
	RecordReceivedPrefixes(session.Name, rxPrefixes)

	if wasEstablished && !isEstablished {
		RecordSessionFlap(session.Name)
	}

	log.Printf("bgp/session: reconciled %s (local=%s remote=%s state=%s)",
		session.Name, localEP.Spec.Address, remoteEP.Spec.Address, sessionState)

	// Only requeue when not yet Established to poll for convergence.
	// Once Established, the BGPEndpoint and BGPSession watches drive re-reconciliation.
	if !isEstablished {
		return ctrl.Result{RequeueAfter: sessionNotEstablishedRequeue}, nil
	}
	return ctrl.Result{}, nil
}

// queryPeerState performs a one-shot ListPeer for the given neighbor address
// and returns the session state string, whether it is Established, and the
// total received prefix count across all address families.
func (r *SessionReconciler) queryPeerState(ctx context.Context, c gobgpapi.GobgpApiClient, neighborAddr string) (state string, isEstablished bool, rxPrefixes int64) {
	stream, err := c.ListPeer(ctx, &gobgpapi.ListPeerRequest{
		Address:          neighborAddr,
		EnableAdvertised: false,
	})
	if err != nil {
		log.Printf("bgp/session: ListPeer %s: %v", neighborAddr, err)
		return "Unknown", false, 0
	}

	for {
		resp, err := stream.Recv()
		if err != nil {
			break
		}
		p := resp.Peer
		if p == nil {
			continue
		}
		stateStr, established := peerStateToString(p)
		var rx int64
		for _, af := range p.AfiSafis {
			if af.State != nil {
				rx += int64(af.State.Received)
			}
		}
		return stateStr, established, rx
	}
	return "Unknown", false, 0
}

// handleDelete calls GoBGP DeletePeer and removes the finalizer.
func (r *SessionReconciler) handleDelete(ctx context.Context, c gobgpapi.GobgpApiClient, session *bgpv1alpha1.BGPSession) error {
	if !controllerutil.ContainsFinalizer(session, SessionFinalizer) {
		return nil
	}

	// Use the in-memory last-address cache to find the address GoBGP knows this peer by.
	// This covers endpoint address changes that happened between reconciles.
	r.lastGoBGPAddrMu.RLock()
	addr := r.lastGoBGPAddr[session.Name]
	r.lastGoBGPAddrMu.RUnlock()

	if addr != "" {
		if _, err := c.DeletePeer(ctx, &gobgpapi.DeletePeerRequest{Address: addr}); err != nil && !isNotFound(err) {
			return fmt.Errorf("DeletePeer %s: %w", addr, err)
		}
		log.Printf("bgp/session: deleted GoBGP peer %s (session=%s)", addr, session.Name)
	}

	// Clean up the in-memory cache entry.
	r.lastGoBGPAddrMu.Lock()
	delete(r.lastGoBGPAddr, session.Name)
	r.lastGoBGPAddrMu.Unlock()

	patch := client.MergeFrom(session.DeepCopy())
	controllerutil.RemoveFinalizer(session, SessionFinalizer)
	if err := r.Patch(ctx, session, patch); err != nil {
		return fmt.Errorf("remove finalizer: %w", err)
	}
	return nil
}

// resolveEndpoints fetches both BGPEndpoint objects referenced by the session.
func (r *SessionReconciler) resolveEndpoints(ctx context.Context, session *bgpv1alpha1.BGPSession) (local, remote *bgpv1alpha1.BGPEndpoint, err error) {
	var localEP bgpv1alpha1.BGPEndpoint
	if err := r.Get(ctx, types.NamespacedName{Name: session.Spec.LocalEndpoint}, &localEP); err != nil {
		return nil, nil, fmt.Errorf("get local endpoint %q: %w", session.Spec.LocalEndpoint, err)
	}

	var remoteEP bgpv1alpha1.BGPEndpoint
	if err := r.Get(ctx, types.NamespacedName{Name: session.Spec.RemoteEndpoint}, &remoteEP); err != nil {
		return nil, nil, fmt.Errorf("get remote endpoint %q: %w", session.Spec.RemoteEndpoint, err)
	}

	return &localEP, &remoteEP, nil
}

// setConfiguredFalse sets the Configured condition to False and returns an error to trigger requeue.
func (r *SessionReconciler) setConfiguredFalse(ctx context.Context, session *bgpv1alpha1.BGPSession, msg string) (reconcile.Result, error) {
	patch := client.MergeFrom(session.DeepCopy())
	apimeta.SetStatusCondition(&session.Status.Conditions, metav1.Condition{
		Type:               bgpv1alpha1.BGPSessionConfigured,
		Status:             metav1.ConditionFalse,
		Reason:             "Error",
		Message:            msg,
		ObservedGeneration: session.Generation,
	})
	if err := r.Status().Patch(ctx, session, patch); err != nil {
		log.Printf("bgp/session: patch status: %v", err)
	}
	return ctrl.Result{}, fmt.Errorf("%s", msg)
}

// SetupWithManager registers the SessionReconciler with the controller-runtime manager.
func (r *SessionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&bgpv1alpha1.BGPSession{}).
		WithOptions(controller.Options{
			RateLimiter: workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](1*time.Second, 30*time.Second),
		}).
		Complete(r)
}
