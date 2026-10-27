package main

import (
	"bytes"
	"context"
	"encoding/base64"
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
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	wherry "github.com/benemon/wherry/adapter"
	"github.com/benemon/wherry/adapter/adaptertest"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/api"
)

const (
	credentialEnv = "WHERRY_TEST_CREDENTIAL"
	testPool      = "build"
	// poolPrefix is agent- and the first 12 hex digits of sha256("build").
	poolPrefix = "agent-44575cf5b285-"
)

func testConfig(address string) Config {
	script := `printf '{"type":"result","result":"prompt=%s token=%s env=%s home=%s"}\n' "$(cat ${NOMAD_TASK_DIR}/prompt.txt)" "$` + credentialEnv + `" "$WHERRY_TEST_ENV" "$HOME"`
	return Config{
		Address: address, Namespace: "default",
		Runtime: Runtime{
			Driver:    "raw_exec",
			Config:    map[string]any{"command": "/bin/sh", "args": []any{"-c", script}},
			Env:       map[string]string{"WHERRY_TEST_ENV": "from-runtime"},
			Resources: Resources{CPU: new(100), MemoryMB: new(64)},
		},
		NodePool: "default", Datacenters: []string{"dc1"},
	}
}

// testProfile is the plugin profile build names, as testRun's runs do.
const testProfile = "container"

// build is newAdapter's path to one adapter named testProfile, from a Config rather than settings.
func build(config Config, pool, token string) (*Adapter, error) {
	client, err := newClient(config, token)
	if err != nil {
		return nil, err
	}
	return New(config, testProfile, pool, client, hclog.NewNullLogger())
}

func devAdapter(t *testing.T) (*Adapter, context.Context) {
	t.Helper()
	address := os.Getenv("WHERRY_NOMAD_DEV_ADDR")
	if address == "" {
		t.Skip("WHERRY_NOMAD_DEV_ADDR not set")
	}
	adapter, err := build(testConfig(address), testPool, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	return adapter, ctx
}

func testRun(name string) wherry.Run {
	id := fmt.Sprintf("run-%s-%d", name, time.Now().UnixNano())
	return wherry.Run{
		ID: id, Profile: testProfile, Agent: "coder", Attempt: 2,
		Prompt: []byte("hello-prompt"), Credentials: []wherry.Secret{{Name: "model", Env: credentialEnv, Value: []byte("test-credential")}},
	}
}

func cleanupRun(t *testing.T, adapter *Adapter, run wherry.Run) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = adapter.Release(ctx, run.ID)
		_, _, _ = adapter.client.Jobs().Deregister(adapter.jobID(run.ID), true, (&api.WriteOptions{}).WithContext(ctx))
	})
}

func dispatch(t *testing.T, adapter *Adapter, ctx context.Context, run wherry.Run) wherry.Handle {
	t.Helper()
	cleanupRun(t, adapter, run)
	outcome, handle, err := adapter.Dispatch(ctx, run)
	if err != nil || outcome != wherry.OutcomeCreated {
		t.Fatalf("dispatch = %v, %v", outcome, err)
	}
	return handle
}

func decoded(t *testing.T, encoded wherry.Handle) handle {
	t.Helper()
	h, err := decodeHandle(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func collect(t *testing.T, ctx context.Context, adapter *Adapter, handle wherry.Handle) []wherry.Line {
	t.Helper()
	lines := make(chan wherry.Line, 16)
	done := make(chan error, 1)
	go func() {
		_, err := adapter.Follow(ctx, handle, 0, lines)
		done <- err
	}()
	var got []wherry.Line
	for {
		select {
		case line := <-lines:
			got = append(got, line)
			if strings.Contains(string(line.Data), `"result"`) {
				return got
			}
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			for len(lines) > 0 {
				got = append(got, <-lines)
			}
			return got
		case <-ctx.Done():
			t.Fatal("follow timed out")
		}
	}
}

func TestReadyFromReadyNodes(t *testing.T) {
	adapter, ctx := devAdapter(t)
	if ready, err := adapter.Ready(ctx); err != nil || !ready {
		t.Fatalf("ready = %v, %v; want a ready dev node with raw_exec", ready, err)
	}
	adapter.nodePool = "wherry-no-such-pool"
	if ready, err := adapter.Ready(ctx); err != nil || ready {
		t.Fatalf("ready = %v, %v; want no node in another node pool", ready, err)
	}
}

func retriesOn(group *api.TaskGroup) bool {
	reschedule, restart := group.ReschedulePolicy, group.RestartPolicy
	return reschedule == nil || *reschedule.Attempts != 0 || *reschedule.Unlimited ||
		restart == nil || *restart.Attempts != 0 || *restart.Mode != api.RestartPolicyModeFail
}

func TestBuildJobFromRuntime(t *testing.T) {
	adapter, err := build(testConfig("http://127.0.0.1:1"), testPool, "")
	if err != nil {
		t.Fatal(err)
	}
	run := testRun("build")
	job, err := adapter.buildJob(run, adapter.variablePath(run.ID))
	if err != nil {
		t.Fatal(err)
	}
	wantMeta := map[string]string{"run_id": run.ID, "attempt": "2", "profile": "container", "agent": "coder"}
	if *job.ID != adapter.jobID(run.ID) || *job.Name != adapter.jobID(run.ID) || *job.Namespace != "default" || *job.Type != api.JobTypeBatch ||
		*job.NodePool != "default" || !reflect.DeepEqual(job.Datacenters, []string{"dc1"}) || !reflect.DeepEqual(job.Meta, wantMeta) {
		t.Fatalf("unexpected job identity or placement: %#v", job)
	}
	if len(job.TaskGroups) != 1 || retriesOn(job.TaskGroups[0]) {
		t.Fatalf("retries not forced off: groups=%#v", job.TaskGroups)
	}
	runtime := testConfig("").Runtime
	wantEnv := maps.Clone(wherry.AgentEnvironment)
	maps.Copy(wantEnv, map[string]string{"WHERRY_TEST_ENV": "from-runtime", "HOME": "${NOMAD_TASK_DIR}",
		"WHERRY_INPUT_DIR": "${NOMAD_TASK_DIR}/inputs", "WHERRY_OUTPUT_DIR": "${NOMAD_TASK_DIR}/outputs", "WHERRY_CREDENTIALS_DIR": "${NOMAD_SECRETS_DIR}/credentials"})
	tasks := job.TaskGroups[0].Tasks
	if len(tasks) != 1 || tasks[0].Name != taskName || tasks[0].Driver != "raw_exec" || !reflect.DeepEqual(tasks[0].Config, runtime.Config) ||
		*tasks[0].Resources.CPU != 100 || *tasks[0].Resources.MemoryMB != 64 ||
		*tasks[0].LogConfig.MaxFiles != 3 || *tasks[0].LogConfig.MaxFileSizeMB != 10 ||
		!reflect.DeepEqual(tasks[0].Env, wantEnv) {
		t.Fatalf("runtime not carried: %#v", tasks)
	}
	templates := tasks[0].Templates
	if len(templates) != 2 || *templates[0].DestPath != "local/prompt.txt" ||
		*templates[1].DestPath != "${NOMAD_SECRETS_DIR}/wherry.env" || !*templates[1].Envvars ||
		!strings.Contains(*templates[1].EmbeddedTmpl, credentialEnv+`={{ (index . "env/`+credentialEnv+`").Value | base64Decode }}`) ||
		*templates[0].ChangeMode != "noop" || *templates[1].ChangeMode != "noop" {
		t.Fatalf("templates %#v", templates)
	}
	run.Credentials = nil
	job, err = adapter.buildJob(run, adapter.variablePath(run.ID))
	if err != nil {
		t.Fatal(err)
	}
	if templates := job.TaskGroups[0].Tasks[0].Templates; len(templates) != 1 || *templates[0].DestPath != "local/prompt.txt" {
		t.Fatalf("run without a credential has templates %#v", templates)
	}
}

func TestJobIDIsTheRunID(t *testing.T) {
	adapter, err := build(testConfig("http://127.0.0.1:1"), testPool, "")
	if err != nil {
		t.Fatal(err)
	}
	run := testRun("ids")
	run.ID = "run-ids"
	path := adapter.variablePath(run.ID)
	job, err := adapter.buildJob(run, path)
	if err != nil {
		t.Fatal(err)
	}
	wantID := poolPrefix + run.ID
	if *job.ID != wantID || *job.Name != wantID || path != "nomad/jobs/"+wantID {
		t.Fatalf("job=%q name=%q path=%q, want %q", *job.ID, *job.Name, path, wantID)
	}
	if template := *job.TaskGroups[0].Tasks[0].Templates[0].EmbeddedTmpl; !strings.Contains(template, path) {
		t.Fatalf("template %q does not use %q", template, path)
	}
}

func TestNewValidatesConfig(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"driver":           func(config *Config) { config.Runtime = Runtime{} },
		"negative uid":     func(config *Config) { config.Runtime.User = "-1" },
		"gid not a number": func(config *Config) { config.Runtime.User = "1001:x" },
		"user name":        func(config *Config) { config.Runtime.User = "Bad Name" },
		"root uid":         func(config *Config) { config.Runtime.User = "0" },
		"root name":        func(config *Config) { config.Runtime.User = "root" },
		"env name":         func(config *Config) { config.Runtime.Env = map[string]string{"BAD-NAME": "x"} },
		"leading zero":     func(config *Config) { config.Runtime.User = "0001" },
		"uid over 32 bits": func(config *Config) { config.Runtime.User = "4294967296" },
		"uid over an int":  func(config *Config) { config.Runtime.User = "99999999999999999999" },
	} {
		t.Run(name, func(t *testing.T) {
			config := testConfig("http://127.0.0.1:1")
			mutate(&config)
			if _, err := build(config, testPool, ""); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}

func TestBuildJobRunsAsTheRuntimeUser(t *testing.T) {
	for user, owner := range map[string]*[2]int{"": nil, "agent": nil, "1001": {1001, 1001}, "1001:2002": {1001, 2002}} {
		t.Run(user, func(t *testing.T) {
			config := testConfig("http://127.0.0.1:1")
			config.Runtime.User = user
			adapter, err := build(config, testPool, "")
			if err != nil {
				t.Fatal(err)
			}
			run := testRun("user")
			run.Inputs = []wherry.File{{Name: "data.csv"}}
			run.Credentials = []wherry.Secret{{Name: "deploy-key", File: "id_ed25519"}, {Name: "token", File: "token"}}
			job, err := adapter.buildJob(run, adapter.variablePath(run.ID))
			if err != nil {
				t.Fatal(err)
			}
			task := job.TaskGroups[0].Tasks[0]
			if task.User != user {
				t.Fatalf("task user %q, want %q", task.User, user)
			}
			credentials := 0
			for _, template := range task.Templates {
				if !strings.HasPrefix(*template.DestPath, "${NOMAD_SECRETS_DIR}/credentials/") || owner == nil {
					if template.Uid != nil || template.Gid != nil || template.Perms != nil {
						t.Errorf("template for %s sets an owner or mode", *template.DestPath)
					}
					continue
				}
				credentials++
				if template.Uid == nil || template.Gid == nil || template.Perms == nil || *template.Uid != owner[0] || *template.Gid != owner[1] || *template.Perms != "0600" {
					t.Errorf("template for %s = uid %s gid %s perms %s, want %d:%d 0600", *template.DestPath, show(template.Uid), show(template.Gid), show(template.Perms), owner[0], owner[1])
				}
			}
			if owner != nil && credentials != len(run.Credentials) {
				t.Fatalf("%d credential templates, want %d", credentials, len(run.Credentials))
			}
		})
	}
}

func TestNewAcceptsGroupsTheDriverSets(t *testing.T) {
	for name, runtime := range map[string]Runtime{
		"docker group and supplementary groups": {Driver: "docker", User: "1001", Group: "1002", SupplementaryGroups: []string{"video", "1003"}},
		"docker group name":                     {Driver: "docker", User: "agent", Group: "agents"},
		"podman group":                          {Driver: "podman", User: "1001", Group: "1002"},
		"exec without groups":                   {Driver: "exec", User: "agent"},
	} {
		t.Run(name, func(t *testing.T) {
			config := testConfig("http://127.0.0.1:1")
			config.Runtime = runtime
			if _, err := build(config, testPool, ""); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNewRefusesGroupsTheDriverCannotSet(t *testing.T) {
	for name, test := range map[string]struct {
		runtime Runtime
		want    string
	}{
		"exec group":                  {Runtime{Driver: "exec", User: "agent", Group: "agents"}, "runtime group cannot be set on the exec driver"},
		"java group":                  {Runtime{Driver: "java", User: "agent", Group: "agents"}, "runtime group cannot be set on the java driver"},
		"raw_exec group":              {Runtime{Driver: "raw_exec", User: "agent", Group: "agents"}, "runtime group cannot be set on the raw_exec driver"},
		"exec supplementary groups":   {Runtime{Driver: "exec", User: "agent", SupplementaryGroups: []string{"video"}}, "runtime supplementary_groups cannot be set on the exec driver"},
		"java supplementary groups":   {Runtime{Driver: "java", User: "agent", SupplementaryGroups: []string{"video"}}, "runtime supplementary_groups cannot be set on the java driver"},
		"podman supplementary groups": {Runtime{Driver: "podman", User: "1001", SupplementaryGroups: []string{"video"}}, "runtime supplementary_groups cannot be set on the podman driver"},
		"docker group without user":   {Runtime{Driver: "docker", Group: "1002"}, "runtime group needs runtime user on the docker driver"},
		"podman group without user":   {Runtime{Driver: "podman", Group: "1002"}, "runtime group needs runtime user on the podman driver"},
		"docker group beside a GID":   {Runtime{Driver: "docker", User: "1001:1001", Group: "1002"}, "runtime group must be unset when runtime user names a GID"},
		"docker group_add beside supplementary groups": {
			Runtime{Driver: "docker", Config: map[string]any{"group_add": []any{"audio"}}, SupplementaryGroups: []string{"video"}},
			"runtime supplementary_groups must be unset when the docker config sets group_add",
		},
		"root group":                       {Runtime{Driver: "docker", User: "1001", Group: "0"}, "runtime group and supplementary_groups must be group names or GIDs other than root"},
		"root group name":                  {Runtime{Driver: "docker", User: "1001", Group: "root"}, "runtime group and supplementary_groups must be group names or GIDs other than root"},
		"root supplementary group":         {Runtime{Driver: "docker", User: "1001", SupplementaryGroups: []string{"video", "0"}}, "runtime group and supplementary_groups must be group names or GIDs other than root"},
		"group leading zero":               {Runtime{Driver: "docker", User: "1001", Group: "01002"}, "runtime group and supplementary_groups must be group names or GIDs other than root"},
		"group over 32 bits":               {Runtime{Driver: "docker", User: "1001", Group: "4294967296"}, "runtime group and supplementary_groups must be group names or GIDs other than root"},
		"supplementary group with a space": {Runtime{Driver: "docker", User: "1001", SupplementaryGroups: []string{"Bad Name"}}, "runtime group and supplementary_groups must be group names or GIDs other than root"},
	} {
		t.Run(name, func(t *testing.T) {
			config := testConfig("http://127.0.0.1:1")
			config.Runtime = test.runtime
			if _, err := build(config, testPool, ""); err == nil || err.Error() != test.want {
				t.Fatalf("error %v, want %q", err, test.want)
			}
		})
	}
}

func TestBuildJobCarriesTheRuntimeGroups(t *testing.T) {
	for name, test := range map[string]struct {
		runtime Runtime
		user    string
		gid     int
	}{
		"numeric":    {Runtime{Driver: "docker", User: "1001", Group: "1002", SupplementaryGroups: []string{"video", "1003"}}, "1001:1002", 1002},
		"group name": {Runtime{Driver: "docker", User: "1001", Group: "agents"}, "1001:agents", 1001},
		"podman":     {Runtime{Driver: "podman", User: "1001", Group: "1002"}, "1001:1002", 1002},
	} {
		t.Run(name, func(t *testing.T) {
			config := testConfig("http://127.0.0.1:1")
			test.runtime.Config = map[string]any{"image": "agent:1"}
			config.Runtime = test.runtime
			adapter, err := build(config, testPool, "")
			if err != nil {
				t.Fatal(err)
			}
			run := testRun("groups")
			run.Credentials = []wherry.Secret{{Name: "token", File: "token"}}
			job, err := adapter.buildJob(run, adapter.variablePath(run.ID))
			if err != nil {
				t.Fatal(err)
			}
			task := job.TaskGroups[0].Tasks[0]
			if task.User != test.user {
				t.Fatalf("task user %q, want %q", task.User, test.user)
			}
			if groups, set := task.Config["group_add"]; set != (test.runtime.SupplementaryGroups != nil) || set && !reflect.DeepEqual(groups, test.runtime.SupplementaryGroups) {
				t.Fatalf("group_add %v, want %v", groups, test.runtime.SupplementaryGroups)
			}
			if task.Config["image"] != "agent:1" || len(test.runtime.Config) != 1 {
				t.Fatalf("task config %v from runtime config %v", task.Config, test.runtime.Config)
			}
			template := task.Templates[len(task.Templates)-1]
			if *template.DestPath != "${NOMAD_SECRETS_DIR}/credentials/token" || template.Gid == nil || *template.Gid != test.gid {
				t.Fatalf("credential template %s gid %s, want %d", *template.DestPath, show(template.Gid), test.gid)
			}
		})
	}
}

// The alloc fs API refuses the secrets directory, so the task lists its credential, with ls's trailing attribute mark.
// raw_exec resolves the task user by name, so the task runs as the client's user.
func TestCredentialFileOwnerAndModeOnNomad(t *testing.T) {
	_, ctx := devAdapter(t)
	config := testConfig(os.Getenv("WHERRY_NOMAD_DEV_ADDR"))
	config.Runtime.Config = map[string]any{"command": "/bin/sh", "args": []any{"-c", `ls -ln "$WHERRY_CREDENTIALS_DIR/id_key"`}}
	config.Runtime.User = fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	adapter, err := build(config, testPool, "")
	if err != nil {
		t.Fatal(err)
	}
	adapter.runtime.User = ""
	run := testRun("owner")
	run.Credentials = []wherry.Secret{{Name: "deploy-key", File: "id_key", Value: []byte("key")}}
	listed := strings.Fields(output(t, ctx, adapter, dispatch(t, adapter, ctx, run)))
	if want := []string{"-rw-------", strconv.Itoa(os.Getuid()), strconv.Itoa(os.Getgid())}; len(listed) < 4 || !strings.HasPrefix(listed[0], want[0]) || listed[2] != want[1] || listed[3] != want[2] {
		t.Fatalf("credential file listed as %q, want mode and owner %q", listed, want)
	}
}

func TestDispatchRegistersPerRunJob(t *testing.T) {
	adapter, ctx := devAdapter(t)
	run := testRun("register")
	dispatch(t, adapter, ctx, run)
	job, _, err := adapter.client.Jobs().Info(adapter.jobID(run.ID), nil)
	if err != nil {
		t.Fatal(err)
	}
	if *job.ID != adapter.jobID(run.ID) || *job.Name != adapter.jobID(run.ID) || *job.Type != api.JobTypeBatch ||
		*job.NodePool != "default" || len(job.Datacenters) != 1 || job.Datacenters[0] != "dc1" || job.Meta["run_id"] != run.ID {
		t.Fatalf("unexpected job: %#v", job)
	}
	if retriesOn(job.TaskGroups[0]) {
		t.Fatalf("retries were not forced off: reschedule=%#v restart=%#v", job.TaskGroups[0].ReschedulePolicy, job.TaskGroups[0].RestartPolicy)
	}
	if task := job.TaskGroups[0].Tasks[0]; task.Driver != "raw_exec" || task.Config["command"] != "/bin/sh" || *task.Resources.MemoryMB != 64 {
		t.Fatalf("registered task does not carry the runtime: %#v", task)
	}
}

func TestDispatchAdoptsExistingJob(t *testing.T) {
	adapter, ctx := devAdapter(t)
	run := testRun("adopt")
	dispatch(t, adapter, ctx, run)
	before, _, err := adapter.client.Jobs().Info(adapter.jobID(run.ID), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := adapter.Dispatch(ctx, run); err != nil {
		t.Fatal(err)
	}
	after, _, err := adapter.client.Jobs().Info(adapter.jobID(run.ID), nil)
	if err != nil {
		t.Fatal(err)
	}
	if *after.ModifyIndex != *before.ModifyIndex {
		t.Fatalf("adoption re-registered job: modify index %d -> %d", *before.ModifyIndex, *after.ModifyIndex)
	}
}

func output(t *testing.T, ctx context.Context, adapter *Adapter, handle wherry.Handle) string {
	t.Helper()
	joined := ""
	for _, line := range collect(t, ctx, adapter, handle) {
		joined += string(line.Data)
	}
	return joined
}

func TestDispatchDeliversPromptAndCredential(t *testing.T) {
	adapter, ctx := devAdapter(t)
	run := testRun("render")
	handle := dispatch(t, adapter, ctx, run)
	got := output(t, ctx, adapter, handle)
	if !strings.Contains(got, "prompt=hello-prompt token=test-credential env=from-runtime home=/") || !strings.Contains(got, "/agent/local\"}") {
		t.Fatalf("task output does not prove prompt, credential, env and HOME delivered: %q", got)
	}
}

func TestDispatchWithoutCredential(t *testing.T) {
	adapter, ctx := devAdapter(t)
	run := testRun("no-credential")
	run.Credentials = nil
	handle := dispatch(t, adapter, ctx, run)
	if got := output(t, ctx, adapter, handle); !strings.Contains(got, "prompt=hello-prompt token= env=from-runtime") {
		t.Fatalf("task output %q", got)
	}
	variable, _, err := adapter.client.Variables().Peek(adapter.variablePath(run.ID), nil)
	if err != nil || variable == nil {
		t.Fatalf("variable = %#v, %v", variable, err)
	}
	if _, ok := variable.Items["env/"+credentialEnv]; ok {
		t.Fatal("variable carries a credential item for a run without one")
	}
}

func TestLookupFindsExistingRun(t *testing.T) {
	adapter, ctx := devAdapter(t)
	run := testRun("lookup")
	dispatched := dispatch(t, adapter, ctx, run)
	found, err := adapter.Lookup(ctx, run.ID)
	if err != nil || found.State == wherry.JobAbsent {
		t.Fatalf("lookup = %#v, %v", found, err)
	}
	if decoded(t, found.Handle) != decoded(t, dispatched) {
		t.Fatalf("lookup handle %s, dispatch handle %s", found.Handle.Data, dispatched.Data)
	}
	if got := output(t, ctx, adapter, found.Handle); !strings.Contains(got, "prompt=hello-prompt") {
		t.Fatalf("followed lookup handle output %q", got)
	}
}

func TestLookupMissingRun(t *testing.T) {
	adapter, ctx := devAdapter(t)
	job, err := adapter.Lookup(ctx, fmt.Sprintf("run-missing-%d", time.Now().UnixNano()))
	if err != nil || job.State != wherry.JobAbsent || job.Handle.Data != nil {
		t.Fatalf("lookup = %#v, %v", job, err)
	}
}

func TestLookupWaitsForUnplacedJob(t *testing.T) {
	adapter, ctx := devAdapter(t)
	run := testRun("lookup-unplaced")
	adapter.datacenters = []string{"wherry-no-such-datacenter"}
	cleanupRun(t, adapter, run)
	job, err := adapter.buildJob(run, adapter.variablePath(run.ID))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := adapter.client.Jobs().Register(job, nil); err != nil {
		t.Fatal(err)
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if job, err := adapter.Lookup(lookupCtx, run.ID); job.State != wherry.JobAbsent || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lookup of a job without an allocation = %#v, %v; want a wait that times out", job, err)
	}
	if _, _, err := adapter.client.Jobs().Info(adapter.jobID(run.ID), nil); !notFound(err) {
		t.Fatalf("timed-out job still registered: %v", err)
	}
}

func TestReleaseDeletesVariable(t *testing.T) {
	adapter, ctx := devAdapter(t)
	run := testRun("release")
	dispatch(t, adapter, ctx, run)
	path := adapter.variablePath(run.ID)
	if variable, _, err := adapter.client.Variables().Peek(path, nil); err != nil || variable == nil {
		t.Fatalf("variable before release = %#v, %v", variable, err)
	}
	if err := adapter.Release(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if variable, _, err := adapter.client.Variables().Peek(path, nil); err != nil || variable != nil {
		t.Fatalf("variable after release = %#v, %v", variable, err)
	}
	if err := adapter.Release(ctx, run.ID); err != nil {
		t.Fatalf("absent release failed: %v", err)
	}
}

func TestFollowAfterTaskEnded(t *testing.T) {
	adapter, ctx := devAdapter(t)
	run := testRun("follow-ended")
	handle := dispatch(t, adapter, ctx, run)
	for adapter.finished(ctx, decoded(t, handle).Alloc) == nil {
		if ctx.Err() != nil {
			t.Fatal("task never finished")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if lines := collect(t, ctx, adapter, handle); len(lines) == 0 || !strings.Contains(string(lines[len(lines)-1].Data), `"result"`) {
		t.Fatalf("lines = %#v", lines)
	}
}

func TestLineReaderJoinsCRIPartials(t *testing.T) {
	raw := "t stdout P {\"type\":\nt stdout P \"result\",\nt stdout F \"result\":\"ok\"}\nnot framed\n"
	lines := make(chan wherry.Line, 4)
	reader := lineReader{lines: lines, cri: true}
	if err := reader.take(context.Background(), []byte(raw)); err != nil {
		t.Fatal(err)
	}
	first, second := <-lines, <-lines
	if string(first.Data) != `{"type":"result","result":"ok"}` || first.Offset != 0 || second.Offset != first.Next || string(second.Data) != "not framed" {
		t.Fatalf("first=%#v second=%#v", first, second)
	}
}

func TestLineReaderFlushesFinalLine(t *testing.T) {
	lines := make(chan wherry.Line, 1)
	reader := lineReader{lines: lines}
	if err := reader.take(context.Background(), []byte("tail")); err != nil {
		t.Fatal(err)
	}
	if err := reader.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if line := <-lines; string(line.Data) != "tail" || line.Next != 4 {
		t.Fatalf("line = %#v", line)
	}
}

// scriptRun sets the adapter's runtime to run script under sh and returns a new run.
func scriptRun(adapter *Adapter, name, script string) wherry.Run {
	adapter.runtime = Runtime{Driver: "raw_exec", Config: map[string]any{"command": "/bin/sh", "args": []any{"-c", script}}}
	return testRun(name)
}

func waitRunning(t *testing.T, ctx context.Context, adapter *Adapter, handle wherry.Handle) {
	t.Helper()
	for {
		alloc, _, err := adapter.client.Allocations().Info(decoded(t, handle).Alloc, nil)
		if err != nil {
			t.Fatal(err)
		}
		if alloc.ClientStatus == api.AllocClientStatusRunning {
			return
		}
		if ctx.Err() != nil {
			t.Fatalf("allocation never ran: %s", alloc.ClientStatus)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func TestStopDoesNotReplace(t *testing.T) {
	adapter, ctx := devAdapter(t)
	handle := dispatch(t, adapter, ctx, scriptRun(adapter, "stop-replace", `trap '' TERM; sleep 120`))
	waitRunning(t, ctx, adapter, handle)
	if err := adapter.Stop(ctx, handle); err != nil {
		t.Fatal(err)
	}
	var allocs []*api.AllocationListStub
	for watchEnd := time.Now().Add(20 * time.Second); time.Now().Before(watchEnd); time.Sleep(time.Second) {
		var err error
		if allocs, _, err = adapter.client.Jobs().Allocations(decoded(t, handle).Job, true, nil); err != nil {
			t.Fatal(err)
		}
		if len(allocs) != 1 {
			var states []string
			for _, alloc := range allocs {
				states = append(states, alloc.ClientStatus+"/"+alloc.DesiredDescription)
			}
			t.Fatalf("job has %d allocations after stop, want 1: %v", len(allocs), states)
		}
	}
	if !terminal(allocs[0].ClientStatus) {
		t.Fatalf("allocation still %s 20 s after stop", allocs[0].ClientStatus)
	}
}

func TestStopSignalsRun(t *testing.T) {
	adapter, ctx := devAdapter(t)
	script := `trap 'echo "{\"type\":\"result\",\"result\":\"terminated\"}"; exit 143' TERM; echo started; sleep 60 & wait $!`
	handle := dispatch(t, adapter, ctx, scriptRun(adapter, "stop-signal", script))
	waitRunning(t, ctx, adapter, handle)
	if err := adapter.Stop(ctx, handle); err != nil {
		t.Fatal(err)
	}
	lines := collect(t, ctx, adapter, handle)
	if len(lines) == 0 || string(lines[len(lines)-1].Data) != `{"type":"result","result":"terminated"}` {
		t.Fatalf("lines = %#v, want the trap's output last", lines)
	}
	alloc, _, err := adapter.client.Allocations().Info(decoded(t, handle).Alloc, nil)
	if err != nil {
		t.Fatal(err)
	}
	job, _, err := adapter.client.Jobs().Info(decoded(t, handle).Job, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !alloc.ClientTerminalStatus() || alloc.DesiredStatus != api.AllocDesiredStatusRun || *job.Stop {
		t.Fatalf("client=%s desired=%s job stopped=%v; want the signal alone to end the run", alloc.ClientStatus, alloc.DesiredStatus, *job.Stop)
	}
}

func TestStopAlreadyTerminalDoesNotSignal(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests = append(requests, request.Method+" "+request.URL.Path)
		if request.Method == http.MethodGet && request.URL.Path == "/v1/allocation/alloc-terminal" {
			_ = json.NewEncoder(response).Encode(&api.Allocation{ID: "alloc-terminal", ClientStatus: api.AllocClientStatusComplete})
			return
		}
		http.Error(response, "unexpected request", http.StatusInternalServerError)
	}))
	defer server.Close()
	adapter, err := build(Config{Address: server.URL, Runtime: Runtime{Driver: "raw_exec"}}, testPool, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.Stop(context.Background(), wherry.Handle{Data: []byte(`{"v":1,"job":"agent-terminal","alloc":"alloc-terminal","format":""}`)}); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 || requests[0] != "GET /v1/allocation/alloc-terminal" {
		t.Fatalf("requests = %v, want terminal check only", requests)
	}
}

// A receiver adopting a job Nomad has already purged stops it as a no-op.
func TestStopAfterPurgeSucceeds(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests = append(requests, request.Method+" "+request.URL.Path)
		http.Error(response, "alloc not found", http.StatusNotFound)
	}))
	defer server.Close()
	adapter, err := build(Config{Address: server.URL, Runtime: Runtime{Driver: "raw_exec"}}, testPool, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.Stop(context.Background(), wherry.Handle{Data: []byte(`{"v":1,"job":"agent-purged","alloc":"alloc-purged","format":""}`)}); err != nil {
		t.Fatalf("stop of a purged allocation = %v; want it stopped", err)
	}
	if len(requests) != 1 || requests[0] != "GET /v1/allocation/alloc-purged" {
		t.Fatalf("requests = %v, want the allocation read only", requests)
	}
}

func TestDispatchTimeoutDeregisters(t *testing.T) {
	adapter, ctx := devAdapter(t)
	run := testRun("dispatch-timeout")
	adapter.datacenters = []string{"wherry-no-such-datacenter"}
	cleanupRun(t, adapter, run)
	dispatchCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if _, _, err := adapter.Dispatch(dispatchCtx, run); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("dispatch of an unplaceable job = %v; want a wait that times out", err)
	}
	if _, _, err := adapter.client.Jobs().Info(adapter.jobID(run.ID), nil); !notFound(err) {
		t.Fatalf("timed-out job still registered: %v", err)
	}
}

func TestDeregisterTimedOutPurgesJob(t *testing.T) {
	var purge string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodDelete || request.URL.Path != "/v1/job/agent-timed-out" {
			http.Error(response, "unexpected request", http.StatusInternalServerError)
			return
		}
		purge = request.URL.Query().Get("purge")
		_, _ = response.Write([]byte(`{"EvalID":""}`))
	}))
	defer server.Close()
	adapter, err := build(Config{Address: server.URL, Runtime: Runtime{Driver: "raw_exec"}}, testPool, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := adapter.deregisterTimedOut(ctx, "agent-timed-out"); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}
	if purge != "true" {
		t.Fatalf("purge = %q, want true", purge)
	}
}

func follow(t *testing.T, ctx context.Context, adapter *Adapter, handle wherry.Handle, offset int64, count int) []wherry.Line {
	t.Helper()
	followCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	lines := make(chan wherry.Line, 16)
	done := make(chan error, 1)
	go func() {
		_, err := adapter.Follow(followCtx, handle, offset, lines)
		done <- err
	}()
	var got []wherry.Line
	for len(got) < count {
		select {
		case line := <-lines:
			got = append(got, line)
		case err := <-done:
			for len(got) < count && len(lines) > 0 {
				got = append(got, <-lines)
			}
			if len(got) < count {
				t.Fatalf("follow ended after %d of %d lines: %v", len(got), count, err)
			}
		case <-ctx.Done():
			t.Fatal("follow timed out")
		}
	}
	return got
}

func TestFollowOffsetsAndReattach(t *testing.T) {
	adapter, ctx := devAdapter(t)
	handle := dispatch(t, adapter, ctx, scriptRun(adapter, "follow-offsets", `echo '{"type":"system"}'; echo '{"type":"assistant"}'; echo '{"type":"result","result":"ok"}'`))
	all := follow(t, ctx, adapter, handle, 0, 3)
	for i, line := range all {
		want := int64(0)
		if i > 0 {
			want = all[i-1].Next
		}
		if line.Offset != want || line.Next != line.Offset+int64(len(line.Data))+1 {
			t.Fatalf("line %d = %#v, want offset %d", i, line, want)
		}
	}
	resumed := follow(t, ctx, adapter, handle, all[1].Offset, 2)
	if !reflect.DeepEqual(resumed, all[1:]) {
		t.Fatalf("follow from offset %d = %#v, want %#v", all[1].Offset, resumed, all[1:])
	}
	first := follow(t, ctx, adapter, handle, 0, 1)
	rest := follow(t, ctx, adapter, handle, first[0].Next, 2)
	if !reflect.DeepEqual(append(first, rest...), all) {
		t.Fatalf("reattach from %d = %#v, want %#v", first[0].Next, rest, all[1:])
	}
}

func TestFollowAcrossLogRotation(t *testing.T) {
	adapter, ctx := devAdapter(t)
	const count = 150000 // 80-byte lines: 12 MB, past the 10 MB log file size
	script := `awk 'BEGIN { for (i = 0; i < 150000; i++) printf "line-%06d-%s\n", i, "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" }'; echo '{"type":"result","result":"ok"}'`
	handle := dispatch(t, adapter, ctx, scriptRun(adapter, "rotation", script))
	all := collect(t, ctx, adapter, handle)
	if len(all) != count+1 || all[count].Next <= 10<<20 {
		t.Fatalf("got %d lines ending at %d, want %d past 10 MB", len(all), all[len(all)-1].Next, count+1)
	}
	for i, line := range all[:count] {
		if string(line.Data[:12]) != fmt.Sprintf("line-%06d-", i) || line.Offset != int64(i*80) || line.Next != line.Offset+80 {
			t.Fatalf("line %d = %.20q offset %d next %d", i, line.Data, line.Offset, line.Next)
		}
	}
	alloc, _, err := adapter.client.Allocations().Info(decoded(t, handle).Alloc, nil)
	if err != nil {
		t.Fatal(err)
	}
	files, _, err := adapter.client.AllocFS().List(alloc, "alloc/logs", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(files, func(file *api.AllocFileInfo) bool { return file.Name == taskName+".stdout.1" }) {
		t.Fatalf("log did not rotate: %d files", len(files))
	}
	mid := all[count-10]
	resumed := follow(t, ctx, adapter, handle, mid.Offset, 11)
	if !reflect.DeepEqual(resumed, all[count-10:]) {
		t.Fatalf("follow from %d after rotation = %.80q", mid.Offset, resumed[0].Data)
	}
}

// The fixture is real Podman-driver stdout.
func TestLineReaderDecodesPodmanCRI(t *testing.T) {
	raw, err := os.ReadFile("testdata/podman-cri-stdout.log")
	if err != nil {
		t.Fatal(err)
	}
	lines := make(chan wherry.Line, 16)
	reader := lineReader{lines: lines, cri: true}
	if err := reader.take(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	got := closeAndRead(lines)
	if len(got) != 4 {
		t.Fatalf("got %d lines, want 4", len(got))
	}
	for i, line := range got {
		if !strings.HasPrefix(string(line.Data), "{") {
			t.Fatalf("line %d still framed: %.50q", i, line.Data)
		}
	}
	if !strings.Contains(string(got[3].Data), `"type":"result"`) || got[3].Next != int64(len(raw)) {
		t.Fatalf("last line %.80q next=%d, want result ending at %d", got[3].Data, got[3].Next, len(raw))
	}
}

func TestCRIPartialBounded(t *testing.T) {
	part := strings.Repeat("x", maxPending/2)
	firstRaw := "t stdout P " + part + "\n"
	secondRaw := "t stdout P " + part + "\n"
	finalRaw := "t stdout F tail\n"
	lines := make(chan wherry.Line, 3)
	reader := lineReader{lines: lines, cri: true}
	if err := reader.take(context.Background(), []byte(firstRaw+secondRaw+finalRaw)); err != nil {
		t.Fatal(err)
	}
	got := closeAndRead(lines)
	if len(got) != 2 {
		t.Fatalf("got %d lines, want 2", len(got))
	}
	first, second := got[0], got[1]
	if len(first.Data) != maxPending || first.Offset != 0 || first.Next != int64(len(firstRaw)+len(secondRaw)) {
		t.Fatalf("bounded partial len=%d offset=%d next=%d", len(first.Data), first.Offset, first.Next)
	}
	if string(second.Data) != "tail" || second.Offset != first.Next || second.Next != int64(len(firstRaw)+len(secondRaw)+len(finalRaw)) {
		t.Fatalf("final part = %#v", second)
	}
}

func TestCRIMalformedAfterPartial(t *testing.T) {
	partialRaw := "t stdout P partial\n"
	malformedRaw := "not framed\n"
	lines := make(chan wherry.Line, 2)
	reader := lineReader{lines: lines, cri: true}
	if err := reader.take(context.Background(), []byte(partialRaw+malformedRaw)); err != nil {
		t.Fatal(err)
	}
	got := closeAndRead(lines)
	if len(got) != 2 {
		t.Fatalf("got %d lines, want 2", len(got))
	}
	partial, malformed := got[0], got[1]
	if string(partial.Data) != "partial" || partial.Offset != 0 || partial.Next != int64(len(partialRaw)) {
		t.Fatalf("partial = %#v", partial)
	}
	if string(malformed.Data) != "not framed" || malformed.Offset != partial.Next || malformed.Next != int64(len(partialRaw)+len(malformedRaw)) {
		t.Fatalf("malformed = %#v", malformed)
	}
}

func TestCRIFlushEmitsPendingPartial(t *testing.T) {
	partialRaw := "t stdout P partial\n"
	pendingRaw := "raw-tail"
	lines := make(chan wherry.Line, 2)
	reader := lineReader{lines: lines, cri: true}
	if err := reader.take(context.Background(), []byte(partialRaw+pendingRaw)); err != nil {
		t.Fatal(err)
	}
	if err := reader.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := closeAndRead(lines)
	if len(got) != 2 {
		t.Fatalf("got %d lines, want 2", len(got))
	}
	partial, pending := got[0], got[1]
	if string(partial.Data) != "partial" || partial.Offset != 0 || partial.Next != int64(len(partialRaw)) {
		t.Fatalf("partial = %#v", partial)
	}
	if string(pending.Data) != pendingRaw || pending.Offset != partial.Next || pending.Next != int64(len(partialRaw)+len(pendingRaw)) {
		t.Fatalf("pending = %#v", pending)
	}
}

func closeAndRead(lines chan wherry.Line) []wherry.Line {
	close(lines)
	var got []wherry.Line
	for line := range lines {
		got = append(got, line)
	}
	return got
}

func TestSecretsNeverInRequestURLs(t *testing.T) {
	const token = "nomad-token-secret"
	run := testRun("urls")
	run.ID = "run-urls"
	run.Inputs = []wherry.File{{Name: "data.csv", Data: []byte("input-content-never-in-urls")}}
	run.Credentials = twoOfEachTarget
	run.Fetches = []wherry.Fetch{{Name: "dataset.bin", URL: "https://store.example.test/b/dataset.bin?X-Amz-Signature=secretsig-never-in-urls", Size: 8, SHA256: strings.Repeat("a", 64)}}
	run.Model = "model-never-in-urls"
	const outputContent = "output-content-never-in-urls"
	var mu sync.Mutex
	var urls []string
	var variable api.Variable
	var job []byte
	registered := false
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		urls = append(urls, request.Method+" "+request.URL.Path+"?"+request.URL.RawQuery)
		body, _ := io.ReadAll(request.Body)
		if request.Header.Get("X-Nomad-Token") != token {
			http.Error(response, "missing token", http.StatusForbidden)
			return
		}
		switch request.Method + " " + request.URL.Path {
		case "PUT /v1/var/nomad/jobs/agent-44575cf5b285-run-urls":
			_ = json.Unmarshal(body, &variable)
			_, _ = response.Write([]byte(`{}`))
		case "DELETE /v1/var/nomad/jobs/agent-44575cf5b285-run-urls",
			"PUT /v1/var/nomad/jobs/agent-44575cf5b285-run-urls/agent", "DELETE /v1/var/nomad/jobs/agent-44575cf5b285-run-urls/agent":
			_, _ = response.Write([]byte(`{}`))
		case "GET /v1/client/fs/ls/alloc-urls":
			fmt.Fprintf(response, `[{"Name":"report.md","IsDir":false,"Size":%d,"FileMode":"-rw-r--r--"}]`, len(outputContent))
		case "GET /v1/client/fs/readat/alloc-urls":
			_, _ = response.Write([]byte(outputContent))
		case "DELETE /v1/job/agent-44575cf5b285-run-urls":
			_, _ = response.Write([]byte(`{"EvalID":""}`))
		case "GET /v1/jobs":
			_, _ = response.Write([]byte(`[{"ID":"agent-44575cf5b285-run-urls","Status":"dead","SubmitTime":0,"Meta":{"profile":"container","run_id":"r"}}]`))
		case "GET /v1/job/agent-44575cf5b285-run-urls":
			if !registered {
				http.Error(response, "job not found", http.StatusNotFound)
				return
			}
			_, _ = response.Write([]byte(`{"ID":"agent-44575cf5b285-run-urls","Status":"dead","Stop":false,"Meta":{"profile":"container","run_id":"r"},"TaskGroups":[{"Tasks":[{"Name":"agent","Driver":"raw_exec"}]}]}`))
		case "GET /v1/vars":
			_, _ = response.Write([]byte(`[{"Path":"nomad/jobs/agent-44575cf5b285-run-urls"},{"Path":"nomad/jobs/agent-44575cf5b285-run-urls/agent"}]`))
		case "PUT /v1/jobs":
			registered, job = true, body
			_, _ = response.Write([]byte(`{}`))
		case "GET /v1/job/agent-44575cf5b285-run-urls/allocations":
			_, _ = response.Write([]byte(`[{"ID":"alloc-urls","ClientStatus":"complete"}]`))
		case "GET /v1/allocation/alloc-urls":
			_, _ = response.Write([]byte(`{"ID":"alloc-urls","ClientStatus":"complete","TaskStates":{"agent":{"State":"dead","StartedAt":"2026-01-01T00:00:00Z"}}}`))
		case "GET /v1/nodes":
			_, _ = response.Write([]byte(`[]`))
		case "GET /v1/client/fs/logs/alloc-urls":
			_, _ = response.Write([]byte(`{}`))
		case "GET /v1/node/":
			http.Error(response, "node not found", http.StatusNotFound)
		default:
			http.Error(response, "unexpected request", http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	adapter, err := build(testConfig(server.URL), testPool, token)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	adapter.logger = hclog.New(&hclog.LoggerOptions{Output: &logs, Level: hclog.Trace, JSONFormat: true})
	ctx := context.Background()
	if _, err := adapter.Ready(ctx); err != nil {
		t.Fatalf("ready: %v", err)
	}
	_, handle, err := adapter.Dispatch(ctx, run)
	if err != nil {
		t.Fatalf("dispatch = %#v, %v", handle, err)
	}
	if found, err := adapter.Lookup(ctx, run.ID); err != nil || found.State != wherry.JobNeverStarted {
		t.Fatalf("lookup = %#v, %v", found, err)
	}
	if _, err := adapter.Follow(ctx, handle, 0, make(chan wherry.Line, 1)); err != nil {
		t.Fatalf("follow: %v", err)
	}
	if err := adapter.Stop(ctx, handle); err != nil {
		t.Fatal(err)
	}
	outputs := make(chan wherry.Output, 1)
	if err := adapter.Collect(ctx, handle, wherry.Limits{Files: 256, FileBytes: 1 << 20, TotalBytes: 1 << 20}, outputs); err != nil || string((<-outputs).Data) != outputContent {
		t.Fatalf("collect: %v", err)
	}
	if err := adapter.Release(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Purge(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(urls) != 34 {
		t.Fatalf("requests = %d %v, want ready, dispatch, lookup, follow, stop, collect, release, purge and sweep", len(urls), urls)
	}
	secrets := []string{token, string(run.Prompt), string(run.Inputs[0].Data), run.Fetches[0].URL, "secretsig-never-in-urls", outputContent, run.Model}
	for _, secret := range run.Credentials {
		secrets = append(secrets, string(secret.Value))
		if got := variable.Items[credentialItem(secret)]; got != base64.StdEncoding.EncodeToString(secret.Value) {
			t.Fatalf("variable item %s = %q; want the base64 of %s", credentialItem(secret), got, secret.Name)
		}
	}
	for _, url := range urls {
		for _, secret := range secrets {
			if strings.Contains(url, secret) {
				t.Fatalf("request URL %q contains %q", url, secret)
			}
		}
	}
	for _, secret := range run.Credentials {
		for _, form := range []string{string(secret.Value), base64.StdEncoding.EncodeToString(secret.Value)} {
			if strings.Contains(string(job), form) || strings.Contains(logs.String(), form) {
				t.Fatalf("the registered job or the logs hold the value of %s", secret.Name)
			}
		}
	}
}

// twoOfEachTarget are two credentials at environment variable targets and two at file targets.
var twoOfEachTarget = []wherry.Secret{
	{Name: "model", Env: "MODEL_API_KEY", Value: []byte("env-credential-one")},
	{Name: "registry", Env: "REGISTRY_TOKEN", Value: []byte("env-credential-two")},
	{Name: "deploy-key", File: "id_ed25519", Value: []byte("file-credential-one")},
	{Name: "kubeconfig", File: "kubeconfig", Value: []byte("file-credential-two")},
}

// TestBuildJobDeliversCredentialsAtTheirTargets builds the job for adaptertest.Credentials and for two credentials at
// each kind of target: each env target is a line of the env template, each file target a template of its own.
func TestBuildJobDeliversCredentialsAtTheirTargets(t *testing.T) {
	adapter, err := build(testConfig("http://127.0.0.1:1"), testPool, "")
	if err != nil {
		t.Fatal(err)
	}
	for name, credentials := range map[string][]wherry.Secret{"conformance": adaptertest.Credentials, "two of each": twoOfEachTarget} {
		t.Run(name, func(t *testing.T) {
			run := testRun("targets")
			run.Credentials = credentials
			path := adapter.variablePath(run.ID)
			job, err := adapter.buildJob(run, path)
			if err != nil {
				t.Fatal(err)
			}
			var want []string
			templates := map[string]string{}
			for _, template := range job.TaskGroups[0].Tasks[0].Templates {
				templates[*template.DestPath] = *template.EmbeddedTmpl
			}
			for _, secret := range credentials {
				if secret.Env != "" {
					want = append(want, secret.Env+`={{ (index . "env/`+secret.Env+`").Value | base64Decode }}`)
					continue
				}
				if got := templates["${NOMAD_SECRETS_DIR}/credentials/"+secret.File]; got != `{{ with nomadVar "`+path+`" }}{{ (index . "credentials/`+secret.File+`").Value | base64Decode }}{{ end }}` {
					t.Fatalf("file template for %s = %q", secret.Name, got)
				}
			}
			if got := templates["${NOMAD_SECRETS_DIR}/wherry.env"]; got != `{{ with nomadVar "`+path+`" }}`+strings.Join(want, "\n")+"\n{{ end }}" {
				t.Fatalf("env template = %q; want a line for each of %v", got, want)
			}
			if len(templates) != 2+len(credentials)-len(want) {
				t.Fatalf("templates %v; want the prompt, the env template and one per file target", templates)
			}
		})
	}
}

// A response body can echo variable items, so the error text goes to the receiver, which redacts it before it logs it.
func TestReturnedErrorCarriesOpAndClass(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		http.Error(response, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	adapter, err := build(Config{Address: server.URL, Runtime: Runtime{Driver: "raw_exec"}}, testPool, "")
	if err != nil {
		t.Fatal(err)
	}
	err = adapter.Sweep(context.Background())
	var failure *wherry.Error
	var response api.UnexpectedResponseError
	if !errors.As(err, &failure) || failure.Op != "list jobs "+adapter.prefix || failure.Class != "unavailable" || !errors.As(err, &response) {
		t.Fatalf("sweep error %#v; want a wherry.Error naming the call and its class over the response", err)
	}
	if got := samples(t, adapter)["wherry_adapter_platform_errors_total/unavailable"].Value; got != 1 {
		t.Fatalf("unavailable errors counted %v; want 1", got)
	}
}

func TestRegisterFailureCarriesOpAndClass(t *testing.T) {
	const signature = "X-Amz-Signature=secretsig-register"
	infos := 0
	adapter, _ := nomadServer(t, map[string]http.HandlerFunc{
		"PUT /v1/var/nomad/jobs/agent-44575cf5b285-run-register": serve([]byte(`{}`)),
		"GET /v1/job/agent-44575cf5b285-run-register": func(response http.ResponseWriter, request *http.Request) {
			if infos++; infos == 1 {
				jobNotFound(response, request)
				return
			}
			_, _ = response.Write(captured(t, "clean.job.json"))
		},
		"PUT /v1/jobs": func(response http.ResponseWriter, request *http.Request) {
			body, _ := io.ReadAll(request.Body)
			http.Error(response, string(body), http.StatusBadRequest)
		},
	})
	run := testRun("register")
	run.ID = "run-register"
	run.Fetches = []wherry.Fetch{{Name: "dataset.bin", URL: "https://store.example.test/b/dataset.bin?" + signature, Size: 8, SHA256: strings.Repeat("a", 64)}}
	_, _, err := adapter.Dispatch(context.Background(), run)
	var failure *wherry.Error
	if !errors.As(err, &failure) || failure.Op != "register job agent-44575cf5b285-run-register" || failure.Class != "other" {
		t.Fatalf("dispatch error %#v, want the register failure as a wherry.Error", err)
	}
}

func TestBuildJobOmitsPlaintext(t *testing.T) {
	adapter, err := build(testConfig("http://127.0.0.1:1"), testPool, "")
	if err != nil {
		t.Fatal(err)
	}
	run := testRun("plaintext")
	run.Inputs = []wherry.File{{Name: "data.csv", Data: []byte("input-content-never-in-the-job")}}
	run.Credentials = twoOfEachTarget
	job, err := adapter.buildJob(run, adapter.variablePath(run.ID))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	secrets := []string{string(run.Prompt), string(run.Inputs[0].Data), base64.StdEncoding.EncodeToString(run.Inputs[0].Data)}
	for _, credential := range run.Credentials {
		secrets = append(secrets, string(credential.Value), base64.StdEncoding.EncodeToString(credential.Value))
	}
	for _, secret := range secrets {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("job JSON contains %q", secret)
		}
	}
}

func sleepRun(adapter *Adapter, name string) wherry.Run {
	adapter.runtime = Runtime{Driver: "raw_exec", Config: map[string]any{"command": "/bin/sleep", "args": []any{"300"}}}
	run := testRun(name)
	return run
}

func putVariable(t *testing.T, adapter *Adapter, ctx context.Context, path string) {
	t.Helper()
	t.Cleanup(func() { _, _ = adapter.client.Variables().Delete(path, nil) })
	if _, _, err := adapter.client.Variables().Update(&api.Variable{Path: path, Items: map[string]string{"credential": "test-credential"}}, (&api.WriteOptions{}).WithContext(ctx)); err != nil {
		t.Fatalf("write %s failed", path)
	}
}

func sweepLeaves(t *testing.T, adapter *Adapter, ctx context.Context, path string, want bool) {
	t.Helper()
	if err := adapter.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	variable, _, err := adapter.client.Variables().Peek(path, nil)
	if err != nil {
		t.Fatalf("peek %s failed", path)
	}
	if (variable != nil) != want {
		t.Fatalf("variable %s present after sweep = %v, want %v", path, variable != nil, want)
	}
}

func shortGrace(t *testing.T) {
	previous := sweepGrace
	sweepGrace = 0
	t.Cleanup(func() { sweepGrace = previous })
}

func TestSweepDeletesVariableOfDeadJob(t *testing.T) {
	adapter, ctx := devAdapter(t)
	run := testRun("sweep-dead")
	dispatch(t, adapter, ctx, run)
	for {
		job, _, err := adapter.client.Jobs().Info(adapter.jobID(run.ID), (&api.QueryOptions{}).WithContext(ctx))
		if err != nil {
			t.Fatal(err)
		}
		if *job.Status == "dead" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	sweepLeaves(t, adapter, ctx, adapter.variablePath(run.ID), false)
}

func TestSweepDeletesVariableOfStoppedJob(t *testing.T) {
	adapter, ctx := devAdapter(t)
	// A running task that ignores SIGTERM keeps the job stopped but not dead for the kill timeout.
	run := scriptRun(adapter, "sweep-stopped", `trap '' TERM; sleep 300`)
	handle := dispatch(t, adapter, ctx, run)
	waitRunning(t, ctx, adapter, handle)
	if _, _, err := adapter.client.Jobs().Deregister(adapter.jobID(run.ID), false, (&api.WriteOptions{}).WithContext(ctx)); err != nil {
		t.Fatal(err)
	}
	// Nomad can mark the stopped job dead while its allocation still runs, so the job's status says nothing here.
	job, _, err := adapter.client.Jobs().Info(adapter.jobID(run.ID), (&api.QueryOptions{}).WithContext(ctx))
	if err != nil || !*job.Stop {
		t.Fatalf("job after stop = %#v, %v; want stopped", job, err)
	}
	alloc, _, err := adapter.client.Allocations().Info(decoded(t, handle).Alloc, (&api.QueryOptions{}).WithContext(ctx))
	if err != nil || alloc.ClientStatus != api.AllocClientStatusRunning {
		t.Fatalf("allocation after stop = %v, %v; want still running", alloc, err)
	}
	sweepLeaves(t, adapter, ctx, adapter.variablePath(run.ID), false)
}

func TestSweepKeepsVariableOfRunningJob(t *testing.T) {
	adapter, ctx := devAdapter(t)
	shortGrace(t)
	run := sleepRun(adapter, "sweep-running")
	dispatch(t, adapter, ctx, run)
	sweepLeaves(t, adapter, ctx, adapter.variablePath(run.ID), true)
}

func TestSweepKeepsRecentVariableWithoutJob(t *testing.T) {
	adapter, ctx := devAdapter(t)
	run := testRun("sweep-recent")
	path := adapter.variablePath(run.ID)
	putVariable(t, adapter, ctx, path)
	sweepLeaves(t, adapter, ctx, path, true)
}

func TestSweepDeletesStaleVariableWithoutJob(t *testing.T) {
	adapter, ctx := devAdapter(t)
	shortGrace(t)
	run := testRun("sweep-stale")
	path := adapter.variablePath(run.ID)
	putVariable(t, adapter, ctx, path)
	sweepLeaves(t, adapter, ctx, path, false)
}

func TestSweepIgnoresNonRunVariables(t *testing.T) {
	adapter, ctx := devAdapter(t)
	shortGrace(t)
	for _, path := range []string{"nomad/jobs/agent-foo/prompt", "nomad/jobs/agent-foo"} {
		putVariable(t, adapter, ctx, path)
		sweepLeaves(t, adapter, ctx, path, true)
	}
}

func TestJobHoldsOnlyTheAgentTask(t *testing.T) {
	adapter, err := build(testConfig("http://127.0.0.1:1"), testPool, "")
	if err != nil {
		t.Fatal(err)
	}
	run := testRun("one-task")
	job, err := adapter.buildJob(run, adapter.variablePath(run.ID))
	if err != nil {
		t.Fatal(err)
	}
	if groups := job.TaskGroups; len(groups) != 1 || len(groups[0].Tasks) != 1 || groups[0].Tasks[0].Name != taskName || groups[0].Tasks[0].Lifecycle != nil {
		t.Fatalf("job task groups %#v, want the agent task alone", groups)
	}
}

// The agent clones a git input itself, so the plugin's code names no git.
func TestPluginCodeNamesNoGit(t *testing.T) {
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	word := regexp.MustCompile(`(?i)\bgit\b`)
	for _, source := range sources {
		if strings.HasSuffix(source, "_test.go") {
			continue
		}
		code, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		if match := word.Find(code); match != nil {
			t.Fatalf("%s names %q", source, match)
		}
	}
}

func TestBuildJobLeavesUnsetPlacementToNomad(t *testing.T) {
	config := testConfig("http://127.0.0.1:1")
	config.NodePool, config.Datacenters = "", nil
	adapter, err := build(config, testPool, "")
	if err != nil {
		t.Fatal(err)
	}
	run := testRun("defaults")
	job, err := adapter.buildJob(run, adapter.variablePath(run.ID))
	if err != nil {
		t.Fatal(err)
	}
	if job.NodePool != nil || !reflect.DeepEqual(job.Datacenters, []string{"*"}) {
		t.Fatalf("node pool %v, datacenters %v; want Nomad's defaults", job.NodePool, job.Datacenters)
	}
}

func TestReadyFiltersNodesByPoolDatacenterAndDriver(t *testing.T) {
	node := func(pool, datacenter, driver string, healthy bool) map[string]any {
		return map[string]any{"ID": pool + datacenter + driver, "Status": "ready", "SchedulingEligibility": "eligible", "NodePool": pool, "Datacenter": datacenter,
			"Drivers": map[string]any{driver: map[string]any{"Detected": true, "Healthy": healthy}}}
	}
	cases := []struct {
		name  string
		pool  string
		dcs   []string
		nodes []map[string]any
		want  bool
	}{
		{"default pool when unset", "", nil, []map[string]any{node("gpu", "dc1", "raw_exec", true)}, false},
		{"unset pool finds default", "", nil, []map[string]any{node("default", "dc1", "raw_exec", true)}, true},
		{"configured pool", "gpu", nil, []map[string]any{node("gpu", "dc1", "raw_exec", true)}, true},
		{"datacenter outside the list", "", []string{"dc2"}, []map[string]any{node("default", "dc1", "raw_exec", true)}, false},
		{"unhealthy driver", "", nil, []map[string]any{node("default", "dc1", "raw_exec", false)}, false},
		{"other driver", "", nil, []map[string]any{node("default", "dc1", "podman", true)}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(c.nodes)
			}))
			defer server.Close()
			adapter, err := build(Config{Address: server.URL, Runtime: Runtime{Driver: "raw_exec"}, NodePool: c.pool, Datacenters: c.dcs}, testPool, "")
			if err != nil {
				t.Fatal(err)
			}
			if ready, err := adapter.Ready(context.Background()); err != nil || ready != c.want {
				t.Fatalf("ready = %v, %v; want %v", ready, err, c.want)
			}
		})
	}
}

// The testdata *.job.json, *.allocations.json and *.allocation.json files are GET /v1/job/<id>,
// GET /v1/job/<id>/allocations and GET /v1/allocation/<id> responses captured from a Nomad 2.0.7 dev
// agent, node name replaced, for batch jobs shaped as buildJob builds them: pull-failure ran the docker
// driver with an image whose registry does not resolve, clean ran raw_exec to completion, signal ran
// raw_exec and killed itself with SIGKILL.
func testdata(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// captured is a testdata capture with its job IDs in the test pool, as the adapter names them.
func captured(t *testing.T, name string) []byte {
	t.Helper()
	return bytes.ReplaceAll(testdata(t, name), []byte("agent-capture-"), []byte(poolPrefix+"capture-"))
}

func serve(body []byte) http.HandlerFunc {
	return func(response http.ResponseWriter, _ *http.Request) { _, _ = response.Write(body) }
}

// jobNotFound is Nomad's answer to GET /v1/job/<id> for a job that does not exist.
func jobNotFound(response http.ResponseWriter, _ *http.Request) {
	http.Error(response, "job not found", http.StatusNotFound)
}

// nomadServer answers requests by "METHOD path" and records each one; an unrouted request is a 500, except a
// node lookup, which is a 404 so the client reads logs through the server.
func nomadServer(t *testing.T, routes map[string]http.HandlerFunc) (*Adapter, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		key := request.Method + " " + request.URL.Path
		mu.Lock()
		requests = append(requests, key)
		mu.Unlock()
		switch route, ok := routes[key]; {
		case ok:
			route(response, request)
		case strings.HasPrefix(request.URL.Path, "/v1/node/"):
			http.Error(response, "node not found", http.StatusNotFound)
		default:
			http.Error(response, "unexpected request", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(server.Close)
	adapter, err := build(testConfig(server.URL), testPool, "")
	if err != nil {
		t.Fatal(err)
	}
	return adapter, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(requests)
	}
}

func deletes(requests []string) []string {
	var deleted []string
	for _, request := range requests {
		if strings.HasPrefix(request, "DELETE ") {
			deleted = append(deleted, request)
		}
	}
	return deleted
}

func TestDispatchFailureOutcomes(t *testing.T) {
	const path = "/v1/var/nomad/jobs/agent-44575cf5b285-run-outcome"
	variables := []string{"DELETE " + path, "DELETE " + path + "/agent"}
	for _, test := range []struct {
		name     string
		routes   map[string]http.HandlerFunc
		outcome  wherry.Outcome
		released bool
		// failed is whether Dispatch reports an error beside the outcome.
		failed bool
	}{
		{"wait fails", map[string]http.HandlerFunc{
			"GET /v1/job/agent-44575cf5b285-run-outcome": jobNotFound,
			"PUT /v1/jobs": serve([]byte(`{"EvalID":"eval-1","EvalCreateIndex":10,"JobModifyIndex":10}`)),
			"GET /v1/job/agent-44575cf5b285-run-outcome/allocations": func(response http.ResponseWriter, _ *http.Request) {
				http.Error(response, "rpc error", http.StatusInternalServerError)
			},
		}, wherry.OutcomeUnknown, false, true},
		{"register fails and no job exists", map[string]http.HandlerFunc{
			"GET /v1/job/agent-44575cf5b285-run-outcome": jobNotFound,
			"PUT /v1/jobs": func(response http.ResponseWriter, _ *http.Request) {
				http.Error(response, "rpc error", http.StatusInternalServerError)
			},
		}, wherry.OutcomeNothingCreated, true, false},
		{"register fails and the job exists", map[string]http.HandlerFunc{
			"GET /v1/job/agent-44575cf5b285-run-outcome": func() http.HandlerFunc {
				infos := 0
				return func(response http.ResponseWriter, request *http.Request) {
					if infos++; infos == 1 {
						jobNotFound(response, request)
						return
					}
					_, _ = response.Write(captured(t, "clean.job.json"))
				}
			}(),
			"PUT /v1/jobs": func(response http.ResponseWriter, _ *http.Request) {
				http.Error(response, "rpc error", http.StatusInternalServerError)
			},
		}, wherry.OutcomeUnknown, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			routes := map[string]http.HandlerFunc{"PUT " + path: serve([]byte(`{}`))}
			for _, deleted := range variables {
				routes[deleted] = func(response http.ResponseWriter, _ *http.Request) { response.WriteHeader(http.StatusNoContent) }
			}
			maps.Copy(routes, test.routes)
			adapter, requests := nomadServer(t, routes)
			run := testRun("outcome")
			run.ID = "run-outcome"
			outcome, handle, err := adapter.Dispatch(context.Background(), run)
			if (err != nil) != test.failed || outcome != test.outcome || handle.Data != nil {
				t.Fatalf("dispatch = %v, %#v, %v; want %v, error %t", outcome, handle, err, test.outcome, test.failed)
			}
			want := []string(nil)
			if test.released {
				want = variables
			}
			if got := deletes(requests()); !slices.Equal(got, want) {
				t.Fatalf("deletes %v, want %v", got, want)
			}
		})
	}
}

func TestPurgeOfAbsentJobSucceeds(t *testing.T) {
	const path = "/v1/var/nomad/jobs/agent-44575cf5b285-run-absent"
	variables := []string{"DELETE " + path, "DELETE " + path + "/agent"}
	for name, deregister := range map[string]http.HandlerFunc{
		// Nomad 2.0.7 answers a purge of a job that does not exist with an empty evaluation.
		"captured": serve([]byte(`{"EvalID":"","EvalCreateIndex":0,"JobModifyIndex":0,"VolumeEvalID":"","VolumeEvalIndex":0,"Index":0,"LastContact":0,"KnownLeader":false,"NextToken":""}`)),
		// Not captured: the not-found answer Nomad gives GET /v1/job/<id>, on DELETE.
		"not found": jobNotFound,
	} {
		t.Run(name, func(t *testing.T) {
			routes := map[string]http.HandlerFunc{"DELETE /v1/job/agent-44575cf5b285-run-absent": deregister}
			for _, deleted := range variables {
				routes[deleted] = func(response http.ResponseWriter, _ *http.Request) { response.WriteHeader(http.StatusNoContent) }
			}
			adapter, requests := nomadServer(t, routes)
			if err := adapter.Purge(context.Background(), "run-absent"); err != nil {
				t.Fatalf("purge of an absent job: %v", err)
			}
			if got, want := deletes(requests()), append([]string{"DELETE /v1/job/agent-44575cf5b285-run-absent"}, variables...); !slices.Equal(got, want) {
				t.Fatalf("deletes %v, want %v", got, want)
			}
		})
	}
}

func TestLookupReadsDispatchMetadata(t *testing.T) {
	var mu sync.Mutex
	var registered *api.Job
	adapter, _ := nomadServer(t, map[string]http.HandlerFunc{
		"PUT /v1/var/nomad/jobs/agent-44575cf5b285-run-meta": serve([]byte(`{}`)),
		"PUT /v1/jobs": func(response http.ResponseWriter, request *http.Request) {
			var body api.JobRegisterRequest
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				http.Error(response, err.Error(), http.StatusBadRequest)
				return
			}
			mu.Lock()
			registered = body.Job
			mu.Unlock()
			_, _ = response.Write([]byte(`{"EvalID":"eval-1","EvalCreateIndex":10,"JobModifyIndex":10}`))
		},
		"GET /v1/job/agent-44575cf5b285-run-meta": func(response http.ResponseWriter, request *http.Request) {
			mu.Lock()
			job := registered
			mu.Unlock()
			if job == nil {
				jobNotFound(response, request)
				return
			}
			_ = json.NewEncoder(response).Encode(job)
		},
		"GET /v1/job/agent-44575cf5b285-run-meta/allocations": serve(captured(t, "clean.allocations.json")),
	})
	run := testRun("meta")
	run.ID, run.Attempt, run.Profile, run.Agent = "run-meta", 3, "container", "coder"
	outcome, dispatched, err := adapter.Dispatch(context.Background(), run)
	if err != nil || outcome != wherry.OutcomeCreated || dispatched.Name != "agent-44575cf5b285-run-meta" {
		t.Fatalf("dispatch = %v, %q, %v; want created with the job ID as its name", outcome, dispatched.Name, err)
	}
	job, err := adapter.Lookup(context.Background(), run.ID)
	if err != nil || job.State != wherry.JobEnded || job.Attempt != 3 || job.Profile != "container" || job.Agent != "coder" || job.Handle.Name != "agent-44575cf5b285-run-meta" {
		t.Fatalf("lookup = %#v, %v; want the ended job named agent-44575cf5b285-run-meta with attempt 3, profile container, agent coder", job, err)
	}
}

func TestLookupMapsCapturedAllocations(t *testing.T) {
	for _, test := range []struct {
		capture string
		want    wherry.JobState
	}{
		{"pull-failure", wherry.JobNeverStarted},
		{"clean", wherry.JobEnded},
	} {
		t.Run(test.capture, func(t *testing.T) {
			runID := "capture-" + strings.TrimSuffix(test.capture, "-failure")
			id := poolPrefix + runID + ""
			adapter, _ := nomadServer(t, map[string]http.HandlerFunc{
				"GET /v1/job/" + id:                  serve(captured(t, test.capture+".job.json")),
				"GET /v1/job/" + id + "/allocations": serve(captured(t, test.capture+".allocations.json")),
			})
			job, err := adapter.Lookup(context.Background(), runID)
			if err != nil || job.State != test.want || job.Attempt != 1 || job.Profile != "container" || job.Agent != "coder" {
				t.Fatalf("lookup = %#v, %v; want state %v from the capture's metadata", job, err, test.want)
			}
			if h := decoded(t, job.Handle); h.Job != id || h.Alloc == "" {
				t.Fatalf("handle %#v", h)
			}
		})
	}
}

func TestFollowReturnsTerminal(t *testing.T) {
	for _, test := range []struct {
		capture string
		lines   []string
		want    wherry.Terminal
	}{
		{"signal", []string{"working"}, wherry.Terminal{Started: true, ExitCode: 137, Signal: "9", Node: "dev-node"}},
		{"pull-failure", nil, wherry.Terminal{Class: "driver", Node: "dev-node", Reason: "Failed to pull `registry.invalid/wherry/no-such-image:1`: Error response from daemon: failed to resolve reference \"registry.invalid/wherry/no-such-image:1\": failed to do request: Head \"https://registry.invalid/v2/wherry/no-such-image/manifests/1\": dialing registry.invalid:443 container via direct connection because Docker Desktop has no HTTPS proxy: connecting to registry.invalid:443: dial tcp: lookup registry.invalid: no such host"}},
	} {
		t.Run(test.capture, func(t *testing.T) {
			var alloc api.Allocation
			raw := testdata(t, test.capture+".allocation.json")
			if err := json.Unmarshal(raw, &alloc); err != nil {
				t.Fatal(err)
			}
			const stdout = "working\n"
			adapter, requests := nomadServer(t, map[string]http.HandlerFunc{
				"GET /v1/allocation/" + alloc.ID: serve(raw),
				"GET /v1/client/fs/logs/" + alloc.ID: func(response http.ResponseWriter, request *http.Request) {
					if !started(alloc.TaskStates[taskName]) {
						// Captured from the dev agent for the pull failure's allocation.
						http.Error(response, `task "agent" not started yet. No logs available`, http.StatusNotFound)
						return
					}
					offset, _ := strconv.Atoi(request.URL.Query().Get("offset"))
					_ = json.NewEncoder(response).Encode(&api.StreamFrame{Data: []byte(stdout[min(offset, len(stdout)):])})
				},
			})
			data, _ := json.Marshal(handle{V: 1, Job: alloc.JobID, Alloc: alloc.ID})
			lines := make(chan wherry.Line, 4)
			terminal, err := adapter.Follow(context.Background(), wherry.Handle{Data: data}, 0, lines)
			if err != nil || terminal != test.want {
				t.Fatalf("follow = %#v, %v; want %#v; requests %v", terminal, err, test.want, requests())
			}
			var got []string
			for _, line := range closeAndRead(lines) {
				got = append(got, string(line.Data))
			}
			if !slices.Equal(got, test.lines) {
				t.Fatalf("lines %q, want %q", got, test.lines)
			}
		})
	}
}

func TestSweepPurgesJobsDeadPastTheGrace(t *testing.T) {
	old, recent := time.Now().Add(-2*sweepGrace).UnixNano(), time.Now().UnixNano()
	const mine, otherPool = poolPrefix + "old", "agent-8934303dc131-old" // pool-a's digest
	// Every agent job carries run_id meta; a job under the prefix without it is not the adapter's.
	jobs := fmt.Appendf(nil, `[{"ID":%q,"Status":"dead","SubmitTime":%d,"Meta":{"profile":"container","run_id":"old"}},{"ID":%q,"Status":"dead","SubmitTime":%d,"Meta":{"profile":"container","run_id":"recent"}},{"ID":%q,"Status":"running","SubmitTime":%d,"Meta":{"profile":"container","run_id":"live"}},{"ID":%q,"Status":"dead","SubmitTime":%d,"Meta":{"profile":"container"}},{"ID":%q,"Status":"dead","SubmitTime":%d,"Meta":{"profile":"container","run_id":"old"}}]`,
		mine, old, poolPrefix+"recent", old, poolPrefix+"live", old, poolPrefix+"other", old, otherPool, old)
	routes := map[string]http.HandlerFunc{
		// The server ignores the prefix query, so the adapter's own pool filter is what is tested.
		"GET /v1/jobs":                                     serve(jobs),
		"GET /v1/job/" + mine + "/allocations":             serve(fmt.Appendf(nil, `[{"ID":"alloc-old","ClientStatus":"complete","ModifyTime":%d}]`, old)),
		"GET /v1/job/" + poolPrefix + "recent/allocations": serve(fmt.Appendf(nil, `[{"ID":"alloc-recent","ClientStatus":"complete","ModifyTime":%d}]`, recent)),
		"DELETE /v1/job/" + mine:                           serve([]byte(`{"EvalID":""}`)),
		"GET /v1/vars":                                     serve(fmt.Appendf(nil, `[{"Path":"nomad/jobs/%s","ModifyTime":%d}]`, otherPool, old)),
	}
	for _, suffix := range []string{"", "/agent"} {
		routes["DELETE /v1/var/nomad/jobs/"+mine+suffix] = func(response http.ResponseWriter, _ *http.Request) { response.WriteHeader(http.StatusNoContent) }
	}
	adapter, requests := nomadServer(t, routes)
	if err := adapter.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := deletes(requests()); len(got) != 3 || got[0] != "DELETE /v1/job/"+mine {
		t.Fatalf("deletes %v, want only this pool's agent job dead past the grace purged with its variables", got)
	}
}

// edited is a captured allocation list with edit applied to each allocation.
func edited(t *testing.T, capture string, edit func(*api.AllocationListStub)) []byte {
	t.Helper()
	var allocs []*api.AllocationListStub
	if err := json.Unmarshal(captured(t, capture), &allocs); err != nil {
		t.Fatal(err)
	}
	for _, alloc := range allocs {
		edit(alloc)
	}
	data, err := json.Marshal(allocs)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestLookupMapsEditedCaptures(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*api.AllocationListStub)
		want wherry.JobState
	}{
		// Nomad keeps a task's latest events only, so a long run can lose its Started event.
		{"started event aged out", func(alloc *api.AllocationListStub) {
			state := alloc.TaskStates[taskName]
			state.Events = slices.DeleteFunc(state.Events, func(event *api.TaskEvent) bool { return event.Type == api.TaskStarted })
		}, wherry.JobEnded},
		{"no StartedAt", func(alloc *api.AllocationListStub) { alloc.TaskStates[taskName].StartedAt = time.Time{} }, wherry.JobEnded},
		{"running", func(alloc *api.AllocationListStub) {
			alloc.ClientStatus = api.AllocClientStatusRunning
			alloc.TaskStates[taskName].State = "running"
		}, wherry.JobRunning},
	} {
		t.Run(test.name, func(t *testing.T) {
			adapter, _ := nomadServer(t, map[string]http.HandlerFunc{
				"GET /v1/job/" + poolPrefix + "capture-clean":             serve(captured(t, "clean.job.json")),
				"GET /v1/job/" + poolPrefix + "capture-clean/allocations": serve(edited(t, "clean.allocations.json", test.edit)),
			})
			if job, err := adapter.Lookup(context.Background(), "capture-clean"); err != nil || job.State != test.want {
				t.Fatalf("lookup = %#v, %v; want state %v", job, err, test.want)
			}
		})
	}
}

func TestSameRunInTwoPoolsRegistersTwoJobs(t *testing.T) {
	var mu sync.Mutex
	registered := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		path := request.URL.Path
		switch {
		case request.Method == http.MethodPut && strings.HasPrefix(path, "/v1/var/"):
			_, _ = response.Write([]byte(`{}`))
		case request.Method == http.MethodPut && path == "/v1/jobs":
			var body api.JobRegisterRequest
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				http.Error(response, err.Error(), http.StatusBadRequest)
				return
			}
			registered[*body.Job.ID] = true
			_, _ = response.Write([]byte(`{"EvalID":"eval-1"}`))
		case strings.HasSuffix(path, "/allocations"):
			_, _ = response.Write(captured(t, "clean.allocations.json"))
		case strings.HasPrefix(path, "/v1/job/") && registered[strings.TrimPrefix(path, "/v1/job/")]:
			_, _ = response.Write([]byte(`{}`))
		case strings.HasPrefix(path, "/v1/job/"):
			jobNotFound(response, request)
		default:
			http.Error(response, "unexpected request", http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	run := testRun("pools")
	run.ID = "run-shared"
	for _, pool := range []string{"pool-a", "pool-b"} {
		adapter, err := build(testConfig(server.URL), pool, "")
		if err != nil {
			t.Fatal(err)
		}
		if outcome, _, err := adapter.Dispatch(context.Background(), run); err != nil || outcome != wherry.OutcomeCreated {
			t.Fatalf("dispatch in %s = %v, %v", pool, outcome, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(registered) != 2 {
		t.Fatalf("registered %v, want one job per pool", registered)
	}
}

// collectServer is a Nomad whose allocation alloc-1 reports running to its first running reads, then ended[0] or
// complete. It lists the captured outputs plus extra, serves files by name, and records each read.
func collectServer(t *testing.T, running int, extra []map[string]any, files map[string]string, ended ...string) (*Adapter, func() []string, func() []string) {
	t.Helper()
	var listing []map[string]any
	if err := json.Unmarshal(testdata(t, "outputs.ls.json"), &listing); err != nil {
		t.Fatal(err)
	}
	root, err := json.Marshal(append(listing, extra...))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var reads []string
	adapter, requests := nomadServer(t, map[string]http.HandlerFunc{
		"GET /v1/allocation/alloc-1": func(response http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			status := "complete"
			if len(ended) > 0 {
				status = ended[0]
			}
			if running--; running >= 0 {
				status = "running"
			}
			mu.Unlock()
			fmt.Fprintf(response, `{"ID":"alloc-1","ClientStatus":%q}`, status)
		},
		"GET /v1/client/fs/ls/alloc-1": func(response http.ResponseWriter, request *http.Request) {
			switch request.URL.Query().Get("path") {
			case outputsDir:
				_, _ = response.Write(root)
			case outputsDir + "/sub":
				_, _ = response.Write(testdata(t, "outputs-sub.ls.json"))
			default:
				http.Error(response, "no such file or directory", http.StatusNotFound)
			}
		},
		"GET /v1/client/fs/readat/alloc-1": func(response http.ResponseWriter, request *http.Request) {
			name := strings.TrimPrefix(request.URL.Query().Get("path"), outputsDir+"/")
			mu.Lock()
			reads = append(reads, name)
			mu.Unlock()
			_, _ = response.Write([]byte(files[name]))
		},
	})
	return adapter, requests, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(reads)
	}
}

var capturedFiles = map[string]string{"report.md": "hello\n", "sub/notes.txt": "nested\n"}

func collectAll(adapter *Adapter, limits wherry.Limits) ([]wherry.Output, error) {
	handle, _ := json.Marshal(handle{V: 1, Job: "job-1", Alloc: "alloc-1"})
	outputs := make(chan wherry.Output, 64)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := adapter.Collect(ctx, wherry.Handle{Data: handle}, limits, outputs)
	close(outputs)
	var all []wherry.Output
	for output := range outputs {
		all = append(all, output)
	}
	return all, err
}

var wide = wherry.Limits{Files: 256, FileBytes: 1 << 20, TotalBytes: 1 << 20}

// The listing is a capture of Nomad 2.0.7's allocation filesystem API; the device entry is Go's fs.FileMode.String,
// which Nomad reports.
func TestCollectReadsOnlyRegularFiles(t *testing.T) {
	device := map[string]any{"Name": "tty", "IsDir": false, "Size": 0, "FileMode": (fs.ModeDevice | fs.ModeCharDevice | 0o620).String()}
	adapter, _, reads := collectServer(t, 0, []map[string]any{device}, capturedFiles)
	outputs, err := collectAll(adapter, wide)
	if err != nil {
		t.Fatal(err)
	}
	type entry struct {
		name string
		mode fs.FileMode
		data string
		read bool
	}
	var got []entry
	for _, output := range outputs {
		got = append(got, entry{output.Name, output.Mode, string(output.Data), output.Data != nil})
	}
	want := []entry{
		{"dirlink", fs.ModeSymlink, "", false}, {"empty", 0, "", true}, {"fifo", fs.ModeNamedPipe, "", false}, {"link", fs.ModeSymlink, "", false},
		{"report.md", 0, "hello\n", true}, {"sub/notes.txt", 0, "nested\n", true}, {"tty", fs.ModeDevice | fs.ModeCharDevice, "", false},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("collected %+v, want %+v", got, want)
	}
	if got := reads(); !slices.Equal(got, []string{"report.md", "sub/notes.txt"}) {
		t.Fatalf("read %v, want the non-empty regular files alone", got)
	}
}

func TestCollectCountsTheTreeBeforeReading(t *testing.T) {
	adapter, _, reads := collectServer(t, 0, nil, capturedFiles)
	if _, err := collectAll(adapter, wherry.Limits{Files: 6, FileBytes: 1 << 20, TotalBytes: 1 << 20}); err == nil {
		t.Fatal("collected a tree of 7 entries under a limit of 6")
	}
	if got := reads(); len(got) != 0 {
		t.Fatalf("read %v before refusing the tree", got)
	}
	if _, err := collectAll(adapter, wherry.Limits{Files: 7, FileBytes: 1 << 20, TotalBytes: 1 << 20}); err != nil {
		t.Fatalf("a tree of 7 entries under a limit of 7: %v", err)
	}
}

func TestCollectLeavesFilesOverTheLimitsUnread(t *testing.T) {
	for name, limits := range map[string]wherry.Limits{
		"file":  {Files: 256, FileBytes: 6, TotalBytes: 1 << 20},
		"total": {Files: 256, FileBytes: 1 << 20, TotalBytes: 6},
	} {
		t.Run(name, func(t *testing.T) {
			adapter, _, reads := collectServer(t, 0, nil, capturedFiles)
			outputs, err := collectAll(adapter, limits)
			if err != nil {
				t.Fatal(err)
			}
			notes := outputs[slices.IndexFunc(outputs, func(output wherry.Output) bool { return output.Name == "sub/notes.txt" })]
			if notes.Data != nil || notes.Size != 7 || !slices.Equal(reads(), []string{"report.md"}) {
				t.Fatalf("notes %+v after reads %v; want it listed with its size and unread", notes, reads())
			}
		})
	}
}

// The agent's outcome file lies in a directory beneath the outputs directory; Collect streams it when Limits.Stream
// names it, outside FileBytes and TotalBytes.
func TestCollectStreamsTheOutcomeFile(t *testing.T) {
	const outcome = `{"state":"TASK_STATE_COMPLETED"}`
	var reads []string
	adapter, _ := nomadServer(t, map[string]http.HandlerFunc{
		"GET /v1/allocation/alloc-1": serve([]byte(`{"ID":"alloc-1","ClientStatus":"complete"}`)),
		"GET /v1/client/fs/ls/alloc-1": func(response http.ResponseWriter, request *http.Request) {
			switch request.URL.Query().Get("path") {
			case outputsDir:
				_, _ = response.Write([]byte(`[{"Name":".wherry","IsDir":true,"Size":96,"FileMode":"drwxr-xr-x"},{"Name":"report.md","IsDir":false,"Size":6,"FileMode":"-rw-r--r--"}]`))
			case outputsDir + "/.wherry":
				fmt.Fprintf(response, `[{"Name":"outcome.json","IsDir":false,"Size":%d,"FileMode":"-rw-r--r--"}]`, len(outcome))
			default:
				http.Error(response, "no such file or directory", http.StatusNotFound)
			}
		},
		"GET /v1/client/fs/readat/alloc-1": func(response http.ResponseWriter, request *http.Request) {
			name := strings.TrimPrefix(request.URL.Query().Get("path"), outputsDir+"/")
			reads = append(reads, name)
			_, _ = response.Write([]byte(map[string]string{".wherry/outcome.json": outcome, "report.md": "hello\n"}[name]))
		},
	})
	outputs, err := collectAll(adapter, wherry.Limits{Files: 256, FileBytes: 6, TotalBytes: 6, Stream: []string{".wherry/outcome.json"}, StreamBytes: 1 << 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(outputs) != 2 || outputs[0].Name != ".wherry/outcome.json" || outputs[0].Data != nil || outputs[0].Body == nil || string(outputs[1].Data) != "hello\n" {
		t.Fatalf("collected %+v; want the outcome file streamed and report.md read", outputs)
	}
	streamed, err := io.ReadAll(outputs[0].Body)
	outputs[0].Body.Close()
	if err != nil || string(streamed) != outcome {
		t.Fatalf("streamed %q, %v; want %q", streamed, err, outcome)
	}
	if !slices.Equal(reads, []string{".wherry/outcome.json", "report.md"}) {
		t.Fatalf("read %v", reads)
	}
}

// Stop on Nomad returns before the allocation ends, so Collect waits for it within its context.
func TestCollectWaitsForTheAllocationToEnd(t *testing.T) {
	adapter, _, reads := collectServer(t, 2, nil, capturedFiles)
	if outputs, err := collectAll(adapter, wide); err != nil || len(outputs) != 6 || !slices.Equal(reads(), []string{"report.md", "sub/notes.txt"}) {
		t.Fatalf("collect = %d outputs, %v; want the outputs once the allocation ends", len(outputs), err)
	}
}

func TestCollectRefusesARunningAllocation(t *testing.T) {
	adapter, requests, _ := collectServer(t, 1000, nil, capturedFiles)
	if _, err := collectAll(adapter, wide); err == nil || err.Error() != "the allocation is still running" {
		t.Fatalf("collect from an allocation running past the context = %v; want still running", err)
	}
	if slices.ContainsFunc(requests(), func(request string) bool { return strings.Contains(request, "/fs/") }) {
		t.Fatalf("requests %v touched the filesystem of a running allocation", requests())
	}
}

func TestCollectRefusesALostAllocation(t *testing.T) {
	adapter, requests, _ := collectServer(t, 0, nil, capturedFiles, "lost")
	if _, err := collectAll(adapter, wide); err == nil || err.Error() != "the allocation was lost" {
		t.Fatalf("collect from a lost allocation = %v", err)
	}
	if slices.ContainsFunc(requests(), func(request string) bool { return strings.Contains(request, "/fs/") }) {
		t.Fatalf("requests %v touched the filesystem of a lost allocation", requests())
	}
}

// Nomad 2.0.7 answers some failed reads with status 200 and the error as the body, as measured for a link that
// escapes the allocation directory.
func TestCollectRefusesABodyOfAnotherLength(t *testing.T) {
	adapter, _, _ := collectServer(t, 0, nil, map[string]string{"report.md": "path escapes the alloc directory", "sub/notes.txt": "nested\n"})
	if _, err := collectAll(adapter, wide); err == nil {
		t.Fatal("collected a body whose length differs from the listing")
	}
}

func TestCollectWithoutAnOutputsDirectory(t *testing.T) {
	adapter, _ := nomadServer(t, map[string]http.HandlerFunc{
		"GET /v1/allocation/alloc-1": serve([]byte(`{"ID":"alloc-1","ClientStatus":"complete"}`)),
		"GET /v1/client/fs/ls/alloc-1": func(response http.ResponseWriter, _ *http.Request) {
			http.Error(response, "no such file or directory", http.StatusNotFound)
		},
	})
	if outputs, err := collectAll(adapter, wide); err != nil || len(outputs) != 0 {
		t.Fatalf("collect = %v, %v; want no outputs", outputs, err)
	}
}

// fs.FileMode.String writes what Nomad's listing reports.
func TestFileModeParsesWhatGoWrites(t *testing.T) {
	for _, mode := range []fs.FileMode{0o644, fs.ModeDir | 0o755, fs.ModeSymlink | 0o777, fs.ModeNamedPipe | 0o600, fs.ModeSocket | 0o600,
		fs.ModeDevice | 0o600, fs.ModeDevice | fs.ModeCharDevice | 0o620, fs.ModeSetuid | 0o755, fs.ModeDir | fs.ModeSticky | 0o777, fs.ModeIrregular} {
		if got := fileMode(mode.String()); got.Type() != mode.Type() {
			t.Errorf("fileMode(%q) type %v, want %v", mode.String(), got.Type(), mode.Type())
		}
	}
	if fileMode("rw") != fs.ModeIrregular {
		t.Error("a mode too short to parse is not irregular")
	}
}

func TestDispatchRefusesInputsOverTheVariableLimit(t *testing.T) {
	adapter, requests := nomadServer(t, nil)
	run := testRun("too-large")
	// 8 bytes of name and 65532 of base64: four over the limit.
	run.Inputs = []wherry.File{{Name: "edge.bin", Data: make([]byte, 49149)}}
	outcome, _, err := adapter.Dispatch(context.Background(), run)
	var tooLarge *wherry.InputTooLargeError
	if outcome != wherry.OutcomeNothingCreated || !errors.As(err, &tooLarge) || tooLarge.Name != "edge.bin" {
		t.Fatalf("dispatch = %v, %v; want nothing created and edge.bin too large", outcome, err)
	}
	if got := requests(); len(got) != 0 {
		t.Fatalf("requests %v before refusing the inputs", got)
	}
}

func scriptAdapter(t *testing.T, script string) (*Adapter, context.Context) {
	t.Helper()
	adapter, ctx := devAdapter(t)
	adapter.runtime = Runtime{Driver: "raw_exec", Config: map[string]any{"command": "/bin/sh", "args": []any{"-c", script}}}
	return adapter, ctx
}

// runToOutputs dispatches run, follows it to its end, stops it and collects its outputs by name.
func runToOutputs(t *testing.T, adapter *Adapter, ctx context.Context, run wherry.Run) map[string]wherry.Output {
	t.Helper()
	handle := dispatch(t, adapter, ctx, run)
	output(t, ctx, adapter, handle)
	if err := adapter.Stop(ctx, handle); err != nil {
		t.Fatal(err)
	}
	outputs := make(chan wherry.Output, 64)
	if err := adapter.Collect(ctx, handle, wide, outputs); err != nil {
		t.Fatalf("collect: %v", err)
	}
	close(outputs)
	byName := map[string]wherry.Output{}
	for output := range outputs {
		byName[output.Name] = output
	}
	return byName
}

func TestInputsCredentialsAndOutputsOnNomad(t *testing.T) {
	adapter, ctx := scriptAdapter(t, `set -e
o="$WHERRY_OUTPUT_DIR"
mkdir -p "$o/sub"
cp "$WHERRY_INPUT_DIR/bin.dat" "$o/bin.dat"
cp "$WHERRY_CREDENTIALS_DIR/id_token" "$o/token.copy"
ls -a "$WHERRY_INPUT_DIR" > "$o/inputs.txt"
env > "$o/env.txt"
echo nested > "$o/sub/notes.txt"
: > "$o/empty"
ln -s ../../secrets/credentials/id_token "$o/link"
mkfifo "$o/fifo"
echo '{"type":"result","result":"done"}'`)
	data := make([]byte, 0, 259)
	for b := range 256 {
		data = append(data, byte(b))
	}
	data = append(data, 0, 0xff, 0xfe)
	const secret = "task-credential-value-on-nomad"
	run := testRun("files")
	run.Inputs = []wherry.File{{Name: "bin.dat", Data: data}, {Name: "empty.txt"}}
	run.Credentials = []wherry.Secret{{Name: "deploy-key", File: "id_token", Value: []byte(secret)}}
	outputs := runToOutputs(t, adapter, ctx, run)
	if got := outputs["bin.dat"].Data; !bytes.Equal(got, data) {
		t.Fatalf("binary input came back as %x, want %x", got, data)
	}
	if got := string(outputs["token.copy"].Data); got != secret {
		t.Fatalf("credential file held %q", got)
	}
	if got := string(outputs["inputs.txt"].Data); got != ".\n..\nbin.dat\nempty.txt\n" {
		t.Fatalf("input directory held %q, want the inputs alone", got)
	}
	if env := string(outputs["env.txt"].Data); strings.Contains(env, secret) || !strings.Contains(env, "WHERRY_CREDENTIALS_DIR=") || !strings.Contains(env, "/agent/secrets/credentials\n") {
		t.Fatalf("environment %q carries the credential or lacks the credentials directory", env)
	}
	for name, want := range map[string]wherry.Output{
		"sub/notes.txt": {Name: "sub/notes.txt", Size: 7, Data: []byte("nested\n")},
		"empty":         {Name: "empty", Data: []byte{}},
		"link":          {Name: "link", Size: int64(len("../../secrets/credentials/id_token")), Mode: fs.ModeSymlink},
		"fifo":          {Name: "fifo", Mode: fs.ModeNamedPipe},
	} {
		if got := outputs[name]; !reflect.DeepEqual(got, want) {
			t.Errorf("output %s = %+v, want %+v", name, got, want)
		}
	}
}

// The variable limit is measured: an input that fills it exactly arrives intact; one byte more is refused.
func TestInputAtTheVariableLimitOnNomad(t *testing.T) {
	adapter, ctx := scriptAdapter(t, `mkdir -p "$WHERRY_OUTPUT_DIR"; cp "$WHERRY_INPUT_DIR/edge.bin" "$WHERRY_OUTPUT_DIR/edge.bin"; echo '{"type":"result","result":"done"}'`)
	run := testRun("edge")
	// 8 bytes of name and 65528 of base64 fill the 64 KiB exactly.
	data := bytes.Repeat([]byte{0xa5, 0x00, 0xff}, 49146/3)
	run.Inputs = []wherry.File{{Name: "edge.bin", Data: data}}
	if got := runToOutputs(t, adapter, ctx, run)["edge.bin"].Data; !bytes.Equal(got, data) {
		t.Fatalf("an input filling the variable came back as %d bytes, want %d", len(got), len(data))
	}
	if adapter.InlineLimit() > int64(len(data)) {
		t.Fatalf("the declared inline limit %d is over the %d bytes that fill the variable", adapter.InlineLimit(), len(data))
	}
	over := testRun("over")
	over.Inputs = []wherry.File{{Name: "edge.bin", Data: append(data, 0)}}
	cleanupRun(t, adapter, over)
	var tooLarge *wherry.InputTooLargeError
	if outcome, _, err := adapter.Dispatch(ctx, over); outcome != wherry.OutcomeNothingCreated || !errors.As(err, &tooLarge) {
		t.Fatalf("dispatch of one byte more = %v, %v; want nothing created", outcome, err)
	}
	if job, err := adapter.Lookup(ctx, over.ID); err != nil || job.State != wherry.JobAbsent {
		t.Fatalf("lookup after the refusal = %+v, %v; want absent", job, err)
	}
}

func TestDirectoriesFollowTheDriver(t *testing.T) {
	for driver, want := range map[string]wherry.Directories{
		"exec":     {Input: "/local/inputs", Output: "/local/outputs", Credentials: "/secrets/credentials"},
		"podman":   {Input: "/local/inputs", Output: "/local/outputs", Credentials: "/secrets/credentials"},
		"raw_exec": {Input: "${NOMAD_TASK_DIR}/inputs", Output: "${NOMAD_TASK_DIR}/outputs", Credentials: "${NOMAD_SECRETS_DIR}/credentials"},
	} {
		config := testConfig("http://127.0.0.1:1")
		config.Runtime.Driver = driver
		adapter, err := build(config, testPool, "")
		if err != nil {
			t.Fatal(err)
		}
		if got := adapter.Directories(); got != want {
			t.Errorf("%s directories %+v, want %+v", driver, got, want)
		}
		run := testRun("directories")
		job, err := adapter.buildJob(run, adapter.variablePath(run.ID))
		if err != nil {
			t.Fatal(err)
		}
		if resolves := strings.Contains(*job.TaskGroups[0].Tasks[0].Templates[0].EmbeddedTmpl, `env "NOMAD_TASK_DIR"`); resolves == (driver != "raw_exec") {
			t.Errorf("%s prompt template resolves the task's variables: %v", driver, resolves)
		}
	}
}

// Under raw_exec the task sees host paths, which the prompt template writes in place of the reported variables.
func TestPromptCarriesTheTasksAbsolutePathsOnNomad(t *testing.T) {
	adapter, ctx := scriptAdapter(t, `cat "$NOMAD_TASK_DIR/prompt.txt"; printf '\n%s %s\n' "$WHERRY_INPUT_DIR" "$WHERRY_CREDENTIALS_DIR"`)
	run := testRun("paths")
	directories := adapter.Directories()
	run.Prompt = []byte("inputs " + directories.Input + "/data.csv credentials " + directories.Credentials + "/id_token")
	lines := collect(t, ctx, adapter, dispatch(t, adapter, ctx, run))
	if len(lines) != 2 {
		t.Fatalf("task printed %d lines", len(lines))
	}
	input, credentials, _ := strings.Cut(string(lines[1].Data), " ")
	if want := "inputs " + input + "/data.csv credentials " + credentials + "/id_token"; string(lines[0].Data) != want || !filepath.IsAbs(input) || !filepath.IsAbs(credentials) {
		t.Fatalf("prompt %q, want %q with the task's absolute directories", lines[0].Data, want)
	}
}

func TestBuildJobDeliversFilesThroughTemplates(t *testing.T) {
	adapter, err := build(testConfig("http://127.0.0.1:1"), testPool, "")
	if err != nil {
		t.Fatal(err)
	}
	run := testRun("files")
	run.Inputs = []wherry.File{{Name: "data.csv"}}
	run.Credentials = []wherry.Secret{{Name: "deploy-key", File: "id_ed25519"}}
	path := adapter.variablePath(run.ID)
	job, err := adapter.buildJob(run, path)
	if err != nil {
		t.Fatal(err)
	}
	templates := map[string]string{}
	for _, template := range job.TaskGroups[0].Tasks[0].Templates {
		templates[*template.DestPath] = *template.EmbeddedTmpl
	}
	for destination, want := range map[string]string{
		"local/inputs/data.csv":                       `{{ with nomadVar "` + path + `/agent" }}{{ (index . "data.csv").Value | base64Decode }}{{ end }}`,
		"${NOMAD_SECRETS_DIR}/credentials/id_ed25519": `{{ with nomadVar "` + path + `" }}{{ (index . "credentials/id_ed25519").Value | base64Decode }}{{ end }}`,
	} {
		if got, ok := templates[destination]; !ok || got != want {
			t.Errorf("template for %s = %q, %v; want %q", destination, got, ok, want)
		}
	}
}

func TestBuildJobIsolatesTheAgentsGit(t *testing.T) {
	adapter, err := build(testConfig("http://127.0.0.1:1"), testPool, "")
	if err != nil {
		t.Fatal(err)
	}
	adapter.runtime.Env = map[string]string{"GIT_SSH_COMMAND": "ssh -o StrictHostKeyChecking=no", "GIT_SSH": "ssh", "GIT_CURL_VERBOSE": "1",
		"GIT_CONFIG_GLOBAL": "/etc/agent-gitconfig", "GIT_TRACE": "/tmp/trace", "KEPT": "yes"}
	run := testRun("isolated")
	job, err := adapter.buildJob(run, adapter.variablePath(run.ID))
	if err != nil {
		t.Fatal(err)
	}
	env := job.TaskGroups[0].Tasks[0].Env
	for _, name := range wherry.AgentUnset {
		if _, set := env[name]; set {
			t.Errorf("task environment sets %s", name)
		}
	}
	for name, value := range wherry.AgentEnvironment {
		if env[name] != value {
			t.Errorf("task environment %s = %q, want %q", name, env[name], value)
		}
	}
	if env["KEPT"] != "yes" {
		t.Errorf("task environment lost the runtime's own variable")
	}
}

// The agent task runs with the adapter's environment over the runtime's.
func TestAgentEnvironmentReachesTheTask(t *testing.T) {
	adapter, ctx := scriptAdapter(t, `env; echo '{"type":"result","result":"ok"}'`)
	adapter.runtime.Env = map[string]string{"GIT_SSH_COMMAND": "ssh -o StrictHostKeyChecking=no", "GIT_CONFIG_GLOBAL": "/etc/agent-gitconfig"}
	run := testRun("agent-env")
	var got []string
	for _, line := range collect(t, ctx, adapter, dispatch(t, adapter, ctx, run)) {
		got = append(got, string(line.Data))
	}
	for name, value := range wherry.AgentEnvironment {
		if !slices.Contains(got, name+"="+value) {
			t.Errorf("task environment lacks %s=%s", name, value)
		}
	}
	if slices.ContainsFunc(got, func(line string) bool { return strings.HasPrefix(line, "GIT_SSH_COMMAND=") }) {
		t.Errorf("task environment sets GIT_SSH_COMMAND")
	}
}

// A patch series written by git format-patch --output into the outputs directory is collected as one regular file
// holding every commit.
func TestCollectReturnsAPatchSeries(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	script := `set -e
export GIT_AUTHOR_NAME=agent GIT_AUTHOR_EMAIL=agent@example.test GIT_COMMITTER_NAME=agent GIT_COMMITTER_EMAIL=agent@example.test
repo="$NOMAD_TASK_DIR/repo"
git init -q "$repo"
echo one > "$repo/README"; git -C "$repo" add README; git -C "$repo" commit -q -m one
git -C "$repo" update-ref refs/wherry/start HEAD
for change in two three; do echo $change > "$repo/README"; git -C "$repo" commit -q -am $change; done
mkdir -p "$NOMAD_TASK_DIR/outputs"
git -C "$repo" format-patch --output="$NOMAD_TASK_DIR/outputs/changes.patch" refs/wherry/start..HEAD
echo '{"type":"result","result":"ok"}'`
	adapter, ctx := scriptAdapter(t, script)
	outputs := runToOutputs(t, adapter, ctx, testRun("patch-series"))
	patch, ok := outputs["changes.patch"]
	if len(outputs) != 1 || !ok || !patch.Mode.IsRegular() || patch.Data == nil {
		t.Fatalf("outputs %v, want changes.patch alone as a regular file", slices.Collect(maps.Keys(outputs)))
	}
	if n := strings.Count(string(patch.Data), "\nSubject: [PATCH "); n != 2 {
		t.Fatalf("changes.patch holds %d patches, want 2:\n%s", n, patch.Data)
	}
}

// show prints a template field's value, or nil.
func show[T any](value *T) string {
	if value == nil {
		return "nil"
	}
	return fmt.Sprint(*value)
}

// samples returns the adapter's samples by family and label value.
func samples(t *testing.T, adapter *Adapter) map[string]wherry.Sample {
	t.Helper()
	reported, err := adapter.Metrics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]wherry.Sample{}
	for _, sample := range reported {
		byName[sample.Family+"/"+sample.Labels["class"]+sample.Labels["kind"]] = sample
	}
	return byName
}

func TestAPIErrorsAreClassifiedByStatus(t *testing.T) {
	for code, class := range map[int]string{
		http.StatusUnauthorized: "denied", http.StatusForbidden: "denied", http.StatusConflict: "conflict",
		http.StatusInternalServerError: "unavailable", http.StatusServiceUnavailable: "unavailable",
		http.StatusGatewayTimeout: "timeout", http.StatusBadRequest: "other", http.StatusNotFound: "",
	} {
		adapter, _ := nomadServer(t, map[string]http.HandlerFunc{"GET /v1/nodes": func(response http.ResponseWriter, _ *http.Request) {
			http.Error(response, "no", code)
		}})
		if _, err := adapter.Ready(context.Background()); err == nil {
			t.Fatalf("ready answered %d without an error", code)
		}
		var counted []string
		for name, sample := range samples(t, adapter) {
			if sample.Family == "wherry_adapter_platform_errors_total" {
				counted = append(counted, fmt.Sprintf("%s=%v", name, sample.Value))
			}
		}
		want := []string(nil)
		if class != "" {
			want = []string{"wherry_adapter_platform_errors_total/" + class + "=1"}
		}
		if !slices.Equal(counted, want) {
			t.Fatalf("status %d counted %v; want %v", code, counted, want)
		}
	}
}

func TestUnreachableNomadIsUnavailable(t *testing.T) {
	adapter, err := build(testConfig("http://127.0.0.1:1"), testPool, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Ready(context.Background()); err == nil {
		t.Fatal("ready reached a closed port")
	}
	if sample := samples(t, adapter)["wherry_adapter_platform_errors_total/unavailable"]; sample.Value != 1 {
		t.Fatalf("unavailable errors %v; want 1", sample.Value)
	}
}

func TestSweepReportsDeletionsAndPendingJobs(t *testing.T) {
	old, recent := time.Now().Add(-2*sweepGrace).UnixNano(), time.Now().UnixNano()
	const dead, gone = poolPrefix + "old", poolPrefix + "gone"
	routes := map[string]http.HandlerFunc{
		"GET /v1/jobs":                                     serve(fmt.Appendf(nil, `[{"ID":%q,"Status":"dead","SubmitTime":%d,"Meta":{"profile":"container","run_id":"r"}},{"ID":%q,"Status":"dead","SubmitTime":%d,"Meta":{"profile":"container","run_id":"r"}}]`, dead, old, poolPrefix+"recent", recent)),
		"GET /v1/job/" + dead + "/allocations":             serve([]byte(`[]`)),
		"GET /v1/job/" + poolPrefix + "recent/allocations": serve([]byte(`[]`)),
		"DELETE /v1/job/" + dead:                           serve([]byte(`{"EvalID":""}`)),
		"GET /v1/vars":                                     serve(fmt.Appendf(nil, `[{"Path":"nomad/jobs/%s","ModifyTime":%d}]`, gone, old)),
		"GET /v1/job/" + gone:                              jobNotFound,
	}
	for _, path := range []string{dead, dead + "/agent", gone} {
		routes["DELETE /v1/var/nomad/jobs/"+path] = func(response http.ResponseWriter, _ *http.Request) { response.WriteHeader(http.StatusNoContent) }
	}
	adapter, _ := nomadServer(t, routes)
	if err := adapter.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := samples(t, adapter)
	if got["wherry_adapter_cleanup_pending/"].Value != 1 || got["wherry_adapter_sweep_deletions_total/job"].Value != 1 || got["wherry_adapter_sweep_deletions_total/variable"].Value != 1 {
		t.Fatalf("samples %+v; want one pending job, one job and one variable deleted", got)
	}
}

func TestDispatchReportsPlacement(t *testing.T) {
	const job = poolPrefix + "run-placed"
	routes := map[string]http.HandlerFunc{
		"PUT /v1/var/nomad/jobs/" + job:       serve([]byte(`{}`)),
		"GET /v1/job/" + job:                  jobNotFound,
		"PUT /v1/jobs":                        serve([]byte(`{"EvalID":"eval-1","EvalCreateIndex":10,"JobModifyIndex":10}`)),
		"GET /v1/job/" + job + "/allocations": serve([]byte(`[{"ID":"alloc-1","ClientStatus":"running","TaskStates":{"agent":{"State":"running","StartedAt":"2026-10-20T00:00:00Z"}}}]`)),
	}
	adapter, _ := nomadServer(t, routes)
	run := testRun("placed")
	run.ID = "run-placed"
	if outcome, _, err := adapter.Dispatch(context.Background(), run); err != nil || outcome != wherry.OutcomeCreated {
		t.Fatalf("dispatch = %v, %v", outcome, err)
	}
	placement := samples(t, adapter)["wherry_adapter_placement_seconds/"].Histogram
	if placement.Count != 1 || len(placement.Buckets) != len(wherry.PlacementBuckets) || placement.Buckets[len(placement.Buckets)-1] != 1 {
		t.Fatalf("placement %+v; want one observation over the placement buckets", placement)
	}
}

func TestJobEnvCarriesTheModelOnlyWhenSet(t *testing.T) {
	adapter, err := build(testConfig("http://127.0.0.1:1"), testPool, "")
	if err != nil {
		t.Fatal(err)
	}
	for model, want := range map[string]bool{"": false, "sonnet": true} {
		run := testRun("model")
		run.Model = model
		job, err := adapter.buildJob(run, adapter.variablePath(run.ID))
		if err != nil {
			t.Fatal(err)
		}
		task := job.TaskGroups[0].Tasks[0]
		if value, set := task.Env["WHERRY_MODEL"]; set != want || value != model {
			t.Fatalf("model %q: env WHERRY_MODEL=%q set=%t, want set=%t", model, value, set, want)
		}
		if encoded, _ := json.Marshal(task.Config); model != "" && strings.Contains(string(encoded), model) {
			t.Fatalf("model %q reached the task's command line: %s", model, encoded)
		}
	}
}

// Every profile of a pool lists the pool's jobs; each counts and sweeps only the jobs its meta names.
func TestSweepCountsOnlyThisProfilesJobs(t *testing.T) {
	old, recent := time.Now().Add(-2*sweepGrace).UnixNano(), time.Now().UnixNano()
	const waiting, expired = poolPrefix + "waiting", poolPrefix + "expired"
	routes := map[string]http.HandlerFunc{
		"GET /v1/jobs": serve(fmt.Appendf(nil, `[{"ID":%q,"Status":"dead","SubmitTime":%d,"Meta":{"profile":"container","run_id":"r"}},{"ID":%q,"Status":"dead","SubmitTime":%d,"Meta":{"profile":"container","run_id":"r"}}]`,
			waiting, recent, expired, old)),
		"GET /v1/job/" + waiting + "/allocations": serve([]byte(`[]`)),
		"GET /v1/job/" + expired + "/allocations": serve([]byte(`[]`)),
		"DELETE /v1/job/" + expired:               serve([]byte(`{"EvalID":""}`)),
		"GET /v1/vars":                            serve(fmt.Appendf(nil, `[{"Path":"nomad/jobs/%s","ModifyTime":%d}]`, waiting, old)),
		"GET /v1/job/" + waiting:                  serve([]byte(`{"ID":"` + waiting + `","Status":"dead","Stop":false,"Meta":{"profile":"container","run_id":"r"}}`)),
	}
	for _, path := range []string{waiting, expired, expired + "/agent"} {
		routes["DELETE /v1/var/nomad/jobs/"+path] = func(response http.ResponseWriter, _ *http.Request) { response.WriteHeader(http.StatusNoContent) }
	}
	a, _ := nomadServer(t, routes)
	b, requestsOfB := nomadServer(t, routes)
	b.name = "host"
	for _, adapter := range []*Adapter{a, b} {
		if err := adapter.Sweep(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := samples(t, a); got["wherry_adapter_cleanup_pending/"].Value != 1 || got["wherry_adapter_sweep_deletions_total/job"].Value != 1 || got["wherry_adapter_sweep_deletions_total/variable"].Value != 1 {
		t.Fatalf("container samples %+v; want its waiting job pending, its expired job and the waiting job's variable deleted", got)
	}
	if got := samples(t, b); got["wherry_adapter_cleanup_pending/"].Value != 0 || got["wherry_adapter_sweep_deletions_total/job"].Value != 0 || got["wherry_adapter_sweep_deletions_total/variable"].Value != 0 {
		t.Fatalf("host samples %+v; want none of container's jobs counted", got)
	}
	if deleted := deletes(requestsOfB()); len(deleted) != 0 {
		t.Fatalf("host's sweep deleted %v of container's jobs", deleted)
	}
}

// A job is this profile's by its profile meta alone; a job whose meta names it under any other key is another's.
func TestSweepOwnsAJobByItsProfileMetaAlone(t *testing.T) {
	old := time.Now().Add(-2 * sweepGrace).UnixNano()
	const own, other = poolPrefix + "own", poolPrefix + "other"
	routes := map[string]http.HandlerFunc{
		"GET /v1/jobs": serve(fmt.Appendf(nil, `[{"ID":%q,"Status":"dead","SubmitTime":%d,"Meta":{"profile":"container","run_id":"r"}},{"ID":%q,"Status":"dead","SubmitTime":%d,"Meta":{"adapter":"container","run_id":"r"}}]`,
			own, old, other, old)),
		"GET /v1/job/" + own + "/allocations":   serve([]byte(`[]`)),
		"GET /v1/job/" + other + "/allocations": serve([]byte(`[]`)),
		"DELETE /v1/job/" + own:                 serve([]byte(`{"EvalID":""}`)),
		"GET /v1/vars":                          serve([]byte(`[]`)),
	}
	for _, path := range []string{own, own + "/agent"} {
		routes["DELETE /v1/var/nomad/jobs/"+path] = func(response http.ResponseWriter, _ *http.Request) { response.WriteHeader(http.StatusNoContent) }
	}
	adapter, requests := nomadServer(t, routes)
	if err := adapter.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if deleted := deletes(requests()); !slices.Contains(deleted, "DELETE /v1/job/"+own) || slices.Contains(deleted, "DELETE /v1/job/"+other) {
		t.Fatalf("deletes %v; want the job whose profile meta is container purged and the other kept", deleted)
	}
	if got := samples(t, adapter)["wherry_adapter_sweep_deletions_total/job"].Value; got != 1 {
		t.Fatalf("job deletions %v; want 1", got)
	}
}

// A returned error is the receiver's to log; one the adapter handles itself, such as a poll read, it logs by op and class.
func TestHandledPlatformErrorIsLoggedOnceWithoutURLOrToken(t *testing.T) {
	const token, body = "nomad-token-secret", "body-never-logged"
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		http.Error(response, body, http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)
	adapter, err := build(testConfig(server.URL), testPool, token)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	adapter.logger = hclog.New(&hclog.LoggerOptions{Output: &logs, JSONFormat: true})
	if _, err := adapter.Ready(context.Background()); err == nil {
		t.Fatal("ready answered 503 without an error")
	}
	if logs.Len() != 0 {
		t.Fatalf("returned error logged by the adapter: %q", logs.String())
	}
	if adapter.finished(context.Background(), "alloc-id") != nil {
		t.Fatal("finished read a 503 as a finished allocation")
	}
	lines := strings.Split(strings.TrimSuffix(logs.String(), "\n"), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], `"class":"unavailable"`) || !strings.Contains(lines[0], `"op":"read allocation"`) || !strings.Contains(lines[0], `"@level":"warn"`) {
		t.Fatalf("logs %q; want one warning naming the operation and class unavailable", logs.String())
	}
	for _, forbidden := range []string{token, body, strings.TrimPrefix(server.URL, "http://"), "/v1/"} {
		if strings.Contains(logs.String(), forbidden) {
			t.Fatalf("log line %q holds %q", lines[0], forbidden)
		}
	}
}

// The variables of another profile's job dead for the grace period, such as one whose profile has left the receiver's
// configuration, hold its prompt and credentials, so any profile's sweep deletes them; the job and its count stay its
// profile's.
func TestSweepDeletesVariablesOfAnotherProfilesExpiredJob(t *testing.T) {
	old := time.Now().Add(-2 * sweepGrace).UnixNano()
	const expired = poolPrefix + "removed"
	routes := map[string]http.HandlerFunc{
		"GET /v1/jobs": serve(fmt.Appendf(nil, `[{"ID":%q,"Status":"dead","SubmitTime":%d,"Meta":{"profile":"removed","run_id":"r"}}]`, expired, old)),
		"GET /v1/job/" + expired + "/allocations": serve([]byte(`[]`)),
		"GET /v1/vars":           serve(fmt.Appendf(nil, `[{"Path":"nomad/jobs/%s","ModifyTime":%d},{"Path":"nomad/jobs/%s/agent","ModifyTime":%d}]`, expired, old, expired, old)),
		"GET /v1/job/" + expired: serve([]byte(`{"ID":"` + expired + `","Status":"dead","Stop":false,"Meta":{"profile":"removed","run_id":"r"}}`)),
	}
	for _, path := range []string{expired, expired + "/agent"} {
		routes["DELETE /v1/var/nomad/jobs/"+path] = func(response http.ResponseWriter, _ *http.Request) { response.WriteHeader(http.StatusNoContent) }
	}
	adapter, requests := nomadServer(t, routes)
	if err := adapter.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, want := deletes(requests()), []string{"DELETE /v1/var/nomad/jobs/" + expired, "DELETE /v1/var/nomad/jobs/" + expired + "/agent"}; !slices.Equal(got, want) {
		t.Fatalf("deletes %v, want the expired job's variables and not the job", got)
	}
	if got := samples(t, adapter); got["wherry_adapter_cleanup_pending/"].Value != 0 || got["wherry_adapter_sweep_deletions_total/job"].Value != 0 || got["wherry_adapter_sweep_deletions_total/variable"].Value != 0 {
		t.Fatalf("samples %+v; want nothing counted for another profile's job", got)
	}
}

func TestTerminalCarriesTheStartFailuresMessage(t *testing.T) {
	for _, test := range []struct {
		name  string
		event api.TaskEvent
		want  wherry.Terminal
	}{
		{"validation", api.TaskEvent{Type: api.TaskFailedValidation, DisplayMessage: "failed to interpolate ${BAD:-x}", ValidationError: "interpolate"}, wherry.Terminal{Class: "validation", Reason: "failed to interpolate ${BAD:-x}"}},
		{"fetch", api.TaskEvent{Type: api.TaskArtifactDownloadFailed, DisplayMessage: `error downloading "https://store.example/object?X-Amz-Signature=abc"`}, wherry.Terminal{Class: wherry.ClassFetch, Reason: `error downloading "https://store.example/object?X-Amz-Signature=abc"`}},
		{"error field", api.TaskEvent{Type: api.TaskSetupFailure, SetupError: "failed to build task directory"}, wherry.Terminal{Class: "setup", Reason: "failed to build task directory"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			alloc := &api.Allocation{ClientStatus: api.AllocClientStatusFailed, TaskStates: map[string]*api.TaskState{taskName: {State: "dead", Failed: true, Events: []*api.TaskEvent{&test.event}}}}
			if got := terminalOf(alloc); got != test.want {
				t.Fatalf("terminal %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestStopWaitsForATaskWhoseSignalWasRefused(t *testing.T) {
	running := api.Allocation{ID: "alloc-1", ClientStatus: api.AllocClientStatusRunning, TaskStates: map[string]*api.TaskState{taskName: {State: "running"}}}
	for _, test := range []struct {
		name string
		// exitedAt is the allocation read from which Nomad reports the task complete; 0 is never.
		exitedAt int
		// stopped is whether Stop counts the signal's failure and stops the job.
		stopped bool
	}{
		{"exit reported late", 3, false},
		{"still running", 0, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			reads := 0
			adapter, requests := nomadServer(t, map[string]http.HandlerFunc{
				"GET /v1/allocation/alloc-1": func(response http.ResponseWriter, _ *http.Request) {
					alloc := running
					if reads++; test.exitedAt > 0 && reads >= test.exitedAt {
						alloc = api.Allocation{ID: "alloc-1", ClientStatus: api.AllocClientStatusComplete}
					}
					_ = json.NewEncoder(response).Encode(&alloc)
				},
				"PUT /v1/client/allocation/alloc-1/signal": func(response http.ResponseWriter, _ *http.Request) {
					http.Error(response, "task not running", http.StatusInternalServerError)
				},
				"DELETE /v1/job/agent-1": serve([]byte(`{"EvalID":"eval-1"}`)),
			})
			if err := adapter.Stop(context.Background(), wherry.Handle{Data: []byte(`{"v":1,"job":"agent-1","alloc":"alloc-1","format":""}`)}); err != nil {
				t.Fatal(err)
			}
			counted, want := samples(t, adapter)["wherry_adapter_platform_errors_total/unavailable"].Value, 0.0
			if test.stopped {
				want = 1
			}
			if stopped := slices.Contains(requests(), "DELETE /v1/job/agent-1"); counted != want || stopped != test.stopped {
				t.Fatalf("counted %v errors, stopped the job %t; want %v, %t", counted, stopped, want, test.stopped)
			}
		})
	}
}

func TestStopReturnsWhenCancelledDuringTheGrace(t *testing.T) {
	running := api.Allocation{ID: "alloc-1", ClientStatus: api.AllocClientStatusRunning, TaskStates: map[string]*api.TaskState{taskName: {State: "running"}}}
	adapter, _ := nomadServer(t, map[string]http.HandlerFunc{
		"GET /v1/allocation/alloc-1":               func(response http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(response).Encode(&running) },
		"PUT /v1/client/allocation/alloc-1/signal": serve([]byte(`{}`)),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := adapter.Stop(ctx, wherry.Handle{Data: []byte(`{"v":1,"job":"agent-1","alloc":"alloc-1","format":""}`)})
	if elapsed := time.Since(start); !errors.Is(err, context.DeadlineExceeded) || elapsed > 3*time.Second {
		t.Fatalf("Stop returned %v after %v; want the context's error well inside the %v grace", err, elapsed, stopGrace)
	}
}

// Nomad's API quotes the bytes of a log frame it cannot decode in its error, and those are the agent's output.
func TestUndecodableLogStreamKeepsItsBytesOut(t *testing.T) {
	const marker = "agent-output-never-in-errors"
	encoded := base64.StdEncoding.EncodeToString([]byte(marker))
	raw := testdata(t, "signal.allocation.json")
	var alloc api.Allocation
	if err := json.Unmarshal(raw, &alloc); err != nil {
		t.Fatal(err)
	}
	adapter, _ := nomadServer(t, map[string]http.HandlerFunc{
		"GET /v1/allocation/" + alloc.ID: serve(raw),
		"GET /v1/client/fs/logs/" + alloc.ID: func(response http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(response, `{"Data":"`+encoded)
		},
	})
	data, _ := json.Marshal(handle{V: 1, Job: alloc.JobID, Alloc: alloc.ID})
	_, err := adapter.Follow(context.Background(), wherry.Handle{Data: data}, 0, make(chan wherry.Line, 4))
	var failure *wherry.Error
	if !errors.As(err, &failure) || failure.Op != "read logs" {
		t.Fatalf("follow = %#v; want the log read's failure", err)
	}
	for _, leaked := range []string{marker, encoded} {
		if strings.Contains(err.Error(), leaked) {
			t.Fatalf("follow error %q holds the stream's bytes", err)
		}
	}
}

func TestReleaseReturnsOneFailureAsItIs(t *testing.T) {
	path := "/v1/var/nomad/jobs/" + poolPrefix + "run-release"
	adapter, _ := nomadServer(t, map[string]http.HandlerFunc{
		"DELETE " + path: func(response http.ResponseWriter, _ *http.Request) {
			http.Error(response, "denied", http.StatusForbidden)
		},
		"DELETE " + path + "/agent": func(response http.ResponseWriter, _ *http.Request) { response.WriteHeader(http.StatusNoContent) },
	})
	err := adapter.Release(context.Background(), "run-release")
	if failure, ok := err.(*wherry.Error); !ok || failure.Class != "denied" {
		t.Fatalf("release = %#v; want the one failure as a wherry.Error", err)
	}
}

func TestBuildJobDefaultsResources(t *testing.T) {
	config := testConfig("http://127.0.0.1:1")
	config.Runtime.Resources = Resources{}
	adapter, err := build(config, testPool, "")
	if err != nil {
		t.Fatal(err)
	}
	run := testRun("resources")
	job, err := adapter.buildJob(run, adapter.variablePath(run.ID))
	if err != nil {
		t.Fatal(err)
	}
	if resources := job.TaskGroups[0].Tasks[0].Resources; *resources.CPU != defaultCPU || *resources.MemoryMB != defaultMemoryMB {
		t.Fatalf("resources %d MHz %d MiB, want the defaults", *resources.CPU, *resources.MemoryMB)
	}
}

func TestJoinedKeepsASingleError(t *testing.T) {
	single := &wherry.Error{Op: "list variables", Class: "unavailable", Err: errors.New("down")}
	if err := joined([]error{nil, single, nil}); err != error(single) {
		t.Fatalf("joined one error = %v; want it as it is, so its op and class cross the link", err)
	}
	if err := joined([]error{nil}); err != nil {
		t.Fatalf("joined no errors = %v", err)
	}
	other := errors.New("other")
	if err := joined([]error{single, other}); !errors.Is(err, single) || !errors.Is(err, other) {
		t.Fatalf("joined two errors = %v; want both", err)
	}
}
