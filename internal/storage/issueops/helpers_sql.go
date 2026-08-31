// Package issueops provides shared transaction-scoped SQL operations for
// issue creation and management. Both DoltStore and EmbeddedDoltStore call
// into these functions, passing their own *sql.Tx obtained through their
// respective connection lifecycle patterns.
package issueops

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/debug"
	"github.com/jonbaldie/beads/internal/storage"
	"github.com/jonbaldie/beads/internal/types"
)

// GetAdaptiveIDLengthTx returns the appropriate hash length based on database size.
//
//nolint:gosec // G201: table is a hardcoded constant
func GetAdaptiveIDLengthTx(ctx context.Context, tx DBTX, table, prefix string) (int, error) {
	var count int
	err := tx.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT COUNT(*)
		FROM %s
		WHERE id LIKE CONCAT(?, '-%%')
		  AND INSTR(SUBSTRING(id, LENGTH(?) + 2), '.') = 0
	`, table), prefix, prefix).Scan(&count)
	if err != nil {
		return 6, err
	}

	cfg := GetAdaptiveConfigTx(ctx, tx)
	return ComputeAdaptiveLength(count, cfg), nil
}

// AdaptiveIDConfig holds configuration for adaptive ID length computation.
type AdaptiveIDConfig struct {
	MaxCollisionProbability float64
	MinLength               int
	MaxLength               int
}

// DefaultAdaptiveConfig returns the default adaptive ID configuration.
func DefaultAdaptiveConfig() AdaptiveIDConfig {
	return AdaptiveIDConfig{
		MaxCollisionProbability: 0.25,
		MinLength:               3,
		MaxLength:               8,
	}
}

// GetAdaptiveConfigTx reads adaptive ID config from the database.
func GetAdaptiveConfigTx(ctx context.Context, tx DBTX) AdaptiveIDConfig {
	cfg := DefaultAdaptiveConfig()

	if prob, ok := readAdaptiveFloat(ctx, tx, "max_collision_prob"); ok {
		cfg.MaxCollisionProbability = prob
	}
	if minLen, ok := readAdaptiveInt(ctx, tx, "min_hash_length"); ok {
		cfg.MinLength = minLen
	}
	if maxLen, ok := readAdaptiveInt(ctx, tx, "max_hash_length"); ok {
		cfg.MaxLength = maxLen
	}

	return cfg
}

func readAdaptiveValue(ctx context.Context, tx DBTX, key string) (string, bool) {
	var value string
	if err := tx.QueryRowContext(ctx, "SELECT value FROM config WHERE `key` = ?", key).Scan(&value); err != nil || value == "" {
		return "", false
	}
	return value, true
}

func readAdaptiveFloat(ctx context.Context, tx DBTX, key string) (float64, bool) {
	value, ok := readAdaptiveValue(ctx, tx, key)
	if !ok {
		return 0, false
	}
	parsed, err := strconv.ParseFloat(value, 64)
	return parsed, err == nil
}

func readAdaptiveInt(ctx context.Context, tx DBTX, key string) (int, bool) {
	value, ok := readAdaptiveValue(ctx, tx, key)
	if !ok {
		return 0, false
	}
	parsed, err := strconv.Atoi(value)
	return parsed, err == nil
}

// ComputeAdaptiveLength uses the birthday paradox to pick a hash length
// that keeps collision probability below the configured threshold.
func ComputeAdaptiveLength(numIssues int, cfg AdaptiveIDConfig) int {
	const base = 36.0
	for length := cfg.MinLength; length <= cfg.MaxLength; length++ {
		totalPossibilities := math.Pow(base, float64(length))
		exponent := -float64(numIssues*numIssues) / (2.0 * totalPossibilities)
		prob := 1.0 - math.Exp(exponent)
		if prob <= cfg.MaxCollisionProbability {
			return length
		}
	}
	return cfg.MaxLength
}

// GetCustomStatusesTx reads custom statuses from config within a transaction.
func GetCustomStatusesTx(ctx context.Context, tx DBTX) ([]string, error) {
	detailed, err := ResolveCustomStatusesDetailedInTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	return types.CustomStatusNames(detailed), nil
}

// ValidateMetadataIfConfigured checks metadata against the schema from config.
func ValidateMetadataIfConfigured(metadata json.RawMessage) error {
	mode := config.MetadataValidationMode()
	if mode == "none" || mode == "" {
		return nil
	}

	rawFields := config.MetadataSchemaFields()
	if rawFields == nil {
		return nil
	}

	fields := metadataSchemaFields(rawFields)

	if len(fields) == 0 {
		return nil
	}

	schemaCfg := storage.MetadataSchemaConfig{
		Mode:   mode,
		Fields: fields,
	}

	return reportMetadataValidation(mode, metadata, schemaCfg)
}

func metadataSchemaFields(rawFields map[string]interface{}) map[string]storage.MetadataFieldSchema {
	fields := make(map[string]storage.MetadataFieldSchema)
	for name, raw := range rawFields {
		fieldMap, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		fields[name] = ParseFieldSchema(fieldMap)
	}
	return fields
}

func reportMetadataValidation(mode string, metadata json.RawMessage, schemaCfg storage.MetadataSchemaConfig) error {
	errs := storage.ValidateMetadataSchema(metadata, schemaCfg)
	if len(errs) == 0 {
		return nil
	}
	if mode == "warn" {
		for _, e := range errs {
			fmt.Fprintf(os.Stderr, "warning: %s\n", e.Error())
		}
		return nil
	}
	return fmt.Errorf("metadata schema violation: %s", errs[0].Error())
}

// ParseFieldSchema converts a raw config map into a MetadataFieldSchema.
func ParseFieldSchema(m map[string]interface{}) storage.MetadataFieldSchema {
	schema := storage.MetadataFieldSchema{}

	if t, ok := m["type"].(string); ok {
		schema.Type = storage.MetadataFieldType(t)
	}
	if req, ok := m["required"].(bool); ok {
		schema.Required = req
	}

	if vals, ok := m["values"]; ok {
		schema.Values = parseSchemaValues(vals)
	}

	if min, ok := toFloat64(m["min"]); ok {
		schema.Min = &min
	}
	if max, ok := toFloat64(m["max"]); ok {
		schema.Max = &max
	}

	return schema
}

func parseSchemaValues(values interface{}) []string {
	var parsed []string
	switch value := values.(type) {
	case []interface{}:
		for _, item := range value {
			if text, ok := item.(string); ok {
				parsed = append(parsed, text)
			}
		}
	case string:
		for _, text := range strings.Split(value, ",") {
			text = strings.TrimSpace(text)
			if text != "" {
				parsed = append(parsed, text)
			}
		}
	}
	return parsed
}

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

// IsDoltNothingToCommit returns true if the error is the benign
// "nothing to commit" Dolt message.
func IsDoltNothingToCommit(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "nothing to commit") ||
		(strings.Contains(s, "no changes") && strings.Contains(s, "commit"))
}

// ReadConfigPrefix reads and normalizes issue_prefix from the config table.
func ReadConfigPrefix(ctx context.Context, tx DBTX) (string, error) {
	var configPrefix string
	err := tx.QueryRowContext(ctx, "SELECT value FROM config WHERE `key` = ?", "issue_prefix").Scan(&configPrefix)
	if err == sql.ErrNoRows || configPrefix == "" {
		yamlPrefix := strings.TrimSpace(config.GetString("issue-prefix"))
		underscoreYamlPrefix := strings.TrimSpace(config.GetString("issue_prefix"))
		debug.Logf("Debug: missing config.issue_prefix in database (err=%v, db value=%q, yaml issue-prefix=%q, yaml issue_prefix=%q)\n",
			err, configPrefix, yamlPrefix, underscoreYamlPrefix)
		return "", fmt.Errorf("%w: issue_prefix config is missing (run 'bd init --prefix <prefix>' for a new project, or 'bd bootstrap' to clone an existing remote; if using config.yaml, use key 'issue-prefix', not 'issue_prefix')", storage.ErrNotInitialized)
	} else if err != nil {
		return "", fmt.Errorf("failed to get config: %w", err)
	}
	return strings.TrimSuffix(configPrefix, "-"), nil
}

// ---------------------------------------------------------------------------
// Nullable value helpers
// ---------------------------------------------------------------------------

// NullString returns nil for empty strings, otherwise the string value.
func NullString(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// NullStringPtr returns nil for nil pointers, otherwise the pointed-to string.
func NullStringPtr(s *string) interface{} {
	if s == nil {
		return nil
	}
	return *s
}

// NullInt returns nil for nil pointers, otherwise the pointed-to int.
func NullInt(i *int) interface{} {
	if i == nil {
		return nil
	}
	return *i
}

// NullIntVal returns nil for zero values, otherwise the int.
func NullIntVal(i int) interface{} {
	if i == 0 {
		return nil
	}
	return i
}

// JSONMetadata returns the metadata as a JSON string, defaulting to "{}".
func JSONMetadata(m []byte) string {
	if len(m) == 0 {
		return "{}"
	}
	if !json.Valid(m) {
		fmt.Fprintf(os.Stderr, "Warning: invalid JSON metadata, using empty object\n")
		return "{}"
	}
	return string(m)
}

// FormatJSONStringArray marshals a string slice to JSON, returning "" for empty/nil.
func FormatJSONStringArray(arr []string) string {
	if len(arr) == 0 {
		return ""
	}
	data, err := json.Marshal(arr)
	if err != nil {
		return ""
	}
	return string(data)
}
