//go:build linux

package main

// The pieces of this file are the Kubernetes plugin's too.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/benemon/wherry/pluginsdk"
)

// maxPending bounds a line LineReader holds; a longer one is split.
const maxPending = 1 << 20

// PlatformMetrics holds a plugin profile's API errors, sweep deletions and cleanup-pending jobs.
type PlatformMetrics struct {
	mu        sync.Mutex
	errors    map[string]float64
	deletions map[string]float64
	pending   float64
}

// NewPlatformMetrics returns empty metrics.
func NewPlatformMetrics() *PlatformMetrics {
	return &PlatformMetrics{errors: map[string]float64{}, deletions: map[string]float64{}}
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

// Samples reports the metrics as the plugin families' samples.
func (m *PlatformMetrics) Samples() []pluginsdk.Sample {
	m.mu.Lock()
	defer m.mu.Unlock()
	samples := []pluginsdk.Sample{
		{Family: "wherry_plugin_cleanup_pending", Kind: pluginsdk.KindGauge, Value: m.pending},
	}
	for class, count := range m.errors {
		samples = append(samples, pluginsdk.Sample{Family: "wherry_plugin_platform_errors_total", Labels: map[string]string{"class": class}, Kind: pluginsdk.KindCounter, Value: count})
	}
	for kind, count := range m.deletions {
		samples = append(samples, pluginsdk.Sample{Family: "wherry_plugin_sweep_deletions_total", Labels: map[string]string{"kind": kind}, Kind: pluginsdk.KindCounter, Value: count})
	}
	return samples
}

// Selected is one output Select keeps, marked for reading or streaming; an output marked neither is only listed.
type Selected struct {
	pluginsdk.Output
	Read, Stream bool
}

// Select refuses more than limits.Files entries, drops directories, sorts by name and marks what Collect reads.
func Select(entries []pluginsdk.Output, limits pluginsdk.Limits) ([]Selected, error) {
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
	Lines    chan<- pluginsdk.Line
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
	line := pluginsdk.Line{Offset: reader.Position, Next: reader.Position + advance, Data: append([]byte(nil), data...)}
	select {
	case reader.Lines <- line:
		reader.Position = line.Next
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// readWorkingFile is pluginsdk.ReadWorkingFile, which a test replaces to hold a read.
var readWorkingFile = pluginsdk.ReadWorkingFile

// WorkingReads runs ReadWorkingFile, which takes no context, at most once at a time per run, so a read hung on a
// network file system holds one goroutine rather than one a call.
type WorkingReads struct {
	mu      sync.Mutex
	reading map[string]bool
}

// Read reads name beneath root for run, returning once ctx ends with its error. A refusal, and a call while the run's
// read is in flight, read as nothing; any other error is returned for the caller to count.
func (reads *WorkingReads) Read(ctx context.Context, run, root, name string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	reads.mu.Lock()
	if reads.reading[run] {
		reads.mu.Unlock()
		return nil, nil
	}
	if reads.reading == nil {
		reads.reading = map[string]bool{}
	}
	reads.reading[run] = true
	reads.mu.Unlock()
	type result struct {
		data []byte
		err  error
	}
	read := make(chan result, 1)
	go func() {
		defer func() {
			reads.mu.Lock()
			delete(reads.reading, run)
			reads.mu.Unlock()
		}()
		data, err := readWorkingFile(root, name)
		read <- result{data, err}
	}()
	select {
	case result := <-read:
		if errors.Is(result.err, pluginsdk.ErrRefused) {
			return nil, nil
		}
		return result.data, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
