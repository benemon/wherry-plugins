# wherry-plugin-nomad

The wherry plugin for [HashiCorp Nomad](https://developer.hashicorp.com/nomad).
A wherry receiver runs it from its plugin directory for its `nomad` plugin,
and one plugin process hosts every plugin profile of that plugin. The plugin
runs each of the receiver's runs as one Nomad batch job.

## Build

The plugin builds against the tagged plugin protocol module,
`github.com/benemon/wherry/pluginsdk`. From a clone of this repository:

```sh
make build
```

This writes `wherry-plugin-nomad` at the repository root. For a Linux
receiver host, set `GOOS=linux GOARCH=amd64`. `make check` runs the CI
gates, and `make dist PLUGIN=nomad VERSION=<version>` writes release
archives for linux and darwin, amd64 and arm64, and `checksums.txt` into
`dist`. A receiver on macOS runs the darwin plugin on its host.

## Install

1. Download `wherry-plugin-nomad` from its
   [release](https://github.com/benemon/wherry-plugins/releases), verify it and
   install it in the receiver's plugin directory, as
   [Install plugins](https://benemon.github.io/wherry/operations#install-plugins) describes
   with `plugin=nomad`. [Build](#build) covers building it from source.

2. Name that directory as `plugin_dir` in the receiver's
   [local configuration](https://benemon.github.io/wherry/configuration#receiver-local-configuration),
   whose default is `/usr/local/libexec/wherry/plugins`, and start the
   receiver with it:

   ```sh
   wherry receiver run --config-file /etc/wherry/config.yaml
   ```

The receiver checks the plugin directory and binary before it runs them;
see [`wherry receiver`](https://benemon.github.io/wherry/commands#receiver-command).
The plugin is not run by hand.

## Credentials

A plugin profile's Nomad token is the
[platform credential](https://benemon.github.io/wherry/configuration#platform-credentials)
its merged `config` names with `credential: <name>`. The receiver resolves
the name from the `credentials` of its
[local configuration](https://benemon.github.io/wherry/configuration#receiver-local-configuration),
a `file`, `env` or `command` source, and passes the value to the plugin
when it configures the plugin profile:

```yaml
credentials:
  nomad-token:
    file: /etc/wherry/nomad-token
```

`env: NOMAD_TOKEN` in place of `file` takes the token from the receiver's
environment. A plugin profile that names no credential takes `NOMAD_TOKEN`
from the receiver's environment, which the plugin inherits, so a receiver
running as a Nomad job with
[workload identity](https://developer.hashicorp.com/nomad/docs/concepts/workload-identity)
supplies it. The token needs the [ACL policy](#acl-policy).

| Variable | Effect |
| --- | --- |
| `NOMAD_TOKEN` | Nomad token for plugin profiles that name no platform credential |
| `NOMAD_ADDR` | Nomad address when `address` is empty |
| `NOMAD_REGION` | Nomad region for API requests |
| `NOMAD_HTTP_AUTH` | HTTP basic auth for API requests |

Other `NOMAD_*` TLS variables are not read; `ca_file` is the only TLS
setting. The plugin presents no client certificate, so it cannot reach a
cluster that sets `verify_https_client`.

## ACL policy

The plugin's calls need this policy, with the namespace adjusted:

```hcl
namespace "agents" {
  capabilities = ["submit-job", "list-jobs", "read-job", "read-logs", "read-fs"]

  variables {
    path "nomad/jobs/agent-*" {
      capabilities = ["write", "read", "destroy"]
    }
  }
}

node {
  policy = "read"
}

node_pool "default" {
  policy = "read"
}
```

- `submit-job`: register a run's job, deregister one that is stopped or
  outlives its dispatch wait, and purge one that never started or
  has been dead for 10 minutes.
- `list-jobs`: find dead agent jobs to purge.
- `read-job`: read a job and its allocations.
- `read-logs`: stream the agent task's standard output.
- `read-fs`: list and read the agent task's outputs directory once the job
  has stopped.
- Variables `write`, `read` and `destroy`: hold each run's prompt,
  credentials and input files, and sweep them after the job
  ends. The sweep
  lists variables under `nomad/jobs/agent-`; see
  [Nomad variables](https://developer.hashicorp.com/nomad/docs/concepts/variables)
  for the access rules.
- `node` read: find a ready node with the runtime's driver healthy.
- `node_pool` read: register jobs in the node pools the plugin profiles name.

## Settings

The `config` keys below are set on the `nomad` plugin's `config`, which
every plugin profile inherits, or on a plugin profile's `config`, and wherry
merges the two as
[Plugin profile merge](https://benemon.github.io/wherry/configuration#plugin-profile-merge)
describes. The `credential` key names a [platform credential](#credentials),
which the receiver resolves from its local configuration and removes from
the `config` before the plugin sees it. The plugin reads the merged
`config`:

| Field | Default | Content |
| --- | --- | --- |
| `address` | `NOMAD_ADDR`, else `http://127.0.0.1:4646` | Nomad API address |
| `ca_file` | none | CA bundle for the Nomad API, a path on the receiver |
| `namespace` | `default` | namespace for agent jobs and variables; `NOMAD_NAMESPACE` is not read |
| `node_pool` | `default` | node pool for agent jobs |
| `datacenters` | every datacenter | datacenters for agent jobs |
| `runtime` | required | the agent task's definition |

Names in `runtime.env` match `[A-Za-z_][A-Za-z0-9_]*`. Unknown fields are
errors. The plugin checks the section when the receiver configures the
plugin profile, and the receiver exits with `profile <name>: ...` on an
error.

`runtime` has these fields:

| Field | Content |
| --- | --- |
| `driver` | Nomad task driver, required |
| `config` | the driver's task configuration |
| `env` | environment variables for the agent task |
| `resources` | `cpu` (MHz, default `1000`) and `memory_mb` (default `2048`) |
| `user` | the agent task's user; unset, the driver's default |
| `group` | the agent task's primary group, a group name or GID; `docker` and `podman` only |
| `supplementary_groups` | a list of group names or GIDs the agent also holds; `docker` only |

`user` is a name matching `[a-z_][a-z0-9_-]*`, or `UID` or `UID:GID` with
positive integers below 2^32 and no leading zeros; root (0) is refused. A numeric `user` also owns the task's credential
files, with the UID as the group when the GID is absent. The plugin cannot
resolve a name to an ID on the client, so with a name the credential files
keep the client's owner; use a numeric `user` where the driver takes one.
The `docker` and `podman` drivers pass `user` to the container runtime,
which takes either form. The `exec` and `raw_exec` drivers look `user` up
by name on the client and fail a task whose user has no account there, so
for them `user` is a name. A numeric `user` fails every `exec` and
`raw_exec` task, and on those drivers the credential files keep the
client's owner and mode 0644 whatever `user` is set.

`group` and `supplementary_groups` take names matching
`[a-z_][a-z0-9_-]*` or GIDs below 2^32 with no leading zeros, quoted in
YAML; root (`root` or 0) is refused. `group` needs `user`, since a
container whose task user names a group alone runs as UID 0, and is
refused beside a `user` that names a GID. The plugin sets the task's
`user` to `USER:GROUP`, which the container runtime resolves; a numeric
`group` beside a numeric `user` also owns the credential files. The
plugin passes `supplementary_groups` to the `docker` driver as
`group_add`, and refuses them beside a driver `config` that sets
`group_add` itself. The `podman` driver sets no supplementary groups, and
the `exec`, `java` and `raw_exec` drivers set no group, so the plugin
refuses either field on a driver that cannot set it, with an error naming
the field and the driver.

The plugin sets `WHERRY_INPUT_DIR`, `WHERRY_OUTPUT_DIR`,
`WHERRY_CREDENTIALS_DIR`, `HOME` on a driver that isolates the task's
filesystem, `WHERRY_MODEL` when the task names a model, and the
variables of the plugin protocol's `AgentEnvironment` over `env`. [Agent jobs](#agent-jobs) describes the
job it builds.

```yaml
capacity: 2
plugins:
  nomad:
    config:
      address: https://nomad.example.com:4646
      ca_file: /secrets/nomad-ca.pem
      namespace: agents
      credential: nomad-token
    profiles:
      - name: container
        agent: coder
        labels:
          isolation: container
        config:
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

### Receiver configuration

This receiver configuration file holds the `nomad` plugin and two plugin
profiles. The plugin's `config` holds the connection settings and the
credential name every plugin profile inherits, and plugin profile `host`
overrides the inherited `node_pool`.
[`testdata/receiver.yaml`](testdata/receiver.yaml) holds this file, and
`TestReadmeReceiverConfigures` merges each plugin profile's `config` onto
the plugin's as wherry does and configures the plugin profile from it
through the plugin's decoding and checks.

```yaml
capacity: 4
plugins:
  nomad:
    # Every plugin profile of this plugin inherits these keys and can override them.
    config:
      # Unset, NOMAD_ADDR, else http://127.0.0.1:4646.
      address: https://nomad.example.com:4646
      # A path on the receiver.
      ca_file: /etc/wherry/nomad-ca.pem
      # Holds the agent jobs and their variables; the ACL policy names it.
      namespace: agents
      # The ACL policy grants node_pool read on this pool.
      node_pool: agents
      # Unset, every datacenter.
      datacenters:
        - dc1
        - dc2
      # The receiver's local configuration gives this name a source.
      credential: nomad-token
    profiles:
      # A container agent that writes its reply to a file.
      - name: container
        agent: coder
        labels:
          isolation: container
        config:
          runtime:
            driver: docker
            # Numeric, so the credential files belong to UID 1001 and GID 1002
            # with mode 0600.
            user: "1001"
            group: "1002"
            # The docker driver's group_add carries these.
            supplementary_groups: [video]
            config:
              image: registry.example.com/agent:1.0
              command: /bin/sh
              args:
                - -c
                - >-
                  test -n "$WHERRY_MODEL" || WHERRY_MODEL=haiku;
                  exec agent-run --model "$WHERRY_MODEL" < "$NOMAD_TASK_DIR/prompt.txt" 2>"$HOME/agent.err"
            env:
              AGENT_LOG_LEVEL: info
            resources:
              cpu: 2000
              memory_mb: 4096
      # A host agent under exec, which looks the user up by name on the client.
      - name: host
        agent: reviewer
        labels:
          isolation: host
        config:
          # Overrides the inherited pool; the ACL policy grants node_pool read on it too.
          node_pool: agents-host
          runtime:
            driver: exec
            # A name, so the credential files keep the client's owner and mode
            # 0644.
            user: agent
            config:
              command: /bin/sh
              args:
                - -c
                - exec agent-run < "$NOMAD_TASK_DIR/prompt.txt" 2>"$HOME/agent.err"
            env:
              AGENT_LOG_LEVEL: warn
            resources:
              cpu: 1000
              memory_mb: 2048
```

## Agent jobs

The plugin registers one batch job per run, `agent-<pool>-<run>`,
in its namespace, node pool and datacenters, with restarts and rescheduling
disabled. `<pool>` is the first 12 hexadecimal digits of the SHA-256 of the
pool name, since Nomad limits a variable path to 128 characters. The job's
task group `agent` holds the task `agent`, built from the plugin profile's
`runtime`, alone. The job's metadata holds `run_id`, `attempt`,
`profile` and `agent`, which the receiver reads to decide whether to adopt
the job.

An agent task in a terminal allocation with neither a `Started` event nor a
start time never started, and the plugin reports the job as never started.
The message of the task event that kept it from starting goes to the receiver
as the terminal's reason.

A plugin profile is ready when some ready, eligible node in its node pool and
datacenters has the runtime's driver detected and healthy.

A stop of an allocation that has finished, or that Nomad has purged or
garbage-collected, succeeds at once. The plugin stops any other job by
deregistering it: the agent task carries
[`kill_signal`](https://developer.hashicorp.com/nomad/docs/job-specification/task#kill_signal)
`SIGTERM` and a 15-second `kill_timeout`, so the Nomad client sends it
`SIGTERM` and then `SIGKILL` once the timeout passes, a timeout the
client's `max_kill_timeout`, 30 seconds by default, caps. The stop then
waits for the allocation to finish, which Nomad bounds by the kill
timeout, or by marking the allocation lost when its client is gone. A
stop whose context ends during the wait returns at once with the
context's error, the job already deregistered.

The signal goes to the task's main process. A `runtime.config` command that
starts the agent through a shell must end with `exec <agent>`, so the agent
becomes that process; an agent the shell keeps as a child never receives
`SIGTERM` and is killed when the timeout passes, with or without the
driver's `init`.

### Credential targets

Nomad renders environment targets through `$NOMAD_SECRETS_DIR/wherry.env`, a
[template env file](https://developer.hashicorp.com/nomad/docs/job-specification/template#env)
of `KEY=value` lines. A value holding a newline does not arrive intact, so
give such a credential a file target. An environment target replaces a
runtime `env` variable of the same name.

The agent task receives:

- the prompt in `$NOMAD_TASK_DIR/prompt.txt`;
- each environment target in the environment variable named by the target;
- each input file in `$NOMAD_TASK_DIR/inputs`, rendered by a template that
  decodes its base64, so binary files arrive intact;
- each S3 input in `$NOMAD_TASK_DIR/inputs`, fetched as described in
  [S3 inputs](#s3-inputs);
- each file target as a file in `$NOMAD_SECRETS_DIR/credentials`,
  rendered by a template. With a numeric `user`, each file belongs to that
  UID and GID with mode 0600. With a user name or no `user`, each file
  belongs to the Nomad client's user, usually root, with Nomad's default
  template mode, 0644, so any user in the task's filesystem can read it.
  `ssh` refuses a private key that others can read when the agent's user
  owns the file;
- `WHERRY_INPUT_DIR`, `WHERRY_OUTPUT_DIR` and `WHERRY_CREDENTIALS_DIR` set
  to `$NOMAD_TASK_DIR/inputs`, `$NOMAD_TASK_DIR/outputs` and
  `$NOMAD_SECRETS_DIR/credentials`, and the same paths written into the
  prompt. For the `docker`, `exec`, `java` and `podman` drivers, whose task
  sees its directories at `/local` and `/secrets`, those are
  `/local/inputs`, `/local/outputs` and `/secrets/credentials`. For any other
  driver the paths are on the host and depend on the allocation, so the
  plugin reports `${NOMAD_TASK_DIR}` and `${NOMAD_SECRETS_DIR}` forms and the
  prompt's template replaces them with the task's own values;
- `HOME` set to the task directory for the `docker`, `exec`, `java` and
  `podman` drivers, which isolate the task's filesystem. On any other
  driver, such as `raw_exec`, the plugin leaves `HOME` unset, so the task
  keeps the `HOME` of the runtime's `env` or the Nomad client's environment
  and an agent CLI finds its own login;
- `WHERRY_MODEL` set to the task's model, one the pool's terms allow, when
  the task has one, and not set by the plugin otherwise. The plugin never puts
  it in the driver `config`; the command reads it from the environment, such
  as `test -n "$WHERRY_MODEL" || WHERRY_MODEL=haiku;` then
  `--model "$WHERRY_MODEL"`. Nomad interpolates `${...}` in driver `config`
  and fails the task with a validation error on `${WHERRY_MODEL:-haiku}`;
- the variables of the plugin protocol's `AgentEnvironment`, as described
  in [Agent environment](#agent-environment);
- the runtime's `env`, driver `config` and `resources`.

The plugin collects outputs from `$NOMAD_TASK_DIR/outputs` through the
allocation filesystem API, once the allocation is terminal. A stop cut
short returns before its allocation ends, so the plugin reads the allocation
every 500 ms until it is terminal, for as long as the receiver's collect
call lasts, and returns an error when the call ends with the allocation
still running. It lists the whole tree first, and refuses it, reading
nothing, when it holds more entries than the receiver's limit. The
listing's modes come from `lstat`, so a link shows as a link; the API's
stat and read follow links, and both block on a FIFO, so the plugin reads
only the regular files the listing shows within the limits, and refuses a
read whose length differs from the listing, since the API answers some
failed reads with status 200 and the error as the body. An output the
receiver streams, an S3 output, it streams as the receiver reads it, at
most the size the listing shows; the receiver's upload fails when the
length differs. The receiver streams the agent's [outcome file](https://benemon.github.io/wherry/configuration#agent-contract) the same
way. The agent creates the outputs directory; without one there are no
outputs. The listing shows no link count, so a hard link to a
credential file reads as a regular file; the receiver's redaction of
delivered credential values covers it.

The plugin reads the agent task's standard output. For the `podman` driver
it reads the logs in CRI format.

On a client with SELinux enforcing, the Podman driver must relabel the task
directory, or the agent cannot read its prompt and inputs: set
[`volumes { selinuxlabel = "z" }`](https://developer.hashicorp.com/nomad/plugins/drivers/podman#plugin-options)
in the client's Podman plugin configuration.

### Variables

The prompt and the credentials travel in the Nomad variable
`nomad/jobs/agent-<pool>-<run>`, which the job's own workload
identity reads. The run's input files travel in the job's group variable,
`nomad/jobs/agent-<pool>-<run>/agent`, one item per file holding
its base64 encoding, since Nomad's implicit workload policy reads no deeper
path. Nomad caps a variable at 64 KiB of item names and values together,
so a run's inputs hold about 48 KiB. The plugin declares an inline limit
of 45056 bytes, which leaves 4 KiB for the inputs' names, so a host sends
larger inputs through the pool's bucket; Dispatch refuses a run over the
variable, naming the input, and creates nothing. The plugin deletes the
variables when the run ends.

### Sweep

On each sweep the receiver requests
([plugins](https://benemon.github.io/wherry/commands#plugins)),
the plugin deletes the variables of jobs that have stopped, and of jobs
never registered within 10 minutes, and purges agent jobs that have been
dead for 10 minutes. A receiver sweeps only its own pool's jobs and
variables, and treats as an agent job only one with `run_id` meta. Each plugin profile purges and counts only the jobs whose `profile`
meta names it, and deletes the variables of any of the pool's jobs dead
for 10 minutes, since they hold the prompt and credentials. A dead job
whose plugin profile is no longer in the receiver's configuration stays until
Nomad's garbage collection or a manual purge; its variables are swept. The
sweep needs a Nomad server that returns job `Meta` in list responses,
`GET /v1/jobs?meta=true`. The 10 minutes run from a job's last allocation
change, so a run whose job ended while no pool member could hold it for
longer than that runs again from the start on its next delivery.

### S3 inputs

The plugin fetches each S3 input from the presigned URL the receiver passes
with an [`artifact`](https://developer.hashicorp.com/nomad/docs/job-specification/artifact)
of the agent task. It sets `options.checksum` to `sha256:<hex>`,
`options.archive` to `false`, mode `file`, and the destination
`local/inputs/<name>`. `archive=false` keeps Nomad from unpacking an input
whose name ends like an archive.

Nomad verifies the checksum after the download ends, so it bounds neither
the redirect target nor the size. On Nomad 2.0.7 the fetcher follows
redirects and no artifact option stops it, so the checksum alone holds a
redirected fetch to the object. The Nomad client's `artifact.http_max_size`
is the only limit on how much a fetch reads. A fetch that fails or does not
verify fails the task before it starts, and the plugin reports the job
never started with class `fetch`, which the receiver ends
`TASK_STATE_FAILED`.

The URL is the artifact's source for the job's lifetime and appears in the
task event of a failed fetch, so anyone who can read the job can read it
until it expires. Purging the job removes it.

### Agent environment

The plugin sets the variables of the plugin protocol's `AgentEnvironment`
over the runtime's `env` and removes those of `AgentUnset` from it, so no
system, global or environment `git` configuration reaches the agent's
clones. It removes them from the runtime's `env` only. A `raw_exec` task
also inherits the Nomad client's environment, a container keeps its
image's `ENV`, and Nomad offers no way to unset either. Add the variables
of [`AgentUnset`](https://github.com/benemon/wherry/blob/main/pluginsdk/pluginsdk.go)
to the client's `env.denylist`, and keep them out of agent images. CA and
proxy variables pass through as the operator sets them. This protects an
agent that follows its prompt. An agent that runs commands of its own can
undo this.

## Metrics

The plugin speaks plugin protocol version 10, and a receiver of another
protocol version refuses it at its start, as
[Plugins](https://benemon.github.io/wherry/commands#plugins) describes, so
upgrade it with the receiver. For each plugin profile it reports these
families, which the receiver serves on its `/metrics` with the plugin
profile's name as `profile`:

| Family | Type | Labels | Counts |
| --- | --- | --- | --- |
| `wherry_plugin_platform_errors_total` | counter | `class` | failed Nomad API calls |
| `wherry_plugin_cleanup_pending` | gauge | none | the plugin profile's dead agent jobs the latest sweep left |
| `wherry_plugin_sweep_deletions_total` | counter | `kind` | the plugin profile's jobs a sweep purged (`job`) and variables it deleted (`variable`) |
| `wherry_plugin_placement_seconds` | histogram | none | time from a Dispatch that registers the job to the plugin seeing its agent task started |

An API call's class comes from its status code: 401 and 403 are `denied`,
409 `conflict`, 408 and 504 `timeout`, and other 5xx codes `unavailable`. A
call that cannot connect is `unavailable`, one that runs past its deadline
`timeout`, and any other failure `other`. A 404, which the plugin reads as
absence, and a call the receiver cancels count nothing.

A failed call the plugin returns reaches the receiver with its operation,
class and text, and the receiver logs it redacted. A log stream the API
cannot decode fails as `log stream not decodable`, since the API's own error
quotes the agent's output. A failed call the plugin handles itself, such as
a poll of the allocation, logs one warning, `nomad call
failed`, naming the plugin profile as `profile`, the operation and the class; the line holds no
URL, token or response body.

A dead job counts as pending while it is inside the 10-minute
[sweep](#sweep) grace or its purge fails. Dispatch polls the allocation
every 2 seconds, which bounds the precision of placement times.

## Agent network reach

The operator guide's
[Agent network reach](https://benemon.github.io/wherry/operations#agent-network-reach)
says what an agent job's egress policy denies and allows.

The plugin's agent jobs carry no `network` block, so the driver decides
the task's network. `exec` and `raw_exec` tasks share the client host's
network namespace. Podman tasks take Podman's default for the client
(bridge networking when rootful, `slirp4netns` or `pasta` when rootless),
or the network the runtime's `config` names in `network_mode`.

1. Enable [Nomad ACLs](https://developer.hashicorp.com/nomad/docs/secure/acl).
   An agent that shares the host's network reaches the client's HTTP API
   on loopback, and without ACLs that API serves every job, URLs included.
2. Place agents in their own node pool (the plugin profile's `node_pool`), on
   hosts that run nothing else, so a host firewall can carry the policy.
3. Use `exec` or Podman. Avoid `raw_exec`: a `raw_exec` task has no filesystem
   isolation and runs as the client's user, often root, so no rule can tell it
   from the client.
4. On a host that runs `exec` tasks, the Nomad client must still reach the
   servers, so a rule for the whole host cannot deny them. Match the agent
   task's user instead (for example nftables `meta skuid`). Set the
   runtime's `user` to a user dedicated to agents, so the rule matches
   agents alone. Without `user`, `exec` tasks run as the driver's default,
   `nobody`, and the rule is as narrow as that user is dedicated to agents.
5. For Podman, attach agents to a Podman network whose firewall rules carry
   the policy, named by `network_mode` in the runtime's `config`.

Every Nomad task has the
[task API](https://developer.hashicorp.com/nomad/docs/runtime/task-api)
socket in its secrets directory, which no address rule covers. It accepts
only an authenticated request. The plugin exposes no workload identity
token to the agent; keep `NOMAD_TOKEN`, `CONSUL_TOKEN`,
`CONSUL_HTTP_TOKEN` and `VAULT_TOKEN` out of the runtime's `env` and the
image. A client `env.denylist` replaces Nomad's default list, which holds
those names, rather than extending it.

The client's artifact fetches run in a subprocess of the client, in the
client host's network namespace, outside any task network. Restrict the
client host's egress separately.

## Run the receiver in a container

The [Nomad example Containerfile](../examples/nomad/Containerfile) builds
from the repository root. It downloads and verifies the wherry release,
builds `wherry-plugin-nomad`, and places both binaries in a UBI minimal
image whose entrypoint is `wherry`. The container runs the receiver as
`wherry receiver run --config-file FILE`, with `FILE` the receiver's local
configuration mounted in the container.
[`examples/nomad/config.yaml`](../examples/nomad/config.yaml) is one for
this image, naming the bundle, state directory, plugin directory and
metrics address, and a source for the `nomad-token` credential.

Build it with Docker and a wherry release version:

```sh
docker build -f examples/nomad/Containerfile --build-arg WHERRY_VERSION=<version> .
```

The receiver needs 30 seconds to stop before a forced kill, as
[Upgrade wherry](https://benemon.github.io/wherry/operations#upgrade-wherry)
describes, so stop the container with `docker stop --time 30`.

## Run the receiver in Nomad

A receiver that drives Nomad can run as a Nomad job itself, in one of three
shapes. Each receiver has its own bundle, so run one job, of count 1, per
receiver.

### The receiver container under the docker driver

This is the primary shape, and the CI lane `nomad` in
[`e2e.yml`](../.github/workflows/e2e.yml) deploys, upgrades and rolls it
back. [`examples/nomad/wherry-receiver.nomad.hcl`](../examples/nomad/wherry-receiver.nomad.hcl)
runs the image the [Containerfile](../examples/nomad/Containerfile) builds,
pushed where the clients pull from:

```sh
nomad var put nomad/jobs/wherry-receiver bundle=@receiver.bundle
nomad job run -var image=registry.example.com/wherry-receiver:<version> examples/nomad/wherry-receiver.nomad.hcl
```

The job renders the bundle from that variable, which its workload
identity reads, into the task's secrets directory, owned by the image's
user with mode 0600; setting the owner needs a Nomad client running as
root. Its local configuration keeps `state_dir` on the allocation's data
directory, which the job's sticky ephemeral disk carries to the next
allocation on the same node. The task's `kill_timeout` is 30 seconds. The
task shares the client's network, so the plugin reaches the client's API on
`127.0.0.1:4646` unless the receiver configuration sets `address`, and
`identity { env = true }` gives it `NOMAD_TOKEN`. With ACLs enabled, attach
the [ACL policy](#acl-policy) to the job:

```sh
nomad acl policy apply -namespace default -job wherry-receiver wherry-receiver policy.hcl
```

Upgrade or roll back by running the job again with another image. The old
receiver releases its runs when it stops, and the new one adopts their
agent jobs.

### Release archives fetched with artifact blocks

A job whose task fetches the wherry and `wherry-plugin-nomad` release
archives with [`artifact`](https://developer.hashicorp.com/nomad/docs/job-specification/artifact)
blocks, and runs `wherry receiver run` from them under the `exec` driver.
An artifact cannot verify a cosign signature, so verify each release's
`checksums.txt` as [Install](#install) describes and pin each archive's
SHA-256 from it in `options.checksum`. The plugin directory the archive
unpacks into must meet the receiver's ownership and mode checks. The
version is part of each artifact's source, so an upgrade or rollback is a
change to the job.

### Versioned host paths

A job that runs `wherry receiver run` from binaries installed on the client
under a directory per version, such as `/opt/wherry/<version>/wherry` with
the plugin directory `/opt/wherry/<version>/plugins`, under `raw_exec`, or
under `exec` with that directory in the client's `chroot_env`. Install each
version on every client that can run the job before the job names it; an
upgrade or rollback changes the version the job names, and the previous
version's directory stays for a rollback.

## Tests

```sh
make test
```

Tests that need a Nomad agent run when `WHERRY_NOMAD_DEV_ADDR` names a dev
agent with `raw_exec` enabled, such as `http://127.0.0.1:4646`, and skip
otherwise. They include the protocol's conformance suite,
`github.com/benemon/wherry/pluginsdk/pluginsdktest`.
