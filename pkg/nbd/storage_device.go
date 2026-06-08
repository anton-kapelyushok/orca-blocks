package nbd

import (
	"context"
	"sync"

	"github.com/anton-k/orca-blocks/pkg/storage"
)

type StorageBackend interface {
	Read(ctx context.Context, sessionID string, offset, length int64) ([]byte, error)
	Write(ctx context.Context, sessionID string, offset int64, data []byte) error
	FlushDirty(sessionID string) error
	CommitWithOptions(ctx context.Context, sessionID string, opts storage.CommitOptions) (storage.Snapshot, error)
	Stop(sessionID string) error
}

type StorageDevice struct {
	Backend            StorageBackend
	SessionID          string
	SizeBytes          int64
	CommitOnDisconnect bool
	CommitOptions      storage.CommitOptions
	OnDisconnect       func()
	ReadAheadBytes     int64

	mu               sync.Mutex
	haveLastRead     bool
	lastReadOffset   int64
	lastReadLength   int64
	sequentialStreak int
	windowOffset     int64
	windowData       []byte
}

func (d *StorageDevice) Size() int64 {
	return d.SizeBytes
}

func (d *StorageDevice) ReadAt(ctx context.Context, offset, length int64) ([]byte, error) {
	if d.ReadAheadBytes <= 0 || length <= 0 {
		return d.Backend.Read(ctx, d.SessionID, offset, length)
	}

	if data, ok := d.readAheadHit(offset, length); ok {
		return data, nil
	}

	fetchLength, storeWindow := d.planReadAhead(offset, length)
	data, err := d.Backend.Read(ctx, d.SessionID, offset, fetchLength)
	if err != nil {
		return nil, err
	}
	if storeWindow {
		d.storeReadAheadWindow(offset, data)
	}
	if int64(len(data)) < length {
		return data, nil
	}
	return append([]byte(nil), data[:length]...), nil
}

func (d *StorageDevice) WriteAt(ctx context.Context, offset int64, data []byte) error {
	d.invalidateReadAhead()
	return d.Backend.Write(ctx, d.SessionID, offset, data)
}

func (d *StorageDevice) Flush(context.Context) error {
	return d.Backend.FlushDirty(d.SessionID)
}

func (d *StorageDevice) Disconnect(ctx context.Context) error {
	if d.OnDisconnect != nil {
		defer d.OnDisconnect()
	}
	if d.CommitOnDisconnect {
		if _, err := d.Backend.CommitWithOptions(ctx, d.SessionID, d.CommitOptions); err != nil {
			return err
		}
		return d.Backend.Stop(d.SessionID)
	}
	return d.Backend.FlushDirty(d.SessionID)
}

func (d *StorageDevice) readAheadHit(offset, length int64) ([]byte, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.windowData == nil || offset < d.windowOffset {
		return nil, false
	}
	start := offset - d.windowOffset
	end := start + length
	if start < 0 || end > int64(len(d.windowData)) {
		return nil, false
	}
	d.recordReadLocked(offset, length)
	out := make([]byte, int(length))
	copy(out, d.windowData[start:end])
	return out, true
}

func (d *StorageDevice) planReadAhead(offset, length int64) (int64, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	sequential := d.haveLastRead && offset == d.lastReadOffset+d.lastReadLength
	d.recordReadLocked(offset, length)
	if !sequential || d.sequentialStreak < 2 {
		return length, false
	}

	fetchLength := d.ReadAheadBytes
	if fetchLength < length {
		fetchLength = length
	}
	if remaining := d.SizeBytes - offset; remaining > 0 && fetchLength > remaining {
		fetchLength = remaining
	}
	return fetchLength, fetchLength > length
}

func (d *StorageDevice) recordReadLocked(offset, length int64) {
	if d.haveLastRead && offset == d.lastReadOffset+d.lastReadLength {
		d.sequentialStreak++
	} else {
		d.sequentialStreak = 0
	}
	d.haveLastRead = true
	d.lastReadOffset = offset
	d.lastReadLength = length
}

func (d *StorageDevice) storeReadAheadWindow(offset int64, data []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.windowOffset = offset
	d.windowData = append(d.windowData[:0], data...)
}

func (d *StorageDevice) invalidateReadAhead() {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.haveLastRead = false
	d.lastReadOffset = 0
	d.lastReadLength = 0
	d.sequentialStreak = 0
	d.windowOffset = 0
	d.windowData = nil
}
