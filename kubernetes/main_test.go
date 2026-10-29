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

	"github.com/benemon/wherry/pluginsdk"
	"github.com/benemon/wherry/pluginsdk/pluginsdktest"
	"github.com/hashicorp/go-hclog"
	"go.yaml.in/yaml/v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func settings(t *testing.T, name string, config map[string]any) pluginsdk.Settings {
	t.Helper()
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	return pluginsdk.Settings{Name: name, Pool: testPool, Agent: "claude", Config: encoded, Credential: testToken}
}

func podConfig(address, caFile, runsDir, image string, command ...string) map[string]any {
	return map[string]any{
		"address": address, "ca_file": caFile, "namespace": testNamespace, "runs_claim": "wherry-runs", "runs_dir": runsDir,
		"runtime": map[string]any{"image": image, "command": command},
	}
}

func TestFactoryDecodesStrictly(t *testing.T) {
	api := newFakeAPI(t)
	valid := podConfig(api.server.URL, api.caFile, "/var/lib/wherry/runs", testImage)
	withCredentialEnv := podConfig(api.server.URL, api.caFile, "/var/lib/wherry/runs", testImage)
	withCredentialEnv["credential_env"] = "MODEL_API_KEY"
	if _, err := newProfile(settings(t, "lab", withCredentialEnv), hclog.NewNullLogger()); err == nil || !strings.Contains(err.Error(), "credential_env") {
		t.Fatalf("got %v, want credential_env refused as an unknown member", err)
	}
	unknown := podConfig(api.server.URL, api.caFile, "/var/lib/wherry/runs", testImage)
	unknown["kubeconfig"] = "/root/.kube/config"
	if _, err := newProfile(settings(t, "lab", unknown), hclog.NewNullLogger()); err == nil {
		t.Fatal("accepted profile config with an unknown member")
	}
	helperImage := podConfig(api.server.URL, api.caFile, "/var/lib/wherry/runs", testImage)
	helperImage["helper_image"] = "registry.example.com/wherry-receiver:0.1.0"
	if _, err := newProfile(settings(t, "lab", helperImage), hclog.NewNullLogger()); err == nil || !strings.Contains(err.Error(), "helper_image") {
		t.Fatalf("got %v, want helper_image refused as an unknown member", err)
	}
	unknown = podConfig(api.server.URL, api.caFile, "/var/lib/wherry/runs", testImage)
	unknown["runtime"].(map[string]any)["driver"] = "docker"
	if _, err := newProfile(settings(t, "lab", unknown), hclog.NewNullLogger()); err == nil {
		t.Fatal("accepted a runtime with an unknown member")
	}
	if _, err := newProfile(settings(t, "lab", valid), hclog.NewNullLogger()); err != nil {
		t.Fatal(err)
	}
}

// TestReadmeReceiverConfigures configures each plugin profile of the README's receiver configuration as the receiver
// does, with the plugin's config merged into the profile's, the credential key taken out, and the platform credential's
// value passed in Configure.
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
		Capacity int64 `yaml:"capacity"`
		Plugins  map[string]struct {
			Labels   map[string]string `yaml:"labels"`
			Config   map[string]any    `yaml:"config"`
			Profiles []struct {
				Name   string            `yaml:"name"`
				Agent  string            `yaml:"agent"`
				Labels map[string]string `yaml:"labels"`
				Config map[string]any    `yaml:"config"`
			} `yaml:"profiles"`
		} `yaml:"plugins"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(example))
	decoder.KnownFields(true)
	if err := decoder.Decode(&receiver); err != nil {
		t.Fatal(err)
	}
	plugin, ok := receiver.Plugins["kubernetes"]
	if len(receiver.Plugins) != 1 || !ok || len(plugin.Profiles) < 2 {
		t.Fatal("want one kubernetes plugin and at least two profiles")
	}
	// ca_file names a path on the receiver; the client reads the CA when the profile is built.
	plugin.Config["ca_file"] = newFakeAPI(t).caFile
	for _, profile := range plugin.Profiles {
		config := merge(plugin.Config, profile.Config)
		// The README says the profiles name kubernetes-token. The receiver resolves the credential's name and sends
		// the plugin the config without it.
		if name := config["credential"]; name != "kubernetes-token" {
			t.Fatalf("profile %s: credential %v, want kubernetes-token", profile.Name, name)
		}
		delete(config, "credential")
		encoded, err := json.Marshal(config)
		if err != nil {
			t.Fatal(err)
		}
		s := pluginsdk.Settings{Name: profile.Name, Pool: testPool, Agent: profile.Agent, Config: encoded, Credential: testToken}
		if _, err := newProfile(s, hclog.NewNullLogger()); err != nil {
			t.Fatalf("profile %s: %v", profile.Name, err)
		}
	}
}

// merge returns base with over merged onto it as wherry merges a plugin profile's config onto its plugin's: maps merge
// recursively, and over wins on every other key.
func merge(base, over map[string]any) map[string]any {
	merged := make(map[string]any, len(base)+len(over))
	for key, value := range base {
		merged[key] = value
	}
	for key, value := range over {
		inherited, inheritedMap := merged[key].(map[string]any)
		overriding, overridingMap := value.(map[string]any)
		if inheritedMap && overridingMap {
			merged[key] = merge(inherited, overriding)
			continue
		}
		merged[key] = value
	}
	return merged
}

func TestFactoryGivesTheProfileServesLogger(t *testing.T) {
	api := newFakeAPI(t)
	logger := hclog.New(&hclog.LoggerOptions{Name: "served"})
	built, err := newProfile(settings(t, "lab", podConfig(api.server.URL, api.caFile, "/var/lib/wherry/runs", testImage)), logger)
	if err != nil {
		t.Fatal(err)
	}
	if built.(*Kubernetes).logger != logger {
		t.Fatal("the profile logs elsewhere than the logger Serve gave the factory")
	}
}

func TestFactoryTokenFromCredentialElseServiceAccount(t *testing.T) {
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
	inCluster := settings(t, "in-cluster", podConfig("", "", t.TempDir(), testImage))
	inCluster.Credential = ""
	withCredential := settings(t, "credential", podConfig("", "", t.TempDir(), testImage))
	withCredential.Credential = "platform-credential"
	for _, s := range []pluginsdk.Settings{inCluster, withCredential} {
		a, err := newProfile(s, hclog.NewNullLogger())
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
	if len(tokens) != 2 || tokens[0] != "Bearer service-account-token" || tokens[1] != "Bearer platform-credential" {
		t.Fatalf("tokens %v; want the service account's without a platform credential, else the credential", tokens)
	}
	remote := settings(t, "remote", podConfig(api.server.URL, api.caFile, "/var/lib/wherry/runs", testImage))
	remote.Credential = ""
	if _, err := newProfile(remote, hclog.NewNullLogger()); err == nil || err.Error() != "address needs a platform credential" {
		t.Fatalf("address without a platform credential: %v", err)
	}
}

func conformanceSettings(t *testing.T) (pluginsdk.Settings, pluginsdk.Settings) {
	t.Helper()
	dev := devConfig(t)
	completes := settings(t, "completes", podConfig(dev.Address, dev.CAFile, dev.RunsDir, "registry.access.redhat.com/ubi9/ubi-minimal",
		"/bin/sh", "-c", `sleep 10; test "$CONFORMANCE_TOKEN" = conformance-env-value && test "$(cat "$WHERRY_CREDENTIALS_DIR/conformance-key")" = conformance-file-value || exit 1; echo '{"type":"result","result":"done"}'`))
	neverStarts := settings(t, "never-starts", podConfig(dev.Address, dev.CAFile, dev.RunsDir, "registry.invalid/wherry-conformance:0"))
	for _, s := range []*pluginsdk.Settings{&completes, &neverStarts} {
		var config map[string]any
		_ = json.Unmarshal(s.Config, &config)
		config["namespace"], config["runs_claim"] = dev.Namespace, dev.RunsClaim
		if user := os.Getenv("WHERRY_KUBERNETES_DEV_USER"); user != "" {
			config["runtime"].(map[string]any)["user"] = user
		}
		s.Config, _ = json.Marshal(config)
		s.Credential = devToken(t)
	}
	return completes, neverStarts
}

func TestConformance(t *testing.T) {
	completes, neverStarts := conformanceSettings(t)
	pluginsdktest.Run(t, newProfile, completes, neverStarts, inlineLimit)
}

// TestConformanceOverTheLink runs the suite against the built plugin, through the receiver's side of the link.
func TestConformanceOverTheLink(t *testing.T) {
	completes, neverStarts := conformanceSettings(t)
	binary := filepath.Join(t.TempDir(), "wherry-plugin-kubernetes")
	if output, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	plugin, err := pluginsdk.Launch(func() (string, error) { return binary, nil }, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer plugin.Close()
	pluginsdktest.Run(t, func(settings pluginsdk.Settings, _ hclog.Logger) (pluginsdk.Plugin, error) {
		return plugin.Configure(settings)
	}, completes, neverStarts, inlineLimit)
}

// admitted wraps a plugin profile and records, for each pod it dispatches, whether the pod was admitted as restricted.
type admitted struct {
	pluginsdk.Plugin
	t      *testing.T
	checks chan error
}

func (a admitted) Dispatch(ctx context.Context, run pluginsdk.Run) (pluginsdk.Outcome, pluginsdk.Handle, error) {
	outcome, handle, err := a.Plugin.Dispatch(ctx, run)
	if built, ok := a.Plugin.(*Kubernetes); ok && err == nil {
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
	pluginsdktest.Run(t, func(settings pluginsdk.Settings, logger hclog.Logger) (pluginsdk.Plugin, error) {
		built, err := newProfile(settings, logger)
		if err != nil {
			return nil, err
		}
		return admitted{Plugin: built, t: t, checks: checks}, nil
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
	profile := api.profile(t)
	began := time.Now()
	for range 200 {
		if _, err := profile.Ready(context.Background()); err != nil {
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
	built, err := newProfile(settings(t, "lab", podConfig(api.server.URL, api.caFile, "/var/lib/wherry/runs", testImage)), hclog.NewNullLogger())
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
		t.Fatalf("the plugin wrote to stderr: %q", written)
	}
}
