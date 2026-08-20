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

- [Lima](https://lima-vm.io) 2.2.0 or newer
- [mise](https://mise.jdx.dev) for the build toolchain
- Virtualization:
  * Linux: x86_64 with KVM (`/dev/kvm` readable and writable by your user) and `virtiofsd`
  * macOS: HVF on macOS 13.5 or newer
  * Windows: Unsupported


Run `code-vm doctor` to check the prerequisites.


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
| `code-vm allow [domain...]` | Add domains to the allowlist and apply them live |
| `code-vm secrets` | List secrets/vars the active profiles declare, mapped or not |
| `code-vm doctor` | Check host prerequisites |

## Configuration

`~/.config/code-vm/config.yaml`:

```yaml
instance: code-sandbox        # the Lima instance this config drives
projectsRoot: ~/projects      # the one directory always shared
extraMounts:                  # added by `code-vm mount`
  - ~/work/other-repo
vmType:                       # hypervisor; empty picks the host's — see below
cpus: 4
memory: 12GiB
disk: 100GiB
extraDomains:                 # added to the Squid allowlist
  - registry.mycompany.com
containerProxy: false         # see below
```

Nothing is read from the project directory. `code-vm` deliberately trusts no
file inside a workspace: the workspace is mounted writable and is exactly what
the agent edits, so anything there is agent-authored input. The host config is
the whole knob surface.

### `vmType`

The Lima hypervisor driver. Leave it unset — the default — and `code-vm` picks
the accelerated one for the host:

| Host | `vmType` | Accelerated by | virtiofs from |
|---|---|---|---|
| Linux | `qemu` | KVM | the `virtiofsd` package |
| macOS 13.5+ | `vz` | Hypervisor.framework (HVF) | Virtualization.framework |

### No workspace credentials

Nothing agent- or workspace-authored is ever a credential source. The
previous `.sandbox-secrets.yaml` mechanism was removed rather than fixed: it
resolved each secret by running its `source:` command **on the host** — from
a file inside the workspace, which the agent can write. That is host command
execution reachable from inside the sandbox, which defeats the boundary the
whole design exists to draw. Its stated protection did not hold either:
rendered files were group-readable by the agent, and the generated deny
rules only matched commands where the path appeared as a separate argument,
which `python -c` (an allowed command) sidesteps. That class of mechanism
stays removed, and the integration suite pins it.

Credentials enter the sandbox in one of two ways:

- Written directly into the guest home, once — it persists across restarts,
  because the guest disk is the sandbox's durable state:

  ```bash
  code-vm                                     # shell into the guest
  $ install -d -m 0700 ~/.gradle
  $ cat > ~/.gradle/gradle.properties          # paste, or pipe it in
  ```

  Assume the agent can read anything you put there, and use credentials
  created for the sandbox rather than your personal ones, so revoking them
  is cheap.

- Through a profile's `templates:` (see [Credentials](#credentials) under
  Profiles) — the host-trusted mechanism for sharing a credentialed config's
  shape while keeping the credential itself in the user's own
  `secrets.yaml`.

### Extending the allowlist

```bash
code-vm allow                        # offer everything Squid recently denied
code-vm allow registry.example.com   # add specific domains
code-vm allow --yes ghcr.example     # no confirmation prompt
```

`code-vm allow` writes accepted domains to `extraDomains` in the host config and
pushes them to Squid immediately — no VM restart, `squid -k reconfigure` takes a
few milliseconds and does not drop connections. With no arguments it reads the
denied entries from the proxy log, which is the quickest way to find what a
build actually needs.

The host config is the **only** source for the allowlist, deliberately. It lives
outside every mount, so the agent cannot reach it; `code-vm` refuses to start if
a mount would expose it. There is no per-project domain file: the agent can
write anything inside the workspace, so a domain file there would let it widen
its own egress — the exfiltration channel the firewall exists to prevent.
Domains a project needs belong in its README, or in each developer's config.

Removing a domain from the config revokes it on the next invocation; the guest
fragment is rewritten to match, rather than keeping stale entries alive for the
VM's lifetime.

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

## Profiles

Profiles are shareable customization bundles for the VM: ship a `CLAUDE.md`,
install tools, set the agent's shell, run setup as the agent user. They live
in `~/.config/code-vm/profiles/<name>/` and are activated in `config.yaml`:

```yaml
profiles:
  - fish-shell
```

Get one from git, or author it locally:

```
code-vm profile add https://github.com/your-org/fish-shell-profile fish-shell
code-vm profile list
code-vm profile update
code-vm profile apply     # push changes into the running VM; boots apply automatically
code-vm profile remove fish-shell
```

A bundle is a directory:

```
fish-shell/
  profile.yaml      # manifest
  files/            # copied into the agent's home (agent-owned, writable)
    .config/fish/config.fish
  hook.sh           # optional; runs as the agent user, through the proxy
```

`profile.yaml`:

```yaml
description: Fish as the agent's shell, with fisher
packages: [fish]            # apt packages, installed as root
shell: /usr/bin/fish        # agent's login shell
domains:                    # merged into the egress allowlist
  - raw.githubusercontent.com
hook: hook.sh               # runs after files and packages, as the agent
```

Notes:

- Installing a profile means trusting its author with the agent's home, apt
  package selection, egress domains, and code execution as the agent user.
  Hooks never run as root, and a profile cannot touch the locked
  `.claude/settings.json`.
- List order matters: later profiles win file collisions; the last declared
  shell wins.
- Profile-shipped files are canonical — re-installed on every boot and every
  apply, so local edits to them do not survive a restart.
- Hooks run on every boot and every apply: write them idempotent.
- Deactivating a profile stops applying it, but does not uninstall packages,
  revert the shell, or delete files already in the home. `code-vm recreate`
  is the clean-slate path.

### Credentials

Profiles can also ship **templates**: files rendered from placeholders
before delivery, so a team can share the whole shape of a credentialed
config (proxies, mirrors, server IDs) while each user supplies only their
own credential sources.

```
wetf-maven/
  profile.yaml
  templates/                 # home-mirroring tree, rendered before delivery
    .m2/settings.xml
```

`profile.yaml` gains two more sections:

```yaml
secrets:
  wetf-repo-user:
    description: Artifactory user for wetf-snapshots/releases
    suggest: gopass show -o wetf/artifactory-user   # inert hint, never executed
  wetf-repo-password:
    suggest: gopass show -o wetf/artifactory-password
vars:
  artifactory-url:
    description: Base URL of the Artifactory instance
```

A template uses `${secret:name}` and `${var:name}` placeholders; anything
else — Maven properties, `${env.FOO}` — passes through untouched. A shipped
`.m2/settings.xml` typically writes the static parts (proxies, mirrors)
verbatim and reserves placeholders only for the credentialed bits:

```xml
<settings>
  <servers>
    <server>
      <id>wetf-snapshots</id>
      <username>${secret:wetf-repo-user}</username>
      <password>${secret:wetf-repo-password}</password>
    </server>
  </servers>
  <proxies>...</proxies>   <!-- shipped as-is, no placeholders needed -->
  <mirrors>...</mirrors>
</settings>
```

`description` and `suggest` are inert display strings: a profile can never
make anything execute. Values come only from the user's own mapping:

- **`~/.config/code-vm/secrets.yaml`** — 0600, host-trusted like
  `config.yaml`, never distributed with the profile:

  ```yaml
  secrets:
    wetf-repo-user:
      command: gopass show -o wetf/artifactory-user
    wetf-repo-password:
      command: gopass show -o wetf/artifactory-password
  ```

  `command` runs on the host through the shell; its stdout, with one
  trailing newline stripped, is the value. `value:` is also accepted for a
  literal — a footgun for a real credential, fine for a low-value token.

- **`vars:` in `config.yaml`** — the same non-secret literal map as any
  other config key, for things like `artifactory-url: https://...`.

Notes:

- Mapping a secret makes its value readable by the agent, and therefore by
  every active profile's hook, not just the one that declared it. Install
  profiles from sources you trust with the secrets you map, and map only
  sandbox-appropriate credentials.
- A declared-but-unmapped secret or var fails `code-vm start` /
  `profile apply` with the exact snippet to paste into `secrets.yaml` or
  `config.yaml`; nothing partial reaches the guest. `code-vm secrets` lists
  every secret and var the active profiles declare, mapped or not, without
  ever printing a value.
- Resolution happens at `code-vm start`, `code-vm profile apply`, and any
  invocation that has to boot the VM — never per invocation against an
  already-running VM, so there is no per-command secret-manager prompt.
- On a cold boot, hooks run as part of the guest's own boot sequence,
  before `code-vm start` pushes the first rendered template: a hook that
  reads a profile-shipped template must tolerate it not existing yet.
- Rotation: rotate the credential at its source (gopass, pass, op, …), then
  either restart the VM or run `code-vm profile apply` — code-vm
  re-resolves and re-pushes every time, never caching an old value.

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
- **One allowlist for every workspace.** There is a single agent user and a
  single Squid, so a domain allowed for one project is allowed for all of them.
- **Mounts need a VM restart**, because Lima declares them in the instance
  config.
- **Guest root is reachable from the host** by anyone who can run `limactl` —
  you, never the agent.

## Testing

```bash
mise run test:unit   # Go tests: config, template rendering, argv construction
mise run lint        # golangci-lint + shellcheck
mise run test:vm     # full VM suite; requires KVM on Linux, HVF on macOS
```

`mise run test:vm` builds its own throwaway VM (`code-sandbox-test`, minimal
resources) from a scratch config and deletes it afterwards, so it never touches
the instance you work in — it asserts that, comparing the default instance's
state and machine-id before and after. `CODE_VM_KEEP=1` leaves the test VM up for
debugging a failure.

Because `instance` is a config key, the same mechanism runs two VMs deliberately:
point `--config` at another file naming another instance.

CI runs `fmt-check`, `lint`, `test:unit` and `build` on every push. The VM suite
needs nested KVM and a Lima install, so it lives in a separate `vm-suite` job
that is **triggered manually** from the Actions tab (`workflow_dispatch`) while we
confirm the stack comes up on a GitHub runner at all. Run it locally with
`mise run test:vm` — and do, before anything that touches a security control,
because CI showing green does not yet mean the suite passed.

The VM suite asserts the primitives testcontainers depends on — API socket,
socket bind-mounting, Ryuk, published ports — rather than driving a JVM
testcontainers run, which belongs with the projects that use it.
