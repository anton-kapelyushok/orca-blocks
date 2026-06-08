package nbd

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

type Stats struct {
	Reads              int64            `json:"reads"`
	ReadBytes          int64            `json:"read_bytes"`
	Writes             int64            `json:"writes"`
	WriteBytes         int64            `json:"write_bytes"`
	Flushes            int64            `json:"flushes"`
	Disconnects        int64            `json:"disconnects"`
	SequentialReads    int64            `json:"sequential_reads"`
	NonSequentialReads int64            `json:"non_sequential_reads"`
	ReadSizeHistogram  map[string]int64 `json:"read_size_histogram"`
}

type StatsCollector struct {
	mu                sync.Mutex
	stats             Stats
	haveLastRead      bool
	lastReadEndOffset int64
}

func NewStatsCollector() *StatsCollector {
	return &StatsCollector{
		stats: Stats{ReadSizeHistogram: map[string]int64{}},
	}
}

func (c *StatsCollector) RecordRead(offset, length int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	c.stats.Reads++
	c.stats.ReadBytes += length
	if c.haveLastRead && offset == c.lastReadEndOffset {
		c.stats.SequentialReads++
	} else {
		c.stats.NonSequentialReads++
	}
	c.haveLastRead = true
	c.lastReadEndOffset = offset + length
	c.stats.ReadSizeHistogram[sizeBucket(length)]++
}

func (c *StatsCollector) RecordWrite(length int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	c.stats.Writes++
	c.stats.WriteBytes += length
}

func (c *StatsCollector) RecordFlush() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stats.Flushes++
}

func (c *StatsCollector) RecordDisconnect() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stats.Disconnects++
}

func (c *StatsCollector) Snapshot() Stats {
	if c == nil {
		return Stats{ReadSizeHistogram: map[string]int64{}}
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	out := c.stats
	out.ReadSizeHistogram = map[string]int64{}
	for k, v := range c.stats.ReadSizeHistogram {
		out.ReadSizeHistogram[k] = v
	}
	return out
}

func (c *StatsCollector) WriteJSON(w io.Writer) error {
	stats := c.Snapshot()
	raw, err := json.MarshalIndent(stats, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(raw))
	return err
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
