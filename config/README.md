# `config/` — per-environment configuration, seed policies and routing defaults

> **These files document the shape of each environment's configuration. Nothing loads them.**
>
> No Go code reads `dev.yaml`, `staging.yaml` or `prod.yaml`, and no Helm chart or Kustomize
> overlay in this tree turns them into `PP_*` variables. They are a reference for what a correctly
> configured environment looks like, kept in step with the struct tags in
> `internal/platform/runtime/env.go` by review rather than by a check.
>
> What actually configures a process:
>
> | Environment | Source of truth |
> |---|---|
> | Local development | [`.env.dev`](../.env.dev), sourced by every `make run-*` target |
> | Containers | `deploy/docker-compose.dev.yml`'s `environment:` blocks |
> | Kubernetes | `deployments/k8s/**` and `helm/**` — which set `PP_*` directly, not from here |
>
> This is stated at the top rather than buried, because the previous version of this file described
> a three-layer precedence order that no code implements, and a documented mechanism that does not
> exist is worse than an undocumented one: someone edits `prod.yaml`, deploys, and nothing changes.
>
> **Wiring these files as a base layer under the environment is the obvious fix** — a
> `config.LoadFile(path, dest)` in `internal/platform/config` that fills a struct before
> `LoadFromEnv` overrides it. It was not done here because it is a larger change than it looks:
> the YAML here is nested by concern (`http:`, `database:`, `auth:`) while the env tags are flat,
> so it needs either a key-mapping layer or a reshaping of every file, plus a decision about what
> a partially-specified file means for a `required:"true"` field. That belongs in a reviewed change
> with tests, not in a footnote.

Baseline §25 places this directory in the binding repository layout. What lives here, and what
deliberately does not:

| Lives here | Does not |
|---|---|
| Per-environment operational values: timeouts, pool sizes, sampling ratios, feature-flag defaults | Any secret, credential, key or token |
| Seed policy documents: the routing and risk defaults a synthetic dataset is built from | Production data, or anything derived from it |
| The default merchant routing and risk policy a new merchant starts with | Per-merchant configuration — that is versioned in the database and published through the API |

## No secrets, references only

Every value that would be a secret is a **reference** — `secret://env/scope/name` — resolved at the
moment of use by the secrets provider. The reason is not that this directory might be committed to
a public repository; it is that a secret in a file is a secret in every backup of that file, in
every developer's checkout, in every CI cache layer and in every container image built from the
tree. A reference has none of those properties, and rotating it does not require a redeploy.

`internal/platform/config` masks anything whose *name* matches the secret pattern when it logs the
effective configuration at startup, using the same pattern the admission policy and the CI scanner
use. That is a second control, not the first one: nothing here should need masking.

## Precedence, as it actually is

Two layers, not three:

1. `PP_*` environment variables set on the process.
2. The binary's own `default:` struct tags in `internal/platform/runtime/env.go`.

A value with no default and `required:"true"` on its tag is a startup failure that names the
variable — and names *every* missing variable at once, so one fix-and-restart cycle is enough. See
`runtime.ReportStartupFailure` and `config.Load`.

The files in this directory are **not** a layer. If a value here is not also in `.env.dev`, in a
compose `environment:` block or in a chart, it is not set anywhere.

### Keeping them honest

When you change a `default:` tag or add a `required:"true"` field, update the corresponding file
here in the same change. Nothing enforces it, which is precisely why it is worth writing down.

## Files

| File | Purpose |
|---|---|
| `dev.yaml` | Local and ephemeral preview environments. Simulator enabled, sampling at 100 %, short timeouts. The values that a local run actually uses live in [`.env.dev`](../.env.dev); this file is the annotated version of the same set. |
| `staging.yaml` | Production-shaped, sandbox gateways. The environment where the DR drill and the certification suite run. |
| `prod.yaml` | Production. Simulator forbidden, sampling at 10 %, the documented pool sizes and budgets. |
| `seed/routing.yaml` | The default routing policy a seeded or newly-onboarded merchant starts with. |
| `seed/risk.yaml` | The default risk policy: limits, velocity and the per-check failure postures. |
| `seed/profiles.yaml` | The synthetic dataset profiles `platformctl seed --profile` selects between. |
