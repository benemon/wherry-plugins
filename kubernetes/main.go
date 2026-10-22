// Command wherry-plugin-kubernetes is the wherry adapter plugin for Kubernetes, which also runs on OpenShift. The
// receiver starts it from its plugin directory; it is not run by hand.
package main

import (
	json "encoding/json/v2"
	"errors"
	"fmt"

	wherry "github.com/benemon/wherry/adapter"
	"github.com/go-logr/logr"
	"github.com/hashicorp/go-hclog"
	"k8s.io/klog/v2"
)

func main() {
	wherry.Serve(newAdapter)
}

func newAdapter(settings wherry.Settings, logger hclog.Logger) (wherry.Adapter, error) {
	// client-go logs through klog to stderr, which the receiver would relay unlevelled.
	klog.SetLogger(logr.Discard())
	var config Config
	if err := json.Unmarshal(settings.Config, &config, json.RejectUnknownMembers(true)); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	if settings.Credential != "" && config.CredentialEnv == "" {
		return nil, errors.New("a credential needs credential_env")
	}
	client, err := newClient(config, settings.Platform)
	if err != nil {
		return nil, err
	}
	return New(config, settings.Name, settings.Pool, client, logger)
}
