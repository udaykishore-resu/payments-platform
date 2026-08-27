package config_test

import (
	"encoding/json"
	"testing"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/platform/config"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

func baseFlag() config.Flag {
	return config.Flag{
		Key:          "networkTokens",
		Class:        config.ClassMoneySemantic,
		Environments: []shared.Environment{shared.EnvironmentProduction},
		Default:      false,
		HasDefault:   true,
		GuardMetric:  "pp_payment_authorization_rate",
	}
}

func subject(m shared.MerchantID) config.Subject {
	return config.Subject{
		TenantID:      tenantA,
		MerchantID:    m,
		Environment:   shared.EnvironmentProduction,
		Country:       "GB",
		Currency:      "GBP",
		PaymentMethod: "card",
		MerchantTier:  "standard",
		Gateway:       "stripe",
	}
}

// The evaluation order is fixed, top to bottom, first match wins.
func TestEvaluationOrder(t *testing.T) {
	t.Parallel()

	t.Run("1: the kill switch beats everything and can only force false", func(t *testing.T) {
		t.Parallel()
		f := baseFlag()
		f.KillSwitch = true
		f.Default = true
		f.RolloutBasisPoints = 10000
		f.MerchantOverrides = map[shared.MerchantID]bool{merchantA: true}
		f.TenantOverrides = map[shared.TenantID]bool{tenantA: true}
		f.Rules = []config.TargetRule{{Attribute: "country", Value: "GB", Result: true}}
		if f.Evaluate(subject(merchantA)) {
			t.Fatal("an engaged kill switch must force false unconditionally")
		}
	})

	t.Run("2: the environment gate beats overrides", func(t *testing.T) {
		t.Parallel()
		f := baseFlag()
		f.Environments = []shared.Environment{shared.EnvironmentSandbox}
		f.MerchantOverrides = map[shared.MerchantID]bool{merchantA: true}
		if f.Evaluate(subject(merchantA)) {
			t.Fatal("a sandbox-only capability must not leak into production")
		}
		// A flag enabled nowhere is off.
		f2 := baseFlag()
		f2.Environments = nil
		f2.Default = true
		if f2.Evaluate(subject(merchantA)) {
			t.Fatal("a flag enabled in no environment must be off")
		}
	})

	t.Run("3: the merchant override beats the tenant override", func(t *testing.T) {
		t.Parallel()
		f := baseFlag()
		f.MerchantOverrides = map[shared.MerchantID]bool{merchantA: false}
		f.TenantOverrides = map[shared.TenantID]bool{tenantA: true}
		if f.Evaluate(subject(merchantA)) {
			t.Fatal("most specific wins")
		}
		if !f.Evaluate(subject(merchantB)) {
			t.Fatal("a merchant with no override falls through to the tenant override")
		}
	})

	t.Run("4: the tenant override beats targeting", func(t *testing.T) {
		t.Parallel()
		f := baseFlag()
		f.TenantOverrides = map[shared.TenantID]bool{tenantA: false}
		f.Rules = []config.TargetRule{{Attribute: "country", Value: "GB", Result: true}}
		if f.Evaluate(subject(merchantA)) {
			t.Fatal("the tenant override wins over an attribute rule")
		}
	})

	t.Run("5: targeting is ordered and first match wins", func(t *testing.T) {
		t.Parallel()
		f := baseFlag()
		f.Rules = []config.TargetRule{
			{Attribute: "currency", Value: "GBP", Result: false},
			{Attribute: "country", Value: "GB", Result: true},
		}
		if f.Evaluate(subject(merchantA)) {
			t.Fatal("the first matching rule wins; reordering is a real semantic change")
		}
		// Reversing the order reverses the answer, which is exactly why the order is versioned.
		f.Rules[0], f.Rules[1] = f.Rules[1], f.Rules[0]
		if !f.Evaluate(subject(merchantA)) {
			t.Fatal("reordered rules must produce the reordered answer")
		}
		// An attribute this binary does not understand never matches.
		f.Rules = []config.TargetRule{{Attribute: "phase_of_moon", Value: "GB", Result: true}}
		if f.Evaluate(subject(merchantA)) {
			t.Fatal("an unknown attribute must never match")
		}
	})

	t.Run("6: percentage rollout, then 7: default", func(t *testing.T) {
		t.Parallel()
		off := baseFlag()
		if off.Evaluate(subject(merchantA)) {
			t.Fatal("with no rollout the default applies")
		}
		full := baseFlag()
		full.RolloutBasisPoints = 10000
		if !full.Evaluate(subject(merchantA)) {
			t.Fatal("a 100% rollout must include everyone")
		}
		def := baseFlag()
		def.Default = true
		if !def.Evaluate(subject(merchantA)) {
			t.Fatal("the declared default applies when nothing else matched")
		}
	})
}

// A merchant must get consistent behaviour across their payments, and a ramp must only ever add
// merchants.
func TestRolloutIsStableMonotonicAndPerMerchant(t *testing.T) {
	t.Parallel()
	merchants := make([]shared.MerchantID, 0, 400)
	for i := 0; i < 400; i++ {
		merchants = append(merchants, shared.MerchantID("mrc_"+string(rune('A'+i%26))+string(rune('a'+i/26))))
	}

	f := baseFlag()
	f.RolloutBasisPoints = 1000 // 10%

	// Stability: the same merchant always gets the same answer.
	for _, m := range merchants {
		first := f.Evaluate(subject(m))
		for i := 0; i < 5; i++ {
			if f.Evaluate(subject(m)) != first {
				t.Fatalf("merchant %s got an unstable answer", m)
			}
		}
	}

	// Monotonicity: ramping up only ever adds merchants.
	enabled := map[shared.MerchantID]bool{}
	prev := 0
	for _, bps := range []int{1000, 2000, 5000, 10000} {
		f.RolloutBasisPoints = bps
		count := 0
		for _, m := range merchants {
			on := f.Evaluate(subject(m))
			if enabled[m] && !on {
				t.Fatalf("ramping to %d bps removed merchant %s; a ramp must never look like a regression", bps, m)
			}
			if on {
				enabled[m] = true
				count++
			}
		}
		if count < prev {
			t.Fatalf("ramping to %d bps reduced the population from %d to %d", bps, prev, count)
		}
		prev = count
	}
	if prev != len(merchants) {
		t.Fatalf("a 100%% rollout enabled %d of %d merchants", prev, len(merchants))
	}

	// Independence: a second flag must not reshuffle the first flag's population onto the same
	// merchants, or every rollout lands on the same unlucky tenants.
	g := baseFlag()
	g.Key = "partialCapture"
	g.RolloutBasisPoints = 1000
	f.RolloutBasisPoints = 1000
	same := 0
	for _, m := range merchants {
		if f.Evaluate(subject(m)) == g.Evaluate(subject(m)) {
			same++
		}
	}
	if same == len(merchants) {
		t.Fatal("two flags at the same percentage selected identical populations; bucketing must include the key")
	}

	// The bucketing subject is the merchant, never anything payment-scoped: two subjects that
	// differ only in their non-merchant attributes must land in the same bucket.
	a := subject(merchantA)
	b := subject(merchantA)
	b.Country, b.Currency, b.Gateway = "DE", "EUR", "adyen"
	if f.Evaluate(a) != f.Evaluate(b) {
		t.Fatal("bucketing must depend on the merchant alone")
	}

	// A subject with no merchant cannot be bucketed, and must fall through to the default rather
	// than to whatever the empty string happens to hash to.
	nomerchant := baseFlag()
	nomerchant.RolloutBasisPoints = 10000
	if nomerchant.Evaluate(config.Subject{Environment: shared.EnvironmentProduction}) {
		t.Fatal("an unbucketable subject must fall through to the default")
	}
}

func TestFlagValidation(t *testing.T) {
	t.Parallel()
	if err := baseFlag().Validate(); err != nil {
		t.Fatalf("a complete flag must validate: %v", err)
	}
	cases := map[string]func(*config.Flag){
		"no key":                       func(f *config.Flag) { f.Key = "" },
		"no class":                     func(f *config.Flag) { f.Class = "" },
		"unknown class":                func(f *config.Flag) { f.Class = "COSMETIC" },
		"no declared default":          func(f *config.Flag) { f.HasDefault = false },
		"negative rollout":             func(f *config.Flag) { f.RolloutBasisPoints = -1 },
		"rollout above 100%":           func(f *config.Flag) { f.RolloutBasisPoints = 10001 },
		"money-semantic with no guard": func(f *config.Flag) { f.GuardMetric = "" },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := baseFlag()
			mut(&f)
			err := f.Validate()
			if err == nil {
				t.Fatal("must not be publishable")
			}
			if apierror.CodeOf(err) != apierror.CodeConfigurationInvalid {
				t.Fatalf("err = %v", err)
			}
		})
	}
	// A non-money-semantic flag needs no guard metric.
	op := baseFlag()
	op.Class, op.GuardMetric = config.ClassOperational, ""
	if err := op.Validate(); err != nil {
		t.Fatalf("an operational flag needs no guard metric: %v", err)
	}
}

func TestSetEvaluate(t *testing.T) {
	t.Parallel()
	set := config.Set{"networkTokens": baseFlag()}
	if set.Evaluate("networkTokens", subject(merchantA)) {
		t.Fatal("default false")
	}
	// An undeclared flag is false: a typo must not enable a capability.
	if set.Evaluate("netwrokTokens", subject(merchantA)) {
		t.Fatal("an unknown key must be false")
	}
}

// The single most important rule in the flag design: a payment resolves its flags once, at
// creation, and is judged by that resolution for its whole lifetime.
func TestFlagsAreResolvedOnceAndStamped(t *testing.T) {
	t.Parallel()
	set := config.Set{
		"networkTokens":  baseFlag(),
		"partialCapture": withKey(baseFlag(), "partialCapture"),
		"prettyErrors":   presentation("prettyErrors"),
	}
	subj := subject(merchantA)

	// Authorization at 13:58: networkTokens is off for this merchant.
	stamped := set.ResolveAtCreation(subj)
	if stamped.Get("networkTokens") {
		t.Fatal("precondition: the flag is off at creation")
	}

	// 14:00 — the flag ramps to 100%.
	ramped := set["networkTokens"]
	ramped.RolloutBasisPoints = 10000
	set["networkTokens"] = ramped
	if !set.Evaluate("networkTokens", subj) {
		t.Fatal("precondition: the live snapshot now says true")
	}

	// 14:05 — the capture reads the stamped context, not the live snapshot. Without this, the
	// capture could select a different token type or gateway than the authorization it is
	// capturing against.
	if stamped.Get("networkTokens") {
		t.Fatal("a flag flipped mid-lifecycle must not change the rules a payment is judged by")
	}

	// Only money-semantic flags are stamped: a presentation flag may legitimately change
	// mid-lifecycle, and stamping it would freeze a cosmetic decision forever.
	if stamped.Has("prettyErrors") {
		t.Fatalf("only MONEY_SEMANTIC flags are stamped, got %v", stamped.Keys())
	}
	if !stamped.Has("networkTokens") || !stamped.Has("partialCapture") {
		t.Fatalf("every money-semantic flag must be stamped, got %v", stamped.Keys())
	}

	// "Resolved to false" must be distinguishable from "this payment predates the flag".
	if stamped.Has("threeDSv3") {
		t.Fatal("an unstamped key must report as absent")
	}
	if stamped.Get("threeDSv3") {
		t.Fatal("an unstamped key reads false")
	}
	if stamped.Len() != 2 {
		t.Fatalf("Len = %d", stamped.Len())
	}
}

// The stamped context is written to the payment row and published in payment.created.v1, so a
// merchant or an auditor can always answer "which rules governed this payment?".
func TestFlagContextRoundTrips(t *testing.T) {
	t.Parallel()
	set := config.Set{"networkTokens": baseFlag(), "partialCapture": withKey(baseFlag(), "partialCapture")}
	stamped := set.ResolveAtCreation(subject(merchantA))

	raw, err := json.Marshal(stamped)
	if err != nil {
		t.Fatal(err)
	}
	var restored config.FlagContext
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Len() != stamped.Len() {
		t.Fatalf("round trip lost keys: %v vs %v", restored.Keys(), stamped.Keys())
	}
	for _, k := range stamped.Keys() {
		if restored.Get(k) != stamped.Get(k) || !restored.Has(k) {
			t.Fatalf("key %q did not survive the round trip", k)
		}
	}
	// A zero context marshals as an empty object, not as null, so the column is never NULL and a
	// reader never has to special-case it.
	var zero config.FlagContext
	b, err := json.Marshal(zero)
	if err != nil || string(b) != "{}" {
		t.Fatalf("zero context marshalled as %s (%v)", b, err)
	}
	if zero.String() != "{}" {
		t.Fatalf("String = %q", zero.String())
	}
	// The log rendering is stably ordered so two payments diff cleanly.
	if stamped.String() != "{networkTokens=false,partialCapture=false}" {
		t.Fatalf("String = %q", stamped.String())
	}
	if err := json.Unmarshal([]byte("not json"), &restored); err == nil {
		t.Fatal("a corrupt stamped context must not decode silently")
	}
}

func TestMoneySemanticKeysAreSorted(t *testing.T) {
	t.Parallel()
	set := config.Set{
		"zeta":  withKey(baseFlag(), "zeta"),
		"alpha": withKey(baseFlag(), "alpha"),
		"mid":   presentation("mid"),
	}
	keys := set.MoneySemanticKeys()
	if len(keys) != 2 || keys[0] != "alpha" || keys[1] != "zeta" {
		t.Fatalf("keys = %v", keys)
	}
}

func withKey(f config.Flag, key string) config.Flag {
	f.Key = key
	return f
}

func presentation(key string) config.Flag {
	f := baseFlag()
	f.Key, f.Class, f.GuardMetric = key, config.ClassPresentation, ""
	return f
}
