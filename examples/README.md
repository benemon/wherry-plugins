# Receiver examples

A receiver takes two files. `wherry operator receiver apply` writes its
bundle to `bundles/receivers/<name>.bundle` in the operator's working
directory. The operator writes its local configuration, `config.yaml`
([Receiver local configuration](https://benemon.github.io/wherry/configuration#receiver-local-configuration)).
Each example here holds the `config.yaml` for its platform and shows how
the platform takes the two files, with the platform's own tools. Its
`drain_timeout` stays at least 10 seconds below the platform's stop window,
which the example sets; `make check-examples` checks it.

These examples show ways to run a receiver and its plugins. Adapt them to
your environment. They are not published images or packages.

## systemd

This example installs a receiver service, its [local
configuration](systemd/config.yaml), an agent slice, and a tmpfiles entry.
The [unit file](systemd/wherry-receiver.service) loads the bundle as a
systemd credential. See the [systemd plugin's full run instructions](../systemd/README.md#run-the-receiver-as-a-systemd-service).

## Kubernetes

This example builds a receiver image and applies, with `kubectl apply -k`,
a Kustomize directory: a Deployment, service account, Role, RoleBinding, state claim,
ReadWriteMany runs claim and metrics Service, with a Secret generated from
the bundle and a ConfigMap generated from the receiver's [local
configuration](kubernetes/config.yaml). See the
[Kubernetes plugin's full image and deploy instructions](../kubernetes/README.md#run-the-receiver-in-the-cluster),
along with the [Containerfile](kubernetes/Containerfile),
[manifest](kubernetes/receiver.yaml) and
[kustomization](kubernetes/kustomization.yaml).

## Nomad

This example builds a receiver image with the Nomad plugin; see its
[Containerfile](nomad/Containerfile). The [Nomad job](nomad/wherry-receiver.nomad.hcl)
runs that image as a receiver inside Nomad, rendering the bundle and its
[local configuration](nomad/config.yaml) from a Nomad variable, with its
`state_dir` on the [host volume](nomad/wherry-receiver.volume.hcl) that
keeps its signing key across a job stop and run; see
[Run the receiver in Nomad](../nomad/README.md#run-the-receiver-in-nomad).
[A job under the exec driver](nomad/wherry-receiver-host.nomad.hcl) is an
example only; its [local configuration](nomad/config-host.yaml) names the
plugin directory of the version it runs.
