# 09 — Traceability matrix

> **Generated file — do not edit.** Regenerate with `scripts/traceability.sh`.
> CI runs `scripts/traceability.sh --check` and fails on drift or on an orphan
> requirement (baseline §26).

Every requirement defined by a heading in `01-business-requirements.md`,
`02-functional-requirements.md` and `03-non-functional-requirements.md` appears
below with the design documents that describe it, the packages that implement it
and the tests that prove it. A `—` in the **Tests** column fails the build.

| Requirement | Title | Design | Code | Tests |
|---|---|---|---|---|
| `BR-01` | Tenant registration and tier assignment | **—** | `internal/domain/tenant/tenant.go` | `internal/domain/tenant/tenant_test.go::TestNewTenantValidation`, `internal/domain/tenant/tenant_test.go::TestPermitsIsTheTenantCeiling` |
| `BR-02` | API client credentials with least-privilege scopes | **—** | `internal/domain/tenant/apiclient.go` | `internal/domain/tenant/tenant_test.go::TestNewAPIClientValidation` |
| `BR-03` | Merchant registration under a tenant | **—** | **—** | **—** |
| `BR-04` | Structured onboarding submission | **—** | `internal/application/onboarding/service.go`, `internal/workflows/onboarding/definition.go` | `internal/application/onboarding/service_test.go::TestStartIsIdempotentOnTheMerchant`, `internal/workflows/onboarding/onboarding_test.go::TestOnboardingHappyPath` |
| `BR-05` | KYC/KYB verification through a vendor port | **—** | `internal/workflows/onboarding/validation.go` | `internal/validation/rules/l2merchant/rules_test.go::TestL2Rules`, `internal/workflows/onboarding/onboarding_test.go::TestKYCRejectionIsABusinessOutcome` |
| `BR-06` | Bank account validation | **—** | `internal/workflows/onboarding/validation.go` | `internal/application/merchant/service_test.go::TestAddBankAccountRefusesAnAccountWithNoSecretReference`, `internal/validation/rules/l2merchant/rules_test.go::TestL2Rules` |
| `BR-07` | Payment-method configuration | **—** | `internal/domain/config/config.go` | `internal/validation/rules/l4config/rules_test.go::TestL4Rules` |
| `BR-08` | Currency and country configuration | **—** | `internal/domain/config/config.go` | `internal/validation/rules/l4config/rules_test.go::TestL4Rules` |
| `BR-09` | Multi-gateway support per merchant | **—** | `internal/domain/config/config.go` | `internal/validation/rules/l4config/rules_test.go::TestL4PublishableConfigurationIsClean` |
| `BR-10` | Automated gateway provisioning | **—** | `internal/domain/gateway/connection.go` | `internal/domain/gateway/connection_test.go::TestConnectionHappyPath` |
| `BR-11` | Gateway credential custody | **—** | `internal/application/payment/gateway_resolver.go`, `internal/domain/gateway/connection.go`, `internal/infrastructure/secrets/awssm.go`, `internal/infrastructure/secrets/provider.go` | `internal/application/payment/components_test.go::TestCredentialsAreResolvedPerCallAndNeverCached`, `internal/domain/gateway/connection_test.go::TestSecretRefValidation`, `internal/workflows/onboarding/onboarding_test.go::TestStoreCredentialsOutputCarriesNoMaterial` |
| `BR-12` | Credential rotation with dual-run overlap | **—** | `internal/domain/gateway/connection.go`, `internal/domain/tenant/apiclient.go` | `internal/domain/gateway/connection_test.go::TestRotateCredentials`, `internal/domain/tenant/tenant_test.go::TestRotationOverlap` |
| `BR-13` | Routing configuration | **—** | `internal/domain/routing/engine.go`, `internal/domain/routing/policy.go` | `internal/domain/routing/engine_test.go::TestDecideReproducesTheDocumentedWorkedExample` |
| `BR-14` | Transaction limits | **—** | `internal/domain/config/config.go`, `internal/domain/risk/engine.go` | `internal/domain/risk/engine_test.go::TestAmountLimitFailsClosed`, `internal/validation/rules/l5payment/rules_test.go::TestL5Rules` |
| `BR-15` | Risk policy and SCA | **—** | `internal/domain/risk/engine.go` | `internal/application/payment/service_test.go::TestRiskDeclineNeverReachesAGateway`, `internal/domain/risk/engine_test.go::TestEvaluateApprovesACleanPayment` |
| `BR-16` | Sandbox validation before certification | **—** | `internal/workflows/onboarding/definition.go` | `internal/workflows/onboarding/onboarding_test.go::TestCertificationCatchesAGatewayThatIgnoresIdempotencyKeys` |
| `BR-17` | Machine-checked certification | **—** | `internal/workflows/onboarding/definition.go` | `internal/workflows/onboarding/onboarding_test.go::TestCertificationReportIsHashedAndStoredUnderRetention` |
| `BR-18` | Production activation with explicit guards | **—** | **—** | **—** |
| `BR-19` | Manual compliance gate | **—** | `internal/application/onboarding/service.go` | `internal/application/onboarding/service_test.go::TestSignalDeliversTheDecisionAndAuditsThePrincipal`, `internal/workflows/onboarding/onboarding_test.go::TestComplianceRejectionUsesTheAmendedState` |
| `BR-20` | Accept and execute a payment instruction | **—** | `internal/application/payment/service.go`, `internal/domain/payment/payment.go` | `internal/application/payment/service_test.go::TestCreateHappyPath` |
| `BR-21` | No double charge, structurally | **—** | `internal/domain/payment/payment.go`, `internal/platform/idempotency/manager.go` | `internal/application/payment/orchestrator_test.go::TestFailoverNeverProducesTwoSuccessfulAttempts`, `internal/application/payment/orchestrator_test.go::TestUnresolvedAttemptBlocksAnyFurtherAttempt`, `internal/application/payment/service_test.go::TestTwoConcurrentCreatesWithOneIdempotencyKeyProduceOnePayment`, `internal/infrastructure/postgres/concurrency_integration_test.go::TestI1ConcurrentPartialRefundsCannotExceedCaptured`, +1 more |
| `BR-22` | Failover on retryable failure | **—** | `internal/application/payment/orchestrator.go`, `internal/domain/routing/engine.go` | `internal/application/payment/orchestrator_test.go::TestSoftDeclineFailsOverAndCreatesASecondAttempt`, `internal/domain/routing/engine_test.go::TestPlanNextIsTheFailoverPicker` |
| `BR-23` | Hard declines must never fail over | **—** | `internal/application/payment/orchestrator.go` | `internal/application/payment/orchestrator_test.go::TestHardDeclineDoesNotFailOver`, `internal/domain/gateway/health_test.go::TestHardDeclinesDoNotOpenTheCircuit` |
| `BR-24` | Two-step flows: authorize then capture | **—** | `internal/domain/payment/payment.go` | `internal/application/payment/service_test.go::TestCaptureGoesToTheAuthorizingGatewayAndNeverRoutes`, `internal/infrastructure/postgres/invariants_integration_test.go::TestI2RejectsCaptureExceedingAuthorization` |
| `BR-25` | Refunds, including after settlement | **—** | `internal/domain/payment/payment.go` | `internal/application/payment/service_test.go::TestRefundGoesToTheGatewayThatTookTheMoney`, `internal/infrastructure/postgres/invariants_integration_test.go::TestI1RejectsRefundExceedingCapture` |
| `BR-26` | Voids | **—** | `internal/domain/payment/payment.go` | `internal/application/payment/service_test.go::TestVoidReleasesTheHoldAtTheAuthorizingGateway` |
| `BR-27` | Disputes and chargebacks | **—** | `internal/domain/ledger/transaction.go` | `internal/application/ledger/service_test.go::TestDisputeStagesMoveMoneyInOppositeDirections` |
| `BR-28` | Resolve unknown outcomes by reconciliation | **—** | `internal/application/payment/reconciler.go`, `internal/domain/payment/payment.go` | `internal/application/payment/orchestrator_test.go::TestTimeoutLeavesPaymentProcessingAndDoesNotFailOver`, `internal/application/payment/orchestrator_test.go::TestUnresolvedAttemptBlocksAnyFurtherAttempt`, `internal/application/payment/reconciler_test.go::TestReconcilerResolvesAnAuthorizedTimeout` |
| `BR-29` | Shadow ledger | **—** | `internal/application/ledger/service.go`, `internal/domain/ledger/transaction.go` | `internal/application/ledger/service_test.go::TestCapturePostsTheDocumentedPair`, `internal/domain/ledger/transaction_test.go::TestBuildersAlwaysBalance` |
| `BR-30` | Settlement observation and reconciliation | **—** | `internal/application/payment/reconciler.go`, `internal/domain/ledger/transaction.go` | `internal/application/payment/reconciler_test.go::TestSettlementIngestion`, `internal/application/payment/reconciler_test.go::TestSettlementThatDoesNotReconcileIsAnExceptionNotAPosting`, `internal/domain/ledger/transaction_test.go::TestSettlementMustReconcile` |
| `BR-31` | Suspension that still permits refunds | **—** | `internal/domain/merchant/merchant.go` | `internal/application/merchant/service_test.go::TestSuspendAndReinstate` |
| `BR-32` | Termination and erasure | **—** | `internal/domain/compliance/retention.go` | `internal/domain/compliance/retention_test.go::TestErasureCarveOut` |
| `BR-33` | Tenant self-service configuration with versioning and rollback | **—** | `internal/application/config/service.go` | `internal/application/config/service_test.go::TestPublishRequiresAndHonoursIfMatch`, `internal/application/config/service_test.go::TestRollbackIsForwardOnly` |
| `BR-34` | Gateway registry and capability descriptors | **—** | `internal/domain/gateway/descriptor.go` | `internal/domain/gateway/descriptor_test.go::TestCapabilitiesValidate`, `internal/domain/gateway/descriptor_test.go::TestNewGatewayValidation` |
| `BR-35` | Gateway health visibility and operator control | **—** | `internal/domain/gateway/health.go` | `internal/domain/gateway/health_test.go::TestCooldownProbeAndClose` |
| `BR-36` | Data residency enforcement | **—** | `internal/domain/tenant/tenant.go` | `internal/domain/tenant/tenant_test.go::TestResidencyPermitsGatewayRegion` |
| `BR-37` | Audit trail and compliance reporting | **—** | `internal/domain/audit/record.go` | `internal/domain/audit/record_test.go::TestChainVerifiesIntactChain` |
| `BR-38` | Metering for billing | **—** | **—** | **—** |
| `FR-01` | Register a tenant | **—** | `internal/domain/tenant/tenant.go` | `internal/domain/tenant/tenant_test.go::TestNewTenantValidation` |
| `FR-02` | Provision isolation resources for a siloed tenant | **—** | `internal/domain/tenant/tenant.go` | `internal/domain/tenant/tenant_test.go::TestNewTenantValidation` |
| `FR-03` | Issue an API client with least-privilege scopes | **—** | `internal/domain/tenant/apiclient.go` | `internal/domain/tenant/tenant_test.go::TestNewAPIClientValidation` |
| `FR-04` | Authenticate a request | **—** | `internal/platform/authn/jwt.go` | `internal/platform/authn/apikey_test.go::TestAPIKeyHappyPath`, `internal/platform/authn/jwt_test.go::TestAttackMatrix`, `internal/platform/authn/jwt_test.go::TestValidateHappyPath` |
| `FR-05` | Authorize a request by scope and attribute | **—** | `internal/platform/authz/policy.go` | `internal/platform/authz/authz_test.go::TestDefaultDeny`, `internal/platform/authz/authz_test.go::TestNoAllowEverCrossesATenantBoundary` |
| `FR-06` | Resolve the tenant and enforce the isolation guard | **—** | `internal/infrastructure/postgres/tenant.go`, `internal/platform/tenantctx/tenantctx.go` | `internal/application/payment/service_test.go::TestCreateAssertsTenantContext`, `internal/infrastructure/postgres/rls_integration_test.go::TestCrossTenantAccessIsImpossible`, `internal/infrastructure/postgres/tenant_guard_test.go::TestRepositoriesRefuseToQueryWithoutATenant`, `internal/platform/tenantctx/tenantctx_test.go::TestAssertTenant`, +1 more |
| `FR-07` | Enforce the tenant data-residency policy | **—** | `internal/domain/tenant/tenant.go` | `internal/domain/tenant/tenant_test.go::TestResidencyPermitsGatewayRegion` |
| `FR-08` | Rotate and revoke API client credentials with overlap | **—** | `internal/domain/tenant/apiclient.go`, `internal/platform/authn/apikey.go` | `internal/domain/tenant/tenant_test.go::TestRotationOverlap`, `internal/platform/authn/apikey_test.go::TestAPIKeyRotationOverlap` |
| `FR-09` | Register a merchant | **—** | `internal/application/merchant/service.go`, `internal/domain/merchant/merchant.go` | `internal/application/merchant/service_test.go::TestCreateWritesTheStateChangeAndTheAuditRecordTogether` |
| `FR-10` | Update a merchant profile with optimistic concurrency | **—** | `internal/application/merchant/service.go` | `internal/application/merchant/service_test.go::TestUpdateRequiresAndHonoursIfMatch` |
| `FR-11` | List and read merchants | **—** | `internal/application/merchant/service.go` | `internal/application/merchant/service_test.go::TestGetRefusesAMerchantFromAnotherTenantAsNotFound` |
| `FR-12` | Activate a merchant under explicit guards | **—** | **—** | **—** |
| `FR-13` | Suspend and unsuspend a merchant | **—** | `internal/application/merchant/service.go`, `internal/domain/merchant/merchant.go` | `internal/application/merchant/service_test.go::TestSuspendAndReinstate` |
| `FR-14` | Terminate a merchant | **—** | `internal/application/merchant/service.go`, `internal/domain/merchant/merchant.go` | `internal/application/merchant/service_test.go::TestTerminateIsRefusedWhilePaymentsAreOpen` |
| `FR-15` | Right-to-erasure by crypto-shredding | **—** | `internal/domain/compliance/retention.go` | `internal/domain/compliance/retention_test.go::TestErasureCarveOut` |
| `FR-16` | Submit the onboarding package with batch validation | **—** | `internal/workflows/onboarding/validation.go` | `internal/workflows/onboarding/onboarding_test.go::TestDefaultValidatorReportsEveryFailureAtOnce` |
| `FR-17` | Start the onboarding workflow under a business key | **—** | `internal/application/onboarding/service.go` | `internal/application/onboarding/service_test.go::TestStartIsIdempotentOnTheMerchant` |
| `FR-18` | Correct and resubmit a failed validation | **—** | **—** | **—** |
| `FR-19` | Submit KYC/KYB to the vendor idempotently | **—** | **—** | **—** |
| `FR-20` | Deliver a signal to a waiting workflow step | **—** | `internal/application/onboarding/service.go` | `internal/application/onboarding/service_test.go::TestSignalDeliversTheDecisionAndAuditsThePrincipal` |
| `FR-21` | Handle a KYC failure and resubmission | **—** | `internal/workflows/onboarding/validation.go` | `internal/workflows/onboarding/onboarding_test.go::TestKYCRejectionIsABusinessOutcome` |
| `FR-22` | Validate bank account structure | **—** | `internal/workflows/onboarding/validation.go` | `internal/validation/rules/l2merchant/rules_test.go::TestL2Rules` |
| `FR-23` | Verify bank account ownership | **—** | `internal/workflows/onboarding/validation.go` | `internal/application/merchant/service_test.go::TestAddBankAccountRefusesAnAccountWithNoSecretReference` |
| `FR-24` | Provision gateway accounts (fan-out) | **—** | `internal/workflows/onboarding/definition.go` | `internal/workflows/onboarding/onboarding_test.go::TestOnboardingHappyPath` |
| `FR-25` | Store credentials and register webhooks | **—** | `internal/workflows/onboarding/definition.go` | `internal/workflows/onboarding/onboarding_test.go::TestOnboardingHappyPath` |
| `FR-26` | Apply the initial configuration | **—** | `internal/workflows/onboarding/definition.go` | `internal/workflows/onboarding/onboarding_test.go::TestOnboardingHappyPath` |
| `FR-27` | Run sandbox validation | **—** | `internal/workflows/onboarding/definition.go` | `internal/workflows/onboarding/onboarding_test.go::TestCertificationCatchesAGatewayThatIgnoresIdempotencyKeys` |
| `FR-28` | Run the certification suite and produce a signed report | **—** | `internal/workflows/onboarding/definition.go` | `internal/workflows/onboarding/onboarding_test.go::TestCertificationReportIsHashedAndStoredUnderRetention` |
| `FR-29` | Clear the manual compliance gate | **—** | `internal/workflows/onboarding/definition.go` | `internal/workflows/onboarding/onboarding_test.go::TestComplianceRejectionUsesTheAmendedState` |
| `FR-30` | Query onboarding case status | **—** | `internal/application/onboarding/service.go` | `internal/application/onboarding/service_test.go::TestGetAssemblesTheCaseFromTheCheckpointedSteps` |
| `FR-31` | Abort a case and compensate in reverse order | **—** | `internal/application/onboarding/service.go` | `internal/application/onboarding/service_test.go::TestCancelRequiresAReasonAndAudits`, `internal/workflows/onboarding/onboarding_test.go::TestCompensationOrderOnCertificationFailure` |
| `FR-32` | Resume a workflow instance after a worker crash, and park exhausted steps | **—** | `internal/application/onboarding/service.go` | `internal/application/onboarding/service_test.go::TestRetryIsAResumeNotARestart`, `internal/workflows/onboarding/onboarding_test.go::TestResumeDoesNotReplayCompletedSteps` |
| `FR-33` | Register a gateway and publish its capability descriptor | **—** | `internal/domain/gateway/descriptor.go` | `internal/domain/gateway/descriptor_test.go::TestNewGatewayValidation` |
| `FR-34` | Change a descriptor with a live-dependency guard | **—** | `internal/domain/gateway/descriptor.go` | `internal/domain/gateway/descriptor_test.go::TestUpdateCapabilitiesRevalidates` |
| `FR-35` | Create and track a gateway connection | **—** | `internal/domain/gateway/connection.go` | `internal/domain/gateway/connection_test.go::TestConnectionHappyPath` |
| `FR-36` | Compute and publish gateway health | **—** | `internal/domain/gateway/health.go` | `internal/domain/gateway/health_test.go::TestCooldownProbeAndClose`, `internal/domain/gateway/health_test.go::TestObserveDuringCooldownExpiryReportsTheChange` |
| `FR-37` | Operator health override with mandatory expiry | **—** | **—** | **—** |
| `FR-38` | Rotate gateway credentials with a dual-run overlap | **—** | `internal/domain/gateway/connection.go` | `internal/domain/gateway/connection_test.go::TestRotateCredentials`, `tests/load/steady-state.js` |
| `FR-39` | Roll back a failed credential rotation | **—** | `internal/domain/gateway/connection.go` | `internal/domain/gateway/connection_test.go::TestRecoveryEdges` |
| `FR-40` | Execute a gateway operation through the adapter | **—** | `internal/application/payment/gateway_resolver.go`, `internal/application/payment/validator.go`, `internal/infrastructure/secrets/awssm.go`, `internal/infrastructure/secrets/provider.go`, +3 more | `internal/application/payment/components_test.go::TestResolveRefusesAConnectionThatMayNotCarryPayments` |
| `FR-41` | Validate the gateway response (L6) | **—** | `internal/application/payment/validator.go` | `internal/application/payment/components_test.go::TestL6RejectsAnEchoedAmountThatDoesNotMatch`, `internal/application/payment/orchestrator_test.go::TestL6ContractViolationParksThePaymentAsUnknown` |
| `FR-42` | Detect and reconcile provisioning drift | **—** | **—** | **—** |
| `FR-43` | Read the current configuration | **—** | `internal/application/config/service.go` | `internal/platform/config/provider_test.go::TestGetPerformsNoIO`, `internal/platform/config/provider_test.go::TestStalenessLadder` |
| `FR-44` | Publish a configuration version | **—** | `internal/application/config/service.go` | `internal/application/config/service_test.go::TestFirstPublishNeedsNoPrecondition`, `internal/application/config/service_test.go::TestPublishRequiresAndHonoursIfMatch` |
| `FR-45` | List configuration versions and diffs | **—** | `internal/application/config/service.go` | `internal/application/config/service_test.go::TestDiffIsStableAndScalarPathed` |
| `FR-46` | Roll back configuration | **—** | `internal/application/config/service.go` | `internal/application/config/service_test.go::TestRollbackIsForwardOnly` |
| `FR-47` | Propagate configuration to the data plane | **—** | `internal/platform/config/provider.go` | `internal/application/payment/components_test.go::TestContextLoaderServesWithinTolerance`, `internal/platform/config/provider_test.go::TestStalenessLadder` |
| `FR-48` | Serve fail-static configuration during a control-plane outage | **—** | `internal/platform/config/provider.go` | `internal/application/payment/components_test.go::TestContextLoaderCliffRefusesUnknownMerchantsAndKeepsServingKnownOnes`, `internal/platform/config/provider_test.go::TestFailedRefreshIsFailStatic` |
| `FR-49` | Configure payment methods, currencies and countries | **—** | `internal/domain/config/config.go` | `internal/validation/rules/l4config/rules_test.go::TestL4Rules` |
| `FR-50` | Configure the routing policy | **—** | `internal/domain/routing/policy.go` | `internal/domain/routing/policy_test.go::TestPolicyValidate` |
| `FR-51` | Configure limits, risk policy and SCA thresholds | **—** | `internal/domain/config/config.go` | `internal/validation/rules/l4config/rules_test.go::TestL4Rules` |
| `FR-52` | Configure webhook endpoints and settlement preferences | **—** | `internal/domain/config/config.go` | `internal/validation/rules/l4config/rules_test.go::TestL4Rules` |
| `FR-53` | Create a payment (main flow) | **—** | `internal/application/payment/service.go`, `internal/domain/payment/payment.go` | `internal/application/payment/service_test.go::TestCreateHappyPath`, `internal/infrastructure/postgres/invariants_integration_test.go::TestDatabaseRefusesAnIllegalStateTransition` |
| `FR-54` | Claim idempotency | **—** | `internal/platform/idempotency/manager.go` | `internal/platform/idempotency/manager_test.go::TestBeginRejectsUnusableKeys`, `internal/platform/idempotency/manager_test.go::TestOutcomeMatrix` |
| `FR-55` | Concurrent duplicate idempotent request | **—** | `internal/platform/idempotency/manager.go` | `internal/application/payment/service_test.go::TestTwoConcurrentCreatesWithOneIdempotencyKeyProduceOnePayment`, `internal/infrastructure/postgres/concurrency_integration_test.go::TestConcurrentIdempotencyClaimsYieldExactlyOneNew`, `internal/platform/idempotency/manager_test.go::TestConcurrentBeginYieldsExactlyOneNew` |
| `FR-56` | Idempotency key reused with a different body | **—** | `internal/platform/idempotency/manager.go` | `internal/application/payment/service_test.go::TestIdempotencyKeyReusedWithADifferentBodyIsRejected`, `internal/platform/idempotency/manager_test.go::TestFingerprintIsInsensitiveToSerialization` |
| `FR-57` | Replay a completed idempotent request | **—** | `internal/platform/idempotency/manager.go` | `internal/platform/idempotency/manager_test.go::TestOutcomeMatrix` |
| `FR-58` | Reclaim an expired idempotency lease | **—** | `internal/platform/idempotency/manager.go` | `internal/infrastructure/postgres/concurrency_integration_test.go::TestConcurrentReclaimsYieldExactlyOneWinner` |
| `FR-59` | Load merchant context and reject non-active merchants | **—** | `internal/application/payment/service.go` | `internal/application/payment/service_test.go::TestCreateRefusesASuspendedMerchant` |
| `FR-60` | Apply L5 payment validation | **—** | `internal/application/payment/validator.go` | `internal/application/payment/components_test.go::TestL5AcceptsAWellFormedCreate`, `internal/application/payment/service_test.go::TestL5FailureStopsBeforeThePaymentExists` |
| `FR-61` | Evaluate risk and force SCA | **—** | `internal/domain/risk/engine.go` | `internal/application/payment/service_test.go::TestRiskDeclineNeverReachesAGateway`, `internal/domain/risk/engine_test.go::TestEvaluateApprovesACleanPayment` |
| `FR-62` | Build and persist the routing plan | **—** | `internal/domain/routing/engine.go` | `internal/application/payment/components_test.go::TestCandidateFieldsComeFromRealSources`, `internal/domain/routing/engine_test.go::TestDecideReproducesTheDocumentedWorkedExample` |
| `FR-63` | Create and dispatch an attempt | **—** | `internal/application/payment/orchestrator.go`, `internal/domain/payment/payment.go` | `internal/application/payment/orchestrator_test.go::TestAttemptIsCommittedBeforeTheGatewayCall` |
| `FR-64` | Fail over after a retryable failure | **—** | `internal/application/payment/orchestrator.go` | `internal/application/payment/orchestrator_test.go::TestSoftDeclineFailsOverAndCreatesASecondAttempt`, `internal/domain/routing/engine_test.go::TestPlanNextIsTheFailoverPicker` |
| `FR-65` | Hard decline: terminate without failover | **—** | `internal/application/payment/orchestrator.go` | `internal/application/payment/orchestrator_test.go::TestHardDeclineDoesNotFailOver` |
| `FR-66` | Gateway timeout with an unknown outcome | **—** | `internal/application/payment/orchestrator.go`, `internal/domain/payment/payment.go` | `internal/application/payment/orchestrator_test.go::TestTimeoutLeavesPaymentProcessingAndDoesNotFailOver` |
| `FR-67` | Routing plan exhausted | **—** | `internal/application/payment/orchestrator.go`, `internal/domain/routing/engine.go` | `internal/application/payment/service_test.go::TestNoEligibleGatewayIsAnAnswerNotJustARefusal`, `internal/domain/routing/engine_test.go::TestNoEligibleGatewayCarriesEveryRejectionReason` |
| `FR-68` | 3DS / REQUIRES_ACTION and its completion | **—** | `internal/domain/risk/engine.go`, `internal/domain/routing/engine.go` | `internal/domain/risk/engine_test.go::TestApproveWithAuthenticationIsRepresentable`, `internal/domain/routing/engine_test.go::TestThreeDSCapabilityIsOnlyCheckedWhenThePaymentNeedsIt` |
| `FR-69` | Capture, full and partial | **—** | `internal/application/payment/service.go`, `internal/domain/payment/payment.go` | `internal/application/payment/service_test.go::TestCaptureGoesToTheAuthorizingGatewayAndNeverRoutes`, `internal/infrastructure/postgres/invariants_integration_test.go::TestI2RejectsCaptureExceedingAuthorization` |
| `FR-70` | Refund, including after settlement and under concurrency | **—** | `internal/application/payment/service.go`, `internal/domain/payment/payment.go` | `internal/application/payment/service_test.go::TestRefundGoesToTheGatewayThatTookTheMoney`, `internal/infrastructure/postgres/concurrency_integration_test.go::TestI1ConcurrentPartialRefundsCannotExceedCaptured`, `internal/infrastructure/postgres/invariants_integration_test.go::TestI1RejectsRefundExceedingCapture` |
| `FR-71` | Void an uncaptured authorization | **—** | `internal/application/payment/service.go`, `internal/domain/payment/payment.go` | `internal/application/payment/service_test.go::TestVoidReleasesTheHoldAtTheAuthorizingGateway` |
| `FR-72` | Read and list payments with read-your-writes | **—** | `internal/application/payment/service.go` | **—** |
| `FR-73` | Asynchronous payment methods | **—** | **—** | **—** |
| `FR-74` | Accept and persist an inbound webhook | **—** | `internal/application/webhook/ingest.go` | `internal/application/webhook/webhook_test.go::TestIngestVerifiesBeforeStoringAnything` |
| `FR-75` | Verify the webhook signature | **—** | `internal/application/webhook/ingest.go` | `internal/application/webhook/webhook_test.go::TestIngestVerifiesBeforeStoringAnything`, `internal/application/webhook/webhook_test.go::TestIngestVerifiesOverTheRawBytes` |
| `FR-76` | Reject a replayed webhook | **—** | `internal/application/webhook/ingest.go` | `internal/application/webhook/webhook_test.go::TestSignatureCoversTheTimestampAndTheBody` |
| `FR-77` | Deduplicate a duplicate webhook | **—** | `internal/application/webhook/ingest.go` | `internal/application/webhook/webhook_test.go::TestIngestDeduplicatesAtTheStorageLayer` |
| `FR-78` | Process a webhook into a domain state transition | **—** | `internal/application/webhook/process.go` | `internal/application/webhook/webhook_test.go::TestProcessAppliesTheTransitionAndPostsTheLedger` |
| `FR-79` | Webhook for an unknown payment | **—** | `internal/application/webhook/process.go` | `internal/application/webhook/webhook_test.go::TestWebhookForAnUnknownPaymentOpensAReconciliationException` |
| `FR-80` | Append ledger entries transactionally | **—** | `internal/application/ledger/service.go`, `internal/domain/ledger/transaction.go` | `internal/application/ledger/service_test.go::TestCapturePostsTheDocumentedPair`, `internal/application/ledger/service_test.go::TestReplayingAnEventDoesNotDoublePost`, `internal/infrastructure/postgres/invariants_integration_test.go::TestLedgerAndAuditAreAppendOnly` |
| `FR-81` | Continuously verify the ledger balance invariant | **—** | `internal/application/ledger/service.go`, `internal/domain/ledger/transaction.go` | `internal/application/ledger/service_test.go::TestEveryPostingBalances`, `internal/domain/ledger/transaction_test.go::TestBuildersAlwaysBalance`, `internal/domain/ledger/transaction_test.go::TestUnbalancedTransactionsAreRejected` |
| `FR-82` | Ingest a settlement report | **—** | `internal/application/payment/reconciler.go` | `internal/application/payment/reconciler_test.go::TestSettlementIngestion` |
| `FR-83` | Match settlement lines to payments | **—** | `internal/application/payment/reconciler.go` | `internal/application/payment/reconciler_test.go::TestSettlementIngestion` |
| `FR-84` | Manage the reconciliation exception lifecycle | **—** | `internal/application/payment/reconciler.go` | `internal/application/payment/reconciler_test.go::TestReconcilerOpensAnExceptionWhenTheGatewayAlsoCannotSay` |
| `FR-85` | Resolve a TIMEOUT_UNKNOWN attempt by gateway lookup | **—** | `internal/application/payment/reconciler.go` | `internal/application/payment/reconciler_test.go::TestReconcilerResolvesAnAuthorizedTimeout` |
| `FR-86` | Handle a dispute lifecycle notification | **—** | **—** | **—** |
| `FR-87` | Produce the billing meter projection | **—** | **—** | **—** |
| `FR-88` | Write a hash-chained audit record | **—** | `internal/domain/audit/record.go` | `internal/application/merchant/service_test.go::TestCreateWritesTheStateChangeAndTheAuditRecordTogether`, `internal/domain/audit/record_test.go::TestNewRecordValidation`, `internal/infrastructure/postgres/invariants_integration_test.go::TestLedgerAndAuditAreAppendOnly` |
| `FR-89` | Verify the audit chain | **—** | `internal/domain/audit/record.go` | `internal/domain/audit/record_test.go::TestChainVerifiesIntactChain`, `internal/domain/audit/record_test.go::TestVerifyFindsTheTamperedRecord` |
| `FR-90` | Export a tenant-scoped audit and compliance report | **—** | **—** | **—** |
| `FR-91` | Raise a security event on an isolation or data-boundary violation | **—** | `internal/platform/secret/pan.go` | `internal/platform/secret/pan_test.go::TestPANDetectorNeverLogsTheValue` |
| `NFR-01` | Payment API server-side latency | **—** | **—** | `tests/load/soak.js`, `tests/load/steady-state.js` |
| `NFR-02` | Per-stage latency budgets are individually enforced | **—** | **—** | `tests/load/ramp.js`, `tests/load/steady-state.js` |
| `NFR-03` | End-to-end payment latency including gateway | **—** | **—** | `tests/load/steady-state.js` |
| `NFR-04` | Gateway call isolation | **—** | **—** | `tests/load/ramp.js` |
| `NFR-05` | Webhook ingress accept latency | **—** | **—** | `tests/load/spike.js` |
| `NFR-06` | Control-plane latency | **—** | **—** | `tests/load/ramp.js` |
| `NFR-07` | Sustained payment throughput | **—** | **—** | `tests/load/spike.js` |
| `NFR-08` | Onboarding throughput | **—** | **—** | `tests/load/soak.js` |
| `NFR-09` | Webhook ingestion throughput | **—** | **—** | **—** |
| `NFR-10` | Event pipeline throughput | **—** | **—** | **—** |
| `NFR-11` | Routing decision throughput | **—** | **—** | **—** |
| `NFR-12` | Idempotency claim throughput | **—** | `internal/platform/idempotency/manager.go` | `internal/platform/idempotency/manager_test.go::TestConcurrentBeginYieldsExactlyOneNew` |
| `NFR-13` | Horizontal scalability of stateless services | **—** | **—** | **—** |
| `NFR-14` | Vertical and storage scaling of the primary datastore | **—** | **—** | **—** |
| `NFR-15` | Partitioning and retention mechanics | **—** | `internal/infrastructure/postgres/payment_repo.go` | `internal/infrastructure/postgres/migrations_test.go::TestPaymentAndAttemptSharePartitionKey`, `internal/infrastructure/postgres/rls_integration_test.go::TestPartitionsCarryRLSAndTheI3Index` |
| `NFR-16` | Index strategy | **—** | **—** | **—** |
| `NFR-17` | Data growth budget | **—** | **—** | **—** |
| `NFR-18` | Event-stream growth budget | **—** | **—** | **—** |
| `NFR-19` | Data-plane availability | **—** | **—** | **—** |
| `NFR-20` | Control-plane availability | **—** | **—** | **—** |
| `NFR-21` | Defined SLI set | **—** | **—** | **—** |
| `NFR-22` | Graceful degradation with defined cliffs | **—** | `internal/platform/config/provider.go` | `internal/application/payment/components_test.go::TestContextLoaderCliffRefusesUnknownMerchantsAndKeepsServingKnownOnes`, `internal/platform/config/provider_test.go::TestFailedRefreshIsFailStatic` |
| `NFR-23` | Error-budget policy | **—** | **—** | **—** |
| `NFR-24` | Durability of committed money state | **—** | **—** | `tests/load/spike.js` |
| `NFR-25` | RPO | **—** | **—** | **—** |
| `NFR-26` | RTO | **—** | **—** | **—** |
| `NFR-27` | Backup and restore | **—** | **—** | **—** |
| `NFR-28` | Zero Trust service-to-service | **—** | `internal/platform/authn/mtls.go` | `internal/platform/authn/mtls_test.go::TestPeerAuthenticationRequiresAVerifiedChain` |
| `NFR-29` | Defence in depth for tenant isolation | **—** | `internal/infrastructure/postgres/tenant.go`, `internal/platform/runtime/auth.go`, `internal/platform/tenantctx/tenantctx.go` | `internal/application/merchant/service_test.go::TestGetRefusesAMerchantFromAnotherTenantAsNotFound`, `internal/application/payment/service_test.go::TestCreateAssertsTenantContext`, `internal/infrastructure/postgres/rls_integration_test.go::TestCrossTenantAccessIsImpossible`, `internal/infrastructure/postgres/rls_integration_test.go::TestPartitionsCarryRLSAndTheI3Index`, +3 more |
| `NFR-30` | Cryptography standards | **—** | `internal/platform/authn/jwt.go` | `internal/platform/authn/jwt_test.go::TestAttackMatrix` |
| `NFR-31` | Key and credential rotation | **—** | `internal/domain/tenant/apiclient.go`, `internal/platform/authn/apikey.go` | `internal/domain/tenant/tenant_test.go::TestRotationOverlap`, `internal/platform/authn/apikey_test.go::TestAPIKeyRotationOverlap` |
| `NFR-32` | Secrets never leave the secrets boundary | **—** | `internal/application/payment/gateway_resolver.go`, `internal/infrastructure/secrets/awssm.go`, `internal/infrastructure/secrets/credentials.go`, `internal/infrastructure/secrets/file.go`, +8 more | `internal/application/payment/components_test.go::TestCredentialsAreResolvedPerCallAndNeverCached`, `internal/domain/gateway/connection_test.go::TestSecretRefValidation`, `internal/platform/secret/secret_test.go::TestSecretRedactsEveryFormattingVerb`, `internal/workflows/onboarding/onboarding_test.go::TestStoreCredentialsOutputCarriesNoMaterial` |
| `NFR-33` | Sensitive-data ingress prevention | **—** | `internal/platform/secret/pan.go` | `internal/infrastructure/postgres/invariants_integration_test.go::TestPANTripwireRejectsABareCardNumber`, `internal/platform/secret/pan_test.go::TestContainsPANDetectsEveryScheme` |
| `NFR-34` | Least privilege and separation of duties | **—** | `internal/platform/authz/dualcontrol.go`, `internal/platform/authz/policy.go` | `internal/domain/compliance/screening_test.go::TestHumanDispositionRequiresDualControlAndAReason`, `internal/platform/authz/authz_test.go::TestDefaultDeny`, `internal/platform/authz/authz_test.go::TestDualControl` |
| `NFR-35` | Supply chain and vulnerability management | **—** | **—** | **—** |
| `NFR-36` | Rate limiting and abuse resistance | **—** | `internal/infrastructure/resilience/ratelimiter.go` | `internal/infrastructure/resilience/ratelimiter_test.go::TestTokenBucketBurstThenRate` |
| `NFR-37` | Data residency | **—** | `internal/domain/tenant/tenant.go` | `internal/domain/tenant/tenant_test.go::TestResidencyPermitsGatewayRegion` |
| `NFR-38` | GDPR — lawful basis, minimisation and erasure | **—** | `internal/domain/compliance/retention.go` | `internal/domain/compliance/retention_test.go::TestErasureCarveOut` |
| `NFR-39` | PCI DSS scope containment | **—** | `internal/platform/secret/pan.go` | `internal/infrastructure/postgres/invariants_integration_test.go::TestPANTripwireRejectsABareCardNumber`, `internal/platform/secret/pan_test.go::TestContainsPANDetectsEveryScheme`, `internal/platform/secret/pan_test.go::TestPANDetectorNeverLogsTheValue` |
| `NFR-40` | PSD2 / SCA compliance | **—** | `internal/domain/risk/engine.go` | `internal/domain/risk/engine_test.go::TestAnExemptionCannotWaiveRiskDrivenAuthentication` |
| `NFR-41` | AML/KYC evidence retention | **—** | `internal/domain/compliance/attestation.go`, `internal/domain/compliance/screening.go` | `internal/domain/compliance/attestation_test.go::TestRetentionIsDerivedFromTheDocumentType`, `internal/domain/compliance/screening_test.go::TestConfirmedMatchIsNotOverridableByAutomation`, `internal/domain/compliance/screening_test.go::TestHumanDispositionRequiresDualControlAndAReason` |
| `NFR-42` | Records retention and legal hold | **—** | `internal/domain/compliance/attestation.go`, `internal/domain/compliance/retention.go` | `internal/domain/compliance/attestation_test.go::TestRetentionIsDerivedFromTheDocumentType`, `internal/domain/compliance/retention_test.go::TestErasureRequestCompleteRespectsTheCarveOut` |
| `NFR-43` | Mandatory telemetry context | **—** | `internal/infrastructure/telemetry/logging.go`, `internal/infrastructure/telemetry/tracing.go` | `internal/infrastructure/telemetry/logging_test.go::TestLoggerBindsContextFields`, `internal/infrastructure/telemetry/tracing_test.go::TestTraceIDFromContext` |
| `NFR-44` | Metric cardinality control | **—** | `internal/infrastructure/telemetry/metrics.go` | `internal/infrastructure/telemetry/metrics_test.go::TestCardinalityGuardRejectsForbiddenLabels` |
| `NFR-45` | RED plus business metrics coverage | **—** | `internal/infrastructure/telemetry/metrics.go` | `internal/infrastructure/telemetry/metrics_test.go::TestEveryBaselineMetricIsRegistered` |
| `NFR-46` | Trace coverage and sampling | **—** | `internal/infrastructure/telemetry/tracing.go` | `internal/infrastructure/telemetry/tracing_test.go::TestSamplerAlwaysKeepsImportantSpans`, `internal/infrastructure/telemetry/tracing_test.go::TestTraceIDFromContext` |
| `NFR-47` | Runbook coverage | **—** | **—** | **—** |
| `NFR-48` | Deployment cadence and safety | **—** | **—** | **—** |
| `NFR-49` | Rollback time | **—** | **—** | **—** |
| `NFR-50` | Toil budget | **—** | **—** | **—** |
| `NFR-51` | Capacity planning cadence | **—** | **—** | **—** |
| `NFR-52` | Test coverage gates | **—** | **—** | **—** |
| `NFR-53` | Architecture fitness functions | **—** | **—** | **—** |
| `NFR-54` | Contract stability and versioning | **—** | **—** | **—** |
| `NFR-55` | Code health and dependency hygiene | **—** | **—** | **—** |
| `NFR-56` | 12-factor conformance | **—** | **—** | **—** |
| `NFR-57` | Substitutability of infrastructure and vendors | **—** | **—** | **—** |
| `NFR-58` | Unit infrastructure cost per 1 000 payments | **—** | **—** | **—** |
| `NFR-59` | Cost attribution and tenant-level unit economics | **—** | **—** | **—** |
| `NFR-60` | WCAG 2.1 AA for admin and operator surfaces | **—** | **—** | **—** |
| `NFR-61` | API ergonomics as an accessibility concern | **—** | **—** | **—** |

## Coverage summary

| Class | Defined | With a test | With code or design | Orphans |
|---|---|---|---|---|
| BR | 38 | 35 | 35 | 3 |
| FR | 91 | 81 | 82 | 10 |
| NFR | 61 | 30 | 21 | 31 |
| **Total** | **190** | **146** | **138** | **44**  |

<!-- generated by scripts/traceability.sh; the date is deliberately omitted so
     that a regeneration with no substantive change produces no diff -->
