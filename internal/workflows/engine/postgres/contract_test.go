package postgres_test

import (
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/workflows/engine"
	"github.com/udaykishore-resu/payments-platform/internal/workflows/engine/enginetest"
	"github.com/udaykishore-resu/payments-platform/internal/workflows/engine/postgres"
	"github.com/udaykishore-resu/payments-platform/internal/workflows/engine/temporal"
)

// TestPostgresEngineSatisfiesTheContract runs the shared behavioural contract against this
// implementation.
//
// The suite lives in the temporal package and is owned by neither engine, which is the point: a
// contract owned by one implementation drifts towards whatever that implementation happens to
// do, and the second one is then judged against the first one's accidents. When the Temporal
// adapter is built with `-tags temporal`, it runs exactly these cases.
func TestPostgresEngineSatisfiesTheContract(t *testing.T) {
	t.Parallel()
	temporal.EngineContractSuite(t, func() engine.Engine {
		clock := enginetest.NewClock(time.Date(2026, time.March, 2, 9, 0, 0, 0, time.UTC))
		repo := enginetest.NewRepo(clock)
		eng, err := postgres.New(postgres.Options{
			Repo:        repo,
			Activities:  temporal.ContractActivities(),
			Definitions: engine.NewRegistry(),
			Clock:       clock,
			WorkerID:    "contract-worker",
			Salt:        []byte("contract"),
			Logger:      discardLogger(),
			// Real ULIDs: the contract's journal is keyed by instance ID, and a per-harness
			// counter would collide across the suite's parallel subtests.
			NewID:  shared.NewWorkflowID,
			Tenant: nil,
		})
		if err != nil {
			t.Fatalf("building the contract engine: %v", err)
		}
		return eng
	})
}
