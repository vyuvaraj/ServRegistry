# ServRegistry Roadmap

This roadmap outlines the planned development phases for the ServRegistry package manager hub.

---

## Differentiating Factors (Why ServRegistry?)

* **Integrated with Serv-lang**: Native compiler-level integration using `serv.toml` to install and resolve modules (`serv install`) without third-party tools.
* **Shared Storage Backend**: Avoids managing separate storage platforms by directly mounting package layers to `ServStore` S3 buckets.
* **On-the-Fly Dependency Resolution**: Native dependency resolution endpoint using BFS to automatically retrieve, traverse, and output complete dependency trees.
* **OTel Integration**: Standardised OTel traces track publishing latency, size metrics, and cache-hit ratios directly to `ServTrace` and `ServConsole`.

---

## Phase 1: Core Registry & Auth (Completed)
- [x] **Package Upload & Retrieval**: HTTP APIs for publishing and fetching tarball payloads.
- [x] **Manifest Parsing**: Automated `serv.toml` parsing within archives to capture names, versions, and dependencies.
- [x] **Embedded Dashboard**: Premium, glassmorphic client interface to view and search package indexes.
- [x] **JWT Security**: Verification of signature tokens to protect publishing scopes.

## Phase 2: Semver Range Resolution (Q3 2026)
- [x] **Range Matching**: Add server-side parsing for semantic version ranges (e.g. `^1.2.0`, `~0.4.1`).
- [x] **Package Deprecation**: API flag to mark versions as deprecated with warning payloads.

## Phase 3: Cryptographic Signatures (Q4 2026)
- [x] **Package Signing**: Support uploading `.sig` signature files alongside tarballs.
- [x] **Public Key Verification**: Cryptographically verify author signatures at the command-line before installation.

## Phase 4: Architectural Depth & DevOps (Pending)
- [ ] **`serv registry audit`** — CLI command that scans installed packages for known vulnerable versions, outdated dependencies, and deprecated packages, with fix suggestions (Security / DX)
- [ ] **Private Namespace Support** — Scoped package namespaces (`@org/package`) with access control lists; teams can publish internal packages without exposing to the public index (DevOps)
- [ ] **Mirror & Offline Cache** — Local proxy mode that caches the public registry to a ServStore bucket; enables air-gapped builds and faster CI pipelines with zero external fetches (DevOps)
- [ ] **Provenance Attestation** — Record build provenance (commit SHA, CI run ID, builder identity) alongside the package; verify with `serv verify --attestation` for supply-chain security (Security)

> See [UNIFIED_ROADMAP.md](../UNIFIED_ROADMAP.md) for the full ecosystem priority matrix and architectural recommendations.
