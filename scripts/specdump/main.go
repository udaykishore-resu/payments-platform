// Command specdump exports the contents of this platform's in-code registries as JSON so
// that shell/Python checks can diff them against the declarative artefacts they are
// supposed to mirror (api/errors/catalog.yaml, api/events/*.schema.json,
// docs/validation-plane.md).
//
// Why a Go program rather than a grep: every one of these registries is a Go map or a
// package-level `init()` side effect. A grep over the source sees the literals someone
// remembered to write in a way a regular expression recognises; linking the real packages
// and printing the real registries sees what the binary will actually do. For the
// validation-rule registry the distinction is load-bearing — the registry is populated by
// blank imports in internal/validation/rules, so anything less than linking it produces a
// check on "the rules I happened to grep for".
//
// Usage:
//
//	specdump errors    # {"CODE": {"category":…, "retryable":…, "message":…}}
//	specdump rules     # [{"id":…, "severity":…, "code":…, "pure":…, "status":…}]
//	specdump events    # [{"type":…, "topic":…, "schemaFile":…, "partitionKey":…}]
//	specdump metrics [-root DIR]
//	                   # {"declared":[names from the registry constants],
//	                   #  "declarations":[{"metric":…, "labels":[…], "pos":…}]}
//
// Exit status: 0 on success, 2 on an internal error. specdump makes no judgements — it
// prints facts and the calling check decides what is a violation. Keeping the policy in
// the check script and the extraction here means a rule can be tightened without
// recompiling anything.
package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/udaykishore-resu/payments-platform/internal/events"
	"github.com/udaykishore-resu/payments-platform/internal/infrastructure/telemetry"
	"github.com/udaykishore-resu/payments-platform/internal/validation/engine"
	"github.com/udaykishore-resu/payments-platform/internal/validation/rules"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

func main() {
	if len(os.Args) < 2 {
		fatal("usage: specdump {errors|rules|events|metrics} [flags]")
	}
	switch os.Args[1] {
	case "errors":
		emit(dumpErrors())
	case "rules":
		emit(dumpRules())
	case "events":
		emit(dumpEvents())
	case "metrics":
		root := "."
		if len(os.Args) > 3 && os.Args[2] == "-root" {
			root = os.Args[3]
		}
		emit(dumpMetrics(root))
	default:
		fatal("unknown subcommand %q", os.Args[1])
	}
}

// --- errors ---------------------------------------------------------------------------

type errorEntry struct {
	Category  string `json:"category"`
	Retryable bool   `json:"retryable"`
	Message   string `json:"message"`
	// HTTPStatus and GRPCCode are included because api/errors/catalog.yaml may state them
	// explicitly, and a catalog that says 409 while the code maps to 422 is exactly the
	// kind of silent divergence the consistency check exists to catch.
	HTTPStatus int `json:"httpStatus"`
	GRPCCode   int `json:"grpcCode"`
}

func dumpErrors() map[string]errorEntry {
	out := map[string]errorEntry{}
	for _, c := range apierror.AllCodes() {
		cat, retryable, msg, ok := apierror.Spec(c)
		if !ok {
			continue
		}
		e := apierror.New(c, "")
		out[string(c)] = errorEntry{
			Category:   string(cat),
			Retryable:  retryable,
			Message:    msg,
			HTTPStatus: e.HTTPStatus(),
			GRPCCode:   e.GRPCCode(),
		}
	}
	return out
}

// --- validation rules -----------------------------------------------------------------

type ruleEntry struct {
	ID          string `json:"id"`
	Level       int    `json:"level"`
	Severity    string `json:"severity"`
	Code        string `json:"code"`
	Description string `json:"description"`
	Remediation string `json:"remediation"`
	Pure        bool   `json:"pure"`
	Stage       string `json:"stage"`
	Owner       string `json:"owner"`
	Since       string `json:"since"`
	Status      string `json:"status"`
}

func dumpRules() []ruleEntry {
	all := rules.Catalog()
	out := make([]ruleEntry, 0, len(all))
	for _, r := range all {
		out = append(out, ruleEntry{
			ID:    string(r.ID),
			Level: levelOf(r.ID),
			// Severity is a uint8 enum with a String method; fmt.Sprint uses the Stringer
			// rather than reinterpreting the byte as a rune, which a string() conversion
			// would silently do (producing "" for Error).
			Severity:    r.Severity.String(),
			Code:        r.Code,
			Description: r.Description,
			Remediation: r.Remediation,
			Pure:        r.Pure,
			Stage:       fmt.Sprint(r.Stage),
			Owner:       r.Owner,
			Since:       r.Since,
			Status:      string(r.Status),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// levelOf extracts the numeric level from an ID of the form "L<n>.<NAME>". A rule ID that
// does not parse gets level 0, which the check script surfaces as a malformed ID rather
// than silently bucketing it.
func levelOf(id engine.RuleID) int {
	s := string(id)
	if len(s) < 3 || s[0] != 'L' {
		return 0
	}
	dot := strings.IndexByte(s, '.')
	if dot < 2 {
		return 0
	}
	n, err := strconv.Atoi(s[1:dot])
	if err != nil {
		return 0
	}
	return n
}

// --- events ---------------------------------------------------------------------------

type eventEntry struct {
	Type         string `json:"type"`
	Topic        string `json:"topic"`
	SchemaFile   string `json:"schemaFile"`
	SchemaURI    string `json:"schemaUri"`
	PartitionKey string `json:"partitionKey"`
	Aggregate    string `json:"aggregate"`
}

func dumpEvents() []eventEntry {
	all := events.AllRegistered()
	out := make([]eventEntry, 0, len(all))
	for _, r := range all {
		out = append(out, eventEntry{
			Type:         r.Type,
			Topic:        r.Topic,
			SchemaFile:   r.SchemaFile,
			SchemaURI:    r.SchemaURI,
			PartitionKey: r.PartitionKeyField,
			Aggregate:    r.Aggregate,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out
}

// --- metrics ---------------------------------------------------------------------------

type metricDecl struct {
	Metric string   `json:"metric"`
	Labels []string `json:"labels"`
	Pos    string   `json:"pos"`
}

type metricsDump struct {
	// Declared is the set of metric-name constants exported by the telemetry package —
	// the names the platform claims to expose.
	Declared []string `json:"declared"`
	// Forbidden mirrors the telemetry package's own forbidden-label map so the lint and
	// the runtime guard can never disagree about the list.
	Forbidden []string `json:"forbidden"`
	// Declarations are every `pp_*` metric declaration site found by scanning the tree.
	Declarations []metricDecl `json:"declarations"`
	// MaxSeriesPerMetric is the baseline §22.3 budget, echoed for the report header.
	MaxSeriesPerMetric int `json:"maxSeriesPerMetric"`
}

// forbiddenLabels is the lint's copy of the rule. It is asserted against the telemetry
// package's runtime behaviour below rather than being maintained twice by hand: any label
// the runtime rejects must appear here, and the check script fails if the two lists
// diverge. Baseline §22.3 names merchant_id, payment_id, attempt_id, idempotency_key,
// email and ip explicitly; the telemetry package adds several of the same kind.
var forbiddenLabels = []string{
	"merchant_id", "payment_id", "attempt_id", "idempotency_key", "email", "ip",
	"user_agent", "url", "path", "error_message",
	// Near-synonyms a well-meaning author reaches for when the obvious name is rejected.
	// Catching the synonym is the difference between a rule and a speed bump.
	"merchantId", "paymentId", "attemptId", "idempotencyKey",
	"customer_id", "card_number", "pan", "user_id", "session_id", "request_id",
	"trace_id", "span_id", "correlation_id", "client_ip", "remote_addr", "email_address",
}

func dumpMetrics(root string) metricsDump {
	d := metricsDump{
		Declared:           declaredMetricNames(),
		Forbidden:          runtimeForbiddenLabels(),
		MaxSeriesPerMetric: telemetry.DefaultMaxSeriesPerMetric,
	}
	d.Declarations = scanMetricDeclarations(root)
	sort.Strings(d.Declared)
	sort.Strings(d.Forbidden)
	sort.Slice(d.Declarations, func(i, j int) bool {
		if d.Declarations[i].Metric != d.Declarations[j].Metric {
			return d.Declarations[i].Metric < d.Declarations[j].Metric
		}
		return d.Declarations[i].Pos < d.Declarations[j].Pos
	})
	return d
}

// declaredMetricNames is the §22.2 contract plus the package's three self-observability
// metrics. Listed explicitly rather than reflected out of the package because the point of
// the "undeclared metric" gate is to require a human edit here when the metric surface
// changes — that edit is the review hook.
func declaredMetricNames() []string {
	return []string{
		telemetry.MetricHTTPRequestsTotal,
		telemetry.MetricHTTPRequestDuration,
		telemetry.MetricPaymentsTotal,
		telemetry.MetricPaymentAuthorizationRate,
		telemetry.MetricGatewayRequestDuration,
		telemetry.MetricGatewayErrorsTotal,
		telemetry.MetricCircuitBreakerState,
		telemetry.MetricIdempotencyOutcomesTotal,
		telemetry.MetricRoutingDecisionsTotal,
		telemetry.MetricWorkflowStepDuration,
		telemetry.MetricWorkflowInstances,
		telemetry.MetricOnboardingDuration,
		telemetry.MetricOutboxBacklog,
		telemetry.MetricConsumerLag,
		telemetry.MetricConfigSnapshotAge,
		telemetry.MetricReconciliationExceptions,
		telemetry.MetricDLQDepth,
		telemetry.MetricLogFieldRejectedTotal,
		telemetry.MetricLogLinesSuppressedTotal,
		telemetry.MetricSeriesOverflowTotal,
	}
}

// runtimeForbiddenLabels probes telemetry.ValidateLabels with each candidate so the list
// this lint enforces is the list the running code enforces. A label that the lint rejects
// but the runtime accepts is a lint nobody trusts; a label the runtime rejects but the
// lint accepts is a production startup failure discovered after merge.
func runtimeForbiddenLabels() []string {
	var out []string
	for _, l := range forbiddenLabels {
		if telemetry.ValidateLabels("pp_probe_total", []string{l}) != nil {
			out = append(out, l)
			continue
		}
		// Not rejected at runtime, but still forbidden by this lint. Emitted with a
		// marker so the check script can report the asymmetry rather than hiding it.
		out = append(out, l+" (lint-only)")
	}
	return out
}

// scanMetricDeclarations finds every call site that names a `pp_*` metric and passes a
// []string label list, whether that is a direct prometheus.NewCounterVec or one of the
// local counter/gauge/histogram helpers in the telemetry registry constructor. Matching on
// shape (a pp_ name + a string-slice literal in the same call) rather than on a specific
// prometheus API means a refactor of the helper layer does not blind the lint.
func scanMetricDeclarations(root string) []metricDecl {
	consts := collectMetricConsts(root)
	var out []metricDecl

	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			// scripts/ is excluded because the check tooling itself names metrics as
			// probe arguments (telemetry.ValidateLabels("pp_probe_total", …)). Scanning
			// it would report the lint's own scaffolding as an undeclared metric.
			switch d.Name() {
			case ".git", "vendor", "node_modules", "testdata", "scripts":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := ""
			var labels []string
			gotLabels := false
			for _, arg := range call.Args {
				if s, ok := metricNameOf(arg, consts); ok && name == "" {
					name = s
				}
				if ls, ok := stringSliceLit(arg); ok && !gotLabels {
					labels, gotLabels = ls, true
				}
				// prometheus.CounterOpts{Name: …} and friends carry the name in a field.
				if cl, ok := arg.(*ast.CompositeLit); ok {
					for _, elt := range cl.Elts {
						kv, ok := elt.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						k, ok := kv.Key.(*ast.Ident)
						if !ok || k.Name != "Name" {
							continue
						}
						if s, ok := metricNameOf(kv.Value, consts); ok && name == "" {
							name = s
						}
					}
				}
			}
			if name != "" && gotLabels {
				out = append(out, metricDecl{
					Metric: name,
					Labels: labels,
					Pos:    fset.Position(call.Pos()).String(),
				})
			}
			return true
		})
		return nil
	})
	return out
}

// collectMetricConsts maps every package-level identifier bound to a "pp_"-prefixed string
// literal to that literal, so a declaration written as counter(MetricPaymentsTotal, …) is
// resolved to its real metric name.
func collectMetricConsts(root string) map[string]string {
	out := map[string]string{}
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules", "testdata", "scripts":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil
		}
		for _, decl := range file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || (gd.Tok != token.CONST && gd.Tok != token.VAR) {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, nm := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					bl, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || bl.Kind != token.STRING {
						continue
					}
					s, err := strconv.Unquote(bl.Value)
					if err != nil || !strings.HasPrefix(s, "pp_") {
						continue
					}
					out[nm.Name] = s
				}
			}
		}
		return nil
	})
	return out
}

func metricNameOf(e ast.Expr, consts map[string]string) (string, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(v.Value)
		if err != nil || !strings.HasPrefix(s, "pp_") {
			return "", false
		}
		return s, true
	case *ast.Ident:
		if s, ok := consts[v.Name]; ok {
			return s, true
		}
	case *ast.SelectorExpr:
		if s, ok := consts[v.Sel.Name]; ok {
			return s, true
		}
	}
	return "", false
}

func stringSliceLit(e ast.Expr) ([]string, bool) {
	cl, ok := e.(*ast.CompositeLit)
	if !ok {
		return nil, false
	}
	at, ok := cl.Type.(*ast.ArrayType)
	if !ok {
		return nil, false
	}
	id, ok := at.Elt.(*ast.Ident)
	if !ok || id.Name != "string" {
		return nil, false
	}
	out := make([]string, 0, len(cl.Elts))
	for _, elt := range cl.Elts {
		bl, ok := elt.(*ast.BasicLit)
		if !ok || bl.Kind != token.STRING {
			continue
		}
		s, err := strconv.Unquote(bl.Value)
		if err != nil {
			continue
		}
		out = append(out, s)
	}
	return out, true
}

// --- plumbing ---------------------------------------------------------------------------

func emit(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fatal("encode: %v", err)
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "specdump: "+format+"\n", args...)
	os.Exit(2)
}
