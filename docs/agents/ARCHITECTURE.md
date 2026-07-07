# Architecture

> Cloud is a Go module that defines Kubernetes CRDs and API types for virtual networking. It is consumed as a library by external controller implementations; this repository contains no runtime code.

_Last updated: 2026-07-07_

---

## Table of Contents

1. [Overview](#overview)
2. [Repository Layout](#repository-layout)
3. [Module / Package Reference](#module--package-reference)
   - [api/v1alpha1](#apiv1alpha1)
4. [API Resource Model](#api-resource-model)
   - [VPC Group](#vpc-group-clouddatumapiscom)
5. [Entry Points](#entry-points)
6. [External Dependencies](#external-dependencies)
7. [CI/CD](#cicd)
8. [Known Constraints & Tech Debt](#known-constraints--tech-debt)
9. [For Claude](#for-claude)

---

## Overview

Cloud defines the Kubernetes Custom Resource Definitions (CRDs) and Go API types for virtual networking:

- **Virtual networking** (`cloud.datumapis.com`) — abstractions for virtual private clouds and interface attachments

The module is a **library**, not a binary. External controllers import `go.datum.net/cloud` to register these types with their scheme and reconcile the resources. It ships only the type definitions, validation rules, CRD YAML, and their tests. There are no controllers, no HTTP servers, no goroutines, and no persistent state within this module.

The design follows standard Kubernetes API conventions: resources have a `Spec` (desired state), a `Status` (observed state), and standard `metav1.Condition` conditions. CEL validation rules embedded in kubebuilder markers enforce invariants at the API server level (requiring Kubernetes 1.28+).

---

## Repository Layout

```
cloud/
├── api/
│   └── v1alpha1/               # cloud.datumapis.com/v1alpha1 — 2 CRD types + generated deepcopy
├── config/
│   └── crd/                    # Generated CRD YAML (controller-gen output — never edit directly)
├── docs/
│   ├── api/                    # Human-readable field reference (vpc.md)
│   └── agents/                 # Architecture and conventions reference
├── hack/
│   └── boilerplate.go.txt      # Copyright header injected into generated files
├── test/
│   └── e2e/                    # chainsaw e2e test suite
│       ├── chainsaw-config.yaml
│       ├── Taskfile.yaml
│       └── tests/
├── bin/                        # Local dev tool binaries (gitignored)
├── go.mod                      # Module: go.datum.net/cloud, Go 1.26
├── go.sum
├── Taskfile.yaml               # Primary task runner (build, lint, generate, test)
├── AGENTS.md                   # Project guidance for AI assistants
└── CLAUDE.md -> AGENTS.md      # Symlink: CLAUDE.md -> AGENTS.md
```

---

## Module / Package Reference

### api/v1alpha1

**Purpose:** Defines all Go types and scheme registration for the `cloud.datumapis.com/v1alpha1` API group. Contains two CRD resources.

**Key files:**

| File                       | Contents                                                                                                                    |
|----------------------------|-----------------------------------------------------------------------------------------------------------------------------|
| `groupversion_info.go`     | `GroupVersion`, `SchemeBuilder`, `AddToScheme`                                                                              |
| `vpc_types.go`             | `VPC`, `VPCSpec`, `VPCStatus`, `Network`                                                                                    |
| `vpcattachment_types.go`   | `VPCAttachment`, `VPCAttachmentSpec`, `VPCAttachmentStatus`, `VPCRef`, `VPCAttachmentInterface`, `VPCAttachmentAnnotation` |
| `zz_generated.deepcopy.go` | Generated `DeepCopy*` methods — do not edit                                                                                 |

**External dependencies:**
- `k8s.io/apimachinery/pkg/apis/meta/v1` — `metav1.TypeMeta`, `metav1.ObjectMeta`
- `sigs.k8s.io/controller-runtime/pkg/scheme` — `scheme.Builder`

**Owns persistent state:** No.

---

## API Resource Model

### VPC Group (`cloud.datumapis.com`)

**VPC** — `vpcs.cloud.datumapis.com`

A virtual private cloud defined by CIDR blocks.

Key spec fields:
- `networks []Network` — 1-64 IPv4 or IPv6 CIDRs; validated via CEL `isCIDR()`

Status: `ready bool`, `identifier string`.

---

**VPCAttachment** — `vpcattachments.cloud.datumapis.com`

Attaches a network interface to a VPC.

Key spec fields:
- `vpc VPCRef` — reference to a VPC by name in the same namespace
- `interface VPCAttachmentInterface` — interface name and 1-16 IPv4/IPv6 addresses (each validated via CEL `isCIDR()`)

Status: `ready bool`, `identifier string`.

Annotation: `k8s.v1alpha1.cloud.datumapis.com/vpc-attachment` (constant `VPCAttachmentAnnotation`).

---

## Entry Points

This module has no binaries. It is imported by external controllers as a Go library.

To register VPC types:
```go
import v1alpha1 "go.datum.net/cloud/api/v1alpha1"

scheme.AddToScheme(v1alpha1.AddToScheme)
```

The `AddToScheme` value is produced by `sigs.k8s.io/controller-runtime/pkg/scheme.Builder` and registered via `init()` in `groupversion_info.go`.

---

## External Dependencies

| Dependency                        | Version  | Purpose                                                                   |
|-----------------------------------|----------|---------------------------------------------------------------------------|
| `k8s.io/api`                      | v0.33.0  | Core Kubernetes API types                                                 |
| `k8s.io/apimachinery`             | v0.33.0  | `metav1` types, `meta.SetStatusCondition`, scheme/GVK machinery           |
| `sigs.k8s.io/controller-runtime`  | v0.21.0  | `scheme.Builder` for type registration (only the scheme package is used)  |

All other entries in `go.sum` are transitive dependencies of the above three.

**Dev tooling (local to `./bin/`, not in `go.mod`):**

| Tool             | Version  | Purpose                                                                                 |
|------------------|----------|-----------------------------------------------------------------------------------------|
| `controller-gen` | v0.18.0  | Generates `zz_generated.deepcopy.go` and `config/crd/*.yaml` from kubebuilder markers  |
| `golangci-lint`  | v2.1.6   | Go linting                                                                              |
| `yamlfmt`        | v0.21.0  | YAML formatting; enforces consistent style                                              |
| `chainsaw`       | v0.2.15  | E2E test framework for Kubernetes resources                                             |
| `crd-ref-docs`   | v0.2.0   | Generates API reference documentation from Go types and markers                         |

---

## CI/CD

### Pipeline (`.github/workflows/ci.yaml`)

```
push/PR → main
      │
      ├── Build (go fmt + go vet + go build)
      │
      ├──┬── Unit Tests (task test:unit, uploads coverage artifact)
      │  │
      │  └── Lint (golangci-lint + yamlfmt + .yml extension check)
      │
      └── E2E Tests (kind cluster + chainsaw, 30m timeout)
```

All jobs use Go 1.26 and cache the Go module download.

---

## Known Constraints & Tech Debt

**VPC types are less mature.** The VPC package has no unit tests, uses `bool` for status (not `metav1.Condition`), and has `+default:value=false` markers on status fields that may not work as intended with controller-gen.

**Kubernetes 1.28+ hard requirement.** CEL functions `isIP()` and `isCIDR()` require Kubernetes 1.28+. The CRDs will fail to install on older clusters with a schema validation error. This is not documented in user-facing docs.

**All tools are version-pinned binaries in `./bin/`.** `task install` must be run before any code generation or linting in a fresh checkout. `go install` is used to install them; there is no lockfile beyond the version suffix in the binary name.

**`CLAUDE.md` is a symlink to `AGENTS.md`.** Both files contain identical content. This is intentional to serve both Claude Code and other AI agents, but can confuse editors and git operations that follow symlinks.

---

## For Claude

**Start here for each concern:**

| Concern                                      | Where to look                                                                 |
|----------------------------------------------|-------------------------------------------------------------------------------|
| Adding a new CRD                             | `api/v1alpha1/` — copy an existing `*_types.go`, register in `groupversion_info.go` via `init()` |
| Understanding a resource's validation rules  | Read the kubebuilder markers in the resource's `*_types.go` file; the CRD YAML is generated from them |
| Running tests locally                        | `task build` (fmt + vet + build), `task test:unit` for unit tests             |
| Regenerating CRDs after marker changes       | `task generate` (runs deepcopy + CRD manifests + docs in one command)         |

**Patterns that differ from Go idioms:**
- `init()` functions are used extensively to register types with the scheme builder. This is a Kubernetes operator convention, not general Go style.
- No error returns from type methods — status updates use the Kubernetes condition pattern instead.

**Gotchas:**
- After any change to `// +kubebuilder:...` markers, run `task generate` (it produces deepcopy, CRDs, and docs). Forgetting this leaves code, CRD YAML, and docs out of sync.
- `GOOS=linux` is required when running `go vet` or `go test` locally (the task targets set this). Some types have Linux-specific constraints.
- `CLAUDE.md` is a symlink. Edit `AGENTS.md` directly; attempts to edit through the symlink will fail.
