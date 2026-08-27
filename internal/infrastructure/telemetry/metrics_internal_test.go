package telemetry

import (
	"errors"
	"strings"
	"testing"
)

// TestEveryForbiddenLabelNameIsRejected walks forbiddenLabelNames itself rather than a
// transcribed copy of it.
//
// The external table in metrics_test.go transcribes the §22.3 list on purpose — a test that
// derived its expectations from the map would pass for a map that had lost half its entries.
// This one is the other half of that pair, and it answers the opposite question: is every
// entry the map *does* carry actually reachable through ValidateLabels? An entry whose key is
// not in canonical snake_case, or a duplicate that overwrites another, would silently stop
// rejecting anything, and no transcribed table would notice because the transcription would
// still list the name.
//
// Each name is probed in four spellings. The guard normalizes before comparing (see
// normalizeLabelName), so all four must be refused: if only the declared spelling were, the
// rule would be a rename away from being bypassed, which is the failure mode §22.3 exists to
// prevent.
func TestEveryForbiddenLabelNameIsRejected(t *testing.T) {
	t.Parallel()

	if len(forbiddenLabelNames) == 0 {
		t.Fatal("forbiddenLabelNames is empty; the cardinality guard rejects nothing")
	}

	for name, reason := range forbiddenLabelNames {
		if reason == "" {
			t.Errorf("%q carries no reason; the rejection error is the only place the rule is explained", name)
		}
		for _, spelling := range []string{
			name,                               // as declared, e.g. merchant_id
			toCamel(name),                      // merchantId
			strings.ToUpper(name),              // MERCHANT_ID
			strings.ReplaceAll(name, "_", "-"), // merchant-id
		} {
			t.Run(spelling, func(t *testing.T) {
				t.Parallel()
				err := ValidateLabels("pp_example_total", []string{"gateway", spelling})
				if err == nil {
					t.Fatalf("label %q was accepted; §22.3 forbids %q in every spelling", spelling, name)
				}
				var fe *ErrForbiddenLabel
				if !asForbidden(err, &fe) {
					t.Fatalf("error is %T, want *ErrForbiddenLabel", err)
				}
				if fe.Label != spelling {
					t.Errorf("error names label %q, want %q", fe.Label, spelling)
				}
				if fe.Reason != reason {
					t.Errorf("reason for %q is %q, want the map's %q", spelling, fe.Reason, reason)
				}
			})
		}
	}
}

// TestForbiddenLabelKeysAreCanonical pins the map's keys to snake_case. The index is built by
// normalizing the keys, so a key written any other way still works — but it would silently
// collide with, or shadow, another entry, and the reason string attached to a rejection would
// then be the wrong one. Keeping the keys canonical keeps that impossible to introduce.
func TestForbiddenLabelKeysAreCanonical(t *testing.T) {
	t.Parallel()

	seen := make(map[string]string, len(forbiddenLabelNames))
	for name := range forbiddenLabelNames {
		if name != strings.ToLower(name) || strings.ContainsAny(name, "-. ") {
			t.Errorf("%q is not canonical snake_case", name)
		}
		n := normalizeLabelName(name)
		if other, dup := seen[n]; dup {
			t.Errorf("%q and %q normalize to the same name %q; one of the two reasons is unreachable", name, other, n)
		}
		seen[n] = name
	}
}

// toCamel renders a snake_case label the way an author who has just been refused writes it.
func toCamel(s string) string {
	parts := strings.Split(s, "_")
	for i := 1; i < len(parts); i++ {
		if parts[i] == "" {
			continue
		}
		parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
	}
	return strings.Join(parts, "")
}

// asForbidden is errors.As specialised to *ErrForbiddenLabel, kept local so this file needs no
// import beyond strings and testing.
func asForbidden(err error, target **ErrForbiddenLabel) bool {
	return errors.As(err, target)
}
