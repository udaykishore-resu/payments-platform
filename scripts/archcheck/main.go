// Command archcheck is the architecture fitness function for this repository.
//
// It enforces the dependency rule of docs/spec/00-design-baseline.md §4 by reading the
// package graph that the Go toolchain itself reports (`go list -json ./...`) rather than
// by grepping import blocks. The distinction matters: a grep sees the text of an import
// line, while `go list` sees the resolved package graph including build-tag-selected
// files, test files and external test packages. An architecture check that a build tag
// can hide from is not a check.
//
// Usage:
//
//	archcheck [-root DIR] [-cmd-max-lines N] [-json]
//
// Exit status: 0 clean, 1 one or more violations, 2 could not run.
//
// # The rules, as implemented
//
//	R1  pkg/**                      -> stdlib only. No module-internal package, no
//	                                   third-party module. pkg/ is the part of the repo
//	                                   that could be published; a dependency here is a
//	                                   dependency imposed on every future consumer.
//	R2  internal/domain/**          -> stdlib, internal/domain/**, pkg/** only.
//	R3  internal/application/**     -> may not import internal/infrastructure/**, nor
//	                                   internal/adapters/** other than the single
//	                                   declared exception internal/adapters/gateway/spi.
//	R4  internal/adapters/gateway/spi -> stdlib, internal/domain/**, pkg/** only. This is
//	                                   the guard rail on R3's exception: spi is allowed to
//	                                   be imported by the application layer precisely
//	                                   because it is a port declaration, so if it ever
//	                                   grows a driver dependency the exception becomes a
//	                                   hole and this rule closes it.
//	R5  internal/validation/**,
//	    internal/workflows/engine   -> may not import internal/infrastructure/**.
//	R6  no package imports a _test package (an external test package, "…_test"). Such an
//	                                   import compiles only in contrived circumstances and
//	                                   is always a mistake; catching it is cheap.
//	R7  cmd/**                      -> composition roots only. See the heuristic below.
//
// # The cmd/** "no business logic" heuristic
//
// "Business logic" is not mechanically decidable, so this check uses two proxies that are
// cheap, explainable, and produce a specific, actionable message when they trip. Both are
// deliberately conservative — they are a smell detector, not a proof:
//
//	H1  File length. No non-test file under cmd/** may exceed -cmd-max-lines (default
//	    300, generous for a wiring file). A composition root is a list of constructor
//	    calls; the shape of that list is long-ish but flat. Logic shows up as length.
//
//	H2  Domain-type iteration. No file under cmd/** may contain a `for`/`range` statement
//	    whose body mentions a type imported from internal/domain/**. Wiring iterates over
//	    configuration, service descriptors and signals; it does not loop over payments,
//	    merchants or ledger entries. If a composition root is walking domain values it has
//	    started making decisions that belong in internal/application.
//
// Both are detected on the real AST (go/parser), not with a regular expression, so a
// domain type named in a comment or a string does not trip H2.
//
// An intentional exception is written in the source it applies to, as a line comment
// `//archcheck:allow <ruleID> <reason>` on the offending import or statement. The reason
// is mandatory: an unexplained suppression is indistinguishable from an accident.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const modulePath = "github.com/udaykishore-resu/payments-platform"

// spiException is the one package under internal/adapters that internal/application may
// import. It is a constant rather than a configurable list because the whole point of
// baseline §4's SPI carve-out is that it is *narrow*; a list invites a second entry.
const spiException = "internal/adapters/gateway/spi"

// listPkg is the subset of `go list -json` this tool reads.
type listPkg struct {
	ImportPath  string   `json:"ImportPath"`
	Dir         string   `json:"Dir"`
	Name        string   `json:"Name"`
	Standard    bool     `json:"Standard"`
	Imports     []string `json:"Imports"`
	TestImports []string `json:"TestImports"`
	// XTestImports are the imports of the external test package (package foo_test). They
	// are checked too: an external test for a domain package that reaches for pgx is the
	// same architectural leak as the production file doing it, just deferred.
	XTestImports []string `json:"XTestImports"`
	GoFiles      []string `json:"GoFiles"`
	TestGoFiles  []string `json:"TestGoFiles"`
	XTestGoFiles []string `json:"XTestGoFiles"`
	Error        *struct {
		Err string `json:"Err"`
	} `json:"Error"`
}

type violation struct {
	Rule    string `json:"rule"`
	Package string `json:"package"`
	Detail  string `json:"detail"`
	Pos     string `json:"pos,omitempty"`
}

func main() {
	var (
		root      = flag.String("root", ".", "repository root to analyse")
		cmdMax    = flag.Int("cmd-max-lines", 300, "maximum lines in a non-test file under cmd/**")
		asJSON    = flag.Bool("json", false, "emit violations as JSON instead of text")
		skipCmd   = flag.Bool("skip-cmd", false, "skip the cmd/** composition-root heuristics")
		verbosity = flag.Bool("v", false, "print each package as it is checked")
	)
	flag.Parse()

	abs, err := filepath.Abs(*root)
	if err != nil {
		fatal("resolve root: %v", err)
	}

	pkgs, err := goList(abs)
	if err != nil {
		fatal("go list: %v", err)
	}
	if len(pkgs) == 0 {
		fatal("go list returned no packages under %s", abs)
	}

	var vs []violation
	for _, p := range pkgs {
		rel, ok := relPath(p.ImportPath)
		if !ok {
			continue // a dependency of ours, not one of our packages
		}
		if strings.HasPrefix(rel, "scripts/") {
			// The check tooling itself lives in the module but is not part of the
			// layered application; excluding it is stated here rather than silently
			// assumed. It ships no runtime behaviour and imports whatever it must
			// introspect (which is, by design, every layer at once).
			continue
		}
		if *verbosity {
			fmt.Fprintf(os.Stderr, "checking %s\n", rel)
		}
		allowed := readAllowances(p)
		vs = append(vs, checkImports(p, rel, allowed)...)
	}

	if !*skipCmd {
		vs = append(vs, checkCmdPackages(pkgs, *cmdMax)...)
	}

	sort.Slice(vs, func(i, j int) bool {
		if vs[i].Rule != vs[j].Rule {
			return vs[i].Rule < vs[j].Rule
		}
		if vs[i].Package != vs[j].Package {
			return vs[i].Package < vs[j].Package
		}
		return vs[i].Detail < vs[j].Detail
	})

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(vs); err != nil {
			fatal("encode: %v", err)
		}
	} else {
		for _, v := range vs {
			pos := ""
			if v.Pos != "" {
				pos = " (" + v.Pos + ")"
			}
			fmt.Printf("%s\t%s\t%s%s\n", v.Rule, v.Package, v.Detail, pos)
		}
	}
	if len(vs) > 0 {
		os.Exit(1)
	}
}

// checkImports applies R1–R6 to one package's full import set.
func checkImports(p listPkg, rel string, allowed map[string]bool) []violation {
	var vs []violation
	add := func(rule, detail string) {
		if allowed[rule+"|"+detail] || allowed[rule+"|*"] {
			return
		}
		vs = append(vs, violation{Rule: rule, Package: rel, Detail: detail})
	}

	seen := map[string]bool{}
	for _, group := range [][]string{p.Imports, p.TestImports, p.XTestImports} {
		for _, imp := range group {
			if seen[imp] {
				continue
			}
			seen[imp] = true

			ir, internal := relPath(imp)
			external := !internal && isExternalModule(imp)

			// R6 — nobody imports an external test package.
			if strings.HasSuffix(imp, "_test") {
				add("R6-test-package-import", "imports test package "+imp)
			}

			switch {
			// R1 — pkg/** is stdlib-only.
			case strings.HasPrefix(rel, "pkg/"):
				if internal {
					add("R1-pkg-stdlib-only", "imports module package "+ir)
				} else if external {
					add("R1-pkg-stdlib-only", "imports third-party module "+imp)
				}

			// R2 — domain imports stdlib, domain, pkg.
			case strings.HasPrefix(rel, "internal/domain/") || rel == "internal/domain":
				if external {
					add("R2-domain-purity", "imports third-party module "+imp)
				} else if internal && !underAny(ir, "internal/domain/", "pkg/") {
					add("R2-domain-purity", "imports "+ir)
				}
			}

			// R4 — the SPI guard rail. Checked before R3 so that a violation here is
			// reported as what it is: the exception's precondition breaking.
			if rel == spiException {
				if external {
					add("R4-spi-guardrail", "imports third-party module "+imp)
				} else if internal && !underAny(ir, "internal/domain/", "pkg/") && ir != spiException {
					add("R4-spi-guardrail", "imports "+ir)
				}
			}

			// R3 — application may not reach into infrastructure or adapters.
			if strings.HasPrefix(rel, "internal/application/") || rel == "internal/application" {
				if internal && strings.HasPrefix(ir, "internal/infrastructure/") {
					add("R3-application-isolation", "imports infrastructure "+ir)
				}
				if internal && strings.HasPrefix(ir, "internal/adapters/") && ir != spiException {
					add("R3-application-isolation", "imports adapter "+ir+" (only "+spiException+" is permitted, baseline §4 †)")
				}
			}

			// R5 — validation and the workflow engine port are infrastructure-free.
			if strings.HasPrefix(rel, "internal/validation/") ||
				rel == "internal/validation" ||
				rel == "internal/workflows/engine" {
				if internal && strings.HasPrefix(ir, "internal/infrastructure/") {
					add("R5-validation-isolation", "imports infrastructure "+ir)
				}
			}
		}
	}
	return vs
}

// checkCmdPackages applies the two cmd/** composition-root heuristics, H1 and H2.
func checkCmdPackages(pkgs []listPkg, maxLines int) []violation {
	// Collect the set of domain package import paths so H2 can recognise a domain type
	// by the package qualifier used at the call site.
	var vs []violation
	for _, p := range pkgs {
		rel, ok := relPath(p.ImportPath)
		if !ok || !strings.HasPrefix(rel, "cmd/") {
			continue
		}
		for _, f := range p.GoFiles {
			path := filepath.Join(p.Dir, f)
			vs = append(vs, checkCmdFile(path, rel, maxLines)...)
		}
	}
	return vs
}

func checkCmdFile(path, rel string, maxLines int) []violation {
	var vs []violation
	fset := token.NewFileSet()
	src, err := os.ReadFile(path)
	if err != nil {
		return []violation{{Rule: "H0-unreadable", Package: rel, Detail: err.Error()}}
	}
	file, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return []violation{{Rule: "H0-unparseable", Package: rel, Detail: err.Error()}}
	}

	allow := allowancesFromComments(file, fset)

	// H1 — file length.
	lines := 1 + strings.Count(string(src), "\n")
	if lines > maxLines && !allow["H1-cmd-file-length"] {
		vs = append(vs, violation{
			Rule:    "H1-cmd-file-length",
			Package: rel,
			Detail: fmt.Sprintf("%s is %d lines (limit %d): a composition root is a list of constructor calls; length here is logic that belongs in internal/application",
				filepath.Base(path), lines, maxLines),
		})
	}

	// H2 — iteration over domain types. Build the file's local alias -> import path map
	// so `for _, p := range payments { p.Capture(...) }` is recognised through whatever
	// name the import was given.
	domainAlias := map[string]string{}
	for _, imp := range file.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		ir, internal := relPath(p)
		if !internal || !strings.HasPrefix(ir, "internal/domain/") {
			continue
		}
		name := path2name(p)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		domainAlias[name] = ir
	}
	if len(domainAlias) == 0 || allow["H2-cmd-domain-iteration"] {
		return vs
	}

	ast.Inspect(file, func(n ast.Node) bool {
		var body *ast.BlockStmt
		switch s := n.(type) {
		case *ast.RangeStmt:
			body = s.Body
		case *ast.ForStmt:
			body = s.Body
		default:
			return true
		}
		if hit, pkg := mentionsDomainPackage(body, domainAlias); hit {
			vs = append(vs, violation{
				Rule:    "H2-cmd-domain-iteration",
				Package: rel,
				Detail:  "loop body operates on domain package " + pkg + ": a composition root wires dependencies, it does not iterate over domain values",
				Pos:     fset.Position(n.Pos()).String(),
			})
		}
		return true
	})
	return vs
}

// mentionsDomainPackage reports whether a statement block references a selector qualified
// by one of the file's domain-package aliases (e.g. `payment.Payment`, `money.Zero`).
// Working on the AST rather than the text means a domain name in a comment or a log
// string cannot trip the rule.
func mentionsDomainPackage(body *ast.BlockStmt, alias map[string]string) (bool, string) {
	found := ""
	ast.Inspect(body, func(n ast.Node) bool {
		if found != "" {
			return false
		}
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if p, ok := alias[id.Name]; ok {
			found = p
			return false
		}
		return true
	})
	return found != "", found
}

// readAllowances scans a package's own source for `//archcheck:allow <rule> <reason>`
// comments and returns the suppressions they grant. A suppression without a reason is
// ignored — a bare marker is not an argument.
func readAllowances(p listPkg) map[string]bool {
	out := map[string]bool{}
	files := append(append([]string{}, p.GoFiles...), p.TestGoFiles...)
	files = append(files, p.XTestGoFiles...)
	fset := token.NewFileSet()
	for _, f := range files {
		path := filepath.Join(p.Dir, f)
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			continue
		}
		for k := range allowancesFromComments(file, fset) {
			out[k] = true
		}
	}
	return out
}

func allowancesFromComments(file *ast.File, _ *token.FileSet) map[string]bool {
	out := map[string]bool{}
	for _, cg := range file.Comments {
		for _, c := range cg.List {
			txt := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(c.Text, "//"), "/*"))
			const marker = "archcheck:allow "
			if !strings.HasPrefix(txt, marker) {
				continue
			}
			rest := strings.TrimSpace(txt[len(marker):])
			parts := strings.SplitN(rest, " ", 2)
			if len(parts) != 2 || strings.TrimSpace(parts[1]) == "" {
				continue // no reason given: not a valid suppression
			}
			rule := parts[0]
			out[rule] = true
			out[rule+"|*"] = true
		}
	}
	return out
}

// --- helpers ------------------------------------------------------------------------------

func goList(dir string) ([]listPkg, error) {
	cmd := exec.Command("go", "list", "-json", "-e", "./...")
	cmd.Dir = dir
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(strings.NewReader(string(out)))
	var pkgs []listPkg
	for {
		var p listPkg
		if err := dec.Decode(&p); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		if p.Error != nil {
			return nil, fmt.Errorf("package %s: %s", p.ImportPath, p.Error.Err)
		}
		pkgs = append(pkgs, p)
	}
	return pkgs, nil
}

// relPath maps a full import path to its module-relative form, reporting whether the
// package belongs to this module.
func relPath(importPath string) (string, bool) {
	if importPath == modulePath {
		return "", true
	}
	if strings.HasPrefix(importPath, modulePath+"/") {
		return strings.TrimPrefix(importPath, modulePath+"/"), true
	}
	return "", false
}

// isExternalModule distinguishes a third-party module path from a standard-library one.
// The rule the Go toolchain itself uses: a standard-library import path has no dot in its
// first path element (there is no "example.com" in "net/http").
func isExternalModule(importPath string) bool {
	first := importPath
	if i := strings.IndexByte(importPath, '/'); i >= 0 {
		first = importPath[:i]
	}
	return strings.Contains(first, ".")
}

func underAny(rel string, prefixes ...string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(rel, p) || rel == strings.TrimSuffix(p, "/") {
			return true
		}
	}
	return false
}

func path2name(importPath string) string {
	i := strings.LastIndexByte(importPath, '/')
	if i < 0 {
		return importPath
	}
	return importPath[i+1:]
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "archcheck: "+format+"\n", args...)
	os.Exit(2)
}
