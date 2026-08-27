package redis

import (
	"context"
	"strings"
	"sync"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/telemetry"
)

const (
	testTenant   = "ten_01JB8Z00000000000000000000"
	testMerchant = "mrc_01JB8Z11111111111111111111"
)

func tenantCtx() context.Context {
	return telemetry.ContextWithFields(context.Background(), telemetry.Fields{TenantID: testTenant})
}

// fakeRedis is an in-memory stand-in for the commands this package issues.
//
// It is deliberately not a Redis emulator: the Lua scripts are exercised against a real server by
// the integration tests, because a hand-written approximation of Redis semantics would test the
// approximation. What this fake is for is everything *around* the scripts — key construction,
// argument marshalling, error handling, single-flight, TTL bounding — which is where the bugs a
// unit test can catch actually live.
type fakeRedis struct {
	mu     sync.Mutex
	values map[string][]byte
	ttls   map[string]time.Duration

	// err, when set, is returned by every command: the "Redis is down" case.
	err error
	// scriptReply is returned by Eval/EvalSha.
	scriptReply any
	scriptErr   error

	// calls records the commands issued, so a test can assert on round trips.
	gets    int
	sets    int
	dels    int
	scripts []scriptCall
}

type scriptCall struct {
	keys []string
	args []any
}

func newFakeRedis() *fakeRedis {
	return &fakeRedis{values: map[string][]byte{}, ttls: map[string]time.Duration{}}
}

func (f *fakeRedis) Get(ctx context.Context, key string) *goredis.StringCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	if f.err != nil {
		return goredis.NewStringResult("", f.err)
	}
	v, ok := f.values[key]
	if !ok {
		return goredis.NewStringResult("", goredis.Nil)
	}
	return goredis.NewStringResult(string(v), nil)
}

func (f *fakeRedis) Set(ctx context.Context, key string, value any, ttl time.Duration) *goredis.StatusCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sets++
	if f.err != nil {
		return goredis.NewStatusResult("", f.err)
	}
	f.values[key] = toBytes(value)
	f.ttls[key] = ttl
	return goredis.NewStatusResult("OK", nil)
}

func (f *fakeRedis) SetArgs(ctx context.Context, key string, value any, a goredis.SetArgs) *goredis.StatusCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sets++
	if f.err != nil {
		return goredis.NewStatusResult("", f.err)
	}
	if strings.EqualFold(a.Mode, "NX") {
		if _, exists := f.values[key]; exists {
			return goredis.NewStatusResult("", goredis.Nil)
		}
	}
	f.values[key] = toBytes(value)
	f.ttls[key] = a.TTL
	return goredis.NewStatusResult("OK", nil)
}

func (f *fakeRedis) Del(ctx context.Context, keys ...string) *goredis.IntCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dels++
	if f.err != nil {
		return goredis.NewIntResult(0, f.err)
	}
	var n int64
	for _, k := range keys {
		if _, ok := f.values[k]; ok {
			delete(f.values, k)
			n++
		}
	}
	return goredis.NewIntResult(n, nil)
}

func (f *fakeRedis) Ping(ctx context.Context) *goredis.StatusCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return goredis.NewStatusResult("", f.err)
	}
	return goredis.NewStatusResult("PONG", nil)
}

// --- Scripter -----------------------------------------------------------------------------------

func (f *fakeRedis) Eval(ctx context.Context, script string, keys []string, args ...any) *goredis.Cmd {
	return f.recordScript(keys, args)
}

func (f *fakeRedis) EvalSha(ctx context.Context, sha1 string, keys []string, args ...any) *goredis.Cmd {
	return f.recordScript(keys, args)
}

func (f *fakeRedis) EvalRO(ctx context.Context, script string, keys []string, args ...any) *goredis.Cmd {
	return f.recordScript(keys, args)
}

func (f *fakeRedis) EvalShaRO(ctx context.Context, sha1 string, keys []string, args ...any) *goredis.Cmd {
	return f.recordScript(keys, args)
}

func (f *fakeRedis) ScriptExists(ctx context.Context, hashes ...string) *goredis.BoolSliceCmd {
	out := make([]bool, len(hashes))
	// Report "not loaded" so go-redis's Script.Run falls through to EVAL, which is the path the
	// fake implements.
	return goredis.NewBoolSliceResult(out, nil)
}

func (f *fakeRedis) ScriptLoad(ctx context.Context, script string) *goredis.StringCmd {
	return goredis.NewStringResult("", nil)
}

func (f *fakeRedis) recordScript(keys []string, args []any) *goredis.Cmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripts = append(f.scripts, scriptCall{keys: keys, args: args})
	if f.scriptErr != nil {
		return goredis.NewCmdResult(nil, f.scriptErr)
	}
	if f.err != nil {
		return goredis.NewCmdResult(nil, f.err)
	}
	return goredis.NewCmdResult(f.scriptReply, nil)
}

// --- assertions --------------------------------------------------------------------------------

func (f *fakeRedis) lastScript() scriptCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.scripts) == 0 {
		return scriptCall{}
	}
	return f.scripts[len(f.scripts)-1]
}

func (f *fakeRedis) scriptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.scripts)
}

func (f *fakeRedis) setCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sets
}

func (f *fakeRedis) ttlOf(key string) time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ttls[key]
}

func (f *fakeRedis) put(key string, value []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.values[key] = value
}

func (f *fakeRedis) has(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.values[key]
	return ok
}

func (f *fakeRedis) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeRedis) setScriptReply(v any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scriptReply = v
}

func toBytes(v any) []byte {
	switch t := v.(type) {
	case []byte:
		return append([]byte(nil), t...)
	case string:
		return []byte(t)
	default:
		return []byte("")
	}
}

var _ UniversalClient = (*fakeRedis)(nil)
