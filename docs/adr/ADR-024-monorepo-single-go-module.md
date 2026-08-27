# ADR-024: Monorepo with a single Go module and CI-enforced architectural fitness functions

- **Status:** Accepted
- **Date:** 2026-08-26
- **Deciders:** Platform Architecture
- **Baseline reference:** §4 (layering and the dependency rule), §25 (repository layout — binding), §26 (traceability), §27 (definition of done) of docs/spec/00-design-baseline.md
- **Supersedes / Related:** Related to ADR-006 (planes), ADR-023 (wiring); enforces ADR-011, ADR-016

## Context

Nine binaries (§5), nine bounded contexts (§3), five planes (ADR-006), and a layering rule (§4)
that is meaningless unless something enforces it. §25 declares the repository layout **binding**
and §27 requires a verification run — build, vet, race, lint, SAST, vuln scan, contract
validation, manifest validation, architecture check — as part of the definition of done.

The forces:

1. **The dependency rule is the architecture.** "`internal/domain` imports nothing but stdlib" and
   "`internal/application` may not import infrastructure" are not style preferences; they are what
   makes the domain testable, portable and free of framework coupling. A rule that lives only in a
   document is violated within a quarter — not maliciously, but by someone under deadline who
   needs a UUID library in an entity.
2. **Cross-cutting changes are the norm here, not the exception.** Adding a field to the payment
   event touches the domain, the application service, the event schema, the OpenAPI contract, the
   consumers and the tests. In a polyrepo that is 5–7 coordinated PRs with a merge order; in a
   monorepo it is one atomic commit that either builds or does not.
3. **Shared contracts are the majority of the interesting code.** `api/openapi/`, `api/proto/`,
   `api/events/`, `api/errors/catalog.yaml`, `pkg/money`, `pkg/ids`, `pkg/apierror` are consumed
   by nearly every binary. Versioning them across repositories means every change is a release,
   a bump and a coordination problem.
4. **Traceability is required** (§26): every requirement maps to design, packages and tests, and
   CI fails on an orphan requirement or an orphan test. That check needs to see everything at once.
5. **Go's module semantics push in this direction.** A single module gives one `go.mod`, one
   dependency graph, one `go test ./...`, and no internal version skew. Multi-module repositories
   require `replace` directives or published versions for internal dependencies — the coordination
   cost of a polyrepo with the layout of a monorepo.
6. **Team size.** ADR-006 assumes a small organisation. Polyrepo's independence benefits accrue to
   organisations with many teams needing genuine autonomy; its coordination costs are paid from
   day one regardless.

What breaks if we choose wrong: the layering rule erodes silently until the domain imports a
database driver; or every cross-cutting change becomes a multi-repo choreography; or version skew
between internal packages produces failures that reproduce nowhere.

## Decision

**One repository, one Go module, with architectural fitness functions enforced in CI. The layout
in §25 is binding and mechanically checked.**

1. **Single `go.mod` at the repository root.** All nine binaries, all internal packages, all
   `pkg/` libraries share one dependency graph and one version of every dependency. No internal
   version skew is possible.
2. **Fitness functions in CI** — each a build-failing gate, not a warning:
   - **Dependency rule (§4):** `scripts/check-architecture.sh` enforces the import table.
     `internal/domain/**` may import only stdlib and other `internal/domain/**` — explicitly not
     `database/sql`, `net/http`, otel, AWS SDKs or UUID libraries. `internal/application/**` may
     not import `internal/infrastructure/**` or `internal/adapters/**`. `pkg/**` is **stdlib-only**
     (it is the part that could be extracted and published; a dependency there is a dependency
     imposed on every future consumer).
   - **Layout:** directories outside §25's tree fail the build.
   - **Contract validation:** OpenAPI and Protobuf specs validated; handlers checked against them;
     breaking-change diff gate within a major version (ADR-022).
   - **Rule documentation:** `TestEveryRuleIsDocumented` (ADR-016).
   - **RLS coverage:** `TestEveryTenantScopedTableHasForcedRLS` (ADR-008).
   - **Adapter contract suite:** every registered gateway adapter runs the full substitutability
     suite (ADR-011).
   - **Money purity:** no float types in money packages (ADR-018).
   - **Metric cardinality lint** (§22.3) and **event registry/partition-key check** (ADR-020).
   - **Traceability (§26):** CI fails on an orphan requirement (no test) or an orphan test (no
     requirement).
   - Plus §27's baseline: build, `go vet`, race detector, lint, SAST, vulnerability scan, manifest
     validation.
3. **`internal/` does the enforcement work Go gives us for free**: nothing outside this module can
   import it, so the platform's internals are not an accidental public API.
4. **Fitness functions are tests, not scripts run by hand.** They live in the repository, run in
   CI on every PR, and fail the build. A rule that only warns is a rule that is ignored.
5. **The build is selective, the module is not.** CI computes which binaries and tests are affected
   by a change and runs the full suite only when shared code changes — a single module does not
   require a monolithic build.

## Options considered

| Option | Pros | Cons | Verdict |
|---|---|---|---|
| **Monorepo, single module, CI fitness functions (chosen)** | Cross-cutting changes are one atomic commit that builds or does not; no internal version skew, ever; one dependency upgrade path for security patches across all nine binaries; fitness functions can see the whole graph, which is what makes §4 and §26 enforceable at all; `go test ./...` is the whole story; refactoring across boundaries is cheap, which keeps boundaries honest rather than ossified | Every binary shares one dependency set, so an upgrade needed by one is an upgrade for all; CI must be made selective or it becomes slow; the repository becomes large; a bad commit on the main branch blocks everyone; no per-team independence in release cadence | **Accepted** |
| **Polyrepo (one repository per service or per plane)** | Genuine team autonomy: independent release cadence, independent dependency choices, independent CI; a broken build blocks one team; smaller repositories are faster to clone and index; access control per repository | Cross-cutting changes become 5–7 coordinated PRs with a required merge order, and the intermediate states are broken by construction; shared contracts (`api/`, `pkg/`) must be versioned and published, so every schema change is a release-and-bump cycle across repositories; internal version skew becomes possible and then inevitable, producing failures that reproduce only in one environment; the §4 dependency rule and the §26 traceability check cannot be enforced from inside any single repository — they need the whole graph. This is the option an engineer from a large multi-team organisation pushes for, and the autonomy argument is real at that scale — it loses here on cross-cutting change cost and on the impossibility of enforcing the architecture | Rejected |
| **Monorepo with multiple Go modules (one per plane, or per binary)** | Stronger enforced boundaries: a module cannot import another module without a declared dependency; independent versioning of `pkg/` for external publication; smaller build graphs per module | Internal dependencies require either `replace` directives (which then must be stripped for external consumption, and which break `go install`) or published versions of internal modules (reintroducing the polyrepo coordination cost inside one repository); tooling — IDEs, linters, `go test ./...`, coverage aggregation — degrades noticeably across module boundaries; dependency upgrades must be applied per module; and the boundary enforcement it buys is the same enforcement the import-rule fitness function already provides, without the tooling cost. Genuinely tempting for `pkg/` specifically | Rejected for now; a future extraction of `pkg/` into its own module is a compatible, contained change |
| **Monorepo, single module, rules documented but not enforced** | No CI investment; no gates to maintain or work around; fastest to start; trusts engineers | The dependency rule survives about one quarter. The first violation is small and reasonable (a UUID library in the domain); the tenth makes the domain untestable without a database. By the time it is visible it is expensive to reverse. §25 says the layout is binding — "binding" without enforcement is aspiration | Rejected |
| **Monorepo with a general build system (Bazel, Please, Pants)** | Precise incremental builds and remote caching; hermetic builds; native support for multi-language; excellent at very large scale | Substantial ongoing investment for a Go-only repository where `go build` is already fast and `go test ./...` runs in minutes; every dependency addition becomes a build-file change; onboarding cost is real and permanent. Revisit only if build times become a genuine constraint | Rejected |

## Consequences

### Positive

- The architecture is enforced by the build. A PR that imports `database/sql` into
  `internal/domain` fails, with a message naming the rule — the review conversation happens once,
  in CI, instead of every time.
- Cross-cutting changes — new event field, new error code, new validation rule — are one atomic,
  reviewable, revertible commit.
- One dependency graph means a CVE is patched once for all nine binaries.
- Traceability (§26) and the fitness functions can see everything, which is a precondition for
  their existence.
- Refactoring across context boundaries stays cheap, which means boundaries can be corrected when
  they are found to be wrong instead of being frozen by coordination cost.

### Negative

- All binaries share one dependency set. A library upgrade required by one binary is an upgrade
  for all, and an incompatible requirement between two binaries has no clean resolution.
- CI must be selective or the feedback loop degrades as the repository grows; that selectivity is
  itself infrastructure to build and maintain.
- A broken main branch blocks everyone, so branch protection and pre-merge verification are
  mandatory rather than optional.
- No per-team release independence. Acceptable at current size; it is the first thing to hurt if
  the organisation grows.
- Fitness functions have a maintenance cost and, when wrong, block legitimate work. A gate that
  fires falsely is quickly worked around, which is worse than not having it.

### Neutral / accepted costs

- Repository size grows over time; shallow clones and IDE indexing settings become part of
  onboarding.
- `pkg/`'s stdlib-only constraint occasionally forces us to write a small helper rather than take
  a dependency. That is the intended trade: `pkg/` is the publishable surface.

## Risks and mitigations

| Risk | Likelihood | Impact | Mitigation | Detection signal |
|---|---|---|---|---|
| CI becomes slow and engineers route around it | Medium | High — the gates stop being gates | Selective builds by affected package; parallel test execution; a target of ≤ 10 minutes for the PR pipeline and ≤ 30 minutes for the full suite | PR pipeline duration p95; count of `--no-verify` or skip-gate usages |
| A fitness function has false positives and gets disabled | Medium | **High** — enforcement collapses | Rules are narrow and specific with clear error messages naming the ADR; exceptions are explicit allowlist entries with a justification comment and an owner, reviewed quarterly — never a disabled check | Allowlist size and age; commits disabling a check |
| Dependency conflict between two binaries' needs | Medium | Medium | Single module forces the conflict to be resolved rather than hidden; if genuinely irresolvable, that is the signal to extract a module | Build failure on upgrade; count of pinned-back dependencies |
| Repository size degrades tooling | Medium | Low–Medium | Generated code and large fixtures kept out of the tree or in a separate path excluded from indexing; no binary artifacts committed | Clone time; IDE index time |
| Main branch broken frequently | Medium | Medium | Required status checks; merge queue; no direct pushes to main | Main-branch red duration per week |
| Fitness functions never grow to cover new invariants | Medium | Medium — architecture drifts in the uncovered areas | Every ADR in this set names a mechanical check; a new ADR without one is incomplete and is rejected in review on that basis | ADR review checklist; count of ADRs with no corresponding CI gate |
| Organisation outgrows the model | Low now, higher later | Medium | Revisit criteria below; multi-module inside the monorepo is the intermediate step, not polyrepo | Team count; merge conflict rate; release-coordination complaints |

## Validation

- **The dependency rule holds:** `scripts/check-architecture.sh` passes on every commit, and a
  deliberately-introduced violation (importing `database/sql` into `internal/domain`) fails the
  build. Tested as a meta-test, so a broken checker is itself detectable.
- **Gate coverage:** every architectural ADR in this set (006–024) has at least one corresponding
  mechanical check. An ADR without one is not done (§27).
- **Cross-cutting change cost:** adding a field to a payment event end-to-end — domain, schema,
  contract, consumers, tests — is one commit. If it is not, the module or layout is wrong.
- **CI duration:** PR pipeline p95 ≤ 10 minutes; full verification ≤ 30 minutes. These are the
  numbers that determine whether the gates are respected or resented.
- **Enforcement integrity:** allowlist entries in the architecture check — target zero, tolerance
  a small number each with a justification and an owner. A growing allowlist is the leading
  indicator that this ADR is failing.

## Revisit criteria

Reopen if:

1. Team count exceeds ~5 independent teams with genuinely divergent release cadences — the first
   step would be multiple modules *inside* the monorepo, not polyrepo.
2. CI duration cannot be held under the targets with selective builds, making a purpose-built
   build system (Bazel and peers) worth its cost.
3. `pkg/` needs independent external versioning for published SDKs — a contained extraction into
   its own module, compatible with everything else here.
4. A binary requires a dependency version irreconcilable with another binary's needs.
5. A non-Go service is introduced, which does not by itself require a polyrepo (a monorepo can
   hold multiple languages) but does require revisiting the single-module assumption.
