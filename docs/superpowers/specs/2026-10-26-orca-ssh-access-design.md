# Orca SSH Access and Detached VM Boot — Design

**Date:** 2026-10-26
**Status:** Approved design, pending implementation plan

## Context

Stably Orca manages git worktrees and runs coding agents in them. It can run
an agent locally or on an SSH host, chosen per workspace. Simon wants the
sandboxed agent to be one of those choices: local Claude for some work, Claude
inside the code-vm guest for other work, in the same Orca window.

Two approaches were tried against Orca 1.4.237:

- **Command override** (Orca Settings → Agents → Claude command set to
  `code-vm -- claude`). Works, including agent state in Orca's sidebar, but
  it is global: every Claude pane runs in the sandbox, and the only way back
  to a local Claude is typing `claude` in a terminal pane.
- **SSH host** (spike on 2026-10-26, connecting as `limaadmin` with Lima's
  own key). Orca connected with its built-in SSH transport, uploaded a relay
  daemon and a Node runtime (about 160 MB) into the remote home, created
  worktrees under `~/orca/workspaces/<project>/<name>`, opened remote
  terminals without a compiler in the guest, launched Claude with
  `--dangerously-skip-permissions`, and showed agent state (Idle) **without
  installing any hooks**. Orca wrote nothing under the remote `~/.claude`:
  the only file that appeared there, a settings file holding a theme, was
  written by Claude Code's own first-run theme picker 83 seconds after
  launch. Orca's agent-hook port is served by the relay on the guest's
  loopback and tunnelled back over the SSH session. Orca's Claude profile
  pointer was set but pointed at a missing file, so Claude used its normal
  home configuration. With a ProxyCommand configured, Orca switched to the
  system `ssh` binary, which failed host-key verification because it had no
  trusted entry and no way to ask.

The spike also exposed a code-vm bug unrelated to SSH. Orca runs each pane in
its own systemd user scope with `KillMode=control-group` and deletes a
workspace by stopping that scope. When the code-vm session in a pane is the
one that boots the VM, Lima's host agent and QEMU are spawned inside that
scope and stay there. Deleting that workspace later killed the VM and every
other session in it (journal: a scope stopped at 15:10:31 with a 16.5 GB
memory peak, the QEMU process). Any terminal that kills by control group
would do the same.

Orca-side facts that bound the design:

- Orca's SSH target form takes host, port, username and an optional identity
  file; Orca manages host-key trust itself and prompts in its UI. A changed
  host key is rejected.
- Orca uses its built-in SSH transport unless a ProxyCommand, ControlMaster
  or GSSAPI/FIDO2 is involved; then it uses the system `ssh`.
- Orca offers only agents it finds on the remote PATH of a non-login shell.
- Orca says "Resume from history is only available in local workspaces";
  remote workspaces can browse local history but not resume into it.

## Goals

- A code-vm host can be added to Orca as an SSH target that logs in as the
  agent user (`devuser`), so a workspace created on that target runs its
  agent inside the sandbox, while workspaces on "Local" keep running the
  host's own agent.
- The target stays valid across VM restarts: fixed port, fixed key.
- Everything the target needs is printed by one code-vm command, so setting
  up Orca is copying four values.
- Booting the VM never ties its lifetime to the process that booted it.
  Deleting an Orca workspace, closing a terminal tab or stopping a scope
  must not stop the VM.
- No new capability for the agent, and no new way for anything in the guest
  to reach the host.

## Non-goals (v1)

- On-demand VM boot from Orca (a ProxyCommand that runs code-vm). Viable
  only through the system `ssh` path with an ssh_config alias carrying host
  key options; left as a follow-up.
- Managing the host's `~/.ssh/config` or `known_hosts`. code-vm prints a
  snippet; the user decides where it goes.
- Host-key trust in Orca after `code-vm recreate`. Orca rejects the changed
  key; the user re-trusts or re-adds the target. Documented, not automated.
- Session history for sandboxed sessions. Orca does not resume in remote
  workspaces; nothing code-vm does changes that.
- Orca-side settings such as the remote workspace directory. Default is the
  remote home, invisible to host tools; pointing it inside a mounted repo is
  the user's Orca setting.
- Detached boot on macOS. The scope problem is a systemd/cgroup behaviour;
  macOS boots stay as they are.
- Multiple agent users or per-target keys per project.

## Decisions

| Area | Decision |
|---|---|
| Detached boot | On Linux, when a systemd user manager is reachable, every VM boot runs `limactl start` inside a transient scope via `systemd-run --user --scope --quiet --collect`, so Lima's host agent and QEMU live in their own scope. Otherwise the boot runs as today. |
| Scope naming | Unnamed scope (systemd assigns `run-r….scope`) with a `--description` naming the instance. A fixed unit name could collide with a stale scope and turn a boot into an error. |
| Boot paths covered | All of them: the default invocation, `start`, `recreate`, `mount` — anything that reaches the client's start calls. |
| SSH identity | A dedicated ed25519 keypair per instance, generated by code-vm on the host, stored under the code-vm config directory with 0700 directory and 0600 private key. Never copied into the guest or the Lima template. |
| Authorized key | The public key is the sole content of `/home/devuser/.ssh/authorized_keys` (mode 0600), installed through the existing agent-identity user-file push at every boot and on demand by `ssh-target` when the VM is running and the key is missing. The agent may edit the file between boots; each boot overwrites it. |
| Port | New config field `sshPort` (default 0 = Lima picks a free port, today's behaviour). When set, the Lima template pins `ssh.localPort`. A busy port fails the boot with Lima's own error. |
| Printing the target | New subcommand `code-vm ssh-target` prints host (`127.0.0.1`), port, user (`devuser`), identity file path, and an OpenSSH config snippet. When `sshPort` is unset it prints the current port with a warning that it changes on every boot. |
| Host key trust | Left to Orca (built-in transport) or to the user's ssh (snippet). The snippet sets `IdentitiesOnly yes` and `UserKnownHostsFile` to a file under the code-vm config directory so the guest's key never pollutes the user's main known_hosts. |
| Guest sshd | No change. Lima's image already has public-key auth and TCP forwarding enabled, both verified in the spike; Orca's relay needs forwarding. |
| Firewall | No change. The relay listens on guest loopback; the only host↔guest SSH path is Lima's existing loopback port forward, host-initiated. |
| Resource limits | SSH logins as devuser land in `user-<uid>.slice`, which provisioning caps at `TasksMax=2048` and `MemoryMax=` RAM minus 2 GiB, sized for rootless Docker. Sessions through `sandbox-exec` run outside that slice today. Orca's relay, Node runtime, Claude and the builds they run will share the slice with Docker, so `TasksMax` is raised to 8192 (my call on the number; the builder may size it from pid_max). `MemoryMax` stays: it protects the guest OS and Squid, and the OOM killer inside the slice is the intended outcome of a runaway workload. |

## Detached boot

`limactl start` forks `limactl hostagent`, which in turn runs QEMU, then
exits once the guest is ready. The host agent detaches from the terminal but
not from the caller's cgroup. Running the start command under a transient
user scope moves the whole subtree into a scope that nothing else owns;
`--collect` lets systemd garbage-collect it once the host agent exits after
`code-vm stop`.

Behaviour:

- Detection: `systemd-run` on PATH and a reachable user manager. The builder
  picks the probe (for example `systemd-run --user --scope --quiet --collect
  -- true`, which costs tens of milliseconds and only runs on the boot path,
  not on the per-invocation fast path).
- Output: `limactl start` runs in the foreground inside the scope, so its
  progress output and exit code reach the user exactly as today.
- Failure of the wrapper (systemd-run exits non-zero before running limactl)
  is reported, not silently retried without the wrapper: a user who sees it
  can run `code-vm start` from a plain shell. The builder may choose to
  distinguish "wrapper unavailable" (fall back) from "limactl failed" (report).
- `code-vm doctor` reports whether detached boot is available.

## SSH access as devuser

### Key lifecycle

1. First boot or first `ssh-target` after this feature: code-vm generates the
   keypair if missing. Generation method is the builder's choice (`ssh-keygen
   -t ed25519 -N ''`, or Go's crypto with an OpenSSH private-key encoder);
   the constraint is permissions and no passphrase.
2. At every boot, after the VM is running, the public key is pushed as
   `.ssh/authorized_keys` with mode 0600 through the same relay profiles use
   for rendered templates (`PushUserFile`). sshd's StrictModes accepts a home
   of 0750 and a `.ssh` directory that is not group- or world-writable; the
   builder verifies the directory mode the relay creates and adjusts the
   relay if needed.
3. `code-vm ssh-target` on a running VM compares the guest file with the
   public key (admin read) and pushes it when it differs, so a VM that was
   already running when code-vm was upgraded works without a restart.
4. `code-vm recreate` keeps the host keypair; only the guest side is rebuilt
   and re-provisioned at boot.

### Port

`sshPort` joins the VM-level config fields. Rendering: when non-zero, the
template emits

```yaml
ssh:
  localPort: <sshPort>
  loadDotSSHPubKeys: false
```

Existing instances pick the change up through the stored-config update path
that already applies template changes on the next start.

### `code-vm ssh-target`

Prints, for humans:

```
Orca / SSH target for instance code-sandbox
  Host:          127.0.0.1
  Port:          60022
  User:          devuser
  Identity file: /home/simon/.config/code-vm/ssh/code-sandbox/agent_ed25519

OpenSSH config snippet (add to ~/.ssh/config or an included file):
Host code-vm-code-sandbox
  HostName 127.0.0.1
  Port 60022
  User devuser
  IdentityFile /home/simon/.config/code-vm/ssh/code-sandbox/agent_ed25519
  IdentitiesOnly yes
  UserKnownHostsFile /home/simon/.config/code-vm/ssh/code-sandbox/known_hosts
```

It works whether or not the VM is running (the port comes from config, or
from Lima's current ssh config when unpinned, in which case it warns). It
never boots the VM.

### What Orca then does

The user adds the four values as an SSH target, adds a project on that host
at the repo's mounted path (identical to the host path), and creates
workspaces on it. Orca installs its relay into `/home/devuser/.orca-remote`
and `/home/devuser/.orca` on first connect. Claude is found on devuser's
PATH (verified in the 2026-10-19 spike for the fish login shell; the builder
confirms Orca's non-login probe sees it, since for limaadmin it did not see
`~/.local/bin`; if it does not, a symlink in a system PATH directory
installed by provisioning fixes it).

## Resource limits for SSH sessions

Orca sessions are the first agent workloads that run inside devuser's
systemd slice; `code-vm -- claude` sessions are children of limaadmin's SSH
session scope and unlimited. The task cap replaced the container sandbox's
`--pids-limit` and was never meant for a Gradle daemon, a Testcontainers run,
Orca's Node relay and Claude at once, so it goes up to 8192 in
`provision-system.sh`. The memory cap is unchanged. The asymmetry with
`sandbox-exec` sessions is documented in the README's known limitations; it
is not fixed here (moving `sandbox-exec` sessions into the slice would be
the consistent fix and is a follow-up).

## Security

- **No new agent capability.** The host user already reaches guest root
  through `limactl`; SSH as devuser is strictly less. The private key only
  protects against other users on the host, hence 0600 in a 0700 directory.
- **authorized_keys is host-owned.** The agent owns its home and can rewrite
  the file, but it gains nothing: the guest's sshd is reachable only from
  the host's loopback, and each boot restores the file.
- **The relay is agent-controllable.** Orca's relay runs as devuser, so a
  compromised agent can alter it and speak to the Orca app over the relay
  protocol. That is Orca's trust boundary, the same as for any SSH host or
  Docker sandbox it manages, and is listed in the README's known limitations
  rather than mitigated here.
- **Permission bypass flag.** Orca launches Claude with
  `--dangerously-skip-permissions` by default for remote agents, as it does
  for local ones and as the command-override path already did. The sandbox's
  containment is the egress allowlist and the VM boundary, not Claude's
  prompts; unchanged.
- **Nothing read from the workspace.** The key, port and authorized key all
  come from host config and host-generated state.
- **Locked settings are untouched by Orca.** Orca writes no Claude
  configuration on the remote and sets no config-directory override (spike,
  Orca 1.4.237). The only process that rewrites `~/.claude/settings.json` in
  the guest is Claude Code itself, which already happens in today's
  `code-vm -- claude` sessions (observed: Claude re-created the file as
  devuser-owned one minute after boot to persist the permission-bypass
  acknowledgement). That is a pre-existing property of the lock, outside
  this design; the integration suite gains an assertion that an SSH login
  plus Claude launch leaves both locked files as the lock step left them,
  so a future Orca that starts writing there is caught.

## CLI/UX

- `code-vm ssh-target` (new). No flags in v1; the builder may add `--json`.
- `sshPort` in `config.yaml`, documented next to `vmType` and `swap`.
- `code-vm doctor`: a line for detached boot availability.
- README: a section "Using the sandbox from Orca" with the four steps and
  the two caveats (re-trust after `recreate`; remote worktrees default to the
  guest home), and under Known limitations the relay trust boundary and the
  limits asymmetry between SSH and `sandbox-exec` sessions.

## Testing

- Unit: template golden file gains the `ssh.localPort` case; `ssh-target`
  output for pinned and unpinned ports; authorized_keys push arguments; the
  boot wrapper is used when the probe succeeds and absent when it fails, on
  Linux, and never on darwin.
- Integration (`test-vm-sandbox.sh`, Linux with KVM):
  - The existing `TasksMax` assertion is updated to the new value.
  - An SSH login as devuser runs inside `user-<uid>.slice` (cgroup check),
    so the limits demonstrably apply.
  - `ssh -i <key> -p <port> -o IdentitiesOnly=yes devuser@127.0.0.1 id -un`
    prints `devuser`; the file in the guest is devuser-owned, 0600, one line.
  - Boot the VM from inside `systemd-run --user --scope`, stop that scope,
    and verify `limactl list` still reports Running. This is the regression
    test for the Orca incident.
  - After an SSH login as devuser and a Claude launch from it, the two
    locked files under `/home/devuser/.claude` are byte-identical to what the
    boot's lock step produced, and no config-directory override is present in
    the Claude process environment.
  - Existing limaadmin access and `code-vm forward` keep working.

## Left to the builder

- Where in the Runner/Client layering the scope wrapper lives, as long as
  every boot path goes through it and tests can observe the argv.
- Key generation method and the exact probe for the user manager.
- Whether `ssh-target` grows `--json`.
- How the README phrases the Orca steps.

## Follow-ups (not in this design)

- On-demand boot via ProxyCommand and an ssh_config alias.
- Running `sandbox-exec` sessions inside devuser's slice so all agent work
  shares one budget.
- Claude Code rewriting the locked `settings.json` after boot (observed in
  the daily VM, independent of Orca).
- Publishing sandboxed sessions' transcripts to the host so Orca can list
  them for local workspaces (would require a dedicated host-side store
  mounted at the guest's projects directory, not the host's own
  `~/.claude/projects`).
