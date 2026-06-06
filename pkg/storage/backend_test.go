package storage

import (
	"bytes"
	"context"
	"os"
	"testing"
)

func TestReadWriteCommitAndCrossNodeCacheStats(t *testing.T) {
	ctx := context.Background()
	t.Log("creating storage: in-memory metadata repo and object store")
	repo := NewMemRepo()
	store := NewMemObjectStore("test")

	t.Log("creating node-1 local cache and backend")
	cache1, err := NewLocalCache(t.TempDir(), 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	node1 := NewBackend("node-1", repo, store, cache1)
	t.Log("creating volume vol-1 with 8-byte chunks")
	volume, err := node1.CreateVolume(ctx, "vol-1", 128, 8)
	if err != nil {
		t.Fatal(err)
	}
	if volume.ChunkSize != 8 {
		t.Fatalf("chunk size = %d", volume.ChunkSize)
	}

	t.Log("creating session on node-1")
	session, err := node1.StartSession(ctx, "vol-1")
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("hello-world")
	t.Logf("writing %d bytes at offset 6, spanning chunk boundaries", len(data))
	if err := node1.Write(ctx, session.ID, 6, data); err != nil {
		t.Fatal(err)
	}
	t.Log("reading dirty overlay before commit")
	readBeforeCommit, err := node1.Read(ctx, session.ID, 6, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readBeforeCommit, data) {
		t.Fatalf("read before commit = %q, want %q", readBeforeCommit, data)
	}
	t.Log("committing dirty chunks to immutable object store and retaining them in node-1 cache")
	if _, err := node1.Commit(ctx, session.ID); err != nil {
		t.Fatal(err)
	}

	t.Log("resuming volume on node-1, expecting local cache hits")
	node1Resume, err := node1.StartSession(ctx, "vol-1")
	if err != nil {
		t.Fatal(err)
	}
	readNode1, err := node1.Read(ctx, node1Resume.ID, 6, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readNode1, data) {
		t.Fatalf("node-1 read = %q, want %q", readNode1, data)
	}
	node1Stats, err := node1.Stats(node1Resume.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("node-1 got hits=%d misses=%d remote_fetches=%d zero_fills=%d", node1Stats.CacheHits, node1Stats.CacheMisses, node1Stats.RemoteFetches, node1Stats.ZeroFills)
	if node1Stats.CacheHits == 0 {
		t.Fatalf("expected node-1 cache hits, got %+v", node1Stats)
	}

	t.Log("creating node-2 with an empty local cache")
	cache2, err := NewLocalCache(t.TempDir(), 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	node2 := NewBackend("node-2", repo, store, cache2)
	t.Log("creating session on node-2, expecting cache misses followed by remote fetches")
	node2Session, err := node2.StartSession(ctx, "vol-1")
	if err != nil {
		t.Fatal(err)
	}
	readNode2, err := node2.Read(ctx, node2Session.ID, 6, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readNode2, data) {
		t.Fatalf("node-2 read = %q, want %q", readNode2, data)
	}
	node2Stats, err := node2.Stats(node2Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("node-2 got hits=%d misses=%d remote_fetches=%d zero_fills=%d", node2Stats.CacheHits, node2Stats.CacheMisses, node2Stats.RemoteFetches, node2Stats.ZeroFills)
	if node2Stats.CacheMisses == 0 || node2Stats.RemoteFetches == 0 {
		t.Fatalf("expected node-2 misses and remote fetches, got %+v", node2Stats)
	}
}

func TestMissingChunksReadAsZeroes(t *testing.T) {
	ctx := context.Background()
	t.Log("creating storage with no committed chunks")
	repo := NewMemRepo()
	store := NewMemObjectStore("test")
	cache, err := NewLocalCache(t.TempDir(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	backend := NewBackend("node-1", repo, store, cache)
	t.Log("creating empty volume vol-zero")
	if _, err := backend.CreateVolume(ctx, "vol-zero", 64, 8); err != nil {
		t.Fatal(err)
	}
	t.Log("creating session on empty volume")
	session, err := backend.StartSession(ctx, "vol-zero")
	if err != nil {
		t.Fatal(err)
	}
	t.Log("reading a range that spans two missing chunks; expecting zero fills")
	got, err := backend.Read(ctx, session.ID, 4, 12)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, make([]byte, 12)) {
		t.Fatalf("expected zero-filled read, got %v", got)
	}
	stats, err := backend.Stats(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("got zero_fills=%d hits=%d misses=%d remote_fetches=%d", stats.ZeroFills, stats.CacheHits, stats.CacheMisses, stats.RemoteFetches)
	if stats.ZeroFills != 2 {
		t.Fatalf("zero fills = %d, want 2", stats.ZeroFills)
	}
}

func TestWritePersistsDirtyChunksToDiskUntilCommit(t *testing.T) {
	ctx := context.Background()
	t.Log("creating storage for disk-backed dirty overlay test")
	repo := NewMemRepo()
	store := NewMemObjectStore("test")
	cache, err := NewLocalCache(t.TempDir(), 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	backend := NewBackend("node-1", repo, store, cache)
	t.Log("creating volume with 8-byte chunks")
	if _, err := backend.CreateVolume(ctx, "vol-dirty-disk", 128, 8); err != nil {
		t.Fatal(err)
	}
	t.Log("creating session with a dedicated dirty overlay directory")
	session, err := backend.StartSession(ctx, "vol-dirty-disk")
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("writing 3 bytes at offset 2; dirty chunk should be materialized on disk at %s", session.dirtyPath(0))
	if err := backend.Write(ctx, session.ID, 2, []byte("abc")); err != nil {
		t.Fatal(err)
	}
	stats, err := backend.Stats(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("got dirty_chunks=%d before commit", stats.DirtyChunks)
	if stats.DirtyChunks != 1 {
		t.Fatalf("dirty chunks = %d, want 1", stats.DirtyChunks)
	}

	dirtyChunk, err := os.ReadFile(session.dirtyPath(0))
	if err != nil {
		t.Fatal(err)
	}
	wantDirtyChunk := []byte{0, 0, 'a', 'b', 'c', 0, 0, 0}
	if !bytes.Equal(dirtyChunk, wantDirtyChunk) {
		t.Fatalf("dirty chunk on disk = %v, want %v", dirtyChunk, wantDirtyChunk)
	}
	t.Logf("dirty chunk exists on disk with %d bytes", len(dirtyChunk))

	t.Log("committing session; dirty overlay files should be cleared after snapshot commit")
	if _, err := backend.Commit(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(session.dirtyPath(0)); !os.IsNotExist(err) {
		t.Fatalf("expected dirty chunk file to be removed after commit, stat err=%v", err)
	}
	stats, err = backend.Stats(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("got dirty_chunks=%d after commit", stats.DirtyChunks)
	if stats.DirtyChunks != 0 {
		t.Fatalf("dirty chunks after commit = %d, want 0", stats.DirtyChunks)
	}
}

func TestReadUsesS3WhenOneChunkMissingFromLocalCache(t *testing.T) {
	ctx := context.Background()
	t.Log("creating storage: shared metadata repo and S3-like object store")
	repo := NewMemRepo()
	store := NewMemObjectStore("test")

	t.Log("creating source node with its own cache")
	sourceCache, err := NewLocalCache(t.TempDir(), 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	sourceNode := NewBackend("node-1", repo, store, sourceCache)
	t.Log("creating volume vol-partial-cache")
	if _, err := sourceNode.CreateVolume(ctx, "vol-partial-cache", 128, 8); err != nil {
		t.Fatal(err)
	}
	t.Log("creating source session on node-1")
	sourceSession, err := sourceNode.StartSession(ctx, "vol-partial-cache")
	if err != nil {
		t.Fatal(err)
	}

	firstChunk := []byte("abcdefgh")
	secondChunk := []byte("ijklmnop")
	data := append(append([]byte(nil), firstChunk...), secondChunk...)
	t.Log("writing two chunks on node-1")
	if err := sourceNode.Write(ctx, sourceSession.ID, 0, data); err != nil {
		t.Fatal(err)
	}
	t.Log("committing both chunks to S3-like store")
	if _, err := sourceNode.Commit(ctx, sourceSession.ID); err != nil {
		t.Fatal(err)
	}

	t.Log("creating reader node cache with only the first chunk preloaded")
	partialCache, err := NewLocalCache(t.TempDir(), 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("preloading first chunk into local cache: chunk_id=%s", HashChunk(firstChunk))
	if err := partialCache.Put(HashChunk(firstChunk), firstChunk); err != nil {
		t.Fatal(err)
	}
	readerNode := NewBackend("node-2", repo, store, partialCache)
	t.Log("creating reader session on node-2")
	readerSession, err := readerNode.StartSession(ctx, "vol-partial-cache")
	if err != nil {
		t.Fatal(err)
	}

	t.Log("reading two chunks: first should hit cache, second should miss cache and fetch remote")
	got, err := readerNode.Read(ctx, readerSession.ID, 0, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("read = %q, want %q", got, data)
	}

	stats, err := readerNode.Stats(readerSession.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("got hits=%d misses=%d remote_fetches=%d zero_fills=%d", stats.CacheHits, stats.CacheMisses, stats.RemoteFetches, stats.ZeroFills)
	if stats.CacheHits != 1 || stats.CacheMisses != 1 || stats.RemoteFetches != 1 {
		t.Fatalf("expected one cache hit and one S3 fetch, got %+v", stats)
	}
	t.Logf("verifying remote-fetched second chunk is now saved locally: chunk_id=%s", HashChunk(secondChunk))
	if _, ok, err := partialCache.Get(HashChunk(secondChunk)); err != nil || !ok {
		t.Fatalf("expected remote-fetched chunk to be saved locally, ok=%v err=%v", ok, err)
	}
}

func TestCacheLRUEviction(t *testing.T) {
	t.Log("creating tiny local cache with 12-byte max")
	cache, err := NewLocalCache(t.TempDir(), 12)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("writing first 8-byte chunk")
	if err := cache.Put("a", []byte("12345678")); err != nil {
		t.Fatal(err)
	}
	t.Log("touching first chunk so LRU metadata is updated")
	if _, ok, err := cache.Get("a"); err != nil || !ok {
		t.Fatalf("expected chunk a before eviction, ok=%v err=%v", ok, err)
	}
	t.Log("writing second 8-byte chunk; cache should evict to remain under max")
	if err := cache.Put("b", []byte("abcdefgh")); err != nil {
		t.Fatal(err)
	}
	stats, err := cache.Stats()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("got cache bytes=%d max_bytes=%d evictions=%d", stats.Bytes, stats.MaxBytes, stats.Evictions)
	if stats.Bytes > 12 || stats.Evictions == 0 {
		t.Fatalf("expected eviction under max size, got %+v", stats)
	}
}
