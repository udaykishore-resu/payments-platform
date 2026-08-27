package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

func TestClassifyIsDrivenByTheRetryableBit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		err           error
		sideEffecting bool
		want          FailureClass
		why           string
	}{
		{
			name: "retryable infrastructure error", err: apierror.New(apierror.CodeServiceUnavailable, ""),
			want: ClassTransient,
			why:  "the retryable bit is the same one the gateway dispatcher and the outbox relay branch on",
		},
		{
			name: "business rule rejection", err: apierror.New(apierror.CodeMerchantNotActive, ""),
			want: ClassTerminalBusiness,
			why:  "a business no is an outcome to record and surface, not something to retry into the ground",
		},
		{
			name: "validation failure", err: apierror.New(apierror.CodeValidationFailed, ""),
			want: ClassTerminalBusiness,
		},
		{
			name: "vendor credentials rejected", err: apierror.New(apierror.CodeGatewayAuthenticationFailed, ""),
			want: ClassTerminalTechnical,
			why:  "our credentials are wrong: a deploy problem, not a merchant problem",
		},
		{
			name: "unclassified error", err: errors.New("something went wrong"),
			want: ClassTerminalTechnical,
			why:  "an error nobody has reasoned about is not evidence that a retry is safe",
		},
		{
			name: "deadline on a side-effecting step", err: context.DeadlineExceeded, sideEffecting: true,
			want: ClassAmbiguous,
			why:  "we do not know whether the vendor acted; the answer is a lookup, not another call",
		},
		{
			name: "deadline on a pure step", err: context.DeadlineExceeded,
			want: ClassTransient,
			why:  "nothing external could have happened, so a retry is free",
		},
		{
			name: "gateway timeout on a side-effecting step", err: apierror.New(apierror.CodeGatewayTimeout, ""),
			sideEffecting: true, want: ClassAmbiguous,
		},
		{
			name: "step timeout on a side-effecting step",
			err:  fmt.Errorf("%w: took too long", ErrStepTimeout), sideEffecting: true,
			want: ClassAmbiguous,
		},
	}

	for _, tc := range cases {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Classify(tc.err, tc.sideEffecting); got != tc.want {
				t.Fatalf("Classify = %s, want %s (%s)", got, tc.want, tc.why)
			}
		})
	}
}

func TestWithClassOverridesInference(t *testing.T) {
	t.Parallel()
	// Only the activity can tell "these documents are unreadable" (business) from "we sent a
	// malformed request" (technical); both arrive as the same 4xx shape.
	err := WithClass(apierror.New(apierror.CodeServiceUnavailable, "vendor 400"), ClassTerminalBusiness)
	if got := ClassifyStep(err, true); got != ClassTerminalBusiness {
		t.Fatalf("ClassifyStep = %s, want the explicit override", got)
	}
	if !errors.Is(err, apierror.New(apierror.CodeServiceUnavailable, "")) {
		t.Fatal("WithClass broke errors.Is on the underlying error")
	}
}

func TestClassPolicyBits(t *testing.T) {
	t.Parallel()
	if !ClassTerminalBusiness.IsTerminal() || !ClassTerminalTechnical.IsTerminal() {
		t.Fatal("terminal classes must forbid another attempt")
	}
	if ClassTransient.IsTerminal() || ClassAmbiguous.IsTerminal() || ClassManual.IsTerminal() {
		t.Fatal("a non-terminal class was reported terminal")
	}
	// Only a business no unwinds the saga. Compensating away a merchant's provisioned gateways
	// because *our* credentials were misconfigured destroys work a redeploy would have saved.
	if !ClassTerminalBusiness.Aborts() {
		t.Fatal("a business rejection must abort the saga")
	}
	if ClassTerminalTechnical.Aborts() {
		t.Fatal("a technical failure must go to the DLQ for a fix-forward, not unwind the saga")
	}
}

func TestChainPreservesEveryLinkInOrder(t *testing.T) {
	t.Parallel()
	root := errors.New("dial tcp 10.0.0.1:443: connect: connection refused")
	middle := apierror.Wrap(root, apierror.CodeDependencyFailure, "the KYC vendor is unreachable")
	outer := fmt.Errorf("submitting case: %w", middle)

	chain := Chain(outer)
	if len(chain) < 3 {
		t.Fatalf("chain has %d links, want at least 3: %+v", len(chain), chain)
	}
	// The retryable bit must survive at the link that carried it. A chain in which an inner
	// retryable error is wrapped by an outer non-retryable one is the classic "why did this give
	// up" mystery, and it is only diagnosable if both bits are preserved.
	sawRetryable := false
	for _, link := range chain {
		if link.Code == string(apierror.CodeDependencyFailure) && link.Retryable {
			sawRetryable = true
		}
	}
	if !sawRetryable {
		t.Fatalf("the chain lost the retryable bit: %+v", chain)
	}
	if got := Summarize(chain); got == "" {
		t.Fatal("Summarize produced nothing")
	}
}

func TestChainTerminatesOnACycle(t *testing.T) {
	t.Parallel()
	// A cycle in an Unwrap chain is a programming error, but it must not hang a worker holding a
	// lease: the instance would then be neither failing nor progressing, which is the one failure
	// mode nobody alerts on.
	c := &cyclic{}
	c.self = c
	if got := len(Chain(c)); got > 33 {
		t.Fatalf("Chain did not bound a cyclic error: %d links", got)
	}
}

type cyclic struct{ self *cyclic }

func (c *cyclic) Error() string { return "cyclic" }
func (c *cyclic) Unwrap() error { return c.self }

func TestIsLeaseLostUnwraps(t *testing.T) {
	t.Parallel()
	err := apierror.Wrap(ErrLeaseLost, apierror.CodeWorkflowNotResumable, "epoch 3 vs 4")
	if !IsLeaseLost(err) {
		t.Fatal("a wrapped ErrLeaseLost was not recognised; a zombie worker would keep writing")
	}
	if IsLeaseLost(errors.New("unrelated")) {
		t.Fatal("an unrelated error was reported as a lost lease")
	}
}

func TestNewFailureRecordCapturesCodeAndChain(t *testing.T) {
	t.Parallel()
	err := apierror.Wrap(errors.New("io timeout"), apierror.CodeGatewayUnavailable, "adyen is down")
	rec := NewFailureRecord("provision-gateways", 3, ClassTransient, err)
	if rec.Step != "provision-gateways" || rec.Attempt != 3 || rec.Class != ClassTransient {
		t.Fatalf("record header is wrong: %+v", rec)
	}
	if rec.Code != string(apierror.CodeGatewayUnavailable) {
		t.Fatalf("Code = %q", rec.Code)
	}
	if len(rec.Chain) < 2 {
		t.Fatalf("the DLQ entry would carry %d links; triage needs the whole chain", len(rec.Chain))
	}
}
