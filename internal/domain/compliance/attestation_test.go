package compliance

import (
	"strings"
	"testing"
	"time"
)

const testHash = "9f2c1b7d3e4a5c6b8d0f1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f"

func validDocumentParams() NewDocumentParams {
	return NewDocumentParams{
		TenantID: testTenant, MerchantID: testMerchant,
		Type: DocIdentityEvidence, StorageKey: "ten_1/kyc/mrc_1/passport.pdf",
		ContentHash: testHash, SizeBytes: 204_800, MediaType: "application/pdf",
		UploadedBy: "svc_kyc_pipeline",
	}
}

func TestNewDocumentValidation(t *testing.T) {
	t.Parallel()

	mutate := func(f func(*NewDocumentParams)) NewDocumentParams {
		p := validDocumentParams()
		f(&p)
		return p
	}

	tests := []struct {
		name    string
		params  NewDocumentParams
		wantErr bool
	}{
		{"valid", validDocumentParams(), false},
		{"no tenant", mutate(func(p *NewDocumentParams) { p.TenantID = "" }), true},
		{"unknown type", mutate(func(p *NewDocumentParams) { p.Type = "SELFIE" }), true},
		{"no storage key", mutate(func(p *NewDocumentParams) { p.StorageKey = " " }), true},
		{"no content hash", mutate(func(p *NewDocumentParams) { p.ContentHash = "" }), true},
		{"short content hash", mutate(func(p *NewDocumentParams) { p.ContentHash = testHash[:63] }), true},
		{"non-hex content hash", mutate(func(p *NewDocumentParams) { p.ContentHash = strings.Repeat("z", 64) }), true},
		{"uppercase hash is normalized", mutate(func(p *NewDocumentParams) { p.ContentHash = strings.ToUpper(testHash) }), false},
		{"no uploader", mutate(func(p *NewDocumentParams) { p.UploadedBy = "" }), true},
		{
			name: "retention deadline already in the past",
			params: mutate(func(p *NewDocumentParams) {
				p.RetentionUntil = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
			}),
			wantErr: true,
		},
	}
	for _, tc := range tests {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			doc, err := NewDocument(tc.params, testClock())
			if tc.wantErr != (err != nil) {
				t.Fatalf("NewDocument() error = %v, wantErr = %v", err, tc.wantErr)
			}
			if err == nil && doc.ContentHash() != testHash {
				t.Errorf("content hash = %q, want the lowercase form %q", doc.ContentHash(), testHash)
			}
		})
	}
}

// TestRetentionIsDerivedFromTheDocumentType — the deadline comes from the schedule, not from the
// caller, so a document filed under the wrong deadline is not something a call site can cause.
func TestRetentionIsDerivedFromTheDocumentType(t *testing.T) {
	// Verifies: NFR-41, NFR-42.
	t.Parallel()

	clock := testClock()
	for _, dt := range AllDocumentTypes() {

		t.Run(string(dt), func(t *testing.T) {
			t.Parallel()

			class, ok := RetentionFor(dt.DataClass())
			if !ok {
				t.Fatalf("document type %s maps to unclassified data class %q", dt, dt.DataClass())
			}
			p := validDocumentParams()
			p.Type = dt
			doc, err := NewDocument(p, clock)
			if err != nil {
				t.Fatalf("NewDocument: %v", err)
			}
			want := class.Expiry(clock.Now())
			if !doc.RetentionUntil().Equal(want) {
				t.Errorf("RetentionUntil = %s, want %s", doc.RetentionUntil(), want)
			}
			if doc.IsWORM() != dt.RequiresWORM() {
				t.Errorf("IsWORM() = %v, want %v", doc.IsWORM(), dt.RequiresWORM())
			}
			if doc.IsPermanent() != class.IsPermanent() {
				t.Errorf("IsPermanent() = %v, want %v", doc.IsPermanent(), class.IsPermanent())
			}
		})
	}
}

func TestDocumentDeletable(t *testing.T) {
	t.Parallel()

	clock := testClock()
	doc, err := NewDocument(validDocumentParams(), clock)
	if err != nil {
		t.Fatalf("NewDocument: %v", err)
	}

	if ok, reason := doc.Deletable(clock.Now()); ok || !strings.Contains(reason, "retention period") {
		t.Fatalf("Deletable inside the window = (%v, %q)", ok, reason)
	}
	if ok, reason := doc.Deletable(doc.RetentionUntil().Add(time.Second)); !ok || reason != "" {
		t.Fatalf("Deletable after the window = (%v, %q)", ok, reason)
	}

	p := validDocumentParams()
	p.Type = DocErasureCertificate
	perm, err := NewDocument(p, clock)
	if err != nil {
		t.Fatalf("NewDocument: %v", err)
	}
	if ok, reason := perm.Deletable(clock.Now().AddDate(100, 0, 0)); ok || !strings.Contains(reason, "permanently") {
		t.Fatalf("a permanent artifact reported deletable: (%v, %q)", ok, reason)
	}
}

func TestDocumentContentIntegrityCheck(t *testing.T) {
	t.Parallel()

	doc, err := NewDocument(validDocumentParams(), testClock())
	if err != nil {
		t.Fatalf("NewDocument: %v", err)
	}
	tests := []struct {
		name string
		hash string
		want bool
	}{
		{"same hash", testHash, true},
		{"same hash uppercased", strings.ToUpper(testHash), true},
		{"padded", "  " + testHash + "  ", true},
		{"different hash", strings.Repeat("a", 64), false},
		{"empty", "", false},
	}
	for _, tc := range tests {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := doc.MatchesContent(tc.hash); got != tc.want {
				t.Fatalf("MatchesContent(%q) = %v, want %v", tc.hash, got, tc.want)
			}
		})
	}
}

func TestDocumentTypeRegistry(t *testing.T) {
	t.Parallel()

	for _, dt := range AllDocumentTypes() {

		t.Run(string(dt), func(t *testing.T) {
			t.Parallel()
			got, err := ParseDocumentType(strings.ToLower(string(dt)))
			if err != nil || got != dt {
				t.Fatalf("ParseDocumentType(%q) = (%q, %v)", dt, got, err)
			}
			if dt.DataClass() == "" {
				t.Fatal("document type has no data class")
			}
		})
	}
	if _, err := ParseDocumentType("SELFIE"); err == nil {
		t.Fatal("ParseDocumentType accepted an unregistered type")
	}
}

func TestRehydrateDocumentRejectsUnknownType(t *testing.T) {
	t.Parallel()

	if _, err := RehydrateDocument(RehydrateDocumentParams{ID: "doc_1", Type: "SELFIE"}); err == nil {
		t.Fatal("RehydrateDocument accepted a type this binary does not know")
	}
	got, err := RehydrateDocument(RehydrateDocumentParams{
		ID: "doc_1", TenantID: testTenant, Type: DocScreeningReport,
		StorageKey: "k", ContentHash: strings.ToUpper(testHash), WORM: true,
		RetentionUntil: time.Date(2033, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("RehydrateDocument: %v", err)
	}
	if got.ContentHash() != testHash {
		t.Errorf("content hash was not normalized on rehydrate: %q", got.ContentHash())
	}
	if !got.IsWORM() {
		t.Error("WORM flag was lost on rehydrate")
	}
}
