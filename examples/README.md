# Receiver examples

Every receiver needs the `wherry` binary, its plugins, and a local
configuration, a YAML file at `/etc/wherry/config.yaml` or at the path
`wherry receiver run --config-file` names. Its fields, as
[wherry's configuration reference](https://benemon.github.io/wherry/configuration#receiver-local-configuration)
describes them, are:

- `bundle`: the receiver's bundle file, required.
- `state_dir`: the directory holding the receiver's signing key, by default
  `state` in the bundle file's directory.
- `plugin_dir`: the directory holding `wherry-plugin-<type>` for each plugin
  type in the receiver configuration, by default
  `/usr/local/libexec/wherry/plugins`. The directory and plugin binaries must
  be owned by root or by the receiver's user, and writable by neither group
  nor others.
- `metrics_addr`: the address the receiver serves `/metrics` on; without it
  the receiver serves no metrics.
- `credentials`: the source of each platform credential a plugin profile
  names, a `file`, `env` or `command`. A file must be a regular file that its
  group cannot write and others cannot read.

The receiver starts a plugin only for a plugin type used in its
configuration, and configures each of its plugin profiles in it. A plugin
for an unused type in the directory never runs.

These examples show ways to run a receiver and its plugins. Adapt them to
your environment. They are not published images or packages.

## systemd

This example installs a receiver service, its [local
configuration](systemd/config.yaml), an agent slice, and a tmpfiles entry.
See the [systemd plugin's full run instructions](../systemd/README.md#run-the-receiver-as-a-systemd-service),
including the [unit file](systemd/wherry-receiver.service).

## Kubernetes

This example builds a receiver image and applies a Deployment, service
account, state claim, metrics Service, and a ConfigMap holding the
receiver's [local configuration](kubernetes/config.yaml). See the
[Kubernetes plugin's full image and deploy instructions](../kubernetes/README.md#run-the-receiver-in-the-cluster),
along with the [Containerfile](kubernetes/Containerfile),
[manifest](kubernetes/receiver.yaml), and [apply script](kubernetes/apply.sh).

## Nomad

This example builds a receiver image with the Nomad plugin, and its [local
configuration](nomad/config.yaml) takes the Nomad token from `NOMAD_TOKEN`.
See the [Nomad plugin's full container instructions](../nomad/README.md#run-the-receiver-in-a-container)
and its [Containerfile](nomad/Containerfile).
