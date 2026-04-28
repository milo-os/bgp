---
status: implementable
stage: alpha
---

# Service Design: BGP Control Plane

> Last verified: 2026-03-16 against main (initial implementation)

## Table of Contents

- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [Developer Experience](#developer-experience)
  - [Resource Model](#resource-model)
  - [Three-Layer Architecture](#three-layer-architecture)
- [Design Details](#design-details)
  - [Controller Architecture](#controller-architecture)
  - [GoBGP Sidecar Pattern](#gobgp-sidecar-pattern)
  - [Session Lifecycle](#session-lifecycle)
  - [Status Polling](#status-polling)
  - [Resilience and Re-reconciliation](#resilience-and-re-reconciliation)
  - [Metrics](#metrics)
- [User Stories](#user-stories)
- [Drawbacks](#drawbacks)
- [Alternatives Considered](#alternatives-considered)

---

## Summary

`milo-os/bgp` is a declarative BGP control plane that manages BGP topology via Custom Resource Definitions (CRDs) on any Kubernetes-compatible API server, powered by [GoBGP](https://github.com/osrg/gobgp). It uses the Kubernetes API as a control plane framework — not as a container orchestrator. The controller runs anywhere you have a Kubernetes-compatible API server (full clusters, k3s, KCP, or standalone kube-apiserver). No kubelet, scheduler, CNI, or pod networking is required. The API group is `bgp.miloapis.com/v1alpha1`.

Resources represent things — endpoints, sessions, policies — not imperative commands. The controller is topology-agnostic: it reconciles CRDs into GoBGP gRPC calls without embedded knowledge of nodes, clusters, borders, or network topology. Any system that can create Kubernetes objects can produce BGP topology by writing BGP CRDs.

> [!NOTE]
> This project is in `v1alpha1`. APIs are subject to change. See the [API reference](../api/README.md) for current field documentation.

---

## Motivation

BGP is fundamental to modern networking infrastructure. It is the protocol that connects routers, establishes reachability across AS boundaries, and carries the route information that makes global IP routing work. In cloud-native environments, BGP is equally central: it distributes pod CIDRs, advertises service VIPs, and, in overlay architectures, carries SRv6 segment lists between nodes.

Existing Kubernetes BGP implementations — Cilium, Calico, MetalLB, Kube-Router — all embed BGP management within their CNI-specific controllers. This coupling has two consequences:

1. **BGP topology is not reusable.** If you want to change your data plane, you also change your BGP management system. There is no standard Kubernetes API for "this node has a BGP speaker" or "these two speakers should peer."

2. **BGP configuration is opaque.** You cannot inspect BGP session state with `kubectl`, write automation against BGP topology, or apply RBAC to BGP operations using standard Kubernetes tooling.

This project decouples BGP topology management from the data plane. The control plane speaks BGP CRDs. The data plane is whatever system reads routes from the kernel FIB.

### Goals

- Declarative BGP topology management via Kubernetes CRDs — `kubectl apply` drives BGP.
- Topology-agnostic controller with no knowledge of nodes, clusters, or network topology. The controller reconciles whatever CRDs exist.
- Independent producer/consumer model: any system can create BGP CRDs; the controller reconciles them uniformly. Labels and selectors drive topology automation.
- Support for iBGP, eBGP, route reflection, prefix advertisement, and route import/export policy.
- Kernel FIB synchronization: learned BGP routes are programmed into the kernel routing table via netlink (proto 196).
- SRv6 VPN overlay support (RFC 9252) as a planned extension (`BGPVPN`, `BGPVPNRoute` resources).
- Status visibility: session FSM state, received/advertised prefix counts, and flap counters are surfaced on BGPSession resources and as Prometheus metrics.

### Non-Goals

- **CNI implementation.** This project does not configure network interfaces, assign pod IPs, or implement pod networking. It manages BGP sessions only.
- **SRv6 encap/decap dataplane.** The route syncer programs next-hops into the kernel FIB; the kernel and hardware handle forwarding.
- **Cluster discovery or fleet management.** Discovering peers across clusters is the responsibility of a higher-level operator (e.g., the node auto-peer operator).
- **BGP security** (MD5 authentication, TCP-AO, RPKI) in the alpha release.
- **Multi-path/ECMP** configuration in the alpha release.
- **IPv4 primary.** The alpha implementation targets IPv6-only deployments. IPv4 address family is supported in configuration but the route syncer programs IPv6 unicast routes only.

---

## Proposal

### Developer Experience

A platform operator declaring a BGP speaker, two endpoints, and a mesh peering policy:

```bash
# Declare the local BGP speaker identity
kubectl apply -f - <<EOF
apiVersion: bgp.miloapis.com/v1alpha1
kind: BGPConfiguration
metadata:
  name: default
spec:
  asNumber: 65001
  listenPort: 1790
  routerIDSource: NodeIP
EOF

# Declare two endpoints (normally created by the node auto-peer operator)
kubectl apply -f - <<EOF
apiVersion: bgp.miloapis.com/v1alpha1
kind: BGPEndpoint
metadata:
  name: node-a
  labels:
    topology.example.com/region: us-east
spec:
  address: "2001:db8::1"
  asNumber: 65001
---
apiVersion: bgp.miloapis.com/v1alpha1
kind: BGPEndpoint
metadata:
  name: node-b
  labels:
    topology.example.com/region: us-east
spec:
  address: "2001:db8::2"
  asNumber: 65001
EOF

# Automate peering — mesh within the region
kubectl apply -f - <<EOF
apiVersion: bgp.miloapis.com/v1alpha1
kind: BGPPeeringPolicy
metadata:
  name: us-east-mesh
spec:
  selector:
    matchLabels:
      topology.example.com/region: us-east
  mode: mesh
EOF
```

Inspect session state:

```bash
kubectl get bgpsessions
# NAME               LOCAL    REMOTE    SESSION       RX PREFIXES
# node-a--node-b     node-a   node-b    Established   4
```

Inspect all resources in the API group:

```bash
kubectl get bgpconfigurations,bgpendpoints,bgpsessions,bgppeeringpolicies,bgpadvertisements,bgproutepolicies
```

### Resource Model

The API defines six resources in `v1alpha1`. Two additional VPN resources (`BGPVPN`, `BGPVPNRoute`) are planned for a future release.

| Resource | Kind | Short Name | Scope | Purpose |
|----------|------|------------|-------|---------|
| `bgpconfigurations` | `BGPConfiguration` | `bgpconfig` | Cluster | Local speaker identity (AS, port, router ID) |
| `bgpendpoints` | `BGPEndpoint` | `bgpep` | Cluster | Self-advertisement ("I exist at this address") |
| `bgpsessions` | `BGPSession` | `bgpsess` | Cluster | Peering relationship between two endpoints |
| `bgppeeringpolicies` | `BGPPeeringPolicy` | `bgppp` | Cluster | Automates session creation via label selectors |
| `bgpadvertisements` | `BGPAdvertisement` | `bgpadvert` | Cluster | Prefix advertisement (phase 2 active) |
| `bgproutepolicies` | `BGPRoutePolicy` | `bgprp` | Cluster | Import/export filtering rules (phase 2 active) |

All resources are cluster-scoped because BGP topology spans the cluster boundary.

**Resource relationships:**

```
BGPPeeringPolicy
  └─ selects BGPEndpoints (via .spec.selector)
       └─ creates BGPSessions (one per endpoint pair)

BGPConfiguration   ─── configures ──→  GoBGP speaker
BGPSession         ─── programs ────→  GoBGP peer (AddPeer/UpdatePeer)
BGPAdvertisement   ─── injects ─────→  GoBGP RIB (AddPath)
BGPRoutePolicy     ─── applies ─────→  GoBGP policy table
```

### Three-Layer Architecture

```
┌─────────────────────────────────────────────────────────────┐
│  Layer 1: CRD Producers                                      │
│                                                               │
│  Node Auto-Peer Operator  ──┐                                │
│  Cluster Discovery          ├──→  BGPEndpoint resources      │
│  Platform Operator (human)  │    BGPPeeringPolicy resources  │
│  Automation system        ──┘    BGPAdvertisement resources  │
└───────────────────────────────────┬─────────────────────────┘
                                    │ Kubernetes API
┌───────────────────────────────────▼─────────────────────────┐
│  Layer 2: BGP Controller (this project)                       │
│                                                               │
│  ConfigReconciler      ──→  GoBGP SetBgp (AS, RouterID)      │
│  SessionReconciler     ──→  GoBGP AddPeer/UpdatePeer/DelPeer │
│  PeeringPolicyReconciler ─→  Creates/deletes BGPSession CRDs │
│  AdvertisementReconciler ─→  GoBGP AddPath/DeletePath        │
│  RoutePolicyReconciler  ──→  GoBGP AddPolicy/DeletePolicy    │
│  StatusPoller (10s)     ──→  Updates BGPSession.status       │
│  HealthWatcher (5s)     ──→  Detects GoBGP restart           │
└───────────────────────────────────┬─────────────────────────┘
                                    │ gRPC (127.0.0.1:50051)
┌───────────────────────────────────▼─────────────────────────┐
│  GoBGP Sidecar (per-node DaemonSet pod)                      │
│                                                               │
│  BGP FSM, RIB, route selection                               │
│  WatchEvent stream ──→ Route Syncer ──→ netlink (proto 196) │
└─────────────────────────────────────────────────────────────┘
```

The controller is **topology-agnostic by design**. It does not know which BGPSession belongs to which node, or whether two endpoints are in the same cluster. It processes every BGPSession resource and programs the local GoBGP instance for sessions where `spec.localEndpoint` matches the value passed to `--local-endpoint` at startup.

---

## Design Details

### Controller Architecture

The controller runs as a DaemonSet — one pod per node — alongside a GoBGP sidecar container. Each instance:

1. Connects to its local GoBGP sidecar via gRPC at `127.0.0.1:50051` (configurable via `--gobgp-addr`).
2. Starts a controller-runtime manager with five reconcilers.
3. Starts three background goroutines: health watcher, status poller, and route watcher.

Each reconciler is responsible for one CRD kind:

| Reconciler | CRD | GoBGP Operation |
|------------|-----|-----------------|
| `ConfigReconciler` | `BGPConfiguration` | `SetBgp` (global AS and router ID) |
| `SessionReconciler` | `BGPSession` | `AddPeer` / `UpdatePeer` / `DeletePeer` |
| `PeeringPolicyReconciler` | `BGPPeeringPolicy` | Creates/deletes `BGPSession` CRDs (no direct GoBGP calls) |
| `AdvertisementReconciler` | `BGPAdvertisement` | `AddPath` / `DeletePath` |
| `RoutePolicyReconciler` | `BGPRoutePolicy` | `AddPolicy` / `DeletePolicy` |

The `PeeringPolicyReconciler` is a pure Kubernetes operator — it reads endpoints and writes sessions. The session-to-GoBGP reconciliation is handled by the `SessionReconciler` in a separate reconcile loop.

### GoBGP Sidecar Pattern

GoBGP runs as a sidecar container in the same DaemonSet pod as the controller. Communication is over gRPC on localhost. This pattern:

- Eliminates network policy concerns between controller and BGP daemon.
- Allows independent restart of GoBGP without restarting the controller.
- Scopes GoBGP state to one node — each node has its own BGP speaker with its own peers.

GoBGP is **treated as stateless** from the controller's perspective. All desired BGP state is in the CRDs. If GoBGP restarts, the controller's `FullReconcile` re-applies the complete desired state.

The controller connects to GoBGP at startup with up to 30 retry attempts (2-second interval), tolerating slow GoBGP startup.

### Session Lifecycle

When a `BGPSession` is created or updated:

1. The `SessionReconciler` reads `spec.localEndpoint` and `spec.remoteEndpoint` (both `BGPEndpoint` names).
2. It resolves both endpoints to get IP addresses and AS numbers.
3. It calls `buildGoBGPPeer` to construct a `gobgpapi.Peer` struct from the session spec and resolved endpoints.
4. It calls `AddPeer`; if GoBGP returns "can't overwrite the existing peer" (GoBGP's equivalent of `AlreadyExists`), it falls back to `UpdatePeer`.
5. On deletion, it calls `DeletePeer` using the remote endpoint's address.

The `SessionReconciler` only reconciles sessions where `spec.localEndpoint` matches the controller's `--local-endpoint` flag. Sessions for other nodes are ignored.

Session status is **not updated by the reconciler** — status comes from the status poller (see below).

### Status Polling

A background goroutine polls GoBGP every 10 seconds using `ListPeer` with `EnableAdvertised: true`. For each `BGPSession` resource, it:

1. Resolves `spec.remoteEndpoint` to get the neighbor address GoBGP uses as its key.
2. Matches the GoBGP peer state by neighbor address.
3. Updates `status.sessionState`, `status.receivedPrefixes`, `status.advertisedPrefixes`, `status.flapCount`, and `status.lastTransitionTime`.
4. Sets the `SessionEstablished` condition.
5. Emits Prometheus metrics.

The polling approach (rather than event-driven) avoids the complexity of managing a long-lived GoBGP event stream for status. The 10-second polling interval means status may lag real session state by up to 10 seconds.

### Resilience and Re-reconciliation

The health watcher goroutine polls GoBGP every 5 seconds via `GetBgp`. When it detects a failure:

1. It closes the stale gRPC connection.
2. It reconnects with retries.
3. On successful reconnection, it closes the `reconnectCh` channel (signaling reconcilers) and calls `FullReconcile`.

`FullReconcile` re-applies:
- All `BGPSession` resources via `AddPeer`/`UpdatePeer`.
- All `BGPAdvertisement` resources by bumping a `bgp.miloapis.com/reconcile-trigger` annotation, which causes the advertisement reconciler to re-inject all prefixes.
- All `BGPRoutePolicy` resources similarly.

This ensures GoBGP state is fully consistent with CRDs after any restart.

### Metrics

The controller exposes Prometheus metrics on `:8082` (configurable via `--metrics-addr`):

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `bgp_session_state` | Gauge | `session`, `state` | 1 for the active FSM state, 0 for all others |
| `bgp_received_prefixes_total` | Gauge | `session` | Prefixes received from the remote peer |
| `bgp_session_flaps_total` | Counter | `session` | Times the session left Established state |
| `bgp_advertised_prefixes_total` | Gauge | `advertisement` | Prefixes currently in the GoBGP RIB |
| `bgp_route_policies_applied` | Gauge | `policy` | 1 if the policy is applied to GoBGP |

All standard controller-runtime metrics (reconcile duration, work queue depth, etc.) are also exposed.

---

## User Stories

**As a platform operator**, I want to declare BGP peering topology via `kubectl apply` so that BGP configuration is version-controlled, auditable, and managed with the same tooling as the rest of my infrastructure.

**As a network engineer**, I want to inspect BGP session state with `kubectl get bgpsessions` so that I can diagnose connectivity problems without logging into individual nodes or using vendor-specific CLI tools.

**As an automation system** (node discovery operator, cluster fleet manager), I want to create `BGPEndpoint` resources for each node and have the `BGPPeeringPolicy` controller automatically create the correct sessions, so that topology scales without per-node manual configuration.

**As a multi-cluster operator**, I want to declare eBGP sessions between clusters by creating `BGPSession` resources with endpoints that have different `asNumber` values, without modifying the BGP controller or its configuration.

**As a platform engineer debugging a network outage**, I want to see flap counts and last-transition timestamps on `BGPSession` resources so that I can correlate BGP instability with other events in the cluster.

---

## Drawbacks

**GoBGP sidecar coupling.** The controller is tightly coupled to GoBGP's gRPC API. Replacing GoBGP with another BGP daemon (Bird, FRRouting) would require rewriting the GoBGP client layer.

**Status polling lag.** Session status updates are polled every 10 seconds. In high-frequency flap scenarios, short-lived state transitions may not be captured.

**Session ownership model.** Each controller instance only reconciles sessions where `spec.localEndpoint` matches its own `--local-endpoint`. If a `BGPSession` names a non-existent endpoint as local, it will never be reconciled without operator intervention.

---

## Alternatives Considered

**Embed BGP in the CNI.** This is the Cilium/Calico/MetalLB approach. Rejected because it couples BGP management to the data plane, preventing reuse across networking stacks.

**Use FRRouting instead of GoBGP.** FRR has broader protocol support and wider production deployment. GoBGP was chosen for alpha because its gRPC API is designed for programmatic control, making the controller implementation simpler. FRR can be evaluated as an alternative backend post-alpha.

**Push configuration files instead of gRPC.** Writing GoBGP config files and sending SIGHUP is an alternative to gRPC. Rejected because the gRPC API provides richer status feedback and is the direction GoBGP intends for programmatic use.

**Namespace-scoped resources.** Rejected because BGP topology spans namespaces (and clusters). Cluster-scoped resources reflect the actual topology boundary.
