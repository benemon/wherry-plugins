# wherry-plugins

Adapter plugins for [wherry](https://github.com/benemon/wherry) receivers.
A receiver ships with no adapters; it runs `wherry-plugin-<type>` from its
plugin directory for each adapter type in its configuration.

| Directory | Plugin | Adapter type |
| --- | --- | --- |
| [`nomad`](nomad) | `wherry-plugin-nomad` | `nomad` |
| [`kubernetes`](kubernetes) | `wherry-plugin-kubernetes` | `kubernetes` |

Each plugin is its own Go module and builds against the tagged adapter
protocol module, `github.com/benemon/wherry/adapter`. `make build` builds
every plugin, `make check` runs the CI gates, and
`make dist PLUGIN=<directory> VERSION=<version>` writes a plugin's release
archives into `dist`. `make image` builds the receiver image for
OpenShift; see the [Kubernetes plugin](kubernetes#image).

To develop against a local wherry checkout beside this repository, create
a Go workspace from the repository root. `go.work` is for local development
only and is not tracked.

```sh
go work init ./nomad ./kubernetes ../wherry/adapter
```
