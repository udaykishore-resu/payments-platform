// Package config is the platform's configuration layer: twelve-factor process configuration on
// one side, and the data plane's fail-static merchant-configuration snapshot on the other.
//
// The two are deliberately different mechanisms because they answer different questions with
// different failure modes. Process configuration (env.go) is deploy-time desired state: it is
// read once at startup, it is wrong only if the deployment is wrong, and the correct response to
// a missing value is to refuse to start. Merchant configuration (provider.go) is runtime desired
// state owned by the control plane: it changes while the process runs, it arrives asynchronously,
// and the correct response to it being unavailable is emphatically *not* to stop processing
// payments — it is to keep serving the last known good snapshot, within a bound, with a defined
// cliff (baseline §15).
//
// Feature flags (flags.go) sit inside the second mechanism but carry one extra rule that is
// worth the whole file: a payment resolves its flags once, at creation, and is judged by that
// resolution for its entire lifetime.
package config

import (
	"fmt"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/platform/secret"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// Lookup resolves an environment variable. It is a parameter rather than a direct call to
// os.LookupEnv so that loading is testable without mutating process state — a test that sets
// real environment variables cannot run in parallel with any other test, and the whole suite
// pays for it.
type Lookup func(key string) (string, bool)

// Struct tags read by Load.
//
//	env:"PP_DATABASE_URL"     the variable name; a field without one is skipped
//	default:"5s"              used when the variable is absent
//	required:"true"           absent and with no default is an error
//	secret:"true"             force redaction regardless of the field's name
const (
	tagEnv      = "env"
	tagDefault  = "default"
	tagRequired = "required"
	tagSecret   = "secret"
)

// secretNamePattern matches variable and field names that must never be rendered in a startup
// log. It is the same pattern the admission policy and the CI scanner use (security.md §5.2), so
// a value that would be rejected in a pod spec is also masked here.
//
// Matching on the *name* rather than on the value is the right way round: entropy heuristics on
// values produce both false negatives (a short password) and false positives (a base64 build
// hash), whereas a name is chosen by a developer who knew what the field was for.
// The `(db|database|redis|kafka|amqp)_?(url|uri)` alternative is there because a datastore URL
// conventionally embeds its own credentials (`postgres://user:pass@host/db`). Generic `*_URL`
// variables are deliberately *not* matched — a JWKS URL or a webhook endpoint is exactly the kind
// of value the startup dump exists to show, and masking all of them would make the dump useless,
// which is how people stop logging configuration at all.
var secretNamePattern = regexp.MustCompile(`(?i)(secret|password|passwd|pwd|token|api_?key|credential|private_?key|signing_?key|salt|dsn|conn(ection)?_?string|(db|database|redis|kafka|amqp)_?(url|uri))`)

// Load populates dest from the environment.
//
// # Reporting every missing variable, not the first
//
// A loader that stops at the first missing variable turns a misconfigured deployment into a
// sequence of failed rollouts: fix one, redeploy, discover the next, fix it, redeploy. With six
// missing variables that is six deploys and an afternoon. Collecting every failure and reporting
// them together makes it one. The same argument applies to type-conversion failures, which are
// collected alongside.
//
// The error is a VALIDATION_FAILED carrying one Detail per problem, so the message a human sees
// at startup enumerates exactly what to set.
func Load(dest any, lookup Lookup) error {
	rv := reflect.ValueOf(dest)
	if rv.Kind() != reflect.Pointer || rv.IsNil() || rv.Elem().Kind() != reflect.Struct {
		return apierror.New(apierror.CodeInternalError, "config: Load requires a non-nil pointer to a struct")
	}
	var details []apierror.Detail
	loadStruct(rv.Elem(), lookup, &details)
	if len(details) == 0 {
		return nil
	}
	sort.SliceStable(details, func(i, j int) bool { return details[i].Field < details[j].Field })
	return apierror.Newf(apierror.CodeConfigurationInvalid,
		"configuration is incomplete: %d problem(s)", len(details)).WithDetails(details...)
}

// LoadFromEnv is Load against the real process environment.
func LoadFromEnv(dest any) error {
	return Load(dest, func(k string) (string, bool) {
		v, ok := envLookup(k)
		return v, ok
	})
}

func loadStruct(v reflect.Value, lookup Lookup, details *[]apierror.Detail) {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		field, fv := t.Field(i), v.Field(i)
		if !fv.CanSet() {
			continue
		}
		// An embedded or nested struct that is not one of the types we decode is descended into,
		// so a configuration can be grouped by concern without every group needing its own Load
		// call.
		if fv.Kind() == reflect.Struct && !isDecodable(fv.Type()) {
			loadStruct(fv, lookup, details)
			continue
		}
		name := field.Tag.Get(tagEnv)
		if name == "" {
			continue
		}
		raw, present := lookup(name)
		if !present || raw == "" {
			if def, hasDefault := field.Tag.Lookup(tagDefault); hasDefault {
				raw, present = def, true
			}
		}
		if !present || raw == "" {
			if field.Tag.Get(tagRequired) == "true" {
				*details = append(*details, apierror.Detail{
					Field:   name,
					Code:    "MISSING",
					Message: "required environment variable is not set",
					RuleID:  "L0.CONFIG_REQUIRED_PRESENT",
				})
			}
			continue
		}
		if err := assign(fv, raw); err != nil {
			*details = append(*details, apierror.Detail{
				Field: name,
				Code:  "INVALID",
				// The message names the type and nothing else. The underlying error is not
				// included because strconv and time.ParseDuration quote the offending input, and
				// a malformed connection string is still a connection string: echoing it into a
				// startup log is exactly the leak this package exists to prevent. The variable's
				// name plus its expected type is enough for anyone fixing the deployment, and
				// they can read the value from their own secret store.
				Message: fmt.Sprintf("cannot be parsed as %s", field.Type),
				RuleID:  "L0.CONFIG_TYPE_VALID",
			})
		}
	}
}

// isDecodable reports whether a struct type is one assign knows how to fill, as opposed to a
// grouping struct to descend into.
func isDecodable(t reflect.Type) bool {
	switch t {
	case reflect.TypeOf(time.Duration(0)), reflect.TypeOf(time.Time{}):
		return true
	}
	// secret.Secret[T] is a struct with an unexported field; it is decodable through its
	// UnmarshalJSON-free path below, keyed on the type name so no import cycle is needed.
	return strings.HasPrefix(t.String(), "secret.Secret[")
}

func assign(fv reflect.Value, raw string) error {
	raw = strings.TrimSpace(raw)

	// Secret-typed fields first, so credential material is wrapped the instant it leaves the
	// environment and exists as a bare string for exactly one statement.
	if s, ok := fv.Addr().Interface().(*secret.Secret[string]); ok {
		*s = secret.New(raw)
		return nil
	}
	if s, ok := fv.Addr().Interface().(*secret.Secret[[]byte]); ok {
		*s = secret.New([]byte(raw))
		return nil
	}

	switch fv.Kind() {
	case reflect.String:
		fv.SetString(raw)
	case reflect.Bool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return err
		}
		fv.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		// time.Duration is an int64 with a string form; check it before the numeric path or
		// "30s" becomes a parse error and "30" becomes 30 nanoseconds.
		if fv.Type() == reflect.TypeOf(time.Duration(0)) {
			d, err := time.ParseDuration(raw)
			if err != nil {
				return err
			}
			fv.SetInt(int64(d))
			return nil
		}
		n, err := strconv.ParseInt(raw, 10, fv.Type().Bits())
		if err != nil {
			return err
		}
		fv.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(raw, 10, fv.Type().Bits())
		if err != nil {
			return err
		}
		fv.SetUint(n)
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(raw, fv.Type().Bits())
		if err != nil {
			return err
		}
		fv.SetFloat(f)
	case reflect.Slice:
		if fv.Type().Elem().Kind() != reflect.String {
			return fmt.Errorf("unsupported slice element %s", fv.Type().Elem())
		}
		parts := strings.Split(raw, ",")
		out := reflect.MakeSlice(fv.Type(), 0, len(parts))
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				out = reflect.Append(out, reflect.ValueOf(p).Convert(fv.Type().Elem()))
			}
		}
		fv.Set(out)
	case reflect.Struct:
		if fv.Type() == reflect.TypeOf(time.Time{}) {
			ts, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				return err
			}
			fv.Set(reflect.ValueOf(ts.UTC()))
			return nil
		}
		return fmt.Errorf("unsupported struct type %s", fv.Type())
	default:
		return fmt.Errorf("unsupported kind %s", fv.Kind())
	}
	return nil
}

// RedactedEntry is one line of the startup configuration dump.
type RedactedEntry struct {
	Name  string
	Value string
	// Masked reports whether the value was replaced. Exposed so a reviewer can see at a glance
	// that the sensitive rows really were masked rather than merely happening to look benign.
	Masked bool
}

// Redacted renders a loaded configuration for the startup log.
//
// Logging the effective configuration at startup is genuinely valuable — most "it works on
// staging" incidents are answered by one line of it — and it is also the single most common way
// a database password reaches a log aggregator. This renderer resolves the tension by making
// masking the default for anything whose *name* suggests a secret, plus anything explicitly
// tagged, plus anything held in a secret.Secret, which redacts itself anyway.
//
// A masked value renders as the same constant the rest of the platform uses, so a log-pipeline
// detector, a test and a human reading a dashboard all match on one token.
func Redacted(v any) []RedactedEntry {
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil
	}
	var out []RedactedEntry
	collect(rv, &out)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// RedactedString renders Redacted as a single `KEY=value` line per entry, ready for a log field.
func RedactedString(v any) string {
	entries := Redacted(v)
	parts := make([]string, 0, len(entries))
	for _, e := range entries {
		parts = append(parts, e.Name+"="+e.Value)
	}
	return strings.Join(parts, " ")
}

func collect(v reflect.Value, out *[]RedactedEntry) {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		field, fv := t.Field(i), v.Field(i)
		if fv.Kind() == reflect.Struct && !isDecodable(fv.Type()) {
			collect(fv, out)
			continue
		}
		name := field.Tag.Get(tagEnv)
		if name == "" {
			continue
		}
		masked := field.Tag.Get(tagSecret) == "true" ||
			secretNamePattern.MatchString(name) ||
			secretNamePattern.MatchString(field.Name) ||
			isSecretType(fv.Type())
		if masked {
			*out = append(*out, RedactedEntry{Name: name, Value: secret.Redacted, Masked: true})
			continue
		}
		*out = append(*out, RedactedEntry{Name: name, Value: render(fv)})
	}
}

func isSecretType(t reflect.Type) bool { return strings.HasPrefix(t.String(), "secret.Secret[") }

func render(fv reflect.Value) string {
	if fv.Type() == reflect.TypeOf(time.Duration(0)) {
		return time.Duration(fv.Int()).String()
	}
	switch fv.Kind() {
	case reflect.String:
		return fv.String()
	case reflect.Slice:
		parts := make([]string, 0, fv.Len())
		for i := 0; i < fv.Len(); i++ {
			parts = append(parts, fmt.Sprint(fv.Index(i).Interface()))
		}
		return strings.Join(parts, ",")
	default:
		return fmt.Sprint(fv.Interface())
	}
}

// envLookup is os.LookupEnv, isolated so that the rest of this file has no dependency on the
// process environment and can be exercised with a map.
func envLookup(key string) (string, bool) { return os.LookupEnv(key) }
