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
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/benemon/wherry-plugins/kubernetes/helper"
	wherry "github.com/benemon/wherry/adapter"
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
	credentialEnv   = "WHERRY_TEST_CREDENTIAL"
	testPool        = "build"
	testNamespace   = "agents"
	testAdapter     = "container"
	testToken       = "kube-token-secret"
	testImage       = "registry.example.com/agent:1.0"
	testHelperImage = "registry.example.com/wherry-receiver:0.1.0"
	// poolPrefix is agent- and the first 12 hex digits of sha256("build").
	poolPrefix = "agent-44575cf5b285-"
	agentUID   = 1000690000
	// agentPID and holderPID are the processes a fake pod's helper sees beside its own.
	agentPID  = 100
	holderPID = 7
)

var getOptions = metav1.GetOptions{}

func testConfig(address, caFile string) Config {
	return Config{
		Address: address, CAFile: caFile, Namespace: testNamespace, CredentialEnv: credentialEnv, HelperImage: testHelperImage,
		Runtime: Runtime{
			Image: testImage, Command: []string{"/bin/sh", "-c"}, Args: []string{"exec agent-run < /wherry/prompt.txt"},
			Env: map[string]string{"WHERRY_TEST_ENV": "from-runtime"},
		},
	}
}

// build is newAdapter's path to one adapter named testAdapter, from a Config rather than settings.
func build(config Config, pool, token string) (*Adapter, error) {
	client, err := newClient(config, token)
	if err != nil {
		return nil, err
	}
	return New(config, testAdapter, pool, client, hclog.NewNullLogger())
}

func testRun(name string) wherry.Run {
	return wherry.Run{
		ID: "run-" + name, Adapter: testAdapter, Agent: "coder", Turn: 1, Attempt: 2,
		Prompt: []byte("hello-prompt"), Credential: []byte("test-credential"),
	}
}

// fast shortens the adapter's polls and grace for one test.
func fast(t *testing.T) {
	t.Helper()
	saved := []time.Duration{waitPoll, followPoll, collectPoll, stopGrace}
	waitPoll, followPoll, collectPoll, stopGrace = 10*time.Millisecond, 10*time.Millisecond, 10*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { waitPoll, followPoll, collectPoll, stopGrace = saved[0], saved[1], saved[2], saved[3] })
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

// adapter builds the adapter against the fake, with a fake execer.
func (api *fakeAPI) adapter(t *testing.T) (*Adapter, *fakeExec) {
	t.Helper()
	adapter, err := build(testConfig(api.server.URL, api.caFile), testPool, testToken)
	if err != nil {
		t.Fatal(err)
	}
	execer := newFakeExec(t)
	adapter.exec = execer
	return adapter, execer
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

// assignUser admits a pod as restricted-v2 does, giving every container the namespace's first UID.
func assignUser(pod *corev1.Pod) {
	for _, containers := range [][]corev1.Container{pod.Spec.InitContainers, pod.Spec.Containers} {
		for i := range containers {
			if containers[i].SecurityContext == nil {
				containers[i].SecurityContext = &corev1.SecurityContext{}
			}
			if containers[i].SecurityContext.RunAsUser == nil {
				containers[i].SecurityContext.RunAsUser = new(int64(agentUID))
			}
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
func setStatus(pod *corev1.Pod, phase corev1.PodPhase, inputs corev1.ContainerStatus, containers ...corev1.ContainerStatus) {
	pod.Spec.NodeName = "worker-1"
	pod.Status.Phase = phase
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{inputs}
	pod.Status.ContainerStatuses = containers
}

// startAgent is a kubelet that starts a new pod's agent at once.
func startAgent(pod *corev1.Pod) {
	if pod.Status.Phase == "" {
		setStatus(pod, corev1.PodRunning, exited("inputs", 0), runningStatus("agent"), runningStatus("outputs"))
	}
}

// agentPod is a pod as buildPod makes it for testRun(name), created and with status set by edit.
func (api *fakeAPI) agentPod(t *testing.T, adapter *Adapter, name string, edit func(*corev1.Pod)) *corev1.Pod {
	t.Helper()
	run := testRun(name)
	pod := adapter.buildPod(run, adapter.jobName(run.ID, run.Turn))
	assignUser(pod)
	if edit != nil {
		edit(pod)
	}
	return api.put(pod)
}

func handleOf(pod *corev1.Pod) wherry.Handle {
	data, _ := json.Marshal(handle{V: 1, Pod: pod.Name, UID: string(pod.UID)})
	return wherry.Handle{Data: data, Name: pod.Name}
}

// fakeExec runs the helper in-process against a temporary root per pod.
type fakeExec struct {
	t     *testing.T
	mu    sync.Mutex
	roots map[string]string
	calls []string
	// fail fails an invocation in transport when it returns an error.
	fail func(container string, args []string) error
	// output answers an invocation in place of the helper when it returns true.
	output func(args []string, stdout io.Writer) bool
	// signal is the pod's signal call; it records every signal.
	signal  func(pid int, sig syscall.Signal) error
	signals []string
	// stdin counts the bytes each invocation could read from its stdin.
	stdin []int64
}

func newFakeExec(t *testing.T) *fakeExec {
	return &fakeExec{t: t, roots: map[string]string{}}
}

// root is the pod's filesystem, laid out as its volumes are, with its process list and holder pid.
func (f *fakeExec) root(pod string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if root, ok := f.roots[pod]; ok {
		return root
	}
	root := f.t.TempDir()
	for _, dir := range []string{"run/wherry/material", "wherry", "run/wherry/credentials", "run/wherry/hold", "proc/1", "proc/" + strconv.Itoa(agentPID), "proc/" + strconv.Itoa(holderPID)} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			f.t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "run/wherry/hold/pid"), []byte(strconv.Itoa(holderPID)), 0o600); err != nil {
		f.t.Fatal(err)
	}
	f.roots[pod] = root
	return root
}

func (f *fakeExec) exec(ctx context.Context, pod, container string, command []string, stdin io.Reader, stdout io.Writer) error {
	if len(command) < 2 || command[0] != helperPath {
		f.t.Errorf("exec of %v; want the helper", command)
	}
	args := command[2:]
	root := f.root(pod)
	f.mu.Lock()
	f.calls = append(f.calls, container+" "+strings.Join(args, " "))
	fail, output := f.fail, f.output
	f.mu.Unlock()
	if fail != nil {
		if err := fail(container, args); err != nil {
			return err
		}
	}
	if output != nil && output(args, stdout) {
		return nil
	}
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	counted := &counter{reader: stdin}
	var stderr bytes.Buffer
	code := helper.Run(helper.Env{Root: root, Stdin: counted, Stdout: stdout, Stderr: &stderr, Signal: f.send}, command[1:])
	_, _ = io.Copy(io.Discard, counted)
	f.mu.Lock()
	f.stdin = append(f.stdin, counted.n)
	f.mu.Unlock()
	if code != 0 {
		return &exitError{code: code, message: stderr.String()}
	}
	return nil
}

func (f *fakeExec) send(pid int, sig syscall.Signal) error {
	f.mu.Lock()
	f.signals = append(f.signals, fmt.Sprintf("%d %s", pid, sig))
	signal := f.signal
	f.mu.Unlock()
	if signal != nil {
		return signal(pid, sig)
	}
	return nil
}

func (f *fakeExec) set(edit func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	edit()
}

func (f *fakeExec) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

type counter struct {
	reader io.Reader
	n      int64
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.reader.Read(p)
	c.n += int64(n)
	return n, err
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

func podJSON(t *testing.T, pod *corev1.Pod) string {
	t.Helper()
	encoded, err := json.Marshal(pod)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func container(pod *corev1.Pod, name string) *corev1.Container {
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == name {
			return &pod.Spec.InitContainers[i]
		}
	}
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

func plainAdapter(t *testing.T, edit func(*Config)) *Adapter {
	t.Helper()
	config := testConfig("https://127.0.0.1:1", "")
	if edit != nil {
		edit(&config)
	}
	adapter, err := build(config, testPool, testToken)
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func TestBuildPodFromRuntime(t *testing.T) {
	adapter := plainAdapter(t, nil)
	run := testRun("build")
	name := adapter.jobName(run.ID, run.Turn)
	pod := adapter.buildPod(run, name)
	wantLabels := map[string]string{labelPool: testPool, labelAdapter: testAdapter, labelAgent: "coder", labelRun: run.ID, labelTurn: "1"}
	if pod.Name != poolPrefix+"run-build-t1" || pod.Namespace != testNamespace || !maps.Equal(pod.Labels, wantLabels) ||
		pod.Annotations[annotationAttempt] != "2" || pod.Spec.RestartPolicy != corev1.RestartPolicyNever || *pod.Spec.TerminationGracePeriodSeconds != 15 {
		t.Fatalf("unexpected pod identity: %#v", pod.ObjectMeta)
	}
	agent, inputs, outputs := container(pod, "agent"), container(pod, "inputs"), container(pod, "outputs")
	if agent.Image != testImage || !slices.Equal(agent.Command, []string{"/bin/sh", "-c"}) || !slices.Equal(agent.Args, []string{"exec agent-run < /wherry/prompt.txt"}) {
		t.Fatalf("runtime not carried: %#v", agent)
	}
	if inputs.Image != testHelperImage || !slices.Equal(inputs.Command, []string{helperPath, "v1", "inputs"}) || inputs.Args != nil ||
		outputs.Image != testHelperImage || !slices.Equal(outputs.Command, []string{helperPath, "v1", "hold"}) {
		t.Fatalf("helper containers %#v, %#v", inputs, outputs)
	}
	wantEnv := maps.Clone(wherry.AgentEnvironment)
	maps.Copy(wantEnv, map[string]string{"WHERRY_TEST_ENV": "from-runtime", "HOME": "/wherry",
		"WHERRY_INPUT_DIR": "/wherry/inputs", "WHERRY_OUTPUT_DIR": "/wherry/outputs", "WHERRY_CREDENTIALS_DIR": "/run/wherry/credentials/files", credentialEnv: ""})
	if env := envOf(agent); !maps.Equal(env, wantEnv) {
		t.Fatalf("agent env %v; want %v", env, wantEnv)
	}
	last := agent.Env[len(agent.Env)-1]
	if last.Name != credentialEnv || last.ValueFrom.SecretKeyRef.Name != name || last.ValueFrom.SecretKeyRef.Key != "credential" {
		t.Fatalf("credential variable %#v; want a reference to the Secret's credential key", last)
	}
	if material := volume(pod, "material"); material.Secret.SecretName != name || *material.Secret.DefaultMode != 0o444 {
		t.Fatalf("material volume %#v", material)
	}
	run.Fetches = []wherry.Fetch{{Name: "data.csv", URL: "https://storage.example.com/data.csv?X-Amz-Signature=s", Size: 12, SHA256: strings.Repeat("a", 64)}}
	if args := container(adapter.buildPod(run, name), "inputs").Args; !slices.Equal(args, []string{"--await", "data.csv", "12", strings.Repeat("a", 64)}) {
		t.Fatalf("inputs args %v; want --await and the fetch's name, size and digest", args)
	}
	run.Credential = nil
	if env := container(adapter.buildPod(run, name), "agent").Env; slices.ContainsFunc(env, func(v corev1.EnvVar) bool { return v.Name == credentialEnv }) {
		t.Fatal("credential variable set for a run without a credential")
	}
}

func TestBuildPodDefaultsAgentResources(t *testing.T) {
	pod := plainAdapter(t, nil).buildPod(testRun("defaults"), "agent-x")
	want := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("2Gi")}
	agent := container(pod, "agent")
	for _, list := range []corev1.ResourceList{agent.Resources.Requests, agent.Resources.Limits} {
		if len(list) != len(want) || !list.Cpu().Equal(want[corev1.ResourceCPU]) || !list.Memory().Equal(want[corev1.ResourceMemory]) {
			t.Fatalf("agent resources %v; want requests and limits of 500m and 2Gi", agent.Resources)
		}
	}
	pod = plainAdapter(t, func(c *Config) { c.Runtime.Resources = Resources{CPU: "2", Memory: "4Gi"} }).buildPod(testRun("set"), "agent-x")
	if agent := container(pod, "agent"); agent.Resources.Limits.Cpu().String() != "2" || agent.Resources.Requests.Memory().String() != "4Gi" {
		t.Fatalf("configured resources %v", agent.Resources)
	}
}

func TestBuildPodSetsStorageLimitsOnlyWhenConfigured(t *testing.T) {
	pod := plainAdapter(t, nil).buildPod(testRun("unset"), "agent-x")
	agent := container(pod, "agent")
	if _, set := agent.Resources.Requests[corev1.ResourceEphemeralStorage]; set || volume(pod, "work").EmptyDir.SizeLimit != nil {
		t.Fatalf("storage limits set unconfigured: %v, %v", agent.Resources, volume(pod, "work").EmptyDir)
	}
	pod = plainAdapter(t, func(c *Config) {
		c.Runtime.WorkLimit, c.Runtime.Resources.EphemeralStorage = "20Gi", "30Gi"
	}).buildPod(testRun("set"), "agent-x")
	agent = container(pod, "agent")
	if agent.Resources.Requests.StorageEphemeral().String() != "30Gi" || agent.Resources.Limits.StorageEphemeral().String() != "30Gi" ||
		volume(pod, "work").EmptyDir.SizeLimit.String() != "20Gi" {
		t.Fatalf("storage limits not set: %v, %v", agent.Resources, volume(pod, "work").EmptyDir)
	}
}

func TestPodSpecGivesEveryContainerResources(t *testing.T) {
	pod := plainAdapter(t, nil).buildPod(testRun("resources"), "agent-x")
	for _, c := range slices.Concat(pod.Spec.InitContainers, pod.Spec.Containers) {
		for _, list := range []corev1.ResourceList{c.Resources.Requests, c.Resources.Limits} {
			if list.Cpu().IsZero() || list.Memory().IsZero() {
				t.Fatalf("container %s resources %v; want cpu and memory requests and limits", c.Name, c.Resources)
			}
		}
	}
}

func TestPodSpecSecurity(t *testing.T) {
	pod := plainAdapter(t, nil).buildPod(testRun("security"), "agent-x")
	spec := pod.Spec
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken || spec.EnableServiceLinks == nil || *spec.EnableServiceLinks ||
		spec.ShareProcessNamespace == nil || !*spec.ShareProcessNamespace || spec.SecurityContext.RunAsNonRoot == nil || !*spec.SecurityContext.RunAsNonRoot ||
		spec.SecurityContext.SeccompProfile == nil || spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("pod security %#v", spec)
	}
	for _, c := range slices.Concat(spec.InitContainers, spec.Containers) {
		s := c.SecurityContext
		if s == nil || s.AllowPrivilegeEscalation == nil || *s.AllowPrivilegeEscalation || s.Capabilities == nil || !slices.Equal(s.Capabilities.Drop, []corev1.Capability{"ALL"}) {
			t.Fatalf("container %s security %#v", c.Name, s)
		}
		if helperContainer := c.Name != "agent"; helperContainer && (s.ReadOnlyRootFilesystem == nil || !*s.ReadOnlyRootFilesystem) {
			t.Fatalf("helper container %s has a writable root", c.Name)
		}
	}
}

func TestPodSpecHoldsNoMaterial(t *testing.T) {
	adapter := plainAdapter(t, nil)
	run := testRun("material")
	run.Model = "sonnet"
	run.Inputs = []wherry.File{{Name: "notes.md", Data: []byte("input-bytes-secret")}}
	run.Credentials = []wherry.Secret{{Name: "deploy", File: "id_ed25519", Value: []byte("task-credential-secret")}}
	run.Fetches = []wherry.Fetch{{Name: "data.csv", URL: "https://storage.example.com/data.csv?X-Amz-Signature=signature-secret", Size: 3, SHA256: strings.Repeat("b", 64)}}
	pod := adapter.buildPod(run, adapter.jobName(run.ID, run.Turn))
	encoded := podJSON(t, pod)
	for _, secret := range []string{"hello-prompt", "test-credential", "input-bytes-secret", "task-credential-secret", "signature-secret", "storage.example.com"} {
		if strings.Contains(encoded, secret) {
			t.Fatalf("pod spec holds %q: %s", secret, encoded)
		}
	}
	for _, c := range slices.Concat(pod.Spec.InitContainers, pod.Spec.Containers) {
		if slices.Contains(mounts(&c), "material") != (c.Name == "inputs") {
			t.Fatalf("container %s mounts %v; only inputs mounts the material", c.Name, mounts(&c))
		}
		for _, variable := range c.Env {
			if variable.Name == credentialEnv && (variable.Value != "" || variable.ValueFrom == nil || variable.ValueFrom.SecretKeyRef == nil) {
				t.Fatalf("credential variable %#v; want a Secret reference", variable)
			}
		}
	}
}

func TestHolderMountsNoCredentials(t *testing.T) {
	pod := plainAdapter(t, nil).buildPod(testRun("holder"), "agent-x")
	if got := mounts(container(pod, "outputs")); !slices.Equal(got, []string{"work", "hold"}) {
		t.Fatalf("outputs mounts %v; want work and hold alone", got)
	}
	for _, mount := range container(pod, "outputs").VolumeMounts {
		if mount.Name == "work" && !mount.ReadOnly {
			t.Fatal("outputs mounts work writable")
		}
	}
	if credentials := volume(pod, "credentials").EmptyDir; credentials == nil || credentials.Medium != corev1.StorageMediumMemory {
		t.Fatalf("credentials volume %#v; want a memory emptyDir", credentials)
	}
}

func TestAgentEnvironmentReachesTheAgent(t *testing.T) {
	adapter := plainAdapter(t, func(c *Config) {
		c.Runtime.Env = map[string]string{"GIT_SSH_COMMAND": "ssh -o StrictHostKeyChecking=no", "GIT_CONFIG_GLOBAL": "/home/agent/.gitconfig", "LANG": "C.UTF-8", "HOME": "/root"}
	})
	env := envOf(container(adapter.buildPod(testRun("env"), "agent-x"), "agent"))
	for name, value := range wherry.AgentEnvironment {
		if env[name] != value {
			t.Fatalf("%s=%q; want %q", name, env[name], value)
		}
	}
	for _, name := range wherry.AgentUnset {
		if _, set := env[name]; set {
			t.Fatalf("%s reached the agent", name)
		}
	}
	if env["LANG"] != "C.UTF-8" || env["HOME"] != "/wherry" {
		t.Fatalf("LANG=%q HOME=%q; want the runtime's LANG and the plugin's HOME", env["LANG"], env["HOME"])
	}
}

func TestPodEnvCarriesTheModelOnlyWhenSet(t *testing.T) {
	adapter := plainAdapter(t, nil)
	for model, want := range map[string]bool{"": false, "sonnet": true} {
		run := testRun("model")
		run.Model = model
		pod := adapter.buildPod(run, "agent-x")
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
	spec := plainAdapter(t, nil).buildPod(testRun("defaults"), "agent-x").Spec
	if spec.NodeSelector != nil || spec.Tolerations != nil || spec.SecurityContext.RunAsUser != nil || spec.SecurityContext.RunAsGroup != nil {
		t.Fatalf("unset placement or user set: %#v", spec)
	}
	spec = plainAdapter(t, func(c *Config) {
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
	want := wherry.Directories{Input: "/wherry/inputs", Output: "/wherry/outputs", Credentials: "/run/wherry/credentials/files"}
	if got := plainAdapter(t, nil).Directories(); got != want {
		t.Fatalf("directories %+v; want %+v", got, want)
	}
	env := envOf(container(plainAdapter(t, nil).buildPod(testRun("dirs"), "agent-x"), "agent"))
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
	more, _ := filepath.Glob("helper/*.go")
	word := regexp.MustCompile(`(?i)\bgit\b`)
	for _, source := range append(sources, append(more, "cmd/wherry-k8s-helper/main.go")...) {
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
		"credential_env not a name": func(c *Config) { c.CredentialEnv = "MODEL-KEY" },
		"no helper image":           func(c *Config) { c.HelperImage = "" },
		"no image":                  func(c *Config) { c.Runtime.Image = "" },
		"image with whitespace":     func(c *Config) { c.Runtime.Image = "agent :1" },
		"env name":                  func(c *Config) { c.Runtime.Env = map[string]string{"A-B": "1"} },
		"cpu not a quantity":        func(c *Config) { c.Runtime.Resources.CPU = "fast" },
		"memory not a quantity":     func(c *Config) { c.Runtime.Resources.Memory = "2 GiB" },
		"work_limit not a quantity": func(c *Config) { c.Runtime.WorkLimit = "lots" },
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
	if _, err := build(config, testPool, testToken); err != nil {
		t.Fatalf("UID alone refused: %v", err)
	}
}

func TestPerTurnPodNames(t *testing.T) {
	adapter := plainAdapter(t, nil)
	if first, second := adapter.jobName("run-a", 1), adapter.jobName("run-a", 2); first != poolPrefix+"run-a-t1" || second != poolPrefix+"run-a-t2" {
		t.Fatalf("names %q, %q", first, second)
	}
	if long := adapter.jobName(strings.Repeat("r", 63), 1<<62); len(long) > 103 {
		t.Fatalf("name of %d characters; want at most 103", len(long))
	}
}

func dispatch(t *testing.T, adapter *Adapter, run wherry.Run) wherry.Handle {
	t.Helper()
	outcome, handle, err := adapter.Dispatch(context.Background(), run)
	if err != nil || outcome != wherry.OutcomeCreated {
		t.Fatalf("dispatch = %v, %v", outcome, err)
	}
	return handle
}

func TestSameRunInTwoPoolsCreatesTwoPods(t *testing.T) {
	api := newFakeAPI(t)
	build1, _ := api.adapter(t)
	review, err := build(testConfig(api.server.URL, api.caFile), "review", testToken)
	if err != nil {
		t.Fatal(err)
	}
	review.exec = newFakeExec(t)
	run := testRun("shared")
	first, second := dispatch(t, build1, run), dispatch(t, review, run)
	if first.Name == second.Name || api.pod(first.Name) == nil || api.pod(second.Name) == nil {
		t.Fatalf("pods %q and %q; want one per pool", first.Name, second.Name)
	}
}

func TestDispatchCreatesSecretThenPod(t *testing.T) {
	api := newFakeAPI(t)
	adapter, _ := api.adapter(t)
	run := testRun("create")
	run.Inputs = []wherry.File{{Name: "notes.md", Data: []byte("notes")}}
	run.Credentials = []wherry.Secret{{Name: "deploy", File: "id_ed25519", Value: []byte("key")}}
	handle := dispatch(t, adapter, run)
	name := poolPrefix + "run-create-t1"
	base := "/api/v1/namespaces/agents/"
	want := []string{"GET " + base + "pods/" + name, "GET " + base + "secrets/" + name, "POST " + base + "secrets", "POST " + base + "pods", "GET " + base + "pods/" + name}
	if got := paths(api.recorded(), ""); !slices.Equal(got, want) {
		t.Fatalf("requests %v; want %v", got, want)
	}
	secret := api.secret(name)
	wantData := map[string]string{"prompt": "hello-prompt", "credential": "test-credential", "inputs.notes.md": "notes", "credentials.id_ed25519": "key"}
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

func TestDispatchConvergesOnExistingPod(t *testing.T) {
	api := newFakeAPI(t)
	adapter, _ := api.adapter(t)
	existing := api.agentPod(t, adapter, "existing", nil)
	if handle := dispatch(t, adapter, testRun("existing")); handle.Name != existing.Name {
		t.Fatalf("handle %q; want the existing pod", handle.Name)
	}
	if created := paths(api.recorded(), http.MethodPost); created != nil {
		t.Fatalf("created %v beside an existing pod", created)
	}
	// A pod another dispatch creates between this one's read and create.
	raced := api.agentPod(t, adapter, "raced", nil)
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
	outcome, handle, err := adapter.Dispatch(context.Background(), testRun("raced"))
	if err != nil || outcome != wherry.OutcomeCreated || handle.Name != raced.Name {
		t.Fatalf("dispatch racing a create = %v, %q, %v; want the existing pod", outcome, handle.Name, err)
	}
	if placement := samples(t, adapter)["wherry_adapter_placement_seconds/"].Histogram; placement.Count != 0 {
		t.Fatalf("placement counted %d for pods this adapter did not create", placement.Count)
	}
}

func TestDispatchLeavesTheSecretOfAnExistingPod(t *testing.T) {
	api := newFakeAPI(t)
	adapter, _ := api.adapter(t)
	api.agentPod(t, adapter, "existing", nil)
	dispatch(t, adapter, testRun("existing"))
	for _, r := range api.recorded() {
		if strings.Contains(r.path, "/secrets") {
			t.Fatalf("dispatch onto an existing pod requested %s", r)
		}
	}
}

func TestDispatchOverwritesStaleSecret(t *testing.T) {
	api := newFakeAPI(t)
	adapter, _ := api.adapter(t)
	name := poolPrefix + "run-stale-t1"
	api.set(func() {
		api.secrets[name] = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, ResourceVersion: "41"}, Data: map[string][]byte{"prompt": []byte("stale"), "inputs.old": []byte("old")}}
	})
	dispatch(t, adapter, testRun("stale"))
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
	name := poolPrefix + "run-outcome-t1"
	for _, test := range []struct {
		test      string
		intercept func(*fakeAPI) func(http.ResponseWriter, *http.Request) bool
		outcome   wherry.Outcome
		deleted   bool
	}{
		{"secret write fails and no pod exists", func(*fakeAPI) func(http.ResponseWriter, *http.Request) bool {
			return failing(http.StatusInternalServerError, http.MethodPost, "/secrets")
		}, wherry.OutcomeNothingCreated, true},
		{"pod create refused and no pod exists", func(*fakeAPI) func(http.ResponseWriter, *http.Request) bool {
			return failing(http.StatusForbidden, http.MethodPost, "/pods")
		}, wherry.OutcomeNothingCreated, true},
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
		}, wherry.OutcomeUnknown, false},
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
		}, wherry.OutcomeUnknown, false},
		{"the first read fails", func(*fakeAPI) func(http.ResponseWriter, *http.Request) bool {
			return failing(http.StatusInternalServerError, http.MethodGet, "/pods/"+name)
		}, wherry.OutcomeUnknown, false},
	} {
		t.Run(test.test, func(t *testing.T) {
			api := newFakeAPI(t)
			adapter, _ := api.adapter(t)
			api.set(func() { api.intercept = test.intercept(api) })
			outcome, _, err := adapter.Dispatch(context.Background(), testRun("outcome"))
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
		adapter, _ := api.adapter(t)
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
		if outcome, _, err := adapter.Dispatch(context.Background(), testRun("outcome")); outcome != wherry.OutcomeUnknown || err == nil {
			t.Fatalf("outcome %v, %v; want unknown", outcome, err)
		}
	})
}

func TestDispatchRefusesInputsOverTheSecretLimit(t *testing.T) {
	api := newFakeAPI(t)
	adapter, _ := api.adapter(t)
	run := testRun("limit")
	run.Credential = []byte("cred")
	run.Credentials = []wherry.Secret{{Name: "deploy", File: "key", Value: []byte("key")}}
	run.Inputs = []wherry.File{{Name: "first.bin", Data: []byte("abc")}, {Name: "fill.bin", Data: bytes.Repeat([]byte{0xff}, maxSecret-len(run.Prompt)-4-3-3)}}
	dispatch(t, adapter, run)
	run.ID = "run-over"
	run.Inputs[1].Data = append(run.Inputs[1].Data, 0)
	before := len(api.recorded())
	outcome, _, err := adapter.Dispatch(context.Background(), run)
	var tooLarge *wherry.InputTooLargeError
	if outcome != wherry.OutcomeNothingCreated || !errors.As(err, &tooLarge) || tooLarge.Name != "fill.bin" {
		t.Fatalf("dispatch one byte over = %v, %v; want fill.bin too large and nothing created", outcome, err)
	}
	if after := len(api.recorded()); after != before {
		t.Fatalf("an oversized turn made %d requests", after-before)
	}
}

func TestDispatchRefusesANameOverTheKeyLimit(t *testing.T) {
	api := newFakeAPI(t)
	adapter, _ := api.adapter(t)
	for i, test := range []struct {
		input, file string
		refused     string
	}{
		{input: strings.Repeat("i", 246)},
		{input: strings.Repeat("i", 247), refused: strings.Repeat("i", 247)},
		{file: strings.Repeat("f", 241)},
		{file: strings.Repeat("f", 242), refused: strings.Repeat("f", 242)},
	} {
		run := testRun("key-" + strconv.Itoa(i))
		if test.input != "" {
			run.Inputs = []wherry.File{{Name: test.input, Data: []byte("x")}}
		} else {
			run.Credentials = []wherry.Secret{{Name: "c", File: test.file, Value: []byte("x")}}
		}
		before := len(api.recorded())
		outcome, _, err := adapter.Dispatch(context.Background(), run)
		var tooLarge *wherry.InputTooLargeError
		switch {
		case test.refused == "" && (err != nil || outcome != wherry.OutcomeCreated):
			t.Fatalf("name of %d characters: %v, %v; want created", len(test.input+test.file), outcome, err)
		case test.refused != "" && (outcome != wherry.OutcomeNothingCreated || !errors.As(err, &tooLarge) || tooLarge.Name != test.refused):
			t.Fatalf("name of %d characters: %v, %v; want it too large", len(test.refused), outcome, err)
		case test.refused != "" && len(api.recorded()) != before:
			t.Fatalf("a refused name made %d requests", len(api.recorded())-before)
		}
	}
}

func TestDispatchRefusesAnOversizedPrompt(t *testing.T) {
	api := newFakeAPI(t)
	adapter, _ := api.adapter(t)
	run := testRun("prompt")
	run.Prompt = bytes.Repeat([]byte("p"), maxSecret)
	outcome, _, err := adapter.Dispatch(context.Background(), run)
	var tooLarge *wherry.InputTooLargeError
	if outcome != wherry.OutcomeNothingCreated || err == nil || err.Error() != "the prompt and credentials exceed the 1 MiB Secret limit" || errors.As(err, &tooLarge) {
		t.Fatalf("dispatch = %v, %v; want the prompt refused, naming no input", outcome, err)
	}
	if requests := api.recorded(); len(requests) != 0 {
		t.Fatalf("an oversized prompt made requests %v", requests)
	}
}

func TestDispatchRefusesPodsOfMixedUsers(t *testing.T) {
	for name, admit := range map[string]func(*corev1.Pod){
		"different": func(pod *corev1.Pod) {
			assignUser(pod)
			pod.Spec.Containers[1].SecurityContext.RunAsUser = new(int64(1001))
		},
		"unset": func(pod *corev1.Pod) {},
	} {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			adapter, _ := api.adapter(t)
			api.set(func() { api.admit = admit })
			outcome, _, err := adapter.Dispatch(context.Background(), testRun("mixed"))
			if outcome != wherry.OutcomeNothingCreated || err == nil || err.Error() != "agent pod containers run as different users; set runtime.user" {
				t.Fatalf("dispatch = %v, %v; want mixed users refused", outcome, err)
			}
			if api.pod(poolPrefix+"run-mixed-t1") != nil || api.secret(poolPrefix+"run-mixed-t1") != nil {
				t.Fatal("the refused pod or its Secret remains")
			}
		})
	}
}

func pendingKubelet(pod *corev1.Pod) {
	setStatus(pod, corev1.PodPending, runningStatus("inputs"), waiting("agent", "PodInitializing", ""), waiting("outputs", "PodInitializing", ""))
}

func TestDispatchTimeoutDeletesThePod(t *testing.T) {
	fast(t)
	api := newFakeAPI(t)
	adapter, _ := api.adapter(t)
	api.set(func() { api.kubelet = pendingKubelet })
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if outcome, _, err := adapter.Dispatch(ctx, testRun("timeout")); outcome != wherry.OutcomeUnknown || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("dispatch = %v, %v; want the deadline", outcome, err)
	}
	name := poolPrefix + "run-timeout-t1"
	if api.pod(name) != nil {
		t.Fatal("the timed-out pod remains")
	}
	if api.secret(name) == nil {
		t.Fatal("the timed-out turn's Secret was deleted; Release or the sweep removes it")
	}
}

func TestDispatchReportsPlacement(t *testing.T) {
	api := newFakeAPI(t)
	adapter, _ := api.adapter(t)
	dispatch(t, adapter, testRun("placed"))
	placement := samples(t, adapter)["wherry_adapter_placement_seconds/"].Histogram
	if placement.Count != 1 || len(placement.Buckets) != len(wherry.PlacementBuckets) || placement.Buckets[len(placement.Buckets)-1] != 1 {
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

// initContainer is the helper's inputs container run in-process in a fake pod's root.
type initContainer struct {
	mu      sync.Mutex
	ended   bool
	code    int
	message string
}

// startInit runs the inputs container of the pod for fetches and makes the fake's kubelet report it; the agent starts
// once it exits 0.
func startInit(t *testing.T, api *fakeAPI, execer *fakeExec, pod string, fetches []wherry.Fetch, before func(reads int) bool) *initContainer {
	t.Helper()
	root := execer.root(pod)
	if err := os.WriteFile(filepath.Join(root, "run/wherry/material/prompt"), []byte("hello-prompt"), 0o444); err != nil {
		t.Fatal(err)
	}
	args := []string{helper.Protocol, "inputs", "--await"}
	for _, fetch := range fetches {
		args = append(args, fetch.Name, strconv.FormatInt(fetch.Size, 10), fetch.SHA256)
	}
	init := &initContainer{}
	go func() {
		var stderr bytes.Buffer
		code := helper.Run(helper.Env{Root: root, Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: &stderr}, args)
		init.mu.Lock()
		init.ended, init.code, init.message = true, code, stderr.String()
		init.mu.Unlock()
	}()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(root, "wherry/.inputs/lock")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the init container never waited")
		}
	}
	reads := 0
	api.set(func() {
		api.kubelet = func(p *corev1.Pod) {
			reads++
			init.mu.Lock()
			defer init.mu.Unlock()
			switch {
			case before != nil && before(reads):
				setStatus(p, corev1.PodPending, waiting("inputs", "PodInitializing", ""), waiting("agent", "PodInitializing", ""), waiting("outputs", "PodInitializing", ""))
			case !init.ended:
				pendingKubelet(p)
			case init.code == 0:
				setStatus(p, corev1.PodRunning, exited("inputs", 0), runningStatus("agent"), runningStatus("outputs"))
			default:
				ended := exited("inputs", int32(init.code))
				ended.State.Terminated.Message = init.message
				setStatus(p, corev1.PodFailed, ended, waiting("agent", "PodInitializing", ""), waiting("outputs", "PodInitializing", ""))
			}
		}
	})
	return init
}

func fetchRun(name string, objects *objects, files map[string][]byte) wherry.Run {
	run := testRun(name)
	for _, file := range slices.Sorted(maps.Keys(files)) {
		run.Fetches = append(run.Fetches, wherry.Fetch{Name: file, URL: objects.server.URL + "/" + file + "?X-Amz-Signature=signature-secret", Size: int64(len(files[file])), SHA256: sha(files[file])})
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

func TestDispatchDeliversFetches(t *testing.T) {
	fast(t)
	api := newFakeAPI(t)
	adapter, execer := api.adapter(t)
	files := map[string][]byte{"data.csv": []byte("a,b\n1,2\n"), "model.bin": {0, 1, 2, 255}}
	store := newObjects(t, serveFiles(files))
	run := fetchRun("fetch", store, files)
	name := adapter.jobName(run.ID, run.Turn)
	startInit(t, api, execer, name, run.Fetches, nil)
	dispatch(t, adapter, run)
	for file, data := range files {
		if got, err := os.ReadFile(filepath.Join(execer.root(name), "wherry/inputs", file)); err != nil || !bytes.Equal(got, data) {
			t.Fatalf("delivered %s = %q, %v", file, got, err)
		}
	}
	if calls := execer.recorded(); !slices.Equal(calls, []string{"inputs inputs put data.csv", "inputs inputs put model.bin", "inputs inputs done"}) {
		t.Fatalf("execs %v; want a put of each fetch, then done", calls)
	}
}

func TestDispatchWaitsForTheInitContainer(t *testing.T) {
	fast(t)
	api := newFakeAPI(t)
	adapter, execer := api.adapter(t)
	files := map[string][]byte{"data.csv": []byte("data")}
	run := fetchRun("init", newObjects(t, serveFiles(files)), files)
	var mu sync.Mutex
	initRunning := false
	startInit(t, api, execer, adapter.jobName(run.ID, run.Turn), run.Fetches, func(reads int) bool {
		mu.Lock()
		defer mu.Unlock()
		initRunning = reads > 3
		return !initRunning
	})
	execer.set(func() {
		execer.fail = func(string, []string) error {
			mu.Lock()
			defer mu.Unlock()
			if !initRunning {
				t.Error("exec before the init container ran")
			}
			return nil
		}
	})
	dispatch(t, adapter, run)
}

// transportFailure is how an exec that failed in transport reads.
var transportFailure = errors.New("error dialing backend: connection reset by peer")

func TestDispatchRetriesAFailedPut(t *testing.T) {
	fast(t)
	files := map[string][]byte{"data.csv": []byte("data")}
	for _, test := range []struct {
		name     string
		failures int
		failFail bool
		state    wherry.JobState
		gets     int
	}{
		{"one transport failure", 1, false, wherry.JobRunning, 2},
		{"three transport failures", 3, false, wherry.JobNeverStarted, 3},
		{"a failed fail", 3, true, 0, 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			api := newFakeAPI(t)
			adapter, execer := api.adapter(t)
			store := newObjects(t, serveFiles(files))
			run := fetchRun("retry", store, files)
			startInit(t, api, execer, adapter.jobName(run.ID, run.Turn), run.Fetches, nil)
			puts := 0
			execer.set(func() {
				execer.fail = func(_ string, args []string) error {
					if args[1] == "put" {
						if puts++; puts <= test.failures {
							return transportFailure
						}
					}
					if args[1] == "fail" && test.failFail {
						return transportFailure
					}
					return nil
				}
			})
			outcome, handle, err := adapter.Dispatch(context.Background(), run)
			if store.count("/data.csv") != test.gets {
				t.Fatalf("fetched %d times; want %d", store.count("/data.csv"), test.gets)
			}
			if test.failFail {
				if outcome != wherry.OutcomeUnknown || err == nil {
					t.Fatalf("dispatch with a failed fail = %v, %v; want its error", outcome, err)
				}
				return
			}
			if err != nil || outcome != wherry.OutcomeCreated {
				t.Fatalf("dispatch = %v, %v", outcome, err)
			}
			job, err := adapter.Lookup(context.Background(), run.ID, run.Turn)
			if err != nil || job.State != test.state || job.Handle.Name != handle.Name {
				t.Fatalf("lookup = %+v, %v; want state %v", job, err, test.state)
			}
		})
	}
}

func TestDispatchStopsDeliveringToAnEndedInitContainer(t *testing.T) {
	fast(t)
	api := newFakeAPI(t)
	adapter, execer := api.adapter(t)
	files := map[string][]byte{"a.csv": []byte("a"), "b.csv": []byte("b")}
	run := fetchRun("ended", newObjects(t, serveFiles(files)), files)
	name := adapter.jobName(run.ID, run.Turn)
	init := startInit(t, api, execer, name, run.Fetches, nil)
	execer.set(func() {
		execer.fail = func(_ string, args []string) error {
			if args[1] == "put" && args[2] == "b.csv" {
				// Another dispatch delivered b.csv and finished the init container.
				root := execer.roots[name]
				if err := os.WriteFile(filepath.Join(root, "wherry/inputs/b.csv"), []byte("b"), 0o600); err != nil {
					t.Error(err)
				}
				if code := helper.Run(helper.Env{Root: root, Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard}, []string{helper.Protocol, "inputs", "done"}); code != 0 {
					t.Errorf("done exited %d", code)
				}
				for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
					init.mu.Lock()
					ended := init.ended
					init.mu.Unlock()
					if ended {
						break
					}
				}
				return transportFailure
			}
			return nil
		}
	})
	dispatch(t, adapter, run)
	if calls := execer.recorded(); !slices.Equal(calls, []string{"inputs inputs put a.csv", "inputs inputs put b.csv"}) {
		t.Fatalf("execs %v; want none after the init container ended", calls)
	}
}

func TestFetchRefusesRedirects(t *testing.T) {
	fast(t)
	api := newFakeAPI(t)
	adapter, execer := api.adapter(t)
	files := map[string][]byte{"data.csv": []byte("data")}
	store := newObjects(t, func(response http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/data.csv" {
			http.Redirect(response, r, "/elsewhere/data.csv", http.StatusFound)
			return
		}
		_, _ = response.Write(files["data.csv"])
	})
	run := fetchRun("redirect", store, files)
	startInit(t, api, execer, adapter.jobName(run.ID, run.Turn), run.Fetches, nil)
	dispatch(t, adapter, run)
	if store.count("/elsewhere/data.csv") != 0 {
		t.Fatal("the fetch followed a redirect")
	}
	if calls := execer.recorded(); !slices.Equal(calls, []string{"inputs inputs fail data.csv"}) {
		t.Fatalf("execs %v; want the fetch failed", calls)
	}
}

func TestFetchBoundsTheBody(t *testing.T) {
	fast(t)
	api := newFakeAPI(t)
	adapter, execer := api.adapter(t)
	files := map[string][]byte{"data.csv": []byte("data")}
	store := newObjects(t, func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write(bytes.Repeat([]byte("x"), 8<<20))
	})
	run := fetchRun("bounded", store, files)
	startInit(t, api, execer, adapter.jobName(run.ID, run.Turn), run.Fetches, nil)
	dispatch(t, adapter, run)
	execer.mu.Lock()
	defer execer.mu.Unlock()
	if len(execer.stdin) == 0 || execer.stdin[0] > 5 {
		t.Fatalf("the put could read %v bytes; want at most the size and one more", execer.stdin)
	}
}

func TestFetchFailureNeverStartsTheAgent(t *testing.T) {
	fast(t)
	api := newFakeAPI(t)
	adapter, execer := api.adapter(t)
	files := map[string][]byte{"data.csv": []byte("data")}
	store := newObjects(t, func(response http.ResponseWriter, _ *http.Request) {
		http.Error(response, "AccessDenied", http.StatusForbidden)
	})
	run := fetchRun("denied", store, files)
	startInit(t, api, execer, adapter.jobName(run.ID, run.Turn), run.Fetches, nil)
	handle := dispatch(t, adapter, run)
	job, err := adapter.Lookup(context.Background(), run.ID, run.Turn)
	if err != nil || job.State != wherry.JobNeverStarted {
		t.Fatalf("lookup = %+v, %v; want never started", job, err)
	}
	terminal, err := adapter.Follow(context.Background(), handle, 0, make(chan wherry.Line))
	if err != nil || terminal.Started || terminal.Class != wherry.ClassFetch || terminal.Reason != "input data.csv: not fetched" {
		t.Fatalf("terminal %+v, %v; want never started with class fetch", terminal, err)
	}
}

func TestLookupMissingPod(t *testing.T) {
	api := newFakeAPI(t)
	adapter, _ := api.adapter(t)
	if job, err := adapter.Lookup(context.Background(), "run-missing", 1); err != nil || job.State != wherry.JobAbsent {
		t.Fatalf("lookup = %+v, %v; want absent", job, err)
	}
}

func TestLookupReadsDispatchMetadata(t *testing.T) {
	api := newFakeAPI(t)
	adapter, _ := api.adapter(t)
	run := testRun("meta")
	handle := dispatch(t, adapter, run)
	job, err := adapter.Lookup(context.Background(), run.ID, run.Turn)
	if err != nil || job.State != wherry.JobRunning || job.Attempt != 2 || job.Adapter != testAdapter || job.Agent != "coder" || job.Handle.Name != handle.Name {
		t.Fatalf("lookup = %+v, %v", job, err)
	}
	api.agentPod(t, adapter, "unannotated", func(pod *corev1.Pod) { pod.Annotations = nil })
	if job, err := adapter.Lookup(context.Background(), "run-unannotated", 1); err != nil || job.Attempt != 0 {
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
	state         wherry.JobState
	settled       bool
	class, reason string
}{
	{"running", wherry.JobRunning, true, "", ""},
	{"exit-0", wherry.JobEnded, true, "", ""},
	{"exit-1", wherry.JobEnded, true, "", ""},
	{"sigterm", wherry.JobEnded, true, "", ""},
	{"oom", wherry.JobEnded, true, "", ""},
	{"image-pull-backoff", wherry.JobNeverStarted, true, "image", `Back-off pulling image "registry.invalid/wherry-conformance:0": ErrImagePull: initializing source docker://registry.invalid/wherry-conformance:0: pinging container registry registry.invalid: Get "https://registry.invalid/v2/": dial tcp: lookup registry.invalid: no such host`},
	{"create-container-config-error", wherry.JobNeverStarted, true, "config", `container has runAsNonRoot and image will run as root (pod: "agent-44575cf5b285-capture-create-container-config-error-t1_wherry-agents(6f1c0b8e)", container: agent)`},
	{"start-error", wherry.JobNeverStarted, true, "start", `failed to create containerd task: exec: "/bin/agent-run": stat /bin/agent-run: no such file or directory`},
	{"fetch-failure", wherry.JobNeverStarted, true, wherry.ClassFetch, "input data.csv: not fetched"},
	{"setup-failure", wherry.JobNeverStarted, true, "setup", "cannot create files"},
	{"protocol-mismatch", wherry.JobNeverStarted, true, "setup", "helper protocol v1 not supported"},
	{"evicted", wherry.JobNeverStarted, true, "pod", "Evicted: The node was low on resource: ephemeral-storage. Threshold quantity: 4902142351, available: 3893504Ki. "},
	{"err-image-pull", 0, false, "", ""},
	{"unschedulable", 0, false, "", ""},
	{"lost-node-running", wherry.JobEnded, true, "", ""},
	{"lost-node-pending", wherry.JobNeverStarted, true, "pod", "the pod is being deleted"},
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
			adapter, _ := api.adapter(t)
			api.set(func() { api.kubelet = nil })
			api.put(pod)
			ctx, cancel := context.Background(), context.CancelFunc(func() {})
			if !test.settled {
				ctx, cancel = context.WithTimeout(ctx, 500*time.Millisecond)
			}
			defer cancel()
			job, err := adapter.Lookup(ctx, "capture-"+test.file, 1)
			if !test.settled {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("lookup of a pending pod = %+v, %v; want the wait cut short", job, err)
				}
				return
			}
			if err != nil || job.State != test.state || job.Adapter != "openshift" || job.Agent != "coder" || job.Attempt != 1 {
				t.Fatalf("lookup = %+v, %v; want %v", job, err, test.state)
			}
		})
	}
}

func TestLookupTreatsADeletingPodAsEnded(t *testing.T) {
	api := newFakeAPI(t)
	adapter, _ := api.adapter(t)
	api.set(func() { api.kubelet = nil })
	for file, want := range map[string]wherry.JobState{"lost-node-running": wherry.JobEnded, "lost-node-pending": wherry.JobNeverStarted} {
		api.put(captured(t, file))
		job, err := adapter.Lookup(context.Background(), "capture-"+file, 1)
		if err != nil || job.State != want {
			t.Fatalf("%s: lookup = %+v, %v; want %v", file, job, err, want)
		}
		_, terminal, _ := judge(api.pod(job.Handle.Name))
		if terminal.ExitCode != -1 || want == wherry.JobNeverStarted && (terminal.Class != "pod" || terminal.Reason != "the pod is being deleted") {
			t.Fatalf("%s: terminal %+v", file, terminal)
		}
	}
}

func TestLookupRefusesAForeignPod(t *testing.T) {
	api := newFakeAPI(t)
	adapter, _ := api.adapter(t)
	api.put(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: poolPrefix + "run-foreign-t1"}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "agent"}}}})
	if _, err := adapter.Lookup(context.Background(), "run-foreign", 1); err == nil || err.Error() != "existing pod is not an agent pod" {
		t.Fatalf("lookup of a foreign pod: %v", err)
	}
}

func TestLookupWaitsForAPendingPod(t *testing.T) {
	fast(t)
	api := newFakeAPI(t)
	adapter, _ := api.adapter(t)
	reads := 0
	api.set(func() {
		api.kubelet = func(pod *corev1.Pod) {
			if reads++; reads < 4 {
				pendingKubelet(pod)
				return
			}
			setStatus(pod, corev1.PodRunning, exited("inputs", 0), runningStatus("agent"), runningStatus("outputs"))
		}
	})
	api.agentPod(t, adapter, "pending", nil)
	if job, err := adapter.Lookup(context.Background(), "run-pending", 1); err != nil || job.State != wherry.JobRunning {
		t.Fatalf("lookup = %+v, %v; want running once the agent started", job, err)
	}
}

func TestLookupTimeoutDeletesThePod(t *testing.T) {
	fast(t)
	api := newFakeAPI(t)
	adapter, _ := api.adapter(t)
	api.set(func() { api.kubelet = pendingKubelet })
	pod := api.agentPod(t, adapter, "stuck", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := adapter.Lookup(ctx, "run-stuck", 1); !errors.Is(err, context.DeadlineExceeded) {
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
	adapter, err := New(testConfig(api.server.URL, api.caFile), testAdapter, testPool, client, hclog.NewNullLogger())
	if err != nil {
		t.Fatal(err)
	}
	api.set(func() { api.kubelet = pendingKubelet })
	pod := api.agentPod(t, adapter, "limited", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var failure *wherry.Error
	if _, err := adapter.Lookup(ctx, "run-limited", 1); !errors.Is(err, context.DeadlineExceeded) || errors.As(err, &failure) && failure.Class != "timeout" {
		t.Fatalf("lookup = %v; want the deadline", err)
	}
	if api.pod(pod.Name) != nil {
		t.Fatal("the pod of a lookup cut short by the rate limiter remains")
	}
	if samples(t, adapter)["wherry_adapter_platform_errors_total/other"].Value != 0 {
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

func followAll(t *testing.T, adapter *Adapter, handle wherry.Handle, from int64) ([]wherry.Line, wherry.Terminal, error) {
	t.Helper()
	lines := make(chan wherry.Line)
	type followed struct {
		terminal wherry.Terminal
		err      error
	}
	done := make(chan followed, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go func() {
		terminal, err := adapter.Follow(ctx, handle, from, lines)
		done <- followed{terminal, err}
	}()
	var got []wherry.Line
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
			setStatus(pod, corev1.PodRunning, exited("inputs", 0), runningStatus("agent"), runningStatus("outputs"))
			return
		}
		setStatus(pod, corev1.PodRunning, exited("inputs", 0), exited("agent", code), runningStatus("outputs"))
	}
}

func TestFollowOffsetsAndReattach(t *testing.T) {
	fast(t)
	api := newFakeAPI(t)
	adapter, _ := api.adapter(t)
	logs, _ := logStream(func(int) []byte { return []byte("a\nbb\nccc\n") }, nil)
	api.set(func() { api.logs, api.kubelet = logs, endsAfter(0, 0) })
	handle := handleOf(api.agentPod(t, adapter, "offsets", nil))
	lines, _, err := followAll(t, adapter, handle, 0)
	want := []wherry.Line{{Offset: 0, Next: 2, Data: []byte("a")}, {Offset: 2, Next: 5, Data: []byte("bb")}, {Offset: 5, Next: 9, Data: []byte("ccc")}}
	if err != nil || fmt.Sprint(lines) != fmt.Sprint(want) {
		t.Fatalf("lines %v, %v; want %v", lines, err, want)
	}
	if lines, _, err := followAll(t, adapter, handle, 2); err != nil || fmt.Sprint(lines) != fmt.Sprint(want[1:]) {
		t.Fatalf("reattached at 2: %v, %v; want %v", lines, err, want[1:])
	}
}

func TestFollowReadsTheStreamToItsEnd(t *testing.T) {
	fast(t)
	api := newFakeAPI(t)
	adapter, _ := api.adapter(t)
	logs, opened := logStream(func(int) []byte { return []byte("one\ntwo\n{\"type\":\"result\"}") }, []byte("rotated"))
	api.set(func() { api.logs, api.kubelet = logs, endsAfter(1, 0) })
	handle := handleOf(api.agentPod(t, adapter, "end", nil))
	lines, terminal, err := followAll(t, adapter, handle, 0)
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
	adapter, _ := api.adapter(t)
	line := append(bytes.Repeat([]byte("x"), 1023), '\n')
	whole := bytes.Repeat(line, 60<<10)
	logs, _ := logStream(func(int) []byte { return whole }, whole[len(whole)-10<<20:])
	api.set(func() { api.logs, api.kubelet = logs, endsAfter(1, 0) })
	handle := handleOf(api.agentPod(t, adapter, "rotation", nil))
	// A slower poll leaves the fake's time to the stream.
	followPoll = 200 * time.Millisecond
	lines, terminal, err := followAll(t, adapter, handle, 0)
	if err != nil || len(lines) != 60<<10 || lines[len(lines)-1].Next != int64(len(whole)) || !terminal.Started {
		t.Fatalf("%d lines ending at %d, %+v, %v; want %d lines ending at %d", len(lines), lines[len(lines)-1].Next, terminal, err, 60<<10, len(whole))
	}
}

func TestFollowReopensAStreamThatClosedEarly(t *testing.T) {
	fast(t)
	api := newFakeAPI(t)
	adapter, _ := api.adapter(t)
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
				setStatus(pod, corev1.PodRunning, exited("inputs", 0), runningStatus("agent"), runningStatus("outputs"))
				return
			}
			setStatus(pod, corev1.PodRunning, exited("inputs", 0), exited("agent", 0), runningStatus("outputs"))
		}
	})
	handle := handleOf(api.agentPod(t, adapter, "early", nil))
	lines, _, err := followAll(t, adapter, handle, 0)
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
	adapter, _ := api.adapter(t)
	logs, _ := logStream(func(int) []byte { return []byte("short\n") }, nil)
	api.set(func() { api.logs, api.kubelet = logs, endsAfter(0, 0) })
	handle := handleOf(api.agentPod(t, adapter, "short", nil))
	if _, _, err := followAll(t, adapter, handle, 100); err == nil || err.Error() != "the agent's log is shorter than the offset" {
		t.Fatalf("follow from past the log's end: %v", err)
	}
}

func TestFollowReturnsTerminal(t *testing.T) {
	fast(t)
	api := newFakeAPI(t)
	adapter, _ := api.adapter(t)
	logs, _ := logStream(func(int) []byte { return []byte("done\n") }, nil)
	api.set(func() {
		api.logs = logs
		api.kubelet = func(pod *corev1.Pod) {
			agent := exited("agent", 137)
			agent.State.Terminated.Signal = 9
			setStatus(pod, corev1.PodRunning, exited("inputs", 0), agent, runningStatus("outputs"))
			pod.Spec.NodeName = "worker-2"
		}
	})
	handle := handleOf(api.agentPod(t, adapter, "terminal", nil))
	_, terminal, err := followAll(t, adapter, handle, 0)
	if want := (wherry.Terminal{Started: true, ExitCode: 137, Signal: "9", Node: "worker-2"}); err != nil || terminal != want {
		t.Fatalf("terminal %+v, %v; want %+v", terminal, err, want)
	}
}

func TestFollowOfANeverStartedPod(t *testing.T) {
	api := newFakeAPI(t)
	adapter, _ := api.adapter(t)
	api.set(func() { api.kubelet = nil })
	pod := api.put(captured(t, "image-pull-backoff"))
	_, terminal, err := followAll(t, adapter, handleOf(pod), 0)
	if err != nil || terminal.Started || terminal.Class != "image" {
		t.Fatalf("terminal %+v, %v; want never started with class image", terminal, err)
	}
	if logs := slices.DeleteFunc(paths(api.recorded(), ""), func(p string) bool { return !strings.HasSuffix(p, "/log") }); len(logs) != 0 {
		t.Fatalf("log requests %v for a pod that never started", logs)
	}
}

func TestFollowOfADeletingPodOpensNoLogs(t *testing.T) {
	fast(t)
	logRequests := func(api *fakeAPI) int {
		return len(slices.DeleteFunc(paths(api.recorded(), ""), func(p string) bool { return !strings.HasSuffix(p, "/log") }))
	}
	api := newFakeAPI(t)
	adapter, _ := api.adapter(t)
	api.set(func() { api.kubelet = nil })
	pod := api.put(captured(t, "lost-node-running"))
	_, terminal, err := followAll(t, adapter, handleOf(pod), 0)
	if err != nil || !terminal.Started || terminal.ExitCode != -1 || logRequests(api) != 0 {
		t.Fatalf("terminal %+v, %v with %d log requests; want exit code -1 and none", terminal, err, logRequests(api))
	}
	api = newFakeAPI(t)
	adapter, _ = api.adapter(t)
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
	pod = api.agentPod(t, adapter, "deleting", nil)
	_, terminal, err = followAll(t, adapter, handleOf(pod), 0)
	if err != nil || !terminal.Started || terminal.ExitCode != -1 || logRequests(api) != 1 {
		t.Fatalf("terminal %+v, %v with %d log requests; want exit code -1 and the first alone", terminal, err, logRequests(api))
	}
}

func TestTerminalCarriesPlatformReason(t *testing.T) {
	const message = `Back-off pulling image "registry.example.com/agent:1.0": unauthorized: token=registry-secret-in-message ` + "é"
	api := newFakeAPI(t)
	adapter, _ := api.adapter(t)
	api.set(func() {
		api.kubelet = func(pod *corev1.Pod) {
			setStatus(pod, corev1.PodPending, exited("inputs", 0), waiting("agent", "ImagePullBackOff", message), runningStatus("outputs"))
		}
	})
	pod := api.agentPod(t, adapter, "reason", nil)
	_, terminal, err := followAll(t, adapter, handleOf(pod), 0)
	if err != nil || terminal.Reason != message {
		t.Fatalf("reason %q, %v; want the kubelet's message as it is", terminal.Reason, err)
	}
}

func TestStaleHandleRefusesReplacedPod(t *testing.T) {
	api := newFakeAPI(t)
	adapter, execer := api.adapter(t)
	pod := api.agentPod(t, adapter, "replaced", nil)
	stale := handleOf(pod)
	stale.Data = bytes.Replace(stale.Data, []byte(pod.UID), []byte("uid-of-the-purged-pod"), 1)
	for name, call := range map[string]func(context.Context) error{
		"follow": func(ctx context.Context) error {
			_, err := adapter.Follow(ctx, stale, 0, make(chan wherry.Line))
			return err
		},
		"stop": func(ctx context.Context) error { return adapter.Stop(ctx, stale) },
		"collect": func(ctx context.Context) error {
			return adapter.Collect(ctx, stale, wherry.Limits{Files: 10}, make(chan wherry.Output, 10))
		},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := call(ctx)
		cancel()
		if !errors.Is(err, errReplaced) {
			t.Fatalf("%s through a stale handle: %v; want the pod was replaced", name, err)
		}
	}
	if calls := execer.recorded(); len(calls) != 0 {
		t.Fatalf("execs %v through a stale handle", calls)
	}
}

// stopsOn is a kubelet and a signal call: the agent ends on the first signal in kills.
func stopsOn(api *fakeAPI, execer *fakeExec, kills ...syscall.Signal) {
	var mu sync.Mutex
	killed := false
	execer.set(func() {
		execer.signal = func(pid int, sig syscall.Signal) error {
			mu.Lock()
			defer mu.Unlock()
			if pid == agentPID && slices.Contains(kills, sig) {
				killed = true
			}
			return nil
		}
	})
	api.set(func() {
		api.kubelet = func(pod *corev1.Pod) {
			mu.Lock()
			defer mu.Unlock()
			if killed {
				setStatus(pod, corev1.PodRunning, exited("inputs", 0), exited("agent", 143), runningStatus("outputs"))
				return
			}
			setStatus(pod, corev1.PodRunning, exited("inputs", 0), runningStatus("agent"), runningStatus("outputs"))
		}
	})
}

func TestStopSignalsThenWaits(t *testing.T) {
	fast(t)
	api := newFakeAPI(t)
	adapter, execer := api.adapter(t)
	stopsOn(api, execer, syscall.SIGTERM)
	pod := api.agentPod(t, adapter, "stop", nil)
	if err := adapter.Stop(context.Background(), handleOf(pod)); err != nil {
		t.Fatal(err)
	}
	if calls := execer.recorded(); !slices.Equal(calls, []string{"outputs stop TERM"}) {
		t.Fatalf("execs %v; want one SIGTERM", calls)
	}
	if deleted := paths(api.recorded(), http.MethodDelete); deleted != nil {
		t.Fatalf("stop deleted %v", deleted)
	}
}

func TestStopEscalatesAfterGrace(t *testing.T) {
	fast(t)
	api := newFakeAPI(t)
	adapter, execer := api.adapter(t)
	stopsOn(api, execer, syscall.SIGKILL)
	pod := api.agentPod(t, adapter, "escalate", nil)
	if err := adapter.Stop(context.Background(), handleOf(pod)); err != nil {
		t.Fatal(err)
	}
	if calls := execer.recorded(); !slices.Equal(calls, []string{"outputs stop TERM", "outputs stop KILL"}) {
		t.Fatalf("execs %v; want SIGTERM then SIGKILL", calls)
	}
	if deleted := paths(api.recorded(), http.MethodDelete); deleted != nil {
		t.Fatalf("stop deleted %v", deleted)
	}
}

func TestStopReturnsNilAfterDelete(t *testing.T) {
	fast(t)
	api := newFakeAPI(t)
	adapter, execer := api.adapter(t)
	var logs bytes.Buffer
	adapter.logger = hclog.New(&hclog.LoggerOptions{Output: &logs, JSONFormat: true})
	stopsOn(api, execer)
	pod := api.agentPod(t, adapter, "survivor", nil)
	if err := adapter.Stop(context.Background(), handleOf(pod)); err != nil {
		t.Fatalf("stop = %v; want nil once the pod is deleted", err)
	}
	requests := api.recorded()
	if deleted := paths(requests, http.MethodDelete); !slices.Equal(deleted, []string{"DELETE /api/v1/namespaces/agents/pods/" + pod.Name}) || !strings.Contains(string(requests[len(requests)-1].body), `"gracePeriodSeconds":0`) {
		t.Fatalf("deletes %v; want the pod deleted with grace 0", deleted)
	}
	if !strings.Contains(logs.String(), "agent outlived SIGKILL; pod deleted") || strings.Count(logs.String(), "\n") != 1 {
		t.Fatalf("logs %q; want one warning", logs.String())
	}
}

func TestStopReturnsTheDeleteError(t *testing.T) {
	fast(t)
	api := newFakeAPI(t)
	adapter, execer := api.adapter(t)
	stopsOn(api, execer)
	pod := api.agentPod(t, adapter, "undeletable", nil)
	api.set(func() { api.intercept = failing(http.StatusInternalServerError, http.MethodDelete, "/pods/"+pod.Name) })
	var failure *wherry.Error
	if err := adapter.Stop(context.Background(), handleOf(pod)); !errors.As(err, &failure) || failure.Op != "delete pod "+pod.Name {
		t.Fatalf("stop = %v; want the delete's error", err)
	}
}

func outputsOf(t *testing.T, adapter *Adapter, handle wherry.Handle, limits wherry.Limits) ([]wherry.Output, error) {
	t.Helper()
	outputs := make(chan wherry.Output, 64)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := adapter.Collect(ctx, handle, limits, outputs)
	close(outputs)
	var collected []wherry.Output
	for output := range outputs {
		collected = append(collected, output)
	}
	return collected, err
}

var limits = wherry.Limits{Files: 64, FileBytes: 1 << 20, TotalBytes: 4 << 20}

func writeOutputs(t *testing.T, execer *fakeExec, pod string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(execer.root(pod), "wherry/outputs")
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

func TestStopKeepsOutputs(t *testing.T) {
	fast(t)
	api := newFakeAPI(t)
	adapter, execer := api.adapter(t)
	stopsOn(api, execer, syscall.SIGTERM)
	pod := api.agentPod(t, adapter, "keeps", nil)
	writeOutputs(t, execer, pod.Name, map[string]string{"report.md": "report"})
	if err := adapter.Stop(context.Background(), handleOf(pod)); err != nil {
		t.Fatal(err)
	}
	collected, err := outputsOf(t, adapter, handleOf(pod), limits)
	if err != nil || len(collected) != 1 || string(collected[0].Data) != "report" {
		t.Fatalf("collected %v, %v after stop; want the report", collected, err)
	}
	if deleted := paths(api.recorded(), http.MethodDelete); deleted != nil {
		t.Fatalf("stop deleted %v", deleted)
	}
}

func TestStopOfAnEndedAgentDoesNotSignal(t *testing.T) {
	api := newFakeAPI(t)
	adapter, execer := api.adapter(t)
	api.set(func() { api.kubelet = endsAfter(0, 0) })
	pod := api.agentPod(t, adapter, "ended", nil)
	if err := adapter.Stop(context.Background(), handleOf(pod)); err != nil || len(execer.recorded()) != 0 {
		t.Fatalf("stop = %v with execs %v; want nil and none", err, execer.recorded())
	}
	absent := handleOf(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: poolPrefix + "run-gone-t1", UID: "uid-gone"}})
	if err := adapter.Stop(context.Background(), absent); err != nil {
		t.Fatalf("stop of an absent pod = %v", err)
	}
}

func TestStopOfANeverStartedPodLeavesIt(t *testing.T) {
	api := newFakeAPI(t)
	adapter, execer := api.adapter(t)
	api.set(func() { api.kubelet = nil })
	pod := api.put(captured(t, "image-pull-backoff"))
	if err := adapter.Stop(context.Background(), handleOf(pod)); err != nil || len(execer.recorded()) != 0 {
		t.Fatalf("stop = %v with execs %v; want nil and none", err, execer.recorded())
	}
	if deleted := paths(api.recorded(), http.MethodDelete); deleted != nil {
		t.Fatalf("stop deleted %v; the core purges a pod that never started", deleted)
	}
}

func TestCollectWaitsForTheAgentToEnd(t *testing.T) {
	fast(t)
	api := newFakeAPI(t)
	adapter, execer := api.adapter(t)
	api.set(func() { api.kubelet = endsAfter(3, 0) })
	pod := api.agentPod(t, adapter, "waits", nil)
	writeOutputs(t, execer, pod.Name, map[string]string{"report.md": "report"})
	if collected, err := outputsOf(t, adapter, handleOf(pod), limits); err != nil || len(collected) != 1 {
		t.Fatalf("collected %v, %v", collected, err)
	}
}

func TestCollectRefusesARunningAgent(t *testing.T) {
	fast(t)
	api := newFakeAPI(t)
	adapter, execer := api.adapter(t)
	pod := api.agentPod(t, adapter, "running", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := adapter.Collect(ctx, handleOf(pod), limits, make(chan wherry.Output, 1)); err == nil || err.Error() != "the agent is still running" {
		t.Fatalf("collect = %v", err)
	}
	if len(execer.recorded()) != 0 {
		t.Fatalf("execs %v into a running agent's pod", execer.recorded())
	}
}

func TestCollectRefusesAnEndedHolder(t *testing.T) {
	api := newFakeAPI(t)
	adapter, _ := api.adapter(t)
	api.set(func() {
		api.kubelet = func(pod *corev1.Pod) {
			setStatus(pod, corev1.PodFailed, exited("inputs", 0), exited("agent", 0), exited("outputs", 137))
		}
	})
	pod := api.agentPod(t, adapter, "holder", nil)
	if _, err := outputsOf(t, adapter, handleOf(pod), limits); err == nil || err.Error() != "the outputs container has ended" {
		t.Fatalf("collect = %v", err)
	}
}

func TestCollectRefusesADeletingPod(t *testing.T) {
	api := newFakeAPI(t)
	adapter, execer := api.adapter(t)
	api.set(func() { api.kubelet = nil })
	pod := api.put(captured(t, "lost-node-running"))
	if _, err := outputsOf(t, adapter, handleOf(pod), limits); err == nil || err.Error() != "the pod is being deleted" {
		t.Fatalf("collect = %v", err)
	}
	if len(execer.recorded()) != 0 {
		t.Fatalf("execs %v into a deleting pod", execer.recorded())
	}
}

func TestCollectCountsTheTreeBeforeReading(t *testing.T) {
	api := newFakeAPI(t)
	adapter, execer := api.adapter(t)
	api.set(func() { api.kubelet = endsAfter(0, 0) })
	pod := api.agentPod(t, adapter, "count", nil)
	writeOutputs(t, execer, pod.Name, map[string]string{"a": "a", "b": "b", "c/d": "d"})
	collected, err := outputsOf(t, adapter, handleOf(pod), wherry.Limits{Files: 3, FileBytes: 10, TotalBytes: 10})
	if err == nil || len(collected) != 0 {
		t.Fatalf("collected %v, %v from a tree of four entries; want it refused", collected, err)
	}
	if calls := execer.recorded(); !slices.Equal(calls, []string{"outputs list 3"}) {
		t.Fatalf("execs %v; want the listing alone", calls)
	}
}

func TestCollectReadsOnlyRegularFiles(t *testing.T) {
	api := newFakeAPI(t)
	adapter, execer := api.adapter(t)
	api.set(func() { api.kubelet = endsAfter(0, 0) })
	pod := api.agentPod(t, adapter, "regular", nil)
	dir := writeOutputs(t, execer, pod.Name, map[string]string{"report.md": "report", "sub/notes.txt": "notes"})
	if err := os.Symlink("report.md", filepath.Join(dir, "link.md")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "fifo"), 0o644); err != nil {
		t.Fatal(err)
	}
	collected, err := outputsOf(t, adapter, handleOf(pod), limits)
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
	if calls := execer.recorded(); !slices.Equal(calls, []string{"outputs list 64", "outputs read report.md 6", "outputs read sub/notes.txt 5"}) {
		t.Fatalf("execs %v; want reads of the regular files alone", calls)
	}
}

func TestCollectLeavesFilesOverTheLimitsUnread(t *testing.T) {
	api := newFakeAPI(t)
	adapter, execer := api.adapter(t)
	api.set(func() { api.kubelet = endsAfter(0, 0) })
	pod := api.agentPod(t, adapter, "limits", nil)
	writeOutputs(t, execer, pod.Name, map[string]string{"a": "aaaa", "b": "bbbbbbbbbbbb", "c": "cccc", "d": "dddd"})
	collected, err := outputsOf(t, adapter, handleOf(pod), wherry.Limits{Files: 10, FileBytes: 8, TotalBytes: 8})
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

func TestCollectRefusesAReadOfAnotherLength(t *testing.T) {
	api := newFakeAPI(t)
	adapter, execer := api.adapter(t)
	api.set(func() { api.kubelet = endsAfter(0, 0) })
	pod := api.agentPod(t, adapter, "length", nil)
	writeOutputs(t, execer, pod.Name, map[string]string{"report.md": "report"})
	for _, body := range []string{"repor", "report!"} {
		execer.set(func() {
			execer.output = func(args []string, stdout io.Writer) bool {
				if args[0] != "read" {
					return false
				}
				_, _ = io.WriteString(stdout, body)
				return true
			}
		})
		if _, err := outputsOf(t, adapter, handleOf(pod), limits); err == nil {
			t.Fatalf("collect accepted a body %q for a listing of 6 bytes", body)
		}
	}
}

func TestCollectStreamsNamedOutputs(t *testing.T) {
	api := newFakeAPI(t)
	adapter, execer := api.adapter(t)
	api.set(func() { api.kubelet = endsAfter(0, 0) })
	pod := api.agentPod(t, adapter, "stream", nil)
	writeOutputs(t, execer, pod.Name, map[string]string{"build.tar": "tarball-bytes", "big.tar": "too-big-to-stream", "report.md": "r"})
	outputs := make(chan wherry.Output, 8)
	if err := adapter.Collect(context.Background(), handleOf(pod), wherry.Limits{Files: 10, FileBytes: 1, TotalBytes: 1, Stream: []string{"build.tar", "big.tar"}, StreamBytes: 13}, outputs); err != nil {
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

func TestCollectWithoutAnOutputsDirectory(t *testing.T) {
	api := newFakeAPI(t)
	adapter, _ := api.adapter(t)
	api.set(func() { api.kubelet = endsAfter(0, 0) })
	pod := api.agentPod(t, adapter, "none", nil)
	if collected, err := outputsOf(t, adapter, handleOf(pod), limits); err != nil || len(collected) != 0 {
		t.Fatalf("collected %v, %v; want nothing", collected, err)
	}
}

func TestReleaseDeletesSecretAndEndsTheHolder(t *testing.T) {
	api := newFakeAPI(t)
	adapter, execer := api.adapter(t)
	api.set(func() { api.kubelet = endsAfter(0, 0) })
	run := testRun("release")
	dispatch(t, adapter, run)
	name := adapter.jobName(run.ID, run.Turn)
	if err := adapter.Release(context.Background(), run.ID, run.Turn); err != nil {
		t.Fatal(err)
	}
	if api.secret(name) != nil {
		t.Fatal("the Secret remains")
	}
	execer.mu.Lock()
	defer execer.mu.Unlock()
	if !slices.Equal(execer.calls, []string{"outputs release"}) || !slices.Equal(execer.signals, []string{fmt.Sprintf("%d terminated", holderPID)}) {
		t.Fatalf("execs %v, signals %v; want the holder sent SIGTERM", execer.calls, execer.signals)
	}
	if api.pod(name) == nil {
		t.Fatal("release removed the pod; the sweep removes it")
	}
}

func TestPurgeDeletesWithGraceZero(t *testing.T) {
	api := newFakeAPI(t)
	adapter, _ := api.adapter(t)
	api.agentPod(t, adapter, "purge", nil)
	if err := adapter.Purge(context.Background(), "run-purge", 1); err != nil {
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
	adapter, _ := api.adapter(t)
	run := testRun("linger")
	dispatch(t, adapter, run)
	api.set(func() { api.linger = 3 })
	if err := adapter.Purge(context.Background(), run.ID, run.Turn); err != nil {
		t.Fatal(err)
	}
	name := adapter.jobName(run.ID, run.Turn)
	if api.pod(name) != nil || api.secret(name) != nil {
		t.Fatal("purge returned with the pod or its Secret left")
	}
	requests := paths(api.recorded(), "")
	if last := requests[len(requests)-1]; last != "DELETE /api/v1/namespaces/agents/secrets/"+name {
		t.Fatalf("last request %s; want the Secret deleted after the pod went", last)
	}
}

func TestPurgeOfAbsentPodSucceeds(t *testing.T) {
	api := newFakeAPI(t)
	adapter, _ := api.adapter(t)
	if err := adapter.Purge(context.Background(), "run-absent", 1); err != nil {
		t.Fatal(err)
	}
}

// sweepPod is an agent pod of adapter owner whose agent ended ago, or is running when ago is 0.
func (api *fakeAPI) sweepPod(t *testing.T, adapter *Adapter, name, owner string, ago time.Duration) string {
	t.Helper()
	pod := api.agentPod(t, adapter, name, func(pod *corev1.Pod) {
		pod.Labels[labelAdapter] = owner
		if ago == 0 {
			setStatus(pod, corev1.PodRunning, exited("inputs", 0), runningStatus("agent"), runningStatus("outputs"))
			return
		}
		agent := exited("agent", 0)
		agent.State.Terminated.FinishedAt = metav1.NewTime(time.Now().Add(-ago))
		setStatus(pod, corev1.PodRunning, exited("inputs", 0), agent, runningStatus("outputs"))
	})
	api.secretFor(pod.Name, owner, 0)
	return pod.Name
}

// secretFor stores the turn Secret of name, created ago.
func (api *fakeAPI) secretFor(name, owner string, ago time.Duration) {
	api.mu.Lock()
	defer api.mu.Unlock()
	_, run, _ := strings.Cut(strings.TrimSuffix(name, "-t1"), poolPrefix)
	api.secrets[name] = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, ResourceVersion: "1", CreationTimestamp: metav1.NewTime(time.Now().Add(-ago)),
		Labels: map[string]string{labelPool: testPool, labelAdapter: owner, labelAgent: "coder", labelRun: run, labelTurn: "1"}}, Data: map[string][]byte{"prompt": []byte("p")}}
}

func sweep(t *testing.T, api *fakeAPI) *Adapter {
	t.Helper()
	adapter, _ := api.adapter(t)
	api.set(func() { api.kubelet = nil })
	return adapter
}

func TestSweepPurgesPodsEndedPastTheGrace(t *testing.T) {
	api := newFakeAPI(t)
	adapter := sweep(t, api)
	name := api.sweepPod(t, adapter, "old", testAdapter, 11*time.Minute)
	if err := adapter.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.pod(name) != nil || api.secret(name) != nil {
		t.Fatal("the ended pod or its Secret remains")
	}
}

func TestSweepKeepsPodsInsideTheGrace(t *testing.T) {
	api := newFakeAPI(t)
	adapter := sweep(t, api)
	name := api.sweepPod(t, adapter, "recent", testAdapter, 9*time.Minute)
	if err := adapter.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.pod(name) == nil {
		t.Fatal("a pod inside the grace was purged")
	}
}

func TestSweepPurgesADeletingPodPastTheGrace(t *testing.T) {
	for ago, purged := range map[time.Duration]bool{11 * time.Minute: true, 9 * time.Minute: false} {
		api := newFakeAPI(t)
		adapter := sweep(t, api)
		pod := captured(t, "lost-node-running")
		pod.Labels = adapter.labels(testRun("lost"))
		pod.Name = adapter.jobName("run-lost", 1)
		pod.Labels[labelAdapter] = testAdapter
		pod.DeletionTimestamp = new(metav1.NewTime(time.Now().Add(-ago)))
		api.put(pod)
		api.secretFor(pod.Name, testAdapter, time.Hour)
		if err := adapter.Sweep(context.Background()); err != nil {
			t.Fatal(err)
		}
		if gone := api.pod(pod.Name) == nil && api.secret(pod.Name) == nil; gone != purged {
			t.Fatalf("deleting for %v: purged %t; want %t", ago, gone, purged)
		}
	}
}

func TestSweepCountsOnlyThisAdaptersPods(t *testing.T) {
	api := newFakeAPI(t)
	container := sweep(t, api)
	waiting := api.sweepPod(t, container, "waiting", testAdapter, time.Minute)
	expired := api.sweepPod(t, container, "expired", testAdapter, 11*time.Minute)
	host, _ := api.adapter(t)
	host.name = "host"
	if err := host.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, deleted := range paths(api.recorded(), http.MethodDelete) {
		if strings.Contains(deleted, "/pods/") {
			t.Fatalf("host's sweep deleted %v of container's", deleted)
		}
	}
	if got := samples(t, host); got["wherry_adapter_cleanup_pending/"].Value != 0 || got["wherry_adapter_sweep_deletions_total/job"].Value != 0 || got["wherry_adapter_sweep_deletions_total/variable"].Value != 0 {
		t.Fatalf("host samples %+v; want none of container's pods counted", got)
	}
	if err := container.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.pod(expired) != nil || api.pod(waiting) == nil || api.secret(waiting) != nil {
		t.Fatal("container's sweep did not purge its expired pod and keep its waiting one")
	}
	if got := samples(t, container); got["wherry_adapter_cleanup_pending/"].Value != 1 || got["wherry_adapter_sweep_deletions_total/job"].Value != 1 || got["wherry_adapter_sweep_deletions_total/variable"].Value != 1 {
		t.Fatalf("container samples %+v; want its waiting pod pending, its expired pod and the waiting pod's Secret deleted", got)
	}
}

func TestSweepDeletesSecretOfEndedPod(t *testing.T) {
	api := newFakeAPI(t)
	adapter := sweep(t, api)
	name := api.sweepPod(t, adapter, "ended", testAdapter, time.Minute)
	if err := adapter.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.secret(name) != nil || api.pod(name) == nil {
		t.Fatal("want the ended pod's Secret deleted and the pod kept")
	}
}

func TestSweepDeletesSecretOfAnotherAdaptersExpiredPod(t *testing.T) {
	api := newFakeAPI(t)
	adapter := sweep(t, api)
	expired := api.sweepPod(t, adapter, "removed", "removed", 11*time.Minute)
	recent := api.sweepPod(t, adapter, "other", "removed", time.Minute)
	if err := adapter.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.secret(expired) != nil || api.pod(expired) == nil || api.secret(recent) == nil {
		t.Fatal("want the expired pod's Secret deleted, the pod kept, and the recent pod's Secret kept")
	}
	if got := samples(t, adapter)["wherry_adapter_sweep_deletions_total/variable"].Value; got != 0 {
		t.Fatalf("counted %v deletions of another adapter's Secrets", got)
	}
}

func TestSweepKeepsSecretOfRunningPod(t *testing.T) {
	api := newFakeAPI(t)
	adapter := sweep(t, api)
	name := api.sweepPod(t, adapter, "running", testAdapter, 0)
	api.secretFor(name, testAdapter, time.Hour)
	if err := adapter.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.secret(name) == nil {
		t.Fatal("the running pod's Secret was deleted")
	}
}

func TestSweepDeletesStaleSecretWithoutPod(t *testing.T) {
	api := newFakeAPI(t)
	adapter := sweep(t, api)
	api.secretFor(poolPrefix+"run-orphan-t1", testAdapter, 11*time.Minute)
	if err := adapter.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.secret(poolPrefix+"run-orphan-t1") != nil {
		t.Fatal("the orphaned Secret remains")
	}
}

func TestSweepKeepsRecentSecretWithoutPod(t *testing.T) {
	api := newFakeAPI(t)
	adapter := sweep(t, api)
	api.secretFor(poolPrefix+"run-new-t1", testAdapter, time.Minute)
	if err := adapter.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.secret(poolPrefix+"run-new-t1") == nil {
		t.Fatal("a Secret inside the grace was deleted")
	}
}

func TestSweepIgnoresForeignNames(t *testing.T) {
	api := newFakeAPI(t)
	adapter := sweep(t, api)
	pod := api.agentPod(t, adapter, "foreign", func(pod *corev1.Pod) {
		pod.Name = "user-pod"
		setStatus(pod, corev1.PodRunning, exited("inputs", 0), exited("agent", 0), runningStatus("outputs"))
		pod.Status.ContainerStatuses[0].State.Terminated.FinishedAt = metav1.NewTime(time.Now().Add(-time.Hour))
	})
	api.secretFor("user-secret", testAdapter, time.Hour)
	if err := adapter.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.pod(pod.Name) == nil || api.secret("user-secret") == nil {
		t.Fatal("the sweep deleted objects whose names are not their labels' job names")
	}
}

func TestSweepListsSecretsAsMetadata(t *testing.T) {
	api := newFakeAPI(t)
	adapter := sweep(t, api)
	if err := adapter.Sweep(context.Background()); err != nil {
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

func TestSweepReportsDeletionsAndPendingPods(t *testing.T) {
	api := newFakeAPI(t)
	adapter := sweep(t, api)
	api.sweepPod(t, adapter, "old", testAdapter, 11*time.Minute)
	api.sweepPod(t, adapter, "recent", testAdapter, time.Minute)
	api.secretFor(poolPrefix+"run-gone-t1", testAdapter, time.Hour)
	if err := adapter.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := samples(t, adapter)
	if got["wherry_adapter_cleanup_pending/"].Value != 1 || got["wherry_adapter_sweep_deletions_total/job"].Value != 1 || got["wherry_adapter_sweep_deletions_total/variable"].Value != 2 {
		t.Fatalf("samples %+v; want one pending pod, one job and two variables deleted", got)
	}
	for name := range got {
		if family, kind, _ := strings.Cut(name, "/"); family == "wherry_adapter_sweep_deletions_total" && kind != "job" && kind != "variable" {
			t.Fatalf("deletion kind %q outside the core's", kind)
		}
	}
}

func TestReadyFromTheNamespace(t *testing.T) {
	for phase, want := range map[corev1.NamespacePhase]bool{corev1.NamespaceActive: true, corev1.NamespaceTerminating: false, "": false} {
		api := newFakeAPI(t)
		adapter, _ := api.adapter(t)
		api.set(func() { api.phase = phase })
		ready, err := adapter.Ready(context.Background())
		if ready != want || (phase == "") != (err != nil) {
			t.Fatalf("namespace %q: ready = %v, %v; want %v", phase, ready, err, want)
		}
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
		adapter, _ := api.adapter(t)
		api.set(func() {
			api.intercept = func(response http.ResponseWriter, _ *http.Request) bool {
				apiStatus(response, test.code, test.reason, "refused")
				return true
			}
		})
		if _, err := adapter.Ready(context.Background()); err == nil {
			t.Fatalf("ready answered %d without an error", test.code)
		}
		var counted []string
		for name, sample := range samples(t, adapter) {
			if sample.Family == "wherry_adapter_platform_errors_total" {
				counted = append(counted, fmt.Sprintf("%s=%v", name, sample.Value))
			}
		}
		want := []string(nil)
		if test.class != "" {
			want = []string{"wherry_adapter_platform_errors_total/" + test.class + "=1"}
		}
		if !slices.Equal(counted, want) {
			t.Fatalf("status %d %s counted %v; want %v", test.code, test.reason, counted, want)
		}
	}
	api := newFakeAPI(t)
	adapter, _ := api.adapter(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := adapter.Ready(ctx); err == nil || len(slices.DeleteFunc(adapter.metrics.Samples(), func(s wherry.Sample) bool { return s.Family != "wherry_adapter_platform_errors_total" })) != 0 {
		t.Fatalf("a cancelled call: %v, counted", err)
	}
}

func TestUnreachableAPIIsUnavailable(t *testing.T) {
	adapter := plainAdapter(t, nil)
	if _, err := adapter.Ready(context.Background()); err == nil {
		t.Fatal("ready reached a closed port")
	}
	if sample := samples(t, adapter)["wherry_adapter_platform_errors_total/unavailable"]; sample.Value != 1 {
		t.Fatalf("unavailable errors %v; want 1", sample.Value)
	}
}

func TestReturnedErrorCarriesOpAndClass(t *testing.T) {
	const message = `namespaces "agents" is forbidden: User "system:serviceaccount:wherry:wherry-receiver" cannot get resource "namespaces"`
	api := newFakeAPI(t)
	adapter, _ := api.adapter(t)
	api.set(func() {
		api.intercept = func(response http.ResponseWriter, _ *http.Request) bool {
			apiStatus(response, http.StatusForbidden, metav1.StatusReasonForbidden, message)
			return true
		}
	})
	_, err := adapter.Ready(context.Background())
	var failure *wherry.Error
	if !errors.As(err, &failure) || failure.Op != "read namespace agents" || failure.Class != "denied" || !strings.Contains(err.Error(), message) {
		t.Fatalf("error %#v; want op, class denied and the API's message", err)
	}
}

// A returned error is the receiver's to log; one the adapter handles itself, such as a poll read, it logs by op and class.
func TestHandledPlatformErrorIsLoggedOnceWithoutURLOrToken(t *testing.T) {
	const body = "body-never-logged"
	api := newFakeAPI(t)
	adapter, _ := api.adapter(t)
	var logs bytes.Buffer
	adapter.logger = hclog.New(&hclog.LoggerOptions{Output: &logs, JSONFormat: true})
	api.set(func() {
		api.intercept = func(response http.ResponseWriter, _ *http.Request) bool {
			apiStatus(response, http.StatusServiceUnavailable, metav1.StatusReasonServiceUnavailable, body)
			return true
		}
	})
	if _, err := adapter.Ready(context.Background()); err == nil {
		t.Fatal("ready answered 503 without an error")
	}
	if logs.Len() != 0 {
		t.Fatalf("returned error logged by the adapter: %q", logs.String())
	}
	if outcome, _, err := adapter.notStarted(context.Background(), poolPrefix+"run-x-t1", errors.New("cause")); outcome != wherry.OutcomeUnknown || err == nil {
		t.Fatalf("notStarted = %v, %v", outcome, err)
	}
	lines := strings.Split(strings.TrimSuffix(logs.String(), "\n"), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], `"class":"unavailable"`) || !strings.Contains(lines[0], `"op":"read pod `+poolPrefix+`run-x-t1"`) || !strings.Contains(lines[0], `"@level":"warn"`) {
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
	adapter, err := New(testConfig(api.server.URL, api.caFile), testAdapter, testPool, mustClient(t, api), hclog.New(&hclog.LoggerOptions{Output: &logs, JSONFormat: true}))
	if err != nil {
		t.Fatal(err)
	}
	api.set(func() {
		api.intercept = func(response http.ResponseWriter, _ *http.Request) bool {
			response.Header().Set("Warning", `299 - "would violate PodSecurity \"restricted:latest\""`)
			return false
		}
	})
	if _, err := adapter.Ready(context.Background()); err != nil {
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

// turn runs a whole turn with every kind of material through the fake, and returns every request, exec argument, pod
// spec, error and log line it produced.
func turn(t *testing.T) (string, []string) {
	t.Helper()
	fast(t)
	api := newFakeAPI(t)
	adapter, execer := api.adapter(t)
	var logs bytes.Buffer
	adapter.logger = hclog.New(&hclog.LoggerOptions{Output: &logs, JSONFormat: true})
	files := map[string][]byte{"data.csv": []byte("fetched-bytes-secret")}
	store := newObjects(t, serveFiles(files))
	run := fetchRun("turn", store, files)
	run.Model = "sonnet"
	run.Inputs = []wherry.File{{Name: "notes.md", Data: []byte("input-bytes-secret")}}
	run.Credentials = []wherry.Secret{{Name: "deploy", File: "id_ed25519", Value: []byte("task-credential-secret")}}
	name := adapter.jobName(run.ID, run.Turn)
	startInit(t, api, execer, name, run.Fetches, nil)
	var errs []error
	handle := dispatch(t, adapter, run)
	stopsOn(api, execer, syscall.SIGTERM)
	logsHandler, _ := logStream(func(int) []byte { return []byte(`{"type":"result","result":"done"}` + "\n") }, nil)
	api.set(func() { api.logs = logsHandler })
	writeOutputs(t, execer, name, map[string]string{"report.md": "output-bytes-secret"})
	errs = append(errs, adapter.Stop(context.Background(), handle))
	_, _, err := followAll(t, adapter, handle, 0)
	errs = append(errs, err)
	collected, err := outputsOf(t, adapter, handle, limits)
	if len(collected) != 1 || string(collected[0].Data) != "output-bytes-secret" {
		t.Fatalf("collected %v, %v", collected, err)
	}
	errs = append(errs, err, adapter.Release(context.Background(), run.ID, run.Turn), adapter.Purge(context.Background(), run.ID, run.Turn), adapter.Sweep(context.Background()))
	// A second turn whose fetches fail, by status and by connection, and a third whose put the helper refuses.
	failed := fetchRun("failed", newObjects(t, func(response http.ResponseWriter, _ *http.Request) {
		http.Error(response, "SignatureDoesNotMatch", http.StatusForbidden)
	}), files)
	startInit(t, api, execer, adapter.jobName(failed.ID, failed.Turn), failed.Fetches, nil)
	_, _, err = adapter.Dispatch(context.Background(), failed)
	errs = append(errs, err)
	unreachable := fetchRun("unreachable", store, files)
	unreachable.Fetches[0].URL = "https://unreachable.invalid/data.csv?X-Amz-Signature=signature-secret"
	startInit(t, api, execer, adapter.jobName(unreachable.ID, unreachable.Turn), unreachable.Fetches, nil)
	_, _, err = adapter.Dispatch(context.Background(), unreachable)
	errs = append(errs, err)
	refused := fetchRun("refused", store, files)
	refused.Fetches[0].SHA256 = strings.Repeat("0", 64)
	startInit(t, api, execer, adapter.jobName(refused.ID, refused.Turn), refused.Fetches, nil)
	_, _, err = adapter.Dispatch(context.Background(), refused)
	errs = append(errs, err)
	if !strings.Contains(logs.String(), "fetch data.csv: 403") || !strings.Contains(logs.String(), "fetch data.csv: unreachable") || !strings.Contains(logs.String(), "does not match its digest") {
		t.Fatalf("logs %q; want each failed fetch logged", logs.String())
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
	for _, call := range execer.recorded() {
		seen = append(seen, "exec "+call)
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
		for _, secret := range []string{"hello-prompt", "test-credential", "input-bytes-secret", "task-credential-secret", "fetched-bytes-secret", "signature-secret", "X-Amz-Signature", "output-bytes-secret", "unreachable.invalid"} {
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
	return Config{
		Address: address, CAFile: os.Getenv("WHERRY_KUBERNETES_DEV_CA_FILE"), Namespace: os.Getenv("WHERRY_KUBERNETES_DEV_NAMESPACE"),
		CredentialEnv: credentialEnv, HelperImage: os.Getenv("WHERRY_KUBERNETES_DEV_HELPER_IMAGE"),
		Runtime: Runtime{Image: "registry.access.redhat.com/ubi9/ubi-minimal", Command: []string{"/bin/sh", "-c"}, Resources: Resources{CPU: "100m", Memory: "256Mi"}},
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

// devAdapter is an adapter on the real cluster whose agent runs script.
func devAdapter(t *testing.T, script string, edit func(*Config)) (*Adapter, context.Context) {
	t.Helper()
	config := devConfig(t)
	config.Runtime.Args = []string{script}
	if edit != nil {
		edit(&config)
	}
	adapter, err := build(config, testPool, devToken(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)
	return adapter, ctx
}

func devRun(t *testing.T, adapter *Adapter, name string) wherry.Run {
	t.Helper()
	run := testRun(fmt.Sprintf("%s-%d", name, time.Now().UnixNano()))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = adapter.Release(ctx, run.ID, run.Turn)
		if err := adapter.Purge(ctx, run.ID, run.Turn); err != nil {
			t.Errorf("purge %s: %v", run.ID, err)
		}
	})
	return run
}

// devTurn dispatches run and follows it to its end.
func devTurn(t *testing.T, ctx context.Context, adapter *Adapter, run wherry.Run) (wherry.Handle, []wherry.Line, wherry.Terminal) {
	t.Helper()
	outcome, handle, err := adapter.Dispatch(ctx, run)
	if err != nil || outcome != wherry.OutcomeCreated {
		t.Fatalf("dispatch = %v, %v", outcome, err)
	}
	lines, terminal, err := followAll(t, adapter, handle, 0)
	if err != nil {
		t.Fatal(err)
	}
	return handle, lines, terminal
}

func devOutputs(t *testing.T, ctx context.Context, adapter *Adapter, handle wherry.Handle, limits wherry.Limits) map[string]wherry.Output {
	t.Helper()
	outputs := make(chan wherry.Output, 256)
	if err := adapter.Collect(ctx, handle, limits, outputs); err != nil {
		t.Fatal(err)
	}
	close(outputs)
	byName := map[string]wherry.Output{}
	for output := range outputs {
		byName[output.Name] = output
	}
	return byName
}

func TestInputsCredentialsAndOutputsOnKubernetes(t *testing.T) {
	adapter, ctx := devAdapter(t, `set -e; out="$WHERRY_OUTPUT_DIR"; mkdir -p "$out/nested/deep";
		cp "$WHERRY_INPUT_DIR/notes.md" "$out/nested/deep/notes.md"; cat "$WHERRY_CREDENTIALS_DIR/token" > "$out/token";
		cp /wherry/prompt.txt "$out/prompt.txt"; ln -s notes.md "$out/nested/link"; mkfifo "$out/fifo";
		echo '{"type":"result","result":"done"}'`, nil)
	run := devRun(t, adapter, "files")
	run.Inputs = []wherry.File{{Name: "notes.md", Data: []byte{'n', 0, 255}}}
	run.Credentials = []wherry.Secret{{Name: "api", File: "token", Value: []byte("task-token")}}
	handle, _, terminal := devTurn(t, ctx, adapter, run)
	if !terminal.Started || terminal.ExitCode != 0 {
		t.Fatalf("terminal %+v", terminal)
	}
	got := devOutputs(t, ctx, adapter, handle, wherry.Limits{Files: 16, FileBytes: 1 << 20, TotalBytes: 1 << 20})
	if string(got["nested/deep/notes.md"].Data) != "n\x00\xff" || string(got["token"].Data) != "task-token" || string(got["prompt.txt"].Data) != "hello-prompt" ||
		got["nested/link"].Mode != fs.ModeSymlink || got["fifo"].Mode != fs.ModeNamedPipe || got["fifo"].Data != nil {
		t.Fatalf("outputs %v", got)
	}
}

func TestCredentialFileOwnerAndModeOnKubernetes(t *testing.T) {
	adapter, ctx := devAdapter(t, `set -e; dir="$WHERRY_CREDENTIALS_DIR";
		{ stat -c '%a %u' "$dir/key"; stat -c '%a %u' "$dir"; id -u; } > "$WHERRY_OUTPUT_DIR/modes";
		echo '{"type":"result","result":"done"}'`, nil)
	run := devRun(t, adapter, "modes")
	run.Credentials = []wherry.Secret{{Name: "deploy", File: "key", Value: []byte("private-key")}}
	handle, _, _ := devTurn(t, ctx, adapter, run)
	modes := strings.Fields(string(devOutputs(t, ctx, adapter, handle, limits)["modes"].Data))
	if len(modes) != 5 || modes[0] != "600" || modes[2] != "700" || modes[1] != modes[4] || modes[3] != modes[4] {
		t.Fatalf("modes %v; want the file 600 and the directory 700, both the agent's", modes)
	}
}

func TestInputAtTheSecretLimitOnKubernetes(t *testing.T) {
	adapter, ctx := devAdapter(t, `echo '{"type":"result","result":"done"}'`, nil)
	run := devRun(t, adapter, "limit")
	run.Credential = nil
	run.Inputs = []wherry.File{{Name: "fill.bin", Data: bytes.Repeat([]byte{0xff}, maxSecret-len(run.Prompt))}}
	devTurn(t, ctx, adapter, run)
	over := devRun(t, adapter, "over")
	over.Credential = nil
	over.Inputs = []wherry.File{{Name: "fill.bin", Data: bytes.Repeat([]byte{0xff}, maxSecret-len(run.Prompt)+1)}}
	var tooLarge *wherry.InputTooLargeError
	if outcome, _, err := adapter.Dispatch(ctx, over); outcome != wherry.OutcomeNothingCreated || !errors.As(err, &tooLarge) {
		t.Fatalf("dispatch one byte over = %v, %v", outcome, err)
	}
	data, _ := secretData(over)
	name := adapter.jobName(over.ID, over.Turn)
	if _, err := adapter.secrets().Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name}, Data: data}, metav1.CreateOptions{}); err == nil {
		t.Fatal("the API server accepted a Secret one byte over 1 MiB")
	}
}

func TestFetchOnKubernetes(t *testing.T) {
	files := map[string][]byte{"data.csv": []byte("a,b\n1,2\n")}
	store := httptest.NewTLSServer(http.HandlerFunc(serveFiles(files)))
	t.Cleanup(store.Close)
	adapter, ctx := devAdapter(t, `set -e; cp "$WHERRY_INPUT_DIR/data.csv" "$WHERRY_OUTPUT_DIR/"; echo '{"type":"result","result":"done"}'`, nil)
	adapter.fetcher.Transport = store.Client().Transport
	run := devRun(t, adapter, "fetch")
	run.Fetches = []wherry.Fetch{{Name: "data.csv", URL: store.URL + "/data.csv?X-Amz-Signature=s", Size: int64(len(files["data.csv"])), SHA256: sha(files["data.csv"])}}
	handle, _, _ := devTurn(t, ctx, adapter, run)
	if got := devOutputs(t, ctx, adapter, handle, limits)["data.csv"].Data; !bytes.Equal(got, files["data.csv"]) {
		t.Fatalf("fetched %q", got)
	}
	mismatch := devRun(t, adapter, "mismatch")
	mismatch.Fetches = []wherry.Fetch{{Name: "data.csv", URL: store.URL + "/data.csv", Size: int64(len(files["data.csv"])), SHA256: strings.Repeat("0", 64)}}
	_, _, terminal := devTurn(t, ctx, adapter, mismatch)
	if terminal.Started || terminal.Class != wherry.ClassFetch {
		t.Fatalf("terminal %+v; want never started with class fetch", terminal)
	}
}

// stopRunning dispatches script, waits for its first line, stops it and returns its handle.
func stopRunning(t *testing.T, ctx context.Context, adapter *Adapter, run wherry.Run) wherry.Handle {
	t.Helper()
	outcome, handle, err := adapter.Dispatch(ctx, run)
	if err != nil || outcome != wherry.OutcomeCreated {
		t.Fatalf("dispatch = %v, %v", outcome, err)
	}
	lines := make(chan wherry.Line)
	followCtx, cancel := context.WithCancel(ctx)
	go func() { _, _ = adapter.Follow(followCtx, handle, 0, lines) }()
	<-lines
	cancel()
	began := time.Now()
	if err := adapter.Stop(ctx, handle); err != nil {
		t.Fatal(err)
	}
	t.Logf("stopped in %v", time.Since(began))
	return handle
}

func TestStopKeepsOutputsOnKubernetes(t *testing.T) {
	adapter, ctx := devAdapter(t, `echo report > "$WHERRY_OUTPUT_DIR/report.md"; echo started; sleep 600`, nil)
	handle := stopRunning(t, ctx, adapter, devRun(t, adapter, "stop"))
	if got := devOutputs(t, ctx, adapter, handle, limits)["report.md"].Data; string(got) != "report\n" {
		t.Fatalf("collected %q after stop", got)
	}
}

func TestStopEscalatesOnKubernetes(t *testing.T) {
	adapter, ctx := devAdapter(t, `trap '' TERM; echo report > "$WHERRY_OUTPUT_DIR/report.md"; echo started; while :; do sleep 1; done`, nil)
	began := time.Now()
	handle := stopRunning(t, ctx, adapter, devRun(t, adapter, "escalate"))
	if elapsed := time.Since(began); elapsed > time.Minute {
		t.Fatalf("stop took %v", elapsed)
	}
	if got := devOutputs(t, ctx, adapter, handle, limits)["report.md"].Data; string(got) != "report\n" {
		t.Fatalf("collected %q after SIGKILL", got)
	}
}

func TestCollectAfterAgentOOMOnKubernetes(t *testing.T) {
	adapter, ctx := devAdapter(t, `echo report > "$WHERRY_OUTPUT_DIR/report.md"; head -c 512m /dev/zero | tail`, func(c *Config) {
		c.Runtime.Resources.Memory = "64Mi"
	})
	handle, _, terminal := devTurn(t, ctx, adapter, devRun(t, adapter, "oom"))
	if !terminal.Started || terminal.ExitCode == 0 {
		t.Fatalf("terminal %+v; want the agent killed", terminal)
	}
	if got := devOutputs(t, ctx, adapter, handle, limits)["report.md"].Data; string(got) != "report\n" {
		t.Fatalf("collected %q after the agent's OOM", got)
	}
}

func TestAdoptionOnKubernetes(t *testing.T) {
	const script = `i=0; while [ $i -lt 60 ]; do echo "line $i"; i=$((i+1)); sleep 1; done; echo report > "$WHERRY_OUTPUT_DIR/report.md"; sleep 600`
	first, ctx := devAdapter(t, script, nil)
	run := devRun(t, first, "adopt")
	run.Credential = nil
	outcome, handle, err := first.Dispatch(ctx, run)
	if err != nil || outcome != wherry.OutcomeCreated {
		t.Fatalf("dispatch = %v, %v", outcome, err)
	}
	lines := make(chan wherry.Line)
	followCtx, cancel := context.WithCancel(ctx)
	go func() { _, _ = first.Follow(followCtx, handle, 0, lines) }()
	var last wherry.Line
	for range 5 {
		last = <-lines
	}
	cancel()
	second, _ := devAdapter(t, script, nil)
	job, err := second.Lookup(ctx, run.ID, run.Turn)
	if err != nil || job.State != wherry.JobRunning || job.Adapter != testAdapter {
		t.Fatalf("lookup = %+v, %v", job, err)
	}
	resumed := make(chan wherry.Line)
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

func outputsOfDev(ctx context.Context, adapter *Adapter, handle wherry.Handle) (map[string]wherry.Output, error) {
	outputs := make(chan wherry.Output, 16)
	collectCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err := adapter.Collect(collectCtx, handle, limits, outputs)
	close(outputs)
	byName := map[string]wherry.Output{}
	for output := range outputs {
		byName[output.Name] = output
	}
	return byName, err
}

// streamLarge streams a 1.5 GiB output, measuring the holder's memory peak meanwhile when measure is set.
func streamLarge(t *testing.T, measure bool) {
	adapter, ctx := devAdapter(t, `head -c 1536m /dev/urandom > "$WHERRY_OUTPUT_DIR/big.bin"; echo '{"type":"result","result":"done"}'`, nil)
	handle, _, _ := devTurn(t, ctx, adapter, devRun(t, adapter, "large"))
	got := devOutputs(t, ctx, adapter, handle, wherry.Limits{Files: 4, FileBytes: 1, TotalBytes: 1, Stream: []string{"big.bin"}, StreamBytes: 2 << 30})
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
	if !measure {
		return
	}
	h, _ := decodeHandle(handle)
	var peak bytes.Buffer
	if err := adapter.exec.exec(ctx, h.Pod, "outputs", []string{"cat", "/sys/fs/cgroup/memory.peak"}, nil, &peak); err != nil {
		t.Fatal(err)
	}
	bytesPeak, err := strconv.ParseInt(strings.TrimSpace(peak.String()), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("holder memory peak %d bytes", bytesPeak)
	if bytesPeak > 32<<20 {
		t.Fatalf("holder memory peak %d bytes; want at most half its 64Mi limit", bytesPeak)
	}
}

func TestLargeStreamedOutputOnKubernetes(t *testing.T) { streamLarge(t, false) }

func TestHolderMemoryOnKubernetes(t *testing.T) { streamLarge(t, true) }

func TestFollowPastLogRotationOnKubernetes(t *testing.T) {
	adapter, ctx := devAdapter(t, `yes "$(printf '%01023d' 0)" | head -n 61440; echo '{"type":"result","result":"done"}'`, nil)
	handle, lines, terminal := devTurn(t, ctx, adapter, devRun(t, adapter, "rotation"))
	if len(lines) != 61441 || string(lines[len(lines)-1].Data) != `{"type":"result","result":"done"}` || terminal.ExitCode != 0 {
		t.Fatalf("%d lines, %+v; want every line and the result", len(lines), terminal)
	}
	// 40 MiB lies in the rotated part and past the end of the current file.
	if _, _, err := followAll(t, adapter, handle, 40<<20); err == nil || err.Error() != "the agent's log is shorter than the offset" {
		t.Fatalf("follow from a rotated offset: %v", err)
	}
}

func TestDeletingPodOnKubernetes(t *testing.T) {
	adapter, ctx := devAdapter(t, `trap '' TERM; echo started; while :; do sleep 1; done`, nil)
	run := devRun(t, adapter, "deleting")
	run.Credential = nil
	outcome, handle, err := adapter.Dispatch(ctx, run)
	if err != nil || outcome != wherry.OutcomeCreated {
		t.Fatalf("dispatch = %v, %v", outcome, err)
	}
	name := adapter.jobName(run.ID, run.Turn)
	if err := adapter.pods().Delete(ctx, name, metav1.DeleteOptions{GracePeriodSeconds: new(int64(600))}); err != nil {
		t.Fatal(err)
	}
	if job, err := adapter.Lookup(ctx, run.ID, run.Turn); err != nil || job.State != wherry.JobEnded {
		t.Fatalf("lookup = %+v, %v; want ended", job, err)
	}
	followCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if terminal, err := adapter.Follow(followCtx, handle, 0, make(chan wherry.Line, 64)); err != nil || terminal.ExitCode != -1 {
		t.Fatalf("follow = %+v, %v; want a terminal without logs", terminal, err)
	}
	if err := adapter.Collect(ctx, handle, limits, make(chan wherry.Output, 4)); err == nil || err.Error() != "the pod is being deleted" {
		t.Fatalf("collect = %v", err)
	}
	began := time.Now()
	if err := adapter.Purge(ctx, run.ID, run.Turn); err != nil || time.Since(began) > 5*time.Second {
		t.Fatalf("purge = %v after %v", err, time.Since(began))
	}
}

func TestPodAdmittedUnderRestricted(t *testing.T) {
	adapter, ctx := devAdapter(t, `echo '{"type":"result","result":"done"}'`, nil)
	run := devRun(t, adapter, "admitted")
	outcome, handle, err := adapter.Dispatch(ctx, run)
	if err != nil || outcome != wherry.OutcomeCreated {
		t.Fatalf("dispatch = %v, %v", outcome, err)
	}
	admitted, err := adapter.pods().Get(ctx, handle.Name, getOptions)
	if err != nil {
		t.Fatal(err)
	}
	built := adapter.buildPod(run, handle.Name)
	if fmt.Sprint(admitted.Spec.SecurityContext.SeccompProfile) != fmt.Sprint(built.Spec.SecurityContext.SeccompProfile) {
		t.Fatalf("seccomp %v; built %v", admitted.Spec.SecurityContext.SeccompProfile, built.Spec.SecurityContext.SeccompProfile)
	}
	for _, c := range slices.Concat(built.Spec.InitContainers, built.Spec.Containers) {
		got := container(admitted, c.Name).SecurityContext
		if fmt.Sprint(got.Capabilities.Drop) != fmt.Sprint(c.SecurityContext.Capabilities.Drop) || *got.AllowPrivilegeEscalation != *c.SecurityContext.AllowPrivilegeEscalation {
			t.Fatalf("container %s admitted with %+v; built %+v", c.Name, got, c.SecurityContext)
		}
	}
}
