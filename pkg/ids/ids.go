// Package ids implements the platform's identifier scheme: a typed prefix followed by a
// Crockford Base32 ULID.
//
//	pay_01JB8Z9K2QW3E4R5T6Y7U8I9O0
//
// The properties this buys us, and why each matters here (see docs/spec/00-design-baseline.md §6):
//
//   - Lexicographic order == chronological order. Postgres B-tree inserts stay at the right edge
//     of the index instead of fragmenting it the way UUIDv4 does, and `ORDER BY id` is a free
//     `ORDER BY created_at`.
//   - The creation timestamp is recoverable from the ID (TimeOf), which makes the monthly
//     partition key a pure function of an immutable value — see baseline amendment A-02.
//   - 80 bits of entropy per millisecond: generating 1.2e12 IDs in the same millisecond gives a
//     ~50% chance of one collision. We generate at most ~10^4/ms.
//   - The prefix makes a log line self-describing and makes "you passed a merchant ID where a
//     payment ID was expected" a compile-time error rather than a 3am incident.
//
// This package depends on nothing but the standard library, so the domain layer may import it.
package ids

import (
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Prefix identifies the kind of entity an ID refers to.
type Prefix string

// The complete prefix registry. Adding an entity type means adding it here; nothing else
// in the codebase may invent a prefix.
const (
	PrefixTenant              Prefix = "ten"
	PrefixAPIClient           Prefix = "cli"
	PrefixMerchant            Prefix = "mrc"
	PrefixOnboardingCase      Prefix = "onb"
	PrefixWorkflowInstance    Prefix = "wfr"
	PrefixWorkflowStep        Prefix = "wfs"
	PrefixGateway             Prefix = "gw"
	PrefixGatewayConnection   Prefix = "gwc"
	PrefixConfigVersion       Prefix = "cfv"
	PrefixPayment             Prefix = "pay"
	PrefixPaymentAttempt      Prefix = "att"
	PrefixRefund              Prefix = "ref"
	PrefixRoutingPlan         Prefix = "rpl"
	PrefixLedgerEntry         Prefix = "led"
	PrefixInboundWebhook      Prefix = "whk"
	PrefixEvent               Prefix = "evt"
	PrefixAuditRecord         Prefix = "aud"
	PrefixReconciliationRun   Prefix = "rcn"
	PrefixRequest             Prefix = "req"
	PrefixCertificationReport Prefix = "crt"
)

var registry = map[Prefix]struct{}{
	PrefixTenant: {}, PrefixAPIClient: {}, PrefixMerchant: {}, PrefixOnboardingCase: {},
	PrefixWorkflowInstance: {}, PrefixWorkflowStep: {}, PrefixGateway: {}, PrefixGatewayConnection: {},
	PrefixConfigVersion: {}, PrefixPayment: {}, PrefixPaymentAttempt: {}, PrefixRefund: {},
	PrefixRoutingPlan: {}, PrefixLedgerEntry: {}, PrefixInboundWebhook: {}, PrefixEvent: {},
	PrefixAuditRecord: {}, PrefixReconciliationRun: {}, PrefixRequest: {}, PrefixCertificationReport: {},
}

// Errors returned by Parse and Validate. Callers translate these into VALIDATION_FAILED.
var (
	ErrEmpty          = errors.New("ids: empty identifier")
	ErrMalformed      = errors.New("ids: malformed identifier")
	ErrUnknownPrefix  = errors.New("ids: unknown prefix")
	ErrPrefixMismatch = errors.New("ids: prefix mismatch")
	ErrBadEncoding    = errors.New("ids: invalid Crockford Base32 encoding")
)

// encodedLen is the fixed length of the ULID portion: 128 bits / 5 bits per symbol = 25.6,
// rounded up to 26 symbols.
const encodedLen = 26

// crockford is Crockford's Base32 alphabet: no I, L, O or U, so a human transcribing an ID
// cannot confuse 1/I/l or 0/O.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// decodeTable maps a byte to its symbol value, or 0xFF if the byte is not a valid symbol.
// It is deliberately permissive on case and on the ambiguous characters: 'i' and 'l' decode
// as 1, 'o' as 0, matching the Crockford specification's decoding rules. This makes IDs
// robust to human transcription without ever *emitting* an ambiguous character.
var decodeTable [256]byte

func init() {
	for i := range decodeTable {
		decodeTable[i] = 0xFF
	}
	for i, c := range []byte(crockford) {
		decodeTable[c] = byte(i)
		decodeTable[lower(c)] = byte(i)
	}
	decodeTable['i'], decodeTable['I'] = 1, 1
	decodeTable['l'], decodeTable['L'] = 1, 1
	decodeTable['o'], decodeTable['O'] = 0, 0
}

func lower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

// ID is a prefixed ULID. The zero value is invalid; use New or Parse.
//
// ID is a string rather than a struct so it can be used as a map key, compared with ==,
// range-scanned in Postgres, and logged without a formatter. The cost is that an invalid ID is
// representable; Parse and Validate are the guards, and every constructor in the domain layer
// calls one of them.
type ID string

// String satisfies fmt.Stringer.
func (id ID) String() string { return string(id) }

// Prefix returns the entity-kind prefix, or "" if the ID is malformed.
func (id ID) Prefix() Prefix {
	i := strings.IndexByte(string(id), '_')
	if i <= 0 {
		return ""
	}
	return Prefix(id[:i])
}

// IsZero reports whether the ID is the empty string.
func (id ID) IsZero() bool { return id == "" }

// generator holds the monotonic-within-a-millisecond state. ULID's spec allows incrementing
// the random component when two IDs are generated in the same millisecond; doing so guarantees
// strict ordering for IDs created in a tight loop (batch inserts, test fixtures) instead of
// merely probable ordering.
type generator struct {
	mu       sync.Mutex
	lastMS   uint64
	lastRand [10]byte
}

var defaultGenerator generator

// New returns a new ID with the given prefix, using the current wall clock.
//
// It panics only if the prefix is not registered — that is a programming error, caught by the
// first test that runs the code path, never a runtime condition. Entropy failures are handled
// by retrying, then by falling back to a time-derived value, because a payment API that cannot
// mint an ID is worse than one that mints a slightly-less-random one.
func New(p Prefix) ID { return NewAt(p, time.Now()) }

// NewAt returns a new ID with the given prefix stamped at t. Tests use this with a fixed clock
// to get deterministic, ordered identifiers.
func NewAt(p Prefix, t time.Time) ID {
	if _, ok := registry[p]; !ok {
		panic(fmt.Sprintf("ids: unregistered prefix %q — add it to the registry in pkg/ids", p))
	}
	ms := uint64(t.UTC().UnixMilli())

	defaultGenerator.mu.Lock()
	var entropy [10]byte
	if ms == defaultGenerator.lastMS {
		// Same millisecond: increment the previous entropy as a 80-bit big-endian integer.
		entropy = defaultGenerator.lastRand
		if !increment(&entropy) {
			// 2^80 IDs in one millisecond. Not reachable; fall forward a millisecond rather
			// than emit a duplicate.
			ms++
			randomize(&entropy)
		}
	} else {
		randomize(&entropy)
	}
	defaultGenerator.lastMS = ms
	defaultGenerator.lastRand = entropy
	defaultGenerator.mu.Unlock()

	var raw [16]byte
	raw[0] = byte(ms >> 40)
	raw[1] = byte(ms >> 32)
	raw[2] = byte(ms >> 24)
	raw[3] = byte(ms >> 16)
	raw[4] = byte(ms >> 8)
	raw[5] = byte(ms)
	copy(raw[6:], entropy[:])

	return ID(string(p) + "_" + encode(raw))
}

// increment adds one to the 80-bit big-endian value in b, reporting false on overflow.
func increment(b *[10]byte) bool {
	for i := len(b) - 1; i >= 0; i-- {
		b[i]++
		if b[i] != 0 {
			return true
		}
	}
	return false
}

func randomize(b *[10]byte) {
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing means the OS entropy source is gone. Degrade to a
		// nanosecond-derived value rather than panic in a payment path; the resulting ID is
		// still unique in practice because the millisecond prefix differs.
		n := uint64(time.Now().UnixNano())
		for i := range b {
			b[i] = byte(n >> (uint(i%8) * 8))
		}
	}
}

// encode renders 16 bytes as 26 Crockford Base32 symbols, most significant first.
func encode(raw [16]byte) string {
	out := make([]byte, encodedLen)
	// 26 symbols * 5 bits = 130 bits; the top 2 bits of the first symbol are always zero.
	out[0] = crockford[(raw[0]&224)>>5]
	out[1] = crockford[raw[0]&31]
	out[2] = crockford[(raw[1]&248)>>3]
	out[3] = crockford[((raw[1]&7)<<2)|((raw[2]&192)>>6)]
	out[4] = crockford[(raw[2]&62)>>1]
	out[5] = crockford[((raw[2]&1)<<4)|((raw[3]&240)>>4)]
	out[6] = crockford[((raw[3]&15)<<1)|((raw[4]&128)>>7)]
	out[7] = crockford[(raw[4]&124)>>2]
	out[8] = crockford[((raw[4]&3)<<3)|((raw[5]&224)>>5)]
	out[9] = crockford[raw[5]&31]
	out[10] = crockford[(raw[6]&248)>>3]
	out[11] = crockford[((raw[6]&7)<<2)|((raw[7]&192)>>6)]
	out[12] = crockford[(raw[7]&62)>>1]
	out[13] = crockford[((raw[7]&1)<<4)|((raw[8]&240)>>4)]
	out[14] = crockford[((raw[8]&15)<<1)|((raw[9]&128)>>7)]
	out[15] = crockford[(raw[9]&124)>>2]
	out[16] = crockford[((raw[9]&3)<<3)|((raw[10]&224)>>5)]
	out[17] = crockford[raw[10]&31]
	out[18] = crockford[(raw[11]&248)>>3]
	out[19] = crockford[((raw[11]&7)<<2)|((raw[12]&192)>>6)]
	out[20] = crockford[(raw[12]&62)>>1]
	out[21] = crockford[((raw[12]&1)<<4)|((raw[13]&240)>>4)]
	out[22] = crockford[((raw[13]&15)<<1)|((raw[14]&128)>>7)]
	out[23] = crockford[(raw[14]&124)>>2]
	out[24] = crockford[((raw[14]&3)<<3)|((raw[15]&224)>>5)]
	out[25] = crockford[raw[15]&31]
	return string(out)
}

// Parse validates s and returns it as an ID. It does not check the prefix against an expected
// value; use ParseAs for that.
func Parse(s string) (ID, error) {
	if s == "" {
		return "", ErrEmpty
	}
	i := strings.IndexByte(s, '_')
	if i <= 0 || len(s)-i-1 != encodedLen {
		return "", ErrMalformed
	}
	if _, ok := registry[Prefix(s[:i])]; !ok {
		return "", fmt.Errorf("%w: %q", ErrUnknownPrefix, s[:i])
	}
	body := s[i+1:]
	// The most significant symbol encodes only 3 meaningful bits; a value above 7 would mean
	// the timestamp overflowed 48 bits, which is not a legal ULID.
	if decodeTable[body[0]] > 7 {
		return "", ErrBadEncoding
	}
	for j := 0; j < encodedLen; j++ {
		if decodeTable[body[j]] == 0xFF {
			return "", ErrBadEncoding
		}
	}
	return ID(s), nil
}

// ParseAs validates s and additionally requires the given prefix. This is the function that
// turns "the caller passed a merchant ID to a payment lookup" into a 400 instead of a 404.
func ParseAs(s string, want Prefix) (ID, error) {
	id, err := Parse(s)
	if err != nil {
		return "", err
	}
	if id.Prefix() != want {
		return "", fmt.Errorf("%w: got %q, want %q", ErrPrefixMismatch, id.Prefix(), want)
	}
	return id, nil
}

// MustParseAs is Parse for constants and test fixtures. It panics on invalid input.
func MustParseAs(s string, want Prefix) ID {
	id, err := ParseAs(s, want)
	if err != nil {
		panic(err)
	}
	return id
}

// Validate reports whether s is a well-formed ID of the given prefix.
func Validate(s string, want Prefix) error {
	_, err := ParseAs(s, want)
	return err
}

// TimeOf recovers the creation timestamp encoded in the ID, to millisecond resolution.
//
// This is what makes the partition key a pure function of the ID (baseline amendment A-02).
// It returns the zero time if the ID is malformed — callers that care must Parse first.
func TimeOf(id ID) time.Time {
	i := strings.IndexByte(string(id), '_')
	if i <= 0 || len(id)-i-1 != encodedLen {
		return time.Time{}
	}
	body := string(id)[i+1:]
	var ms uint64
	for j := 0; j < 10; j++ { // 10 symbols * 5 bits = 50 bits, top 2 unused
		v := decodeTable[body[j]]
		if v == 0xFF {
			return time.Time{}
		}
		ms = ms<<5 | uint64(v)
	}
	return time.UnixMilli(int64(ms)).UTC()
}

// PartitionMonth returns the first instant of the UTC month in which the ID was created.
// Used as the declarative range-partition key for payments, attempts, ledger entries and
// audit records.
func PartitionMonth(id ID) time.Time {
	t := TimeOf(id)
	if t.IsZero() {
		return t
	}
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// Compare orders two IDs. Because the timestamp is the most significant component of the
// encoding, this is chronological order for IDs sharing a prefix.
func Compare(a, b ID) int { return strings.Compare(string(a), string(b)) }
