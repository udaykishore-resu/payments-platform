package postgres_test

import (
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/workflows/engine"
	"github.com/udaykishore-resu/payments-platform/internal/workflows/engine/enginetest"
	"github.com/udaykishore-resu/payments-platform/internal/workflows/engine/postgres"
)

// TestPostgresEngineSatisfiesTheContract runs the shared behavioural contract against this
// implementation.
//
// The suite lives in enginetest and is owned by neither engine, which is the point: a contract
// owned by one implementation drifts towards whatever that implementation happens to do, and the
// second one is then judged against the first one's accidents. The Temporal adapter — a nested
// module under engine/temporal — runs exactly these cases against exactly this suite.
func TestPostgresEngineSatisfiesTheContract(t *testing.T) {
	t.Parallel()
	enginetest.EngineContractSuite(t, func() engine.Engine {
		clock := enginetest.NewClock(time.Date(2026, time.March, 2, 9, 0, 0, 0, time.UTC))
		repo := enginetest.NewRepo(clock)
		eng, err := postgres.New(postgres.Options{
			Repo:        repo,
			Activities:  enginetest.ContractActivities(),
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
