package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/anton-k/orca-blocks/pkg/nbd"
)

type fileDevice struct {
	file      *os.File
	size      int64
	mode      string
	chunkSize int64
	stats     *fileDeviceStats
}

type fileDeviceStats struct {
	mu               sync.Mutex
	BackendReads     int64            `json:"backend_reads"`
	BackendReadBytes int64            `json:"backend_read_bytes"`
	FileReadOps      int64            `json:"file_read_ops"`
	FileReadBytes    int64            `json:"file_read_bytes"`
	ChunkReads       int64            `json:"chunk_reads"`
	ReadHistogram    map[string]int64 `json:"read_histogram"`
}

func newFileDeviceStats() *fileDeviceStats {
	return &fileDeviceStats{ReadHistogram: map[string]int64{}}
}

func (s *fileDeviceStats) recordBackendRead(length int64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.BackendReads++
	s.BackendReadBytes += length
	s.ReadHistogram[sizeBucket(length)]++
}

func (s *fileDeviceStats) recordFileRead(length int64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.FileReadOps++
	s.FileReadBytes += length
}

func (s *fileDeviceStats) recordChunkRead() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ChunkReads++
}

func (s *fileDeviceStats) snapshot() fileDeviceStats {
	if s == nil {
		return fileDeviceStats{ReadHistogram: map[string]int64{}}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := *s
	out.ReadHistogram = map[string]int64{}
	for k, v := range s.ReadHistogram {
		out.ReadHistogram[k] = v
	}
	return out
}

func (d *fileDevice) Size() int64 {
	return d.size
}

func (d *fileDevice) ReadAt(_ context.Context, offset, length int64) ([]byte, error) {
	d.stats.recordBackendRead(length)
	if d.mode == "chunk" {
		return d.readAtViaChunk(offset, length)
	}
	data := make([]byte, int(length))
	n, err := d.file.ReadAt(data, offset)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if n < len(data) {
		clear(data[n:])
	}
	d.stats.recordFileRead(int64(n))
	return data, nil
}

func (d *fileDevice) ReadAtInto(_ context.Context, offset int64, dst []byte) (int, error) {
	d.stats.recordBackendRead(int64(len(dst)))
	if d.mode == "chunk" {
		return d.readAtViaChunkInto(offset, dst)
	}
	n, err := d.file.ReadAt(dst, offset)
	if err != nil && err != io.EOF {
		return n, err
	}
	if n < len(dst) {
		clear(dst[n:])
	}
	d.stats.recordFileRead(int64(n))
	return len(dst), nil
}

func (d *fileDevice) readAtViaChunk(offset, length int64) ([]byte, error) {
	data := make([]byte, int(length))
	if _, err := d.readAtViaChunkInto(offset, data); err != nil {
		return nil, err
	}
	return data, nil
}

func (d *fileDevice) readAtViaChunkInto(offset int64, dst []byte) (int, error) {
	var copied int64
	length := int64(len(dst))
	for copied < length {
		readOffset := offset + copied
		chunkStart := (readOffset / d.chunkSize) * d.chunkSize
		chunkEnd := chunkStart + d.chunkSize
		if chunkEnd > d.size {
			chunkEnd = d.size
		}
		chunk := make([]byte, int(chunkEnd-chunkStart))
		n, err := d.file.ReadAt(chunk, chunkStart)
		if err != nil && err != io.EOF {
			return int(copied), err
		}
		d.stats.recordChunkRead()
		d.stats.recordFileRead(int64(n))
		if n < len(chunk) {
			clear(chunk[n:])
		}

		start := readOffset - chunkStart
		part := int64(len(chunk)) - start
		if part > length-copied {
			part = length - copied
		}
		copy(dst[copied:copied+part], chunk[start:start+part])
		copied += part
	}
	return len(dst), nil
}

func (d *fileDevice) WriteAt(_ context.Context, offset int64, data []byte) error {
	_, err := d.file.WriteAt(data, offset)
	return err
}

func (d *fileDevice) Flush(context.Context) error {
	return d.file.Sync()
}

func (d *fileDevice) Disconnect(context.Context) error {
	return nil
}

func main() {
	addr := flag.String("addr", "127.0.0.1:10909", "TCP address for the NBD server")
	path := flag.String("file", "", "file to expose as an NBD export")
	exportName := flag.String("export", "bench", "accepted NBD export name")
	mode := flag.String("mode", "range", "read mode: range or chunk")
	chunkSize := flag.Int64("chunk-size", 4*1024*1024, "chunk size for -mode chunk")
	statsPath := flag.String("stats", "", "write JSON stats on shutdown")
	verbose := flag.Bool("verbose", false, "log every NBD request")
	flag.Parse()

	if *path == "" {
		log.Fatal("-file is required")
	}
	if *mode != "range" && *mode != "chunk" {
		log.Fatalf("unsupported -mode %q", *mode)
	}
	file, err := os.OpenFile(*path, os.O_RDWR, 0)
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		log.Fatal(err)
	}
	fileStats := newFileDeviceStats()
	device := &fileDevice{file: file, size: info.Size(), mode: *mode, chunkSize: *chunkSize, stats: fileStats}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	defer ln.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	var logger *log.Logger
	if *verbose {
		logger = log.New(os.Stderr, "", log.LstdFlags|log.Lmicroseconds)
	}
	protocolStats := nbd.NewStatsCollector()
	server := &nbd.Server{
		Resolve: func(name string) (nbd.Device, error) {
			if name != "" && name != *exportName {
				return nil, fmt.Errorf("unknown export %q", name)
			}
			return device, nil
		},
		Logger: logger,
		Stats:  protocolStats,
	}

	log.Printf("serving file=%s size=%d addr=%s export=%s mode=%s chunk_size=%d", *path, info.Size(), *addr, *exportName, *mode, *chunkSize)
	if err := server.Serve(ctx, ln); err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
	if *statsPath != "" {
		stats := map[string]any{
			"nbd":    protocolStats.Snapshot(),
			"device": fileStats.snapshot(),
		}
		raw, err := json.MarshalIndent(stats, "", "  ")
		if err != nil {
			log.Printf("marshal stats failed: %v", err)
		} else if err := os.WriteFile(*statsPath, raw, 0o644); err != nil {
			log.Printf("write stats failed: %v", err)
		}
	}
}

func sizeBucket(length int64) string {
	const kib = 1024
	const mib = 1024 * kib
	switch {
	case length < 4*kib:
		return "<4KiB"
	case length == 4*kib:
		return "4KiB"
	case length <= 8*kib:
		return "5-8KiB"
	case length <= 16*kib:
		return "9-16KiB"
	case length <= 32*kib:
		return "17-32KiB"
	case length <= 64*kib:
		return "33-64KiB"
	case length <= 128*kib:
		return "65-128KiB"
	case length <= 256*kib:
		return "129-256KiB"
	case length <= 512*kib:
		return "257-512KiB"
	case length <= mib:
		return "513KiB-1MiB"
	case length <= 2*mib:
		return "1-2MiB"
	case length <= 4*mib:
		return "2-4MiB"
	default:
		return ">4MiB"
	}
}
