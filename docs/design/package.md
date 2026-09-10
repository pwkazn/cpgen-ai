# Internal Package and Export Design

Status: Current under ADR-0006

Implementation note (2026-09-09): `cpgen.package/v2` now provides the generation package model below through the existing `packageprobe` builder/reader. Its layout uses `statement/statement.md`, `solution/{reference,brute}.{cpp,go}`, `judge/{generator,validator}.{cpp,go}`, the fixed `judge/checker.cpp`, `data/tests.json`, paired `tests/*.in` / `tests/*.ans`, and package-safe reports. V1 retains its historical probe layout. The dedicated M26 package ledger, current-evidence assembly, canonical ZIP export and atomic READY transaction are implemented and pass real Docker plus independent CLI crash/export/revalidation acceptance. Format validation is supplemented by same-run committed Quality proof. See [package acceptance evidence](../evidence/mvp-package-commit-foundation.md).

## 1. Principles

A package is a deterministic immutable tree assembled from verified run-scoped artifact occurrences. Stage code cannot copy arbitrary workspace files. All paths, roles, media types, digests, provenance, and gate results are declared and audited.

READY means the same run atomically references a VERIFIED package occurrence and final quality report.

## 2. Internal directory

~~~text
problem/
  manifest.json
  statement/
    statement.md
  solutions/
    reference.*
  data/
    tests.json
    input/
    output/
  checker/
    checker.*
  reports/
    quality.json
    similarity.json
    verification.json
  provenance/
    artifacts.json
~~~

Optional entries are controlled by the manifest schema. Undeclared files, symlinks, device files, absolute paths, traversal components, duplicate normalized paths, and case-fold collisions are rejected.

## 3. Manifest

The canonical manifest records:

- schema version and package identity;
- title, limits, checker type, and supported languages;
- every file path, role, media type, size, and SHA-256 digest;
- statement, solution, data, checker, report, and provenance references;
- workflow, request, configuration, and policy digests;
- toolchain and image identities;
- required quality gates and their evidence digests.

Canonical JSON uses stable field ordering, UTF-8 normalization, exact integers, and no insignificant variation.

## 4. MeteredPackageWriter

The package stage receives only a MeteredPackageWriter. It can:

- declare a package role and normalized relative path;
- open verified source occurrences authorized for the same run and revision;
- stream into a private staging tree with byte accounting;
- finalize the manifest;
- request structural and semantic verification;
- publish atomically after all gates pass.

It cannot read arbitrary filesystem paths or publish directly to the user-selected destination.

## 5. Build protocol

1. The stage loads only verified current occurrences.
2. A short transaction creates package identity, occurrence in STAGING, declarations, and artifact-byte reservations.
3. Outside database transactions, the writer builds a private staging tree, copies through verified reads, hashes every output, and fsyncs.
4. PackageStructuralGate parses the tree independently.
5. Semantic gates validate statements, solutions, tests, checker, reports, and provenance.
6. A verification receipt and final quality report are created.
7. In one short transaction, reservations settle, the package occurrence becomes VERIFIED, and the run points to that occurrence and becomes READY.
8. Publication atomically renames or copies through a verified destination protocol and records the result.

A crash at any point is replay-safe. Unverified staging cannot make a run READY.

## 6. Structural gate

PackageStructuralGate verifies:

- normalized paths and exact manifest membership;
- no links, special files, or unexpected directories;
- digest and size of every file;
- manifest schema and cross references;
- required roles and unique identities;
- package-safe provenance only;
- configured total byte and file-count caps.

The gate treats both local staging and imported packages as untrusted.

## 7. Semantic gates

The package gate aggregates:

- statement completeness and constraint consistency;
- reference solution compile and execution evidence;
- test data validity and expected-output evidence;
- checker or SPJ contract evidence;
- Judge and resource-limit evidence;
- differential and quality reports;
- similarity decision or bound waiver;
- toolchain, image, workflow, policy, and revision consistency.

A gate cannot be skipped by a stage result. A waiver is accepted only for explicitly waivable rules and exact evidence bindings.

## 8. Verification receipt

The receipt contains package ID, manifest and tree digests, gate versions, toolchain identity, verification time, all gate result digests, and final quality-report identity. It is immutable and package-safe.

Imported packages create an IMPORTED occurrence first. They become VERIFIED only after the same complete gate sequence; import never trusts an included receipt without recomputation.

## 9. READY transaction

The database enforces:

- the package occurrence belongs to the run;
- its state is VERIFIED;
- receipt and final quality report are non-null and consistent;
- relation and revision match;
- run state and package pointers update together.

No process-local check can substitute for these constraints.

## 10. Exporters

Exporters consume only a VERIFIED internal package through verified reads. A Polygon exporter maps the canonical model to platform files, records deterministic mapping and loss reports, validates the exported tree, and publishes atomically.

Exporter retries are bounded inside the Package stage. An unknown remote upload boundary reconciles the original provider identity or pauses conservatively; it never creates a second publication under a new identity.

## 11. Repair and review

Package validation errors route to the earliest owning compiled stage. Mechanical staging errors may retry locally. Content or policy failures enter NEEDS_REVIEW or revise the owning stage according to typed rules and remaining budgets.

## 12. Tests

Tests cover canonical manifest encoding, path attacks, case collisions, undeclared files, symlinks, digest corruption, deterministic builds, crash at every staging and publication boundary, gate completeness, waiver binding, imported package verification, same-run READY constraints, exporter loss reports, and bounded retry.
