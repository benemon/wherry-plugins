# wherry-plugin-kubernetes

The wherry adapter plugin for Kubernetes and OpenShift. A wherry receiver
starts it from its plugin directory for adapters of type `kubernetes`, and
runs each of the receiver's turns as one pod and one Secret in a namespace.
One plugin process hosts every `kubernetes` adapter of a receiver.

The plugin works against any conformant cluster. OpenShift 4.18
(Kubernetes 1.31) or later is the supported platform. Plain Kubernetes 1.31
or later is best effort; [OpenShift and plain Kubernetes](#openshift-and-plain-kubernetes)
lists the differences.

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

On OpenShift, run the receiver from the receiver image, which holds wherry,
the plugin and the helper; see
[Run the receiver on OpenShift](#run-the-receiver-on-openshift).

On a receiver outside the cluster:

1. Copy `wherry-plugin-kubernetes` into the receiver's plugin directory:

   ```sh
   install -o root -g root -m 0755 wherry-plugin-kubernetes /usr/local/libexec/wherry/plugins/
   ```

2. Start the receiver with `--plugin-dir` naming that directory:

   ```sh
   wherry receiver --bundle-file FILE --state-dir DIR --plugin-dir /usr/local/libexec/wherry/plugins
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

The token needs the [RBAC](#rbac) Role in the agent namespace.

## RBAC

The plugin's calls need this Role, bound by a RoleBinding to the
receiver's service account, in the agent namespace alone:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: wherry-receiver, namespace: wherry-agents}
rules:
  - apiGroups: [""]
    resources: [pods]
    verbs: [create, get, list, delete]
  - apiGroups: [""]
    resources: [pods/log]
    verbs: [get]
  - apiGroups: [""]
    resources: [pods/exec]
    verbs: [create, get]
  - apiGroups: [""]
    resources: [secrets]
    verbs: [create, get, update, list, delete]
  - apiGroups: [""]
    resources: [namespaces]
    resourceNames: [wherry-agents]
    verbs: [get]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: wherry-receiver, namespace: wherry-agents}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: wherry-receiver}
subjects:
  - {kind: ServiceAccount, name: wherry-receiver, namespace: wherry}
```

- `pods` create, get, list, delete: create a turn's pod, read it for
  lookups and polls, list it for the sweep, and delete it for a stop's last
  resort, a dispatch or lookup cut short, and a purge.
- `pods/log` get: stream the agent's standard output.
- `pods/exec` create and get: signal the agent, list and read its outputs,
  end the outputs holder, and deliver S3 inputs. A WebSocket exec is an
  upgraded GET that Kubernetes 1.31 also authorises as `create`, and the
  SPDY fallback is a POST, so both verbs are needed.
- `secrets` create, get, update, list, delete: write a turn's material,
  delete it when the turn ends, and list its metadata for the sweep.
- `namespaces` get on the agent namespace alone: readiness. A namespaced
  Role can grant this, because a request for a namespace object is
  authorised in that namespace.

No `watch`, `patch`, `deletecollection` or `resourcequotas` access is
needed.

The API does not restrict the command an exec runs. With `pods` create,
`pods/exec` and `secrets` get in the agent namespace, a compromised
receiver, or anyone holding its token, can run commands in any running
agent container, read the agent's environment (the model credential) and
its credentials directory, use that pod's egress, start pods under
restricted-v2 in that namespace, and read every turn's prompt and material
from the Secrets. It cannot reach other namespaces, nodes or a wider SCC,
because the Role is namespaced and admission uses the creator's SCCs. The
outputs holder never mounts the credentials, but a process in it can read
them through `/proc/<agent pid>/root` while the agent runs, since both share
a UID and a process namespace. The holder's mounts are hygiene, not
isolation.

Do not grant:

- the Role as a ClusterRole, or any binding outside the agent namespace;
- the Role in a namespace that holds anything but agent pods. `secrets` and
  `pods/exec` there reach every Secret and pod;
- `pods/attach`, `pods/portforward`, `pods/proxy`, `pods/ephemeralcontainers`
  or `serviceaccounts/token`;
- `escalate`, `bind` or `impersonate`;
- `nodes` access of any kind;
- `use` of any SCC beyond the default restricted-v2 to the receiver's
  service account. OpenShift admits a pod against the SCCs its creator may
  use as well as its service account's, so `anyuid` or `privileged` would
  admit agent pods running as root;
- RBAC to the agent namespace's `default` service account. Its token is
  never mounted, and no agent may hold any API right.

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
| `helper_image` | required | image holding `wherry-k8s-helper` at `/usr/local/libexec/wherry/`; the receiver image at the receiver's tag |
| `runtime` | required | the agent container's definition |

`credential_env` is required when the adapter sets `credential`, and
matches `[A-Za-z_][A-Za-z0-9_]*`. `node_selector` keys and values are
Kubernetes label keys and values. Each toleration has `key`, `operator`
(`Equal`, or `Exists` without a `value`), `value` and `effect`
(`NoSchedule`, `PreferNoSchedule` or `NoExecute`). Unknown fields are
errors. The plugin checks the section when the receiver configures the
adapter, and the receiver exits with `adapter <name>: ...` on an error.

`runtime` has these fields:

| Field | Content |
| --- | --- |
| `image` | the agent image; required |
| `command` | the container's command; unset, the image's entrypoint |
| `args` | the container's arguments |
| `env` | environment variables for the agent; names match `[A-Za-z_][A-Za-z0-9_]*` |
| `resources` | `cpu` (default `500m`), `memory` (default `2Gi`) and `ephemeral_storage` (default unset), Kubernetes quantities, each set as both request and limit |
| `work_limit` | a quantity, the size limit of the agent's `/wherry` volume; unset, no limit |
| `user` | `UID` or `UID:GID`; unset, the platform assigns one |

`user` takes positive integers below 2^31 with no leading zeros; root (0)
is refused. On OpenShift, a `user` outside the namespace's UID range is
refused at admission. The resource defaults are starting points; set them
for the agent.

The plugin sets `HOME`, the `WHERRY_*` variables and the credential
variable over `env`, so a variable of these names in `env` is overwritten.

An adapter is ready when the API answers with the namespace, and the
namespace is active. This proves the address, the token and the namespace.
Readiness checks nothing else:

- Nodes. Nodes are cluster-scoped, and the plugin does not read the
  cluster's topology. A pod no node can take, such as one whose
  `node_selector` no node matches, waits until the run's deadline, holding
  one of the receiver's `capacity` slots, and the turn then fails
  `timeout`.
- Quota. A ResourceQuota in the agent namespace that is full refuses pod
  creation with 403. Each refusal counts a `dispatch` start failure, and
  the task spends its delivery budget.
- Images. An agent or helper image that cannot be pulled shows only at
  dispatch, as a pod that never started with class `image`.

An adapter file in a receiver configuration directory:

```yaml
name: openshift
platform: openshift
agent: coder
credential: model-api-key
labels:
  platform: openshift
config:
  runtime:
    image: image-registry.openshift-image-registry.svc:5000/wherry-images/agent:1.0
    command: [/bin/sh, -c]
    args:
      - >-
        test -n "$WHERRY_MODEL" || WHERRY_MODEL=haiku;
        exec agent-run --model "$WHERRY_MODEL" < /wherry/prompt.txt
```

with `platforms/openshift.yaml`:

```yaml
name: openshift
type: kubernetes
config:
  namespace: wherry-agents
  credential_env: MODEL_API_KEY
  helper_image: image-registry.openshift-image-registry.svc:5000/wherry-images/wherry-receiver:<version>
```

## Agent pods

The plugin creates one Secret and one pod per turn, both named
`agent-<pool>-<run>-t<turn>` in the adapter's namespace. `<pool>` is the
first 12 hexadecimal digits of the SHA-256 of the pool name, as the Nomad
plugin names its jobs. Both carry the labels `wherry.io/pool`,
`wherry.io/adapter`, `wherry.io/agent`, `wherry.io/run` and
`wherry.io/turn`, and the pod carries the annotation `wherry.io/attempt`
with the attempt that created it. The receiver reads the labels to decide
whether to adopt the pod.

The pod is a bare pod with `restartPolicy: Never`, so a turn runs exactly
once and nothing replaces its pod. It has three containers:

- `inputs`, an init container running `wherry-k8s-helper`, copies the
  turn's material from its Secret into the pod's volumes, and takes the S3
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
capabilities. The helper containers have read-only root filesystems and
request 10m CPU and 16Mi memory, limited to 200m and 64Mi. The agent's
requests equal its limits. The plugin sets no `ephemeral-storage` on the
helper containers; where a ResourceQuota covers `ephemeral-storage`, give
the agent namespace a LimitRange default. The pod runs as the namespace's
`default` service account, which carries the pull secrets for the agent
and helper images.

Every container must run as one UID, since the helper signals the agent's
processes and owns its credential files. On OpenShift, restricted-v2 gives
every container the namespace's first UID. If the admitted pod's containers
run as different users, or any has none, the plugin deletes the pod and its
Secret and the turn fails with
`agent pod containers run as different users; set runtime.user`.

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
plus headroom.

The plugin reads the agent's standard output from the logs API, which
interleaves standard error with it. A line the agent writes to standard
error in the middle of a standard output line corrupts the JSON result
line, and the turn ends `agent ended without a result`. Send standard
error elsewhere in the agent's command, such as `2>"$HOME/agent.err"`.
Kubernetes 1.32 adds `PodLogOptions.Stream` to read standard output alone,
which removes this trap on a later minimum platform.

The plugin follows the agent's log while it runs, and the kubelet's follow
stream carries every byte across log rotation. A follow that starts from
an offset, after a receiver restart or an adoption, reads the current log
file only. If the kubelet has rotated the log past that offset, the follow
fails with `the agent's log is shorter than the offset`, and the turn
fails. Agents whose standard output can pass the kubelet's
`containerLogMaxSize` (50 MiB on OpenShift, 10 MiB on Kubernetes by
default) are exposed to this.

The plugin stops an agent by sending `SIGTERM` to every process of the
pod but the holder, through the holder, and waiting up to 15 seconds for
the agent container to end; then `SIGKILL` and 15 seconds more. If the
agent still runs, or the holder cannot signal it, the plugin deletes the
pod at once, logs `agent outlived SIGKILL; pod deleted`, and the turn ends
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

When a turn ends, the plugin deletes its Secret and ends the holder, so the
pod goes terminal and frees its node's requests. The pod and its log stay
until the [sweep](#sweep). Purging a turn deletes its pod with grace 0,
which the API server removes at once, waits until the pod is gone, then
deletes its Secret.

A pod with a `deletionTimestamp`, such as one on a lost node, is ended:
the plugin opens no log for it, reports exit code -1 when the agent has no
recorded end, and does not collect from it.

### Secrets

A turn's Secret holds its prompt (`prompt`), its credential (`credential`),
each task credential (`credentials.<file>`) and each input file
(`inputs.<name>`), as raw bytes, so binary files arrive intact. Only the
`inputs` init container mounts it.

The API server limits a Secret's values together to 1 MiB, and a key to
253 characters. Dispatch adds up the prompt, the credential, the task
credentials and then the inputs, and refuses a turn over either limit,
creating nothing:

- An input that takes the total past 1 MiB, or whose name is over 246
  characters, fails the run naming the input.
- A task credential file name over 241 characters fails the run the same
  way; the receiver calls it an input.
- A prompt and credentials over 1 MiB without any input fail the turn with
  `the prompt and credentials exceed the 1 MiB Secret limit`. A long
  multi-turn prompt is the usual cause.

Under the default pool terms, inputs fit.

The plugin writes the Secret only when the turn's pod does not exist yet,
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

The plugin speaks adapter protocol version 5, and a receiver of another
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
turn polls its pod every second or two, besides its log stream, execs and
the sweep; the API server's priority and fairness protects the server.

## Agent network reach

The operator guide's
[Agent network reach](https://github.com/benemon/wherry/blob/main/docs/operator-guide.md#agent-network-reach)
says what an agent job's egress policy denies and allows.

Agent pods carry no API token and no Service variables, and need no
ingress: the plugin reaches them through the API server's exec and logs.
They need no route to the storage endpoint. Until an egress policy is set,
agent pods have the cluster's default egress.

To restrict agents, put a default-deny egress NetworkPolicy in the agent
namespace with allow rules for what agents reach (DNS, the model API, git
hosts). On OpenShift, a destination known only by DNS name needs an
OVN-Kubernetes `EgressFirewall` with `dnsName` rules, which a cluster
administrator creates. The agent namespace holds nothing but agent pods,
so such a policy constrains agents alone.

## Run the receiver on OpenShift

The receiver runs as a one-replica Deployment in its own namespace, with
its own service account, and holds the [RBAC](#rbac) Role in a separate
namespace that holds only agent pods. The default names, to be changed
together:

- `wherry`: the receiver's namespace, holding the ServiceAccount, the
  bundle Secret, the state PersistentVolumeClaim, the Deployment, the
  metrics Service and ServiceMonitor, and the NetworkPolicies.
- `wherry-agents`: agent pods and their Secrets alone, and the Role and
  RoleBinding.
- `wherry-images`: the receiver and agent images.

### Image

`kubernetes/Containerfile` builds the receiver image. From a checkout of
the tag `kubernetes/<version>`:

```sh
make image VERSION=<version> WHERRY_VERSION=<wherry version>
```

The build compiles the plugin and the helper from the checkout, downloads
the wherry release archive for the target architecture with its
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
root, so the receiver's checks pass under any UID. It runs as UID 1001,
which OpenShift replaces with the namespace's UID, with the entrypoint
`wherry`. It holds no Nomad plugin. The same image is the `helper_image`.
For a storage endpoint under a private CA, add the CA with
`update-ca-trust` in a derived image.

No image is published, since an image couples a wherry release and a
plugins release. Push the image you build to the internal registry:

```sh
oc new-project wherry-images
podman push wherry-receiver:<version> \
  image-registry.openshift-image-registry.svc:5000/wherry-images/wherry-receiver:<version>
```

Agent pods and the receiver pull from `wherry-images`:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: wherry-image-pullers, namespace: wherry-images}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: system:image-puller}
subjects:
  - {kind: ServiceAccount, name: default, namespace: wherry-agents}
  - {kind: ServiceAccount, name: wherry-receiver, namespace: wherry}
```

### Manifests

```yaml
apiVersion: v1
kind: Namespace
metadata: {name: wherry}
---
apiVersion: v1
kind: Namespace
metadata: {name: wherry-agents}
---
apiVersion: v1
kind: ServiceAccount
metadata: {name: wherry-receiver, namespace: wherry}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: wherry-receiver-state, namespace: wherry}
spec:
  accessModes: [ReadWriteOnce]
  resources: {requests: {storage: 1Gi}}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: wherry-receiver, namespace: wherry}
spec:
  replicas: 1
  strategy: {type: Recreate}
  selector: {matchLabels: {app: wherry-receiver}}
  template:
    metadata: {labels: {app: wherry-receiver}}
    spec:
      serviceAccountName: wherry-receiver
      securityContext:
        runAsNonRoot: true
        seccompProfile: {type: RuntimeDefault}
        fsGroupChangePolicy: OnRootMismatch
      containers:
        - name: receiver
          image: image-registry.openshift-image-registry.svc:5000/wherry-images/wherry-receiver:<version>
          args: [receiver, --bundle-file, /etc/wherry/bundle/bundle.yaml, --state-dir, /var/lib/wherry/state,
                 --plugin-dir, /usr/local/libexec/wherry/plugins, --metrics-addr, "0.0.0.0:9464"]
          env:
            - {name: TMPDIR, value: /tmp}
            - {name: HOME, value: /tmp}
          ports: [{name: metrics, containerPort: 9464}]
          securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}, readOnlyRootFilesystem: true}
          resources: {requests: {cpu: 100m, memory: 128Mi}, limits: {memory: 512Mi}}
          volumeMounts:
            - {name: bundle, mountPath: /etc/wherry/bundle, readOnly: true}
            - {name: state, mountPath: /var/lib/wherry}
            - {name: tmp, mountPath: /tmp}
      volumes:
        - {name: bundle, secret: {secretName: wherry-receiver-bundle, defaultMode: 0400}}
        - {name: state, persistentVolumeClaim: {claimName: wherry-receiver-state}}
        - {name: tmp, emptyDir: {medium: Memory, sizeLimit: 16Mi}}
---
apiVersion: v1
kind: Service
metadata: {name: wherry-receiver-metrics, namespace: wherry, labels: {app: wherry-receiver}}
spec:
  selector: {app: wherry-receiver}
  ports: [{name: metrics, port: 9464, targetPort: metrics}]
---
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata: {name: wherry-receiver, namespace: wherry}
spec:
  selector: {matchLabels: {app: wherry-receiver}}
  endpoints: [{port: metrics, scheme: http, path: /metrics}]
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: wherry-receiver-metrics, namespace: wherry}
spec:
  podSelector: {matchLabels: {app: wherry-receiver}}
  policyTypes: [Ingress]
  ingress:
    - from:
        - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: openshift-user-workload-monitoring}}
      ports: [{protocol: TCP, port: 9464}]
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: wherry-receiver-egress, namespace: wherry}
spec:
  podSelector: {matchLabels: {app: wherry-receiver}}
  policyTypes: [Egress]
  egress:
    - to:
        - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: kafka}}
          podSelector: {matchLabels: {strimzi.io/kind: Kafka}}
      ports: [{protocol: TCP, port: 9093}]
    - to:
        - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: openshift-dns}}
      ports: [{protocol: UDP, port: 5353}, {protocol: TCP, port: 5353}]
    - to:
        # One ipBlock for each address of the default/kubernetes Endpoints.
        - ipBlock: {cidr: <API server address>/32}
      ports: [{protocol: TCP, port: 6443}]
    - to:
        - ipBlock: {cidr: <storage endpoint address>/32}
      ports: [{protocol: TCP, port: 443}]
```

Apply the [RBAC](#rbac) Role and RoleBinding and the image-puller
RoleBinding above beside them, and create the bundle Secret as
[Bundle and Kafka](#bundle-and-kafka) describes.

- One replica with the `Recreate` strategy: a receiver is one principal
  with one key pair, and a rolling update would briefly run two.
- The receiver's service account token is mounted, and the plugin, a child
  process of the receiver, uses it.
- `/tmp` is a writable memory volume, and `TMPDIR` and `HOME` point at it.
  The root filesystem is read-only, and the plugin creates its socket for
  the receiver in `$TMPDIR`; without it the plugin cannot start, and the
  receiver exits with `adapter <name>: plugin ...` and restarts in a loop.
- State lives on the PVC, in the `state` subdirectory, which the receiver
  creates at 0700 with its signing key at 0600. `fsGroupChangePolicy:
  OnRootMismatch` keeps the kubelet from adding group access to the key on
  every mount, which would stop the receiver. If the namespace's fsGroup
  ever changes, the receiver stops with `chmod 600 it`; fix the modes. A
  restarted pod mounts the same volume and is the same receiver. Losing
  the PVC loses the receiver's identity; provision it again under a new
  name, as the operator guide says for a lost state directory.
- The bundle Secret is mounted at 0400, which becomes 0440 with the pod's
  fsGroup.
- The receiver has no health endpoint, so the pod has no probes.
- The metrics address binds the pod's own addresses. The ingress policy
  admits only user workload monitoring, and the ServiceMonitor scrapes over
  plain HTTP, since the endpoint has no TLS or authentication. Whoever can
  read the user workload Prometheus sees every tenant of the pool.
- The egress policy allows exactly the Kafka brokers (TCP 9093), the DNS
  pods (5353), the API server (TCP 6443) and the storage endpoint
  (TCP 443). The API server runs on the host network, so a namespace
  selector does not reach it; list the addresses of the `default/kubernetes`
  Endpoints. The policy makes the receiver work under a cluster-wide
  default-deny egress policy, and limits it otherwise. A storage endpoint
  outside the cluster must also pass the cluster's own egress rules, and one
  known only by DNS name needs an `EgressFirewall`.

Some items need more than a project administrator:

- user workload monitoring must be enabled cluster-wide for the
  ServiceMonitor to be scraped;
- an `EgressFirewall` is OVN-Kubernetes only and created by a cluster
  administrator;
- raising `containerLogMaxSize` is a `KubeletConfig`, which rolls every
  node of its MachineConfigPool.

### Bundle and Kafka

The pool's Kafka runs in the same cluster under Streams for Apache Kafka or
Strimzi, with Kafka 4.3, share groups enabled, and a `tls` internal listener
with `authentication: scram-sha-512` and `authorization: simple`. The
operator guide's Strimzi notes apply. The listener's `networkPolicyPeers`
admits the `wherry` namespace.

The bundle `wherry admin receiver add` writes names `WHERRY_TRUST_BROKER`,
the address hosts use. Before creating the Secret, replace the bundle's
`broker` with the internal bootstrap address, such as
`wherry-kafka-kafka-bootstrap.kafka.svc:9093`; Strimzi's broker
certificates carry the internal Service names. The bundle's `ca` is the
Strimzi cluster CA (`<cluster>-cluster-ca-cert`, key `ca.crt`).

```sh
oc create secret generic wherry-receiver-bundle -n wherry --from-file=bundle.yaml=openshift-1.bundle
```

The in-cluster receiver's adapters take no platform entry in the bundle;
the service account supplies the token.

Strimzi renews its cluster CA, and nothing renews the receiver's copy:

1. When the renewal begins, Strimzi keeps the old certificate in
   `<cluster>-cluster-ca-cert` beside the new `ca.crt`. Write both
   certificates into the bundle's `ca`.
2. Replace `wherry-receiver-bundle` and run
   `oc rollout restart deployment/wherry-receiver -n wherry`.
3. Once Strimzi has removed the old certificate, remove it from the bundle
   and restart again.

### Members on Nomad and OpenShift

Members are asymmetric. The in-cluster receiver carries only Kubernetes
adapters, labelled `platform: openshift`, and holds no Nomad token. A
Nomad-hosted receiver carries only Nomad adapters, labelled
`platform: nomad`, and holds no OpenShift token. A task is admitted only by
an adapter whose labels satisfy its selector.

A run is adopted only by a receiver with the same adapter. For a
Kubernetes run that is the in-cluster receiver itself, restarted: the new
pod mounts the same state volume, is the same principal, and finds the
turn's pod.

Every task must select a platform label. A task with no selector can be
placed on either member; redelivered to the other, it meets an adapter that
cannot see its job, and a second job starts while the first runs on
unfollowed.

Members may instead be symmetric, each receiver carrying the other
platform's adapters and tokens. That costs a long-lived token for the
remote receiver, whose service account and token Secret live outside the
agent namespace and are bound in by a RoleBinding, and the sweep's double
count across two receivers sharing an adapter name.

### OpenShift and plain Kubernetes

| Concern | OpenShift | Plain Kubernetes |
| --- | --- | --- |
| Agent UID | restricted-v2 gives every container the namespace's first UID; leave `runtime.user` unset | nothing assigns one; set `runtime.user` |
| fsGroup | restricted-v2 sets it; Secret files take its group | unset; Secret files stay root-owned at mode 0444, which the init container reads |
| Admission | SCCs of the creator and the pod's service account; a refusal is 403 at create | Pod Security Admission; label the agent namespace `pod-security.kubernetes.io/enforce: restricted`, which the pod meets |
| Image user | an arbitrary UID with GID 0; agent images keep their writable paths group-0 writable | the image's own user |
| Node selection | a project's `openshift.io/node-selector` merges with `node_selector`; a conflict is 403 at create | `node_selector` alone |
| Registry | the internal registry, `wherry-images`, with `system:image-puller` | any registry; pull secrets on the namespace's `default` service account |
| Egress by name | OVN-Kubernetes `EgressFirewall` with `dnsName` | a DNS-aware CNI policy, such as Cilium FQDN |
| DNS for NetworkPolicy | `openshift-dns`, pods on port 5353 | the cluster's DNS namespace and port |
| API server for NetworkPolicy | host network; `ipBlock` of the `default/kubernetes` Endpoints, port 6443 | the same, port per distribution |
| Container logs | CRI-O, `containerLogMaxSize` 50 MiB by default | the kubelet's `containerLogMaxSize`, 10 MiB by default |
| Metrics | user workload monitoring and a ServiceMonitor | any Prometheus scrape configuration |

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
| `WHERRY_KUBERNETES_DEV_TOKEN_FILE` | a token of a service account bound only to the [RBAC](#rbac) Role |
| `WHERRY_KUBERNETES_DEV_CA_FILE` | the API server's CA bundle |
| `WHERRY_KUBERNETES_DEV_NAMESPACE` | the agent namespace |
| `WHERRY_KUBERNETES_DEV_HELPER_IMAGE` | the receiver image to run as `helper_image` |

They include the protocol's conformance suite,
`github.com/benemon/wherry/adapter/adaptertest`, in process and against
the built plugin.
