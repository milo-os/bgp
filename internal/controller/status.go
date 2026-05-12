package controller

import (
	"context"
	"fmt"
	"log"
	"time"

	gobgpapi "github.com/osrg/gobgp/v3/api"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bgpv1alpha1 "go.miloapis.com/bgp/api/v1alpha1"
)

const peerWatchRetryInterval = 2 * time.Second

// RunPeerStateWatcher streams GoBGP peer state change events and updates
// BGPSession status fields and Prometheus metrics on each transition.
// It automatically reconnects the event stream on error.
// This function blocks until ctx is cancelled.
func RunPeerStateWatcher(ctx context.Context, k8sClient client.Client, gobgp *GoBGPClient) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		c := gobgp.Client()
		if c == nil {
			log.Printf("bgp/status: GoBGP not connected, retrying in %s", peerWatchRetryInterval)
			select {
			case <-ctx.Done():
				return
			case <-time.After(peerWatchRetryInterval):
			}
			continue
		}

		if err := watchAndUpdatePeers(ctx, c, k8sClient); err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				log.Printf("bgp/status: stream error: %v — restarting in %s", err, peerWatchRetryInterval)
				select {
				case <-ctx.Done():
					return
				case <-time.After(peerWatchRetryInterval):
				}
			}
		}
	}
}

// watchAndUpdatePeers opens a WatchEvent stream filtered for peer state changes
// and updates BGPSession status for each event until the stream ends or ctx is cancelled.
func watchAndUpdatePeers(ctx context.Context, c gobgpapi.GobgpApiClient, k8sClient client.Client) error {
	stream, err := c.WatchEvent(ctx, &gobgpapi.WatchEventRequest{
		Peer: &gobgpapi.WatchEventRequest_Peer{},
	})
	if err != nil {
		return fmt.Errorf("WatchEvent: %w", err)
	}

	for {
		resp, err := stream.Recv()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				return fmt.Errorf("stream recv: %w", err)
			}
		}

		peerEvent := resp.GetPeer()
		if peerEvent == nil {
			continue
		}

		peer := peerEvent.Peer
		if peer == nil {
			continue
		}

		if err := handlePeerEvent(ctx, k8sClient, peer); err != nil {
			log.Printf("bgp/status: handle peer event: %v", err)
		}
	}
}

// handlePeerEvent finds the BGPSession whose remote endpoint matches the peer's
// neighbor address and updates its status fields and Prometheus metrics.
func handlePeerEvent(ctx context.Context, k8sClient client.Client, peer *gobgpapi.Peer) error {
	if peer.State == nil {
		return nil
	}

	neighborAddr := peer.State.NeighborAddress
	if neighborAddr == "" {
		return nil
	}

	// List all BGPSession resources and find the one matching this neighbor.
	var sessionList bgpv1alpha1.BGPSessionList
	if err := k8sClient.List(ctx, &sessionList); err != nil {
		return fmt.Errorf("list BGPSessions: %w", err)
	}

	matched := false
	for i := range sessionList.Items {
		sess := &sessionList.Items[i]
		if sess.DeletionTimestamp != nil {
			continue
		}

		// Resolve the remote endpoint to get the address GoBGP uses.
		var remoteEP bgpv1alpha1.BGPEndpoint
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: sess.Spec.RemoteEndpoint}, &remoteEP); err != nil {
			// Endpoint may not exist yet — skip silently; the session reconciler will handle it.
			continue
		}

		if remoteEP.Spec.Address != neighborAddr {
			continue
		}

		matched = true

		sessionState, isEstablished := peerStateToString(peer)

		// Detect flap using the existing condition before patching.
		prevCondition := apimeta.FindStatusCondition(sess.Status.Conditions, bgpv1alpha1.BGPSessionEstablished)
		wasEstablished := prevCondition != nil && prevCondition.Status == metav1.ConditionTrue

		patch := client.MergeFrom(sess.DeepCopy())

		condStatus := metav1.ConditionFalse
		if isEstablished {
			condStatus = metav1.ConditionTrue
		}
		apimeta.SetStatusCondition(&sess.Status.Conditions, metav1.Condition{
			Type:    bgpv1alpha1.BGPSessionEstablished,
			Status:  condStatus,
			Reason:  sessionState,
			Message: "GoBGP session state: " + sessionState,
		})

		if err := k8sClient.Status().Patch(ctx, sess, patch); err != nil {
			log.Printf("bgp/status: patch %s status: %v", sess.Name, err)
		}

		// Count received prefixes for metrics.
		var rxPrefixes int64
		for _, af := range peer.AfiSafis {
			if af.State != nil {
				rxPrefixes += int64(af.State.Received)
			}
		}

		RecordSessionState(sess.Name, sessionState)
		RecordReceivedPrefixes(sess.Name, rxPrefixes)
		if wasEstablished && !isEstablished {
			RecordSessionFlap(sess.Name)
		}

		// Only one session per neighbor address — no need to continue.
		break
	}

	if !matched {
		log.Printf("bgp/status: peer event for %s did not match any BGPSession", neighborAddr)
	}

	return nil
}

// peerStateToString maps GoBGP peer state to a human-readable string.
// Returns (state string, isEstablished bool).
func peerStateToString(p *gobgpapi.Peer) (string, bool) {
	if p.State == nil {
		return "Unknown", false
	}
	switch p.State.SessionState {
	case gobgpapi.PeerState_UNKNOWN:
		return "Unknown", false
	case gobgpapi.PeerState_IDLE:
		return "Idle", false
	case gobgpapi.PeerState_CONNECT:
		return "Connect", false
	case gobgpapi.PeerState_ACTIVE:
		return "Active", false
	case gobgpapi.PeerState_OPENSENT:
		return "OpenSent", false
	case gobgpapi.PeerState_OPENCONFIRM:
		return "OpenConfirm", false
	case gobgpapi.PeerState_ESTABLISHED:
		return "Established", true
	default:
		return "Unknown", false
	}
}
