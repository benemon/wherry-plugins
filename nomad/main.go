// Command wherry-plugin-nomad is the wherry adapter plugin for HashiCorp Nomad. The receiver starts it
// from its plugin directory; it is not run by hand.
package main

import (
	json "encoding/json/v2"
	"errors"
	"fmt"
	"os"

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
	if settings.Credential != "" && config.CredentialEnv == "" {
		return nil, errors.New("a credential needs credential_env")
	}
	// Without a token in the bundle, the receiver's environment supplies one, as Nomad workload identity does.
	token := settings.Platform
	if token == "" {
		token = os.Getenv("NOMAD_TOKEN")
	}
	client, err := newClient(config, token)
	if err != nil {
		return nil, err
	}
	return New(config, settings.Name, settings.Pool, client, logger)
}
