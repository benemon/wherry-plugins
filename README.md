# wherry-plugins

Plugins for [wherry](https://github.com/benemon/wherry) receivers, each the
plugin for one kind of platform. A receiver ships with no plugins; it runs
`wherry-plugin-<type>` from its plugin directory for each plugin type in its
configuration, and configures each of that plugin's profiles in it. See
[Plugins](https://benemon.github.io/wherry/commands#plugins) for the
protocol and [Install plugins](https://benemon.github.io/wherry/operations#install-plugins)
for their installation.

| Directory | Plugin | Plugin type |
| --- | --- | --- |
| [`nomad`](nomad) | `wherry-plugin-nomad` | `nomad` |
| [`kubernetes`](kubernetes) | `wherry-plugin-kubernetes` | `kubernetes` |
| [`systemd`](systemd) | `wherry-plugin-systemd` | `systemd` |

The plugins name the settings of a plugin profile's `config` alike; units
follow each platform:

| Setting | Nomad | Kubernetes | systemd |
| --- | --- | --- | --- |
| Agent image and command | `runtime.config` (driver-specific) | `runtime.image`, `command`, `args` | `runtime.command`, `args`, on the receiver's host |
| Agent environment | `runtime.env` | `runtime.env` | `runtime.env` |
| CPU | `runtime.resources.cpu`, MHz, default 1000 | `runtime.resources.cpu`, quantity, default `500m` | `runtime.resources.cpu_quota`, percent of one CPU, default 50 |
| Memory | `runtime.resources.memory_mb`, MiB, default 2048 | `runtime.resources.memory`, quantity, default `2Gi` | `runtime.resources.memory_mb`, MiB, default 2048 |
| User | `runtime.user`, a name or `UID[:GID]`, with `group` on `docker` and `podman` and `supplementary_groups` on `docker`; the GID defaults to the UID for credential files | `runtime.user`, `UID[:GID]`, with `group` and `supplementary_groups` as GIDs; the GID defaults to the image's | `runtime.user`, a name or `UID[:GID]`, with `group` and `supplementary_groups`; unset, a dynamic user per run |
| Placement | `node_pool`, `datacenters` | `node_selector`, `tolerations` | the receiver's host |

Each plugin is its own Go module and builds against the tagged plugin
protocol module, `github.com/benemon/wherry/pluginsdk`. `make build` builds
every plugin, `make check` runs the CI gates, and
`make dist PLUGIN=<directory> VERSION=<version>` writes a plugin's release
archives into `dist`. `make image` builds the Kubernetes receiver image
example in [`examples/`](examples/).

To develop against a local wherry checkout beside this repository, create
a Go workspace from the repository root. `go.work` is for local development
only and is not tracked.

```sh
go work init ./nomad ./kubernetes ./systemd ../wherry/pluginsdk
```
