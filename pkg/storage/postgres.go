package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresRepo struct {
	pool *pgxpool.Pool
}

func NewPostgresRepo(ctx context.Context, databaseURL string) (*PostgresRepo, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &PostgresRepo{pool: pool}, nil
}

func (r *PostgresRepo) Close() {
	r.pool.Close()
}

func (r *PostgresRepo) Init(ctx context.Context) error {
	if _, err := r.pool.Exec(ctx, `SELECT pg_advisory_lock(42424242)`); err != nil {
		return err
	}
	defer func() {
		_, _ = r.pool.Exec(context.Background(), `SELECT pg_advisory_unlock(42424242)`)
	}()
	_, err := r.pool.Exec(ctx, `
CREATE TABLE IF NOT EXISTS volumes (
  volume_id TEXT PRIMARY KEY,
  size_bytes BIGINT NOT NULL,
  chunk_size BIGINT NOT NULL,
  latest_snapshot_id TEXT NOT NULL DEFAULT '',
  last_node TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS snapshots (
  snapshot_id TEXT PRIMARY KEY,
  volume_id TEXT NOT NULL REFERENCES volumes(volume_id),
  manifest_key TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);`)
	return err
}

func (r *PostgresRepo) CreateVolume(ctx context.Context, volume Volume) (Volume, error) {
	if volume.SizeBytes == 0 {
		volume.SizeBytes = DefaultSizeBytes
	}
	if volume.ChunkSize == 0 {
		volume.ChunkSize = DefaultChunkSize
	}
	row := r.pool.QueryRow(ctx, `
INSERT INTO volumes (volume_id, size_bytes, chunk_size)
VALUES ($1, $2, $3)
ON CONFLICT (volume_id) DO NOTHING
RETURNING volume_id, size_bytes, chunk_size, latest_snapshot_id, last_node`,
		volume.VolumeID, volume.SizeBytes, volume.ChunkSize)
	var out Volume
	if err := row.Scan(&out.VolumeID, &out.SizeBytes, &out.ChunkSize, &out.LatestSnapshotID, &out.LastNode); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return r.GetVolume(ctx, volume.VolumeID)
		}
		return Volume{}, err
	}
	return out, nil
}

func (r *PostgresRepo) GetVolume(ctx context.Context, volumeID string) (Volume, error) {
	row := r.pool.QueryRow(ctx, `
SELECT volume_id, size_bytes, chunk_size, latest_snapshot_id, last_node
FROM volumes WHERE volume_id = $1`, volumeID)
	var out Volume
	if err := row.Scan(&out.VolumeID, &out.SizeBytes, &out.ChunkSize, &out.LatestSnapshotID, &out.LastNode); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Volume{}, ErrNotFound
		}
		return Volume{}, err
	}
	return out, nil
}

func (r *PostgresRepo) UpdateLatestSnapshot(ctx context.Context, volumeID, snapshotID string) error {
	tag, err := r.pool.Exec(ctx, `UPDATE volumes SET latest_snapshot_id = $2 WHERE volume_id = $1`, volumeID, snapshotID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *PostgresRepo) UpdateLastNode(ctx context.Context, volumeID, nodeID string) error {
	tag, err := r.pool.Exec(ctx, `UPDATE volumes SET last_node = $2 WHERE volume_id = $1`, volumeID, nodeID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *PostgresRepo) CreateSnapshot(ctx context.Context, snapshot Snapshot) error {
	_, err := r.pool.Exec(ctx, `
INSERT INTO snapshots (snapshot_id, volume_id, manifest_key)
VALUES ($1, $2, $3)`, snapshot.SnapshotID, snapshot.VolumeID, snapshot.ManifestKey)
	if err != nil {
		return fmt.Errorf("insert snapshot: %w", err)
	}
	return nil
}

func (r *PostgresRepo) GetSnapshot(ctx context.Context, snapshotID string) (Snapshot, error) {
	row := r.pool.QueryRow(ctx, `
SELECT snapshot_id, volume_id, manifest_key FROM snapshots WHERE snapshot_id = $1`, snapshotID)
	var out Snapshot
	if err := row.Scan(&out.SnapshotID, &out.VolumeID, &out.ManifestKey); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Snapshot{}, ErrNotFound
		}
		return Snapshot{}, err
	}
	return out, nil
}
