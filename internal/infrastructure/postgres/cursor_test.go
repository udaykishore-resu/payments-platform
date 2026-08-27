package postgres

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

func TestCursorRoundTrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		cur  Cursor
	}{
		{"ordinary", Cursor{Time: time.Date(2026, 8, 26, 12, 34, 56, 789000000, time.UTC), ID: "pay_01JB8Z9K2QW3E4R5T6Y7U8I9O0"}},
		{"epoch", Cursor{Time: time.UnixMicro(0).UTC(), ID: "pay_0"}},
		{"far future", Cursor{Time: time.Date(2199, 1, 1, 0, 0, 0, 0, time.UTC), ID: "aud_x"}},
		{"identifier containing the separator is still exact", Cursor{
			Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), ID: "weird|id|with|pipes"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			token := EncodeCursor(tc.cur)
			got, err := DecodeCursor(token)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !got.Time.Equal(tc.cur.Time.Truncate(time.Microsecond)) {
				t.Fatalf("time = %v, want %v", got.Time, tc.cur.Time)
			}
			if got.ID != tc.cur.ID {
				t.Fatalf("id = %q, want %q", got.ID, tc.cur.ID)
			}
		})
	}
}

// TestCursorTruncatesToMicroseconds documents why the truncation exists: PostgreSQL TIMESTAMPTZ
// has microsecond resolution, so a cursor carrying nanoseconds would encode a boundary no row
// can equal — and the row sitting exactly on the boundary would be silently skipped.
func TestCursorTruncatesToMicroseconds(t *testing.T) {
	t.Parallel()
	nanos := time.Date(2026, 8, 26, 0, 0, 0, 123456789, time.UTC)
	got, err := DecodeCursor(EncodeCursor(Cursor{Time: nanos, ID: "x"}))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Time.Nanosecond()%1000 != 0 {
		t.Fatalf("cursor kept sub-microsecond precision: %v", got.Time)
	}
}

func TestEmptyCursorIsTheFirstPage(t *testing.T) {
	t.Parallel()
	if EncodeCursor(Cursor{}) != "" {
		t.Fatal("the zero cursor must encode to the empty string")
	}
	got, err := DecodeCursor("")
	if err != nil {
		t.Fatalf("the empty token is the first page, not an error: %v", err)
	}
	if !got.IsZero() {
		t.Fatal("decoding the empty token must give the zero cursor")
	}
}

// TestCursorTamperingIsDetected is the reason the token is signed at all.
//
// Without a signature a client can decode the base64, move the timestamp, and re-encode —
// turning a pagination token into an arbitrary range query against a listing whose filters were
// validated once, on the first page.
func TestCursorTamperingIsDetected(t *testing.T) {
	t.Parallel()

	valid := EncodeCursor(Cursor{
		Time: time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC),
		ID:   "pay_01JB8Z9K2QW3E4R5T6Y7U8I9O0",
	})
	raw, err := base64.RawURLEncoding.DecodeString(valid)
	if err != nil {
		t.Fatalf("fixture is not decodable: %v", err)
	}

	tampered := map[string]string{
		"flipped payload byte":  mutate(raw, 0),
		"flipped mid payload":   mutate(raw, len(raw)/2),
		"flipped signature":     mutate(raw, len(raw)-1),
		"truncated":             base64.RawURLEncoding.EncodeToString(raw[:len(raw)-2]),
		"extended":              base64.RawURLEncoding.EncodeToString(append(append([]byte(nil), raw...), 0)),
		"not base64":            "!!!! not base64 !!!!",
		"empty payload":         base64.RawURLEncoding.EncodeToString([]byte("|0123456789abcdef")),
		"re-signed under wrong": forgeWithOtherKey(t),
	}

	for name, token := range tampered {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := DecodeCursor(token); err == nil {
				t.Fatalf("tampered cursor %q was accepted", token)
			} else if apierror.CodeOf(err) != apierror.CodeValidationFailed {
				t.Fatalf("tamper detection must be a 400, got %s", apierror.CodeOf(err))
			}
		})
	}
}

// TestCursorErrorIsUniform proves no failure mode is distinguishable from another. A client that
// could tell "bad base64" from "bad signature" would have an oracle for probing the scheme.
func TestCursorErrorIsUniform(t *testing.T) {
	t.Parallel()
	_, e1 := DecodeCursor("!!!not-base64!!!")
	_, e2 := DecodeCursor(base64.RawURLEncoding.EncodeToString([]byte("123|abc|0123456789abcdef")))
	if e1 == nil || e2 == nil {
		t.Fatal("both inputs must be rejected")
	}
	if e1.Error() != e2.Error() {
		t.Fatalf("cursor errors leak which check failed:\n  %v\n  %v", e1, e2)
	}
}

// TestCursorKeyRotationInvalidatesOutstandingTokens documents the intended operational
// behaviour: rotating the key surfaces to a paging client as one 400 on their next page, never
// as wrong data.
func TestCursorKeyRotationInvalidatesOutstandingTokens(t *testing.T) {
	original := append([]byte(nil), cursorSigningKey...)
	t.Cleanup(func() { cursorSigningKey = original })

	token := EncodeCursor(Cursor{Time: time.Unix(1, 0).UTC(), ID: "x"})
	UseCursorKey([]byte("a-different-signing-key-entirely"))
	if _, err := DecodeCursor(token); err == nil {
		t.Fatal("a cursor signed under the previous key must not verify after rotation")
	}
}

func TestPageLimitClamps(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want int }{
		{0, defaultPageLimit},
		{-5, defaultPageLimit},
		{1, 1},
		{50, 50},
		{maxPageLimit, maxPageLimit},
		{maxPageLimit + 1, maxPageLimit},
		{1 << 20, maxPageLimit},
	}
	for _, tc := range cases {
		if got := pageLimit(tc.in); got != tc.want {
			t.Fatalf("pageLimit(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func mutate(raw []byte, i int) string {
	cp := append([]byte(nil), raw...)
	cp[i] ^= 0xFF
	return base64.RawURLEncoding.EncodeToString(cp)
}

// forgeWithOtherKey builds a well-formed token signed with a key this process does not hold.
//
// It recomputes the encoding locally rather than swapping cursorSigningKey and calling
// EncodeCursor: the key is package state, this test runs in parallel with others that encode and
// decode, and mutating shared state from a parallel test is a data race that -race would (rightly)
// fail on. The duplication here is the price of a test that does not reach into the code it tests.
func forgeWithOtherKey(t *testing.T) string {
	t.Helper()
	payload := strconv.FormatInt(time.Unix(99, 0).UTC().UnixMicro(), 10) + "|pay_forged"
	mac := hmac.New(sha256.New, []byte("attacker-guessed-key"))
	mac.Write([]byte(cursorDomain))
	mac.Write([]byte(payload))
	forged := base64.RawURLEncoding.EncodeToString(
		append([]byte(payload+"|"), mac.Sum(nil)[:cursorSigBytes]...))
	if strings.TrimSpace(forged) == "" {
		t.Fatal("forge fixture is empty")
	}
	return forged
}
