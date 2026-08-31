package db

import (
	"context"
	"fmt"

	"github.com/jonbaldie/beads/internal/storage/domain"
)

func NewRemoteSQLRepository(runner Runner) domain.RemoteSQLRepository {
	commands := &remoteCommandRepository{vc: NewDoltVersionControlSQLRepository(runner)}
	listing := &remoteListRepository{runner: runner}
	repo := &remoteSQLRepositoryImpl{
		remoteCommandRepository: commands,
		remoteListRepository:    listing,
	}
	_ = repo.remoteCommandRepository
	_ = repo.remoteListRepository
	return repo
}

type remoteSQLRepositoryImpl struct {
	*remoteCommandRepository
	*remoteListRepository
}

type remoteCommandRepository struct {
	vc DoltVersionControlSQLRepository
}

type remoteListRepository struct {
	runner Runner
}

var _ domain.RemoteSQLRepository = (*remoteSQLRepositoryImpl)(nil)

func (r *remoteCommandRepository) AddRemote(ctx context.Context, name, url string) error {
	if err := r.vc.Remote(ctx, "add", name, url); err != nil {
		return fmt.Errorf("db: AddRemote %s: %w", name, err)
	}
	return nil
}

func (r *remoteCommandRepository) RemoveRemote(ctx context.Context, name string) error {
	if err := r.vc.Remote(ctx, "remove", name); err != nil {
		return fmt.Errorf("db: RemoveRemote %s: %w", name, err)
	}
	return nil
}

func (r *remoteListRepository) ListRemotes(ctx context.Context) ([]domain.Remote, error) {
	rows, err := r.runner.QueryContext(ctx, "SELECT name, url FROM dolt_remotes")
	if err != nil {
		return nil, fmt.Errorf("db: ListRemotes: query: %w", err)
	}
	defer rows.Close()

	var remotes []domain.Remote
	for rows.Next() {
		var rem domain.Remote
		if err := rows.Scan(&rem.Name, &rem.URL); err != nil {
			return nil, fmt.Errorf("db: ListRemotes: scan: %w", err)
		}
		remotes = append(remotes, rem)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: ListRemotes: rows: %w", err)
	}
	return remotes, nil
}
