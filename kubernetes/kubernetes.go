package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/benemon/wherry-plugins/kubernetes/helper"
	wherry "github.com/benemon/wherry/adapter"
	"github.com/hashicorp/go-hclog"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes/scheme"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"
	"k8s.io/client-go/util/flowcontrol"
)

const (
	jobPrefix  = "agent-"
	helperPath = "/usr/local/libexec/wherry/wherry-k8s-helper"
	// maxSecret is the API server's limit on a Secret's values together.
	maxSecret = 1 << 20
	// maxKey is the API server's limit on a Secret key.
	maxKey       = 253
	graceSeconds = 15
	putAttempts  = 3
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
	stopGrace         = graceSeconds * time.Second
	waitPoll          = 2 * time.Second
	followPoll        = time.Second
	collectPoll       = 500 * time.Millisecond
	errReplaced       = errors.New("the pod was replaced")
)

// Config is the adapter's section of the receiver configuration.
type Config struct {
	Address      string            `json:"address"`
	CAFile       string            `json:"ca_file"`
	Namespace    string            `json:"namespace"`
	NodeSelector map[string]string `json:"node_selector,omitzero"`
	Tolerations  []Toleration      `json:"tolerations,omitzero"`
	HelperImage  string            `json:"helper_image"`
	Runtime      Runtime           `json:"runtime"`
}

// Toleration lets agent pods onto nodes with a matching taint.
type Toleration struct {
	Key      string `json:"key"`
	Operator string `json:"operator"`
	Value    string `json:"value"`
	Effect   string `json:"effect"`
}

// Runtime defines the agent container the adapter launches.
type Runtime struct {
	Image               string            `json:"image"`
	Command             []string          `json:"command"`
	Args                []string          `json:"args"`
	Env                 map[string]string `json:"env"`
	Resources           Resources         `json:"resources"`
	WorkLimit           string            `json:"work_limit"`
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

// inlineLimit is the inline input bytes the adapter declares: the run's Secret holds them beside the prompt and
// credentials within maxSecret, so half of it leaves the rest room. secretData keeps its check for the whole.
const inlineLimit = maxSecret / 2

// InlineLimit reports inlineLimit.
func (adapter *Adapter) InlineLimit() int64 { return inlineLimit }

// Adapter runs each run as one pod and one Secret in a namespace.
type Adapter struct {
	name, pool, namespace string
	helperImage           string
	runtime               Runtime
	nodeSelector          map[string]string
	tolerations           []corev1.Toleration
	resources             corev1.ResourceList
	workLimit             *resource.Quantity
	uid, gid              *int64
	groups                []int64
	core                  corev1client.CoreV1Interface
	meta                  metadata.Interface
	exec                  execer
	fetcher               *http.Client
	metrics               *PlatformMetrics
	logger                hclog.Logger
}

// execer runs a command in a container of a pod; a command that exits non-zero returns an *exitError.
type execer interface {
	exec(ctx context.Context, pod, container string, command []string, stdin io.Reader, stdout io.Writer) error
}

// exitError is a helper invocation's non-zero exit, with its message.
type exitError struct {
	code    int
	message string
}

func (e *exitError) Error() string { return fmt.Sprintf("helper exited %d: %s", e.code, e.message) }

// fetchError is a fetch that failed before its put, in words that never quote the URL.
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

// New builds the adapter named name for a pool's runs through client.
func New(config Config, name, pool string, client *rest.Config, logger hclog.Logger) (*Adapter, error) {
	switch {
	case len(validation.IsDNS1123Label(config.Namespace)) > 0:
		return nil, errors.New("namespace must be a DNS-1123 label")
	case !image(config.HelperImage):
		return nil, errors.New("helper_image must be an image")
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
	workLimit, err := quantity("runtime work_limit", config.Runtime.WorkLimit, "")
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
	return &Adapter{
		name: name, pool: pool, namespace: config.Namespace, helperImage: config.HelperImage,
		runtime: config.Runtime, nodeSelector: config.NodeSelector, tolerations: tolerations, resources: resources, workLimit: workLimit,
		uid: uid, gid: gid, groups: groups,
		core: core, meta: meta,
		exec: remote{config: client, client: core.RESTClient(), namespace: config.Namespace},
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

// remote execs through the API server, over WebSocket or else SPDY.
type remote struct {
	config    *rest.Config
	client    rest.Interface
	namespace string
}

func (r remote) exec(ctx context.Context, pod, container string, command []string, stdin io.Reader, stdout io.Writer) error {
	request := r.client.Post().Namespace(r.namespace).Resource("pods").Name(pod).SubResource("exec").VersionedParams(&corev1.PodExecOptions{
		Container: container, Command: command, Stdin: stdin != nil, Stdout: true, Stderr: true,
	}, scheme.ParameterCodec)
	websocket, err := remotecommand.NewWebSocketExecutor(r.config, http.MethodGet, request.URL().String())
	if err != nil {
		return err
	}
	spdy, err := remotecommand.NewSPDYExecutor(r.config, http.MethodPost, request.URL())
	if err != nil {
		return err
	}
	executor, err := remotecommand.NewFallbackExecutor(websocket, spdy, func(err error) bool {
		return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
	})
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdin: stdin, Stdout: stdout, Stderr: &stderr})
	var exit utilexec.ExitError
	if errors.As(err, &exit) {
		return &exitError{code: exit.ExitStatus(), message: stderr.String()}
	}
	return err
}

// command is a helper invocation of protocol helper.Protocol.
func command(args ...string) []string {
	return append([]string{helperPath, helper.Protocol}, args...)
}

// jobName is the name of a run's pod and Secret.
func jobName(pool, runID string) string {
	sum := sha256.Sum256([]byte(pool))
	return fmt.Sprintf("%s%s-%s", jobPrefix, hex.EncodeToString(sum[:])[:12], runID)
}

func (adapter *Adapter) jobName(runID string) string {
	return jobName(adapter.pool, runID)
}

func (adapter *Adapter) pods() corev1client.PodInterface {
	return adapter.core.Pods(adapter.namespace)
}

func (adapter *Adapter) secrets() corev1client.SecretInterface {
	return adapter.core.Secrets(adapter.namespace)
}

// Directories reports the fixed paths of the agent container.
func (adapter *Adapter) Directories() wherry.Directories {
	return wherry.Directories{Input: inputPath, Output: outputPath, Credentials: credentialsPath}
}

// Ready reports whether the namespace exists and is active.
func (adapter *Adapter) Ready(ctx context.Context) (bool, error) {
	namespace, err := adapter.core.Namespaces().Get(ctx, adapter.namespace, metav1.GetOptions{})
	if err != nil {
		return false, adapter.failed("read namespace "+adapter.namespace, err)
	}
	return namespace.Status.Phase == corev1.NamespaceActive, nil
}

// secretKey is the run's Secret key holding a credential; the helper copies credentials.* keys alone into files.
func secretKey(secret wherry.Secret) string {
	if secret.Env != "" {
		return "env." + secret.Env
	}
	return "credentials." + secret.File
}

// secretData is a run's Secret data, refused when a key or the values together pass the API server's limits.
func secretData(run wherry.Run) (map[string][]byte, error) {
	data := map[string][]byte{"prompt": run.Prompt}
	size := len(run.Prompt)
	for _, secret := range run.Credentials {
		key := secretKey(secret)
		if len(key) > maxKey {
			return nil, &wherry.InputTooLargeError{Name: cmp.Or(secret.File, secret.Env)}
		}
		data[key] = secret.Value
		size += len(secret.Value)
	}
	if size > maxSecret {
		return nil, errors.New("the prompt and credentials exceed the 1 MiB Secret limit")
	}
	for _, input := range run.Inputs {
		key := "inputs." + input.Name
		if size += len(input.Data); len(key) > maxKey || size > maxSecret {
			return nil, &wherry.InputTooLargeError{Name: input.Name}
		}
		data[key] = input.Data
	}
	return data, nil
}

func (adapter *Adapter) labels(run wherry.Run) map[string]string {
	return map[string]string{
		labelPool: adapter.pool, labelProfile: run.Profile, labelAgent: run.Agent, labelRun: run.ID,
	}
}

func locked() *corev1.SecurityContext {
	return &corev1.SecurityContext{AllowPrivilegeEscalation: new(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}, ReadOnlyRootFilesystem: new(true)}
}

// buildPod builds the run's pod, whose init container copies its material from the Secret name.
func (adapter *Adapter) buildPod(run wherry.Run, name string) *corev1.Pod {
	env := wherry.AgentEnv(adapter.runtime.Env)
	env["HOME"] = workPath
	env["WHERRY_INPUT_DIR"] = inputPath
	env["WHERRY_OUTPUT_DIR"] = outputPath
	env["WHERRY_CREDENTIALS_DIR"] = credentialsPath
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
	for _, secret := range run.Credentials {
		if secret.Env != "" {
			vars = append(vars, corev1.EnvVar{Name: secret.Env, ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: secretKey(secret)},
			}})
		}
	}
	var args []string
	for _, fetch := range run.Fetches {
		args = append(args, fetch.Name, strconv.FormatInt(fetch.Size, 10), fetch.SHA256)
	}
	if args != nil {
		args = append([]string{"--await"}, args...)
	}
	helperResources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
	}
	agentSecurity := locked()
	agentSecurity.ReadOnlyRootFilesystem = nil
	memoryLimit := resource.MustParse("1Mi")
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: adapter.namespace, Labels: adapter.labels(run),
			Annotations: map[string]string{annotationAttempt: strconv.FormatInt(run.Attempt, 10)},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			ShareProcessNamespace:         new(true),
			AutomountServiceAccountToken:  new(false),
			EnableServiceLinks:            new(false),
			TerminationGracePeriodSeconds: new(int64(graceSeconds)),
			NodeSelector:                  adapter.nodeSelector,
			Tolerations:                   adapter.tolerations,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: new(true), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				RunAsUser: adapter.uid, RunAsGroup: adapter.gid, SupplementalGroups: adapter.groups,
			},
			InitContainers: []corev1.Container{{
				Name: "inputs", Image: adapter.helperImage, Command: command("inputs"), Args: args,
				Resources: helperResources, SecurityContext: locked(),
				// The helper writes a failure's message to stderr, which becomes the termination message.
				TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
				VolumeMounts: []corev1.VolumeMount{
					{Name: "material", MountPath: "/run/wherry/material", ReadOnly: true},
					{Name: "work", MountPath: workPath},
					{Name: "credentials", MountPath: "/run/wherry/credentials"},
				},
			}},
			Containers: []corev1.Container{{
				Name: "agent", Image: adapter.runtime.Image, Command: adapter.runtime.Command, Args: adapter.runtime.Args, Env: vars,
				Resources: corev1.ResourceRequirements{Requests: adapter.resources, Limits: adapter.resources}, SecurityContext: agentSecurity,
				VolumeMounts: []corev1.VolumeMount{
					{Name: "work", MountPath: workPath},
					{Name: "credentials", MountPath: "/run/wherry/credentials", ReadOnly: true},
				},
			}, {
				Name: "outputs", Image: adapter.helperImage, Command: command("hold"),
				Resources: helperResources, SecurityContext: locked(),
				VolumeMounts: []corev1.VolumeMount{
					{Name: "work", MountPath: workPath, ReadOnly: true},
					{Name: "hold", MountPath: "/run/wherry/hold"},
				},
			}},
			Volumes: []corev1.Volume{
				{Name: "material", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: name, DefaultMode: new(int32(0o444))}}},
				{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: adapter.workLimit}}},
				{Name: "credentials", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: &memoryLimit}}},
				{Name: "hold", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: &memoryLimit}}},
			},
		},
	}
}

// Dispatch starts a turn idempotently and returns once its agent has started or its pod has ended.
func (adapter *Adapter) Dispatch(ctx context.Context, run wherry.Run) (wherry.Outcome, wherry.Handle, error) {
	began := time.Now()
	name := adapter.jobName(run.ID)
	data, err := secretData(run)
	if err != nil {
		return wherry.OutcomeNothingCreated, wherry.Handle{}, err
	}
	created := false
	switch existing, err := adapter.pods().Get(ctx, name, metav1.GetOptions{}); {
	case apierrors.IsNotFound(err):
		if err := adapter.upsertSecret(ctx, name, adapter.labels(run), data); err != nil {
			return adapter.notStarted(ctx, name, err)
		}
		pod, err := adapter.pods().Create(ctx, adapter.buildPod(run, name), metav1.CreateOptions{})
		switch {
		case apierrors.IsAlreadyExists(err):
		case err != nil:
			return adapter.notStarted(ctx, name, adapter.failed("create pod "+name, err))
		case !sameUser(pod):
			refused := errors.New("agent pod containers run as different users; set runtime.user")
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupWait)
			defer cancel()
			if err := errors.Join(adapter.deletePod(cleanupCtx, name), adapter.deleteSecret(cleanupCtx, name)); err != nil {
				return wherry.OutcomeUnknown, wherry.Handle{}, errors.Join(refused, err)
			}
			return wherry.OutcomeNothingCreated, wherry.Handle{}, refused
		default:
			created = true
		}
	case err != nil:
		return wherry.OutcomeUnknown, wherry.Handle{}, adapter.failed("read pod "+name, err)
	case !adapter.agentPod(existing):
		return wherry.OutcomeUnknown, wherry.Handle{}, errors.New("existing pod is not an agent pod")
	}
	if err := adapter.deliver(ctx, name, run.Fetches); err != nil {
		return wherry.OutcomeUnknown, wherry.Handle{}, err
	}
	handle, state, err := adapter.wait(ctx, name)
	if err != nil {
		return wherry.OutcomeUnknown, wherry.Handle{}, err
	}
	if created && state != wherry.JobNeverStarted {
		adapter.metrics.Placed(time.Since(began).Seconds())
	}
	return wherry.OutcomeCreated, handle, nil
}

// upsertSecret writes the run's Secret over any an earlier attempt left.
func (adapter *Adapter) upsertSecret(ctx context.Context, name string, labels map[string]string, data map[string][]byte) error {
	existing, err := adapter.secrets().Get(ctx, name, metav1.GetOptions{})
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: adapter.namespace, Labels: labels}, Type: corev1.SecretTypeOpaque, Data: data}
	switch {
	case apierrors.IsNotFound(err):
		if _, err := adapter.secrets().Create(ctx, secret, metav1.CreateOptions{}); err != nil {
			return adapter.failed("create secret "+name, err)
		}
	case err != nil:
		return adapter.failed("read secret "+name, err)
	default:
		secret.ResourceVersion = existing.ResourceVersion
		if _, err := adapter.secrets().Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
			return adapter.failed("update secret "+name, err)
		}
	}
	return nil
}

// notStarted deletes the Secret and returns OutcomeNothingCreated when no pod exists, else OutcomeUnknown, with cause.
func (adapter *Adapter) notStarted(ctx context.Context, name string, cause error) (wherry.Outcome, wherry.Handle, error) {
	if _, err := adapter.pods().Get(ctx, name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		adapter.handled("read pod "+name, err)
		return wherry.OutcomeUnknown, wherry.Handle{}, cause
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupWait)
	defer cancel()
	if err := adapter.deleteSecret(cleanupCtx, name); err != nil {
		return wherry.OutcomeNothingCreated, wherry.Handle{}, errors.Join(cause, err)
	}
	return wherry.OutcomeNothingCreated, wherry.Handle{}, cause
}

// sameUser reports whether every container of the admitted pod runs as one UID.
func sameUser(pod *corev1.Pod) bool {
	var uid *int64
	for _, container := range slices.Concat(pod.Spec.InitContainers, pod.Spec.Containers) {
		var user *int64
		if pod.Spec.SecurityContext != nil {
			user = pod.Spec.SecurityContext.RunAsUser
		}
		if container.SecurityContext != nil && container.SecurityContext.RunAsUser != nil {
			user = container.SecurityContext.RunAsUser
		}
		if user == nil || uid != nil && *uid != *user {
			return false
		}
		uid = user
	}
	return true
}

// deliver streams the run's fetches into the waiting init container; an error means the pod's fate is unknown.
func (adapter *Adapter) deliver(ctx context.Context, name string, fetches []wherry.Fetch) error {
	if len(fetches) == 0 {
		return nil
	}
	for {
		pod, err := adapter.pods().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			adapter.handled("read pod "+name, err)
		} else if inputs := status(pod.Status.InitContainerStatuses, "inputs"); initEnded(pod) {
			return nil
		} else if inputs != nil && inputs.State.Running != nil {
			break
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(waitPoll):
		}
	}
	for _, fetch := range fetches {
		ended, err := adapter.retried(ctx, name, "exec inputs put", func() error { return adapter.put(ctx, name, fetch) })
		switch {
		case ended || ctx.Err() != nil:
			return nil
		case err != nil:
			var exit *exitError
			var fetchErr *fetchError
			if errors.As(err, &exit) || errors.As(err, &fetchErr) {
				adapter.logger.Warn("input not fetched", "profile", adapter.name, "pod", name, "reason", err.Error())
			}
			if err := adapter.exec.exec(ctx, name, "inputs", command("inputs", "fail", fetch.Name), nil, io.Discard); err != nil {
				return adapter.failed("exec inputs fail", err)
			}
			return nil
		}
	}
	ended, err := adapter.retried(ctx, name, "exec inputs done", func() error {
		return adapter.exec.exec(ctx, name, "inputs", command("inputs", "done"), nil, io.Discard)
	})
	if err != nil && !ended && ctx.Err() == nil {
		return adapter.failed("exec inputs done", err)
	}
	return nil
}

// retried calls call up to putAttempts times while it fails in transport or finds the init container not yet waiting,
// and reports whether the init container ended in between.
func (adapter *Adapter) retried(ctx context.Context, name, op string, call func() error) (bool, error) {
	for attempt := 1; ; attempt++ {
		err := call()
		var exit *exitError
		var fetchErr *fetchError
		if err == nil || errors.As(err, &exit) && exit.code != helper.ExitNotWaiting || errors.As(err, &fetchErr) || ctx.Err() != nil {
			return false, err
		}
		adapter.handled(op, err)
		if attempt == putAttempts {
			return false, err
		}
		if pod, err := adapter.pods().Get(ctx, name, metav1.GetOptions{}); err != nil {
			adapter.handled("read pod "+name, err)
		} else if initEnded(pod) {
			return true, nil
		}
	}
}

// initEnded reports whether the init container has exited or the pod is settled before it.
func initEnded(pod *corev1.Pod) bool {
	inputs := status(pod.Status.InitContainerStatuses, "inputs")
	_, _, settled := judge(pod)
	return settled || inputs != nil && inputs.State.Terminated != nil
}

// put fetches one input, refusing a redirect and reading at most one byte past its size, into the helper's put.
func (adapter *Adapter) put(ctx context.Context, name string, fetch wherry.Fetch) error {
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
	return adapter.exec.exec(ctx, name, "inputs", command("inputs", "put", fetch.Name), io.LimitReader(response.Body, fetch.Size+1), io.Discard)
}

// handle is the version 1 encoding of wherry.Handle.Data.
type handle struct {
	V   int    `json:"v"`
	Pod string `json:"pod"`
	UID string `json:"uid"`
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

// current reads the handle's pod and refuses one that replaced it.
func (adapter *Adapter) current(ctx context.Context, h handle) (*corev1.Pod, error) {
	pod, err := adapter.pods().Get(ctx, h.Pod, metav1.GetOptions{})
	if err != nil {
		return nil, adapter.failed("read pod "+h.Pod, err)
	}
	if string(pod.UID) != h.UID {
		return nil, errReplaced
	}
	return pod, nil
}

// wait returns once the pod is settled; a wait cut short deletes the pod.
func (adapter *Adapter) wait(ctx context.Context, name string) (wherry.Handle, wherry.JobState, error) {
	for {
		pod, err := adapter.pods().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if ctx.Err() != nil {
				return wherry.Handle{}, 0, adapter.deleteTimedOut(ctx, name)
			}
			return wherry.Handle{}, 0, adapter.failed("read pod "+name, err)
		}
		if state, _, settled := judge(pod); settled {
			data, _ := json.Marshal(handle{V: 1, Pod: name, UID: string(pod.UID)}) // strings always encode
			return wherry.Handle{Data: data, Name: name}, state, nil
		}
		select {
		case <-ctx.Done():
			return wherry.Handle{}, 0, adapter.deleteTimedOut(ctx, name)
		case <-time.After(waitPoll):
		}
	}
}

func (adapter *Adapter) deleteTimedOut(ctx context.Context, name string) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupWait)
	defer cancel()
	if err := adapter.deletePod(cleanupCtx, name); err != nil {
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
func judge(pod *corev1.Pod) (wherry.JobState, wherry.Terminal, bool) {
	agent := status(pod.Status.ContainerStatuses, "agent")
	terminal := wherry.Terminal{Started: started(agent), Node: pod.Spec.NodeName}
	var ended *corev1.ContainerStateTerminated
	if agent != nil {
		ended = agent.State.Terminated
	}
	if ended != nil {
		terminal.ExitCode = int(ended.ExitCode)
		if ended.Signal != 0 {
			terminal.Signal = strconv.Itoa(int(ended.Signal))
		}
	}
	switch {
	case pod.DeletionTimestamp != nil:
		if ended == nil {
			terminal.ExitCode = -1
		}
		if terminal.Started {
			return wherry.JobEnded, terminal, true
		}
		terminal.Class, terminal.Reason = "pod", "the pod is being deleted"
		return wherry.JobNeverStarted, terminal, true
	case terminal.Started && ended == nil:
		return wherry.JobRunning, terminal, true
	case terminal.Started:
		return wherry.JobEnded, terminal, true
	}
	terminal.Class, terminal.Reason = neverStarted(pod, ended)
	return wherry.JobNeverStarted, terminal, terminal.Class != ""
}

// neverStarted returns the class and the platform's reason of a pod that ended, or cannot start, before its agent.
func neverStarted(pod *corev1.Pod, agent *corev1.ContainerStateTerminated) (string, string) {
	if agent != nil {
		return "start", cmp.Or(agent.Message, agent.Reason)
	}
	// An eviction or a lost node also ends the init container, which is not at fault.
	if pod.Status.Phase == corev1.PodFailed && pod.Status.Reason != "" {
		return "pod", strings.Join(slices.DeleteFunc([]string{pod.Status.Reason, pod.Status.Message}, func(s string) bool { return s == "" }), ": ")
	}
	if inputs := status(pod.Status.InitContainerStatuses, "inputs"); inputs != nil && inputs.State.Terminated != nil && inputs.State.Terminated.ExitCode != 0 {
		ended := inputs.State.Terminated
		if ended.ExitCode == helper.ExitFetch {
			return wherry.ClassFetch, ended.Message
		}
		return "setup", cmp.Or(ended.Message, ended.Reason)
	}
	for _, container := range slices.Concat(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses) {
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
func (adapter *Adapter) Lookup(ctx context.Context, runID string) (wherry.Job, error) {
	name := adapter.jobName(runID)
	pod, err := adapter.pods().Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return wherry.Job{}, nil
	}
	if err != nil {
		return wherry.Job{}, adapter.failed("read pod "+name, err)
	}
	if !adapter.agentPod(pod) {
		return wherry.Job{}, errors.New("existing pod is not an agent pod")
	}
	handle, state, err := adapter.wait(ctx, name)
	if err != nil {
		return wherry.Job{}, err
	}
	// A pod created without the annotation reads as attempt 0.
	attempt, _ := strconv.ParseInt(pod.Annotations[annotationAttempt], 10, 64)
	return wherry.Job{State: state, Handle: handle, Attempt: attempt, Profile: pod.Labels[labelProfile], Agent: pod.Labels[labelAgent]}, nil
}

// agentPod reports whether pod is this pool's agent pod.
func (adapter *Adapter) agentPod(pod *corev1.Pod) bool {
	return pod.Labels[labelPool] == adapter.pool && hasContainer(pod.Spec.InitContainers, "inputs") &&
		hasContainer(pod.Spec.Containers, "agent") && hasContainer(pod.Spec.Containers, "outputs")
}

func hasContainer(containers []corev1.Container, name string) bool {
	return slices.ContainsFunc(containers, func(container corev1.Container) bool { return container.Name == name })
}

// Follow streams the agent's stdout lines from a byte offset until it ends, then returns its terminal.
func (adapter *Adapter) Follow(ctx context.Context, encoded wherry.Handle, from int64, lines chan<- wherry.Line) (wherry.Terminal, error) {
	h, err := decodeHandle(encoded)
	if err != nil {
		return wherry.Terminal{}, err
	}
	pod, err := adapter.current(ctx, h)
	if err != nil {
		return wherry.Terminal{}, err
	}
	// The logs API answers 400 for a container that never started and cannot answer for a lost node.
	for {
		state, terminal, settled := judge(pod)
		if settled && (state == wherry.JobNeverStarted || pod.DeletionTimestamp != nil) {
			return terminal, nil
		}
		if settled {
			break
		}
		select {
		case <-ctx.Done():
			return wherry.Terminal{}, ctx.Err()
		case <-time.After(followPoll):
		}
		if pod, err = adapter.current(ctx, h); err != nil {
			return wherry.Terminal{}, err
		}
	}
	reader := LineReader{Lines: lines, Position: from}
	for {
		deleting, err := adapter.stream(ctx, h, &reader)
		if err != nil {
			return wherry.Terminal{}, err
		}
		if deleting != nil {
			_, terminal, _ := judge(deleting)
			return terminal, nil
		}
		for running := false; !running; {
			select {
			case <-ctx.Done():
				return wherry.Terminal{}, ctx.Err()
			case <-time.After(followPoll):
			}
			pod, err := adapter.current(ctx, h)
			if err != nil {
				return wherry.Terminal{}, err
			}
			agent := status(pod.Status.ContainerStatuses, "agent")
			switch _, terminal, _ := judge(pod); {
			case pod.DeletionTimestamp != nil:
				return terminal, nil
			case agent != nil && agent.State.Terminated != nil:
				if err := reader.Flush(ctx); err != nil {
					return wherry.Terminal{}, err
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
func (adapter *Adapter) stream(ctx context.Context, h handle, reader *LineReader) (*corev1.Pod, error) {
	streamCtx, stop := context.WithCancel(ctx)
	defer stop()
	body, err := adapter.pods().GetLogs(h.Pod, &corev1.PodLogOptions{Container: "agent", Follow: true}).Stream(streamCtx)
	if err != nil {
		return nil, adapter.failed("read logs of "+h.Pod, err)
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
			pod, err := adapter.pods().Get(ctx, h.Pod, metav1.GetOptions{})
			if err != nil {
				adapter.handled("read pod "+h.Pod, err)
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
	return (!settled || state == wherry.JobRunning) && pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed
}

// Stop signals the agent through the holder, SIGTERM then SIGKILL, and deletes the pod only if the agent outlives both.
func (adapter *Adapter) Stop(ctx context.Context, encoded wherry.Handle) error {
	h, err := decodeHandle(encoded)
	if err != nil {
		return err
	}
	pod, err := adapter.current(ctx, h)
	switch {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return err
	case !running(pod):
		return nil
	}
	for _, signal := range []string{"TERM", "KILL"} {
		if err := adapter.exec.exec(ctx, h.Pod, "outputs", command("stop", signal), nil, io.Discard); err != nil {
			adapter.handled("exec outputs stop "+signal, err)
			// A transport failure does not show the signal undelivered; only the helper's refusal does.
			var exit *exitError
			if errors.As(err, &exit) {
				break
			}
		}
		if stopped, err := adapter.stopped(ctx, h); err != nil || stopped {
			return err
		}
	}
	if err := adapter.deletePod(ctx, h.Pod); err != nil {
		return err
	}
	adapter.logger.Warn("agent outlived SIGKILL; pod deleted", "profile", adapter.name, "pod", h.Pod)
	return nil
}

// stopped polls the pod for up to the stop grace and reports whether the agent has ended.
func (adapter *Adapter) stopped(ctx context.Context, h handle) (bool, error) {
	poll := time.NewTicker(time.Second)
	defer poll.Stop()
	for deadline := time.Now().Add(stopGrace); time.Now().Before(deadline); {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-poll.C:
		}
		pod, err := adapter.pods().Get(ctx, h.Pod, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			return true, nil
		case err != nil:
			adapter.handled("read pod "+h.Pod, err)
		case !running(pod):
			return true, nil
		}
	}
	return false, nil
}

// Collect lists the outputs tree through the holder once the agent has ended, and reads what Select chooses.
func (adapter *Adapter) Collect(ctx context.Context, encoded wherry.Handle, limits wherry.Limits, outputs chan<- wherry.Output) error {
	h, err := decodeHandle(encoded)
	if err != nil {
		return err
	}
	var pod *corev1.Pod
	for {
		if pod, err = adapter.current(ctx, h); err != nil {
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
	if holder := status(pod.Status.ContainerStatuses, "outputs"); holder == nil || holder.State.Running == nil {
		return errors.New("the outputs container has ended")
	}
	var listing bytes.Buffer
	if err := adapter.exec.exec(ctx, h.Pod, "outputs", command("list", strconv.Itoa(limits.Files)), nil, &listing); err != nil {
		return adapter.failed("exec outputs list", err)
	}
	var entries []wherry.Output
	for line := range bytes.Lines(listing.Bytes()) {
		var entry helper.Entry
		if err := json.Unmarshal(line, &entry); err != nil {
			return errors.New("the outputs listing is not readable")
		}
		entries = append(entries, wherry.Output{Name: entry.Name, Size: entry.Size, Mode: entry.Mode})
	}
	selected, err := Select(entries, limits)
	if err != nil {
		return err
	}
	for _, chosen := range selected {
		output := chosen.Output
		switch {
		case chosen.Stream:
			output.Body = adapter.open(ctx, h.Pod, output.Name, output.Size)
		case chosen.Read:
			if output.Data, err = adapter.read(ctx, h.Pod, output.Name, output.Size); err != nil {
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

// body is a streamed output; closing it cancels its exec.
type body struct {
	*io.PipeReader
	cancel context.CancelFunc
}

func (b body) Close() error {
	b.cancel()
	return b.PipeReader.Close()
}

// open streams a listed regular file of size bytes through the holder.
func (adapter *Adapter) open(ctx context.Context, pod, name string, size int64) io.ReadCloser {
	ctx, cancel := context.WithCancel(ctx)
	reader, writer := io.Pipe()
	go func() {
		err := adapter.exec.exec(ctx, pod, "outputs", command("read", name, strconv.FormatInt(size, 10)), nil, writer)
		if err != nil && ctx.Err() == nil {
			err = adapter.failed("exec outputs read", err)
		}
		writer.CloseWithError(err)
	}()
	return body{PipeReader: reader, cancel: cancel}
}

// read reads a listed regular file and refuses a body of another length than its listing.
func (adapter *Adapter) read(ctx context.Context, pod, name string, size int64) ([]byte, error) {
	body := adapter.open(ctx, pod, name, size)
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

// Release deletes the run's Secret and ends the outputs holder, so the pod frees its node's requests.
func (adapter *Adapter) Release(ctx context.Context, runID string) error {
	name := adapter.jobName(runID)
	secretErr := adapter.deleteSecret(ctx, name)
	var podErr error
	switch pod, err := adapter.pods().Get(ctx, name, metav1.GetOptions{}); {
	case apierrors.IsNotFound(err):
	case err != nil:
		podErr = adapter.failed("read pod "+name, err)
	default:
		if holder := status(pod.Status.ContainerStatuses, "outputs"); holder != nil && holder.State.Running != nil && pod.DeletionTimestamp == nil {
			if err := adapter.exec.exec(ctx, name, "outputs", command("release"), nil, io.Discard); err != nil {
				adapter.handled("exec outputs release", err)
			}
		}
	}
	// A single error is returned as it is, so it reaches the receiver with its op and class.
	switch {
	case secretErr == nil:
		return podErr
	case podErr == nil:
		return secretErr
	}
	return errors.Join(secretErr, podErr)
}

// Purge deletes the run's pod at once, waits for it to go, then deletes its Secret.
func (adapter *Adapter) Purge(ctx context.Context, runID string) error {
	return adapter.purge(ctx, adapter.jobName(runID))
}

// purge waits for the pod to go, since a terminating pod still holds its name.
func (adapter *Adapter) purge(ctx context.Context, name string) error {
	if err := adapter.deletePod(ctx, name); err != nil {
		return err
	}
	for {
		_, err := adapter.pods().Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			break
		}
		if err != nil {
			return adapter.failed("read pod "+name, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(collectPoll):
		}
	}
	return adapter.deleteSecret(ctx, name)
}

// deletePod deletes a pod with grace 0, which removes it without waiting for its kubelet.
func (adapter *Adapter) deletePod(ctx context.Context, name string) error {
	if err := adapter.pods().Delete(ctx, name, metav1.DeleteOptions{GracePeriodSeconds: new(int64(0))}); err != nil && !apierrors.IsNotFound(err) {
		return adapter.failed("delete pod "+name, err)
	}
	return nil
}

func (adapter *Adapter) deleteSecret(ctx context.Context, name string) error {
	if err := adapter.secrets().Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return adapter.failed("delete secret "+name, err)
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
	if state, _, settled := judge(pod); !settled || state == wherry.JobRunning {
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

// Sweep purges this profile's pods ended for the grace period and deletes the Secrets no live run needs.
func (adapter *Adapter) Sweep(ctx context.Context) error {
	selector := metav1.ListOptions{LabelSelector: labelPool + "=" + adapter.pool}
	pods, err := adapter.pods().List(ctx, selector)
	if err != nil {
		return adapter.failed("list pods", err)
	}
	var errs []error
	pending, purged, deleted := 0, 0, 0
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
		if pod.Labels[labelProfile] != adapter.name {
			releasable[pod.Name] = expired
			continue
		}
		releasable[pod.Name] = true
		if !expired {
			pending++
			continue
		}
		if err := adapter.purge(ctx, pod.Name); err != nil {
			errs = append(errs, err)
			pending++
			continue
		}
		purged++
	}
	// Secrets are listed as metadata, so a list carries no value.
	secrets, err := adapter.meta.Resource(corev1.SchemeGroupVersion.WithResource("secrets")).Namespace(adapter.namespace).List(ctx, selector)
	if err != nil {
		errs = append(errs, adapter.failed("list secrets", err))
	} else {
		for i := range secrets.Items {
			secret := &secrets.Items[i]
			switch {
			case !ownName(secret):
				continue
			case present[secret.Name] && !releasable[secret.Name]:
				continue
			case !present[secret.Name] && time.Since(secret.CreationTimestamp.Time) < sweepGrace:
				continue
			}
			if err := adapter.deleteSecret(ctx, secret.Name); err != nil {
				errs = append(errs, err)
				continue
			}
			if secret.Labels[labelProfile] == adapter.name {
				deleted++
			}
		}
	}
	adapter.metrics.Pending(pending)
	adapter.metrics.Deleted("job", purged)
	adapter.metrics.Deleted("variable", deleted)
	return errors.Join(errs...)
}

// failed counts err from the API call op by class and wraps it; a not-found or cancelled call counts nothing.
func (adapter *Adapter) failed(op string, err error) error {
	var status apierrors.APIStatus
	var network net.Error
	class := "other"
	switch {
	case err == nil:
		return nil
	case apierrors.IsNotFound(err) || errors.Is(err, context.Canceled):
		return &wherry.Error{Op: op, Err: err}
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
	adapter.metrics.Failed(class)
	return &wherry.Error{Op: op, Class: class, Err: err}
}

// handled counts err as failed does and logs its op and class, never its text, which can hold a URL or a response body.
func (adapter *Adapter) handled(op string, err error) {
	var failure *wherry.Error
	if errors.As(adapter.failed(op, err), &failure) && failure.Class != "" {
		adapter.logger.Warn("kubernetes call failed", "profile", adapter.name, "op", failure.Op, "class", failure.Class)
	}
}

// Metrics reports the adapter's API errors, sweep deletions, cleanup-pending pods and placement latency.
func (adapter *Adapter) Metrics(context.Context) ([]wherry.Sample, error) {
	return adapter.metrics.Samples(), nil
}
