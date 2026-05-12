# Getting started

This guide walks you through deploying the [BGP][bgp] control plane on a
Kubernetes cluster and establishing your first BGP session. By the end, you'll
have:

- The BGP controller DaemonSet running on your nodes
- `BGPEndpoint` resources representing your nodes
- A `BGPPeeringPolicy` that creates a full-mesh [iBGP][ibgp] topology
- A `BGPAdvertisement` that injects a prefix into the routing table

---

## Before you begin

Make sure you have:

- A Kubernetes cluster with IPv6 enabled (each node needs a global-scope IPv6
  address)
- `kubectl` configured to reach your cluster
- Access to the BGP controller image (`ghcr.io/milo-os/bgp:latest`) and a
  GoBGP container image (`gobgpd:latest`)

---

## Step 1: Install the CRDs

Apply the CRD manifests:

```bash
kubectl apply -k config/crd
```

Verify that the CRDs are installed:

```bash
kubectl get crds | grep bgp.miloapis.com
```

You should see six CRDs:

```
bgpadvertisements.bgp.miloapis.com    ...
bgpconfigurations.bgp.miloapis.com    ...
bgpendpoints.bgp.miloapis.com         ...
bgppeeringpolicies.bgp.miloapis.com   ...
bgproutepolicies.bgp.miloapis.com     ...
bgpsessions.bgp.miloapis.com          ...
```

---

## Step 2: Deploy the controller

Apply the full deployment, which includes the namespace, RBAC, ConfigMap,
and DaemonSet:

```bash
kubectl apply -k config/deploy
```

This creates the `bgp-system` namespace and deploys the DaemonSet on all
non-control-plane nodes. Each pod runs three containers:

- **`bgp`** — the controller
- **`gobgpd`** — the [GoBGP][gobgp] daemon
- **`config-gen`** (init) — discovers the node's global IPv6 address and
  writes an initial `gobgp.conf`

Verify that the DaemonSet is running:

```bash
kubectl -n bgp-system get daemonset bgp
kubectl -n bgp-system get pods -l app.kubernetes.io/name=bgp
```

All pods should reach `Running` status with both runtime containers ready
(2/2). If a pod is stuck, check the logs:

```bash
kubectl -n bgp-system describe pod <pod-name>
kubectl -n bgp-system logs <pod-name> -c bgp
kubectl -n bgp-system logs <pod-name> -c gobgpd
```

---

## Step 3: Create a BGPConfiguration

Create a `BGPConfiguration` to define your cluster's BGP speaker identity. This
resource sets the [AS number][asn] and controls how [GoBGP][gobgp] is
configured. You need exactly one, named `default`:

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

Apply it:

```bash
kubectl apply -f bgpconfig.yaml
```

Verify that the speaker is ready:

```bash
kubectl get bgpconfig default
```

You should see:

```
NAME      AS      PORT   READY
default   65001   1790   True
```

---

## Step 4: Create BGPEndpoints

Each BGP speaker needs a `BGPEndpoint` resource. In production, a node
operator creates these automatically. For this guide, you create them manually.

First, find your nodes' IPv6 addresses:

```bash
kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.addresses[?(@.type=="InternalIP")].address}{"\n"}{end}'
```

Then create an endpoint for each node. Replace the addresses and names with
your actual values:

```yaml
apiVersion: bgp.miloapis.com/v1alpha1
kind: BGPEndpoint
metadata:
  name: node-worker-01
  labels:
    bgp.miloapis.com/role: node
    bgp.miloapis.com/cluster: my-cluster
spec:
  address: "2001:db8:1::1"
  asNumber: 65001
---
apiVersion: bgp.miloapis.com/v1alpha1
kind: BGPEndpoint
metadata:
  name: node-worker-02
  labels:
    bgp.miloapis.com/role: node
    bgp.miloapis.com/cluster: my-cluster
spec:
  address: "2001:db8:1::2"
  asNumber: 65001
```

Apply and verify:

```bash
kubectl apply -f endpoints.yaml
kubectl get bgpep
```

You should see:

```
NAME             ADDRESS         ASN
node-worker-01   2001:db8:1::1   65001
node-worker-02   2001:db8:1::2   65001
```

---

## Step 5: Create a BGPPeeringPolicy

Create a full-mesh peering policy that selects your node endpoints:

```yaml
apiVersion: bgp.miloapis.com/v1alpha1
kind: BGPPeeringPolicy
metadata:
  name: node-mesh
spec:
  selector:
    matchLabels:
      bgp.miloapis.com/role: node
      bgp.miloapis.com/cluster: my-cluster
  mode: mesh
```

Apply:

```bash
kubectl apply -f policy.yaml
```

The `PeeringPolicyReconciler` creates a `BGPSession` for every pair of
matching endpoints. With two nodes, you get one session.

---

## Step 6: Verify that sessions are established

Check the `BGPSession` resources:

```bash
kubectl get bgpsess
```

The session starts in `Active` or `Connect` state, then transitions to
`Established` once both sides configure each other:

```
NAME                       LOCAL            REMOTE           SESSION       RX PREFIXES
worker-01-to-worker-02     node-worker-01   node-worker-02   Established   0
```

If a session doesn't reach `Established` after a minute or two, check the
GoBGP logs on both nodes:

```bash
kubectl -n bgp-system logs -l app.kubernetes.io/name=bgp -c gobgpd
```

Common causes:

- The remote node's IPv6 address isn't reachable on port 1790
- The `BGPEndpoint` address doesn't match the node's actual IPv6 address
- A firewall is blocking TCP on port 1790

---

## Step 7: Advertise a prefix

Create a `BGPAdvertisement` to inject a prefix into the local
[RIB][rib] (Routing Information Base):

```yaml
apiVersion: bgp.miloapis.com/v1alpha1
kind: BGPAdvertisement
metadata:
  name: cluster-prefix
spec:
  prefixes:
    - "2001:db8:ff00::/40"
```

Apply:

```bash
kubectl apply -f advertisement.yaml
```

Verify the advertisement:

```bash
kubectl get bgpadvert cluster-prefix -o jsonpath='{.status}'
```

After a few seconds, check the session to confirm that the remote node
received the prefix:

```bash
kubectl get bgpsess -o wide
```

The `RX PREFIXES` column should show a non-zero count.

---

## What's next

- Read the [API reference](api/) for complete field documentation on all six
  CRDs.
- Read the [service design](design/) to understand the architecture and
  controller internals.
- Explore the [examples](examples/) for common configuration patterns:
  - [Single-cluster full mesh](examples/single-cluster-mesh.yaml)
  - [Route-reflector topology](examples/route-reflector.yaml)
  - [eBGP peering](examples/ebgp-peering.yaml)
  - [Prefix advertisement with communities](examples/prefix-advertisement.yaml)
  - [Route filtering](examples/route-filtering.yaml)

<!-- References -->
[bgp]: https://datatracker.ietf.org/doc/html/rfc4271
[ibgp]: https://datatracker.ietf.org/doc/html/rfc4271#section-5
[gobgp]: https://github.com/osrg/gobgp
[asn]: https://www.iana.org/assignments/as-numbers/as-numbers.xhtml
[rib]: https://en.wikipedia.org/wiki/Routing_table
