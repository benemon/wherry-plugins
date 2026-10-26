// Command wherry-plugin-nomad is the wherry adapter plugin for HashiCorp Nomad. The receiver starts it
// from its plugin directory; it is not run by hand.
package main

import (
	json "encoding/json/v2"
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
	// Without a platform credential, the receiver's environment supplies a token, as Nomad workload identity does.
	token := settings.Credential
	if token == "" {
		token = os.Getenv("NOMAD_TOKEN")
	}
	client, err := newClient(config, token)
	if err != nil {
		return nil, err
	}
	return New(config, settings.Name, settings.Pool, client, logger)
}
