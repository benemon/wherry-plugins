# wherry-plugin-systemd

The wherry adapter plugin for systemd. A wherry receiver starts it from its
plugin directory for adapters of type `systemd`, and runs each of the
receiver's runs as one transient service on the receiver's own host. One
plugin process hosts every `systemd` adapter of a receiver.

systemd 252 or later with the unified cgroup v2 hierarchy, on Linux, is the
platform. The plugin connects to systemd's
private socket, `/run/systemd/private`, and runs as root.

## Build

The plugin builds against the tagged adapter protocol module,
`github.com/benemon/wherry/adapter`. From a clone of this repository:

```sh
make build
```

This writes `wherry-plugin-systemd` at the repository root. The plugin
builds for Linux alone; on another OS, set `GOOS=linux GOARCH=amd64`.
`make check` runs the CI gates, and
`make dist PLUGIN=systemd VERSION=<version>` writes release archives for
linux amd64 and arm64, and `checksums.txt`, into `dist`.

## Install

[Run the receiver as a systemd service](#run-the-receiver-as-a-systemd-service)
gives the whole install. The plugin goes into the receiver's plugin
directory:

```sh
install -o root -g root -m 0755 wherry-plugin-systemd /usr/local/libexec/wherry/plugins/
```

The receiver checks the plugin directory and binary before it runs them;
see [`wherry receiver`](https://github.com/benemon/wherry/blob/main/docs/reference.md).
The plugin is not run by hand.

## Credentials

The plugin takes no platform credential: it talks to its own host's
systemd as root. An adapter whose bundle holds a `platform` entry fails to
configure with `the systemd plugin takes no platform credential`.

## Privileges

The receiver and the plugin run as root. On systemd 252,
`StartTransientUnit` asks polkit for `org.freedesktop.systemd1.manage-units`
with no details, so a polkit rule cannot tell one transient unit from
another, nor see its `User=` or `ExecStart=`. A user allowed to start
transient units may start one as root, so a non-root receiver bounds
nothing. A non-root receiver also cannot read a dynamic user's outputs
under `/var/lib/private`, mode 0700 root, nor remove what an agent leaves.

A compromised receiver is host root. systemd's private socket admits any
peer of uid 0 and checks none of its capabilities, so a uid-0 process with
an empty capability set still starts root units through it. The
receiver's unit bounds accidents and partial exploits, such as a
file-write bug, with these settings:

- `CapabilityBoundingSet=CAP_DAC_OVERRIDE CAP_DAC_READ_SEARCH CAP_FOWNER`.
  `CAP_DAC_READ_SEARCH` reads outputs the agent left at any mode.
  `CAP_DAC_OVERRIDE` removes a run's tree, whose directories the agent may
  have made unwritable, and removes the prompt and inputs from a work
  directory the agent owns. `CAP_FOWNER` removes entries of a sticky
  directory an agent made. The plugin never mounts, chowns or signals a
  process itself: systemd chowns the work directory and PID 1 sends every
  signal.
- `ProtectSystem=strict`, with `StateDirectory=wherry`,
  `RuntimeDirectory=wherry-systemd` and `ReadWritePaths=` for the agents'
  state roots alone.
- `ProtectHome=yes`, `PrivateTmp=yes`, `PrivateDevices=yes`,
  `NoNewPrivileges=yes` and the kernel protections.

## Settings

An adapter's `config` section in the receiver configuration:

| Field | Default | Content |
| --- | --- | --- |
| `credential_env` | none | environment variable the agent receives the credential in |
| `runtime` | required | the agent's command, identity and limits |

`credential_env` is required when the adapter sets `credential`, matches
`[A-Za-z_][A-Za-z0-9_]*`, and is none of the variables the plugin or
systemd sets for the agent. Unknown fields are errors. The plugin checks
the section when the receiver configures the adapter, and the receiver
exits with `adapter <name>: ...` on an error.

`runtime` has these fields:

| Field | Content |
| --- | --- |
| `command` | required; the agent's command, its first element an absolute path; each element UTF-8 without NUL |
| `args` | appended to `command`; each UTF-8 without NUL |
| `env` | environment variables for the agent; names match `[A-Za-z_][A-Za-z0-9_]*` and are none of `CREDENTIALS_DIRECTORY`, `STATE_DIRECTORY` and `INVOCATION_ID`; values UTF-8 without NUL |
| `resources` | `cpu_quota`, percent of one CPU (default 50), from 1 to 100 times the CPUs the receiver may use; `memory_mb`, MiB (default 2048), from 64 to 2^31 - 1 |
| `user` | a user name or `UID[:GID]`, a string; unset, each run gets a dynamic user of its own |
| `group` | the agent's primary group, a group name or GID; unset, the user's own |
| `supplementary_groups` | a list of group names or GIDs the agent also holds |

`user`, `group` and `supplementary_groups` refuse root (`root` or 0). Numeric IDs
are positive integers below 2^32 with no leading zeros; quote them in
YAML, such as `user: "1001"`. `group` is refused beside a `user` that
names a GID. They set the unit's `User=`, `Group=` and
`SupplementaryGroups=`. `cpu_quota` takes systemd's `CPUQuota=` percent,
and `memory_mb` caps the agent's RAM with no swap.

The plugin sets `SHELL=/bin/sh` before `env`, so `env` may override it.
It sets `HOME`, the `WHERRY_*` variables and the credential variable over
`env`, so a variable of these names in `env` is overwritten.

Configure also fails when:

- the plugin does not run as uid 0: `the systemd plugin runs as root`;
- the manager's `Version` is below 252: `systemd 252 or later is required`;
- `/sys/fs/cgroup` is not cgroup2: `systemd's unified cgroup hierarchy is required`;
- `/var/lib/wherry/systemd/runs`, `/var/lib/private/wherry-agents` or
  `/var/lib/wherry-agents` cannot be created or written;
- `/run/wherry-systemd` is not a directory owned by root with mode 0700:
  `/run/wherry-systemd must be root 0700`.

An adapter is ready while the manager's `SystemState` is `running`,
`degraded` or `starting`. Any failed unit on the host sets `degraded`, a
failed agent unit included until its run is released. `starting` holds
while a boot job is still queued after `basic.target` is active, which an
agent unit's default dependencies wait for. Readiness does not check the
runtime command: a missing command shows at dispatch as a unit that never
started with class `exec`, or, for a run with a credential, as an agent
that exits 127.

An adapter file in a receiver configuration directory:

```yaml
name: host
platform: systemd
agent: coder
credential: model-api-key
labels:
  platform: systemd
config:
  credential_env: ANTHROPIC_API_KEY
  runtime:
    command: [/bin/sh, -c]
    args:
      - >-
        test -n "$WHERRY_MODEL" || WHERRY_MODEL=haiku;
        exec /usr/local/bin/claude -p --output-format stream-json --verbose --model "$WHERRY_MODEL" < /wherry/prompt.txt
    env:
      SHELL: /bin/bash
      DISABLE_AUTOUPDATER: "1"
    resources: {cpu_quota: 100, memory_mb: 2048}
```

`DISABLE_AUTOUPDATER` keeps Claude Code from trying to update itself on
the read-only root. Claude Code runs its shell tool through `$SHELL`.

### Receiver configuration

This receiver configuration file holds one platform and two adapters.
Each adapter's `config` takes the platform's `config` keys at the top level,
and an adapter that sets one of them again is an error.
[`testdata/receiver.yaml`](testdata/receiver.yaml) holds this file, and
`TestReadmeReceiverConfigures` configures each adapter from it through
the plugin's decoding and checks.

```yaml
capacity: 2
platforms:
  - name: systemd
    type: systemd
adapters:
  # Claude Code, each run as a dynamic user of its own.
  - name: host
    platform: systemd
    agent: coder
    labels:
      platform: systemd
    config:
      runtime:
        command: [/bin/sh, -c]
        args:
          - >-
            test -n "$WHERRY_MODEL" || WHERRY_MODEL=haiku;
            exec /usr/local/bin/claude -p --output-format stream-json --verbose --model "$WHERRY_MODEL" < /wherry/prompt.txt
        env:
          SHELL: /bin/bash
          DISABLE_AUTOUPDATER: "1"
        resources:
          cpu_quota: 100
          memory_mb: 2048
  # An agent that needs a provisioned identity, run as a fixed user that
  # every run of the adapter shares.
  - name: provisioned
    platform: systemd
    agent: reviewer
    output: file
    reply_file: reply.md
    labels:
      platform: systemd-provisioned
    config:
      runtime:
        user: wherry-agent
        group: wherry-agent
        supplementary_groups: [video]
        command: [/usr/local/bin/agent-run, --prompt-file, /wherry/prompt.txt]
```

## Agent units

The plugin runs each run as one transient service,
`wherry-agent-<pool>-<run>.service`, in `wherry.slice`. `<pool>` is the
first 12 hexadecimal digits of the SHA-256 of the pool name, as the other
plugins name their jobs, and `agent-<pool>-<run>` names the run's
directories:

| Path | Owner and mode | Holds |
| --- | --- | --- |
| `/var/lib/wherry/systemd/runs/<job>/` | root, 0700 | the run's control directory |
| `.../<job>/job.json` | root, 0600 | the marker: metadata, phase, instance and recorded outcome |
| `.../<job>/stdout`, `.../<job>/stderr` | root, 0600 | the agent's standard output and standard error |
| `/var/lib/private/wherry-agents/<job>/` | the agent's dynamic user, 0700 | the work directory, seen as `/wherry` |
| `/var/lib/wherry-agents/<job>` | root | systemd's symlink to the work directory; with `runtime.user`, the work directory itself, owned by the user |
| `/run/wherry-systemd/<unit>/` | root, 0700; files 0600 | the run's credential files from Dispatch to the end of the unit's start |
| `/run/credentials/<unit>/` | systemd | the unit's credential store, until the unit stops |

The marker records the run's pool, adapter, agent, attempt and a random
instance per dispatch, the phase (`prepared`, `starting`, `started`), and
how the run ended once the plugin settles it. It outlives the unit, so a
restarted receiver finds every run it started, and Lookup reports the
attempt, adapter and agent from it.

The unit is `Type=exec` with `RemainAfterExit=yes` and `Restart=no`, so a
run executes once, its start job fails when the command cannot run, and
its exit status stays readable until the run is released. Each unit has:

- `DynamicUser=yes`, one UID per run from systemd's range 61184 to 65519,
  unless `runtime.user` sets a fixed user;
- `ProtectSystem=strict`, `ProtectHome=yes`, `PrivateTmp=yes`,
  `PrivateDevices=yes`, `PrivateIPC=yes`, `NoNewPrivileges=yes`,
  `RestrictSUIDSGID=yes`, the six kernel and clock protections,
  `ProtectProc=invisible`, `LockPersonality=yes`, `RestrictRealtime=yes`,
  `SystemCallArchitectures=native`, `KeyringMode=private`, `UMask=0077`,
  and an empty capability bounding set;
- `RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK`;
- `InaccessiblePaths=` over `/var/lib/wherry`, `/etc/wherry`,
  `/run/wherry-systemd`, `/run/systemd/transient` and
  `/run/systemd/journal`, which holds the journal's sockets, so an agent's
  `logger` or `syslog(3)` writes reach no journal;
- `LimitCORE=1`, which makes the kernel abort a core dump to a pipe
  handler, so systemd-coredump never logs an agent's environment;
- `CPUQuota=` and `MemoryMax=` from `resources`, `MemorySwapMax=0`,
  `OOMPolicy=stop`, `KillMode=control-group`, a 30-second start timeout and
  a 15-second stop timeout.

The plugin does not set `MemoryDenyWriteExecute=`, since Node.js
JIT-compiles; `RestrictNamespaces=`, since the agents' own sandboxes create
namespaces; `SystemCallFilter=`, `PrivateUsers=` or `ProcSubset=`.

The agent sees:

- the prompt in `/wherry/prompt.txt`, mode 0600;
- each input file in `/wherry/inputs`, mode 0600, in a 0700 directory;
- each S3 input in `/wherry/inputs`, as described in [S3 inputs](#s3-inputs);
- each task credential as a file in `/run/wherry/credentials`, and the
  agent credential as `.credential` beside them, as described in
  [Credentials](#credentials-1);
- its outputs directory, which it creates, at `/wherry/outputs`;
- `HOME` set to `/wherry`, the run's own work directory, so no agent state,
  history or login carries from one run to the next;
- `SHELL` set to `/bin/sh` unless `env` sets it;
- `WHERRY_INPUT_DIR`, `WHERRY_OUTPUT_DIR` and `WHERRY_CREDENTIALS_DIR` set
  to `/wherry/inputs`, `/wherry/outputs` and `/run/wherry/credentials`,
  the same paths the prompt names;
- `WHERRY_MODEL` set to the task's model when the task has one, and not set
  by the plugin otherwise;
- the variables of the adapter protocol's `AgentEnvironment`, as described
  in [Agent environment](#agent-environment);
- the runtime's `command` and `args` exactly: systemd expands no `$` in
  them (`no-env-expand`).

A unit that cannot start is reported as never started:

| Class | Cause | Reason |
| --- | --- | --- |
| `exec` | exit status 203: the command could not be executed | `systemd exit status 203/EXEC` |
| `credentials` | exit status 243: a credential file was missing | `systemd exit status 243/CREDENTIALS` |
| `setup` | another of systemd's own exit statuses, 200 to 244, or a local write failed before the unit | `systemd exit status <n>/<NAME>`, or `input <name>: <error>` |
| `start` | the start job ended `canceled`, `timeout` or `dependency`, or the unit never ran its command | the job result or the unit's `Result` |
| `fetch` | an S3 input was not fetched | `input <name>: not fetched` |

A run with a credential starts the credential wrapper, `/bin/sh`, so its
unit starts whatever the runtime command is, and a missing command ends the
agent with exit code 127. The journal holds systemd's own account of a
failed start; read it with `journalctl -u <unit>`.

On systemd 252 and 253, `systemctl daemon-reload` loads a running agent
unit again as `bad-setting`, without its command: those releases write
`ExecStartEx=` into the transient unit file, a key their unit file parser
does not know. The agent runs on, and the plugin follows, stops and
collects the run as before.

A run whose marker reached `starting` while its unit never appeared, as
after a receiver crash in that window, is never started with class `start`
when its standard output is empty, and ended otherwise.

The plugin follows the agent's standard output from its file by byte
offset; the file is never rotated, so every offset stays readable until
the run is purged. Standard error goes to its own file, which the plugin
never reads, so a standard error line cannot split the result line. Read
it as root until the run is purged. systemd's own messages about the unit
stay in the journal.

The plugin stops an agent by sending `SIGTERM` to every process of the
unit, and waiting up to 15 seconds for its cgroup to empty; then `SIGKILL`
and 15 seconds more. It never stops the unit, which keeps the exit status.
Processes the agent leaves behind after a clean exit run until then. If any
process outlives `SIGKILL`, the stop fails with `the agent outlived SIGKILL`.

The plugin collects outputs from the work directory's `outputs` as root
once no process of the unit is left. It lists the tree with `lstat`, never
descending a link, and refuses the tree, reading nothing, when it holds
more entries than the receiver's limit. It reads only regular files of the
listed size within the limits, through an `os.Root` over the outputs
directory; links, FIFOs and devices are listed with their type and never
read. Outputs stay on disk after any agent death, an OOM kill included.

When a run ends, the plugin records how it ended in the marker, stops and
resets the unit, which drops its credential store, its dynamic user and
its private `/tmp`, and removes the prompt and inputs. It never stops a unit
whose cgroup holds a process: a dynamic UID is reused once its unit lets it
go, and a process still running under it would share it with a later run.
The work directory, standard output, standard error and outputs stay until
the [sweep](#sweep). Purging a run stops its unit, waits for it to unload,
then removes the marker first and the run's directories after it.

`runtime.user` runs every run of the adapter as one fixed user. Concurrent
runs of one user can signal, `ptrace` and read the environment, credential
included, of each other's processes, and read each other's work
directories; `ProtectProc=invisible` hides only other users' processes.
Set it only for an agent that needs a provisioned identity.

Disk use for standard output, standard error and the work directory is
unbounded. Size the filesystem holding `/var/lib` for `capacity` times the
outputs and transcript of a run. That sizing does not bound a hostile
tenant, whose outputs share a filesystem with the receiver's signing key.
A separate filesystem mounted at `/var/lib/private/wherry-agents`, or an
XFS project quota on it, bounds them.

### Credentials

systemd's `LoadCredential=` reads every credential when the unit starts.
Dispatch writes the agent credential as `.credential` and each task
credential under its file name to `/run/wherry-systemd/<unit>/`, root 0600
in a root 0700 directory, before the start, and removes the directory
when the start job ends, whatever its result. The sweep removes any such
directory whose unit is not starting. The plugin never uses
`SetCredential=`, whose value any local user reads from the unit's
properties and its transient unit file.

systemd copies the credentials into the unit's store,
`/run/credentials/<unit>/` on the host, readable by root and the run's
user, until the unit stops. The agent sees the store read-only at
`/run/wherry/credentials`, on a separate filesystem from `/wherry`, so the
agent cannot hard-link a credential into its outputs. Where the store is a
ramfs, as systemd 252 mounts it first, each file is 0400 owned by the
agent's UID in a 0500 directory it owns. Where the store is a tmpfs, as
systemd 255 mounts it first on Linux 6.3 and later, each file is root
0400 in a root 0500 directory, and an ACL entry gives the agent's UID
read access, and search on the directory; `stat` shows them as 0440 and
0550, since the group bits of a file with an ACL are its mask, and the
owning group's entry grants nothing. Either way only root and the agent's
UID read the credentials. `ssh` accepts the key in both.
A run without credentials sees `/run/wherry/credentials` empty.

The credential variable is set by a shell wrapper over the runtime command,
which reads `/run/wherry/credentials/.credential` and keeps its trailing
newlines, then execs the command; the agent keeps the unit's main PID.
Without a credential the unit runs the runtime command alone.

The credential IDs and values of a run together hold at most 1 MiB, as
systemd allows, and so do their files counted in whole pages, as the
1 MiB tmpfs systemd 255 mounts for the store holds them. Dispatch refuses
a run over either with
`the credentials exceed systemd's 1 MiB credential limit`, and a run whose
credential holds a NUL byte, creating nothing. Inputs are files and are not
bounded by it. The plugin declares no inline limit; the pool's inline limit
and the record limit bind. A delivered input name is at most 255 bytes.

The agent credential is in the agent's environment, and so in
`/proc/<pid>/environ` of the agent and its children. `LimitCORE=1` keeps
systemd-coredump from writing a crashed agent's environment to the
journal. A core file, where one is written under
`/var/lib/systemd/coredump`, holds the agent's memory and is root-readable
until vacuumed. An agent process may lower its own core limit to 0, which
re-enables the handler for it.

### Sweep

On each sweep the plugin reads the control directories of the adapter's
pool, each marker and its unit, and ignores units of the pool that have no
marker. A run has ended when its main process has exited, its unit is not
loaded, or it never started; its end time is the main process's exit,
else the unit's last deactivation, else the later change of its marker and
standard output.

- The adapter's own ended runs are settled at once, as at the end of a
  run, and purged 10 minutes after they ended.
- Other adapters' runs ended 10 minutes ago are settled, which drops their
  credential store and deletes their prompt and inputs, and kept until an
  operator deletes them.
- A marker in phase `prepared` with no unit, 10 minutes old, is a Dispatch
  cut short; its directories are removed.
- Control or work directories of the pool without a marker, 10 minutes
  old, are removed.

An adopter can read an ended run's standard output for 10 minutes.
Ownership is by adapter name. One receiver per host carries a pool's
systemd adapters: two would share the control directories and
`/run/wherry-systemd`.

### S3 inputs

The plugin fetches each S3 input on the receiver's host before the unit
exists, with a client that follows no redirect and reads at most one byte
past the input's size. It writes the input to a temporary file in the
inputs directory, checks its size and SHA-256, and moves it into place. A
fetch that fails or does not verify ends the run never started with class
`fetch`, which the receiver ends `TASK_STATE_FAILED`, and the plugin logs
`input not fetched` with the status code or failure class. The presigned
URL never reaches a file, a unit property, a log line or an error, and
agents need no route to the storage endpoint. The placement metric
includes the fetch time.

### Agent environment

The plugin sets the variables of the adapter protocol's `AgentEnvironment`
over the runtime's `env` and removes those of
[`AgentUnset`](https://github.com/benemon/wherry/blob/main/adapter/adapter.go)
from it, so no system, global or environment `git` configuration reaches
the agent's clones. CA and proxy variables pass through as the operator
sets them. This protects an agent that follows its prompt. An agent that
runs commands of its own can undo this.

Any local user, an agent included, reads every unit's `Description`,
`ExecStart` and `Environment` over the system bus, so run IDs, adapter
names, the configured command and the configured environment are public on
the host. Keep material out of `runtime.env`.

## Metrics

The plugin speaks adapter protocol version 7, and a receiver of another
protocol version refuses it, so upgrade it with the receiver. For each
adapter it reports these families, which the receiver serves on its
`/metrics` with the adapter's name as `adapter`:

| Family | Type | Labels | Counts |
| --- | --- | --- | --- |
| `wherry_adapter_platform_errors_total` | counter | `class` | failed systemd and filesystem calls |
| `wherry_adapter_cleanup_pending` | gauge | none | the adapter's ended runs the latest sweep left |
| `wherry_adapter_sweep_deletions_total` | counter | `kind` | the adapter's runs a sweep purged (`job`) and stale material of a Dispatch cut short it removed (`variable`) |
| `wherry_adapter_placement_seconds` | histogram | none | time from a Dispatch that starts the unit to its start job's end, fetches included |

A D-Bus call's class comes from its error name: `AccessDenied` and
`InteractiveAuthorizationRequired` are `denied`; `UnitExists`,
`TransactionIsDestructive`, `TransactionJobsConflicting` and
`JobTypeNotApplicable` `conflict`; `NoReply`, `Timeout`, `TimedOut` and a
call past its deadline `timeout`; a closed connection, `ServiceUnknown`,
`NoServer` and `Disconnected` `unavailable`. A filesystem call's class
comes from its errno: `EACCES`, `EPERM` and `EROFS` are `denied`, and
`ENOSPC`, `EDQUOT` and `EIO` `unavailable`. Any other failure is `other`.
`NoSuchUnit`, an absent file, which the plugin reads as absence, and a call
the receiver cancels count nothing. A connection that `daemon-reexec`
closes is dialled again on the next call.

A failed call the plugin returns reaches the receiver with its operation,
class and systemd's text, and the receiver logs it redacted. A failed call
the plugin handles itself, such as a poll or the removal of credential
files, logs one warning, `systemd call failed`, naming the adapter, the
operation and the class; the line holds no error text.

## Agent network reach

The operator guide's
[Agent network reach](https://github.com/benemon/wherry/blob/main/docs/operator-guide.md#agent-network-reach)
says what an agent job's egress policy denies and allows.

Agents have the host's network: everything the host reaches. On loopback
and the host's addresses that includes:

- the receiver's `/metrics`, which has no authentication and shows what
  any pool tenant's metrics reader sees;
- any other service listening on the host.

Beside the network, an agent reaches the system bus, where it reads every
unit's properties. The journal's sockets are hidden from agents. Agents
run as dynamic users, so files on the host are closed to them by mode,
but a world-readable file is readable.

The plugin does not restrict agents' egress. The knobs are
`IPAddressDeny=` and `IPAddressAllow=` on `wherry.slice`, which match
addresses only, or an `nftables` rule matching the dynamic UID range,
61184 to 65519, with `meta skuid`.

## Run the receiver as a systemd service

The receiver runs as root under its own unit,
[`deploy/wherry-receiver.service`](deploy/wherry-receiver.service), and
its agent units run in [`deploy/wherry.slice`](deploy/wherry.slice),
outside the receiver's cgroup. Restarting or stopping the receiver never
touches them: the receiver exits on `SIGTERM` without stopping its jobs,
and the restarted receiver adopts them. A run is adopted only by a
receiver with the same adapter on the same host, so give each systemd
receiver a pool of its own.

### Install

1. Install `/usr/local/bin/wherry` (root, 0755) and
   `/usr/local/libexec/wherry/plugins/wherry-plugin-systemd` (root, 0755, in
   a root 0755 directory). A binary copied from a home directory keeps the
   `user_home_t` label, so restore the labels:

   ```sh
   restorecon -Rv /usr/local/bin /usr/local/libexec/wherry
   ```

2. Create the agents' state roots and the mount point. `/var/lib/private`
   is systemd's, root 0700:

   ```sh
   install -d -m 0700 /var/lib/private
   install -d -m 0755 /var/lib/private/wherry-agents /var/lib/wherry-agents /wherry
   ```

3. Install the units and the tmpfiles.d line, then create
   `/run/wherry/credentials`, which every boot recreates:

   ```sh
   install -m 0644 deploy/wherry.slice deploy/wherry-receiver.service /etc/systemd/system/
   install -m 0644 deploy/wherry-tmpfiles.conf /etc/tmpfiles.d/wherry.conf
   systemd-tmpfiles --create /etc/tmpfiles.d/wherry.conf
   ```

4. Record `sysctl kernel.core_pattern`, which says whether a core dump
   pipes to systemd-coredump, abrt or neither.
5. Write the bundle `wherry admin receiver add systemd-1` produced to
   `/etc/wherry/systemd-1.bundle`, root 0600, and the receiver
   configuration it names.
6. Start the receiver:

   ```sh
   systemctl daemon-reload && systemctl enable --now wherry-receiver
   ```

The receiver's metrics listen on port 9465. The endpoint has no authentication; admit only Prometheus to it
with a `firewalld` rule.

### SELinux

Before each agent starts, a child of PID 1 opens the run's `stdout` and
`stderr` for append and chowns and bind-mounts its work directory, in
PID 1's domain. The policy must let that domain manage the run
directories, `/var/lib/wherry/systemd` and
`/var/lib/private/wherry-agents`. A denial shows as runs that never start
with class `setup`, such as `systemd exit status 209/STDOUT`. Check for
denials after the first runs:

```sh
ausearch -m AVC,USER_AVC,SELINUX_ERR -ts recent
```

The type names below are those of the targeted policy that Fedora and
RHEL-family distributions ship, as observed on RHEL 9. Other policies may
name the types differently.

PID 1 runs as `init_t`, and `/var/lib/wherry`,
`/var/lib/private/wherry-agents` and `/var/lib/wherry-agents` are
`var_lib_t`. Label the run directories `init_var_lib_t`, which `init_t`
manages:

```sh
semanage fcontext -a -t init_var_lib_t '/var/lib/wherry/systemd(/.*)?'
semanage fcontext -a -t init_var_lib_t '/var/lib/private/wherry-agents(/.*)?'
restorecon -Rv /var/lib/wherry/systemd /var/lib/private/wherry-agents
```

The receiver and the plugin, both `bin_t` under `/usr/local`, run as
`unconfined_service_t`. An agent unit whose command starts with `/bin/sh`,
as every run with a credential does, runs as `initrc_t`; one whose command
is a `bin_t` binary runs as `unconfined_service_t`. Both are unconfined
domains, so DAC and the unit's sandbox confine the agent.

## Tests

```sh
make test
```

The unit tests run the plugin against a fake of the systemd calls it
makes, which runs each unit's command as a child process in a process
group of its own with the unit's environment, working directory, log
files and credentials, and no namespacing. `testdata/` holds unit property
sets of each state the plugin reads.

The tests that need systemd run when `WHERRY_SYSTEMD_DEV=1` and the test
process is root on a host with systemd 252 or later and cgroup v2, and
skip otherwise. They need no Go toolchain on the host: build the test
binary and the plugin for the host's architecture beforehand, into
`dist/test`, and copy them to the host:

```sh
cd systemd
GOOS=linux GOARCH=amd64 go test -c -o ../dist/test/systemd.test .
GOOS=linux GOARCH=amd64 go build -trimpath -o ../dist/test/wherry-plugin-systemd .
```

The suite reads two variables:

| Variable | Value |
| --- | --- |
| `WHERRY_SYSTEMD_DEV` | `1` runs the tests that need systemd |
| `WHERRY_SYSTEMD_PLUGIN` | the path of the plugin binary `TestConformanceOverTheLink` launches |

`TestReceiverBoundingSetOnSystemd` runs the test binary again as a
transient unit, so PID 1 must be able to execute it: on SELinux, install
both binaries under `/usr/local/libexec` and restore their labels, as
[Install](#install-1) does for the plugin. The tests use the plugin's own
host paths, with a pool of their own per test, so prepare the host as
[Install](#install-1) does, then run the suite as root:

```sh
install -d -m 0700 /run/wherry-systemd /var/lib/private
install -d -m 0755 /var/lib/private/wherry-agents /var/lib/wherry-agents /run/wherry/credentials /wherry
install -D -m 0755 -t /usr/local/libexec/wherry/test dist/test/systemd.test dist/test/wherry-plugin-systemd
restorecon -Rv /usr/local/libexec/wherry
cd /usr/local/libexec/wherry/test
WHERRY_SYSTEMD_DEV=1 WHERRY_SYSTEMD_PLUGIN=/usr/local/libexec/wherry/test/wherry-plugin-systemd \
  ./systemd.test -test.count=1 -test.v -test.run 'OnSystemd|Conformance|Transient|DaemonReload|TwoRuns|UnitPropertiesOn'
```

They include the protocol's conformance suite,
`github.com/benemon/wherry/adapter/adaptertest`, in process, against the
plugin binary, and under the receiver unit's capability bounding set. CI
builds both binaries and runs them on the GitHub runner's systemd, 255
on Ubuntu 24.04.
