// Package routesync streams BGP RIB events from GoBGP and programs netlink routes.
package routesync

import (
	"context"
	"fmt"
	"log"
	"net"
	"time"

	gobgpapi "github.com/osrg/gobgp/v3/api"
	"github.com/osrg/gobgp/v3/pkg/apiutil"
	"github.com/osrg/gobgp/v3/pkg/packet/bgp"

	bgpnetlink "go.miloapis.com/bgp/internal/netlink"
	bgpcontroller "go.miloapis.com/bgp/internal/controller"
)

const routeWatchRetryInterval = 2 * time.Second

// RunRouteWatcher streams BGP path events from GoBGP and programs/removes
// netlink routes (proto 196) for received prefixes. It automatically reconnects
// the event stream on error.
//
// srv6Net is this node's own prefix (e.g. a /48); routes matching it are skipped
// so the node does not install a route to itself. Pass an empty string to disable
// the self-route filter.
//
// This function blocks until ctx is cancelled.
func RunRouteWatcher(ctx context.Context, gobgp *bgpcontroller.GoBGPClient, srv6Net string) {
	var ownPrefix *net.IPNet
	if srv6Net != "" {
		_, ownPrefix, _ = net.ParseCIDR(srv6Net)
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		c := gobgp.Client()
		if c == nil {
			log.Printf("bgp/route: GoBGP not connected, retrying in %s", routeWatchRetryInterval)
			select {
			case <-ctx.Done():
				return
			case <-time.After(routeWatchRetryInterval):
			}
			continue
		}

		if err := watchAndProgram(ctx, c, ownPrefix); err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				log.Printf("bgp/route: stream error: %v — restarting in %s", err, routeWatchRetryInterval)
				select {
				case <-ctx.Done():
					return
				case <-time.After(routeWatchRetryInterval):
				}
			}
		}
	}
}

// watchAndProgram opens a WatchEvent stream on GoBGP and programs netlink routes
// until the stream ends or ctx is cancelled.
//
// On startup, it loads the existing proto-196 routes from the kernel into
// knownPrefixes. After the initial RIB dump (paths with Init=true) arrives,
// any kernel route not present in the RIB is considered stale (left over from
// a previous operator lifetime) and is deleted.
func watchAndProgram(ctx context.Context, client gobgpapi.GobgpApiClient, ownPrefix *net.IPNet) error {
	// Seed knownPrefixes with all routes already in the kernel so stale routes
	// left from a prior operator run can be identified after the initial RIB dump.
	existingRoutes, err := bgpnetlink.ListManagedRoutes()
	if err != nil {
		log.Printf("bgp/route: list existing managed routes: %v", err)
	}
	knownPrefixes := make(map[string]net.IP, len(existingRoutes))
	for _, r := range existingRoutes {
		if r.Dst != nil {
			knownPrefixes[r.Dst.String()] = r.Gw
		}
	}
	if len(knownPrefixes) > 0 {
		log.Printf("bgp/route: loaded %d existing managed routes for stale-GC", len(knownPrefixes))
	}

	stream, err := client.WatchEvent(ctx, &gobgpapi.WatchEventRequest{
		Table: &gobgpapi.WatchEventRequest_Table{
			Filters: []*gobgpapi.WatchEventRequest_Table_Filter{
				{
					Type: gobgpapi.WatchEventRequest_Table_Filter_BEST,
					Init: true,
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("WatchEvent: %w", err)
	}

	// ribInitPrefixes tracks prefixes seen during the initial RIB dump so we can
	// garbage-collect kernel routes that are no longer in the RIB. Once GC has
	// run once (after the first event message), initGCDone is set to true.
	ribInitPrefixes := make(map[string]struct{})
	initGCDone := false

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

		table := resp.GetTable()
		if table == nil {
			continue
		}

		for _, path := range table.Paths {
			if path.Family == nil ||
				path.Family.Afi != gobgpapi.Family_AFI_IP6 ||
				path.Family.Safi != gobgpapi.Family_SAFI_UNICAST {
				continue
			}

			prefix, nextHop, err := extractPrefixAndNextHop(path)
			if err != nil {
				log.Printf("bgp/route: skip path: %v", err)
				continue
			}

			// Skip our own prefix to avoid self-routing.
			if ownPrefix != nil && prefix.String() == ownPrefix.String() {
				continue
			}

			if !initGCDone {
				// Collect prefixes seen in the init dump for stale-GC comparison.
				ribInitPrefixes[prefix.String()] = struct{}{}
			}

			if path.IsWithdraw {
				log.Printf("bgp/route: DEL route %s", prefix)
				if err := bgpnetlink.DelRoute(prefix); err != nil {
					log.Printf("bgp/route: del route %s: %v", prefix, err)
				}
				delete(knownPrefixes, prefix.String())
			} else {
				log.Printf("bgp/route: ADD route %s via %s", prefix, nextHop)
				if err := bgpnetlink.AddRoute(prefix, nextHop); err != nil {
					log.Printf("bgp/route: add route %s via %s: %v", prefix, nextHop, err)
				}
				knownPrefixes[prefix.String()] = nextHop
			}
		}

		// After processing the first event message (which contains the full initial
		// RIB dump from GoBGP when Init=true), garbage-collect any kernel routes
		// that were not present in the RIB. These are stale routes from a previous
		// operator lifetime.
		if !initGCDone {
			initGCDone = true
			for prefix, gw := range knownPrefixes {
				if _, inRIB := ribInitPrefixes[prefix]; !inRIB {
					_, pfxNet, parseErr := net.ParseCIDR(prefix)
					if parseErr != nil {
						continue
					}
					log.Printf("bgp/route: GC stale route %s (gw=%s) — not in RIB after init", prefix, gw)
					if delErr := bgpnetlink.DelRoute(pfxNet); delErr != nil {
						log.Printf("bgp/route: GC del route %s: %v", prefix, delErr)
					}
					delete(knownPrefixes, prefix)
				}
			}
		}
	}
}

// extractPrefixAndNextHop parses the NLRI and next-hop from a GoBGP Path.
func extractPrefixAndNextHop(path *gobgpapi.Path) (*net.IPNet, net.IP, error) {
	nlri, err := apiutil.GetNativeNlri(path)
	if err != nil {
		return nil, nil, fmt.Errorf("get native NLRI: %w", err)
	}

	_, ipNet, err := net.ParseCIDR(nlri.String())
	if err != nil {
		return nil, nil, fmt.Errorf("parse prefix %s: %w", nlri.String(), err)
	}

	attrs, err := apiutil.GetNativePathAttributes(path)
	if err != nil {
		return nil, nil, fmt.Errorf("get native path attrs: %w", err)
	}

	var nextHop net.IP
	for _, attr := range attrs {
		switch a := attr.(type) {
		case *bgp.PathAttributeNextHop:
			nextHop = a.Value
		case *bgp.PathAttributeMpReachNLRI:
			if len(a.Nexthop) > 0 {
				nextHop = a.Nexthop
			}
		}
	}
	if nextHop == nil {
		return nil, nil, fmt.Errorf("no next-hop in path attributes")
	}

	return ipNet, nextHop, nil
}
