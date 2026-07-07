# AGENTS.md

This file provides guidance to AI assistants when working with code in this repository.

## What this project is

Cloud defines Kubernetes CRDs and API types for virtual networking. It is **API-only** — no controllers, no binaries, no runtime. Implementations consume these APIs; this repo just defines the contract.

Module: `go.datum.net/cloud`

## Commands

This project uses [Task](https://taskfile.dev/) (`task`), not `make`.

```bash
task install      # Install all dev tools into ./bin/ (run first)
task build        # go fmt + go vet + go build ./...
task test:unit    # Run unit tests
task test:e2e     # Run e2e tests (kind + chainsaw)
task test         # Run unit tests then e2e tests
task lint         # golangci-lint + yamlfmt + .yml extension check
task lint-fix     # Same with auto-fix applied
task generate     # Run all generators: deepcopy, CRDs, docs
task ci           # Full local pipeline: build -> lint -> test:unit -> test:e2e
task clean        # Remove ./bin/ and cover.out
```

Run a single unit test package:
```bash
GOOS=linux go test ./api/v1alpha1/... -run TestName -v
```

All dev tools (golangci-lint, controller-gen, yamlfmt, chainsaw) are installed locally to `./bin/` — they are never installed system-wide.

## Architecture

### API groups

| Group                 | Version    | Resources                  |
|-----------------------|------------|----------------------------|
| `cloud.datumapis.com` | `v1alpha1` | VPC, VPCAttachment         |

Source lives in `api/v1alpha1/`. Each resource has its own `*_types.go` file.

### Key invariants

- **Kubernetes 1.28+ required** — CEL functions `isIP()` and `isCIDR()` are used for field validation.
- **YAML files must use `.yaml` extension**, never `.yml` — the lint task enforces this.

### Code generation

After changing kubebuilder markers (`// +kubebuilder:...`) or adding new types, run `task generate`.

**Never hand-edit generated files.** The following are produced by tooling and must only be modified via their generators:

| Generated artifact | Generator | Source of truth |
|--------------------|-----------|-----------------|
| `api/*/zz_generated.deepcopy.go` | `controller-gen object` | Go types + markers |
| `config/crd/*.yaml` | `controller-gen crd` | Go types + markers |
| `docs/api/vpc.md` | `crd-ref-docs` | Go types + `.crd-ref-docs.yaml` |

## Architecture Reference

See [ARCHITECTURE.md](docs/agents/ARCHITECTURE.md) for a full architecture reference including module layout, package roles, and known constraints.

## Conventions Reference

See [CONVENTIONS.md](docs/agents/CONVENTIONS.md) for coding standards, naming rules, kubebuilder marker patterns, and Go-specific conventions.

## Docs

- `docs/api/vpc.md` — full VPC CRD field reference
