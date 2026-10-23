# wherry-plugin-kubernetes

The wherry adapter plugin for Kubernetes. A wherry receiver starts it from
its plugin directory for adapters of type `kubernetes`, and runs each of the
receiver's runs as one pod and one Secret in a namespace. One plugin
process hosts every `kubernetes` adapter of a receiver.

Kubernetes 1.31 or later is the platform. The plugin, the receiver image
and the receiver's manifests also run unchanged on OpenShift 4.18 or later;
[OpenShift](#openshift) lists what differs there.

## Build

The plugin builds against the tagged adapter protocol module,
`github.com/benemon/wherry/adapter`. From a clone of this repository:

```sh
make build
```

This writes `wherry-plugin-kubernetes` and `wherry-k8s-helper` at the
repository root. `wherry-k8s-helper` is the helper agent pods run; it uses
the Go standard library alone. `make check` runs the CI gates,
`make dist PLUGIN=kubernetes VERSION=<version>` writes release archives,
which hold both binaries, and `checksums.txt` into `dist`, and
`make image VERSION=<version> WHERRY_VERSION=<wherry version>` builds the
receiver image ([Image](#image)).

## Install

In a cluster, run the receiver from the receiver image, which holds wherry,
the plugin and the helper; see
[Run the receiver in the cluster](#run-the-receiver-in-the-cluster).

On a receiver outside the cluster:

1. Copy `wherry-plugin-kubernetes` into the receiver's plugin directory:

   ```sh
   install -o root -g root -m 0755 wherry-plugin-kubernetes /usr/local/libexec/wherry/plugins/
   ```

2. Start the receiver with `--plugin-dir` naming that directory:

   ```sh
   wherry receiver run --bundle-file FILE --state-dir DIR --plugin-dir /usr/local/libexec/wherry/plugins
   ```

Agent pods still run the helper from `helper_image`, which must be the
receiver image at the receiver's tag.

The receiver checks the plugin directory and binary before it runs them;
see [`wherry receiver`](https://github.com/benemon/wherry/blob/main/docs/reference.md).
The plugin is not run by hand.

## Credentials

The plugin takes an adapter's API token from the receiver bundle's
`platform` entry for the adapter's name. Without one, and with `address`
empty, it uses the receiver pod's service account: the API server from
`KUBERNETES_SERVICE_HOST` and `KUBERNETES_SERVICE_PORT`, and the token and
CA the kubelet projects into
`/var/run/secrets/kubernetes.io/serviceaccount`. The plugin rereads the
token file, so the kubelet's rotation of it reaches the plugin. With
`address` set and no platform entry, the receiver exits with
`address needs a platform credential`.

| Variable | Effect |
| --- | --- |
| `KUBERNETES_SERVICE_HOST` | API server host when `address` is empty |
| `KUBERNETES_SERVICE_PORT` | API server port when `address` is empty |

The plugin reads no kubeconfig and no `KUBECONFIG`. A kubeconfig can name
exec credential plugins and auth providers, which run commands on the
receiver, and the plugin needs only an address, a CA and a token.

The token needs the [RBAC](#rbac) grants in the adapter's namespace.

## RBAC

Agent pods run in the receiver's own namespace. The receiver's service
account holds the cluster role `admin` in that namespace alone, through the
RoleBinding in [`deploy/receiver.yaml`](deploy/receiver.yaml), and nothing
outside it.

The plugin's calls, all in that namespace, need:

- `pods` create, get, list, delete: create a run's pod, read it for
  lookups and polls, list it for the sweep, and delete it for a stop's last
  resort, a dispatch or lookup cut short, a pod whose containers run as
  different users, and a purge.
- `pods/log` get: stream the agent's standard output.
- `pods/exec` create and get: signal the agent, list and read its outputs,
  end the outputs holder, and deliver S3 inputs. A WebSocket exec is an
  upgraded GET that Kubernetes 1.31 also authorises as `create`, and the
  SPDY fallback is a POST, so both verbs are needed.
- `secrets` create, get, update, list, delete: write a run's material,
  delete it when the run ends, and list its metadata for the sweep.
- `namespaces` get on the namespace: readiness.

`admin` grants these and more. In the namespace, the receiver, or anyone
holding its token, can:

- read and write every Secret: the bundle Secret, which holds the
  receiver's Kafka password, and every run's prompt and material;
- run commands in every pod, the receiver's own and each agent's, read an
  agent's environment (the model credential) and its credentials
  directory, and use that pod's egress;
- create, change and delete pods, Deployments, Services,
  PersistentVolumeClaims, ConfigMaps, ServiceAccounts and most other
  namespaced objects, the receiver's own Deployment and state volume among
  them;
- impersonate the namespace's service accounts;
- create Roles and RoleBindings in the namespace for permissions it holds.

It cannot write the namespace itself, its ResourceQuotas or its
LimitRanges, and it cannot reach other namespaces, nodes or other
cluster-scoped objects. Pods it creates are admitted by the namespace's Pod
Security level, so with the namespace at `restricted` none runs privileged
or as root.

The API does not restrict the command an exec runs, and the outputs holder
never mounts the credentials, but a process in it can read them through
`/proc/<agent pid>/root` while the agent runs, since both share a UID and a
process namespace. The holder's mounts are hygiene, not isolation.

Do not:

- bind `admin` with a ClusterRoleBinding, or in any other namespace;
- run anything but the receiver and its agent pods in the namespace, since
  `admin` there reaches every Secret and pod;
- lower the namespace's Pod Security level below `restricted`;
- grant RBAC to the namespace's `default` service account. Agent pods run
  as it with no token mounted, and no agent may hold any API right.

## Settings

An adapter's `config` section in the receiver configuration:

| Field | Default | Content |
| --- | --- | --- |
| `address` | in-cluster | API server URL; must be `https://` |
| `ca_file` | the service account's CA when `address` is empty; the system roots otherwise | CA bundle for the API, a path on the receiver |
| `namespace` | required | namespace for agent pods and their Secrets; a DNS-1123 label |
| `credential_env` | none | environment variable the agent receives the credential in |
| `node_selector` | none | node labels agent pods require |
| `tolerations` | none | taints agent pods tolerate |
| `helper_image` | required | image, with no whitespace, holding `wherry-k8s-helper` at `/usr/local/libexec/wherry/`; the receiver image at the receiver's tag |
| `runtime` | required | the agent container's definition |

`credential_env` is required when the adapter sets `credential`, matches
`[A-Za-z_][A-Za-z0-9_]*`, and is none of the variables the plugin sets for the
agent. `node_selector` keys and values are
Kubernetes label keys and values. Each toleration has `key`, `operator`
(`Equal`, or `Exists` without a `value`), `value` and `effect`
(`NoSchedule`, `PreferNoSchedule` or `NoExecute`). Unknown fields are
errors. The plugin checks the section when the receiver configures the
adapter, and the receiver exits with `adapter <name>: ...` on an error.

`runtime` has these fields:

| Field | Content |
| --- | --- |
| `image` | the agent image, with no whitespace; required |
| `command` | the container's command; unset, the image's entrypoint |
| `args` | the container's arguments |
| `env` | environment variables for the agent; names match `[A-Za-z_][A-Za-z0-9_]*` |
| `resources` | `cpu` (default `500m`), `memory` (default `2Gi`) and `ephemeral_storage` (default unset), Kubernetes quantities, each set as both request and limit |
| `work_limit` | a quantity, the size limit of the agent's `/wherry` volume; unset, no limit |
| `user` | `UID` or `UID:GID`, a string; unset, the platform assigns one |

`user` takes positive integers below 2^31 with no leading zeros; root (0)
is refused. Quote it in YAML, such as `user: "1001"`. Kubernetes assigns no
UID, so set it. The resource defaults are starting points; set them for the
agent.

The plugin sets `HOME`, the `WHERRY_*` variables and the credential
variable over `env`, so a variable of these names in `env` is overwritten.

An adapter is ready when the API answers with the namespace, and the
namespace is active. This proves the address, the token and the namespace.
Readiness checks nothing else:

- Nodes. Nodes are cluster-scoped, and the plugin does not read the
  cluster's topology. A pod no node can take, such as one whose
  `node_selector` no node matches, waits until the run's deadline, holding
  one of the receiver's `capacity` slots, and the run then fails
  `timeout`.
- Quota. A ResourceQuota in the namespace that is full refuses pod
  creation with 403. Each refusal counts a `dispatch` start failure, and
  the task spends its delivery budget.
- Images. An agent or helper image that cannot be pulled shows only at
  dispatch, as a pod that never started with class `image`.

An adapter file in a receiver configuration directory:

```yaml
name: kubernetes
platform: kubernetes
agent: coder
credential: model-api-key
labels:
  platform: kubernetes
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

with `platforms/kubernetes.yaml`, whose namespace is the receiver's own:

```yaml
name: kubernetes
type: kubernetes
config:
  namespace: wherry
  credential_env: MODEL_API_KEY
  helper_image: registry.example.com/wherry-receiver:<version>
```

### Receiver configuration

This receiver configuration file holds one platform and two adapters.
Each adapter's `config` takes the platform's `config` keys at the top level,
and an adapter that sets one of them again is an error. With `address`
set, the bundle holds a platform credential for the platform, which
`wherry admin receiver add --platform-credential-file kubernetes=FILE`
writes.
[`testdata/receiver.yaml`](testdata/receiver.yaml) holds this file, and
`TestReadmeReceiverConfigures` configures each adapter from it through
the plugin's decoding and checks.

```yaml
capacity: 4
platforms:
  # Every adapter on this platform takes these keys into its config, and
  # repeats none of them.
  - name: kubernetes
    type: kubernetes
    config:
      # Set, the bundle holds a platform credential for this platform. Unset,
      # the receiver pod's service account applies.
      address: https://kubernetes.example.com:6443
      # A path on the receiver.
      ca_file: /etc/wherry/kubernetes-ca.pem
      # Holds the agent pods and their Secrets; the token holds admin here alone.
      namespace: agents
      # Required while any adapter on the platform sets a credential.
      credential_env: MODEL_API_KEY
      # The receiver image at the receiver's tag.
      helper_image: registry.example.com/wherry-receiver:1.0.0
adapters:
  # An agent on dedicated nodes that takes a credential and writes its reply
  # to a file.
  - name: dedicated
    platform: kubernetes
    agent: coder
    credential: model-api-key
    output: file
    reply_file: reply.md
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
        # Every container of the pod runs as this UID; quoted, since it is a
        # string.
        user: "1001:1001"
        command: [/bin/sh, -c]
        args:
          - >-
            test -n "$WHERRY_MODEL" || WHERRY_MODEL=haiku;
            exec agent-run --model "$WHERRY_MODEL" < /wherry/prompt.txt 2>"$HOME/agent.err"
        env:
          AGENT_LOG_LEVEL: info
        # Each is both request and limit.
        resources:
          cpu: "2"
          memory: 4Gi
          ephemeral_storage: 8Gi
        # Below ephemeral_storage, so the volume limit applies first.
        work_limit: 6Gi
  # An agent on any node that tolerates the taint, with the image's
  # entrypoint.
  - name: shared
    platform: kubernetes
    agent: reviewer
    output: claude
    labels:
      nodes: shared
    config:
      tolerations:
        - key: example.com/agents
          operator: Exists
          effect: NoSchedule
      runtime:
        image: registry.example.com/reviewer:1.0
        user: "1002"
        args: [--prompt-file, /wherry/prompt.txt]
        env:
          AGENT_LOG_LEVEL: warn
        resources:
          cpu: 500m
          memory: 2Gi
```

## Agent pods

The plugin creates one Secret and one pod per run, both named
`agent-<pool>-<run>` in the adapter's namespace. `<pool>` is the
first 12 hexadecimal digits of the SHA-256 of the pool name, as the Nomad
plugin names its jobs. Both carry the labels `wherry.io/pool`,
`wherry.io/adapter`, `wherry.io/agent` and `wherry.io/run`, and the pod carries the annotation `wherry.io/attempt`
with the attempt that created it. The receiver reads the labels to decide
whether to adopt the pod.

The pod is a bare pod with `restartPolicy: Never`, so a run executes exactly
once and nothing replaces its pod. It has three containers:

- `inputs`, an init container running `wherry-k8s-helper`, copies the
  run's material from its Secret into the pod's volumes, and takes the S3
  inputs the plugin streams in.
- `agent` runs the adapter's `runtime`.
- `outputs` runs `wherry-k8s-helper` as a holder that stays up after the
  agent ends. The plugin execs the helper in it to signal the agent, and
  to list and read the agent's outputs, since Kubernetes has no call that
  stops one container and a deleted pod's volumes are gone.

The pod sets `automountServiceAccountToken: false`,
`enableServiceLinks: false`, `shareProcessNamespace: true`,
`terminationGracePeriodSeconds: 15`, `runAsNonRoot`, the `RuntimeDefault`
seccomp profile, and, in every container, no privilege escalation and no
capabilities, which meets the Pod Security `restricted` level. The helper
containers have read-only root filesystems and
request 10m CPU and 16Mi memory, limited to 200m and 64Mi. The agent's
requests equal its limits. The plugin sets no `ephemeral-storage` on the
helper containers; where a ResourceQuota covers `ephemeral-storage`, give
the namespace a LimitRange default. The pod runs as the namespace's
`default` service account, which carries the pull secrets for the agent
and helper images.

Every container must run as one UID, since the helper signals the agent's
processes and owns its credential files; `runtime.user` gives every
container its UID. If the admitted pod's containers
run as different users, or any has none, the plugin deletes the pod and its
Secret and reports nothing created with
`agent pod containers run as different users; set runtime.user`, so the
task is redelivered until it spends its delivery budget, as under Quota.

A pod that cannot start is reported as never started, with the platform's
message as the reason:

| Class | Cause |
| --- | --- |
| `image` | an image pull in back-off, `ErrImageNeverPull` or `InvalidImageName` |
| `config` | `CreateContainerConfigError` or `CreateContainerError` |
| `start` | the agent container ended without starting, such as a CRI-O `StartError` |
| `fetch` | an S3 input was not fetched |
| `setup` | the `inputs` container failed otherwise, including a helper of another protocol version |
| `pod` | the pod failed before the agent started (eviction, a lost node), or is being deleted |

A first `ErrImagePull` and an unscheduled pod are waited on.

The agent sees:

- the prompt in `/wherry/prompt.txt`;
- the credential in the environment variable `credential_env` names,
  through a `secretKeyRef`, so its value never appears in the pod spec;
- each input file in `/wherry/inputs`, mode 0600, in a 0700 directory;
- each S3 input in `/wherry/inputs`, as described in [S3 inputs](#s3-inputs);
- each task credential as a file in `/run/wherry/credentials/files`, mode
  0600, in a 0700 directory, both owned by the agent's UID, so `ssh`
  accepts a private key. The credentials are on a memory-backed volume,
  which the agent mounts read-only and which is a different filesystem from
  its outputs, so the agent cannot hard-link a credential into them;
- its outputs directory, which it creates, at `/wherry/outputs`;
- `HOME` set to `/wherry`;
- `WHERRY_INPUT_DIR`, `WHERRY_OUTPUT_DIR` and `WHERRY_CREDENTIALS_DIR` set
  to `/wherry/inputs`, `/wherry/outputs` and
  `/run/wherry/credentials/files`, the same paths the prompt names;
- `WHERRY_MODEL` set to the task's model when the task has one, and not set
  by the plugin otherwise; the command reads it from the environment;
- the variables of the adapter protocol's `AgentEnvironment`, as described
  in [Agent environment](#agent-environment);
- the runtime's `command`, `args`, `env` and `resources`.

`/wherry` has no size limit unless `work_limit` sets one. An agent that
writes outputs of several GiB on a node short of disk can trigger the
kubelet's disk-pressure eviction, which kills the pod and loses the
outputs. Where nodes are small, set `work_limit` to the pool's output limit
plus headroom. `/wherry` is a disk-backed `emptyDir`, so it also counts
against `resources.ephemeral_storage` when that is set: `work_limit` bounds
the agent's working files, `ephemeral_storage` the container's whole disk use,
and an `ephemeral_storage` below `work_limit` evicts first.

The plugin reads the agent's standard output from the logs API, which
interleaves standard error with it. A line the agent writes to standard
error in the middle of a standard output line corrupts the JSON result
line, and the run ends `agent ended without a result`. Send standard
error elsewhere in the agent's command, such as `2>"$HOME/agent.err"`.
Kubernetes 1.32 adds `PodLogOptions.Stream` to read standard output alone,
which removes this trap on a later minimum platform.

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

The plugin stops an agent by sending `SIGTERM` to every process of the
pod but the holder, through the holder, and waiting up to 15 seconds for
the agent container to end; then `SIGKILL` and 15 seconds more. A signal
whose exec fails in transport may still have been delivered, so the plugin
waits for it as for a delivered one. If the agent still runs, or the holder
refuses to signal it, the plugin deletes the pod at once, logs `agent outlived SIGKILL; pod deleted`, and the run ends
with `outputs not collected`. The agent can see and signal the holder too;
an agent that kills the holder loses its outputs, and collection fails with
`the outputs container has ended`.

The plugin collects outputs from `/wherry/outputs` through the holder once
the agent container has ended. The holder lists the whole tree with
`lstat`, never descending a link, and the plugin refuses the tree, reading
nothing, when it holds more entries than the receiver's limit. The holder
reads only regular files the listing shows within the limits, opened
within the outputs tree and refused when they are anything but a regular
file of the listed size; links, FIFOs and devices are listed with their
type and never read. An output the receiver streams, an S3 output, it
streams as the receiver reads it. The agent creates the outputs directory;
without one there are no outputs.

When a run ends, the plugin deletes its Secret and ends the holder, so the
pod goes terminal and frees its node's requests. The pod and its log stay
until the [sweep](#sweep). Purging a run deletes its pod with grace 0,
which the API server removes at once, waits until the pod is gone, then
deletes its Secret.

A pod with a `deletionTimestamp`, such as one on a lost node, is ended:
the plugin opens no log for it, reports exit code -1 when the agent has no
recorded end, and does not collect from it.

### Secrets

A run's Secret holds its prompt (`prompt`), its credential (`credential`),
each task credential (`credentials.<file>`) and each input file
(`inputs.<name>`), as raw bytes, so binary files arrive intact. Only the
`inputs` init container mounts it.

The API server limits a Secret's values together to 1 MiB, and a key to
253 characters. Dispatch adds up the prompt, the credential, the task
credentials and then the inputs, and refuses a run over either limit,
creating nothing:

- An input that takes the total past 1 MiB, or whose name is over 246
  characters, fails the run naming the input.
- A task credential file name over 241 characters fails the run the same
  way; the receiver calls it an input.
- A prompt and credentials over 1 MiB without any input fail the run with
  `the prompt and credentials exceed the 1 MiB Secret limit`.

The plugin declares an inline limit of 512 KiB, half the Secret, which
leaves the prompt and credentials the rest, so a host sends larger inputs
through the pool's bucket. Under the default pool terms, inputs fit.

The plugin writes the Secret only when the run's pod does not exist yet,
over any Secret an earlier attempt left, and never rewrites it under a
running pod.

### Sweep

On each sweep the receiver requests
([adapter plugins](https://github.com/benemon/wherry/blob/main/docs/reference.md#adapter-plugins)),
the plugin lists the pool's pods, and the pool's Secrets as metadata, so a
list never carries a value. It skips objects whose names are not the job
names their labels give. A pod has ended when its agent container has
ended, it never started, or it has a `deletionTimestamp`; its end time is
the `deletionTimestamp`, else the agent's end, else the pod's last
condition change.

- The adapter's own pods ended for 10 minutes are purged.
- Other adapters' pods are never purged; a removed adapter's pods stay
  until an operator deletes them.
- A Secret is deleted when its pod is the adapter's and has ended, when
  its pod is any adapter's and has ended 10 minutes ago, or when it has no
  pod and is more than 10 minutes old.

An adopter can read an ended pod's log for 10 minutes. A pod stranded on a
lost node is purged 10 minutes after its deletion began.

Ownership is by adapter name, not by receiver. Two receivers that carry an
adapter of the same name over one namespace both sweep its pods, and both
count the same purges, deletions and pending pods, so the metrics count
them twice.

### S3 inputs

The plugin fetches each S3 input on the receiver and streams it into the
pod; the presigned URL never leaves the receiver. It appears in no Secret,
pod spec, exec argument, event, log line or error, and agent pods need no
route to the storage endpoint.

1. The `inputs` init container takes each input's name, size and SHA-256 as
   arguments, creates `/wherry/inputs`, copies the Secret's material, and
   waits.
2. Once the container is running, the plugin fetches each input with a
   client that follows no redirect and reads at most one byte past the
   input's size, and pipes it into an exec of the helper's `put`. The
   helper writes the input to a temporary file, checks its size and
   SHA-256, and moves it into place. A put of an input already in place
   reads nothing.
3. An exec that fails in transport is retried with a fresh fetch, up to
   three attempts. The plugin stops delivering if the container has ended.
4. After the last put, the plugin tells the helper it is done, and the
   container exits once no put is in progress.
5. A fetch that fails, a put the helper refuses, or three failed attempts
   end the container with `input <name>: not fetched`, and the plugin
   reports the pod never started with class `fetch`, which the receiver
   ends `TASK_STATE_FAILED`. The plugin logs `input not fetched` with the
   status code or failure class, never the URL.
6. A container that waits 10 minutes with no put in progress ends the same
   way, so a receiver that dies during delivery leaves no pod waiting.

Every byte travels through the API server to the kubelet, which is slower
than a download from the pod, and the placement metric includes the time.

### Agent environment

The plugin sets the variables of the adapter protocol's `AgentEnvironment`
over the runtime's `env` and removes those of
[`AgentUnset`](https://github.com/benemon/wherry/blob/main/adapter/adapter.go)
from it, so no system, global or environment `git` configuration reaches
the agent's clones. It removes them from the runtime's `env` only; keep
them out of agent images' `ENV`. CA and proxy variables pass through as the
operator sets them. This protects an agent that follows its prompt. An
agent that runs commands of its own can undo this.

## Metrics

The plugin speaks adapter protocol version 7, and a receiver of another
protocol version refuses it, so upgrade it with the receiver. For each
adapter it reports these families, which the receiver serves on its
`/metrics` with the adapter's name as `adapter`:

| Family | Type | Labels | Counts |
| --- | --- | --- | --- |
| `wherry_adapter_platform_errors_total` | counter | `class` | failed API calls |
| `wherry_adapter_cleanup_pending` | gauge | none | the adapter's ended pods the latest sweep left |
| `wherry_adapter_sweep_deletions_total` | counter | `kind` | the adapter's pods a sweep purged (`job`) and Secrets it deleted (`variable`) |
| `wherry_adapter_placement_seconds` | histogram | none | time from a Dispatch that creates the pod to the plugin seeing its agent container running |

An API call's class comes from its status: 401 and 403 are `denied`, 409
`conflict`, 408, 504 and the `Timeout` and `ServerTimeout` reasons
`timeout`, and 429 and other 5xx codes `unavailable`. A call that cannot
connect is `unavailable`, one that runs past its deadline `timeout`, and
any other failure `other`. A 404, which the plugin reads as absence, and a
call the receiver cancels count nothing.

A failed call the plugin returns reaches the receiver with its operation,
class and the API's text, and the receiver logs it redacted. A failed call
the plugin handles itself, such as a poll, a refused signal or a failed
end of the holder, logs one warning, `kubernetes call failed`, naming the
adapter, the operation and the class; the line holds no URL, token or
response body. API warnings, such as PodSecurity notices, are logged as
`kubernetes warning`.

The plugin's client allows 50 requests a second with bursts of 100. Each
run polls its pod every second or two, besides its log stream, execs and
the sweep; the API server's priority and fairness protects the server.

## Agent network reach

The operator guide's
[Agent network reach](https://github.com/benemon/wherry/blob/main/docs/operator-guide.md#agent-network-reach)
says what an agent job's egress policy denies and allows.

Agent pods carry no API token and no Service variables, and need no
ingress: the plugin reaches them through the API server's exec and logs.
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
[`deploy/receiver.yaml`](deploy/receiver.yaml) holds its manifests: the
ServiceAccount, the RoleBinding of `admin` ([RBAC](#rbac)), the state
PersistentVolumeClaim, the Deployment and the metrics Service. The Secret
`wherry-receiver-bundle` holds the bundle. Kubernetes and OpenShift take the
same manifests; only the namespace, the image and the Kafka bootstrap
address differ.

### Image

`kubernetes/Containerfile` builds the receiver image. From a checkout of
the tag `kubernetes/<version>`, where `<version>` carries its `v`, such as
`v0.1.0`:

```sh
make image VERSION=<version> WHERRY_VERSION=<wherry version>
```

`make image` runs Podman; `CONTAINER=docker` runs Docker instead. The
build compiles the plugin and the helper from the checkout, downloads the
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
holds `/usr/local/bin/wherry`,
`/usr/local/libexec/wherry/plugins/wherry-plugin-kubernetes` and
`/usr/local/libexec/wherry/wherry-k8s-helper`, each mode 0755 and owned by
root, so the receiver's checks pass under any UID. It runs as UID 1001 and
group 0, with the entrypoint `wherry`; its writable paths are volumes whose
mount points, `/var/lib/wherry` and `/tmp`, group 0 can write. It holds no
Nomad plugin. The same image is the `helper_image`. For a storage endpoint
under a private CA, add the CA with `update-ca-trust` in a derived image.

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
   says, with adapters whose `namespace` is the receiver's and whose
   `helper_image` is the receiver image ([Settings](#settings)). The
   in-cluster receiver's adapters take no platform entry in the bundle; the
   service account supplies the token.

3. Apply the receiver:

   ```sh
   NAMESPACE=wherry IMAGE=registry.example.com/wherry-receiver:<version> \
     BOOTSTRAP=wherry-kafka-kafka-bootstrap.kafka.svc:9093 BUNDLE=kubernetes-1.bundle \
     kubernetes/deploy/apply.sh
   kubectl rollout status deployment/wherry-receiver -n wherry
   ```

[`deploy/apply.sh`](deploy/apply.sh) creates or replaces the Secret
`wherry-receiver-bundle` from `BUNDLE` with its `broker` replaced by
`BOOTSTRAP` ([Bundle and Kafka](#bundle-and-kafka)), never writing the
bundle to disk, then applies `receiver.yaml` with the namespace and the
image substituted, both with server-side apply. Run it again to change
either, and restart the Deployment to read a new bundle.

- One replica with the `Recreate` strategy: a receiver is one principal
  with one key pair, and a rolling update would briefly run two.
- The receiver's service account token is mounted, and the plugin, a child
  process of the receiver, uses it.
- The pod sets no `runAsUser`, so it runs as the image's UID 1001 with
  group 0.
- `/tmp` is a writable memory volume, and `TMPDIR` and `HOME` point at it.
  The root filesystem is read-only, and the plugin creates its socket for
  the receiver in `$TMPDIR`; without it the plugin cannot start, and the
  receiver exits with `adapter <name>: plugin ...` and restarts in a loop.
- State lives on the PVC, in the `state` subdirectory, which the receiver
  creates at 0700 with its signing key at 0600. The volume's root must be
  writable by the receiver. The pod sets no fsGroup, so use a storage
  class whose volumes UID 1001 or group 0 can write, such as kind's
  local-path volumes, which are mode 0777. A restarted pod mounts the same
  volume and is the same
  receiver. Losing the PVC loses the receiver's identity; provision it
  again under a new name, as the operator guide says for a lost state
  directory.
- The bundle Secret is mounted at 0440, owned by group 0, the receiver's
  group.
- The receiver has no health endpoint, so the pod has no probes.
- The metrics address binds the pod's own addresses, with no TLS or
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
whose live adapters satisfy its selector, so a task meant for one platform is
never delivered to the other's receivers. In one pool, a receiver with no
matching adapter takes the task and hands it back, spending its delivery
budget, and a task both platforms accept can be redelivered to a receiver that
cannot see its running job and start a second one.

A run is adopted only by a receiver with the same adapter. For a Kubernetes
run that is the in-cluster receiver itself, restarted: the new pod mounts the
same state volume, is the same principal, and finds the run's pod.

## OpenShift

The image, `receiver.yaml` and `apply.sh` are the same on OpenShift, which
ships `kubectl` beside `oc`. What differs:

- Create the namespace with `oc new-project wherry`. OpenShift sets the
  namespace's Pod Security labels itself and admits pods by SCC: the
  receiver's pod and agent pods run under restricted-v2. Do not allow the
  receiver's service account any SCC beyond restricted-v2; OpenShift admits
  a pod against the SCCs its creator may use as well as its service
  account's, so `anyuid` or `privileged` would admit agent pods running as
  root.
- restricted-v2 gives every container of a pod the namespace's first UID,
  with group 0. Leave `runtime.user` unset; a `user` outside the
  namespace's UID range is refused at admission.
- restricted-v2 also sets the pod's fsGroup, which the bundle file and the
  state volume take. `fsGroupChangePolicy: OnRootMismatch` keeps the
  kubelet from adding group access to the receiver's signing key on every
  mount, which would stop the receiver. If the namespace's fsGroup ever
  changes, the receiver stops with `chmod 600 it`; fix the modes.
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
| fsGroup | unset; agent Secret files stay root-owned at mode 0444, which the init container reads; the receiver's bundle is group 0 | restricted-v2 sets it; Secret files take its group |
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

The unit tests run the plugin against a fake API server and the helper
in-process against a temporary directory. The tests that need a cluster
run when `WHERRY_KUBERNETES_DEV_ADDR` names an API server, and skip
otherwise. They read:

| Variable | Content |
| --- | --- |
| `WHERRY_KUBERNETES_DEV_TOKEN_FILE` | a token of a service account bound to `admin` in the namespace alone ([RBAC](#rbac)) |
| `WHERRY_KUBERNETES_DEV_CA_FILE` | the API server's CA bundle |
| `WHERRY_KUBERNETES_DEV_NAMESPACE` | the namespace for agent pods |
| `WHERRY_KUBERNETES_DEV_HELPER_IMAGE` | the receiver image to run as `helper_image` |

They include the protocol's conformance suite,
`github.com/benemon/wherry/adapter/adaptertest`, in process and against
the built plugin. They set no `runtime.user`, and `TestRoleSuffices`
requires pods admitted under restricted-v2, so they run on OpenShift.

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
   receiver image's verifying fetch stage.
3. `03-provision.sh` runs `wherry admin` from the machine: the pool, the
   tenant, the host and the receiver, whose adapters run a fake agent and a
   missing image. It writes the host's configuration with a generated dummy
   credential and runs `wherry init`.
4. `04-deploy.sh` builds the receiver image with `make image` and the fake
   agent's image, loads both into kind, and deploys the receiver as
   [Deploy](#deploy) says, into a namespace enforcing Pod Security
   `restricted`.
5. `05-runs.sh` submits and awaits, as the host: a run it cancels, a plain
   run, a credentialed run, a run with an input file and a run whose image
   is missing. It checks each state, the result and output, the
   credential's mode 0600 in a 0700 directory, the input's SHA-256, the
   start failure's reason in the receiver's log, the dummy credential's
   absence from the log, pods and events, and the sweep's removal of the
   cancelled run's Secret and pod.

`collect.sh` writes the receiver's log and the cluster's state, but no
Secret, to `artifacts` in the state directory, `$E2E_WORK`, by default
`$TMPDIR/wherry-e2e`.
