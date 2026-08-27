package secrets

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/udaykishore-resu/payments-platform/internal/platform/secret"
)

// plaintext is the value every test below asserts is absent from every rendering. It is a
// synthetic token with no vendor prefix, so it matches no credential-detector pattern and is
// itself a legitimate committed literal.
const plaintext = "material-plaintext-must-never-render"

func testMaterial() Material {
	return NewMaterial("v7", map[string]string{
		"api_key":      plaintext,
		"webhook_hmac": plaintext + "-2",
	})
}

// TestMaterialRedactsEveryFormattingVerb is the same matrix secret_test.go runs against
// Secret[T], for the same reason: implementing String alone leaves %#v, %d and %x open, which is
// the failure mode that makes a redaction wrapper actively harmful — confidence without coverage.
func TestMaterialRedactsEveryFormattingVerb(t *testing.T) {
	t.Parallel()
	m := testMaterial()
	verbs := []string{"%v", "%+v", "%#v", "%s", "%q", "%d", "%x", "%X", "%T", "%t", "%p", "%e", "%c", "%U"}
	for _, verb := range verbs {
		t.Run(verb, func(t *testing.T) {
			t.Parallel()
			got := fmt.Sprintf(verb, m)
			if strings.Contains(got, plaintext) {
				t.Fatalf("%s rendered the plaintext: %s", verb, got)
			}
			switch verb {
			case "%T":
				// %T prints the type name and never consults Formatter. It cannot leak a value.
				return
			case "%p":
				// %p on a non-pointer takes fmt's badVerb path, which disables Formatter and
				// prints by reflection — the one verb a value type cannot intercept. What it must
				// not do is print the values, and the assertion above is that check. The rendering
				// is `%!p(secrets.Material={0x…})`: the pointer indirection in Material is
				// precisely what keeps the map behind an address here.
				if !strings.Contains(got, "%!p(") {
					t.Errorf("%%p rendered unexpectedly: %s", got)
				}
				return
			}
			want := secret.Redacted
			if verb == "%q" {
				want = `"` + secret.Redacted + `"`
			}
			if got != want {
				t.Errorf("%s = %q, want %q", verb, got, want)
			}
		})
	}
}

// TestMaterialRedactsInsideAContainingStruct covers the realistic leak: nobody prints the
// credential, they print the struct that happens to hold it.
func TestMaterialRedactsInsideAContainingStruct(t *testing.T) {
	t.Parallel()
	type dispatch struct {
		Gateway   string
		Material  Material
		AttemptID string
	}
	d := dispatch{Gateway: "stripe", Material: testMaterial(), AttemptID: "att_1"}
	for _, verb := range []string{"%v", "%+v", "%#v"} {
		got := fmt.Sprintf(verb, d)
		if strings.Contains(got, plaintext) {
			t.Errorf("%s of the containing struct leaked the plaintext: %s", verb, got)
		}
	}
}

func TestMaterialRedactsThroughJSON(t *testing.T) {
	t.Parallel()
	m := testMaterial()

	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	if string(b) != `"`+secret.Redacted+`"` {
		t.Errorf("json.Marshal = %s, want the placeholder", b)
	}

	type envelope struct {
		Gateway  string   `json:"gateway"`
		Material Material `json:"material"`
	}
	b, err = json.Marshal(envelope{Gateway: "stripe", Material: m})
	if err != nil {
		t.Fatalf("marshalling the envelope: %v", err)
	}
	if bytes.Contains(b, []byte(plaintext)) {
		t.Errorf("the envelope leaked the plaintext: %s", b)
	}

	// A Material used as a map key goes through MarshalText, which encoding/json consults
	// instead of MarshalJSON for keys — the gap a type that implements only MarshalJSON has.
	b, err = json.Marshal(map[string]any{"k": m})
	if err != nil {
		t.Fatalf("marshalling a map: %v", err)
	}
	if bytes.Contains(b, []byte(plaintext)) {
		t.Errorf("the map value leaked the plaintext: %s", b)
	}
}

func TestMaterialRedactsThroughSlog(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	m := testMaterial()

	log.Info("dispatching", slog.Any("credentials", m))
	log.Info("dispatching", "credentials", m)
	log.With("credentials", m).Info("dispatching")

	if strings.Contains(buf.String(), plaintext) {
		t.Fatalf("slog leaked the plaintext: %s", buf.String())
	}
	if !strings.Contains(buf.String(), secret.Redacted) {
		t.Errorf("slog did not emit the placeholder: %s", buf.String())
	}
}

// TestMaterialValueIsTheOnlyWayOut asserts the positive half: the accessor works, distinguishes
// absent from empty, and returns exactly what was stored.
func TestMaterialValueIsTheOnlyWayOut(t *testing.T) {
	t.Parallel()
	m := NewMaterial("v1", map[string]string{"api_key": plaintext, "empty": ""})

	if v, ok := m.Value("api_key"); !ok || v != plaintext {
		t.Errorf("Value(api_key) = %q, %v", v, ok)
	}
	if v, ok := m.Value("empty"); !ok || v != "" {
		t.Errorf("an empty stored field must report present: %q, %v", v, ok)
	}
	if _, ok := m.Value("absent"); ok {
		t.Errorf("an absent field must report absent")
	}
	if m.Version() != "v1" {
		t.Errorf("Version() = %q", m.Version())
	}
}

// TestMaterialCopiesItsInputs covers code-conventions §11: retaining the caller's map would
// leave the plaintext reachable through a reference the caller still holds.
func TestMaterialCopiesItsInputs(t *testing.T) {
	t.Parallel()
	in := map[string]string{"api_key": plaintext}
	m := NewMaterial("v1", in)

	in["api_key"] = "mutated"
	in["injected"] = "new"

	if v, _ := m.Value("api_key"); v != plaintext {
		t.Errorf("mutating the caller's map changed the material: %q", v)
	}
	if _, ok := m.Value("injected"); ok {
		t.Errorf("a field added to the caller's map appeared in the material")
	}

	fields := m.Fields()
	fields[0] = "clobbered"
	if got := m.Fields(); got[0] != "api_key" {
		t.Errorf("Fields() returned the live backing array: %v", got)
	}
}

// TestZeroMaterialIsUsable: a zero value must redact and answer honestly rather than panic, so
// that an error path returning Material{} cannot become a nil dereference on the money path.
func TestZeroMaterialIsUsable(t *testing.T) {
	t.Parallel()
	var m Material
	if got := fmt.Sprintf("%+v", m); got != secret.Redacted {
		t.Errorf("the zero Material renders %q", got)
	}
	if _, ok := m.Value("anything"); ok {
		t.Errorf("the zero Material claims to hold a field")
	}
	if len(m.Fields()) != 0 || m.Version() != "" {
		t.Errorf("the zero Material is not empty")
	}
}
