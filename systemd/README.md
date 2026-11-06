# wherry-plugin-systemd

The wherry plugin for systemd. A wherry receiver runs it from its plugin
directory for its `systemd` plugin, and one plugin process hosts every
plugin profile of that plugin. The plugin runs each of the receiver's runs
as one transient service on the receiver's own host.

systemd 252 or later with the unified cgroup v2 hierarchy, on Linux, is the
platform. The plugin connects to systemd's
private socket, `/run/systemd/private`, and runs as root.

## Build

The plugin builds against the tagged plugin protocol module,
`github.com/benemon/wherry/pluginsdk`. From a clone of this repository:

```sh
make build
```

This writes `wherry-plugin-systemd` at the repository root, built for
Linux on any OS; `GOARCH` sets the architecture. `make check` runs the CI
gates, and the release build, `systemd/.goreleaser.yaml`, writes release archives for linux amd64 and
arm64, and `checksums.txt`, into `dist` (see the [repository README](../README.md)).

## Install

Download and verify the release binary using
[Install plugins](https://benemon.github.io/wherry/operations-receivers#install-plugins)
with `plugin=systemd`, or use [Build](#build).
Install it as `/usr/local/libexec/wherry/<version>/plugins/wherry-plugin-systemd`
beside the version's `wherry`, with root ownership and mode 0755.
The receiver runs the plugin only from a binary whose SHA-256 is the pin its record holds for the type:
`plugin_sha256: {systemd: HEX}` in the receiver's file, which `wherry operator receiver apply` records.
The plugin is not run by hand.
[Run the receiver as a systemd service](#run-the-receiver-as-a-systemd-service)
covers the version symlinks, local configuration and pin.

## Credentials

The plugin takes no platform credential: it talks to its own host's
systemd as root. A plugin profile whose merged `config` names a
`credential` fails its configuration with
`the systemd plugin takes no platform credential`.

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

The `config` keys below are set on the `systemd` plugin's `config`, which
every plugin profile inherits, or on a plugin profile's `config`, and wherry
merges the two as
[Plugin profile merge](https://benemon.github.io/wherry/configuration-receiver#plugin-profile-merge)
describes. The `credential` key names a [platform credential](#credentials),
which this plugin refuses. The plugin reads the merged `config`:

| Field | Default | Content |
| --- | --- | --- |
| `runtime` | required | the agent's command, identity and limits |

Unknown fields are errors. The plugin checks the merged section when the
receiver configures the plugin profile, and the receiver exits with
`profile <name>: ...` on an error.

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
It sets `HOME` and the `WHERRY_*` variables over `env`, so a variable of
these names in `env` is overwritten.

Configure also fails when:

- the plugin does not run as uid 0: `the systemd plugin runs as root`;
- the manager's `Version` is below 252: `systemd 252 or later is required`;
- `/sys/fs/cgroup` is not cgroup2: `systemd's unified cgroup hierarchy is required`;
- `/var/lib/wherry/systemd/runs`, `/var/lib/private/wherry-agents` or
  `/var/lib/wherry-agents` cannot be created or written;
- `/run/wherry-systemd` is not a directory owned by root with mode 0700:
  `/run/wherry-systemd must be root 0700`.

A plugin profile is ready while the manager's `SystemState` is `running`,
`degraded` or `starting`. Any failed unit on the host sets `degraded`, a
failed agent unit included until its run is released. `starting` holds
while a boot job is still queued after `basic.target` is active, which an
agent unit's default dependencies wait for. Readiness does not check the
runtime command: a missing command shows at dispatch as a unit that never
started with class `exec`, or, for a run with an environment target, as an agent
that exits 127.

A plugin profile file in a receiver configuration directory, such as
`plugins/systemd/profiles/10-host.yaml`:

```yaml
name: host
agent: claude
labels:
  platform: systemd
  host: <name>
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
    resources: {cpu_quota: 100, memory_mb: 2048}
```

Two receivers meant to share a queue carry identical labels for the profile;
one differing label is a different signature.

`DISABLE_AUTOUPDATER` keeps Claude Code from trying to update itself on
the read-only root. Claude Code runs its shell tool through `$SHELL`.

### Receiver configuration

This receiver configuration file holds the `systemd` plugin and two plugin
profiles. The plugin's `labels` and `config` hold what both share, and each
plugin profile's own merge onto them: `provisioned` replaces the
`platform` label and both run with the plugin's `resources`.
[`testdata/receiver.yaml`](testdata/receiver.yaml) holds this file, and
`TestReadmeReceiverConfigures` configures each plugin profile from it,
merged, through the plugin's decoding and checks.

```yaml
plugins:
  systemd:
    # Every plugin profile of this plugin inherits these keys and can override them.
    labels:
      platform: systemd
    config:
      runtime:
        resources:
          cpu_quota: 100
          memory_mb: 2048
    profiles:
      # Claude Code, each run as a dynamic user of its own.
      - name: host
        agent: claude
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
      # An agent that needs a provisioned identity, run as a fixed user that
      # every run of the plugin profile shares.
      - name: provisioned
        agent: reviewer
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

Dispatch returns once the unit's start job has, with a handle naming the
unit and the marker's instance; a dispatch that finds the run's unit or
marker already there returns that handle at once. Lookup reads the marker
and the unit: running, ended or never started, never started with its class
and reason. systemd never reports a run pending: a unit still `activating`
is waited on, polling every 2 seconds, until it settles or the receiver
cancels the Lookup, which stops nothing.

The marker records the run's pool, plugin profile, agent, attempt and a random
instance per dispatch, the phase (`prepared`, `starting`, `started`), and
how the run ended once the plugin settles it. It outlives the unit, so a
restarted receiver finds every run it started, and Lookup reports the
attempt, plugin profile and agent from it. It names the plugin profile
in its `profile` field, and the unit's `Description` is
`wherry agent <run> (profile <name>)`.

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
- each file target as a file in `/run/wherry/credentials`, and each
  environment target in the environment variable named by its target, as
  described in [Credential targets](#credential-targets);
- its outputs directory, which it creates, at `/wherry/outputs`;
- `HOME` set to `/wherry`, the run's own work directory, so no agent state,
  history or login carries from one run to the next;
- `SHELL` set to `/bin/sh` unless `env` sets it;
- `WHERRY_INPUT_DIR`, `WHERRY_OUTPUT_DIR` and `WHERRY_CREDENTIALS_DIR` set
  to `/wherry/inputs`, `/wherry/outputs` and `/run/wherry/credentials`,
  the same paths the prompt names;
- `WHERRY_MODEL` set to the task's model when the task has one, and not set
  by the plugin otherwise;
- the variables of the plugin protocol's `AgentEnvironment`, as described
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

A run with an environment target starts the credential wrapper, `/bin/sh`,
so its unit starts whatever the runtime command is, and a missing command
ends the agent with exit code 127. The journal holds systemd's own account
of a failed start; read it with `journalctl -u <unit>`.

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
never reads, so a standard error line cannot split a transcript line. Read
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
read. An output the receiver streams, an S3 output or the agent's
[outcome file](https://benemon.github.io/wherry/configuration-receiver#agent-contract), it streams as the receiver reads it. Outputs stay on disk after any agent death, an OOM kill included.

When a run ends, the plugin records how it ended in the marker, stops and
resets the unit, which drops its credential store, its dynamic user and
its private `/tmp`, and removes the prompt and inputs. It never stops a unit
whose cgroup holds a process: a dynamic UID is reused once its unit lets it
go, and a process still running under it would share it with a later run.
The work directory, standard output, standard error and outputs stay until
the [sweep](#sweep). Purging a run stops its unit, waits for it to unload,
then removes the marker first and the run's directories after it.

`runtime.user` runs every run of the plugin profile as one fixed user. Concurrent
runs of one user can signal, `ptrace` and read the environment, credential
included, of each other's processes, and read each other's work
directories; `ProtectProc=invisible` hides only other users' processes.
Set it only for an agent that needs a provisioned identity.

Disk use for standard output, standard error and the work directory is
unbounded. Size the filesystem holding `/var/lib` for the sum of the receiver's
profile `replicas` times the outputs and transcript of a run. That sizing does not bound a hostile
tenant, whose outputs share a filesystem with the receiver's signing key.
A separate filesystem mounted at `/var/lib/private/wherry-agents`, or an
XFS project quota on it, bounds them.

### Credential targets

systemd's `LoadCredential=` reads each target when the unit starts.
Dispatch writes an environment target as `.env.<variable>` and a file
target under its file name to `/run/wherry-systemd/<unit>/`, root 0600 in a
root 0700 directory, before the start, and removes the directory when the
start job ends, whatever its result. The sweep removes any such directory
whose unit is not starting. The plugin never uses `SetCredential=`, whose
value any local user reads from the unit's properties and its transient
unit file.

systemd copies the targets into the unit's credential store,
`/run/credentials/<unit>/` on the host, until the unit stops; the
store's file system and modes are systemd's
([Credentials](https://www.freedesktop.org/software/systemd/man/latest/systemd.exec.html#Credentials)).
Only root and the agent's UID read the targets, and the unit's credential
store is the boundary that enforces it. The agent sees the store read-only
at `/run/wherry/credentials`, on a separate filesystem from `/wherry`, so
the agent cannot hard-link a target into its outputs. `ssh` accepts a key
from it. A run without targets sees `/run/wherry/credentials` empty.

For each environment target, a shell wrapper reads
`/run/wherry/credentials/.env.<variable>`, keeps trailing newlines, and
exports the value before it execs the runtime command, so an environment
target replaces a runtime `env` variable of the same name, and one named
`CREDENTIALS_DIRECTORY`, `STATE_DIRECTORY` or `INVOCATION_ID` replaces the
value systemd sets. The agent keeps the unit's main PID. A file target remains available under its file name in
`/run/wherry/credentials`.

The target IDs and values of a run together hold at most 1 MiB, as systemd
allows, and so do their files counted in whole pages, as the 1 MiB tmpfs
systemd 255 mounts for the store holds them. Dispatch refuses a run over
either with `the credentials exceed systemd's 1 MiB credential limit`, and
a run whose environment target holds a NUL byte, creating nothing. Inputs
are files and are not bounded by it. The plugin declares no inline limit;
the record limit binds. A delivered input name
is at most 255 bytes.

Environment targets are in the agent's environment, and so in
`/proc/<pid>/environ` of the agent and its children. `LimitCORE=1` keeps
systemd-coredump from writing a crashed agent's environment to the
journal. A core file, where one is written under
`/var/lib/systemd/coredump`, holds the agent's memory and is root-readable
until vacuumed. An agent process may lower its own core limit to 0, which
re-enables the handler for it.

### Sweep

On each sweep the plugin reads the control directories of the plugin profile's
pool, each marker and its unit, and ignores units of the pool that have no
marker. A run has ended when its main process has exited, its unit is not
loaded, or it never started; its end time is the main process's exit,
else the unit's last deactivation, else the later change of its marker and
standard output.

- The plugin profile's own ended runs are settled at once, as at the end of a
  run, and purged 10 minutes after they ended.
- Other plugin profiles' runs ended 10 minutes ago are settled, which drops their
  credential store and deletes their prompt and inputs, and kept until an
  operator deletes them.
- A marker in phase `prepared` with no unit, 10 minutes old, is a Dispatch
  cut short; its directories are removed.
- Control or work directories of the pool without a marker, 10 minutes
  old, are removed.

An adopter can read an ended run's standard output for 10 minutes.
Ownership is by plugin profile name. One receiver per host carries a
pool's systemd plugin profiles: two would share the control directories and
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
agents need no route to the storage endpoint.

### Agent environment

The plugin sets the variables of the plugin protocol's `AgentEnvironment`
over the runtime's `env` and removes those of
[`AgentUnset`](https://github.com/benemon/wherry/blob/pluginsdk/v0.9.0-rc.1/pluginsdk/pluginsdk.go#L264)
from it, so no system, global or environment `git` configuration reaches
the agent's clones. CA and proxy variables pass through as the operator
sets them. A command the agent runs can set the variables again.

Any local user, an agent included, reads every unit's `Description`,
`ExecStart` and `Environment` over the system bus, so run IDs, plugin
profile names, the configured command and the configured environment are public on
the host. Keep material out of `runtime.env`.

## Metrics

The plugin speaks plugin protocol version 11, in which Dispatch returns
once the unit's start job has. A receiver of another protocol version
refuses it, so the receiver and its plugins upgrade together: protocol 11
needs wherry v0.1.0-rc.29 or later. The receiver times placement itself.
For each plugin profile the plugin reports these families, which the
receiver serves on its `/metrics` with the plugin profile's name as
`profile`:

| Family | Type | Labels | Counts |
| --- | --- | --- | --- |
| `wherry_plugin_platform_errors_total` | counter | `class` | failed systemd and filesystem calls |
| `wherry_plugin_cleanup_pending` | gauge | none | the plugin profile's ended runs the latest sweep left |
| `wherry_plugin_sweep_deletions_total` | counter | `kind` | the plugin profile's runs a sweep purged (`job`) and stale material of a Dispatch cut short it removed (`variable`) |

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
files, logs one warning, `systemd call failed`, naming the plugin profile
under `profile`, the operation and the class; the line holds no error text.

## Agent network reach

The operator guide's
[Agent network reach](https://benemon.github.io/wherry/operations-hosts#agent-network-reach)
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

<!-- #region install -->
## Run the receiver as a systemd service

Start with your operator working directory, pool and entitled tenant ready.
Use a Linux host with systemd 252 or later and unified cgroup v2.
Run the receiver as root.
Give each systemd receiver a `host` label of its own, so only the receiver restarted on that host serves its signature and adopts its runs.
Clone `https://github.com/benemon/wherry-plugins` and check out the plugin's tag `systemd/<version>`, such as `systemd/v0.1.0-rc.7`.
Run host installation commands as root from that checkout.

1. In an empty directory, download `wherry_<wherry version>_linux_<arch>.tar.gz`, `checksums.txt` and `checksums.txt.sigstore.json` from `https://github.com/benemon/wherry/releases/tag/<wherry version>`.
   `<wherry version>` carries its `v`, such as `v0.1.0-rc.31`.
   `<arch>` is `amd64` or `arm64`.
   Verify the signature of `checksums.txt` with the [cosign command](https://github.com/benemon/wherry-plugins/blob/main/kubernetes/README.md#image).
   Check the archive against the checksums and extract `wherry`:

   ```sh
   sha256sum --ignore-missing -c checksums.txt
   tar -xzf wherry_<wherry version>_linux_<arch>.tar.gz --strip-components=1 wherry_<wherry version>_linux_<arch>/wherry
   ```

   In another empty directory, download and verify the systemd plugin using [Install plugins](https://benemon.github.io/wherry/operations-receivers#install-plugins) with `plugin=systemd`.
   You can instead build the plugin with `make build PLUGINS=systemd` from the checkout.
   Install the binaries in a directory for this deployment version.
   Replace `<wherry directory>` and `<plugin directory>` with the directories that hold the extracted binaries:

   ```sh
   version=<deployment version>
   install -d -m 0755 /usr/local/libexec/wherry/"$version"/plugins
   install -m 0755 <wherry directory>/wherry /usr/local/libexec/wherry/"$version"/wherry
   install -m 0755 <plugin directory>/wherry-plugin-systemd /usr/local/libexec/wherry/"$version"/plugins/
   ln -s "$version" /usr/local/libexec/wherry/current
   ln -s /usr/local/libexec/wherry/current/wherry /usr/local/bin/wherry
   ```

   Keep the binaries and their directories owned by root and writable only by root.
   On SELinux hosts, restore the binary labels with `restorecon -Rv /usr/local/bin /usr/local/libexec/wherry`.
   Follow the [SELinux reference](https://github.com/benemon/wherry-plugins/blob/main/systemd/README.md#selinux) for agent state labels.

2. Obtain the plugin pin:

   ```sh
   sha256sum /usr/local/libexec/wherry/"$version"/plugins/wherry-plugin-systemd
   ```

   For a release binary, check that it matches the binary's entry in the verified release checksums.
   Use this SHA-256 as `plugin_sha256.systemd`.
   The receiver permits symlinks and verifies the resolved plugin's pin.

3. In your operator working directory, create `receivers/systemd-1.yaml` with your pool, `plugin_sha256.systemd` and profiles.
   For example, adapt this record to your pool and installed agent command:

   ```yaml
   pool: build
   plugin_sha256:
     systemd: <the plugin pin>
   plugins:
     systemd:
       profiles:
         - name: host
           agent: claude
           labels:
             host: <name>
           config:
             runtime:
               command: [/bin/sh, -c]
               args:
                 - >-
                   test -n "$WHERRY_MODEL" || WHERRY_MODEL=claude-haiku-5-5;
                   mkdir -p "$WHERRY_INPUT_DIR" "$WHERRY_OUTPUT_DIR" &&
                   exec /usr/local/bin/claude -p --output-format stream-json --verbose --model "$WHERRY_MODEL"
                   --allowedTools "Read,Write,Edit,Glob,Grep,Bash" --add-dir "$WHERRY_INPUT_DIR" --add-dir "$WHERRY_OUTPUT_DIR"
                   < /wherry/prompt.txt
               env:
                 SHELL: /bin/bash
                 DISABLE_AUTOUPDATER: "1"
   ```

   Use the [receiver configuration reference](https://github.com/benemon/wherry-plugins/blob/main/systemd/README.md#receiver-configuration) for agent commands and limits.
   The plugin takes no platform credential.
   Copy [the example config.yaml](https://github.com/benemon/wherry-plugins/blob/main/examples/systemd/config.yaml) to `receivers/systemd-1.config.yaml` before applying:

   ```sh
   wherry operator receiver apply systemd-1
   ```

   You can instead provide `config.yaml` in the configuration directory passed with `-f DIR`.
   See [receiver local configuration](https://benemon.github.io/wherry/configuration-receiver#receiver-local-configuration).
   Set `ca_file` in the supplied file when your broker uses a private CA.
   Keep the bundle and local configuration outside version control.

4. Create the agents' state roots and install the units and tmpfiles configuration:

   ```sh
   install -d -m 0700 /var/lib/private
   install -d -m 0755 /var/lib/private/wherry-agents /var/lib/wherry-agents /wherry
   install -m 0644 examples/systemd/wherry.slice examples/systemd/wherry-receiver.service /etc/systemd/system/
   install -m 0644 examples/systemd/wherry-tmpfiles.conf /etc/tmpfiles.d/wherry.conf
   systemd-tmpfiles --create /etc/tmpfiles.d/wherry.conf
   ```

   The [receiver unit](https://github.com/benemon/wherry-plugins/blob/main/examples/systemd/wherry-receiver.service) starts `/usr/local/bin/wherry`.
   Its [agent slice](https://github.com/benemon/wherry-plugins/blob/main/examples/systemd/wherry.slice) keeps agent units outside the receiver's cgroup.
   Follow [systemd service documentation](https://www.freedesktop.org/software/systemd/man/latest/systemd.service.html) for service administration.
   The [receiver service reference](https://github.com/benemon/wherry-plugins/blob/main/systemd/README.md#receiver-service) describes the unit, the credential and the metrics port.

5. Install the bundle and the local configuration you supplied to `receiver apply`:

   ```sh
   install -D -m 0600 <operator directory>/bundles/receivers/systemd-1.bundle /etc/wherry/receiver.bundle
   install -m 0600 <operator directory>/receivers/systemd-1.config.yaml /etc/wherry/config.yaml
   ```

   For a private broker CA, install its certificates at the configured `ca_file` path.
   The unit loads the bundle as a [systemd credential](https://www.freedesktop.org/software/systemd/man/latest/systemd.exec.html#Credentials).
   The configuration names `/run/credentials/wherry-receiver.service/bundle` as its bundle.
   Its `plugin_dir` is `/usr/local/libexec/wherry/current/plugins`.
   Preserve `/var/lib/wherry/state` because it holds the receiver's signing key.
   Restrict the metrics port to your scraper.
   Record `sysctl kernel.core_pattern` to identify your host's core dump handling.

6. Start the receiver:

   ```sh
   systemctl daemon-reload
   systemctl enable --now wherry-receiver
   journalctl -u wherry-receiver
   ```

   Check for `plugin systemd: version <plugin version> protocol <n>`.
   `<plugin version>` is the version of the plugin tag, such as `v0.1.0-rc.7`.
   A source build without version metadata reports `unreported`.
   From your entitled tenant's host, run `wherry host receivers` and wait for `systemd-1` to show `live`.

### Upgrade

1. Verify and install the new binaries in `/usr/local/libexec/wherry/<new version>/` beside the old version.
   Obtain the new plugin pin.
2. Update `plugin_sha256.systemd` and run `wherry operator receiver apply systemd-1` in your operator working directory.
   [Change a receiver's configuration](https://benemon.github.io/wherry/operations-operator#change-a-receiver-s-configuration) describes this command.
   Until the new version runs, the receiver's restarts refuse the old plugin binary.
3. Immediately repoint `current` and restart:

   ```sh
   ln -sfn <new version> /usr/local/libexec/wherry/current
   systemctl restart wherry-receiver
   ```

   Install any changed bundle and local configuration before restarting.
   Keep the receiver's state directory and agent state.
4. Check the identity line and wait for the receiver to show `live`.
   To roll back, apply the previous plugin pin, repoint `current` to the previous version and restart.

### Uninstall

1. Revoke the receiver in your operator working directory:

   ```sh
   wherry operator receiver remove systemd-1
   ```

2. Stop and disable its service:

   ```sh
   systemctl disable --now wherry-receiver
   ```

3. Review remaining agent units with `systemctl list-units 'wherry-agent-*'`.
   Stop them when their runs are no longer needed.
   Remove `/etc/systemd/system/wherry-receiver.service`, `/etc/systemd/system/wherry.slice` and `/etc/tmpfiles.d/wherry.conf` when no service uses them.
   Run `systemctl daemon-reload`.
4. Remove `/etc/wherry/receiver.bundle` and `/etc/wherry/config.yaml` and any installed broker CA.
   Remove the receiver's state, agent material and credential mount point when they are no longer needed.
   Remove the version directories, `current` and `/usr/local/bin/wherry` when no receiver uses them.
   Remove the operator's local bundle and configuration copies.
<!-- #endregion install -->

## Receiver service

The receiver runs as root under its own unit,
[`examples/systemd/wherry-receiver.service`](../examples/systemd/wherry-receiver.service).
The unit runs `wherry receiver run --config-file /etc/wherry/config.yaml`.
`/usr/local/bin/wherry` points at `/usr/local/libexec/wherry/current/wherry`.
`current` selects the version directory `/usr/local/libexec/wherry/<version>/`.
Its agent units run in [`examples/systemd/wherry.slice`](../examples/systemd/wherry.slice), outside the receiver's cgroup.
Restarting or stopping the receiver never touches them.
The receiver exits without stopping its jobs.
The restarted receiver adopts them.
The local configuration's `drain_timeout` is 20 seconds.
The unit's `TimeoutStopSec=` is 30
([Drain timeout and the stop window](https://benemon.github.io/wherry/configuration-receiver#drain-timeout-and-the-stop-window)).
Only a receiver with the same plugin profile on the same host adopts a run.

On a host with SELinux enabled, a binary copied from a home directory keeps the `user_home_t` label.
That is why the install restores the labels of `/usr/local/bin` and `/usr/local/libexec/wherry`.
`/var/lib/private` is systemd's directory, owned by root with mode 0700.
`/run/wherry/credentials` is the mount point agent units bind their credential store on.
Every boot recreates it from the tmpfiles.d line.
`kernel.core_pattern` says whether a core dump pipes to systemd-coredump, abrt or neither.

[`examples/systemd/config.yaml`](../examples/systemd/config.yaml) is the receiver's local configuration.
It names the bundle, the state directory, the plugin directory and the metrics address.
Its `plugin_dir` is `/usr/local/libexec/wherry/current/plugins`.
For a broker under a private CA, its `ca_file` names `/etc/wherry/kafka-ca.pem`.

The unit takes the bundle as a systemd
[credential](https://www.freedesktop.org/software/systemd/man/latest/systemd.exec.html#Credentials),
with `LoadCredential=bundle:/etc/wherry/receiver.bundle`.
`config.yaml` names the bundle at the credential directory's path, `/run/credentials/wherry-receiver.service/bundle`.
A YAML file cannot expand `$CREDENTIALS_DIRECTORY`, so the configuration names the full path.
A unit of another name has another credential path.
To keep the bundle encrypted on disk, encrypt it with
[`systemd-creds encrypt`](https://www.freedesktop.org/software/systemd/man/latest/systemd-creds.html).
Then load it with `LoadCredentialEncrypted=` in place of `LoadCredential=`.
A new bundle takes effect at the next `systemctl restart wherry-receiver`.

The receiver's metrics listen on port 9464, the configuration's `metrics_addr`.
The endpoint has no authentication.
Admit only Prometheus to it with a `firewalld` rule.

The plugin pin is the SHA-256 of the release binary.
It is the binary's line in the release's verified `checksums.txt`.

## SELinux

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
as every run with an environment target does, runs as `initrc_t`; one whose
command is a `bin_t` binary runs as `unconfined_service_t`. Both are unconfined
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
[Run the receiver as a systemd service](#run-the-receiver-as-a-systemd-service) does for the plugin. The tests use the plugin's own
host paths, with a pool of their own per test, so prepare the host as
[Run the receiver as a systemd service](#run-the-receiver-as-a-systemd-service) does, then run the suite as root:

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
`github.com/benemon/wherry/pluginsdk/pluginsdktest`, in process, against the
plugin binary, and under the receiver unit's capability bounding set. CI
builds both binaries and runs them on the GitHub runner's systemd, 255
on Ubuntu 24.04.

### End to end on the runner's systemd

[`test/e2e`](test/e2e) then runs
[Run the receiver as a systemd service](#run-the-receiver-as-a-systemd-service)
on the same runner, as the `systemd-host` job of the `ci` workflow does for
every change to `systemd/`, `examples/systemd/`, the root `Makefile`,
`.github/changed.sh`, `.github/plugin-protocol.sh` or the workflow. Each
step is a script, run as a user with root through `sudo`: the README's host
commands run as root, and the operator and host commands as the user. With
systemd 252 or later, Docker, cosign, Go, jq and the repository's
`systemd/v*` tags on a disposable Linux machine with no receiver installed,
and the wherry release the workflow pins:

```sh
export WHERRY_VERSION=$(sed -n 's/^  WHERRY_VERSION: //p' .github/workflows/ci.yml)
for step in systemd/test/e2e/0*.sh; do "$step" || { systemd/test/e2e/collect.sh; break; }; done
```

1. `01-broker.sh`, harness, starts a single-broker Kafka with TLS and SCRAM
   on `127.0.0.1:9093` through `test/kafka/up.sh` of `WHERRY_VERSION`,
   pinned by SHA-256.
2. `02-fetch.sh` first runs the plugin protocol check below. It then
   downloads and verifies, each in an empty directory, wherry of
   `WHERRY_VERSION` with step 1's commands, and the plugin of the previous
   release, the latest `systemd/v*` tag before `HEAD`, with the
   [Install plugins](https://benemon.github.io/wherry/operations-receivers#install-plugins)
   commands. The plugin's version is the tag less `systemd/`, with its `v`,
   as the identity line and the release's archive name carry it; the full
   tag selects the release's download route alone. It builds the
   checkout's plugin with version `e2e`, as both Containerfiles do. It
   fails when there is no such tag.
3. `03-provision.sh`, harness, provisions what the procedure starts with:
   `wherry operator init`, the pool `e2e` and the tenant `e2e` entitled to
   it with the host `runner`, then `wherry host init` with the broker's CA
   as `ca.pem` in the host's configuration directory.
4. `04-install.sh` runs steps 1 to 6 with the previous release's plugin:
   it installs the version directory and the `current` and
   `/usr/local/bin/wherry` links, checks the pin against the release's
   checksums, writes `receivers/systemd-1.yaml` from
   [`test/e2e/receiver.yaml`](test/e2e/receiver.yaml), whose plugin profile
   runs `/bin/true`, with
   [`examples/systemd/config.yaml`](../examples/systemd/config.yaml)'s
   `ca_file` set to `/etc/wherry/kafka-ca.pem`, applies it, installs the
   units, the bundle, the configuration and the broker's CA, and starts the
   receiver. The lane submits no run.
5. `05-upgrade.sh` runs [Upgrade](#upgrade)'s steps 1 to 4 to the
   checkout's plugin.
6. `06-rollback.sh` runs Upgrade's rollback sentence: it applies the
   previous release's pin, repoints `current` to its version and restarts.

Immediately before each deliberate start or restart, after the pin and the
repoint, a step records a boundary: the unit's invocation, from
`systemctl show -p InvocationID --value wherry-receiver`, and the cursor of
the journal's newest entry. Within 120 seconds it then reads
`journalctl -u wherry-receiver --after-cursor=<cursor>`, every invocation
after the boundary, failed starts and restarts included, and fails on a
journal it cannot read, on a line ending `and the pin is <pin>` in any of
them, or on any identity line other than
`plugin systemd: version <version> protocol <n>` of the version and
protocol the step installed. It needs a fresh invocation, not the one the
boundary saw, with that identity line. It then waits for
`wherry host receivers` to show `systemd-1` `live`, and checks the same
again and that the unit has not begun another invocation since. A refusal
before the boundary, which Upgrade's order allows between the new pin and
the repoint, does not fail the step.

`02-fetch.sh` runs
[`.github/plugin-protocol.sh`](../.github/plugin-protocol.sh), which reads
the plugin protocols the previous release and the checkout serve: the
pluginsdk each `go.mod` requires, fetched and verified by
`go mod download -json` against the checksum database and that side's
`go.sum`, serves `handshake`'s `ProtocolVersion` from `link.go`, or the
keys of `versionedPlugins` if its `serve.go` sets `VersionedPlugins`. Any
failure fails the step: no tag, a fetch or checksum error, a `replace`,
`go.work` or vendored copy of pluginsdk, or a form the script does not
know. The sides are compatible when they serve a protocol in common. When
they serve none, a 0.x clean break, `04-install.sh` installs the checkout's
plugin, and `05-upgrade.sh` and `06-rollback.sh` skip with a notice naming
both, since the release notes then say to upgrade the receiver and its
plugins together, or to reset.

`collect.sh` writes the receiver unit's journal and status, the agent
slice's status, the last boundary, the protocol check's output and the
broker container's log, but no bundle, credential or configuration, to
`artifacts` in the state directory, `$E2E_WORK`, by default
`$TMPDIR/wherry-systemd-e2e`.
