package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	wherry "github.com/benemon/wherry/adapter"
	"github.com/godbus/dbus/v5"
)

func credentialedRun(name string) wherry.Run {
	run := testRun(name)
	run.Credential = []byte("model-key")
	run.Credentials = []wherry.Secret{{Name: "deploy", File: "deploy-key", Value: []byte("ssh-key\n")}}
	return run
}

func TestCredentialFilesAreRootOnly(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	run := credentialedRun("credential-files")
	dir := filepath.Join(credentialRoot, unitOf(run))
	checked := false
	f.set(func() {
		f.onStart = func(name string, props map[string]any) {
			if name != unitOf(run) {
				return
			}
			checked = true
			if got := mode(t, dir); got != 0o700 {
				t.Errorf("%s: mode %o; want 0700", dir, got)
			}
			for id, want := range map[string]string{agentCredential: "model-key", "deploy-key": "ssh-key\n"} {
				path := filepath.Join(dir, id)
				if got := mode(t, path); got != 0o600 {
					t.Errorf("%s: mode %o; want 0600", path, got)
				}
				if got, _ := os.ReadFile(path); string(got) != want {
					t.Errorf("%s holds %q; want %q", path, got, want)
				}
			}
			got := props["LoadCredential"].([]loadCredential)
			slices.SortFunc(got, func(a, b loadCredential) int { return strings.Compare(a.ID, b.ID) })
			want := []loadCredential{{agentCredential, filepath.Join(dir, agentCredential)}, {"deploy-key", filepath.Join(dir, "deploy-key")}}
			if !slices.Equal(got, want) {
				t.Errorf("LoadCredential %v; want %v", got, want)
			}
		}
	})
	dispatch(t, adapter, run)
	if !checked {
		t.Fatal("no unit started")
	}
	plain := testRun("no-credentials")
	dispatch(t, adapter, plain)
	if _, ok := f.properties(unitOf(plain))["LoadCredential"]; ok || exists(filepath.Join(credentialRoot, unitOf(plain))) {
		t.Fatal("a run without credentials has LoadCredential or a credential directory")
	}
}

func TestCredentialFilesRemovedAfterTheStart(t *testing.T) {
	newHost(t)
	adapter := newAgent(t, agentConfig("sleep 30"))
	run := credentialedRun("credentials-removed")
	dispatch(t, adapter, run)
	if exists(filepath.Join(credentialRoot, unitOf(run))) {
		t.Fatal("the credential files outlived the start")
	}
}

func TestCredentialFilesRemovedAfterAFailedStart(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	missing := credentialedRun("credentials-missing")
	f.set(func() {
		f.onStart = func(name string, _ map[string]any) {
			if name == unitOf(missing) {
				_ = os.Remove(filepath.Join(credentialRoot, name, "deploy-key"))
			}
		}
	})
	dispatch(t, adapter, missing)
	f.set(func() { f.onStart, f.result = nil, "timeout" })
	timedOut := credentialedRun("credentials-timeout")
	dispatch(t, adapter, timedOut)
	f.set(func() { f.result = "" })
	refused := credentialedRun("credentials-refused")
	f.fail("start", dbus.Error{Name: "org.freedesktop.DBus.Error.AccessDenied"})
	if _, _, err := adapter.Dispatch(context.Background(), refused); err == nil {
		t.Fatal("a refused start succeeded")
	}
	for _, run := range []wherry.Run{missing, timedOut} {
		if m := readMarkerOf(t, run); m.NeverStarted == nil {
			t.Fatalf("%s: marker %+v; want a failed start", run.ID, m)
		}
	}
	for _, run := range []wherry.Run{missing, timedOut, refused} {
		if exists(filepath.Join(credentialRoot, unitOf(run))) {
			t.Errorf("%s: the credential files outlived the failed start", run.ID)
		}
	}
}

func TestCredentialFileMissingFailsTheStart(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	run := credentialedRun("credential-missing")
	f.set(func() {
		f.onStart = func(name string, _ map[string]any) {
			_ = os.Remove(filepath.Join(credentialRoot, name, "deploy-key"))
		}
	})
	dispatch(t, adapter, run)
	if m := readMarkerOf(t, run); m.NeverStarted == nil || *m.NeverStarted != (neverStarted{"credentials", "systemd exit status 243/CREDENTIALS"}) {
		t.Fatalf("marker %+v; want never started, credentials", m)
	}
}

func TestSweepRemovesStaleCredentialFiles(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	stale, starting, foreign := unitName(poolPrefix+"run-stale"), unitName(poolPrefix+"run-starting"), unitName("agent-000000000000-run-x")
	for _, name := range []string{stale, starting, foreign} {
		if err := os.MkdirAll(filepath.Join(credentialRoot, name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(credentialRoot, name, agentCredential), []byte("key"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f.put(starting, capturedUnit(t, "activating"))
	sweep(t, adapter)
	if exists(filepath.Join(credentialRoot, stale)) || !exists(filepath.Join(credentialRoot, starting)) || !exists(filepath.Join(credentialRoot, foreign)) {
		t.Fatalf("stale %v, starting %v, foreign %v; want only the stale directory removed",
			exists(filepath.Join(credentialRoot, stale)), exists(filepath.Join(credentialRoot, starting)), exists(filepath.Join(credentialRoot, foreign)))
	}
}
