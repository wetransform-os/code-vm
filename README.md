# code-vm

Run Claude Code in a hardened VM with real Docker.

`code-vm` is the VM-based sibling of the container sandbox. The container
version emulates a container runtime with rootless Podman, which breaks
Docker/Podman compatibility, bridge DNS, and anything needing privileges.
A VM has its own kernel, so it runs a real Docker daemon instead.

## What you get

- **Real Docker.** Rootless `dockerd`, the real Docker CLI and API. `docker
  compose` service discovery, buildx, and testcontainers behave as they do on
  a developer machine.
- **Egress allowlist.** Squid domain allowlist plus iptables default-deny,
  enforced inside the guest where the agent has no sudo.
- **Non-root agent.** The agent runs as `devuser`, whose UID and GID mirror
  yours so workspace files stay host-owned. `limaadmin` holds sudo and is used
  only by `code-vm` for privileged setup.
- **Locked permissions.** `~/.claude/settings.json` is root-owned and
  read-only in the guest; `settings.local.json` is pre-claimed.
- **No host credentials.** Only the directories you configure are shared. Host
  `$HOME`, `~/.ssh` and `~/.aws` are not visible in the guest.

## Prerequisites

- Linux x86_64 with KVM (`/dev/kvm` readable and writable by your user)
- [Lima](https://lima-vm.io) 2.2.0 or newer, and `virtiofsd`
- [mise](https://mise.jdx.dev) for the build toolchain

Run `code-vm doctor` to check all of the above.

macOS is expected to work (Lima supports `vz`) but is not tested.

## Quick start

```bash
mise run build
sudo install -m 0755 dist/code-vm /usr/local/bin/code-vm

# Configure which directories are shared
mkdir -p ~/.config/code-vm
cat > ~/.config/code-vm/config.yaml <<'YAML'
projectsRoot: ~/projects
cpus: 4
memory: 12GiB
disk: 100GiB
YAML

code-vm doctor
code-vm start        # first boot provisions the VM; expect several minutes

cd ~/projects/my-repo
code-vm -- claude login          # once; persists on the guest disk
code-vm -- claude -p "fix the failing test" --max-turns 20
code-vm                          # interactive shell
```

## Commands

| Command | Purpose |
|---|---|
| `code-vm` | Interactive shell in the guest, at the current directory |
| `code-vm -- <cmd>` | Run a command as the agent, at the current directory |
| `code-vm start` / `stop` | Bring the VM up (idempotent) or shut it down |
| `code-vm status` | Instance state, shared paths, firewall verification |
| `code-vm mount <dir>` | Share another host directory (restarts the VM) |
| `code-vm recreate` | Delete and rebuild the guest from scratch |
| `code-vm proxy-log [all\|denied\|allowed\|follow]` | Read the Squid access log |
| `code-vm doctor` | Check host prerequisites |

## Configuration

`~/.config/code-vm/config.yaml`:

```yaml
projectsRoot: ~/projects      # the one directory always shared
extraMounts:                  # added by `code-vm mount`
  - ~/work/other-repo
cpus: 4
memory: 12GiB
disk: 100GiB
extraDomains:                 # added to the Squid allowlist
  - registry.mycompany.com
containerProxy: false         # see below
```

Per-project extras live in the project directory:

- `.sandbox-domains` — extra allowed domains for that project. Compiled into a
  Squid fragment in tmpfs, so it is forgotten when the VM restarts.
- `.sandbox-secrets.yaml` — credentials to inject. Secrets resolve on the
  **host** (where `gopass`/`sops` live), render in the guest, and the payload is
  wiped. Rendered files are `root:devuser 0444` and deny rules are merged into
  `settings.json` so the agent cannot read them.

### `containerProxy`

Off by default. When on, `docker run` and `docker build` containers get
`http_proxy` pointed at the guest's Squid. That is useful when image builds need
to fetch packages, but it also injects the proxy into `docker compose` service
containers — where a bare service name like `db` matches no `noProxy` entry and
would be routed to Squid, breaking service-to-service traffic. Enable it per
project only when you need it.

### Firewall modes

```bash
code-vm firewall              # show the current mode
code-vm firewall audit        # allow all domains, keep the proxy and the log
code-vm firewall open --yes   # unfiltered, unlogged agent egress
code-vm firewall allowlist    # back to the default
```

The mode is runtime-only and lives in tmpfs, so **restarting the VM always
reverts to `allowlist`**. There is deliberately no config key: a loosened
firewall must not become the durable default.

Reach for `audit` first — it solves "the domain I need isn't allowlisted" while
keeping the access log. `open` exists for tooling that ignores `http_proxy`, and
gives up the audit trail as well as the filtering.

Two things to keep in mind before loosening it. This VM is shared by every
workspace you have mounted, under a single agent user, so a loosened firewall
applies to all of them at once — including credentials injected for other
projects. And the shipped permission profile allows `python *`, which means the
firewall is the primary defense against exfiltration; with it open, the
realistic risk is not you but prompt injection from content the agent reads
turning into an exfiltration channel.

In every mode the agent still cannot reach host services, and DNS tunneling to
external resolvers stays blocked.

## Security model

The perimeter is the VM boundary. Inside it, the agent is separated from guest
root: `devuser` has no sudo and the rootful Docker daemon is masked, so the
agent's own rootless `dockerd` cannot be used to become guest root.

### Known limitations

These are consequences of the design, not oversights:

- **`--privileged` grants nothing outside the user namespace.** Rootless
  dockerd accepts the flag, but the capabilities it grants are confined to the
  daemon's user namespace: a privileged container still cannot write host
  kernel state, load modules, or reach guest root. Workloads that need *real*
  privileges (arbitrary `sysctl`s, host networking) do not work — that is the
  cost of rootless Docker, accepted in exchange for real separation between
  the agent and guest root.
- **No cross-project isolation.** One agent user with all workspaces mounted
  means one project's agent can read another's tree and injected credentials.
- **Union allowlist.** The allowlist is the union of every project used during
  the VM's current lifetime.
- **Mounts need a VM restart**, because Lima declares them in the instance
  config.
- **Guest root is reachable from the host** by anyone who can run `limactl` —
  you, never the agent.

## Testing

```bash
mise run test:unit   # Go tests: config, template rendering, argv construction
mise run lint        # golangci-lint + shellcheck
mise run test:vm     # full VM suite; requires KVM
```

CI runs everything except `test:vm`, which needs nested KVM that GitHub-hosted
runners do not reliably provide. Run it locally, or on a KVM-capable runner.

The VM suite asserts the primitives testcontainers depends on — API socket,
socket bind-mounting, Ryuk, published ports — rather than driving a JVM
testcontainers run, which belongs with the projects that use it.
