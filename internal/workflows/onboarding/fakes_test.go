package onboarding_test

import (
	"context"
	"sync"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/config"
	"github.com/udaykishore-resu/payments-platform/internal/domain/merchant"
	"github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/workflows/onboarding"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// The doubles below are deliberately behavioural rather than mock-ish: they record what was
// asked of them and answer the way the real thing would. A test that asserts "Provision was
// called with these arguments" proves that the code calls a function; a double that actually
// deduplicates on the idempotency key proves that a retry does not create a second sub-account,
// which is the property that matters.

// --- clock ---------------------------------------------------------------------------------------

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *testClock {
	return &testClock{t: time.Date(2026, time.April, 1, 10, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func (c *testClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d > 0 {
		c.Advance(d)
	}
	return nil
}

// --- merchant repository -------------------------------------------------------------------------

type merchantRepo struct {
	mu sync.Mutex
	m  map[shared.MerchantID]*merchant.Merchant
	// drained counts the events the repository took off the aggregate, standing in for the
	// outbox write that happens in the same transaction as the state change.
	drained []merchant.Event
}

var _ ports.MerchantRepository = (*merchantRepo)(nil)

func newMerchantRepo() *merchantRepo {
	return &merchantRepo{m: make(map[shared.MerchantID]*merchant.Merchant, 2)}
}

func (r *merchantRepo) Create(_ context.Context, m *merchant.Merchant) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[m.ID()] = m
	r.drained = append(r.drained, m.DrainEvents()...)
	return nil
}

func (r *merchantRepo) Get(_ context.Context, id shared.MerchantID) (*merchant.Merchant, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.m[id]
	if !ok {
		return nil, apierror.Newf(apierror.CodeMerchantNotFound, "merchant %s not found", id)
	}
	return m, nil
}

func (r *merchantRepo) GetForUpdate(ctx context.Context, id shared.MerchantID) (*merchant.Merchant, error) {
	return r.Get(ctx, id)
}

func (r *merchantRepo) GetByExternalRef(context.Context, string) (*merchant.Merchant, error) {
	return nil, apierror.New(apierror.CodeMerchantNotFound, "not implemented in this double")
}

// Save drains the aggregate's pending events, which is what the real repository does inside the
// state-change transaction: the domain raises events, the repository writes them to the outbox,
// and neither can happen without the other.
func (r *merchantRepo) Save(_ context.Context, m *merchant.Merchant) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[m.ID()] = m
	r.drained = append(r.drained, m.DrainEvents()...)
	return nil
}

func (r *merchantRepo) List(context.Context, ports.MerchantFilter, ports.Page) ([]*merchant.Merchant, string, error) {
	return nil, "", nil
}

func (r *merchantRepo) FindKYCExpiring(context.Context, time.Duration, int) ([]*merchant.Merchant, error) {
	return nil, nil
}

// Events returns every event the repository drained, which is how a test asserts that a step
// emitted the event it owns.
func (r *merchantRepo) Events() []merchant.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]merchant.Event(nil), r.drained...)
}

func (r *merchantRepo) HasEvent(t merchant.EventType) bool {
	for _, e := range r.Events() {
		if e.Type == t {
			return true
		}
	}
	return false
}

// --- KYC provider --------------------------------------------------------------------------------

type kycProvider struct {
	mu sync.Mutex
	// byKey is the vendor's own idempotency table. A repeated submission with the same client
	// reference returns the existing case rather than opening a second one — which is the
	// behaviour the whole crash-safety argument depends on.
	byKey     map[string]string
	cases     map[string]ports.KYCDecision
	submits   int
	cancelled []string
	failNext  error
}

var _ ports.KYCProvider = (*kycProvider)(nil)

func newKYCProvider() *kycProvider {
	return &kycProvider{byKey: map[string]string{}, cases: map[string]ports.KYCDecision{}}
}

func (k *kycProvider) Submit(_ context.Context, req ports.KYCSubmission) (ports.KYCSubmissionResult, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.failNext != nil {
		err := k.failNext
		k.failNext = nil
		return ports.KYCSubmissionResult{}, err
	}
	if ref, ok := k.byKey[req.IdempotencyKey]; ok {
		return ports.KYCSubmissionResult{ProviderRef: ref, Status: merchant.KYCInProgress}, nil
	}
	k.submits++
	ref := "kyc_case_" + req.IdempotencyKey[:8]
	k.byKey[req.IdempotencyKey] = ref
	k.cases[ref] = ports.KYCDecision{ProviderRef: ref, Status: merchant.KYCInProgress}
	return ports.KYCSubmissionResult{ProviderRef: ref, Status: merchant.KYCInProgress}, nil
}

func (k *kycProvider) Get(_ context.Context, ref string) (ports.KYCDecision, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	d, ok := k.cases[ref]
	if !ok {
		return ports.KYCDecision{}, apierror.Newf(apierror.CodeOnboardingCaseNotFound, "no case %s", ref)
	}
	return d, nil
}

func (k *kycProvider) Cancel(_ context.Context, ref string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.cases[ref]; !ok {
		return apierror.Newf(apierror.CodeOnboardingCaseNotFound, "no case %s", ref)
	}
	k.cancelled = append(k.cancelled, ref)
	return nil
}

func (k *kycProvider) Submits() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.submits
}

func (k *kycProvider) Cancelled() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.cancelled...)
}

// --- bank validator ------------------------------------------------------------------------------

type bankValidator struct {
	mu      sync.Mutex
	result  ports.BankValidationResult
	calls   int
	byKey   map[string]ports.BankValidationResult
	failErr error
}

var _ ports.BankValidator = (*bankValidator)(nil)

func newBankValidator() *bankValidator {
	return &bankValidator{
		result: ports.BankValidationResult{Verified: true, Reference: "pd_1"},
		byKey:  map[string]ports.BankValidationResult{},
	}
}

func (b *bankValidator) Validate(_ context.Context, req ports.BankValidationRequest) (ports.BankValidationResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failErr != nil {
		return ports.BankValidationResult{}, b.failErr
	}
	if r, ok := b.byKey[req.IdempotencyKey]; ok {
		return r, nil
	}
	b.calls++
	b.byKey[req.IdempotencyKey] = b.result
	return b.result, nil
}

func (b *bankValidator) Calls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

// --- secrets -------------------------------------------------------------------------------------

type secretsStore struct {
	mu      sync.Mutex
	values  map[string]map[string]string
	deleted []string
	writes  int
}

var _ ports.SecretsProvider = (*secretsStore)(nil)

func newSecretsStore() *secretsStore {
	return &secretsStore{values: map[string]map[string]string{}}
}

type secretMaterial struct {
	values  map[string]string
	version string
}

func (s secretMaterial) Value(field string) (string, bool) { v, ok := s.values[field]; return v, ok }
func (s secretMaterial) Fields() []string {
	out := make([]string, 0, len(s.values))
	for k := range s.values {
		out = append(out, k)
	}
	return out
}
func (s secretMaterial) Version() string { return s.version }

func (s *secretsStore) Get(_ context.Context, ref string) (ports.SecretMaterial, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.values[ref]
	if !ok {
		return nil, apierror.Newf(apierror.CodeInternalError, "no secret %s", ref)
	}
	return secretMaterial{values: v, version: "1"}, nil
}

// Put is idempotent on the reference, standing in for Secrets Manager's ClientRequestToken: a
// retry returns the existing version rather than creating a new one.
func (s *secretsStore) Put(_ context.Context, ref string, material map[string]string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.values[ref]; !ok {
		s.writes++
	}
	s.values[ref] = material
	return ref + "#1", nil
}

func (s *secretsStore) Rotate(_ context.Context, ref string, material map[string]string, _ time.Duration) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[ref] = material
	return ref + "#2", nil
}

func (s *secretsStore) Delete(_ context.Context, ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	base := ref
	if i := len(ref) - 2; i > 0 && ref[i] == '#' {
		base = ref[:i]
	}
	if _, ok := s.values[base]; !ok {
		return apierror.Newf(apierror.CodeInternalError, "no secret %s", ref).
			WithDetail(apierror.Detail{Code: "NOT_FOUND"})
	}
	delete(s.values, base)
	s.deleted = append(s.deleted, ref)
	return nil
}

func (s *secretsStore) Deleted() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.deleted...)
}

func (s *secretsStore) Writes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes
}

// --- object store --------------------------------------------------------------------------------

type objectStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	opts    map[string]ports.ObjectOptions
	puts    int
}

var _ ports.ObjectStore = (*objectStore)(nil)

func newObjectStore() *objectStore {
	return &objectStore{objects: map[string][]byte{}, opts: map[string]ports.ObjectOptions{}}
}

func (o *objectStore) Put(_ context.Context, key string, body []byte, _ string, opts ports.ObjectOptions) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, exists := o.objects[key]; exists && o.opts[key].WORM {
		// WORM is not decoration: a certification report that can be overwritten is not evidence.
		return apierror.Newf(apierror.CodeForbidden, "object %s is under retention and cannot be overwritten", key)
	}
	o.puts++
	o.objects[key] = append([]byte(nil), body...)
	o.opts[key] = opts
	return nil
}

func (o *objectStore) Get(_ context.Context, key string) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	b, ok := o.objects[key]
	if !ok {
		return nil, apierror.Newf(apierror.CodeInternalError, "no object %s", key)
	}
	return append([]byte(nil), b...), nil
}

func (o *objectStore) PresignGet(context.Context, string, time.Duration) (string, error) {
	return "https://objects.invalid/signed", nil
}

func (o *objectStore) Exists(_ context.Context, key string) (bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, ok := o.objects[key]
	return ok, nil
}

func (o *objectStore) Options(key string) ports.ObjectOptions {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.opts[key]
}

func (o *objectStore) Puts() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.puts
}

// --- configuration store -------------------------------------------------------------------------

type configStore struct {
	mu       sync.Mutex
	versions map[shared.MerchantID][]*config.MerchantConfig
}

var _ onboarding.ConfigStore = (*configStore)(nil)

func newConfigStore() *configStore {
	return &configStore{versions: map[shared.MerchantID][]*config.MerchantConfig{}}
}

func (c *configStore) GetActive(_ context.Context, m shared.MerchantID) (*config.MerchantConfig, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	list := c.versions[m]
	if len(list) == 0 {
		return nil, apierror.Newf(apierror.CodeConfigurationInvalid, "no configuration for %s", m).
			WithDetail(apierror.Detail{Code: "NOT_FOUND"})
	}
	return list[len(list)-1], nil
}

func (c *configStore) GetVersion(_ context.Context, m shared.MerchantID, version int) (*config.MerchantConfig, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, v := range c.versions[m] {
		if v.Version == version {
			return v, nil
		}
	}
	return nil, apierror.Newf(apierror.CodeConfigurationInvalid, "no version %d", version)
}

// Publish enforces the optimistic-concurrency contract: a mismatch is a conflict, not a silent
// overwrite of somebody else's edit.
func (c *configStore) Publish(_ context.Context, cfg *config.MerchantConfig, expectedVersion int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	list := c.versions[cfg.MerchantID]
	current := 0
	if len(list) > 0 {
		current = list[len(list)-1].Version
	}
	if current != expectedVersion {
		return apierror.Newf(apierror.CodeConfigurationVersionConflict,
			"expected version %d but the merchant is at %d", expectedVersion, current)
	}
	c.versions[cfg.MerchantID] = append(list, cfg)
	return nil
}

func (c *configStore) Versions(m shared.MerchantID) []*config.MerchantConfig {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*config.MerchantConfig(nil), c.versions[m]...)
}

// --- gateway provisioning -------------------------------------------------------------------------

type provisioner struct {
	mu sync.Mutex
	id shared.GatewayID

	// accounts is the gateway's idempotency table, keyed by the client reference we send.
	accounts       map[string]string
	registrations  map[string]string
	provisions     int
	deprovisioned  []string
	unregistered   []string
	provisionErr   error
	registerErr    error
	deprovisionErr error
}

var _ spi.GatewayProvisioner = (*provisioner)(nil)

func newProvisioner(id shared.GatewayID) *provisioner {
	return &provisioner{id: id, accounts: map[string]string{}, registrations: map[string]string{}}
}

func (p *provisioner) ID() shared.GatewayID { return p.id }

func (p *provisioner) Provision(_ context.Context, req spi.ProvisionRequest) (*spi.ProvisionResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.provisionErr != nil {
		return nil, p.provisionErr
	}
	if acct, ok := p.accounts[req.IdempotencyKey]; ok {
		return &spi.ProvisionResult{ExternalAccountID: acct, Status: "ACTIVE"}, nil
	}
	p.provisions++
	acct := string(p.id) + "_acct_" + req.IdempotencyKey[len(req.IdempotencyKey)-6:]
	p.accounts[req.IdempotencyKey] = acct
	return &spi.ProvisionResult{ExternalAccountID: acct, Status: "ACTIVE"}, nil
}

func (p *provisioner) Deprovision(_ context.Context, externalAccountID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.deprovisionErr != nil {
		return p.deprovisionErr
	}
	p.deprovisioned = append(p.deprovisioned, externalAccountID)
	return nil
}

func (p *provisioner) RegisterWebhook(_ context.Context, req spi.WebhookRegistrationRequest) (*spi.WebhookRegistration, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.registerErr != nil {
		return nil, p.registerErr
	}
	if id, ok := p.registrations[req.IdempotencyKey]; ok {
		return &spi.WebhookRegistration{RegistrationID: id, URL: req.URL, SigningSecret: "whsec_test"}, nil
	}
	id := string(p.id) + "_whr_1"
	p.registrations[req.IdempotencyKey] = id
	return &spi.WebhookRegistration{RegistrationID: id, URL: req.URL, SigningSecret: "whsec_test"}, nil
}

func (p *provisioner) UnregisterWebhook(_ context.Context, _, registrationID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.unregistered = append(p.unregistered, registrationID)
	return nil
}

func (p *provisioner) VerifyCredentials(context.Context, spi.Credentials) error { return nil }

func (p *provisioner) Provisions() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.provisions
}

func (p *provisioner) Deprovisioned() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.deprovisioned...)
}

func (p *provisioner) Unregistered() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.unregistered...)
}

type provisionerSet struct {
	mu sync.Mutex
	m  map[shared.GatewayID]*provisioner
}

var _ onboarding.ProvisionerSet = (*provisionerSet)(nil)

func newProvisionerSet(ids ...shared.GatewayID) *provisionerSet {
	s := &provisionerSet{m: map[shared.GatewayID]*provisioner{}}
	for _, id := range ids {
		s.m[id] = newProvisioner(id)
	}
	return s
}

func (s *provisionerSet) Provisioner(id shared.GatewayID) (spi.GatewayProvisioner, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.m[id]
	if !ok {
		return nil, apierror.Newf(apierror.CodeGatewayNotConfigured, "no provisioner for %s", id)
	}
	return p, nil
}

func (s *provisionerSet) get(id shared.GatewayID) *provisioner {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[id]
}

// --- credential source ----------------------------------------------------------------------------

type credentialSource struct{}

var _ onboarding.CredentialSource = (*credentialSource)(nil)

func (c *credentialSource) IssueCredentials(_ context.Context, m shared.MerchantID, g shared.GatewayID, acct string) (map[string]string, error) {
	return map[string]string{
		"apiKey":    "sk_test_" + string(g) + "_supersecret",
		"accountId": acct,
	}, nil
}

// --- sandbox --------------------------------------------------------------------------------------

// sandboxGateway answers every certification assertion the way a healthy sandbox would, and —
// importantly — deduplicates on the idempotency key, so the idempotency assertion is a real
// assertion rather than a tautology.
type sandboxGateway struct {
	mu sync.Mutex
	id shared.GatewayID
	// byKey is the vendor-side idempotency table.
	byKey map[string]*spi.Result
	// noDedupe makes the gateway violate the idempotency contract, so a test can prove the
	// assertion catches it.
	noDedupe bool
	seq      int
}

var _ spi.PaymentGateway = (*sandboxGateway)(nil)

func newSandboxGateway(id shared.GatewayID) *sandboxGateway {
	return &sandboxGateway{id: id, byKey: map[string]*spi.Result{}}
}

func (g *sandboxGateway) ID() shared.GatewayID { return g.id }

func (g *sandboxGateway) Authorize(_ context.Context, req spi.AuthorizeRequest) (*spi.Result, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.noDedupe {
		if r, ok := g.byKey[req.IdempotencyKey]; ok {
			return r, nil
		}
	}
	g.seq++
	amount := req.Amount
	res := &spi.Result{
		Status:           spi.StatusAuthorized,
		GatewayRef:       string(g.id) + "_txn_" + itoa(g.seq),
		AuthorizedAmount: &amount,
	}
	switch req.MethodRef.Token {
	case "tok_declines":
		res.Status = spi.StatusDeclined
		res.DeclineReason = payment.DeclineInsufficientFunds
		res.AuthorizedAmount = nil
	case "tok_3ds":
		res.Status = spi.StatusRequiresAction
		res.NextAction = &spi.NextAction{Type: payment.ActionThreeDSChall, RedirectURL: "https://acs.invalid"}
		res.AuthorizedAmount = nil
	}
	if req.Capture && res.Status == spi.StatusAuthorized {
		res.Status = spi.StatusCaptured
		res.CapturedAmount = &amount
	}
	g.byKey[req.IdempotencyKey] = res
	return res, nil
}

func (g *sandboxGateway) Capture(_ context.Context, req spi.CaptureRequest) (*spi.Result, error) {
	amount := req.Amount
	return &spi.Result{Status: spi.StatusCaptured, GatewayRef: req.GatewayRef, CapturedAmount: &amount}, nil
}

func (g *sandboxGateway) Refund(_ context.Context, req spi.RefundRequest) (*spi.Result, error) {
	return &spi.Result{Status: spi.StatusRefundAccepted, GatewayRef: req.GatewayRef}, nil
}

func (g *sandboxGateway) Void(_ context.Context, req spi.VoidRequest) (*spi.Result, error) {
	return &spi.Result{Status: spi.StatusVoided, GatewayRef: req.GatewayRef}, nil
}

func (g *sandboxGateway) Lookup(_ context.Context, req spi.LookupRequest) (*spi.Result, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if r, ok := g.byKey[req.IdempotencyKey]; ok {
		return r, nil
	}
	return &spi.Result{Status: spi.StatusNotFound}, nil
}

type sandbox struct {
	mu       sync.Mutex
	gateways map[shared.GatewayID]*sandboxGateway
	accounts map[shared.GatewayID]string
	// noWebhook makes the sandbox never deliver a webhook, so a test can prove the assertion
	// fails rather than passing vacuously.
	noWebhook bool
}

var _ onboarding.Sandbox = (*sandbox)(nil)

func newSandbox(ids ...shared.GatewayID) *sandbox {
	s := &sandbox{
		gateways: map[shared.GatewayID]*sandboxGateway{},
		accounts: map[shared.GatewayID]string{},
	}
	for _, id := range ids {
		s.gateways[id] = newSandboxGateway(id)
		s.accounts[id] = string(id) + "_acct"
	}
	return s
}

func (s *sandbox) Gateway(id shared.GatewayID) (spi.PaymentGateway, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.gateways[id]
	if !ok {
		return nil, apierror.Newf(apierror.CodeGatewayNotConfigured, "no sandbox gateway %s", id)
	}
	return g, nil
}

func (s *sandbox) Credentials(context.Context, shared.MerchantID, shared.GatewayID) (spi.Credentials, error) {
	return spi.Credentials{Values: map[string]string{"apiKey": "sk_test"}, Environment: shared.EnvironmentSandbox}, nil
}

func (s *sandbox) ExternalAccountID(_ context.Context, _ shared.MerchantID, g shared.GatewayID) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	acct, ok := s.accounts[g]
	if !ok {
		return "", apierror.Newf(apierror.CodeGatewayNotConfigured, "no sub-account for %s", g)
	}
	return acct, nil
}

func (s *sandbox) TestInstrument(_ shared.GatewayID, _ shared.PaymentMethod, kind onboarding.TestCardKind) (payment.PaymentMethodReference, error) {
	switch kind {
	case onboarding.CardDeclines:
		return payment.PaymentMethodReference{Token: "tok_declines", Brand: "visa", Last4: "0002"}, nil
	case onboarding.CardRequires3DS:
		return payment.PaymentMethodReference{Token: "tok_3ds", Brand: "visa", Last4: "3220"}, nil
	default:
		return payment.PaymentMethodReference{Token: "tok_approves", Brand: "visa", Last4: "4242"}, nil
	}
}

func (s *sandbox) AwaitWebhook(_ context.Context, g shared.GatewayID, gatewayRef string, _ time.Duration) (*spi.WebhookEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.noWebhook {
		return nil, apierror.New(apierror.CodeInternalError, "no webhook was delivered")
	}
	return &spi.WebhookEvent{
		GatewayEventID: string(g) + "_evt_" + gatewayRef,
		EventType:      "payment.captured",
		Kind:           spi.KindCaptureSucceeded,
		GatewayRef:     gatewayRef,
	}, nil
}

func (s *sandbox) CompleteChallenge(_ context.Context, _ shared.GatewayID, gatewayRef string) (*spi.Result, error) {
	return &spi.Result{Status: spi.StatusAuthorized, GatewayRef: gatewayRef}, nil
}

// --- merchant fixture ------------------------------------------------------------------------------

func newTestMerchant(t interface{ Fatalf(string, ...any) }, clock shared.Clock, tenant shared.TenantID) *merchant.Merchant {
	volume, _ := money.New(500000, "USD")
	ticket, _ := money.New(5000, "USD")
	m, err := merchant.New(merchant.NewParams{
		TenantID:    tenant,
		LegalName:   "Northwind Trading Ltd",
		DisplayName: "Northwind",
		Environment: shared.EnvironmentSandbox,
		Profile: merchant.BusinessProfile{
			LegalEntityType:       "LTD",
			RegistrationNumber:    "12345678",
			TaxID:                 "GB123456789",
			Country:               "GB",
			AddressLine1:          "1 Example Street",
			City:                  "London",
			PostalCode:            "EC1A 1AA",
			WebsiteURL:            "https://northwind.example",
			SupportEmail:          "support@northwind.example",
			MCC:                   "5812",
			ExpectedMonthlyVolume: volume,
			ExpectedAverageTicket: ticket,
		},
	}, clock)
	if err != nil {
		t.Fatalf("building the test merchant: %v", err)
	}
	if err := m.AddPrincipal(merchant.Principal{
		ID: "prn_1", Role: merchant.RoleBeneficialOwner, FirstName: "Ada", LastName: "Lovelace",
		OwnershipPct: 100, Country: "GB",
	}, clock); err != nil {
		t.Fatalf("adding a principal: %v", err)
	}
	if err := m.AddBankAccount(merchant.BankAccount{
		ID: "ba_1", Country: "GB", Currency: "USD", HolderName: "Northwind Trading Ltd",
		AccountLast4: "4321", SecretRef: "secret://sandbox/bank/ba_1",
	}, clock); err != nil {
		t.Fatalf("adding a bank account: %v", err)
	}
	return m
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
