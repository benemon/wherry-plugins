//go:build linux

package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/benemon/wherry/pluginsdk"
)

const workingInstance = "0123456789abcdef0123456789abcdef"

// workingRun puts a running run's marker and its working file holding content, and returns its handle.
func workingRun(t *testing.T, name, content string) (pluginsdk.Run, pluginsdk.Handle) {
	t.Helper()
	run := testRun(name)
	putMarker(t, run, "started", nil)
	writeOutputs(t, run, map[string]string{pluginsdk.WorkingFile: content})
	return run, handleFor(unitOf(run), workingInstance)
}

func platformErrors(profile *Systemd) map[string]float64 {
	counted := map[string]float64{}
	for name, sample := range samples(profile) {
		if class, ok := strings.CutPrefix(name, "wherry_plugin_platform_errors_total/"); ok {
			counted[class] = sample.Value
		}
	}
	return counted
}

func TestReadWorkingReadsTheWorkDirectorysFile(t *testing.T) {
	newHost(t)
	profile := newAgent(t, agentConfig("exit 0"))
	full := strings.Repeat("x", pluginsdk.MaxWorking)
	for content, want := range map[string]string{"reading the tests\n": "reading the tests\n", full: full, full + "x": ""} {
		_, handle := workingRun(t, "working", content)
		if data, err := profile.ReadWorking(context.Background(), handle); err != nil || string(data) != want {
			t.Fatalf("read of %d bytes = %q, %v; want %q", len(content), data, err, want)
		}
	}
}

func TestReadWorkingOfAReplacedOrPurgedRunReadsNothing(t *testing.T) {
	newHost(t)
	profile := newAgent(t, agentConfig("exit 0"))
	run, _ := workingRun(t, "replaced", "the new attempt\n")
	if data, err := profile.ReadWorking(context.Background(), handleFor(unitOf(run), strings.Repeat("f", 32))); data != nil || err != nil {
		t.Fatalf("read through a replaced handle = %q, %v; want nothing", data, err)
	}
	purged, handle := workingRun(t, "purged", "left behind\n")
	if err := os.Remove(markerPath(jobOf(purged))); err != nil {
		t.Fatal(err)
	}
	if data, err := profile.ReadWorking(context.Background(), handle); data != nil || err != nil {
		t.Fatalf("read without a marker = %q, %v; want nothing", data, err)
	}
	gone, handle := workingRun(t, "gone", "")
	if err := os.RemoveAll(filepath.Join(agentRoot(true), jobOf(gone))); err != nil {
		t.Fatal(err)
	}
	if data, err := profile.ReadWorking(context.Background(), handle); data != nil || err != nil {
		t.Fatalf("read without a work directory = %q, %v; want nothing", data, err)
	}
	if counted := platformErrors(profile); len(counted) > 0 {
		t.Fatalf("errors counted %v; want none", counted)
	}
}

func TestReadWorkingRefusalsReadNothingUncounted(t *testing.T) {
	newHost(t)
	profile := newAgent(t, agentConfig("exit 0"))
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("host secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, plant := range map[string]func(outputs string) error{
		"file link": func(outputs string) error { return os.Symlink(outside, filepath.Join(outputs, pluginsdk.WorkingFile)) },
		"outputs link": func(outputs string) error {
			elsewhere := t.TempDir()
			if err := os.Mkdir(filepath.Join(elsewhere, ".wherry"), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(elsewhere, pluginsdk.WorkingFile), []byte("host secret\n"), 0o600); err != nil {
				return err
			}
			if err := os.RemoveAll(outputs); err != nil {
				return err
			}
			return os.Symlink(elsewhere, outputs)
		},
		"hard link": func(outputs string) error { return os.Link(outside, filepath.Join(outputs, pluginsdk.WorkingFile)) },
		"FIFO": func(outputs string) error {
			return syscall.Mkfifo(filepath.Join(outputs, pluginsdk.WorkingFile), 0o644)
		},
	} {
		t.Run(name, func(t *testing.T) {
			run, handle := workingRun(t, strings.ReplaceAll(strings.ToLower(name), " ", "-"), "")
			outputs := filepath.Join(agentRoot(true), jobOf(run), "outputs")
			if err := os.Remove(filepath.Join(outputs, pluginsdk.WorkingFile)); err != nil {
				t.Fatal(err)
			}
			if err := plant(outputs); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if data, err := profile.ReadWorking(ctx, handle); data != nil || err != nil {
				t.Fatalf("read = %q, %v; want nothing", data, err)
			}
			if counted := platformErrors(profile); len(counted) > 0 {
				t.Fatalf("errors counted %v; want none", counted)
			}
		})
	}
}

func TestReadWorkingReturnsOnTheContextWithOneReadInFlight(t *testing.T) {
	newHost(t)
	profile := newAgent(t, agentConfig("exit 0"))
	saved := readWorkingFile
	held, reads := make(chan struct{}), &atomic.Int32{}
	readWorkingFile = func(string, string) ([]byte, error) {
		reads.Add(1)
		<-held
		return nil, nil
	}
	t.Cleanup(func() { readWorkingFile = saved })
	defer close(held)
	_, handle := workingRun(t, "hung", "step\n")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if data, err := readWithin(t, ctx, profile, handle); data != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("read = %q, %v; want the deadline's error", data, err)
	}
	if data, err := readWithin(t, context.Background(), profile, handle); data != nil || err != nil {
		t.Fatalf("second read = %q, %v; want nothing while the first is in flight", data, err)
	}
	if n := reads.Load(); n != 1 {
		t.Fatalf("reads started %d; want 1", n)
	}
	if counted := platformErrors(profile); counted["timeout"] != 1 {
		t.Fatalf("errors counted %v; want one timeout", counted)
	}
}

func TestReadWorkingCountsAFailedRead(t *testing.T) {
	newHost(t)
	profile := newAgent(t, agentConfig("exit 0"))
	saved := readWorkingFile
	readWorkingFile = func(string, string) ([]byte, error) {
		return nil, &fs.PathError{Op: "read", Path: "outputs/.wherry/working", Err: syscall.EIO}
	}
	t.Cleanup(func() { readWorkingFile = saved })
	_, handle := workingRun(t, "eio", "step\n")
	var failure *pluginsdk.Error
	if data, err := profile.ReadWorking(context.Background(), handle); data != nil || !errors.As(err, &failure) || failure.Class == "" {
		t.Fatalf("read = %q, %v; want a counted error", data, err)
	}
	if counted := platformErrors(profile); counted[failure.Class] != 1 {
		t.Fatalf("errors counted %v; want one", counted)
	}
}

// readWithin calls ReadWorking, failing the test unless it returns within 2 s.
func readWithin(t *testing.T, ctx context.Context, profile *Systemd, encoded pluginsdk.Handle) ([]byte, error) {
	t.Helper()
	type result struct {
		data []byte
		err  error
	}
	read := make(chan result, 1)
	go func() {
		data, err := profile.ReadWorking(ctx, encoded)
		read <- result{data, err}
	}()
	select {
	case result := <-read:
		return result.data, result.err
	case <-time.After(2 * time.Second):
		t.Fatal("the read did not return within 2 s")
		return nil, nil
	}
}
