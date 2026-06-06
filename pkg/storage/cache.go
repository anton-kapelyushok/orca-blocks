package storage

import (
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type CacheStats struct {
	Hits      int64 `json:"hits"`
	Misses    int64 `json:"misses"`
	Evictions int64 `json:"evictions"`
	Bytes     int64 `json:"bytes"`
	MaxBytes  int64 `json:"max_bytes"`
}

type LocalCache struct {
	dir     string
	maxSize int64
	mu      sync.Mutex
	evicted int64
}

func NewLocalCache(dir string, maxSize int64) (*LocalCache, error) {
	if maxSize <= 0 {
		maxSize = 512 * 1024 * 1024
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &LocalCache{dir: dir, maxSize: maxSize}, nil
}

func (c *LocalCache) Get(chunkID string) ([]byte, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	path := c.path(chunkID)
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	now := time.Now()
	_ = os.Chtimes(path, now, now)
	return b, true, nil
}

func (c *LocalCache) Put(chunkID string, b []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	path := c.path(chunkID)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return err
	}
	now := time.Now()
	_ = os.Chtimes(path, now, now)
	return c.evictLocked()
}

func (c *LocalCache) Stats() (CacheStats, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	size, err := c.currentSizeLocked()
	if err != nil {
		return CacheStats{}, err
	}
	return CacheStats{Evictions: c.evicted, Bytes: size, MaxBytes: c.maxSize}, nil
}

func (c *LocalCache) path(chunkID string) string {
	return filepath.Join(c.dir, chunkID)
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
