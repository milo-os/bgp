---
status: implementable
stage: alpha
---

# API Reference: bgp.miloapis.com/v1alpha1

**API Group:** `bgp.miloapis.com`
**Version:** `v1alpha1`
**Stability:** Alpha — fields and defaults may change

All resources in this API group are **cluster-scoped** because BGP topology spans namespace and cluster boundaries.

## Resources

| Kind | Short Name | Verbs | Phase |
|------|------------|-------|-------|
| [BGPConfiguration](#bgpconfiguration) | `bgpconfig` | get, list, watch, create, update, patch, delete | Alpha |
| [BGPEndpoint](#bgpendpoint) | `bgpep` | get, list, watch, create, update, patch, delete | Alpha |
| [BGPSession](#bgpsession) | `bgpsess` | get, list, watch, create, update, patch, delete | Alpha |
| [BGPPeeringPolicy](#bgppeeringpolicy) | `bgppp` | get, list, watch, create, update, patch, delete | Alpha |
| [BGPAdvertisement](#bgpadvertisement) | `bgpadvert` | get, list, watch, create, update, patch, delete | Alpha (Phase 2) |
| [BGPRoutePolicy](#bgproutepolicy) | `bgprp` | get, list, watch, create, update, patch, delete | Alpha (Phase 2) |

Phase 2 resources have their schemas registered and reconcilers running, but see limited production use in the alpha release.

---

## BGPConfiguration

`BGPConfiguration` declares the local BGP speaker identity for the cluster. There should be exactly one `BGPConfiguration` per cluster, conventionally named `default`.

The controller reads this resource to configure GoBGP's global AS number, router ID, and listen port. If GoBGP restarts, the controller re-applies this configuration as part of full re-reconciliation.

```bash
kubectl get bgpconfigurations
kubectl get bgpconfig       # short name
```

Printed columns: `AS`, `Port`, `Ready`

### Spec

| Field | Type | Default | Required | Description |
|-------|------|---------|----------|-------------|
| `asNumber` | `uint32` | — | Yes | Local AS number. Range: 1–4294967295. For single-cluster iBGP, all nodes share the same AS. For multi-cluster eBGP, each cluster has a unique AS. |
| `listenPort` | `int32` | `1790` | No | TCP port GoBGP listens on. 1790 is the Galactic convention (non-privileged, avoids conflicts with standard BGP port 179). Range: 1–65535. |
| `routerIDSource` | `string` | `NodeIP` | No | Controls how the router ID is determined. `NodeIP` uses the node's IPv6 InternalIP. `Manual` requires `routerID` to be set explicitly. Enum: `NodeIP`, `Manual`. |
| `routerID` | `string` | — | No | BGP router ID in dotted-decimal IPv4 notation (BGP convention, e.g. `10.0.0.1`). Required when `routerIDSource` is `Manual`. |
| `addressFamilies` | `[]AddressFamily` | IPv6 unicast | No | Address families the speaker activates. Defaults to `[{afi: IPv6, safi: Unicast}]`. |

#### AddressFamily

| Field | Type | Default | Required | Description |
|-------|------|---------|----------|-------------|
| `afi` | `string` | — | Yes | Address Family Indicator. Enum: `IPv4`, `IPv6`. |
| `safi` | `string` | `Unicast` | No | Subsequent Address Family Indicator. Enum: `Unicast`, `Multicast`. |

### Status

| Field | Type | Description |
|-------|------|-------------|
| `conditions` | `[]metav1.Condition` | Current state of the BGP configuration. See [Conditions](#bgpconfiguration-conditions). |
| `observedASNumber` | `uint32` | AS number currently configured in GoBGP. |
| `observedRouterID` | `string` | Router ID currently configured in GoBGP. |

#### BGPConfiguration Conditions

| Type | Meaning |
|------|---------|
| `SpeakerReady` | GoBGP is running and configured with the spec values. `False` when GoBGP is unreachable or configuration failed. |

### Example

```yaml
apiVersion: bgp.miloapis.com/v1alpha1
kind: BGPConfiguration
metadata:
  name: default          # convention: one per cluster, named "default"
spec:
  asNumber: 65001        # private AS range: 64512–65534
  listenPort: 1790       # non-privileged port (Galactic convention)
  routerIDSource: NodeIP # derive router ID from node's IPv6 address
  addressFamilies:
    - afi: IPv6
      safi: Unicast
```

---

## BGPEndpoint

`BGPEndpoint` declares a BGP speaker endpoint — an IPv6 address and AS number that other speakers can peer with. An endpoint is a self-advertisement: "I exist at this address, with this AS."

Endpoints are typically created by a node auto-peer operator (one `BGPEndpoint` per node) or manually by platform operators. `BGPPeeringPolicy` resources use label selectors on endpoints to automate session creation.

```bash
kubectl get bgpendpoints
kubectl get bgpep         # short name
```

Printed columns: `Address`, `ASN`

### Spec

| Field | Type | Default | Required | Description |
|-------|------|---------|----------|-------------|
| `address` | `string` | — | Yes | IPv6 address of this BGP speaker. Must be a valid IPv6 address (format: `ipv6`). |
| `asNumber` | `uint32` | — | Yes | AS number this endpoint belongs to. Range: 1–4294967295. |
| `addressFamilies` | `[]AddressFamily` | IPv6 unicast | No | Address families this endpoint supports. See [AddressFamily](#addressfamily) above. |

### Status

| Field | Type | Description |
|-------|------|-------------|
| `conditions` | `[]metav1.Condition` | Current state of the endpoint. No conditions are currently defined; reserved for future use. |

### Example

```yaml
apiVersion: bgp.miloapis.com/v1alpha1
kind: BGPEndpoint
metadata:
  name: node-worker-1    # typically matches the Kubernetes node name
  labels:
    topology.example.com/region: us-east
    topology.example.com/cluster: prod-1
spec:
  address: "2001:db8::1" # node's primary IPv6 address
  asNumber: 65001
  addressFamilies:
    - afi: IPv6
      safi: Unicast
```

> [!NOTE]
>
> Labels on `BGPEndpoint` resources are how `BGPPeeringPolicy` selectors drive topology. Use labels to express topology attributes (region, cluster, tier) that peering policies should target.

---

## BGPSession

`BGPSession` declares a BGP peering relationship between two `BGPEndpoint` resources. Each session names a local endpoint and a remote endpoint by their resource names.

Sessions are created by `BGPPeeringPolicy` controllers or manually by platform operators. Each node's BGP controller instance reconciles all `BGPSession` resources where `spec.localEndpoint` matches the value of its `--local-endpoint` flag, translating them into GoBGP `AddPeer`/`UpdatePeer` calls.

```bash
kubectl get bgpsessions
kubectl get bgpsess        # short name
```

Printed columns: `Local`, `Remote`, `Session`, `RX Prefixes`

### Spec

| Field | Type | Default | Required | Description |
|-------|------|---------|----------|-------------|
| `localEndpoint` | `string` | — | Yes | Name of the local `BGPEndpoint` resource. The controller instance running with `--local-endpoint` matching this value will reconcile this session. |
| `remoteEndpoint` | `string` | — | Yes | Name of the remote `BGPEndpoint` resource. The remote peer's address and AS number are read from this endpoint. |
| `holdTime` | `int32` | `90` | No | BGP hold time in seconds. Minimum: 3. |
| `keepaliveTime` | `int32` | `30` | No | BGP keepalive interval in seconds. Minimum: 1. |
| `routeReflector` | `RouteReflectorConfig` | — | No | When set, configures this session for route reflector client behavior. The local speaker treats the remote peer as a route reflector client. |
| `ebgpConfig` | `EBGPSessionConfig` | — | No | eBGP-specific session parameters. Only meaningful when the local and remote endpoints have different AS numbers. |

#### RouteReflectorConfig

| Field | Type | Default | Required | Description |
|-------|------|---------|----------|-------------|
| `clusterID` | `string` | — | Yes | Route reflector cluster ID in dotted-decimal IPv4 notation (BGP convention, e.g. `10.0.0.1`). |

#### EBGPSessionConfig

| Field | Type | Default | Required | Description |
|-------|------|---------|----------|-------------|
| `multiHop` | `EBGPMultiHop` | — | No | Enables eBGP multi-hop. Mutually exclusive with `ttlSecurity`. |
| `ttlSecurity` | `EBGPTTLSecurity` | — | No | Enables GTSM (Generalized TTL Security Mechanism). Mutually exclusive with `multiHop`. |

#### EBGPMultiHop

| Field | Type | Default | Required | Description |
|-------|------|---------|----------|-------------|
| `ttl` | `uint32` | — | Yes | Maximum number of hops permitted. Range: 1–255. Sets GoBGP `EbgpMultihop.MultihopTtl`. |

#### EBGPTTLSecurity

| Field | Type | Default | Required | Description |
|-------|------|---------|----------|-------------|
| `ttl` | `uint32` | — | Yes | Expected minimum TTL for incoming eBGP packets. Range: 1–255. Sets GoBGP `TtlSecurity.TtlMin`. |

### Status

| Field | Type | Description |
|-------|------|-------------|
| `sessionState` | `string` | Current BGP FSM state as reported by GoBGP, polled every 10 seconds. One of: `Unknown`, `Idle`, `Connect`, `Active`, `OpenSent`, `OpenConfirm`, `Established`. |
| `conditions` | `[]metav1.Condition` | Current state of the session. See [Conditions](#bgpsession-conditions). |
| `receivedPrefixes` | `int64` | Count of prefixes currently received from the remote peer (across all address families). |
| `advertisedPrefixes` | `int64` | Count of prefixes currently advertised to the remote peer. |
| `lastTransitionTime` | `metav1.Time` | Timestamp of the most recent session state change. |
| `flapCount` | `int64` | Number of times this session has transitioned from `Established` to any other state. |

#### BGPSession Conditions

| Type | Meaning |
|------|---------|
| `SessionEstablished` | `True` when the BGP FSM is in `Established` state. `False` in all other states. |
| `Configured` | `True` when the session has been successfully added to GoBGP via `AddPeer`/`UpdatePeer`. |

### Examples

**iBGP session (same AS):**

```yaml
apiVersion: bgp.miloapis.com/v1alpha1
kind: BGPSession
metadata:
  name: node-a--node-b   # convention: local--remote
spec:
  localEndpoint: node-a
  remoteEndpoint: node-b
  holdTime: 90
  keepaliveTime: 30
```

**Route reflector client session:**

```yaml
apiVersion: bgp.miloapis.com/v1alpha1
kind: BGPSession
metadata:
  name: node-a--rr-1
spec:
  localEndpoint: node-a
  remoteEndpoint: rr-1
  routeReflector:
    clusterID: "10.0.0.1"  # identifies the RR cluster
```

**eBGP session between clusters:**

```yaml
apiVersion: bgp.miloapis.com/v1alpha1
kind: BGPSession
metadata:
  name: cluster-a-border--cluster-b-border
spec:
  localEndpoint: cluster-a-border   # AS 65001
  remoteEndpoint: cluster-b-border  # AS 65002
  holdTime: 90
  keepaliveTime: 30
  ebgpConfig:
    multiHop:
      ttl: 2   # peers are one hop apart
```

---

## BGPPeeringPolicy

`BGPPeeringPolicy` automates `BGPSession` creation by selecting `BGPEndpoint` resources via label selectors and creating sessions based on the chosen topology mode. The `PeeringPolicyReconciler` watches for endpoint changes and ensures the desired set of `BGPSession` objects exists.

> [!IMPORTANT]
>
> `BGPPeeringPolicy` creates and manages `BGPSession` resources. If you manually create a session that a policy would also create, the policy controller may conflict with it.

```bash
kubectl get bgppeeringpolicies
kubectl get bgppp              # short name
```

Printed columns: `Mode`, `Endpoints`, `Sessions`

### Spec

| Field | Type | Default | Required | Description |
|-------|------|---------|----------|-------------|
| `selector` | `metav1.LabelSelector` | — | Yes | Selects `BGPEndpoint` resources to include in this policy. |
| `mode` | `string` | `mesh` | No | Topology mode. `mesh` creates a `BGPSession` for every pair of matching endpoints (N*(N-1)/2 sessions for N endpoints). `route-reflector` creates sessions between the designated reflector and each client endpoint. Enum: `mesh`, `route-reflector`. |
| `sessionTemplate` | `BGPSessionTemplate` | — | No | Default hold time and keepalive time for created sessions. |
| `routeReflectorConfig` | `PeeringPolicyRRConfig` | — | No | Required when `mode` is `route-reflector`. Identifies the route reflector endpoint and assigns the cluster ID. |

#### BGPSessionTemplate

| Field | Type | Default | Required | Description |
|-------|------|---------|----------|-------------|
| `holdTime` | `int32` | — | No | BGP hold time in seconds for created sessions. |
| `keepaliveTime` | `int32` | — | No | BGP keepalive interval in seconds for created sessions. |

#### PeeringPolicyRRConfig

| Field | Type | Default | Required | Description |
|-------|------|---------|----------|-------------|
| `reflectorSelector` | `metav1.LabelSelector` | — | Yes | Selects exactly one `BGPEndpoint` to act as the route reflector. If the selector matches more than one endpoint, the `InvalidConfig` condition is set and no sessions are created. |
| `clusterID` | `string` | — | Yes | Route reflector cluster ID in dotted-decimal IPv4 notation. Assigned to all client sessions created by this policy. |

### Status

| Field | Type | Description |
|-------|------|-------------|
| `conditions` | `[]metav1.Condition` | Current state of the policy. See [Conditions](#bgppeeringpolicy-conditions). |
| `matchedEndpoints` | `int32` | Number of `BGPEndpoint` resources currently matching `spec.selector`. |
| `activeSessions` | `int32` | Number of `BGPSession` resources currently created and managed by this policy. |

#### BGPPeeringPolicy Conditions

| Type | Meaning |
|------|---------|
| `InvalidConfig` | The policy spec is invalid — e.g., `route-reflector` mode with a `reflectorSelector` that matches zero or more than one endpoint. When set, no sessions are created or modified. |

### Examples

**Full mesh within a region:**

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
  sessionTemplate:
    holdTime: 90
    keepaliveTime: 30
```

**Route reflector topology:**

```yaml
apiVersion: bgp.miloapis.com/v1alpha1
kind: BGPPeeringPolicy
metadata:
  name: us-east-rr
spec:
  selector:
    matchLabels:
      topology.example.com/region: us-east  # selects all endpoints (reflector + clients)
  mode: route-reflector
  routeReflectorConfig:
    reflectorSelector:
      matchLabels:
        bgp.miloapis.com/role: route-reflector  # must match exactly one endpoint
    clusterID: "10.0.0.1"
```

---

## BGPAdvertisement

`BGPAdvertisement` declares what IPv6 CIDR prefixes the local speaker should advertise into BGP. The `AdvertisementReconciler` injects declared prefixes into the GoBGP RIB using `AddPath`. On deletion, prefixes are withdrawn using `DeletePath`.

> [!NOTE]
>
> BGPAdvertisement is a Phase 2 resource. The schema is registered and the reconciler is running, but this resource sees limited use in the initial alpha deployment.

```bash
kubectl get bgpadvertisements
kubectl get bgpadvert          # short name
```

### Spec

| Field | Type | Default | Required | Description |
|-------|------|---------|----------|-------------|
| `prefixes` | `[]string` | — | Yes | List of IPv6 CIDR prefixes to advertise. Minimum 1 item. Example: `["2001:db8::/32"]`. |
| `peerSelector` | `metav1.LabelSelector` | — | No | Selects which peers this advertisement targets. If omitted, the prefixes are advertised to all peers. |
| `communities` | `[]string` | — | No | BGP community values to attach to advertised routes. Format: `"AS:value"` (e.g. `"65001:100"`). |
| `localPref` | `*uint32` | — | No | LOCAL_PREF attribute value. Only meaningful for iBGP sessions. Minimum: 0. |

### Status

| Field | Type | Description |
|-------|------|-------------|
| `conditions` | `[]metav1.Condition` | Current state of the advertisement. |
| `advertisedPrefixCount` | `int32` | Number of prefixes currently present in the GoBGP RIB. |

### Example

```yaml
apiVersion: bgp.miloapis.com/v1alpha1
kind: BGPAdvertisement
metadata:
  name: node-1-srv6-prefix
spec:
  prefixes:
    - "2001:db8:ff00::/48"   # this node's SRv6 prefix block
  communities:
    - "65001:100"            # internal routing community
  localPref: 100             # prefer this path for iBGP
```

---

## BGPRoutePolicy

`BGPRoutePolicy` declares import or export filtering rules applied to BGP sessions. The `RoutePolicyReconciler` applies the policy to GoBGP's policy table using `AddPolicy`/`DeletePolicy`. Statements are evaluated in order; the first match wins.

> [!NOTE]
>
> BGPRoutePolicy is a Phase 2 resource. The schema is registered and the reconciler is running, but this resource sees limited use in the initial alpha deployment.

```bash
kubectl get bgproutepolicies
kubectl get bgprp              # short name
```

### Spec

| Field | Type | Default | Required | Description |
|-------|------|---------|----------|-------------|
| `type` | `string` | — | Yes | Whether this is an `Import` or `Export` policy. Enum: `Import`, `Export`. |
| `peerSelector` | `metav1.LabelSelector` | — | No | Selects which peers this policy applies to. If omitted, the policy applies to all peers. |
| `statements` | `[]PolicyStatement` | — | Yes | Ordered list of policy rules. Minimum 1 item. First match wins. |

#### PolicyStatement

| Field | Type | Default | Required | Description |
|-------|------|---------|----------|-------------|
| `prefixSet` | `[]PrefixMatch` | — | No | Set of prefixes to match. If empty, the statement matches all prefixes. |
| `action` | `string` | — | Yes | Action when the statement matches. Enum: `Accept`, `Reject`. |

#### PrefixMatch

| Field | Type | Default | Required | Description |
|-------|------|---------|----------|-------------|
| `cidr` | `string` | — | Yes | Prefix to match, e.g. `"2001:db8:ff00::/40"`. |
| `maskLengthMin` | `*uint32` | — | No | Match only prefixes with mask length >= this value. Use with `maskLengthMax` to express a range. |
| `maskLengthMax` | `*uint32` | — | No | Match only prefixes with mask length <= this value. |

### Status

| Field | Type | Description |
|-------|------|-------------|
| `conditions` | `[]metav1.Condition` | Current state of the policy. |

### Example

Reject more-specific /48 prefixes when an aggregate /40 covers them (prevents leaking individual host routes when the aggregate is already being advertised):

```yaml
apiVersion: bgp.miloapis.com/v1alpha1
kind: BGPRoutePolicy
metadata:
  name: reject-specifics-under-aggregate
spec:
  type: Export
  statements:
    - prefixSet:
        - cidr: "2001:db8:ff00::/40"
          maskLengthMin: 48   # match /48 and longer within this /40
          maskLengthMax: 128
      action: Reject
    - action: Accept          # accept everything else
```

---

## Planned Resources (Future)

The following resources are planned for a future release to support SRv6 VPN overlays per RFC 9252. They are not present in `v1alpha1`.

### BGPVPN (planned)

Declares a VPN instance with route targets and SRv6 behavior. Encodes VPN membership and the SRv6 behavior type (e.g., End.DT6) for route advertisement.

### BGPVPNRoute (planned)

Declares a VPN route advertisement within a `BGPVPN` instance. Associates a prefix with a VPN and an SRv6 SID.

---

## Common Patterns

### Labeling Endpoints for Policy Selection

`BGPPeeringPolicy` selectors are the primary mechanism for topology automation. Establish a labeling convention for your endpoints:

```yaml
# Topology location
topology.example.com/region: us-east
topology.example.com/zone: us-east-1a
topology.example.com/cluster: prod-1

# BGP role
bgp.miloapis.com/role: route-reflector  # or: client, border
```

### Naming Conventions

| Resource | Convention | Example |
|----------|------------|---------|
| `BGPConfiguration` | `default` (one per cluster) | `default` |
| `BGPEndpoint` | Kubernetes node name | `node-worker-1` |
| `BGPSession` | `{local}--{remote}` | `node-a--node-b` |
| `BGPPeeringPolicy` | `{scope}-{mode}` | `us-east-mesh` |
| `BGPAdvertisement` | `{owner}-{description}` | `node-1-srv6-prefix` |
| `BGPRoutePolicy` | descriptive | `reject-specifics-under-aggregate` |

### Verifying Configuration

After applying resources, verify the BGP controller has reconciled them:

```bash
# Check speaker is ready
kubectl get bgpconfig default -o jsonpath='{.status.conditions[?(@.type=="SpeakerReady")].status}'

# Check all sessions
kubectl get bgpsessions -o wide

# Watch session state changes
kubectl get bgpsessions -w

# Check a specific session's full status
kubectl get bgpsession node-a--node-b -o yaml | grep -A 20 status:

# Check peering policy created the expected sessions
kubectl get bgppp us-east-mesh -o jsonpath='{.status}'
```
