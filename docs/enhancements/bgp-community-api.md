---
status: proposed
stage: alpha
---

# Enhancement: BGP Community API

> Last verified: 2026-04-23 against main

## Table of Contents

- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [Resource Surface](#resource-surface)
  - [`BGPCommunity`](#bgpcommunity)
  - [`BGPAdvertisement` extensions](#bgpadvertisement-extensions)
  - [`BGPCommunityAttachment`](#bgpcommunityattachment)
  - [`BGPRoutePolicy` extensions](#bgproutepolicy-extensions)
  - [End-to-end example](#end-to-end-example)
- [Design Details](#design-details)
  - [Typed community values](#typed-community-values)
  - [Refs vs. selector semantics](#refs-vs-selector-semantics)
  - [Deprecation of the raw-string placeholder](#deprecation-of-the-raw-string-placeholder)
  - [Controller responsibilities](#controller-responsibilities)
  - [RBAC split](#rbac-split)
- [User Stories](#user-stories)
- [Drawbacks](#drawbacks)
- [Alternatives Considered](#alternatives-considered)
- [Open Questions](#open-questions)
- [Prior Art](#prior-art)

---

## Summary

This enhancement proposes a named, typed API for BGP communities in the
`bgp.miloapis.com/v1alpha1` group. It introduces two new cluster-scoped
kinds — `BGPCommunity` (a typed, named community value) and
`BGPCommunityAttachment` (selector-driven tagging of advertisements owned by
others) — and extends `BGPAdvertisement` and `BGPRoutePolicy` with
community-aware references, selectors, match conditions, and a mutating
`setCommunities` action. The existing
`BGPAdvertisement.spec.communities: []string` placeholder
(`api/v1alpha1/bgpadvertisement_types.go:35-38`) is deprecated in favour of
the typed surface.

Grouping is handled the same way it is elsewhere in this repo: via
`metav1.LabelSelector` over named objects. There is no
`BGPCommunitySet` resource — a selector over labelled `BGPCommunity`
objects covers the same use case without adding a second grouping mechanism.

## Motivation

The control plane already has a `BGPAdvertisement` resource with a
raw-string `communities: ["AS:value"]` field, explicitly marked as a
"Phase 2+ extensibility placeholder"
(`api/v1alpha1/bgpadvertisement_types.go:35-38`). The string list has no
registry, no typing (standard vs. large vs. extended vs. well-known), and
`BGPRoutePolicy.statements` cannot match on communities — only on prefix
and mask ranges (`api/v1alpha1/bgroutepolicy_types.go:42-68`).

Consumers want to **classify** routes (internal vs. external, tenant scope,
traffic-engineering hints, do-not-leak markers) and have that
classification propagate to the eBGP border, where a border policy rejects
anything tagged "internal" from being leaked. That requires three things
the current API cannot express cleanly:

1. A **named registry** so authors reference `internal-route` instead of
   memorising `65001:100`, and so operators can audit every community in
   use.
2. **Typed community values** covering standard (RFC 1997), large
   (RFC 8092), extended (RFC 4360), and well-known (`NO_EXPORT`,
   `NO_ADVERTISE`, `NO_EXPORT_SUBCONFED`).
3. **Decoupled attribution**: both inline on a `BGPAdvertisement` for the
   common case, and out-of-band via a selector so a classification team can
   tag advertisements owned by other teams without write access to them.

### Goals

- A named, cluster-scoped registry of communities that can be referenced
  by label or name from advertisements and policies.
- Typed one-of values for the four community families: standard, large,
  extended, well-known.
- A path to attach communities to rendered routes both inline (on
  `BGPAdvertisement`) and out-of-band (via `BGPCommunityAttachment`).
- Community-aware match and mutate in `BGPRoutePolicy`.
- Fit the repo's existing idioms: cluster scope, `metav1.LabelSelector`,
  name-based refs, storage version = alpha.

### Non-Goals

- RPKI / ROV, MD5 / TCP-AO, or any other authentication or validation
  concern.
- Any data-plane change. This is purely control-plane metadata on rendered
  routes.
- VPN import/export targets. Route-target is supported as an extended
  community *value*, but the planned `BGPVPN` topology work is out of scope
  here.
- A second grouping mechanism beyond label selectors. There is no
  `BGPCommunitySet` (see [Alternatives Considered](#alternatives-considered)).

## Proposal

### Resource Surface

| Kind | Purpose | Scope |
|---|---|---|
| `BGPCommunity` | Registers a named, typed community value. | Cluster |
| `BGPCommunityAttachment` | Attaches communities to advertisements selected by label, without requiring write on the advertisement. | Cluster |
| *(extend)* `BGPAdvertisement` | Adds `communityRefs` and `communitySelector`; deprecates raw `communities`. | — |
| *(extend)* `BGPRoutePolicy` | Adds community match on statements and a mutating `setCommunities` action. | — |

All new kinds are cluster-scoped, consistent with existing BGP CRDs in
this repo.

### `BGPCommunity`

A `BGPCommunity` registers a single community value under a human-readable
name. It carries labels so that selectors — rather than a dedicated set
resource — provide grouping.

The spec is a type discriminator with a one-of value block.

**Standard (RFC 1997):**

```yaml
apiVersion: bgp.miloapis.com/v1alpha1
kind: BGPCommunity
metadata:
  name: internal-route
  labels:
    bgp.miloapis.com/class: internal
    bgp.miloapis.com/team: platform
spec:
  type: Standard
  standard:
    asn: 65001
    value: 100
```

**Large (RFC 8092):**

```yaml
apiVersion: bgp.miloapis.com/v1alpha1
kind: BGPCommunity
metadata:
  name: tenant-a-externally-visible
  labels:
    bgp.miloapis.com/class: external
    bgp.miloapis.com/tenant: tenant-a
spec:
  type: Large
  large:
    globalAdmin: 65001
    localData1: 2
    localData2: 100
```

**Extended / Route Target (RFC 4360):**

```yaml
apiVersion: bgp.miloapis.com/v1alpha1
kind: BGPCommunity
metadata:
  name: rt-tenant-a
  labels:
    bgp.miloapis.com/class: route-target
spec:
  type: Extended
  extended:
    subType: RouteTarget   # RouteTarget | RouteOrigin
    asn: 65001
    value: 100
```

**Well-known (RFC 1997 §5):**

```yaml
apiVersion: bgp.miloapis.com/v1alpha1
kind: BGPCommunity
metadata:
  name: no-export
  labels:
    bgp.miloapis.com/class: well-known
spec:
  type: WellKnown
  wellKnown: NoExport      # NoExport | NoAdvertise | NoExportSubconfed
```

Validation:

- Exactly one of `standard`, `large`, `extended`, `wellKnown` must be set,
  matching `type`. Enforced via CEL rule on the spec.
- `standard.asn`, `standard.value`: `uint16`.
- `large.*`: `uint32`.
- `extended.asn` / `extended.value`: ranges per RFC 4360 sub-type.

### `BGPAdvertisement` extensions

Two new fields on `BGPAdvertisementSpec`. Both are optional and additive;
the union of resolved communities from `communityRefs`,
`communitySelector`, and any matching `BGPCommunityAttachment` is applied
to the rendered routes.

**By explicit refs (common case):**

```yaml
apiVersion: bgp.miloapis.com/v1alpha1
kind: BGPAdvertisement
metadata:
  name: tenant-a-services
  labels:
    bgp.miloapis.com/tenant: tenant-a
spec:
  prefixes: ["2001:db8:a::/48"]
  peerSelector:
    matchLabels:
      bgp.miloapis.com/role: border
  communityRefs:
    - name: tenant-a-externally-visible
    - name: no-export
```

**By selector over labelled communities (grouping without a set resource):**

```yaml
apiVersion: bgp.miloapis.com/v1alpha1
kind: BGPAdvertisement
metadata:
  name: platform-internal
spec:
  prefixes: ["2001:db8:0::/48"]
  communitySelector:
    matchLabels:
      bgp.miloapis.com/class: internal
```

The existing `communities: []string` field remains but is deprecated (see
[Deprecation of the raw-string placeholder](#deprecation-of-the-raw-string-placeholder)).

### `BGPCommunityAttachment`

`BGPCommunityAttachment` exists to tag advertisements a team does not own.
It is deliberately additive: it can only *add* communities to rendered
routes; it cannot remove them. Removal belongs in `BGPRoutePolicy`.

**Mark all tenant-a advertisements as internally classified:**

```yaml
apiVersion: bgp.miloapis.com/v1alpha1
kind: BGPCommunityAttachment
metadata:
  name: classify-tenant-a-as-internal
spec:
  advertisementSelector:
    matchLabels:
      bgp.miloapis.com/tenant: tenant-a
  communityRefs:
    - name: internal-route
    - name: do-not-leak
```

**By selector (every `class=internal` community gets attached to any
advertisement labelled `scope=private`):**

```yaml
apiVersion: bgp.miloapis.com/v1alpha1
kind: BGPCommunityAttachment
metadata:
  name: tag-private-as-internal
spec:
  advertisementSelector:
    matchLabels:
      bgp.miloapis.com/scope: private
  communitySelector:
    matchLabels:
      bgp.miloapis.com/class: internal
```

### `BGPRoutePolicy` extensions

Two extensions to `PolicyStatement` (`api/v1alpha1/bgroutepolicy_types.go:42-52`):

1. A `communityMatch` block for matching imported or exported routes by
   the communities they carry.
2. A `setCommunities` block for adding or removing communities as a
   mutating action.

**Reject at border anything carrying the `do-not-leak` community:**

```yaml
apiVersion: bgp.miloapis.com/v1alpha1
kind: BGPRoutePolicy
metadata:
  name: border-no-leak
spec:
  type: Export
  peerSelector:
    matchLabels:
      bgp.miloapis.com/role: border
  statements:
    - communityMatch:
        matchType: Any                  # Any | All
        communityRefs:
          - name: do-not-leak
      action: Reject
    - action: Accept
```

**Reject at border any route tagged `class=internal` (using the selector
to name a group without a set resource):**

```yaml
apiVersion: bgp.miloapis.com/v1alpha1
kind: BGPRoutePolicy
metadata:
  name: border-reject-internal
spec:
  type: Export
  peerSelector:
    matchLabels:
      bgp.miloapis.com/role: border
  statements:
    - communityMatch:
        matchType: Any
        communitySelector:
          matchLabels:
            bgp.miloapis.com/class: internal
      action: Reject
    - action: Accept
```

**Tag on ingress: everything learned from an upstream peer is marked
`external-route`:**

```yaml
apiVersion: bgp.miloapis.com/v1alpha1
kind: BGPRoutePolicy
metadata:
  name: ingress-tag-external
spec:
  type: Import
  peerSelector:
    matchLabels:
      bgp.miloapis.com/role: upstream
  statements:
    - setCommunities:
        add:
          - name: external-route
      action: Accept
```

**Strip classification on egress so internal markers never leave the AS:**

```yaml
apiVersion: bgp.miloapis.com/v1alpha1
kind: BGPRoutePolicy
metadata:
  name: border-strip-markers
spec:
  type: Export
  peerSelector:
    matchLabels:
      bgp.miloapis.com/role: border
  statements:
    - setCommunities:
        remove:
          - name: internal-route
          - name: do-not-leak
      action: Accept
```

### End-to-end example

```yaml
# 1. Register communities.
---
apiVersion: bgp.miloapis.com/v1alpha1
kind: BGPCommunity
metadata:
  name: internal-route
  labels:
    bgp.miloapis.com/class: internal
spec:
  type: Standard
  standard: { asn: 65001, value: 100 }
---
apiVersion: bgp.miloapis.com/v1alpha1
kind: BGPCommunity
metadata:
  name: do-not-leak
  labels:
    bgp.miloapis.com/class: internal
spec:
  type: Standard
  standard: { asn: 65001, value: 101 }
---
# 2. Tenant team publishes advertisements with no community knowledge.
apiVersion: bgp.miloapis.com/v1alpha1
kind: BGPAdvertisement
metadata:
  name: tenant-a-services
  labels:
    bgp.miloapis.com/tenant: tenant-a
    bgp.miloapis.com/scope: private
spec:
  prefixes: ["2001:db8:a::/48"]
---
# 3. Network team classifies tenant-a traffic as internal,
#    without write on the advertisement itself.
apiVersion: bgp.miloapis.com/v1alpha1
kind: BGPCommunityAttachment
metadata:
  name: classify-private-as-internal
spec:
  advertisementSelector:
    matchLabels:
      bgp.miloapis.com/scope: private
  communitySelector:
    matchLabels:
      bgp.miloapis.com/class: internal
---
# 4. Border enforces: no internal route leaks.
apiVersion: bgp.miloapis.com/v1alpha1
kind: BGPRoutePolicy
metadata:
  name: border-reject-internal
spec:
  type: Export
  peerSelector:
    matchLabels:
      bgp.miloapis.com/role: border
  statements:
    - communityMatch:
        matchType: Any
        communitySelector:
          matchLabels:
            bgp.miloapis.com/class: internal
      action: Reject
    - action: Accept
```

No team touches another's resource. The tenant owns the advertisement, the
network team owns classification and border policy, and the registry
(`BGPCommunity`) is a shared reference catalog.

## Design Details

### Typed community values

`BGPCommunitySpec` is a discriminated union: a `type` enum plus a
matching value block. This is validated with a CEL rule on the spec
object rather than with kubebuilder-level one-of, since Kubernetes does
not yet have a native one-of. The wire encoding on the BGP session is
determined by `type`; the controller never has to guess what family a
community belongs to.

Well-known communities (`NoExport`, `NoAdvertise`, `NoExportSubconfed`)
are modelled as first-class `BGPCommunity` objects rather than a parallel
`wellKnown: true` flag on advertisements. This keeps one reference
mechanism throughout: a community is something you name.

### Refs vs. selector semantics

Every resource that attaches or matches communities accepts both
`communityRefs` (explicit names) and `communitySelector`
(`metav1.LabelSelector` over `BGPCommunity.metadata.labels`). Both may be
set; the effective community set is their union. This mirrors the
existing repo idiom — `BGPAdvertisement.peerSelector`,
`BGPRoutePolicy.peerSelector` — and removes the need for a dedicated set
resource.

For `BGPRoutePolicy.communityMatch`, a `matchType` of `Any` or `All`
controls whether a route must carry at least one or every resolved
community to match.

### Deprecation of the raw-string placeholder

`BGPAdvertisementSpec.Communities []string`
(`api/v1alpha1/bgpadvertisement_types.go:35-38`) is kept for one release
with a `// Deprecated:` comment and a CEL validation rule that rejects
setting both `communities` and any of the new fields on the same object.
At the next API version bump the field is removed.

### Controller responsibilities

- `BGPCommunity`: no controller action beyond object validation. It is a
  pure registry. Deletion is blocked by a finalizer if any other object
  still references it by name, to avoid dangling refs.
- `BGPCommunityAttachment`: a new reconciler lists advertisements
  matching `advertisementSelector` and resolves the community union.
  The resolution is cached and exposed in the status of the attachment
  (`status.matchedAdvertisements`, `status.resolvedCommunityCount`) for
  observability.
- `BGPAdvertisement`: the existing advertisement reconciler is extended
  to resolve `communityRefs`, `communitySelector`, and any
  `BGPCommunityAttachment` union into the community list it sends into
  GoBGP at render time.
- `BGPRoutePolicy`: the policy reconciler resolves communities at the
  same point it resolves `prefixSet` today, and translates
  `communityMatch` + `setCommunities` into the GoBGP policy API.

### RBAC split

- **Registry owner** (platform team): `create/update/delete` on
  `BGPCommunity`.
- **Advertisement author** (tenant team): `create/update` on
  `BGPAdvertisement`; `get/list` on `BGPCommunity` only.
- **Classifier** (central network team): `create/update` on
  `BGPCommunityAttachment`; `get/list` on `BGPAdvertisement` and
  `BGPCommunity`; no write on either.
- **Policy owner** (border/edge team): `create/update` on
  `BGPRoutePolicy`; `get/list` on `BGPCommunity`.

This split is the main operational reason `BGPCommunityAttachment`
exists as a separate resource instead of being another field on
`BGPAdvertisement`.

## User Stories

**Registry catalog.** A platform operator defines
`BGPCommunity/internal-route`, `/do-not-leak`, `/external-route`, and
`/no-export` once, each labelled with `bgp.miloapis.com/class=…`.
Consumers reference by name; audits run
`kubectl get bgpcommunity -l bgp.miloapis.com/class=internal` and see the
full set.

**Decoupled classification.** A tenant team ships
`BGPAdvertisement/tenant-a-services` carrying no community metadata. The
central network team creates a `BGPCommunityAttachment` selecting
`tenant=tenant-a` and attaching the `class=internal` community set. The
tenant team does not need to learn the community API; the network team
does not need write access on the tenant's advertisements.

**Border enforcement.** One cluster-wide
`BGPRoutePolicy/border-reject-internal` on
`bgp.miloapis.com/role=border` peers rejects any export carrying a
`class=internal` community, preventing leakage even if a tenant
accidentally attaches the community themselves or the classification
controller tags a new advertisement.

## Drawbacks

- **New reconciler.** `BGPCommunityAttachment` needs a controller that
  watches advertisements and communities and recomputes on label churn.
  This is non-zero operational cost.
- **Resolution failure mode.** A `communityRefs: [{name: foo}]` that
  references a non-existent `BGPCommunity` has to fail explicitly — a
  status condition on the owning object (`AdvertisementReady=false`
  with reason `UnresolvedCommunityRef`) rather than silently dropping.
- **Mutating policy action.** `setCommunities` is the first mutation in
  `BGPRoutePolicy` — every statement before this proposal is purely
  filter-and-drop. That makes "what communities does route X actually
  carry at egress?" a rendered-at-reconcile question, not a static-read
  question. The answer lives in the session state, not the CRD.
- **Additive-only attachments.** Because `BGPCommunityAttachment` cannot
  remove, an accidental label change that matches a "sticky"
  classification attachment will re-tag an advertisement on every
  reconcile until the labels are fixed. This is by design — subtraction
  belongs in policy — but it is a footgun worth documenting.

## Alternatives Considered

**Keep `communities: []string`, add a validation regex.** Rejected: no
typing for large or extended communities, no catalog for audit, no RBAC
split, no selector idiom.

**Inline struct on `BGPAdvertisement`, no registry.** Rejected: cannot
share a community across advertisements, cannot audit
"what communities exist in this cluster", and re-introduces the typo
surface on every advertisement.

**A dedicated `BGPCommunitySet` resource for grouping.** Rejected. Label
selectors over `BGPCommunity` objects cover the same use case — group
by `matchLabels: {class: internal}` anywhere communities are accepted —
and do so using the idiom already present throughout this repo
(`BGPAdvertisement.peerSelector`, `BGPRoutePolicy.peerSelector`,
`BGPPeeringPolicy.selector`). A set resource would add a second grouping
mechanism, a second reconciler, a second RBAC kind, a second
dangling-ref failure mode, and composition questions (can a set
reference another set?) without enabling anything a selector cannot
already express. OpenConfig and vendor CLIs have community-sets because
YANG and CLI config have no native selector concept; Kubernetes does.
Cilium BGP v2 similarly has no standalone set — advertisements and
policies carry inline community lists or selectors. If real usage shows
that selectors are too implicit or too expensive to evaluate, a set
resource can be added later without breaking the refs/selector surface.

**Borrow OpenConfig `routing-policy` YANG wholesale.** Rejected as too
verbose for the Kubernetes-native conventions elsewhere in this repo.
The proposal is inspired by OpenConfig's community-set + match-any/all +
set-community shape but simplified.

**Vendor-CLI-style community-lists.** Rejected as non-idiomatic for
CRDs; label selectors cover the same ground declaratively.

## Open Questions

1. **Mutating policy action.** Is `setCommunities.add`/`.remove`
   desirable in v1, or should mutation be deferred to a follow-up to
   keep `BGPRoutePolicy` filter-only? Leaning toward shipping it:
   stripping classification at the border is a concrete, already-named
   user story, and GoBGP supports it natively.
2. **Extended communities in v1 vs. with `BGPVPN`.** Route-target
   values are the main user of extended communities, and that user is
   VPN import/export. One option is to gate `type: Extended` until the
   `BGPVPN` design lands. Leaning toward shipping the type now with a
   note that VPN semantics are out of scope.
3. **Dangling-ref behaviour.** Finalizer that blocks `BGPCommunity`
   deletion while referenced (strong consistency, awkward ops), or
   allow deletion and surface `UnresolvedCommunityRef` on every
   dependent object (permissive, more noise)? Default proposal is
   finalizer; revisit if it proves annoying.

## Prior Art

- Existing codebase idioms: `BGPAdvertisement.peerSelector`,
  `BGPRoutePolicy.peerSelector`, `BGPPeeringPolicy.selector`, and the
  `prefixSet` field on `PolicyStatement`
  (`api/v1alpha1/bgroutepolicy_types.go:42-68`).
- RFC 1997 (standard communities), RFC 1998 (common
  well-known-community usage patterns), RFC 4360 (extended
  communities), RFC 8092 (large communities).
- OpenConfig / IETF `routing-policy` YANG (RFC 9067) for
  community-set, match-any/all, set-community semantics.
- Cilium BGP v2 `CiliumBGPAdvertisement` for the typed
  standard/large/well-known split, including its deliberate choice not
  to introduce a separate community-set resource.
- Calico `BGPFilter` for community-match on policy rules.
- MetalLB `BGPAdvertisement.communities: []string` as the
  anti-pattern this enhancement replaces.
