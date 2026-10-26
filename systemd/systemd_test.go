//go:build linux

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	wherry "github.com/benemon/wherry/adapter"
	"github.com/benemon/wherry/adapter/adaptertest"
	sd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/godbus/dbus/v5"
	"github.com/hashicorp/go-hclog"
)

const (
	credentialEnv = "WHERRY_TEST_CREDENTIAL"
	testPool      = "build"
	testProfile   = "host"
	// poolPrefix is agent- and the first 12 hex digits of sha256("build").
	poolPrefix = "agent-44575cf5b285-"
)

// captured is a unit's properties as systemd reports them, the form of testdata/*.json.
type captured struct {
	LoadState, ActiveState, SubState, Result, ControlGroup                string
	Transient                                                             bool
	MainPID                                                               uint32
	ExecMainCode, ExecMainStatus                                          int32
	ExecMainStartTimestamp, ExecMainExitTimestamp, InactiveEnterTimestamp uint64
}

var unloaded = captured{LoadState: "not-found", ActiveState: "inactive", SubState: "dead"}

type fakeUnit struct {
	captured
	pgid      int
	done      chan struct{}
	remain    bool
	lingering int
}

// fakeManager stands in for PID 1. It runs each unit's command as a child process in a process group of its own,
// which stands in for the unit's cgroup, with the unit's environment, working directory, log files and credentials,
// and no namespacing. Its fields after mu change under it; set them with set.
type fakeManager struct {
	t    *testing.T
	root string

	mu        sync.Mutex
	units     map[string]*fakeUnit
	starts    map[string][]sd.Property
	calls     []string
	errs      map[string][]error
	state     string
	version   string
	connected bool
	dials     int
	// result replaces the start job's result when set.
	result string
	// activating keeps started units activating.
	activating bool
	// unkillable makes a kill deliver nothing.
	unkillable bool
	// linger is how many reads a stopped unit stays loaded.
	linger int
	// onStart runs as a start begins and onRead as a unit's service properties are read, both outside mu.
	onStart func(name string, props map[string]any)
	onRead  func(name string)
}

// newHost roots the adapter's host paths in a temporary tree and dials a fake manager.
func newHost(t *testing.T) *fakeManager {
	t.Helper()
	root := t.TempDir()
	saved := []string{controlRoot, stateRoot, credentialRoot, cgroupRoot}
	savedUID, savedStatfs, savedDial := rootUID, statfs, dial
	controlRoot = filepath.Join(root, "var/lib/wherry/systemd/runs")
	stateRoot = filepath.Join(root, "var/lib")
	credentialRoot = filepath.Join(root, "run/wherry-systemd")
	cgroupRoot = filepath.Join(root, "sys/fs/cgroup")
	for _, dir := range []string{credentialRoot, stateRoot} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(credentialRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	rootUID = os.Geteuid()
	statfs = func(_ string, buf *syscall.Statfs_t) error {
		buf.Type = cgroup2Magic
		return nil
	}
	f := &fakeManager{t: t, root: root, units: map[string]*fakeUnit{}, starts: map[string][]sd.Property{}, errs: map[string][]error{},
		state: "running", version: "252-32.el9_4", connected: true}
	dial = func(context.Context) (manager, error) {
		if err := f.call("dial", ""); err != nil {
			return nil, err
		}
		f.set(func() { f.dials++; f.connected = true })
		return f, nil
	}
	t.Cleanup(func() {
		f.mu.Lock()
		for _, u := range f.units {
			if u.pgid > 0 {
				_ = syscall.Kill(-u.pgid, syscall.SIGKILL)
			}
		}
		f.mu.Unlock()
		controlRoot, stateRoot, credentialRoot, cgroupRoot = saved[0], saved[1], saved[2], saved[3]
		rootUID, statfs, dial = savedUID, savedStatfs, savedDial
	})
	return f
}

func (f *fakeManager) set(edit func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	edit()
}

// fail queues errors for a method's next calls; a nil entry lets its call through.
func (f *fakeManager) fail(method string, errs ...error) {
	f.set(func() { f.errs[method] = append(f.errs[method], errs...) })
}

func (f *fakeManager) call(method, detail string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.TrimSpace(method+" "+detail))
	if queue := f.errs[method]; len(queue) > 0 {
		f.errs[method] = queue[1:]
		return queue[0]
	}
	return nil
}

// recorded returns the calls whose method is one of methods.
func (f *fakeManager) recorded(methods ...string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var calls []string
	for _, call := range f.calls {
		if method, _, _ := strings.Cut(call, " "); slices.Contains(methods, method) {
			calls = append(calls, call)
		}
	}
	return calls
}

// put installs a unit with no process.
func (f *fakeManager) put(name string, state captured) {
	f.set(func() { f.units[name] = &fakeUnit{captured: state, done: closed()} })
}

func closed() chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}

func (f *fakeManager) unit(name string) captured {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u := f.units[name]; u != nil {
		return u.captured
	}
	return unloaded
}

func (f *fakeManager) edit(name string, edit func(*captured)) {
	f.set(func() { edit(&f.units[name].captured) })
}

// exited waits for a unit's main process to end.
func (f *fakeManager) exited(t *testing.T, name string) {
	t.Helper()
	f.mu.Lock()
	u := f.units[name]
	f.mu.Unlock()
	select {
	case <-u.done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: the agent did not exit", name)
	}
}

func (f *fakeManager) properties(name string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	props := map[string]any{}
	for _, p := range f.starts[name] {
		props[p.Name] = p.Value.Value()
	}
	return props
}

func (f *fakeManager) StartTransientUnitContext(_ context.Context, name, _ string, properties []sd.Property, ch chan<- string) (int, error) {
	if err := f.call("start", name); err != nil {
		return 0, err
	}
	for _, p := range properties {
		if env, ok := p.Value.Value().([]string); ok && p.Name == "Environment" {
			names := map[string]bool{}
			for _, entry := range env {
				variable, _, _ := strings.Cut(entry, "=")
				if names[variable] {
					return 0, dbus.Error{Name: "org.freedesktop.DBus.Error.InvalidArgs", Body: []any{"Invalid environment block."}}
				}
				names[variable] = true
			}
		}
	}
	f.mu.Lock()
	if u := f.units[name]; u != nil && u.LoadState == "loaded" {
		f.mu.Unlock()
		return 0, dbus.Error{Name: busUnitExists, Body: []any{"Unit " + name + " was already loaded or has a fragment file."}}
	}
	f.starts[name] = properties
	onStart := f.onStart
	f.mu.Unlock()
	props := f.properties(name)
	if onStart != nil {
		onStart(name, props)
	}
	u := &fakeUnit{captured: captured{LoadState: "loaded", ActiveState: "activating", SubState: "start", Transient: true, ControlGroup: "/wherry.slice/" + name},
		done: make(chan struct{}), remain: props["RemainAfterExit"] == true}
	result := f.run(name, u, props)
	f.mu.Lock()
	f.units[name] = u
	if f.result != "" {
		result = f.result
	}
	f.mu.Unlock()
	ch <- result
	return 1, nil
}

// run starts a unit's command as PID 1's child would, after reading its credentials; a start that fails sets the
// unit's systemd exit status.
func (f *fakeManager) run(name string, u *fakeUnit, props map[string]any) string {
	failStart := func(status int32) string {
		u.ActiveState, u.SubState, u.Result = "failed", "failed", "exit-code"
		u.ExecMainCode, u.ExecMainStatus = 1, status
		u.ExecMainStartTimestamp, u.ExecMainExitTimestamp = now(), now()
		close(u.done)
		return "failed"
	}
	store := filepath.Join(f.root, "credentials", name)
	credentials, _ := props["LoadCredential"].([]loadCredential)
	if err := os.MkdirAll(store, 0o700); err != nil {
		f.t.Error(err)
	}
	for _, credential := range credentials {
		data, err := os.ReadFile(credential.Path)
		if err != nil {
			return failStart(243)
		}
		if err := os.WriteFile(filepath.Join(store, credential.ID), data, 0o400); err != nil {
			f.t.Error(err)
		}
	}
	exe := props["ExecStartEx"].([]execStart)[0]
	var args []string
	for _, arg := range exe.Args {
		args = append(args, strings.ReplaceAll(arg, credentialsPath, store))
	}
	// A stream sent anywhere but a file reaches no file of the plugin's, as the journal would take it.
	logFile := func(property string) (*os.File, error) {
		if path, ok := props[property].(string); ok {
			return os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		}
		return os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	}
	stdout, err := logFile("StandardOutputFileToAppend")
	if err != nil {
		return failStart(209)
	}
	defer stdout.Close()
	stderr, err := logFile("StandardErrorFileToAppend")
	if err != nil {
		return failStart(222)
	}
	defer stderr.Close()
	cmd := &exec.Cmd{Path: exe.Path, Args: args, Dir: props["BindPaths"].([]bindPath)[0].Source, Stdout: stdout, Stderr: stderr,
		Env:         append([]string{"PATH=" + os.Getenv("PATH")}, props["Environment"].([]string)...),
		SysProcAttr: &syscall.SysProcAttr{Setpgid: true}}
	if err := cmd.Start(); err != nil {
		return failStart(203)
	}
	u.MainPID, u.pgid = uint32(cmd.Process.Pid), cmd.Process.Pid
	u.ActiveState, u.SubState, u.ExecMainStartTimestamp = "active", "running", now()
	f.mu.Lock()
	if f.activating {
		u.ActiveState = "activating"
	}
	f.mu.Unlock()
	go f.reap(u, cmd)
	return "done"
}

func now() uint64 { return uint64(time.Now().UnixMicro()) }

// reap records how a unit's main process ended, as systemd does for Type=exec.
func (f *fakeManager) reap(u *fakeUnit, cmd *exec.Cmd) {
	_ = cmd.Wait()
	status := cmd.ProcessState.Sys().(syscall.WaitStatus)
	f.mu.Lock()
	defer f.mu.Unlock()
	defer close(u.done)
	u.MainPID, u.ExecMainExitTimestamp = 0, now()
	var clean bool
	if status.Signaled() {
		u.ExecMainCode, u.ExecMainStatus, u.Result = 2, int32(status.Signal()), "signal"
		clean = status.Signal() == syscall.SIGTERM
	} else {
		u.ExecMainCode, u.ExecMainStatus, u.Result = 1, int32(status.ExitStatus()), "exit-code"
		clean = status.ExitStatus() == 0
	}
	switch {
	case !clean:
		u.ActiveState, u.SubState, u.InactiveEnterTimestamp = "failed", "failed", now()
	case u.remain:
		u.ActiveState, u.SubState, u.Result = "active", "exited", "success"
	default:
		_ = syscall.Kill(-u.pgid, syscall.SIGKILL)
		u.captured = unloaded
	}
}

// populated reports whether any live process remains in the unit's process group.
func (u *fakeUnit) populated() bool {
	if u.pgid == 0 {
		return false
	}
	stats, _ := filepath.Glob("/proc/[0-9]*/stat")
	for _, stat := range stats {
		data, err := os.ReadFile(stat)
		if err != nil {
			continue
		}
		fields := strings.Fields(string(data[bytes.LastIndexByte(data, ')')+1:]))
		if len(fields) > 2 && fields[0] != "Z" && fields[2] == strconv.Itoa(u.pgid) {
			return true
		}
	}
	return false
}

func (f *fakeManager) GetUnitPropertiesContext(_ context.Context, name string) (map[string]any, error) {
	if err := f.call("unit", name); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	state := unloaded
	if u := f.units[name]; u != nil {
		if u.lingering > 0 {
			if u.lingering--; u.lingering == 0 {
				u.captured = unloaded
			}
		}
		state = u.captured
	}
	return map[string]any{"LoadState": state.LoadState, "ActiveState": state.ActiveState, "SubState": state.SubState,
		"Transient": state.Transient, "InactiveEnterTimestamp": state.InactiveEnterTimestamp}, nil
}

func (f *fakeManager) GetUnitTypePropertiesContext(_ context.Context, name, unitType string) (map[string]any, error) {
	if err := f.call("service", name); err != nil {
		return nil, err
	}
	if unitType != "Service" {
		f.t.Errorf("read %s properties of %s", unitType, name)
	}
	f.mu.Lock()
	onRead := f.onRead
	f.mu.Unlock()
	if onRead != nil {
		onRead(name)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	state := unloaded
	if u := f.units[name]; u != nil {
		state = u.captured
		if state.ControlGroup != "" {
			events := filepath.Join(cgroupRoot, state.ControlGroup, "cgroup.events")
			populated := 0
			if u.populated() {
				populated = 1
			}
			if err := os.MkdirAll(filepath.Dir(events), 0o755); err != nil {
				f.t.Error(err)
			}
			if err := os.WriteFile(events, fmt.Appendf(nil, "populated %d\nfrozen 0\n", populated), 0o644); err != nil {
				f.t.Error(err)
			}
		}
	}
	return map[string]any{"Result": state.Result, "ControlGroup": state.ControlGroup, "MainPID": state.MainPID,
		"ExecMainCode": state.ExecMainCode, "ExecMainStatus": state.ExecMainStatus,
		"ExecMainStartTimestamp": state.ExecMainStartTimestamp, "ExecMainExitTimestamp": state.ExecMainExitTimestamp}, nil
}

func (f *fakeManager) KillUnitWithTarget(_ context.Context, name string, target sd.Who, signal int32) error {
	if err := f.call("kill", fmt.Sprintf("%s %s %d", name, target, signal)); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	u := f.units[name]
	switch {
	case u == nil || u.LoadState == "not-found":
		return dbus.Error{Name: busNoSuchUnit, Body: []any{"Unit " + name + " not loaded."}}
	case !u.populated():
		return dbus.Error{Name: "org.freedesktop.systemd1.NoSuchProcess", Body: []any{"No matching processes to kill"}}
	case f.unkillable:
		return nil
	case target == sd.Main && u.MainPID == 0:
		return dbus.Error{Name: "org.freedesktop.systemd1.NoSuchProcess", Body: []any{"No main process to kill"}}
	case target == sd.Main:
		return syscall.Kill(int(u.MainPID), syscall.Signal(signal))
	}
	return syscall.Kill(-u.pgid, syscall.Signal(signal))
}

func (f *fakeManager) StopUnitContext(_ context.Context, name, _ string, ch chan<- string) (int, error) {
	if err := f.call("stop", name); err != nil {
		return 0, err
	}
	f.mu.Lock()
	u := f.units[name]
	if u == nil || u.LoadState == "not-found" {
		f.mu.Unlock()
		return 0, dbus.Error{Name: busNoSuchUnit, Body: []any{"Unit " + name + " not loaded."}}
	}
	failed := u.ActiveState == "failed"
	f.mu.Unlock()
	if u.pgid > 0 {
		_ = syscall.Kill(-u.pgid, syscall.SIGKILL)
	}
	<-u.done
	f.set(func() {
		switch {
		case failed:
		case f.linger > 0:
			u.ActiveState, u.SubState, u.lingering = "inactive", "dead", f.linger
		default:
			u.captured = unloaded
		}
	})
	ch <- "done"
	return 1, nil
}

func (f *fakeManager) ResetFailedUnitContext(_ context.Context, name string) error {
	if err := f.call("reset", name); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	u := f.units[name]
	if u == nil || u.LoadState == "not-found" {
		return dbus.Error{Name: busNoSuchUnit, Body: []any{"Unit " + name + " not loaded."}}
	}
	if u.ActiveState == "failed" {
		u.captured = unloaded
	}
	return nil
}

func (f *fakeManager) SystemStateContext(context.Context) (*sd.Property, error) {
	if err := f.call("state", ""); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return &sd.Property{Name: "SystemState", Value: dbus.MakeVariant(f.state)}, nil
}

func (f *fakeManager) GetManagerProperty(prop string) (string, error) {
	if err := f.call("manager", prop); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return dbus.MakeVariant(f.version).String(), nil
}

func (f *fakeManager) Connected() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected
}

// fast shortens the adapter's polls and grace for one test.
func fast(t *testing.T) {
	t.Helper()
	saved := []time.Duration{waitPoll, followPoll, unitPoll, collectPoll, stopPoll, stopGrace}
	waitPoll, followPoll, unitPoll, collectPoll, stopPoll, stopGrace = 5*time.Millisecond, 5*time.Millisecond, 5*time.Millisecond, 5*time.Millisecond, 5*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() {
		waitPoll, followPoll, unitPoll, collectPoll, stopPoll, stopGrace = saved[0], saved[1], saved[2], saved[3], saved[4], saved[5]
	})
}

func agentConfig(script string) Config {
	return Config{Runtime: Runtime{
		Command: []string{"/bin/sh", "-c"}, Args: []string{script}, Env: map[string]string{"WHERRY_TEST_ENV": "from-runtime"},
	}}
}

func newAgent(t *testing.T, config Config) *Adapter {
	t.Helper()
	fast(t)
	adapter, err := New(config, testProfile, testPool, hclog.NewNullLogger())
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func testRun(name string) wherry.Run {
	return wherry.Run{ID: "run-" + name, Profile: testProfile, Agent: "coder", Attempt: 2, Prompt: []byte("hello-prompt")}
}

func unitOf(run wherry.Run) string { return unitName(jobName(testPool, run.ID)) }

func jobOf(run wherry.Run) string { return jobName(testPool, run.ID) }

func dispatch(t *testing.T, adapter *Adapter, run wherry.Run) wherry.Handle {
	t.Helper()
	outcome, handle, err := adapter.Dispatch(context.Background(), run)
	if err != nil || outcome != wherry.OutcomeCreated {
		t.Fatalf("dispatch %s: outcome %d, %v; want created", run.ID, outcome, err)
	}
	return handle
}

func followAll(t *testing.T, adapter *Adapter, handle wherry.Handle, from int64) ([]wherry.Line, wherry.Terminal, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	lines := make(chan wherry.Line)
	var got []wherry.Line
	done := make(chan struct{})
	go func() {
		defer close(done)
		for line := range lines {
			got = append(got, line)
		}
	}()
	terminal, err := adapter.Follow(ctx, handle, from, lines)
	close(lines)
	<-done
	return got, terminal, err
}

func texts(lines []wherry.Line) []string {
	var out []string
	for _, line := range lines {
		out = append(out, string(line.Data))
	}
	return out
}

func readMarkerOf(t *testing.T, run wherry.Run) *marker {
	t.Helper()
	m, err := readMarker(jobOf(run))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func samples(adapter *Adapter) map[string]wherry.Sample {
	got, _ := adapter.Metrics(context.Background())
	byKey := map[string]wherry.Sample{}
	for _, sample := range got {
		key := sample.Family
		for _, label := range []string{"class", "kind"} {
			if value, ok := sample.Labels[label]; ok {
				key += "/" + value
			}
		}
		byKey[key] = sample
	}
	return byKey
}

func capturedUnit(t *testing.T, name string) captured {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var state captured
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

// putMarker writes a run's directories and a marker in phase, as a Dispatch would have left them.
func putMarker(t *testing.T, run wherry.Run, phase string, edit func(*marker)) {
	t.Helper()
	job := jobOf(run)
	for _, dir := range []string{filepath.Join(controlRoot, job), filepath.Join(agentRoot(true), job, "inputs")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	m := &marker{V: 1, Pool: testPool, Profile: run.Profile, Agent: run.Agent, Run: run.ID, Attempt: run.Attempt, Instance: "0123456789abcdef0123456789abcdef", Phase: phase}
	if edit != nil {
		edit(m)
	}
	data, _ := json.Marshal(m)
	if err := os.WriteFile(markerPath(job), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentRoot(true), job, "prompt.txt"), run.Prompt, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, log := range []string{"stdout", "stderr"} {
		if err := os.WriteFile(filepath.Join(controlRoot, job, log), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func handleFor(name, instance string) wherry.Handle {
	data, _ := json.Marshal(handle{V: 1, Unit: name, Instance: instance})
	return wherry.Handle{Data: data, Name: name}
}

func exists(name string) bool {
	_, err := os.Lstat(name)
	return err == nil
}

func mode(t *testing.T, name string) fs.FileMode {
	t.Helper()
	info, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func TestUnitPropertiesFromRuntime(t *testing.T) {
	f := newHost(t)
	config := agentConfig("exec agent-run")
	config.Runtime.Resources = Resources{CPUQuota: new(150), MemoryMB: new(4096)}
	adapter := newAgent(t, config)
	run := testRun("props")
	dispatch(t, adapter, run)
	props := f.properties(unitOf(run))
	job := jobOf(run)
	want := map[string]any{
		"Description":                "wherry agent run-props (profile host)",
		"Slice":                      "wherry.slice",
		"Type":                       "exec",
		"RemainAfterExit":            true,
		"WorkingDirectory":           "/wherry",
		"StandardInput":              "null",
		"StandardOutputFileToAppend": filepath.Join(controlRoot, job, "stdout"),
		"StandardErrorFileToAppend":  filepath.Join(controlRoot, job, "stderr"),
		"StateDirectory":             []string{"wherry-agents/" + job},
		"StateDirectoryMode":         uint32(0o700),
		"BindPaths":                  []bindPath{{Source: filepath.Join(stateRoot, "private/wherry-agents", job), Destination: "/wherry", Flags: syscall.MS_REC}},
		"CPUQuotaPerSecUSec":         uint64(1500000),
		"MemoryMax":                  uint64(4096 << 20),
		"MemorySwapMax":              uint64(0),
		"KillMode":                   "control-group",
		"OOMPolicy":                  "stop",
	}
	for name, value := range want {
		if got := props[name]; fmt.Sprint(got) != fmt.Sprint(value) {
			t.Errorf("%s = %v; want %v", name, got, value)
		}
	}
	exe := props["ExecStartEx"].([]execStart)[0]
	if exe.Path != "/bin/sh" || !slices.Equal(exe.Args, []string{"/bin/sh", "-c", "exec agent-run"}) {
		t.Errorf("ExecStartEx %+v; want the runtime's command and args", exe)
	}
	if env := props["Environment"].([]string); !slices.Contains(env, "WHERRY_TEST_ENV=from-runtime") {
		t.Errorf("Environment %v lacks the runtime's env", env)
	}
	for _, name := range []string{"User", "Group", "SupplementaryGroups"} {
		if _, ok := props[name]; ok {
			t.Errorf("%s set without runtime.user", name)
		}
	}

	for i, c := range []struct {
		user, group, groups string
		wantUser, wantGroup string
	}{
		{"agent", "", "", "agent", ""},
		{"agent", "agents", "video,render", "agent", "agents"},
		{"1001:1002", "", "", "1001", "1002"},
		{"1001", "wheel", "", "1001", "wheel"},
	} {
		config := agentConfig("exit 0")
		config.Runtime.User, config.Runtime.Group = c.user, c.group
		if c.groups != "" {
			config.Runtime.SupplementaryGroups = strings.Split(c.groups, ",")
		}
		fixed := newAgent(t, config)
		run := testRun("user-" + strconv.Itoa(i))
		dispatch(t, fixed, run)
		props := f.properties(unitOf(run))
		if props["User"] != c.wantUser || fmt.Sprint(props["Group"]) != fmt.Sprint(cmpOrNil(c.wantGroup)) {
			t.Errorf("user %q group %q: User %v, Group %v; want %q, %q", c.user, c.group, props["User"], props["Group"], c.wantUser, c.wantGroup)
		}
		if c.groups != "" && !slices.Equal(props["SupplementaryGroups"].([]string), config.Runtime.SupplementaryGroups) {
			t.Errorf("SupplementaryGroups %v; want %v", props["SupplementaryGroups"], config.Runtime.SupplementaryGroups)
		}
		if _, ok := props["DynamicUser"]; ok {
			t.Errorf("user %q: DynamicUser set", c.user)
		}
		if _, ok := props["RemoveIPC"]; ok {
			t.Errorf("user %q: RemoveIPC set, which would remove other runs' IPC objects", c.user)
		}
		if bind := props["BindPaths"].([]bindPath)[0].Source; bind != filepath.Join(stateRoot, "wherry-agents", jobOf(run)) {
			t.Errorf("user %q: work directory %s; want it under %s", c.user, bind, filepath.Join(stateRoot, "wherry-agents"))
		}
	}
}

func cmpOrNil(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func TestUnitPropertiesDefaultResources(t *testing.T) {
	f := newHost(t)
	run := testRun("defaults")
	dispatch(t, newAgent(t, agentConfig("exit 0")), run)
	props := f.properties(unitOf(run))
	if props["CPUQuotaPerSecUSec"] != uint64(500000) || props["MemoryMax"] != uint64(2048<<20) {
		t.Fatalf("CPUQuotaPerSecUSec %v, MemoryMax %v; want 50%% of a CPU and 2 GiB", props["CPUQuotaPerSecUSec"], props["MemoryMax"])
	}
}

func TestUnitPropertiesSandbox(t *testing.T) {
	f := newHost(t)
	run := testRun("sandbox")
	dispatch(t, newAgent(t, agentConfig("exit 0")), run)
	props := f.properties(unitOf(run))
	want := map[string]any{
		"DynamicUser": true, "RemoveIPC": true, "ProtectSystem": "strict", "ProtectHome": "yes", "PrivateTmp": true,
		"PrivateDevices": true, "PrivateIPC": true, "NoNewPrivileges": true, "RestrictSUIDSGID": true,
		"ProtectKernelTunables": true, "ProtectKernelModules": true, "ProtectKernelLogs": true, "ProtectControlGroups": true,
		"ProtectClock": true, "ProtectHostname": true, "ProtectProc": "invisible", "LockPersonality": true, "RestrictRealtime": true,
		"SystemCallArchitectures": []string{"native"}, "CapabilityBoundingSet": uint64(0), "KeyringMode": "private", "UMask": uint32(0o077),
		"RestrictAddressFamilies": addressFamilies{Allow: true, Families: []string{"AF_UNIX", "AF_INET", "AF_INET6", "AF_NETLINK"}},
		"InaccessiblePaths":       []string{"/var/lib/wherry", "-/etc/wherry", "/run/wherry-systemd", "/run/systemd/transient", "-/run/systemd/journal"},
		"BindReadOnlyPaths":       []bindPath{{Source: "/run/credentials/" + unitOf(run), Destination: "/run/wherry/credentials", IgnoreMissing: true, Flags: syscall.MS_REC}},
	}
	for name, value := range want {
		if got := props[name]; fmt.Sprintf("%#v", got) != fmt.Sprintf("%#v", value) {
			t.Errorf("%s = %#v; want %#v", name, got, value)
		}
	}
	for _, name := range []string{"AmbientCapabilities", "SetCredential", "SetCredentialEncrypted", "Restart", "User", "PrivateNetwork", "MemoryDenyWriteExecute", "RestrictNamespaces", "SystemCallFilter", "PrivateUsers"} {
		if _, ok := props[name]; ok {
			t.Errorf("%s is set", name)
		}
	}
}

// signatures are the D-Bus signatures systemd v252 reads each property with, from dbus-execute.c, dbus-service.c,
// dbus-cgroup.c and dbus-unit.c.
var signatures = map[string]string{
	"Description": "s", "Slice": "s", "Type": "s", "RemainAfterExit": "b", "ExecStartEx": "a(sasas)", "Environment": "as",
	"WorkingDirectory": "s", "StandardInput": "s", "StandardOutputFileToAppend": "s", "StandardErrorFileToAppend": "s",
	"DynamicUser": "b", "User": "s", "Group": "s", "SupplementaryGroups": "as", "StateDirectory": "as", "StateDirectoryMode": "u",
	"BindPaths": "a(ssbt)", "BindReadOnlyPaths": "a(ssbt)", "LoadCredential": "a(ss)", "InaccessiblePaths": "as", "LimitCORE": "t",
	"ProtectSystem": "s", "ProtectHome": "s", "PrivateTmp": "b", "PrivateDevices": "b", "PrivateIPC": "b",
	"NoNewPrivileges": "b", "RestrictSUIDSGID": "b", "RemoveIPC": "b", "ProtectKernelTunables": "b", "ProtectKernelModules": "b",
	"ProtectKernelLogs": "b", "ProtectControlGroups": "b", "ProtectClock": "b", "ProtectHostname": "b", "ProtectProc": "s",
	"LockPersonality": "b", "RestrictRealtime": "b", "RestrictAddressFamilies": "(bas)", "SystemCallArchitectures": "as",
	"CapabilityBoundingSet": "t", "KeyringMode": "s", "UMask": "u", "CPUQuotaPerSecUSec": "t", "MemoryMax": "t",
	"MemorySwapMax": "t", "KillMode": "s", "TimeoutStartUSec": "t", "TimeoutStopUSec": "t", "OOMPolicy": "s",
}

func TestUnitPropertiesMatchSystemdSignatures(t *testing.T) {
	f := newHost(t)
	config := agentConfig("exit 0")
	config.Runtime.User, config.Runtime.Group, config.Runtime.SupplementaryGroups = "agent", "agents", []string{"video"}
	run := testRun("signatures")
	run.Credentials = []wherry.Secret{{Name: "model", Env: credentialEnv, Value: []byte("model-key")}}
	dispatch(t, newAgent(t, agentConfig("exit 0")), run)
	fixed := testRun("signatures-fixed")
	dispatch(t, newAgent(t, config), fixed)
	seen := map[string]bool{}
	for _, name := range []string{unitOf(run), unitOf(fixed)} {
		f.mu.Lock()
		properties := f.starts[name]
		f.mu.Unlock()
		for _, p := range properties {
			seen[p.Name] = true
			if want, ok := signatures[p.Name]; !ok {
				t.Errorf("%s has no signature in the table", p.Name)
			} else if got := p.Value.Signature().String(); got != want {
				t.Errorf("%s sent as %s; systemd reads %s", p.Name, got, want)
			}
		}
	}
	for name := range signatures {
		if !seen[name] {
			t.Errorf("%s never sent", name)
		}
	}
}

func TestUnitPropertiesHoldNoMaterial(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	object := []byte("fetched-input-secret")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(object) }))
	defer server.Close()
	run := testRun("no-material")
	run.Prompt = []byte("prompt-secret")
	run.Model = "opus"
	run.Inputs = []wherry.File{{Name: "in.txt", Data: []byte("inline-input-secret")}}
	run.Credentials = []wherry.Secret{{Name: "model", Env: credentialEnv, Value: []byte("credential-secret")}, {Name: "deploy", File: "deploy-key", Value: []byte("task-credential-secret")}}
	run.Fetches = []wherry.Fetch{{Name: "object.bin", URL: server.URL + "/object?X-Amz-Signature=presigned-url-secret", Size: int64(len(object)), SHA256: sha(object)}}
	dispatch(t, adapter, run)
	var seen strings.Builder
	f.mu.Lock()
	for _, p := range f.starts[unitOf(run)] {
		fmt.Fprintf(&seen, "%s=%s\n", p.Name, p.Value.Value())
	}
	f.mu.Unlock()
	for _, secret := range []string{"prompt-secret", "credential-secret", "inline-input-secret", "task-credential-secret", "fetched-input-secret", "presigned-url-secret"} {
		if strings.Contains(seen.String(), secret) {
			t.Errorf("%s reached a unit property:\n%s", secret, seen.String())
		}
	}
}

func TestExecStartDisablesEnvExpansion(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("echo $HOME"))
	plain, credentialed := testRun("expand"), testRun("expand-credential")
	credentialed.Credentials = []wherry.Secret{{Name: "model", Env: credentialEnv, Value: []byte("key")}}
	for _, run := range []wherry.Run{plain, credentialed} {
		dispatch(t, adapter, run)
		exe := f.properties(unitOf(run))["ExecStartEx"].([]execStart)
		if len(exe) != 1 || !slices.Equal(exe[0].Flags, []string{"no-env-expand"}) {
			t.Errorf("%s: ExecStartEx %+v; want one command with no-env-expand", run.ID, exe)
		}
	}
}

func TestCredentialWrapperPreservesBytes(t *testing.T) {
	store := t.TempDir()
	stored := filepath.Join(store, envCredential+credentialEnv)
	for _, value := range []string{"plain", "trailing newlines\n\n", "  leading spaces", `quotes ' " and $HOME`, `back\slash\n`, "non-utf8 \xff\xfe"} {
		if err := os.WriteFile(stored, []byte(value), 0o400); err != nil {
			t.Fatal(err)
		}
		script := strings.ReplaceAll(wrapper([]string{credentialEnv}), credentialsPath, store)
		output, err := exec.Command("/bin/sh", "-c", script, "wherry-agent", "/bin/sh", "-c", `printf %s "$`+credentialEnv+`"`).Output()
		if err != nil {
			t.Fatal(err)
		}
		if string(output) != value {
			t.Errorf("the agent read %q; want %q", output, value)
		}
		if err := os.Chmod(stored, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(stored); err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(wrapper([]string{credentialEnv}), credentialsPath, store)
	if err := exec.Command("/bin/sh", "-c", script, "wherry-agent", "/bin/true").Run(); err == nil {
		t.Fatal("the wrapper ran the agent without its credential")
	}
}

func TestCredentialWrapperOnlyWithACredential(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("exec agent-run"))
	plain, files, credentialed := testRun("wrapper"), testRun("wrapper-files"), testRun("wrapper-credential")
	files.Credentials = []wherry.Secret{{Name: "deploy", File: "deploy-key", Value: []byte("key")}}
	credentialed.Credentials = []wherry.Secret{{Name: "model", Env: credentialEnv, Value: []byte("key")}, files.Credentials[0], {Name: "second", Env: "SECOND_TOKEN", Value: []byte("key")}}
	for _, run := range []wherry.Run{plain, files, credentialed} {
		dispatch(t, adapter, run)
	}
	for _, run := range []wherry.Run{plain, files} {
		if exe := f.properties(unitOf(run))["ExecStartEx"].([]execStart)[0]; !slices.Equal(exe.Args, []string{"/bin/sh", "-c", "exec agent-run"}) {
			t.Errorf("%s: argv %q; want the runtime command alone", run.ID, exe.Args)
		}
	}
	want := []string{"/bin/sh", "-c", wrapper([]string{credentialEnv, "SECOND_TOKEN"}), "wherry-agent", "/bin/sh", "-c", "exec agent-run"}
	if exe := f.properties(unitOf(credentialed))["ExecStartEx"].([]execStart)[0]; exe.Path != "/bin/sh" || !slices.Equal(exe.Args, want) {
		t.Errorf("with a credential: argv %q; want the wrapper over the runtime command", exe.Args)
	}
}

func TestConformanceCredentialsReachTheAgent(t *testing.T) {
	newHost(t)
	adapter := newAgent(t, agentConfig(`printf '%s\n' "$CONFORMANCE_TOKEN"; cat `+credentialsPath+`/conformance-key; echo`))
	run := testRun("credential-targets")
	run.Credentials = adaptertest.Credentials
	lines, terminal, err := followAll(t, adapter, dispatch(t, adapter, run), 0)
	if err != nil || terminal.ExitCode != 0 || !slices.Equal(texts(lines), []string{"conformance-env-value", "conformance-file-value"}) {
		t.Fatalf("lines %q, terminal %+v, %v; want the env credential, then the file credential", texts(lines), terminal, err)
	}
}

func TestTwoCredentialsOfEachTargetReachTheAgent(t *testing.T) {
	newHost(t)
	adapter := newAgent(t, agentConfig(`printf '%s\n' "$`+credentialEnv+`" "$SECOND_TOKEN"; cat `+credentialsPath+`/deploy-key `+credentialsPath+`/token`))
	run := testRun("credential-pairs")
	run.Credentials = []wherry.Secret{
		{Name: "model", Env: credentialEnv, Value: []byte("model-key")},
		{Name: "deploy", File: "deploy-key", Value: []byte("deploy-value\n")},
		{Name: "second", Env: "SECOND_TOKEN", Value: []byte("second-token")},
		{Name: "token", File: "token", Value: []byte("token-value\n")},
	}
	lines, terminal, err := followAll(t, adapter, dispatch(t, adapter, run), 0)
	if err != nil || terminal.ExitCode != 0 || !slices.Equal(texts(lines), []string{"model-key", "second-token", "deploy-value", "token-value"}) {
		t.Fatalf("lines %q, terminal %+v, %v; want both env credentials, then both file credentials", texts(lines), terminal, err)
	}
}

func envOf(f *fakeManager, run wherry.Run) []string {
	return f.properties(unitOf(run))["Environment"].([]string)
}

func TestAgentEnvironmentReachesTheAgent(t *testing.T) {
	f := newHost(t)
	config := agentConfig("exit 0")
	config.Runtime.Env = map[string]string{"GIT_SSH_COMMAND": "ssh -i /tmp/key", "GIT_DIR": "/elsewhere", "AGENT_MODE": "ci", "GIT_TRACE": "1"}
	run := testRun("agent-env")
	dispatch(t, newAgent(t, config), run)
	env := envOf(f, run)
	for name, value := range wherry.AgentEnvironment {
		if !slices.Contains(env, name+"="+value) {
			t.Errorf("Environment lacks %s=%s", name, value)
		}
	}
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if slices.Contains(wherry.AgentUnset, name) {
			t.Errorf("Environment holds %s", entry)
		}
	}
	if !slices.Contains(env, "AGENT_MODE=ci") || slices.Contains(env, "GIT_TRACE=1") {
		t.Errorf("Environment %v; want the runtime's own variables under AgentEnvironment", env)
	}
}

func TestUnitEnvCarriesTheModelOnlyWhenSet(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	plain, modelled := testRun("no-model"), testRun("model")
	modelled.Model = "opus"
	dispatch(t, adapter, plain)
	dispatch(t, adapter, modelled)
	for _, entry := range envOf(f, plain) {
		if strings.HasPrefix(entry, "WHERRY_MODEL=") {
			t.Errorf("without a model: %s", entry)
		}
	}
	if env := envOf(f, modelled); env[len(env)-1] != "WHERRY_MODEL=opus" {
		t.Errorf("with a model: %v; want WHERRY_MODEL=opus last", env)
	}
	want := []string{"HOME=/wherry", "WHERRY_INPUT_DIR=/wherry/inputs", "WHERRY_OUTPUT_DIR=/wherry/outputs", "WHERRY_CREDENTIALS_DIR=/run/wherry/credentials"}
	if env := envOf(f, plain); !slices.Equal(env[len(env)-4:], want) {
		t.Errorf("Environment ends %v; want %v", env[len(env)-4:], want)
	}
}

func TestUnitEnvSetsAnOverridableShell(t *testing.T) {
	f := newHost(t)
	plain := newAgent(t, agentConfig(`printf '%s\n' "$SHELL"`))
	config := agentConfig(`printf '%s\n' "$SHELL"`)
	config.Runtime.Env["SHELL"] = "/bin/bash"
	bash := newAgent(t, config)
	for _, c := range []struct {
		adapter *Adapter
		run     wherry.Run
		want    string
	}{{plain, testRun("shell"), "/bin/sh"}, {bash, testRun("shell-bash"), "/bin/bash"}} {
		handle := dispatch(t, c.adapter, c.run)
		if env := envOf(f, c.run); !slices.Equal(slices.DeleteFunc(env, func(e string) bool { return !strings.HasPrefix(e, "SHELL=") }), []string{"SHELL=" + c.want}) {
			t.Errorf("%s: Environment sets SHELL as %q; want SHELL=%s once", c.run.ID, env, c.want)
		}
		lines, _, err := followAll(t, c.adapter, handle, 0)
		if err != nil || !slices.Equal(texts(lines), []string{c.want}) {
			t.Errorf("%s: the agent's SHELL %q, %v; want %s", c.run.ID, texts(lines), err, c.want)
		}
	}
}

func TestUnitEnvSetsThePluginsVariablesOnce(t *testing.T) {
	newHost(t)
	config := agentConfig(`printf '%s %s\n' "$HOME" "$WHERRY_INPUT_DIR"`)
	config.Runtime.Env["HOME"] = "/elsewhere"
	config.Runtime.Env["WHERRY_INPUT_DIR"] = "/elsewhere/inputs"
	adapter := newAgent(t, config)
	lines, _, err := followAll(t, adapter, dispatch(t, adapter, testRun("home")), 0)
	if err != nil || !slices.Equal(texts(lines), []string{"/wherry /wherry/inputs"}) {
		t.Fatalf("the agent's HOME and WHERRY_INPUT_DIR %q, %v; want the plugin's", texts(lines), err)
	}
}

func TestUnitPropertiesBoundTheStart(t *testing.T) {
	f := newHost(t)
	run := testRun("timeouts")
	dispatch(t, newAgent(t, agentConfig("exit 0")), run)
	props := f.properties(unitOf(run))
	if props["TimeoutStartUSec"] != uint64(30_000_000) || props["TimeoutStopUSec"] != uint64(15_000_000) {
		t.Fatalf("TimeoutStartUSec %v, TimeoutStopUSec %v; want 30 s and 15 s", props["TimeoutStartUSec"], props["TimeoutStopUSec"])
	}
}

func TestUnitPropertiesLimitCoreDumps(t *testing.T) {
	f := newHost(t)
	run := testRun("core")
	dispatch(t, newAgent(t, agentConfig("exit 0")), run)
	if got := f.properties(unitOf(run))["LimitCORE"]; got != uint64(1) {
		t.Fatalf("LimitCORE %v; want 1, which aborts a dump to a pipe handler", got)
	}
}

func TestDirectoriesAreFixed(t *testing.T) {
	newHost(t)
	got := newAgent(t, agentConfig("exit 0")).Directories()
	if got != (wherry.Directories{Input: "/wherry/inputs", Output: "/wherry/outputs", Credentials: "/run/wherry/credentials"}) {
		t.Fatalf("directories %+v; want the paths each unit sees", got)
	}
}

func TestInlineLimitIsZero(t *testing.T) {
	newHost(t)
	if got := newAgent(t, agentConfig("exit 0")).InlineLimit(); got != 0 {
		t.Fatalf("inline limit %d; want 0, no limit of the plugin's", got)
	}
}

func TestPluginImportsNoCgo(t *testing.T) {
	output, err := exec.Command("go", "list", "-deps", "-f", "{{if and .CgoFiles (not .Standard)}}{{.ImportPath}}{{end}}", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, output)
	}
	if got := strings.TrimSpace(string(output)); got != "" {
		t.Fatalf("the plugin imports packages with cgo files:\n%s", got)
	}
}

func TestNewValidatesConfig(t *testing.T) {
	newHost(t)
	cases := map[string]func(*Config){
		"no command":                    func(c *Config) { c.Runtime.Command = nil },
		"relative command":              func(c *Config) { c.Runtime.Command = []string{"sh", "-c"} },
		"NUL in command":                func(c *Config) { c.Runtime.Command = []string{"/bin/sh", "-c\x00"} },
		"NUL in args":                   func(c *Config) { c.Runtime.Args = []string{"a\x00b"} },
		"non-UTF-8 args":                func(c *Config) { c.Runtime.Args = []string{"\xff"} },
		"env name":                      func(c *Config) { c.Runtime.Env = map[string]string{"BAD-NAME": "x"} },
		"env CREDENTIALS_DIRECTORY":     func(c *Config) { c.Runtime.Env = map[string]string{"CREDENTIALS_DIRECTORY": "/tmp"} },
		"env STATE_DIRECTORY":           func(c *Config) { c.Runtime.Env = map[string]string{"STATE_DIRECTORY": "/tmp"} },
		"env INVOCATION_ID":             func(c *Config) { c.Runtime.Env = map[string]string{"INVOCATION_ID": "x"} },
		"env value NUL":                 func(c *Config) { c.Runtime.Env = map[string]string{"A": "x\x00"} },
		"cpu_quota 0":                   func(c *Config) { c.Runtime.Resources.CPUQuota = new(0) },
		"cpu_quota over the CPUs":       func(c *Config) { c.Runtime.Resources.CPUQuota = new(100*runtime.NumCPU() + 1) },
		"memory_mb 63":                  func(c *Config) { c.Runtime.Resources.MemoryMB = new(63) },
		"user root":                     func(c *Config) { c.Runtime.User = "root" },
		"user 0":                        func(c *Config) { c.Runtime.User = "0" },
		"user 0:1000":                   func(c *Config) { c.Runtime.User = "0:1000" },
		"user 1000:0":                   func(c *Config) { c.Runtime.User = "1000:0" },
		"user name:GID":                 func(c *Config) { c.Runtime.User = "agent:1000" },
		"user 1000:name":                func(c *Config) { c.Runtime.User = "1000:agents" },
		"user leading zero":             func(c *Config) { c.Runtime.User = "01000" },
		"user over 32 bits":             func(c *Config) { c.Runtime.User = "4294967296" },
		"user with space":               func(c *Config) { c.Runtime.User = "a gent" },
		"group root":                    func(c *Config) { c.Runtime.User, c.Runtime.Group = "agent", "root" },
		"group 0":                       func(c *Config) { c.Runtime.User, c.Runtime.Group = "agent", "0" },
		"group beside a GID":            func(c *Config) { c.Runtime.User, c.Runtime.Group = "1000:1000", "agents" },
		"supplementary root":            func(c *Config) { c.Runtime.SupplementaryGroups = []string{"video", "root"} },
		"supplementary with whitespace": func(c *Config) { c.Runtime.SupplementaryGroups = []string{"vid eo"} },
	}
	for name, edit := range cases {
		config := agentConfig("exit 0")
		edit(&config)
		if _, err := New(config, testProfile, testPool, hclog.NewNullLogger()); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for name, edit := range map[string]func(*Config){
		"defaults":  func(*Config) {},
		"user name": func(c *Config) { c.Runtime.User = "agent" },
		"UID":       func(c *Config) { c.Runtime.User = "1000" },
		"UID:GID":   func(c *Config) { c.Runtime.User = "1000:1000" },
		"groups": func(c *Config) {
			c.Runtime.User, c.Runtime.Group, c.Runtime.SupplementaryGroups = "agent", "agents", []string{"video", "1005"}
		},
		"bounds": func(c *Config) { c.Runtime.Resources = Resources{CPUQuota: new(1), MemoryMB: new(64)} },
		"upper bounds": func(c *Config) {
			c.Runtime.Resources = Resources{CPUQuota: new(100 * runtime.NumCPU()), MemoryMB: new(1<<31 - 1)}
		},
		"SHELL": func(c *Config) { c.Runtime.Env = map[string]string{"SHELL": "/bin/bash"} },
	} {
		config := agentConfig("exit 0")
		edit(&config)
		if _, err := New(config, testProfile, testPool, hclog.NewNullLogger()); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestConfigureRefusesOldSystemdAndCgroupV1(t *testing.T) {
	f := newHost(t)
	f.set(func() { f.version = "251.9" })
	if _, err := New(agentConfig("exit 0"), testProfile, testPool, hclog.NewNullLogger()); err == nil || err.Error() != "systemd 252 or later is required" {
		t.Fatalf("systemd 251: %v", err)
	}
	f.set(func() { f.version = "252" })
	statfs = func(_ string, buf *syscall.Statfs_t) error {
		buf.Type = 0x01021994 // tmpfs, as /sys/fs/cgroup is under the hybrid hierarchy
		return nil
	}
	if _, err := New(agentConfig("exit 0"), testProfile, testPool, hclog.NewNullLogger()); err == nil || err.Error() != "systemd's unified cgroup hierarchy is required" {
		t.Fatalf("cgroup v1: %v", err)
	}
}

func TestConfigureParsesTheManagerVersion(t *testing.T) {
	for version, want := range map[string]bool{
		`"252-32.el9_4"`: true, `"255.4-1ubuntu8"`: true, `"252"`: true, `"260"`: true,
		`"251.9"`: false, `"v252"`: false, `""`: false, `"25"`: false,
	} {
		if got := supported(version); got != want {
			t.Errorf("supported(%s) = %v; want %v", version, got, want)
		}
	}
}

func TestConfigureRefusesANonRootPlugin(t *testing.T) {
	newHost(t)
	rootUID = os.Geteuid() + 1
	if _, err := New(agentConfig("exit 0"), testProfile, testPool, hclog.NewNullLogger()); err == nil || err.Error() != "the systemd plugin runs as root" {
		t.Fatalf("got %v; want the plugin refused", err)
	}
}

func TestConfigureRefusesAnOpenCredentialDirectory(t *testing.T) {
	f := newHost(t)
	want := credentialRoot + " must be root 0700"
	if err := os.Chmod(credentialRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := New(agentConfig("exit 0"), testProfile, testPool, hclog.NewNullLogger()); err == nil || err.Error() != want {
		t.Fatalf("mode 0755: %v; want %q", err, want)
	}
	if got := f.recorded("dial"); len(got) != 0 {
		t.Fatalf("Configure dialled systemd before refusing: %v", got)
	}
	if err := os.Chmod(credentialRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	rootUID = os.Geteuid() + 1
	if err := checkCredentialRoot(); err == nil || err.Error() != want {
		t.Fatalf("another owner: %v; want %q", err, want)
	}
	rootUID = os.Geteuid()
	target := credentialRoot + ".real"
	if err := os.Rename(credentialRoot, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, credentialRoot); err != nil {
		t.Fatal(err)
	}
	if err := checkCredentialRoot(); err == nil {
		t.Fatal("accepted a symlink")
	}
	if err := os.Remove(credentialRoot); err != nil {
		t.Fatal(err)
	}
	if err := checkCredentialRoot(); err == nil {
		t.Fatal("accepted an absent directory")
	}
}

func TestUnitNames(t *testing.T) {
	if got := unitName(jobName(testPool, "run-1")); got != "wherry-"+poolPrefix+"run-1.service" {
		t.Fatalf("unit %q", got)
	}
}

func TestSameRunInTwoPoolsMakesTwoUnits(t *testing.T) {
	f := newHost(t)
	fast(t)
	first, err := New(agentConfig("exit 0"), testProfile, "build", hclog.NewNullLogger())
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(agentConfig("exit 0"), testProfile, "review", hclog.NewNullLogger())
	if err != nil {
		t.Fatal(err)
	}
	run := testRun("shared")
	a, b := dispatch(t, first, run), dispatch(t, second, run)
	if a.Name == b.Name || len(f.recorded("start")) != 2 {
		t.Fatalf("units %s and %s, starts %v; want one unit per pool", a.Name, b.Name, f.recorded("start"))
	}
}

func TestDispatchWritesMaterialThenStarts(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	run := testRun("order")
	run.Inputs = []wherry.File{{Name: "data.csv", Data: []byte("a,b")}}
	body := []byte("fetched")
	var events []string
	var mu sync.Mutex
	note := func(event string) { mu.Lock(); events = append(events, event); mu.Unlock() }
	job := jobOf(run)
	work := filepath.Join(agentRoot(true), job)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if m, _ := readMarker(job); m != nil && m.Phase == phasePrepared && exists(filepath.Join(work, "prompt.txt")) && exists(filepath.Join(work, "inputs/data.csv")) {
			note("fetch after material")
		}
		_, _ = w.Write(body)
	}))
	defer server.Close()
	run.Fetches = []wherry.Fetch{{Name: "big.bin", URL: server.URL, Size: int64(len(body)), SHA256: sha(body)}}
	f.set(func() {
		f.onStart = func(string, map[string]any) {
			m, _ := readMarker(job)
			note("start in phase " + m.Phase)
			for path, want := range map[string]fs.FileMode{
				filepath.Join(controlRoot, job): 0o700, markerPath(job): 0o600, stdoutPath(job): 0o600, filepath.Join(controlRoot, job, "stderr"): 0o600,
				work: 0o700, filepath.Join(work, "prompt.txt"): 0o600, filepath.Join(work, "inputs"): 0o700,
				filepath.Join(work, "inputs/data.csv"): 0o600, filepath.Join(work, "inputs/big.bin"): 0o600,
			} {
				if got := mode(t, path); got != want {
					t.Errorf("%s: mode %o; want %o", path, got, want)
				}
			}
			for path, want := range map[string]string{filepath.Join(work, "prompt.txt"): "hello-prompt", filepath.Join(work, "inputs/data.csv"): "a,b", filepath.Join(work, "inputs/big.bin"): "fetched"} {
				if got, _ := os.ReadFile(path); string(got) != want {
					t.Errorf("%s holds %q; want %q", path, got, want)
				}
			}
		}
	})
	dispatch(t, adapter, run)
	if m := readMarkerOf(t, run); m.Phase != phaseStarted {
		t.Errorf("marker phase %s after the start; want started", m.Phase)
	}
	if want := []string{"fetch after material", "start in phase starting"}; !slices.Equal(events, want) {
		t.Fatalf("events %v; want %v", events, want)
	}
}

func TestDispatchConvergesOnAnExistingUnit(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("sleep 30"))
	run := testRun("converge")
	first := dispatch(t, adapter, run)
	second := dispatch(t, adapter, run)
	if !bytes.Equal(first.Data, second.Data) || len(f.recorded("start")) != 1 {
		t.Fatalf("handles %s and %s, starts %v; want one unit", first.Data, second.Data, f.recorded("start"))
	}
	// Another start of the run wins between the read and the start.
	raced := testRun("raced")
	adapter.conn = &intercepted{fakeManager: f, start: func(name string) error {
		f.put(name, capturedUnitState("running"))
		return dbus.Error{Name: busUnitExists}
	}}
	outcome, handle, err := adapter.Dispatch(context.Background(), raced)
	if err != nil || outcome != wherry.OutcomeCreated || handle.Name != unitOf(raced) {
		t.Fatalf("a start that lost to another: outcome %d, %v; want created", outcome, err)
	}
}

// intercepted answers starts or kills with its own functions in place of the fake manager's.
type intercepted struct {
	*fakeManager
	start, kill func(name string) error
}

func (i *intercepted) StartTransientUnitContext(ctx context.Context, name, mode string, properties []sd.Property, ch chan<- string) (int, error) {
	if i.start == nil {
		return i.fakeManager.StartTransientUnitContext(ctx, name, mode, properties, ch)
	}
	return 0, i.start(name)
}

func (i *intercepted) KillUnitWithTarget(ctx context.Context, name string, target sd.Who, signal int32) error {
	if i.kill == nil {
		return i.fakeManager.KillUnitWithTarget(ctx, name, target, signal)
	}
	return i.kill(name)
}

func TestDispatchConvergesOnAnEndedJob(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	run := testRun("ended")
	first := dispatch(t, adapter, run)
	f.exited(t, unitOf(run))
	if err := adapter.Release(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	second := dispatch(t, adapter, run)
	if !bytes.Equal(first.Data, second.Data) || len(f.recorded("start")) != 1 {
		t.Fatalf("handles %s and %s, starts %v; want the ended job again and no second unit", first.Data, second.Data, f.recorded("start"))
	}
	if job, err := adapter.Lookup(context.Background(), run.ID); err != nil || job.State != wherry.JobEnded {
		t.Fatalf("lookup %+v, %v; want ended", job, err)
	}
}

func TestDispatchReplacesLeftovers(t *testing.T) {
	newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	run := testRun("leftover")
	putMarker(t, run, phasePrepared, nil)
	old := filepath.Join(agentRoot(true), jobOf(run), "inputs", "stale.txt")
	if err := os.WriteFile(old, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	dispatch(t, adapter, run)
	if exists(old) {
		t.Fatal("a prepared leftover's input survived the Dispatch")
	}
	if m := readMarkerOf(t, run); m.Instance == "0123456789abcdef0123456789abcdef" || m.Phase != phaseStarted {
		t.Fatalf("marker %+v; want a fresh instance, started", m)
	}
}

func TestDispatchFailureOutcomes(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	denied := dbus.Error{Name: "org.freedesktop.DBus.Error.AccessDenied", Body: []any{"denied"}}

	refused := testRun("refused")
	f.fail("start", denied)
	if outcome, _, err := adapter.Dispatch(context.Background(), refused); outcome != wherry.OutcomeNothingCreated || err == nil {
		t.Errorf("refused start: outcome %d, %v; want nothing created", outcome, err)
	}
	if exists(filepath.Join(controlRoot, jobOf(refused))) || exists(filepath.Join(agentRoot(true), jobOf(refused))) {
		t.Error("refused start: the run's directories remain")
	}

	unread := testRun("unread")
	f.fail("start", denied)
	// The first read is Dispatch's own; the second follows the failed start.
	f.fail("unit", nil, errors.New("connection reset"))
	if outcome, _, err := adapter.Dispatch(context.Background(), unread); outcome != wherry.OutcomeUnknown || err == nil {
		t.Errorf("unreadable unit: outcome %d, %v; want unknown", outcome, err)
	}
	if !exists(markerPath(jobOf(unread))) {
		t.Error("unreadable unit: the run's marker was removed")
	}

	loaded := testRun("loaded")
	adapter.conn = &intercepted{fakeManager: f, start: func(name string) error {
		f.put(name, capturedUnitState("running"))
		return denied
	}}
	if outcome, _, err := adapter.Dispatch(context.Background(), loaded); outcome != wherry.OutcomeUnknown || err == nil {
		t.Errorf("a refused start whose unit is loaded: outcome %d, %v; want unknown", outcome, err)
	}
	if !exists(markerPath(jobOf(loaded))) {
		t.Error("a refused start whose unit is loaded: the marker was removed")
	}
}

func capturedUnitState(name string) captured {
	data, _ := os.ReadFile(filepath.Join("testdata", name+".json"))
	var state captured
	_ = json.Unmarshal(data, &state)
	return state
}

func TestDispatchRefusesANameOverNameMax(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	long := strings.Repeat("n", 256)
	for _, run := range []wherry.Run{
		{ID: "run-inline", Profile: testProfile, Inputs: []wherry.File{{Name: long, Data: []byte("x")}}},
		{ID: "run-fetch", Profile: testProfile, Fetches: []wherry.Fetch{{Name: long, URL: "https://storage.invalid/x", Size: 1}}},
	} {
		outcome, _, err := adapter.Dispatch(context.Background(), run)
		var tooLarge *wherry.InputTooLargeError
		if outcome != wherry.OutcomeNothingCreated || !errors.As(err, &tooLarge) || tooLarge.Name != long {
			t.Errorf("%s: outcome %d, %v; want input too large", run.ID, outcome, err)
		}
	}
	if got := f.recorded("start"); len(got) != 0 {
		t.Fatalf("starts %v; want none", got)
	}
	run := testRun("name-max")
	run.Inputs = []wherry.File{{Name: strings.Repeat("n", 255), Data: []byte("x")}}
	dispatch(t, adapter, run)
}

func TestDispatchRefusesCredentialsOverTheLimit(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	// The store's pages bind a run whose env credential takes a page of its own; the IDs and values bind one key.
	for _, c := range []struct {
		name        string
		credentials []wherry.Secret
		value       int
	}{
		{"pages", []wherry.Secret{{Name: "model", Env: credentialEnv, Value: []byte("model-key")}}, maxCredentials - os.Getpagesize()},
		{"bytes", nil, maxCredentials - len("deploy-key")},
	} {
		run := testRun("at-limit-" + c.name)
		run.Credentials = slices.Concat(c.credentials, []wherry.Secret{{Name: "deploy", File: "deploy-key", Value: bytes.Repeat([]byte("k"), c.value)}})
		dispatch(t, adapter, run)
		over := testRun("over-limit-" + c.name)
		over.Credentials = slices.Concat(c.credentials, []wherry.Secret{{Name: "deploy", File: "deploy-key", Value: bytes.Repeat([]byte("k"), c.value+1)}})
		starts := len(f.recorded("start"))
		outcome, _, err := adapter.Dispatch(context.Background(), over)
		if outcome != wherry.OutcomeNothingCreated || err == nil || err.Error() != "the credentials exceed systemd's 1 MiB credential limit" {
			t.Fatalf("%s: one byte over: outcome %d, %v", c.name, outcome, err)
		}
		if exists(filepath.Join(controlRoot, jobOf(over))) || len(f.recorded("start")) != starts {
			t.Fatalf("%s: a run over the limit created something", c.name)
		}
	}
}

func TestDispatchRefusesACredentialWithNUL(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	run := testRun("nul")
	run.Credentials = []wherry.Secret{{Name: "model", Env: credentialEnv, Value: []byte("key\x00tail")}}
	if outcome, _, err := adapter.Dispatch(context.Background(), run); outcome != wherry.OutcomeNothingCreated || err == nil || strings.Contains(err.Error(), "tail") {
		t.Fatalf("outcome %d, %v; want nothing created", outcome, err)
	}
	model := testRun("nul-model")
	model.Model = "opus\x00"
	if outcome, _, err := adapter.Dispatch(context.Background(), model); outcome != wherry.OutcomeNothingCreated || err == nil {
		t.Fatalf("model with NUL: outcome %d, %v; want nothing created", outcome, err)
	}
	binary := testRun("binary-task-credential")
	binary.Credentials = []wherry.Secret{{Name: "key", File: "key.der", Value: []byte{0x30, 0x00, 0x82}}}
	dispatch(t, adapter, binary)
	if len(f.recorded("start")) != 1 {
		t.Fatal("a task credential with NUL bytes was refused")
	}
}

func missingCommand() Config {
	config := agentConfig("")
	config.Runtime.Command, config.Runtime.Args = []string{"/nonexistent/wherry-agent"}, nil
	return config
}

func TestDispatchRecordsAFailedStart(t *testing.T) {
	newHost(t)
	adapter := newAgent(t, missingCommand())
	run := testRun("exec")
	dispatch(t, adapter, run)
	if m := readMarkerOf(t, run); m.NeverStarted == nil || *m.NeverStarted != (neverStarted{"exec", "systemd exit status 203/EXEC"}) {
		t.Fatalf("marker %+v; want never started, exec", m)
	}
	if job, err := adapter.Lookup(context.Background(), run.ID); err != nil || job.State != wherry.JobNeverStarted {
		t.Fatalf("lookup %+v, %v; want never started", job, err)
	}
}

func TestDispatchRecordsAStartJobResult(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	f.set(func() { f.result = "timeout" })
	run := testRun("job-timeout")
	dispatch(t, adapter, run)
	if m := readMarkerOf(t, run); m.NeverStarted == nil || *m.NeverStarted != (neverStarted{"start", "timeout"}) {
		t.Fatalf("marker %+v; want never started, start, timeout", m)
	}
}

func TestDispatchTimeoutStopsTheUnit(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("sleep 30"))
	f.set(func() { f.activating = true })
	run := testRun("hung")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, _, err := adapter.Dispatch(ctx, run); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v; want the deadline", err)
	}
	if f.unit(unitOf(run)).LoadState == "loaded" || len(f.recorded("stop")) != 1 {
		t.Fatalf("unit %+v, stops %v; want the unit stopped", f.unit(unitOf(run)), f.recorded("stop"))
	}
	if exists(markerPath(jobOf(run))) {
		t.Fatal("the marker survived")
	}
}

func TestDispatchReportsPlacement(t *testing.T) {
	newHost(t)
	adapter := newAgent(t, agentConfig("sleep 30"))
	run := testRun("placed")
	dispatch(t, adapter, run)
	dispatch(t, adapter, run)
	dispatch(t, newAgentLike(t, adapter, missingCommand()), testRun("unplaced"))
	if got := samples(adapter)["wherry_adapter_placement_seconds"].Histogram.Count; got != 1 {
		t.Fatalf("placements %d; want 1, the started unit's", got)
	}
}

// newAgentLike builds an adapter of config sharing adapter's metrics.
func newAgentLike(t *testing.T, adapter *Adapter, config Config) *Adapter {
	t.Helper()
	other := newAgent(t, config)
	other.metrics = adapter.metrics
	return other
}

func sha(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func fetchRun(name, url string, body []byte) wherry.Run {
	run := testRun(name)
	run.Fetches = []wherry.Fetch{{Name: "data.bin", URL: url, Size: int64(len(body)), SHA256: sha(body)}}
	return run
}

func TestFetchBeforeTheUnit(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	body := []byte("object bytes")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if starts := f.recorded("start"); len(starts) != 0 {
			t.Errorf("fetched after %v", starts)
		}
		_, _ = w.Write(body)
	}))
	defer server.Close()
	run := fetchRun("fetch", server.URL, body)
	var seen []byte
	f.set(func() {
		f.onStart = func(string, map[string]any) {
			seen, _ = os.ReadFile(filepath.Join(agentRoot(true), jobOf(run), "inputs", "data.bin"))
		}
	})
	dispatch(t, adapter, run)
	if !bytes.Equal(seen, body) {
		t.Fatalf("the unit started with input %q; want %q", seen, body)
	}
}

func TestFetchRefusesRedirects(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	body := []byte("redirected")
	followed := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		followed = true
		_, _ = w.Write(body)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.RedirectHandler(target.URL, http.StatusFound))
	defer redirect.Close()
	run := fetchRun("redirect", redirect.URL, body)
	dispatch(t, adapter, run)
	if m := readMarkerOf(t, run); followed || m.NeverStarted == nil || m.NeverStarted.Class != wherry.ClassFetch || len(f.recorded("start")) != 0 {
		t.Fatalf("followed %v, marker %+v; want the redirect refused and the run never started", followed, m)
	}
}

// counter is an endless body that counts what it serves.
type counter struct{ n int64 }

func (c *counter) Read(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

func TestFetchBoundsTheBody(t *testing.T) {
	newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	endless := &counter{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.Copy(w, io.LimitReader(endless, 64<<20))
	}))
	run := testRun("endless")
	run.Fetches = []wherry.Fetch{{Name: "data.bin", URL: server.URL, Size: 1024, SHA256: sha(make([]byte, 1024))}}
	dispatch(t, adapter, run)
	if m := readMarkerOf(t, run); m.NeverStarted == nil || m.NeverStarted.Class != wherry.ClassFetch {
		t.Fatalf("marker %+v; want never started, fetch", m)
	}
	server.Close()
	if endless.n >= 32<<20 {
		t.Fatalf("the server wrote %d bytes; want the plugin to stop past the size", endless.n)
	}
	if info, err := os.Stat(filepath.Join(agentRoot(true), jobOf(run), "inputs")); err == nil {
		entries, _ := os.ReadDir(filepath.Join(agentRoot(true), jobOf(run), "inputs"))
		if len(entries) != 0 || !info.IsDir() {
			t.Fatalf("inputs hold %v; want nothing", entries)
		}
	}
}

func TestFetchVerifiesSizeAndDigest(t *testing.T) {
	newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	body := []byte("the object")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
	defer server.Close()
	for name, fetch := range map[string]wherry.Fetch{
		"short":  {Name: "data.bin", URL: server.URL, Size: int64(len(body)) + 1, SHA256: sha(append(body, 'x'))},
		"long":   {Name: "data.bin", URL: server.URL, Size: int64(len(body)) - 1, SHA256: sha(body[:len(body)-1])},
		"digest": {Name: "data.bin", URL: server.URL, Size: int64(len(body)), SHA256: sha([]byte("other bytes"))},
	} {
		run := testRun("verify-" + name)
		run.Fetches = []wherry.Fetch{fetch}
		dispatch(t, adapter, run)
		if m := readMarkerOf(t, run); m.NeverStarted == nil || m.NeverStarted.Class != wherry.ClassFetch {
			t.Errorf("%s: marker %+v; want never started, fetch", name, m)
		}
		if entries, _ := os.ReadDir(filepath.Join(agentRoot(true), jobOf(run), "inputs")); len(entries) != 0 {
			t.Errorf("%s: inputs hold %v; want nothing", name, entries)
		}
	}
	run := fetchRun("verified", server.URL, body)
	dispatch(t, adapter, run)
	if got, _ := os.ReadFile(filepath.Join(agentRoot(true), jobOf(run), "inputs/data.bin")); !bytes.Equal(got, body) {
		t.Fatalf("delivered %q; want %q", got, body)
	}
}

func TestFetchFailureNeverStartsTheAgent(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("altered")) }))
	defer server.Close()
	run := fetchRun("altered", server.URL, []byte("uploaded"))
	outcome, handle, err := adapter.Dispatch(context.Background(), run)
	if err != nil || outcome != wherry.OutcomeCreated {
		t.Fatalf("outcome %d, %v; want created", outcome, err)
	}
	if got := f.recorded("start"); len(got) != 0 {
		t.Fatalf("starts %v; want none", got)
	}
	want := wherry.Terminal{Class: wherry.ClassFetch, Reason: "input data.bin: not fetched", Node: adapter.node}
	if job, err := adapter.Lookup(context.Background(), run.ID); err != nil || job.State != wherry.JobNeverStarted {
		t.Fatalf("lookup %+v, %v; want never started", job, err)
	}
	if _, terminal, err := followAll(t, adapter, handle, 0); err != nil || terminal != want {
		t.Fatalf("terminal %+v, %v; want %+v", terminal, err, want)
	}
}

func TestFetchWriteFailureIsSetup(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a directory of any mode")
	}
	newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	run := testRun("unwritable")
	inputs := filepath.Join(agentRoot(true), jobOf(run), "inputs")
	body := []byte("bytes")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = os.Chmod(inputs, 0o500)
		_, _ = w.Write(body)
	}))
	defer server.Close()
	run.Fetches = []wherry.Fetch{{Name: "data.bin", URL: server.URL, Size: int64(len(body)), SHA256: sha(body)}}
	dispatch(t, adapter, run)
	_ = os.Chmod(inputs, 0o700)
	if m := readMarkerOf(t, run); m.NeverStarted == nil || *m.NeverStarted != (neverStarted{"setup", "input data.bin: permission denied"}) {
		t.Fatalf("marker %+v; want never started, setup", m)
	}
}

func TestLookupMissingJob(t *testing.T) {
	newHost(t)
	job, err := newAgent(t, agentConfig("exit 0")).Lookup(context.Background(), "run-absent")
	if err != nil || job.State != wherry.JobAbsent || job.Handle.Data != nil {
		t.Fatalf("lookup %+v, %v; want absent", job, err)
	}
}

func TestLookupOfAPreparedJobIsAbsent(t *testing.T) {
	newHost(t)
	run := testRun("prepared")
	putMarker(t, run, phasePrepared, nil)
	if job, err := newAgent(t, agentConfig("exit 0")).Lookup(context.Background(), run.ID); err != nil || job.State != wherry.JobAbsent {
		t.Fatalf("lookup %+v, %v; want absent", job, err)
	}
}

func TestLookupReadsDispatchMetadata(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("sleep 0.3"))
	run := testRun("metadata")
	handle := dispatch(t, adapter, run)
	job, err := adapter.Lookup(context.Background(), run.ID)
	if err != nil || job.State != wherry.JobRunning || job.Attempt != 2 || job.Profile != testProfile || job.Agent != "coder" || job.Handle.Name != handle.Name {
		t.Fatalf("lookup %+v, %v; want running with the dispatch's metadata", job, err)
	}
	f.exited(t, unitOf(run))
	if job, err := adapter.Lookup(context.Background(), run.ID); err != nil || job.State != wherry.JobEnded {
		t.Fatalf("lookup after the exit %+v, %v; want ended", job, err)
	}
	if f.unit(unitOf(run)).LoadState != "loaded" {
		t.Fatal("the unit unloaded at its agent's exit")
	}
}

func TestLookupMapsCapturedUnits(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	for _, c := range []struct {
		file, phase string
		state       wherry.JobState
		class       string
		reason      string
	}{
		{"running", phaseStarted, wherry.JobRunning, "", ""},
		{"exit-0", phaseStarted, wherry.JobEnded, "", ""},
		{"exit-1", phaseStarted, wherry.JobEnded, "", ""},
		{"sigterm", phaseStarted, wherry.JobEnded, "", ""},
		{"sigkill", phaseStarted, wherry.JobEnded, "", ""},
		{"oom", phaseStarted, wherry.JobEnded, "", ""},
		{"exit-203", phaseStarted, wherry.JobEnded, "", ""},
		{"exit-203", phaseStarting, wherry.JobNeverStarted, "exec", "systemd exit status 203/EXEC"},
		{"exit-243", phaseStarting, wherry.JobNeverStarted, "credentials", "systemd exit status 243/CREDENTIALS"},
		{"exit-226", phaseStarting, wherry.JobNeverStarted, "setup", "systemd exit status 226/NAMESPACE"},
		{"exit-1", phaseStarting, wherry.JobEnded, "", ""},
		{"start-timeout", phaseStarting, wherry.JobNeverStarted, "start", "timeout"},
		{"not-found", phaseStarted, wherry.JobEnded, "", ""},
	} {
		run := testRun("captured-" + c.file + "-" + c.phase)
		putMarker(t, run, c.phase, nil)
		if err := os.WriteFile(stdoutPath(jobOf(run)), []byte("output\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		f.put(unitOf(run), capturedUnit(t, c.file))
		job, err := adapter.Lookup(context.Background(), run.ID)
		if err != nil || job.State != c.state {
			t.Errorf("%s in phase %s: lookup %+v, %v; want state %d", c.file, c.phase, job, err, c.state)
			continue
		}
		if m := readMarkerOf(t, run); c.class != "" {
			_, terminal, _ := adapter.judge(jobOf(run), m, mustUnit(t, adapter, unitOf(run)))
			if terminal.Class != c.class || terminal.Reason != c.reason || terminal.Started {
				t.Errorf("%s in phase %s: terminal %+v; want class %s, reason %q", c.file, c.phase, terminal, c.class, c.reason)
			}
		}
	}
}

// systemd before 254 reloads an agent unit as bad-setting, its ExecStartEx= lost, while its agent runs on.
func TestAUnitReloadedAsBadSettingStaysTheRun(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("sleep 30"))
	run := testRun("bad-setting")
	handle := dispatch(t, adapter, run)
	f.edit(unitOf(run), func(u *captured) { u.LoadState = "bad-setting" })
	if job, err := adapter.Lookup(context.Background(), run.ID); err != nil || job.State != wherry.JobRunning {
		t.Fatalf("lookup %+v, %v; want running", job, err)
	}
	if err := adapter.Stop(context.Background(), handle); err != nil {
		t.Fatal(err)
	}
	if _, terminal, err := followAll(t, adapter, handle, 0); err != nil || terminal.Signal != "15" {
		t.Fatalf("terminal %+v, %v; want the agent ended by SIGTERM", terminal, err)
	}
}

func mustUnit(t *testing.T, adapter *Adapter, name string) unit {
	t.Helper()
	u, err := adapter.unit(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestLookupOfAGoneUnitIsEnded(t *testing.T) {
	newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	bare, recordedRun := testRun("gone"), testRun("gone-recorded")
	putMarker(t, bare, phaseStarted, nil)
	putMarker(t, recordedRun, phaseStarted, func(m *marker) { m.Terminal = &recorded{ExitCode: 137, Signal: "9"} })
	for run, want := range map[*wherry.Run]wherry.Terminal{
		&bare:        {Started: true, ExitCode: -1, Node: adapter.node},
		&recordedRun: {Started: true, ExitCode: 137, Signal: "9", Node: adapter.node},
	} {
		job, err := adapter.Lookup(context.Background(), run.ID)
		if err != nil || job.State != wherry.JobEnded {
			t.Fatalf("%s: lookup %+v, %v; want ended", run.ID, job, err)
		}
		if _, terminal, err := followAll(t, adapter, job.Handle, 0); err != nil || terminal != want {
			t.Fatalf("%s: terminal %+v, %v; want %+v", run.ID, terminal, err, want)
		}
	}
}

func TestLookupOfAStartingJobWithNoUnit(t *testing.T) {
	newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	silent, spoke := testRun("starting-silent"), testRun("starting-spoke")
	putMarker(t, silent, phaseStarting, nil)
	putMarker(t, spoke, phaseStarting, nil)
	if err := os.WriteFile(stdoutPath(jobOf(spoke)), []byte("a line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	job, err := adapter.Lookup(context.Background(), silent.ID)
	if err != nil || job.State != wherry.JobNeverStarted {
		t.Fatalf("starting, no unit, empty stdout: %+v, %v; want never started", job, err)
	}
	if _, terminal, err := followAll(t, adapter, job.Handle, 0); err != nil || terminal.Started || terminal.Class != "start" {
		t.Fatalf("terminal %+v, %v; want never started, class start", terminal, err)
	}
	if job, err := adapter.Lookup(context.Background(), spoke.ID); err != nil || job.State != wherry.JobEnded {
		t.Fatalf("starting, no unit, with output: %+v, %v; want ended", job, err)
	}
}

func TestLookupRefusesAForeignUnit(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	unmarked := testRun("unmarked")
	f.put(unitOf(unmarked), capturedUnit(t, "running"))
	persistent := testRun("persistent")
	putMarker(t, persistent, phaseStarted, nil)
	state := capturedUnit(t, "running")
	state.Transient = false
	f.put(unitOf(persistent), state)
	otherPool := testRun("other-pool")
	putMarker(t, otherPool, phaseStarted, func(m *marker) { m.Pool = "review" })
	for _, run := range []wherry.Run{unmarked, persistent, otherPool} {
		if _, err := adapter.Lookup(context.Background(), run.ID); !errors.Is(err, errNotAgent) {
			t.Errorf("%s: %v; want %v", run.ID, err, errNotAgent)
		}
		if _, _, err := adapter.Dispatch(context.Background(), run); !errors.Is(err, errNotAgent) {
			t.Errorf("dispatch %s: %v; want %v", run.ID, err, errNotAgent)
		}
	}
}

func TestLookupWaitsForAnActivatingUnit(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	run := testRun("activating")
	putMarker(t, run, phaseStarting, nil)
	f.put(unitOf(run), capturedUnit(t, "activating"))
	go func() {
		time.Sleep(100 * time.Millisecond)
		f.edit(unitOf(run), func(c *captured) { *c = capturedUnitState("running") })
	}()
	began := time.Now()
	job, err := adapter.Lookup(context.Background(), run.ID)
	if err != nil || job.State != wherry.JobRunning || time.Since(began) < 100*time.Millisecond {
		t.Fatalf("lookup %+v, %v after %v; want running once activated", job, err, time.Since(began))
	}
}

func TestFollowOffsetsAndReattach(t *testing.T) {
	newHost(t)
	adapter := newAgent(t, agentConfig(`printf 'one\ntwo\nthree\n'`))
	handle := dispatch(t, adapter, testRun("offsets"))
	lines, _, err := followAll(t, adapter, handle, 0)
	if err != nil || !slices.Equal(texts(lines), []string{"one", "two", "three"}) {
		t.Fatalf("lines %q, %v", texts(lines), err)
	}
	if lines[1].Offset != 4 || lines[1].Next != 8 || lines[2].Next != 14 {
		t.Fatalf("offsets %+v; want byte offsets", lines)
	}
	again, _, err := followAll(t, adapter, handle, lines[1].Offset)
	if err != nil || !slices.Equal(texts(again), []string{"two", "three"}) || again[0].Offset != 4 {
		t.Fatalf("reattached at 4: %q, %v", texts(again), err)
	}
}

func TestFollowReadsToTheEndAfterExit(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("sleep 0.2"))
	run := testRun("final")
	handle := dispatch(t, adapter, run)
	// The agent's last line lands as systemd records its exit.
	var once sync.Once
	f.set(func() {
		f.onRead = func(name string) {
			if f.unit(name).ExecMainExitTimestamp == 0 {
				return
			}
			once.Do(func() {
				file, _ := os.OpenFile(stdoutPath(jobOf(run)), os.O_WRONLY|os.O_APPEND, 0)
				_, _ = file.WriteString(`{"type":"result"}` + "\n")
				file.Close()
			})
		}
	})
	lines, _, err := followAll(t, adapter, handle, 0)
	if err != nil || !slices.Equal(texts(lines), []string{`{"type":"result"}`}) {
		t.Fatalf("lines %q, %v; want the line written at the exit", texts(lines), err)
	}
}

func TestFollowFailsOnALogShorterThanTheOffset(t *testing.T) {
	newHost(t)
	adapter := newAgent(t, agentConfig(`echo short`))
	handle := dispatch(t, adapter, testRun("short"))
	if _, _, err := followAll(t, adapter, handle, 100); err == nil || err.Error() != "the agent's log is shorter than the offset" {
		t.Fatalf("got %v", err)
	}
}

func TestFollowReturnsTerminal(t *testing.T) {
	newHost(t)
	node, _ := os.Hostname()
	for i, c := range []struct {
		script string
		want   wherry.Terminal
	}{
		{"exit 0", wherry.Terminal{Started: true, ExitCode: 0, Node: node}},
		{"exit 3", wherry.Terminal{Started: true, ExitCode: 3, Node: node}},
		{"kill -9 $$", wherry.Terminal{Started: true, ExitCode: 137, Signal: "9", Node: node}},
	} {
		script, want := c.script, c.want
		adapter := newAgent(t, agentConfig(script))
		run := testRun("terminal-" + strconv.Itoa(i))
		if _, terminal, err := followAll(t, adapter, dispatch(t, adapter, run), 0); err != nil || terminal != want {
			t.Errorf("%s: terminal %+v, %v; want %+v", script, terminal, err, want)
		}
	}
}

func TestFollowOfANeverStartedJob(t *testing.T) {
	newHost(t)
	adapter := newAgent(t, missingCommand())
	_, terminal, err := followAll(t, adapter, dispatch(t, adapter, testRun("never")), 0)
	want := wherry.Terminal{Class: "exec", Reason: "systemd exit status 203/EXEC", Node: adapter.node}
	if err != nil || terminal != want {
		t.Fatalf("terminal %+v, %v; want %+v", terminal, err, want)
	}
}

func TestFollowOfAGoneUnitReadsTheRest(t *testing.T) {
	newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	run := testRun("gone-rest")
	putMarker(t, run, phaseStarted, nil)
	if err := os.WriteFile(stdoutPath(jobOf(run)), []byte("first\nsecond\nlast"), 0o600); err != nil {
		t.Fatal(err)
	}
	lines, terminal, err := followAll(t, adapter, handleFor(unitOf(run), "0123456789abcdef0123456789abcdef"), 6)
	if err != nil || !slices.Equal(texts(lines), []string{"second", "last"}) || terminal.ExitCode != -1 {
		t.Fatalf("lines %q, terminal %+v, %v; want the rest of the file", texts(lines), terminal, err)
	}
}

func TestFollowCarriesNoStderr(t *testing.T) {
	newHost(t)
	adapter := newAgent(t, agentConfig(`printf 'out-'; echo err >&2; printf 'line\n'`))
	run := testRun("stderr")
	lines, _, err := followAll(t, adapter, dispatch(t, adapter, run), 0)
	if err != nil || !slices.Equal(texts(lines), []string{"out-line"}) {
		t.Fatalf("lines %q, %v; want stdout alone", texts(lines), err)
	}
	if got, _ := os.ReadFile(filepath.Join(controlRoot, jobOf(run), "stderr")); string(got) != "err\n" {
		t.Fatalf("stderr file %q", got)
	}
}

func TestTerminalCarriesPlatformReason(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	reason := "systemd's text: unit failed at step CREDENTIALS with 'quoted' material " + strings.Repeat("x", 600)
	run := testRun("reason")
	putMarker(t, run, phaseStarting, nil)
	state := capturedUnit(t, "start-timeout")
	state.Result = reason
	f.put(unitOf(run), state)
	_, terminal, err := followAll(t, adapter, handleFor(unitOf(run), "0123456789abcdef0123456789abcdef"), 0)
	if err != nil || terminal.Reason != reason {
		t.Fatalf("reason %q, %v; want systemd's text unchanged", terminal.Reason, err)
	}
}

func TestStaleHandleRefusesAReplacedJob(t *testing.T) {
	newHost(t)
	adapter := newAgent(t, agentConfig("sleep 30"))
	run := testRun("replaced")
	stale := dispatch(t, adapter, run)
	if err := adapter.Purge(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	dispatch(t, adapter, run)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for name, call := range map[string]func() error{
		"follow": func() error {
			_, err := adapter.Follow(ctx, stale, 0, make(chan wherry.Line, 64))
			return err
		},
		"stop":    func() error { return adapter.Stop(ctx, stale) },
		"collect": func() error { return adapter.Collect(ctx, stale, wherry.Limits{Files: 8}, make(chan wherry.Output, 8)) },
	} {
		if err := call(); !errors.Is(err, errReplaced) {
			t.Errorf("%s through a stale handle: %v; want %v", name, err, errReplaced)
		}
	}
	if job, _ := adapter.Lookup(ctx, run.ID); job.State != wherry.JobRunning {
		t.Fatalf("the new job %+v; want it running", job)
	}
}

func kills(f *fakeManager) []string {
	var signals []string
	for _, call := range f.recorded("kill") {
		fields := strings.Fields(call)
		signals = append(signals, fields[2]+" "+fields[3])
	}
	return signals
}

func TestStopSignalsThenWaits(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("sleep 30"))
	run := testRun("stop")
	if err := adapter.Stop(context.Background(), dispatch(t, adapter, run)); err != nil {
		t.Fatal(err)
	}
	if got := kills(f); !slices.Equal(got, []string{"all 15"}) {
		t.Fatalf("kills %v; want SIGTERM to every process", got)
	}
	if u := f.unit(unitOf(run)); u.MainPID != 0 || u.ExecMainStatus != 15 {
		t.Fatalf("unit %+v; want the agent ended by SIGTERM", u)
	}
}

func TestStopEscalatesAfterGrace(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig(`trap '' TERM; while :; do sleep 0.05; done`))
	run := testRun("stubborn")
	if err := adapter.Stop(context.Background(), dispatch(t, adapter, run)); err != nil {
		t.Fatal(err)
	}
	if got := kills(f); !slices.Equal(got, []string{"all 15", "all 9"}) {
		t.Fatalf("kills %v; want SIGTERM, then SIGKILL", got)
	}
}

func TestStopKillsLeftoverProcesses(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("sleep 600 & exit 0"))
	run := testRun("leftover-process")
	handle := dispatch(t, adapter, run)
	f.exited(t, unitOf(run))
	if empty, err := adapter.empty(context.Background(), unitOf(run)); err != nil || empty {
		t.Fatalf("empty %v, %v; want the background process left in the cgroup", empty, err)
	}
	if err := adapter.Stop(context.Background(), handle); err != nil {
		t.Fatal(err)
	}
	if empty, err := adapter.empty(context.Background(), unitOf(run)); err != nil || !empty {
		t.Fatalf("empty %v, %v after the stop; want the cgroup empty", empty, err)
	}
}

func TestStopErrsWhenProcessesOutliveSIGKILL(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("sleep 30"))
	handle := dispatch(t, adapter, testRun("immortal"))
	f.set(func() { f.unkillable = true })
	if err := adapter.Stop(context.Background(), handle); err == nil || err.Error() != "the agent outlived SIGKILL" {
		t.Fatalf("got %v", err)
	}
	f.set(func() { f.unkillable = false })
	f.fail("kill", dbus.Error{Name: "org.freedesktop.DBus.Error.AccessDenied", Body: []any{"denied"}})
	var failure *wherry.Error
	if err := adapter.Stop(context.Background(), handle); !errors.As(err, &failure) || !strings.HasPrefix(failure.Op, "kill unit ") || failure.Class != "denied" {
		t.Fatalf("a refused kill: %v; want the kill's error", err)
	}
}

func TestStopKeepsTheUnitLoaded(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("sleep 30"))
	run := testRun("loaded")
	if err := adapter.Stop(context.Background(), dispatch(t, adapter, run)); err != nil {
		t.Fatal(err)
	}
	if got := f.recorded("stop", "reset"); len(got) != 0 || f.unit(unitOf(run)).LoadState != "loaded" {
		t.Fatalf("calls %v, unit %+v; want the unit loaded", got, f.unit(unitOf(run)))
	}
	if job, err := adapter.Lookup(context.Background(), run.ID); err != nil || job.State != wherry.JobEnded {
		t.Fatalf("lookup %+v, %v; want ended", job, err)
	}
}

func TestStopOfAnEmptyCgroupDoesNotSignal(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	run := testRun("empty")
	handle := dispatch(t, adapter, run)
	f.exited(t, unitOf(run))
	if err := adapter.Stop(context.Background(), handle); err != nil || len(kills(f)) != 0 {
		t.Fatalf("stop %v, kills %v; want no signal", err, kills(f))
	}
}

func TestStopOfANeverStartedJobLeavesIt(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, missingCommand())
	run := testRun("never-stop")
	if err := adapter.Stop(context.Background(), dispatch(t, adapter, run)); err != nil {
		t.Fatal(err)
	}
	if got := f.recorded("kill", "stop", "reset"); len(got) != 0 || f.unit(unitOf(run)).LoadState != "loaded" {
		t.Fatalf("calls %v; want the unit left", got)
	}
}

func TestStopOfAGoneUnitSucceeds(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	run := testRun("gone-stop")
	handle := dispatch(t, adapter, run)
	f.exited(t, unitOf(run))
	if err := adapter.Release(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Stop(context.Background(), handle); err != nil || len(kills(f)) != 0 {
		t.Fatalf("stop %v, kills %v; want success and no signal", err, kills(f))
	}
	// The unit goes between the read and the kill.
	racing := newAgent(t, agentConfig("sleep 30"))
	raced := testRun("gone-between")
	racedHandle := dispatch(t, racing, raced)
	racing.conn = &intercepted{fakeManager: f, kill: func(name string) error {
		f.put(name, unloaded)
		return dbus.Error{Name: busNoSuchUnit, Body: []any{"Unit " + name + " not loaded."}}
	}}
	if err := racing.Stop(context.Background(), racedHandle); err != nil {
		t.Fatalf("a unit gone at the kill: %v; want success", err)
	}
}

func writeOutputs(t *testing.T, run wherry.Run, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(agentRoot(true), jobOf(run), "outputs")
	for name, content := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func collect(t *testing.T, adapter *Adapter, handle wherry.Handle, limits wherry.Limits) ([]wherry.Output, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	outputs := make(chan wherry.Output, 1024)
	err := adapter.Collect(ctx, handle, limits, outputs)
	close(outputs)
	var got []wherry.Output
	for output := range outputs {
		got = append(got, output)
	}
	return got, err
}

var limits = wherry.Limits{Files: 64, FileBytes: 1 << 20, TotalBytes: 4 << 20}

// ended dispatches run and waits for its agent to exit.
func ended(t *testing.T, f *fakeManager, adapter *Adapter, run wherry.Run) wherry.Handle {
	t.Helper()
	handle := dispatch(t, adapter, run)
	f.exited(t, unitOf(run))
	return handle
}

func names(outputs []wherry.Output) []string {
	var got []string
	for _, output := range outputs {
		got = append(got, output.Name)
	}
	return got
}

func TestCollectWaitsForAnEmptyCgroup(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig(`mkdir outputs; echo early > outputs/early; (sleep 0.3; echo late > outputs/late) & exit 0`))
	run := testRun("late")
	got, err := collect(t, adapter, ended(t, f, adapter, run), limits)
	if err != nil || !slices.Equal(names(got), []string{"early", "late"}) {
		t.Fatalf("outputs %v, %v; want both, read once the cgroup emptied", names(got), err)
	}
}

func TestCollectRefusesARunningAgent(t *testing.T) {
	newHost(t)
	adapter := newAgent(t, agentConfig("sleep 30"))
	handle := dispatch(t, adapter, testRun("running"))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := adapter.Collect(ctx, handle, limits, make(chan wherry.Output, 8)); err == nil || err.Error() != "the agent is still running" {
		t.Fatalf("got %v", err)
	}
}

func TestCollectCountsTheTreeBeforeReading(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	run := testRun("count")
	handle := ended(t, f, adapter, run)
	writeOutputs(t, run, map[string]string{"a": "1", "b": "2", "sub/c": "3"})
	got, err := collect(t, adapter, handle, wherry.Limits{Files: 3, FileBytes: 1 << 20, TotalBytes: 1 << 20})
	if err == nil || len(got) != 0 {
		t.Fatalf("outputs %v, %v; want four entries over three refused, nothing read", names(got), err)
	}
	if got, err := collect(t, adapter, handle, wherry.Limits{Files: 4, FileBytes: 1 << 20, TotalBytes: 1 << 20}); err != nil || len(got) != 3 {
		t.Fatalf("at the limit: %v, %v", names(got), err)
	}
}

func TestCollectReadsOnlyRegularFiles(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	run := testRun("types")
	handle := ended(t, f, adapter, run)
	dir := writeOutputs(t, run, map[string]string{"report.md": "report", "target": "inside"})
	if err := syscall.Mkfifo(filepath.Join(dir, "fifo"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	got, err := collect(t, adapter, handle, limits)
	if err != nil {
		t.Fatal(err)
	}
	modes := map[string]fs.FileMode{}
	for _, output := range got {
		modes[output.Name] = output.Mode
		if output.Mode != 0 && output.Data != nil {
			t.Errorf("%s of mode %v was read", output.Name, output.Mode)
		}
	}
	if modes["fifo"] != fs.ModeNamedPipe || modes["link"] != fs.ModeSymlink || modes["report.md"] != 0 {
		t.Fatalf("modes %v", modes)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	// A FIFO or a link in place of a listed file, as an agent that outlived the listing could leave.
	for name, size := range map[string]int64{"fifo": 0, "link": 6} {
		done := make(chan error, 1)
		go func() {
			_, err := read(root, name, size)
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("read %s: want a refusal", name)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("read %s blocked", name)
		}
	}
}

func TestCollectDoesNotFollowLinks(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	run := testRun("links")
	handle := ended(t, f, adapter, run)
	dir := writeOutputs(t, run, map[string]string{"real/file": "x"})
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("host secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{"to-dir": "real", "to-host": outside, "to-host-dir": filepath.Dir(outside)} {
		if err := os.Symlink(target, filepath.Join(dir, link)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := collect(t, adapter, handle, limits)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"real/file", "to-dir", "to-host", "to-host-dir"}; !slices.Equal(names(got), want) {
		t.Fatalf("outputs %v; want %v", names(got), want)
	}
	for _, output := range got {
		if bytes.Contains(output.Data, []byte("host secret")) {
			t.Fatalf("%s read a host file", output.Name)
		}
	}
}

func TestCollectRefusesEscapingNames(t *testing.T) {
	newHost(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(filepath.Dir(dir), "escaped"), []byte("host"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, name := range []string{"../escaped", "/etc/hostname", "a/../../escaped"} {
		if _, err := read(root, name, 4); err == nil {
			t.Errorf("read %s outside the outputs", name)
		}
	}
}

func TestCollectLeavesFilesOverTheLimitsUnread(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	run := testRun("limits")
	handle := ended(t, f, adapter, run)
	writeOutputs(t, run, map[string]string{"big": strings.Repeat("b", 100), "small": "s"})
	got, err := collect(t, adapter, handle, wherry.Limits{Files: 8, FileBytes: 10, TotalBytes: 10})
	if err != nil || len(got) != 2 || got[0].Data != nil || got[0].Size != 100 || string(got[1].Data) != "s" {
		t.Fatalf("outputs %+v, %v; want big listed unread and small read", got, err)
	}
}

func TestCollectRefusesAReadOfAnotherLength(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "grew"), []byte("longer than listed"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, size := range []int64{4, 100} {
		if _, err := read(root, "grew", size); err == nil || err.Error() != "an output read back at another length than its listing" {
			t.Errorf("listed at %d: %v", size, err)
		}
	}
}

func TestCollectStreamsNamedOutputs(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	run := testRun("stream")
	handle := ended(t, f, adapter, run)
	writeOutputs(t, run, map[string]string{"archive.tar": strings.Repeat("t", 5000), "note": "n"})
	got, err := collect(t, adapter, handle, wherry.Limits{Files: 8, FileBytes: 10, TotalBytes: 10, Stream: []string{"archive.tar"}, StreamBytes: 1 << 20})
	if err != nil || len(got) != 2 || got[0].Body == nil || got[0].Data != nil {
		t.Fatalf("outputs %+v, %v; want archive.tar streamed", got, err)
	}
	defer got[0].Body.Close()
	if body, err := io.ReadAll(got[0].Body); err != nil || len(body) != 5000 {
		t.Fatalf("streamed %d bytes, %v", len(body), err)
	}
}

func TestCollectStreamsTheOutcome(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	run := testRun("outcome")
	handle := ended(t, f, adapter, run)
	outcome := `{"state":"TASK_STATE_COMPLETED"}`
	writeOutputs(t, run, map[string]string{".wherry/outcome.json": outcome, "reply.md": "done"})
	got, err := collect(t, adapter, handle, wherry.Limits{Files: 256, FileBytes: 1 << 20, TotalBytes: 1 << 20, Stream: []string{".wherry/outcome.json"}, StreamBytes: 1 << 20})
	if err != nil || len(got) != 2 || got[0].Name != ".wherry/outcome.json" || got[0].Body == nil || got[0].Data != nil || string(got[1].Data) != "done" {
		t.Fatalf("outputs %+v, %v; want .wherry/outcome.json streamed beside reply.md", got, err)
	}
	defer got[0].Body.Close()
	if body, err := io.ReadAll(got[0].Body); err != nil || string(body) != outcome {
		t.Fatalf("streamed %q, %v; want %q", body, err, outcome)
	}
}

func TestCollectWithoutAnOutputsDirectory(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	if got, err := collect(t, adapter, ended(t, f, adapter, testRun("no-outputs")), limits); err != nil || len(got) != 0 {
		t.Fatalf("outputs %v, %v; want none", got, err)
	}
}

func TestCollectAfterRelease(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("mkdir outputs; echo done > outputs/reply.md"))
	run := testRun("after-release")
	handle := ended(t, f, adapter, run)
	if err := adapter.Release(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := collect(t, adapter, handle, limits); err != nil || len(got) != 1 || string(got[0].Data) != "done\n" {
		t.Fatalf("outputs %+v, %v; want reply.md", got, err)
	}
}

func TestReleaseRecordsTheTerminalAndStopsTheUnit(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("exit 3"))
	run := testRun("release")
	handle := ended(t, f, adapter, run)
	if err := adapter.Release(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	if m := readMarkerOf(t, run); m.Terminal == nil || *m.Terminal != (recorded{ExitCode: 3}) {
		t.Fatalf("marker %+v; want the terminal recorded", m)
	}
	if got := f.recorded("stop", "reset"); len(got) != 2 || f.unit(unitOf(run)).LoadState == "loaded" {
		t.Fatalf("calls %v; want the unit stopped and reset", got)
	}
	if _, terminal, err := followAll(t, adapter, handle, 0); err != nil || terminal.ExitCode != 3 {
		t.Fatalf("terminal %+v, %v; want exit code 3 from the marker", terminal, err)
	}
}

func TestReleaseRemovesPromptAndInputs(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("mkdir outputs; echo x > outputs/x"))
	run := testRun("release-material")
	run.Inputs = []wherry.File{{Name: "in.txt", Data: []byte("input")}}
	ended(t, f, adapter, run)
	if err := adapter.Release(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(agentRoot(true), jobOf(run))
	if exists(filepath.Join(work, "prompt.txt")) || exists(filepath.Join(work, "inputs")) || !exists(filepath.Join(work, "outputs/x")) {
		t.Fatal("want prompt.txt and inputs removed and outputs kept")
	}
}

func TestReleaseLeavesARunningAgent(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("sleep 30"))
	run := testRun("release-running")
	dispatch(t, adapter, run)
	if err := adapter.Release(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.recorded("stop", "reset", "kill"); len(got) != 0 || !exists(filepath.Join(agentRoot(true), jobOf(run), "prompt.txt")) {
		t.Fatalf("calls %v; want the running agent left as it was", got)
	}
	leftover := testRun("release-leftover")
	ended(t, f, newAgent(t, agentConfig("sleep 600 & exit 0")), leftover)
	if err := adapter.Release(context.Background(), leftover.ID); err != nil || f.unit(unitOf(leftover)).LoadState != "loaded" {
		t.Fatalf("release with a process left: %v, unit %+v; want the unit kept", err, f.unit(unitOf(leftover)))
	}
}

func TestReleaseOfAnAbsentRunSucceeds(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	for range 2 {
		if err := adapter.Release(context.Background(), "run-never"); err != nil {
			t.Fatal(err)
		}
	}
	run := testRun("release-no-work")
	ended(t, f, adapter, run)
	if err := os.RemoveAll(filepath.Join(agentRoot(true), jobOf(run))); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := adapter.Release(context.Background(), run.ID); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPurgeStopsWaitsAndRemoves(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("sleep 30"))
	run := testRun("purge")
	dispatch(t, adapter, run)
	f.set(func() { f.linger = 3 })
	if err := adapter.Purge(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	if f.unit(unitOf(run)).LoadState == "loaded" {
		t.Fatal("Purge returned while the unit was loaded")
	}
	for _, path := range slices.Concat(workDirs(jobOf(run)), []string{filepath.Join(controlRoot, jobOf(run)), filepath.Join(credentialRoot, unitOf(run))}) {
		if exists(path) {
			t.Errorf("%s remains", path)
		}
	}
}

func TestPurgeRemovesTheMarkerFirst(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes a directory of any mode")
	}
	f := newHost(t)
	adapter := newAgent(t, agentConfig("mkdir -p outputs/locked; touch outputs/locked/file; chmod 0500 outputs/locked"))
	run := testRun("purge-fails")
	ended(t, f, adapter, run)
	locked := filepath.Join(agentRoot(true), jobOf(run), "outputs/locked")
	defer os.Chmod(locked, 0o700)
	if err := adapter.Purge(context.Background(), run.ID); err == nil {
		t.Fatal("a Purge whose removal failed succeeded")
	}
	if job, err := adapter.Lookup(context.Background(), run.ID); err != nil || job.State != wherry.JobAbsent {
		t.Fatalf("lookup %+v, %v; want absent", job, err)
	}
}

func TestPurgeOfAnAbsentJobSucceeds(t *testing.T) {
	newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	for range 2 {
		if err := adapter.Purge(context.Background(), "run-absent"); err != nil {
			t.Fatal(err)
		}
	}
}

// backdate moves a run's end, and its files' changes, by ago into the past.
func backdate(t *testing.T, f *fakeManager, run wherry.Run, ago time.Duration) {
	t.Helper()
	at := time.Now().Add(-ago)
	if f != nil && f.unit(unitOf(run)).LoadState == "loaded" {
		f.edit(unitOf(run), func(c *captured) { c.ExecMainExitTimestamp = uint64(at.UnixMicro()) })
	}
	for _, path := range []string{markerPath(jobOf(run)), stdoutPath(jobOf(run)), filepath.Join(controlRoot, jobOf(run))} {
		if err := os.Chtimes(path, at, at); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
	}
}

func sweep(t *testing.T, adapter *Adapter) {
	t.Helper()
	if err := adapter.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSweepPurgesRunsEndedPastTheGrace(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	run := testRun("expired")
	ended(t, f, adapter, run)
	backdate(t, f, run, 11*time.Minute)
	sweep(t, adapter)
	if exists(filepath.Join(controlRoot, jobOf(run))) || f.unit(unitOf(run)).LoadState == "loaded" {
		t.Fatal("an own run ended past the grace survived the sweep")
	}
}

func TestSweepKeepsRunsInsideTheGrace(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	run := testRun("recent")
	ended(t, f, adapter, run)
	backdate(t, f, run, time.Minute)
	sweep(t, adapter)
	if !exists(markerPath(jobOf(run))) || !exists(stdoutPath(jobOf(run))) {
		t.Fatal("a run ended inside the grace was purged")
	}
}

func TestSweepSettlesOwnEndedRunsAtOnce(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("exit 4"))
	run := testRun("settle")
	ended(t, f, adapter, run)
	sweep(t, adapter)
	if f.unit(unitOf(run)).LoadState == "loaded" || exists(filepath.Join(agentRoot(true), jobOf(run), "prompt.txt")) {
		t.Fatal("an own ended run was not settled at once")
	}
	if m := readMarkerOf(t, run); m == nil || m.Terminal == nil || m.Terminal.ExitCode != 4 {
		t.Fatalf("marker %+v; want it kept with the terminal", m)
	}
}

func TestSweepCountsOnlyThisProfilesRuns(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	own, other := testRun("own"), testRun("other")
	other.Profile = "other"
	ended(t, f, adapter, own)
	ended(t, f, adapter, other)
	backdate(t, f, own, 11*time.Minute)
	backdate(t, f, other, 11*time.Minute)
	sweep(t, adapter)
	if !exists(markerPath(jobOf(other))) || exists(markerPath(jobOf(own))) {
		t.Fatal("want the own run purged and the other profile's kept")
	}
	if got := samples(adapter)["wherry_adapter_sweep_deletions_total/job"].Value; got != 1 {
		t.Fatalf("deletions %v; want 1", got)
	}
}

// setMarkerProfile rewrites the profile field of run's job.json as it is on disk.
func setMarkerProfile(t *testing.T, run wherry.Run, profile string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(markerPath(jobOf(run)))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	written := maps.Clone(raw)
	raw["profile"] = profile
	if data, err = json.Marshal(raw); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(markerPath(jobOf(run)), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return written
}

func TestMarkerProfileDecidesOwnership(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	run := testRun("marked")
	ended(t, f, adapter, run)
	if written := setMarkerProfile(t, run, "other"); written["profile"] != testProfile || written["adapter"] != nil {
		t.Fatalf("marker %v; want the profile under profile", written)
	}
	if job, err := adapter.Lookup(context.Background(), run.ID); err != nil || job.Profile != "other" {
		t.Fatalf("lookup %+v, %v; want the marker's profile other", job, err)
	}
	backdate(t, f, run, 11*time.Minute)
	sweep(t, adapter)
	if !exists(markerPath(jobOf(run))) {
		t.Fatal("swept a run whose marker names another profile")
	}
	setMarkerProfile(t, run, testProfile)
	backdate(t, f, run, 11*time.Minute)
	sweep(t, adapter)
	if exists(markerPath(jobOf(run))) {
		t.Fatal("kept an expired run whose marker names this profile")
	}
}

func TestSweepSettlesAnotherProfilesExpiredRun(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	recent, expired := testRun("other-recent"), testRun("other-expired")
	recent.Profile, expired.Profile = "other", "other"
	ended(t, f, adapter, recent)
	ended(t, f, adapter, expired)
	backdate(t, f, expired, 11*time.Minute)
	sweep(t, adapter)
	if f.unit(unitOf(expired)).LoadState == "loaded" || exists(filepath.Join(agentRoot(true), jobOf(expired), "prompt.txt")) || !exists(markerPath(jobOf(expired))) {
		t.Fatal("want the expired run's unit stopped and material deleted, and the run kept")
	}
	if f.unit(unitOf(recent)).LoadState != "loaded" || !exists(filepath.Join(agentRoot(true), jobOf(recent), "prompt.txt")) {
		t.Fatal("another profile's run inside the grace was settled")
	}
}

func TestSweepKeepsARunningRun(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("sleep 30"))
	run := testRun("alive")
	dispatch(t, adapter, run)
	backdate(t, f, run, time.Hour)
	sweep(t, adapter)
	if got := f.recorded("kill", "stop", "reset"); len(got) != 0 || !exists(markerPath(jobOf(run))) {
		t.Fatalf("calls %v; want the running run untouched", got)
	}
}

func TestSweepRemovesStaleMaterial(t *testing.T) {
	newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	own, other := testRun("stale"), testRun("stale-other")
	other.Profile = "other"
	for _, run := range []wherry.Run{own, other} {
		putMarker(t, run, phasePrepared, nil)
		backdate(t, nil, run, 11*time.Minute)
	}
	sweep(t, adapter)
	for _, run := range []wherry.Run{own, other} {
		if exists(filepath.Join(controlRoot, jobOf(run))) || exists(filepath.Join(agentRoot(true), jobOf(run))) {
			t.Errorf("%s: stale material survived", run.ID)
		}
	}
	if got := samples(adapter)["wherry_adapter_sweep_deletions_total/variable"].Value; got != 1 {
		t.Fatalf("variable deletions %v; want the own one", got)
	}
}

func TestSweepKeepsRecentMaterial(t *testing.T) {
	newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	run := testRun("fresh")
	putMarker(t, run, phasePrepared, nil)
	sweep(t, adapter)
	if !exists(filepath.Join(agentRoot(true), jobOf(run), "prompt.txt")) {
		t.Fatal("recent material was removed")
	}
}

func TestSweepRemovesLeftovers(t *testing.T) {
	newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	control, work := filepath.Join(controlRoot, poolPrefix+"run-orphan"), filepath.Join(agentRoot(true), poolPrefix+"run-workless")
	recent := filepath.Join(agentRoot(false), poolPrefix+"run-recent")
	foreign := filepath.Join(agentRoot(true), "agent-000000000000-run-x")
	for _, dir := range []string{control, work, recent, foreign} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-11 * time.Minute)
	for _, dir := range []string{control, work, foreign} {
		if err := os.Chtimes(dir, old, old); err != nil {
			t.Fatal(err)
		}
	}
	sweep(t, adapter)
	if exists(control) || exists(work) || !exists(recent) || !exists(foreign) {
		t.Fatalf("control %v, work %v, recent %v, foreign %v; want only the old leftovers of the pool removed", exists(control), exists(work), exists(recent), exists(foreign))
	}
	for _, sample := range samples(adapter) {
		if sample.Family == "wherry_adapter_sweep_deletions_total" && sample.Value != 0 {
			t.Fatalf("leftovers counted: %+v", sample)
		}
	}
}

func TestSweepIgnoresUnitsWithoutAMarker(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	f.put(unitName(poolPrefix+"run-unmarked"), capturedUnit(t, "exit-1"))
	if err := os.MkdirAll(filepath.Join(controlRoot, poolPrefix+"run-unmarked"), 0o700); err != nil {
		t.Fatal(err)
	}
	sweep(t, adapter)
	if got := f.recorded("kill", "stop", "reset", "unit"); len(got) != 0 {
		t.Fatalf("calls %v; want none", got)
	}
}

func TestSweepReportsDeletionsAndPendingRuns(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	purged, pending, stale := testRun("sweep-purged"), testRun("sweep-pending"), testRun("sweep-stale")
	ended(t, f, adapter, purged)
	ended(t, f, adapter, pending)
	backdate(t, f, purged, 11*time.Minute)
	putMarker(t, stale, phasePrepared, nil)
	backdate(t, nil, stale, 11*time.Minute)
	sweep(t, adapter)
	got := samples(adapter)
	if got["wherry_adapter_sweep_deletions_total/job"].Value != 1 || got["wherry_adapter_sweep_deletions_total/variable"].Value != 1 || got["wherry_adapter_cleanup_pending"].Value != 1 {
		t.Fatalf("samples %+v; want one job, one variable and one pending", got)
	}
	for key := range got {
		if family, kind, ok := strings.Cut(key, "/"); ok && family == "wherry_adapter_sweep_deletions_total" && kind != "job" && kind != "variable" {
			t.Fatalf("deletion kind %s", kind)
		}
	}
}

func TestReadyFromSystemState(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	for state, want := range map[string]bool{"running": true, "degraded": true, "initializing": false, "starting": true, "maintenance": false, "stopping": false} {
		f.set(func() { f.state = state })
		if ready, err := adapter.Ready(context.Background()); err != nil || ready != want {
			t.Errorf("%s: ready %v, %v; want %v", state, ready, err, want)
		}
	}
	f.fail("state", dbus.Error{Name: "org.freedesktop.DBus.Error.NoReply"})
	if ready, err := adapter.Ready(context.Background()); ready || err == nil {
		t.Fatalf("a failed read: %v, %v; want not ready and the error", ready, err)
	}
}

func classOf(t *testing.T, adapter *Adapter, err error) string {
	t.Helper()
	var failure *wherry.Error
	if !errors.As(adapter.failed("op", err), &failure) {
		t.Fatalf("%v: not an adapter error", err)
	}
	return failure.Class
}

func TestDBusErrorsAreClassified(t *testing.T) {
	newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	for name, want := range map[string]string{
		"org.freedesktop.DBus.Error.AccessDenied":                     "denied",
		"org.freedesktop.DBus.Error.InteractiveAuthorizationRequired": "denied",
		"org.freedesktop.systemd1.UnitExists":                         "conflict",
		"org.freedesktop.systemd1.TransactionIsDestructive":           "conflict",
		"org.freedesktop.systemd1.TransactionJobsConflicting":         "conflict",
		"org.freedesktop.systemd1.JobTypeNotApplicable":               "conflict",
		"org.freedesktop.DBus.Error.NoReply":                          "timeout",
		"org.freedesktop.DBus.Error.Timeout":                          "timeout",
		"org.freedesktop.DBus.Error.TimedOut":                         "timeout",
		"org.freedesktop.DBus.Error.ServiceUnknown":                   "unavailable",
		"org.freedesktop.DBus.Error.NoServer":                         "unavailable",
		"org.freedesktop.DBus.Error.Disconnected":                     "unavailable",
		"org.freedesktop.DBus.Error.InvalidArgs":                      "other",
		busNoSuchUnit:                                                 "",
	} {
		if got := classOf(t, adapter, dbus.Error{Name: name}); got != want {
			t.Errorf("%s: class %q; want %q", name, got, want)
		}
	}
	for err, want := range map[error]string{
		dbus.ErrClosed:           "unavailable",
		context.DeadlineExceeded: "timeout",
		context.Canceled:         "",
		errors.New("odd"):        "other",
	} {
		if got := classOf(t, adapter, err); got != want {
			t.Errorf("%v: class %q; want %q", err, got, want)
		}
	}
	if got := samples(adapter); got["wherry_adapter_platform_errors_total/"].Value != 0 {
		t.Fatalf("an error without a class was counted: %+v", got)
	}
	total := 0.0
	for key, sample := range samples(adapter) {
		if strings.HasPrefix(key, "wherry_adapter_platform_errors_total/") {
			total += sample.Value
		}
	}
	if total != 16 {
		t.Fatalf("counted %v errors; want 16, none for NoSuchUnit or a cancellation", total)
	}
}

func TestFilesystemErrorsAreClassified(t *testing.T) {
	newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	for errno, want := range map[syscall.Errno]string{
		syscall.EACCES: "denied", syscall.EPERM: "denied", syscall.EROFS: "denied",
		syscall.ENOSPC: "unavailable", syscall.EDQUOT: "unavailable", syscall.EIO: "unavailable",
		syscall.ENOENT: "", syscall.EINVAL: "other",
	} {
		if got := classOf(t, adapter, &fs.PathError{Op: "write", Path: "/x", Err: errno}); got != want {
			t.Errorf("%v: class %q; want %q", errno, got, want)
		}
	}
}

func TestClosedConnectionIsRedialled(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	if _, err := adapter.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.set(func() { f.connected = false })
	if _, err := adapter.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.dials != 2 {
		t.Fatalf("dials %d; want a second after the connection closed", f.dials)
	}
	f.set(func() { f.connected = false })
	f.fail("dial", errors.New("connection refused"))
	if _, err := adapter.Ready(context.Background()); err == nil {
		t.Fatal("a failed dial succeeded")
	}
	if _, err := adapter.Ready(context.Background()); err != nil {
		t.Fatalf("the call after a failed dial: %v", err)
	}
}

func TestReturnedErrorCarriesOpAndClass(t *testing.T) {
	f := newHost(t)
	adapter := newAgent(t, agentConfig("exit 0"))
	run := testRun("op")
	f.fail("start", dbus.Error{Name: "org.freedesktop.DBus.Error.AccessDenied", Body: []any{"Access denied"}})
	_, _, err := adapter.Dispatch(context.Background(), run)
	var failure *wherry.Error
	if !errors.As(err, &failure) || failure.Op != "start unit "+unitOf(run) || failure.Class != "denied" {
		t.Fatalf("got %#v; want op and class", err)
	}
	if !strings.Contains(err.Error(), "Access denied") {
		t.Fatalf("error %q drops systemd's text", err)
	}
}

func TestHandledErrorIsLoggedOnceWithoutText(t *testing.T) {
	newHost(t)
	var logs bytes.Buffer
	fast(t)
	adapter, err := New(agentConfig("exit 0"), testProfile, testPool, hclog.New(&hclog.LoggerOptions{Output: &logs}))
	if err != nil {
		t.Fatal(err)
	}
	adapter.handled("remove credentials of unit", &fs.PathError{Op: "unlinkat", Path: "/run/secret-text", Err: syscall.EIO})
	adapter.handled("read unit", dbus.Error{Name: busNoSuchUnit, Body: []any{"secret-text"}})
	if lines := strings.Split(strings.TrimSpace(logs.String()), "\n"); len(lines) != 1 || !strings.Contains(lines[0], "systemd call failed") ||
		!strings.Contains(lines[0], "class=unavailable") || !strings.Contains(lines[0], `op="remove credentials of unit"`) || strings.Contains(logs.String(), "secret-text") {
		t.Fatalf("logs %q; want one line with op and class and no text", logs.String())
	}
}

func TestMaterialNeverInPropertiesLogsOrErrors(t *testing.T) {
	f := newHost(t)
	var logs bytes.Buffer
	fast(t)
	adapter, err := New(agentConfig(`cat prompt.txt inputs/* > /dev/null; mkdir outputs; printf 'output-%s-secret' bytes > outputs/reply.md; echo '{"type":"result"}'`),
		testProfile, testPool, hclog.New(&hclog.LoggerOptions{Output: &logs, Level: hclog.Trace}))
	if err != nil {
		t.Fatal(err)
	}
	object := []byte("fetched-input-secret")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fails" {
			http.Error(w, "nope", http.StatusForbidden)
			return
		}
		_, _ = w.Write(object)
	}))
	defer server.Close()
	url := server.URL + "/bucket/object?X-Amz-Signature=presigned-url-secret"
	failing := server.URL + "/fails?X-Amz-Signature=presigned-url-secret"
	run := testRun("material")
	run.Prompt = []byte("prompt-secret")
	run.Model = "opus"
	run.Inputs = []wherry.File{{Name: "in.txt", Data: []byte("inline-input-secret")}}
	run.Credentials = []wherry.Secret{
		{Name: "model", Env: credentialEnv, Value: []byte("credential-secret")},
		{Name: "second", Env: "SECOND_TOKEN", Value: []byte("second-env-secret")},
		{Name: "deploy", File: "deploy-key", Value: []byte("task-credential-secret")},
		{Name: "token", File: "token", Value: []byte("second-file-secret")},
	}
	run.Fetches = []wherry.Fetch{{Name: "object.bin", URL: url, Size: int64(len(object)), SHA256: sha(object)}}
	var errs []error
	note := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	handle := dispatch(t, adapter, run)
	_, _, err = followAll(t, adapter, handle, 0)
	note(err)
	note(adapter.Stop(context.Background(), handle))
	outputs, err := collect(t, adapter, handle, limits)
	note(err)
	note(adapter.Release(context.Background(), run.ID))
	note(adapter.Purge(context.Background(), run.ID))
	note(adapter.Sweep(context.Background()))
	broken := testRun("material-fails")
	broken.Credentials = run.Credentials
	broken.Fetches = []wherry.Fetch{{Name: "object.bin", URL: failing, Size: 1, SHA256: sha([]byte("x"))}}
	dispatch(t, adapter, broken)
	_, terminal, err := followAll(t, adapter, dispatch(t, adapter, broken), 0)
	note(err)
	unreachable := testRun("material-unreachable")
	unreachable.Fetches = []wherry.Fetch{{Name: "object.bin", URL: "https://127.0.0.1:1/x?X-Amz-Signature=presigned-url-secret", Size: 1, SHA256: sha([]byte("x"))}}
	dispatch(t, adapter, unreachable)
	f.fail("start", dbus.Error{Name: "org.freedesktop.DBus.Error.AccessDenied", Body: []any{"denied"}})
	_, _, err = adapter.Dispatch(context.Background(), testRun("material-refused"))
	note(err)
	if len(outputs) != 1 {
		t.Fatalf("outputs %v; want the reply", names(outputs))
	}
	var seen strings.Builder
	f.mu.Lock()
	for _, properties := range f.starts {
		for _, p := range properties {
			fmt.Fprintf(&seen, "%s=%s\n", p.Name, p.Value.Value())
		}
	}
	f.mu.Unlock()
	fmt.Fprintf(&seen, "%s\n%+v\n", logs.String(), terminal)
	for _, err := range errs {
		fmt.Fprintln(&seen, err)
	}
	for _, secret := range []string{"prompt-secret", "credential-secret", "second-env-secret", "inline-input-secret", "task-credential-secret", "second-file-secret", "fetched-input-secret", "presigned-url-secret", "X-Amz", "output-bytes-secret", "/bucket/object"} {
		if strings.Contains(seen.String(), secret) {
			t.Errorf("%s reached a property, log or error:\n%s", secret, seen.String())
		}
	}
	if !strings.Contains(logs.String(), "input not fetched") {
		t.Errorf("logs %q; want the failed fetch logged", logs.String())
	}
}
