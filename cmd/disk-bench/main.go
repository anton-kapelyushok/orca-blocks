package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
	"time"
)

type result struct {
	Kind        string `json:"kind"`
	Mode        string `json:"mode"`
	Path        string `json:"path"`
	SizeBytes   int64  `json:"size_bytes"`
	BlockBytes  int    `json:"block_bytes"`
	Direct      bool   `json:"direct"`
	ReadAheadKB int    `json:"read_ahead_kb"`
	IODepth     int    `json:"io_depth"`
	Ops         int64  `json:"ops"`
	BytesRead   int64  `json:"bytes_read"`
	DurationMS  int64  `json:"duration_ms"`
	MBPerSec    int64  `json:"mb_per_sec"`
	IOPS        int64  `json:"iops"`
	Checksum    uint64 `json:"checksum"`
	GoVersion   string `json:"go_version"`
	NumCPU      int    `json:"num_cpu"`
	GOMAXPROCS  int    `json:"gomaxprocs"`
}

func main() {
	kind := flag.String("kind", "disk-bench", "label to include in the result")
	mode := flag.String("mode", "sequential", "sequential or random")
	path := flag.String("path", "/bench.dat", "file or block device to read")
	sizeBytes := flag.Int64("size-bytes", 256*1024*1024, "number of bytes in the benchmark data set")
	blockBytes := flag.Int("block-bytes", 128*1024, "read block size")
	randomOps := flag.Int64("random-ops", 65536, "number of random reads")
	direct := flag.Bool("direct", false, "open path with O_DIRECT on Linux")
	readAheadKB := flag.Int("read-ahead-kb", -1, "set block device read_ahead_kb before reading; Linux only")
	ioDepth := flag.Int("io-depth", 1, "number of concurrent ReadAt requests")
	flag.Parse()
	if *ioDepth < 1 {
		*ioDepth = 1
	}

	runtime.GOMAXPROCS(1)

	if err := ensureDevice(*path); err != nil {
		fail("prepare path: %v", err)
	}
	if err := configureReadAhead(*path, *readAheadKB); err != nil {
		fail("configure read ahead: %v", err)
	}
	f, err := openForRead(*path, *direct)
	if err != nil {
		fail("open %s: %v", *path, err)
	}
	defer f.Close()

	buf := alignedBuffer(*blockBytes)
	start := time.Now()
	var bytesRead int64
	var ops int64
	var checksum uint64

	switch *mode {
	case "sequential", "serial":
		if *ioDepth == 1 {
			bytesRead, ops, checksum, err = sequentialRead(f, buf, *sizeBytes)
		} else {
			bytesRead, ops, checksum, err = sequentialReadAt(f, *blockBytes, *sizeBytes, *ioDepth)
		}
	case "random", "random_access":
		if *ioDepth == 1 {
			bytesRead, ops, checksum, err = randomRead(f, buf, *sizeBytes, *randomOps)
		} else {
			bytesRead, ops, checksum, err = randomReadAt(f, *blockBytes, *sizeBytes, *randomOps, *ioDepth)
		}
	default:
		fail("unsupported mode %q", *mode)
	}
	if err != nil {
		fail("read: %v", err)
	}
	duration := time.Since(start)
	durationMS := duration.Milliseconds()
	mbPerSec := int64(0)
	iops := int64(0)
	if duration > 0 {
		mbPerSec = int64(float64(bytesRead) / 1024.0 / 1024.0 / duration.Seconds())
		iops = int64(float64(ops) / duration.Seconds())
	}

	out := result{
		Kind:        *kind,
		Mode:        *mode,
		Path:        *path,
		SizeBytes:   *sizeBytes,
		BlockBytes:  *blockBytes,
		Direct:      *direct,
		ReadAheadKB: *readAheadKB,
		IODepth:     *ioDepth,
		Ops:         ops,
		BytesRead:   bytesRead,
		DurationMS:  durationMS,
		MBPerSec:    mbPerSec,
		IOPS:        iops,
		Checksum:    checksum,
		GoVersion:   runtime.Version(),
		NumCPU:      runtime.NumCPU(),
		GOMAXPROCS:  runtime.GOMAXPROCS(0),
	}
	raw, err := json.Marshal(out)
	if err != nil {
		fail("marshal result: %v", err)
	}
	fmt.Printf("DISK_BENCH_RESULT=%s\n", raw)
}

type readJob struct {
	offset int64
	length int
}

type readResult struct {
	bytesRead int64
	ops       int64
	checksum  uint64
	err       error
}

func sequentialReadAt(f *os.File, blockBytes int, sizeBytes int64, ioDepth int) (int64, int64, uint64, error) {
	jobs := make(chan readJob, ioDepth*2)
	results := make(chan readResult, ioDepth)
	var wg sync.WaitGroup
	for i := 0; i < ioDepth; i++ {
		wg.Add(1)
		go readWorker(f, blockBytes, jobs, results, &wg)
	}
	go func() {
		for offset := int64(0); offset < sizeBytes; offset += int64(blockBytes) {
			length := blockBytes
			if remaining := sizeBytes - offset; remaining < int64(length) {
				length = int(remaining)
			}
			jobs <- readJob{offset: offset, length: length}
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()
	return collectResults(results)
}

func randomReadAt(f *os.File, blockBytes int, sizeBytes, randomOps int64, ioDepth int) (int64, int64, uint64, error) {
	maxOffset := sizeBytes - int64(blockBytes)
	if maxOffset < 0 {
		maxOffset = 0
	}
	blocks := maxOffset / int64(blockBytes)
	if blocks < 1 {
		blocks = 1
	}
	jobs := make(chan readJob, ioDepth*2)
	results := make(chan readResult, ioDepth)
	var wg sync.WaitGroup
	for i := 0; i < ioDepth; i++ {
		wg.Add(1)
		go readWorker(f, blockBytes, jobs, results, &wg)
	}
	go func() {
		x := uint64(0x243f6a8885a308d3)
		for i := int64(0); i < randomOps; i++ {
			x ^= x << 13
			x ^= x >> 7
			x ^= x << 17
			offset := int64(x%uint64(blocks)) * int64(blockBytes)
			jobs <- readJob{offset: offset, length: blockBytes}
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()
	return collectResults(results)
}

func readWorker(f *os.File, blockBytes int, jobs <-chan readJob, results chan<- readResult, wg *sync.WaitGroup) {
	defer wg.Done()
	buf := alignedBuffer(blockBytes)
	var out readResult
	for job := range jobs {
		n, err := f.ReadAt(buf[:job.length], job.offset)
		if err != nil && err != io.EOF {
			out.err = err
			break
		}
		if n == 0 {
			continue
		}
		out.checksum ^= mix(0, buf[:n])
		out.bytesRead += int64(n)
		out.ops++
	}
	results <- out
}

func collectResults(results <-chan readResult) (int64, int64, uint64, error) {
	var bytesRead int64
	var ops int64
	var checksum uint64
	for result := range results {
		if result.err != nil {
			return bytesRead, ops, checksum, result.err
		}
		bytesRead += result.bytesRead
		ops += result.ops
		checksum ^= result.checksum
	}
	return bytesRead, ops, checksum, nil
}

func sequentialRead(f *os.File, buf []byte, sizeBytes int64) (int64, int64, uint64, error) {
	var bytesRead int64
	var ops int64
	var checksum uint64
	for bytesRead < sizeBytes {
		want := len(buf)
		remaining := sizeBytes - bytesRead
		if remaining < int64(want) {
			want = int(remaining)
		}
		n, err := io.ReadFull(f, buf[:want])
		if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
			return bytesRead, ops, checksum, err
		}
		if n == 0 {
			break
		}
		checksum = mix(checksum, buf[:n])
		bytesRead += int64(n)
		ops++
		if err == io.ErrUnexpectedEOF || err == io.EOF {
			break
		}
	}
	return bytesRead, ops, checksum, nil
}

func randomRead(f *os.File, buf []byte, sizeBytes, randomOps int64) (int64, int64, uint64, error) {
	maxOffset := sizeBytes - int64(len(buf))
	if maxOffset < 0 {
		maxOffset = 0
	}
	block := int64(len(buf))
	if block <= 0 {
		block = 1
	}
	blocks := maxOffset / block
	if blocks < 1 {
		blocks = 1
	}
	var bytesRead int64
	var ops int64
	var checksum uint64
	x := uint64(0x243f6a8885a308d3)
	for i := int64(0); i < randomOps; i++ {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		offset := int64(x%uint64(blocks)) * block
		n, err := f.ReadAt(buf, offset)
		if err != nil && err != io.EOF {
			return bytesRead, ops, checksum, err
		}
		if n == 0 {
			break
		}
		checksum = mix(checksum, buf[:n])
		bytesRead += int64(n)
		ops++
	}
	return bytesRead, ops, checksum, nil
}

func mix(checksum uint64, data []byte) uint64 {
	for i := 0; i < len(data); i += 4096 {
		checksum ^= uint64(data[i]) + 0x9e3779b97f4a7c15 + (checksum << 6) + (checksum >> 2)
	}
	return checksum
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
