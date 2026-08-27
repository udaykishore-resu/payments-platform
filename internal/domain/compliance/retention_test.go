package compliance

import (
	"strings"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
)

const (
	testTenant   = shared.TenantID("ten_01JB8Z9K2QW3E4R5T6Y7U8I9O0")
	testMerchant = shared.MerchantID("mrc_01JB8Z9K2QW3E4R5T6Y7U8I9O0")
)

func testClock() shared.Clock {
	return shared.FixedClock{T: time.Date(2026, 3, 3, 9, 14, 22, 0, time.UTC)}
}

func newTestErasureRequest(t *testing.T) ErasureRequest {
	t.Helper()
	r, err := NewErasureRequest(NewErasureRequestParams{
		TenantID: testTenant, MerchantID: testMerchant,
		SubjectRef: "subject-ref-7f2a", RequestedBy: "usr_01JB8Z9K2QW3E4R5T6Y7U8I9O0",
	}, testClock())
	if err != nil {
		t.Fatalf("NewErasureRequest: %v", err)
	}
	return r
}

// TestErasureCarveOut is the compliance test that matters most in this package: the classes the
// law requires the platform to keep must survive an erasure request, and each must come back
// with a legal basis the data subject can be told. "Delete everything" is the wrong answer and
// would itself be a compliance failure.
func TestErasureCarveOut(t *testing.T) {
	// Verifies: BR-32, FR-15, NFR-38.
	t.Parallel()

	r := newTestErasureRequest(t)

	tests := []struct {
		dataClass    string
		wantErasable bool
		// wantBasisContains is a phrase the disclosure must contain, so that the reason given to
		// a data subject is specific rather than "some data is retained for legal reasons".
		wantBasisContains string
	}{
		{DataClassPayments, false, "legal obligation"},
		{DataClassLedgerEntries, false, "accounting"},
		{DataClassAuditRecords, false, "hash chain"},
		{DataClassKYCEvidence, false, "money-laundering"},
		{DataClassScreeningRuns, false, "sanctions screening"},
		{DataClassComplianceCases, false, "tipping-off"},
		{DataClassBankAccountData, false, "accounting law"},
		{DataClassConfigVersions, false, "PCI DSS"},
		{DataClassCertificationReports, false, "certification"},
		{DataClassSecurityEvents, false, "forensic"},
		{DataClassCloudTrail, false, "forensic"},
		{DataClassAccessAttestations, false, "PCI DSS"},
		{DataClassMerchantBusiness, false, "due-diligence"},
		{DataClassDSARRecords, false, "accountability"},
		{DataClassErasureCertificates, false, "this very request"},

		{DataClassMerchantPrincipalPII, true, ""},
		{DataClassMerchantContact, true, ""},
		{DataClassSupportCorrespond, true, ""},
		{DataClassApplicationLogs, true, ""},
		{DataClassTraces, true, ""},
		{DataClassMetrics, true, ""},
		{DataClassProjections, true, ""},
		{DataClassBackups, true, ""},
		{DataClassIdempotencyRecords, true, ""},
		{DataClassEventLog, true, ""},
		{DataClassOutboxRows, true, ""},
	}
	for _, tc := range tests {

		t.Run(tc.dataClass, func(t *testing.T) {
			t.Parallel()
			ok, basis := r.Erasable(tc.dataClass)
			if ok != tc.wantErasable {
				t.Fatalf("Erasable(%q) = %v, want %v (basis %q)", tc.dataClass, ok, tc.wantErasable, basis)
			}
			if tc.wantErasable {
				if basis != "" {
					t.Errorf("an erasable class returned a retention basis: %q", basis)
				}
				return
			}
			if basis == "" {
				t.Fatalf("class %q is retained with no stated legal basis", tc.dataClass)
			}
			if !strings.Contains(strings.ToLower(basis), strings.ToLower(tc.wantBasisContains)) {
				t.Errorf("basis for %q = %q, want it to mention %q", tc.dataClass, basis, tc.wantBasisContains)
			}
		})
	}
}

// TestErasableFailsClosedOnUnknownClass — an unclassified data class must not be assumed
// erasable. Guessing "delete" here destroys data nobody has assessed.
func TestErasableFailsClosedOnUnknownClass(t *testing.T) {
	t.Parallel()

	r := newTestErasureRequest(t)
	ok, reason := r.Erasable("shiny_new_projection")
	if ok {
		t.Fatal("Erasable defaulted an unclassified data class to erasable")
	}
	if !strings.Contains(reason, "not in the retention schedule") {
		t.Fatalf("reason = %q, want one saying the class is unclassified", reason)
	}
	if _, found := RetentionFor("shiny_new_projection"); found {
		t.Fatal("RetentionFor invented a class")
	}
}

func TestEveryDataClassIsClassified(t *testing.T) {
	t.Parallel()

	classes := AllDataClasses()
	if len(classes) < 20 {
		t.Fatalf("retention schedule has only %d classes; docs/compliance.md §6 lists more", len(classes))
	}
	for _, dc := range classes {

		t.Run(dc, func(t *testing.T) {
			t.Parallel()
			c, ok := RetentionFor(dc)
			if !ok {
				t.Fatalf("AllDataClasses returned %q but RetentionFor does not know it", dc)
			}
			if c.Name != dc {
				t.Errorf("class name = %q, want %q", c.Name, dc)
			}
			if c.LegalBasis == "" {
				t.Error("class has no stated legal basis")
			}
			if c.Tier == "" || c.Mechanism == "" {
				t.Errorf("class has tier %q and mechanism %q; both are required", c.Tier, c.Mechanism)
			}
			if !c.Permanent && c.Years == 0 && c.Days == 0 {
				t.Error("a non-permanent class with no retention period would be deleted immediately")
			}
			// WORM storage can only be released by expiry, and nothing under it can be erased on
			// request. A class that claimed otherwise would promise a data subject something the
			// storage layer physically cannot do.
			if c.Tier == TierWORM && c.Mechanism != MechanismWORMExpire {
				t.Errorf("class on WORM storage declares mechanism %q", c.Mechanism)
			}
			if c.Tier == TierWORM && c.erasable {
				t.Error("class on WORM storage claims to be erasable on request; Object Lock cannot honour that")
			}
		})
	}
}

func TestRetentionForIsCaseAndSpaceInsensitive(t *testing.T) {
	t.Parallel()

	for _, spelling := range []string{"payments", "PAYMENTS", "  Payments  "} {
		c, ok := RetentionFor(spelling)
		if !ok || c.Name != DataClassPayments {
			t.Fatalf("RetentionFor(%q) = (%v, %v)", spelling, c.Name, ok)
		}
	}
}

// TestExpiryUsesTheCalendar — a retention deadline computed by adding 7*365 days lands two days
// early across two leap years, and an Object Lock released two days early is a compliance
// failure no test of the duration itself would catch.
func TestExpiryUsesTheCalendar(t *testing.T) {
	t.Parallel()

	class, ok := RetentionFor(DataClassPayments)
	if !ok {
		t.Fatal("payments class is missing")
	}
	from := time.Date(2024, 2, 29, 12, 0, 0, 0, time.UTC)
	got := class.Expiry(from)
	want := from.AddDate(7, 0, 0)
	if !got.Equal(want) {
		t.Fatalf("Expiry = %s, want %s", got, want)
	}
	if naive := from.Add(class.Duration()); got.Equal(naive) {
		t.Fatal("Expiry agrees with a naive 365-day-year addition across leap years; it should not")
	}

	daily, ok := RetentionFor(DataClassIdempotencyRecords)
	if !ok {
		t.Fatal("idempotency class is missing")
	}
	if got, want := daily.Expiry(from), from.AddDate(0, 0, 7); !got.Equal(want) {
		t.Fatalf("day-scale Expiry = %s, want %s", got, want)
	}

	perm, ok := RetentionFor(DataClassErasureCertificates)
	if !ok {
		t.Fatal("erasure certificate class is missing")
	}
	if !perm.IsPermanent() || !perm.Expiry(from).IsZero() {
		t.Fatal("a permanent class must have no expiry")
	}
}

func TestErasureRequestPlanIsExhaustive(t *testing.T) {
	t.Parallel()

	r := newTestErasureRequest(t)
	erase, retain := r.Plan(testClock().Now())
	if len(erase)+len(retain) != len(AllDataClasses()) {
		t.Fatalf("plan covers %d classes, want all %d", len(erase)+len(retain), len(AllDataClasses()))
	}
	if len(erase) == 0 {
		t.Fatal("plan erases nothing; a right-to-erasure request that deletes nothing is not compliant either")
	}
	for _, hold := range retain {
		if hold.Reason == "" {
			t.Errorf("retained class %q has no reason to disclose", hold.DataClass)
		}
		if hold.LegalBasis == "" {
			t.Errorf("retained class %q has no legal basis", hold.DataClass)
		}
		if hold.DataClass != DataClassErasureCertificates && hold.RetainedUntil.IsZero() {
			t.Errorf("retained class %q does not say when it will itself be deleted", hold.DataClass)
		}
	}
}

func TestErasureRequestCompleteRespectsTheCarveOut(t *testing.T) {
	// Verifies: NFR-42.
	t.Parallel()

	clock := testClock()

	t.Run("rejects a claim to have erased retained records", func(t *testing.T) {
		t.Parallel()
		r := newTestErasureRequest(t)
		_, err := r.Complete([]string{DataClassMerchantContact, DataClassPayments}, clock)
		if err == nil {
			t.Fatal("Complete accepted a claim to have erased transaction records")
		}
		if !strings.Contains(err.Error(), DataClassPayments) {
			t.Errorf("error does not name the offending class: %v", err)
		}
	})

	t.Run("records a lawful completion with its disclosure", func(t *testing.T) {
		t.Parallel()
		r := newTestErasureRequest(t)
		done, err := r.Complete([]string{DataClassMerchantContact, DataClassMerchantPrincipalPII}, clock)
		if err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if done.CompletedAt() == nil {
			t.Fatal("completion time was not recorded")
		}
		if len(done.Erased()) != 2 {
			t.Errorf("Erased() = %v", done.Erased())
		}
		if len(done.Retained()) == 0 {
			t.Fatal("no disclosure of retained data was produced")
		}
		if r.CompletedAt() != nil {
			t.Fatal("Complete mutated the receiver")
		}
		if _, err := done.Complete([]string{DataClassMerchantContact}, clock); err == nil {
			t.Fatal("Complete ran twice on the same request")
		}
	})
}

func TestErasureRequestDeadline(t *testing.T) {
	t.Parallel()

	clock := testClock()
	r := newTestErasureRequest(t)
	if want := clock.Now().AddDate(0, 0, 30); !r.DueBy().Equal(want) {
		t.Fatalf("DueBy = %s, want %s", r.DueBy(), want)
	}
	if r.IsOverdue(clock.Now().AddDate(0, 0, 29)) {
		t.Error("request reported overdue inside the response window")
	}
	if !r.IsOverdue(clock.Now().AddDate(0, 0, 31)) {
		t.Error("request not reported overdue past the response window")
	}
	done, err := r.Complete(nil, clock)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if done.IsOverdue(clock.Now().AddDate(0, 0, 31)) {
		t.Error("a completed request cannot be overdue")
	}
}

func TestNewErasureRequestValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		params  NewErasureRequestParams
		wantErr bool
	}{
		{"valid", NewErasureRequestParams{TenantID: testTenant, SubjectRef: "ref"}, false},
		{"no tenant", NewErasureRequestParams{SubjectRef: "ref"}, true},
		{"no subject", NewErasureRequestParams{TenantID: testTenant}, true},
		{"blank subject", NewErasureRequestParams{TenantID: testTenant, SubjectRef: "   "}, true},
	}
	for _, tc := range tests {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewErasureRequest(tc.params, testClock())
			if tc.wantErr != (err != nil) {
				t.Fatalf("NewErasureRequest() error = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}
