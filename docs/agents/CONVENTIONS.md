# Conventions

> Cloud is a Go module defining Kubernetes CRDs and API types for virtual networking. It is API-only — no controllers, no runtime code.

_Last updated: 2026-07-07_

---

## Table of Contents

1. [Core Conventions](#core-conventions)
   - [Naming](#naming)
   - [Code Organization](#code-organization)
   - [Comments and Documentation](#comments-and-documentation)
   - [Testing](#testing)
   - [Dependencies and Imports](#dependencies-and-imports)
   - [Git and Version Control](#git-and-version-control)
2. [Go Conventions](#go-conventions)
   - [Module and Package Layout](#module-and-package-layout)
   - [Type Naming](#type-naming)
   - [kubebuilder Markers](#kubebuilder-markers)
   - [Status Conditions](#status-conditions)
   - [Field Types and Invariants](#field-types-and-invariants)
   - [JSON Serialization](#json-serialization)
   - [Code Generation](#code-generation)
   - [Error Handling](#error-handling)
   - [Testing](#go-testing)
   - [Linting and Formatting](#linting-and-formatting)
3. [YAML Conventions](#yaml-conventions)
4. [Markdown Conventions](#markdown-conventions)
5. [For Claude](#for-claude)

---

## Core Conventions

### Naming

**Source files** use `snake_case` with a `_types.go` suffix for CRD type definitions:

```
vpc_types.go              ← VPC CRD
vpcattachment_types.go    ← VPCAttachment CRD
groupversion_info.go      ← scheme registration (fixed name)
```

**Test files** use `_test.go` suffix adjacent to the file under test:

```
vpc_types_test.go         ← tests for vpc_types.go
```

**Generated files** use the `zz_generated.` prefix. Never edit them:

```
zz_generated.deepcopy.go
```

**Directories** use lowercase kebab-case. The version is part of the path (`v1alpha1`).

### Code Organization

All API types live under `api/v1alpha1/`. One `_types.go` file per CRD resource.

```
api/
  v1alpha1/               ← cloud.datumapis.com API group
config/crd/               ← generated CRD YAML — never edit directly
docs/api/                 ← human-readable field reference
hack/                     ← code generation support files
test/e2e/                 ← chainsaw e2e test suite
bin/                      ← local dev tools (gitignored)
```

### Comments and Documentation

**Exported types** require a godoc comment that begins with the type name and ends with a period:

```go
// VPC defines a virtual private cloud. It represents a set of CIDR prefixes.
type VPC struct { ... }
```

**Spec and Status types** follow the "defines the desired/observed state of..." convention:

```go
// VPCSpec defines the desired state of a VPC.
type VPCSpec struct { ... }

// VPCStatus defines the observed state of a VPC.
type VPCStatus struct { ... }
```

**Enum constants** each get a godoc comment:

```go
// VPCPhaseReady indicates the VPC is ready for use.
VPCPhaseReady VPCPhase = "ready"
```

**Fields** get a single-line comment above them (not end-of-line). The comment is a description; it does not repeat the field name. For fields with kubebuilder markers, the comment precedes the markers:

```go
// Networks is a list of IPv4 or IPv6 CIDRs.
// +kubebuilder:validation:MinItems=1
Networks []Network `json:"networks"`
```

Do not write comments that explain what the code obviously does. Write them when the WHY is non-obvious — a hidden constraint, a subtle invariant.

### Testing

- Framework: stdlib `testing` only — no testify, no ginkgo.
- E2E framework: [chainsaw](https://kyverno.io/docs/chainsaw/) for Kubernetes resource tests.
- Style: table-driven tests with `t.Run` for multiple cases; single function for unique behaviors.
- Test function names: `TestTypeName_Behavior` or `TestTypeName` (e.g., `TestVPCDeepCopy`).
- Helper constructors (e.g., `newTestVPC()`) are unexported and defined at the top of the test file.
- Helper lookup functions (e.g., `findCondition`) are pure functions — no `t.Helper()` required.
- Unit tests are in the same package as the production code (white-box, not `_test` suffix package).
- E2E tests live in `test/e2e/tests/` as chainsaw test cases.
- Unit tests cover: DeepCopy correctness, JSON round-trip, field name verification, and boundary values.

### Dependencies and Imports

- `go.sum` is committed; no vendor directory.
- Imports are grouped in two blocks (gofmt standard): stdlib then external. No blank line between the groups unless there is an internal package.
- Aliases follow these conventions:
  - `metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"` — always aliased
  - `"k8s.io/apimachinery/pkg/api/meta"` — used unaliased
- No internal packages — all types are exported API surface.

Adding new dependencies: this repo is API-only. New dependencies must have a clear API type requirement (Kubernetes ecosystem only). Do not add runtime or application dependencies.

### Git and Version Control

**Commit messages** follow [Conventional Commits](https://www.conventionalcommits.org/):

```
type(scope): short imperative description

Optional body explaining the why.
```

Types in use: `feat`, `fix`, `refactor`, `chore`, `ci`, `style`.
Scopes in use: `api`, `vpc`, `docs`, `fmt`.

**Branch naming**: `type/kebab-case-description`

```
feat/vpc-attachment-validation
fix/asn-int64-schema
refactor/vpc-api-v3
```

**Merge strategy**: merge commits (PRs land as merge commits; no squash or rebase).

---

## Go Conventions

### Module and Package Layout

- Module path: `go.datum.net/cloud`
- Go version: 1.26
- Package name equals the directory's last path segment (`v1alpha1`).
- One package per API group version. All types for `cloud.datumapis.com/v1alpha1` live in `api/v1alpha1/`; one `_types.go` file per CRD.
- No `internal/`, `pkg/`, or `cmd/` packages — this is an API-only module.

### Type Naming

| Construct               | Rule                                          | Example                                        |
|-------------------------|-----------------------------------------------|------------------------------------------------|
| CRD resource            | `PascalCase` prefixed with group abbreviation | `VPC`, `VPCAttachment`                         |
| Spec                    | `{Resource}Spec`                              | `VPCSpec`                                      |
| Status                  | `{Resource}Status`                            | `VPCStatus`                                    |
| List                    | `{Resource}List`                              | `VPCList`                                      |
| Enum type               | `{Resource}{Field}` or descriptive noun       | `VPCPhase`                                     |
| Enum constant           | `{TypeName}{Value}`                           | `VPCPhaseReady`                                |
| Condition type constant | `ConditionType{Name}` (`string`)              | `ConditionTypeReady`, `ConditionTypeAccepted`  |
| Reference struct        | `{Target}Ref`                                 | `VPCRef`                                       |

Condition type and reason constants are defined as untyped `string` constants (not a named type) in the same file as the resource they belong to.

### kubebuilder Markers

Every CRD root type requires these markers directly above the type declaration, in this order:

```go
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=<abbrev>
// +kubebuilder:printcolumn:name="...",type="...",JSONPath="..."
// ... additional printcolumns
type VPC struct { ... }
```

Field-level validation markers appear directly above the field comment:

```go
// Networks is a list of IPv4 or IPv6 CIDRs.
// +kubebuilder:validation:MinItems=1
// +kubebuilder:validation:MaxItems=64
Networks []Network `json:"networks"`
```

CEL validation uses `XValidation`:

```go
// +kubebuilder:validation:XValidation:rule="isCIDR(self)",message="address must be a valid IPv4 or IPv6 CIDR"
```

CEL functions `isIP()` and `isCIDR()` require Kubernetes 1.28+.

For lists that are struct maps, use `+listType=map` and `+listMapKey=<key>`:

```go
// +listType=map
// +listMapKey=type
Conditions []metav1.Condition `json:"conditions,omitempty"`
```

For lists that enforce uniqueness, use `+listType=set`:

```go
// +listType=set
Prefixes []string `json:"prefixes"`
```

### Status Conditions

All status types that surface runtime state include:

1. `ObservedGeneration int64` — set to the `.metadata.generation` the status was computed from.
2. `Conditions []metav1.Condition` — with `+listType=map` and `+listMapKey=type`.

```go
type VPCStatus struct {
    // +optional
    ObservedGeneration int64 `json:"observedGeneration,omitempty"`

    // +listType=map
    // +listMapKey=type
    // +optional
    Conditions []metav1.Condition `json:"conditions,omitempty"`
}
```

Condition type constants (`ConditionTypeReady`, `ConditionTypeAccepted`) are `string` constants, not a named type. Use `meta.SetStatusCondition` from `k8s.io/apimachinery/pkg/api/meta` to update conditions (it handles deduplication by type).

### Field Types and Invariants

**Duration fields** use `*metav1.Duration` (pointer, so they can be omitted). CEL validation rules enforce timing semantics.

### JSON Serialization

- Required fields: `json:"fieldName"` (no omitempty).
- Optional fields: `json:"fieldName,omitempty"`.
- Inline embedding: `json:",inline"` (no field name, e.g., `metav1.TypeMeta`, `metav1.ObjectMeta`).
- JSON field names are camelCase matching the Go field name (first letter lowercased).
- Pointer fields (`*T`) are used exclusively for optional fields that can be absent vs. zero-valued.

### Code Generation

Three generated artifacts — never edit directly:

| Artifact                   | Generator        | Regeneration command |
|----------------------------|------------------|----------------------|
| `zz_generated.deepcopy.go` | controller-gen   | `task generate`      |
| `config/crd/*.yaml`        | controller-gen   | `task generate`      |
| `docs/api/vpc.md`          | crd-ref-docs     | `task generate`      |

`task generate` runs all three generators in sequence (deepcopy methods, CRD manifests, API docs). Running it once after marker or type changes keeps all artifacts in sync.

The copyright/license header for generated files is in `hack/boilerplate.go.txt`.

### Error Handling

This is an API-only module — no request path, no controllers. Error handling is minimal:

- Type methods that update status use the Kubernetes condition pattern (no returned `error`).
- `fmt.Errorf` is used in status message strings, not for wrapping errors.
- No panics in this codebase (init() only registers with the scheme builder).
- No logging — API types have no logging infrastructure.

### Go Testing

- Framework: stdlib `testing` (no external test framework).
- E2E framework: chainsaw for Kubernetes resource-level integration tests.
- Table-driven via `t.Run` for parametric cases; dedicated function for single behaviors.
- Test constructors are named `newTest{TypeName}(...)` (unexported, top of test file).
- Pure helper functions (e.g., `findCondition`) do not call `t.Helper()` — they are not test helpers, they are utilities.
- Tests call methods under test then use direct `t.Errorf` / `t.Fatalf` with `got %v, want %v` format.
- Run unit tests: `GOOS=linux go test ./api/v1alpha1/... -v`
- Run e2e tests: `task test:e2e` (requires kind cluster)

### Linting and Formatting

- Linter: `golangci-lint` v2.1.6 — configured via `.golangci.yaml`, run via `task lint`.
- Formatter: `go fmt` runs automatically as a dependency of `task build`.
- `go vet` runs with `GOOS=linux` to catch cross-platform issues.
- YAML formatter: `yamlfmt` v0.21.0 — run via `task lint` (lint mode) or `task lint-fix` (auto-format).

---

## YAML Conventions

- All YAML files **must** use the `.yaml` extension. Files with `.yml` will fail the lint check (`task lint` enforces this).
- YAML is formatted with `yamlfmt` v0.21.0. Run `task lint-fix` to auto-format.
- CRD manifests in `config/crd/` are generated — do not edit manually.

---

## Markdown Conventions

- Markdown tables must have **aligned columns** — pad each cell with spaces so the `|` delimiters line up across all rows, including the separator row. Example:

  ```markdown
  | Construct  | Rule                           | Example         |
  |------------|--------------------------------|-----------------|
  | CRD root   | PascalCase, group prefix       | `VPC`           |
  | Spec type  | `{Resource}Spec`               | `VPCSpec`       |
  ```

  Not:

  ```markdown
  | Construct | Rule | Example |
  |-----------|------|---------|
  | CRD root | PascalCase, group prefix | `VPC` |
  ```

- This applies to all `.md` files in the repository: docs, CONVENTIONS.md, ARCHITECTURE.md, CLAUDE.md, and inline docs.

---

## For Claude

- **The single most important rule**: This is API-only Go. No controllers, no runtime, no logging, no error returns from type methods. Every new file goes in `api/v1alpha1/` and follows the `_types.go` naming pattern.
- **Never edit `zz_generated.deepcopy.go`, `config/crd/*.yaml`, or `docs/api/vpc.md`**. Run `task generate` instead.
- **YAML files use `.yaml` extension only** — `.yml` fails CI.
- New CRD resource → one `{resource}_types.go` file in `api/v1alpha1/`.
- Condition type constants are untyped `string` constants (not a named type), defined in the same file as the resource they describe.
- Status types always include `ObservedGeneration int64` and `Conditions []metav1.Condition` with `+listType=map +listMapKey=type` markers.
- Tests use stdlib `testing` only — no testify. Table-driven with `t.Run`, same-package (not `_test` suffix).
- E2E tests use chainsaw; test cases live in `test/e2e/tests/`.
- Commit messages follow Conventional Commits: `type(scope): message`. Scopes are `api`, `vpc`, `docs`.
- **Most common mistakes to avoid**:
  1. Adding omitempty to required fields or omitting it from optional fields.
  2. Editing generated files directly instead of updating markers and re-running `task generate`.
- Comply with `go fmt` (auto-run by task), `go vet` (`GOOS=linux`), and `golangci-lint` (configured via `.golangci.yaml`).
