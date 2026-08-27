# Repository metadata

What GitHub shows before anyone opens a file: the About blurb, the topic chips and the social
preview. This file is the source of truth for all three, so they stay consistent when somebody
edits them a year from now.

---

## Repository description

For the **About** field. 306 characters, inside GitHub's 350-character limit.

```
Multi-tenant payment gateway onboarding and orchestration platform in Go. Nine deployables, hexagonal, no framework. A twelve-step durable saga onboards merchants; a scored-routing orchestrator executes payments with failover that refuses to guess: an unknown gateway outcome never retries and never fails.
```

**Why this wording.** The About field is read by people deciding whether to open the repository at
all, so it spends its budget on what is unusual rather than on what every payments repo claims.
"Nine deployables, hexagonal, no framework" is the shape. The final clause is the single most
distinctive property of the design — `TIMEOUT_UNKNOWN` is a first-class attempt outcome that
breaks the dispatch loop unconditionally, and refusing to guess is what the whole reconciliation
tier exists to make possible. No adjectives, no "production-grade", no "enterprise".

### Shorter alternative (215 characters)

If the About field renders truncated in your theme:

```
Multi-tenant payment gateway onboarding and orchestration platform in Go. Nine deployables, hexagonal, no framework. Twelve-step durable onboarding saga; a payment orchestrator that never guesses an unknown outcome.
```

---

## Topics

Twenty topics. Each is an existing, searched GitHub topic rather than a phrase invented for this
repository — a topic nobody searches is a chip that takes up space and returns nothing.

| Topic | Covers |
|---|---|
| `payments` | Domain, broadest entry point |
| `payment-gateway` | Domain, the specific integration surface |
| `payment-orchestration` | Domain, the specific category this belongs to |
| `fintech` | Domain, industry |
| `pci-dss` | Domain, the scope boundary that shapes eight of the nine deployables |
| `go` | Stack |
| `golang` | Stack — both spellings are separately searched on GitHub |
| `postgresql` | Stack, system of record |
| `kafka` | Stack, event backbone |
| `kubernetes` | Stack, deployment target |
| `terraform` | Stack, the substrate under `terraform/` |
| `microservices` | Architecture, the split into nine binaries |
| `hexagonal-architecture` | Architecture, ports and adapters with the dependency rule enforced |
| `domain-driven-design` | Architecture, nine bounded contexts and aggregates with invariants |
| `event-driven` | Architecture, the outbox-to-Kafka backbone |
| `transactional-outbox` | Architecture, the specific pattern that removes the dual write |
| `saga-pattern` | Architecture, the compensating twelve-step onboarding workflow |
| `idempotency` | Architecture, the contract that makes retries safe end to end |
| `multi-tenancy` | Architecture, RLS plus the isolation guard |
| `state-machine` | Architecture, fourteen FSMs generated into `docs/state-machines.md` |

Twenty topics is GitHub's maximum, and this list uses all of it. If you need to drop one, drop
`event-driven` — `transactional-outbox` and `kafka` already carry that meaning more precisely.

---

## Applying both

Against remote `github.com/udaykishore-resu/payments-platform`. Requires `gh auth login` with a
token carrying `repo` scope.

**Description** — one call:

```sh
gh repo edit udaykishore-resu/payments-platform \
  --description "Multi-tenant payment gateway onboarding and orchestration platform in Go. Nine deployables, hexagonal, no framework. A twelve-step durable saga onboards merchants; a scored-routing orchestrator executes payments with failover that refuses to guess: an unknown gateway outcome never retries and never fails."
```

**Topics** — `gh repo edit --add-topic` accepts one flag per topic and is additive:

```sh
gh repo edit udaykishore-resu/payments-platform \
  --add-topic payments \
  --add-topic payment-gateway \
  --add-topic payment-orchestration \
  --add-topic fintech \
  --add-topic pci-dss \
  --add-topic go \
  --add-topic golang \
  --add-topic postgresql \
  --add-topic kafka \
  --add-topic kubernetes \
  --add-topic terraform \
  --add-topic microservices \
  --add-topic hexagonal-architecture \
  --add-topic domain-driven-design \
  --add-topic event-driven \
  --add-topic transactional-outbox \
  --add-topic saga-pattern \
  --add-topic idempotency \
  --add-topic multi-tenancy \
  --add-topic state-machine
```

**Replacing the topic set outright** (the `--add-topic` form cannot remove one). This sets the
list to exactly the twenty above:

```sh
gh api -X PUT repos/udaykishore-resu/payments-platform/topics \
  -f 'names[]=payments' \
  -f 'names[]=payment-gateway' \
  -f 'names[]=payment-orchestration' \
  -f 'names[]=fintech' \
  -f 'names[]=pci-dss' \
  -f 'names[]=go' \
  -f 'names[]=golang' \
  -f 'names[]=postgresql' \
  -f 'names[]=kafka' \
  -f 'names[]=kubernetes' \
  -f 'names[]=terraform' \
  -f 'names[]=microservices' \
  -f 'names[]=hexagonal-architecture' \
  -f 'names[]=domain-driven-design' \
  -f 'names[]=event-driven' \
  -f 'names[]=transactional-outbox' \
  -f 'names[]=saga-pattern' \
  -f 'names[]=idempotency' \
  -f 'names[]=multi-tenancy' \
  -f 'names[]=state-machine'
```

**Verify what took effect:**

```sh
gh repo view udaykishore-resu/payments-platform --json description,repositoryTopics
```

---

## Social preview

The paragraph for the Open Graph card, and for anywhere the repository is introduced in prose —
a conference abstract, a portfolio entry, the first message of a pull request to somebody else's
project that links here.

> **payments-platform** is a reference implementation of the two problems that appear the moment a
> platform has more than one payment gateway and more than one merchant: getting a merchant from
> "signed up" to "processing live money", and keeping "did this money move?" answerable without
> ever answering it twice. Onboarding is a twelve-step durable saga with per-step compensations,
> two explicitly different kinds of pivot, and two manual gates that hold no worker resource while
> they wait. Orchestration is a routing engine that filters on correctness before it scores on
> preference — a cost weight can never outvote a data-residency constraint — feeding a dispatcher
> whose ordering is the product: the attempt row and the credential it will be signed with are
> committed before the gateway is called, the answer and its event commit in one transaction, and
> an unknown outcome breaks the loop rather than retrying. Nine Go binaries, one module, hexagonal
> with the dependency rule enforced in CI, PostgreSQL with row-level security and partition-aligned
> uniqueness that makes double-charging structurally impossible, a transactional outbox as the only
> path to Kafka, and forty-eight diagrams that are checked against the code rather than drawn once
> and left. It has never processed real money, and the documentation says so in the first screen.

### Image

The repository has no social-preview image set. If one is added, GitHub renders it at
1280×640 px (2:1) with a 1 MB ceiling; anything smaller than 640×320 is upscaled and looks it.
The obvious candidate is Diagram A of
[`docs/diagrams/02-high-level-design.md`](../docs/diagrams/02-high-level-design.md) — the planes
and deployables view, which is also the diagram reproduced in `README.md` — exported at 2× and
cropped to 2:1. Set it under **Settings → General → Social preview**; there is no `gh` command
for it.
