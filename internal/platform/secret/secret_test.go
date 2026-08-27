package secret_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/udaykishore-resu/payments-platform/internal/platform/secret"
)

// plaintext is a value distinctive enough that any leak is unambiguous in an assertion failure.
const plaintext = "sk_test_FAKE_NOT_A_REAL_KEY_9f2b7c41"

// credentials mirrors the shape a gateway adapter actually holds: an identifier that is safe to
// log next to a secret that is not. The Secret field is exported on purpose — that is the
// pattern the package documents, and the pattern the %#v assertion below exercises.
type credentials struct {
	GatewayID string                `json:"gatewayId"`
	APIKey    secret.Secret[string] `json:"apiKey"`
}

// TestSecretRedactsEveryFormattingVerb is the test that justifies the type existing. A wrapper
// that redacts %v but leaks through %#v is worse than no wrapper: it converts a habit of care
// into a habit of confidence, and the leak then happens in the one place nobody reviews.
func TestSecretRedactsEveryFormattingVerb(t *testing.T) {
	// Verifies: NFR-32.
	t.Parallel()
	s := secret.New(plaintext)

	verbs := []struct {
		verb string
		want string
	}{
		{"%v", secret.Redacted},
		{"%s", secret.Redacted},
		{"%+v", secret.Redacted},
		{"%#v", secret.Redacted},
		{"%q", `"` + secret.Redacted + `"`},
		{"%d", secret.Redacted},
		{"%x", secret.Redacted},
		{"%X", secret.Redacted},
		{"%08s", secret.Redacted},
		{"%-20v|", secret.Redacted + "|"},
	}
	for _, tc := range verbs {
		t.Run(tc.verb, func(t *testing.T) {
			t.Parallel()
			got := fmt.Sprintf(tc.verb, s)
			if strings.Contains(got, plaintext) {
				t.Fatalf("verb %s leaked the plaintext: %s", tc.verb, got)
			}
			if got != tc.want {
				t.Fatalf("verb %s = %q, want %q", tc.verb, got, tc.want)
			}
		})
	}
}

// TestSecretRedactsInsideAContainingStruct covers the realistic leak: nobody prints the Secret,
// they print the struct that holds it, from a debug line that survives review.
func TestSecretRedactsInsideAContainingStruct(t *testing.T) {
	t.Parallel()
	c := credentials{GatewayID: "gw_adyen", APIKey: secret.New(plaintext)}

	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		got := fmt.Sprintf(verb, c)
		if strings.Contains(got, plaintext) {
			t.Fatalf("verb %s on containing struct leaked the plaintext: %s", verb, got)
		}
		if !strings.Contains(got, secret.Redacted) {
			t.Fatalf("verb %s on containing struct did not render the placeholder: %s", verb, got)
		}
		if !strings.Contains(got, "gw_adyen") {
			t.Fatalf("verb %s suppressed the non-secret field too: %s", verb, got)
		}
	}

	// A pointer to the struct takes a different path through fmt; it must land in the same place.
	if got := fmt.Sprintf("%+v", &c); strings.Contains(got, plaintext) {
		t.Fatalf("pointer formatting leaked the plaintext: %s", got)
	}
}

func TestSecretRedactsThroughJSON(t *testing.T) {
	t.Parallel()

	b, err := json.Marshal(secret.New(plaintext))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != `"`+secret.Redacted+`"` {
		t.Fatalf("json.Marshal = %s", b)
	}

	b, err = json.Marshal(credentials{GatewayID: "gw_adyen", APIKey: secret.New(plaintext)})
	if err != nil {
		t.Fatalf("marshal struct: %v", err)
	}
	if strings.Contains(string(b), plaintext) {
		t.Fatalf("json.Marshal of containing struct leaked the plaintext: %s", b)
	}

	// Indented encoding walks a different code path in encoding/json.
	b, err = json.MarshalIndent(map[string]any{"creds": credentials{APIKey: secret.New(plaintext)}}, "", "  ")
	if err != nil {
		t.Fatalf("marshal indent: %v", err)
	}
	if strings.Contains(string(b), plaintext) {
		t.Fatalf("json.MarshalIndent leaked the plaintext: %s", b)
	}

	// A Secret used as a map key goes through MarshalText, not MarshalJSON.
	b, err = json.Marshal(map[secret.Secret[string]]int{secret.New(plaintext): 1})
	if err != nil {
		t.Fatalf("marshal map key: %v", err)
	}
	if strings.Contains(string(b), plaintext) {
		t.Fatalf("map-key encoding leaked the plaintext: %s", b)
	}
}

func TestSecretRedactsThroughSlog(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))

	log.Info("gateway credentials loaded",
		"gatewayId", "gw_adyen",
		"apiKey", secret.New(plaintext),
		"creds", credentials{GatewayID: "gw_adyen", APIKey: secret.New(plaintext)},
	)
	log.Error("gateway auth failed", slog.Group("gateway", slog.Any("key", secret.New(plaintext))))

	out := buf.String()
	if strings.Contains(out, plaintext) {
		t.Fatalf("slog leaked the plaintext: %s", out)
	}
	if !strings.Contains(out, secret.Redacted) {
		t.Fatalf("slog did not render the placeholder: %s", out)
	}
}

func TestSecretRedactsNonStringPayloads(t *testing.T) {
	t.Parallel()
	key := secret.New([]byte{0xde, 0xad, 0xbe, 0xef})

	for _, verb := range []string{"%v", "%s", "%x", "%#v", "%+v"} {
		if got := fmt.Sprintf(verb, key); got != secret.Redacted {
			t.Fatalf("Secret[[]byte] verb %s = %q", verb, got)
		}
	}

	type dek struct{ Material secret.Secret[[]byte] }
	if got := fmt.Sprintf("%#v", dek{Material: key}); strings.Contains(got, "deadbeef") ||
		strings.Contains(got, "222") /* decimal 0xde */ {
		t.Fatalf("Secret[[]byte] leaked through a containing struct: %s", got)
	}
}

func TestExposeReturnsThePlaintext(t *testing.T) {
	t.Parallel()
	if got := secret.New(plaintext).Expose(); got != plaintext {
		t.Fatalf("Expose = %q, want %q", got, plaintext)
	}
	if got := secret.New(42).Expose(); got != 42 {
		t.Fatalf("Expose[int] = %d", got)
	}
}

// TestUnmarshalJSONDoesNotEchoTheValue: the decoder's own error text quotes the offending input,
// which for this type is the secret. The wrapper must not pass that through.
func TestUnmarshalJSONDoesNotEchoTheValue(t *testing.T) {
	t.Parallel()

	var c credentials
	if err := json.Unmarshal([]byte(`{"gatewayId":"gw_adyen","apiKey":"`+plaintext+`"}`), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.APIKey.Expose() != plaintext {
		t.Fatalf("round-trip lost the value: %q", c.APIKey.Expose())
	}

	err := json.Unmarshal([]byte(`{"apiKey":12345678901234567890}`), &c)
	if err == nil {
		t.Fatal("expected a type error decoding a number into Secret[string]")
	}
	if strings.Contains(err.Error(), "12345678901234567890") {
		t.Fatalf("decode error echoed the offending value: %v", err)
	}
}
