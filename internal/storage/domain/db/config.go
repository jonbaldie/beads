package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jonbaldie/beads/internal/config"
	"github.com/jonbaldie/beads/internal/storage/dberrors"
	"github.com/jonbaldie/beads/internal/storage/domain"
	"github.com/jonbaldie/beads/internal/storage/issueops"
	"github.com/jonbaldie/beads/internal/types"
)

func NewConfigSQLRepository(runner Runner) domain.ConfigSQLRepository {
	core := &configRepositoryCore{runner: runner}
	metadata := &configMetadataRepository{configRepositoryCore: core}
	values := &configValueRepository{configRepositoryCore: core}
	statuses := &configStatusRepository{configRepositoryCore: core}
	repo := &configSQLRepositoryImpl{
		configMetadataRepository: metadata,
		configValueRepository:    values,
		configStatusRepository:   statuses,
	}
	_ = repo.configMetadataRepository
	_ = repo.configValueRepository
	_ = repo.configStatusRepository
	return repo
}

type configSQLRepositoryImpl struct {
	*configMetadataRepository
	*configValueRepository
	*configStatusRepository
}

type configRepositoryCore struct {
	runner Runner
}

type configMetadataRepository struct {
	*configRepositoryCore
}

type configValueRepository struct {
	*configRepositoryCore
}

type configStatusRepository struct {
	*configRepositoryCore
}

var _ domain.ConfigSQLRepository = (*configSQLRepositoryImpl)(nil)

func (r *configMetadataRepository) GetMetadata(ctx context.Context, key string) (string, error) {
	var value string
	err := r.runner.QueryRowContext(ctx, "SELECT value FROM metadata WHERE `key` = ?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("db: GetMetadata %s: %w", key, err)
	}
	return value, nil
}

func (r *configMetadataRepository) SetMetadata(ctx context.Context, key, value string) error {
	if _, err := r.runner.ExecContext(ctx, "REPLACE INTO metadata (`key`, value) VALUES (?, ?)", key, value); err != nil {
		return fmt.Errorf("db: SetMetadata %s: %w", key, err)
	}
	return nil
}

func (r *configMetadataRepository) GetLocalMetadata(ctx context.Context, key string) (string, error) {
	var value string
	err := r.runner.QueryRowContext(ctx, "SELECT value FROM local_metadata WHERE `key` = ?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("db: GetLocalMetadata %s: %w", key, err)
	}
	return value, nil
}

func (r *configMetadataRepository) SetLocalMetadata(ctx context.Context, key, value string) error {
	if _, err := r.runner.ExecContext(ctx, "REPLACE INTO local_metadata (`key`, value) VALUES (?, ?)", key, value); err != nil {
		return fmt.Errorf("db: SetLocalMetadata %s: %w", key, err)
	}
	return nil
}

func (r *configValueRepository) GetConfig(ctx context.Context, key string) (string, error) {
	var value string
	err := r.runner.QueryRowContext(ctx, "SELECT value FROM config WHERE `key` = ?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("db: GetConfig %s: %w", key, err)
	}
	return value, nil
}

func (r *configValueRepository) SetConfig(ctx context.Context, key, value string) error {
	if key == "issue_prefix" {
		value = strings.TrimSuffix(value, "-")
	}
	if _, err := r.runner.ExecContext(ctx, "REPLACE INTO config (`key`, value) VALUES (?, ?)", key, value); err != nil {
		return fmt.Errorf("db: SetConfig %s: %w", key, err)
	}
	// Re-sync the normalized lookup table a value backs, mirroring
	// DoltStore.SetConfig. Reads are TABLE-FIRST — GetCustomTypes above
	// consults custom_types and falls back to the string only when the table is
	// empty, and GetCustomStatuses reads custom_statuses outright — so a write
	// that updated only the string left the table holding the previous set,
	// forever: `bd config set types.custom` on a proxied deployment reported
	// success and `bd create -t <the new type>` kept answering "invalid issue
	// type", with doctor re-verifying against the string and reporting all-OK.
	//
	// The caller supplies a transactional runner, so the row and its projection
	// commit together or neither does.
	if _, err := issueops.SyncConfigTables(ctx, r.runner, key, value); err != nil {
		return fmt.Errorf("db: SetConfig %s: %w", key, err)
	}
	return nil
}

func (r *configValueRepository) DeleteConfig(ctx context.Context, key string) error {
	if _, err := r.runner.ExecContext(ctx, "DELETE FROM config WHERE `key` = ?", key); err != nil {
		return fmt.Errorf("db: DeleteConfig %s: %w", key, err)
	}
	return nil
}

func (r *configValueRepository) GetAllConfig(ctx context.Context) (map[string]string, error) {
	rows, err := r.runner.QueryContext(ctx, "SELECT `key`, value FROM config")
	if err != nil {
		return nil, fmt.Errorf("db: GetAllConfig: %w", err)
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("db: GetAllConfig: scan: %w", err)
		}
		out[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: GetAllConfig: read: %w", err)
	}
	return out, nil
}

func (r *configValueRepository) GetCustomTypes(ctx context.Context) ([]string, error) {
	fromTable, err := r.readCustomTypesTable(ctx)
	if err != nil {
		return nil, err
	}

	fromDB := fromTable
	if len(fromDB) == 0 {
		fromConfig, err := r.readCustomTypesConfig(ctx)
		if err != nil {
			return nil, err
		}
		fromDB = fromConfig
	}

	return unionWithYAMLCustomTypes(fromDB, config.GetCustomTypesFromYAML()), nil
}

func (r *configValueRepository) readCustomTypesTable(ctx context.Context) ([]string, error) {
	rows, err := r.runner.QueryContext(ctx, "SELECT name FROM custom_types ORDER BY name")
	if err != nil {
		if dberrors.IsTableNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("db: GetCustomTypes: query custom_types: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("db: GetCustomTypes: scan custom_types: %w", err)
		}
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: GetCustomTypes: read custom_types: %w", err)
	}
	return out, nil
}

func (r *configValueRepository) readCustomTypesConfig(ctx context.Context) ([]string, error) {
	value, err := r.GetConfig(ctx, "types.custom")
	if err != nil {
		return nil, fmt.Errorf("db: GetCustomTypes: %w", err)
	}
	return issueops.ParseTypesConfigValue(value), nil
}

func unionWithYAMLCustomTypes(dbTypes, yamlTypes []string) []string {
	if len(dbTypes) == 0 && len(yamlTypes) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(dbTypes)+len(yamlTypes))
	out := make([]string, 0, len(dbTypes)+len(yamlTypes))
	for _, src := range [][]string{dbTypes, yamlTypes} {
		for _, t := range src {
			t = strings.TrimSpace(t)
			if t == "" {
				continue
			}
			if _, ok := seen[t]; ok {
				continue
			}
			seen[t] = struct{}{}
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (r *configValueRepository) GetAllowedPrefixes(ctx context.Context) (string, error) {
	value, err := r.GetConfig(ctx, "allowed_prefixes")
	if err != nil {
		return "", fmt.Errorf("db: GetAllowedPrefixes: %w", err)
	}
	return value, nil
}

func (r *configValueRepository) GetAdaptiveIDConfig(ctx context.Context) (domain.AdaptiveIDConfig, error) {
	cfg := domain.DefaultAdaptiveConfig()

	probStr, err := r.adaptiveConfigValue(ctx, "max_collision_prob")
	if err != nil {
		return cfg, err
	}
	applyAdaptiveProbability(&cfg, probStr)

	minStr, err := r.adaptiveConfigValue(ctx, "min_hash_length")
	if err != nil {
		return cfg, err
	}
	applyAdaptiveMinLength(&cfg, minStr)

	maxStr, err := r.adaptiveConfigValue(ctx, "max_hash_length")
	if err != nil {
		return cfg, err
	}
	applyAdaptiveMaxLength(&cfg, maxStr)

	return cfg, nil
}

func (r *configValueRepository) adaptiveConfigValue(ctx context.Context, key string) (string, error) {
	value, err := r.GetConfig(ctx, key)
	if err != nil {
		return "", fmt.Errorf("db: GetAdaptiveIDConfig: read %s: %w", key, err)
	}
	return value, nil
}

func applyAdaptiveProbability(cfg *domain.AdaptiveIDConfig, value string) {
	if value == "" {
		return
	}
	if probability, err := strconv.ParseFloat(value, 64); err == nil {
		cfg.MaxCollisionProbability = probability
	}
}

func applyAdaptiveMinLength(cfg *domain.AdaptiveIDConfig, value string) {
	if value == "" {
		return
	}
	if length, err := strconv.Atoi(value); err == nil {
		cfg.MinLength = length
	}
}

func applyAdaptiveMaxLength(cfg *domain.AdaptiveIDConfig, value string) {
	if value == "" {
		return
	}
	if length, err := strconv.Atoi(value); err == nil {
		cfg.MaxLength = length
	}
}

func (r *configStatusRepository) GetCustomStatuses(ctx context.Context) ([]types.CustomStatus, error) {
	return issueops.ResolveCustomStatusesDetailedInTx(ctx, r.runner)
}

func (r *configStatusRepository) ListAllStatusNames(ctx context.Context) ([]string, error) {
	builtins := []types.Status{
		types.StatusOpen, types.StatusInProgress, types.StatusBlocked,
		types.StatusDeferred, types.StatusClosed, types.StatusPinned, types.StatusHooked,
	}
	custom, err := r.GetCustomStatuses(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(builtins)+len(custom))
	for _, s := range builtins {
		out = append(out, string(s))
	}
	for _, c := range custom {
		out = append(out, c.Name)
	}
	return out, nil
}

func (r *configValueRepository) GetInfraTypes(ctx context.Context) (map[string]bool, error) {
	value, err := r.GetConfig(ctx, "types.infra")
	if err != nil {
		return nil, fmt.Errorf("db: GetInfraTypes: %w", err)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return map[string]bool{}, nil
	}
	parts := strings.Split(value, ",")
	result := make(map[string]bool, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result[p] = true
		}
	}
	return result, nil
}
