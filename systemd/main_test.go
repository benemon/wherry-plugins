//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	wherry "github.com/benemon/wherry/adapter"
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

func unitConfig(command ...string) map[string]any {
	return map[string]any{"runtime": map[string]any{"command": command}}
}

func TestFactoryDecodesStrictly(t *testing.T) {
	newHost(t)
	unknown := unitConfig("/bin/true")
	unknown["inaccessible_paths"] = []string{"-/opt/nomad"}
	if _, err := newAdapter(settings(t, "lab", unknown), hclog.NewNullLogger()); err == nil {
		t.Fatal("accepted adapter config with an unknown member")
	}
	unknown = unitConfig("/bin/true")
	unknown["runtime"].(map[string]any)["image"] = "registry.example.com/agent:1.0"
	if _, err := newAdapter(settings(t, "lab", unknown), hclog.NewNullLogger()); err == nil {
		t.Fatal("accepted a runtime with an unknown member")
	}
	unknown = unitConfig("/bin/true")
	unknown["runtime"].(map[string]any)["resources"] = map[string]any{"cpu": 1000}
	if _, err := newAdapter(settings(t, "lab", unknown), hclog.NewNullLogger()); err == nil {
		t.Fatal("accepted resources with an unknown member")
	}
	if _, err := newAdapter(settings(t, "lab", unitConfig("/bin/true")), hclog.NewNullLogger()); err != nil {
		t.Fatal(err)
	}
}

// The core delivers every credential at its target, so the plugin takes no credential setting.
func TestFactoryRefusesCredentialEnv(t *testing.T) {
	newHost(t)
	config := unitConfig("/bin/true")
	config["credential_env"] = "ANTHROPIC_API_KEY"
	if _, err := newAdapter(settings(t, "lab", config), hclog.NewNullLogger()); err == nil || !strings.Contains(err.Error(), "credential_env") {
		t.Fatalf("got %v, want credential_env refused as an unknown member", err)
	}
}

func TestFactoryRefusesAPlatformCredential(t *testing.T) {
	newHost(t)
	withPlatform := settings(t, "lab", unitConfig("/bin/true"))
	withPlatform.Platform = "token"
	if _, err := newAdapter(withPlatform, hclog.NewNullLogger()); err == nil || err.Error() != "the systemd plugin takes no platform credential" {
		t.Fatalf("got %v", err)
	}
}

func TestFactoryGivesTheAdapterServesLogger(t *testing.T) {
	newHost(t)
	logger := hclog.New(&hclog.LoggerOptions{Name: "served"})
	built, err := newAdapter(settings(t, "lab", unitConfig("/bin/true")), logger)
	if err != nil {
		t.Fatal(err)
	}
	if built.(*Adapter).logger != logger {
		t.Fatal("adapter logs elsewhere than the logger Serve gave the factory")
	}
}

// TestReadmeReceiverConfigures configures each adapter of the README's receiver configuration as the receiver does,
// with the platform's config keys merged into the adapter's.
func TestReadmeReceiverConfigures(t *testing.T) {
	newHost(t)
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
			Name     string            `yaml:"name"`
			Platform string            `yaml:"platform"`
			Agent    string            `yaml:"agent"`
			Labels   map[string]string `yaml:"labels"`
			Config   map[string]any    `yaml:"config"`
		} `yaml:"adapters"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(example))
	decoder.KnownFields(true)
	if err := decoder.Decode(&receiver); err != nil {
		t.Fatal(err)
	}
	if len(receiver.Platforms) != 1 || receiver.Platforms[0].Type != "systemd" || len(receiver.Adapters) < 2 {
		t.Fatal("want one systemd platform and at least two adapters")
	}
	platform := receiver.Platforms[0]
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
		s := wherry.Settings{Name: adapter.Name, Pool: testPool, Agent: adapter.Agent, Config: encoded}
		if _, err := newAdapter(s, hclog.NewNullLogger()); err != nil {
			t.Fatalf("adapter %s: %v", adapter.Name, err)
		}
	}
	if !strings.Contains(string(readme), "TestReadmeReceiverConfigures") {
		t.Fatal("README.md does not name this test")
	}
}
