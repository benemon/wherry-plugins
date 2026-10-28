# Receiver examples

Every receiver needs the `wherry` binary, its plugins, and a local
configuration whose elements, `bundle`, `state_dir`, `plugin_dir`,
`metrics_addr`, `drain_timeout` and `credentials`,
[Receiver local configuration](https://benemon.github.io/wherry/configuration#receiver-local-configuration)
describes.

These examples show ways to run a receiver and its plugins. Adapt them to
your environment. They are not published images or packages.

## systemd

This example installs a receiver service, its [local
configuration](systemd/config.yaml), an agent slice, and a tmpfiles entry.
See the [systemd plugin's full run instructions](../systemd/README.md#run-the-receiver-as-a-systemd-service),
including the [unit file](systemd/wherry-receiver.service).

## Kubernetes

This example builds a receiver image and applies a Deployment, service
account, Role, state claim, ReadWriteMany runs claim, metrics Service, and a
ConfigMap holding the
receiver's [local configuration](kubernetes/config.yaml). See the
[Kubernetes plugin's full image and deploy instructions](../kubernetes/README.md#run-the-receiver-in-the-cluster),
along with the [Containerfile](kubernetes/Containerfile),
[manifest](kubernetes/receiver.yaml), and [apply script](kubernetes/apply.sh).

## Nomad

This example builds a receiver image with the Nomad plugin, and its [local
configuration](nomad/config.yaml) takes the Nomad token from `NOMAD_TOKEN`.
See the [Nomad plugin's full container instructions](../nomad/README.md#run-the-receiver-in-a-container)
and its [Containerfile](nomad/Containerfile). The [Nomad job](nomad/wherry-receiver.nomad.hcl)
runs that image as a receiver inside Nomad; see
[Run the receiver in Nomad](../nomad/README.md#run-the-receiver-in-nomad).
