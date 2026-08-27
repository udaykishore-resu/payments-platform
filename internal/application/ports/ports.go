// Package ports declares every interface the application layer needs from the outside world.
//
// The interfaces live here — with the *consumer* — rather than beside their implementations.
// That is the whole point of the dependency inversion in a hexagonal architecture: the
// application says "I need to be able to load a payment by ID", and the Postgres package
// arranges to satisfy that. If the interface lived next to the Postgres implementation, the
// application would import infrastructure, the arrow would point the wrong way, and swapping
// the store would mean editing the use cases.
//
// Two consequences that are easy to get wrong and are enforced here:
//
//   - Interfaces are narrow. A twenty-method Repository interface is not an abstraction, it is
//     a database with extra steps: nothing can implement it but the database, and every test
//     double has to stub twenty methods to exercise one. Each interface below covers one
//     capability the application actually calls.
//   - Interfaces speak the domain's language, not the store's. There is no `Query`, no `Tx`
//     handle, no `sql.Rows` in this file. `UnitOfWork` is the one concession to transactional
//     grouping, and it is expressed as "run this function atomically", not as begin/commit.
//
// This package may import the domain and the standard library. It may not import anything from
// internal/infrastructure or internal/adapters.
package ports

import (
	"context"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/audit"
	"github.com/udaykishore-resu/payments-platform/internal/domain/config"
	"github.com/udaykishore-resu/payments-platform/internal/domain/gateway"
	"github.com/udaykishore-resu/payments-platform/internal/domain/ledger"
	"github.com/udaykishore-resu/payments-platform/internal/domain/merchant"
	"github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/domain/tenant"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// --- transactional grouping ------------------------------------------------------------------

// UnitOfWork runs a function inside one database transaction.
//
// The signature deliberately hides the transaction handle: the callback receives a Repositories
// bundle already bound to the transaction, so a use case cannot accidentally perform half its
// work inside the transaction and half outside — the single most common source of "the state
// committed but the event didn't" bugs, which is exactly what the outbox pattern exists to
// prevent and which a leaky transaction API reintroduces.
//
// Implementations must be re-entrant-safe in the sense that nesting is an error, not a silent
// no-op: a nested call returns an error rather than joining the outer transaction, because a
// caller who believes they have a savepoint and does not is worse off than one who gets an
// error.
type UnitOfWork interface {
	// Within runs fn inside a transaction, committing if fn returns nil and rolling back
	// otherwise. The tenant is taken from ctx and applied as the row-level-security scope for
	// the duration of the transaction.
	Within(ctx context.Context, fn func(ctx context.Context, r Repositories) error) error

	// WithinSerializable is Within at SERIALIZABLE isolation, for the few operations whose
	// correctness depends on it (concurrent refunds against one payment). Callers must be
	// prepared to retry on a serialization failure; the implementation reports those as a
	// retryable apierror so the generic retry wrapper handles them.
	WithinSerializable(ctx context.Context, fn func(ctx context.Context, r Repositories) error) error
}

// Repositories is the bundle of stores bound to one transaction.
type Repositories struct {
	Payments       PaymentRepository
	Merchants      MerchantRepository
	Tenants        TenantRepository
	Gateways       GatewayRepository
	Connections    ConnectionRepository
	Health         HealthRepository
	Configs        ConfigurationRepository
	Idempotency    IdempotencyStore
	Outbox         OutboxWriter
	Ledger         LedgerRepository
	Audit          AuditRepository
	Webhooks       WebhookRepository
	Workflows      WorkflowRepository
	Reconciliation ReconciliationRepository
}

// --- payment context -------------------------------------------------------------------------

// PaymentRepository persists the Payment aggregate together with its attempts and refunds.
type PaymentRepository interface {
	// Create inserts a new payment and drains its pending events into the outbox in the same
	// transaction.
	Create(ctx context.Context, p *payment.Payment) error

	// Get loads a payment by ID within the caller's tenant. It returns
	// apierror.CodePaymentNotFound for a payment that exists under a different tenant — never
	// a 403 — because distinguishing "not yours" from "does not exist" leaks the existence of
	// other tenants' payment IDs to anyone who can guess one.
	Get(ctx context.Context, id shared.PaymentID) (*payment.Payment, error)

	// GetForUpdate loads a payment with a row lock, for operations that read-modify-write.
	GetForUpdate(ctx context.Context, id shared.PaymentID) (*payment.Payment, error)

	// Save persists a modified aggregate using optimistic concurrency on the version loaded.
	// A version mismatch returns a retryable conflict rather than silently overwriting.
	Save(ctx context.Context, p *payment.Payment) error

	// SaveAttempt persists a single attempt without rewriting the whole aggregate. This exists
	// because the attempt must be committed *before* the gateway call, and that commit must be
	// as small and as fast as possible — it sits directly in the payment latency budget.
	SaveAttempt(ctx context.Context, a *payment.Attempt) error

	// List returns a tenant-scoped, cursor-paginated page.
	List(ctx context.Context, f PaymentFilter, page Page) ([]*payment.Payment, string, error)

	// FindUnresolved returns payments with an attempt in TIMEOUT_UNKNOWN older than olderThan.
	// This is the reconciler's work queue and the reason a gateway timeout is survivable.
	FindUnresolved(ctx context.Context, olderThan time.Duration, limit int) ([]*payment.Payment, error)

	// FindExpiredAuthorizations returns authorized payments past their expiry, for the sweeper
	// that moves them to EXPIRED.
	FindExpiredAuthorizations(ctx context.Context, now time.Time, limit int) ([]*payment.Payment, error)

	// CountOpen returns the number of payments in a non-terminal state for a merchant. Used by
	// the termination guard.
	CountOpen(ctx context.Context, merchantID shared.MerchantID) (int, error)
}

// PaymentFilter narrows a payment listing. All fields are optional and are AND-combined.
type PaymentFilter struct {
	MerchantID    shared.MerchantID
	States        []payment.State
	Currency      money.Currency
	GatewayID     shared.GatewayID
	CreatedAfter  *time.Time
	CreatedBefore *time.Time
	AmountMin     *money.Money
	AmountMax     *money.Money
}

// Page is opaque-cursor pagination. Offset pagination is deliberately not offered: it is
// unstable under concurrent inserts, so a client walking a busy merchant's payments with
// offsets silently skips and repeats rows.
type Page struct {
	Limit  int
	Cursor string
}

// --- merchant and tenant contexts --------------------------------------------------------------

// MerchantRepository persists the Merchant aggregate.
type MerchantRepository interface {
	Create(ctx context.Context, m *merchant.Merchant) error
	Get(ctx context.Context, id shared.MerchantID) (*merchant.Merchant, error)
	GetForUpdate(ctx context.Context, id shared.MerchantID) (*merchant.Merchant, error)
	GetByExternalRef(ctx context.Context, ref string) (*merchant.Merchant, error)
	Save(ctx context.Context, m *merchant.Merchant) error
	List(ctx context.Context, f MerchantFilter, page Page) ([]*merchant.Merchant, string, error)
	// FindKYCExpiring returns merchants whose verification lapses within the window, so
	// re-verification starts before processing stops rather than after.
	FindKYCExpiring(ctx context.Context, within time.Duration, limit int) ([]*merchant.Merchant, error)
}

// MerchantFilter narrows a merchant listing.
type MerchantFilter struct {
	Statuses    []merchant.Status
	Country     shared.Country
	Environment shared.Environment
	Search      string
}

// TenantRepository persists tenants and their API clients.
type TenantRepository interface {
	Get(ctx context.Context, id shared.TenantID) (*tenant.Tenant, error)
	Save(ctx context.Context, t *tenant.Tenant) error
	GetAPIClient(ctx context.Context, id shared.APIClientID) (*tenant.APIClient, error)
}

// --- gateway context ---------------------------------------------------------------------------

// GatewayRepository is the gateway registry.
type GatewayRepository interface {
	Get(ctx context.Context, id shared.GatewayID) (*gateway.Gateway, error)
	List(ctx context.Context) ([]*gateway.Gateway, error)
	Save(ctx context.Context, g *gateway.Gateway) error
}

// ConnectionRepository persists merchant-to-gateway bindings.
type ConnectionRepository interface {
	Get(ctx context.Context, id shared.ConnectionID) (*gateway.Connection, error)
	GetByMerchantGateway(ctx context.Context, m shared.MerchantID, g shared.GatewayID) (*gateway.Connection, error)
	ListForMerchant(ctx context.Context, m shared.MerchantID) ([]*gateway.Connection, error)
	Save(ctx context.Context, c *gateway.Connection) error
	// FindCredentialsDueForRotation returns connections whose credentials exceed the maximum
	// age, driving the automated rotation workflow.
	FindCredentialsDueForRotation(ctx context.Context, olderThan time.Duration, limit int) ([]*gateway.Connection, error)
}

// HealthRepository persists gateway health so that a restarting pod does not begin life
// believing every gateway is perfectly healthy — which is how a fleet-wide restart during an
// outage sends a thundering herd at a dead gateway.
type HealthRepository interface {
	Get(ctx context.Context, g shared.GatewayID, op shared.Operation) (*gateway.Health, error)
	ListAll(ctx context.Context) ([]*gateway.Health, error)
	Save(ctx context.Context, h *gateway.Health) error
}

// --- configuration -----------------------------------------------------------------------------

// ConfigurationRepository persists versioned merchant configuration. Writes are append-only:
// a rollback publishes the previous document as a new version rather than deleting, so history
// is never destroyed.
type ConfigurationRepository interface {
	GetActive(ctx context.Context, m shared.MerchantID) (*config.MerchantConfig, error)
	GetVersion(ctx context.Context, m shared.MerchantID, version int) (*config.MerchantConfig, error)
	ListVersions(ctx context.Context, m shared.MerchantID, page Page) ([]*config.MerchantConfig, string, error)
	// Publish stores a new version. expectedVersion implements the If-Match/ETag optimistic
	// concurrency contract; a mismatch is a 412, not a silent overwrite of someone else's edit.
	Publish(ctx context.Context, c *config.MerchantConfig, expectedVersion int) error
	// ListActiveSince returns configurations published after the given version watermark,
	// which is how a data-plane replica warms and refreshes its snapshot.
	ListActiveSince(ctx context.Context, since time.Time, limit int) ([]*config.MerchantConfig, error)
}

// ConfigProvider is the data plane's read-only, cached view of merchant configuration.
//
// It is a separate port from ConfigurationRepository on purpose. The control plane reads
// configuration from the database, strongly consistent. The data plane reads it from a local
// snapshot with bounded staleness, and — critically — keeps serving from the last known good
// snapshot when the control plane is unreachable (baseline §15, fail-static). Giving the data
// plane the repository interface would let someone add a synchronous database call to the
// payment hot path without noticing they had done it.
type ConfigProvider interface {
	// Get returns the merchant's effective configuration. It returns a stale snapshot rather
	// than failing while the snapshot age is within tolerance.
	Get(ctx context.Context, m shared.MerchantID) (*config.MerchantConfig, error)
	// SnapshotAge reports how long ago the local view was last refreshed. Exported as
	// pp_config_snapshot_age_seconds and alerted on past 5 minutes.
	SnapshotAge() time.Duration
	// Invalidate drops a merchant from the local view, used by the priority path that handles
	// merchant.suspended.v1 without waiting for the natural refresh.
	Invalidate(m shared.MerchantID)
}

// --- idempotency --------------------------------------------------------------------------------

// IdempotencyStore is the authoritative record of which logical operations have run.
//
// Postgres implements this. Redis does not: Redis is a read-through accelerator in front of it
// (ADR-009). Making the cache authoritative would mean a Redis eviction under memory pressure
// silently converts a duplicate request into a second payment, and eviction under memory
// pressure is not a rare event.
type IdempotencyStore interface {
	// Claim atomically inserts an IN_FLIGHT record, or reports what already exists. The
	// insert is ON CONFLICT DO NOTHING against a unique index, which is what makes two
	// concurrent identical requests resolve deterministically rather than racing.
	Claim(ctx context.Context, rec IdempotencyRecord) (ClaimResult, error)
	// Complete stores the response snapshot and marks the record COMPLETED.
	Complete(ctx context.Context, key IdempotencyKey, snapshot ResponseSnapshot) error
	// FailTerminal stores a non-retryable failure snapshot, so a client retrying a request
	// that will never succeed gets the same error rather than a fresh attempt.
	FailTerminal(ctx context.Context, key IdempotencyKey, snapshot ResponseSnapshot) error
	// Release removes an IN_FLIGHT claim after a retryable failure, so the client's retry is
	// a genuine new attempt rather than a replay of an error that may since have cleared.
	Release(ctx context.Context, key IdempotencyKey) error
	// PurgeExpired deletes records past their retention. Run by a scheduled job, not inline.
	PurgeExpired(ctx context.Context, before time.Time, limit int) (int, error)
}

// IdempotencyKey is the full scope of an idempotency record: the client's key is only unique
// within a tenant, a merchant and an endpoint. Scoping by the key alone would let one tenant's
// choice of key collide with another's.
type IdempotencyKey struct {
	TenantID     shared.TenantID
	MerchantID   shared.MerchantID
	Method       string
	PathTemplate string
	Key          string
}

// IdempotencyRecord is a claim on an operation.
type IdempotencyRecord struct {
	Key IdempotencyKey
	// Fingerprint is SHA-256 over the canonicalized request body plus the scope. A matching
	// key with a different fingerprint is a client bug — the same key used for two different
	// operations — and must be reported rather than silently treated as a replay.
	Fingerprint    string
	LeaseExpiresAt time.Time
	ExpiresAt      time.Time
	RequestID      string
	TraceID        string
}

// ClaimOutcome is what happened when a claim was attempted.
type ClaimOutcome string

const (
	// ClaimNew means this caller owns the operation and should execute it.
	ClaimNew ClaimOutcome = "NEW"
	// ClaimReplay means the operation already completed; return the stored response.
	ClaimReplay ClaimOutcome = "REPLAY"
	// ClaimInProgress means another caller holds a live lease. The correct response is 409
	// with Retry-After, not blocking on the lease (baseline §14.3, ADR-009).
	ClaimInProgress ClaimOutcome = "IN_PROGRESS"
	// ClaimFingerprintMismatch means the key was reused with a different request body.
	ClaimFingerprintMismatch ClaimOutcome = "FINGERPRINT_MISMATCH"
	// ClaimReclaimed means a previous holder's lease expired and this caller has taken over.
	ClaimReclaimed ClaimOutcome = "RECLAIMED"
)

// ClaimResult reports the outcome of a claim and, for a replay, the stored response.
type ClaimResult struct {
	Outcome       ClaimOutcome
	Snapshot      *ResponseSnapshot
	RetryAfter    time.Duration
	OriginalReqID string
	OriginalTrace string
}

// ResponseSnapshot is the stored result of a completed idempotent operation, replayed verbatim
// to a duplicate request.
type ResponseSnapshot struct {
	StatusCode int
	Body       []byte
	// ResourceID lets the replay path avoid re-serializing: for a payment creation this is the
	// payment ID, so a caller that only wants the ID need not parse the body.
	ResourceID  string
	CompletedAt time.Time
}

// --- events and outbox ----------------------------------------------------------------------------

// OutboxWriter appends events to the transactional outbox.
//
// It takes the same *ctx* and therefore the same transaction as the state change. That is the
// entire mechanism: the state row and the event row commit together or not at all, so the dual
// write problem never arises (baseline §13.4).
type OutboxWriter interface {
	Append(ctx context.Context, msgs ...OutboxMessage) error
}

// OutboxMessage is one event queued for publication.
type OutboxMessage struct {
	ID            shared.EventID
	TenantID      shared.TenantID
	Topic         string
	Type          string
	AggregateID   string
	AggregateType string
	PartitionKey  string
	Payload       []byte
	Headers       map[string]string
	OccurredAt    time.Time
	// AvailableAt supports delayed publication, used by the retry tiers.
	AvailableAt time.Time
}

// OutboxReader is the relay's side of the outbox: claim a batch, publish, mark done.
type OutboxReader interface {
	// Claim locks up to limit unpublished messages with FOR UPDATE SKIP LOCKED, so multiple
	// relay replicas can run without contending or duplicating. shardKey partitions the claim
	// across replicas so that one aggregate's messages are always claimed by the same replica,
	// which is what preserves per-aggregate ordering when the relay is scaled out.
	Claim(ctx context.Context, shard, totalShards, limit int) ([]OutboxMessage, error)
	MarkPublished(ctx context.Context, ids []shared.EventID) error
	MarkFailed(ctx context.Context, id shared.EventID, err error, retryAt time.Time) error
	Backlog(ctx context.Context) (int, error)
}

// EventPublisher sends an event to the broker.
type EventPublisher interface {
	Publish(ctx context.Context, msgs ...OutboxMessage) error
	Close() error
}

// EventConsumer subscribes to topics and delivers events to a handler.
type EventConsumer interface {
	Subscribe(ctx context.Context, topics []string, group string, h EventHandler) error
	Close() error
}

// EventHandler processes one event. Returning nil acknowledges it; returning a retryable error
// sends it to the retry tier; returning a non-retryable error sends it to the dead-letter
// topic. The distinction comes from apierror's Retryable bit, which is why every layer bothers
// to classify its errors.
type EventHandler interface {
	Handle(ctx context.Context, msg OutboxMessage) error
}

// DedupStore records which (consumer group, event) pairs have been processed. Combined with
// at-least-once delivery this produces effectively-once business semantics; combined with the
// database invariants it survives a bug in itself (baseline §13.5).
type DedupStore interface {
	// MarkProcessed inserts the pair, reporting false if it was already present. The insert
	// must happen in the same transaction as the handler's work — a dedup row committed
	// separately from the effect it guards is a dedup row that lies.
	MarkProcessed(ctx context.Context, group string, id shared.EventID) (bool, error)
	Purge(ctx context.Context, before time.Time, limit int) (int, error)
}

// --- ledger, audit, webhooks, reconciliation --------------------------------------------------------

// LedgerRepository appends balanced transactions. There is no update and no delete: a
// correction is a new compensating transaction.
type LedgerRepository interface {
	Append(ctx context.Context, tx *ledger.Transaction) error
	Balance(ctx context.Context, key ledger.AccountKey) (money.Money, error)
	EntriesForPayment(ctx context.Context, id shared.PaymentID) ([]ledger.Entry, error)
}

// AuditRepository appends to the hash chain and reads it back for verification.
type AuditRepository interface {
	Append(ctx context.Context, r audit.Record) error
	// LastDigest returns the tail of the chain for a tenant, which the next append links to.
	LastDigest(ctx context.Context, t shared.TenantID) (string, int64, error)
	Query(ctx context.Context, f AuditFilter, page Page) ([]audit.Record, string, error)
	// VerifyRange re-computes the chain over a range and reports the first tampered sequence.
	VerifyRange(ctx context.Context, t shared.TenantID, from, to int64) (bool, int64, error)
}

// AuditFilter narrows an audit query.
type AuditFilter struct {
	ActorID      string
	Action       string
	ResourceType string
	ResourceID   string
	From, To     *time.Time
}

// WebhookRepository persists inbound gateway webhooks and their deduplication state.
type WebhookRepository interface {
	// Record stores the raw webhook. It returns false if the (gateway, gateway event ID) pair
	// has been seen, which is the deduplication check — done at the storage layer with a
	// unique index rather than in memory, so it survives a pod restart and works across
	// replicas.
	Record(ctx context.Context, w InboundWebhook) (bool, error)
	Get(ctx context.Context, id shared.WebhookID) (*InboundWebhook, error)
	ClaimUnprocessed(ctx context.Context, limit int) ([]InboundWebhook, error)
	MarkProcessed(ctx context.Context, id shared.WebhookID, result string) error
	MarkFailed(ctx context.Context, id shared.WebhookID, err error, retryAt time.Time) error
}

// WebhookRecorder stores a delivery whose tenant is not yet known.
//
// # Why the ingress does not use WebhookRepository for this
//
// Every repository in this platform is reached through a unit of work that refuses to open a
// transaction without a tenant in the context, and that refusal is the isolation guarantee rather
// than a convenience. The inbound webhook is the single row that must be written before the
// tenant can be known: a gateway posts holding no platform credential, and which tenant the
// delivery concerns is not decidable until the payload has been resolved to a payment — which
// happens later, in the processor, deliberately after the 202 has been returned.
//
// Giving the ingress its own one-method port is what keeps that exception visible. The
// alternative — relaxing the unit of work so it tolerates a missing tenant — would relax it for
// every write in the platform to accommodate one, and the next caller to hit the guard would find
// it already disarmed.
type WebhookRecorder interface {
	// Record stores the raw delivery, returning false when the (gateway, gateway event id) pair
	// has been seen before. Implementations must refuse a record that already names a tenant:
	// once tenancy is known the write belongs on the tenanted path, where row-level security
	// proves the row is filed under the tenant the transaction was opened for.
	Record(ctx context.Context, w InboundWebhook) (bool, error)
}

// InboundWebhook is a received gateway notification, stored before any processing.
//
// Store first, process later, always. A webhook endpoint that processes synchronously is an
// endpoint that times out under load and makes the gateway retry, multiplying the load exactly
// when it is highest. The 50 ms budget on this endpoint buys the platform the right to be slow
// at processing without being slow at accepting.
type InboundWebhook struct {
	ID             shared.WebhookID
	GatewayID      shared.GatewayID
	TenantID       shared.TenantID
	MerchantID     shared.MerchantID
	GatewayEventID string
	EventType      string
	Signature      string
	Payload        []byte
	Headers        map[string]string
	ReceivedAt     time.Time
	ProcessedAt    *time.Time
	Attempts       int
	Status         string
	LastError      string
}

// ReconciliationRepository tracks unresolved outcomes and settlement exceptions.
type ReconciliationRepository interface {
	OpenException(ctx context.Context, e ReconciliationException) error
	ListOpen(ctx context.Context, severity string, page Page) ([]ReconciliationException, string, error)
	Resolve(ctx context.Context, id string, resolution string, by string) error
	CountOpen(ctx context.Context) (map[string]int, error)
}

// ReconciliationException is a discrepancy requiring resolution.
type ReconciliationException struct {
	ID         string
	TenantID   shared.TenantID
	MerchantID shared.MerchantID
	PaymentID  shared.PaymentID
	AttemptID  shared.AttemptID
	Kind       string
	Severity   string
	Detail     string
	OpenedAt   time.Time
	ResolvedAt *time.Time
	Resolution string
	ResolvedBy string
}

// WorkflowRepository is the durable store behind the Postgres workflow engine. Its methods are
// intentionally storage-shaped rather than domain-shaped because the engine *is* the domain
// logic here; see internal/workflows/engine.
type WorkflowRepository interface {
	CreateInstance(ctx context.Context, i WorkflowInstanceRecord) error
	GetInstance(ctx context.Context, id shared.WorkflowID) (*WorkflowInstanceRecord, error)
	GetInstanceByBusinessKey(ctx context.Context, def, key string) (*WorkflowInstanceRecord, error)
	// LeaseRunnable claims instances that are ready to advance, using FOR UPDATE SKIP LOCKED
	// with a lease and a fencing epoch so a paused worker that wakes up cannot act on an
	// instance another worker has taken over.
	LeaseRunnable(ctx context.Context, workerID string, lease time.Duration, limit int) ([]WorkflowInstanceRecord, error)
	Heartbeat(ctx context.Context, id shared.WorkflowID, workerID string, epoch int64, extend time.Duration) error
	SaveInstance(ctx context.Context, i WorkflowInstanceRecord) error
	SaveStep(ctx context.Context, s WorkflowStepRecord) error
	ListSteps(ctx context.Context, id shared.WorkflowID) ([]WorkflowStepRecord, error)
	PushDLQ(ctx context.Context, id shared.WorkflowID, step string, payload []byte, reason string) error
	CountByState(ctx context.Context) (map[string]int, error)
	// FindStuck returns instances that have made no progress within the threshold. A workflow
	// that is neither running nor failed nor complete is the failure mode nobody alerts on
	// until a merchant calls.
	FindStuck(ctx context.Context, noProgressFor time.Duration, limit int) ([]WorkflowInstanceRecord, error)
}

// WorkflowInstanceRecord is the persisted form of a workflow instance.
type WorkflowInstanceRecord struct {
	ID            shared.WorkflowID
	TenantID      shared.TenantID
	Definition    string
	Version       int
	BusinessKey   string
	State         string
	CurrentStep   string
	Input         []byte
	Context       []byte
	Attempt       int
	LeaseOwner    string
	LeaseEpoch    int64
	LeaseUntil    *time.Time
	RunAfter      time.Time
	LastError     string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	CompletedAt   *time.Time
	CorrelationID string
}

// WorkflowStepRecord is the persisted, checkpointed result of one step.
type WorkflowStepRecord struct {
	ID            shared.StepID
	WorkflowID    shared.WorkflowID
	TenantID      shared.TenantID
	Name          string
	Sequence      int
	State         string
	Attempt       int
	Input         []byte
	Output        []byte
	Error         string
	StartedAt     *time.Time
	CompletedAt   *time.Time
	CompensatedAt *time.Time
}

// --- external capabilities -----------------------------------------------------------------------

// SecretsProvider resolves a secret reference to its material.
//
// The reference (`secret://env/tenant/merchant/gateway#version`) is what the platform stores
// and passes around; the material never appears in a database row, a config file, a log line
// or a struct that gets serialized. Resolution happens at the moment of use and the result is
// held in a redacting wrapper.
type SecretsProvider interface {
	Get(ctx context.Context, ref string) (SecretMaterial, error)
	Put(ctx context.Context, ref string, material map[string]string) (versionedRef string, err error)
	// Rotate writes a new version and returns its reference, leaving the previous version
	// readable for the overlap window so that in-flight requests using the old credential do
	// not fail during a rotation.
	Rotate(ctx context.Context, ref string, material map[string]string, overlap time.Duration) (string, error)
	Delete(ctx context.Context, ref string) error
}

// SecretMaterial carries resolved secret values. Implementations must return a type whose
// String, Format and MarshalJSON redact; see internal/platform/secret.
type SecretMaterial interface {
	Value(field string) (string, bool)
	Fields() []string
	Version() string
}

// KYCProvider is the anti-corruption boundary around the identity-verification vendor.
type KYCProvider interface {
	Submit(ctx context.Context, req KYCSubmission) (KYCSubmissionResult, error)
	Get(ctx context.Context, providerRef string) (KYCDecision, error)
	Cancel(ctx context.Context, providerRef string) error
}

// KYCSubmission is a verification request. The platform sends this and retains only the
// provider's reference afterwards.
type KYCSubmission struct {
	IdempotencyKey string
	MerchantID     shared.MerchantID
	LegalName      string
	Country        shared.Country
	RegistrationNo string
	TaxID          string
	Address        map[string]string
	Principals     []merchant.Principal
	DocumentRefs   []string
}

// KYCSubmissionResult is the vendor's acknowledgement.
type KYCSubmissionResult struct {
	ProviderRef string
	Status      merchant.KYCStatus
	SubmittedAt time.Time
}

// KYCDecision is the vendor's outcome.
type KYCDecision struct {
	ProviderRef string
	Status      merchant.KYCStatus
	RiskRating  merchant.RiskRating
	ExpiresAt   time.Time
	Reasons     []string
	DecidedAt   time.Time
	DocumentIDs []string
}

// BankValidator verifies a settlement account.
type BankValidator interface {
	Validate(ctx context.Context, req BankValidationRequest) (BankValidationResult, error)
}

// BankValidationRequest carries the account to verify. The account details themselves come
// from the secrets store by reference, not as fields here.
type BankValidationRequest struct {
	IdempotencyKey string
	MerchantID     shared.MerchantID
	AccountID      string
	SecretRef      string
	Country        shared.Country
	Currency       money.Currency
	Method         string
}

// BankValidationResult is the verification outcome. Pending is a real outcome: micro-deposit
// verification takes days, and modelling it as failure would restart onboarding every time.
type BankValidationResult struct {
	Reference     string
	Verified      bool
	Pending       bool
	FailureReason string
	CompletedAt   *time.Time
}

// RiskScorer is the optional external fraud model. It is a port so that the platform can run
// without one, and so that its unavailability degrades to the policy default rather than to
// "approve" (baseline §12, stage 11).
type RiskScorer interface {
	Score(ctx context.Context, req RiskScoreRequest) (RiskScoreResult, error)
}

// RiskScoreRequest carries the signals a scorer needs.
type RiskScoreRequest struct {
	PaymentID     shared.PaymentID
	MerchantID    shared.MerchantID
	Amount        money.Money
	PaymentMethod shared.PaymentMethod
	PayerCountry  shared.Country
	IssuerCountry shared.Country
	EmailHash     string
	IPAddress     string
	DeviceID      string
}

// RiskScoreResult is a 0..100 score with an optional recommendation.
type RiskScoreResult struct {
	Score          int
	Recommendation string
	Signals        map[string]string
}

// VelocityCounter supplies the rolling counters the risk engine needs. It is separate from a
// generic cache because the operation is "increment and read a windowed count atomically", and
// implementing that with get-then-set is a race that under-counts precisely during an attack.
type VelocityCounter interface {
	IncrementAndCount(ctx context.Context, key string, window time.Duration) (int64, error)
	Count(ctx context.Context, key string, window time.Duration) (int64, error)
	SumAndAdd(ctx context.Context, key string, window time.Duration, add money.Money) (money.Money, error)
}

// Cache is the generic tenant-scoped cache port.
type Cache interface {
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	Delete(ctx context.Context, key string) error
	// GetOrLoad performs single-flight loading so that a cold key under load results in one
	// origin call, not one per concurrent request — the cache-stampede failure mode.
	GetOrLoad(ctx context.Context, key string, ttl time.Duration, load func(ctx context.Context) ([]byte, error)) ([]byte, error)
}

// DistributedLock guards operations that must not run concurrently across replicas, such as a
// scheduled sweep. It is deliberately not used for correctness on the payment path: correctness
// there comes from database constraints, which do not evaporate when a lock's TTL expires
// mid-operation.
type DistributedLock interface {
	Acquire(ctx context.Context, key string, ttl time.Duration) (release func(context.Context) error, acquired bool, err error)
}

// ObjectStore holds compliance artifacts, certification reports and audit exports.
type ObjectStore interface {
	Put(ctx context.Context, key string, body []byte, contentType string, opts ObjectOptions) error
	Get(ctx context.Context, key string) ([]byte, error)
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
	Exists(ctx context.Context, key string) (bool, error)
}

// ObjectOptions carries retention and immutability requirements. WORM is not decoration: a
// KYC evidence file that can be overwritten is not evidence.
type ObjectOptions struct {
	WORM        bool
	RetainUntil *time.Time
	SSEKMSKeyID string
	Metadata    map[string]string
}

// Notifier delivers merchant-facing notifications: outbound webhooks and email.
type Notifier interface {
	Notify(ctx context.Context, n Notification) error
}

// Notification is one outbound message.
type Notification struct {
	TenantID   shared.TenantID
	MerchantID shared.MerchantID
	Type       string
	Subject    string
	Payload    []byte
	Endpoints  []string
}
