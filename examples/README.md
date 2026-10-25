# Receiver examples

Every receiver needs the `wherry` binary and a plugin directory named by
`--plugin-dir`. The default is `/usr/local/libexec/wherry/plugins`. That
directory holds `wherry-plugin-<type>` for each adapter type in the receiver
configuration. The directory and plugin binaries must be owned by root or by
the receiver's user, and writable by neither group nor others. The receiver
also needs its bundle file, a state directory, and a metrics address.

The receiver starts a plugin only for an adapter type used in its
configuration. A plugin for an unused type in the directory never runs.

These examples show ways to run a receiver and its plugins. Adapt them to
your environment. They are not published images or packages.

## systemd

This example installs a receiver service, an agent slice, and a tmpfiles
entry. See the [systemd plugin's full run instructions](../systemd/README.md#run-the-receiver-as-a-systemd-service),
including the [unit file](systemd/wherry-receiver.service).

## Kubernetes

This example builds a receiver image and applies a Deployment, service
account, state claim, and metrics Service. See the [Kubernetes plugin's full
image and deploy instructions](../kubernetes/README.md#run-the-receiver-in-the-cluster),
along with the [Containerfile](kubernetes/Containerfile),
[manifest](kubernetes/receiver.yaml), and [apply script](kubernetes/apply.sh).

## Nomad

This example builds a receiver image with the Nomad plugin. See the [Nomad
plugin's full container instructions](../nomad/README.md#run-the-receiver-in-a-container)
and its [Containerfile](nomad/Containerfile).
