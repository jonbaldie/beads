// Package journalscan provides the static-analysis primitives the events
// journal completeness guards share. Both the issueops seam and the domain/db
// unit-of-work seam must journal every mutation that writes a work-bead table;
// their guard tests detect such mutators STRUCTURALLY — by the DML a function
// executes — rather than by matching on method-name prefixes, which could let a
// mutator named off-pattern ship un-journaled. This package holds the parsing,
// bead-table DML detection, and call-graph fixpoint those guards run.
package journalscan

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"regexp"
	"strings"
)

// FuncInfo captures the call/DML shape of one top-level function or method,
// keyed by a package-unique name (receiver-qualified for methods).
type FuncInfo struct {
	Recv       string   // receiver type name ("" for free functions)
	Name       string   // bare method/function name
	Exported   bool     // the bare name is exported
	IdentCalls []string // intra-package bare-identifier calls (free functions)
	SelCalls   []string // selector calls, by selector name (x.Foo -> "Foo")
	OwnBeadDML bool     // body issues INSERT/UPDATE/DELETE against a bead table
}

// AllCallNames returns every called name, both bare-identifier and selector.
func (f *FuncInfo) AllCallNames() []string {
	return append(append([]string{}, f.IdentCalls...), f.SelCalls...)
}

// CallsAnyOf reports whether the function calls any name in set (bare or selector).
func (f *FuncInfo) CallsAnyOf(set map[string]bool) bool {
	for _, c := range f.AllCallNames() {
		if set[c] {
			return true
		}
	}
	return false
}

// ReceiverTypeName returns the bare type name of a method receiver
// (e.g. *fooImpl -> fooImpl).
func ReceiverTypeName(expr ast.Expr) string {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}

// ParsePackage parses dir's non-test .go files and returns one FuncInfo per
// top-level function/method, keyed by "Recv.Name" (or "Name" for free funcs).
func ParsePackage(dir string) (map[string]*FuncInfo, error) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		return nil, err
	}
	out := map[string]*FuncInfo{}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			parseFileFunctions(out, file)
		}
	}
	return out, nil
}

func parseFileFunctions(out map[string]*FuncInfo, file *ast.File) {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		info := parseFuncDecl(fn)
		out[funcKey(info)] = info
	}
}

func parseFuncDecl(fn *ast.FuncDecl) *FuncInfo {
	info := &FuncInfo{Name: fn.Name.Name, Exported: fn.Name.IsExported()}
	if fn.Recv != nil && len(fn.Recv.List) > 0 {
		info.Recv = ReceiverTypeName(fn.Recv.List[0].Type)
	}
	ast.Inspect(fn, func(node ast.Node) bool {
		recordFuncShape(info, node)
		return true
	})
	return info
}

func recordFuncShape(info *FuncInfo, node ast.Node) {
	switch node := node.(type) {
	case *ast.CallExpr:
		recordCall(info, node)
	case *ast.BasicLit:
		if node.Kind == token.STRING && SQLWritesBeadTable(node.Value) {
			info.OwnBeadDML = true
		}
	}
}

func recordCall(info *FuncInfo, call *ast.CallExpr) {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		info.IdentCalls = append(info.IdentCalls, fun.Name)
	case *ast.SelectorExpr:
		info.SelCalls = append(info.SelCalls, fun.Sel.Name)
	}
}

func funcKey(info *FuncInfo) string {
	if info.Recv == "" {
		return info.Name
	}
	return info.Recv + "." + info.Name
}

// Fixpoint returns the set of function keys for which seed is true or which
// (transitively) call a name for which it becomes true, following edges. A
// called bare name resolves to a free function of that name and to any method
// with that name (name-based resolution, sufficient for a guard).
func Fixpoint(fns map[string]*FuncInfo, seed func(*FuncInfo) bool, edges func(*FuncInfo) []string) map[string]bool {
	resolve := buildResolver(fns)
	got := seedFunctions(fns, seed)
	for propagateFixpoint(fns, got, edges, resolve) {
	}
	return got
}

func buildResolver(fns map[string]*FuncInfo) func(string) []string {
	return func(name string) []string {
		var keys []string
		if _, ok := fns[name]; ok {
			keys = append(keys, name)
		}
		for key, f := range fns {
			if f.Recv != "" && f.Name == name {
				keys = append(keys, key)
			}
		}
		return keys
	}
}

func seedFunctions(fns map[string]*FuncInfo, seed func(*FuncInfo) bool) map[string]bool {
	got := map[string]bool{}
	for key, f := range fns {
		if seed(f) {
			got[key] = true
		}
	}
	return got
}

func propagateFixpoint(fns map[string]*FuncInfo, got map[string]bool, edges func(*FuncInfo) []string, resolve func(string) []string) bool {
	changed := false
	for key, f := range fns {
		if got[key] || !callsKnownFunction(f, got, edges, resolve) {
			continue
		}
		got[key] = true
		changed = true
	}
	return changed
}

func callsKnownFunction(f *FuncInfo, got map[string]bool, edges func(*FuncInfo) []string, resolve func(string) []string) bool {
	for _, callee := range edges(f) {
		for _, key := range resolve(callee) {
			if got[key] {
				return true
			}
		}
	}
	return false
}

// BeadTables are the work-bead tables a mutation must be journaled for.
var BeadTables = []string{
	"issues", "wisps",
	"dependencies", "wisp_dependencies",
	"labels", "wisp_labels",
	"comments", "wisp_comments",
}

// indexedVerb matches an explicit-argument-index format verb (%[1]s), which is
// the same templated table name as %s as far as this detector is concerned. It
// is normalized away before matching so a mutator cannot slip past the guard by
// reusing one format argument.
var indexedVerb = regexp.MustCompile(`%\[[0-9]+\]`)

// SQLWritesBeadTable reports whether a SQL string literal issues an
// INSERT / UPDATE / DELETE against a work-bead table, whether the table name is
// literal (INSERT INTO issues) or templated (INSERT INTO %s / INSERT INTO %[1]s
// — which in the mutation seams always routes to a bead table via table-routing
// helpers).
func SQLWritesBeadTable(lit string) bool {
	s := strings.ToUpper(lit)
	s = strings.ReplaceAll(s, "`", "")
	s = indexedVerb.ReplaceAllString(s, "%")
	s = strings.Join(strings.Fields(s), " ") // collapse whitespace/newlines
	targets := []string{"%S"}
	for _, tbl := range BeadTables {
		targets = append(targets, strings.ToUpper(tbl))
	}
	for _, tbl := range targets {
		if strings.Contains(s, "INSERT INTO "+tbl+" ") ||
			strings.Contains(s, "INSERT IGNORE INTO "+tbl+" ") ||
			strings.Contains(s, "REPLACE INTO "+tbl+" ") ||
			strings.Contains(s, "UPDATE "+tbl+" ") ||
			strings.Contains(s, "DELETE FROM "+tbl+" ") {
			return true
		}
	}
	return false
}
