// Package observability wires up the three pillars (metrics, logs, traces) described in
// docs/06-observability.md. Every metric here corresponds directly to a row in that document's
// alerting table — this file and that doc are meant to be read side by side.
package observability

import "github.com/prometheus/client_golang/prometheus"

type Metrics struct {
	HTTPRequestsTotal    *prometheus.CounterVec
	HTTPRequestDuration  *prometheus.HistogramVec

	PaymentsCreatedTotal            *prometheus.CounterVec
	LedgerBalanceCheckFailuresTotal prometheus.Counter
	IdempotencyConflictsTotal       prometheus.Counter

	OutboxUnpublishedCount prometheus.Gauge
	OutboxPublishLatency   prometheus.Histogram

	DBPoolInUseConns prometheus.Gauge
	DBPoolIdleConns  prometheus.Gauge

	CircuitBreakerState *prometheus.GaugeVec // 0=closed, 1=open, 0.5=half-open
}

// NewMetrics constructs and registers every metric against reg. Called once at startup with a
// dedicated prometheus.Registry (not the global default registry), so tests can construct
// independent Metrics instances without global state collisions.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		HTTPRequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total HTTP requests processed, labeled by route, method, and status class.",
		}, []string{"route", "method", "status"}),

		HTTPRequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP request latency in seconds.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, .6, 1, 2.5, 5},
		}, []string{"route", "method"}),

		PaymentsCreatedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "payments_created_total",
			Help: "Total payments created, labeled by final status.",
		}, []string{"status"}),

		LedgerBalanceCheckFailuresTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "ledger_balance_check_failures_total",
			Help: "Count of detected unbalanced-ledger conditions. Should always be zero; any nonzero value is a page-immediately SEV-1 (see docs/08-runbook.md section 2).",
		}),

		IdempotencyConflictsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "idempotency_conflicts_total",
			Help: "Count of requests reusing an Idempotency-Key with a different request body (likely client bug).",
		}),

		OutboxUnpublishedCount: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "outbox_unpublished_count",
			Help: "Current count of outbox_events rows not yet published downstream.",
		}),

		OutboxPublishLatency: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "outbox_publish_latency_seconds",
			Help:    "Time from outbox row creation (DB commit) to successful publish to SNS/SQS.",
			Buckets: prometheus.DefBuckets,
		}),

		DBPoolInUseConns: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "db_pool_in_use_connections",
			Help: "Number of database connections currently checked out of the pool.",
		}),
		DBPoolIdleConns: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "db_pool_idle_connections",
			Help: "Number of idle database connections in the pool.",
		}),

		CircuitBreakerState: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "circuit_breaker_state",
			Help: "Circuit breaker state per dependency: 0=closed, 0.5=half-open, 1=open.",
		}, []string{"dependency"}),
	}

	reg.MustRegister(
		m.HTTPRequestsTotal,
		m.HTTPRequestDuration,
		m.PaymentsCreatedTotal,
		m.LedgerBalanceCheckFailuresTotal,
		m.IdempotencyConflictsTotal,
		m.OutboxUnpublishedCount,
		m.OutboxPublishLatency,
		m.DBPoolInUseConns,
		m.DBPoolIdleConns,
		m.CircuitBreakerState,
	)
	return m
}
