// Command wherry-plugin-kubernetes is the wherry plugin for Kubernetes, which also runs on OpenShift. The
// receiver starts it from its plugin directory; it is not run by hand.
package main

import (
	json "encoding/json/v2"
	"fmt"

	"github.com/benemon/wherry/pluginsdk"
	"github.com/go-logr/logr"
	"github.com/hashicorp/go-hclog"
	"k8s.io/klog/v2"
)

// version is set by the release build.
var version = "dev"

func main() {
	pluginsdk.Version = version
	pluginsdk.Serve(newProfile)
}

func newProfile(settings pluginsdk.Settings, logger hclog.Logger) (pluginsdk.Plugin, error) {
	// client-go logs through klog to stderr, which the receiver would relay unlevelled.
	klog.SetLogger(logr.Discard())
	var config Config
	if err := json.Unmarshal(settings.Config, &config, json.RejectUnknownMembers(true)); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	client, err := newClient(config, settings.Credential)
	if err != nil {
		return nil, err
	}
	return New(config, settings.Name, settings.Pool, client, logger)
}
