package testenv

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"
)

// crockford is the ULID alphabet, and exactly the character class every id CHECK constraint in
// migrations/ accepts: [0-9A-HJKMNP-TV-Z]. I, L, O and U are absent because they are the
// characters humans transcribe wrongly.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// ID prefixes, matching pkg/ids. They are duplicated here rather than imported because the tests
// must be able to construct an id the production generator would refuse — an id in a past month,
// an id with a chosen entropy — and a helper that could only produce well-formed current-time ids
// could not write the negative tests at all.
const (
	PrefixTenant     = "ten"
	PrefixMerchant   = "mrc"
	PrefixPayment    = "pay"
	PrefixAttempt    = "att"
	PrefixRefund     = "ref"
	PrefixEvent      = "evt"
	PrefixWebhook    = "whk"
	PrefixLedger     = "led"
	PrefixWorkflow   = "wfr"
	PrefixStep       = "wfs"
	PrefixRoutingPln = "rpl"
	PrefixConnection = "gwc"
)

// encodeULID renders 16 bytes as the 26-character Crockford base-32 string ULIDs use.
//
// The top two bits of the 130 bits the 26 characters can hold are always zero, which is why a
// real ULID's first character never exceeds '7'. Nothing here depends on that, but keeping the
// encoding identical to the production one means an id from this package is indistinguishable
// from a production id to every parser in the tree — including the database's CHECK constraints,
// which is the point.
func encodeULID(raw [16]byte) string {
	// Bit-at-a-time rather than word arithmetic. 130 iterations is free at test scale, and the
	// obvious implementation cannot be wrong in the way the clever one can: the shift-and-splice
	// version has to get the 2-bit padding and the 64-bit word boundary right simultaneously, and
	// a bug there produces ids that still *look* valid.
	bit := func(k int) byte { // k is an index into the padded 130-bit stream, 0 = most significant
		k -= 2 // two zero pad bits at the front
		if k < 0 {
			return 0
		}
		return (raw[k/8] >> (7 - uint(k%8))) & 1
	}
	var out [26]byte
	for i := 0; i < 26; i++ {
		var v byte
		for j := 0; j < 5; j++ {
			v = v<<1 | bit(i*5+j)
		}
		out[i] = crockford[v]
	}
	return string(out[:])
}

// DeterministicID mints a valid prefixed ULID from a timestamp and a seed string.
//
// The entropy is SHA-256 over the seed rather than a counter so that two different seeds cannot
// collide by accident and the same seed always produces the same id — which is what makes a
// failure message reproducible and a test diff readable. The timestamp is carried in the leading
// 48 bits exactly as pkg/ids does, so ids.TimeOf and the partition-key derivation in the schema
// agree with what the test intends.
func DeterministicID(prefix string, at time.Time, seed string) string {
	var raw [16]byte
	ms := uint64(at.UTC().UnixMilli())
	raw[0] = byte(ms >> 40)
	raw[1] = byte(ms >> 32)
	raw[2] = byte(ms >> 24)
	raw[3] = byte(ms >> 16)
	raw[4] = byte(ms >> 8)
	raw[5] = byte(ms)

	sum := sha256.Sum256([]byte(prefix + "|" + seed))
	copy(raw[6:], sum[:10])
	return prefix + "_" + encodeULID(raw)
}

// Clock is a fixed clock. Advance is explicit; nothing in the suite reads the wall clock for a
// value it later asserts on.
type Clock struct{ t time.Time }

// NewClock returns a clock pinned to t, truncated to millisecond precision because that is the
// precision the event envelope's RFC 3339 format carries. A clock with more precision than the
// wire format produces round-trip assertions that fail for reasons that have nothing to do with
// the behaviour under test.
func NewClock(t time.Time) *Clock { return &Clock{t: t.UTC().Truncate(time.Millisecond)} }

// Now returns the pinned instant.
func (c *Clock) Now() time.Time { return c.t }

// Advance moves the clock forward and returns the new instant.
func (c *Clock) Advance(d time.Duration) time.Time {
	c.t = c.t.Add(d)
	return c.t
}

// BaseTime is the instant every deterministic fixture is anchored to *within the current month*.
//
// It is not a fixed calendar date, and that is a deliberate trade-off. The schema derives a
// payment's partition from its ULID timestamp, and only a bounded window of monthly partitions
// exists at any time; a fixture pinned to 2026-01-01 would be unwritable the moment that
// partition was detached by retention. Anchoring to the 15th of the current month at noon UTC
// keeps every fixture inside a partition that exists while still being identical for every test
// in a run, which is the determinism that actually matters.
func BaseTime() time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), 15, 12, 0, 0, 0, time.UTC)
}

// PartitionMonth is the month a row with this timestamp belongs to, matching the schema's
// date_trunc('month', created_at AT TIME ZONE 'UTC').
func PartitionMonth(at time.Time) time.Time {
	u := at.UTC()
	return time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// Nonce derives a stable per-test namespace from the test's name.
//
// Per-test rather than per-run: two runs of the same test produce the same tenant id, so a failed
// run leaves rows a human can find by grepping the test name's hash out of the failure message.
// Per-test rather than global: t.Parallel() is then safe, and a test that writes outside its own
// namespace is caught by Scope's cleanup assertion rather than by an unrelated flake later.
func Nonce(t testing.TB) string {
	sum := sha256.Sum256([]byte(t.Name()))
	return fmt.Sprintf("%x", sum[:8])
}

// SanitizeName renders a test name into something safe to embed in an identifier.
func SanitizeName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
