# wherry-plugins

Adapter plugins for [wherry](https://github.com/benemon/wherry) receivers.
A receiver ships with no adapters; it runs `wherry-plugin-<type>` from its
plugin directory for each adapter type in its configuration.

| Directory | Plugin | Adapter type |
| --- | --- | --- |
| [`nomad`](nomad) | `wherry-plugin-nomad` | `nomad` |

Each plugin is its own Go module. `go.work` joins them with the adapter
protocol module in a wherry checkout beside this repository, which local
builds need until the protocol module is tagged.
