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
| Egress filtering | In-guest Squid + iptables, as in the container sandbox |
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

1. Update the agent CLIs (Claude Code, OpenCode).
2. Run `lock-settings.sh`.
3. Run `init-firewall.sh`.
4. Start `devuser`'s user services (rootless dockerd).

The ordering is load-bearing: CLI updates need unrestricted egress, so the
firewall closes after them — the same reason the container's entrypoint uses
this order.

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
3. Privileged session setup as `limaadmin`: refresh this workspace's Squid
   allowlist fragment from its `.sandbox-domains`, reload Squid if it changed,
   render `.sandbox-secrets.yaml` credentials, seed git identity from the host's
   `git config user.name/user.email`.
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

Per-workspace `.sandbox-domains` files are compiled into fragments under
`/run/sandbox/squid-allow.d/`, included by the main Squid config via a wildcard
`include`. A `00-base.conf` fragment is always written at boot so the wildcard
never matches an empty set. Squid is reloaded with `squid -k reconfigure` by
`limaadmin` during session setup when a fragment changes.

The directory is tmpfs-backed, so it is empty on every boot. Stale entries from
projects no longer in use cannot widen the allowlist beyond one VM lifetime.

### Credential injection

Host-side resolution is unchanged: `gopass`, `sops`, and similar commands run on
the host where they are configured. The resolved payload is copied into guest
tmpfs via `limactl copy`; `render-credentials.sh` renders the templates as root,
locks the rendered files `root:devuser 0444`, and wipes the payload. Same flow
as the container sandbox, different transport.

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
                        # render-credentials, proxy-log, sandbox-exec
      systemd/sandbox-boot.service
      config/.claude/settings.json
      sandbox-templates/
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
- Credential injection and its generated deny rules.

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

- **No `--privileged` inner containers**, no arbitrary `sysctl`s in containers,
  no host networking. This is the cost of rootless dockerd, accepted in exchange
  for genuine separation between the agent and guest root.
- **No cross-project isolation.** A single agent user with all workspaces
  mounted means project A's agent can read project B's tree and B's injected
  credentials.
- **Union allowlist.** The Squid allowlist is the union of allowlists of all
  projects used during the VM's current lifetime.
- **Mounts require a VM restart.** Lima declares mounts in the instance config.
- **Guest root is reachable from the host** by anyone who can run `limactl` —
  that is, the developer, never the agent.

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
