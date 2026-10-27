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
	"net"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/benemon/wherry/pluginsdk"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/api"
)

const (
	jobPrefix         = "agent-"
	jobVariablePrefix = "nomad/jobs/"
	taskName          = "agent"
	formatCRI         = "cri"
	maxPending        = 1 << 20
	// maxVariable is the size Nomad allows a variable's item keys and values together, measured on Nomad 2.0.7.
	maxVariable = 64 << 10
	outputsDir  = taskName + "/local/outputs"
	stopGrace   = 15 * time.Second
	followPoll  = time.Second
	// maxBrokenStreams bounds how often a running task's followed log stream is opened again after it breaks.
	maxBrokenStreams = 5
	// collectPoll separates the reads of an allocation that Collect waits on, since Stop returns before it ends.
	collectPoll = 500 * time.Millisecond
	cleanupWait = 10 * time.Second
)

// inputsSuffix turns a run variable path into its group's variable path, which holds a run's inputs: Nomad's
// implicit workload policy reads the group's variable, and no deeper path, beside the job's own.
const inputsSuffix = "/" + taskName

// credentialItem is the run variable's item holding the value of a credential, keyed by its target.
func credentialItem(secret pluginsdk.Secret) string {
	if secret.Env != "" {
		return "env/" + secret.Env
	}
	return "credentials/" + secret.File
}

// Config is the plugin's config with the plugin profile's merged onto it and its credential key removed.
type Config struct {
	Address   string  `json:"address"`
	CAFile    string  `json:"ca_file"`
	Namespace string  `json:"namespace"`
	Runtime   Runtime `json:"runtime"`
	// NodePool and Datacenters bound where Nomad may place the profile's
	// jobs; unset, Nomad's defaults apply.
	NodePool    string   `json:"node_pool,omitzero"`
	Datacenters []string `json:"datacenters,omitzero"`
}

// Runtime defines the agent task the plugin launches.
type Runtime struct {
	Driver              string            `json:"driver"`
	Config              map[string]any    `json:"config"`
	Env                 map[string]string `json:"env"`
	Resources           Resources         `json:"resources"`
	User                string            `json:"user"`
	Group               string            `json:"group"`
	SupplementaryGroups []string          `json:"supplementary_groups"`
}

// Resources are the agent task's resources; unset, CPU is defaultCPU MHz and memory defaultMemoryMB.
type Resources struct {
	CPU      *int `json:"cpu"`
	MemoryMB *int `json:"memory_mb"`
}

// Nomad registers per-run agent jobs in one namespace.
type Nomad struct {
	// name is the profile's name, which its jobs' profile meta records.
	name        string
	client      *api.Client
	namespace   string
	runtime     Runtime
	nodePool    string
	datacenters []string
	// uid and gid own the task's credential files when the runtime's user is numeric.
	uid, gid *int
	// prefix starts every job ID of the profile's pool; agentJob matches those IDs and runVariable the variables of those
	// jobs. A job is an agent job only with run_id meta, since a run ID is any DNS label.
	prefix      string
	agentJob    *regexp.Regexp
	runVariable *regexp.Regexp
	metrics     metrics
	logger      hclog.Logger
}

// metrics holds the values Metrics reports: API errors by class, sweep deletions by kind, the dead jobs the latest sweep
// left, and placement latency over pluginsdk.PlacementBuckets.
type metrics struct {
	mu        sync.Mutex
	errors    map[string]float64
	deletions map[string]float64
	pending   float64
	placement pluginsdk.Histogram
}

// inlineLimit is the inline input bytes Dispatch accepts: maxVariable holds the inputs' names and base64 values, so
// raw bytes take three quarters of it, less 4 KiB for the names. Dispatch keeps its check for longer names.
const inlineLimit = maxVariable*3/4 - 4<<10

// InlineLimit reports inlineLimit.
func (profile *Nomad) InlineLimit() int64 { return inlineLimit }

// defaultCPU and defaultMemoryMB match the Kubernetes plugin's defaults: 2 GiB, and about half a core of a 2 GHz node.
const (
	defaultCPU      = 1000
	defaultMemoryMB = 2048
)

var (
	envName     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	runtimeUser = regexp.MustCompile(`^(?:[a-z_][a-z0-9_-]*|([1-9][0-9]*)(?::([1-9][0-9]*))?)$`)
	groupName   = regexp.MustCompile(`^[a-z_][a-z0-9_-]*$`)
	// sweepGrace covers Dispatch's window between writing the variable and registering the job.
	sweepGrace = 10 * time.Minute
)

// validGroup reports whether value is a group name or a positive 32-bit GID, other than root.
func validGroup(value string) bool {
	_, err := strconv.ParseUint(value, 10, 32)
	return groupName.MatchString(value) && value != "root" || err == nil && value[0] != '0'
}

// newClient builds a Nomad client for config's address, CA and namespace with token.
func newClient(config Config, token string) (*api.Client, error) {
	apiConfig := api.DefaultConfig()
	apiConfig.Address = config.Address
	apiConfig.Namespace = config.Namespace
	apiConfig.SecretID = token
	apiConfig.TLSConfig = &api.TLSConfig{CACert: config.CAFile}
	return api.NewClient(apiConfig)
}

// New builds the plugin profile name, for a pool's jobs on client.
func New(config Config, name, pool string, client *api.Client, logger hclog.Logger) (*Nomad, error) {
	for variable := range config.Runtime.Env {
		if !envName.MatchString(variable) {
			return nil, errors.New("runtime env names must be environment variable names")
		}
	}
	if config.Runtime.Driver == "" {
		return nil, errors.New("runtime requires a driver")
	}
	var uid, gid *int
	if user := config.Runtime.User; user != "" {
		match := runtimeUser.FindStringSubmatch(user)
		if match == nil || user == "root" {
			return nil, errors.New("runtime user must be a user name or UID[:GID] other than root")
		}
		// The client resolves a name, so the plugin can give credential files only a numeric owner.
		if match[1] != "" {
			id, err := strconv.ParseUint(match[1], 10, 32)
			group, groupErr := strconv.ParseUint(cmp.Or(match[2], match[1]), 10, 32)
			if err != nil || groupErr != nil {
				return nil, errors.New("runtime user IDs must fit 32 bits")
			}
			u, g := int(id), int(group)
			uid, gid = &u, &g
		}
	}
	group, groups := config.Runtime.Group, config.Runtime.SupplementaryGroups
	if group != "" && !validGroup(group) || slices.ContainsFunc(groups, func(g string) bool { return !validGroup(g) }) {
		return nil, errors.New("runtime group and supplementary_groups must be group names or GIDs other than root")
	}
	// docker and podman take the primary group as the GID part of the task user, where a group without a user runs as
	// UID 0; docker alone sets supplementary groups, through group_add. exec, raw_exec and java set no group.
	switch driver := config.Runtime.Driver; {
	case group != "" && driver != "docker" && driver != "podman":
		return nil, errors.New("runtime group cannot be set on the " + driver + " driver")
	case len(groups) > 0 && driver != "docker":
		return nil, errors.New("runtime supplementary_groups cannot be set on the " + driver + " driver")
	case len(groups) > 0 && config.Runtime.Config["group_add"] != nil:
		return nil, errors.New("runtime supplementary_groups must be unset when the docker config sets group_add")
	case group == "":
	case config.Runtime.User == "":
		return nil, errors.New("runtime group needs runtime user on the " + driver + " driver")
	case strings.Contains(config.Runtime.User, ":"):
		return nil, errors.New("runtime group must be unset when runtime user names a GID")
	default:
		if id, err := strconv.Atoi(group); err == nil && gid != nil {
			gid = &id
		}
		config.Runtime.User += ":" + group
	}
	datacenters := config.Datacenters
	if len(datacenters) == 0 {
		datacenters = []string{"*"}
	}
	// Job IDs carry a digest of the pool, since a full 63-character pool name and run ID would pass Nomad's
	// 128-character variable path limit.
	sum := sha256.Sum256([]byte(pool))
	prefix := jobPrefix + hex.EncodeToString(sum[:])[:12] + "-"
	return &Nomad{
		name: name, client: client, namespace: config.Namespace, runtime: config.Runtime, nodePool: config.NodePool, datacenters: datacenters,
		uid: uid, gid: gid,
		prefix:      prefix,
		agentJob:    regexp.MustCompile(`^` + regexp.QuoteMeta(prefix) + `.+$`),
		runVariable: regexp.MustCompile(`^` + regexp.QuoteMeta(jobVariablePrefix) + `(` + regexp.QuoteMeta(prefix) + `.+?)(` + regexp.QuoteMeta(inputsSuffix) + `)?$`),
		metrics:     metrics{errors: map[string]float64{}, deletions: map[string]float64{}, placement: pluginsdk.Histogram{Buckets: make([]uint64, len(pluginsdk.PlacementBuckets))}},
		logger:      logger,
	}, nil
}

// isolated are the drivers whose task sees its task directory at /local and its secrets at /secrets.
var isolated = []string{"docker", "exec", "java", "podman"}

// Directories reports /local and /secrets paths for a driver that isolates the task's filesystem. Any other driver's
// task sees host paths that depend on its allocation, so the plugin reports Nomad's variables for them, which the
// prompt's template resolves from the task's environment.
func (profile *Nomad) Directories() pluginsdk.Directories {
	task, secrets := "${NOMAD_TASK_DIR}", "${NOMAD_SECRETS_DIR}"
	if slices.Contains(isolated, profile.runtime.Driver) {
		task, secrets = "/local", "/secrets"
	}
	return pluginsdk.Directories{Input: task + "/inputs", Output: task + "/outputs", Credentials: secrets + "/credentials"}
}

// Ready reports whether some ready, eligible node in the profile's node pool
// and datacenters has the runtime's driver healthy.
func (profile *Nomad) Ready(ctx context.Context) (bool, error) {
	nodes, _, err := profile.client.Nodes().List((&api.QueryOptions{}).WithContext(ctx))
	if err != nil {
		return false, profile.failed("list nodes", err)
	}
	for _, node := range nodes {
		if node.Status != api.NodeStatusReady || node.SchedulingEligibility != api.NodeSchedulingEligible {
			continue
		}
		// A job without a node pool lands in Nomad's default pool.
		if pool := cmp.Or(profile.nodePool, "default"); node.NodePool != pool {
			continue
		}
		if !slices.Contains(profile.datacenters, "*") && !slices.Contains(profile.datacenters, node.Datacenter) {
			continue
		}
		if info := node.Drivers[profile.runtime.Driver]; info != nil && info.Detected && info.Healthy {
			return true, nil
		}
	}
	return false, nil
}

// Dispatch starts one run idempotently and returns once its task has started or ended. It removes the
// run's material only when Info confirms that no job exists. Inputs and task credentials travel base64-encoded,
// which the templates decode, so binary files arrive intact.
func (profile *Nomad) Dispatch(ctx context.Context, run pluginsdk.Run) (pluginsdk.Outcome, pluginsdk.Handle, error) {
	began := time.Now()
	path := profile.variablePath(run.ID)
	job, err := profile.buildJob(run, path)
	if err != nil {
		return pluginsdk.OutcomeNothingCreated, pluginsdk.Handle{}, err
	}
	inputs, size := map[string]string{}, 0
	for _, input := range run.Inputs {
		inputs[input.Name] = base64.StdEncoding.EncodeToString(input.Data)
		if size += len(input.Name) + len(inputs[input.Name]); size > maxVariable {
			return pluginsdk.OutcomeNothingCreated, pluginsdk.Handle{}, &pluginsdk.InputTooLargeError{Name: input.Name}
		}
	}
	jobID := *job.ID
	items := map[string]string{"prompt": string(run.Prompt)}
	for _, secret := range run.Credentials {
		items[credentialItem(secret)] = base64.StdEncoding.EncodeToString(secret.Value)
	}
	variable := &api.Variable{Namespace: profile.namespace, Path: path, Items: items}
	if _, _, err := profile.client.Variables().Update(variable, (&api.WriteOptions{}).WithContext(ctx)); err != nil {
		return profile.notStarted(ctx, run, profile.failed("write variable "+path, err))
	}
	if len(inputs) > 0 {
		inputsPath := path + inputsSuffix
		if _, _, err := profile.client.Variables().Update(&api.Variable{Namespace: profile.namespace, Path: inputsPath, Items: inputs}, (&api.WriteOptions{}).WithContext(ctx)); err != nil {
			return profile.notStarted(ctx, run, profile.failed("write variable "+inputsPath, err))
		}
	}
	registered := false
	if _, _, err := profile.client.Jobs().Info(jobID, (&api.QueryOptions{}).WithContext(ctx)); err != nil {
		if !notFound(err) {
			return pluginsdk.OutcomeUnknown, pluginsdk.Handle{}, profile.failed("read job", err)
		}
		if _, _, err := profile.client.Jobs().RegisterOpts(job, &api.RegisterOptions{EnforceIndex: true, ModifyIndex: 0}, (&api.WriteOptions{}).WithContext(ctx)); err != nil {
			return profile.notStarted(ctx, run, profile.failed("register job "+jobID, err))
		}
		registered = true
	}
	handle, state, err := profile.wait(ctx, jobID, job.TaskGroups[0].Tasks[0].Driver)
	if err != nil {
		return pluginsdk.OutcomeUnknown, pluginsdk.Handle{}, err
	}
	// Placement runs from this dispatch only for a job it registered, to when wait sees the agent task started.
	if registered && state != pluginsdk.JobNeverStarted {
		profile.metrics.placed(time.Since(began).Seconds())
	}
	return pluginsdk.OutcomeCreated, handle, nil
}

// notStarted returns OutcomeNothingCreated, with the turn's material released, when Info finds no job;
// otherwise OutcomeUnknown with cause and everything left in place. A failed release is an error, which
// the core resolves with Lookup.
func (profile *Nomad) notStarted(ctx context.Context, run pluginsdk.Run, cause error) (pluginsdk.Outcome, pluginsdk.Handle, error) {
	if _, _, err := profile.client.Jobs().Info(profile.jobID(run.ID), (&api.QueryOptions{}).WithContext(ctx)); !notFound(err) {
		profile.handled("read job", err)
		return pluginsdk.OutcomeUnknown, pluginsdk.Handle{}, cause
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupWait)
	defer cancel()
	return pluginsdk.OutcomeNothingCreated, pluginsdk.Handle{}, profile.Release(cleanupCtx, run.ID)
}

// Lookup finds a run's existing job and waits for it as Dispatch does.
func (profile *Nomad) Lookup(ctx context.Context, runID string) (pluginsdk.Job, error) {
	job, _, err := profile.client.Jobs().Info(profile.jobID(runID), (&api.QueryOptions{}).WithContext(ctx))
	if notFound(err) {
		return pluginsdk.Job{}, nil
	}
	if err != nil {
		return pluginsdk.Job{}, profile.failed("read job", err)
	}
	if len(job.TaskGroups) != 1 || len(job.TaskGroups[0].Tasks) != 1 || job.TaskGroups[0].Tasks[0].Name != taskName {
		return pluginsdk.Job{}, errors.New("existing job is not an agent job")
	}
	handle, state, err := profile.wait(ctx, *job.ID, job.TaskGroups[0].Tasks[0].Driver)
	if err != nil {
		return pluginsdk.Job{}, err
	}
	// A job registered without the key reads as attempt 0.
	attempt, _ := strconv.ParseInt(job.Meta["attempt"], 10, 64)
	return pluginsdk.Job{State: state, Handle: handle, Attempt: attempt, Profile: job.Meta["profile"], Agent: job.Meta["agent"]}, nil
}

// handle is the version 1 encoding of pluginsdk.Handle.Data.
type handle struct {
	V      int    `json:"v"`
	Job    string `json:"job"`
	Alloc  string `json:"alloc"`
	Format string `json:"format"`
}

func decodeHandle(encoded pluginsdk.Handle) (handle, error) {
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
func (profile *Nomad) wait(ctx context.Context, jobID, driver string) (pluginsdk.Handle, pluginsdk.JobState, error) {
	format := ""
	if driver == "podman" {
		format = formatCRI
	}
	for {
		allocs, _, err := profile.client.Jobs().Allocations(jobID, false, (&api.QueryOptions{}).WithContext(ctx))
		if err != nil {
			if ctx.Err() != nil {
				return pluginsdk.Handle{}, 0, profile.deregisterTimedOut(ctx, jobID)
			}
			return pluginsdk.Handle{}, 0, profile.failed("list allocations", err)
		}
		alloc := current(allocs)
		// The logs API answers 404 until the task has started.
		if alloc != nil && (terminal(alloc.ClientStatus) || started(alloc.TaskStates[taskName])) {
			data, _ := json.Marshal(handle{V: 1, Job: jobID, Alloc: alloc.ID, Format: format}) // strings always encode
			return pluginsdk.Handle{Data: data, Name: jobID}, jobState(alloc), nil
		}
		select {
		case <-ctx.Done():
			return pluginsdk.Handle{}, 0, profile.deregisterTimedOut(ctx, jobID)
		case <-time.After(2 * time.Second):
		}
	}
}

// jobState keys never-started on a terminal allocation whose agent task has no Started event.
func jobState(alloc *api.AllocationListStub) pluginsdk.JobState {
	switch {
	case !terminal(alloc.ClientStatus):
		return pluginsdk.JobRunning
	case started(alloc.TaskStates[taskName]):
		return pluginsdk.JobEnded
	}
	return pluginsdk.JobNeverStarted
}

// buildJob builds the run's batch job, whose templates read the run's material from the variables at path.
func (profile *Nomad) buildJob(run pluginsdk.Run, path string) (*api.Job, error) {
	runtime := profile.runtime
	jobID := profile.jobID(run.ID)
	env := pluginsdk.AgentEnv(runtime.Env)
	env["HOME"] = "${NOMAD_TASK_DIR}"
	env["WHERRY_INPUT_DIR"] = "${NOMAD_TASK_DIR}/inputs"
	env["WHERRY_OUTPUT_DIR"] = "${NOMAD_TASK_DIR}/outputs"
	env["WHERRY_CREDENTIALS_DIR"] = "${NOMAD_SECRETS_DIR}/credentials"
	if run.Model != "" {
		env["WHERRY_MODEL"] = run.Model
	}
	prompt := `{{ .prompt }}`
	if !slices.Contains(isolated, runtime.Driver) {
		prompt = `{{ .prompt.Value | replaceAll "${NOMAD_TASK_DIR}" (env "NOMAD_TASK_DIR") | replaceAll "${NOMAD_SECRETS_DIR}" (env "NOMAD_SECRETS_DIR") }}`
	}
	templates := []*api.Template{
		{EmbeddedTmpl: new(`{{ with nomadVar "` + path + `" }}` + prompt + `{{ end }}`), DestPath: new("local/prompt.txt"), ChangeMode: new("noop")},
	}
	var envs []string
	for _, secret := range run.Credentials {
		if secret.Env != "" {
			delete(env, secret.Env)
			envs = append(envs, secret.Env+`={{ (index . "`+credentialItem(secret)+`").Value | base64Decode }}`)
		}
	}
	if len(envs) > 0 {
		templates = append(templates, &api.Template{
			EmbeddedTmpl: new(`{{ with nomadVar "` + path + `" }}` + strings.Join(envs, "\n") + "\n{{ end }}"),
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
		if secret.File == "" {
			continue
		}
		template := &api.Template{
			EmbeddedTmpl: new(`{{ with nomadVar "` + path + `" }}{{ (index . "` + credentialItem(secret) + `").Value | base64Decode }}{{ end }}`),
			DestPath:     new("${NOMAD_SECRETS_DIR}/credentials/" + secret.File), ChangeMode: new("noop"),
		}
		if profile.uid != nil {
			template.Uid, template.Gid, template.Perms = profile.uid, profile.gid, new("0600")
		}
		templates = append(templates, template)
	}
	// go-getter follows redirects and Nomad 2.0.7 has no option that stops it, so the checksum alone holds a
	// redirected fetch to the object. archive=false keeps it from unpacking an object whose name ends like an archive.
	var artifacts []*api.TaskArtifact
	for _, fetch := range run.Fetches {
		artifacts = append(artifacts, &api.TaskArtifact{
			GetterSource: new(fetch.URL), GetterOptions: map[string]string{"checksum": "sha256:" + fetch.SHA256, "archive": "false"},
			GetterMode: new("file"), RelativeDest: new("local/inputs/" + fetch.Name),
		})
	}
	taskConfig := maps.Clone(runtime.Config)
	if groups := runtime.SupplementaryGroups; len(groups) > 0 {
		taskConfig = map[string]any{"group_add": groups}
		maps.Copy(taskConfig, runtime.Config)
	}
	tasks := []*api.Task{{
		Name: taskName, Driver: runtime.Driver, User: runtime.User, Config: taskConfig, Env: env,
		Resources: &api.Resources{CPU: cmp.Or(runtime.Resources.CPU, new(defaultCPU)), MemoryMB: cmp.Or(runtime.Resources.MemoryMB, new(defaultMemoryMB))},
		LogConfig: &api.LogConfig{MaxFiles: new(3), MaxFileSizeMB: new(10)},
		Templates: templates, Artifacts: artifacts,
	}}
	return &api.Job{
		ID: new(jobID), Name: new(jobID), Namespace: new(profile.namespace), Type: new(api.JobTypeBatch),
		NodePool: nodePool(profile.nodePool), Datacenters: profile.datacenters,
		Meta: map[string]string{
			"run_id": run.ID, "attempt": strconv.FormatInt(run.Attempt, 10),
			"profile": run.Profile, "agent": run.Agent,
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

func (profile *Nomad) jobID(runID string) string {
	return profile.prefix + runID
}

func (profile *Nomad) variablePath(runID string) string {
	return jobVariablePrefix + profile.jobID(runID)
}

func notFound(err error) bool {
	var response api.UnexpectedResponseError
	return errors.As(err, &response) && response.StatusCode() == 404
}

func (profile *Nomad) deregisterTimedOut(ctx context.Context, jobID string) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupWait)
	defer cancel()
	_, _, err := profile.client.Jobs().Deregister(jobID, true, (&api.WriteOptions{}).WithContext(cleanupCtx))
	if err != nil {
		return errors.Join(ctx.Err(), fmt.Errorf("deregister timed-out job: %w", profile.failed("deregister job", err)))
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
	api.TaskDriverFailure:          "driver",
	api.TaskSetupFailure:           "setup",
	api.TaskFailedValidation:       "validation",
	api.TaskArtifactDownloadFailed: pluginsdk.ClassFetch,
}

// terminalOf reads the agent task's final state; the class is that of its last failure event, and the reason that
// event's message.
func terminalOf(alloc *api.Allocation) pluginsdk.Terminal {
	state := alloc.TaskStates[taskName]
	result := pluginsdk.Terminal{Started: started(state), Node: alloc.NodeName}
	if state == nil {
		return result
	}
	for _, event := range state.Events {
		if class, ok := failureClasses[event.Type]; ok && !result.Started {
			result.Class = class
			result.Reason = cmp.Or(event.DisplayMessage, event.ValidationError, event.SetupError, event.DriverError, event.DownloadError)
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
func (profile *Nomad) Follow(ctx context.Context, encoded pluginsdk.Handle, offset int64, lines chan<- pluginsdk.Line) (pluginsdk.Terminal, error) {
	handle, err := decodeHandle(encoded)
	if err != nil {
		return pluginsdk.Terminal{}, err
	}
	alloc, _, err := profile.client.Allocations().Info(handle.Alloc, (&api.QueryOptions{}).WithContext(ctx))
	if err != nil {
		return pluginsdk.Terminal{}, profile.failed("read allocation", err)
	}
	// The logs API answers 404 for a task that never started.
	if alloc.ClientTerminalStatus() && !started(alloc.TaskStates[taskName]) {
		return terminalOf(alloc), nil
	}
	reader := lineReader{lines: lines, position: offset, cri: handle.Format == formatCRI}
	stream := profile.follow(ctx, alloc, offset)
	defer func() { stream.stop() }()
	// A followed stream can stay open with no frames after the task ends, can close before the task ends,
	// and can lag a task that ended before it opened; completion is polled instead.
	poll := time.NewTicker(followPoll)
	defer poll.Stop()
	// A long-lived stream can be cut mid-frame by a proxy or load balancer; it is opened again from the last line
	// boundary while the task runs, and a finished task's log is read whole instead.
	broken := 0
	for {
		select {
		case <-ctx.Done():
			return pluginsdk.Terminal{}, ctx.Err()
		case err := <-stream.errs:
			stream.stop()
			if ctx.Err() != nil {
				return pluginsdk.Terminal{}, ctx.Err()
			}
			if final := profile.finished(ctx, handle.Alloc); final != nil {
				if err := profile.drain(ctx, final, &reader); err != nil {
					return pluginsdk.Terminal{}, err
				}
				return terminalOf(final), nil
			}
			if broken++; broken > maxBrokenStreams {
				return pluginsdk.Terminal{}, profile.failed("read logs", logsError(err))
			}
			reader.pending, reader.partial, reader.consumed = nil, nil, 0
			select {
			case <-ctx.Done():
				return pluginsdk.Terminal{}, ctx.Err()
			case <-time.After(followPoll):
			}
			stream = profile.follow(ctx, alloc, reader.position)
		case <-poll.C:
			if final := profile.finished(ctx, handle.Alloc); final != nil {
				stream.stop()
				if err := profile.drain(ctx, final, &reader); err != nil {
					return pluginsdk.Terminal{}, err
				}
				return terminalOf(final), nil
			}
		case frame, ok := <-stream.frames:
			if !ok {
				stream.frames = nil
				continue
			}
			if err := reader.take(ctx, frame.Data); err != nil {
				return pluginsdk.Terminal{}, err
			}
		}
	}
}

// logStream is one followed read of the task's log.
type logStream struct {
	frames <-chan *api.StreamFrame
	errs   <-chan error
	stop   context.CancelFunc
}

func (profile *Nomad) follow(ctx context.Context, alloc *api.Allocation, offset int64) logStream {
	streamCtx, stop := context.WithCancel(ctx)
	frames, errs := profile.logs(streamCtx, alloc, taskName, true, offset)
	return logStream{frames: frames, errs: errs, stop: stop}
}

// drain re-reads the finished task's log from the last line boundary to its end;
// a non-follow read is finite, so nothing written before exit can be missed.
func (profile *Nomad) drain(ctx context.Context, alloc *api.Allocation, reader *lineReader) error {
	reader.pending, reader.partial, reader.consumed = nil, nil, 0
	frames, errs := profile.logs(ctx, alloc, taskName, false, reader.position)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errs:
			if err != nil {
				return profile.failed("read logs", logsError(err))
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

// logsError keeps a log stream's response, network or context error; any other quotes the stream's undecoded bytes,
// the agent's output, so it becomes a fixed error.
func logsError(err error) error {
	var response api.UnexpectedResponseError
	var network net.Error
	if errors.As(err, &response) || errors.As(err, &network) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return errors.New("log stream not decodable")
}

func (profile *Nomad) logs(ctx context.Context, alloc *api.Allocation, task string, follow bool, offset int64) (<-chan *api.StreamFrame, <-chan error) {
	cancel := make(chan struct{})
	go func() { <-ctx.Done(); close(cancel) }()
	return profile.client.AllocFS().Logs(alloc, follow, task, "stdout", api.OriginStart, offset, cancel, (&api.QueryOptions{}).WithContext(ctx))
}

// lineReader splits log bytes into lines; position is the offset after the last emitted line.
type lineReader struct {
	lines    chan<- pluginsdk.Line
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
	line := pluginsdk.Line{Offset: reader.position, Next: reader.position + advance, Data: append([]byte(nil), data...)}
	select {
	case reader.lines <- line:
		reader.position = line.Next
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// finished returns the allocation once it is terminal, else nil.
func (profile *Nomad) finished(ctx context.Context, allocID string) *api.Allocation {
	alloc, _, err := profile.client.Allocations().Info(allocID, (&api.QueryOptions{}).WithContext(ctx))
	if err != nil || !alloc.ClientTerminalStatus() {
		profile.handled("read allocation", err)
		return nil
	}
	return alloc
}

// Stop sends SIGTERM and stops the job if the task outlives the grace period;
// stopping a batch allocation instead would migrate it to a replacement.
// Requires the alloc-lifecycle and submit-job capabilities.
func (profile *Nomad) Stop(ctx context.Context, encoded pluginsdk.Handle) error {
	handle, err := decodeHandle(encoded)
	if err != nil {
		return err
	}
	alloc, _, err := profile.client.Allocations().Info(handle.Alloc, (&api.QueryOptions{}).WithContext(ctx))
	// A purged or garbage-collected allocation has stopped.
	if notFound(err) {
		return nil
	}
	if err != nil {
		return profile.failed("read allocation", err)
	}
	if alloc.ClientTerminalStatus() {
		return nil
	}
	// Nomad refuses to signal a task that has just exited, and the server learns of the exit only when the client
	// next reports, so a refused signal waits for the allocation like a delivered one.
	signalErr := profile.client.Allocations().Signal(alloc, (&api.QueryOptions{}).WithContext(ctx), taskName, "SIGTERM")
	poll := time.NewTicker(time.Second)
	defer poll.Stop()
	for deadline := time.Now().Add(stopGrace); time.Now().Before(deadline); {
		if profile.finished(ctx, handle.Alloc) != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-poll.C:
		}
	}
	if signalErr != nil {
		profile.handled("signal allocation", signalErr)
	}
	_, _, err = profile.client.Jobs().Deregister(handle.Job, false, (&api.WriteOptions{}).WithContext(ctx))
	return profile.failed("deregister job", err)
}

// Release deletes the run's variables holding the credentials, the prompt and the inputs.
func (profile *Nomad) Release(ctx context.Context, runID string) error {
	return profile.release(ctx, profile.jobID(runID))
}

func (profile *Nomad) release(ctx context.Context, jobID string) error {
	path := jobVariablePrefix + jobID
	return joined([]error{profile.deleteVariable(ctx, path), profile.deleteVariable(ctx, path+inputsSuffix)})
}

// joined returns the one non-nil error in errs as it is, so it reaches the receiver with its op and class, and joins
// several.
func joined(errs []error) error {
	errs = slices.DeleteFunc(errs, func(err error) bool { return err == nil })
	if len(errs) == 1 {
		return errs[0]
	}
	return errors.Join(errs...)
}

// Collect lists the outputs directory once the allocation is terminal. The listing's modes are lstat's, and the API's
// stat and read follow links and block on a FIFO, so it reads only listed regular files within the limits, after
// counting the whole tree.
func (profile *Nomad) Collect(ctx context.Context, encoded pluginsdk.Handle, limits pluginsdk.Limits, outputs chan<- pluginsdk.Output) error {
	handle, err := decodeHandle(encoded)
	if err != nil {
		return err
	}
	alloc, _, err := profile.client.Allocations().Info(handle.Alloc, (&api.QueryOptions{}).WithContext(ctx))
	for err == nil && !alloc.ClientTerminalStatus() {
		select {
		case <-ctx.Done():
			return errors.New("the allocation is still running")
		case <-time.After(collectPoll):
		}
		alloc, _, err = profile.client.Allocations().Info(handle.Alloc, (&api.QueryOptions{}).WithContext(ctx))
	}
	if err != nil {
		return profile.failed("read allocation", err)
	}
	// A lost allocation's client is gone, and its filesystem with it.
	if alloc.ClientStatus == api.AllocClientStatusLost {
		return errors.New("the allocation was lost")
	}
	var found []pluginsdk.Output
	pending, listed := []string{""}, 0
	for len(pending) > 0 {
		dir := pending[0]
		pending = pending[1:]
		entries, _, err := profile.client.AllocFS().List(alloc, path.Join(outputsDir, dir), (&api.QueryOptions{}).WithContext(ctx))
		if notFound(err) && dir == "" {
			return nil
		}
		if err != nil {
			return profile.failed("list outputs "+outputsDir, err)
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
			found = append(found, pluginsdk.Output{Name: name, Size: entry.Size, Mode: mode.Type()})
		}
	}
	slices.SortFunc(found, func(a, b pluginsdk.Output) int { return strings.Compare(a.Name, b.Name) })
	var total int64
	for _, output := range found {
		switch {
		case output.Mode == 0 && slices.Contains(limits.Stream, output.Name):
			if output.Size <= limits.StreamBytes {
				if output.Body, err = profile.open(ctx, alloc, output.Name, output.Size); err != nil {
					return err
				}
			}
		case output.Mode == 0 && output.Size <= limits.FileBytes && total+output.Size <= limits.TotalBytes:
			total += output.Size
			if output.Data, err = profile.read(ctx, alloc, output.Name, output.Size); err != nil {
				return err
			}
		}
		select {
		case outputs <- output:
		case <-ctx.Done():
			if output.Body != nil {
				output.Body.Close()
			}
			return ctx.Err()
		}
	}
	return nil
}

// read reads a listed regular file of size bytes; the API answers some failures with an error text as the body, so a
// body of any other length fails the read.
func (profile *Nomad) read(ctx context.Context, alloc *api.Allocation, name string, size int64) ([]byte, error) {
	if size == 0 {
		return []byte{}, nil
	}
	body, err := profile.client.AllocFS().ReadAt(alloc, path.Join(outputsDir, name), 0, size, (&api.QueryOptions{}).WithContext(ctx))
	if err != nil {
		return nil, profile.failed("read output "+outputsDir, err)
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

// open streams the first size bytes of a listed regular file; the receiver of the Output checks the length it reads.
func (profile *Nomad) open(ctx context.Context, alloc *api.Allocation, name string, size int64) (io.ReadCloser, error) {
	if size == 0 {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	body, err := profile.client.AllocFS().ReadAt(alloc, path.Join(outputsDir, name), 0, size, (&api.QueryOptions{}).WithContext(ctx))
	if err != nil {
		return nil, profile.failed("read output "+outputsDir, err)
	}
	return body, nil
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

// Purge deregisters the run's job with purge, then deletes its variables.
func (profile *Nomad) Purge(ctx context.Context, runID string) error {
	return profile.purge(ctx, profile.jobID(runID))
}

func (profile *Nomad) purge(ctx context.Context, jobID string) error {
	if _, _, err := profile.client.Jobs().Deregister(jobID, true, (&api.WriteOptions{}).WithContext(ctx)); err != nil && !notFound(err) {
		return profile.failed("purge job", err)
	}
	return profile.release(ctx, jobID)
}

// Sweep purges this profile's agent jobs dead for the grace period, and deletes per-run variables whose job is this
// profile's and has ended or been stopped, is any profile's and dead for the grace period, or never registered. Every
// profile of the pool shares the job prefix, so a job's profile meta decides whose it is; variables hold the prompt
// and credentials, so a removed profile's are swept too. Pending jobs and deletions count this profile's jobs only.
func (profile *Nomad) Sweep(ctx context.Context) error {
	jobs, _, err := profile.client.Jobs().ListOptions(&api.JobListOptions{Fields: &api.JobListFields{Meta: true}}, (&api.QueryOptions{Prefix: profile.prefix}).WithContext(ctx))
	if err != nil {
		return profile.failed("list jobs "+profile.prefix, err)
	}
	var errs []error
	pending, purged := 0, 0
	// expired holds other profiles' jobs dead for the grace period.
	expired := map[string]bool{}
	for _, job := range jobs {
		if job.Status != "dead" || !profile.agentJob.MatchString(job.ID) || job.Meta["run_id"] == "" {
			continue
		}
		own := job.Meta["profile"] == profile.name
		if own {
			pending++
		}
		allocs, _, err := profile.client.Jobs().Allocations(job.ID, true, (&api.QueryOptions{}).WithContext(ctx))
		if err != nil {
			errs = append(errs, profile.failed("list allocations of "+job.ID, err))
			continue
		}
		// An adopter may still read a dead job's log, so its age runs from its last allocation change.
		ended := job.SubmitTime
		for _, alloc := range allocs {
			ended = max(ended, alloc.ModifyTime)
		}
		if time.Since(time.Unix(0, ended)) < sweepGrace {
			continue
		}
		if !own {
			expired[job.ID] = true
			continue
		}
		if err := profile.purge(ctx, job.ID); err != nil {
			errs = append(errs, err)
			continue
		}
		pending--
		purged++
	}
	profile.metrics.mu.Lock()
	profile.metrics.pending = float64(pending)
	profile.metrics.deletions["job"] += float64(purged)
	profile.metrics.mu.Unlock()
	prefix := jobVariablePrefix + profile.prefix
	variables, _, err := profile.client.Variables().PrefixList(prefix, (&api.QueryOptions{}).WithContext(ctx))
	if err != nil {
		return joined(append(errs, profile.failed("list variables "+prefix, err)))
	}
	for _, variable := range variables {
		match := profile.runVariable.FindStringSubmatch(variable.Path)
		if match == nil {
			continue
		}
		job, _, err := profile.client.Jobs().Info(match[1], (&api.QueryOptions{}).WithContext(ctx))
		own := true
		switch {
		case notFound(err):
			if time.Since(time.Unix(0, variable.ModifyTime)) < sweepGrace {
				continue
			}
		case err != nil:
			errs = append(errs, profile.failed("sweep read job of "+variable.Path, err))
			continue
		case job.Meta["profile"] != profile.name:
			own = false
			if !expired[match[1]] {
				continue
			}
		case *job.Status != "dead" && !*job.Stop:
			continue
		}
		if err := profile.deleteVariable(ctx, variable.Path); err != nil {
			errs = append(errs, err)
			continue
		}
		if !own {
			continue
		}
		profile.metrics.mu.Lock()
		profile.metrics.deletions["variable"]++
		profile.metrics.mu.Unlock()
	}
	return joined(errs)
}

func (profile *Nomad) deleteVariable(ctx context.Context, path string) error {
	if _, err := profile.client.Variables().Delete(path, (&api.WriteOptions{}).WithContext(ctx)); err != nil && !errors.Is(err, api.ErrVariablePathNotFound) && !notFound(err) {
		return profile.failed("delete variable "+path, err)
	}
	return nil
}

// failed counts err, from the Nomad API call op, by its class and returns it as a pluginsdk.Error, which the receiver
// logs redacted. A not-found answer, which callers read as absence, and a call its caller cancelled count nothing and
// have no class.
func (profile *Nomad) failed(op string, err error) error {
	var response api.UnexpectedResponseError
	var network net.Error
	class := "other"
	switch {
	case err == nil:
		return nil
	case notFound(err) || errors.Is(err, context.Canceled):
		return &pluginsdk.Error{Op: op, Err: err}
	case errors.Is(err, context.DeadlineExceeded):
		class = "timeout"
	case errors.As(err, &response):
		switch code := response.StatusCode(); {
		case code == http.StatusUnauthorized || code == http.StatusForbidden:
			class = "denied"
		case code == http.StatusConflict:
			class = "conflict"
		case code == http.StatusRequestTimeout || code == http.StatusGatewayTimeout:
			class = "timeout"
		case code >= 500:
			class = "unavailable"
		}
	case errors.As(err, &network) && network.Timeout():
		class = "timeout"
	case errors.As(err, &network):
		class = "unavailable"
	}
	profile.metrics.mu.Lock()
	profile.metrics.errors[class]++
	profile.metrics.mu.Unlock()
	return &pluginsdk.Error{Op: op, Class: class, Err: err}
}

// handled counts err as failed does and logs its op and class for an error the plugin handles itself and never
// returns. The line leaves out err's text, which can hold a request URL or a response body.
func (profile *Nomad) handled(op string, err error) {
	var failure *pluginsdk.Error
	if errors.As(profile.failed(op, err), &failure) && failure.Class != "" {
		profile.logger.Warn("nomad call failed", "profile", profile.name, "op", failure.Op, "class", failure.Class)
	}
}

func (m *metrics) placed(seconds float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, bound := range pluginsdk.PlacementBuckets {
		if seconds <= bound {
			m.placement.Buckets[i]++
		}
	}
	m.placement.Sum += seconds
	m.placement.Count++
}

// Metrics reports the profile's API errors, sweep deletions, cleanup-pending jobs and placement latency.
func (profile *Nomad) Metrics(context.Context) ([]pluginsdk.Sample, error) {
	m := &profile.metrics
	m.mu.Lock()
	defer m.mu.Unlock()
	placement := m.placement
	placement.Buckets = slices.Clone(placement.Buckets)
	samples := []pluginsdk.Sample{
		{Family: "wherry_plugin_cleanup_pending", Kind: pluginsdk.KindGauge, Value: m.pending},
		{Family: "wherry_plugin_placement_seconds", Kind: pluginsdk.KindHistogram, Histogram: placement},
	}
	for class, count := range m.errors {
		samples = append(samples, pluginsdk.Sample{Family: "wherry_plugin_platform_errors_total", Labels: map[string]string{"class": class}, Kind: pluginsdk.KindCounter, Value: count})
	}
	for kind, count := range m.deletions {
		samples = append(samples, pluginsdk.Sample{Family: "wherry_plugin_sweep_deletions_total", Labels: map[string]string{"kind": kind}, Kind: pluginsdk.KindCounter, Value: count})
	}
	return samples, nil
}
