package dolt

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/storage"
)

// loadMetadataSchema reads the metadata validation config from YAML and
// returns a parsed schema. Returns mode "none" with empty fields if config
// is not initialized, mode is empty/unknown, or no fields are defined.
func loadMetadataSchema() storage.MetadataSchemaConfig {
	mode := config.MetadataValidationMode()
	if mode == "none" {
		return storage.MetadataSchemaConfig{Mode: "none"}
	}

	rawFields := config.MetadataSchemaFields()
	if rawFields == nil {
		return storage.MetadataSchemaConfig{Mode: "none"}
	}

	fields := make(map[string]storage.MetadataFieldSchema)
	for name, raw := range rawFields {
		fieldMap, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		schema := parseFieldSchema(fieldMap)
		fields[name] = schema
	}

	if len(fields) == 0 {
		return storage.MetadataSchemaConfig{Mode: "none"}
	}

	return storage.MetadataSchemaConfig{
		Mode:   mode,
		Fields: fields,
	}
}

// parseFieldSchema converts a raw config map into a MetadataFieldSchema.
func parseFieldSchema(m map[string]interface{}) storage.MetadataFieldSchema {
	min, hasMin := toFloat64(m["min"])
	max, hasMax := toFloat64(m["max"])
	schema := storage.MetadataFieldSchema{
		Type:     fieldSchemaType(m),
		Required: fieldSchemaRequired(m),
		Values:   parseFieldValues(m["values"]),
	}
	if hasMin {
		schema.Min = &min
	}
	if hasMax {
		schema.Max = &max
	}
	return schema
}

func fieldSchemaType(m map[string]interface{}) storage.MetadataFieldType {
	value, _ := m["type"].(string)
	return storage.MetadataFieldType(value)
}

func fieldSchemaRequired(m map[string]interface{}) bool {
	value, _ := m["required"].(bool)
	return value
}

func parseFieldValues(raw interface{}) []string {
	switch values := raw.(type) {
	case []interface{}:
		return stringValues(values)
	case string:
		return splitFieldValues(values)
	default:
		return nil
	}
}

func stringValues(values []interface{}) []string {
	result := make([]string, 0, len(values))
	for _, item := range values {
		if value, ok := item.(string); ok {
			result = append(result, value)
		}
	}
	return result
}

func splitFieldValues(values string) []string {
	result := make([]string, 0)
	for _, value := range strings.Split(values, ",") {
		if value = strings.TrimSpace(value); value != "" {
			result = append(result, value)
		}
	}
	return result
}

// toFloat64 converts an interface{} to float64, handling int and float YAML values.
func toFloat64(v interface{}) (float64, bool) {
	if v == nil {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}

// validateMetadataIfConfigured checks metadata against the schema from config.
// In "warn" mode, prints warnings to stderr and returns nil.
// In "error" mode, returns the first validation error.
// In "none" mode (or if config is not initialized), does nothing.
func validateMetadataIfConfigured(metadata json.RawMessage) error {
	schema := loadMetadataSchema()
	if schema.Mode == "none" {
		return nil
	}

	errs := storage.ValidateMetadataSchema(metadata, schema)
	if len(errs) == 0 {
		return nil
	}

	if schema.Mode == "warn" {
		for _, e := range errs {
			fmt.Fprintf(os.Stderr, "warning: %s\n", e.Error())
		}
		return nil
	}

	// mode == "error"
	return fmt.Errorf("metadata schema violation: %s", errs[0].Error())
}
