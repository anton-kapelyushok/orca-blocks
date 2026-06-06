package nbd

import (
	"context"

	"github.com/anton-k/orca-blocks/pkg/storage"
)

type StorageDevice struct {
	Backend            *storage.Backend
	SessionID          string
	SizeBytes          int64
	CommitOnDisconnect bool
	CommitOptions      storage.CommitOptions
	OnDisconnect       func()
}

func (d *StorageDevice) Size() int64 {
	return d.SizeBytes
}

func (d *StorageDevice) ReadAt(ctx context.Context, offset, length int64) ([]byte, error) {
	return d.Backend.Read(ctx, d.SessionID, offset, length)
}

func (d *StorageDevice) WriteAt(ctx context.Context, offset int64, data []byte) error {
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
