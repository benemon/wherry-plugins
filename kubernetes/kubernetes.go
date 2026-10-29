package main

import (
	"cmp"
	"context"
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
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/benemon/wherry/pluginsdk"
	"github.com/hashicorp/go-hclog"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"
)

const (
	jobPrefix = "agent-"
	// maxSecret is the API server's limit on a Secret's values together.
	maxSecret = 1 << 20
	// maxKey is the API server's limit on a Secret key.
	maxKey       = 253
	graceSeconds = 15
	cleanupWait  = 10 * time.Second

	labelPool         = "wherry.io/pool"
	labelProfile      = "wherry.io/profile"
	labelAgent        = "wherry.io/agent"
	labelRun          = "wherry.io/run"
	annotationAttempt = "wherry.io/attempt"

	workPath        = "/wherry"
	inputPath       = workPath + "/inputs"
	outputPath      = workPath + "/outputs"
	credentialsPath = "/run/wherry/credentials/files"
)

var (
	envName     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	runtimeUser = regexp.MustCompile(`^([1-9][0-9]*)(?::([1-9][0-9]*))?$`)
	runtimeGID  = regexp.MustCompile(`^[1-9][0-9]*$`)
	// serviceAccountDir holds the token and CA the kubelet projects into the receiver's pod.
	serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"
	sweepGrace        = 10 * time.Minute
	waitPoll          = 2 * time.Second
	followPoll        = time.Second
	collectPoll       = 500 * time.Millisecond
	errReplaced       = errors.New("the pod was replaced")
	// receiverUID is the receiver's UID, which the agent shares so that each reads the run directory the other writes.
	receiverUID = int64(os.Getuid())
)

// Config is the plugin's config with the plugin profile's merged onto it and its credential key removed.
type Config struct {
	Address      string            `json:"address"`
	CAFile       string            `json:"ca_file"`
	Namespace    string            `json:"namespace"`
	NodeSelector map[string]string `json:"node_selector,omitzero"`
	Tolerations  []Toleration      `json:"tolerations,omitzero"`
	RunsClaim    string            `json:"runs_claim"`
	RunsDir      string            `json:"runs_dir"`
	RunsFloor    string            `json:"runs_floor"`
	Runtime      Runtime           `json:"runtime"`
}

// Toleration lets agent pods onto nodes with a matching taint.
type Toleration struct {
	Key      string `json:"key"`
	Operator string `json:"operator"`
	Value    string `json:"value"`
	Effect   string `json:"effect"`
}

// Runtime defines the agent container the plugin profile launches.
type Runtime struct {
	Image               string            `json:"image"`
	Command             []string          `json:"command"`
	Args                []string          `json:"args"`
	Env                 map[string]string `json:"env"`
	Resources           Resources         `json:"resources"`
	User                string            `json:"user"`
	Group               string            `json:"group"`
	SupplementaryGroups []string          `json:"supplementary_groups"`
}

// Resources are the agent container's requests and limits, as Kubernetes quantities.
type Resources struct {
	CPU              string `json:"cpu"`
	Memory           string `json:"memory"`
	EphemeralStorage string `json:"ephemeral_storage"`
}

// inlineLimit is the inline input bytes the plugin declares.
const inlineLimit = maxSecret / 2

// InlineLimit reports inlineLimit.
func (profile *Kubernetes) InlineLimit() int64 { return inlineLimit }

// Kubernetes is one plugin profile: it runs each run as one pod, one Secret and one directory on the runs claim.
type Kubernetes struct {
	name, pool, namespace string
	runsClaim             string
	// runsDir is the claim's mount on the receiver; runs holds a directory per run below it.
	runsDir, runs string
	floor         int64
	runtime       Runtime
	nodeSelector  map[string]string
	tolerations   []corev1.Toleration
	resources     corev1.ResourceList
	uid, gid      *int64
	groups        []int64
	core          corev1client.CoreV1Interface
	meta          metadata.Interface
	fetcher       *http.Client
	metrics       *PlatformMetrics
	logger        hclog.Logger
}

// fetchError is a fetch that failed, in words that never quote the URL.
type fetchError struct{ name, reason string }

func (e *fetchError) Error() string { return "fetch " + e.name + ": " + e.reason }

// newClient builds the API configuration for config's address and CA with token, or the in-cluster one.
func newClient(config Config, token string) (*rest.Config, error) {
	client := &rest.Config{Host: config.Address, BearerToken: token, QPS: 50, Burst: 100,
		RateLimiter: limiter{flowcontrol.NewTokenBucketRateLimiter(50, 100)}}
	switch {
	case config.Address == "":
		host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
		if host == "" || port == "" {
			return nil, errors.New("no address, and not in a cluster")
		}
		client.Host = "https://" + net.JoinHostPort(host, port)
		client.CAFile = filepath.Join(serviceAccountDir, "ca.crt")
		if token == "" {
			// client-go rereads the file, so the kubelet's rotation of the projected token reaches it.
			client.BearerTokenFile = filepath.Join(serviceAccountDir, "token")
			if _, err := os.Stat(client.BearerTokenFile); err != nil {
				return nil, errors.New("no platform credential, and no service account token")
			}
		}
	case !strings.HasPrefix(config.Address, "https://"):
		return nil, errors.New("address must be https://")
	case token == "":
		return nil, errors.New("address needs a platform credential")
	}
	if config.CAFile != "" {
		client.CAFile = config.CAFile
	}
	return client, nil
}

// limiter makes the client-side limiter's predictive refusal the context ending, which is what it predicts.
type limiter struct{ flowcontrol.RateLimiter }

func (l limiter) Wait(ctx context.Context) error {
	if err := l.RateLimiter.Wait(ctx); err != nil {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func quantity(field, value, fallback string) (*resource.Quantity, error) {
	if value = cmp.Or(value, fallback); value == "" {
		return nil, nil
	}
	parsed, err := resource.ParseQuantity(value)
	if err != nil {
		return nil, fmt.Errorf("%s must be a quantity", field)
	}
	return &parsed, nil
}

func image(value string) bool {
	return value != "" && !strings.ContainsFunc(value, unicode.IsSpace)
}

// New builds the plugin profile name, for a pool's runs through client.
func New(config Config, name, pool string, client *rest.Config, logger hclog.Logger) (*Kubernetes, error) {
	switch {
	case len(validation.IsDNS1123Label(config.Namespace)) > 0:
		return nil, errors.New("namespace must be a DNS-1123 label")
	case len(validation.IsDNS1123Subdomain(config.RunsClaim)) > 0:
		return nil, errors.New("runs_claim must be a claim name")
	case !filepath.IsAbs(config.RunsDir):
		return nil, errors.New("runs_dir must be an absolute path")
	case !image(config.Runtime.Image):
		return nil, errors.New("runtime image must be an image")
	}
	for key, value := range config.NodeSelector {
		if len(validation.IsQualifiedName(key)) > 0 || len(validation.IsValidLabelValue(value)) > 0 {
			return nil, errors.New("node_selector must hold label keys and values")
		}
	}
	var tolerations []corev1.Toleration
	for _, t := range config.Tolerations {
		switch {
		case t.Key != "" && len(validation.IsQualifiedName(t.Key)) > 0, len(validation.IsValidLabelValue(t.Value)) > 0:
			return nil, errors.New("a toleration's key and value must be a label key and value")
		case t.Operator != "Equal" && t.Operator != "Exists", t.Operator == "Exists" && t.Value != "":
			return nil, errors.New("a toleration's operator must be Equal, or Exists without a value")
		case !slices.Contains([]string{"NoSchedule", "PreferNoSchedule", "NoExecute"}, t.Effect):
			return nil, errors.New("a toleration's effect must be NoSchedule, PreferNoSchedule or NoExecute")
		}
		tolerations = append(tolerations, corev1.Toleration{Key: t.Key, Operator: corev1.TolerationOperator(t.Operator), Value: t.Value, Effect: corev1.TaintEffect(t.Effect)})
	}
	for variable := range config.Runtime.Env {
		if !envName.MatchString(variable) {
			return nil, errors.New("runtime env names must be environment variable names")
		}
	}
	resources := corev1.ResourceList{}
	for _, r := range []struct {
		name            corev1.ResourceName
		field, fallback string
		value           string
	}{
		{corev1.ResourceCPU, "runtime cpu", "500m", config.Runtime.Resources.CPU},
		{corev1.ResourceMemory, "runtime memory", "2Gi", config.Runtime.Resources.Memory},
		{corev1.ResourceEphemeralStorage, "runtime ephemeral_storage", "", config.Runtime.Resources.EphemeralStorage},
	} {
		value, err := quantity(r.field, r.value, r.fallback)
		if err != nil {
			return nil, err
		}
		if value != nil {
			resources[r.name] = *value
		}
	}
	floor, err := quantity("runs_floor", config.RunsFloor, "1Gi")
	if err != nil {
		return nil, err
	}
	var uid, gid *int64
	if user := config.Runtime.User; user != "" {
		match := runtimeUser.FindStringSubmatch(user)
		if match == nil {
			return nil, errors.New("runtime user must be UID or UID:GID other than root")
		}
		id, err := strconv.ParseInt(match[1], 10, 32)
		if err != nil {
			return nil, errors.New("runtime user IDs must be below 2^31")
		}
		if id != receiverUID {
			return nil, errors.New("runtime user must be the receiver's UID")
		}
		uid = &id
		if match[2] != "" {
			group, err := strconv.ParseInt(match[2], 10, 32)
			if err != nil {
				return nil, errors.New("runtime user IDs must be below 2^31")
			}
			gid = &group
		}
	}
	if group := config.Runtime.Group; group != "" {
		if gid != nil {
			return nil, errors.New("runtime group must be unset when runtime user names a GID")
		}
		id, err := strconv.ParseInt(group, 10, 32)
		if !runtimeGID.MatchString(group) || err != nil {
			return nil, errors.New("runtime group must be a GID below 2^31 other than root")
		}
		gid = &id
	}
	var groups []int64
	for _, group := range config.Runtime.SupplementaryGroups {
		id, err := strconv.ParseInt(group, 10, 32)
		if !runtimeGID.MatchString(group) || err != nil {
			return nil, errors.New("runtime supplementary_groups must be GIDs below 2^31 other than root")
		}
		groups = append(groups, id)
	}
	client = rest.CopyConfig(client)
	client.WarningHandler = warnings{logger: logger, profile: name}
	httpClient, err := rest.HTTPClientFor(client)
	if err != nil {
		return nil, err
	}
	core, err := corev1client.NewForConfigAndClient(client, httpClient)
	if err != nil {
		return nil, err
	}
	meta, err := metadata.NewForConfigAndClient(client, httpClient)
	if err != nil {
		return nil, err
	}
	return &Kubernetes{
		name: name, pool: pool, namespace: config.Namespace,
		runsClaim: config.RunsClaim, runsDir: config.RunsDir, runs: filepath.Join(config.RunsDir, "runs"), floor: floor.Value(),
		runtime: config.Runtime, nodeSelector: config.NodeSelector, tolerations: tolerations, resources: resources,
		uid: uid, gid: gid, groups: groups,
		core: core, meta: meta,
		fetcher: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		metrics: NewPlatformMetrics(),
		logger:  logger,
	}, nil
}

// warnings logs the API server's warnings, which client-go would otherwise write to stderr.
type warnings struct {
	logger  hclog.Logger
	profile string
}

func (w warnings) HandleWarningHeader(_ int, _ string, text string) {
	w.logger.Warn("kubernetes warning", "profile", w.profile, "warning", text)
}

// jobName is the name of a run's pod and Secret.
func jobName(pool, runID string) string {
	sum := sha256.Sum256([]byte(pool))
	return fmt.Sprintf("%s%s-%s", jobPrefix, hex.EncodeToString(sum[:])[:12], runID)
}

func (profile *Kubernetes) jobName(runID string) string {
	return jobName(profile.pool, runID)
}

func (profile *Kubernetes) pods() corev1client.PodInterface {
	return profile.core.Pods(profile.namespace)
}

func (profile *Kubernetes) secrets() corev1client.SecretInterface {
	return profile.core.Secrets(profile.namespace)
}

// Directories reports the fixed paths of the agent container.
func (profile *Kubernetes) Directories() pluginsdk.Directories {
	return pluginsdk.Directories{Input: inputPath, Output: outputPath, Credentials: credentialsPath}
}

// Ready reports whether the namespace exists and is active and the runs claim has runs_floor free.
func (profile *Kubernetes) Ready(ctx context.Context) (bool, error) {
	namespace, err := profile.core.Namespaces().Get(ctx, profile.namespace, metav1.GetOptions{})
	if err != nil {
		return false, profile.failed("read namespace "+profile.namespace, err)
	}
	free, err := profile.free()
	if err != nil {
		return false, err
	}
	return namespace.Status.Phase == corev1.NamespaceActive && free >= profile.floor, nil
}

// free reports the bytes the receiver can write to the runs claim.
func (profile *Kubernetes) free() (int64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(profile.runsDir, &stat); err != nil {
		return 0, fmt.Errorf("read the free space of runs_dir: %w", err)
	}
	return int64(stat.Bavail) * int64(stat.Bsize), nil
}

// secretKey is the run's Secret key holding a credential; the pod maps credentials.* keys alone into files.
func secretKey(secret pluginsdk.Secret) string {
	if secret.Env != "" {
		return "env." + secret.Env
	}
	return "credentials." + secret.File
}

// secretData is a run's Secret data, refused when a key or the values together pass the API server's limits.
func secretData(run pluginsdk.Run) (map[string][]byte, error) {
	data := map[string][]byte{"prompt": run.Prompt}
	size := len(run.Prompt)
	for _, secret := range run.Credentials {
		key := secretKey(secret)
		if len(key) > maxKey {
			return nil, &pluginsdk.InputTooLargeError{Name: cmp.Or(secret.File, secret.Env)}
		}
		data[key] = secret.Value
		size += len(secret.Value)
	}
	if size > maxSecret {
		return nil, errors.New("the prompt and credentials exceed the 1 MiB Secret limit")
	}
	return data, nil
}

func (profile *Kubernetes) labels(run pluginsdk.Run) map[string]string {
	return map[string]string{
		labelPool: profile.pool, labelProfile: run.Profile, labelAgent: run.Agent, labelRun: run.ID,
	}
}

// buildPod builds the run's pod, which mounts its directory on the runs claim and its material from the Secret name.
func (profile *Kubernetes) buildPod(run pluginsdk.Run, name string) *corev1.Pod {
	env := pluginsdk.AgentEnv(profile.runtime.Env)
	env["HOME"] = workPath
	env["WHERRY_INPUT_DIR"] = inputPath
	env["WHERRY_OUTPUT_DIR"] = outputPath
	env["WHERRY_CREDENTIALS_DIR"] = credentialsPath
	env["WHERRY_RUN_DIR"] = name
	if run.Model != "" {
		env["WHERRY_MODEL"] = run.Model
	}
	for _, secret := range run.Credentials {
		delete(env, secret.Env)
	}
	var vars []corev1.EnvVar
	for _, variable := range slices.Sorted(maps.Keys(env)) {
		vars = append(vars, corev1.EnvVar{Name: variable, Value: env[variable]})
	}
	items := []corev1.KeyToPath{{Key: "prompt", Path: "prompt"}}
	for _, secret := range run.Credentials {
		if secret.Env != "" {
			vars = append(vars, corev1.EnvVar{Name: secret.Env, ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: secretKey(secret)},
			}})
			continue
		}
		items = append(items, corev1.KeyToPath{Key: secretKey(secret), Path: "files/" + secret.File})
	}
	security := &corev1.SecurityContext{AllowPrivilegeEscalation: new(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: profile.namespace, Labels: profile.labels(run),
			Annotations: map[string]string{annotationAttempt: strconv.FormatInt(run.Attempt, 10)},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			AutomountServiceAccountToken:  new(false),
			EnableServiceLinks:            new(false),
			TerminationGracePeriodSeconds: new(int64(graceSeconds)),
			NodeSelector:                  profile.nodeSelector,
			Tolerations:                   profile.tolerations,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: new(true), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				RunAsUser: profile.uid, RunAsGroup: profile.gid, SupplementalGroups: profile.groups, FSGroup: profile.gid,
			},
			Containers: []corev1.Container{{
				Name: "agent", Image: profile.runtime.Image, Command: profile.runtime.Command, Args: profile.runtime.Args, Env: vars,
				Resources: corev1.ResourceRequirements{Requests: profile.resources, Limits: profile.resources}, SecurityContext: security,
				VolumeMounts: []corev1.VolumeMount{
					{Name: "runs", MountPath: workPath, SubPathExpr: "runs/$(WHERRY_RUN_DIR)"},
					{Name: "material", MountPath: workPath + "/prompt.txt", SubPath: "prompt", ReadOnly: true},
					{Name: "material", MountPath: credentialsPath, SubPath: "files", ReadOnly: true},
				},
			}},
			Volumes: []corev1.Volume{
				{Name: "runs", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: profile.runsClaim}}},
				// A Secret volume is memory-backed, so the agent cannot hard-link a credential into its outputs.
				{Name: "material", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: name, Items: items, DefaultMode: new(int32(0o440))}}},
			},
		},
	}
}

// Dispatch starts a turn idempotently and returns once its agent has started or its pod has ended.
func (profile *Kubernetes) Dispatch(ctx context.Context, run pluginsdk.Run) (pluginsdk.Outcome, pluginsdk.Handle, error) {
	began := time.Now()
	name := profile.jobName(run.ID)
	data, err := secretData(run)
	if err != nil {
		return pluginsdk.OutcomeNothingCreated, pluginsdk.Handle{}, err
	}
	created := false
	switch existing, err := profile.pods().Get(ctx, name, metav1.GetOptions{}); {
	case apierrors.IsNotFound(err):
		if err := profile.writeInputs(ctx, name, run); err != nil {
			return profile.notStarted(ctx, name, err)
		}
		if err := profile.upsertSecret(ctx, name, profile.labels(run), data); err != nil {
			return profile.notStarted(ctx, name, err)
		}
		_, err := profile.pods().Create(ctx, profile.buildPod(run, name), metav1.CreateOptions{})
		switch {
		case apierrors.IsAlreadyExists(err):
		case err != nil:
			return profile.notStarted(ctx, name, profile.failed("create pod "+name, err))
		default:
			created = true
		}
	case err != nil:
		return pluginsdk.OutcomeUnknown, pluginsdk.Handle{}, profile.failed("read pod "+name, err)
	case !profile.agentPod(existing):
		return pluginsdk.OutcomeUnknown, pluginsdk.Handle{}, errors.New("existing pod is not an agent pod")
	}
	handle, state, err := profile.wait(ctx, name)
	if err != nil {
		return pluginsdk.OutcomeUnknown, pluginsdk.Handle{}, err
	}
	if created && state != pluginsdk.JobNeverStarted {
		profile.metrics.Placed(time.Since(began).Seconds())
	}
	return pluginsdk.OutcomeCreated, handle, nil
}

// upsertSecret writes the run's Secret over any an earlier attempt left.
func (profile *Kubernetes) upsertSecret(ctx context.Context, name string, labels map[string]string, data map[string][]byte) error {
	existing, err := profile.secrets().Get(ctx, name, metav1.GetOptions{})
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: profile.namespace, Labels: labels}, Type: corev1.SecretTypeOpaque, Data: data}
	switch {
	case apierrors.IsNotFound(err):
		if _, err := profile.secrets().Create(ctx, secret, metav1.CreateOptions{}); err != nil {
			return profile.failed("create secret "+name, err)
		}
	case err != nil:
		return profile.failed("read secret "+name, err)
	default:
		secret.ResourceVersion = existing.ResourceVersion
		if _, err := profile.secrets().Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
			return profile.failed("update secret "+name, err)
		}
	}
	return nil
}

// notStarted deletes the Secret and returns OutcomeNothingCreated when no pod exists, else OutcomeUnknown, with cause.
func (profile *Kubernetes) notStarted(ctx context.Context, name string, cause error) (pluginsdk.Outcome, pluginsdk.Handle, error) {
	if _, err := profile.pods().Get(ctx, name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		profile.handled("read pod "+name, err)
		return pluginsdk.OutcomeUnknown, pluginsdk.Handle{}, cause
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupWait)
	defer cancel()
	if err := profile.deleteSecret(cleanupCtx, name); err != nil {
		return pluginsdk.OutcomeNothingCreated, pluginsdk.Handle{}, errors.Join(cause, err)
	}
	return pluginsdk.OutcomeNothingCreated, pluginsdk.Handle{}, cause
}

// writeInputs writes the run's inputs into its directory on the runs claim, keeping a directory an earlier dispatch
// completed; the pod is created after, so its agent never sees a partial input.
func (profile *Kubernetes) writeInputs(ctx context.Context, name string, run pluginsdk.Run) error {
	dir := filepath.Join(profile.runs, name)
	if written(dir, run) {
		return nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove the run directory: %w", err)
	}
	if err := os.MkdirAll(profile.runs, 0o700); err != nil {
		return fmt.Errorf("create the runs directory: %w", err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return fmt.Errorf("create the run directory: %w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.Mkdir("inputs", 0o700); err != nil {
		return fmt.Errorf("create the inputs directory: %w", err)
	}
	for _, input := range run.Inputs {
		if err := root.WriteFile("inputs/"+input.Name, input.Data, 0o600); err != nil {
			return fmt.Errorf("write input %s: %w", input.Name, err)
		}
	}
	for _, fetch := range run.Fetches {
		if err := profile.fetch(ctx, root, fetch); err != nil {
			return err
		}
	}
	return nil
}

// written reports whether dir holds the inputs directory alone, with each of the run's inputs verified and no other.
func written(dir string, run pluginsdk.Run) bool {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return false
	}
	defer root.Close()
	want := map[string]pluginsdk.Fetch{}
	for _, input := range run.Inputs {
		want[input.Name] = pluginsdk.Fetch{Size: int64(len(input.Data)), SHA256: digest(input.Data)}
	}
	for _, fetch := range run.Fetches {
		want[fetch.Name] = fetch
	}
	if names, err := names(root, "."); err != nil || !slices.Equal(names, []string{"inputs"}) {
		return false
	}
	if names, err := names(root, "inputs"); err != nil || !slices.Equal(names, slices.Sorted(maps.Keys(want))) {
		return false
	}
	for name, input := range want {
		if !verified(root, "inputs/"+name, input) {
			return false
		}
	}
	return true
}

func names(root *os.Root, dir string) ([]string, error) {
	file, err := root.Open(dir)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	names, err := file.Readdirnames(-1)
	slices.Sort(names)
	return names, err
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// verified reports whether name is a regular file of the fetch's size and digest.
func verified(root *os.Root, name string, fetch pluginsdk.Fetch) bool {
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != fetch.Size {
		return false
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return false
	}
	return hex.EncodeToString(hash.Sum(nil)) == fetch.SHA256
}

// fetch writes one S3 input into the inputs directory, refusing a redirect and reading at most one byte past its size.
func (profile *Kubernetes) fetch(ctx context.Context, root *os.Root, fetch pluginsdk.Fetch) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fetch.URL, nil)
	if err != nil {
		return &fetchError{fetch.Name, "invalid URL"}
	}
	response, err := profile.fetcher.Do(request)
	if err != nil {
		return &fetchError{fetch.Name, unreached(err)}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return &fetchError{fetch.Name, strconv.Itoa(response.StatusCode)}
	}
	name := "inputs/" + fetch.Name
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("write input %s: %w", fetch.Name, err)
	}
	defer file.Close()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(response.Body, fetch.Size+1))
	var write *fs.PathError
	switch {
	case errors.As(err, &write):
		return fmt.Errorf("write input %s: %w", fetch.Name, err)
	case err != nil:
		return &fetchError{fetch.Name, unreached(err)}
	case n < fetch.Size:
		return &fetchError{fetch.Name, "shorter than its size"}
	case n > fetch.Size:
		return &fetchError{fetch.Name, "longer than its size"}
	case hex.EncodeToString(hash.Sum(nil)) != fetch.SHA256:
		return &fetchError{fetch.Name, "does not match its digest"}
	}
	return file.Close()
}

// unreached is the reason a request failed in transport.
func unreached(err error) string {
	var network net.Error
	var certificate *tls.CertificateVerificationError
	switch {
	case errors.Is(err, context.DeadlineExceeded) || errors.As(err, &network) && network.Timeout():
		return "timeout"
	case errors.As(err, &certificate):
		return "certificate"
	}
	return "unreachable"
}

// handle is the version 1 encoding of pluginsdk.Handle.Data.
type handle struct {
	V   int    `json:"v"`
	Pod string `json:"pod"`
	UID string `json:"uid"`
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

// current reads the handle's pod and refuses one that replaced it.
func (profile *Kubernetes) current(ctx context.Context, h handle) (*corev1.Pod, error) {
	pod, err := profile.pods().Get(ctx, h.Pod, metav1.GetOptions{})
	if err != nil {
		return nil, profile.failed("read pod "+h.Pod, err)
	}
	if string(pod.UID) != h.UID {
		return nil, errReplaced
	}
	return pod, nil
}

// wait returns once the pod is settled; a wait cut short deletes the pod.
func (profile *Kubernetes) wait(ctx context.Context, name string) (pluginsdk.Handle, pluginsdk.JobState, error) {
	for {
		pod, err := profile.pods().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if ctx.Err() != nil {
				return pluginsdk.Handle{}, 0, profile.deleteTimedOut(ctx, name)
			}
			return pluginsdk.Handle{}, 0, profile.failed("read pod "+name, err)
		}
		if state, _, settled := judge(pod); settled {
			data, _ := json.Marshal(handle{V: 1, Pod: name, UID: string(pod.UID)}) // strings always encode
			return pluginsdk.Handle{Data: data, Name: name}, state, nil
		}
		select {
		case <-ctx.Done():
			return pluginsdk.Handle{}, 0, profile.deleteTimedOut(ctx, name)
		case <-time.After(waitPoll):
		}
	}
}

func (profile *Kubernetes) deleteTimedOut(ctx context.Context, name string) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupWait)
	defer cancel()
	if err := profile.deletePod(cleanupCtx, name); err != nil {
		return errors.Join(ctx.Err(), fmt.Errorf("delete timed-out pod: %w", err))
	}
	return ctx.Err()
}

func status(statuses []corev1.ContainerStatus, name string) *corev1.ContainerStatus {
	for i := range statuses {
		if statuses[i].Name == name {
			return &statuses[i]
		}
	}
	return nil
}

// unset reports a time the runtime never set; CRI-O reports one as the Unix epoch.
func unset(t metav1.Time) bool {
	return t.IsZero() || t.Unix() == 0
}

func started(agent *corev1.ContainerStatus) bool {
	switch {
	case agent == nil:
		return false
	case agent.State.Terminated != nil:
		return !unset(agent.State.Terminated.StartedAt)
	}
	return agent.State.Running != nil || agent.Started != nil && *agent.Started
}

// judge reads a run's state and terminal from its pod; settled is false while the pod is pending.
func judge(pod *corev1.Pod) (pluginsdk.JobState, pluginsdk.Terminal, bool) {
	agent := status(pod.Status.ContainerStatuses, "agent")
	terminal := pluginsdk.Terminal{Started: started(agent), Node: pod.Spec.NodeName}
	var ended *corev1.ContainerStateTerminated
	if agent != nil {
		ended = agent.State.Terminated
	}
	if ended != nil {
		terminal.ExitCode = int(ended.ExitCode)
		if ended.Signal != 0 {
			terminal.Signal = strconv.Itoa(int(ended.Signal))
		}
		if ended.Reason != "Completed" {
			terminal.Reason = ended.Reason
		}
	}
	switch {
	case pod.DeletionTimestamp != nil:
		if ended == nil {
			terminal.ExitCode = -1
		}
		if terminal.Started {
			return pluginsdk.JobEnded, terminal, true
		}
		terminal.Class, terminal.Reason = "pod", "the pod is being deleted"
		return pluginsdk.JobNeverStarted, terminal, true
	case terminal.Started && ended == nil:
		return pluginsdk.JobRunning, terminal, true
	case terminal.Started:
		return pluginsdk.JobEnded, terminal, true
	}
	terminal.Class, terminal.Reason = neverStarted(pod, ended)
	return pluginsdk.JobNeverStarted, terminal, terminal.Class != ""
}

// neverStarted returns the class and the platform's reason of a pod that ended, or cannot start, before its agent.
func neverStarted(pod *corev1.Pod, agent *corev1.ContainerStateTerminated) (string, string) {
	if agent != nil {
		return "start", cmp.Or(agent.Message, agent.Reason)
	}
	if pod.Status.Phase == corev1.PodFailed && pod.Status.Reason != "" {
		return "pod", strings.Join(slices.DeleteFunc([]string{pod.Status.Reason, pod.Status.Message}, func(s string) bool { return s == "" }), ": ")
	}
	for _, container := range pod.Status.ContainerStatuses {
		if waiting := container.State.Waiting; waiting != nil {
			switch waiting.Reason {
			case "ImagePullBackOff", "ErrImageNeverPull", "InvalidImageName":
				return "image", waiting.Message
			case "CreateContainerConfigError", "CreateContainerError":
				return "config", waiting.Message
			}
		}
	}
	if pod.Status.Phase == corev1.PodFailed {
		return "pod", strings.Join(slices.DeleteFunc([]string{pod.Status.Reason, pod.Status.Message}, func(s string) bool { return s == "" }), ": ")
	}
	return "", ""
}

// Lookup finds a run's existing pod and waits for it as Dispatch does.
func (profile *Kubernetes) Lookup(ctx context.Context, runID string) (pluginsdk.Job, error) {
	name := profile.jobName(runID)
	pod, err := profile.pods().Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return pluginsdk.Job{}, nil
	}
	if err != nil {
		return pluginsdk.Job{}, profile.failed("read pod "+name, err)
	}
	if !profile.agentPod(pod) {
		return pluginsdk.Job{}, errors.New("existing pod is not an agent pod")
	}
	handle, state, err := profile.wait(ctx, name)
	if err != nil {
		return pluginsdk.Job{}, err
	}
	// A pod created without the annotation reads as attempt 0.
	attempt, _ := strconv.ParseInt(pod.Annotations[annotationAttempt], 10, 64)
	return pluginsdk.Job{State: state, Handle: handle, Attempt: attempt, Profile: pod.Labels[labelProfile], Agent: pod.Labels[labelAgent]}, nil
}

// agentPod reports whether pod is this pool's agent pod.
func (profile *Kubernetes) agentPod(pod *corev1.Pod) bool {
	return pod.Labels[labelPool] == profile.pool && slices.ContainsFunc(pod.Spec.Containers, func(container corev1.Container) bool { return container.Name == "agent" })
}

// Follow streams the agent's stdout lines from a byte offset until it ends, then returns its terminal.
func (profile *Kubernetes) Follow(ctx context.Context, encoded pluginsdk.Handle, from int64, lines chan<- pluginsdk.Line) (pluginsdk.Terminal, error) {
	h, err := decodeHandle(encoded)
	if err != nil {
		return pluginsdk.Terminal{}, err
	}
	pod, err := profile.current(ctx, h)
	if err != nil {
		return pluginsdk.Terminal{}, err
	}
	// The logs API answers 400 for a container that never started and cannot answer for a lost node.
	for {
		state, terminal, settled := judge(pod)
		if settled && (state == pluginsdk.JobNeverStarted || pod.DeletionTimestamp != nil) {
			return terminal, nil
		}
		if settled {
			break
		}
		select {
		case <-ctx.Done():
			return pluginsdk.Terminal{}, ctx.Err()
		case <-time.After(followPoll):
		}
		if pod, err = profile.current(ctx, h); err != nil {
			return pluginsdk.Terminal{}, err
		}
	}
	reader := LineReader{Lines: lines, Position: from}
	for {
		deleting, err := profile.stream(ctx, h, &reader)
		if err != nil {
			return pluginsdk.Terminal{}, err
		}
		if deleting != nil {
			_, terminal, _ := judge(deleting)
			return terminal, nil
		}
		for running := false; !running; {
			select {
			case <-ctx.Done():
				return pluginsdk.Terminal{}, ctx.Err()
			case <-time.After(followPoll):
			}
			pod, err := profile.current(ctx, h)
			if err != nil {
				return pluginsdk.Terminal{}, err
			}
			agent := status(pod.Status.ContainerStatuses, "agent")
			switch _, terminal, _ := judge(pod); {
			case pod.DeletionTimestamp != nil:
				return terminal, nil
			case agent != nil && agent.State.Terminated != nil:
				if err := reader.Flush(ctx); err != nil {
					return pluginsdk.Terminal{}, err
				}
				return terminal, nil
			case agent != nil && agent.State.Running != nil:
				running = true
			}
		}
		// The stream closed while the agent runs; the reopened one starts again at the last line's end.
		reader = LineReader{Lines: lines, Position: reader.Position}
	}
}

// stream reads one follow stream of the agent's log until it closes, or returns the pod once it is being deleted.
func (profile *Kubernetes) stream(ctx context.Context, h handle, reader *LineReader) (*corev1.Pod, error) {
	streamCtx, stop := context.WithCancel(ctx)
	defer stop()
	body, err := profile.pods().GetLogs(h.Pod, &corev1.PodLogOptions{Container: "agent", Follow: true}).Stream(streamCtx)
	if err != nil {
		return nil, profile.failed("read logs of "+h.Pod, err)
	}
	defer body.Close()
	chunks, closed := make(chan []byte), make(chan error, 1)
	go func() {
		if _, err := io.CopyN(io.Discard, body, reader.Position); err != nil {
			closed <- errors.New("the agent's log is shorter than the offset")
			return
		}
		for {
			chunk := make([]byte, 32<<10)
			n, err := body.Read(chunk)
			if n > 0 {
				select {
				case chunks <- chunk[:n]:
				case <-streamCtx.Done():
					return
				}
			}
			if err != nil {
				closed <- nil
				return
			}
		}
	}()
	poll := time.NewTicker(followPoll)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case err := <-closed:
			return nil, err
		case chunk := <-chunks:
			if err := reader.Take(ctx, chunk); err != nil {
				return nil, err
			}
		case <-poll.C:
			pod, err := profile.pods().Get(ctx, h.Pod, metav1.GetOptions{})
			if err != nil {
				profile.handled("read pod "+h.Pod, err)
				continue
			}
			if pod.DeletionTimestamp != nil {
				return pod, nil
			}
		}
	}
}

// running reports whether the agent may still run in a live pod; a pod not yet settled may hold a running agent.
func running(pod *corev1.Pod) bool {
	state, _, settled := judge(pod)
	return (!settled || state == pluginsdk.JobRunning) && pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed
}

// Stop deletes the pod with the grace, so its kubelet sends the agent SIGTERM and then SIGKILL, and waits for it to go.
// The outputs stay on the runs claim.
func (profile *Kubernetes) Stop(ctx context.Context, encoded pluginsdk.Handle) error {
	h, err := decodeHandle(encoded)
	if err != nil {
		return err
	}
	pod, err := profile.current(ctx, h)
	switch {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return err
	case !running(pod):
		return nil
	}
	if err := profile.pods().Delete(ctx, h.Pod, metav1.DeleteOptions{GracePeriodSeconds: new(int64(graceSeconds))}); err != nil && !apierrors.IsNotFound(err) {
		return profile.failed("delete pod "+h.Pod, err)
	}
	return profile.gone(ctx, h.Pod)
}

// gone waits for the pod to go, since a terminating pod's agent may still run, and its pod still holds its name.
func (profile *Kubernetes) gone(ctx context.Context, name string) error {
	for {
		_, err := profile.pods().Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return profile.failed("read pod "+name, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(collectPoll):
		}
	}
}

// Collect lists the outputs tree on the runs claim once the agent has ended or its pod has gone, and reads what Select
// chooses.
func (profile *Kubernetes) Collect(ctx context.Context, encoded pluginsdk.Handle, limits pluginsdk.Limits, outputs chan<- pluginsdk.Output) error {
	h, err := decodeHandle(encoded)
	if err != nil {
		return err
	}
	for {
		pod, err := profile.current(ctx, h)
		if apierrors.IsNotFound(err) {
			break
		}
		if err != nil {
			if ctx.Err() != nil {
				return errors.New("the agent is still running")
			}
			return err
		}
		if pod.DeletionTimestamp != nil {
			return errors.New("the pod is being deleted")
		}
		if agent := status(pod.Status.ContainerStatuses, "agent"); agent != nil && agent.State.Terminated != nil {
			break
		}
		select {
		case <-ctx.Done():
			return errors.New("the agent is still running")
		case <-time.After(collectPoll):
		}
	}
	root, err := profile.outputs(h.Pod)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("the outputs directory is not readable")
	}
	defer root.Close()
	entries, err := list(root, limits.Files)
	if err != nil {
		return err
	}
	selected, err := Select(entries, limits)
	if err != nil {
		return err
	}
	for _, chosen := range selected {
		output := chosen.Output
		switch {
		case chosen.Stream:
			file, err := open(root, output.Name, output.Size)
			if err != nil {
				return err
			}
			output.Body = struct {
				io.Reader
				io.Closer
			}{io.LimitReader(file, output.Size), file}
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

// outputs opens the run's outputs tree as a root, which refuses every path that leaves it, links included; an agent
// writes the tree, and the runs claim holds every other run's directory.
func (profile *Kubernetes) outputs(pod string) (*os.Root, error) {
	run, err := os.OpenRoot(filepath.Join(profile.runs, pod))
	if err != nil {
		return nil, err
	}
	defer run.Close()
	// A root follows a link in its own path.
	info, err := run.Lstat("outputs")
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("the outputs path is not a directory")
	}
	return run.OpenRoot("outputs")
}

// list lists the outputs tree, lstat'd and never through a link, stopping after limit+1 entries.
func list(root *os.Root, limit int) ([]pluginsdk.Output, error) {
	var entries []pluginsdk.Output
	for pending := []string{"."}; len(pending) > 0; {
		dir := pending[0]
		pending = pending[1:]
		names, err := names(root, dir)
		if err != nil {
			return nil, errors.New("an outputs directory is not readable")
		}
		for _, base := range names {
			name := filepath.Join(dir, base)
			info, err := root.Lstat(name)
			if err != nil {
				return nil, errors.New("an output is not readable")
			}
			if entries = append(entries, pluginsdk.Output{Name: filepath.ToSlash(name), Size: info.Size(), Mode: info.Mode().Type()}); len(entries) > limit {
				return entries, nil
			}
			if info.IsDir() {
				pending = append(pending, name)
			}
		}
	}
	return entries, nil
}

// open opens a listed regular file of size bytes.
func open(root *os.Root, name string, size int64) (*os.File, error) {
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("output %s is not readable", name)
	}
	info, err := file.Stat()
	// The root follows a link in the name's last element whatever the flags, so the name must lstat as the file opened.
	named, namedErr := root.Lstat(name)
	if err != nil || namedErr != nil || !info.Mode().IsRegular() || !os.SameFile(info, named) || info.Size() != size {
		file.Close()
		return nil, fmt.Errorf("output %s is not a regular file of its listed size", name)
	}
	return file, nil
}

// read reads a listed regular file and refuses a body of another length than its listing.
func read(root *os.Root, name string, size int64) ([]byte, error) {
	file, err := open(root, name, size)
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

// Release deletes the run's Secret.
func (profile *Kubernetes) Release(ctx context.Context, runID string) error {
	return profile.deleteSecret(ctx, profile.jobName(runID))
}

// Purge deletes the run's pod at once, waits for it to go, then deletes its Secret and its directory.
func (profile *Kubernetes) Purge(ctx context.Context, runID string) error {
	return profile.purge(ctx, profile.jobName(runID))
}

func (profile *Kubernetes) purge(ctx context.Context, name string) error {
	if err := profile.deletePod(ctx, name); err != nil {
		return err
	}
	if err := profile.gone(ctx, name); err != nil {
		return err
	}
	if err := profile.deleteSecret(ctx, name); err != nil {
		return err
	}
	return profile.removeRun(name)
}

func (profile *Kubernetes) removeRun(name string) error {
	if err := os.RemoveAll(filepath.Join(profile.runs, name)); err != nil {
		return fmt.Errorf("remove the run directory: %w", err)
	}
	return nil
}

// deletePod deletes a pod with grace 0, which removes it without waiting for its kubelet.
func (profile *Kubernetes) deletePod(ctx context.Context, name string) error {
	if err := profile.pods().Delete(ctx, name, metav1.DeleteOptions{GracePeriodSeconds: new(int64(0))}); err != nil && !apierrors.IsNotFound(err) {
		return profile.failed("delete pod "+name, err)
	}
	return nil
}

func (profile *Kubernetes) deleteSecret(ctx context.Context, name string) error {
	if err := profile.secrets().Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return profile.failed("delete secret "+name, err)
	}
	return nil
}

// ownName reports whether an object's name is the job name its own labels give.
func ownName(object metav1.Object) bool {
	labels := object.GetLabels()
	return labels[labelRun] != "" && object.GetName() == jobName(labels[labelPool], labels[labelRun])
}

// endOf reports whether a pod has ended, and when.
func endOf(pod *corev1.Pod) (bool, time.Time) {
	if state, _, settled := judge(pod); !settled || state == pluginsdk.JobRunning {
		return false, time.Time{}
	}
	if pod.DeletionTimestamp != nil {
		return true, pod.DeletionTimestamp.Time
	}
	if agent := status(pod.Status.ContainerStatuses, "agent"); agent != nil && agent.State.Terminated != nil && !unset(agent.State.Terminated.FinishedAt) {
		return true, agent.State.Terminated.FinishedAt.Time
	}
	at := pod.CreationTimestamp.Time
	for _, condition := range pod.Status.Conditions {
		if condition.LastTransitionTime.After(at) {
			at = condition.LastTransitionTime.Time
		}
	}
	return true, at
}

// Sweep purges this profile's pods ended for the grace period, deletes the Secrets no live run needs, and removes the run
// directories of the pool's runs with neither.
func (profile *Kubernetes) Sweep(ctx context.Context) error {
	selector := metav1.ListOptions{LabelSelector: labelPool + "=" + profile.pool}
	pods, err := profile.pods().List(ctx, selector)
	if err != nil {
		return profile.failed("list pods", err)
	}
	var errs []error
	pending, purged, deleted, removed := 0, 0, 0, 0
	// present names the pool's pods; releasable those whose Secret may go.
	present, releasable := map[string]bool{}, map[string]bool{}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !ownName(pod) {
			continue
		}
		present[pod.Name] = true
		ended, at := endOf(pod)
		if !ended {
			continue
		}
		expired := time.Since(at) >= sweepGrace
		if pod.Labels[labelProfile] != profile.name {
			releasable[pod.Name] = expired
			continue
		}
		releasable[pod.Name] = true
		if !expired {
			pending++
			continue
		}
		if err := profile.purge(ctx, pod.Name); err != nil {
			errs = append(errs, err)
			pending++
			continue
		}
		purged++
	}
	// Secrets are listed as metadata, so a list carries no value.
	secrets, err := profile.meta.Resource(corev1.SchemeGroupVersion.WithResource("secrets")).Namespace(profile.namespace).List(ctx, selector)
	if err != nil {
		errs = append(errs, profile.failed("list secrets", err))
	} else {
		for i := range secrets.Items {
			secret := &secrets.Items[i]
			switch {
			case !ownName(secret):
				continue
			case present[secret.Name] && !releasable[secret.Name]:
				continue
			case !present[secret.Name] && time.Since(secret.CreationTimestamp.Time) < sweepGrace:
				present[secret.Name] = true
				continue
			}
			if err := profile.deleteSecret(ctx, secret.Name); err != nil {
				errs = append(errs, err)
				present[secret.Name] = true
				continue
			}
			if secret.Labels[labelProfile] == profile.name {
				deleted++
			}
		}
		// present now names the pool's runs with a pod or a Secret left.
		runs, err := os.ReadDir(profile.runs)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("list the runs directory: %w", err))
		}
		for _, run := range runs {
			info, err := run.Info()
			if err != nil || !strings.HasPrefix(run.Name(), jobName(profile.pool, "")) || present[run.Name()] || time.Since(info.ModTime()) < sweepGrace {
				continue
			}
			if err := profile.removeRun(run.Name()); err != nil {
				errs = append(errs, err)
				continue
			}
			removed++
		}
	}
	profile.metrics.Pending(pending)
	profile.metrics.Deleted("job", purged)
	profile.metrics.Deleted("variable", deleted)
	profile.metrics.Deleted("directory", removed)
	return errors.Join(errs...)
}

// failed counts err from the API call op by class and wraps it; a not-found or cancelled call counts nothing.
func (profile *Kubernetes) failed(op string, err error) error {
	var status apierrors.APIStatus
	var network net.Error
	class := "other"
	switch {
	case err == nil:
		return nil
	case apierrors.IsNotFound(err) || errors.Is(err, context.Canceled):
		return &pluginsdk.Error{Op: op, Err: err}
	case errors.Is(err, context.DeadlineExceeded):
		class = "timeout"
	case errors.As(err, &status):
		switch code, reason := status.Status().Code, status.Status().Reason; {
		case code == http.StatusUnauthorized || code == http.StatusForbidden:
			class = "denied"
		case code == http.StatusConflict:
			class = "conflict"
		case code == http.StatusRequestTimeout || code == http.StatusGatewayTimeout || reason == metav1.StatusReasonTimeout || reason == metav1.StatusReasonServerTimeout:
			class = "timeout"
		case code == http.StatusTooManyRequests || code >= 500:
			class = "unavailable"
		}
	case errors.As(err, &network) && network.Timeout():
		class = "timeout"
	case errors.As(err, &network):
		class = "unavailable"
	}
	profile.metrics.Failed(class)
	return &pluginsdk.Error{Op: op, Class: class, Err: err}
}

// handled counts err as failed does and logs its op and class, never its text, which can hold a URL or a response body.
func (profile *Kubernetes) handled(op string, err error) {
	var failure *pluginsdk.Error
	if errors.As(profile.failed(op, err), &failure) && failure.Class != "" {
		profile.logger.Warn("kubernetes call failed", "profile", profile.name, "op", failure.Op, "class", failure.Class)
	}
}

// Metrics reports the profile's API errors, sweep deletions, cleanup-pending pods, placement latency and the runs
// claim's free bytes.
func (profile *Kubernetes) Metrics(context.Context) ([]pluginsdk.Sample, error) {
	samples := profile.metrics.Samples()
	if free, err := profile.free(); err == nil {
		samples = append(samples, pluginsdk.Sample{Family: "wherry_plugin_storage_free_bytes", Kind: pluginsdk.KindGauge, Value: float64(free)})
	}
	return samples, nil
}
