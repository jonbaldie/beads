package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/jonbaldie/beads/internal/formula"
	"github.com/jonbaldie/beads/internal/metrics"
	"github.com/spf13/cobra"
)

func runFormulaConvert(cmd *cobra.Command, args []string) error {
	evt := metrics.NewCommandEvent("formula-convert")
	defer func() {
		if c := metrics.Global(); c != nil {
			c.CloseEventAndAdd(evt)
		}
	}()

	convertAll, _ := cmd.Flags().GetBool("all")
	convertDelete, _ := cmd.Flags().GetBool("delete")
	convertStdout, _ := cmd.Flags().GetBool("stdout")
	if convertAll {
		convertAllFormulas(convertDelete)
		return nil
	}
	return convertFormula(args, convertDelete, convertStdout)
}

func convertFormula(args []string, convertDelete, convertStdout bool) error {
	if len(args) == 0 {
		return HandleErrorWithHint("formula name or path required", "Usage: bd formula convert <name|path> [--all]")
	}

	jsonPath, err := resolveFormulaJSONPath(args[0])
	if err != nil {
		return err
	}

	parser := formula.NewParser()
	f, err := parser.ParseFile(jsonPath)
	if err != nil {
		return HandleError("parsing %s: %v", jsonPath, err)
	}

	tomlData, err := formulaToTOML(f)
	if err != nil {
		return HandleError("converting to TOML: %v", err)
	}

	if convertStdout {
		fmt.Print(string(tomlData))
		return nil
	}

	return writeConvertedFormula(jsonPath, tomlData, convertDelete)
}

func resolveFormulaJSONPath(name string) (string, error) {
	if strings.HasSuffix(name, formula.FormulaExtJSON) {
		return name, nil
	}
	if strings.HasSuffix(name, formula.FormulaExtTOML) {
		return "", HandleError("%s is already a TOML file", name)
	}

	jsonPath := findFormulaJSON(name)
	if jsonPath != "" {
		return jsonPath, nil
	}

	fmt.Fprintf(os.Stderr, "Error: JSON formula %q not found\n", name)
	fmt.Fprintf(os.Stderr, "\nSearch paths:\n")
	for _, p := range getFormulaSearchPaths() {
		fmt.Fprintf(os.Stderr, "  %s\n", p)
	}
	return "", SilentExit()
}

func writeConvertedFormula(jsonPath string, tomlData []byte, convertDelete bool) error {
	tomlPath := strings.TrimSuffix(jsonPath, formula.FormulaExtJSON) + formula.FormulaExtTOML
	if err := os.WriteFile(tomlPath, tomlData, 0600); err != nil {
		return HandleError("writing %s: %v", tomlPath, err)
	}

	fmt.Printf("✓ Converted: %s\n", tomlPath)
	if convertDelete {
		deleteConvertedFormula(jsonPath)
	}
	return nil
}

func deleteConvertedFormula(jsonPath string) {
	if err := os.Remove(jsonPath); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not delete %s: %v\n", jsonPath, err) //nolint:gosec // G705: CLI stderr, not HTML.
		return
	}
	fmt.Printf("✓ Deleted: %s\n", jsonPath)
}

func convertAllFormulas(convertDelete bool) {
	converted, errors := convertAllFormulaDirectories(convertDelete)

	fmt.Printf("\nConverted %d formulas", converted)
	if errors > 0 {
		fmt.Printf(" (%d errors)", errors)
	}
	fmt.Println()
}

func convertAllFormulaDirectories(convertDelete bool) (int, int) {
	converted := 0
	errors := 0

	for _, dir := range getFormulaSearchPaths() {
		convertedInDir, errorsInDir := convertFormulaDirectory(dir, convertDelete)
		converted += convertedInDir
		errors += errorsInDir
	}
	return converted, errors
}

func convertFormulaDirectory(dir string, convertDelete bool) (int, int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0
	}

	parser := formula.NewParser(dir)
	converted := 0
	errors := 0
	for _, entry := range entries {
		convertedEntry, failedEntry := convertFormulaEntry(parser, dir, entry, convertDelete)
		if convertedEntry {
			converted++
		}
		if failedEntry {
			errors++
		}
	}
	return converted, errors
}

func convertFormulaEntry(parser *formula.Parser, dir string, entry os.DirEntry, convertDelete bool) (bool, bool) {
	if entry.IsDir() || !strings.HasSuffix(entry.Name(), formula.FormulaExtJSON) {
		return false, false
	}

	jsonPath := filepath.Join(dir, entry.Name())
	tomlPath := strings.TrimSuffix(jsonPath, formula.FormulaExtJSON) + formula.FormulaExtTOML
	if _, err := os.Stat(tomlPath); err == nil {
		fmt.Printf("⏭ Skipped (TOML exists): %s\n", entry.Name())
		return false, false
	}

	f, err := parser.ParseFile(jsonPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ Error parsing %s: %v\n", jsonPath, err)
		return false, true
	}

	tomlData, err := formulaToTOML(f)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ Error converting %s: %v\n", jsonPath, err)
		return false, true
	}

	if err := os.WriteFile(tomlPath, tomlData, 0600); err != nil {
		fmt.Fprintf(os.Stderr, "✗ Error writing %s: %v\n", tomlPath, err)
		return false, true
	}

	fmt.Printf("✓ Converted: %s\n", tomlPath)
	if convertDelete {
		deleteConvertedFormula(jsonPath)
	}
	return true, false
}

// findFormulaJSON searches for a JSON formula file by name.
func findFormulaJSON(name string) string {
	for _, dir := range getFormulaSearchPaths() {
		path := filepath.Join(dir, name+formula.FormulaExtJSON)
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return ""
}

// formulaToTOML converts a Formula to TOML bytes.
// Uses a custom structure optimized for TOML readability.
func formulaToTOML(f *formula.Formula) ([]byte, error) {
	// We need to re-read the original JSON to get the raw structure
	// because the Formula struct loses some ordering/formatting
	if f.Source == "" {
		return nil, fmt.Errorf("formula has no source path")
	}

	// Read the original JSON
	jsonData, err := os.ReadFile(f.Source)
	if err != nil {
		return nil, fmt.Errorf("reading source: %w", err)
	}

	// Parse into a map to preserve structure
	var raw map[string]interface{}
	if err := json.Unmarshal(jsonData, &raw); err != nil {
		return nil, fmt.Errorf("parsing JSON: %w", err)
	}

	// Fix float64 to int for known integer fields
	fixIntegerFields(raw)

	// Encode to TOML
	var buf bytes.Buffer
	encoder := toml.NewEncoder(&buf)
	encoder.Indent = ""
	if err := encoder.Encode(raw); err != nil {
		return nil, fmt.Errorf("encoding TOML: %w", err)
	}

	// Post-process to convert escaped \n in strings to multi-line strings
	result := convertToMultiLineStrings(buf.String())

	return []byte(result), nil
}

// convertToMultiLineStrings post-processes TOML to use multi-line strings
// where strings contain newlines. This improves readability for descriptions.
func convertToMultiLineStrings(input string) string {
	// Regular expression to match key = "value with \n"
	// We look for description fields specifically as those benefit most
	lines := strings.Split(input, "\n")
	var result []string

	for _, line := range lines {
		// Check if this line has a string with escaped newlines
		if strings.Contains(line, "\\n") {
			// Find the key = "..." pattern
			eqIdx := strings.Index(line, " = \"")
			if eqIdx > 0 && strings.HasSuffix(line, "\"") {
				key := strings.TrimSpace(line[:eqIdx])
				// Only convert description fields
				if key == "description" {
					// Extract the value (without quotes)
					value := line[eqIdx+4 : len(line)-1]
					// Unescape the newlines
					value = strings.ReplaceAll(value, "\\n", "\n")
					// Use multi-line string syntax
					result = append(result, fmt.Sprintf("%s = \"\"\"\n%s\"\"\"", key, value))
					continue
				}
			}
		}
		result = append(result, line)
	}

	return strings.Join(result, "\n")
}

// fixIntegerFields recursively fixes float64 values that should be integers.
// JSON unmarshals all numbers as float64, but TOML needs proper int types.
func fixIntegerFields(m map[string]interface{}) {
	// Known integer fields
	intFields := map[string]bool{
		"version":  true,
		"priority": true,
		"count":    true,
		"max":      true,
	}

	for k, v := range m {
		switch val := v.(type) {
		case float64:
			// Convert whole numbers to int64 if they're known int fields
			if intFields[k] && val == float64(int64(val)) {
				m[k] = int64(val)
			}
		case map[string]interface{}:
			fixIntegerFields(val)
		case []interface{}:
			for _, item := range val {
				if subMap, ok := item.(map[string]interface{}); ok {
					fixIntegerFields(subMap)
				}
			}
		}
	}
}

func init() {
	formulaListCmd.Flags().String("type", "", "Filter by type (workflow, expansion, aspect, convoy)")
	formulaConvertCmd.Flags().Bool("all", false, "Convert all JSON formulas")
	formulaConvertCmd.Flags().Bool("delete", false, "Delete JSON file after conversion")
	formulaConvertCmd.Flags().Bool("stdout", false, "Print TOML to stdout instead of file")

	formulaCmd.AddCommand(formulaListCmd)
	formulaCmd.AddCommand(formulaShowCmd)
	formulaCmd.AddCommand(formulaConvertCmd)
	rootCmd.AddCommand(formulaCmd)
}
