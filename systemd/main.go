//go:build linux

// Command wherry-plugin-systemd is the wherry adapter plugin for systemd, which runs each run's agent as a transient
// unit on the receiver's host. The receiver starts it from its plugin directory; it is not run by hand.
package main

import (
	json "encoding/json/v2"
	"errors"
	"fmt"

	wherry "github.com/benemon/wherry/adapter"
	"github.com/hashicorp/go-hclog"
)

func main() {
	wherry.Serve(newAdapter)
}

func newAdapter(settings wherry.Settings, logger hclog.Logger) (wherry.Adapter, error) {
	var config Config
	if err := json.Unmarshal(settings.Config, &config, json.RejectUnknownMembers(true)); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	if settings.Platform != "" {
		return nil, errors.New("the systemd plugin takes no platform credential")
	}
	return New(config, settings.Name, settings.Pool, logger)
}
