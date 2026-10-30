# wherry-plugin-kubernetes

The wherry plugin for Kubernetes. A wherry receiver runs it from its
plugin directory for its `kubernetes` plugin, and one plugin process hosts
every plugin profile of that plugin. The plugin runs each of the receiver's
runs as one pod and one Secret in a namespace, with one directory on a
ReadWriteMany claim that the receiver's pod and every agent pod mount.

Kubernetes 1.31 or later is the platform. The plugin, the receiver image
and the receiver's manifests also run unchanged on OpenShift 4.18 or later;
[OpenShift](#openshift) lists what differs there.

## Build

The plugin builds against the tagged plugin protocol module,
`github.com/benemon/wherry/pluginsdk`. From a clone of this repository:

```sh
make build
```

This writes `wherry-plugin-kubernetes` at the repository root. `make check`
runs the CI gates, and the release build, `kubernetes/.goreleaser.yaml`,
writes release archives for linux and darwin, amd64 and arm64, and
`checksums.txt` into `dist` (see the [repository README](../README.md)).
The darwin archives serve a receiver on macOS that reaches a remote cluster.
`make image VERSION=<version> WHERRY_VERSION=<wherry version>` builds the
receiver image ([Image](#image)).

## Install

In a cluster, run the receiver from the receiver image, which holds wherry
and the plugin; see
[Run the receiver in the cluster](#run-the-receiver-in-the-cluster).

On a receiver outside the cluster:

1. Download `wherry-plugin-kubernetes` from its
   [release](https://github.com/benemon/wherry-plugins/releases), verify it and
   install it in the receiver's plugin directory, as
   [Install plugins](https://benemon.github.io/wherry/operations#install-plugins) describes
   with `plugin=kubernetes`. [Build](#build) covers building it from source.

2. Name that directory as `plugin_dir` in the receiver's
   [local configuration](https://benemon.github.io/wherry/configuration#receiver-local-configuration),
   `/usr/local/libexec/wherry/plugins` by default, and start the receiver:

   ```sh
   wherry receiver run --config-file /etc/wherry/config.yaml
   ```

A receiver outside the cluster mounts the file system behind the runs claim,
such as the claim's NFS export, at `runs_dir`, and runs as the agents' UID
([Agent pods](#agent-pods)).

The receiver runs the plugin only from a binary whose SHA-256 is the pin
its record holds for the type, which `receiver add` and `receiver update`
take as `--plugin-sha256 kubernetes=HEX` from the binary's line in the release's
`checksums.txt`; see [`wherry receiver`](https://benemon.github.io/wherry/commands#receiver-command).
The plugin is not run by hand.

## Credentials

The plugin takes a plugin profile's API token as its platform credential:
the value of the credential the profile's merged `config` names with
`credential: <name>`, which the receiver resolves from the `credentials` of
its [local configuration](https://benemon.github.io/wherry/configuration#receiver-local-configuration)
and passes when it configures the profile
([Platform credentials](https://benemon.github.io/wherry/configuration#platform-credentials)).
The token never reaches `wherry.admin` or the bundle.

With `address` empty, the plugin reaches the API server of the cluster the
receiver runs in, from `KUBERNETES_SERVICE_HOST` and
`KUBERNETES_SERVICE_PORT`, with the CA the kubelet projects into
`/var/run/secrets/kubernetes.io/serviceaccount`. It authenticates with the
platform credential when the profile names one, and with the receiver pod's
service account token from that directory otherwise. The plugin rereads the
service account token file, so the kubelet's rotation of it reaches the
plugin. With `address` set, the profile must name a platform credential,
and without one the receiver exits with
`profile <name>: address needs a platform credential`.

A token for a remote cluster held in a Secret is mounted into the receiver's
pod with `defaultMode` 0440 or stricter and named as a `file` source:

```yaml
credentials:
  kubernetes-token:
    file: /etc/wherry/kubernetes-token/token
```

The receiver refuses a credential file that its group can write or others
can read, write or execute. A rotated token takes effect on the receiver's
next start.

| Variable | Effect |
| --- | --- |
| `KUBERNETES_SERVICE_HOST` | API server host when `address` is empty |
| `KUBERNETES_SERVICE_PORT` | API server port when `address` is empty |

The plugin reads no kubeconfig and no `KUBECONFIG`. A kubeconfig can name
exec credential plugins and auth providers, which run commands on the
receiver, and the plugin needs only an address, a CA and a token.

The token needs the [RBAC](#rbac) grants in the plugin profile's namespace.

## RBAC

Agent pods run in the receiver's own namespace. The receiver's service
account holds the Role `wherry-receiver` in that namespace alone, through the
RoleBinding in [`examples/kubernetes/receiver.yaml`](../examples/kubernetes/receiver.yaml), and nothing
outside it. The plugin's calls, all in that namespace, need:

| Resource | Verbs | Use |
| --- | --- | --- |
| `pods` | create, get, list, watch, delete | create a run's pod, read it for lookups and polls, list it for the sweep, and delete it for a stop, a dispatch or lookup cut short, and a purge |
| `pods/log` | get | stream the agent's standard output |
| `secrets` | create, get, update, delete, list | write a run's material, delete it when the run ends, and list its metadata for the sweep |
| `namespaces` | get | readiness |

A Role grants `get` on its own namespace's Namespace object, since the API
server authorises a read of a namespace within that namespace; the Role
reaches no other namespace. The Role grants no `pods/exec`, so the
receiver, or anyone holding its token, runs no command in any pod. In the
namespace it can:

- read and write every Secret: the bundle Secret, which holds the
  receiver's Kafka password, and every run's prompt and material;
- create and delete pods, which mount any Secret or claim of the namespace.
  A pod it creates can mount the runs claim, which holds every run's inputs
  and outputs, and the state claim, which holds the receiver's signing key.

It cannot write Deployments, Services, PersistentVolumeClaims, ConfigMaps,
ServiceAccounts, Roles or RoleBindings, and it cannot reach other
namespaces, nodes or other cluster-scoped objects. Pods it creates are
admitted by the namespace's Pod Security level, so with the namespace at
`restricted` none runs privileged or as root.

Do not:

- bind the Role's rules with a ClusterRoleBinding, or in any other
  namespace;
- run anything but the receiver and its agent pods in the namespace, since
  the Role there reaches every Secret, and a pod reaches every claim;
- lower the namespace's Pod Security level below `restricted`;
- grant RBAC to the namespace's `default` service account. Agent pods run
  as it with no token mounted, and no agent may hold any API right.

## Settings

The `config` keys below are set on the `kubernetes` plugin's `config`,
which every plugin profile inherits, or on a plugin profile's `config`, and
wherry merges the two as
[Plugin profile merge](https://benemon.github.io/wherry/configuration#plugin-profile-merge)
describes. The `credential` key names a [platform credential](#credentials),
which the receiver resolves from its local configuration and removes from
the `config` before the plugin sees it. The plugin reads the merged
`config`:

| Field | Default | Content |
| --- | --- | --- |
| `address` | in-cluster | API server URL; must be `https://` |
| `ca_file` | the service account's CA when `address` is empty; the system roots otherwise | CA bundle for the API, a path on the receiver |
| `namespace` | required | namespace for agent pods and their Secrets; a DNS-1123 label |
| `node_selector` | none | node labels agent pods require |
| `tolerations` | none | taints agent pods tolerate |
| `runs_claim` | required | the ReadWriteMany PersistentVolumeClaim in `namespace` that agent pods mount; a claim name |
| `runs_dir` | required | the absolute path where the receiver mounts the same claim, such as `/var/lib/wherry/runs` |
| `runs_floor` | `1Gi` | a quantity; the plugin profile is not ready while the claim has less free space |
| `runtime` | required | the agent container's definition |

`node_selector` keys and values are
Kubernetes label keys and values. Each toleration has `key`, `operator`
(`Equal`, or `Exists` without a `value`), `value` and `effect`
(`NoSchedule`, `PreferNoSchedule` or `NoExecute`). Unknown fields are
errors. The plugin checks the merged section when the receiver configures
the plugin profile, and the receiver exits with `profile <name>: ...` on an
error.

`runtime` has these fields:

| Field | Content |
| --- | --- |
| `image` | the agent image, with no whitespace; required |
| `command` | the container's command; unset, the image's entrypoint |
| `args` | the container's arguments |
| `env` | environment variables for the agent; names match `[A-Za-z_][A-Za-z0-9_]*` |
| `resources` | `cpu` (default `500m`), `memory` (default `2Gi`) and `ephemeral_storage` (default unset), Kubernetes quantities, each set as both request and limit |
| `user` | `UID` or `UID:GID`, a string; the UID is the receiver's; unset, the platform assigns one |
| `group` | the agent's primary group and the pod's fsGroup, a GID |
| `supplementary_groups` | a list of GIDs the agent also holds |

`user` takes positive integers below 2^31 with no leading zeros; root (0)
is refused. Quote it in YAML, such as `user: "1001"`. Kubernetes assigns no
UID, so set it. The UID must be the receiver's own, the UID the plugin runs
as, since the agent reads the inputs the plugin writes and the plugin reads
the outputs the agent writes, in a directory only that UID can enter; the
plugin refuses another with `runtime user must be the receiver's UID`. The
resource defaults are starting points; set them for the agent.

`group` and `supplementary_groups` take GIDs below 2^31 with no leading
zeros, quoted in YAML; Kubernetes resolves no group names, so a name is
refused, and so is root (0). `group` is refused beside a `user` that names
a GID. They set the pod's `securityContext.runAsGroup`, `fsGroup` and
`securityContext.supplementalGroups`. The agent reads the prompt and its
credential files through the group, so set a GID on Kubernetes.

The plugin sets `HOME` and the `WHERRY_*` variables over `env`, so a
variable of these names in `env` is overwritten. Environment targets are
injected from the run's Secret.

A plugin profile is ready when the API answers with the namespace, the
namespace is active, and the file system at `runs_dir` has `runs_floor`
free. This proves the address, the token, the namespace and the receiver's
mount of the runs claim. While the claim is short of space the receiver
withdraws its offer and takes no new work. Readiness checks nothing else:

- Nodes. Nodes are cluster-scoped, and the plugin does not read the
  cluster's topology. A pod no node can take, such as one whose
  `node_selector` no node matches, waits until the run's deadline, holding
  one of the receiver's `capacity` slots, and the run then fails
  `timeout`.
- Quota. A ResourceQuota in the namespace that is full refuses pod
  creation with 403. Each refusal counts a `dispatch` start failure, and
  the task spends its delivery budget.
- Images. An agent image that cannot be pulled shows only at dispatch, as a
  pod that never started with class `image`.

In a [receiver configuration directory](https://benemon.github.io/wherry/configuration#receiver-configuration-directory),
`plugins/kubernetes/plugin.yaml` holds the plugin's settings, here for a
receiver in the cluster, whose namespace is its own and whose service
account supplies the token:

```yaml
labels:
  platform: kubernetes
config:
  namespace: wherry
  runs_claim: wherry-runs
  runs_dir: /var/lib/wherry/runs
```

and each file in `plugins/kubernetes/profiles` holds one plugin profile,
such as `plugins/kubernetes/profiles/10-coder.yaml`:

```yaml
name: coder
agent: coder
config:
  runtime:
    image: registry.example.com/agent:1.0
    user: "1001"
    command: [/bin/sh, -c]
    args:
      - >-
        test -n "$WHERRY_MODEL" || WHERRY_MODEL=haiku;
        exec agent-run --model "$WHERRY_MODEL" < /wherry/prompt.txt
```

### Receiver configuration

This receiver configuration file holds one plugin and two plugin profiles.
The plugin's `config` holds the connection settings, the namespace, the
runs claim, the agents' user and an agent environment default, which each
plugin profile inherits; `shared` overrides the default. With `address` set, the plugin
names the platform credential `kubernetes-token`, which the receiver's
local configuration maps to a source on the receiver
([Credentials](#credentials)).
[`testdata/receiver.yaml`](testdata/receiver.yaml) holds this file, and
`TestReadmeReceiverConfigures` merges the plugin's `config` into each
plugin profile's, takes out the `credential` key, and configures each
plugin profile through the plugin's decoding and checks.

```yaml
capacity: 4
plugins:
  kubernetes:
    # Every plugin profile of this plugin inherits these keys and can override
    # them.
    config:
      # Set, a profile names a platform credential. Unset, the receiver pod's
      # service account applies.
      address: https://kubernetes.example.com:6443
      # A path on the receiver.
      ca_file: /etc/wherry/kubernetes-ca.pem
      # A name in the credentials of the receiver's local configuration; its
      # value is the API token.
      credential: kubernetes-token
      # Holds the agent pods, their Secrets and the runs claim; the token holds
      # the plugin's Role here alone.
      namespace: agents
      # The ReadWriteMany claim agent pods mount, which the receiver mounts at
      # runs_dir.
      runs_claim: wherry-runs
      runs_dir: /var/lib/wherry/runs
      # Not ready below 8Gi free on the claim.
      runs_floor: 8Gi
      runtime:
        # The agent runs as the receiver's UID; quoted, since it is a string.
        user: "1001"
        # A profile's env merges into this map by variable name.
        env:
          AGENT_LOG_LEVEL: info
    profiles:
      # An agent on dedicated nodes that writes its reply to a file.
      - name: dedicated
        agent: coder
        labels:
          nodes: dedicated
        config:
          node_selector:
            example.com/pool: agents
          tolerations:
            - key: example.com/agents
              operator: Equal
              value: "true"
              effect: NoSchedule
          runtime:
            image: registry.example.com/agent:1.0
            # The pod's runAsGroup, fsGroup and supplementalGroups.
            group: "1001"
            supplementary_groups: ["2001"]
            command: [/bin/sh, -c]
            args:
              - >-
                test -n "$WHERRY_MODEL" || WHERRY_MODEL=haiku;
                exec agent-run --model "$WHERRY_MODEL" < /wherry/prompt.txt 2>"$HOME/agent.err"
            # Each is both request and limit.
            resources:
              cpu: "2"
              memory: 4Gi
              ephemeral_storage: 8Gi
      # An agent on any node that tolerates the taint, with the image's
      # entrypoint.
      - name: shared
        agent: reviewer
        labels:
          nodes: shared
        config:
          tolerations:
            - key: example.com/agents
              operator: Exists
              effect: NoSchedule
          runtime:
            image: registry.example.com/reviewer:1.0
            group: "1001"
            args: [--prompt-file, /wherry/prompt.txt]
            env:
              AGENT_LOG_LEVEL: warn
            resources:
              cpu: 500m
              memory: 2Gi
```

## Agent pods

The plugin creates one Secret, one pod and one run directory per run, all
named `agent-<pool>-<run>`, the Secret and the pod in the plugin profile's
namespace and the directory in `runs/` on the runs claim. `<pool>` is the
first 12 hexadecimal digits of the SHA-256 of the pool name, as the Nomad
plugin names its jobs. Both carry the labels `wherry.io/pool`,
`wherry.io/profile`, `wherry.io/agent` and `wherry.io/run`, and the pod carries the annotation `wherry.io/attempt`
with the attempt that created it. The receiver reads the labels to decide
whether to adopt the pod.

The pod is a bare pod with `restartPolicy: Never`, so a run executes exactly
once and nothing replaces its pod. Its one container, `agent`, runs the
plugin profile's `runtime`, and the pod ends when the agent does. It mounts
two volumes:

- `runs`, the runs claim, at `/wherry`, through
  `subPathExpr: runs/$(WHERRY_RUN_DIR)`, with `WHERRY_RUN_DIR` the run's
  name in the agent's environment, so the agent sees its own run directory
  alone;
- `material`, the run's Secret, whose `items` map the key `prompt` to
  `prompt` and each `credentials.<file>` key to `files/<file>`, with
  `defaultMode: 0440`. The agent mounts `prompt` by `subPath` at
  `/wherry/prompt.txt` and `files` at `/run/wherry/credentials/files`, both
  read-only.

The pod sets `automountServiceAccountToken: false`,
`enableServiceLinks: false`, `terminationGracePeriodSeconds: 15`,
`runAsNonRoot`, the `RuntimeDefault` seccomp profile, `fsGroup` to the
runtime's GID, and, in the container, no privilege escalation and no
capabilities, which meets the Pod Security `restricted` level. The agent's
requests equal its limits. The pod runs as the namespace's `default`
service account, which carries the pull secrets for the agent image.

A pod that cannot start is reported as never started, with the platform's
message as the reason:

| Class | Cause |
| --- | --- |
| `image` | an image pull in back-off, `ErrImageNeverPull` or `InvalidImageName` |
| `config` | `CreateContainerConfigError` or `CreateContainerError` |
| `start` | the agent container ended without starting, such as a CRI-O `StartError` |
| `pod` | the pod failed before the agent started (eviction, a lost node), or is being deleted |

A first `ErrImagePull` and an unscheduled pod are waited on.

### Credential targets

The run's Secret holds each environment target under `env.<variable>` and
each file target under `credentials.<file>`, and a Secret key is at most 253
bytes, so an environment target's name is at most 249 bytes and a file
target's at most 241. Dispatch refuses a longer one. An environment target
replaces a runtime `env` variable of the same name.

The agent sees:

- the prompt in `/wherry/prompt.txt`, mode 0440, owned by root and the
  pod's fsGroup;
- each environment target in the environment variable named by the target,
  through a `secretKeyRef`, so its value never appears in the pod spec;
- each input file in `/wherry/inputs`, mode 0600, in a 0700 directory,
  both owned by the agent's UID, written by the plugin before it creates
  the pod;
- each S3 input in `/wherry/inputs`, as described in [S3 inputs](#s3-inputs);
- each file target as a file in `/run/wherry/credentials/files`, mode 0440,
  owned by root and the pod's fsGroup, so the agent reads it through its
  group and writes none. The run's Secret volume is the boundary: it is a
  tmpfs that Kubernetes mounts into the run's pod alone, and the pod's
  fsGroup is the only group that reads it. `ssh` refuses a private key that
  others can read only when the agent's user owns it, so it accepts a
  root-owned key at 0440. An agent image that uses `ssh` needs a passwd
  entry for the agent's UID, since `ssh` and `ssh-keygen` refuse a UID
  without one; on OpenShift, which picks a random UID, that takes
  nss_wrapper or a writable `/etc/passwd`. The Secret volume is a different
  filesystem from the agent's outputs, so the agent cannot hard-link a
  credential into them;
- its run directory at `/wherry`, mode 0700 and owned by its UID, and its
  outputs directory, which it creates, at `/wherry/outputs`;
- `HOME` set to `/wherry`;
- `WHERRY_INPUT_DIR`, `WHERRY_OUTPUT_DIR` and `WHERRY_CREDENTIALS_DIR` set
  to `/wherry/inputs`, `/wherry/outputs` and
  `/run/wherry/credentials/files`, the same paths the prompt names;
- `WHERRY_MODEL` set to the task's model when the task has one, and not set
  by the plugin otherwise; the command reads it from the environment;
- the variables of the plugin protocol's `AgentEnvironment`, as described
  in [Agent environment](#agent-environment);
- the runtime's `command`, `args`, `env` and `resources`.

### Runs claim

Every run of the receiver keeps its inputs, its working files and its
outputs on the runs claim, a ReadWriteMany PersistentVolumeClaim in the
namespace that the receiver's pod mounts at `runs_dir` and every agent pod
mounts at `/wherry`. The claim's storage class must provision
`ReadWriteMany` volumes, such as CephFS, NFS, EFS or Azure Files; on
OpenShift, ODF CephFS. The plugin keeps each run's directory in `runs/`
below `runs_dir`, created by the receiver's UID at 0700.

The claim's capacity is shared by every run of the receiver, so size it at
the receiver's `capacity` times the inputs and the pool's output limit per
run (`output_limit` times the files), plus headroom. The pool's output
limits are checked only at collection, so a runaway agent fills the claim
for every run on that receiver. Below `runs_floor` free the plugin profile
is not ready, and the receiver takes no new work until space returns;
`wherry_plugin_storage_free_bytes` reports the free space.

The plugin reads the agent's standard output from the logs API, which
interleaves standard error with it. A line the agent writes to standard
error in the middle of a standard output line splits that transcript line.
Send standard error elsewhere in the agent's command, such as
`2>"$HOME/agent.err"`. Kubernetes 1.32 adds `PodLogOptions.Stream` to read
standard output alone, which removes this on a later minimum platform.

The plugin follows the agent's log while it runs, and the kubelet's follow
stream carries every byte across log rotation. A follow that starts from
an offset, after a receiver restart or an adoption, reads the current log
file only. If the kubelet has rotated the log past that offset, the follow
fails with `the agent's log is shorter than the offset`, and the run
fails. Agents whose standard output can pass the kubelet's
`containerLogMaxSize`, 10 MiB by default, are exposed to this. An offset inside the rotated part that is
still below the current file's length is not detected: the follow discards
that many bytes of the current file, so the transcript loses lines without
an error.

The plugin stops an agent by deleting its pod with `gracePeriodSeconds`
15: the kubelet sends the agent `SIGTERM`, waits 15 seconds, and sends
`SIGKILL`. The plugin waits until the pod is gone. The outputs stay on the
runs claim.

The plugin collects outputs from the run's `outputs` directory on the runs
claim, through its own mount at `runs_dir`, once the agent container has
ended or the pod has gone. The plugin mounts every run's directory, and an
agent can create links in its outputs, so the plugin opens the outputs
directory as an `os.Root`, which refuses every path that leaves it, links
included, and refuses an outputs directory that is itself a link. It lists
the whole tree with `lstat`, never descending a link, and refuses the tree,
reading nothing, when it holds more entries than the receiver's limit. It
reads only regular files the listing shows within the limits, refused when
they are anything but a regular file of the listed size; links, FIFOs and
devices are listed with their type and never read. An output the receiver
streams, an S3 output or the agent's [outcome file](https://benemon.github.io/wherry/configuration#agent-contract), it streams as the receiver reads it. The agent creates the outputs directory;
without one there are no outputs. The agent's own mount reaches no other
run, since it is the run's directory alone and a link resolves inside the
agent's mount namespace.

When a run ends, the plugin deletes its Secret. The pod, its log and the
run directory stay until the [sweep](#sweep). Purging a run deletes its pod
with grace 0, which the API server removes at once, waits until the pod is
gone, then deletes its Secret and its run directory.

A pod with a `deletionTimestamp`, such as one on a lost node, is ended:
the plugin opens no log for it, reports exit code -1 when the agent has no
recorded end, and does not collect from it.

An agent container that ended other than `Completed` is reported with its
exit code and its termination reason, such as 137 and `OOMKilled`.

### Secrets

A run's Secret holds its prompt (`prompt`), each environment target
(`env.<variable>`) and each file target (`credentials.<file>`), as raw
bytes, so binary files arrive intact. The agent container mounts the prompt
and file targets from it and reads environment targets through Secret key
references.

The API server limits a Secret's values together to 1 MiB, and a key to
253 characters. Dispatch refuses a run over either limit, creating nothing:

- A file target name over 241 characters fails the run naming the target;
  the receiver calls it an input.
- A prompt and target values over 1 MiB fail the run with
  `the prompt and credentials exceed the 1 MiB Secret limit`.

Input files go to the run directory, outside the Secret. The plugin
declares an inline limit of 512 KiB, so a host sends larger inputs through
the pool's bucket.

The plugin writes the run directory, then the Secret, then the pod, only
when the run's pod does not exist yet, over any Secret an earlier attempt
left, and never rewrites either under a running pod. A run directory that
holds the inputs directory alone, with each of the run's inputs verified
and no other file, is kept; any other is removed and written again. A
receiver that restarts between the inputs and the pod finds the directory
complete, or rewrites it.

### Sweep

On each sweep the receiver requests
([plugins](https://benemon.github.io/wherry/commands#plugins)),
the plugin lists the pool's pods, and the pool's Secrets as metadata, so a
list never carries a value. It skips objects whose names are not the job
names their labels give. A pod has ended when its agent container has
ended, it never started, or it has a `deletionTimestamp`; its end time is
the `deletionTimestamp`, else the agent's end, else the pod's last
condition change.

- The plugin profile's own pods ended for 10 minutes are purged.
- Other plugin profiles' pods are never purged; a removed plugin profile's
  pods stay until an operator deletes them.
- A Secret is deleted when its pod is the plugin profile's and has ended,
  when its pod is any plugin profile's and has ended 10 minutes ago, or
  when it has no pod and is more than 10 minutes old.
- A run directory of the pool in `runs/` is removed when neither its pod
  nor its Secret remains and it has not changed for 10 minutes. A purge
  removes the directory with its pod.

An adopter can read an ended pod's log for 10 minutes. A pod stranded on a
lost node is purged 10 minutes after its deletion began.

Ownership is by plugin profile name, through the label `wherry.io/profile`.
Two receivers that carry a plugin profile of the same name over one
namespace both sweep its pods, and both
count the same purges, deletions and pending pods, so the metrics count
them twice.

### S3 inputs

The plugin fetches each S3 input on the receiver and writes it into the run
directory before it creates the pod; the presigned URL never leaves the
receiver. It appears in no Secret, pod spec, event, log line or error, and
agent pods need no route to the storage endpoint.

The plugin fetches each input with a client that follows no redirect and
reads at most one byte past the input's size, writes it to
`inputs/<name>`, mode 0600, and checks its size and SHA-256. A fetch that
fails or does not verify fails the dispatch before any pod exists, with
`fetch <name>: <reason>`, where the reason is a status code, `timeout`,
`certificate`, `unreachable`, `invalid URL`, `shorter than its size`,
`longer than its size` or `does not match its digest`, of class `fetch`.
The receiver ends the run at once, `TASK_STATE_FAILED` with `an S3 input
was not fetched, or did not match its size and SHA-256`, and does not
dispatch it again. A large S3 input crosses the receiver's
network and the runs claim's.

The receiver container makes every S3 request: the plugin's fetches of S3
inputs and the receiver's uploads of S3 outputs from the runs claim. The
receiver container trusts the storage endpoint's CA, and agent pods need
no CA for it ([Image](#image)). Against an endpoint whose certificate the
receiver does not trust, each S3 input fails with `fetch <name>:
certificate` and each S3 output with `not uploaded: request failed`.

### Agent environment

The plugin sets the variables of the plugin protocol's `AgentEnvironment`
over the runtime's `env` and removes those of
[`AgentUnset`](https://github.com/benemon/wherry/blob/main/pluginsdk/pluginsdk.go)
from it, so no system, global or environment `git` configuration reaches
the agent's clones. It removes them from the runtime's `env` only; keep
them out of agent images' `ENV`. CA and proxy variables pass through as the
operator sets them. This protects an agent that follows its prompt. An
agent that runs commands of its own can undo this.

## Metrics

The plugin speaks plugin protocol version 10, and a receiver of another
protocol version refuses it, so upgrade it with the receiver. For each
plugin profile it reports these families, which the receiver serves on its
`/metrics` with the plugin profile's name as `profile`:

| Family | Type | Labels | Counts |
| --- | --- | --- | --- |
| `wherry_plugin_platform_errors_total` | counter | `class` | failed API calls |
| `wherry_plugin_cleanup_pending` | gauge | none | the plugin profile's ended pods the latest sweep left |
| `wherry_plugin_sweep_deletions_total` | counter | `kind` | the plugin profile's pods a sweep purged (`job`), Secrets it deleted (`variable`) and run directories it removed (`directory`) |
| `wherry_plugin_placement_seconds` | histogram | none | time from a Dispatch that creates the pod to the plugin seeing its agent container running |
| `wherry_plugin_storage_free_bytes` | gauge | none | the free bytes of the runs claim at `runs_dir` |

`wherry_plugin_storage_free_bytes` and the `directory` kind need a receiver
that declares them; an older receiver drops their samples and counts each
in `wherry_metrics_dropped_total`.

An API call's class comes from its status: 401 and 403 are `denied`, 409
`conflict`, 408, 504 and the `Timeout` and `ServerTimeout` reasons
`timeout`, and 429 and other 5xx codes `unavailable`. A call that cannot
connect is `unavailable`, one that runs past its deadline `timeout`, and
any other failure `other`. A 404, which the plugin reads as absence, and a
call the receiver cancels count nothing.

A failed call the plugin returns reaches the receiver with its operation,
class and the API's text, and the receiver logs it redacted. A failed call
the plugin handles itself, such as a poll, logs one warning, `kubernetes call failed`, naming the
plugin profile under the key `profile`, the operation and the class; the line holds no URL, token or
response body. API warnings, such as PodSecurity notices, are logged as
`kubernetes warning`.

The plugin's client allows 50 requests a second with bursts of 100. Each
run polls its pod every second or two, besides its log stream and the
sweep; the API server's priority and fairness protects the server.

## Agent network reach

The operator guide's
[Agent network reach](https://benemon.github.io/wherry/operations#agent-network-reach)
says what an agent job's egress policy denies and allows.

Agent pods carry no API token and no Service variables, and need no
ingress: the plugin reads their logs through the API server and their
outputs on the runs claim.
They need no route to the storage endpoint. Until an egress policy is set,
agent pods have the cluster's default egress.

To restrict agents, put a default-deny egress NetworkPolicy in the
namespace with allow rules for what agents reach (DNS, the model API, git
hosts). Agent pods share the namespace with the receiver, so select them
by the label `wherry.io/pool`, which the receiver's pod does not carry:

```yaml
podSelector:
  matchExpressions: [{key: wherry.io/pool, operator: Exists}]
```

## Run the receiver in the cluster

The receiver runs as a one-replica Deployment with its own service account,
and its agent pods run beside it in the same namespace.
[`examples/kubernetes/receiver.yaml`](../examples/kubernetes/receiver.yaml) holds its manifests: the
ServiceAccount, the Role and its RoleBinding ([RBAC](#rbac)), the state
PersistentVolumeClaim, the runs claim `wherry-runs`, the Deployment and the
metrics Service. The Secret
`wherry-receiver-bundle` holds the bundle, and the ConfigMap
`wherry-receiver-config` holds the receiver's local configuration,
[`examples/kubernetes/config.yaml`](../examples/kubernetes/config.yaml).
Kubernetes and OpenShift take the
same manifests; only the namespace, the image and the Kafka bootstrap
address differ.

### Image

[`examples/kubernetes/Containerfile`](../examples/kubernetes/Containerfile) builds the receiver image from the repository root. From a checkout of
the tag `kubernetes/<version>`, where `<version>` carries its `v`, such as
`v0.1.0`:

```sh
make image VERSION=<version> WHERRY_VERSION=<wherry version>
```

`make image` runs Podman; `CONTAINER=docker` runs Docker instead. The
build compiles the plugin from the checkout, downloads the
wherry release archive for the target architecture with its
`checksums.txt` and `checksums.txt.sigstore.json`, and verifies the
checksums' signature, then the archive's SHA-256 against them:

```sh
cosign verify-blob checksums.txt --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github\.com/benemon/wherry/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

The image is based on `registry.access.redhat.com/ubi9/ubi-minimal`, whose
CA bundle the receiver's S3 uploads and the plugin's S3 fetches trust. It
holds `/usr/local/bin/wherry` and
`/usr/local/libexec/wherry/plugins/wherry-plugin-kubernetes`, each mode
0755 and owned by root. It
runs as UID 1001 and group 0, with the entrypoint `wherry`; its writable
paths are volumes whose mount points, `/var/lib/wherry` and `/tmp`, group 0
can write. It holds no Nomad plugin. Agent pods run the agent's image
alone, so an upgrade of the plugin is a change of the receiver image.

For a storage endpoint under a private CA, the receiver reads the system
bundle `/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem`, which takes no
configuration. Supply it one of two ways:

- On OpenShift, mount the cluster's
  [injected trusted CA bundle](https://docs.redhat.com/en/documentation/openshift_container_platform/4.18/html/networking/enable-cluster-wide-proxy#certificate-injection-using-operators_config-cluster-wide-proxy),
  which holds the system roots and the cluster proxy's `trustedCA`, so the
  private CA goes in the proxy's `trustedCA`. A ConfigMap with the inject
  label gets the bundle under the key `ca-bundle.crt`:

  ```yaml
  apiVersion: v1
  kind: ConfigMap
  metadata:
    name: wherry-receiver-ca
    labels: {config.openshift.io/inject-trusted-cabundle: "true"}
  ```

  Mount it read-only over the image's bundle directory:

  ```yaml
  volumeMounts:
    - {name: ca, mountPath: /etc/pki/ca-trust/extracted/pem, readOnly: true}
  volumes:
    - {name: ca, configMap: {name: wherry-receiver-ca, items: [{key: ca-bundle.crt, path: tls-ca-bundle.pem}]}}
  ```

- On other Kubernetes, put the full bundle, the system roots and the
  private CA, in a ConfigMap under the key `ca-bundle.crt` and mount it the
  same way; the mount replaces the image's bundle, so a ConfigMap holding
  the private CA alone leaves the receiver trusting nothing else. Or build
  a derived image that adds the CA with `update-ca-trust`.

[`examples/kubernetes/receiver.yaml`](../examples/kubernetes/receiver.yaml) holds these lines commented
out; a storage endpoint under a public CA needs none of them.

No image is published, since an image couples a wherry release and a
plugins release. Push the image you build to a registry the cluster pulls
from.

### Deploy

1. Create the receiver's namespace, enforcing Pod Security `restricted`:

   ```sh
   kubectl create namespace wherry
   kubectl label namespace wherry pod-security.kubernetes.io/enforce=restricted
   ```

2. Add the receiver with `wherry admin receiver add`, as the operator guide
   says, with a `kubernetes` plugin whose `config` sets the receiver's
   `namespace`, `runs_claim: wherry-runs` and
   `runs_dir: /var/lib/wherry/runs` ([Settings](#settings)), and the
   receiver's plugin profiles. The image builds the plugin from the
   checkout, so its pin is the SHA-256 of the image's binary,
   `--plugin-sha256 kubernetes=HEX` from
   `podman run --rm --entrypoint sha256sum <image> /usr/local/libexec/wherry/plugins/wherry-plugin-kubernetes`.
   The in-cluster receiver's configuration names no platform credential;
   the service account supplies the token.

3. Apply the receiver with [`examples/kubernetes/apply.sh`](../examples/kubernetes/apply.sh):

   ```sh
   NAMESPACE=wherry IMAGE=registry.example.com/wherry-receiver:<version> \
     BOOTSTRAP=wherry-kafka-kafka-bootstrap.kafka.svc:9093 BUNDLE=kubernetes-1.bundle \
     examples/kubernetes/apply.sh
   kubectl rollout status deployment/wherry-receiver -n wherry
   ```

[`examples/kubernetes/apply.sh`](../examples/kubernetes/apply.sh) creates or replaces the Secret
`wherry-receiver-bundle` from `BUNDLE` with its `broker` replaced by
`BOOTSTRAP` ([Bundle and Kafka](#bundle-and-kafka)), never writing the
bundle to disk, and the ConfigMap `wherry-receiver-config` from the
receiver's local configuration,
[`examples/kubernetes/config.yaml`](../examples/kubernetes/config.yaml),
under the key `config.yaml`. It then applies `receiver.yaml` with the
namespace and the image substituted, all with server-side apply. Run it
again to change any of them, and restart the Deployment to read a new
bundle or local configuration.

The receiver runs as
`wherry receiver run --config-file /etc/wherry/config/config.yaml`, with
the ConfigMap mounted read-only at `/etc/wherry/config`.

- One replica with the `Recreate` strategy: a receiver is one principal
  with one key pair, and a rolling update would briefly run two.
- `terminationGracePeriodSeconds: 30`, with `drain_timeout: 25s` in the
  local configuration, so held runs go on for up to 25 seconds after
  `SIGTERM` before the receiver releases them for adoption.
- The receiver's service account token is mounted, and the plugin, a child
  process of the receiver, uses it.
- The pod sets no `runAsUser`, so it runs as the image's UID 1001 with
  group 0.
- `/tmp` is a writable memory volume, and `TMPDIR` and `HOME` point at it.
  The root filesystem is read-only, and the plugin creates its socket for
  the receiver in `$TMPDIR`; without it the plugin cannot start, and the
  receiver exits with `profile <name>: plugin ...` and restarts in a loop.
- State lives on the PVC, in the `state` subdirectory, which the receiver
  creates at 0700 with its signing key at 0600. The volume's root must be
  writable by the receiver. The pod sets no fsGroup, so use a storage
  class whose volumes UID 1001 or group 0 can write, such as kind's
  local-path volumes, which are mode 0777. A restarted pod mounts the same
  volume and is the same
  receiver. Losing the PVC loses the receiver's identity; provision it
  again under a new name, as the operator guide says for a lost state
  directory.
- The runs claim `wherry-runs` is `ReadWriteMany` and mounted at
  `/var/lib/wherry/runs`. Its storage class's default applies; set one that
  provisions `ReadWriteMany` volumes ([Runs claim](#runs-claim)). The
  volume's root must be writable by the receiver's UID, as for the state
  volume.
- The bundle Secret is mounted at 0440, owned by group 0, the receiver's
  group.
- The receiver has no health endpoint, so the pod has no probes.
- The metrics address, `metrics_addr` in the local configuration, binds
  the pod's own addresses, with no TLS or
  authentication. Scrape the Service `wherry-receiver-metrics`, port
  `metrics`, path `/metrics`. Whoever reads the scrape sees every tenant of
  the pool; where the CNI enforces NetworkPolicy, admit only the scraper to
  port 9464.
- Under a default-deny egress policy, allow the receiver's pod
  (`app: wherry-receiver`) the Kafka brokers (TCP 9093), the cluster's DNS,
  the API server and the storage endpoint. The API server usually runs on
  the host network, where a namespace selector does not reach it; allow the
  addresses of the `default/kubernetes` Endpoints.

### Bundle and Kafka

The pool's Kafka runs in the same cluster under Strimzi, with Kafka 4.3, share groups enabled, and a
`tls` internal listener with `authentication: scram-sha-512` and
`authorization: simple`. The operator guide's Strimzi notes apply. Where
the listener sets `networkPolicyPeers`, they admit the receiver's
namespace.

The bundle `wherry admin receiver add` writes names `WHERRY_TRUST_BROKER`,
the address hosts use. `apply.sh` replaces it with `BOOTSTRAP`, the
internal bootstrap address, such as
`wherry-kafka-kafka-bootstrap.kafka.svc:9093`; Strimzi's broker
certificates carry the internal Service names. The bundle's `ca` is the
Strimzi cluster CA (`<cluster>-cluster-ca-cert`, key `ca.crt`).

Strimzi renews its cluster CA, and nothing renews the receiver's copy:

1. When the renewal begins, Strimzi keeps the old certificate in
   `<cluster>-cluster-ca-cert` beside the new `ca.crt`. Write both
   certificates into the bundle's `ca`.
2. Run `apply.sh` with the new bundle and
   `kubectl rollout restart deployment/wherry-receiver -n wherry`.
3. Once Strimzi has removed the old certificate, remove it from the bundle
   and run both again.

### Kubernetes and Nomad together

Put the in-cluster receiver in a pool of its own, and Nomad-hosted receivers in
another, and entitle the tenant to both. A submit places the task in a pool
whose live plugin profiles satisfy its selector, so a task meant for one platform is
never delivered to the other's receivers. In one pool, a receiver with no
matching plugin profile takes the task and hands it back, spending its delivery
budget, and a task both platforms accept can be redelivered to a receiver that
cannot see its running job and start a second one.

A run is adopted only by a receiver with the same plugin profile. For a Kubernetes
run that is the in-cluster receiver itself, restarted: the new pod mounts the
same state volume, is the same principal, and finds the run's pod.

## OpenShift

The image, `receiver.yaml`, `config.yaml` and `apply.sh` are the same on OpenShift, which
ships `kubectl` beside `oc`. What differs:

- Create the namespace with `oc new-project wherry`. OpenShift sets the
  namespace's Pod Security labels itself and admits pods by SCC: the
  receiver's pod and agent pods run under restricted-v2. Do not allow the
  receiver's service account any SCC beyond restricted-v2; OpenShift admits
  a pod against the SCCs its creator may use as well as its service
  account's, so `anyuid` or `privileged` would admit agent pods running as
  root.
- restricted-v2 gives every container of a pod the namespace's first UID,
  with group 0, so the receiver and its agents share a UID. Leave
  `runtime.user` and `runtime.group` unset; a `user` or a `group` outside
  the namespace's ranges is refused at admission.
- restricted-v2 also sets the pod's fsGroup, which the bundle file, the
  state volume and an agent's prompt and credential files take. `fsGroupChangePolicy: OnRootMismatch` keeps the
  kubelet from adding group access to the receiver's signing key on every
  mount, which would stop the receiver. If the namespace's fsGroup ever
  changes, the receiver stops with `chmod 600 it`; fix the modes.
- For a storage endpoint under a private CA, mount the injected trusted CA
  bundle in the receiver ([Image](#image)).
- Streams for Apache Kafka serves as Strimzi.
- Push the receiver and agent images to the internal registry, into the
  receiver's namespace, through the registry's route, since the Service
  name resolves only inside the cluster. Pods in the namespace pull its
  images by the Service name with no further grant, such as
  `IMAGE=image-registry.openshift-image-registry.svc:5000/wherry/wherry-receiver:<version>`:

  ```sh
  registry=$(oc get route default-route -n openshift-image-registry -o jsonpath='{.spec.host}')
  oc whoami -t | podman login -u "$(oc whoami)" --password-stdin "$registry"
  podman push wherry-receiver:<version> "$registry/wherry/wherry-receiver:<version>"
  ```

- With user workload monitoring enabled, a ServiceMonitor selecting
  `app: wherry-receiver`, port `metrics`, scheme `http`, scrapes the
  receiver.
- An `EgressFirewall` with `dnsName` rules restricts egress by host name.
  It applies to every pod of the namespace, the receiver's included, so its
  rules also allow the receiver's storage endpoint.
- Some items need a cluster administrator: enabling user workload
  monitoring, an `EgressFirewall` (OVN-Kubernetes only), and raising
  `containerLogMaxSize`, a `KubeletConfig` that rolls every node of its
  MachineConfigPool.

| Concern | Kubernetes | OpenShift |
| --- | --- | --- |
| Agent UID | nothing assigns one; set `runtime.user` | restricted-v2 gives every container the namespace's first UID; leave `runtime.user` unset |
| Receiver UID | the image's, 1001, with group 0 | the namespace's, with group 0 |
| fsGroup | `runtime.group`; agent Secret files are root-owned at mode 0440 in that group; the receiver's bundle is group 0 | restricted-v2 sets it; Secret files take its group |
| Admission | Pod Security Admission; label the namespace `pod-security.kubernetes.io/enforce: restricted`, which the pods meet | SCCs of the creator and the pod's service account; a refusal is 403 at create |
| Image user | the image's own user | an arbitrary UID with GID 0; agent images keep their writable paths group-0 writable |
| Node selection | `node_selector` alone | a project's `openshift.io/node-selector` merges with `node_selector`; a conflict is 403 at create |
| Registry | any registry; pull secrets on the namespace's `default` service account | the internal registry, in the receiver's namespace |
| Egress by name | a DNS-aware CNI policy, such as Cilium FQDN | OVN-Kubernetes `EgressFirewall` with `dnsName` |
| DNS for NetworkPolicy | the cluster's DNS namespace and port | `openshift-dns`, pods on port 5353 |
| API server for NetworkPolicy | `ipBlock` of the `default/kubernetes` Endpoints, port per distribution | the same, port 6443 |
| Container logs | the kubelet's `containerLogMaxSize`, 10 MiB by default | CRI-O, `containerLogMaxSize` 50 MiB by default |
| Metrics | any Prometheus scrape configuration | user workload monitoring and a ServiceMonitor |

## Tests

```sh
make test
```

The unit tests run the plugin against a fake API server, with a temporary
directory as the runs claim. The tests that need a cluster run when
`WHERRY_KUBERNETES_DEV_ADDR` names an API server, and skip otherwise. They
write the run directories as the receiver does, so they run on a machine
that mounts the runs claim's file system, as the UID agent pods run as.
They read:

| Variable | Content |
| --- | --- |
| `WHERRY_KUBERNETES_DEV_TOKEN_FILE` | a token of a service account bound to the plugin's Role in the namespace alone ([RBAC](#rbac)) |
| `WHERRY_KUBERNETES_DEV_CA_FILE` | the API server's CA bundle |
| `WHERRY_KUBERNETES_DEV_NAMESPACE` | the namespace for agent pods |
| `WHERRY_KUBERNETES_DEV_RUNS_CLAIM` | the runs claim in the namespace |
| `WHERRY_KUBERNETES_DEV_RUNS_DIR` | where the machine mounts the runs claim's file system |
| `WHERRY_KUBERNETES_DEV_USER` | optional; `runtime.user`, the machine's UID, for a cluster that assigns no UID |

They include the protocol's conformance suite,
`github.com/benemon/wherry/pluginsdk/pluginsdktest`, in process and against
the built plugin, and `TestRoleSuffices`, which runs the suite and checks
each pod it dispatches. They set `runtime.user` from
`WHERRY_KUBERNETES_DEV_USER` and leave it unset without it, so they run on
OpenShift without it and on other clusters with it, such as
`WHERRY_KUBERNETES_DEV_USER=1001`. The other tests that need a cluster do
the same.

`TestRoleSuffices` checks each pod by where it runs:

| Cluster | A pod carries | The test requires |
| --- | --- | --- |
| OpenShift | the annotation `openshift.io/scc` | the annotation's value is `restricted-v2` |
| Kubernetes | no `openshift.io/scc` annotation | the namespace's label `pod-security.kubernetes.io/enforce` is `restricted`, and the pod was admitted |

### End to end on kind

[`test/e2e`](test/e2e) runs a receiver from the receiver image and the
plugin together on a kind cluster, as the `e2e` workflow does for every
change to `kubernetes/`, the root `Makefile` or the workflow. Each step is
a script; with Docker, kind and kubectl on a Linux machine, and the wherry
release the workflow pins:

```sh
export WHERRY_VERSION=$(sed -n 's/^  WHERRY_VERSION: //p' .github/workflows/e2e.yml)
for step in kubernetes/test/e2e/0*.sh; do "$step" || { kubernetes/test/e2e/collect.sh; break; }; done
```

1. `01-cluster.sh` replaces the kind cluster `wherry-e2e`, installs
   Strimzi and a single-node Kafka 4.3 (KRaft, share groups, TLS with
   SCRAM-SHA-512, simple authorization, `kafka-admin` a super user and
   `wherry.operator`), and maps its external listener to `localhost`.
2. `02-wherry.sh` fetches the wherry CLI of `WHERRY_VERSION` through the
   receiver image's verifying fetch stage, and builds the receiver image
   with `make image` and the fake agent's image.
3. `03-provision.sh` runs `wherry admin` from the machine: the pool, the
   tenant, the host and the receiver, from the receiver configuration
   directory `test/e2e/receiver`: `receiver.yaml`,
   `plugins/kubernetes/plugin.yaml`, and the plugin profiles
   `plugins/kubernetes/profiles/10-fake.yaml`, which runs a fake agent, and
   `20-broken.yaml`, which runs a missing image, pinned with
   `--plugin-sha256` to the plugin binary in the receiver image. It runs
   `wherry admin check`, writes the host's configuration, `config.yaml`,
   with a generated dummy credential and runs `wherry init`.
4. `04-deploy.sh` loads the receiver image and the fake agent's image into
   kind, creates the runs claim's volume,
   [`runs.yaml`](test/e2e/runs.yaml), a hostPath directory on the single
   node marked `ReadWriteMany` and owned by UID 1001, and deploys the
   receiver as [Deploy](#deploy) says, into a namespace enforcing Pod
   Security `restricted`.
5. `05-runs.sh` submits, as the host, a run it cancels, a plain run, a
   credentialed run with an input file and an SSH private key, a run with
   an input file and a run whose image is missing, and waits for each with
   `wherry wait`, keeping its `wherry logs`. It checks each state by
   `wait`'s exit code, the result and output, the credentials' mode 0440
   and root owner, `ssh-keygen -y` accepting the key as the agent, the
   input's mode 0600 and SHA-256, the start failure's reason in the
   receiver's log, the credentials' absence from the log, pods and events,
   and the sweep's removal of the cancelled run's Secret and pod.

`collect.sh` writes the receiver's log and the cluster's state, but no
Secret, to `artifacts` in the state directory, `$E2E_WORK`, by default
`$TMPDIR/wherry-e2e`.
