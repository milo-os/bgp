# BGP Control Plane

Manage [BGP][bgp] topology declaratively through Kubernetes
[Custom Resource Definitions][crds] (CRDs), powered by
[GoBGP][gobgp].

You define BGP speakers, sessions, and policies as Kubernetes resources. The
controller reconciles them into a running GoBGP instance on each node and
programs learned routes into the kernel.

**API group:** `bgp.miloapis.com` | **Version:** `v1alpha1`

---

## Key features

- **Topology-agnostic.** The controller has no built-in knowledge of nodes,
  clusters, or datacenter layout. All topology lives in the CRDs you create.
- **CNI-independent.** Works with any [CNI][cni] — or no CNI at all. No
  dependency on Cilium, Calico, or any other network plugin.
- **Declarative session management.** `BGPPeeringPolicy` automates session
  creation through [label selectors][label-selectors]. Full-mesh and
  [route-reflector][rr] topologies are both supported.
- **Producer/consumer model.** Any system — node operators, cluster discovery,
  automation pipelines, or humans — can create BGP CRDs. The controller
  reconciles them uniformly.
- **[GoBGP][gobgp] sidecar.** Each node runs its own GoBGP daemon. The
  controller configures it over [gRPC][grpc] and programs kernel routes from
  BGP [RIB][rib] events.

---

## Quick start

Install the CRDs and deploy the controller:

```bash
kubectl apply -k config/deploy
```

Create a `BGPConfiguration` (one per cluster):

```yaml
apiVersion: bgp.miloapis.com/v1alpha1
kind: BGPConfiguration
metadata:
  name: default
spec:
  asNumber: 65001
  listenPort: 1790
  routerIDSource: NodeIP
```

Create `BGPEndpoint` resources for your nodes and a peering policy:

```yaml
apiVersion: bgp.miloapis.com/v1alpha1
kind: BGPEndpoint
metadata:
  name: node-worker-01
  labels:
    bgp.miloapis.com/role: node
spec:
  address: "2001:db8:1::1"
  asNumber: 65001
---
apiVersion: bgp.miloapis.com/v1alpha1
kind: BGPPeeringPolicy
metadata:
  name: mesh
spec:
  selector:
    matchLabels:
      bgp.miloapis.com/role: node
  mode: mesh
```

Check session state:

```bash
kubectl get bgpsess
```

For a complete walkthrough, see the
[Getting Started guide](docs/getting-started.md).

---

## CRDs

| Resource | Short name | Scope | Description |
|----------|-----------|-------|-------------|
| `BGPConfiguration` | `bgpconfig` | Cluster | Speaker identity: [AS number][asn], listen port, router ID. One per cluster. |
| `BGPEndpoint` | `bgpep` | Cluster | BGP speaker address and AS number. One per node (or manually created). |
| `BGPSession` | `bgpsess` | Cluster | Peering relationship between two endpoints. |
| `BGPPeeringPolicy` | `bgppp` | Cluster | Automates session creation through label selectors. |
| `BGPAdvertisement` | `bgpadvert` | Cluster | Prefix advertisement with optional [communities][bgp-communities] and [LOCAL_PREF][local-pref]. |
| `BGPRoutePolicy` | `bgprp` | Cluster | Import/export filtering with prefix matching. |

---

## Documentation

- [Service design](docs/design/) — motivation, architecture, controller
  internals, and design decisions
- [API reference](docs/api/) — complete field documentation for all six CRDs
- [Getting started](docs/getting-started.md) — deploy the control plane and
  establish your first BGP session

### Examples

- [Single-cluster full mesh](docs/examples/single-cluster-mesh.yaml)
- [Route-reflector topology](docs/examples/route-reflector.yaml)
- [eBGP peering](docs/examples/ebgp-peering.yaml)
- [Prefix advertisement with communities](docs/examples/prefix-advertisement.yaml)
- [Route filtering](docs/examples/route-filtering.yaml)

---

## Building

```bash
# Build the controller binary
CGO_ENABLED=0 go build -o bgp ./cmd/bgp

# Build the container image
docker build -f build/Dockerfile -t ghcr.io/milo-os/bgp:latest .
```

<!-- References -->
[bgp]: https://datatracker.ietf.org/doc/html/rfc4271
[gobgp]: https://github.com/osrg/gobgp
[crds]: https://kubernetes.io/docs/concepts/extend-kubernetes/api-extension/custom-resources/
[cni]: https://www.cni.dev/
[label-selectors]: https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/
[rr]: https://datatracker.ietf.org/doc/html/rfc4456
[grpc]: https://grpc.io/
[rib]: https://en.wikipedia.org/wiki/Routing_table
[asn]: https://www.iana.org/assignments/as-numbers/as-numbers.xhtml
[bgp-communities]: https://datatracker.ietf.org/doc/html/rfc1997
[local-pref]: https://datatracker.ietf.org/doc/html/rfc4271#section-5.1.5
