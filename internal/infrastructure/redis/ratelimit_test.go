package redis

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/resilience"
)

func TestRateLimitKeyIsTenantScoped(t *testing.T) {
	t.Parallel()
	got, err := NewRateLimiter(newFakeRedis()).Key(tenantCtx(), "api:payments")
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	if want := "pp:{" + testTenant + "}:rl:api:payments"; got != want {
		t.Fatalf("Key = %q, want %q", got, want)
	}
}

// TestScriptArgsUnitConversions is the test that catches the bug that matters: passing seconds
// where the script expects milliseconds makes the limiter a thousand times too permissive and
// looks completely normal in a log.
func TestScriptArgsUnitConversions(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 26, 14, 3, 11, 412_000_000, time.UTC)
	limit := resilience.Limit{Rate: 100, Burst: 200}

	args := ScriptArgs(limit, now, 1, time.Minute)
	if len(args) != 5 {
		t.Fatalf("ScriptArgs returned %d args, want 5", len(args))
	}
	if args[0] != "100" {
		t.Errorf("rate = %v, want tokens per second", args[0])
	}
	if args[1] != "200" {
		t.Errorf("burst = %v", args[1])
	}
	if got, want := args[2], strconv.FormatInt(now.UnixMilli(), 10); got != want {
		t.Errorf("now = %v, want milliseconds %v", got, want)
	}
	if args[3] != "1" {
		t.Errorf("cost = %v", args[3])
	}
	// TTL must outlive a full refill (200 tokens / 100 per second = 2s, doubled = 4s), and is
	// floored at the minimum.
	ttlMs, err := strconv.ParseInt(args[4].(string), 10, 64)
	if err != nil {
		t.Fatalf("ttl is not an integer: %v", args[4])
	}
	if ttlMs < time.Minute.Milliseconds() {
		t.Errorf("ttl = %dms, must be at least the configured floor", ttlMs)
	}
}

func TestScriptArgsTTLOutlivesAFullRefill(t *testing.T) {
	t.Parallel()
	// A slow bucket: 1 token/second with a burst of 600 needs 600s to refill, so a one-minute TTL
	// would expire mid-refill and silently hand the caller a fresh full bucket.
	args := ScriptArgs(resilience.Limit{Rate: 1, Burst: 600}, time.Now(), 1, time.Minute)
	ttlMs, _ := strconv.ParseInt(args[4].(string), 10, 64)
	if ttlMs < 600_000 {
		t.Fatalf("ttl = %dms, must outlive the %ds refill", ttlMs, 600)
	}
}

func TestScriptArgsNormalizesTheLimit(t *testing.T) {
	t.Parallel()
	// Zero burst means the documented multiple of the rate, mirroring resilience.Limit.
	args := ScriptArgs(resilience.Limit{Rate: 50}, time.Now(), 0, time.Minute)
	if args[1] != strconv.Itoa(int(50*resilience.DefaultBurstMultiplier)) {
		t.Errorf("burst = %v, want the default multiple", args[1])
	}
	if args[3] != "1" {
		t.Errorf("cost = %v, want a floor of 1", args[3])
	}

	// A negative rate is a closed door, not a negative allowance.
	args = ScriptArgs(resilience.Limit{Rate: -5}, time.Now(), 1, time.Minute)
	if args[0] != "0" {
		t.Errorf("rate = %v, want 0", args[0])
	}
	if args[1] != "1" {
		t.Errorf("burst = %v, want a floor of 1", args[1])
	}
}

// TestParseDecisionFillsEveryHeaderField. A client told to back off with no reset interprets it
// as "immediately", which is the retry storm the limiter exists to prevent.
func TestParseDecisionFillsEveryHeaderField(t *testing.T) {
	t.Parallel()
	limit := resilience.Limit{Rate: 100, Burst: 200}

	allowed, err := ParseDecision([]any{int64(1), int64(150), int64(500), int64(0)}, limit)
	if err != nil {
		t.Fatalf("ParseDecision: %v", err)
	}
	if !allowed.Allowed {
		t.Error("allowed = false")
	}
	if allowed.Limit != 200 {
		t.Errorf("limit = %d", allowed.Limit)
	}
	if allowed.Remaining != 150 {
		t.Errorf("remaining = %d", allowed.Remaining)
	}
	if allowed.ResetAfter != 500*time.Millisecond {
		t.Errorf("reset = %v", allowed.ResetAfter)
	}
	if allowed.RetryAfter != 0 {
		t.Errorf("an allowed decision must not tell the client to back off: %v", allowed.RetryAfter)
	}

	denied, err := ParseDecision([]any{int64(0), int64(0), int64(2000), int64(10)}, limit)
	if err != nil {
		t.Fatalf("ParseDecision: %v", err)
	}
	if denied.Allowed {
		t.Error("allowed = true for a denial")
	}
	if denied.RetryAfter != 10*time.Millisecond {
		t.Errorf("retryAfter = %v", denied.RetryAfter)
	}
	// And the headers render.
	h := denied.Headers()
	for _, k := range []string{"RateLimit-Limit", "RateLimit-Remaining", "RateLimit-Reset", "Retry-After"} {
		if h[k] == "" {
			t.Errorf("header %s is empty: %v", k, h)
		}
	}
}

func TestParseDecisionAcceptsTheShapesRedisReturns(t *testing.T) {
	t.Parallel()
	limit := resilience.Limit{Rate: 10, Burst: 20}
	for name, raw := range map[string][]any{
		"int64":  {int64(1), int64(5), int64(100), int64(0)},
		"int":    {1, 5, 100, 0},
		"string": {"1", "5", "100", "0"},
	} {
		d, err := ParseDecision(raw, limit)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !d.Allowed || d.Remaining != 5 {
			t.Errorf("%s: %+v", name, d)
		}
	}
}

func TestParseDecisionRejectsAMalformedReply(t *testing.T) {
	t.Parallel()
	if _, err := ParseDecision([]any{int64(1)}, resilience.Limit{Rate: 1}); err == nil {
		t.Error("ParseDecision accepted a short reply")
	}
	if _, err := ParseDecision([]any{int64(1), []any{}, int64(0), int64(0)}, resilience.Limit{Rate: 1}); err == nil {
		t.Error("ParseDecision accepted a non-integer field")
	}
}

func TestParseDecisionClampsNegativeRemaining(t *testing.T) {
	t.Parallel()
	d, err := ParseDecision([]any{int64(0), int64(-3), int64(0), int64(0)}, resilience.Limit{Rate: 1, Burst: 2})
	if err != nil {
		t.Fatalf("ParseDecision: %v", err)
	}
	if d.Remaining != 0 {
		t.Fatalf("remaining = %d, want a floor of 0 so the header is sane", d.Remaining)
	}
}

// TestAllowPassesTheKeyAndArgsToTheScript pins the whole call shape without a server.
func TestAllowPassesTheKeyAndArgsToTheScript(t *testing.T) {
	t.Parallel()
	f := newFakeRedis()
	f.setScriptReply([]any{int64(1), int64(9), int64(100), int64(0)})
	l := NewRateLimiter(f)

	d, err := l.Allow(tenantCtx(), "api:payments", resilience.Limit{Rate: 10, Burst: 10})
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if !d.Allowed || d.Remaining != 9 {
		t.Fatalf("decision = %+v", d)
	}

	call := f.lastScript()
	if len(call.keys) != 1 || !strings.HasSuffix(call.keys[0], ":rl:api:payments") {
		t.Fatalf("script keys = %v", call.keys)
	}
	if len(call.args) != 5 {
		t.Fatalf("script args = %v", call.args)
	}
}

// TestAllowReportsInfrastructureFailureAsAnError, never as a denial: the resilience package's
// contract says a non-nil error means infrastructure, and returning one for a denial would make
// the local fallback the normal path and multiply the effective limit by the pod count.
func TestAllowReportsInfrastructureFailureAsAnError(t *testing.T) {
	t.Parallel()
	f := newFakeRedis()
	f.setErr(errors.New("connection refused"))

	_, err := NewRateLimiter(f).Allow(tenantCtx(), "k", resilience.Limit{Rate: 10})
	if err == nil {
		t.Fatal("Allow swallowed an outage; the caller would never fall back")
	}
}

func TestRateLimiterSatisfiesTheResilienceBackend(t *testing.T) {
	t.Parallel()
	var _ resilience.Backend = NewRateLimiter(newFakeRedis())
}
