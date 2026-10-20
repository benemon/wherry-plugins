package main

import (
	"context"
	"encoding/json"
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
	"github.com/hashicorp/nomad/api"
)

func newFactory() *factory {
	return &factory{clients: map[clientKey]*api.Client{}}
}

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
	f := newFactory()
	withCredential := settings(t, "lab", execConfig("http://127.0.0.1:1", "/bin/true"))
	withCredential.Credential = "model"
	if _, err := f.adapter(withCredential); err == nil || !strings.Contains(err.Error(), "credential_env") {
		t.Fatalf("got %v, want credential_env required", err)
	}
	unknown := execConfig("http://127.0.0.1:1", "/bin/true")
	unknown["token"] = "x"
	if _, err := f.adapter(settings(t, "lab", unknown)); err == nil {
		t.Fatal("accepted adapter config with an unknown member")
	}
	if _, err := f.adapter(settings(t, "lab", execConfig("http://127.0.0.1:1", "/bin/true"))); err != nil {
		t.Fatal(err)
	}
}

func TestFactorySharesClients(t *testing.T) {
	f := newFactory()
	first, err := f.adapter(settings(t, "exec", execConfig("http://127.0.0.1:1", "/bin/true")))
	if err != nil {
		t.Fatal(err)
	}
	same, err := f.adapter(settings(t, "podman", execConfig("http://127.0.0.1:1", "/bin/false")))
	if err != nil {
		t.Fatal(err)
	}
	if first.(*Adapter).client != same.(*Adapter).client {
		t.Fatal("adapters with the same address, CA, namespace and token got separate clients")
	}
	other := settings(t, "other", execConfig("http://127.0.0.1:1", "/bin/true"))
	other.Platform = "another-token"
	elsewhere := execConfig("http://127.0.0.1:1", "/bin/true")
	elsewhere["namespace"] = "wherry"
	for _, s := range []wherry.Settings{other, settings(t, "elsewhere", elsewhere)} {
		distinct, err := f.adapter(s)
		if err != nil {
			t.Fatal(err)
		}
		if distinct.(*Adapter).client == first.(*Adapter).client {
			t.Fatalf("adapter %s shares a client across different connection settings", s.Name)
		}
	}
}

func TestFactoryTokenFallsBackToNomadToken(t *testing.T) {
	t.Setenv("NOMAD_TOKEN", "workload-identity-token")
	tokens := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		tokens <- request.Header.Get("X-Nomad-Token")
		_, _ = response.Write([]byte("[]"))
	}))
	defer server.Close()
	f := newFactory()
	withBundle := settings(t, "bundle", execConfig(server.URL, "/bin/true"))
	withBundle.Platform = "bundle-token"
	for _, s := range []wherry.Settings{settings(t, "environment", execConfig(server.URL, "/bin/true")), withBundle} {
		a, err := f.adapter(s)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.Ready(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := []string{<-tokens, <-tokens}; got[0] != "workload-identity-token" || got[1] != "bundle-token" {
		t.Fatalf("tokens %v; want the environment's without a bundle token, else the bundle's", got)
	}
}

func conformanceSettings(t *testing.T) (wherry.Settings, wherry.Settings) {
	t.Helper()
	address := os.Getenv("WHERRY_NOMAD_DEV_ADDR")
	if address == "" {
		t.Skip("WHERRY_NOMAD_DEV_ADDR not set")
	}
	return settings(t, "completes", execConfig(address, "/bin/sh", "-c", `sleep 10; echo '{"type":"result","result":"done"}'`)),
		settings(t, "never-starts", execConfig(address, "/nonexistent/wherry-conformance"))
}

func TestConformance(t *testing.T) {
	completes, neverStarts := conformanceSettings(t)
	adaptertest.Run(t, newFactory().adapter, completes, neverStarts)
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
	adaptertest.Run(t, plugin.Configure, completes, neverStarts)
}
