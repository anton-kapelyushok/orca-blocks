package storage

import (
	"context"
	"sync"
)

type MemRepo struct {
	mu        sync.Mutex
	volumes   map[string]Volume
	snapshots map[string]Snapshot
}

func NewMemRepo() *MemRepo {
	return &MemRepo{
		volumes:   map[string]Volume{},
		snapshots: map[string]Snapshot{},
	}
}

func (r *MemRepo) Init(context.Context) error { return nil }

func (r *MemRepo) CreateVolume(_ context.Context, volume Volume) (Volume, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if volume.SizeBytes == 0 {
		volume.SizeBytes = DefaultSizeBytes
	}
	if volume.ChunkSize == 0 {
		volume.ChunkSize = DefaultChunkSize
	}
	r.volumes[volume.VolumeID] = volume
	return volume, nil
}

func (r *MemRepo) GetVolume(_ context.Context, volumeID string) (Volume, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.volumes[volumeID]
	if !ok {
		return Volume{}, ErrNotFound
	}
	return v, nil
}

func (r *MemRepo) UpdateLatestSnapshot(_ context.Context, volumeID, snapshotID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.volumes[volumeID]
	if !ok {
		return ErrNotFound
	}
	v.LatestSnapshotID = snapshotID
	r.volumes[volumeID] = v
	return nil
}

func (r *MemRepo) UpdateLastNode(_ context.Context, volumeID, nodeID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.volumes[volumeID]
	if !ok {
		return ErrNotFound
	}
	v.LastNode = nodeID
	r.volumes[volumeID] = v
	return nil
}

func (r *MemRepo) CreateSnapshot(_ context.Context, snapshot Snapshot) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.snapshots[snapshot.SnapshotID] = snapshot
	return nil
}

func (r *MemRepo) GetSnapshot(_ context.Context, snapshotID string) (Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.snapshots[snapshotID]
	if !ok {
		return Snapshot{}, ErrNotFound
	}
	return s, nil
}

type MemObjectStore struct {
	bucket string
	mu     sync.Mutex
	data   map[string][]byte
}

func NewMemObjectStore(bucket string) *MemObjectStore {
	return &MemObjectStore{bucket: bucket, data: map[string][]byte{}}
}

func (s *MemObjectStore) Bucket() string { return s.bucket }

func (s *MemObjectStore) Put(_ context.Context, key string, b []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := append([]byte(nil), b...)
	s.data[key] = cp
	return nil
}

func (s *MemObjectStore) Get(_ context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.data[key]
	if !ok {
		return nil, ErrNotFound
	}
	return append([]byte(nil), b...), nil
}

func (s *MemObjectStore) Exists(_ context.Context, key string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.data[key]
	return ok, nil
}
