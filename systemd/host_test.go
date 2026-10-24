package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	wherry "github.com/benemon/wherry/adapter"
	"github.com/benemon/wherry/adapter/adaptertest"
	sd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/hashicorp/go-hclog"
)

// The real-host suite runs as root on a host with systemd 252 or later and cgroup v2 when WHERRY_SYSTEMD_DEV is 1, and
// skips otherwise. It uses the plugin's own host paths and a pool of its own per test.

func onSystemd(t *testing.T) {
	t.Helper()
	if os.Getenv("WHERRY_SYSTEMD_DEV") != "1" {
		t.Skip("WHERRY_SYSTEMD_DEV not set")
	}
	if os.Geteuid() != 0 {
		t.Skip("the real-host suite runs as root")
	}
}

func hostPool(t *testing.T) string {
	t.Helper()
	random := make([]byte, 4)
	_, _ = rand.Read(random)
	return "wherry-test-" + hex.EncodeToString(random)
}

// hostAdapter configures an adapter of a fresh pool whose runs are purged after the test.
func hostAdapter(t *testing.T, config Config) *Adapter {
	t.Helper()
	onSystemd(t)
	adapter, err := New(config, testAdapter, hostPool(t), hclog.NewNullLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		entries, _ := os.ReadDir(controlRoot)
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), adapter.jobName("")) {
				if err := adapter.purge(ctx, entry.Name()); err != nil {
					t.Errorf("purge %s: %v", entry.Name(), err)
				}
			}
		}
	})
	return adapter
}

func shConfig(script string) Config {
	config := agentConfig(script)
	config.Runtime.Env = nil
	return config
}

// hostTurn dispatches run, follows it to its end and collects its outputs.
func hostTurn(t *testing.T, adapter *Adapter, run wherry.Run) ([]wherry.Line, wherry.Terminal, map[string]wherry.Output) {
	t.Helper()
	handle := dispatch(t, adapter, run)
	lines, terminal, err := followAll(t, adapter, handle, 0)
	if err != nil {
		t.Fatal(err)
	}
	return lines, terminal, hostOutputs(t, adapter, handle)
}

func hostOutputs(t *testing.T, adapter *Adapter, handle wherry.Handle) map[string]wherry.Output {
	t.Helper()
	outputs, err := collect(t, adapter, handle, wherry.Limits{Files: 256, FileBytes: 4 << 20, TotalBytes: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]wherry.Output{}
	for _, output := range outputs {
		byName[output.Name] = output
	}
	return byName
}

func hostConn(t *testing.T) *sd.Conn {
	t.Helper()
	conn, err := sd.NewSystemdConnectionContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(conn.Close)
	return conn
}

func conformanceSettings(t *testing.T) (wherry.Settings, wherry.Settings) {
	t.Helper()
	onSystemd(t)
	pool := hostPool(t)
	completes := settings(t, "completes", unitConfig("/bin/sh", "-c", `sleep 10; echo '{"type":"result","result":"done"}'`))
	neverStarts := settings(t, "never-starts", unitConfig("/nonexistent/wherry-conformance"))
	completes.Pool, neverStarts.Pool = pool, pool
	return completes, neverStarts
}

func TestConformance(t *testing.T) {
	completes, neverStarts := conformanceSettings(t)
	adaptertest.Run(t, newAdapter, completes, neverStarts, 0)
}

// TestConformanceOverTheLink runs the suite against the built plugin, through the receiver's side of the link.
func TestConformanceOverTheLink(t *testing.T) {
	completes, neverStarts := conformanceSettings(t)
	binary := filepath.Join(t.TempDir(), "wherry-plugin-systemd")
	if output, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	plugin, err := wherry.Launch(func() (string, error) { return binary, nil }, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer plugin.Close()
	adaptertest.Run(t, func(settings wherry.Settings, _ hclog.Logger) (wherry.Adapter, error) {
		return plugin.Configure(settings)
	}, completes, neverStarts, 0)
}

// TestReceiverBoundingSetOnSystemd runs the conformance suite in a process whose bounding set is the receiver unit's.
func TestReceiverBoundingSetOnSystemd(t *testing.T) {
	onSystemd(t)
	if os.Getenv("WHERRY_SYSTEMD_BOUNDED") == "1" {
		t.Skip("already bounded")
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := os.Getwd()
	output, err := exec.Command("systemd-run", "--wait", "--pipe", "--collect", "--quiet",
		"-p", "CapabilityBoundingSet=CAP_DAC_OVERRIDE CAP_DAC_READ_SEARCH CAP_FOWNER",
		"-p", "WorkingDirectory="+dir, "-E", "WHERRY_SYSTEMD_DEV=1", "-E", "WHERRY_SYSTEMD_BOUNDED=1",
		binary, "-test.run", "^TestConformance$", "-test.v", "-test.count", "1").CombinedOutput()
	if err != nil || !bytes.Contains(output, []byte("--- PASS: TestConformance")) {
		t.Fatalf("the conformance suite under the receiver's bounding set: %v\n%s", err, output)
	}
}

func TestUnitPropertiesOnSystemd(t *testing.T) {
	adapter := hostAdapter(t, shConfig("sleep 30"))
	run := testRun("properties")
	run.Credential = []byte("model-key")
	run.Credentials = []wherry.Secret{{Name: "deploy", File: "deploy-key", Value: []byte("key")}}
	sent := map[string]any{}
	for _, p := range adapter.properties(run, adapter.jobName(run.ID), []loadCredential{{agentCredential, "unused"}}) {
		sent[p.Name] = p.Value.Value()
	}
	handle := dispatch(t, adapter, run)
	conn := hostConn(t)
	unitProps, err := conn.GetUnitPropertiesContext(context.Background(), handle.Name)
	if err != nil {
		t.Fatal(err)
	}
	serviceProps, err := conn.GetUnitTypePropertiesContext(context.Background(), handle.Name, "Service")
	if err != nil {
		t.Fatal(err)
	}
	// Structs read back as slices of their fields; ExecStartEx reads back with its run's timestamps and PIDs.
	normal := func(value any) string {
		return strings.NewReplacer("{", "[", "}", "]").Replace(fmt.Sprint(value))
	}
	for name, value := range sent {
		switch name {
		case "StandardOutputFileToAppend", "StandardErrorFileToAppend", "LoadCredential":
			continue
		case "ExecStartEx":
			got, _ := serviceProps[name].([][]any)
			want := value.([]execStart)[0]
			if len(got) != 1 || normal(got[0][:3]) != normal([]any{want.Path, want.Args, want.Flags}) {
				t.Errorf("ExecStartEx read back %v; want %+v", got, want)
			}
			continue
		case "RestrictAddressFamilies":
			got := serviceProps[name].([]any)
			families := slices.Sorted(slices.Values(got[1].([]string)))
			if got[0] != true || !slices.Equal(families, []string{"AF_INET", "AF_INET6", "AF_NETLINK", "AF_UNIX"}) {
				t.Errorf("RestrictAddressFamilies read back %v", got)
			}
			continue
		}
		got, ok := serviceProps[name]
		if !ok {
			got, ok = unitProps[name]
		}
		if !ok || normal(got) != normal(value) {
			t.Errorf("%s read back %v; sent %v", name, got, value)
		}
	}
	for _, name := range []string{"StandardOutput", "StandardError"} {
		if got := serviceProps[name]; got != "append" {
			t.Errorf("%s = %v; want append", name, got)
		}
	}
	if got := serviceProps["LoadCredential"].([][]any); len(got) != 2 {
		t.Errorf("LoadCredential read back %v; want two", got)
	}
}

func TestDaemonReloadKeepsTheUnit(t *testing.T) {
	adapter := hostAdapter(t, shConfig(`sleep 5; echo '{"type":"result"}'`))
	run := testRun("reload")
	handle := dispatch(t, adapter, run)
	conn := hostConn(t)
	before, err := conn.GetUnitTypePropertiesContext(context.Background(), handle.Name, "Service")
	if err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("systemctl", "daemon-reload").CombinedOutput(); err != nil {
		t.Fatalf("daemon-reload: %v\n%s", err, output)
	}
	after, err := conn.GetUnitTypePropertiesContext(context.Background(), handle.Name, "Service")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Environment", "InaccessiblePaths", "BindPaths", "BindReadOnlyPaths", "DynamicUser", "LimitCORE", "StateDirectory"} {
		if fmt.Sprint(before[name]) != fmt.Sprint(after[name]) {
			t.Errorf("%s: %v before the reload, %v after", name, before[name], after[name])
		}
	}
	if fmt.Sprint(before["ExecStartEx"].([][]any)[0][:3]) != fmt.Sprint(after["ExecStartEx"].([][]any)[0][:3]) {
		t.Errorf("ExecStartEx changed across the reload")
	}
	lines, terminal, err := followAll(t, adapter, handle, 0)
	if err != nil || terminal.ExitCode != 0 || !slices.Equal(texts(lines), []string{`{"type":"result"}`}) {
		t.Fatalf("lines %q, terminal %+v, %v; want the run completed", texts(lines), terminal, err)
	}
}

const viewScript = `mkdir -p outputs
{
echo "uid $(id -u)"
echo "home $HOME"
echo "pwd $(pwd)"
echo "prompt $(stat -c '%a %u' /wherry/prompt.txt)"
echo "inputs $(stat -c '%a' /wherry/inputs)"
echo "credentials $(ls -A /run/wherry/credentials | tr '\n' ' ')"
echo "shell $(printenv SHELL)"
touch /etc/wherry-probe 2>/dev/null && echo "etc writable" || echo "etc refused"
ls /var/lib/wherry >/dev/null 2>&1 && echo "state listed" || echo "state refused"
echo "homes $(ls -A /home 2>/dev/null | wc -l)"
cat /run/systemd/transient/* >/dev/null 2>&1 && echo "transient read" || echo "transient refused"
ls -A /run/wherry-systemd 2>/dev/null | grep -q . && echo "credential files listed" || echo "credential files refused"
[ -e /proc/1 ] && echo "pid 1 visible" || echo "pid 1 hidden"
logger "$PROBE" 2>/dev/null && echo "logger wrote" || echo "logger refused"
} > outputs/view`

func view(t *testing.T, output wherry.Output) map[string]string {
	t.Helper()
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(output.Data)), "\n") {
		if key, value, ok := strings.Cut(line, " "); ok && slices.Contains([]string{"uid", "home", "pwd", "prompt", "inputs", "credentials", "shell", "homes"}, key) {
			got[key] = strings.TrimSpace(value)
		} else {
			got[line] = "seen"
		}
	}
	return got
}

func TestAgentViewOnSystemd(t *testing.T) {
	probe := "wherry-probe-" + hex.EncodeToString([]byte(time.Now().Format("150405.000")))
	config := shConfig(viewScript)
	config.Runtime.Env = map[string]string{"PROBE": probe}
	adapter := hostAdapter(t, config)
	withShell := shConfig(viewScript)
	withShell.Runtime.Env = map[string]string{"PROBE": probe, "SHELL": "/bin/bash"}
	bash := hostAdapter(t, withShell)
	credentialed := testRun("view-credentials")
	credentialed.Credential = []byte("model-key")
	credentialed.Credentials = []wherry.Secret{{Name: "deploy", File: "deploy-key", Value: []byte("key")}, {Name: "token", File: "token", Value: []byte("t")}}
	for _, c := range []struct {
		adapter     *Adapter
		run         wherry.Run
		credentials string
		shell       string
	}{
		{adapter, credentialed, ".credential deploy-key token", "/bin/sh"},
		{adapter, testRun("view-plain"), "", "/bin/sh"},
		{bash, testRun("view-bash"), "", "/bin/bash"},
	} {
		_, terminal, outputs := hostTurn(t, c.adapter, c.run)
		if terminal.ExitCode != 0 {
			t.Fatalf("%s: terminal %+v", c.run.ID, terminal)
		}
		got := view(t, outputs["view"])
		uid, _ := strconv.Atoi(got["uid"])
		switch {
		case uid < 61184 || uid > 65519:
			t.Errorf("%s: uid %d; want a dynamic user", c.run.ID, uid)
		case got["home"] != "/wherry" || got["pwd"] != "/wherry":
			t.Errorf("%s: HOME %s, pwd %s", c.run.ID, got["home"], got["pwd"])
		case got["prompt"] != "600 "+got["uid"] || got["inputs"] != "700":
			t.Errorf("%s: prompt %s, inputs %s", c.run.ID, got["prompt"], got["inputs"])
		case got["credentials"] != c.credentials:
			t.Errorf("%s: credentials %q; want %q", c.run.ID, got["credentials"], c.credentials)
		case got["shell"] != c.shell:
			t.Errorf("%s: SHELL %s; want %s", c.run.ID, got["shell"], c.shell)
		case got["homes"] != "0":
			t.Errorf("%s: /home holds %s entries; want none", c.run.ID, got["homes"])
		}
		for _, want := range []string{"etc refused", "state refused", "transient refused", "credential files refused", "pid 1 hidden", "logger refused"} {
			if got[want] != "seen" {
				t.Errorf("%s: want %q in %v", c.run.ID, want, got)
			}
		}
		if mode := mode(t, filepath.Join(controlRoot, c.adapter.jobName(c.run.ID))); mode != 0o700 {
			t.Errorf("%s: control directory mode %o; want 0700", c.run.ID, mode)
		}
	}
	if output, _ := exec.Command("journalctl", "-o", "export", "--since", "-10min").Output(); bytes.Contains(output, []byte(probe)) {
		t.Error("an agent wrote to the journal")
	}
}

func TestFixedUserOnSystemd(t *testing.T) {
	config := shConfig(`mkdir outputs; id -u > outputs/uid; id -G > outputs/groups; stat -c '%u' /wherry > outputs/owner`)
	config.Runtime.User, config.Runtime.Group, config.Runtime.SupplementaryGroups = "nobody", "nobody", []string{"adm"}
	adapter := hostAdapter(t, config)
	_, terminal, outputs := hostTurn(t, adapter, testRun("fixed"))
	if terminal.ExitCode != 0 || strings.TrimSpace(string(outputs["uid"].Data)) != "65534" || strings.TrimSpace(string(outputs["owner"].Data)) != "65534" {
		t.Fatalf("terminal %+v, uid %q, owner %q; want the run as nobody", terminal, outputs["uid"].Data, outputs["owner"].Data)
	}
	if !exists(filepath.Join(agentRoot(false), adapter.jobName("run-fixed"))) {
		t.Fatal("the fixed user's work directory is not under /var/lib/wherry-agents")
	}
}

func TestCredentialFileOwnerAndModeOnSystemd(t *testing.T) {
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen not installed")
	}
	key := filepath.Join(t.TempDir(), "id_ed25519")
	if output, err := exec.Command(keygen, "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, output)
	}
	private, err := os.ReadFile(key)
	if err != nil {
		t.Fatal(err)
	}
	adapter := hostAdapter(t, shConfig(`mkdir outputs; stat -c '%a %u' "$WHERRY_CREDENTIALS_DIR/deploy-key" > outputs/file; stat -c '%a' "$WHERRY_CREDENTIALS_DIR" > outputs/dir; id -u > outputs/uid; ssh-keygen -y -f "$WHERRY_CREDENTIALS_DIR/deploy-key" > outputs/public`))
	run := testRun("key")
	run.Credentials = []wherry.Secret{{Name: "deploy", File: "deploy-key", Value: private}}
	_, _, outputs := hostTurn(t, adapter, run)
	uid := strings.TrimSpace(string(outputs["uid"].Data))
	if got := strings.TrimSpace(string(outputs["file"].Data)); got != "400 "+uid {
		t.Errorf("credential file %q; want 400 %s", got, uid)
	}
	if got := strings.TrimSpace(string(outputs["dir"].Data)); got != "500" {
		t.Errorf("credential directory %q; want 500", got)
	}
	if !bytes.HasPrefix(outputs["public"].Data, []byte("ssh-ed25519 ")) {
		t.Errorf("public key %q", outputs["public"].Data)
	}
}

func TestCredentialEnvOnSystemd(t *testing.T) {
	adapter := hostAdapter(t, shConfig(`mkdir outputs; printf %s "$`+credentialEnv+`" > outputs/env`))
	run := testRun("env")
	run.Credential = []byte("model-key with $dollar and trailing newline\n")
	_, _, outputs := hostTurn(t, adapter, run)
	if !bytes.Equal(outputs["env"].Data, run.Credential) {
		t.Fatalf("the agent's %s is %q; want %q", credentialEnv, outputs["env"].Data, run.Credential)
	}
}

func TestCredentialsAtTheLimitOnSystemd(t *testing.T) {
	adapter := hostAdapter(t, shConfig("exit 0"))
	run := testRun("limit")
	run.Credential = []byte("model-key")
	value := maxCredentials - len(agentCredential) - len(run.Credential) - len("deploy-key")
	run.Credentials = []wherry.Secret{{Name: "deploy", File: "deploy-key", Value: bytes.Repeat([]byte("k"), value)}}
	if _, terminal, _ := hostTurn(t, adapter, run); !terminal.Started || terminal.ExitCode != 0 {
		t.Fatalf("at the limit: terminal %+v; want the run started", terminal)
	}
	over := testRun("over")
	over.Credential = run.Credential
	over.Credentials = []wherry.Secret{{Name: "deploy", File: "deploy-key", Value: bytes.Repeat([]byte("k"), value+1)}}
	if outcome, _, err := adapter.Dispatch(context.Background(), over); outcome != wherry.OutcomeNothingCreated || err == nil {
		t.Fatalf("one byte over: outcome %d, %v", outcome, err)
	}
	conn := hostConn(t)
	if props, err := conn.GetUnitPropertiesContext(context.Background(), unitName(adapter.jobName(over.ID))); err != nil || props["LoadState"] == "loaded" {
		t.Fatalf("one byte over: unit %v, %v; want none", props["LoadState"], err)
	}
}

func TestTwoRunsCannotReachEachOther(t *testing.T) {
	first := hostAdapter(t, shConfig("sleep 60"))
	run := testRun("first")
	run.Credential = []byte("first-credential")
	dispatch(t, first, run)
	u, err := first.unit(context.Background(), unitName(first.jobName(run.ID)))
	if err != nil || u.mainPID == 0 {
		t.Fatalf("first unit %+v, %v", u, err)
	}
	config := shConfig(`mkdir outputs
ls /proc/$TARGET >/dev/null 2>&1 && echo "proc listed" >> outputs/reach
cat /proc/$TARGET/environ >/dev/null 2>&1 && echo "environ read" >> outputs/reach
kill -0 $TARGET 2>/dev/null && echo "signalled" >> outputs/reach
ls $TARGET_DIR >/dev/null 2>&1 && echo "work listed" >> outputs/reach
ls /var/lib/wherry-agents/$TARGET_JOB/ >/dev/null 2>&1 && echo "symlink listed" >> outputs/reach
true`)
	config.Runtime.Env = map[string]string{"TARGET": strconv.Itoa(int(u.mainPID)), "TARGET_DIR": first.workDir(first.jobName(run.ID)), "TARGET_JOB": first.jobName(run.ID)}
	second := hostAdapter(t, config)
	_, _, outputs := hostTurn(t, second, testRun("second"))
	if reach, ok := outputs["reach"]; ok {
		t.Fatalf("the second run reached the first: %s", reach.Data)
	}
}

func TestFetchOnSystemd(t *testing.T) {
	adapter := hostAdapter(t, shConfig(`mkdir outputs; cat inputs/data.bin > outputs/copy`))
	body := []byte("object bytes")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
	defer server.Close()
	adapter.fetcher.Transport = server.Client().Transport
	_, _, outputs := hostTurn(t, adapter, fetchRun("fetch", server.URL, body))
	if !bytes.Equal(outputs["copy"].Data, body) {
		t.Fatalf("the agent read %q; want %q", outputs["copy"].Data, body)
	}
	altered := fetchRun("fetch-altered", server.URL, []byte("uploaded bytes"))
	handle := dispatch(t, adapter, altered)
	if _, terminal, err := followAll(t, adapter, handle, 0); err != nil || terminal.Started || terminal.Class != wherry.ClassFetch {
		t.Fatalf("terminal %+v, %v; want never started, fetch", terminal, err)
	}
}

func stopRunning(t *testing.T, adapter *Adapter, run wherry.Run) wherry.Handle {
	t.Helper()
	handle := dispatch(t, adapter, run)
	time.Sleep(time.Second)
	began := time.Now()
	if err := adapter.Stop(context.Background(), handle); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(began); elapsed > 35*time.Second {
		t.Fatalf("stop took %v", elapsed)
	}
	return handle
}

func TestStopKeepsOutputsOnSystemd(t *testing.T) {
	adapter := hostAdapter(t, shConfig(`mkdir outputs; echo kept > outputs/kept; sleep 600`))
	handle := stopRunning(t, adapter, testRun("stop-outputs"))
	if outputs := hostOutputs(t, adapter, handle); string(outputs["kept"].Data) != "kept\n" {
		t.Fatalf("outputs %v; want kept", outputs)
	}
}

func TestStopEscalatesOnSystemd(t *testing.T) {
	adapter := hostAdapter(t, shConfig(`trap '' TERM; while :; do sleep 1; done`))
	handle := stopRunning(t, adapter, testRun("stop-escalates"))
	if job, err := adapter.Lookup(context.Background(), "run-stop-escalates"); err != nil || job.State != wherry.JobEnded || job.Handle.Name != handle.Name {
		t.Fatalf("lookup %+v, %v; want ended", job, err)
	}
}

func TestStopKillsLeftoversOnSystemd(t *testing.T) {
	adapter := hostAdapter(t, shConfig(`sleep 600 & exit 0`))
	run := testRun("leftovers")
	handle := dispatch(t, adapter, run)
	if _, _, err := followAll(t, adapter, handle, 0); err != nil {
		t.Fatal(err)
	}
	name := unitName(adapter.jobName(run.ID))
	if empty, err := adapter.empty(context.Background(), name); err != nil || empty {
		t.Fatalf("empty %v, %v; want the background sleep in the cgroup", empty, err)
	}
	if err := adapter.Stop(context.Background(), handle); err != nil {
		t.Fatal(err)
	}
	if empty, err := adapter.empty(context.Background(), name); err != nil || !empty {
		t.Fatalf("empty %v, %v after the stop", empty, err)
	}
}

func TestCollectAfterAgentOOMOnSystemd(t *testing.T) {
	config := shConfig(`mkdir outputs; echo before > outputs/before; head -c 1g /dev/zero | tail > /dev/null`)
	config.Runtime.Resources.MemoryMB = new(64)
	adapter := hostAdapter(t, config)
	_, terminal, outputs := hostTurn(t, adapter, testRun("oom"))
	if terminal.Signal != "9" || string(outputs["before"].Data) != "before\n" {
		t.Fatalf("terminal %+v, outputs %v; want signal 9 and the output", terminal, outputs)
	}
}

func TestAdoptionOnSystemd(t *testing.T) {
	config := shConfig(`mkdir outputs; i=0; while [ $i -lt 20 ]; do echo "line $i"; i=$((i+1)); sleep 0.5; done; echo done > outputs/done`)
	first := hostAdapter(t, config)
	run := testRun("adopt")
	handle := dispatch(t, first, run)
	ctx, cancel := context.WithCancel(context.Background())
	lines := make(chan wherry.Line)
	followed := make(chan error, 1)
	go func() {
		_, err := first.Follow(ctx, handle, 0, lines)
		followed <- err
	}()
	var seen []wherry.Line
	for len(seen) < 4 {
		seen = append(seen, <-lines)
	}
	cancel()
	<-followed
	second, err := New(config, testAdapter, first.pool, hclog.NewNullLogger())
	if err != nil {
		t.Fatal(err)
	}
	job, err := second.Lookup(context.Background(), run.ID)
	if err != nil || job.State != wherry.JobRunning {
		t.Fatalf("lookup %+v, %v; want running", job, err)
	}
	rest, terminal, err := followAll(t, second, job.Handle, seen[len(seen)-1].Next)
	if err != nil || terminal.ExitCode != 0 {
		t.Fatalf("terminal %+v, %v", terminal, err)
	}
	all := texts(append(seen, rest...))
	for i, line := range all {
		if line != fmt.Sprintf("line %d", i) {
			t.Fatalf("lines %q; want line 0 to line 19 with no gap and no repeat", all)
		}
	}
	if len(all) != 20 {
		t.Fatalf("%d lines; want 20", len(all))
	}
	if err := second.Stop(context.Background(), job.Handle); err != nil {
		t.Fatal(err)
	}
	if outputs := hostOutputs(t, second, job.Handle); string(outputs["done"].Data) != "done\n" {
		t.Fatalf("outputs %v", outputs)
	}
}

func TestLargeStreamedOutputOnSystemd(t *testing.T) {
	adapter := hostAdapter(t, shConfig(`mkdir outputs; head -c 1536m /dev/zero > outputs/large.bin`))
	handle := dispatch(t, adapter, testRun("large"))
	if _, _, err := followAll(t, adapter, handle, 0); err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	outputs, err := collect(t, adapter, handle, wherry.Limits{Files: 8, Stream: []string{"large.bin"}, StreamBytes: 2 << 30})
	if err != nil || len(outputs) != 1 || outputs[0].Body == nil {
		t.Fatalf("outputs %v, %v; want large.bin streamed", outputs, err)
	}
	defer outputs[0].Body.Close()
	n, err := io.Copy(io.Discard, outputs[0].Body)
	if err != nil || n != 1536<<20 {
		t.Fatalf("streamed %d bytes, %v", n, err)
	}
	t.Logf("streamed %d bytes in %v", n, time.Since(began))
}

func TestStderrStaysOutOfTheTranscriptOnSystemd(t *testing.T) {
	adapter := hostAdapter(t, shConfig(`printf '{"type":'; echo noise >&2; printf '"result"}\n'`))
	lines, _, _ := hostTurn(t, adapter, testRun("stderr"))
	if !slices.Equal(texts(lines), []string{`{"type":"result"}`}) {
		t.Fatalf("lines %q; want the result line whole", texts(lines))
	}
}

// unlinking removes a credential file before the start, as a source that has gone.
type unlinking struct{ manager }

func (u unlinking) StartTransientUnitContext(ctx context.Context, name, mode string, properties []sd.Property, ch chan<- string) (int, error) {
	_ = os.Remove(filepath.Join(credentialRoot, name, "deploy-key"))
	return u.manager.StartTransientUnitContext(ctx, name, mode, properties, ch)
}

func TestFailedStartsOnSystemd(t *testing.T) {
	missing := hostAdapter(t, missingCommand())
	if _, terminal, err := followAll(t, missing, dispatch(t, missing, testRun("missing")), 0); err != nil || terminal.Class != "exec" || terminal.Reason != "systemd exit status 203/EXEC" {
		t.Fatalf("terminal %+v, %v; want never started, exec", terminal, err)
	}
	adapter := hostAdapter(t, shConfig("exit 0"))
	bus, err := adapter.bus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	adapter.conn = unlinking{bus}
	run := testRun("no-source")
	run.Credentials = []wherry.Secret{{Name: "deploy", File: "deploy-key", Value: []byte("key")}}
	if _, terminal, err := followAll(t, adapter, dispatch(t, adapter, run), 0); err != nil || terminal.Class != "credentials" || terminal.Reason != "systemd exit status 243/CREDENTIALS" {
		t.Fatalf("terminal %+v, %v; want never started, credentials", terminal, err)
	}
}

func TestCoreDumpKeepsTheCredentialOutOfTheJournalOnSystemd(t *testing.T) {
	adapter := hostAdapter(t, shConfig(`sh -c 'kill -SEGV $$'; kill -SEGV $$`))
	pattern, _ := exec.Command("sysctl", "-n", "kernel.core_pattern").Output()
	t.Logf("kernel.core_pattern: %s", bytes.TrimSpace(pattern))
	since := time.Now().Add(-time.Second).Format("2006-01-02 15:04:05")
	run := testRun("core")
	run.Credential = []byte("core-dump-credential-" + hex.EncodeToString([]byte(since)))
	_, terminal, _ := hostTurn(t, adapter, run)
	if terminal.Signal != "11" {
		t.Errorf("terminal %+v; want signal 11", terminal)
	}
	time.Sleep(2 * time.Second)
	for _, args := range [][]string{
		{"-o", "export", "_SYSTEMD_UNIT=" + unitName(adapter.jobName(run.ID))},
		{"-o", "export", "-t", "systemd-coredump", "--since", since},
		{"-o", "export", "--since", since},
	} {
		output, err := exec.Command("journalctl", args...).Output()
		if err != nil {
			t.Fatalf("journalctl %v: %v", args, err)
		}
		if bytes.Contains(output, run.Credential) {
			t.Errorf("journalctl %v holds the credential", args)
		}
	}
}

func TestTransientFileHoldsNoMaterial(t *testing.T) {
	adapter := hostAdapter(t, shConfig("sleep 30"))
	run := testRun("transient")
	run.Prompt = []byte("transient-prompt-secret")
	run.Credential = []byte("transient-credential-secret")
	run.Credentials = []wherry.Secret{{Name: "deploy", File: "deploy-key", Value: []byte("transient-task-secret")}}
	run.Inputs = []wherry.File{{Name: "in.txt", Data: []byte("transient-input-secret")}}
	handle := dispatch(t, adapter, run)
	file, err := os.ReadFile(filepath.Join("/run/systemd/transient", handle.Name))
	if err != nil {
		t.Fatal(err)
	}
	props, err := hostConn(t).GetUnitTypePropertiesContext(context.Background(), handle.Name, "Service")
	if err != nil {
		t.Fatal(err)
	}
	seen := string(file) + fmt.Sprint(props["SetCredential"], props["LoadCredential"], props["Environment"], props["ExecStartEx"])
	for _, secret := range []string{"transient-prompt-secret", "transient-credential-secret", "transient-task-secret", "transient-input-secret"} {
		if strings.Contains(seen, secret) {
			t.Errorf("%s in the transient file or properties", secret)
		}
	}
}

func TestPurgeLeavesNothingOnSystemd(t *testing.T) {
	adapter := hostAdapter(t, shConfig(`mkdir outputs; echo x > outputs/x; sleep 30`))
	run := testRun("purge")
	run.Credential = []byte("key")
	handle := dispatch(t, adapter, run)
	if err := adapter.Purge(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	job := adapter.jobName(run.ID)
	for _, path := range slices.Concat(workDirs(job), []string{filepath.Join(controlRoot, job), filepath.Join(credentialRoot, handle.Name), filepath.Join("/run/credentials", handle.Name)}) {
		if exists(path) {
			t.Errorf("%s remains", path)
		}
	}
	props, err := hostConn(t).GetUnitPropertiesContext(context.Background(), handle.Name)
	if err != nil || props["LoadState"] != "not-found" {
		t.Fatalf("unit %v, %v; want not-found", props["LoadState"], err)
	}
}
