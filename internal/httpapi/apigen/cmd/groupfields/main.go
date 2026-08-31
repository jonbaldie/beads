// Command groupfields keeps generated OpenAPI transport structs below the
// repository's field-count limit while preserving their promoted field API.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
)

const fieldsPerGroup = 14

var groupedTypes = map[string]bool{
	"ApplyCreateItem":      true,
	"ApplyPatchBody":       true,
	"ClaimNextIssueParams": true,
	"CountIssuesParams":    true,
	"CreateIssueRequest":   true,
	"IssuePatchBody":       true,
	"ListIssuesParams":     true,
	"ListReadyWorkParams":  true,
	"Problem":              true,
}

type sourceEdit struct {
	start int
	end   int
	text  string
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		panic(err)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("groupfields", flag.ContinueOnError)
	path := flags.String("file", "", "generated Go file to rewrite")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *path == "" {
		return fmt.Errorf("-file is required")
	}
	source, err := os.ReadFile(*path)
	if err != nil {
		return fmt.Errorf("read %s: %w", *path, err)
	}
	rewritten, err := groupSource(source)
	if err != nil {
		return fmt.Errorf("group %s: %w", *path, err)
	}
	if err := os.WriteFile(*path, rewritten, 0o644); err != nil { //nolint:gosec // G306: generated Go source is intentionally repository-readable.
		return fmt.Errorf("write %s: %w", *path, err)
	}
	return nil
}

func groupSource(source []byte) ([]byte, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "generated.go", source, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	edits := collectEdits(fset, file, source)
	sort.Slice(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
	for _, edit := range edits {
		source = append(source[:edit.start], append([]byte(edit.text), source[edit.end:]...)...)
	}
	return format.Source(source)
}

func collectEdits(fset *token.FileSet, file *ast.File, source []byte) []sourceEdit {
	edits := make([]sourceEdit, 0, len(groupedTypes))
	for _, decl := range file.Decls {
		name, structure := oversizedGeneratedStruct(decl)
		if structure != nil {
			edits = append(edits, makeEdit(fset, source, name, structure))
		}
	}
	return edits
}

func oversizedGeneratedStruct(decl ast.Decl) (string, *ast.StructType) {
	gen, ok := decl.(*ast.GenDecl)
	if !ok || gen.Tok != token.TYPE || len(gen.Specs) != 1 {
		return "", nil
	}
	spec, ok := gen.Specs[0].(*ast.TypeSpec)
	if !ok || !groupedTypes[spec.Name.Name] {
		return "", nil
	}
	structure, ok := spec.Type.(*ast.StructType)
	if !ok || len(structure.Fields.List) <= fieldsPerGroup {
		return "", nil
	}
	return spec.Name.Name, structure
}

func makeEdit(fset *token.FileSet, source []byte, typeName string, structure *ast.StructType) sourceEdit {
	fields := structure.Fields.List
	fieldCount := len(fields)
	groupCount := (fieldCount + fieldsPerGroup - 1) / fieldsPerGroup
	names := make([]string, groupCount)
	chunks := make([]string, groupCount)
	for group := range groupCount {
		start := group * fieldsPerGroup
		end := min(start+fieldsPerGroup, fieldCount)
		names[group] = fmt.Sprintf("%sFields%d", typeName, group+1)
		chunks[group] = fieldSource(fset, source, fields, start, end, structure.Fields.Closing)
	}

	var replacement strings.Builder
	replacement.WriteString("{\n")
	for _, name := range names {
		fmt.Fprintf(&replacement, "\t%s\n", name)
	}
	replacement.WriteString("}")
	for i, name := range names {
		fmt.Fprintf(&replacement, "\n\n// %s groups generated fields for %s.\ntype %s struct {\n%s}", name, typeName, name, chunks[i])
	}
	return sourceEdit{
		start: fset.Position(structure.Fields.Opening).Offset,
		end:   fset.Position(structure.Fields.Closing).Offset + 1,
		text:  replacement.String(),
	}
}

func fieldSource(fset *token.FileSet, source []byte, fields []*ast.Field, start, end int, rbrace token.Pos) string {
	startOffset := lineStart(source, fset.Position(fieldStart(fields[start])).Offset)
	endOffset := lineStart(source, fset.Position(rbrace).Offset)
	if end < len(fields) {
		endOffset = lineStart(source, fset.Position(fieldStart(fields[end])).Offset)
	}
	return string(source[startOffset:endOffset])
}

func fieldStart(field *ast.Field) token.Pos {
	if field.Doc != nil {
		return field.Doc.Pos()
	}
	return field.Pos()
}

func lineStart(source []byte, offset int) int {
	if newline := bytes.LastIndexByte(source[:offset], '\n'); newline >= 0 {
		return newline + 1
	}
	return 0
}
