package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/gateway"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// HealthRepository persists gateway circuit state.
//
// It exists for one failure mode: a restarting pod that begins life believing every gateway is
// perfectly healthy. During an outage, a rolling restart of nine orchestrator pods would then
// send a synchronised burst at a gateway that is already down — the thundering herd arriving
// precisely when the gateway can least handle it, and precisely when the circuit that would have
// stopped it has just been forgotten.
//
// What is persisted is the circuit *position* — state, cooldown, deadline — and not the sliding
// window. The window describes the last thirty seconds; a process that has just started has not
// observed anything, and restoring a window written minutes ago would have the new process make
// admission decisions about traffic it never saw.
//
// The row carries no tenant_id, because health is per (gateway, operation) and never per
// merchant: a per-merchant sample is too sparse to be statistically meaningful, and a gateway
// that is down is down for everyone.
type HealthRepository struct {
	q     querier
	clock shared.Clock
}

var _ ports.HealthRepository = (*HealthRepository)(nil)

const selectHealth = `
SELECT gateway_id, operation, state, cooldown_seconds, cooldown_until,
       consecutive_probe_successes, last_observed_at, state_changed_at, version
FROM pp.gateway_health`

// Get loads the circuit state for one (gateway, operation).
func (r *HealthRepository) Get(
	ctx context.Context, g shared.GatewayID, op shared.Operation,
) (*gateway.Health, error) {
	if _, err := tenantOf(ctx); err != nil {
		return nil, err
	}
	h, err := r.scan(r.q.QueryRow(ctx, selectHealth+
		" WHERE gateway_id = $1 AND operation = $2", g.String(), string(op)))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, notFound(apierror.CodeGatewayNotConfigured,
				"gateway health", g.String()+"/"+string(op))
		}
		return nil, mapError(err, "get gateway health")
	}
	return h, nil
}

// ListAll returns every (gateway, operation) circuit, for the routing engine's warm start and
// for the operator dashboard.
func (r *HealthRepository) ListAll(ctx context.Context) ([]*gateway.Health, error) {
	if _, err := tenantOf(ctx); err != nil {
		return nil, err
	}
	rows, err := r.q.Query(ctx, selectHealth+" ORDER BY gateway_id, operation")
	if err != nil {
		return nil, mapError(err, "list gateway health")
	}
	defer rows.Close()
	var out []*gateway.Health
	for rows.Next() {
		h, err := r.scan(rows)
		if err != nil {
			return nil, mapError(err, "list gateway health")
		}
		out = append(out, h)
	}
	return out, mapError(rows.Err(), "list gateway health")
}

// Save upserts the circuit state.
//
// It is called on a state *change*, not on every sample. Writing every sample would be a
// 5 000 TPS write amplifier against a row that only matters when it moves — and the write
// amplification would land on the same writer instance the payments go through.
//
// The version guard is a plain `>=` rather than an exact match: two pods can legitimately
// observe the same transition at almost the same instant, and rejecting the second one would
// produce a conflict error on a path that has no caller to report it to. What must not happen is
// an *older* observation overwriting a newer one, and that is what the comparison prevents.
func (r *HealthRepository) Save(ctx context.Context, h *gateway.Health) error {
	if _, err := tenantOf(ctx); err != nil {
		return err
	}
	const q = `
INSERT INTO pp.gateway_health (
    gateway_id, operation, state, error_rate, p99_latency_ms, sample_count,
    cooldown_seconds, cooldown_until, consecutive_probe_successes,
    last_observed_at, state_changed_at, version)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
ON CONFLICT (gateway_id, operation) DO UPDATE SET
    state                       = EXCLUDED.state,
    error_rate                  = EXCLUDED.error_rate,
    p99_latency_ms              = EXCLUDED.p99_latency_ms,
    sample_count                = EXCLUDED.sample_count,
    cooldown_seconds            = EXCLUDED.cooldown_seconds,
    cooldown_until              = EXCLUDED.cooldown_until,
    consecutive_probe_successes = EXCLUDED.consecutive_probe_successes,
    last_observed_at            = EXCLUDED.last_observed_at,
    state_changed_at            = EXCLUDED.state_changed_at,
    version                     = EXCLUDED.version
WHERE EXCLUDED.version >= pp.gateway_health.version`

	total, _, _, _ := h.Counters()
	cooldown := int(h.Cooldown() / time.Second)
	if cooldown < 30 {
		cooldown = 30
	}
	if cooldown > 300 {
		cooldown = 300
	}

	if _, err := r.q.Exec(ctx, q,
		h.GatewayID().String(), string(h.Operation()), string(h.State()),
		clampRate(h.ErrorRate()), int(h.P99Latency()/time.Millisecond), total,
		cooldown, nullableTime(h.CooldownUntil()), h.ConsecutiveProbeSuccesses(),
		nullableTime(h.LastObservedAt()), h.LastChangedAt(), int64(h.Version()),
	); err != nil {
		return mapError(err, "save gateway health")
	}
	return nil
}

func (r *HealthRepository) scan(row scanRow) (*gateway.Health, error) {
	var (
		p               gateway.RehydrateHealthParams
		gw, op, state   string
		cooldownSeconds int
		cooldownUntil   *time.Time
		lastObserved    *time.Time
		version         int64
	)
	if err := row.Scan(&gw, &op, &state, &cooldownSeconds, &cooldownUntil,
		&p.ConsecutiveProbeSuccesses, &lastObserved, &p.LastChangedAt, &version); err != nil {
		return nil, err
	}
	p.GatewayID = shared.GatewayID(gw)
	p.Operation = shared.Operation(op)
	p.State = gateway.HealthState(state)
	p.Cooldown = time.Duration(cooldownSeconds) * time.Second
	p.Version = shared.Version(version)
	if cooldownUntil != nil {
		p.CooldownUntil = *cooldownUntil
	}
	if lastObserved != nil {
		p.LastObservedAt = *lastObserved
	}
	return gateway.RehydrateHealth(p, r.clock)
}

// clampRate keeps the persisted error rate inside the column's CHECK.
//
// A rate outside [0,1] means the window computed something impossible — a divide by a stale
// sample count, most likely. Clamping and storing is better than failing the write: the health
// row is not the authority (the in-process window is), and refusing to record a circuit opening
// because the rate was 1.0000001 would lose the one fact on the row that matters.
func clampRate(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 1:
		return 1
	default:
		return v
	}
}

func nullableTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}
