package storage

import (
	"context"
	"encoding/json"
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
);

CREATE TABLE IF NOT EXISTS base_images (
  base_image_id TEXT PRIMARY KEY,
  image_ref TEXT NOT NULL,
  image_digest TEXT NOT NULL,
  volume_id TEXT NOT NULL REFERENCES volumes(volume_id),
  snapshot_id TEXT NOT NULL REFERENCES snapshots(snapshot_id),
  rootfs_size_bytes BIGINT NOT NULL,
  env_json JSONB NOT NULL DEFAULT '[]'::jsonb,
  working_dir TEXT NOT NULL DEFAULT '',
  entrypoint_json JSONB NOT NULL DEFAULT '[]'::jsonb,
  cmd_json JSONB NOT NULL DEFAULT '[]'::jsonb,
  user_name TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE base_images ADD COLUMN IF NOT EXISTS env_json JSONB NOT NULL DEFAULT '[]'::jsonb;
ALTER TABLE base_images ADD COLUMN IF NOT EXISTS working_dir TEXT NOT NULL DEFAULT '';
ALTER TABLE base_images ADD COLUMN IF NOT EXISTS entrypoint_json JSONB NOT NULL DEFAULT '[]'::jsonb;
ALTER TABLE base_images ADD COLUMN IF NOT EXISTS cmd_json JSONB NOT NULL DEFAULT '[]'::jsonb;
ALTER TABLE base_images ADD COLUMN IF NOT EXISTS user_name TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS base_images_image_ref_idx ON base_images(image_ref);
CREATE INDEX IF NOT EXISTS base_images_image_digest_idx ON base_images(image_digest);

CREATE TABLE IF NOT EXISTS envs (
  env_id TEXT PRIMARY KEY,
  base_image_id TEXT NOT NULL REFERENCES base_images(base_image_id),
  image_ref TEXT NOT NULL,
  volume_id TEXT NOT NULL REFERENCES volumes(volume_id),
  latest_snapshot_id TEXT NOT NULL REFERENCES snapshots(snapshot_id),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS envs_base_image_id_idx ON envs(base_image_id);
`)
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
INSERT INTO volumes (volume_id, size_bytes, chunk_size, latest_snapshot_id, last_node)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (volume_id) DO NOTHING
RETURNING volume_id, size_bytes, chunk_size, latest_snapshot_id, last_node`,
		volume.VolumeID, volume.SizeBytes, volume.ChunkSize, volume.LatestSnapshotID, volume.LastNode)
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

func (r *PostgresRepo) CreateBaseImage(ctx context.Context, image BaseImage) (BaseImage, error) {
	envJSON, err := json.Marshal(image.Env)
	if err != nil {
		return BaseImage{}, err
	}
	entrypointJSON, err := json.Marshal(image.Entrypoint)
	if err != nil {
		return BaseImage{}, err
	}
	cmdJSON, err := json.Marshal(image.Cmd)
	if err != nil {
		return BaseImage{}, err
	}
	row := r.pool.QueryRow(ctx, `
INSERT INTO base_images (base_image_id, image_ref, image_digest, volume_id, snapshot_id, rootfs_size_bytes, env_json, working_dir, entrypoint_json, cmd_json, user_name)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (base_image_id) DO NOTHING
RETURNING base_image_id, image_ref, image_digest, volume_id, snapshot_id, rootfs_size_bytes, env_json, working_dir, entrypoint_json, cmd_json, user_name`,
		image.BaseImageID, image.ImageRef, image.ImageDigest, image.VolumeID, image.SnapshotID, image.RootFSSizeBytes, envJSON, image.WorkingDir, entrypointJSON, cmdJSON, image.User)
	out, err := scanBaseImage(row)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return r.GetBaseImage(ctx, image.BaseImageID)
		}
		return BaseImage{}, err
	}
	return out, nil
}

func (r *PostgresRepo) GetBaseImage(ctx context.Context, baseImageID string) (BaseImage, error) {
	row := r.pool.QueryRow(ctx, `
SELECT base_image_id, image_ref, image_digest, volume_id, snapshot_id, rootfs_size_bytes, env_json, working_dir, entrypoint_json, cmd_json, user_name
FROM base_images WHERE base_image_id = $1`, baseImageID)
	return scanBaseImage(row)
}

func (r *PostgresRepo) GetBaseImageByRef(ctx context.Context, imageRef string) (BaseImage, error) {
	row := r.pool.QueryRow(ctx, `
SELECT base_image_id, image_ref, image_digest, volume_id, snapshot_id, rootfs_size_bytes, env_json, working_dir, entrypoint_json, cmd_json, user_name
FROM base_images WHERE image_ref = $1
ORDER BY created_at DESC
LIMIT 1`, imageRef)
	return scanBaseImage(row)
}

func (r *PostgresRepo) ListBaseImages(ctx context.Context) ([]BaseImage, error) {
	rows, err := r.pool.Query(ctx, `
SELECT base_image_id, image_ref, image_digest, volume_id, snapshot_id, rootfs_size_bytes, env_json, working_dir, entrypoint_json, cmd_json, user_name
FROM base_images
ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BaseImage
	for rows.Next() {
		image, err := scanBaseImage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, image)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanBaseImage(row scanner) (BaseImage, error) {
	var out BaseImage
	var envJSON, entrypointJSON, cmdJSON []byte
	if err := row.Scan(&out.BaseImageID, &out.ImageRef, &out.ImageDigest, &out.VolumeID, &out.SnapshotID, &out.RootFSSizeBytes, &envJSON, &out.WorkingDir, &entrypointJSON, &cmdJSON, &out.User); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return BaseImage{}, ErrNotFound
		}
		return BaseImage{}, err
	}
	if len(envJSON) > 0 {
		_ = json.Unmarshal(envJSON, &out.Env)
	}
	if len(entrypointJSON) > 0 {
		_ = json.Unmarshal(entrypointJSON, &out.Entrypoint)
	}
	if len(cmdJSON) > 0 {
		_ = json.Unmarshal(cmdJSON, &out.Cmd)
	}
	return out, nil
}

func (r *PostgresRepo) CreateEnv(ctx context.Context, env Env) (Env, error) {
	row := r.pool.QueryRow(ctx, `
INSERT INTO envs (env_id, base_image_id, image_ref, volume_id, latest_snapshot_id)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (env_id) DO NOTHING
RETURNING env_id, base_image_id, image_ref, volume_id, latest_snapshot_id`,
		env.EnvID, env.BaseImageID, env.ImageRef, env.VolumeID, env.LatestSnapshotID)
	var out Env
	if err := row.Scan(&out.EnvID, &out.BaseImageID, &out.ImageRef, &out.VolumeID, &out.LatestSnapshotID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return r.GetEnv(ctx, env.EnvID)
		}
		return Env{}, err
	}
	return out, nil
}

func (r *PostgresRepo) GetEnv(ctx context.Context, envID string) (Env, error) {
	row := r.pool.QueryRow(ctx, `
SELECT env_id, base_image_id, image_ref, volume_id, latest_snapshot_id
FROM envs WHERE env_id = $1`, envID)
	var out Env
	if err := row.Scan(&out.EnvID, &out.BaseImageID, &out.ImageRef, &out.VolumeID, &out.LatestSnapshotID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Env{}, ErrNotFound
		}
		return Env{}, err
	}
	return out, nil
}

func (r *PostgresRepo) ListEnvs(ctx context.Context) ([]Env, error) {
	rows, err := r.pool.Query(ctx, `
SELECT env_id, base_image_id, image_ref, volume_id, latest_snapshot_id
FROM envs
ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Env
	for rows.Next() {
		var env Env
		if err := rows.Scan(&env.EnvID, &env.BaseImageID, &env.ImageRef, &env.VolumeID, &env.LatestSnapshotID); err != nil {
			return nil, err
		}
		out = append(out, env)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *PostgresRepo) UpdateEnvSnapshot(ctx context.Context, envID, snapshotID string) error {
	tag, err := r.pool.Exec(ctx, `
UPDATE envs
SET latest_snapshot_id = $2, updated_at = now()
WHERE env_id = $1`, envID, snapshotID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
