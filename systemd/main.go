//go:build linux

// Command wherry-plugin-systemd is the wherry plugin for systemd, which runs each run's agent as a transient
// unit on the receiver's host. The receiver starts it from its plugin directory; it is not run by hand.
package main

import (
	json "encoding/json/v2"
	"errors"
	"fmt"

	"github.com/benemon/wherry/pluginsdk"
	"github.com/hashicorp/go-hclog"
)

func main() {
	pluginsdk.Serve(newProfile)
}

func newProfile(settings pluginsdk.Settings, logger hclog.Logger) (pluginsdk.Plugin, error) {
	var config Config
	if err := json.Unmarshal(settings.Config, &config, json.RejectUnknownMembers(true)); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	if settings.Credential != "" {
		return nil, errors.New("the systemd plugin takes no platform credential")
	}
	return New(config, settings.Name, settings.Pool, logger)
}
