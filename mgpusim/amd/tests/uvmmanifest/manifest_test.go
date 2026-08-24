// Package uvmmanifest_test audits the checked-in UVM evaluation manifest
// against the actual benchmark source. It proves that every required buffer
// row has BOTH the enabled AllocateManagedMemory expression AND the exact
// disabled expression at its existing allocation seam, that every excluded
// row is truthfully excluded, that the 24 case contracts are complete and
// duplicate-free, and that the sbin_codex edit markers are present.
//
// sbin_codex
package uvmmanifest

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot walks up from the package directory to the mgpusim module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("mgpusim repo root not found")
		}
		dir = parent
	}
	return ""
}

// parsePackage parses every non-test Go file in a package directory.
func parsePackage(t *testing.T, dir string) (*token.FileSet, []*ast.File) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") ||
			strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		t.Fatalf("no Go files in %s", dir)
	}
	return fset, files
}

// callName returns the name of the function called by a CallExpr.
func callName(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		return fun.Sel.Name
	case *ast.Ident:
		return fun.Name
	}
	return ""
}

// callMatches reports whether a CallExpr is structurally equal to the recorded
// expression text. Structural comparison (exprEqual) makes the audit
// insensitive to source formatting while still requiring the exact call.
func callMatches(fset *token.FileSet, call *ast.CallExpr, exprSrc string) bool {
	if exprSrc == "" {
		return false
	}
	parsed, err := parser.ParseExpr(exprSrc)
	if err != nil {
		return false
	}
	return exprEqual(call, parsed)
}

// exprEqual compares two expression ASTs structurally, ignoring positions.
// It covers every node kind that appears in allocation call expressions.
func exprEqual(a, b ast.Expr) bool {
	switch x := a.(type) {
	case *ast.Ident:
		y, ok := b.(*ast.Ident)
		return ok && x.Name == y.Name
	case *ast.SelectorExpr:
		y, ok := b.(*ast.SelectorExpr)
		return ok && exprEqual(x.X, y.X) && x.Sel.Name == y.Sel.Name
	case *ast.CallExpr:
		y, ok := b.(*ast.CallExpr)
		if !ok || len(x.Args) != len(y.Args) || !exprEqual(x.Fun, y.Fun) {
			return false
		}
		for i := range x.Args {
			if !exprEqual(x.Args[i], y.Args[i]) {
				return false
			}
		}
		return true
	case *ast.BasicLit:
		y, ok := b.(*ast.BasicLit)
		return ok && x.Kind == y.Kind && x.Value == y.Value
	case *ast.BinaryExpr:
		y, ok := b.(*ast.BinaryExpr)
		return ok && x.Op == y.Op && exprEqual(x.X, y.X) && exprEqual(x.Y, y.Y)
	case *ast.ParenExpr:
		y, ok := b.(*ast.ParenExpr)
		return ok && exprEqual(x.X, y.X)
	case *ast.IndexExpr:
		y, ok := b.(*ast.IndexExpr)
		return ok && exprEqual(x.X, y.X) && exprEqual(x.Index, y.Index)
	case *ast.UnaryExpr:
		y, ok := b.(*ast.UnaryExpr)
		return ok && x.Op == y.Op && exprEqual(x.X, y.X)
	case *ast.StarExpr:
		y, ok := b.(*ast.StarExpr)
		return ok && exprEqual(x.X, y.X)
	case *ast.ArrayType:
		y, ok := b.(*ast.ArrayType)
		return ok && exprEqual(x.Elt, y.Elt)
	}
	return false
}

// fieldName extracts the field name from an assignment LHS, unwrapping a
// slice-index LHS (e.g. b.dMasks[i] -> dMasks).
func fieldName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.SelectorExpr:
		return e.Sel.Name
	case *ast.IndexExpr:
		if sel, ok := e.X.(*ast.SelectorExpr); ok {
			return sel.Sel.Name
		}
	}
	return ""
}

// isMethodRow reports whether the owning symbol names a method (a FuncDecl
// exists with that name) rather than a struct field.
func isMethodRow(files []*ast.File, symbol string) bool {
	parts := strings.Split(symbol, ".")
	if len(parts) < 2 {
		return false
	}
	name := parts[len(parts)-1]
	for _, f := range files {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == name {
				return true
			}
		}
	}
	return false
}

// auditMethodRow checks that the named method body contains both the enabled
// AllocateManagedMemory call and the exact disabled call.
func auditMethodRow(fset *token.FileSet, files []*ast.File, row BufferRow) (enabled, disabled bool) {
	parts := strings.Split(row.OwningSymbol, ".")
	methodName := parts[1]
	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Name.Name != methodName || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if callName(call) == "AllocateManagedMemory" &&
					callMatches(fset, call, row.EnabledExpr) {
					enabled = true
				}
				if callMatches(fset, call, row.DisabledExpr) {
					disabled = true
				}
				return true
			})
		}
	}
	return enabled, disabled
}

// auditFieldRow checks that the named struct field is assigned both via the
// enabled AllocateManagedMemory call and via the exact disabled call.
func auditFieldRow(fset *token.FileSet, files []*ast.File, row BufferRow) (enabled, disabled bool) {
	parts := strings.Split(row.OwningSymbol, ".")
	field := parts[len(parts)-1]
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for i, lhs := range as.Lhs {
				if fieldName(lhs) != field || i >= len(as.Rhs) {
					continue
				}
				call, ok := as.Rhs[i].(*ast.CallExpr)
				if !ok {
					continue
				}
				if callName(call) == "AllocateManagedMemory" &&
					callMatches(fset, call, row.EnabledExpr) {
					enabled = true
				}
				if callMatches(fset, call, row.DisabledExpr) {
					disabled = true
				}
			}
			return true
		})
	}
	return enabled, disabled
}

// auditRow audits one buffer row against the current source.
func auditRow(t *testing.T, root string, row BufferRow) (enabled, disabled bool) {
	t.Helper()
	dir := filepath.Join(root, "amd", "benchmarks", filepath.FromSlash(row.Package))
	fset, files := parsePackage(t, dir)
	parts := strings.Split(row.OwningSymbol, ".")
	if len(parts) >= 3 || isMethodRow(files, row.OwningSymbol) {
		return auditMethodRow(fset, files, row)
	}
	return auditFieldRow(fset, files, row)
}

var expectedCoreCases = []string{
	"atax", "bfs", "bicg", "fastwalshtransform", "fft", "fir",
	"floydwarshall", "kmeans", "matrixmultiplication", "matrixtranspose",
	"nbody", "nw", "pagerank", "relu", "simpleconvolution", "spmv",
	"stencil2d", "vectoradd",
}

var expectedDNNCases = []string{
	"conv2d", "im2col", "lenet", "minerva", "vgg16", "xor",
}

// dnnBenchmarkPackages maps each DNN case to its benchmark package so the
// edit-marker audit can check the benchmark's ManagedMemoryCapable wiring.
var dnnBenchmarkPackages = map[string]string{
	"conv2d":  "dnn/layer_benchmarks/conv2d",
	"im2col":  "dnn/layer_benchmarks/im2col",
	"lenet":   "dnn/training_benchmarks/lenet",
	"minerva": "dnn/training_benchmarks/minerva",
	"vgg16":   "dnn/training_benchmarks/vgg16",
	"xor":     "dnn/training_benchmarks/xor",
}

// TestManifestCaseContracts validates the manifest structure: exactly 24
// unique cases covering the expected set, each with an existing sample path,
// a non-empty exact argument vector, a valid D/R/E profile, a valid oracle
// class, and non-empty buffer rows. Duplicate or missing case, argument,
// profile, oracle, buffer, or exclusion entries fail.
func TestManifestCaseContracts(t *testing.T) {
	root := repoRoot(t)
	expected := append(append([]string{}, expectedCoreCases...), expectedDNNCases...)

	seen := make(map[string]bool)
	for _, c := range Manifest {
		if seen[c.Case] {
			t.Errorf("duplicate case %q", c.Case)
		}
		seen[c.Case] = true

		if c.SamplePath == "" {
			t.Errorf("case %s: empty sample path", c.Case)
		} else if _, err := os.Stat(filepath.Join(root, c.SamplePath)); err != nil {
			t.Errorf("case %s: sample path %s does not exist", c.Case, c.SamplePath)
		}
		if len(c.Args) == 0 {
			t.Errorf("case %s: empty argument vector", c.Case)
		}
		for _, a := range c.Args {
			if !strings.HasPrefix(a, "-") {
				t.Errorf("case %s: malformed argument %q", c.Case, a)
			}
		}
		if c.Profile != ProfileD && c.Profile != ProfileR && c.Profile != ProfileE {
			t.Errorf("case %s: invalid profile %q", c.Case, c.Profile)
		}
		if c.OracleClass != OracleCPUCompare && c.OracleClass != OracleReceipt {
			t.Errorf("case %s: invalid oracle class %q", c.Case, c.OracleClass)
		}
		if len(c.Buffers) == 0 {
			t.Errorf("case %s: no buffer rows", c.Case)
		}

		bufSeen := make(map[string]bool)
		for _, b := range c.Buffers {
			key := b.Package + "/" + b.OwningSymbol
			if bufSeen[key] {
				t.Errorf("case %s: duplicate buffer %s", c.Case, key)
			}
			bufSeen[key] = true
			if b.Package == "" {
				t.Errorf("case %s buffer %s: empty package", c.Case, b.OwningSymbol)
			} else if _, err := os.Stat(filepath.Join(root, "amd", "benchmarks",
				filepath.FromSlash(b.Package))); err != nil {
				t.Errorf("case %s buffer %s: package %s does not exist",
					c.Case, b.OwningSymbol, b.Package)
			}
			if b.OwningSymbol == "" {
				t.Errorf("case %s: empty owning symbol", c.Case)
			}
			if b.Exclusion == "" {
				if b.EnabledExpr == "" {
					t.Errorf("case %s buffer %s: missing enabled expression",
						c.Case, b.OwningSymbol)
				}
				if b.DisabledExpr == "" {
					t.Errorf("case %s buffer %s: missing disabled expression",
						c.Case, b.OwningSymbol)
				}
			} else {
				if b.DisabledExpr == "" {
					t.Errorf("case %s buffer %s: excluded buffer missing disabled expression",
						c.Case, b.OwningSymbol)
				}
			}
		}
	}

	for _, name := range expected {
		if !seen[name] {
			t.Errorf("missing case %q", name)
		}
	}
	if len(Manifest) != len(expected) {
		t.Errorf("expected %d cases, got %d", len(expected), len(Manifest))
	}
}

// TestManifestComplete verifies the manifest covers the full 18 core + 6 DNN
// case set, that every case maps to an existing sample and package, and that
// every case carries at least one required (non-excluded) buffer.
func TestManifestComplete(t *testing.T) {
	root := repoRoot(t)
	byName := make(map[string]CaseRow)
	for _, c := range Manifest {
		byName[c.Case] = c
	}

	all := append(append([]string{}, expectedCoreCases...), expectedDNNCases...)
	for _, name := range all {
		c, ok := byName[name]
		if !ok {
			t.Errorf("missing case %q", name)
			continue
		}
		if _, err := os.Stat(filepath.Join(root, c.SamplePath)); err != nil {
			t.Errorf("case %s: sample %s missing", c.Case, c.SamplePath)
		}
		required := 0
		for _, b := range c.Buffers {
			if b.Exclusion == "" {
				required++
			}
		}
		if required == 0 {
			t.Errorf("case %s: no required (non-excluded) buffer", c.Case)
		}
	}
}

// TestManifestAllocationAST is the AST enabled/disabled audit. For every
// required buffer it proves the enabled AllocateManagedMemory expression and
// the exact disabled expression both exist at the recorded allocation seam in
// the current source. For every excluded buffer it proves the disabled
// expression is the current reality and no AllocateManagedMemory branch was
// introduced.
func TestManifestAllocationAST(t *testing.T) {
	root := repoRoot(t)
	for _, c := range Manifest {
		for _, b := range c.Buffers {
			enabled, disabled := auditRow(t, root, b)
			if b.Exclusion != "" {
				if enabled {
					t.Errorf("case %s buffer %s: excluded but has AllocateManagedMemory branch",
						c.Case, b.OwningSymbol)
				}
				if !disabled {
					t.Errorf("case %s buffer %s: excluded but disabled expression %q not found",
						c.Case, b.OwningSymbol, b.DisabledExpr)
				}
				continue
			}
			if !enabled {
				t.Errorf("case %s buffer %s: missing enabled AllocateManagedMemory expression %q",
					c.Case, b.OwningSymbol, b.EnabledExpr)
			}
			if !disabled {
				t.Errorf("case %s buffer %s: missing exact disabled expression %q",
					c.Case, b.OwningSymbol, b.DisabledExpr)
			}
		}
	}
}

// TestManifestExclusions verifies every exclusion category is recorded with a
// reason, that excluded benchmarks (bitonicsort, aes) are absent from the case
// list, and that every excluded buffer row carries a meaningful reason.
func TestManifestExclusions(t *testing.T) {
	categories := make(map[string]bool)
	for _, e := range Exclusions {
		if e.Target == "" {
			t.Errorf("exclusion with empty target")
		}
		if len(e.Reason) < 10 {
			t.Errorf("exclusion %q: reason too short", e.Target)
		}
		categories[e.Target] = true
	}
	for _, cat := range []string{
		"bitonicsort",
		"aes",
		"multi-GPU dataparallelism scratch",
		"GCN3-unreachable CDNA3 buffers",
		"infrastructure",
		"generated copies",
	} {
		if !categories[cat] {
			t.Errorf("missing exclusion category %q", cat)
		}
	}

	for _, c := range Manifest {
		if c.Case == "bitonicsort" || c.Case == "aes" {
			t.Errorf("excluded benchmark %q present in case list", c.Case)
		}
		for _, b := range c.Buffers {
			if b.Exclusion != "" && len(b.Exclusion) < 10 {
				t.Errorf("case %s buffer %s: exclusion reason too short",
					c.Case, b.OwningSymbol)
			}
		}
	}
}

// dirContainsMarker reports whether any file under dir contains the marker.
func dirContainsMarker(dir, marker string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		if bytes.Contains(data, []byte(marker)) {
			return true
		}
	}
	return false
}

// fileContains reports whether the file at path contains the marker.
func fileContains(path, marker string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return bytes.Contains(data, []byte(marker))
}

// TestManifestEditMarkers verifies the sbin_codex edit markers are present in
// every converted package, that the DNN benchmarks wire SetManagedMemory into
// their GPU operators, and that matrixtranspose proves reachability with its
// managed-memory markers and branch.
func TestManifestEditMarkers(t *testing.T) {
	root := repoRoot(t)

	// Every required buffer's package must carry the sbin_codex edit marker.
	for _, c := range Manifest {
		for _, b := range c.Buffers {
			if b.Exclusion != "" {
				continue
			}
			dir := filepath.Join(root, "amd", "benchmarks",
				filepath.FromSlash(b.Package))
			if !dirContainsMarker(dir, "sbin_codex") {
				t.Errorf("package %s (case %s): missing sbin_codex edit marker",
					b.Package, c.Case)
			}
		}
	}

	// Every DNN benchmark implements ManagedMemoryCapable and propagates the
	// capability to its GPU operator(s).
	for _, name := range expectedDNNCases {
		pkg, ok := dnnBenchmarkPackages[name]
		if !ok {
			t.Errorf("DNN case %s: no benchmark package mapping", name)
			continue
		}
		dir := filepath.Join(root, "amd", "benchmarks", filepath.FromSlash(pkg))
		if !dirContainsMarker(dir, "SetManagedMemory") {
			t.Errorf("DNN benchmark %s: missing SetManagedMemory wiring", name)
		}
		if !dirContainsMarker(dir, "sbin_codex") {
			t.Errorf("DNN benchmark %s: missing sbin_codex edit marker", name)
		}
	}

	// matrixtranspose reachability: the Todo-6 managed branch and markers.
	mt := filepath.Join(root, "amd", "benchmarks", "amdappsdk",
		"matrixtranspose", "matrixtranspose.go")
	for _, needle := range []string{
		"SetManagedMemory", "useManagedMemory", "AllocateManagedMemory", "sbin_uvm",
	} {
		if !fileContains(mt, needle) {
			t.Errorf("matrixtranspose: missing reachability marker %q", needle)
		}
	}
}
