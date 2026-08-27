package onboarding

import (
	"context"

	"github.com/udaykishore-resu/payments-platform/internal/domain/merchant"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
)

// MerchantValidator is the validation plane's level-2 entry point, declared here by its consumer.
//
// It returns rule *outcomes* rather than an error, and that shape is the point: step 1's job is
// to tell a merchant everything that is wrong with their submission at once. A validator that
// stops at the first failure produces the interaction where a merchant fixes one field, resubmits,
// waits, and is told about the next one — five times.
//
// An error return is reserved for "the rules could not be evaluated": a database read failed, a
// tenant policy could not be loaded. Conflating that with a rejection would reject merchants
// during a database blip, which is a far worse failure than a slow one.
type MerchantValidator interface {
	Validate(ctx context.Context, m *merchant.Merchant, in Input) ([]RuleOutcome, error)
}

// MerchantValidatorFunc adapts a function to MerchantValidator.
type MerchantValidatorFunc func(ctx context.Context, m *merchant.Merchant, in Input) ([]RuleOutcome, error)

// Validate implements MerchantValidator.
func (f MerchantValidatorFunc) Validate(ctx context.Context, m *merchant.Merchant, in Input) ([]RuleOutcome, error) {
	return f(ctx, m, in)
}

// DefaultMerchantValidator is the pure, dependency-free subset of level 2.
//
// These are the rules that hold for every merchant in every tenant, and every one of them is
// statically decidable from the aggregate and the request. Tenant-specific policy — which MCCs
// this tenant may onboard, which corridors it is licensed for — needs the tenant's configuration
// and belongs to a validator that has it; this type is the floor beneath that, and it is
// deliberately pure so that step 1 keeps its "no external side effect exists yet" property.
type DefaultMerchantValidator struct{}

var _ MerchantValidator = DefaultMerchantValidator{}

// Validate runs every rule and returns every outcome, passing and failing alike. The passing ones
// are kept because the checkpointed output is the evidence that a control ran — a report listing
// only failures is indistinguishable from a validator that was skipped.
func (DefaultMerchantValidator) Validate(_ context.Context, m *merchant.Merchant, in Input) ([]RuleOutcome, error) {
	out := make([]RuleOutcome, 0, 10)
	add := func(ruleID, field string, ok bool, msg string) {
		r := RuleOutcome{RuleID: ruleID, Field: field, Passed: ok}
		if !ok {
			r.Message = msg
		}
		out = append(out, r)
	}
	profile := m.Profile()

	add("L2.LEGAL_NAME_PRESENT", "legalName", m.LegalName() != "",
		"the registered legal name of the business is required")

	add("L2.COUNTRY_IS_VALID_ISO", "profile.country", profile.Country.IsValid(),
		"the business country must be an ISO 3166-1 alpha-2 code")

	prohibited, why := profile.MCC.IsProhibited()
	add("L2.MCC_NOT_PROHIBITED", "profile.mcc", !prohibited, why)

	add("L2.REGISTRATION_NUMBER_PRESENT", "profile.registrationNumber",
		profile.RegistrationNumber != "",
		"a company registration number is required for know-your-business verification")

	add("L2.SUPPORT_CONTACT_PRESENT", "profile.supportEmail", profile.SupportEmail != "",
		"a support contact is required; it is what a cardholder is shown on a disputed charge")

	// Beneficial ownership: at least one principal, and the declared percentages must not exceed
	// 100. The aggregate enforces the ceiling on the way in; this rule catches the case where the
	// merchant declared nobody at all, which no aggregate invariant can see.
	principals := m.Principals()
	add("L2.AT_LEAST_ONE_PRINCIPAL", "principals", len(principals) > 0,
		"at least one beneficial owner or director must be declared")

	total := 0
	for _, p := range principals {
		total += p.OwnershipPct
	}
	add("L2.OWNERSHIP_SUMS_CORRECTLY", "principals", total <= 100,
		"declared beneficial ownership exceeds 100%")

	// Settlement: an account must exist to be verified at step 4. Discovering its absence there
	// would mean discovering it after the KYC submission — past the retained pivot, where the
	// abort is no longer clean.
	add("L2.SETTLEMENT_ACCOUNT_PRESENT", "bankAccounts", len(m.BankAccounts()) > 0,
		"a settlement account is required before onboarding can proceed")

	// The requested corridors must be expressible: unsupported currencies and unknown payment
	// methods are rejected here rather than by a gateway three steps later.
	badCurrency := ""
	for _, c := range in.Currencies {
		if !c.IsSupported() {
			badCurrency = string(c)
			break
		}
	}
	add("L2.CURRENCIES_SUPPORTED", "currencies", badCurrency == "",
		"currency "+badCurrency+" is not supported by the platform")

	badMethod := ""
	for _, pm := range in.PaymentMethods {
		if !pm.IsValid() {
			badMethod = string(pm)
			break
		}
	}
	add("L2.PAYMENT_METHODS_VALID", "paymentMethods", badMethod == "",
		"payment method "+badMethod+" is not a recognised method")

	badCountry := ""
	for _, c := range in.Countries {
		if !c.IsValid() {
			badCountry = string(c)
			break
		}
	}
	add("L2.COUNTRIES_VALID", "countries", badCountry == "",
		"country "+badCountry+" is not an ISO 3166-1 alpha-2 code")

	add("L2.AT_LEAST_ONE_GATEWAY", "gateways", len(in.Gateways) > 0,
		"at least one gateway must be selected or there is nothing to provision")

	dupGateway := shared.GatewayID("")
	seen := make(map[shared.GatewayID]bool, len(in.Gateways))
	for _, g := range in.Gateways {
		if seen[g] {
			dupGateway = g
			break
		}
		seen[g] = true
	}
	add("L2.GATEWAYS_DISTINCT", "gateways", dupGateway == "",
		"gateway "+string(dupGateway)+" is listed twice; provisioning it twice would create two sub-accounts")

	// Environment consistency. The failure mode this rule exists for is a certification run
	// charging a real card, which is worth a rule of its own rather than a comment.
	add("L2.ENVIRONMENT_MATCHES_MERCHANT", "environment",
		in.Environment == m.Environment(),
		"the onboarding environment does not match the merchant's own environment")

	return out, nil
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: BR-05, BR-06, FR-16, FR-21, FR-22, FR-23.
//
// Batch validation of the onboarding package before any side effect, and the KYC/KYB and
// bank-account checks whose failure is a business outcome to be corrected rather than an error
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
