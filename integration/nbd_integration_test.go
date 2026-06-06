//go:build integration

package integration

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

const (
	nbdMagic      = uint64(0x4e42444d41474943)
	iHaveOpt      = uint64(0x49484156454f5054)
	requestMagic  = uint32(0x25609513)
	replyMagic    = uint32(0x67446698)
	optExportName = uint32(1)
	flagNoZeroes  = uint16(2)
	cmdRead       = uint16(0)
	cmdWrite      = uint16(1)
	cmdDisconnect = uint16(2)
	cmdFlush      = uint16(3)
)

func TestNBDWriteCommitsAndCanResumeThroughNode(t *testing.T) {
	control := getenv("CONTROL_URL", "http://localhost:18080")

	t.Logf("waiting for control service at %s", control)
	waitFor(t, control+"/healthz")

	volumeID := fmt.Sprintf("nbd-itest-%d", time.Now().UnixNano())
	t.Logf("creating volume %s for session-local NBD export", volumeID)
	createNBDTestVolume(t, control, volumeID)

	t.Log("starting session on node-1 with low-level NBD export test runtime enabled")
	start := startNBDSession(t, control, volumeID)
	nbdAddr := start["nbd_addr"]
	exportName := start["nbd_export_name"]
	t.Logf("session=%s nbd_addr=%s export=%s", start["session_id"], nbdAddr, exportName)
	t.Logf("waiting for node-local NBD test listener at %s", nbdAddr)
	waitForTCP(t, nbdAddr)

	conn, size := dialNBD(t, nbdAddr, exportName)
	t.Cleanup(func() { _ = conn.Close() })
	t.Logf("connected to NBD export size=%d", size)

	data := []byte(fmt.Sprintf("nbd-block-write-payload-%d", time.Now().UnixNano()))
	offset := int64(128 * 1024)
	if offset+int64(len(data)) > int64(size) {
		t.Fatalf("test write exceeds NBD export size")
	}

	t.Logf("writing %d bytes to NBD block device at offset=%d", len(data), offset)
	nbdWrite(t, conn, 1, offset, data)
	t.Log("flushing NBD dirty overlay")
	nbdFlush(t, conn, 2)

	t.Log("reading bytes back through NBD before disconnect")
	gotNBD := nbdRead(t, conn, 3, offset, int64(len(data)))
	if !bytes.Equal(gotNBD, data) {
		t.Fatalf("NBD read = %q, want %q", gotNBD, data)
	}

	t.Log("disconnecting NBD client; session export is configured to commit on disconnect")
	nbdDisconnect(t, conn, 4)

	t.Log("resuming same volume through node-1 and verifying NBD-written bytes from committed snapshot")
	session := startSession(t, control, volumeID, "node-1")
	gotHTTP := getRaw(t, fmt.Sprintf("%s/sessions/%s/read?offset=%d&length=%d", session["node_url"], session["session_id"], offset, len(data)))
	if !bytes.Equal(gotHTTP, data) {
		t.Fatalf("node read after NBD commit = %q, want %q", gotHTTP, data)
	}
	stats := getJSON(t, fmt.Sprintf("%s/sessions/%s/stats", session["node_url"], session["session_id"]))
	t.Logf("node-1 read after NBD commit got hits=%v misses=%v remote_fetches=%v zero_fills=%v", stats["cache_hits"], stats["cache_misses"], stats["remote_fetches"], stats["zero_fills"])
}

func TestNBDOneNodeServesTwoSessionExports(t *testing.T) {
	control := getenv("CONTROL_URL", "http://localhost:18080")

	t.Logf("waiting for control service at %s", control)
	waitFor(t, control+"/healthz")

	volumeA := fmt.Sprintf("nbd-two-a-%d", time.Now().UnixNano())
	volumeB := fmt.Sprintf("nbd-two-b-%d", time.Now().UnixNano())
	t.Logf("creating volumes %s and %s", volumeA, volumeB)
	createNBDTestVolume(t, control, volumeA)
	createNBDTestVolume(t, control, volumeB)

	t.Log("starting two NBD-backed sessions on node-1")
	startA := startNBDSession(t, control, volumeA)
	startB := startNBDSession(t, control, volumeB)
	if startA["nbd_addr"] != startB["nbd_addr"] {
		t.Fatalf("expected both sessions on the same node NBD listener, got %s and %s", startA["nbd_addr"], startB["nbd_addr"])
	}
	if startA["nbd_export_name"] == startB["nbd_export_name"] {
		t.Fatalf("expected distinct export names, got %s", startA["nbd_export_name"])
	}
	t.Logf("same NBD listener=%s exportA=%s exportB=%s", startA["nbd_addr"], startA["nbd_export_name"], startB["nbd_export_name"])
	waitForTCP(t, startA["nbd_addr"])

	connA, sizeA := dialNBD(t, startA["nbd_addr"], startA["nbd_export_name"])
	defer connA.Close()
	connB, sizeB := dialNBD(t, startB["nbd_addr"], startB["nbd_export_name"])
	defer connB.Close()
	t.Logf("connected export A size=%d and export B size=%d", sizeA, sizeB)

	offset := int64(192 * 1024)
	dataA := []byte("payload-for-session-export-A")
	dataB := []byte("payload-for-session-export-B")

	t.Log("writing different bytes to each NBD export at the same offset")
	nbdWrite(t, connA, 1, offset, dataA)
	nbdWrite(t, connB, 1, offset, dataB)
	nbdFlush(t, connA, 2)
	nbdFlush(t, connB, 2)

	t.Log("reading each export before commit to prove routing stays session-local")
	gotA := nbdRead(t, connA, 3, offset, int64(len(dataA)))
	gotB := nbdRead(t, connB, 3, offset, int64(len(dataB)))
	if !bytes.Equal(gotA, dataA) {
		t.Fatalf("export A read = %q, want %q", gotA, dataA)
	}
	if !bytes.Equal(gotB, dataB) {
		t.Fatalf("export B read = %q, want %q", gotB, dataB)
	}

	t.Log("disconnecting both exports; each session commits independently")
	nbdDisconnect(t, connA, 4)
	nbdDisconnect(t, connB, 4)

	t.Log("resuming both volumes and verifying they kept distinct contents")
	verifyCommittedBytes(t, control, volumeA, offset, dataA)
	verifyCommittedBytes(t, control, volumeB, offset, dataB)
}

func createNBDTestVolume(t *testing.T, control, volumeID string) {
	t.Helper()
	var volume map[string]any
	postJSON(t, control+"/volumes/create", map[string]any{
		"volume_id":  volumeID,
		"size_bytes": 1024 * 1024,
		"chunk_size": 64 * 1024,
	}, &volume)
}

func startNBDSession(t *testing.T, control, volumeID string) map[string]string {
	t.Helper()
	var start map[string]string
	postJSON(t, control+"/sessions/start", map[string]any{
		"volume_id":            volumeID,
		"force_node":           "node-1",
		"runtime":              "nbd-export-test",
		"commit_on_disconnect": true,
	}, &start)
	if start["nbd_addr"] == "" || start["nbd_export_name"] == "" {
		t.Fatalf("missing NBD session fields in start response: %+v", start)
	}
	return start
}

func verifyCommittedBytes(t *testing.T, control, volumeID string, offset int64, want []byte) {
	t.Helper()
	session := startSession(t, control, volumeID, "node-1")
	got := getRaw(t, fmt.Sprintf("%s/sessions/%s/read?offset=%d&length=%d", session["node_url"], session["session_id"], offset, len(want)))
	if !bytes.Equal(got, want) {
		t.Fatalf("committed read for volume %s = %q, want %q", volumeID, got, want)
	}
}

func waitForTCP(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("timed out waiting for tcp %s", addr)
}

func dialNBD(t *testing.T, addr, exportName string) (net.Conn, uint64) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var serverHeader [18]byte
	if _, err := io.ReadFull(conn, serverHeader[:]); err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	if got := binary.BigEndian.Uint64(serverHeader[0:8]); got != nbdMagic {
		_ = conn.Close()
		t.Fatalf("NBD magic = 0x%x", got)
	}
	if got := binary.BigEndian.Uint64(serverHeader[8:16]); got != iHaveOpt {
		_ = conn.Close()
		t.Fatalf("NBD option magic = 0x%x", got)
	}
	if _, err := conn.Write([]byte{0, 0, 0, byte(flagNoZeroes)}); err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	var opt [16]byte
	binary.BigEndian.PutUint64(opt[0:8], iHaveOpt)
	binary.BigEndian.PutUint32(opt[8:12], optExportName)
	binary.BigEndian.PutUint32(opt[12:16], uint32(len(exportName)))
	if _, err := conn.Write(opt[:]); err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	if exportName != "" {
		if _, err := conn.Write([]byte(exportName)); err != nil {
			_ = conn.Close()
			t.Fatal(err)
		}
	}
	var export [10]byte
	if _, err := io.ReadFull(conn, export[:]); err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	return conn, binary.BigEndian.Uint64(export[0:8])
}

func nbdWrite(t *testing.T, conn net.Conn, handle uint64, offset int64, data []byte) {
	t.Helper()
	writeNBDRequest(t, conn, handle, cmdWrite, offset, uint32(len(data)))
	if _, err := conn.Write(data); err != nil {
		t.Fatal(err)
	}
	readNBDReply(t, conn, handle, nil)
}

func nbdRead(t *testing.T, conn net.Conn, handle uint64, offset, length int64) []byte {
	t.Helper()
	writeNBDRequest(t, conn, handle, cmdRead, offset, uint32(length))
	data := make([]byte, int(length))
	readNBDReply(t, conn, handle, data)
	return data
}

func nbdFlush(t *testing.T, conn net.Conn, handle uint64) {
	t.Helper()
	writeNBDRequest(t, conn, handle, cmdFlush, 0, 0)
	readNBDReply(t, conn, handle, nil)
}

func nbdDisconnect(t *testing.T, conn net.Conn, handle uint64) {
	t.Helper()
	writeNBDRequest(t, conn, handle, cmdDisconnect, 0, 0)
	var one [1]byte
	_, err := conn.Read(one[:])
	if err == nil {
		t.Fatal("expected NBD server to close connection after disconnect")
	}
	if err != io.EOF {
		t.Fatalf("waiting for NBD disconnect EOF: %v", err)
	}
}

func writeNBDRequest(t *testing.T, w io.Writer, handle uint64, cmd uint16, offset int64, length uint32) {
	t.Helper()
	var req [28]byte
	binary.BigEndian.PutUint32(req[0:4], requestMagic)
	binary.BigEndian.PutUint16(req[6:8], cmd)
	binary.BigEndian.PutUint64(req[8:16], handle)
	binary.BigEndian.PutUint64(req[16:24], uint64(offset))
	binary.BigEndian.PutUint32(req[24:28], length)
	if _, err := w.Write(req[:]); err != nil {
		t.Fatal(err)
	}
}

func readNBDReply(t *testing.T, r io.Reader, wantHandle uint64, data []byte) {
	t.Helper()
	var reply [16]byte
	if _, err := io.ReadFull(r, reply[:]); err != nil {
		t.Fatal(err)
	}
	if got := binary.BigEndian.Uint32(reply[0:4]); got != replyMagic {
		t.Fatalf("NBD reply magic = 0x%x", got)
	}
	if got := binary.BigEndian.Uint32(reply[4:8]); got != 0 {
		t.Fatalf("NBD reply errno = %d", got)
	}
	if got := binary.BigEndian.Uint64(reply[8:16]); got != wantHandle {
		t.Fatalf("NBD reply handle = %d, want %d", got, wantHandle)
	}
	if data != nil {
		if _, err := io.ReadFull(r, data); err != nil {
			t.Fatal(err)
		}
	}
}
