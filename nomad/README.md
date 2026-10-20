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
task `agent`, built from the adapter's `runtime`, alone. The job's metadata holds
`run_id`, `turn`, `attempt`, `adapter` and `agent`, which the receiver
reads to decide whether to adopt the job.

The prompt and the credentials travel in Nomad variables
under `nomad/jobs/agent-<pool>-<run>-t<turn>`, which the job's own workload
identity reads. The turn's input files travel in the job's group variable,
`nomad/jobs/agent-<pool>-<run>-t<turn>/agent`, one item per file holding
its base64 encoding, since Nomad's implicit workload policy reads no deeper
path. Nomad caps a variable at 64 KiB of item names and values together,
so a turn's inputs hold about 48 KiB; Dispatch refuses a turn over that,
naming the input, and creates nothing. The plugin deletes them when the turn ends. Every 30
seconds it also deletes the variables of jobs that have stopped, and of
jobs never registered within 10 minutes, and purges agent jobs that have
been dead for 10 minutes. `<pool>` is the first 12 hexadecimal digits of
the SHA-256 of the pool name, since Nomad limits a variable path to 128
characters. A receiver sweeps only its own pool's jobs and variables. The
10 minutes run from a job's last allocation change, so a turn whose job
ended while no pool member could hold it for longer than that runs again
from the start on its next delivery. The sweep also deletes the per-task
variables of earlier releases, `nomad/jobs/agent-<pool>-<run>-t<turn>/agent/<task>`.
Lookup does not adopt a job of an earlier release that holds a task beside
`agent`, so drain such jobs before upgrading.

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
- each input file in `$NOMAD_TASK_DIR/inputs`, rendered by a template that
  decodes its base64, so binary files arrive intact;
- each S3 input in `$NOMAD_TASK_DIR/inputs`, fetched by an `artifact` of
  the agent task from the presigned URL the receiver passes, with
  `options.checksum` `sha256:<hex>`, `options.archive` `false` and mode
  `file`. Nomad fetches it before the task starts and verifies its SHA-256
  once the download ends. The checksum does not bound the transfer: the
  Nomad client's `artifact.http_max_size` is the only limit on how much a
  fetch reads. `archive=false` keeps it from unpacking an
  input whose name ends like an archive. A fetch that fails or does not
  verify fails the task before it starts, and the plugin reports the job
  never started with class `fetch`, which the receiver ends
  `TASK_STATE_FAILED`. Measured on Nomad 2.0.7: the fetcher follows
  redirects and no artifact option stops it, so the checksum alone holds a
  redirected fetch to the object. The URL is the artifact's source for the
  job's lifetime and appears in the task event of a failed fetch, so anyone
  who can read the job can read it until it expires; purging the job
  removes it. Each Nomad client reaches the endpoint and trusts its
  certificate through its system roots;
- each task credential as a file in `$NOMAD_SECRETS_DIR/credentials`,
  rendered the same way, with Nomad's default template mode, 0644. `ssh`
  refuses a private key that others can read when the agent's user owns
  the file, so a deploy key works only where it does not; that is not
  measured here;
- `WHERRY_INPUT_DIR`, `WHERRY_OUTPUT_DIR` and `WHERRY_CREDENTIALS_DIR` set
  to `$NOMAD_TASK_DIR/inputs`, `$NOMAD_TASK_DIR/outputs` and
  `$NOMAD_SECRETS_DIR/credentials`, and the same paths written into the
  prompt. For the `docker`, `exec`, `java` and `podman` drivers, whose task
  sees its directories at `/local` and `/secrets`, those are
  `/local/inputs`, `/local/outputs` and `/secrets/credentials`. For any other
  driver the paths are on the host and depend on the allocation, so the
  plugin reports `${NOMAD_TASK_DIR}` and `${NOMAD_SECRETS_DIR}` forms and the
  prompt's template replaces them with the task's own values;
- `HOME` set to the task directory;
- the variables of the adapter protocol's `AgentEnvironment` over the
  runtime's `env`, with those of `AgentUnset` removed from the runtime's
  `env`, so no system, global or environment `git` configuration reaches
  the agent's clones. The plugin removes them from the runtime's `env`
  only: a `raw_exec` task also inherits the Nomad client's environment,
  and a container keeps its image's `ENV`, and Nomad offers no way to unset
  either. Add `GIT_SSH`, `GIT_SSH_COMMAND`, `GIT_SSH_VARIANT`, `GIT_DIR`,
  `GIT_WORK_TREE`, `GIT_COMMON_DIR`, `GIT_INDEX_FILE`, `GIT_OBJECT_DIRECTORY`,
  `GIT_ALTERNATE_OBJECT_DIRECTORIES`, `GIT_EXEC_PATH`, `GIT_SSL_NO_VERIFY` and
  `GIT_CURL_VERBOSE` to the client's `env.denylist`, and keep them out of
  agent images. CA and proxy variables pass through as the operator sets
  them. This protects an agent that follows its prompt; it does not
  constrain one that runs commands of its own, which can undo it;
- the runtime's `env`, driver `config` and `resources`.

The plugin collects outputs from `$NOMAD_TASK_DIR/outputs` through the
allocation filesystem API, once the allocation is terminal. Stopping a job
returns before its allocation ends, so the plugin reads the allocation
every 500 ms until it is terminal, within the receiver's 2-minute
collection bound, and reports the outputs not collected when it is still
running at the bound. It lists the
whole tree first, and refuses it, reading nothing, when it holds more
entries than the receiver's limit. The listing's modes come from `lstat`,
so a link shows as a link; the API's stat and read follow links, and both
block on a FIFO, so the plugin reads only the regular files the listing
shows within the limits, and refuses a read whose length differs from the
listing, since the API answers some failed reads with status 200 and the
error as the body. An output the receiver streams, an S3 output, it
streams as the receiver reads it, at most the size the listing shows; the
receiver's upload fails when the length differs. The agent creates the outputs directory; without one
there are no outputs. The listing shows no link count, so a hard link to
a credential file reads as a regular file; on the macOS dev agent such a
link is created and read. Nomad's source mounts the secrets directory as a
tmpfs on Linux clients that run as root, where the link would cross
filesystems; that is not measured here. The receiver's redaction of
delivered credential values covers both cases.

The plugin reads the agent task's standard output. For the `podman` driver
it reads the logs in CRI format.

## Tests

```sh
go test ./nomad/...
```

Tests that need a Nomad agent run when `WHERRY_NOMAD_DEV_ADDR` names a dev
agent with `raw_exec` enabled, such as `http://127.0.0.1:4646`, and skip
otherwise. They include the protocol's conformance suite,
`github.com/benemon/wherry/adapter/adaptertest`.
