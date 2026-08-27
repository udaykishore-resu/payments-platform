package engine

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

type kycIn struct {
	MerchantID string `json:"merchantId"`
}

type kycOut struct {
	VendorRef string `json:"vendorCaseRef"`
}

func TestTypedActivityRoundTripsJSON(t *testing.T) {
	t.Parallel()
	act := NewTypedActivity("submit-kyc", func(_ context.Context, meta Input, in kycIn) (kycOut, error) {
		if meta.IdempotencyKey == "" {
			t.Error("the typed wrapper dropped the idempotency key, which is what the vendor dedupes on")
		}
		return kycOut{VendorRef: "case_" + in.MerchantID}, nil
	})

	payload, _ := json.Marshal(kycIn{MerchantID: "mrc_1"})
	out, err := act.Execute(context.Background(), Input{
		Step: "submit-kyc", IdempotencyKey: "K", Payload: payload,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var decoded kycOut
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("output does not decode: %v", err)
	}
	if decoded.VendorRef != "case_mrc_1" {
		t.Fatalf("VendorRef = %q", decoded.VendorRef)
	}
}

// TestTypedActivityClassifiesAnUndecodableInputAsTechnical: the bytes will not change on a
// retry. A definition drift or a rollback onto older code is a deployment problem, and retrying
// it just burns the vendor's rate limit on a request we cannot even build.
func TestTypedActivityClassifiesAnUndecodableInputAsTechnical(t *testing.T) {
	t.Parallel()
	act := NewTypedActivity("x", func(_ context.Context, _ Input, _ kycIn) (kycOut, error) {
		t.Fatal("the activity body ran despite an undecodable input")
		return kycOut{}, nil
	})
	_, err := act.Execute(context.Background(), Input{Step: "x", Payload: []byte(`"not an object"`)})
	if err == nil {
		t.Fatal("an undecodable payload was accepted")
	}
	if got, ok := ClassOf(err); !ok || got != ClassTerminalTechnical {
		t.Fatalf("class = %v (explicit: %v), want TERMINAL_TECHNICAL", got, ok)
	}
}

func TestInputStepOutputReadsAPredecessorsCheckpoint(t *testing.T) {
	t.Parallel()
	in := Input{Context: []byte(`{"submit-kyc":{"vendorCaseRef":"case_9"},"validate":null}`)}

	var out kycOut
	found, err := in.StepOutput("submit-kyc", &out)
	if err != nil || !found {
		t.Fatalf("StepOutput: found=%v err=%v", found, err)
	}
	if out.VendorRef != "case_9" {
		t.Fatalf("VendorRef = %q", out.VendorRef)
	}

	// A JSON null and a missing key are both "absent". Conflating absent with corrupt is how an
	// optional predecessor's missing output becomes a retry loop.
	if found, err := in.StepOutput("validate", &out); found || err != nil {
		t.Fatalf("a null checkpoint reported found=%v err=%v", found, err)
	}
	if found, err := in.StepOutput("nope", &out); found || err != nil {
		t.Fatalf("a missing checkpoint reported found=%v err=%v", found, err)
	}
}

func TestInputHooksAreNilSafe(t *testing.T) {
	t.Parallel()
	// A zero Input is what a unit test of an activity constructs by hand. It must not panic.
	var in Input
	if err := in.Heartbeat(context.Background(), nil); err != nil {
		t.Fatalf("Heartbeat on a zero Input: %v", err)
	}
	if err := in.Checkpoint(context.Background(), "k", []byte("1")); err != nil {
		t.Fatalf("Checkpoint on a zero Input: %v", err)
	}
	if _, ok, err := in.Lookup(context.Background(), "k"); ok || err != nil {
		t.Fatalf("Lookup on a zero Input: ok=%v err=%v", ok, err)
	}
}

func TestActivitiesRegistryRejectsDuplicates(t *testing.T) {
	t.Parallel()
	r := NewActivities()
	a := ActivityFunc{ActivityName: "one", Fn: func(context.Context, Input) (Output, error) { return nil, nil }}
	if err := r.Register(a); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Silently replacing a registered activity makes a step's behaviour depend on package
	// initialization order, which is not a thing anyone should have to debug.
	if err := r.Register(a); err == nil {
		t.Fatal("a duplicate registration was accepted")
	}
	if _, err := r.Get("missing"); !errors.Is(err, ErrUnknownActivity) {
		t.Fatalf("Get of an unregistered activity: %v", err)
	}
}

func TestVerifyDefinitionCatchesMissingActivitiesAtStartup(t *testing.T) {
	t.Parallel()
	r := NewActivities()
	r.MustRegister(ActivityFunc{ActivityName: "present", Fn: func(context.Context, Input) (Output, error) { return nil, nil }})

	def := &Definition{Name: "d", Version: 1, Steps: []Step{
		{Name: "a", Activity: "present", Compensation: "absent"},
	}}
	err := r.VerifyDefinition(def)
	if err == nil {
		t.Fatal("a definition naming a missing compensation was accepted; the first instance to abort would discover it")
	}
	var ae *apierror.Error
	if !errors.As(err, &ae) || len(ae.Details) == 0 {
		t.Fatalf("the error does not name what is missing: %v", err)
	}
}
