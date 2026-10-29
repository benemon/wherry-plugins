package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/benemon/wherry/pluginsdk"
	"github.com/benemon/wherry/pluginsdk/pluginsdktest"
	"github.com/hashicorp/go-hclog"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"
)

const (
	credentialEnv = "WHERRY_TEST_CREDENTIAL"
	testPool      = "build"
	testNamespace = "agents"
	testProfile   = "container"
	testToken     = "kube-token-secret"
	testImage     = "registry.example.com/agent:1.0"
	// poolPrefix is agent- and the first 12 hex digits of sha256("build").
	poolPrefix = "agent-44575cf5b285-"
	agentUID   = 1000690000
)

var getOptions = metav1.GetOptions{}

// The README's profiles and the tests' run as UID 1001, the receiver image's user.
func init() { receiverUID = 1001 }

func testConfig(address, caFile string) Config {
	return Config{
		Address: address, CAFile: caFile, Namespace: testNamespace, RunsClaim: "wherry-runs", RunsDir: "/var/lib/wherry/runs",
		Runtime: Runtime{
			Image: testImage, Command: []string{"/bin/sh", "-c"}, Args: []string{"exec agent-run < /wherry/prompt.txt"},
			Env: map[string]string{"WHERRY_TEST_ENV": "from-runtime"},
		},
	}
}

// build is newProfile's path to one profile named testProfile, from a Config rather than settings.
func build(config Config, pool, token string) (*Kubernetes, error) {
	client, err := newClient(config, token)
	if err != nil {
		return nil, err
	}
	return New(config, testProfile, pool, client, hclog.NewNullLogger())
}

func testRun(name string) pluginsdk.Run {
	return pluginsdk.Run{
		ID: "run-" + name, Profile: testProfile, Agent: "coder", Attempt: 2,
		Prompt: []byte("hello-prompt"), Credentials: []pluginsdk.Secret{{Name: "model", Env: credentialEnv, Value: []byte("test-credential")}},
	}
}

// fast shortens the profile's polls for one test.
func fast(t *testing.T) {
	t.Helper()
	saved := []time.Duration{waitPoll, followPoll, collectPoll}
	waitPoll, followPoll, collectPoll = 10*time.Millisecond, 10*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { waitPoll, followPoll, collectPoll = saved[0], saved[1], saved[2] })
}

// request is one request the fake API server answered.
type request struct {
	method, path, query string
	header              http.Header
	body                []byte
}

func (r request) String() string { return r.method + " " + r.path }

// fakeAPI is an API server holding pods, Secrets and one namespace. Its hooks run under mu; set them with set.
type fakeAPI struct {
	t      *testing.T
	server *httptest.Server
	caFile string

	mu       sync.Mutex
	pods     map[string]*corev1.Pod
	secrets  map[string]*corev1.Secret
	phase    corev1.NamespacePhase
	requests []request
	serial   int
	// kubelet edits a pod's status as each read of it sees it.
	kubelet func(*corev1.Pod)
	// admit edits a pod as admission does when it is created.
	admit func(*corev1.Pod)
	// intercept answers a request in place of the fake when it returns true.
	intercept func(http.ResponseWriter, *http.Request) bool
	// logs answers log requests, outside mu.
	logs http.HandlerFunc
	// linger is how many reads a deleted pod survives.
	linger    int
	lingering map[string]int
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	api := &fakeAPI{t: t, pods: map[string]*corev1.Pod{}, secrets: map[string]*corev1.Secret{}, phase: corev1.NamespaceActive,
		kubelet: startAgent, admit: assignUser, lingering: map[string]int{}}
	mux := http.NewServeMux()
	const namespace = "/api/v1/namespaces/{namespace}"
	mux.HandleFunc("GET "+namespace, api.namespace)
	mux.HandleFunc("POST "+namespace+"/pods", api.createPod)
	mux.HandleFunc("GET "+namespace+"/pods", api.listPods)
	mux.HandleFunc("GET "+namespace+"/pods/{name}", api.getPod)
	mux.HandleFunc("DELETE "+namespace+"/pods/{name}", api.deletePod)
	mux.HandleFunc("POST "+namespace+"/secrets", api.createSecret)
	mux.HandleFunc("GET "+namespace+"/secrets", api.listSecrets)
	mux.HandleFunc("GET "+namespace+"/secrets/{name}", api.getSecret)
	mux.HandleFunc("PUT "+namespace+"/secrets/{name}", api.updateSecret)
	mux.HandleFunc("DELETE "+namespace+"/secrets/{name}", api.deleteSecret)
	api.server = httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// client-go writes protobuf; the fake reads and records JSON.
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/vnd.kubernetes.protobuf") {
			object, _, err := scheme.Codecs.UniversalDeserializer().Decode(body, nil, nil)
			if err != nil {
				t.Errorf("decode %s %s: %v", r.Method, r.URL.Path, err)
			}
			body, _ = json.Marshal(object)
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		api.mu.Lock()
		api.requests = append(api.requests, request{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), body})
		handled := api.intercept != nil && api.intercept(response, r)
		logs := api.logs
		api.mu.Unlock()
		switch {
		case handled:
		case strings.HasSuffix(r.URL.Path, "/log") && logs != nil:
			logs(response, r)
		case strings.HasSuffix(r.URL.Path, "/log"):
			apiStatus(response, http.StatusBadRequest, metav1.StatusReasonBadRequest, "container is waiting to start")
		default:
			mux.ServeHTTP(response, r)
		}
	}))
	t.Cleanup(api.server.Close)
	api.caFile = filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(api.caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: api.server.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return api
}

// profile builds a plugin profile against the fake, with a runs directory of its own.
func (api *fakeAPI) profile(t *testing.T) *Kubernetes {
	t.Helper()
	config := testConfig(api.server.URL, api.caFile)
	config.RunsDir = t.TempDir()
	profile, err := build(config, testPool, testToken)
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

func (api *fakeAPI) set(edit func()) {
	api.mu.Lock()
	defer api.mu.Unlock()
	edit()
}

func (api *fakeAPI) recorded() []request {
	api.mu.Lock()
	defer api.mu.Unlock()
	return slices.Clone(api.requests)
}

// put stores pod as created, with a UID.
func (api *fakeAPI) put(pod *corev1.Pod) *corev1.Pod {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.serial++
	pod.Namespace = testNamespace
	if pod.UID == "" {
		pod.UID = types.UID(fmt.Sprintf("uid-%d", api.serial))
	}
	if pod.CreationTimestamp.IsZero() {
		pod.CreationTimestamp = metav1.Now()
	}
	api.pods[pod.Name] = pod
	return pod
}

func (api *fakeAPI) pod(name string) *corev1.Pod {
	api.mu.Lock()
	defer api.mu.Unlock()
	if pod, ok := api.pods[name]; ok {
		return pod.DeepCopy()
	}
	return nil
}

func (api *fakeAPI) secret(name string) *corev1.Secret {
	api.mu.Lock()
	defer api.mu.Unlock()
	if secret, ok := api.secrets[name]; ok {
		return secret.DeepCopy()
	}
	return nil
}

func writeJSON(response http.ResponseWriter, code int, value any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(code)
	_ = json.NewEncoder(response).Encode(value)
}

func apiStatus(response http.ResponseWriter, code int, reason metav1.StatusReason, message string) {
	writeJSON(response, code, &metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"}, Status: metav1.StatusFailure, Code: int32(code), Reason: reason, Message: message})
}

func notFound(response http.ResponseWriter, kind, name string) {
	apiStatus(response, http.StatusNotFound, metav1.StatusReasonNotFound, kind+" \""+name+"\" not found")
}

func (api *fakeAPI) namespace(response http.ResponseWriter, r *http.Request) {
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.phase == "" {
		notFound(response, "namespaces", r.PathValue("namespace"))
		return
	}
	writeJSON(response, http.StatusOK, &corev1.Namespace{TypeMeta: metav1.TypeMeta{Kind: "Namespace", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{Name: r.PathValue("namespace")}, Status: corev1.NamespaceStatus{Phase: api.phase}})
}

func (api *fakeAPI) createPod(response http.ResponseWriter, r *http.Request) {
	var pod corev1.Pod
	if err := json.NewDecoder(r.Body).Decode(&pod); err != nil {
		apiStatus(response, http.StatusBadRequest, metav1.StatusReasonBadRequest, err.Error())
		return
	}
	api.mu.Lock()
	_, exists := api.pods[pod.Name]
	admit := api.admit
	api.mu.Unlock()
	if exists {
		apiStatus(response, http.StatusConflict, metav1.StatusReasonAlreadyExists, "pods \""+pod.Name+"\" already exists")
		return
	}
	pod.TypeMeta = metav1.TypeMeta{Kind: "Pod", APIVersion: "v1"}
	if admit != nil {
		admit(&pod)
	}
	writeJSON(response, http.StatusCreated, api.put(&pod))
}

func (api *fakeAPI) read(name string) (*corev1.Pod, bool) {
	pod, ok := api.pods[name]
	if !ok {
		return nil, false
	}
	if left, deleting := api.lingering[name]; deleting {
		if left == 0 {
			delete(api.pods, name)
			delete(api.lingering, name)
			return nil, false
		}
		api.lingering[name] = left - 1
	}
	if api.kubelet != nil {
		api.kubelet(pod)
	}
	pod.TypeMeta = metav1.TypeMeta{Kind: "Pod", APIVersion: "v1"}
	return pod, true
}

func (api *fakeAPI) getPod(response http.ResponseWriter, r *http.Request) {
	api.mu.Lock()
	defer api.mu.Unlock()
	pod, ok := api.read(r.PathValue("name"))
	if !ok {
		notFound(response, "pods", r.PathValue("name"))
		return
	}
	writeJSON(response, http.StatusOK, pod)
}

func (api *fakeAPI) listPods(response http.ResponseWriter, _ *http.Request) {
	api.mu.Lock()
	defer api.mu.Unlock()
	list := corev1.PodList{TypeMeta: metav1.TypeMeta{Kind: "PodList", APIVersion: "v1"}}
	for _, name := range slices.Sorted(maps.Keys(api.pods)) {
		if pod, ok := api.read(name); ok {
			list.Items = append(list.Items, *pod)
		}
	}
	writeJSON(response, http.StatusOK, &list)
}

func (api *fakeAPI) deletePod(response http.ResponseWriter, r *http.Request) {
	api.mu.Lock()
	defer api.mu.Unlock()
	name := r.PathValue("name")
	pod, ok := api.pods[name]
	if !ok {
		notFound(response, "pods", name)
		return
	}
	if api.linger > 0 {
		if _, deleting := api.lingering[name]; !deleting {
			api.lingering[name] = api.linger
		}
		pod.DeletionTimestamp = new(metav1.Now())
	} else {
		delete(api.pods, name)
	}
	writeJSON(response, http.StatusOK, pod)
}

func (api *fakeAPI) createSecret(response http.ResponseWriter, r *http.Request) {
	var secret corev1.Secret
	if err := json.NewDecoder(r.Body).Decode(&secret); err != nil {
		apiStatus(response, http.StatusBadRequest, metav1.StatusReasonBadRequest, err.Error())
		return
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if _, exists := api.secrets[secret.Name]; exists {
		apiStatus(response, http.StatusConflict, metav1.StatusReasonAlreadyExists, "secrets \""+secret.Name+"\" already exists")
		return
	}
	api.serial++
	secret.TypeMeta = metav1.TypeMeta{Kind: "Secret", APIVersion: "v1"}
	secret.ResourceVersion = strconv.Itoa(api.serial)
	secret.CreationTimestamp = metav1.Now()
	api.secrets[secret.Name] = &secret
	writeJSON(response, http.StatusCreated, &secret)
}

func (api *fakeAPI) getSecret(response http.ResponseWriter, r *http.Request) {
	api.mu.Lock()
	defer api.mu.Unlock()
	secret, ok := api.secrets[r.PathValue("name")]
	if !ok {
		notFound(response, "secrets", r.PathValue("name"))
		return
	}
	writeJSON(response, http.StatusOK, secret)
}

func (api *fakeAPI) updateSecret(response http.ResponseWriter, r *http.Request) {
	var secret corev1.Secret
	if err := json.NewDecoder(r.Body).Decode(&secret); err != nil {
		apiStatus(response, http.StatusBadRequest, metav1.StatusReasonBadRequest, err.Error())
		return
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	existing, ok := api.secrets[secret.Name]
	switch {
	case !ok:
		notFound(response, "secrets", secret.Name)
		return
	case existing.ResourceVersion != secret.ResourceVersion:
		apiStatus(response, http.StatusConflict, metav1.StatusReasonConflict, "the object has been modified")
		return
	}
	api.serial++
	secret.TypeMeta = existing.TypeMeta
	secret.ResourceVersion = strconv.Itoa(api.serial)
	secret.CreationTimestamp = existing.CreationTimestamp
	api.secrets[secret.Name] = &secret
	writeJSON(response, http.StatusOK, &secret)
}

func (api *fakeAPI) deleteSecret(response http.ResponseWriter, r *http.Request) {
	api.mu.Lock()
	defer api.mu.Unlock()
	secret, ok := api.secrets[r.PathValue("name")]
	if !ok {
		notFound(response, "secrets", r.PathValue("name"))
		return
	}
	delete(api.secrets, secret.Name)
	writeJSON(response, http.StatusOK, secret)
}

// listSecrets answers as metadata when the client asks for it, else with every value.
func (api *fakeAPI) listSecrets(response http.ResponseWriter, r *http.Request) {
	api.mu.Lock()
	defer api.mu.Unlock()
	if strings.Contains(r.Header.Get("Accept"), "as=PartialObjectMetadataList") {
		list := metav1.PartialObjectMetadataList{TypeMeta: metav1.TypeMeta{Kind: "PartialObjectMetadataList", APIVersion: "meta.k8s.io/v1"}}
		for _, name := range slices.Sorted(maps.Keys(api.secrets)) {
			list.Items = append(list.Items, metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{Kind: "PartialObjectMetadata", APIVersion: "meta.k8s.io/v1"}, ObjectMeta: api.secrets[name].ObjectMeta})
		}
		writeJSON(response, http.StatusOK, &list)
		return
	}
	list := corev1.SecretList{TypeMeta: metav1.TypeMeta{Kind: "SecretList", APIVersion: "v1"}}
	for _, name := range slices.Sorted(maps.Keys(api.secrets)) {
		list.Items = append(list.Items, *api.secrets[name])
	}
	writeJSON(response, http.StatusOK, &list)
}

// assignUser admits a pod as restricted-v2 does, giving its container the namespace's first UID.
func assignUser(pod *corev1.Pod) {
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].SecurityContext == nil {
			pod.Spec.Containers[i].SecurityContext = &corev1.SecurityContext{}
		}
		if pod.Spec.Containers[i].SecurityContext.RunAsUser == nil {
			pod.Spec.Containers[i].SecurityContext.RunAsUser = new(int64(agentUID))
		}
	}
}

func runningStatus(name string) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: name, Started: new(true), State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}}}
}

func exited(name string, code int32) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: name, Started: new(false), State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		ExitCode: code, StartedAt: metav1.NewTime(time.Now().Add(-time.Minute)), FinishedAt: metav1.Now(),
	}}}
}

func waiting(name, reason, message string) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: message}}}
}

// status sets a pod's phase and container statuses.
func setStatus(pod *corev1.Pod, phase corev1.PodPhase, containers ...corev1.ContainerStatus) {
	pod.Spec.NodeName = "worker-1"
	pod.Status.Phase = phase
	pod.Status.ContainerStatuses = containers
}

// startAgent is a kubelet that starts a new pod's agent at once.
func startAgent(pod *corev1.Pod) {
	if pod.Status.Phase == "" {
		setStatus(pod, corev1.PodRunning, runningStatus("agent"))
	}
}

// agentPod is a pod as buildPod makes it for testRun(name), created and with status set by edit.
func (api *fakeAPI) agentPod(t *testing.T, profile *Kubernetes, name string, edit func(*corev1.Pod)) *corev1.Pod {
	t.Helper()
	run := testRun(name)
	pod := profile.buildPod(run, profile.jobName(run.ID))
	assignUser(pod)
	if edit != nil {
		edit(pod)
	}
	return api.put(pod)
}

func handleOf(pod *corev1.Pod) pluginsdk.Handle {
	data, _ := json.Marshal(handle{V: 1, Pod: pod.Name, UID: string(pod.UID)})
	return pluginsdk.Handle{Data: data, Name: pod.Name}
}

func paths(requests []request, method string) []string {
	var matched []string
	for _, r := range requests {
		if method == "" || r.method == method {
			matched = append(matched, r.String())
		}
	}
	return matched
}

// samples returns the profile's samples by family and label value.
func samples(t *testing.T, profile *Kubernetes) map[string]pluginsdk.Sample {
	t.Helper()
	reported, err := profile.Metrics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]pluginsdk.Sample{}
	for _, sample := range reported {
		byName[sample.Family+"/"+sample.Labels["class"]+sample.Labels["kind"]] = sample
	}
	return byName
}

func podJSON(t *testing.T, pod *corev1.Pod) string {
	t.Helper()
	encoded, err := json.Marshal(pod)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func container(pod *corev1.Pod, name string) *corev1.Container {
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == name {
			return &pod.Spec.Containers[i]
		}
	}
	return nil
}

func mounts(c *corev1.Container) []string {
	var names []string
	for _, mount := range c.VolumeMounts {
		names = append(names, mount.Name)
	}
	return names
}

func volume(pod *corev1.Pod, name string) *corev1.Volume {
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == name {
			return &pod.Spec.Volumes[i]
		}
	}
	return nil
}

func envOf(c *corev1.Container) map[string]string {
	env := map[string]string{}
	for _, variable := range c.Env {
		env[variable.Name] = variable.Value
	}
	return env
}

func plainProfile(t *testing.T, edit func(*Config)) *Kubernetes {
	t.Helper()
	config := testConfig("https://127.0.0.1:1", "")
	config.RunsDir = t.TempDir()
	if edit != nil {
		edit(&config)
	}
	profile, err := build(config, testPool, testToken)
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

func TestBuildPodFromRuntime(t *testing.T) {
	profile := plainProfile(t, func(c *Config) { c.Runtime.User = "1001:1002" })
	run := testRun("build")
	run.Credentials = append(run.Credentials, pluginsdk.Secret{Name: "deploy", File: "id_ed25519", Value: []byte("key")})
	name := profile.jobName(run.ID)
	pod := profile.buildPod(run, name)
	wantLabels := map[string]string{labelPool: testPool, "wherry.io/profile": testProfile, labelAgent: "coder", labelRun: run.ID}
	if pod.Name != poolPrefix+"run-build" || pod.Namespace != testNamespace || !maps.Equal(pod.Labels, wantLabels) ||
		pod.Annotations[annotationAttempt] != "2" || pod.Spec.RestartPolicy != corev1.RestartPolicyNever || *pod.Spec.TerminationGracePeriodSeconds != 15 {
		t.Fatalf("unexpected pod identity: %#v", pod.ObjectMeta)
	}
	if len(pod.Spec.Containers) != 1 || pod.Spec.InitContainers != nil || pod.Spec.ShareProcessNamespace != nil {
		t.Fatalf("containers %v, init containers %v, shared process namespace %v; want the agent alone", pod.Spec.Containers, pod.Spec.InitContainers, pod.Spec.ShareProcessNamespace)
	}
	agent := container(pod, "agent")
	if agent.Image != testImage || !slices.Equal(agent.Command, []string{"/bin/sh", "-c"}) || !slices.Equal(agent.Args, []string{"exec agent-run < /wherry/prompt.txt"}) {
		t.Fatalf("runtime not carried: %#v", agent)
	}
	wantEnv := maps.Clone(pluginsdk.AgentEnvironment)
	maps.Copy(wantEnv, map[string]string{"WHERRY_TEST_ENV": "from-runtime", "HOME": "/wherry", "WHERRY_RUN_DIR": name,
		"WHERRY_INPUT_DIR": "/wherry/inputs", "WHERRY_OUTPUT_DIR": "/wherry/outputs", "WHERRY_CREDENTIALS_DIR": "/run/wherry/credentials/files", credentialEnv: ""})
	if env := envOf(agent); !maps.Equal(env, wantEnv) {
		t.Fatalf("agent env %v; want %v", env, wantEnv)
	}
	last := agent.Env[len(agent.Env)-1]
	if last.Name != credentialEnv || last.ValueFrom.SecretKeyRef.Name != name || last.ValueFrom.SecretKeyRef.Key != "env."+credentialEnv {
		t.Fatalf("credential variable %#v; want a reference to the Secret's credential key", last)
	}
	wantMounts := []corev1.VolumeMount{
		{Name: "runs", MountPath: "/wherry", SubPathExpr: "runs/$(WHERRY_RUN_DIR)"},
		{Name: "material", MountPath: "/wherry/prompt.txt", SubPath: "prompt", ReadOnly: true},
		{Name: "material", MountPath: "/run/wherry/credentials/files", SubPath: "files", ReadOnly: true},
	}
	if !reflect.DeepEqual(agent.VolumeMounts, wantMounts) {
		t.Fatalf("agent mounts %+v; want %+v", agent.VolumeMounts, wantMounts)
	}
	if runs := volume(pod, "runs"); len(pod.Spec.Volumes) != 2 || runs.PersistentVolumeClaim == nil || runs.PersistentVolumeClaim.ClaimName != "wherry-runs" || runs.PersistentVolumeClaim.ReadOnly {
		t.Fatalf("volumes %+v; want the runs claim and the material", pod.Spec.Volumes)
	}
	wantItems := []corev1.KeyToPath{{Key: "prompt", Path: "prompt"}, {Key: "credentials.id_ed25519", Path: "files/id_ed25519"}}
	if material := volume(pod, "material").Secret; material.SecretName != name || *material.DefaultMode != 0o440 || !reflect.DeepEqual(material.Items, wantItems) {
		t.Fatalf("material volume %+v; want the prompt and file credentials of the Secret at mode 0440", material)
	}
	if security := pod.Spec.SecurityContext; security.FSGroup == nil || *security.FSGroup != 1002 || *security.RunAsUser != 1001 {
		t.Fatalf("pod security %+v; want fsGroup the agent's group", security)
	}
	run.Credentials = nil
	if env := container(profile.buildPod(run, name), "agent").Env; slices.ContainsFunc(env, func(v corev1.EnvVar) bool { return v.Name == credentialEnv }) {
		t.Fatal("credential variable set for a run without a credential")
	}
}

func TestBuildPodDefaultsAgentResources(t *testing.T) {
	pod := plainProfile(t, nil).buildPod(testRun("defaults"), "agent-x")
	want := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("2Gi")}
	agent := container(pod, "agent")
	for _, list := range []corev1.ResourceList{agent.Resources.Requests, agent.Resources.Limits} {
		if len(list) != len(want) || !list.Cpu().Equal(want[corev1.ResourceCPU]) || !list.Memory().Equal(want[corev1.ResourceMemory]) {
			t.Fatalf("agent resources %v; want requests and limits of 500m and 2Gi", agent.Resources)
		}
	}
	pod = plainProfile(t, func(c *Config) { c.Runtime.Resources = Resources{CPU: "2", Memory: "4Gi"} }).buildPod(testRun("set"), "agent-x")
	if agent := container(pod, "agent"); agent.Resources.Limits.Cpu().String() != "2" || agent.Resources.Requests.Memory().String() != "4Gi" {
		t.Fatalf("configured resources %v", agent.Resources)
	}
}

func TestBuildPodSetsStorageLimitsOnlyWhenConfigured(t *testing.T) {
	agent := container(plainProfile(t, nil).buildPod(testRun("unset"), "agent-x"), "agent")
	if _, set := agent.Resources.Requests[corev1.ResourceEphemeralStorage]; set {
		t.Fatalf("storage limits set unconfigured: %v", agent.Resources)
	}
	agent = container(plainProfile(t, func(c *Config) { c.Runtime.Resources.EphemeralStorage = "30Gi" }).buildPod(testRun("set"), "agent-x"), "agent")
	if agent.Resources.Requests.StorageEphemeral().String() != "30Gi" || agent.Resources.Limits.StorageEphemeral().String() != "30Gi" {
		t.Fatalf("storage limits not set: %v", agent.Resources)
	}
}

func TestPodSpecGivesEveryContainerResources(t *testing.T) {
	pod := plainProfile(t, nil).buildPod(testRun("resources"), "agent-x")
	for _, c := range pod.Spec.Containers {
		for _, list := range []corev1.ResourceList{c.Resources.Requests, c.Resources.Limits} {
			if list.Cpu().IsZero() || list.Memory().IsZero() {
				t.Fatalf("container %s resources %v; want cpu and memory requests and limits", c.Name, c.Resources)
			}
		}
	}
}

func TestPodSpecSecurity(t *testing.T) {
	pod := plainProfile(t, nil).buildPod(testRun("security"), "agent-x")
	spec := pod.Spec
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken || spec.EnableServiceLinks == nil || *spec.EnableServiceLinks ||
		spec.ShareProcessNamespace != nil || spec.SecurityContext.RunAsNonRoot == nil || !*spec.SecurityContext.RunAsNonRoot ||
		spec.SecurityContext.SeccompProfile == nil || spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("pod security %#v", spec)
	}
	for _, c := range spec.Containers {
		s := c.SecurityContext
		if s == nil || s.AllowPrivilegeEscalation == nil || *s.AllowPrivilegeEscalation || s.Capabilities == nil || !slices.Equal(s.Capabilities.Drop, []corev1.Capability{"ALL"}) {
			t.Fatalf("container %s security %#v", c.Name, s)
		}
	}
}

func TestPodSpecHoldsNoMaterial(t *testing.T) {
	profile := plainProfile(t, nil)
	run := testRun("material")
	run.Model = "sonnet"
	run.Inputs = []pluginsdk.File{{Name: "notes.md", Data: []byte("input-bytes-secret")}}
	run.Credentials = append(run.Credentials, pluginsdk.Secret{Name: "deploy", File: "id_ed25519", Value: []byte("task-credential-secret")})
	run.Fetches = []pluginsdk.Fetch{{Name: "data.csv", URL: "https://storage.example.com/data.csv?X-Amz-Signature=signature-secret", Size: 3, SHA256: strings.Repeat("b", 64)}}
	pod := profile.buildPod(run, profile.jobName(run.ID))
	encoded := podJSON(t, pod)
	for _, secret := range []string{"hello-prompt", "test-credential", "input-bytes-secret", "task-credential-secret", "signature-secret", "storage.example.com"} {
		if strings.Contains(encoded, secret) {
			t.Fatalf("pod spec holds %q: %s", secret, encoded)
		}
	}
	for _, variable := range container(pod, "agent").Env {
		if variable.Name == credentialEnv && (variable.Value != "" || variable.ValueFrom == nil || variable.ValueFrom.SecretKeyRef == nil) {
			t.Fatalf("credential variable %#v; want a Secret reference", variable)
		}
	}
}

func TestAgentEnvironmentReachesTheAgent(t *testing.T) {
	profile := plainProfile(t, func(c *Config) {
		c.Runtime.Env = map[string]string{"GIT_SSH_COMMAND": "ssh -o StrictHostKeyChecking=no", "GIT_CONFIG_GLOBAL": "/home/agent/.gitconfig", "LANG": "C.UTF-8", "HOME": "/root"}
	})
	env := envOf(container(profile.buildPod(testRun("env"), "agent-x"), "agent"))
	for name, value := range pluginsdk.AgentEnvironment {
		if env[name] != value {
			t.Fatalf("%s=%q; want %q", name, env[name], value)
		}
	}
	for _, name := range pluginsdk.AgentUnset {
		if _, set := env[name]; set {
			t.Fatalf("%s reached the agent", name)
		}
	}
	if env["LANG"] != "C.UTF-8" || env["HOME"] != "/wherry" {
		t.Fatalf("LANG=%q HOME=%q; want the runtime's LANG and the plugin's HOME", env["LANG"], env["HOME"])
	}
}

func TestPodEnvCarriesTheModelOnlyWhenSet(t *testing.T) {
	profile := plainProfile(t, nil)
	for model, want := range map[string]bool{"": false, "sonnet": true} {
		run := testRun("model")
		run.Model = model
		pod := profile.buildPod(run, "agent-x")
		agent := container(pod, "agent")
		if value, set := envOf(agent)["WHERRY_MODEL"]; set != want || value != model {
			t.Fatalf("model %q: WHERRY_MODEL=%q set=%t, want set=%t", model, value, set, want)
		}
		if model != "" && (slices.Contains(agent.Args, model) || slices.Contains(agent.Command, model)) {
			t.Fatalf("model %q reached the agent's command line", model)
		}
	}
}

func TestBuildPodLeavesUnsetPlacementToKubernetes(t *testing.T) {
	spec := plainProfile(t, nil).buildPod(testRun("defaults"), "agent-x").Spec
	if spec.NodeSelector != nil || spec.Tolerations != nil || spec.SecurityContext.RunAsUser != nil || spec.SecurityContext.RunAsGroup != nil {
		t.Fatalf("unset placement or user set: %#v", spec)
	}
	spec = plainProfile(t, func(c *Config) {
		c.NodeSelector = map[string]string{"wherry.io/agents": "true"}
		c.Tolerations = []Toleration{{Key: "dedicated", Operator: "Equal", Value: "agents", Effect: "NoSchedule"}}
		c.Runtime.User = "1001:1002"
	}).buildPod(testRun("set"), "agent-x").Spec
	if spec.NodeSelector["wherry.io/agents"] != "true" || len(spec.Tolerations) != 1 || spec.Tolerations[0].Key != "dedicated" ||
		*spec.SecurityContext.RunAsUser != 1001 || *spec.SecurityContext.RunAsGroup != 1002 {
		t.Fatalf("configured placement or user not carried: %#v", spec)
	}
}

func TestDirectoriesAreFixed(t *testing.T) {
	want := pluginsdk.Directories{Input: "/wherry/inputs", Output: "/wherry/outputs", Credentials: "/run/wherry/credentials/files"}
	if got := plainProfile(t, nil).Directories(); got != want {
		t.Fatalf("directories %+v; want %+v", got, want)
	}
	env := envOf(container(plainProfile(t, nil).buildPod(testRun("dirs"), "agent-x"), "agent"))
	if env["WHERRY_INPUT_DIR"] != want.Input || env["WHERRY_OUTPUT_DIR"] != want.Output || env["WHERRY_CREDENTIALS_DIR"] != want.Credentials {
		t.Fatalf("env %v; want the directories", env)
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

func TestNewValidatesConfig(t *testing.T) {
	for name, edit := range map[string]func(*Config){
		"root user":                 func(c *Config) { c.Runtime.User = "0" },
		"user name":                 func(c *Config) { c.Runtime.User = "agent" },
		"leading zero":              func(c *Config) { c.Runtime.User = "01001" },
		"root group":                func(c *Config) { c.Runtime.User = "1001:0" },
		"user past 2^31":            func(c *Config) { c.Runtime.User = "2147483648" },
		"http address":              func(c *Config) { c.Address = "http://127.0.0.1:6443" },
		"no namespace":              func(c *Config) { c.Namespace = "" },
		"namespace not a label":     func(c *Config) { c.Namespace = "Agents_1" },
		"no runs claim":             func(c *Config) { c.RunsClaim = "" },
		"runs claim not a name":     func(c *Config) { c.RunsClaim = "Runs_1" },
		"relative runs dir":         func(c *Config) { c.RunsDir = "runs" },
		"runs_floor not a quantity": func(c *Config) { c.RunsFloor = "lots" },
		"another user than ours":    func(c *Config) { c.Runtime.User = "1002" },
		"no image":                  func(c *Config) { c.Runtime.Image = "" },
		"image with whitespace":     func(c *Config) { c.Runtime.Image = "agent :1" },
		"env name":                  func(c *Config) { c.Runtime.Env = map[string]string{"A-B": "1"} },
		"cpu not a quantity":        func(c *Config) { c.Runtime.Resources.CPU = "fast" },
		"memory not a quantity":     func(c *Config) { c.Runtime.Resources.Memory = "2 GiB" },
		"node selector key":         func(c *Config) { c.NodeSelector = map[string]string{"bad key": "x"} },
		"node selector value":       func(c *Config) { c.NodeSelector = map[string]string{"zone": "a b"} },
		"toleration operator":       func(c *Config) { c.Tolerations = []Toleration{{Key: "k", Operator: "In", Effect: "NoSchedule"}} },
		"toleration effect":         func(c *Config) { c.Tolerations = []Toleration{{Key: "k", Operator: "Exists", Effect: "Never"}} },
		"toleration exists and value": func(c *Config) {
			c.Tolerations = []Toleration{{Key: "k", Operator: "Exists", Value: "v", Effect: "NoSchedule"}}
		},
	} {
		config := testConfig("https://127.0.0.1:6443", "")
		edit(&config)
		if _, err := build(config, testPool, testToken); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	if _, err := build(testConfig("https://127.0.0.1:6443", ""), testPool, ""); err == nil || err.Error() != "address needs a platform credential" {
		t.Fatalf("address without a platform credential: %v", err)
	}
	config := testConfig("https://127.0.0.1:6443", "")
	config.Runtime.User = "1001"
	profile, err := build(config, testPool, testToken)
	if err != nil {
		t.Fatalf("the receiver's UID refused: %v", err)
	}
	if profile.floor != 1<<30 || profile.runs != "/var/lib/wherry/runs/runs" {
		t.Fatalf("floor %d, runs %q; want 1Gi and runs below runs_dir", profile.floor, profile.runs)
	}
}

func TestNewRefusesGroupsKubernetesCannotTake(t *testing.T) {
	groupErr := "runtime group must be a GID below 2^31 other than root"
	supplementaryErr := "runtime supplementary_groups must be GIDs below 2^31 other than root"
	for name, test := range map[string]struct {
		edit func(*Runtime)
		want string
	}{
		"group name":                {func(r *Runtime) { r.Group = "agents" }, groupErr},
		"root group":                {func(r *Runtime) { r.Group = "0" }, groupErr},
		"group leading zero":        {func(r *Runtime) { r.Group = "01002" }, groupErr},
		"group past 2^31":           {func(r *Runtime) { r.Group = "2147483648" }, groupErr},
		"supplementary group name":  {func(r *Runtime) { r.SupplementaryGroups = []string{"1003", "video"} }, supplementaryErr},
		"root supplementary group":  {func(r *Runtime) { r.SupplementaryGroups = []string{"0"} }, supplementaryErr},
		"supplementary past 2^31":   {func(r *Runtime) { r.SupplementaryGroups = []string{"2147483648"} }, supplementaryErr},
		"group beside a user's GID": {func(r *Runtime) { r.User, r.Group = "1001:1001", "1002" }, "runtime group must be unset when runtime user names a GID"},
	} {
		t.Run(name, func(t *testing.T) {
			config := testConfig("https://127.0.0.1:6443", "")
			config.Runtime.User = "1001"
			test.edit(&config.Runtime)
			if _, err := build(config, testPool, testToken); err == nil || err.Error() != test.want {
				t.Fatalf("error %v, want %q", err, test.want)
			}
		})
	}
}

func TestBuildPodCarriesTheRuntimeGroups(t *testing.T) {
	for name, test := range map[string]struct {
		edit   func(*Runtime)
		gid    *int64
		groups []int64
	}{
		"unset":                 {func(r *Runtime) {}, nil, nil},
		"group":                 {func(r *Runtime) { r.User, r.Group = "1001", "1002" }, new(int64(1002)), nil},
		"group without a user":  {func(r *Runtime) { r.Group = "1002" }, new(int64(1002)), nil},
		"supplementary groups":  {func(r *Runtime) { r.User, r.SupplementaryGroups = "1001", []string{"1003", "1004"} }, nil, []int64{1003, 1004}},
		"user's GID and groups": {func(r *Runtime) { r.User, r.SupplementaryGroups = "1001:1002", []string{"1003"} }, new(int64(1002)), []int64{1003}},
	} {
		config := testConfig("https://127.0.0.1:6443", "")
		test.edit(&config.Runtime)
		profile, err := build(config, testPool, testToken)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		security := profile.buildPod(testRun("groups"), "agent-x").Spec.SecurityContext
		gid := security.RunAsGroup
		if (gid == nil) != (test.gid == nil) || gid != nil && *gid != *test.gid || !slices.Equal(security.SupplementalGroups, test.groups) || security.FSGroup != gid {
			t.Fatalf("%s: runAsGroup %v supplementalGroups %v, want %v %v", name, gid, security.SupplementalGroups, test.gid, test.groups)
		}
	}
}

func TestPodNamesAreTheRunID(t *testing.T) {
	profile := plainProfile(t, nil)
	if name := profile.jobName("run-a"); name != poolPrefix+"run-a" {
		t.Fatalf("name %q", name)
	}
	if long := profile.jobName(strings.Repeat("r", 63)); len(long) > 103 {
		t.Fatalf("name of %d characters; want at most 103", len(long))
	}
}

func dispatch(t *testing.T, profile *Kubernetes, run pluginsdk.Run) pluginsdk.Handle {
	t.Helper()
	// A pod that never settles fails the test rather than hanging it.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	outcome, handle, err := profile.Dispatch(ctx, run)
	if err != nil || outcome != pluginsdk.OutcomeCreated {
		t.Fatalf("dispatch = %v, %v", outcome, err)
	}
	return handle
}

func TestSameRunInTwoPoolsCreatesTwoPods(t *testing.T) {
	api := newFakeAPI(t)
	build1 := api.profile(t)
	config := testConfig(api.server.URL, api.caFile)
	config.RunsDir = t.TempDir()
	review, err := build(config, "review", testToken)
	if err != nil {
		t.Fatal(err)
	}
	run := testRun("shared")
	first, second := dispatch(t, build1, run), dispatch(t, review, run)
	if first.Name == second.Name || api.pod(first.Name) == nil || api.pod(second.Name) == nil {
		t.Fatalf("pods %q and %q; want one per pool", first.Name, second.Name)
	}
}

func TestDispatchCreatesSecretThenPod(t *testing.T) {
	api := newFakeAPI(t)
	profile := api.profile(t)
	run := testRun("create")
	run.Inputs = []pluginsdk.File{{Name: "notes.md", Data: []byte("notes")}}
	run.Credentials = append(run.Credentials, pluginsdk.Secret{Name: "deploy", File: "id_ed25519", Value: []byte("key")})
	handle := dispatch(t, profile, run)
	name := poolPrefix + "run-create"
	base := "/api/v1/namespaces/agents/"
	want := []string{"GET " + base + "pods/" + name, "GET " + base + "secrets/" + name, "POST " + base + "secrets", "POST " + base + "pods", "GET " + base + "pods/" + name}
	if got := paths(api.recorded(), ""); !slices.Equal(got, want) {
		t.Fatalf("requests %v; want %v", got, want)
	}
	secret := api.secret(name)
	wantData := map[string]string{"prompt": "hello-prompt", "env." + credentialEnv: "test-credential", "credentials.id_ed25519": "key"}
	if secret.Type != corev1.SecretTypeOpaque || len(secret.Data) != len(wantData) || secret.Labels[labelRun] != run.ID {
		t.Fatalf("secret %#v", secret)
	}
	for key, value := range wantData {
		if string(secret.Data[key]) != value {
			t.Fatalf("secret %s = %q; want %q", key, secret.Data[key], value)
		}
	}
	if decoded, err := decodeHandle(handle); err != nil || handle.Name != name || decoded.Pod != name || decoded.UID != string(api.pod(name).UID) {
		t.Fatalf("handle %s %+v, %v", handle.Name, decoded, err)
	}
}

// Each env credential reaches the agent as a reference into the run's Secret, and each file credential as a key the
// material volume maps into the credentials directory.
func TestDispatchDeliversCredentialsAtTheirTargets(t *testing.T) {
	for name, credentials := range map[string][]pluginsdk.Secret{
		"conformance": pluginsdktest.Credentials,
		"two of each": {
			{Name: "model", Env: "MODEL_API_KEY", Value: []byte("env-value-one")}, {Name: "git", Env: "GIT_TOKEN", Value: []byte("env-value-two")},
			{Name: "deploy", File: "id_ed25519", Value: []byte("file-value-one")}, {Name: "ca", File: "ca.pem", Value: []byte("file-value-two")},
		},
	} {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			profile := api.profile(t)
			run := testRun("credentials")
			run.Credentials = credentials
			dispatch(t, profile, run)
			job := profile.jobName(run.ID)
			secret, agent := api.secret(job), container(api.pod(job), "agent")
			for _, credential := range credentials {
				key := "credentials." + credential.File
				if credential.Env != "" {
					key = "env." + credential.Env
					i := slices.IndexFunc(agent.Env, func(v corev1.EnvVar) bool { return v.Name == credential.Env })
					if i < 0 || agent.Env[i].Value != "" || agent.Env[i].ValueFrom == nil || *agent.Env[i].ValueFrom.SecretKeyRef != (corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: job}, Key: key}) {
						t.Fatalf("agent env %#v; want %s from the Secret's key %s", agent.Env, credential.Env, key)
					}
				}
				if string(secret.Data[key]) != string(credential.Value) {
					t.Fatalf("secret %s = %q; want credential %s", key, secret.Data[key], credential.Name)
				}
				items := volume(api.pod(job), "material").Secret.Items
				if credential.File != "" && !slices.ContainsFunc(items, func(item corev1.KeyToPath) bool { return item.Key == key && item.Path == "files/"+credential.File }) {
					t.Fatalf("material items %v; want %s at its file", items, key)
				}
			}
			if len(secret.Data) != 1+len(credentials) {
				t.Fatalf("secret keys %v; want the prompt and one per credential", slices.Sorted(maps.Keys(secret.Data)))
			}
		})
	}
}

func TestDispatchConvergesOnExistingPod(t *testing.T) {
	api := newFakeAPI(t)
	profile := api.profile(t)
	existing := api.agentPod(t, profile, "existing", nil)
	if handle := dispatch(t, profile, testRun("existing")); handle.Name != existing.Name {
		t.Fatalf("handle %q; want the existing pod", handle.Name)
	}
	if created := paths(api.recorded(), http.MethodPost); created != nil {
		t.Fatalf("created %v beside an existing pod", created)
	}
	// A pod another dispatch creates between this one's read and create.
	raced := api.agentPod(t, profile, "raced", nil)
	reads := 0
	api.set(func() {
		api.intercept = func(response http.ResponseWriter, r *http.Request) bool {
			if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pods/"+raced.Name) {
				if reads++; reads == 1 {
					notFound(response, "pods", raced.Name)
					return true
				}
			}
			return false
		}
	})
	outcome, handle, err := profile.Dispatch(context.Background(), testRun("raced"))
	if err != nil || outcome != pluginsdk.OutcomeCreated || handle.Name != raced.Name {
		t.Fatalf("dispatch racing a create = %v, %q, %v; want the existing pod", outcome, handle.Name, err)
	}
	if placement := samples(t, profile)["wherry_plugin_placement_seconds/"].Histogram; placement.Count != 0 {
		t.Fatalf("placement counted %d for pods this profile did not create", placement.Count)
	}
}

func TestDispatchLeavesTheSecretOfAnExistingPod(t *testing.T) {
	api := newFakeAPI(t)
	profile := api.profile(t)
	pod := api.agentPod(t, profile, "existing", nil)
	run := testRun("existing")
	run.Inputs = []pluginsdk.File{{Name: "notes.md", Data: []byte("notes")}}
	dispatch(t, profile, run)
	for _, r := range api.recorded() {
		if strings.Contains(r.path, "/secrets") {
			t.Fatalf("dispatch onto an existing pod requested %s", r)
		}
	}
	if _, err := os.Stat(runDir(profile, pod.Name)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("dispatch onto an existing pod wrote its inputs: %v", err)
	}
}

func TestDispatchRefusesAForeignPod(t *testing.T) {
	api := newFakeAPI(t)
	profile := api.profile(t)
	api.put(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: poolPrefix + "run-foreign"}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "agent"}}}})
	if outcome, _, err := profile.Dispatch(context.Background(), testRun("foreign")); outcome != pluginsdk.OutcomeUnknown || err == nil || err.Error() != "existing pod is not an agent pod" {
		t.Fatalf("dispatch onto a foreign pod = %v, %v", outcome, err)
	}
	if _, err := os.Stat(runDir(profile, poolPrefix+"run-foreign")); api.secret(poolPrefix+"run-foreign") != nil || !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a Secret or a run directory for a foreign pod")
	}
}

func TestDispatchOverwritesStaleSecret(t *testing.T) {
	api := newFakeAPI(t)
	profile := api.profile(t)
	name := poolPrefix + "run-stale"
	api.set(func() {
		api.secrets[name] = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, ResourceVersion: "41"}, Data: map[string][]byte{"prompt": []byte("stale"), "inputs.old": []byte("old")}}
	})
	dispatch(t, profile, testRun("stale"))
	secret := api.secret(name)
	if string(secret.Data["prompt"]) != "hello-prompt" || secret.Data["inputs.old"] != nil {
		t.Fatalf("secret data %v; want the turn's alone", secret.Data)
	}
	if puts := paths(api.recorded(), http.MethodPut); len(puts) != 1 {
		t.Fatalf("puts %v; want one update", puts)
	}
}

func failing(code int, method, suffix string) func(http.ResponseWriter, *http.Request) bool {
	return func(response http.ResponseWriter, r *http.Request) bool {
		if r.Method == method && strings.HasSuffix(r.URL.Path, suffix) {
			apiStatus(response, code, "", "refused by the test")
			return true
		}
		return false
	}
}

func TestDispatchFailureOutcomes(t *testing.T) {
	name := poolPrefix + "run-outcome"
	for _, test := range []struct {
		test      string
		intercept func(*fakeAPI) func(http.ResponseWriter, *http.Request) bool
		outcome   pluginsdk.Outcome
		deleted   bool
	}{
		{"secret write fails and no pod exists", func(*fakeAPI) func(http.ResponseWriter, *http.Request) bool {
			return failing(http.StatusInternalServerError, http.MethodPost, "/secrets")
		}, pluginsdk.OutcomeNothingCreated, true},
		{"pod create refused and no pod exists", func(*fakeAPI) func(http.ResponseWriter, *http.Request) bool {
			return failing(http.StatusForbidden, http.MethodPost, "/pods")
		}, pluginsdk.OutcomeNothingCreated, true},
		{"pod create fails and the pod cannot be read", func(*fakeAPI) func(http.ResponseWriter, *http.Request) bool {
			reads := 0
			return func(response http.ResponseWriter, r *http.Request) bool {
				switch {
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pods"):
					apiStatus(response, http.StatusInternalServerError, "", "etcd unavailable")
					return true
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pods/"+name):
					if reads++; reads > 1 {
						apiStatus(response, http.StatusServiceUnavailable, "", "etcd unavailable")
						return true
					}
				}
				return false
			}
		}, pluginsdk.OutcomeUnknown, false},
		{"pod create fails and the pod exists", func(api *fakeAPI) func(http.ResponseWriter, *http.Request) bool {
			return func(response http.ResponseWriter, r *http.Request) bool {
				if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pods") {
					api.serial++
					api.pods[name] = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, UID: "uid-raced"}}
					apiStatus(response, http.StatusGatewayTimeout, metav1.StatusReasonTimeout, "the create timed out")
					return true
				}
				return false
			}
		}, pluginsdk.OutcomeUnknown, false},
		{"the first read fails", func(*fakeAPI) func(http.ResponseWriter, *http.Request) bool {
			return failing(http.StatusInternalServerError, http.MethodGet, "/pods/"+name)
		}, pluginsdk.OutcomeUnknown, false},
	} {
		t.Run(test.test, func(t *testing.T) {
			api := newFakeAPI(t)
			profile := api.profile(t)
			api.set(func() { api.intercept = test.intercept(api) })
			outcome, _, err := profile.Dispatch(context.Background(), testRun("outcome"))
			if outcome != test.outcome || err == nil {
				t.Fatalf("outcome %v, %v; want %v and the cause", outcome, err, test.outcome)
			}
			deleted := slices.Contains(paths(api.recorded(), http.MethodDelete), "DELETE /api/v1/namespaces/agents/secrets/"+name)
			if deleted != test.deleted {
				t.Fatalf("secret deleted %t; want %t", deleted, test.deleted)
			}
		})
	}
	t.Run("wait fails", func(t *testing.T) {
		api := newFakeAPI(t)
		profile := api.profile(t)
		reads := 0
		api.set(func() {
			api.intercept = func(response http.ResponseWriter, r *http.Request) bool {
				if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pods/"+name) {
					if reads++; reads > 1 {
						apiStatus(response, http.StatusInternalServerError, "", "etcd unavailable")
						return true
					}
				}
				return false
			}
		})
		if outcome, _, err := profile.Dispatch(context.Background(), testRun("outcome")); outcome != pluginsdk.OutcomeUnknown || err == nil {
			t.Fatalf("outcome %v, %v; want unknown", outcome, err)
		}
	})
}

// The Secret holds the prompt and credentials alone, so inputs pass its limit.
func TestDispatchWritesInputsPastTheSecretLimit(t *testing.T) {
	api := newFakeAPI(t)
	profile := api.profile(t)
	run := testRun("limit")
	run.Inputs = []pluginsdk.File{{Name: "fill.bin", Data: bytes.Repeat([]byte{0xff}, maxSecret+1)}}
	dispatch(t, profile, run)
	name := profile.jobName(run.ID)
	if data, err := os.ReadFile(filepath.Join(runDir(profile, name), "inputs/fill.bin")); err != nil || !bytes.Equal(data, run.Inputs[0].Data) {
		t.Fatalf("input of %d bytes, %v; want it whole", len(data), err)
	}
	if secret := api.secret(name); len(secret.Data) != 2 {
		t.Fatalf("secret keys %v; want the prompt and the credential", slices.Sorted(maps.Keys(secret.Data)))
	}
}

func TestDispatchRefusesANameOverTheKeyLimit(t *testing.T) {
	api := newFakeAPI(t)
	profile := api.profile(t)
	for i, test := range []struct {
		file    string
		refused bool
	}{
		{file: strings.Repeat("f", 241)},
		{file: strings.Repeat("f", 242), refused: true},
	} {
		run := testRun("key-" + strconv.Itoa(i))
		run.Credentials = []pluginsdk.Secret{{Name: "c", File: test.file, Value: []byte("x")}}
		before := len(api.recorded())
		outcome, _, err := profile.Dispatch(context.Background(), run)
		var tooLarge *pluginsdk.InputTooLargeError
		switch {
		case !test.refused && (err != nil || outcome != pluginsdk.OutcomeCreated):
			t.Fatalf("name of %d characters: %v, %v; want created", len(test.file), outcome, err)
		case test.refused && (outcome != pluginsdk.OutcomeNothingCreated || !errors.As(err, &tooLarge) || tooLarge.Name != test.file):
			t.Fatalf("name of %d characters: %v, %v; want it too large", len(test.file), outcome, err)
		case test.refused && len(api.recorded()) != before:
			t.Fatalf("a refused name made %d requests", len(api.recorded())-before)
		}
	}
}

func TestDispatchRefusesAnOversizedPrompt(t *testing.T) {
	api := newFakeAPI(t)
	profile := api.profile(t)
	run := testRun("prompt")
	run.Prompt = bytes.Repeat([]byte("p"), maxSecret)
	outcome, _, err := profile.Dispatch(context.Background(), run)
	var tooLarge *pluginsdk.InputTooLargeError
	if outcome != pluginsdk.OutcomeNothingCreated || err == nil || err.Error() != "the prompt and credentials exceed the 1 MiB Secret limit" || errors.As(err, &tooLarge) {
		t.Fatalf("dispatch = %v, %v; want the prompt refused, naming no input", outcome, err)
	}
	if requests := api.recorded(); len(requests) != 0 {
		t.Fatalf("an oversized prompt made requests %v", requests)
	}
}

func pendingKubelet(pod *corev1.Pod) {
	setStatus(pod, corev1.PodPending, waiting("agent", "ContainerCreating", ""))
}

func TestDispatchTimeoutDeletesThePod(t *testing.T) {
	fast(t)
	api := newFakeAPI(t)
	profile := api.profile(t)
	api.set(func() { api.kubelet = pendingKubelet })
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if outcome, _, err := profile.Dispatch(ctx, testRun("timeout")); outcome != pluginsdk.OutcomeUnknown || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("dispatch = %v, %v; want the deadline", outcome, err)
	}
	name := poolPrefix + "run-timeout"
	if api.pod(name) != nil {
		t.Fatal("the timed-out pod remains")
	}
	if api.secret(name) == nil {
		t.Fatal("the timed-out turn's Secret was deleted; Release or the sweep removes it")
	}
}

func TestDispatchReportsPlacement(t *testing.T) {
	api := newFakeAPI(t)
	profile := api.profile(t)
	dispatch(t, profile, testRun("placed"))
	placement := samples(t, profile)["wherry_plugin_placement_seconds/"].Histogram
	if placement.Count != 1 || len(placement.Buckets) != len(pluginsdk.PlacementBuckets) || placement.Buckets[len(placement.Buckets)-1] != 1 {
		t.Fatalf("placement %+v; want one observation over the placement buckets", placement)
	}
}

func sha(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// objects serves fetches by path and counts the requests for each.
type objects struct {
	server *httptest.Server
	mu     sync.Mutex
	gets   map[string]int
}

func newObjects(t *testing.T, handler func(http.ResponseWriter, *http.Request)) *objects {
	o := &objects{gets: map[string]int{}}
	o.server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		o.gets[r.URL.Path]++
		o.mu.Unlock()
		handler(response, r)
	}))
	t.Cleanup(o.server.Close)
	return o
}

func (o *objects) count(path string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.gets[path]
}

func fetchRun(name string, objects *objects, files map[string][]byte) pluginsdk.Run {
	run := testRun(name)
	for _, file := range slices.Sorted(maps.Keys(files)) {
		run.Fetches = append(run.Fetches, pluginsdk.Fetch{Name: file, URL: objects.server.URL + "/" + file + "?X-Amz-Signature=signature-secret", Size: int64(len(files[file])), SHA256: sha(files[file])})
	}
	return run
}

func serveFiles(files map[string][]byte) func(http.ResponseWriter, *http.Request) {
	return func(response http.ResponseWriter, r *http.Request) {
		data, ok := files[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			http.Error(response, "no such object", http.StatusNotFound)
			return
		}
		_, _ = response.Write(data)
	}
}

// runDir is the directory of pod's run on the profile's runs claim.
func runDir(profile *Kubernetes, pod string) string {
	return filepath.Join(profile.runs, pod)
}

// tree reads every file below dir as its mode and content, by path.
func tree(dir string) (map[string]string, error) {
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		name, _ := filepath.Rel(dir, path)
		files[name] = info.Mode().String()
		if entry.Type().IsRegular() {
			data, err := os.ReadFile(path)
			files[name] += " " + string(data)
			return err
		}
		return nil
	})
	return files, err
}

func TestDispatchWritesInputsBeforeThePod(t *testing.T) {
	api := newFakeAPI(t)
	profile := api.profile(t)
	files := map[string][]byte{"data.csv": []byte("a,b\n1,2\n"), "model.bin": {0, 1, 2, 255}}
	run := fetchRun("inputs", newObjects(t, serveFiles(files)), files)
	run.Inputs = []pluginsdk.File{{Name: "notes.md", Data: []byte("notes")}}
	dir := runDir(profile, profile.jobName(run.ID))
	var created map[string]string
	api.set(func() {
		api.intercept = func(_ http.ResponseWriter, r *http.Request) bool {
			if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pods") {
				created, _ = tree(dir)
			}
			return false
		}
	})
	dispatch(t, profile, run)
	want := map[string]string{".": "drwx------", "inputs": "drwx------", "inputs/notes.md": "-rw------- notes",
		"inputs/data.csv": "-rw------- a,b\n1,2\n", "inputs/model.bin": "-rw------- \x00\x01\x02\xff"}
	if !maps.Equal(created, want) {
		t.Fatalf("run directory %q when the pod was created; want %q", created, want)
	}
}

func TestDispatchKeepsACompleteRunDirectory(t *testing.T) {
	api := newFakeAPI(t)
	profile := api.profile(t)
	files := map[string][]byte{"data.csv": []byte("data")}
	store := newObjects(t, serveFiles(files))
	run := fetchRun("again", store, files)
	run.Inputs = []pluginsdk.File{{Name: "notes.md", Data: []byte("notes")}}
	name := profile.jobName(run.ID)
	dispatch(t, profile, run)
	// A receiver that restarted between the inputs and the pod finds the directory complete and no pod.
	api.set(func() { delete(api.pods, name) })
	dispatch(t, profile, run)
	api.set(func() { delete(api.pods, name) })
	if store.count("/data.csv") != 1 {
		t.Fatalf("fetched %d times; want the complete directory kept", store.count("/data.csv"))
	}
	for _, partial := range []func(dir string) error{
		func(dir string) error { return os.Truncate(filepath.Join(dir, "inputs/data.csv"), 2) },
		func(dir string) error { return os.WriteFile(filepath.Join(dir, "inputs/stray"), nil, 0o600) },
		func(dir string) error { return os.Mkdir(filepath.Join(dir, "outputs"), 0o700) },
		func(dir string) error { return os.Remove(filepath.Join(dir, "inputs/notes.md")) },
	} {
		if err := partial(runDir(profile, name)); err != nil {
			t.Fatal(err)
		}
		before := store.count("/data.csv")
		dispatch(t, profile, run)
		api.set(func() { delete(api.pods, name) })
		got, err := tree(runDir(profile, name))
		if err != nil {
			t.Fatal(err)
		}
		if store.count("/data.csv") != before+1 || len(got) != 4 || got["inputs/data.csv"] != "-rw------- data" || got["inputs/notes.md"] != "-rw------- notes" {
			t.Fatalf("run directory %q; want it rewritten whole", got)
		}
	}
}

func TestFetchFailureCreatesNoPod(t *testing.T) {
	data := []byte("data")
	for _, test := range []struct {
		reason string
		serve  func(http.ResponseWriter, *http.Request)
	}{
		{"403", func(response http.ResponseWriter, _ *http.Request) {
			http.Error(response, "AccessDenied", http.StatusForbidden)
		}},
		{"302", func(response http.ResponseWriter, r *http.Request) {
			http.Redirect(response, r, "/elsewhere/data.csv", http.StatusFound)
		}},
		{"shorter than its size", func(response http.ResponseWriter, _ *http.Request) { _, _ = response.Write(data[:3]) }},
		{"longer than its size", func(response http.ResponseWriter, _ *http.Request) { _, _ = response.Write(bytes.Repeat(data, 2<<20)) }},
		{"does not match its digest", func(response http.ResponseWriter, _ *http.Request) { _, _ = response.Write([]byte("date")) }},
	} {
		t.Run(test.reason, func(t *testing.T) {
			api := newFakeAPI(t)
			profile := api.profile(t)
			store := newObjects(t, test.serve)
			run := fetchRun("fetch", store, map[string][]byte{"data.csv": data})
			outcome, _, err := profile.Dispatch(context.Background(), run)
			var failed *fetchError
			if outcome != pluginsdk.OutcomeNothingCreated || !errors.As(err, &failed) || err.Error() != "fetch data.csv: "+test.reason {
				t.Fatalf("dispatch = %v, %v; want fetch data.csv: %s and nothing created", outcome, err, test.reason)
			}
			if created := paths(api.recorded(), http.MethodPost); created != nil || store.count("/elsewhere/data.csv") != 0 {
				t.Fatalf("created %v, or followed a redirect, for a failed fetch", created)
			}
			if info, err := os.Stat(filepath.Join(runDir(profile, profile.jobName(run.ID)), "inputs/data.csv")); err == nil && info.Size() > int64(len(data))+1 {
				t.Fatalf("wrote %d bytes of a fetch of %d", info.Size(), len(data))
			}
		})
	}
}

func TestLookupMissingPod(t *testing.T) {
	api := newFakeAPI(t)
	profile := api.profile(t)
	if job, err := profile.Lookup(context.Background(), "run-missing"); err != nil || job.State != pluginsdk.JobAbsent {
		t.Fatalf("lookup = %+v, %v; want absent", job, err)
	}
}

func TestLookupReadsDispatchMetadata(t *testing.T) {
	api := newFakeAPI(t)
	profile := api.profile(t)
	run := testRun("meta")
	handle := dispatch(t, profile, run)
	job, err := profile.Lookup(context.Background(), run.ID)
	if err != nil || job.State != pluginsdk.JobRunning || job.Attempt != 2 || job.Profile != testProfile || job.Agent != "coder" || job.Handle.Name != handle.Name {
		t.Fatalf("lookup = %+v, %v", job, err)
	}
	api.agentPod(t, profile, "unannotated", func(pod *corev1.Pod) { pod.Annotations = nil })
	if job, err := profile.Lookup(context.Background(), "run-unannotated"); err != nil || job.Attempt != 0 {
		t.Fatalf("lookup without the annotation = %+v, %v; want attempt 0", job, err)
	}
}

// The testdata files are pod objects written to the shape of OpenShift 4.18 pods under restricted-v2, in the states the
// plugin reads; they are not captures.
func captured(t *testing.T, name string) *corev1.Pod {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var pod corev1.Pod
	if err := json.Unmarshal(bytes.ReplaceAll(data, []byte("agent-capture-"), []byte(poolPrefix+"capture-")), &pod); err != nil {
		t.Fatal(err)
	}
	pod.Labels[labelPool] = testPool
	return &pod
}

var capturedStates = []struct {
	file          string
	state         pluginsdk.JobState
	settled       bool
	class, reason string
}{
	{"running", pluginsdk.JobRunning, true, "", ""},
	{"exit-0", pluginsdk.JobEnded, true, "", ""},
	{"exit-1", pluginsdk.JobEnded, true, "", "Error"},
	{"sigterm", pluginsdk.JobEnded, true, "", "Error"},
	{"oom", pluginsdk.JobEnded, true, "", "OOMKilled"},
	{"image-pull-backoff", pluginsdk.JobNeverStarted, true, "image", `Back-off pulling image "registry.invalid/wherry-conformance:0": ErrImagePull: initializing source docker://registry.invalid/wherry-conformance:0: pinging container registry registry.invalid: Get "https://registry.invalid/v2/": dial tcp: lookup registry.invalid: no such host`},
	{"create-container-config-error", pluginsdk.JobNeverStarted, true, "config", `container has runAsNonRoot and image will run as root (pod: "agent-44575cf5b285-capture-create-container-config-error_wherry-agents(6f1c0b8e)", container: agent)`},
	{"start-error", pluginsdk.JobNeverStarted, true, "start", `failed to create containerd task: exec: "/bin/agent-run": stat /bin/agent-run: no such file or directory`},
	{"evicted", pluginsdk.JobNeverStarted, true, "pod", "Evicted: The node was low on resource: ephemeral-storage. Threshold quantity: 4902142351, available: 3893504Ki. "},
	{"err-image-pull", 0, false, "", ""},
	{"unschedulable", 0, false, "", ""},
	{"lost-node-running", pluginsdk.JobEnded, true, "", ""},
	{"lost-node-pending", pluginsdk.JobNeverStarted, true, "pod", "the pod is being deleted"},
}

func TestLookupMapsCapturedPods(t *testing.T) {
	fast(t)
	for _, test := range capturedStates {
		t.Run(test.file, func(t *testing.T) {
			pod := captured(t, test.file)
			if state, terminal, settled := judge(pod); state != test.state && test.settled || settled != test.settled || terminal.Class != test.class || terminal.Reason != test.reason {
				t.Fatalf("judged %v, %+v, settled %t; want %v, class %q, reason %q, settled %t", state, terminal, settled, test.state, test.class, test.reason, test.settled)
			}
			api := newFakeAPI(t)
			profile := api.profile(t)
			api.set(func() { api.kubelet = nil })
			api.put(pod)
			ctx, cancel := context.Background(), context.CancelFunc(func() {})
			if !test.settled {
				ctx, cancel = context.WithTimeout(ctx, 500*time.Millisecond)
			}
			defer cancel()
			job, err := profile.Lookup(ctx, "capture-"+test.file)
			if !test.settled {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("lookup of a pending pod = %+v, %v; want the wait cut short", job, err)
				}
				return
			}
			if err != nil || job.State != test.state || job.Profile != "openshift" || job.Agent != "coder" || job.Attempt != 1 {
				t.Fatalf("lookup = %+v, %v; want %v", job, err, test.state)
			}
		})
	}
}

func TestLookupTreatsADeletingPodAsEnded(t *testing.T) {
	api := newFakeAPI(t)
	profile := api.profile(t)
	api.set(func() { api.kubelet = nil })
	for file, want := range map[string]pluginsdk.JobState{"lost-node-running": pluginsdk.JobEnded, "lost-node-pending": pluginsdk.JobNeverStarted} {
		api.put(captured(t, file))
		job, err := profile.Lookup(context.Background(), "capture-"+file)
		if err != nil || job.State != want {
			t.Fatalf("%s: lookup = %+v, %v; want %v", file, job, err, want)
		}
		_, terminal, _ := judge(api.pod(job.Handle.Name))
		if terminal.ExitCode != -1 || want == pluginsdk.JobNeverStarted && (terminal.Class != "pod" || terminal.Reason != "the pod is being deleted") {
			t.Fatalf("%s: terminal %+v", file, terminal)
		}
	}
}

func TestLookupRefusesAForeignPod(t *testing.T) {
	api := newFakeAPI(t)
	profile := api.profile(t)
	api.put(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: poolPrefix + "run-foreign"}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "agent"}}}})
	if _, err := profile.Lookup(context.Background(), "run-foreign"); err == nil || err.Error() != "existing pod is not an agent pod" {
		t.Fatalf("lookup of a foreign pod: %v", err)
	}
}

func TestLookupWaitsForAPendingPod(t *testing.T) {
	fast(t)
	api := newFakeAPI(t)
	profile := api.profile(t)
	reads := 0
	api.set(func() {
		api.kubelet = func(pod *corev1.Pod) {
			if reads++; reads < 4 {
				pendingKubelet(pod)
				return
			}
			setStatus(pod, corev1.PodRunning, runningStatus("agent"))
		}
	})
	api.agentPod(t, profile, "pending", nil)
	if job, err := profile.Lookup(context.Background(), "run-pending"); err != nil || job.State != pluginsdk.JobRunning {
		t.Fatalf("lookup = %+v, %v; want running once the agent started", job, err)
	}
}

func TestLookupTimeoutDeletesThePod(t *testing.T) {
	fast(t)
	api := newFakeAPI(t)
	profile := api.profile(t)
	api.set(func() { api.kubelet = pendingKubelet })
	pod := api.agentPod(t, profile, "stuck", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := profile.Lookup(ctx, "run-stuck"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lookup = %v; want the deadline", err)
	}
	if api.pod(pod.Name) != nil {
		t.Fatal("the pod of a cut-short lookup remains")
	}
}

func TestRateLimitedWaitPastTheDeadlineIsTheDeadline(t *testing.T) {
	fast(t)
	api := newFakeAPI(t)
	client := mustClient(t, api)
	if _, ok := client.RateLimiter.(limiter); !ok {
		t.Fatalf("rate limiter %T; want the limiter wrapper", client.RateLimiter)
	}
	// One token a second: the second read of the wait needs a token past the deadline.
	client.RateLimiter = limiter{flowcontrol.NewTokenBucketRateLimiter(1, 1)}
	profile, err := New(testConfig(api.server.URL, api.caFile), testProfile, testPool, client, hclog.NewNullLogger())
	if err != nil {
		t.Fatal(err)
	}
	api.set(func() { api.kubelet = pendingKubelet })
	pod := api.agentPod(t, profile, "limited", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var failure *pluginsdk.Error
	if _, err := profile.Lookup(ctx, "run-limited"); !errors.Is(err, context.DeadlineExceeded) || errors.As(err, &failure) && failure.Class != "timeout" {
		t.Fatalf("lookup = %v; want the deadline", err)
	}
	if api.pod(pod.Name) != nil {
		t.Fatal("the pod of a lookup cut short by the rate limiter remains")
	}
	if samples(t, profile)["wherry_plugin_platform_errors_total/other"].Value != 0 {
		t.Fatal("the rate limiter's refusal was counted as class other")
	}
}

// logStream serves the agent's log: a follow request gets follow's bytes, a plain one plain's.
func logStream(follow func(n int) []byte, plain []byte) (http.HandlerFunc, func() int) {
	var mu sync.Mutex
	opened := 0
	return func(response http.ResponseWriter, r *http.Request) {
			mu.Lock()
			opened++
			n := opened
			mu.Unlock()
			if r.URL.Query().Get("container") != "agent" {
				http.Error(response, "container not named", http.StatusBadRequest)
				return
			}
			if r.URL.Query().Get("follow") == "true" {
				_, _ = response.Write(follow(n))
				return
			}
			_, _ = response.Write(plain)
		}, func() int {
			mu.Lock()
			defer mu.Unlock()
			return opened
		}
}

func followAll(t *testing.T, profile *Kubernetes, handle pluginsdk.Handle, from int64) ([]pluginsdk.Line, pluginsdk.Terminal, error) {
	t.Helper()
	lines := make(chan pluginsdk.Line)
	type followed struct {
		terminal pluginsdk.Terminal
		err      error
	}
	done := make(chan followed, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go func() {
		terminal, err := profile.Follow(ctx, handle, from, lines)
		done <- followed{terminal, err}
	}()
	var got []pluginsdk.Line
	for {
		select {
		case line := <-lines:
			got = append(got, line)
		case end := <-done:
			return got, end.terminal, end.err
		}
	}
}

// endsAfter is a kubelet that runs the agent for reads reads of the pod, then ends it with code.
func endsAfter(reads int, code int32) func(*corev1.Pod) {
	seen := 0
	return func(pod *corev1.Pod) {
		if seen++; seen <= reads {
			setStatus(pod, corev1.PodRunning, runningStatus("agent"))
			return
		}
		setStatus(pod, corev1.PodRunning, exited("agent", code))
	}
}

func TestFollowOffsetsAndReattach(t *testing.T) {
	fast(t)
	api := newFakeAPI(t)
	profile := api.profile(t)
	logs, _ := logStream(func(int) []byte { return []byte("a\nbb\nccc\n") }, nil)
	api.set(func() { api.logs, api.kubelet = logs, endsAfter(0, 0) })
	handle := handleOf(api.agentPod(t, profile, "offsets", nil))
	lines, _, err := followAll(t, profile, handle, 0)
	want := []pluginsdk.Line{{Offset: 0, Next: 2, Data: []byte("a")}, {Offset: 2, Next: 5, Data: []byte("bb")}, {Offset: 5, Next: 9, Data: []byte("ccc")}}
	if err != nil || fmt.Sprint(lines) != fmt.Sprint(want) {
		t.Fatalf("lines %v, %v; want %v", lines, err, want)
	}
	if lines, _, err := followAll(t, profile, handle, 2); err != nil || fmt.Sprint(lines) != fmt.Sprint(want[1:]) {
		t.Fatalf("reattached at 2: %v, %v; want %v", lines, err, want[1:])
	}
}

func TestFollowReadsTheStreamToItsEnd(t *testing.T) {
	fast(t)
	api := newFakeAPI(t)
	profile := api.profile(t)
	logs, opened := logStream(func(int) []byte { return []byte("one\ntwo\n{\"type\":\"result\"}") }, []byte("rotated"))
	api.set(func() { api.logs, api.kubelet = logs, endsAfter(1, 0) })
	handle := handleOf(api.agentPod(t, profile, "end", nil))
	lines, terminal, err := followAll(t, profile, handle, 0)
	if err != nil || len(lines) != 3 || string(lines[2].Data) != `{"type":"result"}` || !terminal.Started {
		t.Fatalf("lines %v, %+v, %v", lines, terminal, err)
	}
	if opened() != 1 {
		t.Fatalf("log opened %d times; want once", opened())
	}
}

func TestFollowSurvivesRotationWithoutReattach(t *testing.T) {
	fast(t)
	api := newFakeAPI(t)
	profile := api.profile(t)
	line := append(bytes.Repeat([]byte("x"), 1023), '\n')
	whole := bytes.Repeat(line, 60<<10)
	logs, _ := logStream(func(int) []byte { return whole }, whole[len(whole)-10<<20:])
	api.set(func() { api.logs, api.kubelet = logs, endsAfter(1, 0) })
	handle := handleOf(api.agentPod(t, profile, "rotation", nil))
	// A slower poll leaves the fake's time to the stream.
	followPoll = 200 * time.Millisecond
	lines, terminal, err := followAll(t, profile, handle, 0)
	if err != nil || len(lines) != 60<<10 || lines[len(lines)-1].Next != int64(len(whole)) || !terminal.Started {
		t.Fatalf("%d lines ending at %d, %+v, %v; want %d lines ending at %d", len(lines), lines[len(lines)-1].Next, terminal, err, 60<<10, len(whole))
	}
}

func TestFollowReopensAStreamThatClosedEarly(t *testing.T) {
	fast(t)
	api := newFakeAPI(t)
	profile := api.profile(t)
	logs, opened := logStream(func(n int) []byte {
		if n == 1 {
			return []byte("one\ntw")
		}
		return []byte("one\ntwo\nthree\n")
	}, nil)
	api.set(func() {
		api.logs = logs
		api.kubelet = func(pod *corev1.Pod) {
			if opened() < 2 {
				setStatus(pod, corev1.PodRunning, runningStatus("agent"))
				return
			}
			setStatus(pod, corev1.PodRunning, exited("agent", 0))
		}
	})
	handle := handleOf(api.agentPod(t, profile, "early", nil))
	lines, _, err := followAll(t, profile, handle, 0)
	var data []string
	for _, line := range lines {
		data = append(data, string(line.Data))
	}
	if err != nil || !slices.Equal(data, []string{"one", "two", "three"}) || lines[1].Offset != 4 {
		t.Fatalf("lines %v, %v; want one, two and three once each", lines, err)
	}
}

func TestFollowFailsOnALogShorterThanTheOffset(t *testing.T) {
	fast(t)
	api := newFakeAPI(t)
	profile := api.profile(t)
	logs, _ := logStream(func(int) []byte { return []byte("short\n") }, nil)
	api.set(func() { api.logs, api.kubelet = logs, endsAfter(0, 0) })
	handle := handleOf(api.agentPod(t, profile, "short", nil))
	if _, _, err := followAll(t, profile, handle, 100); err == nil || err.Error() != "the agent's log is shorter than the offset" {
		t.Fatalf("follow from past the log's end: %v", err)
	}
}

func TestFollowReturnsTerminal(t *testing.T) {
	fast(t)
	api := newFakeAPI(t)
	profile := api.profile(t)
	logs, _ := logStream(func(int) []byte { return []byte("done\n") }, nil)
	api.set(func() {
		api.logs = logs
		api.kubelet = func(pod *corev1.Pod) {
			agent := exited("agent", 137)
			agent.State.Terminated.Signal = 9
			setStatus(pod, corev1.PodRunning, agent)
			pod.Spec.NodeName = "worker-2"
		}
	})
	handle := handleOf(api.agentPod(t, profile, "terminal", nil))
	_, terminal, err := followAll(t, profile, handle, 0)
	if want := (pluginsdk.Terminal{Started: true, ExitCode: 137, Signal: "9", Node: "worker-2"}); err != nil || terminal != want {
		t.Fatalf("terminal %+v, %v; want %+v", terminal, err, want)
	}
}

func TestFollowCarriesTheAgentsTermination(t *testing.T) {
	fast(t)
	for _, test := range []struct {
		reason string
		code   int32
		want   pluginsdk.Terminal
	}{
		{"OOMKilled", 137, pluginsdk.Terminal{Started: true, ExitCode: 137, Node: "worker-1", Reason: "OOMKilled"}},
		{"Completed", 0, pluginsdk.Terminal{Started: true, Node: "worker-1"}},
		{"Error", 1, pluginsdk.Terminal{Started: true, ExitCode: 1, Node: "worker-1", Reason: "Error"}},
	} {
		t.Run(test.reason, func(t *testing.T) {
			api := newFakeAPI(t)
			profile := api.profile(t)
			logs, _ := logStream(func(int) []byte { return []byte("done\n") }, nil)
			api.set(func() {
				api.logs = logs
				api.kubelet = func(pod *corev1.Pod) {
					agent := exited("agent", test.code)
					agent.State.Terminated.Reason = test.reason
					setStatus(pod, corev1.PodRunning, agent)
				}
			})
			_, terminal, err := followAll(t, profile, handleOf(api.agentPod(t, profile, "ended", nil)), 0)
			if err != nil || terminal != test.want {
				t.Fatalf("terminal %+v, %v; want %+v", terminal, err, test.want)
			}
		})
	}
}

func TestFollowOfANeverStartedPod(t *testing.T) {
	api := newFakeAPI(t)
	profile := api.profile(t)
	api.set(func() { api.kubelet = nil })
	pod := api.put(captured(t, "image-pull-backoff"))
	_, terminal, err := followAll(t, profile, handleOf(pod), 0)
	if err != nil || terminal.Started || terminal.Class != "image" {
		t.Fatalf("terminal %+v, %v; want never started with class image", terminal, err)
	}
	if logs := slices.DeleteFunc(paths(api.recorded(), ""), func(p string) bool { return !strings.HasSuffix(p, "/log") }); len(logs) != 0 {
		t.Fatalf("log requests %v for a pod that never started", logs)
	}
}

// blipsThen is a kubelet whose first reads of a pod report no container statuses, before next's.
func blipsThen(reads int, next func(*corev1.Pod)) func(*corev1.Pod) {
	seen := 0
	return func(pod *corev1.Pod) {
		if seen++; seen <= reads {
			setStatus(pod, corev1.PodRunning)
			return
		}
		next(pod)
	}
}

func TestFollowWaitsOutAnUnsettledPod(t *testing.T) {
	fast(t)
	api := newFakeAPI(t)
	profile := api.profile(t)
	logs, _ := logStream(func(int) []byte { return []byte("a\n") }, nil)
	api.set(func() { api.logs, api.kubelet = logs, blipsThen(2, endsAfter(0, 0)) })
	handle := handleOf(api.agentPod(t, profile, "blip", nil))
	lines, terminal, err := followAll(t, profile, handle, 0)
	if err != nil || !terminal.Started || len(lines) != 1 {
		t.Fatalf("lines %v, terminal %+v, %v; want the agent's line and its end, not a never-started terminal", lines, terminal, err)
	}
}

func TestFollowOfADeletingPodOpensNoLogs(t *testing.T) {
	fast(t)
	logRequests := func(api *fakeAPI) int {
		return len(slices.DeleteFunc(paths(api.recorded(), ""), func(p string) bool { return !strings.HasSuffix(p, "/log") }))
	}
	api := newFakeAPI(t)
	profile := api.profile(t)
	api.set(func() { api.kubelet = nil })
	pod := api.put(captured(t, "lost-node-running"))
	_, terminal, err := followAll(t, profile, handleOf(pod), 0)
	if err != nil || !terminal.Started || terminal.ExitCode != -1 || logRequests(api) != 0 {
		t.Fatalf("terminal %+v, %v with %d log requests; want exit code -1 and none", terminal, err, logRequests(api))
	}
	api = newFakeAPI(t)
	profile = api.profile(t)
	release := make(chan struct{})
	defer close(release)
	reads := 0
	api.set(func() {
		api.logs = func(response http.ResponseWriter, _ *http.Request) {
			_, _ = response.Write([]byte("partial\n"))
			response.(http.Flusher).Flush()
			<-release
		}
		api.kubelet = func(pod *corev1.Pod) {
			startAgent(pod)
			if reads++; reads > 3 && pod.DeletionTimestamp == nil {
				pod.DeletionTimestamp = new(metav1.Now())
			}
		}
	})
	pod = api.agentPod(t, profile, "deleting", nil)
	_, terminal, err = followAll(t, profile, handleOf(pod), 0)
	if err != nil || !terminal.Started || terminal.ExitCode != -1 || logRequests(api) != 1 {
		t.Fatalf("terminal %+v, %v with %d log requests; want exit code -1 and the first alone", terminal, err, logRequests(api))
	}
}

func TestTerminalCarriesPlatformReason(t *testing.T) {
	const message = `Back-off pulling image "registry.example.com/agent:1.0": unauthorized: token=registry-secret-in-message ` + "é"
	api := newFakeAPI(t)
	profile := api.profile(t)
	api.set(func() {
		api.kubelet = func(pod *corev1.Pod) {
			setStatus(pod, corev1.PodPending, waiting("agent", "ImagePullBackOff", message))
		}
	})
	pod := api.agentPod(t, profile, "reason", nil)
	_, terminal, err := followAll(t, profile, handleOf(pod), 0)
	if err != nil || terminal.Reason != message {
		t.Fatalf("reason %q, %v; want the kubelet's message as it is", terminal.Reason, err)
	}
}

func TestStaleHandleRefusesReplacedPod(t *testing.T) {
	api := newFakeAPI(t)
	profile := api.profile(t)
	pod := api.agentPod(t, profile, "replaced", nil)
	writeOutputs(t, profile, pod.Name, map[string]string{"report.md": "report"})
	stale := handleOf(pod)
	stale.Data = bytes.Replace(stale.Data, []byte(pod.UID), []byte("uid-of-the-purged-pod"), 1)
	for name, call := range map[string]func(context.Context) error{
		"follow": func(ctx context.Context) error {
			_, err := profile.Follow(ctx, stale, 0, make(chan pluginsdk.Line))
			return err
		},
		"stop": func(ctx context.Context) error { return profile.Stop(ctx, stale) },
		"collect": func(ctx context.Context) error {
			return profile.Collect(ctx, stale, pluginsdk.Limits{Files: 10}, make(chan pluginsdk.Output, 10))
		},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := call(ctx)
		cancel()
		if !errors.Is(err, errReplaced) {
			t.Fatalf("%s through a stale handle: %v; want the pod was replaced", name, err)
		}
	}
	if deleted := paths(api.recorded(), http.MethodDelete); deleted != nil {
		t.Fatalf("deletes %v through a stale handle", deleted)
	}
}

func TestStopDeletesWithTheGraceAndKeepsOutputs(t *testing.T) {
	fast(t)
	api := newFakeAPI(t)
	profile := api.profile(t)
	api.set(func() { api.linger = 3 })
	pod := api.agentPod(t, profile, "stop", nil)
	writeOutputs(t, profile, pod.Name, map[string]string{"report.md": "report"})
	if err := profile.Stop(context.Background(), handleOf(pod)); err != nil {
		t.Fatal(err)
	}
	if api.pod(pod.Name) != nil {
		t.Fatal("stop returned before the pod went")
	}
	var deletes []string
	for _, r := range api.recorded() {
		if r.method == http.MethodDelete {
			deletes = append(deletes, r.path+" "+string(r.body))
		}
	}
	if len(deletes) != 1 || !strings.HasPrefix(deletes[0], "/api/v1/namespaces/agents/pods/"+pod.Name+" ") || !strings.Contains(deletes[0], `"gracePeriodSeconds":15`) {
		t.Fatalf("deletes %v; want the pod deleted with grace 15", deletes)
	}
	collected, err := outputsOf(t, profile, handleOf(pod), limits)
	if err != nil || len(collected) != 1 || string(collected[0].Data) != "report" {
		t.Fatalf("collected %v, %v after stop; want the report", collected, err)
	}
}

func TestStopReturnsWhenCutShortOfThePodGoing(t *testing.T) {
	fast(t)
	api := newFakeAPI(t)
	profile := api.profile(t)
	api.set(func() { api.linger = 1 << 20 })
	pod := api.agentPod(t, profile, "slow", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := profile.Stop(ctx, handleOf(pod)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stop = %v; want the deadline while the pod terminates", err)
	}
}

func TestStopReturnsTheDeleteError(t *testing.T) {
	fast(t)
	api := newFakeAPI(t)
	profile := api.profile(t)
	pod := api.agentPod(t, profile, "undeletable", nil)
	api.set(func() { api.intercept = failing(http.StatusInternalServerError, http.MethodDelete, "/pods/"+pod.Name) })
	var failure *pluginsdk.Error
	if err := profile.Stop(context.Background(), handleOf(pod)); !errors.As(err, &failure) || failure.Op != "delete pod "+pod.Name {
		t.Fatalf("stop = %v; want the delete's error", err)
	}
}

func outputsOf(t *testing.T, profile *Kubernetes, handle pluginsdk.Handle, limits pluginsdk.Limits) ([]pluginsdk.Output, error) {
	t.Helper()
	outputs := make(chan pluginsdk.Output, 64)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := profile.Collect(ctx, handle, limits, outputs)
	close(outputs)
	var collected []pluginsdk.Output
	for output := range outputs {
		collected = append(collected, output)
	}
	return collected, err
}

var limits = pluginsdk.Limits{Files: 64, FileBytes: 1 << 20, TotalBytes: 4 << 20}

func writeOutputs(t *testing.T, profile *Kubernetes, pod string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(runDir(profile, pod), "outputs")
	for name, data := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestStopOfAnEndedAgentDeletesNothing(t *testing.T) {
	api := newFakeAPI(t)
	profile := api.profile(t)
	api.set(func() { api.kubelet = endsAfter(0, 0) })
	pod := api.agentPod(t, profile, "ended", nil)
	if err := profile.Stop(context.Background(), handleOf(pod)); err != nil || paths(api.recorded(), http.MethodDelete) != nil {
		t.Fatalf("stop = %v with deletes %v; want nil and none", err, paths(api.recorded(), http.MethodDelete))
	}
	absent := handleOf(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: poolPrefix + "run-gone", UID: "uid-gone"}})
	if err := profile.Stop(context.Background(), absent); err != nil {
		t.Fatalf("stop of an absent pod = %v", err)
	}
}

func TestStopOfANeverStartedPodLeavesIt(t *testing.T) {
	api := newFakeAPI(t)
	profile := api.profile(t)
	api.set(func() { api.kubelet = nil })
	pod := api.put(captured(t, "image-pull-backoff"))
	if err := profile.Stop(context.Background(), handleOf(pod)); err != nil {
		t.Fatalf("stop = %v; want nil", err)
	}
	if deleted := paths(api.recorded(), http.MethodDelete); deleted != nil {
		t.Fatalf("stop deleted %v; the core purges a pod that never started", deleted)
	}
}

func TestStopDeletesAnUnsettledPod(t *testing.T) {
	fast(t)
	api := newFakeAPI(t)
	profile := api.profile(t)
	api.set(func() { api.kubelet = blipsThen(1, api.kubelet) })
	pod := api.agentPod(t, profile, "blip", nil)
	if err := profile.Stop(context.Background(), handleOf(pod)); err != nil {
		t.Fatal(err)
	}
	if deleted := paths(api.recorded(), http.MethodDelete); len(deleted) != 1 {
		t.Fatalf("deletes %v; want the pod deleted", deleted)
	}
}

func TestCollectWaitsForTheAgentToEnd(t *testing.T) {
	fast(t)
	api := newFakeAPI(t)
	profile := api.profile(t)
	api.set(func() { api.kubelet = endsAfter(3, 0) })
	pod := api.agentPod(t, profile, "waits", nil)
	writeOutputs(t, profile, pod.Name, map[string]string{"report.md": "report"})
	if collected, err := outputsOf(t, profile, handleOf(pod), limits); err != nil || len(collected) != 1 {
		t.Fatalf("collected %v, %v", collected, err)
	}
}

func TestCollectRefusesARunningAgent(t *testing.T) {
	fast(t)
	api := newFakeAPI(t)
	profile := api.profile(t)
	pod := api.agentPod(t, profile, "running", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := profile.Collect(ctx, handleOf(pod), limits, make(chan pluginsdk.Output, 1)); err == nil || err.Error() != "the agent is still running" {
		t.Fatalf("collect = %v", err)
	}
}

func TestCollectRefusesADeletingPod(t *testing.T) {
	api := newFakeAPI(t)
	profile := api.profile(t)
	api.set(func() { api.kubelet = nil })
	pod := api.put(captured(t, "lost-node-running"))
	if _, err := outputsOf(t, profile, handleOf(pod), limits); err == nil || err.Error() != "the pod is being deleted" {
		t.Fatalf("collect = %v", err)
	}
}

func TestCollectCountsTheTreeBeforeReading(t *testing.T) {
	api := newFakeAPI(t)
	profile := api.profile(t)
	api.set(func() { api.kubelet = endsAfter(0, 0) })
	pod := api.agentPod(t, profile, "count", nil)
	writeOutputs(t, profile, pod.Name, map[string]string{"a": "a", "b": "b", "c/d": "d"})
	collected, err := outputsOf(t, profile, handleOf(pod), pluginsdk.Limits{Files: 3, FileBytes: 10, TotalBytes: 10})
	if err == nil || len(collected) != 0 {
		t.Fatalf("collected %v, %v from a tree of four entries; want it refused", collected, err)
	}
}

func TestCollectReadsOnlyRegularFiles(t *testing.T) {
	api := newFakeAPI(t)
	profile := api.profile(t)
	api.set(func() { api.kubelet = endsAfter(0, 0) })
	pod := api.agentPod(t, profile, "regular", nil)
	dir := writeOutputs(t, profile, pod.Name, map[string]string{"report.md": "report", "sub/notes.txt": "notes"})
	if err := os.Symlink("report.md", filepath.Join(dir, "link.md")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "fifo"), 0o644); err != nil {
		t.Fatal(err)
	}
	collected, err := outputsOf(t, profile, handleOf(pod), limits)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, output := range collected {
		got = append(got, fmt.Sprintf("%s %v %q", output.Name, output.Mode, output.Data))
	}
	want := []string{`fifo p--------- ""`, `link.md L--------- ""`, `report.md ---------- "report"`, `sub/notes.txt ---------- "notes"`}
	if !slices.Equal(got, want) {
		t.Fatalf("collected %v; want %v", got, want)
	}
}

func TestCollectLeavesFilesOverTheLimitsUnread(t *testing.T) {
	api := newFakeAPI(t)
	profile := api.profile(t)
	api.set(func() { api.kubelet = endsAfter(0, 0) })
	pod := api.agentPod(t, profile, "limits", nil)
	writeOutputs(t, profile, pod.Name, map[string]string{"a": "aaaa", "b": "bbbbbbbbbbbb", "c": "cccc", "d": "dddd"})
	collected, err := outputsOf(t, profile, handleOf(pod), pluginsdk.Limits{Files: 10, FileBytes: 8, TotalBytes: 8})
	if err != nil {
		t.Fatal(err)
	}
	read := map[string]bool{}
	for _, output := range collected {
		read[output.Name] = output.Data != nil
	}
	if want := map[string]bool{"a": true, "b": false, "c": true, "d": false}; !maps.Equal(read, want) {
		t.Fatalf("read %v; want %v", read, want)
	}
}

// An agent can plant links in its outputs; the plugin mounts every run's directory, so it follows none out of the run.
func TestCollectFollowsNoLinkOutOfTheRun(t *testing.T) {
	api := newFakeAPI(t)
	profile := api.profile(t)
	api.set(func() { api.kubelet = endsAfter(0, 0) })
	other := filepath.Join(runDir(profile, poolPrefix+"run-other"), "inputs")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "secret"), []byte("other-run-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	pod := api.agentPod(t, profile, "links", nil)
	dir := writeOutputs(t, profile, pod.Name, map[string]string{"report.md": "r"})
	for link, target := range map[string]string{
		"relative": "../../" + poolPrefix + "run-other/inputs/secret", "absolute": filepath.Join(other, "secret"), "dir": "../../" + poolPrefix + "run-other/inputs",
	} {
		if err := os.Symlink(target, filepath.Join(dir, link)); err != nil {
			t.Fatal(err)
		}
	}
	collected, err := outputsOf(t, profile, handleOf(pod), pluginsdk.Limits{Files: 10, FileBytes: 1 << 20, TotalBytes: 1 << 20, Stream: []string{"relative", "dir/secret"}, StreamBytes: 1 << 20})
	if err != nil || len(collected) != 4 {
		t.Fatalf("collected %v, %v; want the report and three links", collected, err)
	}
	for _, output := range collected {
		if output.Name != "report.md" && (output.Mode != fs.ModeSymlink || output.Data != nil || output.Body != nil) {
			t.Fatalf("output %+v; want a link listed and never read", output)
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, err := read(root, "dir/secret", int64(len("other-run-secret"))); err == nil {
		t.Fatal("read an output through a link out of the run")
	}
	// The outputs directory itself may be a link.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../"+poolPrefix+"run-other/inputs", dir); err != nil {
		t.Fatal(err)
	}
	if collected, err := outputsOf(t, profile, handleOf(pod), limits); err == nil || len(collected) != 0 {
		t.Fatalf("collected %v, %v through a linked outputs directory", collected, err)
	}
}

func TestCollectReadsOutputsAfterThePodWent(t *testing.T) {
	api := newFakeAPI(t)
	profile := api.profile(t)
	pod := api.agentPod(t, profile, "gone", nil)
	writeOutputs(t, profile, pod.Name, map[string]string{"report.md": "report"})
	api.set(func() { delete(api.pods, pod.Name) })
	if collected, err := outputsOf(t, profile, handleOf(pod), limits); err != nil || len(collected) != 1 || string(collected[0].Data) != "report" {
		t.Fatalf("collected %v, %v; want the report of the deleted pod", collected, err)
	}
}

func TestCollectStreamsNamedOutputs(t *testing.T) {
	api := newFakeAPI(t)
	profile := api.profile(t)
	api.set(func() { api.kubelet = endsAfter(0, 0) })
	pod := api.agentPod(t, profile, "stream", nil)
	writeOutputs(t, profile, pod.Name, map[string]string{"build.tar": "tarball-bytes", "big.tar": "too-big-to-stream", "report.md": "r"})
	outputs := make(chan pluginsdk.Output, 8)
	if err := profile.Collect(context.Background(), handleOf(pod), pluginsdk.Limits{Files: 10, FileBytes: 1, TotalBytes: 1, Stream: []string{"build.tar", "big.tar"}, StreamBytes: 13}, outputs); err != nil {
		t.Fatal(err)
	}
	close(outputs)
	for output := range outputs {
		switch output.Name {
		case "build.tar":
			data, err := io.ReadAll(output.Body)
			output.Body.Close()
			if err != nil || string(data) != "tarball-bytes" || output.Data != nil {
				t.Fatalf("streamed %q, %v", data, err)
			}
		case "big.tar":
			if output.Body != nil || output.Data != nil {
				t.Fatal("an output over StreamBytes was streamed")
			}
		case "report.md":
			if string(output.Data) != "r" || output.Body != nil {
				t.Fatalf("report.md %+v", output)
			}
		}
	}
}

// The receiver reads the agent's outcome from .wherry/outcome.json, nested beneath the outputs directory, by streaming it.
func TestCollectStreamsTheNestedOutcome(t *testing.T) {
	const outcome = `{"state":"TASK_STATE_COMPLETED"}`
	api := newFakeAPI(t)
	profile := api.profile(t)
	api.set(func() { api.kubelet = endsAfter(0, 0) })
	pod := api.agentPod(t, profile, "outcome", nil)
	writeOutputs(t, profile, pod.Name, map[string]string{".wherry/outcome.json": outcome, "report.md": "r"})
	collected, err := outputsOf(t, profile, handleOf(pod), pluginsdk.Limits{Files: 10, FileBytes: 1 << 20, TotalBytes: 1 << 20, Stream: []string{".wherry/outcome.json"}, StreamBytes: 1 << 10})
	if err != nil || len(collected) != 2 || collected[0].Name != ".wherry/outcome.json" || collected[0].Data != nil || collected[0].Body == nil {
		t.Fatalf("collected %+v, %v; want the outcome streamed and report.md", collected, err)
	}
	data, err := io.ReadAll(collected[0].Body)
	collected[0].Body.Close()
	if err != nil || string(data) != outcome {
		t.Fatalf("streamed outcome %q, %v", data, err)
	}
}

func TestCollectWithoutAnOutputsDirectory(t *testing.T) {
	api := newFakeAPI(t)
	profile := api.profile(t)
	api.set(func() { api.kubelet = endsAfter(0, 0) })
	pod := api.agentPod(t, profile, "none", nil)
	if collected, err := outputsOf(t, profile, handleOf(pod), limits); err != nil || len(collected) != 0 {
		t.Fatalf("collected %v, %v; want nothing", collected, err)
	}
}

func TestReleaseDeletesTheSecret(t *testing.T) {
	api := newFakeAPI(t)
	profile := api.profile(t)
	api.set(func() { api.kubelet = endsAfter(0, 0) })
	run := testRun("release")
	dispatch(t, profile, run)
	name := profile.jobName(run.ID)
	if err := profile.Release(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	if api.secret(name) != nil {
		t.Fatal("the Secret remains")
	}
	if _, err := os.Stat(runDir(profile, name)); api.pod(name) == nil || err != nil {
		t.Fatalf("release removed the pod or its run directory (%v); the sweep removes them", err)
	}
}

func TestPurgeDeletesWithGraceZero(t *testing.T) {
	api := newFakeAPI(t)
	profile := api.profile(t)
	api.agentPod(t, profile, "purge", nil)
	if err := profile.Purge(context.Background(), "run-purge"); err != nil {
		t.Fatal(err)
	}
	for _, r := range api.recorded() {
		if r.method == http.MethodDelete && strings.Contains(r.path, "/pods/") && !strings.Contains(string(r.body), `"gracePeriodSeconds":0`) {
			t.Fatalf("delete body %s; want grace 0", r.body)
		}
	}
}

func TestPurgeWaitsForThePodToGo(t *testing.T) {
	fast(t)
	api := newFakeAPI(t)
	profile := api.profile(t)
	run := testRun("linger")
	dispatch(t, profile, run)
	api.set(func() { api.linger = 3 })
	if err := profile.Purge(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	name := profile.jobName(run.ID)
	if api.pod(name) != nil || api.secret(name) != nil {
		t.Fatal("purge returned with the pod or its Secret left")
	}
	requests := paths(api.recorded(), "")
	if last := requests[len(requests)-1]; last != "DELETE /api/v1/namespaces/agents/secrets/"+name {
		t.Fatalf("last request %s; want the Secret deleted after the pod went", last)
	}
	if _, err := os.Stat(runDir(profile, name)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("purge left the run directory: %v", err)
	}
}

func TestPurgeOfAbsentPodSucceeds(t *testing.T) {
	api := newFakeAPI(t)
	profile := api.profile(t)
	if err := profile.Purge(context.Background(), "run-absent"); err != nil {
		t.Fatal(err)
	}
}

// sweepPod is an agent pod of profile owner whose agent ended ago, or is running when ago is 0.
func (api *fakeAPI) sweepPod(t *testing.T, profile *Kubernetes, name, owner string, ago time.Duration) string {
	t.Helper()
	pod := api.agentPod(t, profile, name, func(pod *corev1.Pod) {
		pod.Labels[labelProfile] = owner
		if ago == 0 {
			setStatus(pod, corev1.PodRunning, runningStatus("agent"))
			return
		}
		agent := exited("agent", 0)
		agent.State.Terminated.FinishedAt = metav1.NewTime(time.Now().Add(-ago))
		setStatus(pod, corev1.PodRunning, agent)
	})
	api.secretFor(pod.Name, owner, 0)
	return pod.Name
}

// secretFor stores the turn Secret of name, created ago.
func (api *fakeAPI) secretFor(name, owner string, ago time.Duration) {
	api.mu.Lock()
	defer api.mu.Unlock()
	_, run, _ := strings.Cut(name, poolPrefix)
	api.secrets[name] = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, ResourceVersion: "1", CreationTimestamp: metav1.NewTime(time.Now().Add(-ago)),
		Labels: map[string]string{labelPool: testPool, labelProfile: owner, labelAgent: "coder", labelRun: run}}, Data: map[string][]byte{"prompt": []byte("p")}}
}

func sweep(t *testing.T, api *fakeAPI) *Kubernetes {
	t.Helper()
	profile := api.profile(t)
	api.set(func() { api.kubelet = nil })
	return profile
}

func TestSweepPurgesPodsEndedPastTheGrace(t *testing.T) {
	api := newFakeAPI(t)
	profile := sweep(t, api)
	name := api.sweepPod(t, profile, "old", testProfile, 11*time.Minute)
	if err := profile.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.pod(name) != nil || api.secret(name) != nil {
		t.Fatal("the ended pod or its Secret remains")
	}
}

func TestSweepKeepsPodsInsideTheGrace(t *testing.T) {
	api := newFakeAPI(t)
	profile := sweep(t, api)
	name := api.sweepPod(t, profile, "recent", testProfile, 9*time.Minute)
	if err := profile.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.pod(name) == nil {
		t.Fatal("a pod inside the grace was purged")
	}
}

func TestSweepPurgesADeletingPodPastTheGrace(t *testing.T) {
	for ago, purged := range map[time.Duration]bool{11 * time.Minute: true, 9 * time.Minute: false} {
		api := newFakeAPI(t)
		profile := sweep(t, api)
		pod := captured(t, "lost-node-running")
		pod.Labels = profile.labels(testRun("lost"))
		pod.Name = profile.jobName("run-lost")
		pod.Labels[labelProfile] = testProfile
		pod.DeletionTimestamp = new(metav1.NewTime(time.Now().Add(-ago)))
		api.put(pod)
		api.secretFor(pod.Name, testProfile, time.Hour)
		if err := profile.Sweep(context.Background()); err != nil {
			t.Fatal(err)
		}
		if gone := api.pod(pod.Name) == nil && api.secret(pod.Name) == nil; gone != purged {
			t.Fatalf("deleting for %v: purged %t; want %t", ago, gone, purged)
		}
	}
}

func TestSweepCountsOnlyThisProfilesPods(t *testing.T) {
	api := newFakeAPI(t)
	container := sweep(t, api)
	waiting := api.sweepPod(t, container, "waiting", testProfile, time.Minute)
	expired := api.sweepPod(t, container, "expired", testProfile, 11*time.Minute)
	host := api.profile(t)
	host.name = "host"
	if err := host.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, deleted := range paths(api.recorded(), http.MethodDelete) {
		if strings.Contains(deleted, "/pods/") {
			t.Fatalf("host's sweep deleted %v of container's", deleted)
		}
	}
	if got := samples(t, host); got["wherry_plugin_cleanup_pending/"].Value != 0 || got["wherry_plugin_sweep_deletions_total/job"].Value != 0 || got["wherry_plugin_sweep_deletions_total/variable"].Value != 0 {
		t.Fatalf("host samples %+v; want none of container's pods counted", got)
	}
	if err := container.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.pod(expired) != nil || api.pod(waiting) == nil || api.secret(waiting) != nil {
		t.Fatal("container's sweep did not purge its expired pod and keep its waiting one")
	}
	if got := samples(t, container); got["wherry_plugin_cleanup_pending/"].Value != 1 || got["wherry_plugin_sweep_deletions_total/job"].Value != 1 || got["wherry_plugin_sweep_deletions_total/variable"].Value != 1 {
		t.Fatalf("container samples %+v; want its waiting pod pending, its expired pod and the waiting pod's Secret deleted", got)
	}
}

func TestSweepDeletesSecretOfEndedPod(t *testing.T) {
	api := newFakeAPI(t)
	profile := sweep(t, api)
	name := api.sweepPod(t, profile, "ended", testProfile, time.Minute)
	if err := profile.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.secret(name) != nil || api.pod(name) == nil {
		t.Fatal("want the ended pod's Secret deleted and the pod kept")
	}
}

func TestSweepDeletesSecretOfAnotherProfilesExpiredPod(t *testing.T) {
	api := newFakeAPI(t)
	profile := sweep(t, api)
	expired := api.sweepPod(t, profile, "removed", "removed", 11*time.Minute)
	recent := api.sweepPod(t, profile, "other", "removed", time.Minute)
	if err := profile.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.secret(expired) != nil || api.pod(expired) == nil || api.secret(recent) == nil {
		t.Fatal("want the expired pod's Secret deleted, the pod kept, and the recent pod's Secret kept")
	}
	if got := samples(t, profile)["wherry_plugin_sweep_deletions_total/variable"].Value; got != 0 {
		t.Fatalf("counted %v deletions of another profile's Secrets", got)
	}
}

func TestSweepKeepsSecretOfRunningPod(t *testing.T) {
	api := newFakeAPI(t)
	profile := sweep(t, api)
	name := api.sweepPod(t, profile, "running", testProfile, 0)
	api.secretFor(name, testProfile, time.Hour)
	if err := profile.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.secret(name) == nil {
		t.Fatal("the running pod's Secret was deleted")
	}
}

func TestSweepDeletesStaleSecretWithoutPod(t *testing.T) {
	api := newFakeAPI(t)
	profile := sweep(t, api)
	api.secretFor(poolPrefix+"run-orphan", testProfile, 11*time.Minute)
	if err := profile.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.secret(poolPrefix+"run-orphan") != nil {
		t.Fatal("the orphaned Secret remains")
	}
}

func TestSweepKeepsRecentSecretWithoutPod(t *testing.T) {
	api := newFakeAPI(t)
	profile := sweep(t, api)
	api.secretFor(poolPrefix+"run-new", testProfile, time.Minute)
	if err := profile.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.secret(poolPrefix+"run-new") == nil {
		t.Fatal("a Secret inside the grace was deleted")
	}
}

func TestSweepIgnoresForeignNames(t *testing.T) {
	api := newFakeAPI(t)
	profile := sweep(t, api)
	pod := api.agentPod(t, profile, "foreign", func(pod *corev1.Pod) {
		pod.Name = "user-pod"
		setStatus(pod, corev1.PodRunning, exited("agent", 0))
		pod.Status.ContainerStatuses[0].State.Terminated.FinishedAt = metav1.NewTime(time.Now().Add(-time.Hour))
	})
	api.secretFor("user-secret", testProfile, time.Hour)
	if err := profile.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.pod(pod.Name) == nil || api.secret("user-secret") == nil {
		t.Fatal("the sweep deleted objects whose names are not their labels' job names")
	}
}

func TestSweepListsSecretsAsMetadata(t *testing.T) {
	api := newFakeAPI(t)
	profile := sweep(t, api)
	if err := profile.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, r := range api.recorded() {
		if r.method == http.MethodGet && strings.HasSuffix(r.path, "/secrets") {
			if !strings.Contains(r.header.Get("Accept"), "as=PartialObjectMetadataList") {
				t.Fatalf("secret list accepts %q; want metadata", r.header.Get("Accept"))
			}
			if !strings.Contains(r.query, "labelSelector=wherry.io%2Fpool%3Dbuild") {
				t.Fatalf("secret list query %q; want the pool's selector", r.query)
			}
			return
		}
	}
	t.Fatal("the sweep listed no Secrets")
}

// runDirFor makes the run directory of name, last changed ago.
func runDirFor(t *testing.T, profile *Kubernetes, name string, ago time.Duration) string {
	t.Helper()
	dir := runDir(profile, name)
	if err := os.MkdirAll(filepath.Join(dir, "outputs"), 0o700); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-ago)
	if err := os.Chtimes(dir, at, at); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSweepRemovesTheDirectoriesOfGoneRuns(t *testing.T) {
	api := newFakeAPI(t)
	profile := sweep(t, api)
	gone := runDirFor(t, profile, poolPrefix+"run-gone", 11*time.Minute)
	running := runDirFor(t, profile, api.sweepPod(t, profile, "running", testProfile, 0), 11*time.Minute)
	other := runDirFor(t, profile, api.sweepPod(t, profile, "other", "another", 11*time.Minute), 11*time.Minute)
	api.secretFor(poolPrefix+"run-released", testProfile, time.Minute)
	released := runDirFor(t, profile, poolPrefix+"run-released", 11*time.Minute)
	recent := runDirFor(t, profile, poolPrefix+"run-recent", 9*time.Minute)
	foreign := runDirFor(t, profile, jobName("review", "run-gone"), time.Hour)
	if err := profile.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	for dir, kept := range map[string]bool{gone: false, running: true, other: true, released: true, recent: true, foreign: true} {
		if _, err := os.Stat(dir); (err == nil) != kept {
			t.Fatalf("%s: kept %t; want %t", filepath.Base(dir), err == nil, kept)
		}
	}
	if got := samples(t, profile)["wherry_plugin_sweep_deletions_total/directory"].Value; got != 1 {
		t.Fatalf("counted %v directories; want 1", got)
	}
	// The sweep purges an expired pod with its directory.
	expired := runDirFor(t, profile, api.sweepPod(t, profile, "expired", testProfile, 11*time.Minute), time.Minute)
	if err := profile.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(expired); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the purged run's directory remains: %v", err)
	}
}

func TestSweepReportsDeletionsAndPendingPods(t *testing.T) {
	api := newFakeAPI(t)
	profile := sweep(t, api)
	api.sweepPod(t, profile, "old", testProfile, 11*time.Minute)
	api.sweepPod(t, profile, "recent", testProfile, time.Minute)
	api.secretFor(poolPrefix+"run-gone", testProfile, time.Hour)
	if err := profile.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := samples(t, profile)
	if got["wherry_plugin_cleanup_pending/"].Value != 1 || got["wherry_plugin_sweep_deletions_total/job"].Value != 1 || got["wherry_plugin_sweep_deletions_total/variable"].Value != 2 {
		t.Fatalf("samples %+v; want one pending pod, one job and two variables deleted", got)
	}
	for name := range got {
		if family, kind, _ := strings.Cut(name, "/"); family == "wherry_plugin_sweep_deletions_total" && kind != "job" && kind != "variable" && kind != "directory" {
			t.Fatalf("deletion kind %q outside the core's", kind)
		}
	}
}

func TestReadyFromTheNamespace(t *testing.T) {
	for phase, want := range map[corev1.NamespacePhase]bool{corev1.NamespaceActive: true, corev1.NamespaceTerminating: false, "": false} {
		api := newFakeAPI(t)
		profile := api.profile(t)
		profile.floor = 0
		api.set(func() { api.phase = phase })
		ready, err := profile.Ready(context.Background())
		if ready != want || (phase == "") != (err != nil) {
			t.Fatalf("namespace %q: ready = %v, %v; want %v", phase, ready, err, want)
		}
	}
}

func TestReadyBelowTheFloor(t *testing.T) {
	api := newFakeAPI(t)
	profile := api.profile(t)
	free, err := profile.free()
	if err != nil || free <= 0 {
		t.Fatalf("free %d, %v", free, err)
	}
	for floor, want := range map[int64]bool{0: true, free + 1<<30: false} {
		profile.floor = floor
		if ready, err := profile.Ready(context.Background()); err != nil || ready != want {
			t.Fatalf("floor %d over %d free: ready = %v, %v; want %v", floor, free, ready, err, want)
		}
	}
	if sample := samples(t, profile)["wherry_plugin_storage_free_bytes/"]; sample.Kind != pluginsdk.KindGauge || sample.Value < float64(free)/2 {
		t.Fatalf("free bytes sample %+v; want about %d", sample, free)
	}
	profile.runsDir = filepath.Join(t.TempDir(), "absent")
	if ready, err := profile.Ready(context.Background()); ready || err == nil {
		t.Fatalf("ready = %v, %v without the runs claim; want an error", ready, err)
	}
}

func TestAPIErrorsAreClassifiedByStatus(t *testing.T) {
	for _, test := range []struct {
		code   int
		reason metav1.StatusReason
		class  string
	}{
		{http.StatusUnauthorized, metav1.StatusReasonUnauthorized, "denied"},
		{http.StatusForbidden, metav1.StatusReasonForbidden, "denied"},
		{http.StatusConflict, metav1.StatusReasonConflict, "conflict"},
		{http.StatusRequestTimeout, "", "timeout"},
		{http.StatusGatewayTimeout, "", "timeout"},
		{http.StatusInternalServerError, metav1.StatusReasonServerTimeout, "timeout"},
		{http.StatusTooManyRequests, metav1.StatusReasonTooManyRequests, "unavailable"},
		{http.StatusInternalServerError, metav1.StatusReasonInternalError, "unavailable"},
		{http.StatusServiceUnavailable, metav1.StatusReasonServiceUnavailable, "unavailable"},
		{http.StatusBadRequest, metav1.StatusReasonBadRequest, "other"},
		{http.StatusNotFound, metav1.StatusReasonNotFound, ""},
	} {
		api := newFakeAPI(t)
		profile := api.profile(t)
		api.set(func() {
			api.intercept = func(response http.ResponseWriter, _ *http.Request) bool {
				apiStatus(response, test.code, test.reason, "refused")
				return true
			}
		})
		if _, err := profile.Ready(context.Background()); err == nil {
			t.Fatalf("ready answered %d without an error", test.code)
		}
		var counted []string
		for name, sample := range samples(t, profile) {
			if sample.Family == "wherry_plugin_platform_errors_total" {
				counted = append(counted, fmt.Sprintf("%s=%v", name, sample.Value))
			}
		}
		want := []string(nil)
		if test.class != "" {
			want = []string{"wherry_plugin_platform_errors_total/" + test.class + "=1"}
		}
		if !slices.Equal(counted, want) {
			t.Fatalf("status %d %s counted %v; want %v", test.code, test.reason, counted, want)
		}
	}
	api := newFakeAPI(t)
	profile := api.profile(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := profile.Ready(ctx); err == nil || len(slices.DeleteFunc(profile.metrics.Samples(), func(s pluginsdk.Sample) bool { return s.Family != "wherry_plugin_platform_errors_total" })) != 0 {
		t.Fatalf("a cancelled call: %v, counted", err)
	}
}

func TestUnreachableAPIIsUnavailable(t *testing.T) {
	profile := plainProfile(t, nil)
	if _, err := profile.Ready(context.Background()); err == nil {
		t.Fatal("ready reached a closed port")
	}
	if sample := samples(t, profile)["wherry_plugin_platform_errors_total/unavailable"]; sample.Value != 1 {
		t.Fatalf("unavailable errors %v; want 1", sample.Value)
	}
}

func TestReturnedErrorCarriesOpAndClass(t *testing.T) {
	const message = `namespaces "agents" is forbidden: User "system:serviceaccount:wherry:wherry-receiver" cannot get resource "namespaces"`
	api := newFakeAPI(t)
	profile := api.profile(t)
	api.set(func() {
		api.intercept = func(response http.ResponseWriter, _ *http.Request) bool {
			apiStatus(response, http.StatusForbidden, metav1.StatusReasonForbidden, message)
			return true
		}
	})
	_, err := profile.Ready(context.Background())
	var failure *pluginsdk.Error
	if !errors.As(err, &failure) || failure.Op != "read namespace agents" || failure.Class != "denied" || !strings.Contains(err.Error(), message) {
		t.Fatalf("error %#v; want op, class denied and the API's message", err)
	}
}

// A returned error is the receiver's to log; one the plugin handles itself, such as a poll read, it logs by op and class.
func TestHandledPlatformErrorIsLoggedOnceWithoutURLOrToken(t *testing.T) {
	const body = "body-never-logged"
	api := newFakeAPI(t)
	profile := api.profile(t)
	var logs bytes.Buffer
	profile.logger = hclog.New(&hclog.LoggerOptions{Output: &logs, JSONFormat: true})
	api.set(func() {
		api.intercept = func(response http.ResponseWriter, _ *http.Request) bool {
			apiStatus(response, http.StatusServiceUnavailable, metav1.StatusReasonServiceUnavailable, body)
			return true
		}
	})
	if _, err := profile.Ready(context.Background()); err == nil {
		t.Fatal("ready answered 503 without an error")
	}
	if logs.Len() != 0 {
		t.Fatalf("returned error logged by the plugin: %q", logs.String())
	}
	if outcome, _, err := profile.notStarted(context.Background(), poolPrefix+"run-x", errors.New("cause")); outcome != pluginsdk.OutcomeUnknown || err == nil {
		t.Fatalf("notStarted = %v, %v", outcome, err)
	}
	lines := strings.Split(strings.TrimSuffix(logs.String(), "\n"), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], `"class":"unavailable"`) || !strings.Contains(lines[0], `"op":"read pod `+poolPrefix+`run-x"`) || !strings.Contains(lines[0], `"@level":"warn"`) {
		t.Fatalf("logs %q; want one warning naming the operation and class unavailable", logs.String())
	}
	for _, forbidden := range []string{testToken, body, strings.TrimPrefix(api.server.URL, "https://"), "/api/v1"} {
		if strings.Contains(logs.String(), forbidden) {
			t.Fatalf("log line %q holds %q", lines[0], forbidden)
		}
	}
}

func TestWarningsReachTheLogger(t *testing.T) {
	api := newFakeAPI(t)
	var logs bytes.Buffer
	config := testConfig(api.server.URL, api.caFile)
	config.RunsDir = t.TempDir()
	profile, err := New(config, testProfile, testPool, mustClient(t, api), hclog.New(&hclog.LoggerOptions{Output: &logs, JSONFormat: true}))
	if err != nil {
		t.Fatal(err)
	}
	api.set(func() {
		api.intercept = func(response http.ResponseWriter, _ *http.Request) bool {
			response.Header().Set("Warning", `299 - "would violate PodSecurity \"restricted:latest\""`)
			return false
		}
	})
	if _, err := profile.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), `"@message":"kubernetes warning"`) || !strings.Contains(logs.String(), `would violate PodSecurity`) {
		t.Fatalf("logs %q; want the warning", logs.String())
	}
}

func mustClient(t *testing.T, api *fakeAPI) *rest.Config {
	t.Helper()
	client, err := newClient(testConfig(api.server.URL, api.caFile), testToken)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// turn runs a whole turn with every kind of material through the fake, and returns every request, pod spec, error and
// log line it produced.
func turn(t *testing.T) (string, []string) {
	t.Helper()
	fast(t)
	api := newFakeAPI(t)
	profile := api.profile(t)
	var logs bytes.Buffer
	profile.logger = hclog.New(&hclog.LoggerOptions{Output: &logs, JSONFormat: true})
	files := map[string][]byte{"data.csv": []byte("fetched-bytes-secret")}
	store := newObjects(t, serveFiles(files))
	run := fetchRun("turn", store, files)
	run.Model = "sonnet"
	run.Inputs = []pluginsdk.File{{Name: "notes.md", Data: []byte("input-bytes-secret")}}
	run.Credentials = []pluginsdk.Secret{
		{Name: "model", Env: "MODEL_API_KEY", Value: []byte("env-credential-secret")}, {Name: "git", Env: "GIT_TOKEN", Value: []byte("second-env-credential-secret")},
		{Name: "deploy", File: "id_ed25519", Value: []byte("task-credential-secret")}, {Name: "ca", File: "ca.pem", Value: []byte("second-file-credential-secret")},
	}
	name := profile.jobName(run.ID)
	var errs []error
	handle := dispatch(t, profile, run)
	logsHandler, _ := logStream(func(int) []byte { return []byte(`{"type":"result","result":"done"}` + "\n") }, nil)
	api.set(func() { api.logs = logsHandler })
	writeOutputs(t, profile, name, map[string]string{"report.md": "output-bytes-secret"})
	errs = append(errs, profile.Stop(context.Background(), handle))
	_, _, err := followAll(t, profile, handle, 0)
	errs = append(errs, err)
	collected, err := outputsOf(t, profile, handle, limits)
	if len(collected) != 1 || string(collected[0].Data) != "output-bytes-secret" {
		t.Fatalf("collected %v, %v", collected, err)
	}
	errs = append(errs, err, profile.Release(context.Background(), run.ID), profile.Purge(context.Background(), run.ID), profile.Sweep(context.Background()))
	// Turns whose fetches fail, by status, by connection, by certificate and by digest.
	failed := fetchRun("failed", newObjects(t, func(response http.ResponseWriter, _ *http.Request) {
		http.Error(response, "SignatureDoesNotMatch", http.StatusForbidden)
	}), files)
	unreachable := fetchRun("unreachable", store, files)
	unreachable.Fetches[0].URL = "https://unreachable.invalid/data.csv?X-Amz-Signature=signature-secret"
	untrusted := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(untrusted.Close)
	certificate := fetchRun("certificate", store, files)
	certificate.Fetches[0].URL = untrusted.URL + "/data.csv?X-Amz-Signature=signature-secret"
	refused := fetchRun("refused", store, files)
	refused.Fetches[0].SHA256 = strings.Repeat("0", 64)
	var fetchErrs []string
	for _, run := range []pluginsdk.Run{failed, unreachable, certificate, refused} {
		_, _, err := profile.Dispatch(context.Background(), run)
		errs = append(errs, err)
		fetchErrs = append(fetchErrs, fmt.Sprint(err))
	}
	if want := []string{"fetch data.csv: 403", "fetch data.csv: unreachable", "fetch data.csv: certificate", "fetch data.csv: does not match its digest"}; !slices.Equal(fetchErrs, want) {
		t.Fatalf("fetch errors %q; want %q", fetchErrs, want)
	}
	var seen []string
	for _, r := range api.recorded() {
		seen = append(seen, "request "+r.path+"?"+r.query)
		if r.method == http.MethodPost && strings.HasSuffix(r.path, "/pods") {
			seen = append(seen, "pod "+string(r.body))
		}
		if !strings.Contains(r.path, "/secrets") {
			seen = append(seen, "body "+string(r.body))
		}
	}
	for _, err := range errs {
		if err != nil {
			seen = append(seen, "error "+err.Error())
		}
	}
	for line := range strings.Lines(logs.String()) {
		seen = append(seen, "log "+line)
	}
	// Secret request bodies carry the material by design, but never a URL.
	var secretBodies []string
	for _, r := range api.recorded() {
		if strings.Contains(r.path, "/secrets") {
			var secret corev1.Secret
			_ = json.Unmarshal(r.body, &secret)
			secretBodies = append(secretBodies, string(r.body))
			for _, value := range secret.Data {
				secretBodies = append(secretBodies, string(value))
			}
		}
	}
	return strings.Join(secretBodies, "\n"), seen
}

func TestSecretsNeverInRequestURLs(t *testing.T) {
	secretBodies, seen := turn(t)
	for _, line := range seen {
		for _, secret := range []string{"hello-prompt", "test-credential", "input-bytes-secret", "env-credential-secret", "second-env-credential-secret",
			"task-credential-secret", "second-file-credential-secret", "fetched-bytes-secret", "signature-secret", "X-Amz-Signature", "output-bytes-secret", "unreachable.invalid"} {
			if strings.Contains(line, secret) {
				t.Fatalf("%q holds %q", line, secret)
			}
		}
	}
	for _, url := range []string{"signature-secret", "X-Amz-Signature", "unreachable.invalid", "http://127.0.0.1"} {
		if strings.Contains(secretBodies, url) {
			t.Fatalf("a Secret holds %q", url)
		}
	}
}

func TestTokenNeverInErrorsOrLogs(t *testing.T) {
	secretBodies, seen := turn(t)
	for _, line := range append(seen, secretBodies) {
		if strings.Contains(line, testToken) {
			t.Fatalf("%q holds the token", line)
		}
	}
}

// devConfig is the real-cluster suite's configuration; the suite skips without WHERRY_KUBERNETES_DEV_ADDR.
func devConfig(t *testing.T) Config {
	t.Helper()
	address := os.Getenv("WHERRY_KUBERNETES_DEV_ADDR")
	if address == "" {
		t.Skip("WHERRY_KUBERNETES_DEV_ADDR not set")
	}
	// The suite writes the run directories as the receiver does, through the runs claim mounted at its runs_dir.
	saved := receiverUID
	receiverUID = int64(os.Getuid())
	t.Cleanup(func() { receiverUID = saved })
	return Config{
		Address: address, CAFile: os.Getenv("WHERRY_KUBERNETES_DEV_CA_FILE"), Namespace: os.Getenv("WHERRY_KUBERNETES_DEV_NAMESPACE"),
		RunsClaim: os.Getenv("WHERRY_KUBERNETES_DEV_RUNS_CLAIM"), RunsDir: os.Getenv("WHERRY_KUBERNETES_DEV_RUNS_DIR"),
		Runtime: Runtime{Image: "registry.access.redhat.com/ubi9/ubi-minimal", Command: []string{"/bin/sh", "-c"}, Resources: Resources{CPU: "100m", Memory: "256Mi"},
			User: os.Getenv("WHERRY_KUBERNETES_DEV_USER")},
	}
}

func devToken(t *testing.T) string {
	t.Helper()
	token, err := os.ReadFile(os.Getenv("WHERRY_KUBERNETES_DEV_TOKEN_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(token))
}

// devProfile is a plugin profile on the real cluster whose agent runs script.
func devProfile(t *testing.T, script string, edit func(*Config)) (*Kubernetes, context.Context) {
	t.Helper()
	config := devConfig(t)
	config.Runtime.Args = []string{script}
	if edit != nil {
		edit(&config)
	}
	profile, err := build(config, testPool, devToken(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)
	return profile, ctx
}

func devRun(t *testing.T, profile *Kubernetes, name string) pluginsdk.Run {
	t.Helper()
	run := testRun(fmt.Sprintf("%s-%d", name, time.Now().UnixNano()))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = profile.Release(ctx, run.ID)
		if err := profile.Purge(ctx, run.ID); err != nil {
			t.Errorf("purge %s: %v", run.ID, err)
		}
	})
	return run
}

// devTurn dispatches run and follows it to its end.
func devTurn(t *testing.T, ctx context.Context, profile *Kubernetes, run pluginsdk.Run) (pluginsdk.Handle, []pluginsdk.Line, pluginsdk.Terminal) {
	t.Helper()
	outcome, handle, err := profile.Dispatch(ctx, run)
	if err != nil || outcome != pluginsdk.OutcomeCreated {
		t.Fatalf("dispatch = %v, %v", outcome, err)
	}
	lines, terminal, err := followAll(t, profile, handle, 0)
	if err != nil {
		t.Fatal(err)
	}
	return handle, lines, terminal
}

func devOutputs(t *testing.T, ctx context.Context, profile *Kubernetes, handle pluginsdk.Handle, limits pluginsdk.Limits) map[string]pluginsdk.Output {
	t.Helper()
	outputs := make(chan pluginsdk.Output, 256)
	if err := profile.Collect(ctx, handle, limits, outputs); err != nil {
		t.Fatal(err)
	}
	close(outputs)
	byName := map[string]pluginsdk.Output{}
	for output := range outputs {
		byName[output.Name] = output
	}
	return byName
}

func TestInputsCredentialsAndOutputsOnKubernetes(t *testing.T) {
	profile, ctx := devProfile(t, `set -e; out="$WHERRY_OUTPUT_DIR"; mkdir -p "$out/nested/deep";
		cp "$WHERRY_INPUT_DIR/notes.md" "$out/nested/deep/notes.md"; cat "$WHERRY_CREDENTIALS_DIR/token" > "$out/token";
		cp /wherry/prompt.txt "$out/prompt.txt"; ln -s notes.md "$out/nested/link"; mkfifo "$out/fifo";
		echo '{"type":"result","result":"done"}'`, nil)
	run := devRun(t, profile, "files")
	run.Inputs = []pluginsdk.File{{Name: "notes.md", Data: []byte{'n', 0, 255}}}
	run.Credentials = []pluginsdk.Secret{{Name: "api", File: "token", Value: []byte("task-token")}}
	handle, _, terminal := devTurn(t, ctx, profile, run)
	if !terminal.Started || terminal.ExitCode != 0 {
		t.Fatalf("terminal %+v", terminal)
	}
	got := devOutputs(t, ctx, profile, handle, pluginsdk.Limits{Files: 16, FileBytes: 1 << 20, TotalBytes: 1 << 20})
	if string(got["nested/deep/notes.md"].Data) != "n\x00\xff" || string(got["token"].Data) != "task-token" || string(got["prompt.txt"].Data) != "hello-prompt" ||
		got["nested/link"].Mode != fs.ModeSymlink || got["fifo"].Mode != fs.ModeNamedPipe || got["fifo"].Data != nil {
		t.Fatalf("outputs %v", got)
	}
}

func TestCredentialFileOwnerAndModeOnKubernetes(t *testing.T) {
	profile, ctx := devProfile(t, `set -e; key="$WHERRY_CREDENTIALS_DIR/key";
		{ stat -c '%a %u' "$key"; cat "$key"; echo; ssh-keygen -y -f "$key" >/dev/null 2>&1 && echo accepted || echo refused; } > "$WHERRY_OUTPUT_DIR/modes";
		echo '{"type":"result","result":"done"}'`, nil)
	run := devRun(t, profile, "modes")
	run.Credentials = []pluginsdk.Secret{{Name: "deploy", File: "key", Value: []byte("private-key")}}
	handle, _, _ := devTurn(t, ctx, profile, run)
	modes := strings.Fields(string(devOutputs(t, ctx, profile, handle, limits)["modes"].Data))
	if len(modes) < 3 || modes[0] != "440" || modes[1] != "0" || modes[2] != "private-key" {
		t.Fatalf("modes %v; want the file root's at 0440 and readable by the agent", modes)
	}
}

func TestFetchOnKubernetes(t *testing.T) {
	files := map[string][]byte{"data.csv": []byte("a,b\n1,2\n")}
	store := httptest.NewTLSServer(http.HandlerFunc(serveFiles(files)))
	t.Cleanup(store.Close)
	profile, ctx := devProfile(t, `set -e; cp "$WHERRY_INPUT_DIR/data.csv" "$WHERRY_OUTPUT_DIR/"; echo '{"type":"result","result":"done"}'`, nil)
	profile.fetcher.Transport = store.Client().Transport
	run := devRun(t, profile, "fetch")
	run.Fetches = []pluginsdk.Fetch{{Name: "data.csv", URL: store.URL + "/data.csv?X-Amz-Signature=s", Size: int64(len(files["data.csv"])), SHA256: sha(files["data.csv"])}}
	handle, _, _ := devTurn(t, ctx, profile, run)
	if got := devOutputs(t, ctx, profile, handle, limits)["data.csv"].Data; !bytes.Equal(got, files["data.csv"]) {
		t.Fatalf("fetched %q", got)
	}
	mismatch := devRun(t, profile, "mismatch")
	mismatch.Fetches = []pluginsdk.Fetch{{Name: "data.csv", URL: store.URL + "/data.csv", Size: int64(len(files["data.csv"])), SHA256: strings.Repeat("0", 64)}}
	var failed *fetchError
	if outcome, _, err := profile.Dispatch(ctx, mismatch); outcome != pluginsdk.OutcomeNothingCreated || !errors.As(err, &failed) {
		t.Fatalf("dispatch = %v, %v; want the fetch failed and nothing created", outcome, err)
	}
}

// stopRunning dispatches script, waits for its first line, stops it and returns its handle.
func stopRunning(t *testing.T, ctx context.Context, profile *Kubernetes, run pluginsdk.Run) pluginsdk.Handle {
	t.Helper()
	outcome, handle, err := profile.Dispatch(ctx, run)
	if err != nil || outcome != pluginsdk.OutcomeCreated {
		t.Fatalf("dispatch = %v, %v", outcome, err)
	}
	lines := make(chan pluginsdk.Line)
	followCtx, cancel := context.WithCancel(ctx)
	go func() { _, _ = profile.Follow(followCtx, handle, 0, lines) }()
	<-lines
	cancel()
	began := time.Now()
	if err := profile.Stop(ctx, handle); err != nil {
		t.Fatal(err)
	}
	t.Logf("stopped in %v", time.Since(began))
	return handle
}

func TestStopKeepsOutputsOnKubernetes(t *testing.T) {
	profile, ctx := devProfile(t, `echo report > "$WHERRY_OUTPUT_DIR/report.md"; echo started; sleep 600`, nil)
	handle := stopRunning(t, ctx, profile, devRun(t, profile, "stop"))
	if got := devOutputs(t, ctx, profile, handle, limits)["report.md"].Data; string(got) != "report\n" {
		t.Fatalf("collected %q after stop", got)
	}
}

func TestStopEscalatesOnKubernetes(t *testing.T) {
	profile, ctx := devProfile(t, `trap '' TERM; echo report > "$WHERRY_OUTPUT_DIR/report.md"; echo started; while :; do sleep 1; done`, nil)
	began := time.Now()
	handle := stopRunning(t, ctx, profile, devRun(t, profile, "escalate"))
	if elapsed := time.Since(began); elapsed > time.Minute {
		t.Fatalf("stop took %v", elapsed)
	}
	if got := devOutputs(t, ctx, profile, handle, limits)["report.md"].Data; string(got) != "report\n" {
		t.Fatalf("collected %q after SIGKILL", got)
	}
}

func TestCollectAfterAgentOOMOnKubernetes(t *testing.T) {
	profile, ctx := devProfile(t, `echo report > "$WHERRY_OUTPUT_DIR/report.md"; head -c 512m /dev/zero | tail`, func(c *Config) {
		c.Runtime.Resources.Memory = "64Mi"
	})
	handle, _, terminal := devTurn(t, ctx, profile, devRun(t, profile, "oom"))
	if !terminal.Started || terminal.ExitCode != 137 || terminal.Reason != "OOMKilled" {
		t.Fatalf("terminal %+v; want the agent OOMKilled with exit code 137", terminal)
	}
	if got := devOutputs(t, ctx, profile, handle, limits)["report.md"].Data; string(got) != "report\n" {
		t.Fatalf("collected %q after the agent's OOM", got)
	}
}

func TestAdoptionOnKubernetes(t *testing.T) {
	const script = `i=0; while [ $i -lt 60 ]; do echo "line $i"; i=$((i+1)); sleep 1; done; echo report > "$WHERRY_OUTPUT_DIR/report.md"; sleep 600`
	first, ctx := devProfile(t, script, nil)
	run := devRun(t, first, "adopt")
	run.Credentials = nil
	outcome, handle, err := first.Dispatch(ctx, run)
	if err != nil || outcome != pluginsdk.OutcomeCreated {
		t.Fatalf("dispatch = %v, %v", outcome, err)
	}
	lines := make(chan pluginsdk.Line)
	followCtx, cancel := context.WithCancel(ctx)
	go func() { _, _ = first.Follow(followCtx, handle, 0, lines) }()
	var last pluginsdk.Line
	for range 5 {
		last = <-lines
	}
	cancel()
	second, _ := devProfile(t, script, nil)
	job, err := second.Lookup(ctx, run.ID)
	if err != nil || job.State != pluginsdk.JobRunning || job.Profile != testProfile {
		t.Fatalf("lookup = %+v, %v", job, err)
	}
	resumed := make(chan pluginsdk.Line)
	followCtx, cancel = context.WithCancel(ctx)
	defer cancel()
	go func() { _, _ = second.Follow(followCtx, job.Handle, last.Next, resumed) }()
	if next := <-resumed; next.Offset != last.Next || string(next.Data) != "line 5" {
		t.Fatalf("resumed at %d with %q; want %d and line 5", next.Offset, next.Data, last.Next)
	}
	cancel()
	for {
		if collected, err := outputsOfDev(ctx, second, job.Handle); err == nil && string(collected["report.md"].Data) == "report\n" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("the report never appeared")
		case <-time.After(5 * time.Second):
		}
		if err := second.Stop(ctx, job.Handle); err != nil {
			t.Fatal(err)
		}
	}
}

func outputsOfDev(ctx context.Context, profile *Kubernetes, handle pluginsdk.Handle) (map[string]pluginsdk.Output, error) {
	outputs := make(chan pluginsdk.Output, 16)
	collectCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err := profile.Collect(collectCtx, handle, limits, outputs)
	close(outputs)
	byName := map[string]pluginsdk.Output{}
	for output := range outputs {
		byName[output.Name] = output
	}
	return byName, err
}

func TestLargeStreamedOutputOnKubernetes(t *testing.T) {
	profile, ctx := devProfile(t, `head -c 1536m /dev/urandom > "$WHERRY_OUTPUT_DIR/big.bin"; echo '{"type":"result","result":"done"}'`, nil)
	handle, _, _ := devTurn(t, ctx, profile, devRun(t, profile, "large"))
	got := devOutputs(t, ctx, profile, handle, pluginsdk.Limits{Files: 4, FileBytes: 1, TotalBytes: 1, Stream: []string{"big.bin"}, StreamBytes: 2 << 30})
	body := got["big.bin"].Body
	if body == nil {
		t.Fatal("big.bin was not streamed")
	}
	defer body.Close()
	began := time.Now()
	n, err := io.Copy(io.Discard, body)
	if err != nil || n != 1536<<20 {
		t.Fatalf("streamed %d bytes, %v", n, err)
	}
	t.Logf("streamed %d bytes in %v", n, time.Since(began))
}

func TestFollowPastLogRotationOnKubernetes(t *testing.T) {
	profile, ctx := devProfile(t, `yes "$(printf '%01023d' 0)" | head -n 61440; echo '{"type":"result","result":"done"}'`, nil)
	handle, lines, terminal := devTurn(t, ctx, profile, devRun(t, profile, "rotation"))
	if len(lines) != 61441 || string(lines[len(lines)-1].Data) != `{"type":"result","result":"done"}` || terminal.ExitCode != 0 {
		t.Fatalf("%d lines, %+v; want every line and the result", len(lines), terminal)
	}
	// 40 MiB lies in the rotated part and past the end of the current file.
	if _, _, err := followAll(t, profile, handle, 40<<20); err == nil || err.Error() != "the agent's log is shorter than the offset" {
		t.Fatalf("follow from a rotated offset: %v", err)
	}
}

func TestDeletingPodOnKubernetes(t *testing.T) {
	profile, ctx := devProfile(t, `trap '' TERM; echo started; while :; do sleep 1; done`, nil)
	run := devRun(t, profile, "deleting")
	run.Credentials = nil
	outcome, handle, err := profile.Dispatch(ctx, run)
	if err != nil || outcome != pluginsdk.OutcomeCreated {
		t.Fatalf("dispatch = %v, %v", outcome, err)
	}
	name := profile.jobName(run.ID)
	if err := profile.pods().Delete(ctx, name, metav1.DeleteOptions{GracePeriodSeconds: new(int64(600))}); err != nil {
		t.Fatal(err)
	}
	if job, err := profile.Lookup(ctx, run.ID); err != nil || job.State != pluginsdk.JobEnded {
		t.Fatalf("lookup = %+v, %v; want ended", job, err)
	}
	followCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if terminal, err := profile.Follow(followCtx, handle, 0, make(chan pluginsdk.Line, 64)); err != nil || terminal.ExitCode != -1 {
		t.Fatalf("follow = %+v, %v; want a terminal without logs", terminal, err)
	}
	if err := profile.Collect(ctx, handle, limits, make(chan pluginsdk.Output, 4)); err == nil || err.Error() != "the pod is being deleted" {
		t.Fatalf("collect = %v", err)
	}
	began := time.Now()
	if err := profile.Purge(ctx, run.ID); err != nil || time.Since(began) > 5*time.Second {
		t.Fatalf("purge = %v after %v", err, time.Since(began))
	}
}

func TestPodAdmittedUnderRestricted(t *testing.T) {
	profile, ctx := devProfile(t, `echo '{"type":"result","result":"done"}'`, nil)
	run := devRun(t, profile, "admitted")
	outcome, handle, err := profile.Dispatch(ctx, run)
	if err != nil || outcome != pluginsdk.OutcomeCreated {
		t.Fatalf("dispatch = %v, %v", outcome, err)
	}
	admitted, err := profile.pods().Get(ctx, handle.Name, getOptions)
	if err != nil {
		t.Fatal(err)
	}
	built := profile.buildPod(run, handle.Name)
	if fmt.Sprint(admitted.Spec.SecurityContext.SeccompProfile) != fmt.Sprint(built.Spec.SecurityContext.SeccompProfile) {
		t.Fatalf("seccomp %v; built %v", admitted.Spec.SecurityContext.SeccompProfile, built.Spec.SecurityContext.SeccompProfile)
	}
	for _, c := range built.Spec.Containers {
		got := container(admitted, c.Name).SecurityContext
		if fmt.Sprint(got.Capabilities.Drop) != fmt.Sprint(c.SecurityContext.Capabilities.Drop) || *got.AllowPrivilegeEscalation != *c.SecurityContext.AllowPrivilegeEscalation {
			t.Fatalf("container %s admitted with %+v; built %+v", c.Name, got, c.SecurityContext)
		}
	}
}
