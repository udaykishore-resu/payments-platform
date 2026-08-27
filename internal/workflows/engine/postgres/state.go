package postgres

import (
	"encoding/json"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/workflows/engine"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// instanceContext is the decoded form of `workflow_instances.context`.
//
// The column is the accumulated output of completed steps, and that is precisely what makes
// resume *replay-free*: a worker taking over an instance reads this one document and runs the
// next step. It does not fold an event history, it does not re-execute anything, and it
// therefore imposes no determinism constraints on activity code — which is the single biggest
// design difference between this engine and Temporal (docs/automation-plane.md §1.7).
//
// Signals, the cancellation flag and the intra-activity progress checkpoints live here too
// rather than in separate tables, because ports.WorkflowRepository — the narrow contract the
// application declares — has no method for them. The shapes below preserve the properties the
// separate tables would have given: a signal is keyed by name, so a duplicate vendor webhook is
// a no-op; and a signal may be recorded with no wait active, so a signal that races ahead of
// the wait is not lost. Losing a racing signal is the classic bug here, and the KYC vendor's
// webhook routinely beats our own commit.
type instanceContext struct {
	// Steps maps step name to that step's checkpointed output. It is the document activities
	// read their predecessors' results from.
	Steps map[string]json.RawMessage `json:"steps,omitempty"`

	// Signals maps signal name to the delivery. At most one per name, ever.
	Signals map[string]signalRecord `json:"signals,omitempty"`

	// Progress maps step name to that step's intra-activity checkpoints, so a fan-out that
	// crashed after two of four branches resumes at branch three.
	Progress map[string]map[string]json.RawMessage `json:"progress,omitempty"`

	// Cancel records a cooperative cancellation request.
	Cancel *cancelRecord `json:"cancel,omitempty"`

	Meta metaRecord `json:"meta"`
}

// metaRecord is the engine's own bookkeeping.
type metaRecord struct {
	// CrashCount is incremented at lease acquisition, *before* any activity runs, and reset to
	// zero on any clean step completion.
	//
	// Incrementing before execution is the entire mechanism: an instance that kills its worker
	// — an OOM, a panic that escapes recover, a wedged driver — never reaches the code that
	// would record a failure, because that code died with the worker. A counter incremented at
	// acquisition is written by the *previous* statement, so it survives. Three acquisitions
	// without progress quarantines the instance, which bounds the blast radius of a poison
	// instance to three worker deaths instead of an indefinite cycle through the fleet.
	CrashCount int `json:"crashCount,omitempty"`

	// LookupFirst is set when the previous attempt ended ambiguously, and instructs the next
	// attempt to query the external system for its own prior effect before acting.
	LookupFirst bool `json:"lookupFirst,omitempty"`

	// Compensations records each step's compensation outcome, so a crash mid-compensation
	// resumes at the right place and a completed compensation is not re-run needlessly.
	Compensations map[string]string `json:"compensations,omitempty"`

	// AbortCause distinguishes a cancellation from a failure. They compensate identically and
	// end differently: a cancelled instance reaches CANCELED, a failed one reaches FAILED with
	// a DLQ entry, and conflating them makes the operator surface lie about why a merchant's
	// onboarding stopped.
	AbortCause string `json:"abortCause,omitempty"`

	// Failure is the classified reason the instance stopped.
	Failure *engine.FailureRecord `json:"failure,omitempty"`

	// ParkReason explains a PARKED instance to the human who has to act on it.
	ParkReason string `json:"parkReason,omitempty"`

	// WaitingFor names the signal a manual gate has already begun waiting for.
	//
	// It exists to distinguish "we are entering this gate now" from "the gate's deadline has
	// arrived and nobody signalled", which are the same code path reached with the same
	// instance state and are treated completely differently: one releases the lease and waits,
	// the other parks the instance and raises an escalation. Inferring the difference from the
	// clock alone would misread a lease acquired in the same millisecond as the gate opened.
	WaitingFor string `json:"waitingFor,omitempty"`
}

// Abort causes.
const (
	abortFailure = "FAILURE"
	abortCancel  = "CANCEL"
)

// signalRecord is one durable signal delivery, with everything the audit trail needs.
type signalRecord struct {
	Data           json.RawMessage `json:"data,omitempty"`
	Principal      string          `json:"principal"`
	Scopes         []string        `json:"scopes,omitempty"`
	Reason         string          `json:"reason,omitempty"`
	SourceIP       string          `json:"sourceIp,omitempty"`
	IdempotencyKey string          `json:"idempotencyKey,omitempty"`
	ReceivedAt     time.Time       `json:"receivedAt"`
	// ConsumedAt is set in the same write that advances the step past the gate, giving
	// at-most-once consumption.
	ConsumedAt *time.Time `json:"consumedAt,omitempty"`
}

type cancelRecord struct {
	Requested bool      `json:"requested"`
	Reason    string    `json:"reason,omitempty"`
	Actor     string    `json:"actor,omitempty"`
	At        time.Time `json:"at"`
}

func decodeContext(raw []byte) (*instanceContext, error) {
	c := &instanceContext{}
	if len(raw) == 0 {
		return c, nil
	}
	if err := json.Unmarshal(raw, c); err != nil {
		// A context column that does not decode is not something to guess around: the instance's
		// entire memory of what it has already done lives there, and running the next step on a
		// corrupt one risks repeating a side effect.
		return nil, apierror.Wrap(err, apierror.CodeInternalError,
			"the workflow instance context is not readable by this binary")
	}
	return c, nil
}

func (c *instanceContext) encode() ([]byte, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return nil, apierror.Wrap(err, apierror.CodeInternalError, "the workflow context does not encode")
	}
	return b, nil
}

// stepsJSON renders just the completed-step outputs, which is what an activity is given as its
// Input.Context. The engine's own bookkeeping — crash counts, signals, cancellation — is
// deliberately withheld: an activity that can read the cancellation flag is an activity that
// will eventually branch on it, and cancellation is the engine's decision to make.
func (c *instanceContext) stepsJSON() []byte {
	if len(c.Steps) == 0 {
		return []byte("{}")
	}
	b, err := json.Marshal(c.Steps)
	if err != nil {
		return []byte("{}")
	}
	return b
}

func (c *instanceContext) putStep(name string, out engine.Output) {
	if c.Steps == nil {
		c.Steps = make(map[string]json.RawMessage, 8)
	}
	if len(out) == 0 {
		c.Steps[name] = json.RawMessage("null")
		return
	}
	c.Steps[name] = json.RawMessage(append([]byte(nil), out...))
}

func (c *instanceContext) putProgress(step, key string, value []byte) {
	if c.Progress == nil {
		c.Progress = make(map[string]map[string]json.RawMessage, 2)
	}
	if c.Progress[step] == nil {
		c.Progress[step] = make(map[string]json.RawMessage, 4)
	}
	c.Progress[step][key] = json.RawMessage(append([]byte(nil), value...))
}

func (c *instanceContext) progress(step, key string) ([]byte, bool) {
	m, ok := c.Progress[step]
	if !ok {
		return nil, false
	}
	v, ok := m[key]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), v...), true
}

func (c *instanceContext) markCompensation(step, state string) {
	if c.Meta.Compensations == nil {
		c.Meta.Compensations = make(map[string]string, 8)
	}
	c.Meta.Compensations[step] = state
}

// cancelRequested reports whether an operator has asked for cooperative cancellation.
func (c *instanceContext) cancelRequested() bool {
	return c.Cancel != nil && c.Cancel.Requested
}
