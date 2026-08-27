package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

// TestDispatch covers the CLI's routing and its exit-code contract.
//
// The exit codes are part of the interface: a runbook branches on them, and CI branches on them.
// 0 means the command ran and succeeded, 1 means it ran and reported a failure (a broken audit
// chain, a failed drill), and 2 means it could not run at all. Conflating 1 and 2 is what makes a
// pipeline treat "the database is unreachable" as "the audit chain is broken".
func TestDispatch(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  string
		wantErr  string
	}{
		{
			name: "no arguments prints usage and exits 2",
			args: nil, wantCode: 2, wantErr: "platformctl",
		},
		{
			name: "help exits 0", args: []string{"help"}, wantCode: 0, wantOut: "Commands:",
		},
		{
			name: "an unknown command exits 2 and names it",
			args: []string{"frobnicate"}, wantCode: 2, wantErr: `unknown command "frobnicate"`,
		},
		{
			name: "version prints the build stamp",
			args: []string{"version"}, wantCode: 0, wantOut: "platformctl",
		},
		{
			name: "migrate with no sub-command exits 2",
			args: []string{"migrate"}, wantCode: 2, wantErr: "up, down or status",
		},
		{
			name: "an unknown migrate sub-command exits 2",
			args: []string{"migrate", "sideways"}, wantCode: 2, wantErr: "unknown migrate sub-command",
		},
		{
			name: "migrate down without --to exits 2",
			args: []string{"migrate", "down"}, wantCode: 2, wantErr: "--to VERSION",
		},
		{
			name:     "migrate down without --confirm exits 2 and explains why",
			args:     []string{"migrate", "down", "--to", "3"},
			wantCode: 2, wantErr: "forward-only",
		},
		{
			name: "config with no sub-command exits 2",
			args: []string{"config"}, wantCode: 2, wantErr: "validate FILE",
		},
		{
			name: "config validate with no file exits 2",
			args: []string{"config", "validate"}, wantCode: 2, wantErr: "exactly one file path",
		},
		{
			name:     "config validate on a missing file exits 2",
			args:     []string{"config", "validate", "/nonexistent/config.yaml"},
			wantCode: 2, wantErr: "cannot read",
		},
		{
			name: "certify without a merchant id exits 2",
			args: []string{"certify"}, wantCode: 2, wantErr: "exactly one merchant id",
		},
		{
			name: "certify with a malformed merchant id exits 2",
			args: []string{"certify", "not-a-ulid"}, wantCode: 2, wantErr: "identifier",
		},
		{
			name: "workflow with no sub-command exits 2",
			args: []string{"workflow"}, wantCode: 2, wantErr: "list, resume or dlq",
		},
		{
			name: "verify-audit-chain without a tenant exits 2",
			args: []string{"verify-audit-chain"}, wantCode: 2, wantErr: "exactly one tenant id",
		},
		{
			name: "outbox with no sub-command exits 2",
			args: []string{"outbox"}, wantCode: 2, wantErr: "status",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errOut := capture(t, tc.args)
			if code != tc.wantCode {
				t.Errorf("exit code = %d, want %d\nstdout: %s\nstderr: %s",
					code, tc.wantCode, out, errOut)
			}
			if tc.wantOut != "" && !strings.Contains(out, tc.wantOut) {
				t.Errorf("stdout does not contain %q:\n%s", tc.wantOut, out)
			}
			if tc.wantErr != "" && !strings.Contains(errOut, tc.wantErr) {
				t.Errorf("stderr does not contain %q:\n%s", tc.wantErr, errOut)
			}
		})
	}
}

// TestSeedRefusesProduction is the control that keeps synthetic data out of tables that hold money.
//
// There is deliberately no override flag, so the only thing to assert is that the refusal happens
// before anything is written — which it does, because the check precedes the connection.
func TestSeedRefusesProduction(t *testing.T) {
	for _, env := range []string{"prod", "production"} {
		t.Run(env, func(t *testing.T) {
			t.Setenv("PP_ENVIRONMENT", env)
			t.Setenv("PP_DSN", "postgres://unreachable:5432/x")
			code, _, errOut := capture(t, []string{"seed"})
			if code != 2 {
				t.Fatalf("exit code = %d, want 2", code)
			}
			if !strings.Contains(errOut, "refusing to seed production") {
				t.Errorf("stderr does not carry the refusal:\n%s", errOut)
			}
			if !strings.Contains(errOut, "no override") {
				t.Error("the refusal does not state that there is no override")
			}
		})
	}
}

// TestSeedRejectsAnUnknownProfile asserts the profile set is closed, so a typo is a message rather
// than a dataset nobody recognises.
func TestSeedRejectsAnUnknownProfile(t *testing.T) {
	t.Setenv("PP_ENVIRONMENT", "sandbox")
	code, _, errOut := capture(t, []string{"seed", "--profile", "enormous"})
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut, "unknown profile") {
		t.Errorf("stderr does not name the bad profile:\n%s", errOut)
	}
}

// TestCommandsRequiringADatabaseSaySoWhenItIsAbsent asserts the missing-DSN message names both
// accepted variables, so an operator with DATABASE_URL exported does not have to read the source.
func TestCommandsRequiringADatabaseSaySoWhenItIsAbsent(t *testing.T) {
	for _, args := range [][]string{
		{"migrate", "status"},
		{"outbox", "status"},
		{"workflow", "list"},
		{"verify-audit-chain", "ten_01JB8Z00000000000000000000"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Setenv("PP_DSN", "")
			t.Setenv("DATABASE_URL", "")
			code, _, errOut := capture(t, args)
			if code != 2 {
				t.Errorf("exit code = %d, want 2", code)
			}
			if !strings.Contains(errOut, "PP_DSN") || !strings.Contains(errOut, "DATABASE_URL") {
				t.Errorf("the message does not name both accepted variables:\n%s", errOut)
			}
		})
	}
}

// TestUsageListsEveryCommand asserts the help text and the dispatcher agree.
//
// They drift the moment somebody adds a command and forgets the help, and the symptom is a
// capability nobody knows exists.
func TestUsageListsEveryCommand(t *testing.T) {
	_, out, _ := capture(t, []string{"help"})
	for _, cmd := range []string{
		"migrate up", "migrate down", "migrate status", "seed", "config validate",
		"certify", "dr-drill", "outbox status", "workflow list", "workflow resume",
		"workflow dlq", "verify-audit-chain", "version",
	} {
		if !strings.Contains(out, cmd) {
			t.Errorf("usage does not document %q", cmd)
		}
	}
}

// capture runs the CLI with its streams redirected to pipes.
//
// run takes *os.File rather than io.Writer specifically so this is possible without a global: the
// alternative — writing to os.Stdout and capturing it — makes the tests order-dependent and
// unparallelisable.
func capture(t *testing.T, args []string) (int, string, string) {
	t.Helper()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}

	code := run(args, outW, errW)
	_ = outW.Close()
	_ = errW.Close()

	outB, _ := io.ReadAll(outR)
	errB, _ := io.ReadAll(errR)
	return code, string(outB), string(errB)
}
