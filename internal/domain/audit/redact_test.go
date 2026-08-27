package audit

import (
	"testing"
	"time"
)

type gatewayConnection struct {
	ID            string `json:"id"`
	MerchantID    string `json:"merchantId"`
	Gateway       string `json:"gateway"`
	Enabled       bool   `json:"enabled"`
	Weight        int    `json:"weight"`
	APIKey        string `json:"apiKey"`
	WebhookSecret string `json:"webhookSecret"`
	// SigningKey has no json tag, to check that the Go field name also matches.
	SigningKey string
	// rotatedAt is unexported and must never appear, allowlisted or not.
	rotatedAt time.Time
}

// TestSnapshotIncludesOnlyAllowedFields is the core of the allowlist guarantee: fields nobody
// named do not appear, and that includes the ones a denylist would have had to know about in
// advance.
func TestSnapshotIncludesOnlyAllowedFields(t *testing.T) {
	t.Parallel()

	conn := gatewayConnection{
		ID: "gwc_1", MerchantID: "mrc_1", Gateway: "stripe", Enabled: true, Weight: 40,
		APIKey: "sk_test_FAKE_do_not_leak", WebhookSecret: "whsec_do_not_leak",
		SigningKey: "-----BEGIN PRIVATE KEY-----", rotatedAt: time.Now(),
	}
	allowed := []string{"id", "merchantId", "gateway", "enabled", "weight"}

	for _, in := range []any{conn, &conn} {
		got := Snapshot(in, allowed)
		if got == nil {
			t.Fatal("Snapshot returned nil for a populated struct")
		}
		if len(got) != len(allowed) {
			t.Fatalf("Snapshot returned %d fields %v, want %d", len(got), got, len(allowed))
		}
		for _, forbidden := range []string{"apiKey", "APIKey", "webhookSecret", "WebhookSecret", "SigningKey", "rotatedAt"} {
			if _, present := got[forbidden]; present {
				t.Errorf("Snapshot leaked %q", forbidden)
			}
		}
		if got["gateway"] != "stripe" || got["enabled"] != true || got["weight"] != int64(40) {
			t.Errorf("Snapshot returned wrong values: %v", got)
		}
	}
}

// TestSnapshotFailsClosed is the property that distinguishes an allowlist from a denylist: a
// field nobody has considered is absent, not present.
func TestSnapshotFailsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		value   any
		allowed []string
		want    int
	}{
		{"nil value", nil, []string{"id"}, 0},
		{"nil pointer", (*gatewayConnection)(nil), []string{"id"}, 0},
		{"empty allowlist", gatewayConnection{ID: "gwc_1"}, nil, 0},
		{"allowlist names nothing that exists", gatewayConnection{ID: "gwc_1"}, []string{"nope"}, 0},
		{"unsupported kind", "just a string", []string{"id"}, 0},
		{"one field allowed", gatewayConnection{ID: "gwc_1", APIKey: "sk"}, []string{"id"}, 1},
	}
	for _, tc := range tests {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Snapshot(tc.value, tc.allowed)
			if len(got) != tc.want {
				t.Fatalf("Snapshot() returned %d fields (%v), want %d", len(got), got, tc.want)
			}
		})
	}
}

func TestSnapshotMatchesEitherSpellingAndPrefersTheJSONName(t *testing.T) {
	t.Parallel()

	conn := gatewayConnection{MerchantID: "mrc_1", SigningKey: "key"}

	// Allowlisted by json tag.
	got := Snapshot(conn, []string{"merchantId"})
	if got["merchantId"] != "mrc_1" {
		t.Fatalf("Snapshot by json tag = %v", got)
	}
	// Allowlisted by Go field name: the field still appears under its json name, so a snapshot
	// reads the way the API does.
	got = Snapshot(conn, []string{"MerchantID"})
	if got["merchantId"] != "mrc_1" {
		t.Fatalf("Snapshot by field name = %v", got)
	}
	// A field with no json tag matches on its Go name.
	got = Snapshot(conn, []string{"signingkey"})
	if got["SigningKey"] != "key" {
		t.Fatalf("Snapshot of an untagged field = %v", got)
	}
}

func TestSnapshotAcceptsMaps(t *testing.T) {
	t.Parallel()

	in := map[string]any{
		"status":      "ACTIVE",
		"amount":      int64(8450),
		"apiKey":      "sk_test_FAKE_do_not_leak",
		"nested":      map[string]any{"a": 1},
		"emptyString": "",
	}
	got := Snapshot(in, []string{"status", "amount", "nested", "emptyString"})
	if len(got) != 4 {
		t.Fatalf("Snapshot returned %v", got)
	}
	if _, leaked := got["apiKey"]; leaked {
		t.Fatal("Snapshot leaked a map key that was not allowlisted")
	}
}

// TestSnapshotOutputIsCanonicalizable checks the contract between this file and the digest: a
// snapshot must consist only of kinds the canonical encoder renders deterministically, or the
// chain digest becomes unstable.
func TestSnapshotOutputIsCanonicalizable(t *testing.T) {
	t.Parallel()

	type exotic struct {
		When     time.Time
		Amount   float64
		Count    uint32
		Tags     []string
		Nested   map[string]string
		Fallback chan int
	}
	v := exotic{
		When: time.Date(2026, 3, 3, 9, 0, 0, 0, time.UTC), Amount: 1.5, Count: 7,
		Tags: []string{"a", "b"}, Nested: map[string]string{"k": "v"}, Fallback: make(chan int),
	}
	snap := Snapshot(v, []string{"When", "Amount", "Count", "Tags", "Nested", "Fallback"})
	first := canonicalJSON(snap)
	for i := 0; i < 100; i++ {
		if got := canonicalJSON(snap); got != first {
			t.Fatalf("canonical rendering is not stable: %q vs %q", got, first)
		}
	}
	if snap["When"] != "2026-03-03T09:00:00Z" {
		t.Errorf("time was not normalized: %v", snap["When"])
	}
	if _, ok := snap["Fallback"].(string); !ok {
		t.Errorf("an exotic kind was not reduced to a string: %T", snap["Fallback"])
	}
}

func TestCanonicalJSONSortsKeys(t *testing.T) {
	t.Parallel()

	got := canonicalJSON(map[string]any{
		"z": 1, "a": "x", "m": true, "n": nil,
		"nested": map[string]any{"b": 2, "a": 1},
		"list":   []any{"one", 2, false},
	})
	want := `{"a":"x","list":["one",2,false],"m":true,"n":null,"nested":{"a":1,"b":2},"z":1}`
	if got != want {
		t.Fatalf("canonicalJSON() =\n%s\nwant\n%s", got, want)
	}
}

func TestCanonicalJSONEscaping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   any
		want string
	}{
		{map[string]any{"a": "q\"uote"}, `{"a":"q\"uote"}`},
		{map[string]any{"a": "back\\slash"}, `{"a":"back\\slash"}`},
		{map[string]any{"a": "new\nline"}, `{"a":"new\nline"}`},
		{map[string]any{"a": "tab\there"}, `{"a":"tab\there"}`},
		// Control characters are escaped rather than emitted raw: a raw control byte in a
		// canonical pre-image is a byte that a downstream JSON parser may reject or normalize,
		// and a pre-image that cannot survive a round trip cannot be re-verified.
		{map[string]any{"a": string(rune(0))}, `{"a":"` + lowUnicodeEscape(0) + `"}`},
		{map[string]any{"a": string(rune(0x1f))}, `{"a":"` + lowUnicodeEscape(0x1f) + `"}`},
	}
	for _, tc := range tests {

		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			if got := canonicalJSON(tc.in); got != tc.want {
				t.Fatalf("canonicalJSON() = %s, want %s", got, tc.want)
			}
		})
	}
}

// lowUnicodeEscape builds the six-character escape the canonical encoder emits for a control
// character, without putting a raw control byte in this source file.
func lowUnicodeEscape(r rune) string {
	const hexDigits = "0123456789abcdef"
	return `\u00` + string(hexDigits[(r>>4)&0xf]) + string(hexDigits[r&0xf])
}
