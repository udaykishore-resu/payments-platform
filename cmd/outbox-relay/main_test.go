package main

import (
	"testing"

	"github.com/udaykishore-resu/payments-platform/internal/platform/config"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// TestConfigurationReportsEveryMissingVariableAtOnce is this binary's startup-failure contract.
//
// A loader that stops at the first missing variable turns a misconfigured deployment into a
// sequence of failed rollouts. This asserts one attempt produces the whole list, and — the more
// subtle half — asserts *which* variables are required: `required` is reserved for values that
// must never have a default, and a default quietly appearing on one of them is how a security
// control becomes optional.
func TestConfigurationReportsEveryMissingVariableAtOnce(t *testing.T) {
	t.Parallel()
	var cfg env
	err := config.Load(&cfg, func(string) (string, bool) { return "", false })
	if err == nil {
		t.Fatal("a completely unset environment was accepted")
	}
	e := apierror.From(err)
	if e.Code != apierror.CodeConfigurationInvalid {
		t.Errorf("code = %s, want CONFIGURATION_INVALID", e.Code)
	}

	got := map[string]bool{}
	for _, d := range e.Details {
		got[d.Field] = true
	}
	want := []string{
		"PP_ENVIRONMENT",
		"PP_REGION",
		"PP_DATABASE_URL",
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("%s is not reported as required", name)
		}
	}
	if len(e.Details) != len(want) {
		t.Errorf("reported %d problems, want exactly %d — adding or removing a required "+
			"variable is a deliberate decision:\n%v", len(e.Details), len(want), got)
	}
}

// TestTheDocumentedMinimumIsAccepted asserts the required set really is the minimum, so an
// operator who sets exactly the variables the failure message named gets a process that starts.
func TestTheDocumentedMinimumIsAccepted(t *testing.T) {
	t.Parallel()
	supplied := map[string]string{
		"PP_ENVIRONMENT":  "sandbox",
		"PP_REGION":       "eu-central-1",
		"PP_DATABASE_URL": "postgres://u:p@localhost:5432/payments",
	}
	var cfg env
	if err := config.Load(&cfg, func(k string) (string, bool) {
		v, ok := supplied[k]
		return v, ok
	}); err != nil {
		t.Fatalf("the documented minimum was rejected: %v", err)
	}
	// A single shard is the safe default: sharding preserves per-aggregate ordering when the
	// relay scales out, and a default of zero shards would divide by zero.
	if cfg.TotalShards < 1 {
		t.Errorf("TotalShards defaults to %d; it must be at least 1", cfg.TotalShards)
	}
	if cfg.Shard < 0 || cfg.Shard >= cfg.TotalShards {
		t.Errorf("the default shard %d is outside [0, %d)", cfg.Shard, cfg.TotalShards)
	}
}
