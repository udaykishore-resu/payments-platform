package redis

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

func TestVelocityKeyIsTenantScoped(t *testing.T) {
	t.Parallel()
	got, err := NewVelocityCounter(newFakeRedis()).Key(tenantCtx(), "card:abc123")
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	if want := "pp:{" + testTenant + "}:vel:card:abc123"; got != want {
		t.Fatalf("Key = %q, want %q", got, want)
	}
}

// TestIncrementAndCountArgs pins the argument construction: every duration crosses in
// milliseconds, and the member is unique.
func TestIncrementAndCountArgs(t *testing.T) {
	t.Parallel()
	f := newFakeRedis()
	f.setScriptReply(int64(3))
	v := NewVelocityCounter(f, WithMemberFactory(func() string { return "fixed-member" }))

	n, err := v.IncrementAndCount(tenantCtx(), "card:abc", time.Hour)
	if err != nil {
		t.Fatalf("IncrementAndCount: %v", err)
	}
	if n != 3 {
		t.Fatalf("count = %d", n)
	}

	call := f.lastScript()
	if len(call.keys) != 1 || !strings.HasSuffix(call.keys[0], ":vel:card:abc") {
		t.Fatalf("keys = %v", call.keys)
	}
	if len(call.args) != 4 {
		t.Fatalf("args = %v, want now/window/member/ttl", call.args)
	}
	if got := call.args[1]; got != strconv.FormatInt(time.Hour.Milliseconds(), 10) {
		t.Errorf("window = %v, want milliseconds", got)
	}
	if call.args[2] != "fixed-member" {
		t.Errorf("member = %v", call.args[2])
	}
	// The TTL must outlive the window, or a key can expire while its window still matters and the
	// count silently resets to zero — the same failure the counter exists to prevent.
	ttlMs, err := strconv.ParseInt(call.args[3].(string), 10, 64)
	if err != nil {
		t.Fatalf("ttl is not an integer: %v", call.args[3])
	}
	if ttlMs <= time.Hour.Milliseconds() {
		t.Fatalf("ttl = %dms, must exceed the %dms window", ttlMs, time.Hour.Milliseconds())
	}
}

// TestMembersAreUnique. ZADD with an existing member updates its score instead of adding a row,
// so two events with the same member would count as one — and colliding members are most likely
// at exactly the rate this counter exists to measure.
func TestMembersAreUnique(t *testing.T) {
	t.Parallel()
	seen := map[string]struct{}{}
	for i := 0; i < 2000; i++ {
		m := randomMember()
		if _, dup := seen[m]; dup {
			t.Fatalf("member %q was minted twice", m)
		}
		seen[m] = struct{}{}
	}
}

func TestWindowIsBounded(t *testing.T) {
	t.Parallel()
	f := newFakeRedis()
	f.setScriptReply(int64(1))
	v := NewVelocityCounter(f, WithMaxWindow(time.Hour))

	// An unbounded window is an unbounded sorted set.
	if _, err := v.IncrementAndCount(tenantCtx(), "k", 365*24*time.Hour); err != nil {
		t.Fatalf("IncrementAndCount: %v", err)
	}
	if got := f.lastScript().args[1]; got != strconv.FormatInt(time.Hour.Milliseconds(), 10) {
		t.Fatalf("window = %v, want the cap", got)
	}

	// A zero window is a caller mistake, not a request for "no window".
	if _, err := v.IncrementAndCount(tenantCtx(), "k", 0); err != nil {
		t.Fatalf("IncrementAndCount: %v", err)
	}
	if got := f.lastScript().args[1]; got != strconv.FormatInt(time.Minute.Milliseconds(), 10) {
		t.Fatalf("zero window = %v, want the one-minute default", got)
	}
}

func TestCountDoesNotRecordAnEvent(t *testing.T) {
	t.Parallel()
	f := newFakeRedis()
	f.setScriptReply(int64(7))
	v := NewVelocityCounter(f)

	n, err := v.Count(tenantCtx(), "card:abc", time.Hour)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 7 {
		t.Fatalf("count = %d", n)
	}
	// The read-only script takes now and window only — no member, no TTL.
	if got := len(f.lastScript().args); got != 2 {
		t.Fatalf("Count passed %d args, want 2 (now, window)", got)
	}
}

func TestSumAndAddCarriesTheAmountInTheMember(t *testing.T) {
	t.Parallel()
	f := newFakeRedis()
	f.setScriptReply(int64(15_000))
	v := NewVelocityCounter(f, WithMemberFactory(func() string { return "m1" }))

	add, err := money.New(5_000, money.Currency("EUR"))
	if err != nil {
		t.Fatalf("money.New: %v", err)
	}
	total, err := v.SumAndAdd(tenantCtx(), "merchant:daily", 24*time.Hour, add)
	if err != nil {
		t.Fatalf("SumAndAdd: %v", err)
	}
	if total.Amount() != 15_000 {
		t.Fatalf("total = %d", total.Amount())
	}
	if total.Currency() != money.Currency("EUR") {
		t.Fatalf("currency = %s; the window is per-currency and the caller's currency must survive", total.Currency())
	}
	// The amount is in the member so that trimming an aged-out event also removes its
	// contribution; a separate running total would drift.
	if got := f.lastScript().args[2]; got != "5000:m1" {
		t.Fatalf("member = %v, want <amount>:<unique>", got)
	}
}

func TestSumAndAddWithZeroDoesNotRecordAnEvent(t *testing.T) {
	t.Parallel()
	f := newFakeRedis()
	f.setScriptReply(int64(0))
	v := NewVelocityCounter(f)

	zero, err := money.New(0, money.Currency("EUR"))
	if err != nil {
		t.Fatalf("money.New: %v", err)
	}
	if _, err := v.SumAndAdd(tenantCtx(), "merchant:daily", time.Hour, zero); err != nil {
		t.Fatalf("SumAndAdd: %v", err)
	}
	if got := f.lastScript().args[2]; got != "" {
		t.Fatalf("member = %q; a zero-amount read must not add a row", got)
	}
}

func TestVelocityReportsAnOutage(t *testing.T) {
	t.Parallel()
	f := newFakeRedis()
	f.setErr(errors.New("connection refused"))
	v := NewVelocityCounter(f)

	if _, err := v.IncrementAndCount(tenantCtx(), "k", time.Hour); err == nil {
		t.Error("IncrementAndCount swallowed an outage; the risk engine must know it is blind")
	}
	if _, err := v.Count(tenantCtx(), "k", time.Hour); err == nil {
		t.Error("Count swallowed an outage")
	}
}

// TestScriptsAreAtomicByConstruction is a source-level assertion, which is unusual and is
// justified here: the entire correctness argument for this file is that the read and the write
// happen inside one Lua script. A refactor that split them into client-side commands would pass
// every behavioural test and silently reintroduce the lost-update race.
func TestScriptsAreAtomicByConstruction(t *testing.T) {
	t.Parallel()
	for name, script := range map[string]string{
		"incrementAndCount": incrementAndCountScript,
		"sumAndAdd":         sumAndAddScript,
	} {
		if !strings.Contains(script, "ZREMRANGEBYSCORE") {
			t.Errorf("%s does not trim its window; the count would include aged-out events", name)
		}
		if !strings.Contains(script, "ZADD") {
			t.Errorf("%s does not record the event", name)
		}
		if !strings.Contains(script, "PEXPIRE") {
			t.Errorf("%s does not bound the key's memory", name)
		}
	}
	if !strings.Contains(incrementAndCountScript, "ZCARD") {
		t.Error("incrementAndCount does not return the window size")
	}
	if strings.Contains(incrementAndCountScript, "TIME") {
		t.Error("the script reads the server clock; the timestamp must be passed in so the platform shares one clock and tests are deterministic")
	}
}
