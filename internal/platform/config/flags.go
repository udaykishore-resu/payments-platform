package config

import (
	"encoding/json"
	"hash/crc32"
	"sort"
	"strconv"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// Class is a flag's declared blast radius (control-plane.md §6.1).
//
// Declaring it is mandatory at flag creation. The class is not documentation: MONEY_SEMANTIC
// flags are the ones that must be resolved once and stamped, and a flag with no class would
// silently escape that rule.
type Class string

const (
	// ClassPresentation affects only response shape or another non-behavioural surface.
	ClassPresentation Class = "PRESENTATION"
	// ClassOperational affects mechanism, not money outcomes — a connection-pool strategy, a
	// shadow-mode call. It must be a no-op for money outcomes, asserted by a test.
	ClassOperational Class = "OPERATIONAL"
	// ClassMoneySemantic can change whether, how much, or through whom money moves. Network
	// tokens, partial capture, a routing strategy, a risk rule set, a decline mapping.
	ClassMoneySemantic Class = "MONEY_SEMANTIC"
)

// IsValid reports whether c is a declared class.
func (c Class) IsValid() bool {
	return c == ClassPresentation || c == ClassOperational || c == ClassMoneySemantic
}

// TargetRule is one attribute-targeting rule. Rules are ordered and the first match wins, so
// reordering them is a real semantic change — which is why the order is part of the versioned
// configuration document and shows up in its diff.
type TargetRule struct {
	// Attribute is one of "country", "currency", "payment_method", "merchant_tier", "gateway".
	Attribute string
	// Value is compared for exact equality against the subject's attribute.
	Value string
	// Result is what the flag evaluates to when the rule matches.
	Result bool
}

// Flag is one feature flag's desired state.
type Flag struct {
	Key   string
	Class Class
	// KillSwitch, when engaged, forces false unconditionally. It can only ever force false: a
	// kill switch that could turn something *on* is not a safety mechanism, and during an
	// incident the only question anyone should have to answer about it is "does this stop the
	// thing".
	KillSwitch bool
	// Environments lists the environments the flag is enabled for. Empty means none — a flag
	// that has not been enabled anywhere is off, which is the default-deny reading.
	Environments []shared.Environment
	// MerchantOverrides and TenantOverrides are explicit values. Most specific wins, so a
	// merchant override beats a tenant override.
	MerchantOverrides map[shared.MerchantID]bool
	TenantOverrides   map[shared.TenantID]bool
	// Rules are ordered attribute-targeting rules.
	Rules []TargetRule
	// RolloutBasisPoints is the percentage rollout, in hundredths of a percent, so 1000 is 10 %.
	RolloutBasisPoints int
	// Default is the value when nothing above matched. A flag with no declared default fails
	// validation, because "no default" in practice means "whatever the zero value happens to be".
	Default bool
	// HasDefault distinguishes a declared `false` default from an undeclared one.
	HasDefault bool
	// GuardMetric names the metric the rollout controller watches for a MONEY_SEMANTIC flag. A
	// ramp with no guard metric is a ramp nobody is measuring.
	GuardMetric string
}

// Validate enforces the declaration rules from control-plane.md §6.
func (f Flag) Validate() error {
	var details []apierror.Detail
	add := func(field, code, msg, rule string) {
		details = append(details, apierror.Detail{Field: field, Code: code, Message: msg, RuleID: rule})
	}
	if f.Key == "" {
		add("key", "MISSING", "a flag requires a key", "L4.FLAG_KEY_REQUIRED")
	}
	if !f.Class.IsValid() {
		add("class", "MISSING", "a flag must declare its class", "L4.FLAG_CLASS_REQUIRED")
	}
	if !f.HasDefault {
		add("default", "MISSING", "a flag must declare a default", "L4.FLAG_DEFAULT_REQUIRED")
	}
	if f.RolloutBasisPoints < 0 || f.RolloutBasisPoints > 10000 {
		add("rolloutBasisPoints", "OUT_OF_RANGE", "rollout must be between 0 and 10000 basis points",
			"L4.FLAG_ROLLOUT_IN_RANGE")
	}
	if f.Class == ClassMoneySemantic && f.GuardMetric == "" {
		add("guardMetric", "MISSING",
			"a money-semantic flag must declare a guard metric so its ramp can be observed rather than assumed",
			"L4.FLAG_MONEY_SEMANTIC_GUARDED")
	}
	if len(details) > 0 {
		return apierror.Newf(apierror.CodeConfigurationInvalid, "flag %q is not publishable", f.Key).
			WithDetails(details...)
	}
	return nil
}

// Subject is what a flag is evaluated against.
//
// MerchantID is the bucketing subject and is required for a percentage rollout; the other fields
// feed attribute targeting. There is no payment identifier here, deliberately — see Evaluate.
type Subject struct {
	TenantID      shared.TenantID
	MerchantID    shared.MerchantID
	Environment   shared.Environment
	Country       string
	Currency      string
	PaymentMethod string
	MerchantTier  string
	Gateway       string
}

// Evaluate resolves a flag for a subject.
//
// # A pure function, deliberately
//
// It performs no I/O, cannot fail, reads no clock and draws no random number, and returns the
// same answer for the same inputs forever. That purity is what makes flag behaviour reproducible
// in an incident reconstruction: given the configuration version and the subject, anyone can
// re-derive exactly what the platform decided, six months later, without the platform running.
//
// # Order, first match wins
//
//  1. Kill switch → false, unconditionally.
//  2. Environment gate → false if the flag is not enabled for this environment.
//  3. Merchant override → that value. Most specific wins.
//  4. Tenant override → that value.
//  5. Attribute targeting → the first matching rule's value.
//  6. Percentage rollout → crc32(key ‖ ":" ‖ merchant_id) mod 10000 < bps.
//  7. Default.
//
// # Why the bucketing subject is the merchant and never the payment
//
// A merchant must get consistent behaviour across their payments. Bucketing per payment would
// mean partial capture works on one payment and not the next, for the same integration, at the
// same instant — an integration nightmare and a support case nobody can reproduce. Hashing
// (key ‖ subject) rather than the subject alone means adding a second flag does not reshuffle
// the first flag's population, and increasing the percentage only ever *adds* merchants, so a
// ramp never looks like a regression to someone already in it.
func (f Flag) Evaluate(s Subject) bool {
	if f.KillSwitch {
		return false
	}
	if !f.enabledIn(s.Environment) {
		return false
	}
	if v, ok := f.MerchantOverrides[s.MerchantID]; ok {
		return v
	}
	if v, ok := f.TenantOverrides[s.TenantID]; ok {
		return v
	}
	for _, r := range f.Rules {
		if r.matches(s) {
			return r.Result
		}
	}
	if f.RolloutBasisPoints > 0 && s.MerchantID != "" {
		if bucket(f.Key, string(s.MerchantID)) < uint32(f.RolloutBasisPoints) {
			return true
		}
	}
	return f.Default
}

func (f Flag) enabledIn(env shared.Environment) bool {
	for _, e := range f.Environments {
		if e == env {
			return true
		}
	}
	return false
}

func (r TargetRule) matches(s Subject) bool {
	switch r.Attribute {
	case "country":
		return r.Value == s.Country
	case "currency":
		return r.Value == s.Currency
	case "payment_method":
		return r.Value == s.PaymentMethod
	case "merchant_tier":
		return r.Value == s.MerchantTier
	case "gateway":
		return r.Value == s.Gateway
	default:
		// An attribute this binary does not understand never matches. A newer control plane can
		// therefore publish a rule an older data-plane pod ignores, which degrades to the
		// default rather than to a crash or to an accidental match.
		return false
	}
}

// bucket is the stable rollout hash. CRC32 rather than a cryptographic hash because the property
// needed is uniform distribution, not preimage resistance, and this runs once per payment.
func bucket(key, subject string) uint32 {
	return crc32.ChecksumIEEE([]byte(key+":"+subject)) % 10000
}

// Set is a collection of flags by key.
type Set map[string]Flag

// Evaluate resolves one flag by key. An unknown key is false: a flag that has not been declared
// is not a flag whose behaviour anybody has decided, and defaulting an unknown key to true is
// how a typo enables a capability.
func (s Set) Evaluate(key string, subj Subject) bool {
	f, ok := s[key]
	if !ok {
		return false
	}
	return f.Evaluate(subj)
}

// MoneySemanticKeys returns the keys of every MONEY_SEMANTIC flag, sorted, so the stamped context
// is deterministic and diffable.
func (s Set) MoneySemanticKeys() []string {
	keys := make([]string, 0, len(s))
	for k, f := range s {
		if f.Class == ClassMoneySemantic {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// FlagContext is the resolved, frozen set of flags a payment is judged by.
//
// # Resolve once, stamp, freeze — the single most important rule here
//
// At payment creation the orchestrator evaluates every MONEY_SEMANTIC flag and writes the result
// into the payment record. Capture, refund, void, webhook resolution and reconciliation — which
// may happen days later — read this stamped map and never the live snapshot.
//
// The failure this prevents is concrete. Suppose `networkTokens` ramps from 10 % to 50 % at
// 14:00. A payment authorized at 13:58 used a gateway-scoped token and is pinned to one gateway.
// At 14:05 the merchant captures it. Without stamping, the capture path re-evaluates the flag,
// sees `true`, and may select a different token type or a different gateway — against an
// authorization that lives somewhere else. The capture fails, or worse, succeeds against the
// wrong authorization. With stamping, the capture reads `false` from the payment and behaves
// exactly as the authorization did.
//
// The general form: a flag flip between two steps of one payment's lifecycle would mean the
// payment is judged by one rule set at authorization and another at capture, and the ledger
// entries that result cannot be reconciled against either. Freezing the resolution makes the
// payment's own record the answer to "which rules governed this?", which is also the answer an
// auditor needs and the answer a support engineer needs.
//
// Structurally, the way this rule is *enforced* rather than merely documented: the API available
// to a capture or refund use case is FlagContext.Get, reading the stamped map. Those use cases
// take no Set and no evaluator in their constructors, so late evaluation is not expressible.
type FlagContext struct {
	values map[string]bool
}

// ResolveAtCreation evaluates every MONEY_SEMANTIC flag for a subject and freezes the result.
//
// It is named for the only moment it may be called. Calling it later in a payment's lifecycle is
// the bug this whole mechanism exists to prevent, and the name is the warning.
func (s Set) ResolveAtCreation(subj Subject) FlagContext {
	keys := s.MoneySemanticKeys()
	values := make(map[string]bool, len(keys))
	for _, k := range keys {
		values[k] = s[k].Evaluate(subj)
	}
	return FlagContext{values: values}
}

// Get returns the value a payment was created under. An unstamped key is false, for the same
// reason an unknown flag key is false.
func (c FlagContext) Get(key string) bool { return c.values[key] }

// Has reports whether a key was stamped at all, which distinguishes "resolved to false" from
// "this payment predates the flag" — a distinction an incident reconstruction needs.
func (c FlagContext) Has(key string) bool { _, ok := c.values[key]; return ok }

// Keys returns the stamped keys in sorted order.
func (c FlagContext) Keys() []string {
	keys := make([]string, 0, len(c.values))
	for k := range c.values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Len reports how many flags were stamped.
func (c FlagContext) Len() int { return len(c.values) }

// MarshalJSON renders the context for the payment's `flag_context` column and for
// `payment.created.v1`. Publishing it is what lets a merchant or an auditor answer "which rules
// governed this payment?" from the payment itself.
func (c FlagContext) MarshalJSON() ([]byte, error) {
	if c.values == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(c.values)
}

// UnmarshalJSON restores a stamped context from the payment record.
func (c *FlagContext) UnmarshalJSON(b []byte) error {
	var m map[string]bool
	if err := json.Unmarshal(b, &m); err != nil {
		return apierror.Wrap(err, apierror.CodeInternalError, "decoding a stamped flag context")
	}
	c.values = m
	return nil
}

// String renders the context for a log field, in a stable order.
func (c FlagContext) String() string {
	keys := c.Keys()
	out := make([]byte, 0, len(keys)*16)
	out = append(out, '{')
	for i, k := range keys {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, k...)
		out = append(out, '=')
		out = append(out, strconv.FormatBool(c.values[k])...)
	}
	return string(append(out, '}'))
}
