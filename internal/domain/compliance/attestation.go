// Package compliance models the regulatory artifacts the platform is obliged to produce and
// keep: evidence documents, screening outcomes, the retention schedule, and the handling of
// erasure requests against records the law requires us to retain.
//
// Three decisions shape everything here.
//
// First, **no document content lives in the domain.** A Document is a reference — a storage
// key, a content hash, who put it there and how long it must stay. KYC evidence is passports,
// bank statements and utility bills; it is the most sensitive data the platform touches, it is
// stored encrypted in object storage under Object Lock, and pulling its bytes into an aggregate
// would put them in memory, in logs, in heap dumps and in every projection anyone later builds
// from the aggregate. The domain reasons about the *existence and integrity* of evidence, which
// is all the domain's questions actually need.
//
// Second, **compliance outcomes are ports, not implementations.** Screening is done by a vendor
// against lists we do not own; the domain models the shape of the answer and the rules about
// what may be done with it, and knows nothing about the vendor.
//
// Third, **retention and erasure are code, not prose.** A retention schedule that lives only in
// a document is a schedule nothing enforces. The table in docs/compliance.md §6 is reproduced
// here as data, and the erasure carve-out is a function rather than a paragraph, so that "may
// this be deleted" has one answer and one place to change it.
//
// This package imports only the standard library, pkg/* and internal/domain/shared, per the
// dependency rule in docs/spec/00-design-baseline.md §4.
package compliance

import (
	"sort"
	"strings"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// DocumentType classifies a compliance artifact.
//
// The type is what drives the retention class, the access restriction and whether the object
// must be written under a WORM lock, so it is a closed enum rather than a free-form label:
// a mistyped type would silently give a KYC document the retention of an application log.
type DocumentType string

const (
	// DocIdentityEvidence is a natural person's identity document (passport, national ID).
	// Highest sensitivity in the platform: it may incidentally reveal special-category data
	// under GDPR Art. 9 (a photograph implies ethnicity), so access is restricted to the
	// automated KYC pipeline and a narrowly-scoped compliance role.
	DocIdentityEvidence DocumentType = "IDENTITY_EVIDENCE"
	// DocRegistrationEvidence is a company registration extract or equivalent KYB evidence.
	DocRegistrationEvidence DocumentType = "REGISTRATION_EVIDENCE"
	// DocProofOfAddress is a utility bill or bank statement evidencing an address.
	DocProofOfAddress DocumentType = "PROOF_OF_ADDRESS"
	// DocBankVerification evidences ownership of the settlement bank account.
	DocBankVerification DocumentType = "BANK_VERIFICATION"
	// DocScreeningReport is a sanctions/PEP/adverse-media screening run's raw output, retained
	// because "which list version cleared this merchant on this date" must be answerable years
	// later and a bare "no hit" is not evidence.
	DocScreeningReport DocumentType = "SCREENING_REPORT"
	// DocCertificationReport evidences that a merchant's integration passed certification before
	// going live — the machine check that the shared-responsibility split is real.
	DocCertificationReport DocumentType = "CERTIFICATION_REPORT"
	// DocDisputeEvidence is the compelling-evidence bundle submitted to defend a chargeback.
	// Retained under GDPR Art. 17(3)(e) — establishment and defence of legal claims — for as
	// long as scheme dispute windows can run, which reaches 540 days.
	DocDisputeEvidence DocumentType = "DISPUTE_EVIDENCE"
	// DocSettlementFile is a raw gateway settlement report, kept as the authoritative input to
	// the reconciliation tie-out.
	DocSettlementFile DocumentType = "SETTLEMENT_FILE"
	// DocAuditExport is a WORM bundle of audit records.
	DocAuditExport DocumentType = "AUDIT_EXPORT"
	// DocErasureCertificate proves an erasure was carried out. Retained permanently: it is the
	// only evidence that a right was honoured, and destroying it on its own schedule would
	// leave the platform unable to demonstrate compliance with the request it satisfied.
	DocErasureCertificate DocumentType = "ERASURE_CERTIFICATE"
)

// documentSpecs ties each document type to the data class whose retention it inherits, and to
// whether it must be written under an immutable lock.
var documentSpecs = map[DocumentType]struct {
	dataClass string
	worm      bool
}{
	DocIdentityEvidence:     {dataClass: DataClassKYCEvidence, worm: true},
	DocRegistrationEvidence: {dataClass: DataClassKYCEvidence, worm: true},
	DocProofOfAddress:       {dataClass: DataClassKYCEvidence, worm: true},
	DocBankVerification:     {dataClass: DataClassKYCEvidence, worm: true},
	DocScreeningReport:      {dataClass: DataClassScreeningRuns, worm: true},
	DocCertificationReport:  {dataClass: DataClassCertificationReports, worm: true},
	DocDisputeEvidence:      {dataClass: DataClassPayments, worm: false},
	DocSettlementFile:       {dataClass: DataClassPayments, worm: true},
	DocAuditExport:          {dataClass: DataClassAuditRecords, worm: true},
	DocErasureCertificate:   {dataClass: DataClassErasureCertificates, worm: true},
}

// IsValid reports whether t is a known document type.
func (t DocumentType) IsValid() bool { _, ok := documentSpecs[t]; return ok }

// String satisfies fmt.Stringer.
func (t DocumentType) String() string { return string(t) }

// DataClass returns the retention data class this document type falls under, which is what ties
// a stored object to the schedule in docs/compliance.md §6.
func (t DocumentType) DataClass() string { return documentSpecs[t].dataClass }

// RequiresWORM reports whether this document type must be written to storage whose retention
// cannot be shortened by any principal, including root.
//
// Object Lock in *Compliance* mode, never Governance: Governance mode can be bypassed by a
// sufficiently privileged principal, which makes it useless as evidence — the whole point of
// the control is that it holds against the person who most wants it not to.
func (t DocumentType) RequiresWORM() bool { return documentSpecs[t].worm }

// ParseDocumentType validates a persisted or transported document type.
func ParseDocumentType(s string) (DocumentType, error) {
	v := DocumentType(strings.ToUpper(strings.TrimSpace(s)))
	if !v.IsValid() {
		return "", apierror.Newf(apierror.CodeValidationFailed, "unknown compliance document type %q", s).
			WithDetail(apierror.Detail{
				Field: "documentType", Code: "UNKNOWN_DOCUMENT_TYPE",
				Message: "the document type drives retention and access control and may not be invented at the call site",
				RuleID:  "L5.COMPLIANCE_DOCUMENT_TYPE_KNOWN",
			})
	}
	return v, nil
}

// AllDocumentTypes returns the document types, sorted.
func AllDocumentTypes() []DocumentType {
	out := make([]DocumentType, 0, len(documentSpecs))
	for t := range documentSpecs {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// sha256HexLen is the length of a hex-encoded SHA-256 digest.
const sha256HexLen = 64

// Document is a reference to a compliance artifact held in object storage.
//
// Immutable by construction: unexported fields, no setters. A document reference that can be
// repointed is a document reference that proves nothing — the whole value of the record is that
// the object it names, with the content hash it names, was uploaded by that principal at that
// time. Replacing a document is uploading a new one and superseding the old, which leaves both.
//
// The content hash is why the reference is worth keeping at all. Object storage can be
// re-uploaded to; a bucket policy can be changed; a lifecycle rule can be misconfigured. The
// hash recorded here, inside a database that is itself audited and hash-chained, is what makes
// "the passport we verified is the passport in the bucket" a check rather than an assumption.
type Document struct {
	id         string
	tenantID   shared.TenantID
	merchantID shared.MerchantID

	docType DocumentType
	// storageKey is the object-storage key. It is a reference, not a URL: URLs carry
	// credentials, regions and expiry, none of which belong in a seven-year record.
	storageKey string
	// contentHash is the lowercase hex SHA-256 of the object's bytes.
	contentHash string
	sizeBytes   int64
	mediaType   string

	uploadedBy string
	uploadedAt time.Time

	retentionUntil time.Time
	worm           bool
}

// NewDocumentParams are the inputs to recording a document.
type NewDocumentParams struct {
	ID          string
	TenantID    shared.TenantID
	MerchantID  shared.MerchantID
	Type        DocumentType
	StorageKey  string
	ContentHash string
	SizeBytes   int64
	MediaType   string
	UploadedBy  string
	// RetentionUntil may be left zero, in which case it is derived from the type's retention
	// class. Passing it explicitly is for ingesting historical records whose clock started
	// before this code existed.
	RetentionUntil time.Time
}

// NewDocument records a compliance artifact, deriving the retention deadline from the document
// type where the caller does not supply one.
//
// It refuses a document with no content hash. A stored object nobody hashed is an object whose
// integrity cannot be asserted later, and an evidence store that cannot assert integrity is a
// filing cabinet.
func NewDocument(p NewDocumentParams, clock shared.Clock) (Document, error) {
	if p.TenantID.IsZero() {
		return Document{}, apierror.New(apierror.CodeMissingTenantContext, "a compliance document requires a tenant")
	}
	if !p.Type.IsValid() {
		return Document{}, apierror.Newf(apierror.CodeValidationFailed,
			"unknown compliance document type %q", p.Type).
			WithDetail(apierror.Detail{
				Field: "documentType", Code: "UNKNOWN_DOCUMENT_TYPE",
				Message: "the document type drives retention and access control",
				RuleID:  "L5.COMPLIANCE_DOCUMENT_TYPE_KNOWN",
			})
	}
	if strings.TrimSpace(p.StorageKey) == "" {
		return Document{}, apierror.New(apierror.CodeValidationFailed,
			"a compliance document requires a storage key").
			WithDetail(apierror.Detail{
				Field: "storageKey", Code: "MISSING_STORAGE_KEY",
				Message: "the domain holds a reference to the artifact, never its content",
				RuleID:  "L5.COMPLIANCE_DOCUMENT_REFERENCED",
			})
	}
	hash := strings.ToLower(strings.TrimSpace(p.ContentHash))
	if !isHex(hash, sha256HexLen) {
		return Document{}, apierror.New(apierror.CodeValidationFailed,
			"a compliance document requires a hex-encoded SHA-256 content hash").
			WithDetail(apierror.Detail{
				Field: "contentHash", Code: "INVALID_CONTENT_HASH",
				Message: "must be 64 lowercase hex characters; without it the artifact's integrity cannot be asserted later",
				RuleID:  "L5.COMPLIANCE_DOCUMENT_HASHED",
			})
	}
	if strings.TrimSpace(p.UploadedBy) == "" {
		return Document{}, apierror.New(apierror.CodeValidationFailed,
			"a compliance document requires an uploading principal").
			WithDetail(apierror.Detail{
				Field: "uploadedBy", Code: "MISSING_UPLOADER",
				Message: "evidence with no provenance is not evidence",
				RuleID:  "L5.COMPLIANCE_DOCUMENT_ATTRIBUTED",
			})
	}

	now := clock.Now().UTC()
	until := p.RetentionUntil
	if until.IsZero() {
		class, ok := RetentionFor(p.Type.DataClass())
		if !ok {
			return Document{}, apierror.Newf(apierror.CodeInternalError,
				"document type %s maps to unknown data class %q", p.Type, p.Type.DataClass())
		}
		until = class.Expiry(now)
	}
	if !until.IsZero() && until.Before(now) {
		return Document{}, apierror.New(apierror.CodeValidationFailed,
			"a compliance document cannot be recorded with a retention deadline in the past").
			WithDetail(apierror.Detail{
				Field: "retentionUntil", Code: "RETENTION_IN_PAST",
				Message: "an artifact that is already expired on arrival was filed under the wrong class",
				RuleID:  "L5.COMPLIANCE_RETENTION_FUTURE",
			})
	}

	id := p.ID
	if strings.TrimSpace(id) == "" {
		id = string(shared.NewRequestID())
	}
	return Document{
		id:             id,
		tenantID:       p.TenantID,
		merchantID:     p.MerchantID,
		docType:        p.Type,
		storageKey:     strings.TrimSpace(p.StorageKey),
		contentHash:    hash,
		sizeBytes:      p.SizeBytes,
		mediaType:      strings.TrimSpace(p.MediaType),
		uploadedBy:     strings.TrimSpace(p.UploadedBy),
		uploadedAt:     now,
		retentionUntil: until.UTC(),
		worm:           p.Type.RequiresWORM(),
	}, nil
}

// Accessors. No setters; see the type comment.

// ID returns the document reference's identifier.
func (d Document) ID() string { return d.id }

// TenantID returns the owning tenant.
func (d Document) TenantID() shared.TenantID { return d.tenantID }

// MerchantID returns the merchant the document evidences, where there is one.
func (d Document) MerchantID() shared.MerchantID { return d.merchantID }

// Type returns the document's classification.
func (d Document) Type() DocumentType { return d.docType }

// StorageKey returns the object-storage key.
func (d Document) StorageKey() string { return d.storageKey }

// ContentHash returns the hex SHA-256 of the object's bytes.
func (d Document) ContentHash() string { return d.contentHash }

// SizeBytes returns the object's size.
func (d Document) SizeBytes() int64 { return d.sizeBytes }

// MediaType returns the object's media type.
func (d Document) MediaType() string { return d.mediaType }

// UploadedBy returns the principal that uploaded the artifact.
func (d Document) UploadedBy() string { return d.uploadedBy }

// UploadedAt returns when the artifact was recorded.
func (d Document) UploadedAt() time.Time { return d.uploadedAt }

// RetentionUntil returns the instant before which the artifact may not be deleted. A zero value
// means permanent retention.
func (d Document) RetentionUntil() time.Time { return d.retentionUntil }

// IsWORM reports whether the artifact is held under an immutable lock.
func (d Document) IsWORM() bool { return d.worm }

// IsPermanent reports whether the artifact is never to be deleted.
func (d Document) IsPermanent() bool { return d.retentionUntil.IsZero() }

// MatchesContent reports whether the given hex digest is the one recorded for this artifact.
// This is the check that turns the stored hash into a control: object storage is re-uploadable
// and lifecycle rules are misconfigurable, so a periodic re-hash compared against this value is
// what detects an artifact that changed underneath its reference.
func (d Document) MatchesContent(hexDigest string) bool {
	return d.contentHash != "" && d.contentHash == strings.ToLower(strings.TrimSpace(hexDigest))
}

// Deletable reports whether the artifact's retention period has run, and if not, why.
//
// It answers "may this be deleted", not "is this old" — the difference being that a permanent
// artifact and a WORM-locked artifact inside its window both answer no for reasons a caller
// must be able to distinguish and log.
func (d Document) Deletable(now time.Time) (bool, string) {
	if d.IsPermanent() {
		return false, "retained permanently: this artifact is the evidence that an obligation was discharged"
	}
	if now.UTC().Before(d.retentionUntil) {
		return false, "retention period runs until " + d.retentionUntil.Format(time.RFC3339)
	}
	return true, ""
}

// RehydrateDocumentParams carries a persisted document reference back into the domain.
type RehydrateDocumentParams struct {
	ID             string
	TenantID       shared.TenantID
	MerchantID     shared.MerchantID
	Type           DocumentType
	StorageKey     string
	ContentHash    string
	SizeBytes      int64
	MediaType      string
	UploadedBy     string
	UploadedAt     time.Time
	RetentionUntil time.Time
	WORM           bool
}

// RehydrateDocument reconstructs a Document from persisted state, refusing a row whose type this
// binary does not know — a rollback landing on data written by a newer version must fail loudly
// rather than be coerced into the nearest known type and given the wrong retention.
func RehydrateDocument(p RehydrateDocumentParams) (Document, error) {
	if !p.Type.IsValid() {
		return Document{}, apierror.Newf(apierror.CodeInternalError,
			"compliance document %s has unknown type %q; this row may have been written by a newer version of the service",
			p.ID, p.Type)
	}
	return Document{
		id:             p.ID,
		tenantID:       p.TenantID,
		merchantID:     p.MerchantID,
		docType:        p.Type,
		storageKey:     p.StorageKey,
		contentHash:    strings.ToLower(p.ContentHash),
		sizeBytes:      p.SizeBytes,
		mediaType:      p.MediaType,
		uploadedBy:     p.UploadedBy,
		uploadedAt:     p.UploadedAt.UTC(),
		retentionUntil: p.RetentionUntil.UTC(),
		worm:           p.WORM,
	}, nil
}

func isHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return false
	}
	return true
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: NFR-41, NFR-42.
//
// Compliance attestations and documents, held by reference and hash, under a retention class
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
