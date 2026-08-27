# API contracts

These files are **authoritative**. Server code is written to match them and CI validates
both directions: a handler that diverges from the OpenAPI document fails the build, and a
document that describes an endpoint no handler serves fails it too. When a contract and
an implementation disagree, the contract is right and the implementation is a defect.

Everything here derives from [`docs/spec/00-design-baseline.md`](../docs/spec/00-design-baseline.md).
If a contract disagrees with the baseline, the contract is the defect.

```
api/
├── openapi/payments-platform.v1.yaml   Public REST surface (OpenAPI 3.1)
├── proto/payments/v1/*.proto            Internal gRPC surface (proto3, buf)
├── events/*.schema.json                 Event envelope + 25 payload schemas (JSON Schema 2020-12)
├── errors/catalog.yaml                  Every error code, on both transports
└── README.md                            This file
```

---

## How the four contracts relate

They are four projections of **one** model, and the seams between them are where drift
gets in — so each seam has a check that closes it.

```
                     docs/spec/00-design-baseline.md
                        (states, invariants, IDs)
                                   │
        ┌──────────────────┬───────┴────────┬───────────────────┐
        ▼                  ▼                ▼                   ▼
   openapi/           proto/           events/             errors/
   REST, external     gRPC, internal   published language  one code space
        │                  │                │                   │
        │  same enums      │  same enums    │  same enums       │
        └──────────────────┴────────────────┘                   │
                           │                                    │
                    same error codes ───────────────────────────┘
```

| Seam | What must agree | Check |
|---|---|---|
| OpenAPI ↔ proto | State names, enum values, ID prefixes, `Money` shape | `TestEnumsMatchAcrossContracts` |
| OpenAPI ↔ errors | Every `code` in an example or response exists in the catalogue | `make openapi-error-examples` |
| proto ↔ errors | Every `Error.code` a service can return exists in the catalogue | `TestEveryReturnedCodeIsCatalogued` |
| events ↔ baseline | The 25 catalogued types, their topics and partition keys | `TestCatalogMatchesSchemas` |
| events ↔ producers | Every event a producer can emit validates against its schema | `TestEveryPublishedEventValidatesAgainstItsSchema` |
| errors ↔ code | Go constants are **generated** from the catalogue, never hand-written | `go generate ./pkg/apierror` + `git diff --exit-code` |

### Who uses which

| Contract | Audience | Transport | Stability |
|---|---|---|---|
| `openapi/` | Tenant integrations, the console, partner SDKs | HTTPS, `application/json` | Public. Additive-only within `/v1`. Retirement announced by `Deprecation` and `Sunset` headers with at least 180 days' notice. |
| `proto/` | Our own nine deployables, service to service | gRPC over mTLS inside the mesh | Internal, but versioned as if public: nine binaries owned by five teams cannot deploy in lockstep, so a breaking change still costs a migration. |
| `events/` | Every bounded context, plus merchant webhooks | Kafka; JSON archived to S3 | Public by consequence. The envelope is the one shape that must never break. |
| `errors/` | All of the above | — | A code is a public contract. Its `retryable`, `http_status` and `grpc_code` are branched on by code we do not own. |

### Three rules that show up in all four

**Money is integer minor units plus a currency.** `{"amount": 1050, "currency": "USD"}`,
in the OpenAPI schema, the proto message, every event payload and every error detail.
Never a float — `float64` cannot represent 0.10 — and never a decimal string, which
merely relocates the parsing bug. The one deliberate exception is `payment.settled.v1`'s
`fxRate`, a decimal string because it is a rate rather than an amount.

**Cardholder data is structurally unrepresentable.** There is no `pan`, `cardNumber`,
`cvv`, `cvc`, `track1`, `track2` or `expiry` field anywhere in these contracts — not
optional, not deprecated, absent — and the proto message permanently reserves those field
names and numbers so a future contributor cannot quietly add one. Token fields must begin
with a letter, which makes a 13–19 digit PAN unable to satisfy the pattern; a bare network
token is PAN-formatted and is therefore referenced by vault handle rather than accepted
inline. Independently, the L1 validator runs a Luhn-checked detector over every string
field in every request. Schema and detector are belt and braces: a schema can be bypassed
by a field somebody adds later, a detector cannot.

**A timeout is not a failure.** `GATEWAY_TIMEOUT` is catalogued `retryable: false`, and
that flag is the most consequential one in the catalogue. It does not mean "give up"; it
means an automatic retry under a *fresh* idempotency key is forbidden, because the
authorization may already have succeeded. `POST /v1/payments` answers `202` with state
`PROCESSING` rather than an error, and the correct client behaviour is to poll. Retrying
with the *same* key is always safe.

---

## Compatibility policy

### REST (`openapi/`)

Major version in the URI, additive-only within it.

| Change | Allowed in `/v1`? |
|---|---|
| New endpoint, new optional request field, new response field | Yes |
| New enum value in a **response** | Yes — clients must tolerate unknown values |
| New enum value in a **request** | Yes |
| New **required** request field | No — `/v2` |
| Removing or renaming a field; narrowing a type; tightening validation | No — `/v2` |
| Making an optional request field required | No — `/v2` |
| Changing an error code's `retryable`, `http_status` or `grpc_code` | No — new code, deprecate the old |

Retirement: `Deprecation` announces the date the deprecation took effect, `Sunset` the
date the operation stops working, never less than 180 days later.

#### Corrections applied to `/v1` where the contract, not the platform, was wrong

The table above governs changes to a contract that describes the system accurately. It does not
govern the case where the contract described something the system never did — there the contract
is the defect, and correcting it is not a compatibility event, because no conforming
implementation ever produced the documented shape.

| Correction | What it was | What it is | Why the contract was the wrong end |
|---|---|---|---|
| `GatewayId` pattern | `^gw_[0-9A-Z]{26}$` | `^[a-z][a-z0-9-]{0,31}$` | `shared.GatewayID` is a stable, human-authored slug — `stripe`, `adyen` — and always has been. It is the routing key, the metric label, the adapter registry key and the value an operator types into a runbook, all of which want a name rather than a generated identifier. The ULID pattern described a value the platform has never emitted, so a client that validated against it rejected every real response. `gatewayCode` carried the slug all along, which is why nobody noticed. Both fields remain, with the same value. |
| Example identifiers | Several examples contained `U`, `I` and `O` | Regenerated within Crockford Base32 | `pkg/ids` excludes those four characters (`I`, `L`, `O`, `U`) so that a human transcribing an identifier cannot confuse `1`/`I` or `0`/`O`. The examples therefore failed the platform's own parser: anyone copying one out of the documentation into a request got `VALIDATION_FAILED`. Applied to `openapi/` and to `events/*.schema.json` alike. |

Both corrections narrow nothing a real client could have been relying on: the first accepts every
value the platform actually emits and the previous pattern accepted none of them, and the second
touches examples only.

### gRPC (`proto/`)

`buf breaking` against `main`, not against a release tag — a breaking change must be
caught in the pull request that introduces it, not at release time when it is already
merged. Field numbers are never reused; removed fields are `reserved` by number *and* by
name. Enums carry an explicit `_UNSPECIFIED = 0`, and a zero value is treated as "not
set" rather than as a default with meaning.

### Events (`events/`)

Major version in the type name; additive-only within it; a breaking change is a new
`.v2` published alongside `.v1` until the registry reports zero consumer groups on `.v1`
for 14 consecutive days. Adding an enum value is breaking unless the schema's
`x-unknown-behaviour` for that field is `ignore` — most are `route-to-dlq`, because
silently ignoring an unrecognised payment state is how money gets lost. Full policy and
the migration protocol: [`events/README.md`](./events/README.md).

### Errors (`errors/`)

Adding a code is additive. Changing an existing code's classification is breaking and is
done by introducing a new code and deprecating the old one with a `sunset` date. A code
is never deleted while any published contract references it.

---

## Codegen

All commands run from the repository root unless stated otherwise.

### Go server and client from OpenAPI

```bash
# Types, server interfaces and a strict-mode chi router
oapi-codegen -config build/oapi-codegen.server.yaml \
  api/openapi/payments-platform.v1.yaml > internal/infrastructure/httpx/gen/server_gen.go

# Typed client for tests/contract and tests/e2e
oapi-codegen -config build/oapi-codegen.client.yaml \
  api/openapi/payments-platform.v1.yaml > tests/contract/gen/client_gen.go
```

Generated files are committed. A build that regenerates them cannot be reproduced from a
tag once a generator version drifts, and reviewers should see the wire shape change in
the diff that changes it.

### Go from protobuf

```bash
cd api/proto
buf lint                       # STANDARD rule set, no exceptions
buf breaking --against 'https://github.com/udaykishore-resu/payments-platform.git#branch=main,subdir=api/proto'
buf generate                   # messages, gRPC stubs, validators, descriptor set
```

### Go error constants from the catalogue

```bash
go generate ./pkg/apierror     # -> pkg/apierror/codes_gen.go
make docs-errors               # -> docs/errors/<CODE>.md, one page per code
```

`codes_gen.go` carries the constants, the category lookup, the HTTP and gRPC status
mappings and the `retryable` table. Nothing in it is hand-written; a hand-edit is
detected by the `git diff --exit-code` step below.

### Merchant-facing SDKs

```bash
make sdk-typescript            # openapi-generator, published as @example/payments-sdk
make sdk-python                # openapi-generator, published as example-payments
```

---

## CI validation

`scripts/verify-contracts.sh` runs all of it and is wired into the `contracts` job. Each
step is independently runnable.

| # | Step | Command | Fails when |
|---|---|---|---|
| 1 | YAML parses | `python3 -c 'import yaml,sys;[yaml.safe_load(open(f)) for f in sys.argv[1:]]' api/**/*.yaml` | Any YAML file is malformed |
| 2 | JSON parses | `python3 -c 'import json,sys;[json.load(open(f)) for f in sys.argv[1:]]' api/events/*.json` | Any schema file is malformed |
| 3 | OpenAPI is valid 3.1 | `openapi-spec-validator api/openapi/payments-platform.v1.yaml` | The document violates the spec |
| 4 | No broken `$ref` | `scripts/check-openapi-refs.py` | A `$ref` does not resolve within the document |
| 5 | API style | `spectral lint api/openapi/payments-platform.v1.yaml --ruleset build/spectral.yaml` | An operation lacks `operationId`, a summary, a `4xx`, `x-rate-limit` or `x-idempotent`; an unsafe operation lacks `Idempotency-Key` |
| 6 | Proto lint | `cd api/proto && buf lint` | Naming, enum-zero-value or file-layout violations |
| 7 | Proto compatibility | `cd api/proto && buf breaking --against main` | A wire-breaking change |
| 8 | Event schemas | `python3 scripts/verify_event_schemas.py api/events` | A schema is invalid, or **any `examples` entry fails to validate against its own schema** |
| 9 | Event compatibility | `scripts/check-event-compat.sh` | A non-additive change without a new `.v<n+1>` file |
| 10 | Event catalogue | `go test ./api/... -run TestCatalogMatchesSchemas` | The baseline catalogue, this directory and the registry index disagree |
| 11 | Error catalogue | `scripts/verify-error-catalog.py` | A duplicate code, an unknown category or subsystem, a classification that diverges from its category without a stated reason, or a code used in a contract but absent from the catalogue |
| 12 | PAN detector | `scripts/scan-for-pan.sh api/` | A Luhn-valid 13–19 digit sequence appears in any contract, fixture or example |
| 13 | Codegen is current | `make generate && git diff --exit-code` | A generated file was hand-edited or a source contract changed without regeneration |
| 14 | Server conformance | `go test ./tests/contract/...` | A handler's responses do not validate against the OpenAPI document |
| 15 | Consumer conformance | `go test ./tests/contract/events/...` | A consumer rejects a golden fixture, or fails the unknown-field-injection test |

Steps 12 and 13 deserve a note. The PAN scan runs over the contracts themselves, not only
over runtime traffic, because a realistic-looking card number in an example gets copied
into a test fixture, then into a seed script, and eventually into a log. And step 13 is
what makes "generated from the catalogue" true rather than aspirational: without it,
`codes_gen.go` drifts the first time somebody is in a hurry.

### Local pre-commit

```bash
make verify-contracts          # steps 1-12
make generate                  # step 13's prerequisite
```

---

## Surface at a glance

| Contract | Size |
|---|---|
| REST operations | 26 across 21 paths (15 control-plane, 7 data-plane, 4 operational) |
| REST outbound webhooks | 10 |
| REST schemas | 112 components, 13 named error responses, 13 reusable parameters |
| gRPC services | 5 (`MerchantService`, `OnboardingService`, `PaymentService`, `GatewayService`, `ConfigurationService`) |
| gRPC RPCs | 45, including 2 server-streaming |
| Event types | 25, plus the envelope |
| Error codes | 64 top-level, 16 field-level sub-codes, across 11 categories |
