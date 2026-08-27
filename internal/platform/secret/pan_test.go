package secret_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/platform/secret"
)

// The scheme test numbers published by the card networks. They are the correct fixtures
// precisely because they are public and worthless: a real PAN in a test file is itself the
// breach the detector exists to prevent.
var schemeTestPANs = map[string]string{
	"visa":       "4111111111111111",
	"mastercard": "5555555555554444",
	"amex":       "378282246310005",
	"discover":   "6011111111111117",
	"jcb":        "3530111333300000",
}

func TestContainsPANDetectsEveryScheme(t *testing.T) {
	// Verifies: NFR-33, NFR-39.
	t.Parallel()
	for scheme, pan := range schemeTestPANs {
		t.Run(scheme, func(t *testing.T) {
			t.Parallel()
			// Bare, and in the three shapes real clients actually send.
			variants := []string{
				pan,
				spaced(pan, 4, ' '),
				spaced(pan, 4, '-'),
				"card number: " + pan + " (do not store)",
				"txn20240211" + pan, // embedded in a longer digit run
			}
			for _, v := range variants {
				if !secret.ContainsPAN(v) {
					t.Errorf("missed a %s PAN in a %d-char field", scheme, len(v))
				}
			}
		})
	}
}

func TestContainsPANRejectsNonPANs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		input string
	}{
		{"luhn invalid, visa prefix and length", "4111111111111112"},
		{"luhn invalid 16 digits", "9876543210987654"},
		{"luhn valid but no scheme prefix", "1234567812345670"},
		{"amex prefix at the wrong length", "3782822463100050"}, // amex is 15 digits, not 16
		{"visa prefix at the wrong length", "41111111111111"},   // visa is 13, 16 or 19
		{"too short", "411111111111"},
		{"order reference", "ORD-2024-000198"},
		{"uuid", "3fa85f64-5717-4562-b3fc-2c963f66afa6"},
		{"iso timestamp", "2024-02-11T09:31:47.221Z"},
		{"amount in minor units", "1050"},
		{"empty", ""},
		{"all zeroes", "0000000000000000"}, // Luhn-valid, but 0 is not an issued IIN
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if secret.ContainsPAN(tc.input) {
				t.Errorf("false positive on %s", tc.name)
			}
		})
	}
}

// TestPANDetectorNeverLogsTheValue is the guarantee from security.md §5.4. The detector's whole
// job is to keep cardholder data out of the process; a detector that reported what it found
// would put the PAN into the rejection log line, which is exactly the outcome it exists to
// prevent — and worse, it would do so on the path that is by definition carrying a real PAN.
func TestPANDetectorNeverLogsTheValue(t *testing.T) {
	// Verifies: FR-91, NFR-39.
	t.Parallel()
	pan := schemeTestPANs["visa"]

	req := struct {
		Reference string `json:"reference"`
		Card      struct {
			Number string `json:"number"`
			Holder string `json:"holder"`
		} `json:"card"`
	}{Reference: "ORD-991"}
	req.Card.Number = pan
	req.Card.Holder = "A Merchant"

	paths := secret.ScanStruct(req)
	if len(paths) != 1 || paths[0] != "card.number" {
		t.Fatalf("paths = %v, want [card.number]", paths)
	}

	// Everything a caller can build out of the detector's output — the rejection line, the
	// security event, an error message — must be free of the value.
	rendered := fmt.Sprintf("%v %+v %q", paths, paths, paths) +
		fmt.Sprintf("sensitive data in request: fields=%s", strings.Join(paths, ","))
	if strings.Contains(rendered, pan) {
		t.Fatalf("the detector's output carried the PAN: %s", rendered)
	}
	for _, run := range []string{"4111", "1111111111", pan[:8], pan[8:]} {
		if strings.Contains(rendered, run) {
			t.Fatalf("the detector's output carried a fragment of the PAN (%q): %s", run, rendered)
		}
	}
}

func TestScanStructFindsNestedFields(t *testing.T) {
	t.Parallel()
	type item struct {
		SKU       string `json:"sku"`
		Reference string `json:"reference"`
	}
	type customer struct {
		Note *string `json:"note"`
	}
	pan := schemeTestPANs["mastercard"]
	note := "call me on " + pan

	body := struct {
		Amount   int64             `json:"amountMinor"`
		Currency string            `json:"currency"`
		Items    []item            `json:"items"`
		Metadata map[string]string `json:"metadata"`
		Customer *customer         `json:"customer"`
		Raw      []byte            `json:"raw"`
		unseen   string            // exercises the unexported-field skip
	}{
		Amount:   1050,
		Currency: "USD",
		Items: []item{
			{SKU: "SKU-1", Reference: "ORD-1"},
			{SKU: "SKU-2", Reference: "ORD-2"},
			{SKU: "SKU-3", Reference: spaced(schemeTestPANs["amex"], 4, ' ')},
		},
		Metadata: map[string]string{"legacyCard": schemeTestPANs["discover"]},
		Customer: &customer{Note: &note},
		Raw:      []byte(schemeTestPANs["jcb"]),
		unseen:   schemeTestPANs["visa"],
	}
	_ = body.unseen

	got := secret.ScanStruct(body)
	want := map[string]bool{
		"items[2].reference":  true,
		"metadata.legacyCard": true,
		"customer.note":       true,
		"raw":                 true,
	}
	if len(got) != len(want) {
		t.Fatalf("paths = %v, want %d entries", got, len(want))
	}
	for _, p := range got {
		if !want[p] {
			t.Errorf("unexpected path %q", p)
		}
		delete(want, p)
	}
	for p := range want {
		t.Errorf("missed path %q", p)
	}
}

func TestScanStructIsDepthBounded(t *testing.T) {
	t.Parallel()
	// A self-referential map is a request body an attacker cannot actually send, but a decoder
	// bug or an internal caller can produce one. The scan must terminate rather than hang a
	// request goroutine.
	m := map[string]any{}
	m["self"] = m
	m["card"] = schemeTestPANs["visa"]

	done := make(chan []string, 1)
	go func() { done <- secret.ScanStruct(m) }()
	select {
	case paths := <-done:
		if len(paths) == 0 {
			t.Fatal("expected the reachable PAN to be reported")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ScanStruct did not terminate on a cyclic value")
	}
}

func TestScanStructHandlesNilAndScalars(t *testing.T) {
	t.Parallel()
	for _, v := range []any{nil, 1, 1.5, true, (*int)(nil), []string(nil), map[string]string(nil)} {
		if paths := secret.ScanStruct(v); len(paths) != 0 {
			t.Errorf("ScanStruct(%#v) = %v, want none", v, paths)
		}
	}
}

// BenchmarkContainsPAN measures the cost on the shape the validator actually sees: the detector
// runs over every string field of every request, so its per-call cost is multiplied by the field
// count and then by the request rate.
func BenchmarkContainsPAN(b *testing.B) {
	cases := []struct {
		name  string
		input string
	}{
		{"clean_short", "ORD-2024-000198"},
		{"clean_typical", "Payment for invoice INV-2024-0099 from ACME Widgets GmbH, Berlin"},
		{"long_digit_run", "1122334455667788990011223344556677889900"},
		{"hit_visa", "4111111111111111"},
		{"hit_embedded", "reference txn20240211 4111 1111 1111 1111 trailing text"},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			var sink bool
			for i := 0; i < b.N; i++ {
				sink = secret.ContainsPAN(tc.input)
			}
			_ = sink
		})
	}
}

func BenchmarkScanStruct(b *testing.B) {
	type item struct {
		SKU       string `json:"sku"`
		Reference string `json:"reference"`
	}
	body := struct {
		Amount   int64             `json:"amountMinor"`
		Currency string            `json:"currency"`
		Items    []item            `json:"items"`
		Metadata map[string]string `json:"metadata"`
	}{
		Amount: 1050, Currency: "USD",
		Items:    []item{{"SKU-1", "ORD-1"}, {"SKU-2", "ORD-2"}, {"SKU-3", "ORD-3"}},
		Metadata: map[string]string{"channel": "web", "campaign": "spring-2024"},
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = secret.ScanStruct(body)
	}
}

// spaced re-inserts a separator every n digits, reproducing what a browser autofill or a
// hand-typed field actually produces.
func spaced(s string, n int, sep byte) string {
	var b strings.Builder
	for i, c := range []byte(s) {
		if i > 0 && i%n == 0 {
			b.WriteByte(sep)
		}
		b.WriteByte(c)
	}
	return b.String()
}
