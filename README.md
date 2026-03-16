# BGP

Kubernetes-native BGP control plane powered by [GoBGP](https://github.com/osrg/gobgp).

## Overview

BGP is a declarative, CRD-driven BGP control plane for Kubernetes. It manages
BGP topology through standard Kubernetes resources — endpoints declare speaker
identity, sessions declare peering relationships, and policies automate topology.
The controller reconciles these resources against a local GoBGP instance and
programs learned routes into the kernel.

**API Group:** `bgp.miloapis.com/v1alpha1`

## Status

This project is in early development (`v1alpha1`). APIs may change.

## License

Apache License 2.0 — see [LICENSE](LICENSE) for details.
