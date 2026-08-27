package contract

import (
	"context"
	"net/http"
	"testing"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/httpx"
	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
)

// assertNeverNilNil proves no method ever answers with a nil result and a nil error.
//
// It is the least glamorous assertion here and it catches a genuinely dangerous bug: a caller
// handed (nil, nil) has no way to know what happened, and the code that dereferences the result
// panics in a request path — which, for a money-moving call, means the process dies between "the
// gateway acted" and "we recorded it".
//
// The sweep is over the cross product of every operation and every failure class, because the
// (nil, nil) return is always in the arm the author did not write a test for.
func assertNeverNilNil(t *testing.T, s Subject) {
	responses := []struct {
		name     string
		exchange httpx.Exchange
	}{
		{"Approved", httpx.Exchange{Response: s.Responses.AuthorizeApproved(SuiteGatewayRef, SuiteAmount, SuiteIdempotencyKey)}},
		{"Declined", httpx.Exchange{Response: s.Responses.AuthorizeHardDecline(SuiteGatewayRef)}},
		{"Timeout", httpx.Exchange{Response: httpx.TimeoutResponse()}},
		{"Refused", httpx.Exchange{Err: httpx.ConnectionRefused()}},
		{"AuthFailure", httpx.Exchange{Response: s.Responses.AuthFailure()}},
		{"Malformed", httpx.Exchange{Response: MalformedResponse()}},
		{"NotFound", httpx.Exchange{Response: s.Responses.LookupNotFound()}},
	}

	type call struct {
		name string
		run  func(spi.PaymentGateway) (*spi.Result, error)
	}
	calls := []call{
		{"Authorize", func(g spi.PaymentGateway) (*spi.Result, error) {
			return g.Authorize(context.Background(), s.authorizeRequest(SuiteIdempotencyKey))
		}},
		{"Capture", func(g spi.PaymentGateway) (*spi.Result, error) {
			return g.Capture(context.Background(), s.captureRequest(SuiteIdempotencyKey))
		}},
		{"Refund", func(g spi.PaymentGateway) (*spi.Result, error) {
			return g.Refund(context.Background(), s.refundRequest(SuiteIdempotencyKey))
		}},
		{"Void", func(g spi.PaymentGateway) (*spi.Result, error) {
			return g.Void(context.Background(), s.voidRequest(SuiteIdempotencyKey))
		}},
		{"LookupByRef", func(g spi.PaymentGateway) (*spi.Result, error) {
			return g.Lookup(context.Background(), s.lookupRequest(SuiteGatewayRef, SuiteIdempotencyKey))
		}},
		{"LookupByKey", func(g spi.PaymentGateway) (*spi.Result, error) {
			return g.Lookup(context.Background(), s.lookupRequest("", SuiteIdempotencyKey))
		}},
	}

	for _, r := range responses {
		for _, c := range calls {
			d := httpx.NewRecordingDoer(s.preamble()...).WithFallback(r.exchange)
			g, err := s.NewGateway(d)
			if err != nil {
				continue
			}
			res, callErr := c.run(g)
			if res == nil && callErr == nil {
				t.Fatalf("%s: %s under %s returned a nil result and a nil error; the caller cannot tell what happened "+
					"and the next dereference panics in a request path", s.Name, c.name, r.name)
			}
		}
	}

	// The same obligation applies to the provisioner, which has its own nil-returning paths.
	d := httpx.NewRecordingDoer(s.preamble()...).WithFallback(httpx.Exchange{Response: s.Responses.AuthFailure()})
	p, err := s.NewProvisioner(d)
	if err == nil && p != nil {
		res, provErr := p.Provision(context.Background(), s.provisionRequest(SuiteIdempotencyKey))
		if res == nil && provErr == nil {
			t.Fatalf("%s: Provision returned a nil result and a nil error", s.Name)
		}
	}
}

// assertProvisionIdempotent proves a repeated provisioning call converges rather than forking.
//
// This is the assertion with the least recoverable failure behind it. The onboarding workflow
// retries after a crash, and a second connected account for one merchant cannot be deleted at any
// of the three gateways: the merchant ends up with two KYC identities, split settlement, and a
// support case that takes weeks.
//
// Two things are checked: the same key must produce the same external account id, and the key must
// actually reach the gateway — an adapter that converges only because the test scripted one
// response would pass the first check alone.
func assertProvisionIdempotent(t *testing.T, s Subject) {
	d := s.doer(httpx.Exchange{Response: s.Responses.Provisioned(SuiteAccountID)})
	p, err := s.NewProvisioner(d)
	if err != nil {
		t.Fatalf("%s: NewProvisioner: %v", s.Name, err)
	}
	if p == nil {
		t.Fatalf("%s: NewProvisioner returned nil with no error", s.Name)
	}

	first, err := p.Provision(context.Background(), s.provisionRequest(SuiteIdempotencyKey))
	if err != nil {
		t.Fatalf("%s: first Provision: %v", s.Name, err)
	}
	if first.ExternalAccountID == "" {
		t.Fatalf("%s: Provision returned no external account id; the saga has nothing to compensate", s.Name)
	}
	second, err := p.Provision(context.Background(), s.provisionRequest(SuiteIdempotencyKey))
	if err != nil {
		t.Fatalf("%s: second Provision: %v", s.Name, err)
	}
	if first.ExternalAccountID != second.ExternalAccountID {
		t.Fatalf("%s: a retried provisioning produced account %q where the first produced %q; a duplicate connected "+
			"account is a manual-cleanup incident at every gateway", s.Name, second.ExternalAccountID, first.ExternalAccountID)
	}

	creating, ok := d.FirstMatching(func(r httpx.RecordedRequest) bool {
		return r.Operation == "provision" && r.Method == http.MethodPost
	})
	if !ok {
		t.Fatalf("%s: no provisioning request was recorded", s.Name)
	}
	if s.IdempotencyKeyOf(creating) != SuiteIdempotencyKey {
		t.Fatalf("%s: the provisioning request did not carry the idempotency key; the gateway cannot deduplicate a retry",
			s.Name)
	}

	// Provisioning without a key must be refused rather than performed. A workflow that lost its key
	// has lost the only thing that makes a retry safe.
	if _, err := p.Provision(context.Background(), s.provisionRequest("")); err == nil {
		t.Fatalf("%s: Provision accepted an empty idempotency key", s.Name)
	}
}

// assertDeprovisionTolerant proves a compensation succeeds when its target does not exist.
//
// Compensation runs after a crash, and the crash may have landed before the account was created —
// or after a previous compensation already removed it. A compensation that fails on a missing
// target leaves the saga permanently stuck, which is a worse outcome than the failure it was
// undoing.
func assertDeprovisionTolerant(t *testing.T, s Subject) {
	d := s.doer(httpx.Exchange{Response: s.Responses.DeprovisionMissing()})
	p, err := s.NewProvisioner(d)
	if err != nil {
		t.Fatalf("%s: NewProvisioner: %v", s.Name, err)
	}
	if err := p.Deprovision(context.Background(), SuiteAccountID); err != nil {
		t.Fatalf("%s: Deprovision failed for an account the gateway says does not exist (%v); the onboarding saga would "+
			"be stuck forever on a compensation whose work is already done", s.Name, err)
	}

	// An empty id means nothing was ever created, which is the crash-before-create case. It must be
	// a no-op rather than a call, and certainly not an error.
	before := d.Count()
	if err := p.Deprovision(context.Background(), ""); err != nil {
		t.Fatalf("%s: Deprovision with an empty account id returned %v, want nil", s.Name, err)
	}
	if d.Count() != before {
		t.Fatalf("%s: Deprovision with an empty account id issued a gateway call", s.Name)
	}

	// The success path must also work, so the assertion is not passing merely because Deprovision
	// swallows everything.
	if s.Responses.DeprovisionOK != nil {
		d2 := s.doer(httpx.Exchange{Response: s.Responses.DeprovisionOK(SuiteAccountID)})
		p2, err := s.NewProvisioner(d2)
		if err != nil {
			t.Fatalf("%s: NewProvisioner: %v", s.Name, err)
		}
		if err := p2.Deprovision(context.Background(), SuiteAccountID); err != nil {
			t.Fatalf("%s: Deprovision failed on the success path: %v", s.Name, err)
		}
	}
}
