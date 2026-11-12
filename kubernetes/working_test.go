package main

import (
	"context"
	"encoding/json"
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

func namedHandle(pod string) pluginsdk.Handle {
	data, _ := json.Marshal(handle{V: 1, Pod: pod, UID: "uid-working"})
	return pluginsdk.Handle{Data: data, Name: pod}
}

func platformErrors(t *testing.T, profile *Kubernetes) map[string]float64 {
	t.Helper()
	counted := map[string]float64{}
	for name, sample := range samples(t, profile) {
		if class, ok := strings.CutPrefix(name, "wherry_plugin_platform_errors_total/"); ok {
			counted[class] = sample.Value
		}
	}
	return counted
}

// holdReads makes every working file read wait for release, counting the reads.
func holdReads(t *testing.T) (release func(), reads *atomic.Int32) {
	t.Helper()
	saved := readWorkingFile
	held, reads := make(chan struct{}), &atomic.Int32{}
	readWorkingFile = func(string, string) ([]byte, error) {
		reads.Add(1)
		<-held
		return nil, nil
	}
	t.Cleanup(func() { readWorkingFile = saved })
	return func() { close(held) }, reads
}

func TestReadWorkingReadsTheRunsFile(t *testing.T) {
	profile := plainProfile(t, nil)
	pod := poolPrefix + "run-working"
	full := strings.Repeat("x", pluginsdk.MaxWorking)
	for content, want := range map[string]string{"reading the tests\n": "reading the tests\n", full: full, full + "x": ""} {
		writeOutputs(t, profile, pod, map[string]string{pluginsdk.WorkingFile: content})
		if data, err := profile.ReadWorking(context.Background(), namedHandle(pod)); err != nil || string(data) != want {
			t.Fatalf("read of %d bytes = %q, %v; want %q", len(content), data, err, want)
		}
	}
}

func TestReadWorkingRefusalsReadNothingUncounted(t *testing.T) {
	profile := plainProfile(t, nil)
	other := writeOutputs(t, profile, poolPrefix+"run-other", map[string]string{pluginsdk.WorkingFile: "other run\n", "secret": "other run\n"})
	for name, plant := range map[string]func(outputs string) error{
		"file link": func(outputs string) error {
			return os.Symlink(filepath.Join(other, "secret"), filepath.Join(outputs, pluginsdk.WorkingFile))
		},
		"directory link": func(outputs string) error {
			if err := os.RemoveAll(filepath.Join(outputs, ".wherry")); err != nil {
				return err
			}
			return os.Symlink(filepath.Join(other, ".wherry"), filepath.Join(outputs, ".wherry"))
		},
		"outputs link": func(outputs string) error {
			if err := os.RemoveAll(outputs); err != nil {
				return err
			}
			return os.Symlink(other, outputs)
		},
		"hard link": func(outputs string) error {
			return os.Link(filepath.Join(other, "secret"), filepath.Join(outputs, pluginsdk.WorkingFile))
		},
		"directory": func(outputs string) error { return os.Mkdir(filepath.Join(outputs, pluginsdk.WorkingFile), 0o755) },
		"FIFO": func(outputs string) error {
			return syscall.Mkfifo(filepath.Join(outputs, pluginsdk.WorkingFile), 0o644)
		},
		"unreadable": func(outputs string) error {
			if err := os.WriteFile(filepath.Join(outputs, pluginsdk.WorkingFile), []byte("step\n"), 0o644); err != nil {
				return err
			}
			return os.Chmod(filepath.Join(outputs, ".wherry"), 0)
		},
	} {
		t.Run(name, func(t *testing.T) {
			if name == "unreadable" && os.Geteuid() == 0 {
				t.Skip("root reads a file whatever its mode")
			}
			pod := poolPrefix + "run-" + strings.ReplaceAll(strings.ToLower(name), " ", "-")
			outputs := writeOutputs(t, profile, pod, map[string]string{".wherry/placeholder": ""})
			if err := plant(outputs); err != nil {
				t.Fatal(err)
			}
			if name == "unreadable" {
				t.Cleanup(func() { _ = os.Chmod(filepath.Join(outputs, ".wherry"), 0o755) })
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if data, err := profile.ReadWorking(ctx, namedHandle(pod)); data != nil || err != nil {
				t.Fatalf("read = %q, %v; want nothing", data, err)
			}
			if counted := platformErrors(t, profile); len(counted) > 0 {
				t.Fatalf("errors counted %v; want none", counted)
			}
		})
	}
}

func TestReadWorkingOfAPurgedRunReadsNothing(t *testing.T) {
	profile := plainProfile(t, nil)
	for _, pod := range []string{poolPrefix + "run-purged", poolPrefix + "run-unwritten"} {
		if pod == poolPrefix+"run-unwritten" {
			writeOutputs(t, profile, pod, nil)
		}
		if data, err := profile.ReadWorking(context.Background(), namedHandle(pod)); data != nil || err != nil {
			t.Fatalf("read of %s = %q, %v; want nothing", pod, data, err)
		}
	}
	if counted := platformErrors(t, profile); len(counted) > 0 {
		t.Fatalf("errors counted %v; want none", counted)
	}
}

func TestReadWorkingRefusesAPodOfAnotherForm(t *testing.T) {
	profile := plainProfile(t, nil)
	writeOutputs(t, profile, "elsewhere", map[string]string{pluginsdk.WorkingFile: "elsewhere\n"})
	writeOutputs(t, profile, "agent-000000000000-run-foreign", map[string]string{pluginsdk.WorkingFile: "another pool\n"})
	for _, pod := range []string{poolPrefix + "run/../../elsewhere", poolPrefix + "x/../../elsewhere", "agent-000000000000-run-foreign", poolPrefix + "Run"} {
		if data, err := profile.ReadWorking(context.Background(), namedHandle(pod)); data != nil || err == nil {
			t.Fatalf("read of pod %q = %q, %v; want refused", pod, data, err)
		}
	}
}

func TestReadWorkingReturnsOnTheContextWithOneReadInFlight(t *testing.T) {
	profile := plainProfile(t, nil)
	release, reads := holdReads(t)
	defer release()
	pod := poolPrefix + "run-hung"
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if data, err := readWithin(t, ctx, profile, namedHandle(pod)); data != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("read = %q, %v; want the deadline's error", data, err)
	}
	if data, err := readWithin(t, context.Background(), profile, namedHandle(pod)); data != nil || err != nil {
		t.Fatalf("second read = %q, %v; want nothing while the first is in flight", data, err)
	}
	if n := reads.Load(); n != 1 {
		t.Fatalf("reads started %d; want 1", n)
	}
	if counted := platformErrors(t, profile); counted["timeout"] != 1 {
		t.Fatalf("errors counted %v; want one timeout", counted)
	}
	if data, err := profile.ReadWorking(ctx, namedHandle(poolPrefix+"run-other")); data != nil || !errors.Is(err, context.DeadlineExceeded) || reads.Load() != 1 {
		t.Fatalf("read under an ended context = %q, %v with %d reads; want its error and no read", data, err, reads.Load())
	}
}

func TestReadWorkingCountsAFailedRead(t *testing.T) {
	profile := plainProfile(t, nil)
	saved := readWorkingFile
	readWorkingFile = func(string, string) ([]byte, error) {
		return nil, &fs.PathError{Op: "read", Path: "outputs/.wherry/working", Err: syscall.EIO}
	}
	t.Cleanup(func() { readWorkingFile = saved })
	var failure *pluginsdk.Error
	if data, err := profile.ReadWorking(context.Background(), namedHandle(poolPrefix+"run-eio")); data != nil || !errors.As(err, &failure) || failure.Class == "" {
		t.Fatalf("read = %q, %v; want a counted error", data, err)
	}
	if counted := platformErrors(t, profile); counted[failure.Class] != 1 {
		t.Fatalf("errors counted %v; want one", counted)
	}
}

// readWithin calls ReadWorking, failing the test unless it returns within 2 s.
func readWithin(t *testing.T, ctx context.Context, profile *Kubernetes, encoded pluginsdk.Handle) ([]byte, error) {
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
