package audit

import (
	"strings"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
)

const (
	testTenant = shared.TenantID("ten_01JB8Z9K2QW3E4R5T6Y7U8I9O0")
	testNonce  = "genesis-nonce-01JB8Z9K2QW3E4R5T6Y7U8I9O0"
)

func testClock() shared.Clock {
	return shared.FixedClock{T: time.Date(2026, 3, 3, 9, 14, 22, 881_000_000, time.UTC)}
}

func testActor() Actor {
	return Actor{
		Type: ActorUser, ID: "usr_01JB8Z9K2QW3E4R5T6Y7U8I9O0", Name: "j.okafor",
		IP: "203.0.113.7", UserAgent: "Mozilla/5.0",
	}
}

func mustRecord(t *testing.T, p NewRecordParams) Record {
	t.Helper()
	if p.TenantID == "" {
		p.TenantID = testTenant
	}
	if p.Actor.Type == "" {
		p.Actor = testActor()
	}
	if p.Action == "" {
		p.Action = ActionPaymentCaptured
	}
	if p.Outcome == "" {
		p.Outcome = OutcomeSuccess
	}
	if p.ResourceType == "" {
		p.ResourceType = "payment"
	}
	if p.ResourceID == "" {
		p.ResourceID = "pay_01JB8Z9K2QW3E4R5T6Y7U8I9O0"
	}
	r, err := NewRecord(p, testClock())
	if err != nil {
		t.Fatalf("NewRecord: %v", err)
	}
	return r
}

func mustChain(t *testing.T, n int) *Chain {
	t.Helper()
	c, err := NewChain(testTenant, testNonce)
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}
	for i := 0; i < n; i++ {
		r := mustRecord(t, NewRecordParams{
			ResourceID: "pay_" + strings.Repeat("A", 25) + string(rune('0'+i%10)),
			After:      map[string]any{"amount": int64(1000 + i), "currency": "USD"},
		})
		if err := c.Append(r); err != nil {
			t.Fatalf("Append(%d): %v", i, err)
		}
	}
	return c
}

// TestChainVerifiesIntactChain establishes the baseline the tamper tests measure against.
func TestChainVerifiesIntactChain(t *testing.T) {
	// Verifies: BR-37, FR-89.
	t.Parallel()

	c := mustChain(t, 8)
	ok, idx, err := c.Verify()
	if !ok || idx != -1 || err != nil {
		t.Fatalf("Verify() = (%v, %d, %v), want (true, -1, nil)", ok, idx, err)
	}
	if c.Sequence() != 8 {
		t.Errorf("Sequence() = %d, want 8", c.Sequence())
	}
	if c.Head() == c.Genesis() {
		t.Error("head did not advance past genesis")
	}
	// Each record links to the one before it, and the first to the genesis digest.
	prev := c.Genesis()
	for i, r := range c.Records() {
		if r.PrevDigest() != prev {
			t.Fatalf("record %d links to %q, want %q", i, r.PrevDigest(), prev)
		}
		if r.Sequence() != uint64(i+1) {
			t.Fatalf("record %d has sequence %d, want %d", i, r.Sequence(), i+1)
		}
		prev = r.Digest()
	}
	if prev != c.Head() {
		t.Error("the last record's digest is not the chain head")
	}
}

// TestVerifyFindsTheTamperedRecord is the test the whole package exists for: a record altered in
// the middle of a chain must be found, and Verify must name its exact index — the range between
// the last good record and the first bad one is what an investigation has to reconstruct.
func TestVerifyFindsTheTamperedRecord(t *testing.T) {
	// Verifies: FR-89.
	t.Parallel()

	const chainLen = 9
	for _, tamperAt := range []int{0, 1, 4, chainLen - 1} {

		t.Run("index_"+string(rune('0'+tamperAt)), func(t *testing.T) {
			t.Parallel()

			c := mustChain(t, chainLen)
			// Reach in and edit a field, exactly as an attacker with UPDATE on the table would.
			// The digest and the links are left untouched, which is what makes this the case the
			// chain is designed to catch.
			c.records[tamperAt].reason = "backdated justification"

			ok, idx, err := c.Verify()
			if ok {
				t.Fatal("Verify() accepted a chain with an edited record")
			}
			if idx != tamperAt {
				t.Fatalf("Verify() reported index %d, want %d", idx, tamperAt)
			}
			if err == nil || !strings.Contains(err.Error(), "digest") {
				t.Fatalf("Verify() error = %v, want one naming the digest mismatch", err)
			}
			// Everything before the tampered record still verifies: the chain localizes the
			// damage rather than condemning the whole table.
			for i := 0; i < tamperAt; i++ {
				if c.records[i].ComputeDigest() != c.records[i].digest {
					t.Fatalf("record %d, before the tamper point, does not verify", i)
				}
			}
		})
	}
}

func TestVerifyDetectsStructuralTampering(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		mutate   func(c *Chain)
		wantIdx  int
		wantWord string
	}{
		{
			name: "record removed from the middle",
			mutate: func(c *Chain) {
				c.records = append(c.records[:3], c.records[4:]...)
			},
			wantIdx:  3,
			wantWord: "sequence",
		},
		{
			name: "records reordered",
			mutate: func(c *Chain) {
				c.records[2], c.records[5] = c.records[5], c.records[2]
			},
			wantIdx:  2,
			wantWord: "sequence",
		},
		{
			name: "records truncated from the end",
			mutate: func(c *Chain) {
				c.records = c.records[:len(c.records)-2]
			},
			wantIdx:  6,
			wantWord: "head",
		},
		{
			name: "link rewritten to skip a record",
			mutate: func(c *Chain) {
				c.records[4].prevDigest = c.records[2].digest
			},
			wantIdx:  4,
			wantWord: "link",
		},
	}
	for _, tc := range tests {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := mustChain(t, 8)
			tc.mutate(c)
			ok, idx, err := c.Verify()
			if ok {
				t.Fatal("Verify() accepted a structurally tampered chain")
			}
			if idx != tc.wantIdx {
				t.Errorf("Verify() reported index %d, want %d", idx, tc.wantIdx)
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantWord) {
				t.Errorf("Verify() error = %v, want one mentioning %q", err, tc.wantWord)
			}
		})
	}
}

// TestCanonicalDigestResistsBoundaryShifting is the collision-resistance test: two records whose
// fields differ only in where a delimiter would fall must produce different digests.
//
// Every pair below concatenates to identical bytes under naive concatenation
// (fmt.Sprintf("%s%s%s", …)), and several remain identical under any single-character delimiter
// that a value is allowed to contain. Length-prefixed framing makes all of them distinct.
func TestCanonicalDigestResistsBoundaryShifting(t *testing.T) {
	t.Parallel()

	base := RehydrateRecordParams{
		ID: shared.AuditID("aud_01JB8Z9K2QW3E4R5T6Y7U8I9O0"), TenantID: testTenant, Sequence: 88421,
		Actor:      Actor{Type: ActorUser, ID: "usr_1", Name: "j.okafor"},
		Action:     ActionConfigurationChanged,
		Outcome:    OutcomeSuccess,
		OccurredAt: time.Date(2026, 3, 3, 9, 14, 22, 0, time.UTC),
		RecordedAt: time.Date(2026, 3, 3, 9, 14, 22, 0, time.UTC),
		PrevDigest: strings.Repeat("a", 64),
	}

	tests := []struct {
		name string
		a, b func(p RehydrateRecordParams) RehydrateRecordParams
	}{
		{
			name: "actor name absorbs the resource type",
			a: func(p RehydrateRecordParams) RehydrateRecordParams {
				p.Actor.Name, p.ResourceType, p.ResourceID = "j.okafor", "configuration", "cfv_7"
				return p
			},
			b: func(p RehydrateRecordParams) RehydrateRecordParams {
				p.Actor.Name, p.ResourceType, p.ResourceID = "j.okaforconfig", "uration", "cfv_7"
				return p
			},
		},
		{
			name: "resource type absorbs the resource id",
			a: func(p RehydrateRecordParams) RehydrateRecordParams {
				p.ResourceType, p.ResourceID = "merchant", "mrc_123"
				return p
			},
			b: func(p RehydrateRecordParams) RehydrateRecordParams {
				p.ResourceType, p.ResourceID = "merchantmrc_", "123"
				return p
			},
		},
		{
			name: "a pipe delimiter inside a value shifts the boundary",
			a: func(p RehydrateRecordParams) RehydrateRecordParams {
				p.Actor.Name, p.ResourceType, p.ResourceID = "j.okafor|merchant", "|mrc_123", "x"
				return p
			},
			b: func(p RehydrateRecordParams) RehydrateRecordParams {
				p.Actor.Name, p.ResourceType, p.ResourceID = "j.okafor", "merchant|", "mrc_123|x"
				return p
			},
		},
		{
			name: "a null byte inside a user agent shifts the boundary",
			a: func(p RehydrateRecordParams) RehydrateRecordParams {
				p.Actor.UserAgent, p.Reason = "curl/8.0\x00escalated", ""
				return p
			},
			b: func(p RehydrateRecordParams) RehydrateRecordParams {
				p.Actor.UserAgent, p.Reason = "curl/8.0", "\x00escalated"
				return p
			},
		},
		{
			name: "an empty field is not the same as a shorter neighbour",
			a: func(p RehydrateRecordParams) RehydrateRecordParams {
				p.Actor.ID, p.Actor.Name = "usr_1", ""
				return p
			},
			b: func(p RehydrateRecordParams) RehydrateRecordParams {
				p.Actor.ID, p.Actor.Name = "usr_", "1"
				return p
			},
		},
		{
			name: "a snapshot value absorbs the reason",
			a: func(p RehydrateRecordParams) RehydrateRecordParams {
				p.After, p.Reason = map[string]any{"gateway": "adyen"}, "renegotiated"
				return p
			},
			b: func(p RehydrateRecordParams) RehydrateRecordParams {
				p.After, p.Reason = map[string]any{"gateway": "adyenrenegotiated"}, ""
				return p
			},
		},
		{
			name: "a sequence digit migrates into the tenant id",
			a: func(p RehydrateRecordParams) RehydrateRecordParams {
				p.TenantID, p.Sequence = shared.TenantID("ten_1"), 8842
				return p
			},
			b: func(p RehydrateRecordParams) RehydrateRecordParams {
				p.TenantID, p.Sequence = shared.TenantID("ten_18"), 842
				return p
			},
		},
	}

	seen := make(map[string]string, len(tests)*2)
	for _, tc := range tests {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := RehydrateRecord(tc.a(base))
			b := RehydrateRecord(tc.b(base))
			da, db := a.ComputeDigest(), b.ComputeDigest()
			if da == db {
				t.Fatalf("two records differing only in field boundaries share digest %s", da)
			}
			if len(da) != 64 || len(db) != 64 {
				t.Fatalf("digests are not hex SHA-256: %q, %q", da, db)
			}
		})
		// Also assert globally that no two of the generated records collide.
		for _, r := range []Record{RehydrateRecord(tc.a(base)), RehydrateRecord(tc.b(base))} {
			d := r.ComputeDigest()
			if prev, ok := seen[d]; ok {
				t.Fatalf("digest collision between %q and %q", prev, tc.name)
			}
			seen[d] = tc.name
		}
	}
}

// TestDigestIsStable checks the other half of canonicalization: the same record must hash to the
// same value every time, including when its snapshot maps were built in a different order. Go
// randomizes map iteration, so an encoder that did not sort keys would fail this intermittently
// — which in production is a chain-wide false tamper alarm raised by a rolling deploy.
func TestDigestIsStable(t *testing.T) {
	t.Parallel()

	build := func(order int) Record {
		before := map[string]any{}
		after := map[string]any{}
		keys := []string{"gateway", "costWeight", "latencyWeight", "fallback", "primary"}
		vals := []any{"stripe", 0.2, 0.1, "adyen", "stripe"}
		if order == 1 {
			for i := len(keys) - 1; i >= 0; i-- {
				before[keys[i]] = vals[i]
				after[keys[i]] = vals[i]
			}
		} else {
			for i := range keys {
				before[keys[i]] = vals[i]
				after[keys[i]] = vals[i]
			}
		}
		return RehydrateRecord(RehydrateRecordParams{
			ID: shared.AuditID("aud_01JB8Z9K2QW3E4R5T6Y7U8I9O0"), TenantID: testTenant, Sequence: 3,
			Actor: testActor(), Action: ActionRoutingChanged, Outcome: OutcomeSuccess,
			ResourceType: "configuration", ResourceID: "cfv_7",
			Before: before, After: after, Reason: "CHG-2026-0412",
			OccurredAt: time.Date(2026, 3, 3, 9, 14, 22, 0, time.UTC),
			RecordedAt: time.Date(2026, 3, 3, 9, 14, 23, 0, time.UTC),
			PrevDigest: strings.Repeat("b", 64),
		})
	}

	want := build(0).ComputeDigest()
	for i := 0; i < 200; i++ {
		if got := build(i % 2).ComputeDigest(); got != want {
			t.Fatalf("digest is not stable: iteration %d gave %s, want %s", i, got, want)
		}
	}
}

func TestGenesisDigestDependsOnTenantAndNonce(t *testing.T) {
	t.Parallel()

	base := GenesisDigest(testTenant, testNonce)
	tests := []struct {
		name   string
		tenant shared.TenantID
		nonce  string
	}{
		{"different tenant", shared.TenantID("ten_01JB8Z9K2QW3E4R5T6Y7U8I9O1"), testNonce},
		{"different nonce", testTenant, testNonce + "x"},
		{"boundary shifted between tenant and nonce", testTenant + "g", strings.TrimPrefix(testNonce, "g")},
	}
	for _, tc := range tests {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := GenesisDigest(tc.tenant, tc.nonce); got == base {
				t.Fatal("genesis digest collides with the base chain's")
			}
		})
	}
	if GenesisDigest(testTenant, testNonce) != base {
		t.Fatal("genesis digest is not deterministic")
	}
}

func TestChainRejectsForeignTenant(t *testing.T) {
	t.Parallel()

	c, err := NewChain(testTenant, testNonce)
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}
	other := mustRecord(t, NewRecordParams{TenantID: shared.TenantID("ten_01JB8Z9K2QW3E4R5T6Y7U8I9O1")})
	if err := c.Append(other); err == nil {
		t.Fatal("Append accepted a record belonging to another tenant's chain")
	}
	if _, err := NewChain(testTenant, "  "); err == nil {
		t.Fatal("NewChain accepted an empty genesis nonce")
	}
	if _, err := NewChain("", testNonce); err == nil {
		t.Fatal("NewChain accepted a missing tenant")
	}
}

// TestAppendDoesNotMutateTheCaller'sCopy — a caller who retains the record it appended must not
// be able to change what the chain holds.
func TestAppendLeavesTheCallersCopyUnstamped(t *testing.T) {
	t.Parallel()

	c, err := NewChain(testTenant, testNonce)
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}
	r := mustRecord(t, NewRecordParams{})
	if err := c.Append(r); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if r.Sequence() != 0 || r.Digest() != "" {
		t.Fatal("Append mutated the caller's copy of the record")
	}
	stamped, ok := c.Last()
	if !ok || stamped.Sequence() != 1 || stamped.Digest() == "" {
		t.Fatal("the chain's copy was not stamped")
	}
}

func TestRecordSnapshotsAreCopiedOnTheWayInAndOut(t *testing.T) {
	t.Parallel()

	src := map[string]any{"state": "CAPTURED"}
	r := mustRecord(t, NewRecordParams{After: src})
	before := r.ComputeDigest()

	// Mutating the map the caller passed in must not change the record.
	src["state"] = "REFUNDED"
	if r.ComputeDigest() != before {
		t.Fatal("the record shares its snapshot map with the caller")
	}
	// Mutating the map the record handed out must not change it either.
	out := r.After()
	out["state"] = "VOIDED"
	if r.ComputeDigest() != before {
		t.Fatal("After() returned the live map")
	}
}

func TestNewRecordValidation(t *testing.T) {
	// Verifies: FR-88.
	t.Parallel()

	valid := NewRecordParams{
		TenantID: testTenant, Actor: testActor(), Action: ActionPaymentCaptured,
		ResourceType: "payment", ResourceID: "pay_1", Outcome: OutcomeSuccess,
	}
	mutate := func(f func(*NewRecordParams)) NewRecordParams {
		p := valid
		f(&p)
		return p
	}

	tests := []struct {
		name    string
		params  NewRecordParams
		wantErr bool
	}{
		{"valid", valid, false},
		{"no tenant", mutate(func(p *NewRecordParams) { p.TenantID = "" }), true},
		{"no actor id", mutate(func(p *NewRecordParams) { p.Actor.ID = "" }), true},
		{"unknown actor type", mutate(func(p *NewRecordParams) { p.Actor.Type = "ROBOT" }), true},
		{"unknown action", mutate(func(p *NewRecordParams) { p.Action = "merchant.deleted" }), true},
		{"unknown outcome", mutate(func(p *NewRecordParams) { p.Outcome = "MAYBE" }), true},
		{"no resource type", mutate(func(p *NewRecordParams) { p.ResourceType = " " }), true},
		{"no resource id", mutate(func(p *NewRecordParams) { p.ResourceID = "" }), true},
		{
			name: "dual-controlled action without a reason",
			params: mutate(func(p *NewRecordParams) {
				p.Action, p.ResourceType, p.ResourceID = ActionMerchantTerminated, "merchant", "mrc_1"
			}),
			wantErr: true,
		},
		{
			name: "dual-controlled action with a reason",
			params: mutate(func(p *NewRecordParams) {
				p.Action, p.ResourceType, p.ResourceID = ActionMerchantTerminated, "merchant", "mrc_1"
				p.Reason = "CHG-2026-0412: sanctions match confirmed"
			}),
			wantErr: false,
		},
		{
			name: "denial is recorded like any other outcome",
			params: mutate(func(p *NewRecordParams) {
				p.Action, p.Outcome = ActionPermissionDenied, OutcomeDenied
			}),
			wantErr: false,
		},
	}
	for _, tc := range tests {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewRecord(tc.params, testClock())
			if tc.wantErr != (err != nil) {
				t.Fatalf("NewRecord() error = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func TestActionRegistry(t *testing.T) {
	t.Parallel()

	// Every action required by the auditable-action list must exist and be parseable.
	required := []Action{
		ActionMerchantCreated, ActionMerchantApproved, ActionMerchantSuspended, ActionMerchantTerminated,
		ActionConfigurationChanged, ActionRoutingChanged, ActionCredentialRotated,
		ActionPaymentCreated, ActionPaymentCaptured, ActionPaymentRefunded, ActionPaymentVoided,
		ActionWorkflowSignal, ActionManualGateApproved, ActionAdminAction,
		ActionLogin, ActionPermissionDenied,
	}
	for _, a := range required {

		t.Run(string(a), func(t *testing.T) {
			t.Parallel()
			if !a.IsValid() {
				t.Fatalf("action %s is not registered", a)
			}
			got, err := ParseAction(string(a))
			if err != nil || got != a {
				t.Fatalf("ParseAction(%q) = (%q, %v)", a, got, err)
			}
		})
	}
	if got, want := len(AllActions()), len(required); got != want {
		t.Fatalf("AllActions() has %d entries, want %d", got, want)
	}
	if _, err := ParseAction("merchant.deleted"); err == nil {
		t.Fatal("ParseAction accepted an unregistered action")
	}
}

func TestPartitionMonthComesFromTheIdentifier(t *testing.T) {
	t.Parallel()

	r := mustRecord(t, NewRecordParams{})
	got := r.PartitionMonth()
	if got.IsZero() {
		t.Fatal("PartitionMonth is zero; the record's identifier is not a ULID")
	}
	if got.Day() != 1 || got.Hour() != 0 || got.Location() != time.UTC {
		t.Fatalf("PartitionMonth = %s, want the first instant of a UTC month", got)
	}
}
