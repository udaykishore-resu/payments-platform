# 09 — Production Readiness Checklist

Gate before this feature is allowed to serve real traffic. Every item requires an explicit owner
and evidence (link to dashboard, test run, or doc), not just a checkbox.

## Architecture & Design
- [ ] ADRs reviewed and accepted by Solution/Enterprise Architect
- [ ] Failure mode table (`04-failure-recovery-design.md`) reviewed by SRE
- [ ] Threat model (`05-security-architecture.md`) reviewed by Security Architect

## Correctness
- [ ] Unit test coverage on domain/service layer ≥ 85%, ledger-balance logic specifically 100%
- [ ] Integration tests cover: idempotent replay, concurrent duplicate requests, serialization
      conflict retry, outbox crash-recovery
- [ ] Load test passed (500 req/s / 30 min, P95 < 250ms, P99 < 600ms, zero inconsistency)
- [ ] Soak test passed (24h, no memory/goroutine/connection leak)
- [ ] Chaos experiments (pod kill, DB failover, SQS latency injection) passed in staging

## Security
- [ ] SAST + dependency/CVE scan clean (no unresolved critical/high) in CI
- [ ] Container image scan clean, distroless/minimal base image confirmed
- [ ] IAM roles reviewed for least privilege (no `*` resource/action grants)
- [ ] Secrets sourced from Secrets Manager only, none in env vars/ConfigMaps/image layers
- [ ] NetworkPolicy default-deny confirmed, explicit allow rules reviewed
- [ ] WAF rules active and tested against a known-bad request sample
- [ ] Penetration test completed or scheduled with sign-off to proceed pending results

## Observability
- [ ] RED + domain metrics visible in Grafana dashboards (linked in `06-observability.md`)
- [ ] Alerts wired to on-call paging (test-fired at least once, not just configured)
- [ ] Distributed tracing confirmed end-to-end including async outbox→consumer link
- [ ] Structured logs confirmed to include `trace_id`/`payment_id` correlation
- [ ] Synthetic canary running from both regions

## Reliability
- [ ] SLOs agreed with product/business, error budget policy communicated to the team
- [ ] Runbook (`08-runbook.md`) reviewed and walked through in a tabletop exercise
- [ ] On-call rotation staffed, escalation path documented
- [ ] DR drill (region failover) executed at least once in staging with RTO/RPO measured, not
      assumed

## Deployment
- [ ] Zero-downtime rolling deploy verified (deploy under synthetic load, zero dropped requests)
- [ ] Automated rollback on canary-analysis failure tested (deliberately deploy a broken canary in
      staging, confirm auto-rollback triggers)
- [ ] PodDisruptionBudget + resource requests/limits sized from load-test data, not guessed
- [ ] HPA min/max and target metrics reviewed against capacity plan

## Data
- [ ] Backup/restore verified via an actual restore drill, not just "backups are enabled"
- [ ] Point-in-time recovery tested to a scratch instance
- [ ] Migration procedure (expand-contract) documented and rehearsed for at least one real schema
      change

## Compliance & Documentation
- [ ] Audit log retention policy (7 years) implemented and verified (write-once enforcement at DB
      role level)
- [ ] API documented (OpenAPI spec) and published to consumers
- [ ] This checklist itself reviewed and signed off by Release Manager before first prod traffic
