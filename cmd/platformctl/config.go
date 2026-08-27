package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/transport/httpapi"
	"github.com/udaykishore-resu/payments-platform/internal/transport/httpapi/handlers"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// runConfig dispatches `config validate FILE`.
func runConfig(ctx context.Context, args []string, stdout, stderr *os.File) int {
	if len(args) == 0 || args[0] != "validate" {
		fmt.Fprintln(stderr, "config requires a sub-command: validate FILE")
		return 2
	}
	fs := flag.NewFlagSet("config validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "config validate requires exactly one file path")
		return 2
	}
	path := fs.Arg(0)
	if _, err := os.Stat(path); err != nil {
		fmt.Fprintf(stderr, "cannot read %s: %v\n", path, err)
		return 2
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(stderr, "cannot read %s: %v\n", path, err)
		return 2
	}
	fmt.Fprintf(stdout, "validating %s (%d bytes)\n\n", path, len(raw))

	// Validation is offline by design: it checks a document's *shape and internal consistency*,
	// which needs no database and no network. That is what makes it usable in a pull-request
	// check, which is where a configuration mistake is cheapest to catch.
	//
	// The decode is the strict one the API itself uses — unknown fields are rejected — because
	// the overwhelmingly common configuration mistake is a misspelled key, and a lenient decoder
	// answers "valid" for a document whose routing rules it silently dropped.
	var doc httpapi.MerchantConfiguration
	if err := httpapi.DecodeJSON(raw, &doc); err != nil {
		fmt.Fprintf(stderr, "the document is not a valid MerchantConfiguration:\n  %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "  ✓ decodes as MerchantConfiguration with no unknown fields")

	// The domain constructor is the second gate. Aggregate constructors validate (code-conventions
	// §10), so converting the document is itself a check of every value the domain has an opinion
	// about: currencies, payment methods, countries, gateway slugs, amounts and the routing
	// policy's internal consistency.
	cfg, err := handlers.ConfigurationToDomain(doc, placeholderTenant, placeholderMerchant)
	if err != nil {
		fmt.Fprintf(stderr, "the document is structurally valid but the domain refuses it:\n  %v\n", err)
		printDetails(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "  ✓ converts to the domain model: %d currencies, %d methods, %d routing rule(s), %d webhook endpoint(s)\n",
		len(cfg.SupportedCurrencies), len(cfg.PaymentMethods), len(cfg.Routing.Rules), len(cfg.Webhook.Endpoints))
	// Validate takes a capability lookup because two of its checks compare the document against
	// what the named gateways can actually do. Offline there is no descriptor set, so the
	// permissive lookup is supplied and those two checks are listed below as not performed —
	// which is the honest arrangement. A restrictive stub would be worse: it would fail
	// documents that are fine, and the operator would learn to ignore this command's output.
	if err := cfg.Validate(offlineCapabilities{}); err != nil {
		fmt.Fprintf(stderr, "the document fails the domain's own invariants:\n  %v\n", err)
		// The per-rule details, not just the summary. The summary says "the configuration is not
		// valid", which tells an operator nothing they can act on; the details name the field, the
		// rule and the remediation, and printing them is the whole reason this command is worth
		// running in a pull-request check rather than publishing and reading the 422.
		printDetails(stderr, err)
		return 1
	}
	fmt.Fprintln(stdout, "  ✓ satisfies the configuration aggregate's invariants")

	// What this command deliberately does NOT check, stated rather than implied. Every rule below
	// compares the document against state that only the control plane holds, so an offline run
	// cannot evaluate them — and reporting "valid" without saying so would let somebody publish a
	// document that names an uncertified gateway believing it had been checked.
	fmt.Fprint(stdout, `
Not checked offline — these L4 rules compare the document against control-plane state:
  L4.GATEWAY_CERTIFIED_FOR_ENVIRONMENT   needs the merchant's connection states
  L4.CURRENCY_WITHIN_TENANT_ENTITLEMENT  needs the tenant's entitlement set
  L4.GATEWAY_SUPPORTS_COMBINATION        needs the gateway descriptors' capabilities
  L4.LIMIT_WITHIN_TENANT_CEILING         needs the tenant's ceilings
  L4.SETTLEMENT_CURRENCY_HAS_ACCOUNT     needs the merchant's validated bank accounts
  L4.REFUND_WINDOW_WITHIN_GATEWAY        needs the gateway descriptors' refund windows
  L4.WEBHOOK_SECRET_REF_VALID            needs a metadata lookup of the secret reference

Run them by publishing against a control plane:
  PUT /v1/merchants/{merchantId}/configuration   (validate-only: add ?dryRun=true)
`)
	return 0
}

// placeholderTenant and placeholderMerchant stand in for the identity a file does not carry.
//
// A configuration document is scoped to a merchant, but the file on disk is the *body* only —
// the scope comes from the URL it is published to. The converter needs both, so the offline check
// supplies well-formed placeholders and the rules that depend on the real identity are the ones
// listed as not checked above. Fabricating a *plausible* merchant id here would be worse: it
// would make the output look as though a specific merchant had been validated.
const (
	placeholderTenant   = shared.TenantID("ten_00000000000000000000000000")
	placeholderMerchant = shared.MerchantID("mrc_00000000000000000000000000")
)

// offlineCapabilities answers every capability question permissively.
//
// It exists because config.Validate takes a lookup and there is no descriptor set offline. The
// two checks it neutralises — "can this gateway refund after the configured window" and "does any
// routed gateway support this currency and method" — are named in the not-checked list this
// command prints, so the permissiveness is disclosed rather than hidden.
type offlineCapabilities struct{}

func (offlineCapabilities) CanRefundAfter(shared.GatewayID, time.Duration) bool { return true }
func (offlineCapabilities) AnySupports([]shared.GatewayID, money.Currency, shared.PaymentMethod) bool {
	return true
}

// printDetails renders an apierror's per-field details as an operator-readable list.
//
// A validation failure carries one entry per rule that was not satisfied, each naming the field,
// the published rule id and what to do about it. Those entries are the actionable part: the
// top-level message is a category, and a command that printed only the category would send the
// operator to the source to find out which of nineteen L4 rules refused their document.
func printDetails(w io.Writer, err error) {
	var e *apierror.Error
	if !errors.As(err, &e) || len(e.Details) == 0 {
		return
	}
	fmt.Fprintln(w)
	for _, d := range e.Details {
		field := d.Field
		if field == "" {
			field = "(document)"
		}
		rule := d.RuleID
		if rule == "" {
			rule = d.Code
		}
		fmt.Fprintf(w, "  %-28s %-34s %s\n", field, rule, d.Message)
	}
}
