package main

// The pieces of this file are the Nomad plugin's too, and move into the adapter module once both plugins are green.

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	wherry "github.com/benemon/wherry/adapter"
)

// maxPending bounds a line LineReader holds; a longer one is split.
const maxPending = 1 << 20

// PlatformMetrics holds an adapter's API errors, sweep deletions, cleanup-pending jobs and placement latency.
type PlatformMetrics struct {
	mu        sync.Mutex
	errors    map[string]float64
	deletions map[string]float64
	pending   float64
	placement wherry.Histogram
}

// NewPlatformMetrics returns empty metrics.
func NewPlatformMetrics() *PlatformMetrics {
	return &PlatformMetrics{errors: map[string]float64{}, deletions: map[string]float64{}, placement: wherry.Histogram{Buckets: make([]uint64, len(wherry.PlacementBuckets))}}
}

// Failed counts a failed platform call of class.
func (m *PlatformMetrics) Failed(class string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.errors[class]++
}

// Deleted counts n sweep deletions of kind.
func (m *PlatformMetrics) Deleted(kind string, n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletions[kind] += float64(n)
}

// Pending sets the ended jobs the latest sweep left.
func (m *PlatformMetrics) Pending(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending = float64(n)
}

// Placed observes one placement.
func (m *PlatformMetrics) Placed(seconds float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, bound := range wherry.PlacementBuckets {
		if seconds <= bound {
			m.placement.Buckets[i]++
		}
	}
	m.placement.Sum += seconds
	m.placement.Count++
}

// Samples reports the metrics as the adapter families' samples.
func (m *PlatformMetrics) Samples() []wherry.Sample {
	m.mu.Lock()
	defer m.mu.Unlock()
	placement := m.placement
	placement.Buckets = slices.Clone(placement.Buckets)
	samples := []wherry.Sample{
		{Family: "wherry_adapter_cleanup_pending", Kind: wherry.KindGauge, Value: m.pending},
		{Family: "wherry_adapter_placement_seconds", Kind: wherry.KindHistogram, Histogram: placement},
	}
	for class, count := range m.errors {
		samples = append(samples, wherry.Sample{Family: "wherry_adapter_platform_errors_total", Labels: map[string]string{"class": class}, Kind: wherry.KindCounter, Value: count})
	}
	for kind, count := range m.deletions {
		samples = append(samples, wherry.Sample{Family: "wherry_adapter_sweep_deletions_total", Labels: map[string]string{"kind": kind}, Kind: wherry.KindCounter, Value: count})
	}
	return samples
}

// AgentEnv returns configured without wherry.AgentUnset and with wherry.AgentEnvironment over it.
func AgentEnv(configured map[string]string) map[string]string {
	env := maps.Clone(configured)
	if env == nil {
		env = map[string]string{}
	}
	for _, name := range wherry.AgentUnset {
		delete(env, name)
	}
	maps.Copy(env, wherry.AgentEnvironment)
	return env
}

// Selected is one output Select keeps, marked for reading or streaming; an output marked neither is only listed.
type Selected struct {
	wherry.Output
	Read, Stream bool
}

// Select refuses more than limits.Files entries, drops directories, sorts by name and marks what Collect reads.
func Select(entries []wherry.Output, limits wherry.Limits) ([]Selected, error) {
	if len(entries) > limits.Files {
		return nil, fmt.Errorf("the outputs directory holds more than %d entries", limits.Files)
	}
	var selected []Selected
	for _, entry := range entries {
		if !entry.Mode.IsDir() {
			selected = append(selected, Selected{Output: entry})
		}
	}
	slices.SortFunc(selected, func(a, b Selected) int { return strings.Compare(a.Name, b.Name) })
	var total int64
	for i := range selected {
		output := &selected[i]
		switch {
		case output.Mode != 0:
		case slices.Contains(limits.Stream, output.Name):
			output.Stream = output.Size <= limits.StreamBytes
		case output.Size <= limits.FileBytes && total+output.Size <= limits.TotalBytes:
			total += output.Size
			output.Read = true
		}
	}
	return selected, nil
}

// LineReader splits bytes into lines; Position is the offset after the last emitted line.
type LineReader struct {
	Lines    chan<- wherry.Line
	Position int64
	pending  []byte
}

// Take adds data and emits every complete line, splitting one that grows past 1 MiB.
func (reader *LineReader) Take(ctx context.Context, data []byte) error {
	reader.pending = append(reader.pending, data...)
	for {
		end := bytes.IndexByte(reader.pending, '\n')
		if end < 0 && len(reader.pending) < maxPending {
			return nil
		}
		size, skip := end, 1
		if end < 0 {
			size, skip = len(reader.pending), 0
		}
		if err := reader.emit(ctx, reader.pending[:size], int64(size+skip)); err != nil {
			return err
		}
		reader.pending = reader.pending[size+skip:]
	}
}

// Flush emits a final line without a newline.
func (reader *LineReader) Flush(ctx context.Context) error {
	if len(reader.pending) == 0 {
		return nil
	}
	err := reader.emit(ctx, reader.pending, int64(len(reader.pending)))
	reader.pending = nil
	return err
}

func (reader *LineReader) emit(ctx context.Context, data []byte, advance int64) error {
	line := wherry.Line{Offset: reader.Position, Next: reader.Position + advance, Data: append([]byte(nil), data...)}
	select {
	case reader.Lines <- line:
		reader.Position = line.Next
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// reservedEnv reports whether the plugin sets the variable name for the agent, so a credential under it would be lost.
func reservedEnv(name string) bool {
	_, set := wherry.AgentEnvironment[name]
	return set || slices.Contains([]string{"HOME", "WHERRY_INPUT_DIR", "WHERRY_OUTPUT_DIR", "WHERRY_CREDENTIALS_DIR", "WHERRY_MODEL"}, name)
}
