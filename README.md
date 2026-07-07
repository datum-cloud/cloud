# Services

Kubernetes CRDs for virtual networking — API-only, no controller.

**API group:** `cloud.datumapis.com/v1alpha1`
**Stability:** Alpha
**Requires:** Kubernetes 1.28+

---

## What it is

Services defines Kubernetes Custom Resource Definitions for virtual tenant networking. It ships type definitions, validation rules, and CRD manifests — no controller, no runtime, no binaries. External implementations import this module to register these types and reconcile the resources.

## Resources

| Resource        | Kind          | Description                                            |
|-----------------|---------------|--------------------------------------------------------|
| `VPC`           | `vpcs`        | Virtual network with one or more IPv4/IPv6 CIDR blocks |
| `VPCAttachment` | `vpcattachments` | Binds a network interface to a VPC with addresses    |

A `VPC` defines a set of CIDR prefixes. A `VPCAttachment` connects a workload interface to that VPC:

```yaml
apiVersion: cloud.datumapis.com/v1alpha1
kind: VPC
metadata:
  name: tenant-a
  namespace: default
spec:
  networks:
    - "10.100.0.0/24"
    - "fd00:a::/48"
---
apiVersion: cloud.datumapis.com/v1alpha1
kind: VPCAttachment
metadata:
  name: tenant-a-node-1
  namespace: default
spec:
  vpc:
    name: tenant-a
  interface:
    name: eth0
    addresses:
      - "10.100.0.5"
      - "fd00:a::5"
```

## Quick start

```bash
kubectl apply -k config/crd
```

## Development

```bash
task install      # Install all dev tools into ./bin/
task build        # go fmt + go vet + go build ./...
task test         # Run unit tests then e2e tests
task test:unit    # Run unit tests only
task lint         # golangci-lint + yamlfmt + .yml extension check
task lint-fix     # Auto-fix lint issues
task generate     # Regenerate deepcopy methods, CRD manifests, and API docs
task ci           # Full pipeline: build + lint + unit + e2e
task clean        # Remove ./bin/ and cover.out
```

## Documentation

- [VPC API reference](docs/api/vpc.md) — field definitions, validation rules
- [Architecture](docs/agents/ARCHITECTURE.md) — module layout, data flow
- [Conventions](docs/agents/CONVENTIONS.md) — naming, markers, code style

## License

[AGPL-3.0](LICENSE)
