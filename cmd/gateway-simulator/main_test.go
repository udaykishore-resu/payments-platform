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
		"PP_HTTP_ADDR",
		"PP_SIMULATOR_API_KEY",
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
		"PP_ENVIRONMENT":       "sandbox",
		"PP_REGION":            "local",
		"PP_HTTP_ADDR":         ":8090",
		"PP_SIMULATOR_API_KEY": "sim-key",
	}
	var cfg env
	if err := config.Load(&cfg, func(k string) (string, bool) {
		v, ok := supplied[k]
		return v, ok
	}); err != nil {
		t.Fatalf("the documented minimum was rejected: %v", err)
	}
	// There is no database group and no Kafka group: the simulator holds its state in memory on
	// purpose, because a simulator whose fixtures survive a restart makes a test suite that
	// passes locally and fails in CI.
	if cfg.AdminAddr == "" {
		t.Error("PP_ADMIN_ADDR has no default")
	}
}
