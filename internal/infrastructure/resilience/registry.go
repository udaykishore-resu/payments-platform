package resilience

import (
	"container/list"
	"sync"
	"time"
)

// Registry defaults. A per-key resilience primitive is only as safe as its cardinality bound:
// the key is usually derived from request data, and request data is attacker-influenced.
const (
	// DefaultRegistryMaxKeys bounds live keys. 1 024 is generous for the real key space —
	// (gateway, operation) is a few dozen entries — and small enough that the worst case is a
	// few hundred kilobytes rather than an out-of-memory kill.
	DefaultRegistryMaxKeys = 1024

	// DefaultRegistryIdleTTL is 10 minutes. A breaker whose key has seen no traffic for ten
	// minutes has a rolling window (30 s) that is entirely empty; its state is not information,
	// it is residue.
	DefaultRegistryIdleTTL = 10 * time.Minute
)

// keyedRegistry is a bounded, idle-evicting, LRU map from string keys to per-key resilience
// primitives. It is the shared machinery behind BreakerRegistry and BulkheadRegistry.
//
// Two bounds, both load-bearing:
//
//   - maxKeys caps live entries. Without it, a key derived from anything a caller controls
//     (a gateway id from a routing plan, a tenant id from a header, an operation name from a
//     path) is an unbounded allocation an attacker can drive by sending requests with novel
//     values. When the map is full the least-recently-used entry is evicted, which is the right
//     trade: an entry that has not been touched recently holds no history worth keeping, and
//     dropping it costs at most one cold start of a rolling window.
//   - idleTTL evicts entries nobody has used, lazily on every get and eagerly from an optional
//     sweeper. Lazy eviction is the authoritative path precisely because it cannot leak a
//     goroutine; the sweeper exists only so that a registry which stops receiving traffic
//     entirely still shrinks.
type keyedRegistry[T any] struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	order   *list.List // front = most recently used
	maxKeys int
	idleTTL time.Duration
	clock   Clock
	onEvict func(key string, value T)

	stopOnce sync.Once
	stop     chan struct{}
	wg       sync.WaitGroup
}

type regEntry[T any] struct {
	key      string
	value    T
	lastUsed time.Time
}

func newKeyedRegistry[T any](maxKeys int, idleTTL time.Duration, clk Clock, onEvict func(string, T)) *keyedRegistry[T] {
	if maxKeys <= 0 {
		maxKeys = DefaultRegistryMaxKeys
	}
	if idleTTL <= 0 {
		idleTTL = DefaultRegistryIdleTTL
	}
	return &keyedRegistry[T]{
		entries: make(map[string]*list.Element),
		order:   list.New(),
		maxKeys: maxKeys,
		idleTTL: idleTTL,
		clock:   orSystem(clk),
		onEvict: onEvict,
	}
}

// getOrCreate returns the value for key, constructing it with mk on first use. Every call
// refreshes the entry's recency, which is what idle eviction and LRU both key off.
func (r *keyedRegistry[T]) getOrCreate(key string, mk func(string) T) T {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.clock.Now()
	if el, ok := r.entries[key]; ok {
		e := entryOf[T](el)
		e.lastUsed = now
		r.order.MoveToFront(el)
		return e.value
	}

	// Sweep idle entries before growing. Doing it here rather than only in the sweeper is what
	// makes the bound hold in a process that never calls Close.
	r.evictIdleLocked(now)
	for len(r.entries) >= r.maxKeys {
		if !r.evictOldestLocked() {
			break
		}
	}

	e := &regEntry[T]{key: key, value: mk(key), lastUsed: now}
	r.entries[key] = r.order.PushFront(e)
	return e.value
}

// remove deletes key if present, returning whether it was.
func (r *keyedRegistry[T]) remove(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	el, ok := r.entries[key]
	if !ok {
		return false
	}
	r.dropLocked(el)
	return true
}

func (r *keyedRegistry[T]) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

// snapshot returns the live keys and values. The map is a copy; the values are the live
// primitives, which are themselves concurrency-safe.
func (r *keyedRegistry[T]) snapshot() map[string]T {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]T, len(r.entries))
	for k, el := range r.entries {
		out[k] = entryOf[T](el).value
	}
	return out
}

// evictIdle drops every entry untouched for longer than idleTTL and returns the count.
func (r *keyedRegistry[T]) evictIdle() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.evictIdleLocked(r.clock.Now())
}

func (r *keyedRegistry[T]) evictIdleLocked(now time.Time) int {
	n := 0
	// The list is ordered by recency, so eviction walks from the back and stops at the first
	// entry still inside the TTL. That makes a sweep O(evicted), not O(live).
	for el := r.order.Back(); el != nil; el = r.order.Back() {
		e := entryOf[T](el)
		if now.Sub(e.lastUsed) < r.idleTTL {
			break
		}
		r.dropLocked(el)
		n++
	}
	return n
}

func (r *keyedRegistry[T]) evictOldestLocked() bool {
	el := r.order.Back()
	if el == nil {
		return false
	}
	r.dropLocked(el)
	return true
}

// entryOf reads the registry entry a list element carries.
//
// container/list stores its payload as `any`, so every read of the recency list needs this
// assertion. It is concentrated in one function rather than repeated at each call site: the
// entries map and the order list are written only by this file and only ever with a
// *regEntry[T], so the assertion cannot fail — and a single place to say that is a single
// place to check it, instead of four unexplained assertions that each read like an oversight.
func entryOf[T any](el *list.Element) *regEntry[T] {
	return el.Value.(*regEntry[T]) //nolint:errcheck // see above: this list holds only *regEntry[T]
}

func (r *keyedRegistry[T]) dropLocked(el *list.Element) {
	e := entryOf[T](el)
	r.order.Remove(el)
	delete(r.entries, e.key)
	if r.onEvict != nil {
		r.onEvict(e.key, e.value)
	}
}

// startSweeper launches the single background goroutine this package ever creates. It is owned
// by the registry, it holds no reference the registry does not, and close stops it and waits
// for it — so a registry that is closed cannot leak it.
//
// The ticker runs on wall time even when the registry's Clock is a ManualClock: a sweeper is a
// housekeeping cadence, not a state machine, and the authoritative eviction decision inside it
// still uses the injected clock.
func (r *keyedRegistry[T]) startSweeper(interval time.Duration) {
	if interval <= 0 {
		return
	}
	r.stop = make(chan struct{})
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-r.stop:
				return
			case <-t.C:
				r.evictIdle()
			}
		}
	}()
}

func (r *keyedRegistry[T]) close() {
	r.stopOnce.Do(func() {
		if r.stop != nil {
			close(r.stop)
		}
	})
	r.wg.Wait()
}
