# Orca SSH Access and Detached VM Boot Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let Orca add the code-vm guest as an SSH host that logs in as `devuser`, and boot the VM in its own systemd scope so no terminal or Orca workspace can take it down.

**Architecture:** Two independent changes to the Go CLI and the guest scripts. (1) `limactl start` runs through `systemd-run --user --scope` on Linux hosts with a user manager, via a new `RunDetached` on the lima Runner. (2) A per-instance ed25519 keypair on the host, its public key pushed into `devuser`'s `authorized_keys` through the existing agent-identity relay at every boot, a pinned `ssh.localPort` from a new `sshPort` config field, and a `code-vm ssh-target` command that prints what Orca needs. The guest's task cap rises because SSH sessions land in `devuser`'s systemd slice.

**Tech Stack:** Go 1.26.5 (cobra, yaml.v3, no new modules), `ssh-keygen` on the host, Lima 2.2 templates, bash guest scripts (shellcheck 0.11, shfmt 3.13), the bash integration suite `test-vm-sandbox.sh`.

**Spec:** `docs/superpowers/specs/2026-10-26-orca-ssh-access-design.md`

## Global Constraints

- No new Go module dependencies: key generation shells out to `ssh-keygen`; the only ssh client code is argv construction.
- Detached boot is Linux-only. On darwin, and on Linux without `systemd-run` or without a reachable user manager, every boot path behaves exactly as today.
- Unit tests never need a VM, systemd, or network. Tests that run the real `ssh-keygen` skip when it is not on PATH.
- The private key is `0600` inside a `0700` directory under the config directory: `<config dir>/ssh/<instance>/agent_ed25519`, with `agent_ed25519.pub` and `known_hosts` beside it. It never enters the guest or the Lima template.
- `authorized_keys` in the guest is exactly one line, the agent's public key, mode `0600`, installed through `PushUserFile` (the `install-user-file.sh` relay), never by a root write into the home.
- `sshPort` default `0` keeps Lima's random port; when set it must be `1024..65535` and renders `ssh.localPort`.
- `TasksMax` for the agent slice becomes `8192`; `MemoryMax` is unchanged.
- `code-vm ssh-target` never boots the VM and exits 0 whether or not it is running.
- Commit messages follow Conventional Commits (`feat:`, `fix:`, `test:`, `docs:`), no `Co-authored-by` trailers. Guest script changes reach a VM only on its next start.

## Review Focus

- **Agent-planted `authorized_keys`.** The agent owns its home and can append its own key or replace the file with a symlink; the next boot and `ssh-target` must leave exactly the one host-issued line. Pinned in Task 7 (integration: plant a second line, run `ssh-target`, expect one line).
- **`ssh-target` against a stopped, unpinned VM.** It must print, warn, exit 0 and never issue `limactl start`. Pinned in Task 6 (`TestSSHTargetNeverBoots`).
- **Key directory with loose permissions** (copied by hand, restored from a backup): ssh refuses a world-readable key, so `Ensure` must tighten `0700`/`0600` every time. Pinned in Task 4 (`TestEnsureTightensPermissions`).
- **`systemd-run` present but no user manager** (an ssh login without lingering, a `sudo` shell): the probe fails and the boot must proceed plainly rather than error. Pinned in Task 1 (`TestDetachPrefixOnlyOnLinuxWithUserManager`, probe-false case).
- **Half a keypair on disk** (`.pub` deleted, or private deleted): generating a new half would silently desynchronise host and guest; `Ensure` must fail with a message naming the directory to delete. Pinned in Task 4 (`TestEnsureRefusesHalfKeypair`).

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/lima/detach.go` (new) | Decide whether and how a boot is wrapped in a transient systemd scope; build the wrapped argv. |
| `internal/lima/limactl.go` | `Runner.RunDetached`, `ExecRunner.Detach`, `Start`/`StartExisting` use it; `ParseSSHPort`. |
| `internal/cli/doctor.go` | WARN-level check for detached boot availability. |
| `internal/config/config.go`, `paths.go` | `SSHPort` field and validation; `SSHDirFor`; `MountsExcludeTree` takes a label. |
| `internal/guest/files/lima/code-sandbox.yaml.tpl` | Renders `ssh.localPort` when pinned. |
| `internal/agentssh/key.go` (new package) | Host keypair lifecycle: paths, generation via `ssh-keygen`, permission tightening. |
| `internal/session/authorizedkey.go` (new) | Push and probe `devuser`'s `authorized_keys` through the relay. |
| `internal/cli/agentkey.go` (new) | `ensureAgentKey`: glue called after every boot. |
| `internal/cli/sshtarget.go` (new) | `code-vm ssh-target` command and its output rendering. |
| `internal/cli/root.go`, `start.go`, `shell.go`, `recreate.go`, `mount.go` | Register the command; call `ensureAgentKey` after boots; guard the ssh dir against mounts. |
| `internal/guest/files/scripts/provision-system.sh` | `TasksMax=8192`. |
| `test-vm-sandbox.sh` | Scratch config pins `sshPort`; new "Agent SSH access" section; scope regression in restart hygiene; TasksMax value. |
| `README.md` | `sshPort`, `ssh-target`, "Using the sandbox from Orca", known limitations. |

---

### Task 1: Boot the VM inside a transient systemd scope

**Files:**
- Create: `internal/lima/detach.go`, `internal/lima/detach_test.go`
- Modify: `internal/lima/limactl.go` (Runner interface, ExecRunner, Start, StartExisting, NewClient)
- Modify: `internal/lima/limactl_test.go` (fakeRunner), `internal/cli/start_test.go` (recordingRunner)

**Interfaces:**
- Produces: `Runner.RunDetached(ctx context.Context, args ...string) error`; `ExecRunner.Detach func(ctx context.Context) []string`; `func DetachPrefix(ctx context.Context, goos string, lookPath func(string) (string, error), probe func(context.Context) bool, instance string) []string`; `func ProbeUserManager(ctx context.Context) bool`; `func detachedArgv(prefix []string, bin string, args []string) (string, []string)`.
- The exact prefix for a Linux host with a user manager: `[]string{"systemd-run", "--user", "--scope", "--quiet", "--collect", "--description=code-vm VM boot (" + instance + ")", "--"}`.

- [ ] **Step 1: Write the failing tests in `internal/lima/detach_test.go`**

```go
func TestDetachPrefixOnlyOnLinuxWithUserManager(t *testing.T) {
	found := func(string) (string, error) { return "/usr/bin/systemd-run", nil }
	missing := func(string) (string, error) { return "", errors.New("not found") }
	yes := func(context.Context) bool { return true }
	no := func(context.Context) bool { return false }
	ctx := context.Background()
	if got := DetachPrefix(ctx, "darwin", found, yes, "code-sandbox"); got != nil {
		t.Errorf("darwin: got %v, want nil", got)
	}
	if got := DetachPrefix(ctx, "linux", missing, yes, "code-sandbox"); got != nil {
		t.Errorf("no systemd-run: got %v, want nil", got)
	}
	if got := DetachPrefix(ctx, "linux", found, no, "code-sandbox"); got != nil {
		t.Errorf("probe failed: got %v, want nil", got)
	}
	want := []string{"systemd-run", "--user", "--scope", "--quiet", "--collect",
		"--description=code-vm VM boot (code-sandbox)", "--"}
	if got := DetachPrefix(ctx, "linux", found, yes, "code-sandbox"); !reflect.DeepEqual(got, want) {
		t.Errorf("linux with user manager: got %q, want %q", got, want)
	}
}

func TestDetachedArgvWrapsLimactl(t *testing.T) {
	bin, args := detachedArgv([]string{"systemd-run", "--user", "--"}, "limactl", []string{"start", "--tty=false", "code-sandbox"})
	if bin != "systemd-run" {
		t.Errorf("bin = %q, want systemd-run", bin)
	}
	want := []string{"--user", "--", "limactl", "start", "--tty=false", "code-sandbox"}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("args = %q, want %q", args, want)
	}
}

// A probe that lies is the only way to exercise the exec path without
// systemd: "env --" is a wrapper with the same shape as systemd-run's prefix.
func TestExecRunnerRunDetachedUsesThePrefix(t *testing.T) {
	r := ExecRunner{Bin: "true", Detach: func(context.Context) []string { return []string{"env", "--"} }}
	if err := r.RunDetached(context.Background(), "start"); err != nil {
		t.Fatalf("RunDetached via env: %v", err)
	}
	plain := ExecRunner{Bin: "true", Detach: func(context.Context) []string { return nil }}
	if err := plain.RunDetached(context.Background(), "start"); err != nil {
		t.Fatalf("RunDetached without prefix: %v", err)
	}
	if err := (ExecRunner{Bin: "true"}).RunDetached(context.Background(), "start"); err != nil {
		t.Fatalf("RunDetached with nil Detach: %v", err)
	}
}
```

And in `internal/lima/limactl_test.go`, after giving `fakeRunner` a `detached [][]string` field and a `RunDetached` method that appends to both `detached` and `calls`:

```go
func TestStartAndStartExistingRunDetached(t *testing.T) {
	f := &fakeRunner{}
	c := Client{R: f}
	if err := c.Start(context.Background(), "/tmp/inst.yaml"); err != nil {
		t.Fatal(err)
	}
	if err := c.StartExisting(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"start", "--tty=false", "--name", InstanceName, "/tmp/inst.yaml"},
		{"start", "--tty=false", InstanceName},
	}
	if !reflect.DeepEqual(f.detached, want) {
		t.Errorf("detached calls = %q, want %q", f.detached, want)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/lima/ -run 'TestDetach|TestExecRunnerRunDetached|TestStartAndStartExisting' -v`
Expected: compile errors (`DetachPrefix`, `detachedArgv`, `RunDetached` undefined).

- [ ] **Step 3: Implement `internal/lima/detach.go`**

- `DetachPrefix` returns nil unless `goos == "linux"`, `lookPath("systemd-run")` succeeds, and `probe(ctx)` is true; otherwise the exact prefix above.
- `ProbeUserManager` runs `systemd-run --user --scope --quiet --collect -- true` with a 5-second timeout derived from ctx and returns `err == nil`.
- `detachedArgv(prefix, bin, args)` returns `prefix[0]` and `append(prefix[1:], bin, args...)` on fresh slices.

Add a file comment stating why: Lima's host agent and QEMU stay in the caller's cgroup, and a terminal or Orca pane that stops its scope (`KillMode=control-group`) kills the VM with it.

- [ ] **Step 4: Extend the Runner in `internal/lima/limactl.go`**

- Add `RunDetached(ctx context.Context, args ...string) error` to the `Runner` interface.
- Add `Detach func(ctx context.Context) []string` to `ExecRunner`. `RunDetached` calls `e.Detach` when non-nil; an empty result falls through to `Run`; otherwise it execs `detachedArgv(prefix, bin, args)` with the same stdio wiring as `Run`, wrapping failures as `fmt.Errorf("limactl %s (via systemd-run): %w", strings.Join(args, " "), err)`.
- `NewClient` sets `Detach: func(ctx context.Context) []string { return DetachPrefix(ctx, runtime.GOOS, exec.LookPath, ProbeUserManager, InstanceName) }`.
- `Start` and `StartExisting` call `c.R.RunDetached` instead of `c.R.Run`.
- `recordingRunner` in `internal/cli/start_test.go` gains `RunDetached` that appends to `calls` (so `started()` keeps working) and `fakeRunner` in `limactl_test.go` gains the `detached` field and method.

- [ ] **Step 5: Run the whole unit suite**

Run: `go test ./...`
Expected: PASS, including the existing `TestEnsureRunningStartsWhenAbsentOrStopped`.

- [ ] **Step 6: Commit**

```bash
git add internal/lima internal/cli/start_test.go
git commit -m "fix: boot the VM in its own systemd scope so a dying terminal cannot take it down"
```

---

### Task 2: `doctor` reports detached-boot availability as a warning

**Files:**
- Modify: `internal/cli/doctor.go`
- Test: `internal/cli/doctor_test.go`

**Interfaces:**
- Consumes: `lima.DetachPrefix`, `lima.ProbeUserManager` from Task 1.
- Produces: `check.warn bool`; `func printChecks(out io.Writer, checks []check) (failed int)`; `func checkDetachedBoot(ctx context.Context) error`.

- [ ] **Step 1: Write the failing test**

```go
func TestPrintChecksWarnDoesNotFail(t *testing.T) {
	var out bytes.Buffer
	failed := printChecks(&out, []check{
		{name: "fine", err: nil},
		{name: "soft", err: errors.New("degraded"), warn: true},
		{name: "hard", err: errors.New("broken")},
	})
	if failed != 1 {
		t.Errorf("failed = %d, want 1 (warnings never count)", failed)
	}
	for _, want := range []string{"  OK   fine", "  WARN soft: degraded", "  FAIL hard: broken"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/cli/ -run TestPrintChecksWarnDoesNotFail -v`
Expected: FAIL, `printChecks` undefined / unknown field `warn`.

- [ ] **Step 3: Implement**

- Extract the loop in `newDoctorCmd` into `printChecks`, printing `  WARN %s: %v` for `warn` checks and not counting them.
- `checkDetachedBoot(ctx)` returns nil when `lima.DetachPrefix(ctx, "linux", exec.LookPath, lima.ProbeUserManager, "doctor")` is non-empty, else `errors.New("systemd-run --user is unavailable, so a VM booted from a terminal that kills by control group (an Orca workspace delete, some terminal tabs) dies with it; boot with `code-vm start` from a plain shell")`.
- In `newDoctorCmd`, when `runtime.GOOS == "linux"`, append `check{name: "VM boot detaches into its own systemd scope", err: checkDetachedBoot(ctx), warn: true}` after the hypervisor checks.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/cli/ -run 'TestPrintChecks|TestDoctor' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/doctor.go internal/cli/doctor_test.go
git commit -m "feat: doctor warns when VM boots cannot detach into a systemd scope"
```

---

### Task 3: `sshPort` config field renders a pinned Lima SSH port

**Files:**
- Modify: `internal/config/config.go`, `internal/config/config_test.go`
- Modify: `internal/guest/files/lima/code-sandbox.yaml.tpl`, `internal/lima/template_test.go`
- Modify: `README.md` (Configuration snippet)

**Interfaces:**
- Produces: `Config.SSHPort int` with yaml tag `sshPort,omitempty`.

- [ ] **Step 1: Write the failing tests**

In `internal/config/config_test.go`:

```go
func TestValidateSSHPort(t *testing.T) {
	for _, tc := range []struct {
		port int
		ok   bool
	}{{0, true}, {1024, true}, {60022, true}, {65535, true}, {22, false}, {1023, false}, {65536, false}, {-1, false}} {
		c := Default()
		c.ProjectsRoot = "/home/st/projects"
		c.SSHPort = tc.port
		err := c.Validate()
		if (err == nil) != tc.ok {
			t.Errorf("sshPort %d: err = %v, want ok=%v", tc.port, err, tc.ok)
		}
	}
}
```

In `internal/lima/template_test.go`:

```go
func TestRenderPinsSSHPortOnlyWhenSet(t *testing.T) {
	out, err := Render(testConfig(), testParams(nil))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "localPort") {
		t.Error("default config must leave Lima's random SSH port alone")
	}
	c := testConfig()
	c.SSHPort = 60022
	out, err = Render(c, testParams(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "  localPort: 60022\n") {
		t.Errorf("pinned port missing from rendered ssh block:\n%s", out)
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("not valid YAML: %v", err)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/config/ ./internal/lima/ -run 'TestValidateSSHPort|TestRenderPinsSSHPort' -v`
Expected: FAIL (`SSHPort` undefined).

- [ ] **Step 3: Implement**

- `config.go`: add after `Swap`:
  ```go
  // SSHPort pins the host port Lima forwards to the guest's sshd. 0 lets
  // Lima pick a free port on every boot. Set it to give SSH clients that
  // store the target (Orca, an ssh_config alias) a stable address.
  SSHPort int `yaml:"sshPort,omitempty"`
  ```
  and in `Validate`: `if c.SSHPort != 0 && (c.SSHPort < 1024 || c.SSHPort > 65535) { return fmt.Errorf("sshPort must be 0 (Lima picks a free port) or between 1024 and 65535, got %d", c.SSHPort) }`.
- Template `ssh:` block becomes:
  ```yaml
  ssh:
  {{- if .Config.SSHPort}}
    # Pinned by sshPort so an SSH target stored elsewhere stays valid across boots.
    localPort: {{.Config.SSHPort}}
  {{- end}}
    # Do not copy host SSH public keys into the guest.
    loadDotSSHPubKeys: false
  ```

- [ ] **Step 4: Run the tests, including the golden file**

Run: `go test ./internal/config/ ./internal/lima/`
Expected: PASS; the golden template is unchanged because `testConfig()` leaves `SSHPort` at 0.

- [ ] **Step 5: Commit**

```bash
git add internal/config internal/guest/files/lima/code-sandbox.yaml.tpl internal/lima/template_test.go
git commit -m "feat: sshPort config pins the host port of the guest's sshd"
```

---

### Task 4: Host keypair for the agent user

**Files:**
- Create: `internal/agentssh/key.go`, `internal/agentssh/key_test.go`
- Modify: `internal/config/paths.go`, `internal/config/paths_test.go` (`SSHDirFor`)
- Modify: `internal/config/config.go` (`MountsExcludeTree` label), `internal/cli/root.go`, `internal/cli/mount.go`, their tests

**Interfaces:**
- Produces:
  ```go
  // package agentssh
  const PrivateKeyName = "agent_ed25519"
  type Key struct {
      PrivatePath, PublicPath, KnownHostsPath string
      AuthorizedLine string // the one line that goes into authorized_keys, no newline
  }
  var Keygen = func(ctx context.Context, privatePath, comment string) error // ssh-keygen -q -t ed25519 -N "" -C comment -f privatePath
  func InstanceDir(sshDir, instance string) string                        // filepath.Join(sshDir, instance)
  func Ensure(ctx context.Context, dir, instance string) (Key, error)
  ```
  `config.SSHDirFor(configPath string) string` returns `filepath.Join(filepath.Dir(configPath), "ssh")`.
  `config.MountsExcludeTree(dir, what string) error` — `what` names the protected thing in the message ("the code-vm profiles", "the agent SSH key").

- [ ] **Step 1: Write the failing tests in `internal/agentssh/key_test.go`**

```go
// fakeKeygen writes a deterministic pair so tests need no ssh-keygen.
func fakeKeygen(t *testing.T) {
	t.Helper()
	old := Keygen
	Keygen = func(_ context.Context, path, comment string) error {
		if err := os.WriteFile(path, []byte("PRIVATE\n"), 0o600); err != nil {
			return err
		}
		return os.WriteFile(path+".pub", []byte("ssh-ed25519 AAAATEST "+comment+"\n"), 0o644)
	}
	t.Cleanup(func() { Keygen = old })
}

func TestEnsureGeneratesOnceAndReportsPaths(t *testing.T) {
	fakeKeygen(t)
	dir := filepath.Join(t.TempDir(), "ssh", "code-sandbox")
	k, err := Ensure(context.Background(), dir, "code-sandbox")
	if err != nil {
		t.Fatal(err)
	}
	if k.PrivatePath != filepath.Join(dir, "agent_ed25519") || k.PublicPath != k.PrivatePath+".pub" || k.KnownHostsPath != filepath.Join(dir, "known_hosts") {
		t.Errorf("paths = %+v", k)
	}
	if k.AuthorizedLine != "ssh-ed25519 AAAATEST code-vm code-sandbox agent" {
		t.Errorf("AuthorizedLine = %q", k.AuthorizedLine)
	}
	Keygen = func(context.Context, string, string) error { t.Fatal("Keygen called again for an existing key"); return nil }
	if _, err := Ensure(context.Background(), dir, "code-sandbox"); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureTightensPermissions(t *testing.T) {
	fakeKeygen(t)
	dir := filepath.Join(t.TempDir(), "ssh", "code-sandbox")
	if _, err := Ensure(context.Background(), dir, "code-sandbox"); err != nil {
		t.Fatal(err)
	}
	os.Chmod(dir, 0o755)
	os.Chmod(filepath.Join(dir, "agent_ed25519"), 0o644)
	if _, err := Ensure(context.Background(), dir, "code-sandbox"); err != nil {
		t.Fatal(err)
	}
	if m := mode(t, dir); m != 0o700 {
		t.Errorf("dir mode = %o, want 700", m)
	}
	if m := mode(t, filepath.Join(dir, "agent_ed25519")); m != 0o600 {
		t.Errorf("key mode = %o, want 600", m)
	}
}

func TestEnsureRefusesHalfKeypair(t *testing.T) {
	fakeKeygen(t)
	for _, remove := range []string{"agent_ed25519", "agent_ed25519.pub"} {
		dir := filepath.Join(t.TempDir(), "ssh", "code-sandbox")
		if _, err := Ensure(context.Background(), dir, "code-sandbox"); err != nil {
			t.Fatal(err)
		}
		os.Remove(filepath.Join(dir, remove))
		_, err := Ensure(context.Background(), dir, "code-sandbox")
		if err == nil || !strings.Contains(err.Error(), dir) {
			t.Errorf("after removing %s: err = %v, want an error naming %s", remove, err, dir)
		}
	}
}

func TestEnsureRejectsForeignPublicKey(t *testing.T) {
	fakeKeygen(t)
	dir := filepath.Join(t.TempDir(), "ssh", "code-sandbox")
	if _, err := Ensure(context.Background(), dir, "code-sandbox"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "agent_ed25519.pub"), []byte("ssh-rsa AAAA x\nssh-ed25519 BBBB y\n"), 0o644)
	if _, err := Ensure(context.Background(), dir, "code-sandbox"); err == nil {
		t.Error("a multi-line or non-ed25519 .pub must be refused, not pushed into the guest")
	}
}

func TestEnsureWithRealSSHKeygen(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not on PATH")
	}
	dir := filepath.Join(t.TempDir(), "ssh", "code-sandbox")
	k, err := Ensure(context.Background(), dir, "code-sandbox")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(k.AuthorizedLine, "ssh-ed25519 ") || strings.Contains(k.AuthorizedLine, "\n") {
		t.Errorf("AuthorizedLine = %q", k.AuthorizedLine)
	}
}
```

`mode(t, path)` is a small helper returning `fi.Mode().Perm()`.

In `internal/config/paths_test.go`:

```go
func TestSSHDirFor(t *testing.T) {
	if got := SSHDirFor("/home/st/.config/code-vm/config.yaml"); got != "/home/st/.config/code-vm/ssh" {
		t.Errorf("SSHDirFor = %q", got)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/agentssh/ ./internal/config/ -run 'TestEnsure|TestSSHDirFor' -v`
Expected: FAIL (package / function undefined).

- [ ] **Step 3: Implement `internal/agentssh/key.go`**

`Ensure`:
1. `os.MkdirAll(dir, 0o700)` then `os.Chmod(dir, 0o700)`.
2. Stat private and `.pub`. Both missing → `Keygen(ctx, private, "code-vm "+instance+" agent")`. Exactly one missing → `fmt.Errorf("%s holds half a keypair (%s is missing); delete the directory to generate a fresh one", dir, missingName)`.
3. `os.Chmod(private, 0o600)`.
4. Read `.pub`, `strings.TrimSpace`; it must contain no `\n` and start with `ssh-ed25519 `, else `fmt.Errorf("%s is not a single ed25519 public key line", pub)`.
5. Return the Key.

`Keygen` default: `exec.CommandContext(ctx, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", comment, "-f", privatePath)`, stderr captured into the error. A missing binary surfaces as an error wrapping `exec.ErrNotFound`, which callers test with `errors.Is`.

- [ ] **Step 4: `SSHDirFor` and the mount guard**

- Add `SSHDirFor` to `paths.go` beside `ProfilesDirFor`.
- Change `MountsExcludeTree(dir string)` to `MountsExcludeTree(dir, what string)`; both error messages interpolate `what` where they said "the code-vm profiles". Update the two callers (`root.go` `loadConfig`, `mount.go`) to pass `"the code-vm profiles"`, and add a third guard right after each: `c.MountsExcludeTree(config.SSHDirFor(path), "the agent SSH key")`. Update existing tests that match the message text.
- Add `TestLoadConfigRefusesMountOverSSHDir` to `internal/cli/root_test.go`: a scratch config whose `extraMounts` includes `<config dir>/ssh` makes `loadConfig` fail with a message containing "agent SSH key". Mirror the existing profiles-dir refusal in `mount_test.go` (around line 71) for `code-vm mount <config dir>/ssh`.

- [ ] **Step 5: Run the unit suite**

Run: `go test ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/agentssh internal/config internal/cli/root.go internal/cli/mount.go internal/cli/*_test.go
git commit -m "feat: generate a host keypair for the agent user and keep it out of every mount"
```

---

### Task 5: Install the public key into `devuser`'s `authorized_keys` at every boot

**Files:**
- Create: `internal/session/authorizedkey.go`, `internal/session/authorizedkey_test.go`
- Create: `internal/cli/agentkey.go`
- Modify: `internal/cli/shell.go` (`runDefault`), `internal/cli/start.go` (`newStartCmd`), `internal/cli/recreate.go`, `internal/cli/mount.go`
- Test: `internal/cli/start_test.go`

**Interfaces:**
- Consumes: `agentssh.Ensure`, `config.SSHDirFor` (Task 4); `session.PushUserFile`, `lima.Client.ReadFile`.
- Produces:
  ```go
  // package session
  const AuthorizedKeysRel = ".ssh/authorized_keys"
  func PushAuthorizedKey(ctx context.Context, d Deps, line string) error
  func AuthorizedKeyInstalled(ctx context.Context, d Deps, line string) bool
  // package cli
  func ensureAgentKey(ctx context.Context, cl lima.Client, c config.Config, cfgPath string, out io.Writer) error
  ```

- [ ] **Step 1: Write the failing tests**

In `internal/session/authorizedkey_test.go`, using the package's existing `fakeRunner` and `testDeps(t, r)` from `allowlist_test.go` (give `fakeRunner` an `out []byte` field that `Output` returns, if it has none yet):

```go
func TestPushAuthorizedKeyGoesThroughTheRelay(t *testing.T) {
	f := &fakeRunner{}
	d := testDeps(t, f)
	if err := PushAuthorizedKey(context.Background(), d, "ssh-ed25519 AAAA code-vm code-sandbox agent"); err != nil {
		t.Fatal(err)
	}
	if !f.ranAny("/usr/local/lib/sandbox/install-user-file.sh") || !f.ranAny(".ssh/authorized_keys 0600") {
		t.Errorf("expected a relay install of .ssh/authorized_keys with mode 0600, calls=%q", f.calls)
	}
}

func TestAuthorizedKeyInstalledComparesTrimmedContent(t *testing.T) {
	line := "ssh-ed25519 AAAA code-vm code-sandbox agent"
	for _, tc := range []struct {
		guest string
		want  bool
	}{{line + "\n", true}, {line, true}, {"", false}, {line + "\nssh-rsa BBBB planted\n", false}} {
		f := &fakeRunner{out: []byte(tc.guest)} // what ReadFile returns
		d := testDeps(t, f)
		if got := AuthorizedKeyInstalled(context.Background(), d, line); got != tc.want {
			t.Errorf("guest %q: got %v, want %v", tc.guest, got, tc.want)
		}
	}
}
```

In `internal/cli/start_test.go`, first add the helper later tasks reuse:

```go
// fakeAgentKeygen replaces ssh-keygen with a writer of fixed files, restored on cleanup.
func fakeAgentKeygen(t *testing.T) {
	t.Helper()
	old := agentssh.Keygen
	agentssh.Keygen = func(_ context.Context, path, comment string) error {
		if err := os.WriteFile(path, []byte("PRIVATE\n"), 0o600); err != nil {
			return err
		}
		return os.WriteFile(path+".pub", []byte("ssh-ed25519 AAAATEST "+comment+"\n"), 0o644)
	}
	t.Cleanup(func() { agentssh.Keygen = old })
}

func TestStartInstallsTheAgentKey(t *testing.T) {
	root := NewRootCmd()
	dir := withScratchConfig(t)
	fakeAgentKeygen(t)
	r := installFakeClient(t, "Stopped")
	root.SetArgs([]string{"start"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !ranAny(r.calls, ".ssh/authorized_keys 0600") {
		t.Errorf("start did not install the agent key, calls=%q", r.calls)
	}
	if _, err := os.Stat(filepath.Join(dir, "ssh", "code-sandbox-clitest", "agent_ed25519")); err != nil {
		t.Errorf("keypair not generated under the config dir: %v", err)
	}
}

func TestStartWarnsWhenSSHKeygenIsMissing(t *testing.T) {
	root := NewRootCmd()
	withScratchConfig(t)
	old := agentssh.Keygen
	agentssh.Keygen = func(context.Context, string, string) error { return fmt.Errorf("ssh-keygen: %w", exec.ErrNotFound) }
	t.Cleanup(func() { agentssh.Keygen = old })
	installFakeClient(t, "Stopped")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"start"})
	if err := root.Execute(); err != nil {
		t.Fatalf("a missing ssh-keygen must not fail the boot: %v", err)
	}
	if !strings.Contains(out.String(), "ssh-keygen") {
		t.Errorf("expected a warning naming ssh-keygen:\n%s", out.String())
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/session/ ./internal/cli/ -run 'TestPushAuthorizedKey|TestAuthorizedKeyInstalled|TestStartInstalls|TestStartWarns' -v`
Expected: FAIL (undefined functions).

- [ ] **Step 3: Implement**

- `session/authorizedkey.go`: `PushAuthorizedKey` → `PushUserFile(ctx, d, []byte(line+"\n"), AuthorizedKeysRel, "0600")`. `AuthorizedKeyInstalled` → `strings.TrimSpace(string(d.Client.ReadFile(ctx, "/home/"+d.AgentUser+"/"+AuthorizedKeysRel))) == line`.
- `cli/agentkey.go` `ensureAgentKey`:
  1. `key, err := agentssh.Ensure(ctx, agentssh.InstanceDir(config.SSHDirFor(cfgPath), c.Instance), c.Instance)`.
  2. `errors.Is(err, exec.ErrNotFound)` → `fmt.Fprintln(out, "warning: ssh-keygen not found on the host; skipping the agent SSH key (code-vm ssh-target needs it)")`, return nil. Any other error → return it.
  3. `session.PushAuthorizedKey(ctx, agentDeps(cl, c, nil), key.AuthorizedLine)`.
- Call sites, each immediately after the boot's `pushRendered` succeeds:
  - `shell.go` `runDefault`: inside `if started { ... }`, before returning to `cl.Agent`.
  - `start.go` `newStartCmd`: after `pushRendered` (runs on every explicit `start`; idempotent and cheap).
  - `recreate.go`, `mount.go`: after their `pushRendered`.

- [ ] **Step 4: Run the unit suite**

Run: `go test ./...`
Expected: PASS. Add to `TestRunDefaultRunningFastPathSkipsResolution` in `shell_test.go` the assertion that no call contains `install-user-file.sh`: the hot path against a running VM stays untouched.

- [ ] **Step 5: Commit**

```bash
git add internal/session internal/cli
git commit -m "feat: install the agent's SSH public key into the guest at every boot"
```

---

### Task 6: `code-vm ssh-target` and the README

**Files:**
- Create: `internal/cli/sshtarget.go`, `internal/cli/sshtarget_test.go`
- Modify: `internal/lima/limactl.go` (`ParseSSHPort`), `internal/lima/limactl_test.go`
- Modify: `internal/cli/root.go` (register), `README.md`

**Interfaces:**
- Consumes: `agentssh.Ensure`, `session.AuthorizedKeyInstalled`, `session.PushAuthorizedKey`, `lima.Client.SSHConfigFile`, `Status`.
- Produces:
  ```go
  // package lima
  func ParseSSHPort(sshConfig string) (int, error) // first "Port N" line of a Lima ssh.config
  // package cli
  type sshTarget struct {
      Instance             string
      Port                 int  // 0 when unknown
      Pinned               bool // from config.sshPort
      User                 string
      IdentityFile         string
      KnownHosts           string
  }
  func renderSSHTarget(t sshTarget) string
  func newSSHTargetCmd() *cobra.Command
  ```

- [ ] **Step 1: Write the failing tests**

`internal/lima/limactl_test.go`:

```go
func TestParseSSHPort(t *testing.T) {
	cfg := "Host lima-code-sandbox\n  IdentityFile \"/x\"\n  Hostname 127.0.0.1\n  Port 38421\n"
	got, err := ParseSSHPort(cfg)
	if err != nil || got != 38421 {
		t.Errorf("ParseSSHPort = %d, %v; want 38421", got, err)
	}
	if _, err := ParseSSHPort("Host x\n"); err == nil {
		t.Error("a config without Port must be an error")
	}
}
```

`internal/cli/sshtarget_test.go`:

```go
func TestRenderSSHTargetPinned(t *testing.T) {
	got := renderSSHTarget(sshTarget{Instance: "code-sandbox", Port: 60022, Pinned: true, User: "devuser",
		IdentityFile: "/h/.config/code-vm/ssh/code-sandbox/agent_ed25519", KnownHosts: "/h/.config/code-vm/ssh/code-sandbox/known_hosts"})
	want := `Orca / SSH target for instance code-sandbox
  Host:          127.0.0.1
  Port:          60022
  User:          devuser
  Identity file: /h/.config/code-vm/ssh/code-sandbox/agent_ed25519

OpenSSH config snippet (add to ~/.ssh/config or an included file):
Host code-vm-code-sandbox
  HostName 127.0.0.1
  Port 60022
  User devuser
  IdentityFile /h/.config/code-vm/ssh/code-sandbox/agent_ed25519
  IdentitiesOnly yes
  UserKnownHostsFile /h/.config/code-vm/ssh/code-sandbox/known_hosts
`
	if got != want {
		t.Errorf("renderSSHTarget pinned:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestRenderSSHTargetUnpinned(t *testing.T) {
	running := renderSSHTarget(sshTarget{Instance: "code-sandbox", Port: 38421, User: "devuser", IdentityFile: "/k", KnownHosts: "/kh"})
	if !strings.Contains(running, "  Port:          38421\n") || !strings.Contains(running, "warning: sshPort is not set in config.yaml, so this port changes on every boot") {
		t.Errorf("unpinned running output:\n%s", running)
	}
	stopped := renderSSHTarget(sshTarget{Instance: "code-sandbox", Port: 0, User: "devuser", IdentityFile: "/k", KnownHosts: "/kh"})
	if !strings.Contains(stopped, "  Port:          unknown (VM not running; set sshPort in config.yaml to pin one)") || strings.Contains(stopped, "Host code-vm-") {
		t.Errorf("unpinned stopped output must say unknown and omit the snippet:\n%s", stopped)
	}
}

func TestSSHTargetNeverBoots(t *testing.T) {
	root := NewRootCmd()
	withScratchConfig(t)
	fakeAgentKeygen(t)
	r := installFakeClient(t, "Stopped")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"ssh-target"})
	if err := root.Execute(); err != nil {
		t.Fatalf("ssh-target on a stopped VM must succeed: %v", err)
	}
	if r.started() {
		t.Error("ssh-target must never boot the VM")
	}
	if !strings.Contains(out.String(), "User:          devuser") {
		t.Errorf("output:\n%s", out.String())
	}
}

func TestSSHTargetUsesPinnedPortAndInstallsMissingKey(t *testing.T) {
	root := NewRootCmd()
	withScratchConfig(t)
	appendConfig(t, "sshPort: 60122\n")
	fakeAgentKeygen(t)
	r := installFakeClient(t, "Running") // ReadFile of authorized_keys returns "Running" too: not the key, so a push follows
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"ssh-target"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "  Port:          60122\n") {
		t.Errorf("pinned port not shown:\n%s", out.String())
	}
	if !ranAny(r.calls, ".ssh/authorized_keys 0600") || !strings.Contains(out.String(), "Installed the agent's SSH key into the running VM.") {
		t.Errorf("a running VM without the key must get it installed; calls=%q out=%s", r.calls, out.String())
	}
	if ranAny(r.calls, "{{.SSHConfigFile}}") {
		t.Error("a pinned port needs no look at Lima's ssh config")
	}
}

func TestSSHTargetReadsLimaPortWhenUnpinned(t *testing.T) {
	root := NewRootCmd()
	withScratchConfig(t)
	fakeAgentKeygen(t)
	sshCfg := filepath.Join(t.TempDir(), "ssh.config")
	os.WriteFile(sshCfg, []byte("Host lima-code-sandbox-clitest\n  Port 38421\n"), 0o600)
	r := installFakeClient(t, "Running")
	r.outputs = map[string]string{"{{.SSHConfigFile}}": sshCfg}
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"ssh-target"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "  Port:          38421\n") || !strings.Contains(out.String(), "warning: sshPort is not set") {
		t.Errorf("output:\n%s", out.String())
	}
}
```

Note `recordingRunner.Output` answers `ReadFile` calls with `statusOut` unless `outputs` matches; in the Running tests that means `authorized_keys` reads back "Running", which correctly counts as "not installed".

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/lima/ ./internal/cli/ -run 'TestParseSSHPort|TestRenderSSHTarget|TestSSHTarget' -v`
Expected: FAIL (undefined).

- [ ] **Step 3: Implement**

- `lima.ParseSSHPort`: scan lines, regexp `^\s*Port\s+(\d+)\s*$`, first match wins; no match → `errors.New("no Port line in Lima's ssh config")`.
- `renderSSHTarget`: exact text from the test. Rules: Port 0 → the "unknown" line and no snippet; Port set and not Pinned → the port line followed by `  (warning: sshPort is not set in config.yaml, so this port changes on every boot)`; snippet printed whenever Port is non-zero.
- `newSSHTargetCmd` (`Use: "ssh-target"`, `Short: "Print the SSH target Orca or an ssh_config alias needs to reach the agent user"`, `Args: cobra.NoArgs`):
  1. `loadConfig()`; `cl := clientFor(c)`; `status, _ := cl.Status(ctx)`.
  2. `key, err := agentssh.Ensure(...)` as in `ensureAgentKey`; a missing `ssh-keygen` is a hard error here (the command's whole output depends on the key).
  3. Port: `c.SSHPort` if non-zero; else if `status == "Running"`, `cl.SSHConfigFile` → `os.ReadFile` → `lima.ParseSSHPort`; else 0.
  4. If `status == "Running"` and `!session.AuthorizedKeyInstalled(ctx, d, key.AuthorizedLine)`: `PushAuthorizedKey` and print `Installed the agent's SSH key into the running VM.` before the rendered block.
  5. Print `renderSSHTarget`.
- Register `root.AddCommand(newSSHTargetCmd())` in `root.go`.
- README:
  - Configuration snippet: add `sshPort: 0                    # fixed host port for the guest's sshd; 0 lets Lima pick — see below` after the `swap` line.
  - Commands table row: `| `code-vm ssh-target` | Print the SSH target (host, port, user, key) for Orca or an ssh_config alias |`.
  - New section `### Using the sandbox from Orca` after "Reaching guest servers from the host", covering: set `sshPort` (example `60022`) and restart; run `code-vm ssh-target`; in Orca Settings → SSH → Add target enter the four values, Test, Save; add the project on that host at the repo's mounted path (identical to the host path) and create workspaces on it, while "Local" workspaces keep the host's own Claude. Caveats: after `code-vm recreate` the guest's host key changes and Orca will refuse the target until it is re-trusted or re-added; Orca's remote worktrees default to `~/orca/workspaces` in the guest home, which host tools cannot see, so point Orca's remote workspace directory inside the repo if that matters; agent session history is not available in remote workspaces.
  - Known limitations: two new bullets. "**Orca's relay is agent-controllable.** Orca installs its relay into the agent's home and trusts what it says; a compromised agent can speak to the Orca app through it, as with any SSH host Orca manages." and "**SSH sessions run under the agent's resource limits, `code-vm -- ...` sessions do not.** Logins as `devuser` land in its systemd slice (`TasksMax`, `MemoryMax`); sessions started through `sandbox-exec` are children of the admin login and unlimited."

- [ ] **Step 4: Run the unit suite, lint, formatting**

Run: `go test ./... && mise run lint && mise run fmt-check`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/lima internal/cli README.md
git commit -m "feat: add ssh-target to print the agent user's SSH target for Orca"
```

---

### Task 7: Guest task cap and the integration suite

**Files:**
- Modify: `internal/guest/files/scripts/provision-system.sh:143-147`
- Modify: `test-vm-sandbox.sh` (scratch config, "Resource limits", new "Agent SSH access" section after "Session setup", "Restart hygiene")

**Interfaces:**
- Consumes: `code-vm ssh-target` output format (Task 6); key path `<config dir>/ssh/<instance>/agent_ed25519` (Task 4).

- [ ] **Step 1: Raise the cap**

In `provision-system.sh` set `TasksMax=8192` and extend the comment: the cap replaced the container sandbox's `--pids-limit`; SSH logins as the agent (Orca's relay and Node runtime, Claude, Gradle and Testcontainers runs) share this slice with rootless Docker, which 2048 did not allow for.

- [ ] **Step 2: Update and extend the suite**

- Scratch config heredoc: add `sshPort: 60122` (a test-only port, so a daily VM pinned to the README's `60022` never collides).
- "Resource limits": the assertion matches `TasksMax=8192`.
- New section after "Session setup":

```bash
echo ""
echo "── Agent SSH access ──────────────────────────────────────────────"

AGENT_KEY="$TEST_CONFIG_DIR/ssh/$INSTANCE/agent_ed25519"
SSH_TARGET_OUT=$("${CODE_VM_ARGS[@]}" ssh-target 2>&1)
if echo "$SSH_TARGET_OUT" | grep -q "Port:          60122" && echo "$SSH_TARGET_OUT" | grep -q "User:          $AGENT_USER"; then
    pass "ssh-target prints the pinned port and the agent user"
else
    fail "ssh-target prints the pinned port and the agent user (got: $SSH_TARGET_OUT)"
fi
if [ "$(stat -c '%a' "$AGENT_KEY" 2> /dev/null || stat -f '%Lp' "$AGENT_KEY")" = "600" ]; then
    pass "the agent private key is 0600 on the host"
else
    fail "the agent private key is 0600 on the host"
fi

# Trust is left to the client here: the suite's VM is throwaway, so its host
# key is irrelevant, and known_hosts must not accumulate per run.
agent_ssh() {
    ssh -i "$AGENT_KEY" -p 60122 -o IdentitiesOnly=yes -o BatchMode=yes \
        -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR \
        "$AGENT_USER@127.0.0.1" "$@"
}
if [ "$(agent_ssh id -un)" = "$AGENT_USER" ]; then
    pass "ssh as the agent user with the host-issued key works"
else
    fail "ssh as the agent user with the host-issued key works"
fi
# ControlPath=none: Lima's config multiplexes over the admin's master, which
# would otherwise answer as limaadmin no matter which user is asked for.
assert_fails "ssh as the agent user with Lima's admin key is refused" \
    ssh -F "$(limactl list "$INSTANCE" --format '{{.SSHConfigFile}}')" -o ControlPath=none -o BatchMode=yes \
    -o User="$AGENT_USER" "lima-$INSTANCE" true

if [ "$(adm stat -c '%U %a' "/home/$AGENT_USER/.ssh/authorized_keys")" = "$AGENT_USER 600" ] && [ "$(adm wc -l < "/home/$AGENT_USER/.ssh/authorized_keys")" = "1" ]; then
    pass "authorized_keys is agent-owned, 0600, one line"
else
    fail "authorized_keys is agent-owned, 0600, one line"
fi
# sshd's StrictModes rejects a .ssh with a group or other write bit (2, 3, 6, 7).
if adm stat -c '%a' "/home/$AGENT_USER/.ssh" | grep -Eq '^7[2367].$|^7.[2367]$'; then
    fail ".ssh is not group- or world-writable"
else
    pass ".ssh is not group- or world-writable"
fi

# An agent-planted extra key must not survive ssh-target.
agent bash -c "echo 'ssh-rsa AAAAPLANTED planted' >> ~/.ssh/authorized_keys"
"${CODE_VM_ARGS[@]}" ssh-target > /dev/null 2>&1
if [ "$(adm wc -l < "/home/$AGENT_USER/.ssh/authorized_keys")" = "1" ]; then
    pass "ssh-target restores the single host-issued key over a planted one"
else
    fail "ssh-target restores the single host-issued key over a planted one"
fi

if agent_ssh cat /proc/self/cgroup | grep -q "/user.slice/user-$(id -u).slice/"; then
    pass "an SSH login runs inside the agent's resource slice"
else
    fail "an SSH login runs inside the agent's resource slice"
fi

LOCK_BEFORE=$(adm sha256sum "/home/$AGENT_USER/.claude/settings.json" "/home/$AGENT_USER/.claude/settings.local.json")
agent_ssh claude --version > /dev/null 2>&1 || true
LOCK_AFTER=$(adm sha256sum "/home/$AGENT_USER/.claude/settings.json" "/home/$AGENT_USER/.claude/settings.local.json")
if [ "$LOCK_BEFORE" = "$LOCK_AFTER" ]; then
    pass "an SSH login and claude launch leave the locked settings untouched"
else
    fail "an SSH login and claude launch leave the locked settings untouched"
fi
if agent_ssh env | grep -q '^CLAUDE_CONFIG_DIR='; then
    fail "no Claude config-directory override reaches an SSH login"
else
    pass "no Claude config-directory override reaches an SSH login"
fi
```

  `assert_ok`/`assert_fails` run `"$@"` in the suite's own shell, so `agent_ssh` can be passed to them directly (`assert_ok "..." agent_ssh true`); it is only unavailable inside a `bash -c` string.

- "Restart hygiene": replace the first `"${CODE_VM_ARGS[@]}" start > /dev/null 2>&1` (after `yq -i '.swap = "0B"'`) with:

```bash
# The boot that follows doubles as the regression test for the Orca
# incident: Lima's host agent used to inherit the booting process's cgroup,
# so stopping that scope (an Orca workspace delete) killed the VM. Boot
# from inside a throwaway scope, stop the scope, and the VM must survive.
if [ "$(uname)" = Linux ] && systemctl --user is-system-running > /dev/null 2>&1; then
    BOOT_SCOPE="code-vm-suite-$$"
    systemd-run --user --scope --quiet --collect --unit="$BOOT_SCOPE" -- "${CODE_VM_ARGS[@]}" start > /dev/null 2>&1
    systemctl --user stop "$BOOT_SCOPE.scope" 2> /dev/null || true
    sleep 2
    if [ "$(limactl list "$INSTANCE" --format '{{.Status}}')" = "Running" ]; then
        pass "the VM survives its booting scope being stopped"
    else
        fail "the VM survives its booting scope being stopped"
        "${CODE_VM_ARGS[@]}" start > /dev/null 2>&1
    fi
else
    echo "  SKIP: the scope-survival check needs a Linux systemd user manager"
    "${CODE_VM_ARGS[@]}" start > /dev/null 2>&1
fi
```

  and after the restart's existing assertions add `assert_ok "the pinned SSH port is stable across a restart" agent_ssh true`.

- [ ] **Step 3: Lint the shell**

Run: `mise run lint && mise run fmt-check`
Expected: shellcheck and shfmt clean.

- [ ] **Step 4: Run the integration suite**

Run: `mise run build && mise run test:vm`
Expected: every new assertion passes; `TasksMax=8192`; the scope-survival check passes on this host (systemd 262 with a user manager). Takes 15–20 minutes and needs KVM; it uses the `code-sandbox-test` instance and port 60122, so the daily VM can keep running.

- [ ] **Step 5: Commit**

```bash
git add internal/guest/files/scripts/provision-system.sh test-vm-sandbox.sh
git commit -m "test: cover agent SSH access and scope survival; raise the agent slice task cap"
```

---

### Task 8: Final verification and handoff

- [ ] **Step 1: Full local gate**

Run: `mise run fmt-check && mise run lint && mise run test:unit && mise run build`
Expected: all green.

- [ ] **Step 2: Manual check against the daily VM** (Simon's machine only)

1. Set `sshPort: 60022` in `~/.config/code-vm/config.yaml`, `code-vm stop && code-vm start`.
2. `code-vm ssh-target` prints port 60022 and the key path; `ssh -i <key> -p 60022 -o IdentitiesOnly=yes devuser@127.0.0.1 id -un` → `devuser`.
3. `systemctl --user status` shows a `run-r….scope` described "code-vm VM boot (code-sandbox)" holding `limactl hostagent` and `qemu-system-x86_64`.
4. In Orca: add the target, add the project at its mounted path, create a workspace, pick Claude; the agent state shows in the sidebar.

- [ ] **Step 3: Update the spec status and finish the branch**

Set the spec's status line to `Implemented` and commit with `docs: mark the Orca SSH access design implemented`. Then use `superpowers:finishing-a-development-branch`.
