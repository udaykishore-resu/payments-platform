package onboarding

import (
	"context"
	"encoding/json"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/merchant"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/workflows/engine"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// The compensations, and the two properties every one of them has.
//
//  1. **Idempotent.** A compensation is retried with the same deterministic key, so a crash
//     mid-compensation is safe: re-running a completed one is a no-op at the vendor, not a
//     second delete.
//  2. **Tolerant of the forward operation never having happened.** Compensation runs after
//     crashes, and the crash may have happened before the thing being undone existed. Every
//     "not found" below is therefore success, not failure — treating it as failure would page
//     an engineer to look for an orphan that was never created, and, worse, would abort the
//     remaining compensations that *do* have work to do.
//
// They receive the step's checkpointed **output**, not its input, because undoing a provisioning
// step needs the external account reference the step produced. A compensation that only saw the
// input would have to re-derive or re-discover that reference, which is fragile in the ordinary
// case and impossible in the crash case.

func (d Deps) compensations() []engine.Activity {
	return []engine.Activity{
		engine.ActivityFunc{ActivityName: CompCancelKYCCase, Fn: d.cancelKYCCase},
		engine.ActivityFunc{ActivityName: CompDeprovisionGateways, Fn: d.deprovisionGateways},
		engine.ActivityFunc{ActivityName: CompDeleteSecrets, Fn: d.deleteSecretVersions},
		engine.ActivityFunc{ActivityName: CompDeleteWebhooks, Fn: d.deleteWebhookRegistrations},
		engine.ActivityFunc{ActivityName: CompRollbackConfig, Fn: d.rollbackConfiguration},
		engine.ActivityFunc{ActivityName: CompSuspendMerchant, Fn: d.suspendMerchant},
	}
}

// kycCompensationInput accepts either step 2's or step 3's output, because both steps declare
// this compensation and each carries the vendor reference under its own field name. Decoding
// both is cheaper and more honest than making the two steps agree on a shape they do not share.
type kycCompensationInput struct {
	VendorCaseRef string `json:"vendorCaseRef"`
	ProviderRef   string `json:"providerRef"`
	Decision      string `json:"decision"`
}

// cancelKYCCase stops a pending verification.
//
// It is meaningful only while the case is still *pending*. Once a decision has landed the
// vendor's record is retained for five years under a legal-obligation basis, and cancelling is
// neither possible nor desirable — which is why the engine skips this compensation entirely once
// the retained pivot has completed. The check below is the belt to that braces: if an operator
// replays this compensation by hand against a decided case, it must be a no-op rather than an
// error that looks like orphaned state.
func (d Deps) cancelKYCCase(ctx context.Context, in engine.Input) (engine.Output, error) {
	var payload kycCompensationInput
	if err := decodeInto(in.Payload, &payload); err != nil {
		return nil, err
	}
	ref := firstNonEmpty(payload.VendorCaseRef, payload.ProviderRef)
	if ref == "" {
		// Nothing was ever submitted. The step crashed before the vendor saw anything, and there
		// is genuinely nothing to cancel.
		return encode(map[string]any{"cancelled": false, "reason": "no vendor case reference was recorded"})
	}
	if payload.Decision != "" {
		return encode(map[string]any{"cancelled": false,
			"reason": "the case is already decided; the record is retained by law"})
	}

	if err := d.KYC.Cancel(ctx, ref); err != nil {
		if isNotFound(err) {
			return encode(map[string]any{"cancelled": false, "reason": "the vendor has no such case"})
		}
		return nil, err
	}
	return encode(map[string]any{"cancelled": true, "vendorCaseRef": ref})
}

// deprovisionGateways removes each sub-account, in reverse completion order.
//
// Reverse order within the branch list matters for the same reason it matters between steps: the
// last thing created is the one most likely to depend on the others. It also mirrors what an
// operator doing this by hand would do, which is what makes the runbook and the code agree.
func (d Deps) deprovisionGateways(ctx context.Context, in engine.Input) (engine.Output, error) {
	var payload ProvisionGatewaysOutput
	if err := decodeInto(in.Payload, &payload); err != nil {
		return nil, err
	}

	removed := make([]string, 0, len(payload.Connections))
	var failures []string
	for i := len(payload.Connections) - 1; i >= 0; i-- {
		conn := payload.Connections[i]
		if conn.ExternalAccountID == "" {
			continue
		}
		p, err := d.Gateways.Provisioner(conn.GatewayID)
		if err != nil {
			failures = append(failures, string(conn.GatewayID)+": "+err.Error())
			continue
		}
		if err := p.Deprovision(ctx, conn.ExternalAccountID); err != nil {
			if isNotFound(err) {
				removed = append(removed, string(conn.GatewayID))
				continue
			}
			// Reported rather than swallowed: a sub-account we believe is gone and is not is
			// exactly the orphan the compensation-failure alert exists for.
			failures = append(failures, string(conn.GatewayID)+": "+err.Error())
			continue
		}
		removed = append(removed, string(conn.GatewayID))
	}
	if len(failures) > 0 {
		return nil, apierror.Newf(apierror.CodeDependencyFailure,
			"could not de-provision %d of %d gateway sub-accounts: %v",
			len(failures), len(payload.Connections), failures)
	}
	return encode(map[string]any{"deprovisioned": removed})
}

// deleteSecretVersions schedules each stored credential for deletion.
//
// Scheduled deletion with a recovery window, never immediate destruction: a compensation that
// runs because of a transient failure, followed by an operator requeue an hour later, must not
// have irrecoverably destroyed the credential the requeue needs. The secrets store's own
// recovery window is what makes that survivable.
func (d Deps) deleteSecretVersions(ctx context.Context, in engine.Input) (engine.Output, error) {
	var payload StoreCredentialsOutput
	if err := decodeInto(in.Payload, &payload); err != nil {
		return nil, err
	}
	deleted := make([]string, 0, len(payload.Refs))
	var failures []string
	for i := len(payload.Refs) - 1; i >= 0; i-- {
		ref := payload.Refs[i]
		if ref.SecretRef == "" {
			continue
		}
		if err := d.Secrets.Delete(ctx, ref.SecretRef); err != nil {
			if isNotFound(err) {
				deleted = append(deleted, ref.SecretRef)
				continue
			}
			failures = append(failures, ref.SecretRef+": "+err.Error())
			continue
		}
		deleted = append(deleted, ref.SecretRef)
	}
	if len(failures) > 0 {
		return nil, apierror.Newf(apierror.CodeDependencyFailure,
			"could not delete %d secret version(s): %v", len(failures), failures)
	}
	return encode(map[string]any{"deleted": deleted})
}

// deleteWebhookRegistrations removes each subscription.
//
// This runs *before* de-provisioning, because the registration belongs to the sub-account:
// deleting the account first leaves the gateway rejecting the registration delete and the
// subscription orphaned. That ordering is the engine's strict-reverse-order guarantee doing its
// job, and it is the concrete reason the guarantee is "strict" rather than "roughly".
func (d Deps) deleteWebhookRegistrations(ctx context.Context, in engine.Input) (engine.Output, error) {
	var payload RegisterWebhooksOutput
	if err := decodeInto(in.Payload, &payload); err != nil {
		return nil, err
	}
	removed := make([]string, 0, len(payload.Registrations))
	var failures []string
	for i := len(payload.Registrations) - 1; i >= 0; i-- {
		reg := payload.Registrations[i]
		if reg.RegistrationRef == "" {
			continue
		}
		p, err := d.Gateways.Provisioner(reg.GatewayID)
		if err != nil {
			failures = append(failures, string(reg.GatewayID)+": "+err.Error())
			continue
		}
		if err := p.UnregisterWebhook(ctx, reg.ExternalAccountID, reg.RegistrationRef); err != nil {
			if isNotFound(err) {
				removed = append(removed, reg.RegistrationRef)
				continue
			}
			failures = append(failures, reg.RegistrationRef+": "+err.Error())
			continue
		}
		removed = append(removed, reg.RegistrationRef)
	}
	if len(failures) > 0 {
		return nil, apierror.Newf(apierror.CodeDependencyFailure,
			"could not delete %d webhook registration(s): %v", len(failures), failures)
	}
	return encode(map[string]any{"deleted": removed})
}

// rollbackConfiguration republishes the previous version.
//
// It is a *forward* operation over an append-only history: the rollback publishes a **new**
// version whose content is the old one's, rather than deleting the version that was published.
// Two reasons, both load-bearing. The audit trail must show that a rollback happened and who did
// it; and a "delete the bad version" implementation leaves every data-plane snapshot pointing at
// a version number that no longer exists.
func (d Deps) rollbackConfiguration(ctx context.Context, in engine.Input) (engine.Output, error) {
	var payload ApplyConfigurationOutput
	if err := decodeInto(in.Payload, &payload); err != nil {
		return nil, err
	}
	merchantID := shared.MerchantID(in.BusinessKey)
	if payload.ConfigVersion == 0 {
		return encode(map[string]any{"rolledBack": false, "reason": "no configuration version was published"})
	}
	if payload.PreviousVersion == 0 {
		// The onboarding published the merchant's first configuration. There is no earlier
		// version to restore, and deleting the only one would leave the merchant with no
		// configuration at all — which is worse than an unused one attached to a merchant that
		// never activated.
		return encode(map[string]any{"rolledBack": false,
			"reason": "this was the first configuration version; there is nothing to roll back to"})
	}

	current, err := d.Configs.GetActive(ctx, merchantID)
	if err != nil {
		if isNotFound(err) {
			return encode(map[string]any{"rolledBack": false, "reason": "no active configuration"})
		}
		return nil, err
	}
	if current.Version != payload.ConfigVersion {
		// Somebody has published since. Rolling back on top of their change would silently
		// revert work this saga knows nothing about.
		return encode(map[string]any{"rolledBack": false,
			"reason": "the active configuration has moved on; a manual review is required"})
	}
	target, err := d.Configs.GetVersion(ctx, merchantID, payload.PreviousVersion)
	if err != nil {
		return nil, err
	}
	next, err := current.RollbackTo(target, "onboarding-compensation", d.Clock.Now().UTC())
	if err != nil {
		return nil, err
	}
	if err := d.Configs.Publish(ctx, next, current.Version); err != nil {
		return nil, err
	}
	return encode(map[string]any{
		"rolledBack": true, "restoredFrom": payload.PreviousVersion, "newVersion": next.Version,
	})
}

// suspendMerchant is step 12's compensation, and it is **forward recovery, not rollback**.
//
// Once the merchant is ACTIVE, real payments can exist and each has a lifecycle that must
// complete. Suspension stops *new* payments while deliberately continuing to permit refunds,
// voids and webhook processing; "undoing" activation by blocking refunds would trap merchant
// money and convert a workflow problem into a consumer-harm one.
//
// The engine will not normally call this: it refuses to abort past an irreversible pivot and
// parks the instance for an operator instead. It exists so that an operator's explicit
// `workflow compensate --step activate` has a reviewed, tested implementation to run rather than
// an ad-hoc SQL statement.
func (d Deps) suspendMerchant(ctx context.Context, in engine.Input) (engine.Output, error) {
	merchantID := shared.MerchantID(in.BusinessKey)
	m, err := d.Merchants.Get(ctx, merchantID)
	if err != nil {
		if isNotFound(err) {
			return encode(map[string]any{"suspended": false, "reason": "the merchant no longer exists"})
		}
		return nil, err
	}
	if m.Status() != merchant.StatusActive {
		// Idempotent: a merchant that never activated, or that is already suspended, needs
		// nothing done to it.
		return encode(map[string]any{"suspended": false, "state": string(m.Status())})
	}
	if err := m.Suspend(merchant.SuspendOperatorAction,
		"onboarding was aborted after activation; suspended pending operator review", d.Clock); err != nil {
		return nil, err
	}
	if err := d.Merchants.Save(ctx, m); err != nil {
		return nil, err
	}
	return encode(map[string]any{
		"suspended": true, "at": d.Clock.Now().UTC().Format(time.RFC3339),
	})
}

// --- encoding helpers -----------------------------------------------------------------------------

func encode(v any) (engine.Output, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, apierror.Wrap(err, apierror.CodeInternalError, "a compensation result does not encode")
	}
	return b, nil
}

func decodeInto(raw []byte, v any) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return engine.WithClass(
			apierror.Wrapf(err, apierror.CodeMalformedRequest,
				"a checkpointed value does not decode into %T", v),
			engine.ClassTerminalTechnical)
	}
	return nil
}
