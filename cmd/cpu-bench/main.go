package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime"
	"time"
)

type result struct {
	Kind       string `json:"kind"`
	Iterations uint64 `json:"iterations"`
	DurationMS int64  `json:"duration_ms"`
	OpsPerSec  uint64 `json:"ops_per_sec"`
	Checksum   uint64 `json:"checksum"`
	GoVersion  string `json:"go_version"`
	NumCPU     int    `json:"num_cpu"`
	GOMAXPROCS int    `json:"gomaxprocs"`
}

func main() {
	iterations := flag.Uint64("iterations", 600_000_000, "number of single-threaded hash iterations")
	kind := flag.String("kind", "cpu-bench", "label to include in the result")
	flag.Parse()

	runtime.GOMAXPROCS(1)

	x := uint64(0x123456789abcdef0)
	start := time.Now()
	for i := uint64(0); i < *iterations; i++ {
		x += 0x9e3779b97f4a7c15
		x ^= x >> 30
		x *= 0xbf58476d1ce4e5b9
		x ^= x >> 27
		x *= 0x94d049bb133111eb
		x ^= x >> 31
	}
	duration := time.Since(start)
	durationMS := duration.Milliseconds()
	opsPerSec := uint64(0)
	if duration > 0 {
		opsPerSec = uint64(float64(*iterations) / duration.Seconds())
	}

	out := result{
		Kind:       *kind,
		Iterations: *iterations,
		DurationMS: durationMS,
		OpsPerSec:  opsPerSec,
		Checksum:   x,
		GoVersion:  runtime.Version(),
		NumCPU:     runtime.NumCPU(),
		GOMAXPROCS: runtime.GOMAXPROCS(0),
	}
	raw, err := json.Marshal(out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal result: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("CPU_BENCH_RESULT=%s\n", raw)
}
