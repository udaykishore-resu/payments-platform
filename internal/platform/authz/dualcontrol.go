package authz

import (
	"context"
	"sync"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// Approval is a second person's authorization for one specific pending operation.
//
// The four fields that make it a control rather than a formality:
//
//   - RequesterID and ApproverID must differ. Without that check the "two-person rule" is a
//     one-person rule with extra steps, and it is the single thing an attacker with one
//     compromised session would try first.
//   - RequestFingerprint binds the approval to the exact request. An approval for a £10 refund
//     must not authorize a £10 000 one, and without the binding it would: the approver sees one
//     request, the requester submits another.
//   - ExpiresAt bounds how long an approval remains usable. A standing approval is a credential,
//     and one nobody remembers granting is the worst kind.
//   - TenantID scopes it. An approval is a tenant-scoped record like everything else, so an
//     approval reference from one tenant cannot satisfy a request in another.
type Approval struct {
	Ref                string
	TenantID           shared.TenantID
	Permission         Permission
	RequesterID        string
	ApproverID         string
	RequestFingerprint string
	Reason             string
	RequestedAt        time.Time
	ApprovedAt         time.Time
	ExpiresAt          time.Time
}

// Approved reports whether a second person has actually approved. A pending record with no
// approver is not an approval, and treating it as one would make the control satisfiable by the
// requester alone.
func (a Approval) Approved() bool { return a.ApproverID != "" }

// Expired reports whether the approval is past its window.
func (a Approval) Expired(now time.Time) bool {
	return a.ExpiresAt.IsZero() || !now.Before(a.ExpiresAt)
}

// ApprovalStore looks up pending approvals.
//
// One method, because evaluation only ever reads. Creating and approving are control-plane
// operations with their own authorization, their own audit records and their own endpoints; if
// they were on this interface, the policy engine would hold a handle capable of minting the
// approval it is about to check.
type ApprovalStore interface {
	Lookup(ctx context.Context, ref string) (*Approval, error)
}

// DualControlResult is why a dual-control check failed, or that it passed.
type DualControlResult string

// The dual-control outcomes. Each is a distinct denial reason so an operator can tell "you need
// an approval" from "you cannot approve your own request" without reading the code.
const (
	DualControlOK           DualControlResult = "OK"
	DualControlRequired     DualControlResult = "DUAL_CONTROL_REQUIRED"
	DualControlSelfApproval DualControlResult = "DUAL_CONTROL_SELF_APPROVAL"
	DualControlStale        DualControlResult = "DUAL_CONTROL_STALE_OR_MISMATCHED"
	DualControlWrongTenant  DualControlResult = "DUAL_CONTROL_WRONG_TENANT"
)

// CheckDualControl validates an approval against the request it is meant to authorize.
//
// It is a free function rather than a method so that the whole rule is one readable block that a
// reviewer can check against the policy document, and so that a store failure is handled in one
// place: an unreachable approvals store denies. Authorization is the one place this platform is
// deliberately not fail-static — a fail-static authorization decision is called "fail open".
func CheckDualControl(ctx context.Context, store ApprovalStore, req Request, tenant shared.TenantID, now time.Time) DualControlResult {
	if store == nil || req.ApprovalRef == "" {
		return DualControlRequired
	}
	appr, err := store.Lookup(ctx, req.ApprovalRef)
	if err != nil || appr == nil || !appr.Approved() {
		return DualControlRequired
	}
	if appr.TenantID != tenant {
		return DualControlWrongTenant
	}
	// The self-approval check, which is the whole point of the rule. It is also enforced at the
	// storage layer by a CHECK (approver_id <> requester_id) constraint, because a control that
	// exists in exactly one place is a control that one refactor removes.
	if req.Principal != nil && appr.ApproverID == req.Principal.ID {
		return DualControlSelfApproval
	}
	if appr.ApproverID == appr.RequesterID {
		return DualControlSelfApproval
	}
	if appr.Expired(now) || appr.Permission != req.Permission || appr.RequestFingerprint != req.Fingerprint {
		return DualControlStale
	}
	return DualControlOK
}

// MemoryApprovals is an in-memory ApprovalStore.
//
// It exists for tests and for a single-process tool; production uses the Postgres-backed store,
// whose CHECK constraint is the second enforcement point for the self-approval rule. The
// semantics implemented here are the ones the database enforces, so a test against this store is
// a meaningful test of the rule.
type MemoryApprovals struct {
	mu   sync.RWMutex
	byID map[string]*Approval
}

// NewMemoryApprovals builds an empty store.
func NewMemoryApprovals() *MemoryApprovals {
	return &MemoryApprovals{byID: map[string]*Approval{}}
}

// Request records a pending approval. The record exists before anyone approves it, so that the
// approver has something to look at and so that "who asked for this" survives even if the
// request is never approved.
func (m *MemoryApprovals) Request(a Approval) error {
	switch {
	case a.Ref == "":
		return apierror.New(apierror.CodeValidationFailed, "an approval requires a reference")
	case a.TenantID.IsZero():
		return apierror.New(apierror.CodeMissingTenantContext, "an approval must be tenant-scoped")
	case a.RequesterID == "":
		return apierror.New(apierror.CodeValidationFailed, "an approval requires a requester")
	case a.RequestFingerprint == "":
		return apierror.New(apierror.CodeValidationFailed,
			"an approval must be bound to the fingerprint of the request it authorizes")
	case a.ExpiresAt.IsZero():
		return apierror.New(apierror.CodeValidationFailed, "an approval must expire")
	case a.ApproverID != "":
		// A record that arrives already approved has not been through two people.
		return apierror.New(apierror.CodeValidationFailed,
			"an approval request must not carry an approver")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := a
	m.byID[a.Ref] = &cp
	return nil
}

// Approve records a second person's approval.
//
// The self-approval refusal lives here as well as in CheckDualControl on purpose. Rejecting at
// approval time gives the human immediate, comprehensible feedback; rejecting at evaluation time
// catches a record that was created some other way. Two checks, two failure modes, one rule.
func (m *MemoryApprovals) Approve(ref, approverID string, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.byID[ref]
	if !ok {
		return apierror.New(apierror.CodeValidationFailed, "no such approval request")
	}
	if approverID == "" {
		return apierror.New(apierror.CodeValidationFailed, "an approval requires an approver")
	}
	if approverID == a.RequesterID {
		return apierror.New(apierror.CodeForbidden,
			"the approver of an operation must not be its requester")
	}
	if a.Expired(now) {
		return apierror.New(apierror.CodeValidationFailed, "this approval request has expired")
	}
	a.ApproverID = approverID
	a.ApprovedAt = now
	return nil
}

// Lookup returns a copy of the approval, or nil.
func (m *MemoryApprovals) Lookup(_ context.Context, ref string) (*Approval, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	a, ok := m.byID[ref]
	if !ok {
		return nil, nil //nolint:nilnil // absence is not an error here: the caller branches on a nil approval to mean "no such reference", which is what a dual-control check asks
	}
	cp := *a
	return &cp, nil
}

// Purge removes approvals that expired before the given instant, so the store does not grow
// without bound. Run by a scheduled job, never inline.
func (m *MemoryApprovals) Purge(before time.Time) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for ref, a := range m.byID {
		if a.Expired(before) {
			delete(m.byID, ref)
			n++
		}
	}
	return n
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: NFR-34.
//
// Separation of duties for the operations that require a second, distinct approver
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
