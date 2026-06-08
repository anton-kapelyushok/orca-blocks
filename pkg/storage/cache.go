package storage

import (
	"container/list"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type CacheStats struct {
	Hits            int64 `json:"hits"`
	Misses          int64 `json:"misses"`
	Evictions       int64 `json:"evictions"`
	Bytes           int64 `json:"bytes"`
	MaxBytes        int64 `json:"max_bytes"`
	MemoryBytes     int64 `json:"memory_bytes"`
	MemoryMaxBytes  int64 `json:"memory_max_bytes"`
	MemoryEvictions int64 `json:"memory_evictions"`
}

type LocalCache struct {
	dir         string
	maxSize     int64
	mu          sync.Mutex
	evicted     int64
	memoryMax   int64
	memoryBytes int64
	memoryEvict int64
	memoryList  *list.List
	memoryItems map[string]*list.Element
}

type memoryChunk struct {
	id   string
	data []byte
}

func NewLocalCache(dir string, maxSize int64) (*LocalCache, error) {
	return NewLocalCacheWithMemory(dir, maxSize, 0)
}

func NewLocalCacheWithMemory(dir string, maxSize, memoryMax int64) (*LocalCache, error) {
	if maxSize <= 0 {
		maxSize = 512 * 1024 * 1024
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &LocalCache{
		dir:         dir,
		maxSize:     maxSize,
		memoryMax:   memoryMax,
		memoryList:  list.New(),
		memoryItems: map[string]*list.Element{},
	}, nil
}

func (c *LocalCache) Get(chunkID string) ([]byte, bool, error) {
	if b, ok := c.getMemory(chunkID); ok {
		return b, true, nil
	}

	path := c.path(chunkID)
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	c.touch(path)
	c.putMemory(chunkID, b)
	return b, true, nil
}

func (c *LocalCache) GetRange(chunkID string, offset int64, length int) ([]byte, bool, error) {
	if out, ok := c.getMemoryRange(chunkID, offset, length); ok {
		return out, true, nil
	}

	path := c.path(chunkID)
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer file.Close()

	out := make([]byte, length)
	n, err := file.ReadAt(out, offset)
	if err != nil && err != io.EOF {
		return nil, false, err
	}
	if n < length {
		clear(out[n:])
	}
	c.touch(path)
	return out, true, nil
}

func (c *LocalCache) Put(chunkID string, b []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	path := c.path(chunkID)
	tmp, err := os.CreateTemp(c.dir, ".chunk-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	now := time.Now()
	_ = os.Chtimes(path, now, now)
	c.putMemoryLocked(chunkID, b)
	return c.evictLocked()
}

func (c *LocalCache) Stats() (CacheStats, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	size, err := c.currentSizeLocked()
	if err != nil {
		return CacheStats{}, err
	}
	return CacheStats{
		Evictions:       c.evicted,
		Bytes:           size,
		MaxBytes:        c.maxSize,
		MemoryBytes:     c.memoryBytes,
		MemoryMaxBytes:  c.memoryMax,
		MemoryEvictions: c.memoryEvict,
	}, nil
}

func (c *LocalCache) path(chunkID string) string {
	return filepath.Join(c.dir, chunkID)
}

func (c *LocalCache) getMemory(chunkID string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	elem, ok := c.memoryItems[chunkID]
	if !ok {
		return nil, false
	}
	c.memoryList.MoveToFront(elem)
	chunk := elem.Value.(*memoryChunk)
	return append([]byte(nil), chunk.data...), true
}

func (c *LocalCache) getMemoryRange(chunkID string, offset int64, length int) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	elem, ok := c.memoryItems[chunkID]
	if !ok {
		return nil, false
	}
	c.memoryList.MoveToFront(elem)
	chunk := elem.Value.(*memoryChunk)
	out := make([]byte, length)
	if offset < int64(len(chunk.data)) {
		copy(out, chunk.data[offset:minInt64(offset+int64(length), int64(len(chunk.data)))])
	}
	return out, true
}

func (c *LocalCache) putMemory(chunkID string, b []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.putMemoryLocked(chunkID, b)
}

func (c *LocalCache) putMemoryLocked(chunkID string, b []byte) {
	if c.memoryMax <= 0 || int64(len(b)) > c.memoryMax {
		return
	}
	if elem, ok := c.memoryItems[chunkID]; ok {
		chunk := elem.Value.(*memoryChunk)
		c.memoryBytes -= int64(len(chunk.data))
		chunk.data = append(chunk.data[:0], b...)
		c.memoryBytes += int64(len(chunk.data))
		c.memoryList.MoveToFront(elem)
		c.evictMemoryLocked()
		return
	}
	chunk := &memoryChunk{id: chunkID, data: append([]byte(nil), b...)}
	elem := c.memoryList.PushFront(chunk)
	c.memoryItems[chunkID] = elem
	c.memoryBytes += int64(len(chunk.data))
	c.evictMemoryLocked()
}

func (c *LocalCache) evictMemoryLocked() {
	for c.memoryBytes > c.memoryMax {
		elem := c.memoryList.Back()
		if elem == nil {
			return
		}
		chunk := elem.Value.(*memoryChunk)
		delete(c.memoryItems, chunk.id)
		c.memoryList.Remove(elem)
		c.memoryBytes -= int64(len(chunk.data))
		c.memoryEvict++
	}
}

func (c *LocalCache) touch(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	_ = os.Chtimes(path, now, now)
}

func (c *LocalCache) currentSizeLocked() (int64, error) {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return 0, err
	}
	var size int64
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return 0, err
		}
		size += info.Size()
	}
	return size, nil
}

func (c *LocalCache) evictLocked() error {
	type item struct {
		path    string
		size    int64
		modTime time.Time
	}
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return err
	}
	items := make([]item, 0, len(entries))
	var total int64
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		items = append(items, item{
			path:    filepath.Join(c.dir, entry.Name()),
			size:    info.Size(),
			modTime: info.ModTime(),
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].modTime.Before(items[j].modTime) })
	for _, it := range items {
		if total <= c.maxSize {
			break
		}
		if err := os.Remove(it.path); err != nil && !os.IsNotExist(err) {
			return err
		}
		total -= it.size
		c.evicted++
	}
	return nil
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
