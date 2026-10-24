# wherry-plugins

Adapter plugins for [wherry](https://github.com/benemon/wherry) receivers.
A receiver ships with no adapters; it runs `wherry-plugin-<type>` from its
plugin directory for each adapter type in its configuration.

| Directory | Plugin | Adapter type |
| --- | --- | --- |
| [`nomad`](nomad) | `wherry-plugin-nomad` | `nomad` |
| [`kubernetes`](kubernetes) | `wherry-plugin-kubernetes` | `kubernetes` |
| [`systemd`](systemd) | `wherry-plugin-systemd` | `systemd` |

The plugins name shared settings alike; units follow each platform:

| Setting | Nomad | Kubernetes | systemd |
| --- | --- | --- | --- |
| Credential variable | `credential_env` | `credential_env` | `credential_env` |
| Agent image and command | `runtime.config` (driver-specific) | `runtime.image`, `command`, `args` | `runtime.command`, `args`, on the receiver's host |
| Agent environment | `runtime.env` | `runtime.env` | `runtime.env` |
| CPU | `runtime.resources.cpu`, MHz, default 1000 | `runtime.resources.cpu`, quantity, default `500m` | `runtime.resources.cpu_quota`, percent of one CPU, default 50 |
| Memory | `runtime.resources.memory_mb`, MiB, default 2048 | `runtime.resources.memory`, quantity, default `2Gi` | `runtime.resources.memory_mb`, MiB, default 2048 |
| User | `runtime.user`, a name or `UID[:GID]`; the GID defaults to the UID for credential files | `runtime.user`, `UID[:GID]`; the GID defaults to the image's | `runtime.user`, a name or `UID[:GID]`, with `group` and `supplementary_groups`; unset, a dynamic user per run |
| Placement | `node_pool`, `datacenters` | `node_selector`, `tolerations` | the receiver's host |

Each plugin is its own Go module and builds against the tagged adapter
protocol module, `github.com/benemon/wherry/adapter`. `make build` builds
every plugin, `make check` runs the CI gates, and
`make dist PLUGIN=<directory> VERSION=<version>` writes a plugin's release
archives into `dist`. `make image` builds the receiver image for
Kubernetes and OpenShift; see the [Kubernetes plugin](kubernetes#image).

To develop against a local wherry checkout beside this repository, create
a Go workspace from the repository root. `go.work` is for local development
only and is not tracked.

```sh
go work init ./nomad ./kubernetes ./systemd ../wherry/adapter
```
