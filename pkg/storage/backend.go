package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"

	"github.com/google/uuid"
)

type Backend struct {
	NodeID string
	Repo   Repository
	Store  ObjectStore
	Cache  *LocalCache

	mu       sync.Mutex
	sessions map[string]*Session
}

type CommitOptions struct {
	UploadBatchChunks int
	OnProgress        func(CommitProgress)
}

type CommitProgress struct {
	Phase         string
	DoneChunks    int
	TotalChunks   int
	Uploaded      int
	Skipped       int
	Bytes         int64
	SnapshotID    string
	ManifestKey   string
	ManifestItems int
}

type Session struct {
	ID             string
	Volume         Volume
	BaseSnapshotID string
	BaseManifest   Manifest
	Dirty          map[int64]struct{}
	DirtyDir       string
	Stats          SessionStats
}

type SessionStats struct {
	CacheHits     int64 `json:"cache_hits"`
	CacheMisses   int64 `json:"cache_misses"`
	RemoteFetches int64 `json:"remote_fetches"`
	ZeroFills     int64 `json:"zero_fills"`
	DirtyChunks   int   `json:"dirty_chunks"`
}

func NewBackend(nodeID string, repo Repository, store ObjectStore, cache *LocalCache) *Backend {
	return &Backend{
		NodeID:   nodeID,
		Repo:     repo,
		Store:    store,
		Cache:    cache,
		sessions: map[string]*Session{},
	}
}

func (b *Backend) CreateVolume(ctx context.Context, id string, sizeBytes, chunkSize int64) (Volume, error) {
	if id == "" {
		id = uuid.NewString()
	}
	return b.Repo.CreateVolume(ctx, Volume{
		VolumeID:  id,
		SizeBytes: sizeBytes,
		ChunkSize: chunkSize,
	})
}

func (b *Backend) StartSession(ctx context.Context, volumeID string) (*Session, error) {
	volume, err := b.Repo.GetVolume(ctx, volumeID)
	if err != nil {
		return nil, err
	}
	manifest := Manifest{}
	if volume.LatestSnapshotID != "" {
		snapshot, err := b.Repo.GetSnapshot(ctx, volume.LatestSnapshotID)
		if err != nil {
			return nil, err
		}
		manifest, err = b.loadManifest(ctx, snapshot.ManifestKey)
		if err != nil {
			return nil, err
		}
	}

	sessionID := uuid.NewString()
	dirtyDir := filepath.Join(b.Cache.dir, "dirty-sessions", sessionID)
	if err := os.MkdirAll(dirtyDir, 0o755); err != nil {
		return nil, err
	}

	session := &Session{
		ID:             sessionID,
		Volume:         volume,
		BaseSnapshotID: volume.LatestSnapshotID,
		BaseManifest:   manifest,
		Dirty:          map[int64]struct{}{},
		DirtyDir:       dirtyDir,
	}
	b.mu.Lock()
	b.sessions[session.ID] = session
	b.mu.Unlock()
	if err := b.Repo.UpdateLastNode(ctx, volumeID, b.NodeID); err != nil {
		b.mu.Lock()
		delete(b.sessions, session.ID)
		b.mu.Unlock()
		_ = os.RemoveAll(dirtyDir)
		return nil, err
	}
	return session, nil
}

func (b *Backend) Read(ctx context.Context, sessionID string, offset, length int64) ([]byte, error) {
	session, err := b.session(sessionID)
	if err != nil {
		return nil, err
	}
	if offset+length > session.Volume.SizeBytes {
		return nil, fmt.Errorf("read exceeds volume size")
	}
	ranges, err := ChunkIndexes(offset, length, session.Volume.ChunkSize)
	if err != nil {
		return nil, err
	}
	out := make([]byte, length)
	var outPos int64
	for _, r := range ranges {
		partLen := int(r.ReqEnd - r.ReqStart)
		part, err := b.resolveChunkRange(ctx, session, r.Index, int64(r.ChunkStart), partLen)
		if err != nil {
			return nil, err
		}
		copy(out[outPos:outPos+int64(partLen)], part)
		outPos += int64(partLen)
	}
	return out, nil
}

func (b *Backend) Write(ctx context.Context, sessionID string, offset int64, data []byte) error {
	session, err := b.session(sessionID)
	if err != nil {
		return err
	}
	if offset < 0 || offset+int64(len(data)) > session.Volume.SizeBytes {
		return fmt.Errorf("write exceeds volume size")
	}
	ranges, err := ChunkIndexes(offset, int64(len(data)), session.Volume.ChunkSize)
	if err != nil {
		return err
	}
	var dataPos int64
	for _, r := range ranges {
		var chunk []byte
		if _, ok := session.Dirty[r.Index]; ok {
			chunk, err = session.readDirtyChunk(r.Index, session.Volume.ChunkSize)
			if err != nil {
				return err
			}
		} else {
			chunk, err = b.resolveChunk(ctx, session, r.Index)
			if err != nil {
				return err
			}
			chunk = append([]byte(nil), chunk...)
		}
		partLen := r.ReqEnd - r.ReqStart
		copy(chunk[r.ChunkStart:r.ChunkStart+int(partLen)], data[dataPos:dataPos+partLen])
		if err := session.writeDirtyChunk(r.Index, chunk); err != nil {
			return err
		}
		session.Dirty[r.Index] = struct{}{}
		dataPos += partLen
	}
	return nil
}

func (b *Backend) Commit(ctx context.Context, sessionID string) (Snapshot, error) {
	return b.CommitWithOptions(ctx, sessionID, CommitOptions{})
}

func (b *Backend) CommitWithOptions(ctx context.Context, sessionID string, opts CommitOptions) (Snapshot, error) {
	session, err := b.session(sessionID)
	if err != nil {
		return Snapshot{}, err
	}
	batchSize := opts.UploadBatchChunks
	if batchSize <= 0 {
		batchSize = 16
	}

	manifest := CopyManifest(session.BaseManifest)
	dirtyIndexes := make([]int64, 0, len(session.Dirty))
	for idx := range session.Dirty {
		dirtyIndexes = append(dirtyIndexes, idx)
	}
	sort.Slice(dirtyIndexes, func(i, j int) bool { return dirtyIndexes[i] < dirtyIndexes[j] })
	progress := func(p CommitProgress) {
		if opts.OnProgress != nil {
			opts.OnProgress(p)
		}
	}
	progress(CommitProgress{Phase: "start", TotalChunks: len(dirtyIndexes), ManifestItems: len(manifest)})
	doneChunks := 0
	uploadedChunks := 0
	skippedChunks := 0
	var committedBytes int64
	for start := 0; start < len(dirtyIndexes); start += batchSize {
		end := start + batchSize
		if end > len(dirtyIndexes) {
			end = len(dirtyIndexes)
		}
		stats, err := b.commitDirtyBatch(ctx, session, dirtyIndexes[start:end], manifest)
		if err != nil {
			return Snapshot{}, err
		}
		doneChunks += stats.chunks
		uploadedChunks += stats.uploaded
		skippedChunks += stats.skipped
		committedBytes += stats.bytes
		progress(CommitProgress{
			Phase:         "chunks",
			DoneChunks:    doneChunks,
			TotalChunks:   len(dirtyIndexes),
			Uploaded:      uploadedChunks,
			Skipped:       skippedChunks,
			Bytes:         committedBytes,
			ManifestItems: len(manifest),
		})
	}

	snapshotID := uuid.NewString()
	manifestKey := manifestKey(snapshotID)
	progress(CommitProgress{Phase: "save_manifest", DoneChunks: doneChunks, TotalChunks: len(dirtyIndexes), Uploaded: uploadedChunks, Skipped: skippedChunks, Bytes: committedBytes, SnapshotID: snapshotID, ManifestKey: manifestKey, ManifestItems: len(manifest)})
	if err := b.saveManifest(ctx, manifestKey, manifest); err != nil {
		return Snapshot{}, err
	}
	snapshot := Snapshot{SnapshotID: snapshotID, VolumeID: session.Volume.VolumeID, ManifestKey: manifestKey}
	progress(CommitProgress{Phase: "record_snapshot", DoneChunks: doneChunks, TotalChunks: len(dirtyIndexes), Uploaded: uploadedChunks, Skipped: skippedChunks, Bytes: committedBytes, SnapshotID: snapshotID, ManifestKey: manifestKey, ManifestItems: len(manifest)})
	if err := b.Repo.CreateSnapshot(ctx, snapshot); err != nil {
		return Snapshot{}, err
	}
	progress(CommitProgress{Phase: "update_volume", DoneChunks: doneChunks, TotalChunks: len(dirtyIndexes), Uploaded: uploadedChunks, Skipped: skippedChunks, Bytes: committedBytes, SnapshotID: snapshotID, ManifestKey: manifestKey, ManifestItems: len(manifest)})
	if err := b.Repo.UpdateLatestSnapshot(ctx, session.Volume.VolumeID, snapshotID); err != nil {
		return Snapshot{}, err
	}
	session.BaseSnapshotID = snapshotID
	session.BaseManifest = manifest
	progress(CommitProgress{Phase: "clear_dirty", DoneChunks: doneChunks, TotalChunks: len(dirtyIndexes), Uploaded: uploadedChunks, Skipped: skippedChunks, Bytes: committedBytes, SnapshotID: snapshotID, ManifestKey: manifestKey, ManifestItems: len(manifest)})
	if err := session.clearDirtyOverlay(); err != nil {
		return Snapshot{}, err
	}
	progress(CommitProgress{Phase: "done", DoneChunks: doneChunks, TotalChunks: len(dirtyIndexes), Uploaded: uploadedChunks, Skipped: skippedChunks, Bytes: committedBytes, SnapshotID: snapshotID, ManifestKey: manifestKey, ManifestItems: len(manifest)})
	return snapshot, nil
}

type commitBatchStats struct {
	chunks   int
	uploaded int
	skipped  int
	bytes    int64
}

func (b *Backend) commitDirtyBatch(ctx context.Context, session *Session, indexes []int64, manifest Manifest) (commitBatchStats, error) {
	type dirtyChunk struct {
		index int64
		id    string
		bytes []byte
	}

	batch := make([]dirtyChunk, 0, len(indexes))
	var stats commitBatchStats
	for _, idx := range indexes {
		chunk, err := session.readDirtyChunk(idx, session.Volume.ChunkSize)
		if err != nil {
			return stats, err
		}
		batch = append(batch, dirtyChunk{
			index: idx,
			id:    HashChunk(chunk),
			bytes: chunk,
		})
	}

	for _, item := range batch {
		key := chunkKey(item.id)
		exists, err := b.Store.Exists(ctx, key)
		if err != nil {
			return stats, err
		}
		if !exists {
			if err := b.Store.Put(ctx, key, item.bytes); err != nil {
				return stats, err
			}
			stats.uploaded++
		} else {
			stats.skipped++
		}
		if err := b.Cache.Put(item.id, item.bytes); err != nil {
			return stats, err
		}
		manifest[item.index] = item.id
		stats.chunks++
		stats.bytes += int64(len(item.bytes))
	}
	return stats, nil
}

func (b *Backend) Stop(sessionID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	session, ok := b.sessions[sessionID]
	if !ok {
		return ErrNotFound
	}
	delete(b.sessions, sessionID)
	return os.RemoveAll(session.DirtyDir)
}

func (b *Backend) Stats(sessionID string) (SessionStats, error) {
	session, err := b.session(sessionID)
	if err != nil {
		return SessionStats{}, err
	}
	stats := session.Stats
	stats.DirtyChunks = len(session.Dirty)
	return stats, nil
}

func (b *Backend) FlushDirty(sessionID string) error {
	session, err := b.session(sessionID)
	if err != nil {
		return err
	}
	return session.flushDirtyOverlay()
}

func (b *Backend) session(sessionID string) (*Session, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.sessions[sessionID]
	if !ok {
		return nil, ErrNotFound
	}
	return s, nil
}

func (b *Backend) resolveChunk(ctx context.Context, session *Session, index int64) ([]byte, error) {
	if _, ok := session.Dirty[index]; ok {
		return session.readDirtyChunk(index, session.Volume.ChunkSize)
	}
	chunkID, ok := session.BaseManifest[index]
	if !ok {
		session.Stats.ZeroFills++
		return ZeroChunk(session.Volume.ChunkSize), nil
	}
	if cached, ok, err := b.Cache.Get(chunkID); err != nil {
		return nil, err
	} else if ok {
		session.Stats.CacheHits++
		return normalizeChunk(cached, session.Volume.ChunkSize)
	}

	session.Stats.CacheMisses++
	remote, err := b.Store.Get(ctx, chunkKey(chunkID))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			session.Stats.ZeroFills++
			return ZeroChunk(session.Volume.ChunkSize), nil
		}
		return nil, err
	}
	session.Stats.RemoteFetches++
	remote, err = normalizeChunk(remote, session.Volume.ChunkSize)
	if err != nil {
		return nil, err
	}
	if err := b.Cache.Put(chunkID, remote); err != nil {
		return nil, err
	}
	return remote, nil
}

func (b *Backend) resolveChunkRange(ctx context.Context, session *Session, index, chunkOffset int64, length int) ([]byte, error) {
	if _, ok := session.Dirty[index]; ok {
		return session.readDirtyRange(index, chunkOffset, length)
	}
	chunkID, ok := session.BaseManifest[index]
	if !ok {
		session.Stats.ZeroFills++
		return make([]byte, length), nil
	}
	if cached, ok, err := b.Cache.GetRange(chunkID, chunkOffset, length); err != nil {
		return nil, err
	} else if ok {
		session.Stats.CacheHits++
		return cached, nil
	}

	session.Stats.CacheMisses++
	remote, err := b.Store.Get(ctx, chunkKey(chunkID))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			session.Stats.ZeroFills++
			return make([]byte, length), nil
		}
		return nil, err
	}
	session.Stats.RemoteFetches++
	remote, err = normalizeChunk(remote, session.Volume.ChunkSize)
	if err != nil {
		return nil, err
	}
	if err := b.Cache.Put(chunkID, remote); err != nil {
		return nil, err
	}
	return append([]byte(nil), remote[chunkOffset:chunkOffset+int64(length)]...), nil
}

func (b *Backend) loadManifest(ctx context.Context, key string) (Manifest, error) {
	raw, err := b.Store.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	var wire map[string]string
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, err
	}
	return ManifestFromWire(wire)
}

func (b *Backend) saveManifest(ctx context.Context, key string, manifest Manifest) error {
	raw, err := json.MarshalIndent(ManifestToWire(manifest), "", "  ")
	if err != nil {
		return err
	}
	return b.Store.Put(ctx, key, raw)
}

func normalizeChunk(b []byte, chunkSize int64) ([]byte, error) {
	if int64(len(b)) == chunkSize {
		return b, nil
	}
	if int64(len(b)) > chunkSize {
		return nil, fmt.Errorf("chunk length %d exceeds chunk size %d", len(b), chunkSize)
	}
	out := ZeroChunk(chunkSize)
	copy(out, b)
	return out, nil
}

func chunkKey(chunkID string) string {
	return "chunks/" + chunkID
}

func manifestKey(snapshotID string) string {
	return "manifests/" + snapshotID + ".json"
}

func (s *Session) dirtyPath(index int64) string {
	return filepath.Join(s.DirtyDir, strconv.FormatInt(index, 10))
}

func (s *Session) readDirtyChunk(index, chunkSize int64) ([]byte, error) {
	chunk, err := os.ReadFile(s.dirtyPath(index))
	if err != nil {
		return nil, err
	}
	return normalizeChunk(chunk, chunkSize)
}

func (s *Session) readDirtyRange(index, offset int64, length int) ([]byte, error) {
	file, err := os.Open(s.dirtyPath(index))
	if err != nil {
		return nil, err
	}
	defer file.Close()

	out := make([]byte, length)
	n, err := file.ReadAt(out, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if n < length {
		clear(out[n:])
	}
	return out, nil
}

func (s *Session) writeDirtyChunk(index int64, chunk []byte) error {
	return os.WriteFile(s.dirtyPath(index), chunk, 0o644)
}

func (s *Session) flushDirtyOverlay() error {
	for index := range s.Dirty {
		file, err := os.OpenFile(s.dirtyPath(index), os.O_RDONLY, 0)
		if err != nil {
			return err
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
	}
	dir, err := os.Open(s.DirtyDir)
	if err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return err
	}
	return dir.Close()
}

func (s *Session) clearDirtyOverlay() error {
	if err := os.RemoveAll(s.DirtyDir); err != nil {
		return err
	}
	if err := os.MkdirAll(s.DirtyDir, 0o755); err != nil {
		return err
	}
	s.Dirty = map[int64]struct{}{}
	return nil
}
