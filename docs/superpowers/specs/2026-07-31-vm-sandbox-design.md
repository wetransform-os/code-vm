# VM-based Agent Sandbox — Design

**Date:** 2026-07-31
**Status:** Approved design, pending implementation plan

## Context

The existing Docker-based `claude-code-sandbox` runs Claude Code in a hardened
container with four defense layers: locked permission settings, an egress
firewall (Squid allowlist + iptables default-deny), a non-root agent, and
container isolation.

Its `--enable-docker` mode emulates Docker with rootless Podman inside the
container. This causes recurring problems in three areas:

1. **Podman/Docker incompatibility** — buildx, compose features, API version
   differences, testcontainers quirks, Ryuk disabled.
2. **Networking and DNS** — host-netns default for standalone containers,
   fragile netavark/aardvark-dns bridge setup, service discovery surprises.
3. **Missing privileges** — a custom seccomp profile granting `unshare`/`mount`/
   `setns`, `systempaths=unconfined` plus a `/proc` re-hardening step to undo the
   damage, nested userns limits.

The root cause is structural: the container is *emulating* a container runtime.
A virtual machine removes that constraint — a real kernel can run a real Docker
daemon.

## Goals

- Run a real Docker daemon inside the sandbox so `docker`, `docker compose`,
  buildx, and testcontainers behave as they do on a developer machine.
- Preserve the security properties of the container sandbox: egress allowlist,
  non-root agent, locked permission settings, no host credentials exposed.
- Keep the developer UX equivalent: run one command from a project directory and
  land in a sandboxed shell or agent session at that path.
- Linux x86_64 is the required host platform; macOS support is desirable but not
  required.

## Non-goals

- Replacing the container sandbox. It remains the right tool when Docker is not
  needed — faster startup, lower resource cost.
- Multi-tenant use, or sandboxing agents from mutually untrusted parties.
- Supporting `--privileged` inner containers (see Known Limitations).

## Decisions

| Area | Decision |
|---|---|
| VM stack | Lima v2.2.0 (QEMU/KVM on Linux, `vz` on macOS) |
| Guest Docker | Rootless dockerd, running as the agent user |
| Security perimeter | VM boundary, plus in-guest privilege separation |
| Egress filtering | In-guest Squid + iptables, as in the container sandbox; runtime-switchable modes |
| VM lifecycle | One shared long-lived VM |
| Guest isolation | Single agent user; all mounted workspaces visible to it |
| Workspace sharing | virtiofs mount of a configured projects root, plus on-demand additions |
| Guest definition | VM provisioning scripts (no Dockerfile reuse) |
| Host CLI | Go + Cobra v1.10.2, single static binary |
| Project layout | Standalone project, own git history |

## Architecture

### Users

A single long-lived Lima VM named `code-sandbox`. Inside it, two users:

| User | UID | Role |
|---|---|---|
| `limaadmin` | 60000 | Lima's own guest user. Passwordless sudo. Used by the host CLI for privileged per-session setup. Never runs the agent. |
| `devuser` | host UID | The agent. No sudo. Owns rootless dockerd, the mounted workspaces, and the Claude Code session. |

`devuser`'s UID and primary GID are set to the host user's UID/GID so that
virtiofs-shared workspace files are genuinely owned by it. No UID mapping is
needed and files remain host-owned when viewed from the host.

`limaadmin` uses UID 60000 to avoid colliding with the host UID (typically
1000).

The Lima YAML is rendered by the host CLI with concrete numeric UIDs
substituted, rather than relying on Lima's `{{.UID}}` template variable, whose
meaning shifts when `user.uid` is overridden.

### Defense layers

1. **VM boundary** — a separate guest kernel. Strictly stronger than the
   container boundary it replaces. No host filesystem is visible except what is
   explicitly mounted.

2. **Mount scope** — one virtiofs mount (`writable: true`) of the configured
   projects root, mounted at the *same path* inside the guest, plus any
   on-demand additions.

   We write our own Lima template rather than basing it on the upstream
   `docker.yaml`, for two reasons. First, `docker.yaml` inherits
   `_default/mounts`, which mounts the host `$HOME` read-only — that would
   expose `~/.ssh`, `~/.aws`, and `~/.config`. Second, `docker.yaml` sets up
   rootless Docker for Lima's own guest user, which in our design is the
   privileged `limaadmin`; we need it set up for `devuser` instead. The
   template borrows `docker.yaml`'s Docker install and unit-masking steps but
   declares `mounts` explicitly, and a test asserts the host `$HOME` is not
   reachable from the guest.

   `mountType: virtiofs` is pinned. `reverse-sshfs` must not be used: it maps
   ownership to the SSH user, which breaks the UID-matching scheme above.

3. **Egress firewall** — `init-firewall.sh` ports over near-unchanged: Squid
   with a domain allowlist, and iptables with a default-deny `OUTPUT` policy
   plus `-m owner --uid-owner devuser` REJECT rules for traffic not going
   through the proxy. Installed and started by root; `devuser` has no sudo, so
   it remains a real boundary.

   Rootless dockerd runs *as* `devuser`, so image pulls exit under that UID and
   must traverse Squid. Proxy environment is configured on the rootless dockerd
   systemd **user** unit.

   The guest does not define `host.docker.internal`, and iptables rejects
   `devuser` traffic to the Lima host gateway. The agent has no reason to reach
   host services, and Squid runs inside the guest.

4. **Permission settings** — `lock-settings.sh` ports as-is: canonical config
   copied to `/home/devuser/.claude/`, made root-owned and read-only, with the
   user's `enabledPlugins` preserved across the overwrite.

### What the VM removes

Relative to the container sandbox's `--enable-docker` mode, the following are
deleted outright rather than ported:

- The custom seccomp profile (`config/dind-seccomp.json`).
- `--security-opt systempaths=unconfined` and the `sandbox::reharden_proc_paths`
  step that compensates for it.
- Rootless Podman, netavark, aardvark-dns, and the iptables-vs-nftables
  firewall-driver pinning.
- `TESTCONTAINERS_RYUK_DISABLED` — Ryuk works with real Docker and can be
  re-enabled.
- The split between `config/` and `config-dind/` settings trees: there is one
  guest, and it always has Docker.

Bridge networks and compose service discovery work natively through dockerd's
embedded DNS at `127.0.0.11` inside the RootlessKit network namespace.

### Firewall modes

The egress policy is switchable at runtime, because the two things that
legitimately make an allowlist painful have different answers:

| Mode | Squid | iptables | Audit log | Fixes |
|---|---|---|---|---|
| `allowlist` (default) | domain allowlist | default-deny, proxy mandatory | yes | — |
| `audit` | allow all | default-deny, proxy mandatory | yes | a domain that is not allowlisted |
| `open` | allow all | agent TCP permitted directly | no | tooling that ignores `http_proxy` |

`init-firewall.sh` already regenerates `squid.conf` and every iptables rule from
scratch on each run, so a mode switch is just reapplying it with a different
parameter.

Constraints that hold in every mode: the agent cannot reach host services (the
host-gateway REJECT survives), DNS tunneling to external resolvers stays
blocked, and the agent cannot change the mode itself — it has no sudo, and the
switch runs through `limaadmin`.

Two deliberate restrictions on the loosening:

- **The mode is runtime-only.** It lives in `/run/sandbox/firewall-mode`, which
  is tmpfs, so a VM restart always reverts to `allowlist`. There is no config
  key: a loosened firewall must not be able to become the durable default.
- **`open` requires explicit confirmation**, because it gives up the audit trail
  as well as the filtering.

The risk that makes this worth stating rather than assuming: the VM is shared by
every mounted workspace under a single agent user, so a loosened firewall applies
to all of them simultaneously, including credentials injected for other
projects. The shipped permission profile allows `python *`, which makes the
firewall the primary defense against exfiltration — so with it open, the
realistic threat is not the developer but prompt injection from content the agent
reads becoming an exfiltration channel.

### Inner-container egress

In the container sandbox, bridged inner containers had no external egress at
all. In the VM, Squid is *reachable* from containers, but the proxy environment
is not injected by default.

Squid binds port 3128 on all guest interfaces, so a container can reach it at
the guest's own address. Rootless Docker NATs container traffic out as
`devuser`, so an explicit iptables rule allows `devuser` to reach
`<guest-ip>:3128`; everything else still hits the REJECT rule, leaving the
allowlist as the only path out.

Injecting the proxy into containers is opt-in, via the `containerProxy` config
key (default `false`). When enabled, `devuser`'s
`~/.config/docker/config.json` sets `proxies.default` to
`http://<guest-ip>:3128`. It is off by default because Docker applies
`proxies.default` to `docker run` *and* `docker build`: a bare compose service
name such as `db` matches no `noProxy` entry, so service-to-service HTTP would
be routed at Squid and fail — breaking precisely the compose service discovery
this design exists to fix. Enable it per project when image builds need to
fetch packages.

## Guest environment

### Boot sequence

`sandbox-boot.service` is the VM's equivalent of the container's
`entrypoint.sh`: a single root `oneshot` unit running the same ordered sequence.

1. Run `lock-settings.sh`.
2. Run `init-firewall.sh`.
3. Verify the agent can reach the API through the proxy (warn only).
4. Update the agent CLIs (Claude Code, OpenCode) — through the proxy.
5. Start `devuser`'s user services (rootless dockerd).

**Revised during implementation.** The design originally put the CLI update
first, mirroring the container's `entrypoint.sh`, on the premise that the
installers need unrestricted egress. They do not: every host they use is already
allowlisted, and both were observed installing cleanly through Squid. Keeping
that premise had a cost — for as long as it held, one step per boot ran with no
egress filtering and no audit trail, and a code review found the agent could
reach into it through its own `~/.profile` or a planted `~/.local/bin/curl`
(those are fenced off separately, and still are).

With the update last, no step in the VM's life has unfiltered egress: a
compromised installer is confined to the allowlist and recorded in the proxy log
like any other traffic. The trade is that a vendor moving a download host becomes
a failed update rather than an invisible one. That failure is a warning, not
fatal, `code-vm proxy-log denied` names the blocked host, and `code-vm allow`
fixes it — and the test suite asserts both CLIs are runnable, so the degradation
cannot ship unnoticed.

The remaining order is load-bearing: the settings lock must precede any agent
process, and the connectivity check sits between the firewall and the update so
that a failed install is preceded by a warning saying whether egress works at
all.

### First-boot provisioning

Lima `provision: mode: system` (as root):

- Create `limaadmin` (UID 60000) and `devuser` (host UID/GID). Grant `devuser`
  no sudo. Allocate subuid/subgid ranges for `devuser` and run
  `loginctl enable-linger devuser`.
- Install packages: `uidmap`, `dbus-user-session`, `iptables`, `squid`,
  `util-linux` (for `setpriv`), `git`, `jq`, `curl`. Install `mise` via its
  official installer, and `yq` and `gomplate` through `mise`.
- Install Docker via `get.docker.com`, then mask the rootful `docker.service`,
  `docker.socket`, `containerd.service`, and `containerd.socket` units, as
  Lima's own `docker.yaml` template does.
- Write `/etc/environment`: `http_proxy`/`https_proxy`/`no_proxy`,
  `JAVA_TOOL_OPTIONS` proxy system properties, and `DOCKER_HOST` pointing at
  `unix:///run/user/<host-uid>/docker.sock`. No testcontainers-specific
  workarounds are set: with real Docker, Ryuk works and the defaults are
  correct.
- Install the systemd drop-in on `user-<uid>.slice` setting `TasksMax` and
  `MemoryMax`.
- Install `sandbox-boot.service` and the firewall unit.

Lima `provision: mode: user` runs as Lima's own guest user (`limaadmin`), so the
`devuser` steps below are invoked from a system-stage script via
`setpriv`/`machinectl shell` under `devuser`'s own systemd user session, which
`enable-linger` has already started:

- `dockerd-rootless-setuptool.sh install`; `docker context use rootless`.
- Configure proxy environment on the rootless dockerd user unit.
- Install Claude Code and OpenCode.

### Resource limits

VM-level `cpus`, `memory`, and `disk` come from host config. Defaults: 4 CPU,
12 GB memory, 100 GB disk — Docker image layers need the disk.

Fork-bomb protection moves from `--pids-limit` to a systemd drop-in on
`user-<uid>.slice` with `TasksMax` and `MemoryMax`.

### Persistence

The guest disk replaces the `claude-state-home` Docker volume. `/home/devuser`
persists across VM restarts, so Claude authentication, installed plugins, and
the Docker image cache survive. `code-vm recreate` is the "start clean" action.

## Host CLI

A Go binary named `code-vm`, built with Cobra v1.10.2. Go is pinned via `mise`
at implementation time.

Rationale: a static binary has no host runtime dependency beyond `limactl`;
`go:embed` lets the binary carry the Lima template *and* the guest provisioning
scripts, so the CLI and guest side can never skew; config becomes typed data
with validated errors rather than `yq` subshells; help text and shell completion
are generated.

### Commands

```
code-vm                          # interactive shell in the VM, cwd = this project
code-vm -- claude -p "..."       # run a command in the sandbox
code-vm mount ~/some/other/repo  # add an out-of-tree mount (restarts the VM)
code-vm status
code-vm stop
code-vm recreate
code-vm proxy-log [all|denied|allowed|follow]
code-vm doctor                   # prereq checks
```

`code-vm doctor` checks KVM access, `limactl` and `virtiofsd` presence and
versions, and projects-root configuration. The failure modes here are
host-environment-shaped, and a clear diagnostic is worth more than the code
costs.

### Invocation flow

Every default invocation performs four steps:

1. Ensure the VM is running (`limactl start` if not; first boot provisions,
   later boots are a few seconds).
2. Verify `$PWD` is under a declared mount. If not, report that and suggest
   `code-vm mount`.
3. Privileged session setup as `limaadmin`: refresh the Squid allowlist fragment
   from the host config's `extraDomains`, reload Squid if it changed, seed git
   identity from the host's `git config user.name/user.email`. Nothing in this
   step reads anything out of the workspace.
4. Exec the command as `devuser` at the same path:
   `limactl shell --workdir "$PWD" code-sandbox sudo /usr/local/bin/sandbox-exec …`

`sandbox-exec` sources `/etc/environment` (proxy vars, `DOCKER_HOST`) and then
drops privileges to `devuser` with the working directory preserved — the same
role the container's `sandbox-exec` plays, with `setpriv` in place of `gosu`.

The Docker socket is deliberately **not** port-forwarded to the host. The agent
runs in the guest and does not need it exposed.

### Host config

`~/.config/code-vm/config.yaml`:

```yaml
projectsRoot: ~/projects
extraMounts:
  - ~/work/other-repo
cpus: 4
memory: 12GiB
disk: 100GiB
extraDomains:
  - registry.mycompany.com
```

The CLI renders the Lima template from this config, so the VM shape is
reproducible and the config file is the entire knob surface. Adding a mount
requires a VM restart, because Lima mounts are declared in the instance config;
`code-vm mount` regenerates the config and restarts, reporting that it is doing
so.

### Squid allowlist fragments

**Revised during implementation.** The design originally compiled per-workspace
`.sandbox-domains` files into fragments. That was dropped: the workspace is
mounted writable and is exactly what the agent edits, so a domain file there let
the agent widen its own egress — the exfiltration channel the firewall exists to
prevent. A code review confirmed it as a live issue rather than a theoretical
one.

The host config's `extraDomains` is now the only source. It lives outside every
mount, so the agent cannot reach it, and `code-vm` refuses to run when a mount
would expose it. Entries are validated against a domain pattern before they can
reach `squid.conf`.

Extra domains are rendered into a single fragment,
`/run/sandbox/squid-allow.d/10-host-config.conf`, included by the main Squid
config via a wildcard `include`; `00-base.conf` is always written at boot so the
wildcard never matches an empty set. `init-firewall.sh` writes the fragment at
boot from `provision.env`, and session setup (plus `code-vm allow`) rewrites it
whenever the host config differs, reloading with `squid -k reconfigure` — a few
milliseconds, without dropping connections.

Writing the extra domains to a fragment rather than into the generated
`squid.conf` is deliberate: `init-firewall.sh` regenerates that file from scratch
on every firewall mode switch, so ACL lines edited into it directly would vanish
the first time someone ran `code-vm firewall audit`. The fragment survives
regeneration because the `include` is preserved and only `00-base.conf` is
overwritten. (The container sandbox's `code-sandbox-allow` does edit `squid.conf`
in place; it has no mode switching, so the problem never arises there.)

The directory is tmpfs-backed, so the guest cannot drift from the host config:
after a reboot it holds only what `code-vm` puts back.

### Extending the allowlist at runtime

`code-vm allow [domain ...]` appends to `extraDomains` and applies the change
live. With no arguments it parses the denied entries out of the Squid access log
and offers each one, mirroring `code-sandbox-allow`'s log-driven mode. Because
the log drives this, `init-firewall.sh` creates `access.log` only when it is
absent — truncating on every run would have erased the evidence a user reaches
for `firewall audit` to inspect.

### Credential injection — removed

**Removed during implementation.** The design ported the container sandbox's
flow: `.sandbox-secrets.yaml` in the workspace declares secrets, each resolved by
running its `source:` command on the host where `gopass`/`sops` live, with the
payload copied into guest tmpfs and rendered by `render-credentials.sh`.

Two problems, found while reviewing the allowlist rework:

1. **Host command execution from an agent-authored file.** The workspace is
   mounted writable and is what the agent edits. An agent that writes
   `.sandbox-secrets.yaml` gets arbitrary command execution *on the host* the
   next time `code-vm` runs in that directory. Every other control in this design
   fails inside the VM; this one fails outside it, which makes it categorically
   worse than anything the sandbox is meant to contain.
2. **The protection story did not hold.** Rendered files were `root:devuser
   0444` — group-readable by the agent — so the filesystem never stopped it. The
   only barrier was a set of generated Claude Code deny patterns, which match
   command strings where the path appears as a separate token; the shipped
   profile allows `Bash(python *)`, so `python -c "open('<path>').read()"` is not
   covered. The documented property ("the agent cannot read them") was false.

Removing it also closed the deny-rule lifetime defect the review raised
separately: the rules lived in tmpfs while the rendered files persisted on the
guest disk, so a restart re-locked `settings.json` without them.

Credentials are now a manual, one-time step into the persistent guest home. The
VM makes that viable in a way the container did not: the container was recreated
per run, so injection had to be automated; `/home/devuser` here survives
restarts.

**What a replacement must do differently.** Declare secrets in the *host config*,
never in the workspace — the same trust-boundary lesson as the allowlist. Be
honest about readability rather than asserting a protection that pattern-matching
cannot deliver: either state that injected credentials are readable by the agent
(so they should be sandbox-scoped and cheap to revoke), or keep the secret out of
the guest entirely. The most promising form of the latter is to let the proxy
authenticate on the agent's behalf, since Squid already sits in the path for
every request — the credential would live in root-owned proxy config and the
agent would never hold it. That works for plain HTTP and for an authenticating
upstream peer, but not for origin auth inside a CONNECT tunnel without TLS
interception, so it needs its own design pass. A narrower `code-vm secret`
command — config-declared, run explicitly rather than implicitly on every
invocation — is the pragmatic version.

## Project layout

A standalone project with its own git history, developed in a `vm-sandbox/`
subfolder of the container sandbox repository and gitignored there until
extraction.

```
vm-sandbox/
  cmd/code-vm/main.go
  internal/cli/         # one file per subcommand + root
  internal/config/      # host config: load, defaults, validate
  internal/lima/        # limactl wrapper + template rendering
  internal/session/     # per-invocation setup
  internal/guest/
    embed.go            # go:embed of files/
    files/
      lima/code-sandbox.yaml.tpl
      scripts/          # provision-system, provision-user-docker,
                        # sandbox-boot, init-firewall, lock-settings,
                        # update-agent-clis, set-firewall-mode,
                        # proxy-log, sandbox-exec
      systemd/sandbox-boot.service
      config/.claude/settings.json
  test-vm-sandbox.sh
  docs/
```

The guest assets live *under* `internal/guest/` rather than at the project root
because `go:embed` cannot reference paths outside its own package directory.

Guest assets are delivered as Lima `mode: data` provision entries with
`overwrite: true`, which Lima writes before every boot's `mode: system` scripts.
A `code-vm` upgrade therefore refreshes the guest side on the next start, and
the binary and the guest scripts cannot fall out of sync.

Adding a subcommand is one file in `internal/cli` plus one register call.
Adding guest capability is a script in `guest/` plus a step in `internal/session`
or provisioning.

The shared bash scripts are **copied** from the container sandbox rather than
shared across repositories. The two projects will diverge, and that divergence
should be explicit rather than mediated by a cross-repo abstraction.

## Testing

`test-vm-sandbox.sh` ports the existing suite, driven from the host through
`code-vm`.

Ported assertions:

- Settings lock enforcement.
- iptables rules present and correct.
- Privilege escalation: `devuser` has no sudo, cannot write `/etc`, cannot
  modify `~/.claude`.
- Sensitive path exposure.
- Firewall bypass attempts (direct `curl`, Python `requests`, DNS exfiltration).
- No workspace file is trusted: a planted `.sandbox-secrets.yaml` must neither
  run anything on the host nor render anything in the guest.

New VM-specific assertions:

- Host `$HOME` is not visible in the guest; `~/.ssh` is unreachable.
- Only the configured projects root and declared extra mounts are present.
- virtiofs files are owned by `devuser`.
- Rootful Docker units are masked.
- `docker build` works.
- `docker compose` service-name DNS resolution works.
- A testcontainers smoke test passes.
- `docker run --privileged` fails, as documented.
- `/run/sandbox/squid-allow.d/` is empty after reboot except `00-base.conf`.
- `TasksMax` is enforced.
- `devuser` cannot read `limaadmin`'s files or invoke sudo.

### CI

The full suite needs nested KVM, which GitHub-hosted runners do not reliably
provide. The split:

- **Always in CI:** Go unit tests, Lima template rendering tests, `shellcheck`
  over the guest scripts.
- **`mise run test:vm`:** the full VM suite, run locally and on a KVM-capable
  runner where one is available.

Every CI step invokes a `mise` task, so local and CI entry points are identical.
This split should be revisited once a KVM-capable runner is available.

## Known limitations

These are consequences of the decisions above, not oversights:

- **`--privileged` grants nothing outside the user namespace.** Corrected during
  implementation: rootless dockerd *accepts* the flag rather than refusing it,
  but the capabilities are confined to the daemon's user namespace — a privileged
  container still cannot write host kernel state or reach guest root. Workloads
  needing real privileges (arbitrary `sysctl`s, host networking) do not work.
  This is the cost of rootless dockerd, accepted in exchange for genuine
  separation between the agent and guest root.
- **No cross-project isolation.** A single agent user with all workspaces
  mounted means project A's agent can read project B's tree and B's injected
  credentials.
- **One allowlist for every workspace.** One agent user and one Squid, so a
  domain allowed for one project is allowed for all of them. Projects cannot
  declare their own domains (see *Squid allowlist fragments*).
- **Mounts require a VM restart.** Lima declares mounts in the instance config.
- **Guest root is reachable from the host** by anyone who can run `limactl` —
  that is, the developer, never the agent.
- **A loosened firewall is VM-wide.** `audit` and `open` apply to every mounted
  workspace at once, because there is one agent user. Mitigated by reverting on
  restart, not by scoping.

## Implementation sequencing

Rough order; the implementation plan will refine it.

1. Lima template plus guest provisioning scripts; VM boots with rootless Docker
   and a non-sudo `devuser`.
2. Port `init-firewall.sh` and `lock-settings.sh`; `sandbox-boot.service`.
3. Go CLI skeleton: config, `limactl` wrapper, default exec path, `doctor`.
4. Session setup: allowlist fragments, git identity.
5. Credential injection.
6. Remaining subcommands: `mount`, `status`, `stop`, `recreate`, `proxy-log`.
7. Test suite and `mise` tasks.
8. Runtime firewall modes (`code-vm firewall`).
