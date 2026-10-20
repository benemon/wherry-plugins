package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	wherry "github.com/benemon/wherry/adapter"
	"github.com/hashicorp/nomad/api"
)

const (
	jobPrefix         = "agent-"
	jobVariablePrefix = "nomad/jobs/"
	taskName          = "agent"
	gitTaskName       = "git"
	formatCRI         = "cri"
	maxPending        = 1 << 20
	// maxVariable is the size Nomad allows a variable's item keys and values together, measured on Nomad 2.0.7.
	maxVariable = 64 << 10
	outputsDir  = taskName + "/local/outputs"
	stopGrace   = 15 * time.Second
	followPoll  = time.Second
	// collectPoll separates the reads of an allocation that Collect waits on, since Stop returns before it ends.
	collectPoll = 500 * time.Millisecond
	cleanupWait = 10 * time.Second
)

// The suffixes turn a run variable path into a task's own variable path; among the job's tasks,
// Nomad's implicit workload policy lets only that task read it. The policy reaches no deeper path, so a turn's
// inputs share the group's variable, which every task of the job can read.
const (
	promptSuffix = "/" + taskName + "/" + taskName
	gitSuffix    = "/" + taskName + "/" + gitTaskName
	inputsSuffix = "/" + taskName
)

// credentialItem is the run variable's item holding the value of the task credential delivered as file.
func credentialItem(file string) string { return "credentials/" + file }

// cloneScript reads the git context from a rendered file, so no task value becomes shell syntax or
// meets Nomad interpolation; the receiver admits no ref or path starting with a dash. Its last
// two stdout lines are the checkout directory and the commit.
const cloneScript = `set -eu
{ read -r url; read -r ref; read -r dir; } < "$NOMAD_TASK_DIR/git"
dest="$NOMAD_ALLOC_DIR/$dir"
git init -q "$dest"
cd "$dest"
git remote add origin "$url"
git fetch -q --depth 1 origin "$ref"
git checkout -q --detach FETCH_HEAD
printf '%s\n' "$dest"
git rev-parse HEAD
`

// Config is the adapter's section of the receiver configuration.
type Config struct {
	Address       string  `json:"address"`
	CAFile        string  `json:"ca_file"`
	Namespace     string  `json:"namespace"`
	CredentialEnv string  `json:"credential_env"`
	Runtime       Runtime `json:"runtime"`
	// NodePool and Datacenters bound where Nomad may place the adapter's
	// jobs; unset, Nomad's defaults apply.
	NodePool    string   `json:"node_pool,omitzero"`
	Datacenters []string `json:"datacenters,omitzero"`
}

// Runtime defines the agent task the adapter launches.
type Runtime struct {
	Driver    string            `json:"driver"`
	Config    map[string]any    `json:"config"`
	Env       map[string]string `json:"env"`
	Resources Resources         `json:"resources"`
}

// Resources are the agent task's resources; unset values take Nomad's defaults.
type Resources struct {
	CPU      *int `json:"cpu"`
	MemoryMB *int `json:"memory_mb"`
}

// Adapter registers per-run agent jobs in one namespace.
type Adapter struct {
	client        *api.Client
	namespace     string
	credentialEnv string
	runtime       Runtime
	nodePool      string
	datacenters   []string
	// prefix starts every job ID of the adapter's pool; agentJob matches those IDs, runVariable a run
	// variable, or a task variable of a run with a git context, of one of those jobs.
	prefix      string
	agentJob    *regexp.Regexp
	runVariable *regexp.Regexp
}

var (
	envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	commit  = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	// sweepGrace covers Dispatch's window between writing the variable and registering the job.
	sweepGrace = 10 * time.Minute
)

// newClient builds a Nomad client for config's address, CA and namespace with token.
func newClient(config Config, token string) (*api.Client, error) {
	apiConfig := api.DefaultConfig()
	apiConfig.Address = config.Address
	apiConfig.Namespace = config.Namespace
	apiConfig.SecretID = token
	apiConfig.TLSConfig = &api.TLSConfig{CACert: config.CAFile}
	return api.NewClient(apiConfig)
}

// New builds an adapter for a pool's jobs on client, which config's address, CA and namespace built.
func New(config Config, pool string, client *api.Client) (*Adapter, error) {
	if config.CredentialEnv != "" && !envName.MatchString(config.CredentialEnv) {
		return nil, errors.New("credential_env must be an environment variable name")
	}
	if config.Runtime.Driver == "" {
		return nil, errors.New("runtime requires a driver")
	}
	datacenters := config.Datacenters
	if len(datacenters) == 0 {
		datacenters = []string{"*"}
	}
	// Job IDs carry a digest of the pool, since a full 63-character pool name and run ID would pass Nomad's
	// 128-character variable path limit.
	sum := sha256.Sum256([]byte(pool))
	prefix := jobPrefix + hex.EncodeToString(sum[:])[:12] + "-"
	return &Adapter{
		client: client, namespace: config.Namespace, credentialEnv: config.CredentialEnv, runtime: config.Runtime, nodePool: config.NodePool, datacenters: datacenters,
		prefix:      prefix,
		agentJob:    regexp.MustCompile(`^` + regexp.QuoteMeta(prefix) + `.+-t[0-9]+$`),
		runVariable: regexp.MustCompile(`^` + regexp.QuoteMeta(jobVariablePrefix) + `(` + regexp.QuoteMeta(prefix) + `.+-t[0-9]+)(` + regexp.QuoteMeta(promptSuffix) + `|` + regexp.QuoteMeta(gitSuffix) + `|` + regexp.QuoteMeta(inputsSuffix) + `)?$`),
	}, nil
}

// Git reports that the adapter delivers a run's git context, through its prestart git task.
func (adapter *Adapter) Git() bool { return true }

// isolated are the drivers whose task sees its task directory at /local and its secrets at /secrets.
var isolated = []string{"docker", "exec", "java", "podman"}

// Directories reports /local and /secrets paths for a driver that isolates the task's filesystem. Any other driver's
// task sees host paths that depend on its allocation, so the adapter reports Nomad's variables for them, which the
// prompt's template resolves from the task's environment.
func (adapter *Adapter) Directories() wherry.Directories {
	task, secrets := "${NOMAD_TASK_DIR}", "${NOMAD_SECRETS_DIR}"
	if slices.Contains(isolated, adapter.runtime.Driver) {
		task, secrets = "/local", "/secrets"
	}
	return wherry.Directories{Input: task + "/inputs", Output: task + "/outputs", Credentials: secrets + "/credentials"}
}

// Ready reports whether some ready, eligible node in the adapter's node pool
// and datacenters has the runtime's driver healthy.
func (adapter *Adapter) Ready(ctx context.Context) (bool, error) {
	nodes, _, err := adapter.client.Nodes().List((&api.QueryOptions{}).WithContext(ctx))
	if err != nil {
		return false, err
	}
	for _, node := range nodes {
		if node.Status != api.NodeStatusReady || node.SchedulingEligibility != api.NodeSchedulingEligible {
			continue
		}
		// A job without a node pool lands in Nomad's default pool.
		if pool := cmp.Or(adapter.nodePool, "default"); node.NodePool != pool {
			continue
		}
		if !slices.Contains(adapter.datacenters, "*") && !slices.Contains(adapter.datacenters, node.Datacenter) {
			continue
		}
		if info := node.Drivers[adapter.runtime.Driver]; info != nil && info.Detected && info.Healthy {
			return true, nil
		}
	}
	return false, nil
}

// Dispatch starts one run idempotently and returns once its task has started or ended. It removes the
// turn's material only when Info confirms that no job exists. Inputs and task credentials travel base64-encoded,
// which the templates decode, so binary files arrive intact.
func (adapter *Adapter) Dispatch(ctx context.Context, run wherry.Run) (wherry.Outcome, wherry.Handle, error) {
	path := adapter.variablePath(run.ID, run.Turn)
	job, err := adapter.buildJob(run, path)
	if err != nil {
		return wherry.OutcomeNothingCreated, wherry.Handle{}, err
	}
	inputs, size := map[string]string{}, 0
	for _, input := range run.Inputs {
		inputs[input.Name] = base64.StdEncoding.EncodeToString(input.Data)
		if size += len(input.Name) + len(inputs[input.Name]); size > maxVariable {
			return wherry.OutcomeNothingCreated, wherry.Handle{}, &wherry.InputTooLargeError{Name: input.Name}
		}
	}
	jobID := *job.ID
	items := map[string]string{"prompt": string(run.Prompt)}
	if run.Credential != nil {
		items["credential"] = string(run.Credential)
	}
	for _, secret := range run.Credentials {
		items[credentialItem(secret.File)] = base64.StdEncoding.EncodeToString(secret.Value)
	}
	variable := &api.Variable{Namespace: adapter.namespace, Path: path, Items: items}
	if _, _, err := adapter.client.Variables().Update(variable, (&api.WriteOptions{}).WithContext(ctx)); err != nil {
		return adapter.notStarted(ctx, run, redacted("write", path, err))
	}
	if len(inputs) > 0 {
		inputsPath := path + inputsSuffix
		if _, _, err := adapter.client.Variables().Update(&api.Variable{Namespace: adapter.namespace, Path: inputsPath, Items: inputs}, (&api.WriteOptions{}).WithContext(ctx)); err != nil {
			return adapter.notStarted(ctx, run, redacted("write", inputsPath, err))
		}
	}
	if run.Git != nil {
		gitItems := map[string]string{"git_url": run.Git.URL, "git_ref": run.Git.Ref, "git_path": run.Git.Path}
		gitPath := path + gitSuffix
		if _, _, err := adapter.client.Variables().Update(&api.Variable{Namespace: adapter.namespace, Path: gitPath, Items: gitItems}, (&api.WriteOptions{}).WithContext(ctx)); err != nil {
			return adapter.notStarted(ctx, run, redacted("write", gitPath, err))
		}
	}
	if _, _, err := adapter.client.Jobs().Info(jobID, (&api.QueryOptions{}).WithContext(ctx)); err != nil {
		if !notFound(err) {
			return wherry.OutcomeUnknown, wherry.Handle{}, err
		}
		if _, _, err := adapter.client.Jobs().RegisterOpts(job, &api.RegisterOptions{EnforceIndex: true, ModifyIndex: 0}, (&api.WriteOptions{}).WithContext(ctx)); err != nil {
			return adapter.notStarted(ctx, run, fmt.Errorf("register: %w", err))
		}
	}
	handle, _, err := adapter.wait(ctx, jobID, job.TaskGroups[0].Tasks[0].Driver, run.Git != nil, path)
	if err != nil {
		return wherry.OutcomeUnknown, wherry.Handle{}, err
	}
	return wherry.OutcomeCreated, handle, nil
}

// notStarted returns OutcomeNothingCreated, with the turn's material released, when Info finds no job;
// otherwise OutcomeUnknown with cause and everything left in place. A failed release is an error, which
// the core resolves with Lookup.
func (adapter *Adapter) notStarted(ctx context.Context, run wherry.Run, cause error) (wherry.Outcome, wherry.Handle, error) {
	if _, _, err := adapter.client.Jobs().Info(adapter.jobID(run.ID, run.Turn), (&api.QueryOptions{}).WithContext(ctx)); !notFound(err) {
		return wherry.OutcomeUnknown, wherry.Handle{}, cause
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupWait)
	defer cancel()
	return wherry.OutcomeNothingCreated, wherry.Handle{}, adapter.Release(cleanupCtx, run.ID, run.Turn)
}

// Lookup finds a turn's existing job and waits for it as Dispatch does.
func (adapter *Adapter) Lookup(ctx context.Context, runID string, turn int64) (wherry.Job, error) {
	job, _, err := adapter.client.Jobs().Info(adapter.jobID(runID, turn), (&api.QueryOptions{}).WithContext(ctx))
	if notFound(err) {
		return wherry.Job{}, nil
	}
	if err != nil {
		return wherry.Job{}, err
	}
	if len(job.TaskGroups) != 1 {
		return wherry.Job{}, errors.New("existing job is not an agent job")
	}
	driver, git := "", false
	for _, task := range job.TaskGroups[0].Tasks {
		switch task.Name {
		case taskName:
			driver = task.Driver
		case gitTaskName:
			git = true
		default:
			return wherry.Job{}, errors.New("existing job is not an agent job")
		}
	}
	if driver == "" {
		return wherry.Job{}, errors.New("existing job is not an agent job")
	}
	handle, state, err := adapter.wait(ctx, *job.ID, driver, git, adapter.variablePath(runID, turn))
	if err != nil {
		return wherry.Job{}, err
	}
	// A job registered without the key reads as attempt 0.
	attempt, _ := strconv.ParseInt(job.Meta["attempt"], 10, 64)
	return wherry.Job{State: state, Handle: handle, Attempt: attempt, Adapter: job.Meta["adapter"], Agent: job.Meta["agent"]}, nil
}

// handle is the version 1 encoding of wherry.Handle.Data.
type handle struct {
	V      int    `json:"v"`
	Job    string `json:"job"`
	Alloc  string `json:"alloc"`
	Format string `json:"format"`
}

func decodeHandle(encoded wherry.Handle) (handle, error) {
	var decoded handle
	if err := json.Unmarshal(encoded.Data, &decoded); err != nil {
		return handle{}, err
	}
	if decoded.V != 1 {
		return handle{}, fmt.Errorf("handle version %d", decoded.V)
	}
	return decoded, nil
}

// wait returns once the job's agent task has started or ended; a wait cut short deregisters the job.
// With a git context, it delivers the agent's prompt once the clone task has finished.
func (adapter *Adapter) wait(ctx context.Context, jobID, driver string, git bool, path string) (wherry.Handle, wherry.JobState, error) {
	format := ""
	if driver == "podman" {
		format = formatCRI
	}
	checkedOut := ""
	for {
		allocs, _, err := adapter.client.Jobs().Allocations(jobID, false, (&api.QueryOptions{}).WithContext(ctx))
		if err != nil {
			if ctx.Err() != nil {
				return wherry.Handle{}, 0, adapter.deregisterTimedOut(ctx, jobID)
			}
			return wherry.Handle{}, 0, err
		}
		alloc := current(allocs)
		if alloc != nil && git && checkedOut == "" {
			switch state := alloc.TaskStates[gitTaskName]; {
			case state != nil && state.Failed:
				return wherry.Handle{}, 0, wherry.ErrGitContext
			case state != nil && state.State == "dead":
				if checkedOut, err = adapter.deliverPrompt(ctx, alloc.ID, format == formatCRI, path); err != nil {
					return wherry.Handle{}, 0, err
				}
			}
		}
		// The logs API answers 404 until the task has started.
		if alloc != nil && (terminal(alloc.ClientStatus) || started(alloc.TaskStates[taskName])) {
			data, _ := json.Marshal(handle{V: 1, Job: jobID, Alloc: alloc.ID, Format: format}) // strings always encode
			return wherry.Handle{Data: data, Name: jobID, Commit: checkedOut}, jobState(alloc), nil
		}
		select {
		case <-ctx.Done():
			return wherry.Handle{}, 0, adapter.deregisterTimedOut(ctx, jobID)
		case <-time.After(2 * time.Second):
		}
	}
}

// jobState keys never-started on a terminal allocation whose agent task has no Started event.
func jobState(alloc *api.AllocationListStub) wherry.JobState {
	switch {
	case !terminal(alloc.ClientStatus):
		return wherry.JobRunning
	case started(alloc.TaskStates[taskName]):
		return wherry.JobEnded
	}
	return wherry.JobNeverStarted
}

// deliverPrompt reads the clone task's checkout and commit and writes the agent's prompt with a
// line naming them; the agent task's prompt template waits for that variable. It returns the commit.
func (adapter *Adapter) deliverPrompt(ctx context.Context, allocID string, cri bool, path string) (string, error) {
	alloc, _, err := adapter.client.Allocations().Info(allocID, (&api.QueryOptions{}).WithContext(ctx))
	if err != nil {
		return "", err
	}
	var output []byte
	frames, errs := adapter.logs(ctx, alloc, gitTaskName, false, 0)
	for done := false; !done; {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case err := <-errs:
			if err != nil {
				return "", err
			}
		case frame, ok := <-frames:
			if !ok {
				done = true
				break
			}
			output = append(output, frame.Data...)
		}
	}
	lines := make(chan wherry.Line, bytes.Count(output, []byte("\n"))+1)
	reader := lineReader{lines: lines, cri: cri}
	if err := reader.take(ctx, output); err != nil {
		return "", err
	}
	if err := reader.flush(ctx); err != nil {
		return "", err
	}
	close(lines)
	var printed []string
	for line := range lines {
		printed = append(printed, string(line.Data))
	}
	if len(printed) < 2 || !commit.MatchString(printed[len(printed)-1]) {
		return "", wherry.ErrGitContext
	}
	location, checkedOut := printed[len(printed)-2], printed[len(printed)-1]
	variable, _, err := adapter.client.Variables().Read(path, (&api.QueryOptions{}).WithContext(ctx))
	if err != nil {
		return "", redacted("read", path, err)
	}
	gitPath := path + gitSuffix
	git, _, err := adapter.client.Variables().Read(gitPath, (&api.QueryOptions{}).WithContext(ctx))
	if err != nil {
		return "", redacted("read", gitPath, err)
	}
	prompt := fmt.Sprintf("%s\nThe git repository %s at %s is checked out in %s at commit %s.", variable.Items["prompt"], git.Items["git_url"], git.Items["git_ref"], location, checkedOut)
	agentPath := path + promptSuffix
	if _, _, err := adapter.client.Variables().Update(&api.Variable{Namespace: adapter.namespace, Path: agentPath, Items: map[string]string{"prompt": prompt}}, (&api.WriteOptions{}).WithContext(ctx)); err != nil {
		return "", redacted("write", agentPath, err)
	}
	return checkedOut, nil
}

// buildJob takes nothing from the task except the run ID and the matched offer.
func (adapter *Adapter) buildJob(run wherry.Run, path string) (*api.Job, error) {
	runtime := adapter.runtime
	jobID := adapter.jobID(run.ID, run.Turn)
	env := maps.Clone(runtime.Env)
	if env == nil {
		env = make(map[string]string)
	}
	env["HOME"] = "${NOMAD_TASK_DIR}"
	env["WHERRY_INPUT_DIR"] = "${NOMAD_TASK_DIR}/inputs"
	env["WHERRY_OUTPUT_DIR"] = "${NOMAD_TASK_DIR}/outputs"
	env["WHERRY_CREDENTIALS_DIR"] = "${NOMAD_SECRETS_DIR}/credentials"
	promptPath := path
	if run.Git != nil {
		promptPath = path + promptSuffix
	}
	prompt := `{{ .prompt }}`
	if !slices.Contains(isolated, runtime.Driver) {
		prompt = `{{ .prompt.Value | replaceAll "${NOMAD_TASK_DIR}" (env "NOMAD_TASK_DIR") | replaceAll "${NOMAD_SECRETS_DIR}" (env "NOMAD_SECRETS_DIR") }}`
	}
	templates := []*api.Template{
		{EmbeddedTmpl: new(`{{ with nomadVar "` + promptPath + `" }}` + prompt + `{{ end }}`), DestPath: new("local/prompt.txt"), ChangeMode: new("noop")},
	}
	if run.Credential != nil {
		templates = append(templates, &api.Template{
			EmbeddedTmpl: new(`{{ with nomadVar "` + path + `" }}` + adapter.credentialEnv + `={{ .credential }}{{ end }}`),
			DestPath:     new("${NOMAD_SECRETS_DIR}/wherry.env"), Envvars: new(true), ChangeMode: new("noop"),
		})
	}
	// The core admits only names of letters, digits, '.', '_' and '-', so none ends a template string or path.
	for _, input := range run.Inputs {
		templates = append(templates, &api.Template{
			EmbeddedTmpl: new(`{{ with nomadVar "` + path + inputsSuffix + `" }}{{ (index . "` + input.Name + `").Value | base64Decode }}{{ end }}`),
			DestPath:     new("local/inputs/" + input.Name), ChangeMode: new("noop"),
		})
	}
	for _, secret := range run.Credentials {
		templates = append(templates, &api.Template{
			EmbeddedTmpl: new(`{{ with nomadVar "` + path + `" }}{{ (index . "` + credentialItem(secret.File) + `").Value | base64Decode }}{{ end }}`),
			DestPath:     new("${NOMAD_SECRETS_DIR}/credentials/" + secret.File), ChangeMode: new("noop"),
		})
	}
	tasks := []*api.Task{{
		Name: taskName, Driver: runtime.Driver, Config: maps.Clone(runtime.Config), Env: env,
		Resources: &api.Resources{CPU: runtime.Resources.CPU, MemoryMB: runtime.Resources.MemoryMB},
		LogConfig: &api.LogConfig{MaxFiles: new(3), MaxFileSizeMB: new(10)},
		Templates: templates,
	}}
	if run.Git != nil {
		tasks = append(tasks, gitTask(runtime, path+gitSuffix))
	}
	return &api.Job{
		ID: new(jobID), Name: new(jobID), Namespace: new(adapter.namespace), Type: new(api.JobTypeBatch),
		NodePool: nodePool(adapter.nodePool), Datacenters: adapter.datacenters,
		Meta: map[string]string{
			"run_id": run.ID, "turn": strconv.FormatInt(run.Turn, 10), "attempt": strconv.FormatInt(run.Attempt, 10),
			"adapter": run.Adapter, "agent": run.Agent,
		},
		TaskGroups: []*api.TaskGroup{{
			Name:             new(taskName),
			ReschedulePolicy: &api.ReschedulePolicy{Attempts: new(0), Unlimited: new(false)},
			RestartPolicy:    &api.RestartPolicy{Attempts: new(0), Mode: new(api.RestartPolicyModeFail)},
			Tasks:            tasks,
		}},
	}, nil
}

func nodePool(name string) *string {
	if name == "" {
		return nil
	}
	return &name
}

// gitTask is the prestart task that clones the run's git context into the shared alloc directory;
// repository access and host trust come from the platform.
func gitTask(runtime Runtime, gitPath string) *api.Task {
	config := maps.Clone(runtime.Config)
	if runtime.Driver == "podman" {
		delete(config, "command")
		config["entrypoint"] = []any{"/bin/sh", "-c"}
		config["args"] = []any{cloneScript}
	} else {
		config["command"] = "/bin/sh"
		config["args"] = []any{"-c", cloneScript}
	}
	env := maps.Clone(runtime.Env)
	if env == nil {
		env = make(map[string]string)
	}
	env["HOME"] = "${NOMAD_TASK_DIR}"
	env["GIT_TERMINAL_PROMPT"] = "0"
	env["GIT_SSH_COMMAND"] = "ssh -o StrictHostKeyChecking=yes -o BatchMode=yes"
	templates := []*api.Template{{
		EmbeddedTmpl: new(`{{ with nomadVar "` + gitPath + `" }}{{ .git_url }}` + "\n" + `{{ .git_ref }}` + "\n" + `{{ .git_path }}` + "\n" + `{{ end }}`),
		DestPath:     new("local/git"), ChangeMode: new("noop"),
	}}
	return &api.Task{
		Name: gitTaskName, Driver: runtime.Driver, Config: config, Env: env,
		Lifecycle: &api.TaskLifecycle{Hook: api.TaskLifecycleHookPrestart},
		Resources: &api.Resources{CPU: runtime.Resources.CPU, MemoryMB: runtime.Resources.MemoryMB},
		LogConfig: &api.LogConfig{MaxFiles: new(3), MaxFileSizeMB: new(10)},
		Templates: templates,
	}
}

func (adapter *Adapter) jobID(runID string, turn int64) string {
	return fmt.Sprintf("%s%s-t%d", adapter.prefix, runID, turn)
}

func (adapter *Adapter) variablePath(runID string, turn int64) string {
	return jobVariablePrefix + adapter.jobID(runID, turn)
}

func notFound(err error) bool {
	var response api.UnexpectedResponseError
	return errors.As(err, &response) && response.StatusCode() == 404
}

func (adapter *Adapter) deregisterTimedOut(ctx context.Context, jobID string) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupWait)
	defer cancel()
	_, _, err := adapter.client.Jobs().Deregister(jobID, true, (&api.WriteOptions{}).WithContext(cleanupCtx))
	if err != nil {
		return errors.Join(ctx.Err(), fmt.Errorf("deregister timed-out job: %w", err))
	}
	return ctx.Err()
}

// current prefers a non-terminal allocation, else the newest.
func current(allocs []*api.AllocationListStub) *api.AllocationListStub {
	var newest *api.AllocationListStub
	for _, alloc := range allocs {
		if !terminal(alloc.ClientStatus) {
			return alloc
		}
		if newest == nil || alloc.CreateIndex > newest.CreateIndex {
			newest = alloc
		}
	}
	return newest
}

func terminal(status string) bool {
	switch status {
	case api.AllocClientStatusComplete, api.AllocClientStatusFailed, api.AllocClientStatusLost:
		return true
	}
	return false
}

// started reads StartedAt as well as the Started event, since Nomad keeps only a task's latest events.
func started(state *api.TaskState) bool {
	return state != nil && (!state.StartedAt.IsZero() || slices.ContainsFunc(state.Events, func(event *api.TaskEvent) bool { return event.Type == api.TaskStarted }))
}

// failureClasses maps the task events that end a task before it starts to Terminal.Class.
var failureClasses = map[string]string{
	api.TaskDriverFailure:    "driver",
	api.TaskSetupFailure:     "setup",
	api.TaskFailedValidation: "validation",
}

// terminalOf reads the agent task's final state; the class is that of its last failure event.
func terminalOf(alloc *api.Allocation) wherry.Terminal {
	state := alloc.TaskStates[taskName]
	result := wherry.Terminal{Started: started(state), Node: alloc.NodeName}
	if state == nil {
		return result
	}
	for _, event := range state.Events {
		if class, ok := failureClasses[event.Type]; ok && !result.Started {
			result.Class = class
		}
		if event.Type == api.TaskTerminated {
			result.ExitCode = event.ExitCode
			if event.Signal != 0 {
				result.Signal = strconv.Itoa(event.Signal)
			}
		}
	}
	return result
}

// Follow streams stdout lines from a byte offset until the task ends, then returns its final state.
func (adapter *Adapter) Follow(ctx context.Context, encoded wherry.Handle, offset int64, lines chan<- wherry.Line) (wherry.Terminal, error) {
	handle, err := decodeHandle(encoded)
	if err != nil {
		return wherry.Terminal{}, err
	}
	alloc, _, err := adapter.client.Allocations().Info(handle.Alloc, (&api.QueryOptions{}).WithContext(ctx))
	if err != nil {
		return wherry.Terminal{}, err
	}
	// The logs API answers 404 for a task that never started.
	if alloc.ClientTerminalStatus() && !started(alloc.TaskStates[taskName]) {
		return terminalOf(alloc), nil
	}
	reader := lineReader{lines: lines, position: offset, cri: handle.Format == formatCRI}
	streamCtx, stopStream := context.WithCancel(ctx)
	defer stopStream()
	frames, errs := adapter.logs(streamCtx, alloc, taskName, true, offset)
	// A followed stream can stay open with no frames after the task ends, can close before the task ends,
	// and can lag a task that ended before it opened; completion is polled instead.
	poll := time.NewTicker(followPoll)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return wherry.Terminal{}, ctx.Err()
		case err := <-errs:
			return wherry.Terminal{}, err
		case <-poll.C:
			if final := adapter.finished(ctx, handle.Alloc); final != nil {
				stopStream()
				if err := adapter.drain(ctx, final, &reader); err != nil {
					return wherry.Terminal{}, err
				}
				return terminalOf(final), nil
			}
		case frame, ok := <-frames:
			if !ok {
				frames = nil
				continue
			}
			if err := reader.take(ctx, frame.Data); err != nil {
				return wherry.Terminal{}, err
			}
		}
	}
}

// drain re-reads the finished task's log from the last line boundary to its end;
// a non-follow read is finite, so nothing written before exit can be missed.
func (adapter *Adapter) drain(ctx context.Context, alloc *api.Allocation, reader *lineReader) error {
	reader.pending, reader.partial, reader.consumed = nil, nil, 0
	frames, errs := adapter.logs(ctx, alloc, taskName, false, reader.position)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errs:
			if err != nil {
				return err
			}
		case frame, ok := <-frames:
			if !ok {
				return reader.flush(ctx)
			}
			if err := reader.take(ctx, frame.Data); err != nil {
				return err
			}
		}
	}
}

func (adapter *Adapter) logs(ctx context.Context, alloc *api.Allocation, task string, follow bool, offset int64) (<-chan *api.StreamFrame, <-chan error) {
	cancel := make(chan struct{})
	go func() { <-ctx.Done(); close(cancel) }()
	return adapter.client.AllocFS().Logs(alloc, follow, task, "stdout", api.OriginStart, offset, cancel, (&api.QueryOptions{}).WithContext(ctx))
}

// lineReader splits log bytes into lines; position is the offset after the last emitted line.
type lineReader struct {
	lines    chan<- wherry.Line
	position int64
	pending  []byte
	cri      bool
	// partial holds CRI "P" parts not yet closed by an "F" part; position stays at their start.
	partial  []byte
	consumed int64
}

func (reader *lineReader) take(ctx context.Context, data []byte) error {
	reader.pending = append(reader.pending, data...)
	for {
		end := bytes.IndexByte(reader.pending, '\n')
		if end < 0 && len(reader.pending) < maxPending {
			return nil
		}
		size, skip := end, 1
		if end < 0 {
			size, skip = len(reader.pending), 0 // over-long line: split rather than grow without bound
		}
		if err := reader.raw(ctx, reader.pending[:size], int64(size+skip)); err != nil {
			return err
		}
		reader.pending = reader.pending[size+skip:]
	}
}

// raw handles one log line occupying advance bytes, decoding CRI framing when enabled.
func (reader *lineReader) raw(ctx context.Context, data []byte, advance int64) error {
	if !reader.cri {
		return reader.emit(ctx, data, advance)
	}
	fields := bytes.SplitN(data, []byte(" "), 4)
	if len(fields) != 4 || (string(fields[2]) != "P" && string(fields[2]) != "F") {
		if reader.consumed > 0 {
			if err := reader.emitPartial(ctx); err != nil {
				return err
			}
		}
		return reader.emit(ctx, data, advance)
	}
	reader.partial = append(reader.partial, fields[3]...)
	reader.consumed += advance
	if len(reader.partial) >= maxPending {
		return reader.emitPartial(ctx)
	}
	if string(fields[2]) == "P" {
		return nil
	}
	return reader.emitPartial(ctx)
}

func (reader *lineReader) flush(ctx context.Context) error {
	if reader.consumed > 0 {
		if err := reader.emitPartial(ctx); err != nil {
			return err
		}
	}
	if len(reader.pending) == 0 {
		return nil
	}
	err := reader.emit(ctx, reader.pending, int64(len(reader.pending)))
	reader.pending = nil
	return err
}

func (reader *lineReader) emitPartial(ctx context.Context) error {
	content, total := reader.partial, reader.consumed
	reader.partial, reader.consumed = nil, 0
	return reader.emit(ctx, content, total)
}

func (reader *lineReader) emit(ctx context.Context, data []byte, advance int64) error {
	line := wherry.Line{Offset: reader.position, Next: reader.position + advance, Data: append([]byte(nil), data...)}
	select {
	case reader.lines <- line:
		reader.position = line.Next
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// finished returns the allocation once it is terminal, else nil.
func (adapter *Adapter) finished(ctx context.Context, allocID string) *api.Allocation {
	alloc, _, err := adapter.client.Allocations().Info(allocID, (&api.QueryOptions{}).WithContext(ctx))
	if err != nil || !alloc.ClientTerminalStatus() {
		return nil
	}
	return alloc
}

// Stop sends SIGTERM and stops the job if the task outlives the grace period;
// stopping a batch allocation instead would migrate it to a replacement.
// Requires the alloc-lifecycle and submit-job capabilities.
func (adapter *Adapter) Stop(ctx context.Context, encoded wherry.Handle) error {
	handle, err := decodeHandle(encoded)
	if err != nil {
		return err
	}
	alloc, _, err := adapter.client.Allocations().Info(handle.Alloc, (&api.QueryOptions{}).WithContext(ctx))
	if err != nil {
		return err
	}
	if alloc.ClientTerminalStatus() {
		return nil
	}
	if err := adapter.client.Allocations().Signal(alloc, (&api.QueryOptions{}).WithContext(ctx), taskName, "SIGTERM"); err == nil {
		for deadline := time.Now().Add(stopGrace); time.Now().Before(deadline); time.Sleep(time.Second) {
			if adapter.finished(ctx, handle.Alloc) != nil {
				return nil
			}
		}
	}
	_, _, err = adapter.client.Jobs().Deregister(handle.Job, false, (&api.WriteOptions{}).WithContext(ctx))
	return err
}

// Release deletes the per-turn variables holding the credentials, the prompt, the inputs and the git context.
func (adapter *Adapter) Release(ctx context.Context, runID string, turn int64) error {
	return adapter.release(ctx, adapter.jobID(runID, turn))
}

func (adapter *Adapter) release(ctx context.Context, jobID string) error {
	path := jobVariablePrefix + jobID
	return errors.Join(adapter.deleteVariable(ctx, path), adapter.deleteVariable(ctx, path+promptSuffix), adapter.deleteVariable(ctx, path+gitSuffix),
		adapter.deleteVariable(ctx, path+inputsSuffix))
}

// Collect waits until the allocation is terminal, for as long as ctx allows, then lists the outputs directory through
// the allocation filesystem API. The listing's modes are lstat's, so a link is never followed; the API's stat and
// read follow links and block on a FIFO, so only regular files within the limits are read, and only after the whole
// tree is listed and counted.
func (adapter *Adapter) Collect(ctx context.Context, encoded wherry.Handle, limits wherry.Limits, outputs chan<- wherry.Output) error {
	handle, err := decodeHandle(encoded)
	if err != nil {
		return err
	}
	alloc, _, err := adapter.client.Allocations().Info(handle.Alloc, (&api.QueryOptions{}).WithContext(ctx))
	for err == nil && !alloc.ClientTerminalStatus() {
		select {
		case <-ctx.Done():
			return errors.New("the allocation is still running")
		case <-time.After(collectPoll):
		}
		alloc, _, err = adapter.client.Allocations().Info(handle.Alloc, (&api.QueryOptions{}).WithContext(ctx))
	}
	if err != nil {
		return err
	}
	// A lost allocation's client is gone, and its filesystem with it.
	if alloc.ClientStatus == api.AllocClientStatusLost {
		return errors.New("the allocation was lost")
	}
	var found []wherry.Output
	pending, listed := []string{""}, 0
	for len(pending) > 0 {
		dir := pending[0]
		pending = pending[1:]
		entries, _, err := adapter.client.AllocFS().List(alloc, path.Join(outputsDir, dir), (&api.QueryOptions{}).WithContext(ctx))
		if notFound(err) && dir == "" {
			return nil
		}
		if err != nil {
			return redacted("list", outputsDir, err)
		}
		for _, entry := range entries {
			name, mode := path.Join(dir, entry.Name), fileMode(entry.FileMode)
			if listed++; listed > limits.Files {
				return fmt.Errorf("the outputs directory holds more than %d entries", limits.Files)
			}
			if mode.IsDir() {
				pending = append(pending, name)
				continue
			}
			found = append(found, wherry.Output{Name: name, Size: entry.Size, Mode: mode.Type()})
		}
	}
	slices.SortFunc(found, func(a, b wherry.Output) int { return strings.Compare(a.Name, b.Name) })
	var total int64
	for _, output := range found {
		if output.Mode == 0 && output.Size <= limits.FileBytes && total+output.Size <= limits.TotalBytes {
			total += output.Size
			if output.Data, err = adapter.read(ctx, alloc, output.Name, output.Size); err != nil {
				return err
			}
		}
		select {
		case outputs <- output:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// read reads a listed regular file of size bytes; the API answers some failures with an error text as the body, so a
// body of any other length fails the read.
func (adapter *Adapter) read(ctx context.Context, alloc *api.Allocation, name string, size int64) ([]byte, error) {
	if size == 0 {
		return []byte{}, nil
	}
	body, err := adapter.client.AllocFS().ReadAt(alloc, path.Join(outputsDir, name), 0, size, (&api.QueryOptions{}).WithContext(ctx))
	if err != nil {
		return nil, redacted("read", outputsDir, err)
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, size+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != size {
		return nil, errors.New("an output read back at another length than its listing")
	}
	return data, nil
}

// fileMode parses the type letters Go's fs.FileMode.String writes before the permission bits, as the allocation
// filesystem API reports them; an unparsable mode is irregular.
func fileMode(text string) fs.FileMode {
	if len(text) < 9 {
		return fs.ModeIrregular
	}
	var mode fs.FileMode
	for _, letter := range text[:len(text)-9] {
		switch letter {
		case 'd':
			mode |= fs.ModeDir
		case 'L':
			mode |= fs.ModeSymlink
		case 'D':
			mode |= fs.ModeDevice
		case 'p':
			mode |= fs.ModeNamedPipe
		case 'S':
			mode |= fs.ModeSocket
		case 'c':
			mode |= fs.ModeCharDevice
		case '?':
			mode |= fs.ModeIrregular
		}
	}
	return mode
}

// Purge deregisters the turn's job with purge, then deletes its variables.
func (adapter *Adapter) Purge(ctx context.Context, runID string, turn int64) error {
	return adapter.purge(ctx, adapter.jobID(runID, turn))
}

func (adapter *Adapter) purge(ctx context.Context, jobID string) error {
	if _, _, err := adapter.client.Jobs().Deregister(jobID, true, (&api.WriteOptions{}).WithContext(ctx)); err != nil && !notFound(err) {
		return err
	}
	return adapter.release(ctx, jobID)
}

// Sweep purges agent jobs dead for the grace period, and deletes per-run variables whose job has ended,
// been stopped, or never registered.
func (adapter *Adapter) Sweep(ctx context.Context) error {
	jobs, _, err := adapter.client.Jobs().List((&api.QueryOptions{Prefix: adapter.prefix}).WithContext(ctx))
	if err != nil {
		return redacted("list jobs", adapter.prefix, err)
	}
	var errs []error
	for _, job := range jobs {
		if job.Status != "dead" || !adapter.agentJob.MatchString(job.ID) {
			continue
		}
		allocs, _, err := adapter.client.Jobs().Allocations(job.ID, true, (&api.QueryOptions{}).WithContext(ctx))
		if err != nil {
			errs = append(errs, redacted("list allocations of", job.ID, err))
			continue
		}
		// An adopter may still read a dead job's log, so its age runs from its last allocation change.
		ended := job.SubmitTime
		for _, alloc := range allocs {
			ended = max(ended, alloc.ModifyTime)
		}
		if time.Since(time.Unix(0, ended)) >= sweepGrace {
			errs = append(errs, adapter.purge(ctx, job.ID))
		}
	}
	prefix := jobVariablePrefix + adapter.prefix
	variables, _, err := adapter.client.Variables().PrefixList(prefix, (&api.QueryOptions{}).WithContext(ctx))
	if err != nil {
		return errors.Join(append(errs, redacted("list", prefix, err))...)
	}
	for _, variable := range variables {
		match := adapter.runVariable.FindStringSubmatch(variable.Path)
		if match == nil {
			continue
		}
		job, _, err := adapter.client.Jobs().Info(match[1], (&api.QueryOptions{}).WithContext(ctx))
		switch {
		case notFound(err):
			if time.Since(time.Unix(0, variable.ModifyTime)) < sweepGrace {
				continue
			}
		case err != nil:
			errs = append(errs, redacted("sweep", variable.Path, err))
			continue
		case *job.Status != "dead" && !*job.Stop:
			continue
		}
		errs = append(errs, adapter.deleteVariable(ctx, variable.Path))
	}
	return errors.Join(errs...)
}

func (adapter *Adapter) deleteVariable(ctx context.Context, path string) error {
	if _, err := adapter.client.Variables().Delete(path, (&api.WriteOptions{}).WithContext(ctx)); err != nil && !errors.Is(err, api.ErrVariablePathNotFound) && !notFound(err) {
		return redacted("delete", path, err)
	}
	return nil
}

// redacted keeps response bodies, which may echo variable items, out of errors.
func redacted(op, path string, err error) error {
	var response api.UnexpectedResponseError
	if errors.As(err, &response) {
		return fmt.Errorf("%s %s: status %d", op, path, response.StatusCode())
	}
	return fmt.Errorf("%s %s failed", op, path)
}
