//go:build integration

package integration

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/postgres"
	"github.com/udaykishore-resu/payments-platform/internal/workflows/onboarding"
	"github.com/udaykishore-resu/payments-platform/tests/testenv"
)

// Workflow resume after a worker is killed mid-step.
//
// Verifies: baseline §11 (owned workflow engine, lease, fencing epoch, checkpoint-before-advance),
// ADR-014, docs/testing.md §5.4 and FS-8, chaos scenario C-13.
//
// The in-process execution semantics — retry policy, compensation ordering, manual gates — are
// covered by internal/workflows/engine/postgres's own suite against an in-memory repository. What
// that suite cannot cover, and what this file exists for, is the durable half: whether the
// PostgreSQL statements that implement the lease, the fencing token and the checkpoint actually
// hold when two workers race on a real database.
//
// The crash modelled is a SIGKILL, not a shutdown: the lease is left held with its deadline in the
// past and no release, which is exactly what a `kubectl delete pod --grace-period=0` produces. A
// graceful shutdown releases the lease and is the easy case.
//
// The scenario is run at every one of the twelve onboarding steps, because the interesting failures
// are not uniform across them: a crash before `provision-gateways` costs nothing, a crash during it
// risks a second sub-account at the gateway, and a crash during `activate` risks a merchant that is
// live in one system and not in another.

// stepNames returns the onboarding definition's steps, in order, from the real definition.
//
// Reading them from the definition rather than transcribing them is what makes this test notice a
// thirteenth step: it fails on the count assertion below, which is the moment someone has to decide
// what a crash during the new step means.
func stepNames(t *testing.T) []string {
	t.Helper()
	def := onboarding.Definition()
	if def == nil {
		t.Fatal("the onboarding definition is nil")
	}
	out := make([]string, 0, len(def.Steps))
	for _, s := range def.Steps {
		out = append(out, s.Name)
	}
	return out
}

// crashedInstance is one instance seeded mid-flight, with its lease already expired.
type crashedInstance struct {
	id       shared.WorkflowID
	epoch    int64
	upTo     int // the number of steps already SUCCEEDED
	sideFxOf map[string]int
}

// seedCrashedInstance writes an instance that was killed while executing steps[upTo].
//
// The steps before it are SUCCEEDED with a recorded side effect each; the crashed step has no row
// at all, because the engine checkpoints a step's result *before* the next one begins and a
// process killed mid-activity never got that far. Seeding a half-written row instead would be
// modelling a state the engine cannot produce.
func seedCrashedInstance(
	t *testing.T, uow *postgres.UnitOfWork, s *testenv.Scope, tenant string, steps []string, upTo int, tag string,
) *crashedInstance {
	t.Helper()
	inst := shared.WorkflowID(s.ID(testenv.PrefixWorkflow, "resume/"+runToken+"/"+tag))
	now := s.Clock.Now()
	ci := &crashedInstance{id: inst, upTo: upTo, sideFxOf: map[string]int{}}

	// The instance, its completed steps and their side effects are written with raw SQL rather
	// than through the repository, for the reason recorded in repos_test.go: the repository's
	// writes into jsonb columns are broken under the pool's exec mode. Everything this test
	// actually asserts — LeaseRunnable, Heartbeat, GetInstance, ListSteps and the uniqueness
	// constraints — is the production code path.
	//
	// The kill is raw SQL for a better reason: there is no repository method for "die", and
	// production has none either. The lease stays owned with its deadline in the past and nothing
	// releases it, which is what `kubectl delete pod --grace-period=0` leaves behind.
	c := ctx(t)
	if err := s.TenantedCommitted(c, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(c, `
INSERT INTO pp.workflow_instances (
    instance_id, tenant_id, workflow_name, workflow_version, business_key, state, current_step,
    input, checkpoint, lease_owner, lease_expires_at, attempt_epoch, run_after,
    created_at, updated_at)
VALUES ($1,$2,$3,$4,$5,'RUNNING',$6,$7,'{}'::jsonb,'worker-killed',
        now() - interval '1 minute',1,$8,$8,$8)`,
			inst.String(), tenant, onboarding.WorkflowName, onboarding.WorkflowVersion,
			"resume-"+runToken+"-"+tag, steps[upTo], []byte(`{"probe":"resume"}`), now); err != nil {
			return fmt.Errorf("create instance: %w", err)
		}

		for i := 0; i < upTo; i++ {
			done := now.Add(time.Duration(i) * time.Second)
			if _, err := tx.Exec(c, `
INSERT INTO pp.workflow_steps (step_id, instance_id, tenant_id, name, sequence, state, attempt,
                               started_at, completed_at)
VALUES ($1,$2,$3,$4,$5,'SUCCEEDED',1,$6,$6)`,
				s.ID(testenv.PrefixStep, fmt.Sprintf("resume/%s/%s/%d", runToken, tag, i)),
				inst.String(), tenant, steps[i], i, done); err != nil {
				return fmt.Errorf("checkpoint step %s: %w", steps[i], err)
			}
			if err := recordSideEffect(c, tx, s, tenant, inst, steps[i], tag); err != nil {
				return fmt.Errorf("record the side effect of %s: %w", steps[i], err)
			}
			ci.sideFxOf[steps[i]]++
		}
		return nil
	}); err != nil {
		t.Fatalf("seed a crashed instance: %v", err)
	}
	inTx(t, uow, tenant, func(ctx context.Context, r ports.Repositories) error {
		rec, err := r.Workflows.GetInstance(ctx, inst)
		if err != nil {
			return err
		}
		ci.epoch = rec.LeaseEpoch
		return nil
	})
	return ci
}

// recordSideEffect stands in for whatever the step actually does at a gateway or a KYC provider.
//
// An outbox row is the right stand-in rather than a counter in memory: the property under test is
// that a resumed workflow does not repeat an effect that has already *committed*, and only a
// durable effect can be committed. A counter would reset with the process, which is exactly the
// assumption the test is trying to disprove.
func recordSideEffect(
	ctx context.Context, tx pgx.Tx, s *testenv.Scope, tenant string,
	inst shared.WorkflowID, step, tag string,
) error {
	id := s.IDAt(testenv.PrefixEvent, s.Clock.Now(),
		fmt.Sprintf("resume/%s/%s/%s", runToken, tag, step))
	_, err := tx.Exec(ctx, `
INSERT INTO pp.outbox_events (
    event_id, tenant_id, aggregate_type, aggregate_id, event_type, topic, partition_key,
    payload, headers, occurred_at, available_at)
VALUES ($1,$2,'merchant',$3,'merchant.gateway_provisioned.v1','pp.merchants.merchant.v1',$3,
        $4,'{}'::jsonb,$5,now())`,
		id, tenant, inst.String(),
		[]byte(fmt.Sprintf(`{"instance":%q,"step":%q}`, inst, step)), s.Clock.Now())
	return err
}

// TestWorkerCrashAtEveryOnboardingStepResumesWithoutRepeatingWork is FS-8(a) and C-13.
//
// For each of the twelve steps: kill a worker mid-step, let four workers race to take the instance
// over, and assert the four things that together mean "resumed correctly".
//
// The subtests are deliberately **not** parallel. LeaseRunnable claims every runnable instance of
// the tenant, so two subtests running at once would each take the other's work — which is exactly
// the behaviour the production worker wants and exactly the wrong thing for an assertion about one
// instance.
func TestWorkerCrashAtEveryOnboardingStepResumesWithoutRepeatingWork(t *testing.T) {
	t.Parallel()
	_, s := setup(t)
	uow := newUoW(t, shared.SystemClock{})

	steps := stepNames(t)
	if len(steps) != 12 {
		t.Fatalf("the onboarding definition has %d steps, want the 12 of baseline §11: %v.\n"+
			"If a step was added, decide what a crash during it costs and extend this test rather "+
			"than relaxing the count.", len(steps), steps)
	}

	for k, step := range steps {
		t.Run(fmt.Sprintf("%02d_%s", k, step), func(t *testing.T) {
			ci := seedCrashedInstance(t, uow, s, s.TenantA, steps, k, fmt.Sprintf("s%02d", k))

			// 1. Takeover is exclusive. Four workers poll at once; exactly one may get it.
			winners := raceForLease(t, uow, s.TenantA, ci.id, 4)
			if len(winners) != 1 {
				t.Fatalf("%d workers leased the same instance (%v). Two workers advancing one "+
					"workflow is how a step's side effect happens twice.", len(winners), winners)
			}
			winner := winners[0]

			// 2. The fencing epoch advanced, and the dead worker is fenced out. A lease deadline
			//    alone only stops a *polite* worker; a process that paused past its lease and woke
			//    up still believes it holds the instance.
			var newEpoch int64
			inTx(t, uow, s.TenantA, func(ctx context.Context, r ports.Repositories) error {
				rec, err := r.Workflows.GetInstance(ctx, ci.id)
				if err != nil {
					return err
				}
				newEpoch = rec.LeaseEpoch
				if rec.LeaseOwner != winner {
					t.Fatalf("the instance is leased by %q but %q won the race", rec.LeaseOwner, winner)
				}
				return nil
			})
			if newEpoch != ci.epoch+1 {
				t.Fatalf("attempt_epoch went %d -> %d on takeover, want exactly one increment",
					ci.epoch, newEpoch)
			}
			if err := tryTx(uow, s.TenantA, func(ctx context.Context, r ports.Repositories) error {
				return r.Workflows.Heartbeat(ctx, ci.id, "worker-killed", ci.epoch, time.Minute)
			}); err == nil {
				t.Fatal("the killed worker extended a lease it no longer holds. It would go on to " +
					"finish its step and apply a side effect on behalf of an instance another " +
					"worker is already advancing.")
			}

			// 3. No completed step was reset or re-opened by the takeover.
			completed := completedSteps(t, uow, s.TenantA, ci.id)
			if len(completed) != k {
				t.Fatalf("after takeover the instance has %d completed steps, want %d: %v",
					len(completed), k, completed)
			}
			for i := 0; i < k; i++ {
				if completed[i] != steps[i] {
					t.Fatalf("completed step %d is %q, want %q; the checkpoint was rewritten",
						i, completed[i], steps[i])
				}
			}

			// 4. The database refuses to record a second execution of an already-completed step at
			//    the same attempt. This is the guarantee that survives a bug in the resume logic:
			//    even a worker that ignored the checkpoint entirely cannot double-record.
			if k > 0 {
				dupCtx := ctx(t)
				err := s.Tenanted(dupCtx, s.TenantA, func(tx pgx.Tx) error {
					_, err := tx.Exec(dupCtx, `
INSERT INTO pp.workflow_steps (step_id, instance_id, tenant_id, name, sequence, state, attempt,
                               started_at, completed_at)
VALUES ($1,$2,$3,$4,99,'SUCCEEDED',1,$5,$5)`,
						s.ID(testenv.PrefixStep, "resume/dup/"+runToken+"/"+step),
						ci.id.String(), s.TenantA, steps[0], s.Clock.Now())
					return err
				})
				if err == nil {
					t.Fatalf("the database accepted a second record of completed step %q at "+
						"attempt 1; uq_step_name_attempt is not enforcing", steps[0])
				}
			}

			// 5. Resume: the winner runs only what the checkpoint says is outstanding, and the
			//    side-effect ledger ends with exactly one entry per step.
			resumeFrom(t, uow, s, s.TenantA, ci, steps, winner, newEpoch, fmt.Sprintf("s%02d", k))

			effects := sideEffectsByStep(t, s, s.TenantA, ci.id)
			if len(effects) != len(steps) {
				t.Fatalf("the run produced side effects for %d steps, want %d", len(effects), len(steps))
			}
			for name, n := range effects {
				if n != 1 {
					t.Fatalf("step %q produced %d side effects; resuming re-executed work that had "+
						"already committed", name, n)
				}
			}
		})
	}
}

// raceForLease has n workers poll simultaneously and returns those that got the named instance.
func raceForLease(t *testing.T, uow *postgres.UnitOfWork, tenant string, inst shared.WorkflowID, n int) []string {
	t.Helper()
	var (
		mu      sync.Mutex
		winners []string
		errs    []error
	)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		worker := fmt.Sprintf("worker-%d/pid%d/boot%s", i, i, runToken[:4])
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := tryTx(uow, tenant, func(ctx context.Context, r ports.Repositories) error {
				leased, err := r.Workflows.LeaseRunnable(ctx, worker, time.Minute, 10)
				if err != nil {
					return err
				}
				for _, rec := range leased {
					if rec.ID == inst {
						mu.Lock()
						winners = append(winners, worker)
						mu.Unlock()
					}
				}
				return nil
			})
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	if len(errs) > 0 {
		t.Fatalf("a worker's poll failed: %v", errs[0])
	}
	sort.Strings(winners)
	return winners
}

// completedSteps returns the names of an instance's SUCCEEDED steps, in sequence order.
func completedSteps(t *testing.T, uow *postgres.UnitOfWork, tenant string, inst shared.WorkflowID) []string {
	t.Helper()
	var out []string
	inTx(t, uow, tenant, func(ctx context.Context, r ports.Repositories) error {
		recs, err := r.Workflows.ListSteps(ctx, inst)
		if err != nil {
			return err
		}
		for _, rec := range recs {
			if rec.State == "SUCCEEDED" {
				out = append(out, rec.Name)
			}
		}
		return nil
	})
	return out
}

// resumeFrom advances the instance to completion, running only the outstanding steps.
//
// The resume rule it applies — "a step whose checkpoint says SUCCEEDED is not re-executed" — is
// the engine's rule, restated here because this file drives the repository directly rather than
// the engine. That is a real limitation and it is why assertion 4 above exists: the database
// refuses a duplicate record regardless of whether the resume logic is correct, so the test does
// not rest on this function being right.
func resumeFrom(
	t *testing.T, uow *postgres.UnitOfWork, s *testenv.Scope, tenant string,
	ci *crashedInstance, steps []string, worker string, epoch int64, tag string,
) {
	t.Helper()
	done := map[string]bool{}
	for _, name := range completedSteps(t, uow, tenant, ci.id) {
		done[name] = true
	}

	for i, name := range steps {
		if done[name] {
			continue
		}
		// The heartbeat goes through the real repository: it is the fencing check, and it is the
		// one statement in this loop whose behaviour the test depends on. The checkpoint write is
		// raw SQL for the jsonb reason recorded in repos_test.go.
		inTx(t, uow, tenant, func(ctx context.Context, r ports.Repositories) error {
			if err := r.Workflows.Heartbeat(ctx, ci.id, worker, epoch, time.Minute); err != nil {
				return fmt.Errorf("heartbeat before %s: %w", name, err)
			}
			return nil
		})

		c := ctx(t)
		if err := s.TenantedCommitted(c, tenant, func(tx pgx.Tx) error {
			at := s.Clock.Now()
			if _, err := tx.Exec(c, `
INSERT INTO pp.workflow_steps (step_id, instance_id, tenant_id, name, sequence, state, attempt,
                               started_at, completed_at)
VALUES ($1,$2,$3,$4,$5,'SUCCEEDED',1,$6,$6)`,
				s.ID(testenv.PrefixStep, fmt.Sprintf("resume/%s/%s/r%d", runToken, tag, i)),
				ci.id.String(), tenant, name, i, at); err != nil {
				return fmt.Errorf("checkpoint %s: %w", name, err)
			}
			return recordSideEffect(c, tx, s, tenant, ci.id, name, tag)
		}); err != nil {
			t.Fatalf("resume %s: %v", name, err)
		}
	}

	// Terminal state, so the instance stops being runnable and the next subtest's poll cannot
	// pick it up.
	c := ctx(t)
	if err := s.TenantedCommitted(c, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(c, `
UPDATE pp.workflow_instances
   SET state = 'COMPLETED', completed_at = $3, lease_owner = NULL, lease_expires_at = NULL,
       updated_at = now(), version = version + 1
 WHERE tenant_id = $1 AND instance_id = $2`, tenant, ci.id.String(), s.Clock.Now())
		return err
	}); err != nil {
		t.Fatalf("complete the instance: %v", err)
	}
}

// sideEffectsByStep counts the durable effects recorded for an instance, per step.
func sideEffectsByStep(t *testing.T, s *testenv.Scope, tenant string, inst shared.WorkflowID) map[string]int {
	t.Helper()
	out := map[string]int{}
	c := ctx(t)
	err := s.Tenanted(c, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(c, `
SELECT payload ->> 'step', count(*)
  FROM (SELECT convert_from(payload, 'UTF8')::jsonb AS payload
          FROM pp.outbox_events
         WHERE tenant_id = $1 AND aggregate_id = $2) x
 GROUP BY 1`, tenant, inst.String())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			var n int
			if err := rows.Scan(&name, &n); err != nil {
				return err
			}
			out[name] = n
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("count side effects: %v", err)
	}
	return out
}

// TestStartingOnboardingTwiceIsRefusedByTheDatabase is the business-key half of §11.
//
// Verifies: baseline §11 ("starting a workflow twice is a no-op returning the existing instance"),
// docs/testing.md §5.4. The no-op is implemented as a partial unique index over
// (tenant, definition, business_key) restricted to live states — so the guarantee holds under
// concurrency, which a read-then-write check would not. And the index is partial so that a merchant
// whose onboarding failed can legitimately be onboarded again.
func TestStartingOnboardingTwiceIsRefusedByTheDatabase(t *testing.T) {
	t.Parallel()
	_, s := setup(t)
	uow := newUoW(t, shared.SystemClock{})

	businessKey := "twice-" + runToken + "-" + s.Nonce()[:6]
	now := s.Clock.Now()

	create := func(seed string) error {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return s.TenantedCommitted(c, s.TenantA, func(tx pgx.Tx) error {
			_, err := tx.Exec(c, `
INSERT INTO pp.workflow_instances (
    instance_id, tenant_id, workflow_name, workflow_version, business_key, state, current_step,
    input, checkpoint, run_after, created_at, updated_at)
VALUES ($1,$2,$3,$4,$5,'RUNNING',$6,'{}'::jsonb,'{}'::jsonb,$7,$7,$7)`,
				s.ID(testenv.PrefixWorkflow, "twice/"+runToken+"/"+seed), s.TenantA,
				onboarding.WorkflowName, onboarding.WorkflowVersion, businessKey,
				onboarding.StepValidateMerchant, now)
			return err
		})
	}

	if err := create("a"); err != nil {
		t.Fatalf("the first start failed: %v", err)
	}
	if err := create("b"); err == nil {
		t.Fatal("a second live onboarding was created for the same merchant. Two instances mean " +
			"two sets of gateway sub-accounts, two KYC cases and one very confused merchant.")
	}

	// The index is partial: once the first instance reaches a terminal state, onboarding may
	// legitimately be started again. A non-partial index would make a failed onboarding permanent.
	//
	// The lookup goes through the real repository — GetInstanceByBusinessKey is the statement the
	// "starting twice is a no-op" behaviour reads — and only the state change is raw SQL.
	var live shared.WorkflowID
	inTx(t, uow, s.TenantA, func(ctx context.Context, r ports.Repositories) error {
		rec, err := r.Workflows.GetInstanceByBusinessKey(ctx, onboarding.WorkflowName, businessKey)
		if err != nil {
			return err
		}
		live = rec.ID
		return nil
	})
	c := ctx(t)
	if err := s.TenantedCommitted(c, s.TenantA, func(tx pgx.Tx) error {
		_, err := tx.Exec(c, `
UPDATE pp.workflow_instances
   SET state = 'FAILED', completed_at = $3, updated_at = now(), version = version + 1
 WHERE tenant_id = $1 AND instance_id = $2`, s.TenantA, live.String(), s.Clock.Now())
		return err
	}); err != nil {
		t.Fatalf("fail the first instance: %v", err)
	}
	if err := create("c"); err != nil {
		t.Fatalf("re-onboarding after a FAILED instance was refused: %v.\n"+
			"The unique index must be partial, or a merchant whose onboarding failed can never be "+
			"onboarded again.", err)
	}
}
