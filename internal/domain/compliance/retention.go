package compliance

import (
	"sort"
	"strings"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// The data classes of the retention schedule in docs/compliance.md §6.
//
// They are constants rather than free-form strings because they are the join key between three
// separate things that must agree: this table, the machine-readable `config/retention-policy.yaml`
// the retention job reads, and the storage-tier lifecycle configuration. A typo in any of them
// is either data deleted early (a compliance failure) or data kept forever (a different one),
// and neither announces itself.
const (
	DataClassPayments             = "payments"
	DataClassLedgerEntries        = "ledger_entries"
	DataClassAuditRecords         = "audit_records"
	DataClassKYCEvidence          = "kyc_evidence"
	DataClassScreeningRuns        = "screening_runs"
	DataClassComplianceCases      = "compliance_cases"
	DataClassMerchantBusiness     = "merchant_business_data"
	DataClassMerchantPrincipalPII = "merchant_principal_pii"
	DataClassMerchantContact      = "merchant_contact_data"
	DataClassSupportCorrespond    = "support_correspondence"
	DataClassBankAccountData      = "bank_account_data"
	DataClassConfigVersions       = "configuration_versions"
	DataClassCertificationReports = "certification_reports"
	DataClassIdempotencyRecords   = "idempotency_records"
	DataClassEventLog             = "event_log"
	DataClassOutboxRows           = "outbox_rows"
	DataClassApplicationLogs      = "application_logs"
	DataClassSecurityEvents       = "security_events"
	DataClassTraces               = "traces"
	DataClassMetrics              = "metrics"
	DataClassProjections          = "projections_and_caches"
	DataClassBackups              = "backups"
	DataClassCloudTrail           = "cloudtrail"
	DataClassAccessAttestations   = "access_review_attestations"
	DataClassDSARRecords          = "dsar_records"
	DataClassErasureCertificates  = "erasure_certificates"
)

// StorageTier names where a data class lives, which determines how deletion is actually
// effected: a row in Aurora is deleted with SQL, an object under Object Lock cannot be deleted
// at all before expiry, and a Kafka topic is deleted only by its retention setting.
type StorageTier string

const (
	// TierPrimary is the transactional database.
	TierPrimary StorageTier = "PRIMARY_DATABASE"
	// TierArchive is object storage without a lock: deletable, lifecycle-managed.
	TierArchive StorageTier = "OBJECT_ARCHIVE"
	// TierWORM is object storage under Object Lock in Compliance mode. Nothing, including the
	// root principal, can delete or shorten retention before expiry — which is exactly why it
	// is used for evidence and exactly why it must never be used for anything a data subject
	// might have a right to erase.
	TierWORM StorageTier = "WORM_OBJECT_LOCK"
	// TierStream is the event log.
	TierStream StorageTier = "EVENT_STREAM"
	// TierObservability is logs, traces and metrics.
	TierObservability StorageTier = "OBSERVABILITY"
	// TierBackup is snapshots and point-in-time recovery.
	TierBackup StorageTier = "BACKUP"
)

// DeletionMechanism is how data in a class is actually destroyed.
//
// The three mechanisms are not interchangeable, and choosing the wrong one produces a deletion
// that did not happen. HARD_DELETE on data that also exists in a WORM export deletes the copy
// nobody was worried about. CRYPTO_SHRED on data whose key is shared with records under a legal
// hold destroys evidence the platform is obliged to keep.
type DeletionMechanism string

const (
	// MechanismCryptoShred destroys the key rather than the ciphertext. It is the only
	// mechanism that works across backups, snapshots, replicas and archives simultaneously —
	// which matters because "delete the row" leaves the row in thirty-five days of point-in-time
	// recovery, and no amount of SQL reaches into a snapshot.
	MechanismCryptoShred DeletionMechanism = "CRYPTO_SHRED"
	// MechanismHardDelete removes the rows or objects.
	MechanismHardDelete DeletionMechanism = "HARD_DELETE"
	// MechanismWORMExpire waits for the Object Lock retention to run out. There is no earlier
	// path, by construction: that is the property being bought.
	MechanismWORMExpire DeletionMechanism = "WORM_EXPIRE"
)

// RetentionClass is one row of the retention schedule, as code.
//
// A schedule that lives only in a document is a schedule nothing enforces, and the failure is
// asymmetric: nobody notices data being kept too long until an auditor asks, and nobody notices
// data being deleted too early until it is needed. Keeping the table here, with
// TestRetentionPolicyMatchesDocumentation asserting it agrees with the published version, makes
// the schedule a thing the code obeys rather than a thing the code is supposed to obey.
type RetentionClass struct {
	// Name is the data class this row governs.
	Name string
	// Years and Days express the retention period. They are separate, rather than a single
	// time.Duration, because a calendar year is not 365 days and a seven-year Object Lock that
	// expires two days early because leap days were rounded away is a compliance failure that
	// no test would catch. Expiry does the calendar arithmetic; Duration is the nominal value
	// for display and metrics.
	Years int
	Days  int
	// Permanent marks a class that is never deleted.
	Permanent bool
	// Tier is where the data lives.
	Tier StorageTier
	// Mechanism is how it is destroyed when its time comes.
	Mechanism DeletionMechanism
	// LegalBasis is why the period is what it is. It is a sentence, not a citation code,
	// because it is quoted verbatim to data subjects and to auditors, and because a bare
	// article number does not explain anything to the person who has to act on it.
	LegalBasis string
	// erasable records whether a data subject's erasure request reaches this class. See
	// ErasureRequest.Erasable.
	erasable bool
	// retentionReason is the sentence given to a data subject when a class survives their
	// erasure request. Empty for erasable classes.
	retentionReason string
}

// Duration returns the nominal retention period, treating a year as 365 days. It is for display,
// metrics and comparisons; use Expiry for any decision about when data may actually go.
func (c RetentionClass) Duration() time.Duration {
	return time.Duration(c.Years)*365*24*time.Hour + time.Duration(c.Days)*24*time.Hour
}

// IsPermanent reports whether the class is never deleted.
func (c RetentionClass) IsPermanent() bool { return c.Permanent }

// Expiry returns the instant at which data in this class may be deleted, computed on the
// calendar rather than by adding a duration. A zero value means permanent.
func (c RetentionClass) Expiry(from time.Time) time.Time {
	if c.Permanent {
		return time.Time{}
	}
	t := from.UTC()
	if c.Years != 0 {
		t = t.AddDate(c.Years, 0, 0)
	}
	if c.Days != 0 {
		t = t.AddDate(0, 0, c.Days)
	}
	return t
}

// retentionSchedule is the authoritative in-code copy of docs/compliance.md §6.
//
// The uniform seven-year figure across the financial classes is deliberate: the underlying
// obligations range from five to ten years depending on jurisdiction, and applying the longest
// commonly-required period uniformly is simpler to operate and to explain than a per-tenant
// matrix that has to be right for every jurisdiction a merchant might be in. The cost is
// storage; the alternative cost is a jurisdiction-specific bug that deletes evidence.
var retentionSchedule = map[string]RetentionClass{
	DataClassPayments: {
		Name: DataClassPayments, Years: 7, Tier: TierPrimary, Mechanism: MechanismHardDelete,
		LegalBasis: "payment services regulations and tax/accounting law; GDPR Art. 17(3)(b) legal obligation",
		retentionReason: "transaction records are retained under a legal obligation " +
			"(payment services, tax and accounting law); GDPR Art. 17(3)(b) disapplies erasure",
	},
	DataClassLedgerEntries: {
		Name: DataClassLedgerEntries, Years: 7, Tier: TierPrimary, Mechanism: MechanismHardDelete,
		LegalBasis:      "accounting obligation; the ledger is append-only for its whole life",
		retentionReason: "accounting records are retained under a legal obligation; GDPR Art. 17(3)(b)",
	},
	DataClassAuditRecords: {
		Name: DataClassAuditRecords, Years: 7, Tier: TierWORM, Mechanism: MechanismWORMExpire,
		LegalBasis: "PCI DSS Requirement 10, evidential value, GDPR Art. 17(3)(e)",
		retentionReason: "the audit chain is retained in full: a hash chain with records removed is not a hash " +
			"chain, and the tamper-evidence property is precisely what removal would destroy. Personal data " +
			"inside an audit record is encrypted under a separate retention key and is destroyed on that key's schedule",
	},
	DataClassKYCEvidence: {
		Name: DataClassKYCEvidence, Years: 7, Tier: TierWORM, Mechanism: MechanismWORMExpire,
		LegalBasis:      "AMLD Art. 40 (five years minimum; seven applied to align with the financial schedule)",
		retentionReason: "anti-money-laundering evidence is retained under a legal obligation; GDPR Art. 17(3)(b)",
	},
	DataClassScreeningRuns: {
		Name: DataClassScreeningRuns, Years: 7, Tier: TierWORM, Mechanism: MechanismWORMExpire,
		LegalBasis:      "AMLD; which list version cleared a subject on which date must remain answerable",
		retentionReason: "sanctions screening evidence is retained under a legal obligation; GDPR Art. 17(3)(b)",
	},
	DataClassComplianceCases: {
		Name: DataClassComplianceCases, Years: 7, Tier: TierWORM, Mechanism: MechanismWORMExpire,
		LegalBasis: "AMLD record-keeping for suspicious-activity-adjacent cases; access restricted by tipping-off rules",
		retentionReason: "suspicious-activity-adjacent case records are retained under a legal obligation and are " +
			"excluded from subject access by tipping-off rules",
	},
	DataClassMerchantBusiness: {
		Name: DataClassMerchantBusiness, Years: 7, Tier: TierPrimary, Mechanism: MechanismCryptoShred,
		LegalBasis: "contract plus the legal obligation attaching to the KYB record",
		retentionReason: "business registration data forms part of the customer due-diligence record retained " +
			"under AMLD; GDPR Art. 17(3)(b)",
	},
	DataClassMerchantPrincipalPII: {
		Name: DataClassMerchantPrincipalPII, Years: 7, Tier: TierPrimary, Mechanism: MechanismCryptoShred,
		LegalBasis: "GDPR Art. 6(1)(b) contract and Art. 6(1)(c) legal obligation for the due-diligence subset",
		erasable:   true,
	},
	DataClassMerchantContact: {
		Name: DataClassMerchantContact, Years: 7, Tier: TierPrimary, Mechanism: MechanismCryptoShred,
		LegalBasis: "GDPR Art. 6(1)(b) contract",
		erasable:   true,
	},
	DataClassSupportCorrespond: {
		Name: DataClassSupportCorrespond, Years: 3, Tier: TierPrimary, Mechanism: MechanismCryptoShred,
		LegalBasis: "GDPR Art. 6(1)(f) legitimate interests in supporting the service",
		erasable:   true,
	},
	DataClassBankAccountData: {
		Name: DataClassBankAccountData, Years: 7, Tier: TierPrimary, Mechanism: MechanismCryptoShred,
		LegalBasis: "contract plus accounting retention of settlement instructions",
		retentionReason: "settlement account details form part of the transaction record retained under " +
			"accounting law; GDPR Art. 17(3)(b)",
	},
	DataClassConfigVersions: {
		Name: DataClassConfigVersions, Years: 7, Tier: TierPrimary, Mechanism: MechanismHardDelete,
		LegalBasis:      "change evidence for PCI DSS Requirements 6 and 10",
		retentionReason: "configuration history is change-control evidence required by PCI DSS Requirements 6 and 10",
	},
	DataClassCertificationReports: {
		Name: DataClassCertificationReports, Years: 7, Tier: TierWORM, Mechanism: MechanismWORMExpire,
		LegalBasis:      "evidence of secure integration for the life of the gateway connection plus seven years",
		retentionReason: "certification evidence demonstrates the shared-responsibility split was enforced",
	},
	DataClassIdempotencyRecords: {
		Name: DataClassIdempotencyRecords, Days: 7, Tier: TierPrimary, Mechanism: MechanismHardDelete,
		LegalBasis: "operational; must exceed the longest client retry window",
		erasable:   true,
	},
	DataClassEventLog: {
		Name: DataClassEventLog, Days: 400, Tier: TierStream, Mechanism: MechanismHardDelete,
		LegalBasis: "operational; the audit stream is the exception at 400 days before archive",
		erasable:   true,
	},
	DataClassOutboxRows: {
		Name: DataClassOutboxRows, Days: 1, Tier: TierPrimary, Mechanism: MechanismHardDelete,
		LegalBasis: "operational; deleted once publication is confirmed",
		erasable:   true,
	},
	DataClassApplicationLogs: {
		Name: DataClassApplicationLogs, Days: 400, Tier: TierObservability, Mechanism: MechanismHardDelete,
		LegalBasis: "PCI DSS Requirement 10 (twelve months, three immediately available); no personal data in logs",
		erasable:   true,
	},
	DataClassSecurityEvents: {
		Name: DataClassSecurityEvents, Years: 7, Tier: TierWORM, Mechanism: MechanismWORMExpire,
		LegalBasis:      "PCI DSS Requirement 10 and incident forensics",
		retentionReason: "security event records are retained as forensic evidence under PCI DSS Requirement 10",
	},
	DataClassTraces: {
		Name: DataClassTraces, Days: 30, Tier: TierObservability, Mechanism: MechanismHardDelete,
		LegalBasis: "operational",
		erasable:   true,
	},
	DataClassMetrics: {
		Name: DataClassMetrics, Days: 395, Tier: TierObservability, Mechanism: MechanismHardDelete,
		LegalBasis: "operational and capacity planning; aggregated, not identifying",
		erasable:   true,
	},
	DataClassProjections: {
		Name: DataClassProjections, Days: 30, Tier: TierPrimary, Mechanism: MechanismCryptoShred,
		LegalBasis: "derived data; rebuilt from the primary store",
		erasable:   true,
	},
	DataClassBackups: {
		Name: DataClassBackups, Years: 7, Tier: TierBackup, Mechanism: MechanismCryptoShred,
		LegalBasis: "disaster recovery and the financial retention schedule; keys are destroyed on shred",
		erasable:   true,
	},
	DataClassCloudTrail: {
		Name: DataClassCloudTrail, Years: 7, Tier: TierWORM, Mechanism: MechanismWORMExpire,
		LegalBasis:      "PCI DSS Requirement 10 and account forensics",
		retentionReason: "infrastructure access records are retained as forensic evidence under PCI DSS Requirement 10",
	},
	DataClassAccessAttestations: {
		Name: DataClassAccessAttestations, Years: 7, Tier: TierWORM, Mechanism: MechanismWORMExpire,
		LegalBasis:      "PCI DSS Requirements 7 and 8 evidence",
		retentionReason: "access review attestations are compliance evidence required by PCI DSS Requirements 7 and 8",
	},
	DataClassDSARRecords: {
		Name: DataClassDSARRecords, Years: 3, Tier: TierPrimary, Mechanism: MechanismHardDelete,
		LegalBasis: "demonstrating accountability under GDPR Art. 5(2)",
		retentionReason: "the record of how a data-subject request was handled is itself required to demonstrate " +
			"accountability under GDPR Art. 5(2); erasing it would destroy the proof that the request was honoured",
	},
	DataClassErasureCertificates: {
		Name: DataClassErasureCertificates, Permanent: true, Tier: TierWORM, Mechanism: MechanismWORMExpire,
		LegalBasis: "permanent proof that an erasure was carried out",
		retentionReason: "the erasure certificate is the evidence that this very request was fulfilled; it is " +
			"retained permanently and contains no content beyond the fact, the scope and the date",
	},
}

// RetentionFor returns the retention class governing a data class.
//
// The boolean is not decoration. An unknown data class must not silently default to anything —
// defaulting to "delete" destroys evidence and defaulting to "keep forever" quietly turns a
// storage bug into a GDPR breach. A caller that gets false has found a data class nobody has
// classified, and the correct response is to fail the job and classify it.
func RetentionFor(dataClass string) (RetentionClass, bool) {
	c, ok := retentionSchedule[strings.ToLower(strings.TrimSpace(dataClass))]
	return c, ok
}

// AllDataClasses returns every classified data class, sorted. Used by the retention job, by the
// consistency test against config/retention-policy.yaml, and by the DSAR disclosure builder.
func AllDataClasses() []string {
	out := make([]string, 0, len(retentionSchedule))
	for k := range retentionSchedule {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// RetentionHold is one class of data that survives an erasure request, with the reason and the
// date it will itself be destroyed.
//
// It exists because the disclosure obligation is specific: a data subject must be told what was
// erased, what was retained, under which basis, and when the retained data will go. A vague
// "some data is retained for legal reasons" is not a compliant response, and building the
// disclosure by hand at each call site is how it becomes one.
type RetentionHold struct {
	DataClass  string
	LegalBasis string
	Reason     string
	// RetainedUntil is when this class will itself be deleted. Zero means permanent.
	RetainedUntil time.Time
	Mechanism     DeletionMechanism
}

// ErasureRequest records a GDPR Art. 17 right-to-erasure request and its outcome.
//
// Immutable: the fields are unexported and Complete returns a new value. A request record that
// could be edited would be a record of what someone last said happened rather than of what
// happened, which is the opposite of what an accountability record is for.
type ErasureRequest struct {
	id         string
	tenantID   shared.TenantID
	merchantID shared.MerchantID
	// subjectRef identifies the data subject in the controller's terms. It is a reference, not
	// a name: putting the subject's personal data into the record of their erasure request is a
	// joke that regulators have not found funny.
	subjectRef string

	requestedBy string
	requestedAt time.Time
	dueBy       time.Time

	completedAt *time.Time
	erased      []string
	retained    []RetentionHold
}

// gdprResponseWindow is the Art. 12(3) deadline: one month, extendable to three with notice.
// Expressed in days because that is how the SLA is measured and reported.
const gdprResponseWindowDays = 30

// NewErasureRequestParams are the inputs to recording an erasure request.
type NewErasureRequestParams struct {
	ID          string
	TenantID    shared.TenantID
	MerchantID  shared.MerchantID
	SubjectRef  string
	RequestedBy string
	RequestedAt time.Time
}

// NewErasureRequest records a right-to-erasure request and starts its clock.
func NewErasureRequest(p NewErasureRequestParams, clock shared.Clock) (ErasureRequest, error) {
	if p.TenantID.IsZero() {
		return ErasureRequest{}, apierror.New(apierror.CodeMissingTenantContext,
			"an erasure request requires a tenant")
	}
	if strings.TrimSpace(p.SubjectRef) == "" {
		return ErasureRequest{}, apierror.New(apierror.CodeValidationFailed,
			"an erasure request requires a subject reference").
			WithDetail(apierror.Detail{
				Field: "subjectRef", Code: "MISSING_SUBJECT",
				Message: "identify the subject by reference; do not copy their personal data into this record",
				RuleID:  "L5.ERASURE_SUBJECT_REFERENCED",
			})
	}
	now := clock.Now().UTC()
	requested := p.RequestedAt
	if requested.IsZero() {
		requested = now
	}
	id := p.ID
	if strings.TrimSpace(id) == "" {
		id = string(shared.NewRequestID())
	}
	return ErasureRequest{
		id:          id,
		tenantID:    p.TenantID,
		merchantID:  p.MerchantID,
		subjectRef:  strings.TrimSpace(p.SubjectRef),
		requestedBy: strings.TrimSpace(p.RequestedBy),
		requestedAt: requested.UTC(),
		dueBy:       requested.UTC().AddDate(0, 0, gdprResponseWindowDays),
	}, nil
}

// ID returns the request's identifier.
func (r ErasureRequest) ID() string { return r.id }

// TenantID returns the tenant the request was made against.
func (r ErasureRequest) TenantID() shared.TenantID { return r.tenantID }

// MerchantID returns the merchant the subject relates to, where there is one.
func (r ErasureRequest) MerchantID() shared.MerchantID { return r.merchantID }

// SubjectRef returns the reference identifying the data subject.
func (r ErasureRequest) SubjectRef() string { return r.subjectRef }

// RequestedBy returns the principal that lodged the request.
func (r ErasureRequest) RequestedBy() string { return r.requestedBy }

// RequestedAt returns when the request was lodged.
func (r ErasureRequest) RequestedAt() time.Time { return r.requestedAt }

// DueBy returns the response deadline.
func (r ErasureRequest) DueBy() time.Time { return r.dueBy }

// IsOverdue reports whether the response deadline has passed with no completion.
func (r ErasureRequest) IsOverdue(now time.Time) bool {
	return r.completedAt == nil && now.UTC().After(r.dueBy)
}

// CompletedAt returns when the request was fulfilled, if it has been.
func (r ErasureRequest) CompletedAt() *time.Time { return r.completedAt }

// Erased returns the data classes that were destroyed.
func (r ErasureRequest) Erased() []string { return append([]string(nil), r.erased...) }

// Retained returns the disclosure of what survived the request and why.
func (r ErasureRequest) Retained() []RetentionHold {
	return append([]RetentionHold(nil), r.retained...)
}

// Erasable reports whether a data class is reached by this erasure request, and — when it is
// not — the legal basis the data subject must be told.
//
// # Why "delete everything" is the wrong answer, and is itself a compliance failure
//
// The intuitive reading of Art. 17 is that a data subject can require the erasure of their
// data, full stop, and that a system which cannot do that is non-compliant. The opposite is
// closer to true. Art. 17(3)(b) disapplies the right where processing is necessary for
// compliance with a legal obligation, and Art. 17(3)(e) where it is necessary for the
// establishment, exercise or defence of legal claims. Payment records fall under payment
// services regulations and tax and accounting law; customer due-diligence evidence falls under
// AMLD Art. 40; dispute evidence falls under 17(3)(e) with scheme windows running to 540 days.
// A platform that honoured a blanket "delete everything" would be destroying records it is
// legally obliged to keep — a breach of the obligations that disapply the right in the first
// place, and, for the AML records, potentially an offence in its own right.
//
// The audit chain is the sharpest case. It cannot have records removed at all, ever, for a
// reason that has nothing to do with retention policy: a hash chain with records removed is not
// a hash chain. Deleting one record breaks every digest after it, which destroys the
// tamper-evidence property for the entire tenant — the property the whole control exists to
// provide, and the one an auditor relies on. So the personal data *inside* an audit record is
// encrypted under a separate retention key and destroyed on that key's own schedule, leaving
// the record verifiable and the personal data gone. That is the shape of the right answer
// everywhere here: erase what can be erased, retain what must be retained, encrypt the personal
// data inside the retained records so it too has an end date, and tell the subject precisely
// which is which.
//
// So the correct answer is neither "delete everything" nor "we keep everything for legal
// reasons". It is a per-class decision with a stated basis, which is what this method is.
func (r ErasureRequest) Erasable(dataClass string) (bool, string) {
	c, ok := RetentionFor(dataClass)
	if !ok {
		// An unclassified data class is not erasable by default, and the reason says so plainly
		// rather than pretending to a legal basis. Guessing "erasable" here would destroy data
		// nobody has assessed; the correct response is to classify it and re-run.
		return false, "data class " + dataClass + " is not in the retention schedule; " +
			"it must be classified before an erasure decision can be made"
	}
	if c.erasable {
		return true, ""
	}
	return false, c.retentionReason
}

// Plan computes what an erasure request will destroy and what it will retain, across every
// classified data class.
//
// It is computed rather than assembled by hand at each call site because the disclosure to the
// data subject has to be exhaustive and accurate, and an exhaustive list maintained by hand
// diverges from the schedule the retention job actually uses — at which point the platform is
// telling data subjects one thing and doing another.
func (r ErasureRequest) Plan(now time.Time) (erase []string, retain []RetentionHold) {
	for _, dc := range AllDataClasses() {
		ok, reason := r.Erasable(dc)
		if ok {
			erase = append(erase, dc)
			continue
		}
		c, found := RetentionFor(dc)
		if !found {
			retain = append(retain, RetentionHold{DataClass: dc, Reason: reason})
			continue
		}
		retain = append(retain, RetentionHold{
			DataClass:     dc,
			LegalBasis:    c.LegalBasis,
			Reason:        reason,
			RetainedUntil: c.Expiry(now.UTC()),
			Mechanism:     c.Mechanism,
		})
	}
	return erase, retain
}

// Complete records the fulfilment of the request and returns the updated value. The receiver is
// unchanged.
//
// It refuses to record a completion that claims to have erased a class the carve-out retains.
// That refusal is the point of the method: the failure mode it prevents is a well-meaning
// operator or a broken job reporting to a data subject that their transaction records were
// deleted when they were not — a false statement to a data subject about their own rights,
// which is a worse compliance failure than the retention it misreports.
func (r ErasureRequest) Complete(erased []string, clock shared.Clock) (ErasureRequest, error) {
	if r.completedAt != nil {
		return ErasureRequest{}, apierror.Newf(apierror.CodeInvalidStateTransition,
			"erasure request %s is already complete", r.id)
	}
	for _, dc := range erased {
		if ok, reason := r.Erasable(dc); !ok {
			return ErasureRequest{}, apierror.Newf(apierror.CodeValidationFailed,
				"erasure request %s cannot report data class %q as erased: %s", r.id, dc, reason).
				WithDetail(apierror.Detail{
					Field: "erased", Code: "CLASS_UNDER_LEGAL_HOLD",
					Message: reason,
					RuleID:  "L5.ERASURE_RESPECTS_CARVE_OUT",
				})
		}
	}
	now := clock.Now().UTC()
	_, retained := r.Plan(now)
	r.completedAt = &now
	r.erased = append([]string(nil), erased...)
	r.retained = retained
	return r, nil
}

// RehydrateErasureParams carries a persisted erasure request back into the domain.
type RehydrateErasureParams struct {
	ID          string
	TenantID    shared.TenantID
	MerchantID  shared.MerchantID
	SubjectRef  string
	RequestedBy string
	RequestedAt time.Time
	DueBy       time.Time
	CompletedAt *time.Time
	Erased      []string
	Retained    []RetentionHold
}

// RehydrateErasureRequest reconstructs an ErasureRequest from persisted state.
func RehydrateErasureRequest(p RehydrateErasureParams) ErasureRequest {
	var completed *time.Time
	if p.CompletedAt != nil {
		t := p.CompletedAt.UTC()
		completed = &t
	}
	return ErasureRequest{
		id:          p.ID,
		tenantID:    p.TenantID,
		merchantID:  p.MerchantID,
		subjectRef:  p.SubjectRef,
		requestedBy: p.RequestedBy,
		requestedAt: p.RequestedAt.UTC(),
		dueBy:       p.DueBy.UTC(),
		completedAt: completed,
		erased:      append([]string(nil), p.Erased...),
		retained:    append([]RetentionHold(nil), p.Retained...),
	}
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: BR-32, FR-15, NFR-38, NFR-42.
//
// Retention classes, legal hold, and the erasure carve-out that keeps money records a subject
// cannot erase
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
