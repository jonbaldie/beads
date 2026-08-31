// Package schemagen walks an internal/formula types.go file and produces
// the body of internal/formula/schema_gen.go (a `var Primitives` index of
// every exported struct). The package is consumed both by the standalone
// generator binary in cmd/schemagen and by the TestSchemaGenIsCurrent test
// in internal/formula.
package schemagen

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/printer"
	"go/token"
	"sort"
	"strconv"
	"strings"
)

// Primitive is the in-memory representation of one PrimitiveDoc. Kept
// distinct from formula.PrimitiveDoc to avoid an import cycle: this package
// generates the source that defines formula.Primitives.
type Primitive struct {
	Name   string
	Doc    string
	Fields []Field
}

// Field is the in-memory representation of one FieldDoc.
type Field struct {
	Name     string
	Type     string
	JSONName string
	TOMLName string
	Required bool
	Doc      string
}

// Generate parses typesPath and returns gofmt'd source for schema_gen.go.
// The output is deterministic given the same input.
func Generate(typesPath string) ([]byte, error) {
	prims, err := Parse(typesPath)
	if err != nil {
		return nil, err
	}
	return Render(prims)
}

// Parse extracts every exported struct in typesPath, sorted by name.
func Parse(typesPath string) ([]Primitive, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, typesPath, nil, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", typesPath, err)
	}

	// Keep the struct declarations available while walking fields so anonymous
	// embedded structs can be flattened just as encoding/json flattens them.
	// The generated schema is a description of the wire shape, not of the
	// implementation's grouping seams.
	structs := collectStructs(file)
	prims := collectPrimitives(fset, file, structs)

	sort.Slice(prims, func(i, j int) bool {
		return prims[i].Name < prims[j].Name
	})
	return prims, nil
}

func collectStructs(file *ast.File) map[string]*ast.StructType {
	structs := make(map[string]*ast.StructType)
	for _, decl := range file.Decls {
		gen, ok := typeDecl(decl)
		if !ok {
			continue
		}
		addStructs(structs, gen)
	}
	return structs
}

func addStructs(structs map[string]*ast.StructType, gen *ast.GenDecl) {
	for _, spec := range gen.Specs {
		ts, st, ok := structSpec(spec)
		if ok {
			structs[ts.Name.Name] = st
		}
	}
}

func collectPrimitives(fset *token.FileSet, file *ast.File, structs map[string]*ast.StructType) []Primitive {
	var prims []Primitive
	for _, decl := range file.Decls {
		gen, ok := typeDecl(decl)
		if !ok {
			continue
		}
		prims = append(prims, primitivesFromDecl(fset, gen, structs)...)
	}
	return prims
}

func primitivesFromDecl(fset *token.FileSet, gen *ast.GenDecl, structs map[string]*ast.StructType) []Primitive {
	var prims []Primitive
	for _, spec := range gen.Specs {
		primitive, ok := primitiveFromSpec(fset, gen, spec, structs)
		if ok {
			prims = append(prims, primitive)
		}
	}
	return prims
}

func primitiveFromSpec(fset *token.FileSet, gen *ast.GenDecl, spec ast.Spec, structs map[string]*ast.StructType) (Primitive, bool) {
	ts, st, ok := structSpec(spec)
	if !ok || !ts.Name.IsExported() {
		return Primitive{}, false
	}
	return Primitive{
		Name:   ts.Name.Name,
		Doc:    extractDoc(ts.Doc, gen.Doc),
		Fields: collectFields(fset, st, structs, nil),
	}, true
}

func typeDecl(decl ast.Decl) (*ast.GenDecl, bool) {
	gen, ok := decl.(*ast.GenDecl)
	return gen, ok && gen.Tok == token.TYPE
}

func structSpec(spec ast.Spec) (*ast.TypeSpec, *ast.StructType, bool) {
	ts, ok := spec.(*ast.TypeSpec)
	if !ok {
		return nil, nil, false
	}
	st, ok := ts.Type.(*ast.StructType)
	return ts, st, ok
}

func collectFields(fset *token.FileSet, st *ast.StructType, structs map[string]*ast.StructType, seen map[string]bool) []Field {
	if st.Fields == nil {
		return nil
	}
	var fields []Field
	for _, f := range st.Fields.List {
		fields = append(fields, fieldsForField(fset, f, structs, seen)...)
	}
	return fields
}

func fieldsForField(fset *token.FileSet, field *ast.Field, structs map[string]*ast.StructType, seen map[string]bool) []Field {
	if len(field.Names) == 0 {
		return fieldsForEmbedded(fset, field.Type, structs, seen)
	}
	return fieldsForNamed(fset, field)
}

func fieldsForEmbedded(fset *token.FileSet, expr ast.Expr, structs map[string]*ast.StructType, seen map[string]bool) []Field {
	name, ok := embeddedStructName(expr)
	if !ok || seen[name] {
		return nil
	}
	nested, ok := structs[name]
	if !ok {
		return nil
	}
	if seen == nil {
		seen = make(map[string]bool)
	}
	seen[name] = true
	fields := collectFields(fset, nested, structs, seen)
	delete(seen, name)
	return fields
}

func fieldsForNamed(fset *token.FileSet, field *ast.Field) []Field {
	jsonTag, tomlTag := extractTags(field.Tag)
	// Skip fields explicitly excluded from JSON serialization — these
	// are internal implementation details (e.g. Step.SourceFormula).
	if jsonTag.name == "-" {
		return nil
	}
	typeStr := renderType(fset, field.Type)
	doc := extractDoc(field.Doc, nil)
	var fields []Field
	for _, name := range field.Names {
		if parsed, ok := fieldDoc(name, typeStr, jsonTag, tomlTag, doc); ok {
			fields = append(fields, parsed)
		}
	}
	return fields
}

func fieldDoc(name *ast.Ident, typeStr string, jsonTag, tomlTag structTag, doc string) (Field, bool) {
	if !name.IsExported() {
		return Field{}, false
	}
	jsonName := jsonTag.name
	if jsonName == "" {
		jsonName = name.Name
	}
	tomlName := tomlTag.name
	if tomlName == "" {
		tomlName = jsonName
	}
	return Field{
		Name:     name.Name,
		Type:     typeStr,
		JSONName: jsonName,
		TOMLName: tomlName,
		Required: !jsonTag.omitempty && jsonTag.name != "-",
		Doc:      doc,
	}, true
}

func embeddedStructName(expr ast.Expr) (string, bool) {
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name, true
	}
	if star, ok := expr.(*ast.StarExpr); ok {
		ident, ok := star.X.(*ast.Ident)
		if ok {
			return ident.Name, true
		}
	}
	return "", false
}

type structTag struct {
	name      string
	omitempty bool
}

func extractTags(tag *ast.BasicLit) (jsonTag, tomlTag structTag) {
	if tag == nil {
		return
	}
	raw, err := strconv.Unquote(tag.Value)
	if err != nil {
		return
	}
	st := newReflectStructTag(raw)
	if v, ok := st.lookup("json"); ok {
		jsonTag = parseTagValue(v)
	}
	if v, ok := st.lookup("toml"); ok {
		tomlTag = parseTagValue(v)
	}
	return
}

func parseTagValue(v string) structTag {
	parts := strings.Split(v, ",")
	t := structTag{name: parts[0]}
	for _, p := range parts[1:] {
		if p == "omitempty" {
			t.omitempty = true
		}
	}
	return t
}

// reflectStructTag is a tiny stand-in for reflect.StructTag.Lookup that
// avoids depending on reflect at codegen time. Behavior matches the stdlib:
// keys are space-separated, values are double-quoted.
type reflectStructTag string

func newReflectStructTag(s string) reflectStructTag { return reflectStructTag(s) }

func (t reflectStructTag) lookup(key string) (string, bool) {
	tag := string(t)
	for tag != "" {
		name, quoted, rest, ok := nextStructTag(tag)
		if !ok {
			return "", false
		}
		if name == key {
			value, err := strconv.Unquote(quoted)
			if err != nil {
				return "", false
			}
			return value, true
		}
		tag = rest
	}
	return "", false
}

func nextStructTag(tag string) (name, quoted, rest string, ok bool) {
	tag = strings.TrimLeft(tag, " ")
	if tag == "" {
		return "", "", "", false
	}
	separator := strings.IndexAny(tag, ":\"")
	if separator <= 0 || separator+1 >= len(tag) || tag[separator] != ':' || tag[separator+1] != '"' {
		return "", "", "", false
	}
	value := tag[separator+1:]
	end := quotedValueEnd(value)
	if end < 0 {
		return "", "", "", false
	}
	return tag[:separator], value[:end+1], value[end+1:], true
}

func quotedValueEnd(value string) int {
	n := len(value)
	for i := 1; i < n; i++ {
		if value[i] == '\\' {
			i++
			continue
		}
		if value[i] == '"' {
			return i
		}
	}
	return -1
}

func renderType(fset *token.FileSet, expr ast.Expr) string {
	var buf bytes.Buffer
	cfg := printer.Config{Mode: printer.RawFormat, Tabwidth: 8}
	if err := cfg.Fprint(&buf, fset, expr); err != nil {
		return fmt.Sprintf("<unprintable: %v>", err)
	}
	return buf.String()
}

func extractDoc(primary, fallback *ast.CommentGroup) string {
	g := primary
	if g == nil {
		g = fallback
	}
	if g == nil {
		return ""
	}
	return strings.TrimRight(g.Text(), "\n")
}

// Render emits a gofmt'd schema_gen.go body from prims.
func Render(prims []Primitive) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("// Code generated by internal/formula/cmd/schemagen. DO NOT EDIT.\n")
	buf.WriteString("\n")
	buf.WriteString("package formula\n")
	buf.WriteString("\n")
	buf.WriteString("// Primitives is the discoverability index of every exported struct in\n")
	buf.WriteString("// types.go. Surfaced via `bd formula schema`. Regenerate with `go generate ./...`.\n")
	buf.WriteString("var Primitives = []PrimitiveDoc{\n")
	for _, p := range prims {
		buf.WriteString("\t{\n")
		fmt.Fprintf(&buf, "\t\tName: %q,\n", p.Name)
		if p.Doc != "" {
			fmt.Fprintf(&buf, "\t\tDoc:  %s,\n", goStringLit(p.Doc))
		}
		if len(p.Fields) > 0 {
			buf.WriteString("\t\tFields: []FieldDoc{\n")
			for _, f := range p.Fields {
				buf.WriteString("\t\t\t{\n")
				fmt.Fprintf(&buf, "\t\t\t\tName:     %q,\n", f.Name)
				fmt.Fprintf(&buf, "\t\t\t\tType:     %q,\n", f.Type)
				fmt.Fprintf(&buf, "\t\t\t\tJSONName: %q,\n", f.JSONName)
				if f.TOMLName != "" && f.TOMLName != f.JSONName {
					fmt.Fprintf(&buf, "\t\t\t\tTOMLName: %q,\n", f.TOMLName)
				}
				if f.Required {
					buf.WriteString("\t\t\t\tRequired: true,\n")
				}
				if f.Doc != "" {
					fmt.Fprintf(&buf, "\t\t\t\tDoc:      %s,\n", goStringLit(f.Doc))
				}
				buf.WriteString("\t\t\t},\n")
			}
			buf.WriteString("\t\t},\n")
		}
		buf.WriteString("\t},\n")
	}
	buf.WriteString("}\n")
	return format.Source(buf.Bytes())
}

// goStringLit emits a Go string literal — backtick-quoted when the content
// is "clean" (no backticks, no control chars except newline), otherwise
// double-quoted via strconv.Quote so escapes survive.
func goStringLit(s string) string {
	if strings.ContainsRune(s, '`') {
		return strconv.Quote(s)
	}
	for _, r := range s {
		if r < 0x20 && r != '\n' && r != '\t' {
			return strconv.Quote(s)
		}
	}
	return "`" + s + "`"
}
