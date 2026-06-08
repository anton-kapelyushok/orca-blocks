package storage

import (
	"bytes"
	"testing"
)

func BenchmarkLocalCacheReadGranularity(b *testing.B) {
	const (
		chunkSize = 4 * 1024 * 1024
		rangeSize = 4 * 1024
	)

	chunk := bytes.Repeat([]byte("a"), chunkSize)

	b.Run("full_chunk_for_4k", func(b *testing.B) {
		cache := newBenchmarkCache(b, 0, chunk)
		offsets := benchmarkOffsets(chunkSize, rangeSize, b.N)
		b.SetBytes(rangeSize)
		b.ReportAllocs()
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			got, ok, err := cache.Get("chunk")
			if err != nil || !ok {
				b.Fatalf("Get ok=%v err=%v", ok, err)
			}
			start := offsets[i]
			_ = got[start : start+rangeSize]
		}
	})

	b.Run("range_4k", func(b *testing.B) {
		cache := newBenchmarkCache(b, 0, chunk)
		offsets := benchmarkOffsets(chunkSize, rangeSize, b.N)
		b.SetBytes(rangeSize)
		b.ReportAllocs()
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			start := offsets[i]
			if _, ok, err := cache.GetRange("chunk", int64(start), rangeSize); err != nil || !ok {
				b.Fatalf("GetRange ok=%v err=%v", ok, err)
			}
		}
	})

	b.Run("memory_range_4k", func(b *testing.B) {
		cache := newBenchmarkCache(b, chunkSize, chunk)
		offsets := benchmarkOffsets(chunkSize, rangeSize, b.N)
		b.SetBytes(rangeSize)
		b.ReportAllocs()
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			start := offsets[i]
			if _, ok, err := cache.GetRange("chunk", int64(start), rangeSize); err != nil || !ok {
				b.Fatalf("GetRange ok=%v err=%v", ok, err)
			}
		}
	})

	b.Run("range_1m", func(b *testing.B) {
		const readSize = 1024 * 1024
		cache := newBenchmarkCache(b, 0, chunk)
		offsets := benchmarkOffsets(chunkSize, readSize, b.N)
		b.SetBytes(readSize)
		b.ReportAllocs()
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			start := offsets[i]
			if _, ok, err := cache.GetRange("chunk", int64(start), readSize); err != nil || !ok {
				b.Fatalf("GetRange ok=%v err=%v", ok, err)
			}
		}
	})
}

func newBenchmarkCache(tb testing.TB, memoryMax int64, chunk []byte) *LocalCache {
	tb.Helper()

	cache, err := NewLocalCacheWithMemory(tb.TempDir(), int64(len(chunk))*4, memoryMax)
	if err != nil {
		tb.Fatal(err)
	}
	if err := cache.Put("chunk", chunk); err != nil {
		tb.Fatal(err)
	}
	return cache
}

func benchmarkOffsets(chunkSize, readSize, count int) []int {
	offsets := make([]int, count)
	maxOffset := chunkSize - readSize
	for i := range offsets {
		offsets[i] = (i * 4099) % maxOffset
	}
	return offsets
}
