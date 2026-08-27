package secret

import (
	"reflect"
	"strconv"
	"strings"
)

// The detector's shape is fixed by baseline §17.2 and security.md §6.3: strip separators, find
// digit runs of 13–19, Luhn-check them, and confirm the leading digits against a known issuer
// identification number (IIN) range. It runs in the L1 validator over every string field of
// every request, which is why it is written to allocate nothing on the common path.
const (
	// minPANLen and maxPANLen bracket every card number a scheme actually issues. 13 covers the
	// legacy 13-digit Visa, 19 covers Visa/Discover/UnionPay extended numbers.
	minPANLen = 13
	maxPANLen = 19

	// MaxScanDepth bounds the recursion of ScanStruct. It mirrors the request-body nesting cap
	// the L1 validator enforces before this detector runs, so a document deep enough to escape
	// the scan has already been rejected for being too deep. Without a cap, a self-referential
	// map would hang a request goroutine — a denial of service delivered through the control
	// that exists to prevent a breach.
	MaxScanDepth = 32
)

// ContainsPAN reports whether s contains something that is very likely a primary account number.
//
// It returns a bool and nothing else, and this is a security property rather than an API
// preference: a function that returned the match would put a PAN into a caller's variable, from
// where it reaches an error message and then a log. There is deliberately no variant of this
// function that tells you what it found. The rejection path logs the field path and the field
// length; that is enough to tell a merchant which field to fix and never enough to reconstruct
// the number.
//
// Separators (space, hyphen, period) are stripped before scanning, because "4111 1111 1111 1111"
// and "4111-1111-1111-1111" are how humans and badly written clients actually send card numbers.
//
// The false-positive trade-off, stated at the decision point. Roughly one in ten random
// 16-digit numbers is Luhn-valid by chance, so a merchant's 16-digit Luhn-valid order number
// whose first digits happen to fall in a scheme range will be rejected with
// 400 SENSITIVE_DATA_IN_REQUEST. The IIN check plus the per-scheme length check (a 16-digit
// number starting 34 is not an Amex, because Amex is 15) cuts that rate by more than an order
// of magnitude, but it does not reach zero, and it is not supposed to. A false positive is a
// 400 that a merchant fixes by changing a reference format — recoverable, visible, and cheap.
// A false negative is cardholder data inside a system assessed at SAQ-A, which is a reportable
// PCI scope breach, an incident, and potentially a re-assessment of the whole platform —
// unrecoverable, invisible, and expensive. When the two errors are that asymmetric the detector
// belongs on the blocking side of the line, and every tuning decision below resolves that way.
func ContainsPAN(s string) bool {
	if len(s) < minPANLen {
		return false
	}
	// Digit runs are accumulated into a stack array; a run longer than this escapes to the heap
	// through append, which is fine because a 64-digit run is not a request field we optimize for.
	var stack [64]byte
	run := stack[:0]

	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			run = append(run, c-'0')
		case c == ' ' || c == '-' || c == '.' || c == '\t':
			// A separator does not terminate a run: that is the entire point of stripping them.
		default:
			if scanRun(run) {
				return true
			}
			run = run[:0]
		}
	}
	return scanRun(run)
}

// scanRun decides whether one maximal run of digits contains a PAN, using two rules chosen for
// two different threats.
//
//   - A run of 13–19 digits is checked *anchored*: the whole run must be a valid PAN. This is
//     the accidental case, and it is by far the common one — a merchant who sends cardholder
//     data sends it as the field value, because they believe it is a card field.
//   - A run of 20 or more digits is checked with a sliding window. A digit run that long is not
//     a plausible single identifier, and burying a PAN inside one is an evasion technique rather
//     than a mistake.
//
// The rejected alternative was to slide a window over every run. It sounds strictly safer and is
// not: a random 16-digit merchant order number contains four 13-digit windows, three 14-digit
// windows and two 15-digit windows, and the chance that one of them starts with a scheme digit
// and is Luhn-valid pushes the false-positive rate on ordinary references from roughly 2 % to
// well over 5 %. At platform volume that is a steady stream of 400s on legitimate traffic, which
// is how a security control gets an exemption carved into it and then gets disabled. The
// residual gap — a PAN with a handful of extra adjacent digits, run length 14–19 — is covered by
// the two independent detectors on the same data (the WAF rule and the log-pipeline backstop of
// security.md §6.3), which is what defence in depth is for.
func scanRun(d []byte) bool {
	switch {
	case len(d) < minPANLen:
		return false
	case len(d) <= maxPANLen:
		return plausibleIIN(d) && luhn(d)
	}
	for n := maxPANLen; n >= minPANLen; n-- {
		for i := 0; i+n <= len(d); i++ {
			w := d[i : i+n]
			// IIN first: it is a handful of integer comparisons and rejects the overwhelming
			// majority of windows, where Luhn is a full pass over 13–19 digits.
			if !plausibleIIN(w) {
				continue
			}
			if luhn(w) {
				return true
			}
		}
	}
	return false
}

// luhn runs the mod-10 checksum over a slice of digit values (0–9, not ASCII).
func luhn(d []byte) bool {
	sum := 0
	double := false
	for i := len(d) - 1; i >= 0; i-- {
		v := int(d[i])
		if double {
			v *= 2
			if v > 9 {
				v -= 9
			}
		}
		sum += v
		double = !double
	}
	return sum%10 == 0
}

// plausibleIIN reports whether the leading digits and the length together match a range some
// scheme actually issues.
//
// Both halves matter. Prefix alone is weak — one digit in ten starts with '4' — and length alone
// is weaker still. Together they are a real filter: a Luhn-valid 16-digit number beginning 34 is
// rejected as a PAN candidate because American Express issues 15 digits, and a Luhn-valid
// 14-digit number beginning 4 is rejected because Visa issues 13, 16 or 19.
//
// The ranges are deliberately generous where a scheme is still expanding (UnionPay, JCB) and
// tight where it is not (Amex, Mastercard). An unknown future range means a false negative,
// which is the expensive direction, so the table is reviewed whenever a scheme is added to the
// platform's payment-method set.
func plausibleIIN(d []byte) bool {
	n := len(d)
	if n < minPANLen {
		return false
	}
	// Prefixes as integers, computed once without a closure: this function is called once per
	// candidate window, which at request rates is the hottest line in the validator.
	d1 := int(d[0])
	d2 := d1*10 + int(d[1])
	d3 := d2*10 + int(d[2])
	d4 := d3*10 + int(d[3])
	d6 := (d4*10+int(d[4]))*10 + int(d[5])

	switch {
	case d1 == 4: // Visa
		return n == 13 || n == 16 || n == 19
	case d2 >= 51 && d2 <= 55: // Mastercard
		return n == 16
	case d4 >= 2221 && d4 <= 2720: // Mastercard 2-series
		return n == 16
	case d2 == 34 || d2 == 37: // American Express
		return n == 15
	case d4 == 6011: // Discover
		return n == 16 || n == 19
	case d3 >= 644 && d3 <= 649: // Discover
		return n == 16 || n == 19
	case d2 == 65: // Discover
		return n == 16 || n == 19
	case d6 >= 622126 && d6 <= 622925: // Discover / China UnionPay co-brand
		return n == 16 || n == 19
	case d4 >= 3528 && d4 <= 3589: // JCB
		return n >= 16 && n <= 19
	case d3 >= 300 && d3 <= 305, d4 == 3095, d2 == 36, d2 == 38, d2 == 39: // Diners Club
		return n >= 14 && n <= 19
	case d2 == 62: // UnionPay
		return n >= 16 && n <= 19
	case d4 == 5018, d4 == 5020, d4 == 5038, d4 == 5893,
		d4 == 6304, d4 == 6759, d4 == 6761, d4 == 6762, d4 == 6763: // Maestro
		return n >= 13 && n <= 19
	default:
		return false
	}
}

// ScanStruct walks v and returns the JSON-ish paths of every string-bearing field that trips the
// detector. An empty result means the value is clean.
//
// It returns paths and never values, for the same reason ContainsPAN returns a bool: the caller
// of this function is the code that builds the 400 response and the security event, and neither
// of those may contain cardholder data. `[]string{"card.number", "items[2].reference"}` is
// exactly what a merchant needs to fix their integration and exactly nothing an attacker or a
// log index can use.
//
// Coverage and its limits: exported string fields, []byte fields, map values and slice/array
// elements are scanned, recursively, to MaxScanDepth. Unexported fields are not reachable
// through reflection and are not scanned — which is also why a Secret[T] is skipped rather than
// unwrapped. That is correct: a Secret's contents are a credential the platform put there, not
// merchant input arriving from the internet, and unwrapping one here would mean this function
// held plaintext.
func ScanStruct(v any) []string {
	if v == nil {
		return nil
	}
	var paths []string
	scanValue(reflect.ValueOf(v), "", 0, &paths)
	return paths
}

func scanValue(rv reflect.Value, path string, depth int, out *[]string) {
	if depth > MaxScanDepth || !rv.IsValid() {
		return
	}
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			return
		}
		scanValue(rv.Elem(), path, depth+1, out)

	case reflect.String:
		if ContainsPAN(rv.String()) {
			*out = append(*out, path)
		}

	case reflect.Slice, reflect.Array:
		// A []byte carrying digits is a PAN just as much as a string is; some decoders produce
		// one where the API contract says the other.
		if rv.Type().Elem().Kind() == reflect.Uint8 && rv.Kind() == reflect.Slice {
			if ContainsPAN(string(rv.Bytes())) {
				*out = append(*out, path)
			}
			return
		}
		for i := 0; i < rv.Len(); i++ {
			scanValue(rv.Index(i), path+"["+strconv.Itoa(i)+"]", depth+1, out)
		}

	case reflect.Map:
		for _, k := range rv.MapKeys() {
			key := "*"
			if k.Kind() == reflect.String {
				key = k.String()
			}
			scanValue(rv.MapIndex(k), join(path, key), depth+1, out)
		}

	case reflect.Struct:
		t := rv.Type()
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			scanValue(rv.Field(i), join(path, fieldName(f)), depth+1, out)
		}
	}
}

// fieldName prefers the JSON tag, because the path reported to a merchant must name the field as
// it appears in the request they sent, not as the Go struct spells it.
func fieldName(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if tag == "" || tag == "-" {
		return f.Name
	}
	if i := strings.IndexByte(tag, ','); i >= 0 {
		tag = tag[:i]
	}
	if tag == "" {
		return f.Name
	}
	return tag
}

func join(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

// --- requirement traceability ----------------------------------------------------------------
//
// Implements: FR-91, NFR-33, NFR-39.
//
// The primary-account-number detector that keeps card data out of this platform, and out of
// its logs when someone sends it anyway
//
// The matrix in docs/spec/09-traceability.md is derived from these annotations by
// scripts/traceability.sh; baseline §26 fails the build on a requirement that has none.
