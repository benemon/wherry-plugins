package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benemon/wherry/pluginsdk"
	"github.com/gorilla/websocket"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/api"
)

const workingAlloc = "alloc-working"

var workingHandle = encodeHandle(poolPrefix+"working", workingAlloc, "raw_exec")

// allocation answers GET /v1/allocation/<id> with an allocation in clientStatus and desiredStatus whose agent task is
// in taskState; an empty taskState leaves the task out.
func allocation(clientStatus, desiredStatus, taskState string) http.HandlerFunc {
	alloc := api.Allocation{ID: workingAlloc, NodeID: "node-working", TaskGroup: taskName, ClientStatus: clientStatus, DesiredStatus: desiredStatus}
	if taskState != "" {
		alloc.TaskStates = map[string]*api.TaskState{taskName: {State: taskState}}
	}
	body, _ := json.Marshal(alloc) // the allocation always encodes
	return serve(body)
}

var runningAllocation = allocation(api.AllocClientStatusRunning, api.AllocDesiredStatusRun, "running")

// execSession is what a fake exec endpoint received: its query and whether stdin closed.
type execSession struct {
	command, task, tty string
	stdinClosed        bool
}

// fakeExec upgrades the exec request to a websocket, records the session, and answers with frames, the client's reads
// of stdin running alongside.
func fakeExec(t *testing.T, sessions chan<- execSession, frames ...api.ExecStreamingOutput) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(response, request, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		query := request.URL.Query()
		session := execSession{command: query.Get("command"), task: query.Get("task"), tty: query.Get("tty")}
		stdin := make(chan bool, 1)
		go func() {
			for {
				var input api.ExecStreamingInput
				if conn.ReadJSON(&input) != nil {
					stdin <- false
					return
				}
				if input.Stdin != nil && input.Stdin.Close {
					stdin <- true
					return
				}
			}
		}()
		select {
		case session.stdinClosed = <-stdin:
		case <-time.After(5 * time.Second):
		}
		for _, frame := range frames {
			if err := conn.WriteJSON(frame); err != nil {
				t.Error(err)
				return
			}
		}
		sessions <- session
	}
}

func stdoutFrame(data string) api.ExecStreamingOutput {
	return api.ExecStreamingOutput{Stdout: &api.ExecStreamingIOOperation{Data: []byte(data)}}
}

func exitFrame(code int) api.ExecStreamingOutput {
	return api.ExecStreamingOutput{Exited: true, Result: &api.ExecStreamingExitResult{ExitCode: code}}
}

// closeWith closes the exec websocket with text, as the Nomad client reports a failed exec.
func closeWith(text string) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(response, request, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, text))
	}
}

func workingServer(t *testing.T, exec http.HandlerFunc, alloc ...http.HandlerFunc) (*Nomad, func() []string) {
	t.Helper()
	info := runningAllocation
	if len(alloc) > 0 {
		var mu sync.Mutex
		info = func(response http.ResponseWriter, request *http.Request) {
			mu.Lock()
			next := alloc[0]
			if len(alloc) > 1 {
				alloc = alloc[1:]
			}
			mu.Unlock()
			next(response, request)
		}
	}
	return nomadServer(t, map[string]http.HandlerFunc{
		"GET /v1/allocation/" + workingAlloc:                  info,
		"GET /v1/client/allocation/" + workingAlloc + "/exec": exec,
	})
}

func fsRequests(requests []string) []string {
	return slices.DeleteFunc(requests, func(request string) bool { return !strings.Contains(request, "/v1/client/fs/") })
}

func TestReadWorkingExecsInTheAgentTask(t *testing.T) {
	sessions := make(chan execSession, 1)
	stderr := api.ExecStreamingOutput{Stderr: &api.ExecStreamingIOOperation{Data: []byte("head: warning\n")}}
	profile, requests := workingServer(t, fakeExec(t, sessions, stderr, stdoutFrame("reading the "), stdoutFrame("tests\n"+workingEnd), exitFrame(0)))
	data, err := profile.ReadWorking(context.Background(), workingHandle)
	if err != nil || string(data) != "reading the tests\n" {
		t.Fatalf("read = %q, %v; want the line", data, err)
	}
	session := <-sessions
	command, _ := json.Marshal(workingCommand)
	if session.command != string(command) || session.task != taskName || session.tty != "false" || !session.stdinClosed {
		t.Fatalf("exec %+v; want %s in the agent task without a TTY, stdin closed", session, command)
	}
	if !slices.Equal(workingCommand[:2], []string{"sh", "-c"}) || !strings.Contains(workingCommand[2], `timeout -s KILL 2 head -c 1025 -- "$NOMAD_TASK_DIR/outputs/.wherry/working" && echo wherry-working-end`) {
		t.Fatalf("command %q; want the bounded read of the task's working file, then the terminator", workingCommand)
	}
	if fs := fsRequests(requests()); len(fs) > 0 {
		t.Fatalf("requests %v; want no allocation filesystem API call", fs)
	}
}

func TestReadWorkingOutputs(t *testing.T) {
	full := strings.Repeat("x", pluginsdk.MaxWorking)
	for name, test := range map[string]struct {
		frames []api.ExecStreamingOutput
		want   string
	}{
		"at the limit":       {[]api.ExecStreamingOutput{stdoutFrame(full + workingEnd), exitFrame(0)}, full},
		"no newline":         {[]api.ExecStreamingOutput{stdoutFrame("step" + workingEnd), exitFrame(0)}, "step"},
		"over the limit":     {[]api.ExecStreamingOutput{stdoutFrame(full + "x" + workingEnd), exitFrame(0)}, ""},
		"cut short":          {[]api.ExecStreamingOutput{stdoutFrame("step\n"), exitFrame(0)}, ""},
		"more than the read": {[]api.ExecStreamingOutput{stdoutFrame(full + "xx" + workingEnd), exitFrame(0)}, ""},
		"missing file":       {[]api.ExecStreamingOutput{exitFrame(1)}, ""},
		"FIFO killed":        {[]api.ExecStreamingOutput{exitFrame(137)}, ""},
		"FIFO busybox":       {[]api.ExecStreamingOutput{exitFrame(143)}, ""},
		"partial then fails": {[]api.ExecStreamingOutput{stdoutFrame("step\n"), exitFrame(1)}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			profile, _ := workingServer(t, fakeExec(t, make(chan execSession, 1), test.frames...))
			data, err := profile.ReadWorking(context.Background(), workingHandle)
			if err != nil || string(data) != test.want {
				t.Fatalf("read = %q, %v; want %q", data, err, test.want)
			}
			if errors := samples(t, profile); len(errors) != 1 {
				t.Fatalf("samples %v; want no platform error counted", errors)
			}
		})
	}
}

func TestReadWorkingCountsAnImageWithoutTheCommand(t *testing.T) {
	var logs bytes.Buffer
	for _, code := range []int{126, 127} {
		profile, _ := workingServer(t, fakeExec(t, make(chan execSession, 2), exitFrame(code)))
		logs.Reset()
		profile.logger = hclog.New(&hclog.LoggerOptions{Output: &logs, JSONFormat: true})
		for range 2 {
			var failure *pluginsdk.Error
			if data, err := profile.ReadWorking(context.Background(), workingHandle); data != nil || !errors.As(err, &failure) || failure.Class != "other" {
				t.Fatalf("exit %d: read = %q, %v; want a counted error", code, data, err)
			}
		}
		if got := samples(t, profile)["wherry_plugin_platform_errors_total/other"].Value; got != 2 {
			t.Fatalf("exit %d: errors counted %v; want one a read", code, got)
		}
		if got := strings.Count(logs.String(), "needs sh, timeout and head"); got != 1 {
			t.Fatalf("exit %d: logged %d times; want once a profile:\n%s", code, got, logs.String())
		}
	}
}

func TestReadWorkingOnlyExecsARunningTask(t *testing.T) {
	notFound := func(response http.ResponseWriter, _ *http.Request) {
		http.Error(response, "alloc not found", http.StatusNotFound)
	}
	for name, test := range map[string]struct {
		handle pluginsdk.Handle
		alloc  http.HandlerFunc
	}{
		"no allocation":   {encodeHandle(poolPrefix+"working", "", "raw_exec"), runningAllocation},
		"purged":          {workingHandle, notFound},
		"pending":         {workingHandle, allocation(api.AllocClientStatusPending, api.AllocDesiredStatusRun, "pending")},
		"task pending":    {workingHandle, allocation(api.AllocClientStatusRunning, api.AllocDesiredStatusRun, "pending")},
		"no task state":   {workingHandle, allocation(api.AllocClientStatusRunning, api.AllocDesiredStatusRun, "")},
		"stopping":        {workingHandle, allocation(api.AllocClientStatusRunning, api.AllocDesiredStatusStop, "running")},
		"complete":        {workingHandle, allocation(api.AllocClientStatusComplete, api.AllocDesiredStatusRun, "dead")},
		"failed":          {workingHandle, allocation(api.AllocClientStatusFailed, api.AllocDesiredStatusRun, "dead")},
		"lost":            {workingHandle, allocation(api.AllocClientStatusLost, api.AllocDesiredStatusStop, "running")},
		"task terminated": {workingHandle, allocation(api.AllocClientStatusRunning, api.AllocDesiredStatusRun, "dead")},
	} {
		t.Run(name, func(t *testing.T) {
			profile, requests := workingServer(t, fakeExec(t, make(chan execSession, 1), stdoutFrame("stale\n"+workingEnd), exitFrame(0)), test.alloc)
			if data, err := profile.ReadWorking(context.Background(), test.handle); data != nil || err != nil {
				t.Fatalf("read = %q, %v; want nothing", data, err)
			}
			if slices.ContainsFunc(requests(), func(request string) bool { return strings.HasSuffix(request, "/exec") }) {
				t.Fatalf("requests %v; want no exec", requests())
			}
			if errors := samples(t, profile); len(errors) != 1 {
				t.Fatalf("samples %v; want no platform error counted", errors)
			}
		})
	}
}

func TestReadWorkingExecErrors(t *testing.T) {
	stopped := allocation(api.AllocClientStatusComplete, api.AllocDesiredStatusRun, "dead")
	for name, test := range map[string]struct {
		alloc []http.HandlerFunc
		text  string
		class string
	}{
		"task ended":        {[]http.HandlerFunc{runningAllocation, stopped}, `task "agent" is not running.`, ""},
		"desired stop":      {[]http.HandlerFunc{runningAllocation, allocation(api.AllocClientStatusRunning, api.AllocDesiredStatusStop, "running")}, `task "agent" is not running.`, ""},
		"task still runs":   {[]http.HandlerFunc{runningAllocation}, "exec failed", "other"},
		"permission denied": {[]http.HandlerFunc{runningAllocation}, "Permission denied", "denied"},
	} {
		t.Run(name, func(t *testing.T) {
			profile, _ := workingServer(t, closeWith(test.text), test.alloc...)
			data, err := profile.ReadWorking(context.Background(), workingHandle)
			var failure *pluginsdk.Error
			switch {
			case test.class == "" && (data != nil || err != nil):
				t.Fatalf("read = %q, %v; want nothing", data, err)
			case test.class != "" && (data != nil || !errors.As(err, &failure) || failure.Class != test.class):
				t.Fatalf("read = %q, %v; want an error of class %s", data, err, test.class)
			}
			want := 1
			if test.class != "" {
				want = 2
			}
			if got := samples(t, profile); len(got) != want {
				t.Fatalf("samples %v; want %d", got, want)
			}
		})
	}
}

// TestReadWorkingReturnsOnTheContextWithOneExecInFlight holds the exec's websocket handshake, which the exec's dial
// does not bound by the context.
func TestReadWorkingReturnsOnTheContextWithOneExecInFlight(t *testing.T) {
	release := make(chan struct{})
	held := make(chan struct{}, 4)
	profile, requests := workingServer(t, func(http.ResponseWriter, *http.Request) {
		held <- struct{}{}
		<-release
	})
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	data, err := profile.ReadWorking(ctx, workingHandle)
	if !errors.Is(err, context.DeadlineExceeded) || data != nil || time.Since(start) > 2*time.Second {
		t.Fatalf("read = %q, %v after %s; want the deadline's error at once", data, err, time.Since(start))
	}
	<-held
	if data, err := profile.ReadWorking(context.Background(), workingHandle); data != nil || err != nil {
		t.Fatalf("second read = %q, %v; want nothing while the first is in flight", data, err)
	}
	if execs := slices.DeleteFunc(requests(), func(request string) bool { return !strings.HasSuffix(request, "/exec") }); len(execs) != 1 {
		t.Fatalf("exec requests %v; want one", execs)
	}
}

// shRuntime runs script under sh on raw_exec as user, or the client's user without one.
func shRuntime(script, user string) Runtime {
	return Runtime{Driver: "raw_exec", Config: map[string]any{"command": "/bin/sh", "args": []any{"-c", script}}, User: user}
}

// devProfileOn is a profile on the dev agent whose jobs run runtime.
func devProfileOn(t *testing.T, runtime Runtime) *Nomad {
	t.Helper()
	config := testConfig(os.Getenv("WHERRY_NOMAD_DEV_ADDR"))
	config.Runtime = runtime
	profile, err := build(config, testPool, "")
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

// devRead reads the working file of handle's job on the dev agent until done returns true for a read, each read bounded
// by the receiver's 5 s deadline; a read error fails the test.
func devRead(t *testing.T, ctx context.Context, profile *Nomad, handle pluginsdk.Handle, done func(data []byte, took time.Duration) bool) {
	t.Helper()
	for ctx.Err() == nil {
		readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		start := time.Now()
		data, err := profile.ReadWorking(readCtx, handle)
		cancel()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if done(data, time.Since(start)) {
			return
		}
	}
	t.Fatal("reads ended before the condition held")
}

// TestReadWorkingInTheTask reads working files on the dev agent through alloc exec, as the agent task's user: raw_exec
// as a user other than the agent's, which a swapped link must not take to a file only root reads, the cases that read
// nothing, and the docker driver with the e2e's busybox agent image, wherry-e2e-agent:e2e.
func TestReadWorkingInTheTask(t *testing.T) {
	_, ctx := devProfile(t)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 4*time.Minute)
	t.Cleanup(cancel)

	t.Run("Swap", func(t *testing.T) {
		marker := fmt.Sprintf("/tmp/wherry-working-marker-%d", time.Now().UnixNano())
		owner := devProfileOn(t, shRuntime(`umask 077 && printf 'root only\n' >`+marker+` || exit 1; trap 'rm -f `+marker+`; exit 0' TERM; while :; do sleep 1; done`, ""))
		dispatch(t, owner, ctx, testRun("marker"))
		for {
			if _, err := os.Stat(marker); err == nil {
				break
			}
			if ctx.Err() != nil {
				t.Fatal("the marker job wrote no marker")
			}
			time.Sleep(200 * time.Millisecond)
		}
		swapper := devProfileOn(t, shRuntime(`d=$WHERRY_OUTPUT_DIR/.wherry; mkdir -p "$d" || exit 1
while :; do
	printf 'inside\n' >"$d/file" && mv -f "$d/file" "$d/working"
	ln -sf `+marker+` "$d/link" && mv -f "$d/link" "$d/working"
done`, "nobody"))
		handle := dispatch(t, swapper, ctx, testRun("swap"))
		inside, reads, end := 0, 0, time.Now().Add(30*time.Second)
		devRead(t, ctx, swapper, handle, func(data []byte, _ time.Duration) bool {
			reads++
			switch string(data) {
			case "root only\n":
				t.Fatal("read the root-only marker through a swapped link")
			case "inside\n":
				inside++
			}
			return time.Now().After(end)
		})
		// Each read that returned the inside line ran in the task, so exec failures cannot pass the test.
		if inside < 10 {
			t.Fatalf("%d of %d reads returned the inside line; want at least 10", inside, reads)
		}
		t.Logf("%d of %d reads returned the inside line", inside, reads)
	})

	t.Run("Nothing", func(t *testing.T) {
		for name, plant := range map[string]string{
			"FIFO":       `mkfifo "$d/working"`,
			"directory":  `mkdir "$d/working"`,
			"over 1 KiB": `head -c 1025 /dev/zero | tr '\0' x >"$d/next" && mv -f "$d/next" "$d/working"`,
		} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				planted := devProfileOn(t, shRuntime(`d=$WHERRY_OUTPUT_DIR/.wherry; mkdir -p "$d" || exit 1
printf 'ready\n' >"$d/next" && mv -f "$d/next" "$d/working" && sleep 15 && rm -f "$d/working" && `+plant+` || exit 1
sleep 300`, ""))
				handle := dispatch(t, planted, ctx, testRun(strings.ReplaceAll(strings.ToLower(name), " ", "-")))
				// The ready line shows the read runs in the task before the planted file reads as nothing.
				devRead(t, ctx, planted, handle, func(data []byte, _ time.Duration) bool { return string(data) == "ready\n" })
				devRead(t, ctx, planted, handle, func(data []byte, _ time.Duration) bool { return data == nil })
				for range 3 {
					devRead(t, ctx, planted, handle, func(data []byte, took time.Duration) bool {
						if data != nil || took > 3*time.Second {
							t.Fatalf("read = %q after %s; want nothing within 3 s", data, took)
						}
						return true
					})
				}
			})
		}
	})

	t.Run("Docker", func(t *testing.T) {
		docker := devProfileOn(t, Runtime{Driver: "docker", Config: map[string]any{
			"image": "wherry-e2e-agent:e2e", "entrypoint": []any{"/bin/sh", "-c", `d=$WHERRY_OUTPUT_DIR/.wherry; mkdir -p "$d" && printf 'in the container\n' >"$d/next" && mv -f "$d/next" "$d/working" || exit 1; exec sleep 300`},
		}})
		handle := dispatch(t, docker, ctx, testRun("docker"))
		devRead(t, ctx, docker, handle, func(data []byte, _ time.Duration) bool { return string(data) == "in the container\n" })
	})
}
