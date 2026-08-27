package onboarding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/workflows/engine"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// The seven assertions of baseline §11.4. Certification is a machine-checked matrix, not a
// checkbox: for every (gateway, payment method, currency) the merchant enabled, all seven must
// pass. Each one exists because a specific class of integration defect is invisible until
// production, and each is named here so a failing report tells an engineer what to fix rather
// than that "certification failed".
const (
	// AssertRoundTrip proves the happy path end to end. An integration that authorizes but
	// cannot refund is one the merchant discovers on their first return.
	AssertRoundTrip = "AUTH_CAPTURE_REFUND_ROUND_TRIP"
	// AssertVoid proves reversal works *before* real money is at risk.
	AssertVoid = "AUTHORIZE_THEN_VOID"
	// AssertDeclineMapping proves the anti-corruption layer maps vendor errors correctly. An
	// unmapped decline becomes DeclineUnknown, which forbids failover — so a broken mapping
	// silently turns every soft decline into a lost sale.
	AssertDeclineMapping = "DECLINE_MAPPED_TO_NORMALIZED_REASON"
	// AssertWebhook proves the asynchronous loop is closed: received, signature-verified, and
	// actually moving the payment state. This is why webhooks are registered at step 7, before
	// any sandbox transaction — registering them after certification would make this
	// unverifiable.
	AssertWebhook = "WEBHOOK_VERIFIED_AND_APPLIED"
	// AssertThreeDS proves SCA compliance: the challenge is reachable and completable.
	AssertThreeDS = "THREE_DS_CHALLENGE_COMPLETES"
	// AssertIdempotency proves idempotency in the *real* integration rather than in our tests.
	// Every crash-safety argument in the automation plane rests on the vendor deduplicating a
	// repeated idempotency key; this is where that assumption is checked against the vendor.
	AssertIdempotency = "DUPLICATE_IDEMPOTENCY_KEY_RETURNS_SAME_RESULT"
	// AssertEcho proves L6 response validation: a gateway that echoes a different amount or
	// currency than we sent is a contract violation we must catch, not silently accept.
	AssertEcho = "AMOUNT_AND_CURRENCY_ECHOED"
)

// AllAssertions is the full list, in the order the suite runs them.
var AllAssertions = []string{
	AssertRoundTrip, AssertVoid, AssertDeclineMapping, AssertWebhook,
	AssertThreeDS, AssertIdempotency, AssertEcho,
}

// MatrixCell is one (gateway, payment method, currency) combination.
type MatrixCell struct {
	Gateway  shared.GatewayID     `json:"gateway"`
	Method   shared.PaymentMethod `json:"paymentMethod"`
	Currency money.Currency       `json:"currency"`
}

// Key is the stable string form, used as the per-cell checkpoint key so a fifteen-minute run
// that crashes at minute twelve resumes at cell n rather than at cell zero.
func (c MatrixCell) Key() string {
	return string(c.Gateway) + "/" + string(c.Method) + "/" + string(c.Currency)
}

// AssertionResult is one assertion's outcome for one cell.
type AssertionResult struct {
	ID     string `json:"id"`
	Cell   string `json:"matrixCell"`
	Passed bool   `json:"passed"`
	// Detail is what an engineer reads first. On failure it says what was expected and what
	// happened; a bare "failed" turns a five-minute fix into an afternoon.
	Detail string `json:"detail,omitempty"`
	// Skipped marks an assertion that does not apply to this cell — a 3DS challenge on a bank
	// transfer, a void on a method with no separate capture. A skipped assertion is recorded
	// rather than silently omitted, because a report with fewer rows than expected is
	// indistinguishable from a report whose runner crashed.
	Skipped  bool          `json:"skipped,omitempty"`
	Duration time.Duration `json:"durationNs,omitempty"`
}

// CellResult is every assertion for one matrix cell.
type CellResult struct {
	Cell       MatrixCell        `json:"cell"`
	Passed     bool              `json:"passed"`
	Assertions []AssertionResult `json:"assertions"`
}

// CertificationReport is the signed, immutable evidence that a merchant's integration works.
//
// It is *evidence*, not a status flag, and the distinction has consequences: it is stored under
// Object Lock with a retention period, it is never deleted (a superseded report is marked
// superseded), and `PRODUCTION_READY` is unreachable without one — the merchant aggregate's
// Approve refuses to transition without a report reference, so the control is structural rather
// than procedural.
type CertificationReport struct {
	RunID       string            `json:"runId"`
	MerchantID  shared.MerchantID `json:"merchantId"`
	TenantID    shared.TenantID   `json:"tenantId"`
	Environment string            `json:"environment"`
	Workflow    string            `json:"workflow"`
	StartedAt   time.Time         `json:"startedAt"`
	CompletedAt time.Time         `json:"completedAt"`

	Cells  []CellResult `json:"cells"`
	Passed bool         `json:"passed"`

	// FailedAssertions lists `assertion@cell` for every failure, so an operator reading a DLQ
	// entry sees what to fix without opening the object store.
	FailedAssertions []string `json:"failedAssertions,omitempty"`

	// ContentHash is SHA-256 over the canonical JSON of everything above.
	//
	// It is what makes the report tamper-evident. The merchant record stores the hash alongside
	// the object key, so a report swapped in the bucket no longer matches the record — and a
	// certification whose evidence can be quietly replaced is not evidence, it is a filename.
	ContentHash string `json:"contentHash"`

	// StorageKey is where the report lives in the object store.
	StorageKey string `json:"storageKey"`
}

// hashable is the report minus the fields that are derived from it. Excluding them is what makes
// the hash reproducible: hashing a struct that contains its own hash cannot be verified.
type hashable struct {
	RunID            string            `json:"runId"`
	MerchantID       shared.MerchantID `json:"merchantId"`
	TenantID         shared.TenantID   `json:"tenantId"`
	Environment      string            `json:"environment"`
	Workflow         string            `json:"workflow"`
	StartedAt        time.Time         `json:"startedAt"`
	CompletedAt      time.Time         `json:"completedAt"`
	Cells            []CellResult      `json:"cells"`
	Passed           bool              `json:"passed"`
	FailedAssertions []string          `json:"failedAssertions,omitempty"`
}

// ComputeHash returns the report's content hash.
//
// Determinism is the whole requirement, so the cells are sorted by key before hashing: Go's map
// iteration order is randomized, and a hash that depends on it would differ between two runs
// over identical results, which makes the tamper check useless in exactly the direction that
// matters — it would cry wolf, and then be ignored.
func (r *CertificationReport) ComputeHash() (string, error) {
	cells := append([]CellResult(nil), r.Cells...)
	sort.Slice(cells, func(i, j int) bool { return cells[i].Cell.Key() < cells[j].Cell.Key() })
	failed := append([]string(nil), r.FailedAssertions...)
	sort.Strings(failed)

	body, err := json.Marshal(hashable{
		RunID: r.RunID, MerchantID: r.MerchantID, TenantID: r.TenantID,
		Environment: r.Environment, Workflow: r.Workflow,
		StartedAt: r.StartedAt.UTC(), CompletedAt: r.CompletedAt.UTC(),
		Cells: cells, Passed: r.Passed, FailedAssertions: failed,
	})
	if err != nil {
		return "", apierror.Wrap(err, apierror.CodeInternalError, "the certification report does not encode")
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// VerifyHash recomputes the hash and compares it, which is what a reader does before trusting a
// report fetched from the object store.
func (r *CertificationReport) VerifyHash() error {
	want, err := r.ComputeHash()
	if err != nil {
		return err
	}
	if want != r.ContentHash {
		return apierror.Newf(apierror.CodeCertificationFailed,
			"certification report %s has been modified since it was signed", r.RunID)
	}
	return nil
}

// TestCardKind selects which sandbox instrument a cell should use.
type TestCardKind string

const (
	// CardApproves is the instrument the gateway's sandbox always approves.
	CardApproves TestCardKind = "APPROVES"
	// CardDeclines is the instrument that yields a mappable decline.
	CardDeclines TestCardKind = "DECLINES"
	// CardRequires3DS is the instrument that triggers a challenge.
	CardRequires3DS TestCardKind = "REQUIRES_3DS"
)

// Sandbox is everything the certification suite needs from the outside world beyond the payment
// SPI itself.
//
// It is declared here, by the consumer, and it is narrow: three capabilities that only a sandbox
// has. Putting these on spi.PaymentGateway would mean every production adapter carries methods
// whose only purpose is testing, and a production code path that can synthesize a webhook or
// complete a 3DS challenge on the payer's behalf is a control that has been handed to an
// attacker.
type Sandbox interface {
	// Gateway returns the sandbox adapter for a gateway.
	Gateway(id shared.GatewayID) (spi.PaymentGateway, error)

	// Credentials resolves the merchant's sandbox credentials for a gateway. The material is
	// fetched at the moment of use and never enters the workflow context.
	Credentials(ctx context.Context, merchantID shared.MerchantID, gateway shared.GatewayID) (spi.Credentials, error)

	// ExternalAccountID returns the gateway's sub-account reference produced at step 5.
	ExternalAccountID(ctx context.Context, merchantID shared.MerchantID, gateway shared.GatewayID) (string, error)

	// TestInstrument returns a tokenized instrument of the requested kind.
	TestInstrument(gateway shared.GatewayID, method shared.PaymentMethod, kind TestCardKind) (payment.PaymentMethodReference, error)

	// AwaitWebhook blocks until the ingress has received and *verified* a webhook correlated
	// with gatewayRef, or the deadline passes. It goes through the real ingress path on purpose:
	// asserting that a webhook was sent proves nothing about whether we can authenticate it.
	AwaitWebhook(ctx context.Context, gateway shared.GatewayID, gatewayRef string, within time.Duration) (*spi.WebhookEvent, error)

	// CompleteChallenge drives a 3DS challenge to completion the way a payer's browser would.
	CompleteChallenge(ctx context.Context, gateway shared.GatewayID, gatewayRef string) (*spi.Result, error)
}

// Certifier runs the certification matrix.
type Certifier struct {
	sandbox Sandbox
	objects ports.ObjectStore
	clock   shared.Clock

	// webhookWait bounds the wait for the asynchronous assertion. It is generous because a
	// sandbox webhook is not on any latency budget, and short enough that a gateway which never
	// sends one fails the run rather than consuming the whole step timeout.
	webhookWait time.Duration

	// retention is how long a report is locked in object storage. Certification evidence is
	// referenced by the merchant record and by audits, so it outlives the merchant relationship.
	retention time.Duration
}

// NewCertifier builds the suite runner.
func NewCertifier(sandbox Sandbox, objects ports.ObjectStore, clock shared.Clock) *Certifier {
	return &Certifier{
		sandbox:     sandbox,
		objects:     objects,
		clock:       clock,
		webhookWait: 60 * time.Second,
		retention:   7 * 365 * 24 * time.Hour,
	}
}

// RunSpec describes one certification run.
type RunSpec struct {
	// RunID is the deterministic idempotency key of the step. Reusing it is what makes a retry
	// resume from the last per-cell checkpoint rather than re-running the whole matrix.
	RunID       string
	MerchantID  shared.MerchantID
	TenantID    shared.TenantID
	Environment shared.Environment
	Matrix      []MatrixCell
	// Store is false for the sandbox subset (step 9) and true for the full matrix (step 10):
	// only the full run produces evidence worth keeping under Object Lock.
	Store bool
}

// Matrix expands the merchant's enabled combinations into cells, in a deterministic order.
func Matrix(gateways []shared.GatewayID, methods []shared.PaymentMethod, currencies []money.Currency) []MatrixCell {
	cells := make([]MatrixCell, 0, len(gateways)*len(methods)*len(currencies))
	for _, g := range gateways {
		for _, m := range methods {
			for _, c := range currencies {
				cells = append(cells, MatrixCell{Gateway: g, Method: m, Currency: c})
			}
		}
	}
	sort.Slice(cells, func(i, j int) bool { return cells[i].Key() < cells[j].Key() })
	return cells
}

// SandboxSubset is the smoke subset step 9 runs: one cell per gateway, on the first method and
// currency. It exists so that a broken integration fails in fifteen minutes rather than in
// thirty, and before the run that produces retained evidence.
func SandboxSubset(cells []MatrixCell) []MatrixCell {
	seen := make(map[shared.GatewayID]bool, 4)
	out := make([]MatrixCell, 0, 4)
	for _, c := range cells {
		if seen[c.Gateway] {
			continue
		}
		seen[c.Gateway] = true
		out = append(out, c)
	}
	return out
}

// Run executes the matrix, checkpointing each cell as it completes.
//
// The checkpointing is what makes a thirty-minute activity survivable: a crash at minute
// twenty-eight resumes at the cell it was on, not at the beginning, and the cells that already
// ran do not re-run their sandbox transactions. It also heartbeats after every cell, because the
// step's timeout is thirty minutes against a sixty-second lease and only the heartbeat keeps the
// lease alive across it.
func (c *Certifier) Run(ctx context.Context, in engine.Input, spec RunSpec) (*CertificationReport, error) {
	started := c.clock.Now().UTC()
	report := &CertificationReport{
		RunID:       spec.RunID,
		MerchantID:  spec.MerchantID,
		TenantID:    spec.TenantID,
		Environment: string(spec.Environment),
		Workflow:    WorkflowName,
		StartedAt:   started,
		Passed:      true,
	}

	for _, cell := range spec.Matrix {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		// Resume from a checkpoint rather than re-running the cell.
		if raw, ok, err := in.Lookup(ctx, cell.Key()); err == nil && ok {
			var cached CellResult
			if json.Unmarshal(raw, &cached) == nil {
				report.Cells = append(report.Cells, cached)
				if !cached.Passed {
					report.Passed = false
					report.FailedAssertions = append(report.FailedAssertions, failedIDs(cached)...)
				}
				continue
			}
		}

		result := c.runCell(ctx, spec, cell)
		report.Cells = append(report.Cells, result)
		if !result.Passed {
			report.Passed = false
			report.FailedAssertions = append(report.FailedAssertions, failedIDs(result)...)
		}

		encoded, err := json.Marshal(result)
		if err != nil {
			return nil, apierror.Wrap(err, apierror.CodeInternalError, "a cell result does not encode")
		}
		if err := in.Checkpoint(ctx, cell.Key(), encoded); err != nil {
			return nil, err
		}
		if err := in.Heartbeat(ctx, nil); err != nil {
			// The lease is gone: another worker owns this instance and is already redoing the
			// run. Abandon immediately rather than spending another twenty minutes on work that
			// will be discarded.
			return nil, err
		}
	}

	report.CompletedAt = c.clock.Now().UTC()
	hash, err := report.ComputeHash()
	if err != nil {
		return nil, err
	}
	report.ContentHash = hash

	if spec.Store {
		if err := c.store(ctx, report); err != nil {
			return nil, err
		}
	}
	return report, nil
}

// store writes the report under Object Lock.
//
// WORM is not decoration: a certification report that can be overwritten is not evidence. The
// key embeds the run ID, so a re-run produces a new object and supersedes rather than replaces —
// which is also why the write is skipped when the object already exists, making the whole step
// idempotent on the run ID.
func (c *Certifier) store(ctx context.Context, report *CertificationReport) error {
	key := ReportKey(report.MerchantID, report.RunID)
	report.StorageKey = key

	exists, err := c.objects.Exists(ctx, key)
	if err != nil {
		return apierror.Wrapf(err, apierror.CodeDependencyFailure,
			"could not check for an existing certification report at %s", key)
	}
	if exists {
		// A retry after a crash. The object is immutable, so the one already there is the one
		// this run would have written.
		return nil
	}

	body, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return apierror.Wrap(err, apierror.CodeInternalError, "the certification report does not encode")
	}
	retainUntil := c.clock.Now().Add(c.retention).UTC()
	if err := c.objects.Put(ctx, key, body, "application/json", ports.ObjectOptions{
		WORM:        true,
		RetainUntil: &retainUntil,
		Metadata: map[string]string{
			"merchantId":  string(report.MerchantID),
			"runId":       report.RunID,
			"contentHash": report.ContentHash,
			"passed":      fmt.Sprintf("%t", report.Passed),
		},
	}); err != nil {
		return apierror.Wrapf(err, apierror.CodeDependencyFailure,
			"could not store the certification report at %s", key)
	}
	return nil
}

// ReportKey is the object-store key for a report. Deterministic in (merchant, run), which is
// what makes storing it idempotent.
func ReportKey(merchantID shared.MerchantID, runID string) string {
	return "certification/" + string(merchantID) + "/" + runID + ".json"
}

// runCell executes all seven assertions for one matrix cell.
//
// It never returns an error: an assertion that could not be evaluated is a *failed* assertion
// with a detail, not an aborted run. Aborting on the first infrastructure hiccup would produce a
// report that says nothing about the other twenty cells, and the engineer fixing the integration
// would then have to re-run the whole matrix to find the second problem.
func (c *Certifier) runCell(ctx context.Context, spec RunSpec, cell MatrixCell) CellResult {
	result := CellResult{Cell: cell, Passed: true}

	gw, err := c.sandbox.Gateway(cell.Gateway)
	if err != nil {
		return unevaluable(cell, "no sandbox adapter for gateway "+string(cell.Gateway)+": "+err.Error())
	}
	creds, err := c.sandbox.Credentials(ctx, spec.MerchantID, cell.Gateway)
	if err != nil {
		return unevaluable(cell, "sandbox credentials could not be resolved: "+err.Error())
	}
	if creds.Environment.IsProduction() {
		// The failure mode this guards is a certification run charging a real card. It is worth
		// a hard stop rather than a warning.
		return unevaluable(cell, "refusing to certify against production credentials")
	}
	account, err := c.sandbox.ExternalAccountID(ctx, spec.MerchantID, cell.Gateway)
	if err != nil {
		return unevaluable(cell, "the gateway sub-account reference is missing: "+err.Error())
	}

	env := cellEnv{
		gw: gw, creds: creds, account: account, cell: cell,
		merchantID: spec.MerchantID, runID: spec.RunID,
	}

	for _, id := range AllAssertions {
		start := c.clock.Now()
		a := c.assert(ctx, env, id)
		a.ID = id
		a.Cell = cell.Key()
		a.Duration = c.clock.Now().Sub(start)
		result.Assertions = append(result.Assertions, a)
		if !a.Passed && !a.Skipped {
			result.Passed = false
		}
	}
	return result
}

type cellEnv struct {
	gw         spi.PaymentGateway
	creds      spi.Credentials
	account    string
	cell       MatrixCell
	merchantID shared.MerchantID
	runID      string
}

// key derives a deterministic idempotency key for one certification call. It is stable in
// (run, cell, assertion, leg), so a re-run of the same run ID hits the vendor's deduplication
// rather than creating new sandbox transactions.
func (e cellEnv) key(assertion, leg string) string {
	return "cert:" + e.runID + ":" + e.cell.Key() + ":" + assertion + ":" + leg
}

func (c *Certifier) assert(ctx context.Context, env cellEnv, id string) AssertionResult {
	switch id {
	case AssertRoundTrip:
		return c.assertRoundTrip(ctx, env)
	case AssertVoid:
		return c.assertVoid(ctx, env)
	case AssertDeclineMapping:
		return c.assertDeclineMapping(ctx, env)
	case AssertWebhook:
		return c.assertWebhook(ctx, env)
	case AssertThreeDS:
		return c.assertThreeDS(ctx, env)
	case AssertIdempotency:
		return c.assertIdempotency(ctx, env)
	case AssertEcho:
		return c.assertEcho(ctx, env)
	default:
		return AssertionResult{Passed: false, Detail: "unknown assertion " + id}
	}
}

// certAmount is the sandbox amount every assertion uses: 12.34 in the cell's currency, in minor
// units. A distinctive non-round value on purpose — a gateway that echoes 1000 for everything
// passes an assertion written against 10.00 and fails this one.
func certAmount(cur money.Currency) (money.Money, error) { return money.New(1234, cur) }

func (c *Certifier) assertRoundTrip(ctx context.Context, env cellEnv) AssertionResult {
	amount, err := certAmount(env.cell.Currency)
	if err != nil {
		return fail("the cell currency is not supported by the platform: " + err.Error())
	}
	instrument, err := c.sandbox.TestInstrument(env.cell.Gateway, env.cell.Method, CardApproves)
	if err != nil {
		return skip("no approving sandbox instrument for this cell: " + err.Error())
	}

	auth, err := env.gw.Authorize(ctx, spi.AuthorizeRequest{
		IdempotencyKey: env.key(AssertRoundTrip, "auth"), Credentials: env.creds,
		ExternalAccountID: env.account, Amount: amount, Method: env.cell.Method,
		MethodRef: instrument, Reference: env.runID,
	})
	if err != nil {
		return fail("authorize failed: " + err.Error())
	}
	if auth.Status != spi.StatusAuthorized && auth.Status != spi.StatusCaptured {
		return fail("authorize returned " + string(auth.Status) + ", want AUTHORIZED")
	}

	if env.cell.Method.SupportsSeparateCapture() {
		captured, err := env.gw.Capture(ctx, spi.CaptureRequest{
			IdempotencyKey: env.key(AssertRoundTrip, "capture"), Credentials: env.creds,
			ExternalAccountID: env.account, GatewayRef: auth.GatewayRef, Amount: amount, Final: true,
		})
		if err != nil {
			return fail("capture failed: " + err.Error())
		}
		if captured.Status != spi.StatusCaptured {
			return fail("capture returned " + string(captured.Status) + ", want CAPTURED")
		}
	}

	if !env.cell.Method.IsRefundable() {
		return AssertionResult{Passed: true, Detail: "authorize and capture succeeded; " +
			string(env.cell.Method) + " is not refundable, so the refund leg does not apply"}
	}
	ref, err := env.gw.Refund(ctx, spi.RefundRequest{
		IdempotencyKey: env.key(AssertRoundTrip, "refund"), Credentials: env.creds,
		ExternalAccountID: env.account, GatewayRef: auth.GatewayRef, Amount: amount,
		Reason: payment.RefundReasonOther,
	})
	if err != nil {
		return fail("refund failed: " + err.Error())
	}
	// A refund is asynchronous at every real gateway: accepted is the correct success, and an
	// adapter reporting REFUNDED synchronously is optimistic rather than right.
	if ref.Status != spi.StatusRefundAccepted && ref.Status != spi.StatusRefunded {
		return fail("refund returned " + string(ref.Status) + ", want REFUND_ACCEPTED")
	}
	return pass("authorize → capture → refund completed")
}

func (c *Certifier) assertVoid(ctx context.Context, env cellEnv) AssertionResult {
	if !env.cell.Method.SupportsSeparateCapture() {
		return skip(string(env.cell.Method) + " has no separate capture, so there is no authorization to void")
	}
	amount, err := certAmount(env.cell.Currency)
	if err != nil {
		return fail(err.Error())
	}
	instrument, err := c.sandbox.TestInstrument(env.cell.Gateway, env.cell.Method, CardApproves)
	if err != nil {
		return skip("no approving sandbox instrument: " + err.Error())
	}
	auth, err := env.gw.Authorize(ctx, spi.AuthorizeRequest{
		IdempotencyKey: env.key(AssertVoid, "auth"), Credentials: env.creds,
		ExternalAccountID: env.account, Amount: amount, Method: env.cell.Method, MethodRef: instrument,
	})
	if err != nil {
		return fail("authorize failed: " + err.Error())
	}
	v, err := env.gw.Void(ctx, spi.VoidRequest{
		IdempotencyKey: env.key(AssertVoid, "void"), Credentials: env.creds,
		ExternalAccountID: env.account, GatewayRef: auth.GatewayRef,
	})
	if err != nil {
		return fail("void failed: " + err.Error())
	}
	if v.Status != spi.StatusVoided {
		return fail("void returned " + string(v.Status) + ", want VOIDED")
	}
	return pass("authorize → void completed")
}

func (c *Certifier) assertDeclineMapping(ctx context.Context, env cellEnv) AssertionResult {
	amount, err := certAmount(env.cell.Currency)
	if err != nil {
		return fail(err.Error())
	}
	instrument, err := c.sandbox.TestInstrument(env.cell.Gateway, env.cell.Method, CardDeclines)
	if err != nil {
		return skip("no declining sandbox instrument for this cell: " + err.Error())
	}
	res, err := env.gw.Authorize(ctx, spi.AuthorizeRequest{
		IdempotencyKey: env.key(AssertDeclineMapping, "auth"), Credentials: env.creds,
		ExternalAccountID: env.account, Amount: amount, Method: env.cell.Method, MethodRef: instrument,
	})
	if err != nil {
		// A decline is a successful *call*. An adapter that returns an error for a decline has
		// conflated "the payer's card was refused" with "we could not reach the gateway", which
		// makes every decline look like an outage to the circuit breaker.
		return fail("the adapter returned an error for a declined authorization instead of a DECLINED result: " + err.Error())
	}
	if res.Status != spi.StatusDeclined {
		return fail("the declining instrument returned " + string(res.Status) + ", want DECLINED")
	}
	if res.DeclineReason == "" || res.DeclineReason == payment.DeclineUnknown {
		return fail("the decline mapped to " + string(res.DeclineReason) +
			"; an unmapped reason forbids failover, so every soft decline becomes a lost sale")
	}
	return pass("declined with normalized reason " + string(res.DeclineReason))
}

func (c *Certifier) assertWebhook(ctx context.Context, env cellEnv) AssertionResult {
	amount, err := certAmount(env.cell.Currency)
	if err != nil {
		return fail(err.Error())
	}
	instrument, err := c.sandbox.TestInstrument(env.cell.Gateway, env.cell.Method, CardApproves)
	if err != nil {
		return skip("no approving sandbox instrument: " + err.Error())
	}
	auth, err := env.gw.Authorize(ctx, spi.AuthorizeRequest{
		IdempotencyKey: env.key(AssertWebhook, "auth"), Credentials: env.creds,
		ExternalAccountID: env.account, Amount: amount, Method: env.cell.Method,
		MethodRef: instrument, Capture: true,
	})
	if err != nil {
		return fail("authorize failed: " + err.Error())
	}
	ev, err := c.sandbox.AwaitWebhook(ctx, env.cell.Gateway, auth.GatewayRef, c.webhookWait)
	if err != nil {
		return fail("no verified webhook arrived within " + c.webhookWait.String() + ": " + err.Error())
	}
	if ev == nil || ev.GatewayEventID == "" {
		return fail("the webhook arrived without a deduplication identifier, so a redelivery would be applied twice")
	}
	if ev.Kind == spi.KindIgnored || ev.Kind == "" {
		return fail("the webhook was received but mapped to no normalized kind, so it moves no payment state")
	}
	return pass("webhook " + ev.GatewayEventID + " received, signature-verified and mapped to " + string(ev.Kind))
}

func (c *Certifier) assertThreeDS(ctx context.Context, env cellEnv) AssertionResult {
	if !env.cell.Method.RequiresSCAConsideration() {
		return skip(string(env.cell.Method) + " is out of scope for strong customer authentication")
	}
	amount, err := certAmount(env.cell.Currency)
	if err != nil {
		return fail(err.Error())
	}
	instrument, err := c.sandbox.TestInstrument(env.cell.Gateway, env.cell.Method, CardRequires3DS)
	if err != nil {
		return skip("no 3DS sandbox instrument for this cell: " + err.Error())
	}
	res, err := env.gw.Authorize(ctx, spi.AuthorizeRequest{
		IdempotencyKey: env.key(AssertThreeDS, "auth"), Credentials: env.creds,
		ExternalAccountID: env.account, Amount: amount, Method: env.cell.Method, MethodRef: instrument,
		ThreeDS: spi.ThreeDSRequest{Requested: true}, ReturnURL: "https://certification.invalid/return",
	})
	if err != nil {
		return fail("authorize failed: " + err.Error())
	}
	if res.Status != spi.StatusRequiresAction {
		return fail("the 3DS instrument returned " + string(res.Status) + ", want REQUIRES_ACTION")
	}
	if res.NextAction == nil || res.NextAction.Type != payment.ActionThreeDSChall {
		return fail("REQUIRES_ACTION carried no 3DS challenge, so the payer has nowhere to be sent")
	}
	done, err := c.sandbox.CompleteChallenge(ctx, env.cell.Gateway, res.GatewayRef)
	if err != nil {
		return fail("the challenge could not be completed: " + err.Error())
	}
	if done.Status != spi.StatusAuthorized && done.Status != spi.StatusCaptured {
		return fail("after the challenge the payment was " + string(done.Status) + ", want AUTHORIZED")
	}
	return pass("3DS reached REQUIRES_ACTION and completed to " + string(done.Status))
}

func (c *Certifier) assertIdempotency(ctx context.Context, env cellEnv) AssertionResult {
	amount, err := certAmount(env.cell.Currency)
	if err != nil {
		return fail(err.Error())
	}
	instrument, err := c.sandbox.TestInstrument(env.cell.Gateway, env.cell.Method, CardApproves)
	if err != nil {
		return skip("no approving sandbox instrument: " + err.Error())
	}
	req := spi.AuthorizeRequest{
		IdempotencyKey: env.key(AssertIdempotency, "auth"), Credentials: env.creds,
		ExternalAccountID: env.account, Amount: amount, Method: env.cell.Method, MethodRef: instrument,
	}
	first, err := env.gw.Authorize(ctx, req)
	if err != nil {
		return fail("the first authorization failed: " + err.Error())
	}
	second, err := env.gw.Authorize(ctx, req)
	if err != nil {
		return fail("the repeated authorization failed: " + err.Error())
	}
	if first.GatewayRef != second.GatewayRef {
		// This is the assertion that underwrites every crash-safety argument in the automation
		// plane: the deterministic idempotency key is only useful if the vendor honours it.
		return fail("the same idempotency key produced two transactions (" +
			first.GatewayRef + " and " + second.GatewayRef + "): a retry after a crash would double-charge")
	}
	if first.Status != second.Status {
		return fail("the same idempotency key produced statuses " +
			string(first.Status) + " and " + string(second.Status))
	}
	return pass("the duplicate submission returned the original transaction " + first.GatewayRef)
}

func (c *Certifier) assertEcho(ctx context.Context, env cellEnv) AssertionResult {
	amount, err := certAmount(env.cell.Currency)
	if err != nil {
		return fail(err.Error())
	}
	instrument, err := c.sandbox.TestInstrument(env.cell.Gateway, env.cell.Method, CardApproves)
	if err != nil {
		return skip("no approving sandbox instrument: " + err.Error())
	}
	res, err := env.gw.Authorize(ctx, spi.AuthorizeRequest{
		IdempotencyKey: env.key(AssertEcho, "auth"), Credentials: env.creds,
		ExternalAccountID: env.account, Amount: amount, Method: env.cell.Method, MethodRef: instrument,
	})
	if err != nil {
		return fail("authorize failed: " + err.Error())
	}
	echoed := res.AuthorizedAmount
	if echoed == nil {
		echoed = res.CapturedAmount
	}
	if echoed == nil {
		return fail("the gateway echoed no amount, so L6 response validation has nothing to check")
	}
	if echoed.Currency() != amount.Currency() {
		return fail("the gateway echoed currency " + string(echoed.Currency()) +
			" for a request in " + string(amount.Currency()))
	}
	if echoed.Amount() != amount.Amount() {
		return fail(fmt.Sprintf("the gateway echoed %d minor units for a request of %d",
			echoed.Amount(), amount.Amount()))
	}
	return pass("the gateway echoed " + echoed.String())
}

func pass(detail string) AssertionResult { return AssertionResult{Passed: true, Detail: detail} }
func fail(detail string) AssertionResult { return AssertionResult{Passed: false, Detail: detail} }
func skip(detail string) AssertionResult {
	return AssertionResult{Passed: true, Skipped: true, Detail: detail}
}

// unevaluable marks every assertion of a cell as failed because the cell itself could not be set
// up. Recording all seven rather than one keeps the report's shape constant, so a diff between
// two runs lines up.
func unevaluable(cell MatrixCell, why string) CellResult {
	res := CellResult{Cell: cell, Passed: false}
	for _, id := range AllAssertions {
		res.Assertions = append(res.Assertions, AssertionResult{
			ID: id, Cell: cell.Key(), Passed: false, Detail: why,
		})
	}
	return res
}

func failedIDs(c CellResult) []string {
	var out []string
	for _, a := range c.Assertions {
		if !a.Passed && !a.Skipped {
			out = append(out, a.ID+"@"+a.Cell)
		}
	}
	return out
}

// FailureSummary renders the failing assertions for a DLQ entry and a merchant-facing message.
func (r *CertificationReport) FailureSummary() string {
	if r.Passed {
		return ""
	}
	return strings.Join(r.FailedAssertions, "; ")
}
