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
gates, and the release build, `nomad/.goreleaser.yaml`, writes release
archives for linux and darwin, amd64 and arm64, and `checksums.txt` into
`dist` (see the [repository README](../README.md)). A receiver on macOS runs the darwin
plugin on its host.

## Install

Download and verify the release binary using
[Install plugins](https://benemon.github.io/wherry/operations-receivers#install-plugins)
with `plugin=nomad`, or use [Build](#build).
Install it as `/usr/local/libexec/wherry/<version>/plugins/wherry-plugin-nomad`
beside `/usr/local/libexec/wherry/<version>/wherry`, with root ownership and mode 0755.
Keep the directories writable only by root.
Point `/usr/local/libexec/wherry/current` at the version directory and
`/usr/local/bin/wherry` at `/usr/local/libexec/wherry/current/wherry`.
Set `plugin_dir: /usr/local/libexec/wherry/current/plugins` in the receiver's local configuration.
Its default, `/usr/local/libexec/wherry/plugins`, does not follow the versioned layout.
The receiver runs the plugin only from a binary whose SHA-256 is the pin its record holds for the type:
`plugin_sha256: {nomad: HEX}` in the receiver's file, which `wherry operator receiver apply` records.
The plugin is not run by hand.
[Run the receiver in Nomad](#run-the-receiver-in-nomad) covers pinning and deployment.

## Credentials

A plugin profile's Nomad token is the
[platform credential](https://benemon.github.io/wherry/configuration-receiver#platform-credentials)
its merged `config` names with `credential: <name>`. The receiver resolves
the name from the `credentials` of its
[local configuration](https://benemon.github.io/wherry/configuration-receiver#receiver-local-configuration),
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
[Plugin profile merge](https://benemon.github.io/wherry/configuration-receiver#plugin-profile-merge)
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
        agent: claude
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
        agent: claude
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
deregistering it. The agent task carries
[`kill_signal`](https://developer.hashicorp.com/nomad/docs/job-specification/task#kill_signal)
`SIGTERM` and a 15-second
[`kill_timeout`](https://developer.hashicorp.com/nomad/docs/job-specification/task#kill_timeout),
which the client's
[`max_kill_timeout`](https://developer.hashicorp.com/nomad/docs/configuration/client#max_kill_timeout)
caps. The stop then waits for the allocation to finish, with no deadline of
its own. A stop whose context ends during the wait returns at once with the
context's error. The job stays deregistered.

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
  Nomad renders the files inside the client's `data_dir`, mode 0700, and
  that directory is the boundary that keeps other users of the client from
  them. `ssh` refuses a private key that others can read when the agent's
  user owns the file;
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
short returns before its allocation ends, so the plugin waits on the
allocation with
[blocking queries](https://developer.hashicorp.com/nomad/api-docs#blocking-queries)
on its modify index until it is terminal, for as long as the receiver's
collect call lasts. A call that ends first fails with
`the allocation is still running`. A lost allocation fails with
`the allocation was lost`, since its client, and the task's filesystem
with it, is gone. The plugin lists the whole tree first, and refuses it, reading
nothing, when it holds more entries than the receiver's limit. The
listing's modes come from `lstat`, so a link shows as a link; the API's
stat and read follow links, and both block on a FIFO, so the plugin reads
only the regular files the listing shows within the limits, and refuses a
read whose length differs from the listing, since the API answers some
failed reads with status 200 and the error as the body. An output the
receiver streams, an S3 output, it streams as the receiver reads it, at
most the size the listing shows; the receiver's upload fails when the
length differs. The receiver streams the agent's [outcome file](https://benemon.github.io/wherry/configuration-receiver#agent-contract) the same
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
path. Nomad caps a variable's size
([Variables API](https://developer.hashicorp.com/nomad/api-docs/variables/variables)),
and base64 adds a third. The plugin declares an inline limit of 45056
bytes, which leaves room for the inputs' names, so a host sends
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
the redirect target nor the size. On Nomad 2.1.1, the release CI runs, the
fetcher follows redirects and no artifact option stops it, so the checksum alone holds a
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
of [`AgentUnset`](https://github.com/benemon/wherry/blob/pluginsdk/v0.8.0-rc.2/pluginsdk/pluginsdk.go#L249)
to the client's `env.denylist`, and keep them out of agent images. CA and
proxy variables pass through as the operator sets them. A command the
agent runs can set the variables again.

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
a read of the allocation it waits on, logs one warning, `nomad call
failed`, naming the plugin profile as `profile`, the operation and the class; the line holds no
URL, token or response body.

A dead job counts as pending while it is inside the 10-minute
[sweep](#sweep) grace or its purge fails. Dispatch polls the allocation
every 2 seconds, which bounds the precision of placement times.

## Agent network reach

The operator guide's
[Agent network reach](https://benemon.github.io/wherry/operations-hosts#agent-network-reach)
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

<!-- #region install -->
## Run the receiver in Nomad

Start with your [operator working directory](https://benemon.github.io/wherry/operations-operator#set-up-wherry-operator), pool and entitled tenant ready.
Run one receiver job with count 1 per receiver identity.
Use the container under the docker driver as the primary deployment.
Follow the [Nomad job documentation](https://developer.hashicorp.com/nomad/docs/job-specification) for platform settings.
Clone `https://github.com/benemon/wherry-plugins` and check out the plugin's tag `nomad/<version>`, such as `nomad/v0.1.0-rc.18`.
Run these steps from that checkout unless a step names your operator working directory.

1. Build the receiver image from the checkout and push it to a registry your clients can pull from:

   ```sh
   docker build -f examples/nomad/Containerfile --build-arg VERSION=<plugin version> \
     --build-arg WHERRY_VERSION=<wherry version> -t registry.example.com/wherry-receiver:<image tag> .
   docker push registry.example.com/wherry-receiver:<image tag>
   ```

   `<plugin version>` is the version of the tag you checked out, such as `v0.1.0-rc.18`.
   `<wherry version>` is the wherry release the image holds, such as `v0.1.0-rc.28`.
   `<image tag>` is the tag you give the image in your registry, such as `v0.1.0-rc.18`.
   The [Containerfile](https://github.com/benemon/wherry-plugins/blob/main/examples/nomad/Containerfile) builds the plugin and verifies the downloaded wherry release.
   It runs as UID 1001 with group 0.
   The [receiver job reference](https://github.com/benemon/wherry-plugins/blob/main/nomad/README.md#receiver-job) describes the image and the job.

2. Read the plugin pin from the image you pushed:

   ```sh
   docker run --rm --entrypoint sha256sum registry.example.com/wherry-receiver:<image tag> \
     /usr/local/libexec/wherry/plugins/wherry-plugin-nomad
   ```

   Use this SHA-256 as `plugin_sha256.nomad` in the receiver record.

3. In your operator working directory, create `receivers/nomad-1.yaml` with your pool, pin and profiles.
   For example, adapt this single-plugin record to your pool and agent image:

   ```yaml
   pool: build
   plugin_sha256:
     nomad: <the image's plugin pin>
   capacity: 2
   plugins:
     nomad:
       config:
         namespace: agents
         runtime:
           driver: docker
           config:
             image: registry.example.com/claude-agent:1.0
             command: /bin/sh
             args:
               - -c
               - cd "$NOMAD_TASK_DIR" && exec claude -p --output-format stream-json --verbose < prompt.txt
           resources:
             cpu: 1000
             memory_mb: 2048
       profiles:
         - name: container
           agent: claude
           labels:
             isolation: container
   ```

   Create the Nomad namespace `agents` before running the job; the record and the ACL policy name it.

   Use the [receiver configuration reference](https://github.com/benemon/wherry-plugins/blob/main/nomad/README.md#receiver-configuration) for the plugin settings.
   Leave `address` unset to use the job's task API socket.
   Leave profile `credential` unset to use the job's workload identity token.
   Copy [config.yaml](https://github.com/benemon/wherry-plugins/blob/main/examples/nomad/config.yaml) to `receivers/nomad-1.config.yaml` before applying:

   ```sh
   wherry operator receiver apply nomad-1
   ```

   This writes `bundles/receivers/nomad-1.bundle`.
   You can instead provide `config.yaml` in the receiver configuration directory passed with `-f DIR`.
   See [receiver local configuration](https://benemon.github.io/wherry/configuration-receiver#receiver-local-configuration).
   For a private broker CA, enable `ca_file` in your local configuration.
   Keep the local configuration and bundle outside version control.

4. Create the persistent state volume with [the example volume](https://github.com/benemon/wherry-plugins/blob/main/examples/nomad/wherry-receiver.volume.hcl):

   ```sh
   nomad volume create examples/nomad/wherry-receiver.volume.hcl
   ```

   Use Nomad 1.10 or later for this dynamic host volume.
   With ACLs enabled, `nomad volume create` needs a token with the `host-volume-create` capability.
   The client must run as root to set the volume and rendered bundle ownership.
   Keep a separate volume for each receiver.
   Preserve it across restarts because it holds the receiver's signing key.
   The [state volume reference](https://github.com/benemon/wherry-plugins/blob/main/nomad/README.md#the-state-volume) describes a static host volume and a lost volume.
   Follow [Nomad host volumes](https://developer.hashicorp.com/nomad/docs/other-specifications/volume/host) for volume administration.
   The bridge network requires the [CNI plugins](https://developer.hashicorp.com/nomad/docs/networking/cni).
   Kafka must advertise addresses the task can reach.

5. Put the bundle and the local configuration you supplied to `receiver apply` in the job variable:

   ```sh
   nomad var put nomad/jobs/wherry-receiver \
     bundle=@<operator directory>/bundles/receivers/nomad-1.bundle \
     config=@<operator directory>/receivers/nomad-1.config.yaml
   ```

   Add `ca=@<CA file>` for a private broker CA.
   With ACLs enabled, save the [plugin ACL policy](https://github.com/benemon/wherry-plugins/blob/main/nomad/README.md#acl-policy) as `policy.hcl` and attach it:

   ```sh
   nomad acl policy apply -namespace default -job wherry-receiver wherry-receiver policy.hcl
   ```

6. Deploy [the docker job](https://github.com/benemon/wherry-plugins/blob/main/examples/nomad/wherry-receiver.nomad.hcl):

   ```sh
   nomad job run -var image=registry.example.com/wherry-receiver:<image tag> examples/nomad/wherry-receiver.nomad.hcl
   ```

7. Find the allocation with `nomad job status wherry-receiver` and read its logs:

   ```sh
   nomad alloc logs <allocation ID> receiver
   ```

   Check for `plugin nomad: version <plugin version> protocol <n>`.
   From your entitled tenant's host, run `wherry host receivers` and wait for `nomad-1` to show `live`.

### The exec driver (example only)

1. In an empty directory, download `wherry_<wherry version>_linux_<arch>.tar.gz`, `checksums.txt` and `checksums.txt.sigstore.json` from `https://github.com/benemon/wherry/releases/tag/<wherry version>`.
   `<wherry version>` carries its `v`, such as `v0.1.0-rc.28`.
   `<arch>` is `amd64` or `arm64`.
   Verify the signature of `checksums.txt` with the [cosign command](https://github.com/benemon/wherry-plugins/blob/main/kubernetes/README.md#image).
   Check the archive against the checksums and extract `wherry`:

   ```sh
   sha256sum --ignore-missing -c checksums.txt
   tar -xzf wherry_<wherry version>_linux_<arch>.tar.gz --strip-components=1 wherry_<wherry version>_linux_<arch>/wherry
   ```

   In another empty directory, fetch and verify the Nomad plugin using [Install plugins](https://benemon.github.io/wherry/operations-receivers#install-plugins) with `plugin=nomad`.
   On every eligible client, install root-owned binaries with mode 0755 in `/usr/local/libexec/wherry/<version>/wherry` and `/usr/local/libexec/wherry/<version>/plugins/wherry-plugin-nomad`.
   `<version>` names this deployment's version directory, such as the plugin version.
   Point `/usr/local/libexec/wherry/current` at that version directory.
   Point `/usr/local/bin/wherry` at `/usr/local/libexec/wherry/current/wherry`.
   Keep the directories writable only by root.
   Create a dedicated `wherry` user and group on each client.
2. Record the release binary's SHA-256 as `plugin_sha256.nomad`.
   Provide [config-host.yaml](https://github.com/benemon/wherry-plugins/blob/main/examples/nomad/config-host.yaml) as `receivers/nomad-1.config.yaml` before `wherry operator receiver apply nomad-1`.
   Its `plugin_dir` is `/usr/local/libexec/wherry/current/plugins`.
3. Follow the commands in [the exec job](https://github.com/benemon/wherry-plugins/blob/main/examples/nomad/wherry-receiver-host.nomad.hcl) to create the state volume with the wherry user's UID and GID, upload the bundle and configuration, attach the ACL policy and run the job.
   Follow [the exec driver documentation](https://developer.hashicorp.com/nomad/docs/drivers/exec) for client requirements.
4. Check the allocation logs for `plugin nomad: version <plugin version> protocol <n>`.
   Run `wherry host receivers` and wait for `nomad-1` to show `live`.

### Upgrade

1. Build and push the new image, then obtain its plugin pin.
   For exec, install the verified binaries in a new version directory beside the old one on every eligible client.
2. Update `plugin_sha256.nomad` and run `wherry operator receiver apply nomad-1` in your operator working directory.
   [Change a receiver's configuration](https://benemon.github.io/wherry/operations-operator#change-a-receiver-s-configuration) describes this command.
   Until the new version runs, the receiver's restarts refuse the old plugin binary.
3. Immediately run the docker job with the new image.
   For exec, repoint `current` on each eligible client and restart the job with `nomad job restart wherry-receiver`.
   Keep the state volume.
4. Check the plugin identity line and wait for the receiver to show `live`.
   To roll back, apply the previous pin and redeploy the previous image, or repoint `current` to the previous host version and restart.

### Uninstall

1. Revoke the receiver in your operator working directory:

   ```sh
   wherry operator receiver remove nomad-1
   ```

2. Stop and purge the job, then remove its variable:

   ```sh
   nomad job stop -purge wherry-receiver
   nomad var purge nomad/jobs/wherry-receiver
   ```

3. Remove the job's ACL policy with `nomad acl policy delete wherry-receiver` when you created it.
   Delete the receiver's volume with `nomad volume delete <volume ID>` after deciding that its retained state is no longer needed.
   Remove the receiver's local bundle and configuration copies.
   For exec, remove the installed version directories and symlinks when no receiver uses them.
<!-- #endregion install -->

## Receiver job

A receiver that drives Nomad runs as a Nomad job itself.
Each receiver has its own bundle, so you run one job, of count 1, per receiver.

### The receiver image and the docker job

The [Nomad example Containerfile](../examples/nomad/Containerfile) builds from the repository root.
It downloads and verifies the wherry release.
It builds `wherry-plugin-nomad`.
It places both binaries in a UBI minimal image whose entrypoint is `wherry`.
The image holds `/usr/local/bin/wherry` and `/usr/local/libexec/wherry/plugins/wherry-plugin-nomad`.
The image holds one version, so it has no `/usr/local/libexec/wherry/<version>/` directory or `current` link.

The docker job is the primary shape.
The CI lane `nomad` in [`e2e.yml`](../.github/workflows/e2e.yml) deploys, upgrades and rolls it back.
[`examples/nomad/wherry-receiver.nomad.hcl`](../examples/nomad/wherry-receiver.nomad.hcl) runs the image the Containerfile builds.

The job reads the Nomad variable `nomad/jobs/wherry-receiver` through its workload identity.
Its `template` blocks render the variable's items into the task:

- The item `bundle`, the receiver's bundle that `wherry operator receiver apply` writes, goes into the task's secrets directory.
  It is owned by the image's user with mode 0600.
  Setting that owner needs a Nomad client running as root.
- The item `config`, the receiver's local configuration, goes to `/local/config.yaml`.
  [`examples/nomad/config.yaml`](../examples/nomad/config.yaml) is the example.
- The item `ca`, for a broker under a private CA, goes to `/secrets/kafka-ca.pem`.
  `config.yaml`'s `ca_file` names that path.

A changed item restarts the task.
The restarted task reads the new bundle or local configuration.

The task's `kill_timeout` is 30 seconds.
The local configuration's `drain_timeout` is 20 seconds
([Drain timeout and the stop window](https://benemon.github.io/wherry/configuration-receiver#drain-timeout-and-the-stop-window)).
On an upgrade or a rollback, the old receiver releases its runs when it stops.
The new receiver adopts their agent jobs.
The CI lane reads the recorded pin back with `wherry operator receiver show nomad-1` before it runs the job.

### The task API and NOMAD_ADDR

The receiver reaches Nomad through the
[task API](https://developer.hashicorp.com/nomad/api-docs/task-api).
The task's environment sets `NOMAD_ADDR` to `unix:///secrets/api.sock`.
That path is the task API socket in the task's secrets directory.
The plugin dials it when the receiver configuration leaves `address` unset.
`identity { env = true }` gives the task `NOMAD_TOKEN`, the workload identity's token.
The task API takes that token.

The receiver reaches Kafka through the network.
The group's
[`network`](https://developer.hashicorp.com/nomad/docs/job-specification/network)
block runs the task in a bridge network.
The bridge network needs the
[CNI reference plugins](https://developer.hashicorp.com/nomad/docs/networking/cni)
on the client.
The block maps the receiver's metrics on port 9464 to the dynamic port `metrics`.
The task reaches the Kafka brokers through the bridge.
The brokers must therefore advertise an address other than loopback.

### The state volume

The receiver's state directory holds its signing key, and its identity with it.
The job keeps `state_dir` at `/var/lib/wherry/state` on a
[host volume](https://developer.hashicorp.com/nomad/docs/other-specifications/volume/host).
The group's
[`volume`](https://developer.hashicorp.com/nomad/docs/job-specification/volume)
block claims it.
The task's
[`volume_mount`](https://developer.hashicorp.com/nomad/docs/job-specification/volume_mount)
mounts it at `/var/lib/wherry`.
The volume outlives every allocation.
The receiver keeps its key across a `nomad job stop` and `nomad job run`, a group scaled to 0 and back, an update and a reschedule.
An allocation's data directory goes with the allocation.
A sticky, migrating
[`ephemeral_disk`](https://developer.hashicorp.com/nomad/docs/job-specification/ephemeral_disk)
carries that directory only to a replacement allocation.
A receiver whose new allocation starts without its key fails every start.
That is why the job uses a host volume.

[`examples/nomad/wherry-receiver.volume.hcl`](../examples/nomad/wherry-receiver.volume.hcl) specifies the dynamic host volume `wherry-receiver`.
A dynamic host volume needs Nomad 1.10 or later.
With ACLs enabled, `nomad volume create` needs a token with the `host-volume-create` capability.
The built-in `mkdir` plugin creates the volume on one client.
The volume is owned by the image's user, 1001 with group 0, with mode 0700.
Setting that owner needs a Nomad client running as root.
The receiver creates the `state` subdirectory at 0700 with its signing key at 0600.
The volume stays on its client, and the job's allocations run there.
Volume names are not unique across clients.
The group therefore claims the volume with `sticky = true`
([stateful deployments](https://developer.hashicorp.com/nomad/docs/concepts/stateful-deployments)).
The sticky claim binds the job to that volume's ID.
A volume of the same name on another client is never mounted in its place.

Each receiver has its own volume.
Two receivers never share a state directory.
For another receiver, copy the volume specification and the job with another volume name.
Losing the volume, or the client that holds it, loses the receiver's identity.
Provision the receiver again under a new name, as
[Replace a lost key](https://benemon.github.io/wherry/operations-hosts#replace-a-lost-key)
says for a lost state directory.
Create a new volume for it.

A client can instead declare a static
[`host_volume`](https://developer.hashicorp.com/nomad/docs/configuration/client#host_volume-block)
named `wherry-receiver`.
Its directory is owned by 1001 with group 0 and has mode 0700.
The job claims it unchanged:

```hcl
client {
  host_volume "wherry-receiver" {
    path = "/opt/wherry/receiver"
  }
}
```

### The exec job

[`examples/nomad/wherry-receiver-host.nomad.hcl`](../examples/nomad/wherry-receiver-host.nomad.hcl) is an example only.
It runs `wherry receiver run` under the
[`exec`](https://developer.hashicorp.com/nomad/docs/drivers/exec) driver.
It runs the binaries installed on the client under `/usr/local/libexec/wherry/<version>/`, through the `current` link.
It runs as a dedicated `wherry` user.
It has the bundle, state volume, `kill_timeout` and workload identity of the docker job.
It shares the client's network.
Its plugin reaches the Nomad API at the receiver configuration's `address`, else `NOMAD_ADDR`, else the client's own `127.0.0.1:4646`.
It renders its local configuration from the variable's item `config`, as the docker job does.
[`examples/nomad/config-host.yaml`](../examples/nomad/config-host.yaml) is that configuration.
Its `plugin_dir` is `/usr/local/libexec/wherry/current/plugins`.
The release binary's pin is its line in the release's verified `checksums.txt`.
The job's comments give its steps.
CI validates the job but does not run it.

## Tests

```sh
make test
```

Tests that need a Nomad agent run when `WHERRY_NOMAD_DEV_ADDR` names a dev
agent with `raw_exec` enabled, such as `http://127.0.0.1:4646` or a task
API socket, and skip otherwise. They send `NOMAD_TOKEN`, as the plugin
does, which an agent with ACLs or the task API needs. They include the
protocol's conformance suite,
`github.com/benemon/wherry/pluginsdk/pluginsdktest`.

### End to end on Nomad

[`test/e2e`](test/e2e) runs the receiver job
[`examples/nomad/wherry-receiver.nomad.hcl`](../examples/nomad/wherry-receiver.nomad.hcl)
on a Nomad dev agent, as the `nomad` lane of the `e2e` workflow does for
every change to `nomad/`, `examples/nomad/`, the fake agent or the
workflow. Each step is a script; with systemd, root through sudo, Docker,
Nomad, the
[CNI reference plugins](https://github.com/containernetworking/plugins/releases)
in `/opt/cni/bin` at the version the workflow pins, Go, the repository's
`nomad/v*` tags on a Linux machine, and the wherry release the workflow
pins:

```sh
export WHERRY_VERSION=$(sed -n 's/^  WHERRY_VERSION: //p' .github/workflows/e2e.yml)
for step in nomad/test/e2e/0*.sh; do "$step" || { nomad/test/e2e/collect.sh; break; }; done
```

1. `01-nomad.sh` replaces the systemd unit `wherry-nomad` with a Nomad dev
   agent and the docker driver, waits for it to fingerprint the CNI
   plugins, and starts a single-broker Kafka with TLS and SCRAM through
   `test/kafka/up.sh` of `WHERRY_VERSION`, with `ADVERTISED_IP` the
   machine's address on its default route, so the receiver reaches the
   broker from its bridge network.
2. `02-wherry.sh` fetches the wherry CLI of `WHERRY_VERSION` through the
   receiver image's verifying fetch stage, and builds the receiver image
   from the checkout and from the latest `nomad/v*` tag before `HEAD`, both
   with `WHERRY_VERSION`, and the fake agent's image. It fails when there is
   no such tag.
3. `03-provision.sh` provisions as the operator guide does, in an operator
   working directory: `wherry operator init` with the broker's Kafka super
   user, `operator pool apply -f`, `tenant add`, `tenant pool add`, `tenant host add`, and
   `receiver apply -f` of the receiver configuration directory
   `test/e2e/receiver`, whose plugin profile
   `plugins/nomad/profiles/10-fake.yaml` runs a fake agent, with the pin of
   the plugin binary in the checkout's image appended. It runs
   `operator check`, then `wherry host init` with the host's bundle and
   the broker's CA as `ca.pem`.
4. `04-deploy.sh` puts the receiver's bundle,
   [`examples/nomad/config.yaml`](../examples/nomad/config.yaml) with its
   `ca_file` uncommented, and the broker's CA in the job's Nomad variable,
   creates the state volume from
   [`examples/nomad/wherry-receiver.volume.hcl`](../examples/nomad/wherry-receiver.volume.hcl),
   validates the example job and the host paths example job, runs the
   example job with the checkout's image, and checks that its receiver
   logs the plugin's version. It then stops the job, runs it again, and
   checks that the new allocation's receiver starts with the key on the
   volume.
5. `05-run.sh` submits a run as the host, waits for it with `wherry wait`,
   and checks its exit code, reply and output.
6. `06-upgrade.sh` rolls the job to the previous release's image, submits a
   slow run, and rolls the job to the checkout's image once the run's agent
   task runs. The new receiver adopts the agent job and completes the run,
   and its metrics, on the client port the allocation maps the `metrics`
   port to, count the adoption. Both images hold `WHERRY_VERSION`, so the
   step upgrades the plugin.
7. `07-rollback.sh` rolls the job back to the previous release's image and
   completes a run on it.

Each roll follows [Upgrade](#upgrade)'s order: it first
applies the receiver's configuration with the SHA-256 of the plugin binary
in the image it runs, then runs the job with the image. It fails as soon as
the new allocation's receiver refuses its plugin's pin, and checks that
allocation's log alone for the identity line
`plugin nomad: version <version> protocol <n>` of the version it rolled to:
`e2e` for the checkout's image, the tag's version for the previous
release's.

`06-upgrade.sh` and `07-rollback.sh` first run
[`.github/plugin-protocol.sh`](../.github/plugin-protocol.sh), which
compares the plugin protocol the previous release and the checkout serve,
from `pluginsdk/link.go` at the version each `go.mod` requires. When they
differ, a 0.x clean break, each step skips its rolls with a notice naming
both protocols, since the release notes then say to upgrade the receiver
and its plugins together, or to reset. Steps 1 to 5 still run.

`collect.sh` writes the Nomad agent's and the receiver's logs and the jobs'
state to `artifacts` in the state directory, `$E2E_WORK`, by default
`$TMPDIR/wherry-nomad-e2e`.
