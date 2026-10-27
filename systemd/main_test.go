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
	withPlatform.Credential = "token"
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

// TestReadmeReceiverConfigures configures each plugin profile of the README's receiver configuration as the receiver
// does, with the plugin's config merged into the profile's.
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
	plugin, ok := receiver.Plugins["systemd"]
	if len(receiver.Plugins) != 1 || !ok || len(plugin.Profiles) < 2 {
		t.Fatal("want one systemd plugin and at least two profiles")
	}
	for _, profile := range plugin.Profiles {
		encoded, err := json.Marshal(merge(plugin.Config, profile.Config))
		if err != nil {
			t.Fatal(err)
		}
		s := wherry.Settings{Name: profile.Name, Pool: testPool, Agent: profile.Agent, Config: encoded}
		if _, err := newAdapter(s, hclog.NewNullLogger()); err != nil {
			t.Fatalf("profile %s: %v", profile.Name, err)
		}
	}
	if !strings.Contains(string(readme), "TestReadmeReceiverConfigures") {
		t.Fatal("README.md does not name this test")
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
