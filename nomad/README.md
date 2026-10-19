# wherry-plugin-nomad

The wherry adapter plugin for [HashiCorp Nomad](https://developer.hashicorp.com/nomad).
A wherry receiver starts it from its plugin directory for adapters of type
`nomad`, and runs each of the receiver's turns as one Nomad batch job. One
plugin process hosts every `nomad` adapter of a receiver, and adapters with
the same address, CA file, namespace and token share one Nomad client.

## Build

The plugin builds against the adapter protocol module,
`github.com/benemon/wherry/adapter`. Until that module is tagged, build in
this repository's Go workspace with a checkout of
[wherry](https://github.com/benemon/wherry) beside it:

```
wherry/
wherry-plugins/
```

From `wherry-plugins`:

```sh
go build -o wherry-plugin-nomad ./nomad
```

For a Linux receiver host, set `GOOS=linux GOARCH=amd64`.

## Install

Copy `wherry-plugin-nomad` into the receiver's plugin directory and start
the receiver with `--plugin-dir` naming it:

```sh
install -o root -g root -m 0755 wherry-plugin-nomad /usr/local/libexec/wherry/plugins/
wherry receiver --bundle FILE --state-dir DIR --plugin-dir /usr/local/libexec/wherry/plugins
```

The receiver refuses a plugin directory or binary owned by a user other
than root or the receiver's, or that group or others can write, and a
plugin that is not a regular file. The plugin is not run by hand.

## Credentials

The plugin takes an adapter's Nomad token from the receiver bundle's
`platform` entry for the adapter's name, or else from `NOMAD_TOKEN`. The
plugin inherits the receiver's environment, so a receiver running as a
Nomad job with
[workload identity](https://developer.hashicorp.com/nomad/docs/concepts/workload-identity)
supplies `NOMAD_TOKEN` to it. The token needs the
[receiver policy](https://github.com/benemon/wherry/blob/main/docs/operator-guide.md#nomad-acl-policy).

| Variable | Effect |
| --- | --- |
| `NOMAD_TOKEN` | Nomad token for adapters without a platform credential in the bundle |
| `NOMAD_ADDR` | Nomad address when `address` is empty |
| `NOMAD_REGION` | Nomad region for API requests |

## Settings

An adapter's `config` section in the receiver configuration:

| Field | Default | Content |
| --- | --- | --- |
| `address` | `NOMAD_ADDR`, else `http://127.0.0.1:4646` | Nomad API address |
| `ca_file` | none | CA bundle for the Nomad API, a path on the receiver |
| `namespace` | `default` | namespace for agent jobs and variables; `NOMAD_NAMESPACE` is not read |
| `credential_env` | none | environment variable the job receives the credential in |
| `node_pool` | `default` | node pool for agent jobs |
| `datacenters` | every datacenter | datacenters for agent jobs |
| `runtime` | required | the agent task's definition |

`credential_env` is required when the adapter sets `credential`, and
matches `[A-Za-z_][A-Za-z0-9_]*`. Unknown fields are errors. The plugin
checks the section when the receiver configures the adapter, and the
receiver exits with `adapter <name>: ...` on an error.

`runtime` has these fields:

| Field | Content |
| --- | --- |
| `driver` | Nomad task driver, required |
| `config` | the driver's task configuration |
| `env` | environment variables for the agent task |
| `resources` | `cpu` (MHz) and `memory_mb`; unset values take Nomad defaults |

The plugin sets `HOME` in `env`. [Agent jobs](#agent-jobs) describes the
job it builds.

```yaml
capacity: 2
adapters:
  - name: container
    type: nomad
    agent: coder
    credential: model-api-key
    labels:
      isolation: container
    config:
      address: https://nomad.example.com:4646
      ca_file: /secrets/nomad-ca.pem
      namespace: agents
      credential_env: MODEL_API_KEY
      runtime:
        driver: docker
        config:
          image: registry.example.com/agent:1.0
          command: /bin/sh
          args:
            - -c
            - exec agent-run < "$NOMAD_TASK_DIR/prompt.txt"
        resources:
          cpu: 1000
          memory_mb: 2048
```

## Agent jobs

The plugin registers one batch job per turn,
`agent-<pool>-<run>-t<turn>`, in its namespace, node pool and datacenters,
with restarts and rescheduling disabled. Its task group `agent` holds the
task `agent`, built from the adapter's `runtime`, and the prestart task
`git` when the run carries a git context. The job's metadata holds
`run_id`, `turn`, `attempt`, `adapter` and `agent`, which the receiver
reads to decide whether to adopt the job.

The prompt, the credential and the git context travel in Nomad variables
under `nomad/jobs/agent-<pool>-<run>-t<turn>`, which the job's own workload
identity reads. The plugin deletes them when the turn ends. Every 30
seconds it also deletes the variables of jobs that have stopped, and of
jobs never registered within 10 minutes, and purges agent jobs that have
been dead for 10 minutes. `<pool>` is the first 12 hexadecimal digits of
the SHA-256 of the pool name, since Nomad limits a variable path to 128
characters. A receiver sweeps only its own pool's jobs and variables. The
10 minutes run from a job's last allocation change, so a turn whose job
ended while no pool member could hold it for longer than that runs again
from the start on its next delivery.

An agent task with no `Started` event in a terminal allocation never
started, and the plugin reports the job as never started.

An adapter is ready when some ready, eligible node in its node pool and
datacenters has the runtime's driver detected and healthy.

The plugin stops a job by sending `SIGTERM` to the agent task. If the task
is still running 15 seconds later, it deregisters the job.

The agent task receives:

- the prompt in `$NOMAD_TASK_DIR/prompt.txt`;
- the credential in the environment variable `credential_env` names, when
  the adapter delivers one;
- `HOME` set to the task directory;
- the runtime's `env`, driver `config` and `resources`.

The plugin reads the agent task's standard output. For the `podman` driver
it reads the logs in CRI format, and the git task gets `entrypoint` and
`args` in place of `command`. For other drivers the git task's `command`
and `args` are replaced.

## Tests

```sh
go test ./nomad/...
```

Tests that need a Nomad agent run when `WHERRY_NOMAD_DEV_ADDR` names a dev
agent with `raw_exec` enabled, such as `http://127.0.0.1:4646`, and skip
otherwise. They include the protocol's conformance suite,
`github.com/benemon/wherry/adapter/adaptertest`.
