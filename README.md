# BGP

Kubernetes-native BGP control plane powered by [GoBGP](https://github.com/osrg/gobgp).

## Overview

BGP is a declarative, CRD-driven BGP control plane for Kubernetes. It manages
BGP topology through standard Kubernetes resources — endpoints declare speaker
identity, sessions declare peering relationships, and policies automate topology.
The controller reconciles these resources against a local GoBGP instance and
programs learned routes into the kernel.

**API Group:** `bgp.miloapis.com/v1alpha1`

## Architecture

```
  CRD Producers                Controller                GoBGP + Kernel
  ─────────────                ──────────                ──────────────
  Node operator ──┐            ConfigReconciler   ───→   GoBGP SetBgp
  Cluster disco   ├──→  CRDs   SessionReconciler  ───→   GoBGP AddPeer
  Human operator ─┘            PolicyReconciler   ───→   BGPSession CRDs
                               AdvertReconciler   ───→   GoBGP AddPath
                               RoutePolicyRecon   ───→   GoBGP AddPolicy
                               StatusPoller (10s) ───→   BGPSession.status
                               RouteWatcher       ───→   netlink (proto 196)
```

The controller runs as a DaemonSet. Each node runs one controller instance alongside a GoBGP sidecar container. The controller connects to GoBGP via gRPC at `127.0.0.1:50051`. GoBGP is treated as stateless — if it restarts, the controller re-applies all CRD state automatically.

## Custom Resources

| Kind | Short Name | Scope | Purpose |
|------|------------|-------|---------|
| `BGPConfiguration` | `bgpconfig` | Cluster | Local speaker identity (AS number, port, router ID). One per cluster. |
| `BGPEndpoint` | `bgpep` | Cluster | Self-advertisement: "I exist at this address with this AS." |
| `BGPSession` | `bgpsess` | Cluster | Peering relationship between two endpoints. |
| `BGPPeeringPolicy` | `bgppp` | Cluster | Automates session creation via label selectors. |
| `BGPAdvertisement` | `bgpadvert` | Cluster | IPv6 prefix advertisement with optional communities and local-pref. |
| `BGPRoutePolicy` | `bgprp` | Cluster | Import/export filtering rules. |

## Quick Start

**1. Declare the BGP speaker:**

```yaml
apiVersion: bgp.miloapis.com/v1alpha1
kind: BGPConfiguration
metadata:
  name: default
spec:
  asNumber: 65001
  listenPort: 1790       # non-privileged port (Galactic convention)
  routerIDSource: NodeIP
```

**2. Declare endpoints (typically created by a node auto-peer operator):**

```yaml
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
```

**3. Automate peering with a policy:**

```yaml
apiVersion: bgp.miloapis.com/v1alpha1
kind: BGPPeeringPolicy
metadata:
  name: us-east-mesh
spec:
  selector:
    matchLabels:
      topology.example.com/region: us-east
  mode: mesh
```

**4. Check session state:**

```bash
kubectl get bgpsessions
# NAME             LOCAL    REMOTE    SESSION       RX PREFIXES
# node-a--node-b   node-a   node-b    Established   4
```

## Documentation

- [Service Design](docs/design/README.md) — Architecture, motivation, three-layer model, design decisions
- [API Reference](docs/api/README.md) — Complete field documentation for all CRDs with examples

## Status

This project is in early development (`v1alpha1`). APIs may change.

## License

Apache License 2.0 — see [LICENSE](LICENSE) for details.
