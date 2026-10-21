// Command wherry-plugin-nomad is the wherry adapter plugin for HashiCorp Nomad. The receiver starts it
// from its plugin directory; it is not run by hand.
package main

import (
	json "encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"sync"

	wherry "github.com/benemon/wherry/adapter"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/api"
)

func main() {
	wherry.Serve((&factory{clients: map[clientKey]*api.Client{}}).adapter)
}

type clientKey struct{ address, ca, namespace, token string }

// factory builds adapters, sharing one Nomad client among adapters with the same connection settings.
type factory struct {
	mu      sync.Mutex
	clients map[clientKey]*api.Client
}

func (f *factory) adapter(settings wherry.Settings, logger hclog.Logger) (wherry.Adapter, error) {
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
	client, err := f.client(config, token)
	if err != nil {
		return nil, err
	}
	return New(config, settings.Name, settings.Pool, client, logger)
}

func (f *factory) client(config Config, token string) (*api.Client, error) {
	key := clientKey{config.Address, config.CAFile, config.Namespace, token}
	f.mu.Lock()
	defer f.mu.Unlock()
	if client, ok := f.clients[key]; ok {
		return client, nil
	}
	client, err := newClient(config, token)
	if err != nil {
		return nil, err
	}
	f.clients[key] = client
	return client, nil
}
