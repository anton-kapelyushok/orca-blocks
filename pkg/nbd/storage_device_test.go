package nbd

import (
	"bytes"
	"context"
	"testing"

	"github.com/anton-k/orca-blocks/pkg/storage"
)

type recordingBackend struct {
	data   []byte
	reads  []readCall
	writes []writeCall
}

type readCall struct {
	offset int64
	length int64
}

type writeCall struct {
	offset int64
	data   []byte
}

func (b *recordingBackend) Read(_ context.Context, _ string, offset, length int64) ([]byte, error) {
	b.reads = append(b.reads, readCall{offset: offset, length: length})
	out := make([]byte, int(length))
	copy(out, b.data[offset:minInt64(offset+length, int64(len(b.data)))])
	return out, nil
}

func (b *recordingBackend) Write(_ context.Context, _ string, offset int64, data []byte) error {
	b.writes = append(b.writes, writeCall{offset: offset, data: append([]byte(nil), data...)})
	copy(b.data[offset:], data)
	return nil
}

func (b *recordingBackend) FlushDirty(string) error {
	return nil
}

func (b *recordingBackend) CommitWithOptions(context.Context, string, storage.CommitOptions) (storage.Snapshot, error) {
	return storage.Snapshot{}, nil
}

func (b *recordingBackend) Stop(string) error {
	return nil
}

func TestStorageDeviceReadAheadRequiresStrictSequentialStreak(t *testing.T) {
	backend := &recordingBackend{data: patternedBytes(2 * 1024 * 1024)}
	device := &StorageDevice{
		Backend:        backend,
		SessionID:      "session",
		SizeBytes:      int64(len(backend.data)),
		ReadAheadBytes: 1024 * 1024,
	}

	t.Log("first read is exact; no read-ahead yet")
	mustReadAt(t, device, 0, 64*1024)
	t.Log("second adjacent read is exact; streak is only one")
	mustReadAt(t, device, 64*1024, 64*1024)
	t.Log("third adjacent read triggers a 1 MiB backend read-ahead window")
	mustReadAt(t, device, 128*1024, 64*1024)
	t.Log("fourth adjacent read is served from the read-ahead window")
	mustReadAt(t, device, 192*1024, 64*1024)

	want := []readCall{
		{offset: 0, length: 64 * 1024},
		{offset: 64 * 1024, length: 64 * 1024},
		{offset: 128 * 1024, length: 1024 * 1024},
	}
	if len(backend.reads) != len(want) {
		t.Fatalf("backend reads = %+v, want %+v", backend.reads, want)
	}
	for i := range want {
		if backend.reads[i] != want[i] {
			t.Fatalf("backend read %d = %+v, want %+v", i, backend.reads[i], want[i])
		}
	}
}

func TestStorageDeviceReadAheadDoesNotTriggerForRandomForwardReads(t *testing.T) {
	backend := &recordingBackend{data: patternedBytes(2 * 1024 * 1024)}
	device := &StorageDevice{
		Backend:        backend,
		SessionID:      "session",
		SizeBytes:      int64(len(backend.data)),
		ReadAheadBytes: 1024 * 1024,
	}

	t.Log("reading forward with gaps; this should not look sequential")
	mustReadAt(t, device, 0, 64*1024)
	mustReadAt(t, device, 256*1024, 64*1024)
	mustReadAt(t, device, 512*1024, 64*1024)

	for _, call := range backend.reads {
		if call.length != 64*1024 {
			t.Fatalf("random forward read triggered read-ahead: %+v", backend.reads)
		}
	}
}

func TestStorageDeviceWriteInvalidatesReadAheadWindow(t *testing.T) {
	backend := &recordingBackend{data: patternedBytes(2 * 1024 * 1024)}
	device := &StorageDevice{
		Backend:        backend,
		SessionID:      "session",
		SizeBytes:      int64(len(backend.data)),
		ReadAheadBytes: 1024 * 1024,
	}

	t.Log("creating a read-ahead window")
	mustReadAt(t, device, 0, 64*1024)
	mustReadAt(t, device, 64*1024, 64*1024)
	mustReadAt(t, device, 128*1024, 64*1024)
	if len(backend.reads) != 3 || backend.reads[2].length != 1024*1024 {
		t.Fatalf("expected read-ahead window, got reads %+v", backend.reads)
	}

	t.Log("writing inside the old window; future reads must not use stale memory")
	if err := device.WriteAt(context.Background(), 192*1024, []byte("changed")); err != nil {
		t.Fatal(err)
	}
	got := mustReadAt(t, device, 192*1024, int64(len("changed")))
	if !bytes.Equal(got, []byte("changed")) {
		t.Fatalf("read after write = %q, want changed", got)
	}
	if got := backend.reads[len(backend.reads)-1]; got.offset != 192*1024 || got.length != int64(len("changed")) {
		t.Fatalf("read after write used stale read-ahead window; reads=%+v", backend.reads)
	}
}

func mustReadAt(t *testing.T, device *StorageDevice, offset, length int64) []byte {
	t.Helper()
	got, err := device.ReadAt(context.Background(), offset, length)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func patternedBytes(size int) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i % 251)
	}
	return data
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
