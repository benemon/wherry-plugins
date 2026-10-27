package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	wherry "github.com/benemon/wherry/adapter"
	"github.com/benemon/wherry/adapter/adaptertest"
	"github.com/hashicorp/go-hclog"
	"go.yaml.in/yaml/v3"
)

func settings(t *testing.T, name string, config map[string]any) wherry.Settings {
	t.Helper()
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	return wherry.Settings{Name: name, Pool: testPool, Agent: "claude", Config: encoded}
}

func execConfig(address string, command string, args ...any) map[string]any {
	return map[string]any{
		"address": address, "namespace": "default", "node_pool": "default", "datacenters": []any{"dc1"},
		"runtime": map[string]any{"driver": "raw_exec", "config": map[string]any{"command": command, "args": args}},
	}
}

func TestFactoryDecodesStrictly(t *testing.T) {
	retired := execConfig("http://127.0.0.1:1", "/bin/true")
	retired["credential_env"] = "MODEL_API_KEY"
	if _, err := newAdapter(settings(t, "lab", retired), hclog.NewNullLogger()); err == nil || !strings.Contains(err.Error(), "unknown") || !strings.Contains(err.Error(), "credential_env") {
		t.Fatalf("got %v, want credential_env refused as an unknown member", err)
	}
	unknown := execConfig("http://127.0.0.1:1", "/bin/true")
	unknown["token"] = "x"
	if _, err := newAdapter(settings(t, "lab", unknown), hclog.NewNullLogger()); err == nil {
		t.Fatal("accepted adapter config with an unknown member")
	}
	if _, err := newAdapter(settings(t, "lab", execConfig("http://127.0.0.1:1", "/bin/true")), hclog.NewNullLogger()); err != nil {
		t.Fatal(err)
	}
}

// TestReadmeReceiverConfigures configures each plugin profile of the README's receiver configuration as the receiver
// does, with the plugin's config merged under the profile's and the credential key resolved and removed.
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
	plugin, ok := receiver.Plugins["nomad"]
	if len(receiver.Plugins) != 1 || !ok || len(plugin.Profiles) < 2 {
		t.Fatal("want one nomad plugin and at least two profiles")
	}
	// ca_file names a path on the receiver; the client reads the CA when the adapter is built.
	server := httptest.NewTLSServer(http.NotFoundHandler())
	defer server.Close()
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	if plugin.Config == nil {
		plugin.Config = map[string]any{}
	}
	plugin.Config["ca_file"] = caFile
	for _, profile := range plugin.Profiles {
		config := merge(plugin.Config, profile.Config)
		s := wherry.Settings{Name: profile.Name, Pool: testPool, Agent: profile.Agent}
		// The receiver sends the plugin the credential's value and never its name.
		if name, ok := config["credential"]; ok {
			if _, ok := name.(string); !ok {
				t.Fatalf("profile %s: credential %v, want a name", profile.Name, name)
			}
			delete(config, "credential")
			s.Credential = "readme-nomad-token"
		}
		encoded, err := json.Marshal(config)
		if err != nil {
			t.Fatal(err)
		}
		s.Config = encoded
		if _, err := newAdapter(s, hclog.NewNullLogger()); err != nil {
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

func TestFactoryGivesTheAdapterServesLogger(t *testing.T) {
	logger := hclog.New(&hclog.LoggerOptions{Name: "served"})
	built, err := newAdapter(settings(t, "lab", execConfig("http://127.0.0.1:1", "/bin/true")), logger)
	if err != nil {
		t.Fatal(err)
	}
	if built.(*Adapter).logger != logger {
		t.Fatal("adapter logs elsewhere than the logger Serve gave the factory")
	}
}

func TestFactoryTokenFromCredentialElseNomadToken(t *testing.T) {
	t.Setenv("NOMAD_TOKEN", "workload-identity-token")
	tokens := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		tokens <- request.Header.Get("X-Nomad-Token")
		_, _ = response.Write([]byte("[]"))
	}))
	defer server.Close()
	withCredential := settings(t, "credential", execConfig(server.URL, "/bin/true"))
	withCredential.Credential = "platform-credential"
	for _, s := range []wherry.Settings{settings(t, "environment", execConfig(server.URL, "/bin/true")), withCredential} {
		a, err := newAdapter(s, hclog.NewNullLogger())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.Ready(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := []string{<-tokens, <-tokens}; got[0] != "workload-identity-token" || got[1] != "platform-credential" {
		t.Fatalf("tokens %v; want the environment's without a platform credential, else the credential", got)
	}
}

func conformanceSettings(t *testing.T) (wherry.Settings, wherry.Settings) {
	t.Helper()
	address := os.Getenv("WHERRY_NOMAD_DEV_ADDR")
	if address == "" {
		t.Skip("WHERRY_NOMAD_DEV_ADDR not set")
	}
	// The completing job exits 0 only with adaptertest.Credentials delivered at their targets.
	return settings(t, "completes", execConfig(address, "/bin/sh", "-c", `sleep 10; test "$CONFORMANCE_TOKEN" = conformance-env-value && test "$(cat "$WHERRY_CREDENTIALS_DIR/conformance-key")" = conformance-file-value || exit 1; echo '{"type":"result","result":"done"}'`)),
		settings(t, "never-starts", execConfig(address, "/nonexistent/wherry-conformance"))
}

func TestConformance(t *testing.T) {
	completes, neverStarts := conformanceSettings(t)
	adaptertest.Run(t, newAdapter, completes, neverStarts, inlineLimit)
}

// TestConformanceOverTheLink runs the suite against the built plugin, through the receiver's side of the link.
func TestConformanceOverTheLink(t *testing.T) {
	completes, neverStarts := conformanceSettings(t)
	binary := filepath.Join(t.TempDir(), "wherry-plugin-nomad")
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
