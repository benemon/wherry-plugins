package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wherry "github.com/benemon/wherry/adapter"
	"github.com/benemon/wherry/adapter/adaptertest"
	"github.com/hashicorp/go-hclog"
	"go.yaml.in/yaml/v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func settings(t *testing.T, name string, config map[string]any) wherry.Settings {
	t.Helper()
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	return wherry.Settings{Name: name, Pool: testPool, Agent: "claude", Config: encoded, Platform: testToken}
}

func podConfig(address, caFile, helperImage, image string, command ...string) map[string]any {
	return map[string]any{
		"address": address, "ca_file": caFile, "namespace": testNamespace, "helper_image": helperImage,
		"runtime": map[string]any{"image": image, "command": command},
	}
}

func TestFactoryDecodesStrictly(t *testing.T) {
	api := newFakeAPI(t)
	valid := podConfig(api.server.URL, api.caFile, testHelperImage, testImage)
	withCredentialEnv := podConfig(api.server.URL, api.caFile, testHelperImage, testImage)
	withCredentialEnv["credential_env"] = "MODEL_API_KEY"
	if _, err := newAdapter(settings(t, "lab", withCredentialEnv), hclog.NewNullLogger()); err == nil || !strings.Contains(err.Error(), "credential_env") {
		t.Fatalf("got %v, want credential_env refused as an unknown member", err)
	}
	unknown := podConfig(api.server.URL, api.caFile, testHelperImage, testImage)
	unknown["kubeconfig"] = "/root/.kube/config"
	if _, err := newAdapter(settings(t, "lab", unknown), hclog.NewNullLogger()); err == nil {
		t.Fatal("accepted adapter config with an unknown member")
	}
	unknown = podConfig(api.server.URL, api.caFile, testHelperImage, testImage)
	unknown["runtime"].(map[string]any)["driver"] = "docker"
	if _, err := newAdapter(settings(t, "lab", unknown), hclog.NewNullLogger()); err == nil {
		t.Fatal("accepted a runtime with an unknown member")
	}
	if _, err := newAdapter(settings(t, "lab", valid), hclog.NewNullLogger()); err != nil {
		t.Fatal(err)
	}
}

// TestReadmeReceiverConfigures configures each adapter of the README's receiver configuration as the receiver does,
// with the platform's config keys merged into the adapter's and a platform credential in the bundle.
func TestReadmeReceiverConfigures(t *testing.T) {
	example, err := os.ReadFile("testdata/receiver.yaml")
	if err != nil {
		t.Fatal(err)
	}
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(readme, example) {
		t.Fatal("README.md does not hold testdata/receiver.yaml")
	}
	var receiver struct {
		Capacity  int64 `yaml:"capacity"`
		Platforms []struct {
			Name   string         `yaml:"name"`
			Type   string         `yaml:"type"`
			Config map[string]any `yaml:"config"`
		} `yaml:"platforms"`
		Adapters []struct {
			Name      string            `yaml:"name"`
			Platform  string            `yaml:"platform"`
			Agent     string            `yaml:"agent"`
			Output    string            `yaml:"output"`
			ReplyFile string            `yaml:"reply_file"`
			Labels    map[string]string `yaml:"labels"`
			Config    map[string]any    `yaml:"config"`
		} `yaml:"adapters"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(example))
	decoder.KnownFields(true)
	if err := decoder.Decode(&receiver); err != nil {
		t.Fatal(err)
	}
	if len(receiver.Platforms) != 1 || receiver.Platforms[0].Type != "kubernetes" || len(receiver.Adapters) < 2 {
		t.Fatal("want one kubernetes platform and at least two adapters")
	}
	platform := receiver.Platforms[0]
	// ca_file names a path on the receiver; the client reads the CA when the adapter is built.
	platform.Config["ca_file"] = newFakeAPI(t).caFile
	for _, adapter := range receiver.Adapters {
		if adapter.Platform != platform.Name {
			t.Fatalf("adapter %s: platform %q, want %q", adapter.Name, adapter.Platform, platform.Name)
		}
		for key, value := range platform.Config {
			if _, ok := adapter.Config[key]; ok {
				t.Fatalf("adapter %s: config key %q is also set by the platform", adapter.Name, key)
			}
			adapter.Config[key] = value
		}
		encoded, err := json.Marshal(adapter.Config)
		if err != nil {
			t.Fatal(err)
		}
		s := wherry.Settings{Name: adapter.Name, Pool: testPool, Agent: adapter.Agent, Config: encoded, Platform: testToken}
		if _, err := newAdapter(s, hclog.NewNullLogger()); err != nil {
			t.Fatalf("adapter %s: %v", adapter.Name, err)
		}
	}
}

func TestFactoryGivesTheAdapterServesLogger(t *testing.T) {
	api := newFakeAPI(t)
	logger := hclog.New(&hclog.LoggerOptions{Name: "served"})
	built, err := newAdapter(settings(t, "lab", podConfig(api.server.URL, api.caFile, testHelperImage, testImage)), logger)
	if err != nil {
		t.Fatal(err)
	}
	if built.(*Adapter).logger != logger {
		t.Fatal("adapter logs elsewhere than the logger Serve gave the factory")
	}
}

func TestFactoryTokenFromBundleElseServiceAccount(t *testing.T) {
	api := newFakeAPI(t)
	accountDir := t.TempDir()
	ca, err := os.ReadFile(api.caFile)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"token": []byte("service-account-token"), "ca.crt": ca} {
		if err := os.WriteFile(filepath.Join(accountDir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	defer func(dir string) { serviceAccountDir = dir }(serviceAccountDir)
	serviceAccountDir = accountDir
	host, port, _ := strings.Cut(strings.TrimPrefix(api.server.URL, "https://"), ":")
	t.Setenv("KUBERNETES_SERVICE_HOST", host)
	t.Setenv("KUBERNETES_SERVICE_PORT", port)
	inCluster := settings(t, "in-cluster", podConfig("", "", testHelperImage, testImage))
	inCluster.Platform = ""
	withBundle := settings(t, "bundle", podConfig("", "", testHelperImage, testImage))
	withBundle.Platform = "bundle-token"
	for _, s := range []wherry.Settings{inCluster, withBundle} {
		a, err := newAdapter(s, hclog.NewNullLogger())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.Ready(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	var tokens []string
	for _, r := range api.recorded() {
		tokens = append(tokens, r.header.Get("Authorization"))
	}
	if len(tokens) != 2 || tokens[0] != "Bearer service-account-token" || tokens[1] != "Bearer bundle-token" {
		t.Fatalf("tokens %v; want the service account's without a bundle token, else the bundle's", tokens)
	}
	remote := settings(t, "remote", podConfig(api.server.URL, api.caFile, testHelperImage, testImage))
	remote.Platform = ""
	if _, err := newAdapter(remote, hclog.NewNullLogger()); err == nil || err.Error() != "address needs a platform credential" {
		t.Fatalf("address without a platform credential: %v", err)
	}
}

func conformanceSettings(t *testing.T) (wherry.Settings, wherry.Settings) {
	t.Helper()
	dev := devConfig(t)
	completes := settings(t, "completes", podConfig(dev.Address, dev.CAFile, dev.HelperImage, "registry.access.redhat.com/ubi9/ubi-minimal",
		"/bin/sh", "-c", `sleep 10; test "$CONFORMANCE_TOKEN" = conformance-env-value && test "$(cat "$WHERRY_CREDENTIALS_DIR/conformance-key")" = conformance-file-value || exit 1; echo '{"type":"result","result":"done"}'`))
	neverStarts := settings(t, "never-starts", podConfig(dev.Address, dev.CAFile, dev.HelperImage, "registry.invalid/wherry-conformance:0"))
	for _, s := range []*wherry.Settings{&completes, &neverStarts} {
		var config map[string]any
		_ = json.Unmarshal(s.Config, &config)
		config["namespace"] = dev.Namespace
		if user := os.Getenv("WHERRY_KUBERNETES_DEV_USER"); user != "" {
			config["runtime"].(map[string]any)["user"] = user
		}
		s.Config, _ = json.Marshal(config)
		s.Platform = devToken(t)
	}
	return completes, neverStarts
}

func TestConformance(t *testing.T) {
	completes, neverStarts := conformanceSettings(t)
	adaptertest.Run(t, newAdapter, completes, neverStarts, inlineLimit)
}

// TestConformanceOverTheLink runs the suite against the built plugin, through the receiver's side of the link.
func TestConformanceOverTheLink(t *testing.T) {
	completes, neverStarts := conformanceSettings(t)
	binary := filepath.Join(t.TempDir(), "wherry-plugin-kubernetes")
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
	}, completes, neverStarts, inlineLimit)
}

// admitted wraps an adapter and records, for each pod it dispatches, whether the pod was admitted as restricted.
type admitted struct {
	wherry.Adapter
	t      *testing.T
	checks chan error
}

func (a admitted) Dispatch(ctx context.Context, run wherry.Run) (wherry.Outcome, wherry.Handle, error) {
	outcome, handle, err := a.Adapter.Dispatch(ctx, run)
	if built, ok := a.Adapter.(*Adapter); ok && err == nil {
		pod, podErr := built.pods().Get(ctx, handle.Name, getOptions)
		namespace, namespaceErr := built.core.Namespaces().Get(ctx, built.namespace, getOptions)
		if readErr := errors.Join(podErr, namespaceErr); readErr != nil {
			a.t.Errorf("read dispatched pod and its namespace: %v", readErr)
		} else {
			a.checks <- restricted(namespace, pod)
		}
	}
	return outcome, handle, err
}

// restricted checks an admitted pod against OpenShift's restricted-v2 SCC where OpenShift annotated the pod with
// its SCC, and against the namespace's Pod Security enforce label elsewhere.
func restricted(namespace *corev1.Namespace, pod *corev1.Pod) error {
	if scc, ok := pod.Annotations["openshift.io/scc"]; ok {
		if scc != "restricted-v2" {
			return fmt.Errorf("a pod was admitted under SCC %q; want restricted-v2", scc)
		}
		return nil
	}
	if level := namespace.Labels["pod-security.kubernetes.io/enforce"]; level != "restricted" {
		return fmt.Errorf("namespace %s enforces Pod Security level %q; want restricted", namespace.Name, level)
	}
	return nil
}

func TestRestrictedByPlatform(t *testing.T) {
	enforcing := func(level string) *corev1.Namespace {
		namespace := &corev1.Namespace{}
		namespace.Name = testNamespace
		if level != "" {
			namespace.Labels = map[string]string{"pod-security.kubernetes.io/enforce": level}
		}
		return namespace
	}
	underSCC := func(scc string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"openshift.io/scc": scc}}}
	}
	for name, c := range map[string]struct {
		namespace *corev1.Namespace
		pod       *corev1.Pod
		admitted  bool
	}{
		"openshift restricted-v2":           {enforcing("restricted"), underSCC("restricted-v2"), true},
		"openshift restricted-v2 unlabeled": {enforcing(""), underSCC("restricted-v2"), true},
		"openshift anyuid":                  {enforcing("restricted"), underSCC("anyuid"), false},
		"kubernetes restricted":             {enforcing("restricted"), &corev1.Pod{}, true},
		"kubernetes baseline":               {enforcing("baseline"), &corev1.Pod{}, false},
		"kubernetes unlabeled":              {enforcing(""), &corev1.Pod{}, false},
	} {
		if err := restricted(c.namespace, c.pod); (err == nil) != c.admitted {
			t.Errorf("%s: %v; want admitted %v", name, err, c.admitted)
		}
	}
}

func TestRoleSuffices(t *testing.T) {
	completes, neverStarts := conformanceSettings(t)
	checks := make(chan error, 64)
	adaptertest.Run(t, func(settings wherry.Settings, logger hclog.Logger) (wherry.Adapter, error) {
		built, err := newAdapter(settings, logger)
		if err != nil {
			return nil, err
		}
		return admitted{Adapter: built, t: t, checks: checks}, nil
	}, completes, neverStarts, inlineLimit)
	close(checks)
	count := 0
	for err := range checks {
		if count++; err != nil {
			t.Fatal(err)
		}
	}
	if count == 0 {
		t.Fatal("no pod was dispatched")
	}
}

func TestClientIsNotRateLimited(t *testing.T) {
	api := newFakeAPI(t)
	client, err := newClient(testConfig(api.server.URL, api.caFile), testToken)
	if err != nil {
		t.Fatal(err)
	}
	if client.QPS != 50 || client.Burst != 100 {
		t.Fatalf("QPS %v, burst %d; want 50 and 100", client.QPS, client.Burst)
	}
	adapter, _ := api.adapter(t)
	began := time.Now()
	for range 200 {
		if _, err := adapter.Ready(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	// The limiter admits the first 100 at once and the rest at 50 a second; client-go's default would take 38 seconds.
	if elapsed := time.Since(began); elapsed > 5*time.Second {
		t.Fatalf("200 reads took %v; want under 5 seconds", elapsed)
	}
}

func TestClientWritesNothingToStderr(t *testing.T) {
	api := newFakeAPI(t)
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := os.Stderr
	os.Stderr = write
	defer func() { os.Stderr = stderr }()
	built, err := newAdapter(settings(t, "lab", podConfig(api.server.URL, api.caFile, testHelperImage, testImage)), hclog.NewNullLogger())
	if err != nil {
		t.Fatal(err)
	}
	requests := 0
	api.set(func() {
		// Every other read is cut short, a failure client-go logs through klog; the rest carry a warning, which its
		// default handler prints.
		api.intercept = func(response http.ResponseWriter, _ *http.Request) bool {
			if requests++; requests%2 == 0 {
				response.Header().Set("Warning", `299 - "would violate PodSecurity"`)
				return false
			}
			response.Header().Set("Content-Type", "application/json")
			response.Header().Set("Content-Length", "100")
			response.WriteHeader(http.StatusOK)
			_, _ = response.Write([]byte(`{"kind":`))
			if conn, _, err := response.(http.Hijacker).Hijack(); err == nil {
				conn.Close()
			}
			return true
		}
	})
	for range 20 {
		_, _ = built.Ready(context.Background())
	}
	os.Stderr = stderr
	write.Close()
	written, _ := io.ReadAll(read)
	if len(written) != 0 {
		t.Fatalf("the adapter wrote to stderr: %q", written)
	}
}
