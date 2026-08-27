// Package temporal is the second implementation of engine.Engine, behind the same port as
// engine/postgres.
//
// # Build constraint
//
// The adapter itself — temporal.go — carries `//go:build temporal` and **compiles only with
// `-tags temporal`**, after `go get go.temporal.io/sdk`. The default build of this repository
// deliberately excludes it and does not depend on the Temporal SDK: the module is pinned, the
// SDK pulls in a large gRPC and protobuf surface, and a dependency that only one alternative
// implementation needs should not be in every binary's supply chain. `go build ./...` and
// `go test ./...` therefore see only this file and contract.go, both of which are SDK-free.
//
// The point of the port is that `internal/workflows/onboarding` — the definition and its twelve
// activities — is **unchanged** between the two implementations. Switching costs an adapter and
// a migration of live instances (drain the old engine, start new instances on the new one), not
// a rewrite of the saga. That is what makes the build-vs-buy decision reversible rather than a
// one-way door.
//
// # Concept mapping
//
//	Ours                          Temporal                       Notes on the mapping
//	────────────────────────────  ─────────────────────────────  ─────────────────────────────────
//	Definition                    Workflow Type + a generated    The workflow function is generated
//	(merchant-onboarding@v1)      workflow function that walks   *from* the Definition, so the
//	                              the same step list             definition stays the single
//	                                                             source of truth
//	Version                       Type-name suffix + GetVersion  Temporal patches in-flight code;
//	                                                             ours is "new version, new
//	                                                             definition, old instances finish
//	                                                             on the old one"
//	Instance                      Workflow Execution             1:1
//	BusinessKey (merchant_id)     Workflow ID +                  Gives the same "starting twice is
//	                              WorkflowIDReusePolicy:         a no-op" guarantee
//	                              AllowDuplicateFailedOnly
//	Step / Activity               Activity Type                  1:1
//	Step checkpoint               Activity result in Event       Same guarantee, different storage
//	(workflow_steps.output)       History
//	Replay-free resume            **Deterministic replay** of    *The fundamental difference.*
//	(read context, run next step) workflow code, completed       Temporal re-executes the workflow
//	                              activities short-circuited     function and short-circuits
//	                              from history                   completed activities. Ours never
//	                                                             re-executes anything. Consequence:
//	                                                             Temporal imposes determinism
//	                                                             constraints on workflow code (no
//	                                                             time.Now, no rand, no map
//	                                                             iteration, no direct I/O); we
//	                                                             impose none, because we never
//	                                                             replay
//	LeaseEpoch fencing            Activity task token validity   Temporal rejects a stale task
//	                              + workflow task versioning     token; we reject a stale epoch
//	lease_duration                ScheduleToStart + StartToClose Ours is one lease covering the
//	                              + HeartbeatTimeout             instance; Temporal's are three
//	                                                             per-activity timeouts
//	Input.Heartbeat               activity.RecordHeartbeat       1:1, heartbeat details ≈ progress
//	Input.Checkpoint / Lookup     Heartbeat details +            Same idea
//	                              activity.GetHeartbeatDetails
//	RetryPolicy                   temporal.RetryPolicy           Near 1:1. **Temporal's jitter is
//	                                                             fixed at ±20 %**, not full jitter
//	                                                             — a real behavioural difference
//	                                                             under a vendor-wide blip
//	FailureClass                  NonRetryableErrorTypes +       Our classifier is a *function* and
//	                              ApplicationError type strings  can classify on error content (an
//	                                                             HTTP status inside a wrapped
//	                                                             chain); Temporal's is a type list
//	                                                             and needs the class encoded in the
//	                                                             error type
//	Compensation, reverse order   **No native construct.**       Temporal has no first-class
//	                              An explicit saga stack in the  compensation
//	                              generated workflow code
//	Manual gate (SignalStep)      workflow.GetSignalChannel +    1:1, including durable
//	                              workflow.Await with a timer    early-arriving signals
//	Cancel                        Cancellation scope +           1:1
//	                              CancelWorkflow
//	workflow_dlq                  **No native DLQ.** A terminally Ours is a first-class table with
//	                              failed execution plus a custom  a triage runbook
//	                              archival/requeue handler
//	Requeue                       ResetWorkflowExecution to an   Temporal's reset is more powerful;
//	                              event ID                       ours is simpler to reason about
//	Operator surface              Temporal Web UI + tctl         Theirs is far better out of the
//	                                                             box; Temporal's strongest argument
//	Audit                         Event History + Search         Ours is audit_records,
//	                              Attributes                     hash-chained, in the same store and
//	                                                             retention regime as everything else
//	**Transactional coupling to   **Not available.** An activity *The fundamental cost.* Our step
//	domain state**                commits to our database; the   commit updates workflow_steps,
//	                              workflow's progress commits to workflow_instances, merchants and
//	                              Temporal's. Two stores, two    outbox_events in one transaction.
//	                              commits                        With Temporal, "the activity
//	                                                             succeeded" and "the merchant is
//	                                                             KYC_APPROVED" are two commits with
//	                                                             a window between them, and closing
//	                                                             it makes every activity's
//	                                                             idempotency load-bearing rather
//	                                                             than defence in depth
//
// # Choosing an implementation
//
// Choose the **Postgres** engine when:
//
//   - Workflow progress must commit atomically with domain state. This is our case: step 3's KYC
//     approval and the merchant's KYC_APPROVED transition are one fact, and splitting them across
//     two stores creates a window that has to be reconciled forever.
//   - Volume is low — tens to hundreds of instances a day — so the engine's cost is dominated by
//     our own code rather than by throughput.
//   - The workflow catalogue is small and stable: one definition, twelve steps.
//   - Zero additional stateful infrastructure and one backup/DR story are worth having.
//   - Activity code should be free of determinism constraints.
//   - Auditability must live in our own hash-chained store under our own retention policy.
//
// Choose **Temporal** when:
//
//   - Workflows span systems that do not share a database.
//   - Volume is high — thousands per second — or workflows are long-lived at large fan-out.
//   - Many teams author many workflows and need a shared platform with a shared operator UI.
//   - Running one more stateful cluster, or paying for Temporal Cloud, is acceptable.
//   - The team is disciplined about determinism, or needs Temporal's timers, child workflows or
//     continue-as-new.
//
// **The trigger to switch**, written down now so it is a decision rather than a drift: more than
// three distinct workflow definitions, or more than 10⁵ instances per day, or a workflow that
// must span a system with no shared database.
//
// # What is in this package
//
//   - doc.go (this file, no build tag): the mapping and the decision criteria.
//   - contract.go (no build tag): EngineContractSuite, the behavioural contract **both**
//     implementations must satisfy. It imports `testing` from a non-test file on purpose, the
//     same way net/http/httptest does, so that an implementation in any package can run it. It
//     does not import the Temporal SDK.
//   - temporal.go (`//go:build temporal`): the adapter, written against the real
//     go.temporal.io/sdk client, workflow, activity and worker APIs.
//   - temporal_guard_test.go (`//go:build temporal`): a compile-time assertion that the adapter
//     satisfies engine.Engine, so a signature change in the port breaks the tagged build rather
//     than being discovered the day someone tries to switch.
package temporal
