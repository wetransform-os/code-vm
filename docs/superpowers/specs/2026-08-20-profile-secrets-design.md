# Profile Secrets and Templates — Design

**Date:** 2026-08-20
**Status:** Approved design, pending implementation plan

## Context

Credentialed tool configuration is the remaining gap in the sandbox: a Maven
`~/.m2/settings.xml` mixes shareable shape (proxies, mirrors) with per-user
credentials (repository server auth). The predecessor mechanism
(`.sandbox-secrets.yaml`) was removed deliberately: it resolved `source:`
commands on the host from an agent-authored workspace file — host command
execution from inside the sandbox. The integration suite still pins that
removal.

Profiles (2026-08-19 design) now provide a host-trusted, team-shareable
bundle format. This design extends them with **templates** rendered from
**secrets** (resolved on the host from the user's own configuration) and
**variables** (plain literals), delivered per start/apply directly into the
agent's home.

## Goals

- A team profile ships the whole shape of a credentialed config (e.g.
  `settings.xml` with proxies and mirrors) once; each user supplies only
  their credential sources.
- Secret values come from the host — a user-authored command (gopass, pass,
  op, …) or a literal — never from the guest, the workspace, or the profile.
- Profile authors never gain host command execution: profiles may *suggest*
  a source as inert, displayed-only metadata; nothing executes until the
  user copies it into their own mapping.
- No per-invocation secret-manager calls (pinentry): resolution happens at
  VM start and explicit `profile apply` only.
- Secrets never travel through the Lima template (`mode: data` persists in
  `~/.lima/<instance>/lima.yaml`) and never land in the world-readable guest
  profile tree.

## Non-goals (v1)

- Environment-variable or interactive-prompt sources (command + literal
  only).
- Per-profile secret scoping. Any active profile's hook can read the whole
  agent home, so scoping names to profiles would be an illusion; the design
  is honest about the real boundary instead (see Security).
- Removing rendered files on profile deactivation (consistent with the
  existing files/ non-goal; `code-vm recreate` is the clean slate).
- Escaping/encoding of substituted values (values are opaque bytes; a value
  that breaks the target format is the user's own, as in any hand-written
  config).
- Secret rotation detection. Rotation = rotate at the source, then restart
  or `profile apply`.

## Decisions

| Area | Decision |
|---|---|
| Trust line | Hybrid: profiles declare secret/var *names* with optional inert `suggest:`/`description:`; only the user's local mapping executes anything |
| Templates | Shipped by profiles under `templates/`, a home-mirroring tree with placeholders; generic across tools (Maven, npm, Gradle, …) |
| Secret sources (v1) | `command:` (host shell, stdout is the value, trailing newline stripped) and `value:` (literal) |
| Variables | Plain literals in `config.yaml` under `vars:`; non-secret knobs |
| Resolve/render/push timing | `code-vm start`, `code-vm profile apply`, and any invocation whose `ensureRunning` actually booted the VM |
| Delivery | Host-side render, staged push directly to `/home/<agent>/<rel>`, agent-owned, 0600, no exec bit — the git-identity path |
| Placeholder syntax | `${secret:name}` and `${var:name}` only; all other `${...}` passes through untouched |
| Failure mode | A declared-but-unmapped secret/var fails start/apply with an actionable error carrying a ready-to-paste snippet |

## Profile bundle extensions

```
wetf-maven/
  profile.yaml
  templates/                 # home-mirroring tree, rendered before delivery
    .m2/settings.xml
  files/ ...                 # unchanged
```

`profile.yaml` gains two sections:

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

Validation, host-side at load, extending the existing rules:

- Secret and var names match the profile-name pattern
  (`[a-zA-Z0-9][a-zA-Z0-9-]{0,62}`): they appear in placeholders and error
  messages, never in shell commands or guest paths.
- `description` and `suggest` are inert display strings — never executed,
  never substituted into anything, never delivered to the guest.
- `templates/` entries get exactly the `files/` treatment: conservative
  charset, no `..`, no absolute paths, no symlinks anywhere (tree root,
  directories, entries), locked Claude settings paths rejected.
- A template and a `files/` entry targeting the same destination within one
  profile is a validation error. Across profiles, list order wins, as with
  files.
- A template referencing an undeclared `${secret:...}` or `${var:...}` name
  is a load-time validation error: bundles cannot quietly depend on inputs
  they never declared. Declared-but-unreferenced names are allowed (hooks
  may not need templates).
- A profile with `secrets:`/`vars:`/`templates/` but nothing else remains a
  valid, non-empty profile.

## User-side values

Sensitivity decides the home:

- **`~/.config/code-vm/secrets.yaml`** — 0600, user-authored, never
  distributed. It lives inside the config tree the mount guards already
  protect, and is host-trusted exactly like `config.yaml`:

  ```yaml
  secrets:
    wetf-repo-user:
      command: gopass show -o wetf/artifactory-user
    wetf-repo-password:
      command: gopass show -o wetf/artifactory-password
    low-value-token:
      value: abc123        # literal; a footgun for real credentials
  ```

  `command` runs on the host through the shell at resolve time; stdout with
  one trailing newline stripped is the value. A failing command fails the
  start/apply with the command's stderr. `command` and `value` are mutually
  exclusive per entry. code-vm warns when the file is group/world readable.

- **`vars:` in `config.yaml`** — a plain `map[string]string` of non-secret
  literals (URLs, org names), handled like every other config key.

Mappings are global, not per-profile: a name maps once, and every active
profile declaring that name receives the same value (see Security for why
scoping would be an illusion).

## Rendering

Plain string substitution, host-side. Only the exact forms `${secret:name}`
and `${var:name}` substitute; any other `${...}` (Maven properties,
`${env.FOO}`) passes through byte-for-byte. Values are opaque bytes with no
escaping layer. Each secret's command runs once per resolve pass, however
many templates reference it. Rendering happens after profile load and before
any push; a resolution or substitution failure aborts the whole start/apply
before anything reaches the guest.

## Delivery and lifecycle

Resolve → render → push runs at exactly three moments:

1. `code-vm start`, after the readiness gate;
2. `code-vm profile apply`;
3. any invocation whose `ensureRunning` actually booted the VM — the first
   command after a cold boot must not see a half-configured home.

A plain invocation against a running VM resolves nothing — no pinentry.

Delivery is a direct staged push (the git-identity path: host temp file,
0600, random staging name, agent-privilege install) to
`/home/<agent>/<rel>`, owner agent, mode 0600, no exec bit. Rendered content
never enters `mode: data`, never enters `/usr/local/share/sandbox-profiles`,
and exists on the host only as the transient staging temp file. Rendered
files persist in the agent home across restarts and are canonically
re-pushed on every start/apply while the profile is active. The guest
applier is uninvolved: an unattended guest boot has no host to resolve
secrets, and `code-vm start` — the only way users boot — is host-driven and
pushes immediately after readiness.

## Security

The trust statement, extended (printed by `profile add`, now including the
profile's declared secret names):

> Mapping a secret makes its value readable by the agent — and therefore by
> every active profile's hook. Install profiles from sources you trust with
> the secrets you map.

The real boundaries:

- **The user chooses what exists at all.** Only names mapped in the user's
  own `secrets.yaml` ever resolve; declared-but-unmapped names fail loudly
  and execute nothing.
- **Hints never execute.** `suggest:` strings are displayed and offered as
  copy-paste snippets, nothing more. Host command execution comes only from
  the user's own file.
- **Guest exposure is inherent and bounded.** Tools read their configs as
  the agent, so rendered values are agent-readable by design. Exfiltration
  is bounded by the egress allowlist and recorded in the proxy log. Users
  should map only sandbox-appropriate credentials (e.g. a repo-read token,
  not an org admin password).
- **Nothing agent- or workspace-authored reaches resolution.** Profile
  sources are mount-guarded and symlink-rejected (2026-08-19 design, as
  hardened); `secrets.yaml` sits in the same guarded config tree; the
  workspace remains untrusted. The old mechanism's regression guards stay.
- **Host residue is transient.** Rendered values exist on the host only in
  the 0600 staging temp file, deleted after the push; never in
  `~/.lima/*/lima.yaml`.
- `profile list` marks profiles that declare secrets.

## CLI/UX

- `code-vm secrets` — lists the union of declared secrets and vars across
  active profiles: name, declaring profile(s), description, mapped/unmapped
  (names and status only, never values), and for unmapped secrets with a
  hint, a ready-to-paste `secrets.yaml` snippet.
- Start/apply failure for a missing mapping names the profile, the key, the
  description, and prints the same snippet.
- README gains a "Credentials" section documenting the trust model, the
  Maven example end to end, and the rotation story (rotate at source →
  restart or `profile apply`).

## Testing

Existing style: table-driven unit tests, fake runners, integration suite.

- **Manifest/template validation** — secret/var name rules, inert hint
  handling, undeclared-placeholder rejection, template/file collision,
  symlinked `templates/` rejection, locked-settings rejection.
- **Rendering** — substitution of both namespaces, `${...}` passthrough,
  opaque-bytes fidelity, one-command-per-secret resolution with an injected
  host runner, command-failure and missing-mapping error text (including
  the snippet).
- **secrets.yaml** — load, 0600 permission warning, command/value mutual
  exclusion.
- **Session push** — fake-runner argv assertions: staged install to home
  paths, agent uid/gid numeric, 0600.
- **CLI** — resolve/push triggered by start, apply, and boot-causing
  invocations, and by nothing else; `code-vm secrets` output for
  mapped/unmapped states.
- **Integration** — fixture profile with a template using one
  literal-mapped secret and one var: rendered file lands 0600 agent-owned
  with substituted content, survives a restart, updates after a mapping
  change plus `profile apply`; a declared-but-unmapped secret fails apply
  with the snippet in the error.
