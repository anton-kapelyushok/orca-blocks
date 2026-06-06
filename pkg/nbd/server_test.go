package nbd

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
)

type memDevice struct {
	data       []byte
	flushes    int
	disconnect int
}

func (d *memDevice) Size() int64 { return int64(len(d.data)) }

func (d *memDevice) ReadAt(_ context.Context, offset, length int64) ([]byte, error) {
	return append([]byte(nil), d.data[offset:offset+length]...), nil
}

func (d *memDevice) WriteAt(_ context.Context, offset int64, data []byte) error {
	copy(d.data[offset:], data)
	return nil
}

func (d *memDevice) Flush(context.Context) error {
	d.flushes++
	return nil
}

func (d *memDevice) Disconnect(context.Context) error {
	d.disconnect++
	return nil
}

func TestServerReadWriteFlushDisconnect(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()

	device := &memDevice{data: make([]byte, 64)}
	errCh := make(chan error, 1)
	go func() {
		errCh <- (&Server{Device: device}).Handle(context.Background(), serverConn)
	}()

	t.Log("performing NBD handshake")
	handshake(t, clientConn, 64)

	t.Log("writing bytes through NBD")
	writeRequest(t, clientConn, 1, cmdWrite, 10, []byte("hello"))
	readReply(t, clientConn, 1, nil)

	t.Log("reading bytes back through NBD")
	writeRequest(t, clientConn, 2, cmdRead, 10, make([]byte, 5))
	got := readReply(t, clientConn, 2, make([]byte, 5))
	if !bytes.Equal(got, []byte("hello")) {
		t.Fatalf("read = %q, want hello", got)
	}

	t.Log("flushing dirty overlay through NBD")
	writeRequest(t, clientConn, 3, cmdFlush, 0, nil)
	readReply(t, clientConn, 3, nil)
	if device.flushes != 1 {
		t.Fatalf("flushes = %d, want 1", device.flushes)
	}

	t.Log("disconnecting NBD client")
	writeRequest(t, clientConn, 4, cmdDisconnect, 0, nil)
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	if device.disconnect != 1 {
		t.Fatalf("disconnects = %d, want 1", device.disconnect)
	}
}

func handshake(t *testing.T, rw io.ReadWriter, wantSize uint64) {
	t.Helper()
	var serverHeader [18]byte
	if _, err := io.ReadFull(readerOnly{rw}, serverHeader[:]); err != nil {
		t.Fatal(err)
	}
	if got := binary.BigEndian.Uint64(serverHeader[0:8]); got != nbdMagic {
		t.Fatalf("server magic = 0x%x", got)
	}
	if _, err := rw.Write([]byte{0, 0, 0, byte(flagNoZeroes)}); err != nil {
		t.Fatal(err)
	}
	var opt [16]byte
	binary.BigEndian.PutUint64(opt[0:8], iHaveOpt)
	binary.BigEndian.PutUint32(opt[8:12], optExportName)
	if _, err := rw.Write(opt[:]); err != nil {
		t.Fatal(err)
	}
	var export [10]byte
	if _, err := io.ReadFull(readerOnly{rw}, export[:]); err != nil {
		t.Fatal(err)
	}
	if got := binary.BigEndian.Uint64(export[0:8]); got != wantSize {
		t.Fatalf("export size = %d, want %d", got, wantSize)
	}
}

func writeRequest(t *testing.T, w io.Writer, handle uint64, cmd uint16, offset int64, data []byte) {
	t.Helper()
	var req [28]byte
	binary.BigEndian.PutUint32(req[0:4], requestMagic)
	binary.BigEndian.PutUint16(req[6:8], cmd)
	binary.BigEndian.PutUint64(req[8:16], handle)
	binary.BigEndian.PutUint64(req[16:24], uint64(offset))
	if cmd == cmdRead {
		binary.BigEndian.PutUint32(req[24:28], uint32(len(data)))
		if _, err := w.Write(req[:]); err != nil {
			t.Fatal(err)
		}
		return
	}
	binary.BigEndian.PutUint32(req[24:28], uint32(len(data)))
	if _, err := w.Write(req[:]); err != nil {
		t.Fatal(err)
	}
	if len(data) > 0 {
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
}

func readReply(t *testing.T, r io.Reader, wantHandle uint64, data []byte) []byte {
	t.Helper()
	var reply [16]byte
	if _, err := io.ReadFull(r, reply[:]); err != nil {
		t.Fatal(err)
	}
	if got := binary.BigEndian.Uint32(reply[0:4]); got != replyMagic {
		t.Fatalf("reply magic = 0x%x", got)
	}
	if got := binary.BigEndian.Uint32(reply[4:8]); got != 0 {
		t.Fatalf("reply errno = %d", got)
	}
	if got := binary.BigEndian.Uint64(reply[8:16]); got != wantHandle {
		t.Fatalf("reply handle = %d, want %d", got, wantHandle)
	}
	if data == nil {
		return nil
	}
	if _, err := io.ReadFull(r, data); err != nil {
		t.Fatal(err)
	}
	return data
}
