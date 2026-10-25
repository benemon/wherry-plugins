//go:build linux

package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	wherry "github.com/benemon/wherry/adapter"
	sd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/godbus/dbus/v5"
	"github.com/hashicorp/go-hclog"
)

const (
	jobPrefix  = "agent-"
	unitPrefix = "wherry-"
	unitSuffix = ".service"
	// maxCredentials is systemd's CREDENTIALS_TOTAL_SIZE_MAX, over IDs and values together, and the size of the tmpfs
	// systemd mounts as a unit's credential store.
	maxCredentials = 1 << 20
	// maxName is NAME_MAX, the longest delivered input name.
	maxName      = 255
	minVersion   = 252
	cgroup2Magic = 0x63677270
	// envCredential starts the store ID of a credential delivered as environment variable.
	envCredential = ".env."
	cleanupWait   = 10 * time.Second
	startTimeout  = 30 * time.Second
	stopTimeout   = 15 * time.Second

	phasePrepared = "prepared"
	phaseStarting = "starting"
	phaseStarted  = "started"

	workPath        = "/wherry"
	inputPath       = workPath + "/inputs"
	outputPath      = workPath + "/outputs"
	credentialsPath = "/run/wherry/credentials"

	busUnitExists = "org.freedesktop.systemd1.UnitExists"
	busNoSuchUnit = "org.freedesktop.systemd1.NoSuchUnit"
)

var (
	envName  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	idName   = regexp.MustCompile(`^[a-z_][a-z0-9_-]*$`)
	idNumber = regexp.MustCompile(`^[1-9][0-9]*$`)
	leading  = regexp.MustCompile(`^[0-9]+`)
	// systemdEnv are the variables systemd sets for the unit, which an Environment= value would override.
	systemdEnv = []string{"CREDENTIALS_DIRECTORY", "STATE_DIRECTORY", "INVOCATION_ID"}
	// exitNames are systemd's own exit statuses from 200, as exit-status.h at v252 names them; 223 and 234 are
	// reserved.
	exitNames = []string{
		"CHDIR", "NICE", "FDS", "EXEC", "MEMORY", "LIMITS", "OOM_ADJUST", "SIGNAL_MASK", "STDIN", "STDOUT",
		"CHROOT", "IOPRIO", "TIMERSLACK", "SECUREBITS", "SETSCHEDULER", "CPUAFFINITY", "GROUP", "USER", "CAPABILITIES", "CGROUP",
		"SETSID", "CONFIRM", "STDERR", "", "PAM", "NETWORK", "NAMESPACE", "NO_NEW_PRIVILEGES", "SECCOMP", "SELINUX_CONTEXT",
		"PERSONALITY", "APPARMOR_PROFILE", "ADDRESS_FAMILIES", "RUNTIME_DIRECTORY", "", "CHOWN", "SMACK_PROCESS_LABEL", "KEYRING", "STATE_DIRECTORY", "CACHE_DIRECTORY",
		"LOGS_DIRECTORY", "CONFIGURATION_DIRECTORY", "NUMA_POLICY", "CREDENTIALS", "BPF",
	}

	// The host paths are variables so the unit tests can root them in temporary directories.
	controlRoot    = "/var/lib/wherry/systemd/runs"
	stateRoot      = "/var/lib"
	credentialRoot = "/run/wherry-systemd"
	cgroupRoot     = "/sys/fs/cgroup"
	rootUID        = 0
	statfs         = syscall.Statfs
	dial           = func(ctx context.Context) (manager, error) { return sd.NewSystemdConnectionContext(ctx) }

	sweepGrace  = 10 * time.Minute
	stopGrace   = 15 * time.Second
	stopPoll    = time.Second
	waitPoll    = 500 * time.Millisecond
	followPoll  = 250 * time.Millisecond
	unitPoll    = time.Second
	collectPoll = 500 * time.Millisecond
	errReplaced = errors.New("the job was replaced")
	errNotAgent = errors.New("existing unit is not an agent unit")
)

// manager is the part of go-systemd's connection to PID 1 the adapter uses.
type manager interface {
	StartTransientUnitContext(ctx context.Context, name, mode string, properties []sd.Property, ch chan<- string) (int, error)
	StopUnitContext(ctx context.Context, name, mode string, ch chan<- string) (int, error)
	KillUnitWithTarget(ctx context.Context, name string, target sd.Who, signal int32) error
	ResetFailedUnitContext(ctx context.Context, name string) error
	GetUnitPropertiesContext(ctx context.Context, unit string) (map[string]any, error)
	GetUnitTypePropertiesContext(ctx context.Context, unit, unitType string) (map[string]any, error)
	SystemStateContext(ctx context.Context) (*sd.Property, error)
	GetManagerProperty(prop string) (string, error)
	Connected() bool
}

// Config is the adapter's section of the receiver configuration.
type Config struct {
	Runtime Runtime `json:"runtime"`
}

// Runtime defines the agent command the adapter runs in each unit.
type Runtime struct {
	Command             []string          `json:"command"`
	Args                []string          `json:"args"`
	Env                 map[string]string `json:"env"`
	Resources           Resources         `json:"resources"`
	User                string            `json:"user"`
	Group               string            `json:"group"`
	SupplementaryGroups []string          `json:"supplementary_groups"`
}

// Resources are the agent unit's limits; unset, the CPU quota is defaultCPUQuota and memory defaultMemoryMB.
type Resources struct {
	CPUQuota *int `json:"cpu_quota"`
	MemoryMB *int `json:"memory_mb"`
}

// defaultCPUQuota and defaultMemoryMB match the Kubernetes plugin's defaults of 500m and 2Gi.
const (
	defaultCPUQuota = 50
	defaultMemoryMB = 2048
)

// Adapter runs each run as one transient service on the receiver's host.
type Adapter struct {
	name, pool, node string
	runtime          Runtime
	user, group      string
	cpuQuota         int
	memoryMB         int
	fetcher          *http.Client
	metrics          *PlatformMetrics
	logger           hclog.Logger

	mu    sync.Mutex
	conn  manager
	locks map[string]*runLock
}

type runLock struct {
	sync.Mutex
	users int
}

// marker is the version 1 encoding of a run's job.json.
type marker struct {
	V            int           `json:"v"`
	Pool         string        `json:"pool"`
	Adapter      string        `json:"adapter"`
	Agent        string        `json:"agent"`
	Run          string        `json:"run"`
	Attempt      int64         `json:"attempt"`
	Instance     string        `json:"instance"`
	Phase        string        `json:"phase"`
	NeverStarted *neverStarted `json:"never_started,omitzero"`
	Terminal     *recorded     `json:"terminal,omitzero"`
}

type neverStarted struct {
	Class  string `json:"class"`
	Reason string `json:"reason"`
}

type recorded struct {
	ExitCode int    `json:"exit_code"`
	Signal   string `json:"signal"`
}

// handle is the version 1 encoding of wherry.Handle.Data.
type handle struct {
	V        int    `json:"v"`
	Unit     string `json:"unit"`
	Instance string `json:"instance"`
}

// unit is what the adapter reads of a unit; timestamps are microseconds since the epoch, 0 when unset.
type unit struct {
	loaded, transient      bool
	active, result, cgroup string
	mainPID                uint32
	code, status           int32
	exited, inactive       uint64
}

// fetchError is a fetch that failed, in words that never quote the URL.
type fetchError struct{ name, reason string }

func (e *fetchError) Error() string { return "fetch " + e.name + ": " + e.reason }

// text reports whether value can be a unit property's string: UTF-8 without NUL.
func text(value string) bool {
	return utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

// validID reports whether value is a user or group name or a positive 32-bit ID, other than root.
func validID(value string) bool {
	if idName.MatchString(value) {
		return value != "root"
	}
	_, err := strconv.ParseUint(value, 10, 32)
	return idNumber.MatchString(value) && err == nil
}

// New builds the adapter named name for a pool's runs, after checking the host it runs on.
func New(config Config, name, pool string, logger hclog.Logger) (*Adapter, error) {
	command := config.Runtime.Command
	switch {
	case len(command) == 0 || !path.IsAbs(command[0]):
		return nil, errors.New("runtime command must start with an absolute path")
	case slices.ContainsFunc(slices.Concat(command, config.Runtime.Args), func(s string) bool { return !text(s) }):
		return nil, errors.New("runtime command and args must be UTF-8 without NUL")
	}
	for variable, value := range config.Runtime.Env {
		switch {
		case !envName.MatchString(variable):
			return nil, errors.New("runtime env names must be environment variable names")
		case slices.Contains(systemdEnv, variable):
			return nil, errors.New("runtime env must not set " + variable + ", which systemd sets")
		case !text(value):
			return nil, errors.New("runtime env values must be UTF-8 without NUL")
		}
	}
	cpuQuota, memoryMB := defaultCPUQuota, defaultMemoryMB
	if quota := config.Runtime.Resources.CPUQuota; quota != nil {
		if *quota < 1 || *quota > 100*runtime.NumCPU() {
			return nil, fmt.Errorf("runtime cpu_quota must be from 1 to %d", 100*runtime.NumCPU())
		}
		cpuQuota = *quota
	}
	if memory := config.Runtime.Resources.MemoryMB; memory != nil {
		if *memory < 64 || *memory > 1<<31-1 {
			return nil, errors.New("runtime memory_mb must be from 64 to 2^31 - 1")
		}
		memoryMB = *memory
	}
	user, group := config.Runtime.User, config.Runtime.Group
	errUser := errors.New("runtime user must be a user name or UID[:GID] other than root")
	if uid, gid, numeric := strings.Cut(user, ":"); numeric {
		if !idNumber.MatchString(uid) || !idNumber.MatchString(gid) || !validID(uid) || !validID(gid) {
			return nil, errUser
		}
		if group != "" {
			return nil, errors.New("runtime group must be unset when runtime user names a GID")
		}
		user, group = uid, gid
	} else if user != "" && !validID(user) {
		return nil, errUser
	}
	if group != "" && !validID(group) || slices.ContainsFunc(config.Runtime.SupplementaryGroups, func(g string) bool { return !validID(g) }) {
		return nil, errors.New("runtime group and supplementary_groups must be group names or GIDs other than root")
	}
	if err := checkHost(); err != nil {
		return nil, err
	}
	node, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("read the host name: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), cleanupWait)
	defer cancel()
	conn, err := dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("connect to systemd: %w", err)
	}
	version, err := conn.GetManagerProperty("Version")
	if err != nil {
		return nil, fmt.Errorf("read the systemd version: %w", err)
	}
	if !supported(version) {
		return nil, errors.New("systemd 252 or later is required")
	}
	return &Adapter{
		name: name, pool: pool, node: node, runtime: config.Runtime,
		user: user, group: group, cpuQuota: cpuQuota, memoryMB: memoryMB,
		fetcher: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		metrics: NewPlatformMetrics(),
		logger:  logger,
		conn:    conn,
		locks:   map[string]*runLock{},
	}, nil
}

// checkHost refuses a plugin that is not root, a cgroup v1 host, and run directories it cannot use.
func checkHost() error {
	if os.Geteuid() != rootUID {
		return errors.New("the systemd plugin runs as root")
	}
	var cgroups syscall.Statfs_t
	if err := statfs(cgroupRoot, &cgroups); err != nil || int64(cgroups.Type) != cgroup2Magic {
		return errors.New("systemd's unified cgroup hierarchy is required")
	}
	// systemd keeps the private parent of dynamic users' directories root 0700.
	if err := os.Mkdir(filepath.Join(stateRoot, "private"), 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("%s cannot be created: %w", filepath.Join(stateRoot, "private"), err)
	}
	for _, dir := range []string{controlRoot, agentRoot(true), agentRoot(false)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("%s cannot be created: %w", dir, err)
		}
		probe, err := os.CreateTemp(dir, ".probe-*")
		if err != nil {
			return fmt.Errorf("%s cannot be written: %w", dir, err)
		}
		probe.Close()
		os.Remove(probe.Name())
	}
	return checkCredentialRoot()
}

// supported reports whether a manager Version, in GVariant text form, starts with a release of minVersion or later.
func supported(version string) bool {
	if unquoted, err := strconv.Unquote(version); err == nil {
		version = unquoted
	}
	release, err := strconv.Atoi(leading.FindString(version))
	return err == nil && release >= minVersion
}

// InlineLimit reports no limit: inputs are files, so only the pool's inline limit and the link bound them.
func (adapter *Adapter) InlineLimit() int64 { return 0 }

// Directories reports the fixed paths each agent unit sees.
func (adapter *Adapter) Directories() wherry.Directories {
	return wherry.Directories{Input: inputPath, Output: outputPath, Credentials: credentialsPath}
}

// jobName is the name of a run's directories.
func jobName(pool, runID string) string {
	sum := sha256.Sum256([]byte(pool))
	return fmt.Sprintf("%s%s-%s", jobPrefix, hex.EncodeToString(sum[:])[:12], runID)
}

func (adapter *Adapter) jobName(runID string) string {
	return jobName(adapter.pool, runID)
}

// unitName is the name of a run's transient unit.
func unitName(job string) string {
	return unitPrefix + job + unitSuffix
}

// agentRoot is the directory of dynamic users' work directories, or of fixed users'.
func agentRoot(dynamic bool) string {
	if dynamic {
		return filepath.Join(stateRoot, "private", "wherry-agents")
	}
	return filepath.Join(stateRoot, "wherry-agents")
}

// workDirs are the paths a run's work directory may have: the private one and systemd's symlink, or a fixed user's.
func workDirs(job string) []string {
	return []string{filepath.Join(agentRoot(true), job), filepath.Join(agentRoot(false), job)}
}

func (adapter *Adapter) workDir(job string) string {
	return filepath.Join(agentRoot(adapter.user == ""), job)
}

func markerPath(job string) string {
	return filepath.Join(controlRoot, job, "job.json")
}

func stdoutPath(job string) string {
	return filepath.Join(controlRoot, job, "stdout")
}

// bus returns the connection to PID 1, dialling again once daemon-reexec or a failure has closed it.
func (adapter *Adapter) bus(ctx context.Context) (manager, error) {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if adapter.conn != nil && adapter.conn.Connected() {
		return adapter.conn, nil
	}
	conn, err := dial(ctx)
	if err != nil {
		return nil, adapter.failed("connect to systemd", err)
	}
	adapter.conn = conn
	return conn, nil
}

// lock takes the lock of a run's job, which Dispatch holds and the sweep skips; without wait it returns false at once
// when the lock is held.
func (adapter *Adapter) lock(job string, wait bool) (func(), bool) {
	adapter.mu.Lock()
	held := adapter.locks[job]
	if held == nil {
		held = &runLock{}
		adapter.locks[job] = held
	}
	held.users++
	adapter.mu.Unlock()
	leave := func() {
		adapter.mu.Lock()
		if held.users--; held.users == 0 {
			delete(adapter.locks, job)
		}
		adapter.mu.Unlock()
	}
	if !wait && !held.TryLock() {
		leave()
		return nil, false
	}
	if wait {
		held.Lock()
	}
	return func() { held.Unlock(); leave() }, true
}

// property is a unit property as StartTransientUnit takes it.
func property(name string, value any) sd.Property {
	return sd.Property{Name: name, Value: dbus.MakeVariant(value)}
}

// execStart, bindPath, loadCredential and addressFamilies are the structs of ExecStartEx, BindPaths, LoadCredential and
// RestrictAddressFamilies.
type execStart struct {
	Path  string
	Args  []string
	Flags []string
}

type bindPath struct {
	Source, Destination string
	IgnoreMissing       bool
	Flags               uint64
}

type loadCredential struct{ ID, Path string }

type addressFamilies struct {
	Allow    bool
	Families []string
}

// wrapper is the shell script that exports each variable's credential from the store and execs the agent; the trailing
// x keeps a credential's trailing newlines.
func wrapper(variables []string) string {
	script := ""
	for _, variable := range variables {
		script += `c=$(cat -- ` + credentialsPath + `/` + envCredential + variable + ` && printf x) || exit; export ` + variable + `="${c%x}"; `
	}
	return script + `exec "$@"`
}

// properties builds the run's unit, whose agent reads its credentials from the files LoadCredential= names.
func (adapter *Adapter) properties(run wherry.Run, job string, credentials []loadCredential) []sd.Property {
	name := unitName(job)
	env := wherry.AgentEnv(adapter.runtime.Env)
	own := []string{"HOME=" + workPath, "WHERRY_INPUT_DIR=" + inputPath, "WHERRY_OUTPUT_DIR=" + outputPath, "WHERRY_CREDENTIALS_DIR=" + credentialsPath}
	if run.Model != "" {
		own = append(own, "WHERRY_MODEL="+run.Model)
	}
	// systemd refuses an Environment= that sets a variable twice, so SHELL is set only where the runtime's env leaves it
	// unset, and the plugin's own variables replace the runtime's.
	var vars []string
	if _, set := env["SHELL"]; !set {
		vars = append(vars, "SHELL=/bin/sh")
	}
	for _, entry := range own {
		variable, _, _ := strings.Cut(entry, "=")
		delete(env, variable)
	}
	for _, variable := range slices.Sorted(maps.Keys(env)) {
		vars = append(vars, variable+"="+env[variable])
	}
	vars = append(vars, own...)
	argv := slices.Concat(adapter.runtime.Command, adapter.runtime.Args)
	var variables []string
	for _, secret := range run.Credentials {
		if secret.Env != "" {
			variables = append(variables, secret.Env)
		}
	}
	if len(variables) > 0 {
		argv = slices.Concat([]string{"/bin/sh", "-c", wrapper(variables), "wherry-agent"}, argv)
	}
	control := filepath.Join(controlRoot, job)
	properties := []sd.Property{
		property("Description", "wherry agent "+run.ID+" (adapter "+run.Adapter+")"),
		property("Slice", "wherry.slice"),
		property("Type", "exec"),
		property("RemainAfterExit", true),
		property("ExecStartEx", []execStart{{Path: argv[0], Args: argv, Flags: []string{"no-env-expand"}}}),
		property("Environment", vars),
		property("WorkingDirectory", workPath),
		property("StandardInput", "null"),
		property("StandardOutputFileToAppend", filepath.Join(control, "stdout")),
		property("StandardErrorFileToAppend", filepath.Join(control, "stderr")),
		property("StateDirectory", []string{"wherry-agents/" + job}),
		property("StateDirectoryMode", uint32(0o700)),
		property("BindPaths", []bindPath{{Source: adapter.workDir(job), Destination: workPath, Flags: syscall.MS_REC}}),
		property("BindReadOnlyPaths", []bindPath{{Source: "/run/credentials/" + name, Destination: credentialsPath, IgnoreMissing: true, Flags: syscall.MS_REC}}),
		property("InaccessiblePaths", []string{"/var/lib/wherry", "-/etc/wherry", "/run/wherry-systemd", "/run/systemd/transient", "-/run/systemd/journal"}),
		// A limit of exactly 1 makes the kernel abort a dump to a pipe handler, which would log the environment.
		property("LimitCORE", uint64(1)),
		property("ProtectSystem", "strict"),
		property("ProtectHome", "yes"),
		property("PrivateTmp", true),
		property("PrivateDevices", true),
		property("PrivateIPC", true),
		property("NoNewPrivileges", true),
		property("RestrictSUIDSGID", true),
		property("ProtectKernelTunables", true),
		property("ProtectKernelModules", true),
		property("ProtectKernelLogs", true),
		property("ProtectControlGroups", true),
		property("ProtectClock", true),
		property("ProtectHostname", true),
		property("ProtectProc", "invisible"),
		property("LockPersonality", true),
		property("RestrictRealtime", true),
		property("RestrictAddressFamilies", addressFamilies{Allow: true, Families: []string{"AF_UNIX", "AF_INET", "AF_INET6", "AF_NETLINK"}}),
		property("SystemCallArchitectures", []string{"native"}),
		property("CapabilityBoundingSet", uint64(0)),
		property("KeyringMode", "private"),
		property("UMask", uint32(0o077)),
		property("CPUQuotaPerSecUSec", uint64(adapter.cpuQuota)*10000),
		property("MemoryMax", uint64(adapter.memoryMB)<<20),
		property("MemorySwapMax", uint64(0)),
		property("KillMode", "control-group"),
		property("TimeoutStartUSec", uint64(startTimeout/time.Microsecond)),
		property("TimeoutStopUSec", uint64(stopTimeout/time.Microsecond)),
		property("OOMPolicy", "stop"),
	}
	if adapter.user == "" {
		properties = append(properties, property("DynamicUser", true), property("RemoveIPC", true))
	} else {
		// RemoveIPC= would remove the IPC objects of every run of a fixed user when one run stops.
		properties = append(properties, property("User", adapter.user))
	}
	if adapter.group != "" {
		properties = append(properties, property("Group", adapter.group))
	}
	if groups := adapter.runtime.SupplementaryGroups; len(groups) > 0 {
		properties = append(properties, property("SupplementaryGroups", groups))
	}
	if len(credentials) > 0 {
		properties = append(properties, property("LoadCredential", credentials))
	}
	return properties
}

// Ready reports whether the manager is running, degraded included, or starting: basic.target is active, which an
// agent unit's default dependencies wait for, though the boot's other jobs have not all ended.
func (adapter *Adapter) Ready(ctx context.Context) (bool, error) {
	bus, err := adapter.bus(ctx)
	if err != nil {
		return false, err
	}
	state, err := bus.SystemStateContext(ctx)
	if err != nil {
		return false, adapter.failed("read system state", err)
	}
	value, _ := state.Value.Value().(string)
	return value == "running" || value == "degraded" || value == "starting", nil
}

// deliverable refuses a run systemd cannot deliver: an input name over NAME_MAX, credentials over systemd's limit, and
// a credential or model that cannot be an environment value.
func deliverable(run wherry.Run) error {
	for _, input := range run.Inputs {
		if len(input.Name) > maxName {
			return &wherry.InputTooLargeError{Name: input.Name}
		}
	}
	for _, fetch := range run.Fetches {
		if len(fetch.Name) > maxName {
			return &wherry.InputTooLargeError{Name: fetch.Name}
		}
	}
	if credentialTotal(run) > maxCredentials {
		return errors.New("the credentials exceed systemd's 1 MiB credential limit")
	}
	for _, secret := range run.Credentials {
		if secret.Env != "" && slices.Contains(secret.Value, 0) {
			return errors.New("credential " + secret.Name + " holds a NUL byte, which an environment variable cannot")
		}
	}
	if !text(run.Model) {
		return errors.New("the model must be UTF-8 without NUL")
	}
	return nil
}

// Dispatch starts a run's unit idempotently and returns once its agent has started or its unit has ended.
func (adapter *Adapter) Dispatch(ctx context.Context, run wherry.Run) (wherry.Outcome, wherry.Handle, error) {
	began := time.Now()
	if err := deliverable(run); err != nil {
		return wherry.OutcomeNothingCreated, wherry.Handle{}, err
	}
	job := adapter.jobName(run.ID)
	unlock, _ := adapter.lock(job, true)
	defer unlock()
	m, err := readMarker(job)
	if err != nil {
		return wherry.OutcomeUnknown, wherry.Handle{}, adapter.failed("read "+markerPath(job), err)
	}
	u, err := adapter.unit(ctx, unitName(job))
	if err != nil {
		return wherry.OutcomeUnknown, wherry.Handle{}, err
	}
	if adapter.foreign(m, u) {
		return wherry.OutcomeUnknown, wherry.Handle{}, errNotAgent
	}
	started := false
	if !u.loaded && (m == nil || m.Phase == phasePrepared && m.NeverStarted == nil) {
		var outcome wherry.Outcome
		if started, outcome, err = adapter.start(ctx, run, job); err != nil {
			return outcome, wherry.Handle{}, err
		}
	}
	handle, _, err := adapter.wait(ctx, job)
	if err != nil {
		return wherry.OutcomeUnknown, wherry.Handle{}, err
	}
	if started {
		adapter.metrics.Placed(time.Since(began).Seconds())
	}
	return wherry.OutcomeCreated, handle, nil
}

// foreign reports whether a run's marker or unit is not this pool's agent unit.
func (adapter *Adapter) foreign(m *marker, u unit) bool {
	return u.loaded && (m == nil || !u.transient) || m != nil && m.Pool != adapter.pool
}

// start writes the run's material over any leftover and starts its unit. It reports whether the start job was done,
// or the outcome and error of a run that never reached its start.
func (adapter *Adapter) start(ctx context.Context, run wherry.Run, job string) (bool, wherry.Outcome, error) {
	name := unitName(job)
	if err := adapter.removeRun(job); err != nil {
		return false, wherry.OutcomeNothingCreated, err
	}
	instance := make([]byte, 16)
	_, _ = rand.Read(instance)
	m := &marker{V: 1, Pool: adapter.pool, Adapter: run.Adapter, Agent: run.Agent, Run: run.ID, Attempt: run.Attempt,
		Instance: hex.EncodeToString(instance), Phase: phasePrepared}
	abandon := func(cause error) (bool, wherry.Outcome, error) {
		if err := adapter.removeRun(job); err != nil {
			return false, wherry.OutcomeNothingCreated, errors.Join(cause, err)
		}
		return false, wherry.OutcomeNothingCreated, cause
	}
	if err := adapter.prepare(job, run, m); err != nil {
		return abandon(err)
	}
	if never, err := adapter.fetchAll(ctx, job, run.Fetches); ctx.Err() != nil {
		return abandon(ctx.Err())
	} else if err != nil {
		return abandon(err)
	} else if never != nil {
		m.NeverStarted = never
		if err := adapter.writeMarker(job, m); err != nil {
			return abandon(err)
		}
		return false, wherry.OutcomeCreated, nil
	}
	credentials, err := writeCredentials(name, run)
	if err != nil {
		return abandon(adapter.failed("write credentials of "+name, err))
	}
	defer func() {
		if err := removeCredentials(name); err != nil {
			adapter.handled("remove credentials of "+name, err)
		}
	}()
	m.Phase = phaseStarting
	if err := adapter.writeMarker(job, m); err != nil {
		return abandon(err)
	}
	bus, err := adapter.bus(ctx)
	if err != nil {
		return abandon(err)
	}
	results := make(chan string, 1)
	_, err = bus.StartTransientUnitContext(ctx, name, "fail", adapter.properties(run, job, credentials), results)
	switch {
	case busName(err) == busUnitExists:
		return false, wherry.OutcomeCreated, nil
	case err != nil:
		return adapter.notStarted(ctx, job, adapter.failed("start unit "+name, err))
	}
	// A connection lost before the job's end leaves the result to the wait, which reads the unit.
	var result string
	for result == "" && ctx.Err() == nil && bus.Connected() {
		select {
		case result = <-results:
		case <-ctx.Done():
		case <-time.After(waitPoll):
		}
	}
	switch result {
	case "":
		return false, wherry.OutcomeCreated, nil
	case "done":
		m.Phase = phaseStarted
		if err := adapter.writeMarker(job, m); err != nil {
			adapter.handled("write "+markerPath(job), err)
		}
		return true, wherry.OutcomeCreated, nil
	case "failed":
		u, err := adapter.unit(ctx, name)
		if err != nil {
			return false, wherry.OutcomeUnknown, err
		}
		m.NeverStarted = &neverStarted{"start", result}
		if class, reason, own := exitClass(u.status); own {
			m.NeverStarted = &neverStarted{class, reason}
		}
	default:
		m.NeverStarted = &neverStarted{"start", result}
	}
	if err := adapter.writeMarker(job, m); err != nil {
		return false, wherry.OutcomeUnknown, err
	}
	return false, wherry.OutcomeCreated, nil
}

// notStarted removes the run's material and returns OutcomeNothingCreated when its unit is not loaded, else
// OutcomeUnknown, with cause.
func (adapter *Adapter) notStarted(ctx context.Context, job string, cause error) (bool, wherry.Outcome, error) {
	if u, err := adapter.unit(ctx, unitName(job)); err != nil || u.loaded {
		return false, wherry.OutcomeUnknown, cause
	}
	if err := adapter.removeRun(job); err != nil {
		return false, wherry.OutcomeNothingCreated, errors.Join(cause, err)
	}
	return false, wherry.OutcomeNothingCreated, cause
}

// exitClass reports the never-started class and reason of one of systemd's own exit statuses.
func exitClass(status int32) (string, string, bool) {
	if status < 200 || int(status) >= 200+len(exitNames) {
		return "", "", false
	}
	reason := "systemd exit status " + strconv.Itoa(int(status))
	if name := exitNames[status-200]; name != "" {
		reason += "/" + name
	}
	switch status {
	case 203:
		return "exec", reason, true
	case 243:
		return "credentials", reason, true
	}
	return "setup", reason, true
}

// prepare writes the marker, the empty logs, the prompt and the inline inputs, each owner-only.
func (adapter *Adapter) prepare(job string, run wherry.Run, m *marker) error {
	control, work := filepath.Join(controlRoot, job), adapter.workDir(job)
	if err := os.Mkdir(control, 0o700); err != nil {
		return adapter.failed("create "+control, err)
	}
	if err := adapter.writeMarker(job, m); err != nil {
		return err
	}
	for _, file := range []string{filepath.Join(control, "stdout"), filepath.Join(control, "stderr")} {
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			return adapter.failed("write "+file, err)
		}
	}
	for _, dir := range []string{work, filepath.Join(work, "inputs")} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return adapter.failed("create "+dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(work, "prompt.txt"), run.Prompt, 0o600); err != nil {
		return adapter.failed("write "+filepath.Join(work, "prompt.txt"), err)
	}
	inputs, err := os.OpenRoot(filepath.Join(work, "inputs"))
	if err != nil {
		return adapter.failed("open "+filepath.Join(work, "inputs"), err)
	}
	defer inputs.Close()
	for _, input := range run.Inputs {
		if err := inputs.WriteFile(input.Name, input.Data, 0o600); err != nil {
			return adapter.failed("write "+filepath.Join(work, "inputs", input.Name), err)
		}
	}
	return nil
}

// fetchAll fetches each S3 input into the run's inputs; a fetch that fails or does not verify, or a local write that
// fails, returns the never-started record it leaves the run with.
func (adapter *Adapter) fetchAll(ctx context.Context, job string, fetches []wherry.Fetch) (*neverStarted, error) {
	if len(fetches) == 0 {
		return nil, nil
	}
	inputs, err := os.OpenRoot(filepath.Join(adapter.workDir(job), "inputs"))
	if err != nil {
		return nil, adapter.failed("open "+filepath.Join(adapter.workDir(job), "inputs"), err)
	}
	defer inputs.Close()
	for _, fetch := range fetches {
		err := adapter.fetch(ctx, inputs, fetch)
		var fetchErr *fetchError
		switch {
		case err == nil:
			continue
		case errors.As(err, &fetchErr):
			adapter.logger.Warn("input not fetched", "adapter", adapter.name, "job", job, "reason", err.Error())
			return &neverStarted{wherry.ClassFetch, "input " + fetch.Name + ": not fetched"}, nil
		}
		var errno syscall.Errno
		reason := "write failed"
		if errors.As(err, &errno) {
			reason = errno.Error()
		}
		return &neverStarted{"setup", "input " + fetch.Name + ": " + reason}, nil
	}
	return nil, nil
}

// fetch reads one input, refusing a redirect and reading at most one byte past its size, into a temporary file it
// renames into place once its size and SHA-256 verify.
func (adapter *Adapter) fetch(ctx context.Context, inputs *os.Root, fetch wherry.Fetch) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fetch.URL, nil)
	if err != nil {
		return &fetchError{fetch.Name, "invalid URL"}
	}
	response, err := adapter.fetcher.Do(request)
	if err != nil {
		var network net.Error
		var certificate *tls.CertificateVerificationError
		reason := "unreachable"
		switch {
		case errors.Is(err, context.DeadlineExceeded) || errors.As(err, &network) && network.Timeout():
			reason = "timeout"
		case errors.As(err, &certificate):
			reason = "certificate"
		}
		return &fetchError{fetch.Name, reason}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return &fetchError{fetch.Name, strconv.Itoa(response.StatusCode)}
	}
	random := make([]byte, 8)
	_, _ = rand.Read(random)
	part := ".fetch-" + hex.EncodeToString(random)
	file, err := inputs.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	digest := sha256.New()
	size, err := io.Copy(io.MultiWriter(file, digest), io.LimitReader(response.Body, fetch.Size+1))
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	var network net.Error
	switch {
	case errors.As(err, &network):
		err = &fetchError{fetch.Name, "unreachable"}
	case err != nil:
	case size != fetch.Size:
		err = &fetchError{fetch.Name, "size"}
	case hex.EncodeToString(digest.Sum(nil)) != fetch.SHA256:
		err = &fetchError{fetch.Name, "digest"}
	default:
		err = inputs.Rename(part, fetch.Name)
	}
	if err != nil {
		inputs.Remove(part)
	}
	return err
}

// readMarker reads a run's marker; an absent marker is nil.
func readMarker(job string) (*marker, error) {
	data, err := os.ReadFile(markerPath(job))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m marker
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%s is not readable", markerPath(job))
	}
	if m.V != 1 {
		return nil, fmt.Errorf("%s has version %d", markerPath(job), m.V)
	}
	return &m, nil
}

func (adapter *Adapter) writeMarker(job string, m *marker) error {
	data, _ := json.Marshal(m) // strings and numbers always encode
	if err := replace(filepath.Join(controlRoot, job), "job.json", data); err != nil {
		return adapter.failed("write "+markerPath(job), err)
	}
	return nil
}

// replace writes data to dir/name through a temporary file and a rename, so a reader sees a whole version.
func replace(dir, name string, data []byte) error {
	file, err := os.CreateTemp(dir, "."+name+"-*")
	if err != nil {
		return err
	}
	_, err = file.Write(data)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(file.Name(), filepath.Join(dir, name))
	}
	if err != nil {
		os.Remove(file.Name())
	}
	return err
}

// removeRun removes a run's marker first, then its work directory, systemd's symlink, its credential files and its
// control directory.
func (adapter *Adapter) removeRun(job string) error {
	if err := os.Remove(markerPath(job)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return adapter.failed("remove "+markerPath(job), err)
	}
	for _, dir := range slices.Concat(workDirs(job), []string{filepath.Join(credentialRoot, unitName(job)), filepath.Join(controlRoot, job)}) {
		if err := os.RemoveAll(dir); err != nil {
			return adapter.failed("remove "+dir, err)
		}
	}
	return nil
}

// unit reads a unit's state; a unit systemd does not know reads as not loaded. A unit that failed to load again, as
// systemd before 254 leaves an agent unit after daemon-reload, still holds its name and its running agent.
func (adapter *Adapter) unit(ctx context.Context, name string) (unit, error) {
	bus, err := adapter.bus(ctx)
	if err != nil {
		return unit{}, err
	}
	props, err := bus.GetUnitPropertiesContext(ctx, name)
	if err != nil {
		return unit{}, adapter.failed("read unit "+name, err)
	}
	u := unit{loaded: props["LoadState"] != "not-found"}
	if !u.loaded {
		return u, nil
	}
	service, err := bus.GetUnitTypePropertiesContext(ctx, name, "Service")
	if err != nil {
		return unit{}, adapter.failed("read unit "+name, err)
	}
	u.transient, _ = props["Transient"].(bool)
	u.active, _ = props["ActiveState"].(string)
	u.inactive, _ = props["InactiveEnterTimestamp"].(uint64)
	u.result, _ = service["Result"].(string)
	u.cgroup, _ = service["ControlGroup"].(string)
	u.mainPID, _ = service["MainPID"].(uint32)
	u.code, _ = service["ExecMainCode"].(int32)
	u.status, _ = service["ExecMainStatus"].(int32)
	u.exited, _ = service["ExecMainExitTimestamp"].(uint64)
	return u, nil
}

// populated reports whether any process remains in a unit's cgroup.
func populated(u unit) (bool, error) {
	if u.cgroup == "" {
		return false, nil
	}
	events, err := os.ReadFile(filepath.Join(cgroupRoot, u.cgroup, "cgroup.events"))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	return !slices.Contains(strings.Split(string(events), "\n"), "populated 0"), nil
}

// empty reports whether a unit is not loaded or holds no process.
func (adapter *Adapter) empty(ctx context.Context, name string) (bool, error) {
	u, err := adapter.unit(ctx, name)
	if err != nil || !u.loaded {
		return err == nil, err
	}
	busy, err := populated(u)
	if err != nil {
		return false, adapter.failed("read cgroup of "+name, err)
	}
	return !busy, nil
}

// judge reads a run's state and terminal from its marker and unit; settled is false while the unit is activating.
func (adapter *Adapter) judge(job string, m *marker, u unit) (wherry.JobState, wherry.Terminal, bool) {
	terminal := wherry.Terminal{Started: true, Node: adapter.node}
	never := func(class, reason string) (wherry.JobState, wherry.Terminal, bool) {
		return wherry.JobNeverStarted, wherry.Terminal{Node: adapter.node, Class: class, Reason: reason}, true
	}
	switch {
	case m == nil || m.Phase == phasePrepared && m.NeverStarted == nil:
		return wherry.JobAbsent, wherry.Terminal{}, true
	case m.NeverStarted != nil:
		return never(m.NeverStarted.Class, m.NeverStarted.Reason)
	case u.active == "activating":
		return 0, wherry.Terminal{}, false
	case u.loaded && u.mainPID != 0:
		return wherry.JobRunning, terminal, true
	case u.loaded && u.exited != 0 && m.Phase == phaseStarting && u.result == "exit-code":
		if class, reason, own := exitClass(u.status); own {
			return never(class, reason)
		}
	}
	switch {
	case u.loaded && u.exited != 0:
		terminal.ExitCode = int(u.status)
		// ExecMainCode is the CLD_* code: 1 exited, 2 killed, 3 dumped.
		if u.code == 2 || u.code == 3 {
			terminal.ExitCode, terminal.Signal = 128+int(u.status), strconv.Itoa(int(u.status))
		}
		return wherry.JobEnded, terminal, true
	case u.loaded:
		return never("start", u.result)
	case m.Phase == phaseStarting && emptyFile(stdoutPath(job)):
		return never("start", "the unit was never created")
	case m.Terminal != nil:
		terminal.ExitCode, terminal.Signal = m.Terminal.ExitCode, m.Terminal.Signal
	default:
		terminal.ExitCode = -1
	}
	return wherry.JobEnded, terminal, true
}

// emptyFile reports whether a file is empty or absent.
func emptyFile(name string) bool {
	info, err := os.Stat(name)
	return err != nil || info.Size() == 0
}

// wait returns once the run's unit is not activating; a wait cut short stops the unit and removes the marker.
func (adapter *Adapter) wait(ctx context.Context, job string) (wherry.Handle, wherry.JobState, error) {
	m, err := readMarker(job)
	if err != nil {
		return wherry.Handle{}, 0, adapter.failed("read "+markerPath(job), err)
	}
	name := unitName(job)
	for {
		u, err := adapter.unit(ctx, name)
		if err != nil {
			if ctx.Err() != nil {
				return wherry.Handle{}, 0, adapter.stopTimedOut(ctx, job)
			}
			return wherry.Handle{}, 0, err
		}
		if state, _, settled := adapter.judge(job, m, u); settled {
			if state == wherry.JobAbsent {
				return wherry.Handle{}, state, nil
			}
			data, _ := json.Marshal(handle{V: 1, Unit: name, Instance: m.Instance}) // strings always encode
			return wherry.Handle{Data: data, Name: name}, state, nil
		}
		select {
		case <-ctx.Done():
			return wherry.Handle{}, 0, adapter.stopTimedOut(ctx, job)
		case <-time.After(waitPoll):
		}
	}
}

func (adapter *Adapter) stopTimedOut(ctx context.Context, job string) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupWait)
	defer cancel()
	err := adapter.halt(cleanupCtx, unitName(job))
	if removeErr := os.Remove(markerPath(job)); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
		err = errors.Join(err, adapter.failed("remove "+markerPath(job), removeErr))
	}
	if err != nil {
		return errors.Join(ctx.Err(), fmt.Errorf("stop timed-out unit: %w", err))
	}
	return ctx.Err()
}

// Lookup finds a run's existing job and waits for it as Dispatch does.
func (adapter *Adapter) Lookup(ctx context.Context, runID string) (wherry.Job, error) {
	job := adapter.jobName(runID)
	m, err := readMarker(job)
	if err != nil {
		return wherry.Job{}, adapter.failed("read "+markerPath(job), err)
	}
	u, err := adapter.unit(ctx, unitName(job))
	if err != nil {
		return wherry.Job{}, err
	}
	if adapter.foreign(m, u) {
		return wherry.Job{}, errNotAgent
	}
	if m == nil {
		return wherry.Job{}, nil
	}
	handle, state, err := adapter.wait(ctx, job)
	if err != nil || state == wherry.JobAbsent {
		return wherry.Job{}, err
	}
	return wherry.Job{State: state, Handle: handle, Attempt: m.Attempt, Adapter: m.Adapter, Agent: m.Agent}, nil
}

// current decodes a handle and reads its run's marker, refusing one a later Dispatch replaced; an absent marker is nil.
func (adapter *Adapter) current(encoded wherry.Handle) (string, *marker, error) {
	var h handle
	if err := json.Unmarshal(encoded.Data, &h); err != nil {
		return "", nil, err
	}
	if h.V != 1 {
		return "", nil, fmt.Errorf("handle version %d", h.V)
	}
	job := strings.TrimSuffix(strings.TrimPrefix(h.Unit, unitPrefix), unitSuffix)
	if !strings.HasPrefix(job, adapter.jobName("")) || strings.ContainsRune(job, '/') || unitName(job) != h.Unit {
		return "", nil, errors.New("the handle names no agent unit of this pool")
	}
	m, err := readMarker(job)
	if err != nil {
		return "", nil, adapter.failed("read "+markerPath(job), err)
	}
	if m != nil && m.Instance != h.Instance {
		return "", nil, errReplaced
	}
	return job, m, nil
}

// Follow streams the agent's stdout lines from a byte offset until its main process has exited, then returns its
// terminal.
func (adapter *Adapter) Follow(ctx context.Context, encoded wherry.Handle, from int64, lines chan<- wherry.Line) (wherry.Terminal, error) {
	job, m, err := adapter.current(encoded)
	if err != nil {
		return wherry.Terminal{}, err
	}
	if m == nil {
		return wherry.Terminal{}, errReplaced
	}
	if m.NeverStarted != nil {
		_, terminal, _ := adapter.judge(job, m, unit{})
		return terminal, nil
	}
	file, err := os.OpenFile(stdoutPath(job), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return wherry.Terminal{}, adapter.failed("open "+stdoutPath(job), err)
	}
	defer file.Close()
	shorter := errors.New("the agent's log is shorter than the offset")
	if _, err := file.Seek(from, io.SeekStart); err != nil {
		return wherry.Terminal{}, err
	}
	reader := LineReader{Lines: lines, Position: from}
	offset, chunk := from, make([]byte, 32<<10)
	drain := func() error {
		for {
			n, err := file.Read(chunk)
			offset += int64(n)
			if n > 0 {
				if err := reader.Take(ctx, chunk[:n]); err != nil {
					return err
				}
			}
			if errors.Is(err, io.EOF) {
				if info, err := file.Stat(); err != nil || info.Size() < offset {
					return shorter
				}
				return nil
			}
			if err != nil {
				return err
			}
		}
	}
	var checked time.Time
	for {
		if err := drain(); err != nil {
			return wherry.Terminal{}, err
		}
		if time.Since(checked) >= unitPoll {
			checked = time.Now()
			u, err := adapter.unit(ctx, unitName(job))
			if err != nil {
				return wherry.Terminal{}, err
			}
			if state, terminal, settled := adapter.judge(job, m, u); settled && state != wherry.JobRunning {
				if err := drain(); err != nil {
					return wherry.Terminal{}, err
				}
				if err := reader.Flush(ctx); err != nil {
					return wherry.Terminal{}, err
				}
				return terminal, nil
			}
		}
		select {
		case <-ctx.Done():
			return wherry.Terminal{}, ctx.Err()
		case <-time.After(followPoll):
		}
	}
}

// Stop signals every process of the unit, SIGTERM then SIGKILL, and never stops the unit, which keeps its exit status.
func (adapter *Adapter) Stop(ctx context.Context, encoded wherry.Handle) error {
	job, m, err := adapter.current(encoded)
	if err != nil || m == nil {
		return err
	}
	name := unitName(job)
	u, err := adapter.unit(ctx, name)
	if err != nil {
		return err
	}
	if state, _, _ := adapter.judge(job, m, u); state == wherry.JobNeverStarted {
		return nil
	}
	return adapter.stop(ctx, name)
}

// stop signals a unit's cgroup until it is empty, and errs when processes outlive SIGKILL.
func (adapter *Adapter) stop(ctx context.Context, name string) error {
	for _, signal := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		if empty, err := adapter.empty(ctx, name); err != nil || empty {
			return err
		}
		bus, err := adapter.bus(ctx)
		if err != nil {
			return err
		}
		if err := bus.KillUnitWithTarget(ctx, name, sd.All, int32(signal)); err != nil {
			// A unit that ended in between refuses the kill.
			if empty, readErr := adapter.empty(ctx, name); readErr == nil && empty {
				return nil
			}
			return adapter.failed("kill unit "+name, err)
		}
		for deadline := time.Now().Add(stopGrace); time.Now().Before(deadline); {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(stopPoll):
			}
			if empty, err := adapter.empty(ctx, name); err != nil {
				adapter.handled("read unit "+name, err)
			} else if empty {
				return nil
			}
		}
	}
	if empty, err := adapter.empty(ctx, name); err != nil || empty {
		return err
	}
	return errors.New("the agent outlived SIGKILL")
}

// Collect reads the outputs tree once no process of the unit is left, and reads what Select chooses.
func (adapter *Adapter) Collect(ctx context.Context, encoded wherry.Handle, limits wherry.Limits, outputs chan<- wherry.Output) error {
	job, m, err := adapter.current(encoded)
	if err != nil {
		return err
	}
	if m == nil {
		return errReplaced
	}
	for {
		empty, err := adapter.empty(ctx, unitName(job))
		if err != nil {
			if ctx.Err() != nil {
				return errors.New("the agent is still running")
			}
			return err
		}
		if empty {
			break
		}
		select {
		case <-ctx.Done():
			return errors.New("the agent is still running")
		case <-time.After(collectPoll):
		}
	}
	dir := filepath.Join(adapter.workDir(job), "outputs")
	root, err := os.OpenRoot(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return adapter.failed("open "+dir, err)
	}
	defer root.Close()
	entries, err := walk(root, limits.Files+1)
	if err != nil {
		return adapter.failed("list "+dir, err)
	}
	selected, err := Select(entries, limits)
	if err != nil {
		return err
	}
	for _, chosen := range selected {
		output := chosen.Output
		switch {
		case chosen.Stream:
			if output.Body, err = open(root, output.Name); err != nil {
				return adapter.failed("open "+filepath.Join(dir, output.Name), err)
			}
		case chosen.Read:
			if output.Data, err = read(root, output.Name, output.Size); err != nil {
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

// walk lists the tree beneath root without following a link, stopping after limit entries.
func walk(root *os.Root, limit int) ([]wherry.Output, error) {
	var entries []wherry.Output
	err := fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || name == "." {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		entries = append(entries, wherry.Output{Name: name, Size: info.Size(), Mode: info.Mode().Type()})
		if len(entries) >= limit {
			return fs.SkipAll
		}
		return nil
	})
	return entries, err
}

// open opens a listed entry only as a regular file: never blocking on a FIFO, and never through a link.
func open(root *os.Root, name string) (*os.File, error) {
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	// os.Root follows a link that stays inside it, so the open file must also be what lstat finds at name.
	info, err := file.Stat()
	listed, lstatErr := root.Lstat(name)
	if err != nil || lstatErr != nil || !info.Mode().IsRegular() || !os.SameFile(info, listed) {
		file.Close()
		return nil, errors.New("output " + name + " is not a regular file")
	}
	return file, nil
}

// read reads a listed regular file and refuses a body of another length than its listing.
func read(root *os.Root, name string, size int64) ([]byte, error) {
	file, err := open(root, name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, size+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != size {
		return nil, errors.New("an output read back at another length than its listing")
	}
	return data, nil
}

// Release settles a run whose agent has ended: it records the outcome, stops the unit, which drops the credential
// store and the dynamic user, and removes the prompt and inputs.
func (adapter *Adapter) Release(ctx context.Context, runID string) error {
	return adapter.release(ctx, adapter.jobName(runID))
}

func (adapter *Adapter) release(ctx context.Context, job string) error {
	name := unitName(job)
	u, err := adapter.unit(ctx, name)
	if err != nil {
		return err
	}
	var unitErr error
	if u.loaded {
		// Stopping the unit releases its dynamic UID, which a later run may take while a process here still holds it.
		if busy, err := populated(u); err != nil || busy {
			if err != nil {
				return adapter.failed("read cgroup of "+name, err)
			}
			return nil
		}
		unitErr = adapter.record(job, u)
		if unitErr == nil {
			unitErr = adapter.halt(ctx, name)
		}
	}
	var removeErr error
	for _, dir := range workDirs(job) {
		for _, entry := range []string{"prompt.txt", "inputs"} {
			if err := os.RemoveAll(filepath.Join(dir, entry)); err != nil && removeErr == nil {
				removeErr = adapter.failed("remove "+filepath.Join(dir, entry), err)
			}
		}
	}
	// A single error is returned as it is, so it reaches the receiver with its op and class.
	switch {
	case unitErr == nil:
		return removeErr
	case removeErr == nil:
		return unitErr
	}
	return errors.Join(unitErr, removeErr)
}

// record writes into the marker how a loaded unit's run ended, unless the marker already holds it.
func (adapter *Adapter) record(job string, u unit) error {
	m, err := readMarker(job)
	if err != nil {
		return adapter.failed("read "+markerPath(job), err)
	}
	if m == nil || m.Terminal != nil || m.NeverStarted != nil {
		return nil
	}
	switch state, terminal, _ := adapter.judge(job, m, u); state {
	case wherry.JobEnded:
		m.Terminal = &recorded{ExitCode: terminal.ExitCode, Signal: terminal.Signal}
	case wherry.JobNeverStarted:
		m.NeverStarted = &neverStarted{terminal.Class, terminal.Reason}
	default:
		return nil
	}
	return adapter.writeMarker(job, m)
}

// halt stops a unit, waiting for the job, and resets it, so a transient unit unloads; an unknown unit is success.
func (adapter *Adapter) halt(ctx context.Context, name string) error {
	bus, err := adapter.bus(ctx)
	if err != nil {
		return err
	}
	results := make(chan string, 1)
	if _, err := bus.StopUnitContext(ctx, name, "replace", results); err != nil && busName(err) != busNoSuchUnit {
		return adapter.failed("stop unit "+name, err)
	} else if err == nil {
		select {
		case <-results:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := bus.ResetFailedUnitContext(ctx, name); err != nil && busName(err) != busNoSuchUnit {
		return adapter.failed("reset unit "+name, err)
	}
	return nil
}

// Purge stops the run's unit, waits for it to unload, then removes its marker and directories.
func (adapter *Adapter) Purge(ctx context.Context, runID string) error {
	return adapter.purge(ctx, adapter.jobName(runID))
}

// purge waits for the unit to unload, since a loaded unit still holds the name.
func (adapter *Adapter) purge(ctx context.Context, job string) error {
	name := unitName(job)
	if err := adapter.halt(ctx, name); err != nil {
		return err
	}
	for {
		u, err := adapter.unit(ctx, name)
		if err != nil {
			return err
		}
		if !u.loaded {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(collectPoll):
		}
	}
	return adapter.removeRun(job)
}

// endOf reports when a run ended: its main process's exit, else the unit's last deactivation, else the later of its
// marker's and stdout's changes.
func endOf(job string, u unit) time.Time {
	for _, at := range []uint64{u.exited, u.inactive} {
		if at != 0 {
			return time.UnixMicro(int64(at))
		}
	}
	var at time.Time
	for _, file := range []string{markerPath(job), stdoutPath(job)} {
		if info, err := os.Stat(file); err == nil && info.ModTime().After(at) {
			at = info.ModTime()
		}
	}
	return at
}

// stale reports whether a path changed more than the sweep grace ago.
func stale(name string) bool {
	info, err := os.Lstat(name)
	return err == nil && time.Since(info.ModTime()) >= sweepGrace
}

// Sweep settles and purges this adapter's ended runs, settles other adapters' expired ones, and removes stale
// material, leftovers and credential files.
func (adapter *Adapter) Sweep(ctx context.Context) error {
	prefix := adapter.jobName("")
	controls, err := os.ReadDir(controlRoot)
	if err != nil {
		return adapter.failed("list "+controlRoot, err)
	}
	var errs []error
	counts := map[string]int{}
	jobs := map[string]bool{}
	for _, entry := range controls {
		job := entry.Name()
		if !strings.HasPrefix(job, prefix) {
			continue
		}
		jobs[job] = true
		unlock, ok := adapter.lock(job, false)
		if !ok {
			continue
		}
		kind, err := adapter.sweepJob(ctx, job)
		unlock()
		counts[kind]++
		if err != nil {
			errs = append(errs, err)
		}
	}
	for _, root := range []string{agentRoot(true), agentRoot(false)} {
		works, err := os.ReadDir(root)
		if err != nil {
			errs = append(errs, adapter.failed("list "+root, err))
			continue
		}
		for _, entry := range works {
			if job := entry.Name(); strings.HasPrefix(job, prefix) && !jobs[job] && stale(filepath.Join(root, job)) {
				if err := os.RemoveAll(filepath.Join(root, job)); err != nil {
					errs = append(errs, adapter.failed("remove "+filepath.Join(root, job), err))
				}
			}
		}
	}
	credentials, err := os.ReadDir(credentialRoot)
	if err != nil {
		errs = append(errs, adapter.failed("list "+credentialRoot, err))
	}
	for _, entry := range credentials {
		name := entry.Name()
		job := strings.TrimSuffix(strings.TrimPrefix(name, unitPrefix), unitSuffix)
		if !strings.HasPrefix(job, prefix) || unitName(job) != name {
			continue
		}
		unlock, ok := adapter.lock(job, false)
		if !ok {
			continue
		}
		if u, err := adapter.unit(ctx, name); err != nil {
			errs = append(errs, err)
		} else if u.active != "activating" {
			if err := removeCredentials(name); err != nil {
				errs = append(errs, adapter.failed("remove credentials of "+name, err))
			}
		}
		unlock()
	}
	adapter.metrics.Pending(counts["pending"])
	adapter.metrics.Deleted("job", counts["job"])
	adapter.metrics.Deleted("variable", counts["variable"])
	return errors.Join(errs...)
}

// sweepJob sweeps one run, and reports what it counts: "job" purged, "variable" stale material removed, or "pending" an
// own ended run left.
func (adapter *Adapter) sweepJob(ctx context.Context, job string) (string, error) {
	m, err := readMarker(job)
	if err != nil {
		return "", adapter.failed("read "+markerPath(job), err)
	}
	if m == nil {
		if stale(filepath.Join(controlRoot, job)) {
			return "", adapter.removeRun(job)
		}
		return "", nil
	}
	u, err := adapter.unit(ctx, unitName(job))
	if err != nil {
		return "", err
	}
	own := m.Adapter == adapter.name
	if m.Phase == phasePrepared && m.NeverStarted == nil && !u.loaded {
		if !stale(markerPath(job)) {
			return "", nil
		}
		if err := adapter.removeRun(job); err != nil || !own {
			return "", err
		}
		return "variable", nil
	}
	if state, _, settled := adapter.judge(job, m, u); !settled || state == wherry.JobRunning || state == wherry.JobAbsent {
		return "", nil
	}
	expired := time.Since(endOf(job, u)) >= sweepGrace
	if !own && !expired {
		return "", nil
	}
	pending := ""
	if own {
		pending = "pending"
	}
	if err := adapter.stop(ctx, unitName(job)); err != nil {
		return pending, err
	}
	if err := adapter.release(ctx, job); err != nil {
		return pending, err
	}
	if !own || !expired {
		return pending, nil
	}
	if err := adapter.purge(ctx, job); err != nil {
		return pending, err
	}
	return "job", nil
}

// busName returns the D-Bus error name of err, empty for an error that is not a D-Bus one.
func busName(err error) string {
	var busErr dbus.Error
	if errors.As(err, &busErr) {
		return busErr.Name
	}
	return ""
}

// failed counts err from the operation op by class and wraps it; an unknown unit, an absent file or a cancelled call
// counts nothing.
func (adapter *Adapter) failed(op string, err error) error {
	class := "other"
	var errno syscall.Errno
	var network net.Error
	switch name := busName(err); {
	case err == nil:
		return nil
	case name == busNoSuchUnit || errors.Is(err, fs.ErrNotExist) || errors.Is(err, context.Canceled):
		return &wherry.Error{Op: op, Err: err}
	case errors.Is(err, context.DeadlineExceeded):
		class = "timeout"
	case name != "":
		switch name {
		case "org.freedesktop.DBus.Error.AccessDenied", "org.freedesktop.DBus.Error.InteractiveAuthorizationRequired":
			class = "denied"
		case busUnitExists, "org.freedesktop.systemd1.TransactionIsDestructive", "org.freedesktop.systemd1.TransactionJobsConflicting", "org.freedesktop.systemd1.JobTypeNotApplicable":
			class = "conflict"
		case "org.freedesktop.DBus.Error.NoReply", "org.freedesktop.DBus.Error.Timeout", "org.freedesktop.DBus.Error.TimedOut":
			class = "timeout"
		case "org.freedesktop.DBus.Error.ServiceUnknown", "org.freedesktop.DBus.Error.NoServer", "org.freedesktop.DBus.Error.Disconnected":
			class = "unavailable"
		}
	case errors.As(err, &errno):
		switch errno {
		case syscall.EACCES, syscall.EPERM, syscall.EROFS:
			class = "denied"
		case syscall.ENOSPC, syscall.EDQUOT, syscall.EIO:
			class = "unavailable"
		}
	case errors.Is(err, dbus.ErrClosed) || errors.Is(err, io.EOF) || errors.As(err, &network):
		class = "unavailable"
	}
	adapter.metrics.Failed(class)
	return &wherry.Error{Op: op, Class: class, Err: err}
}

// handled counts err as failed does and logs its op and class, never its text.
func (adapter *Adapter) handled(op string, err error) {
	var failure *wherry.Error
	if errors.As(adapter.failed(op, err), &failure) && failure.Class != "" {
		adapter.logger.Warn("systemd call failed", "adapter", adapter.name, "op", failure.Op, "class", failure.Class)
	}
}

// Metrics reports the adapter's platform errors, sweep deletions, cleanup-pending runs and placement latency.
func (adapter *Adapter) Metrics(context.Context) ([]wherry.Sample, error) {
	return adapter.metrics.Samples(), nil
}
