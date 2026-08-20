package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wetransform/code-vm/internal/lima"
)

// makeGitProfile creates a local git repo laid out as a valid profile and
// returns its path, usable as a clone URL.
func makeGitProfile(t *testing.T) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "team-fish")
	if err := os.MkdirAll(filepath.Join(src, "files"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "profile.yaml"), []byte("description: fixture\npackages: [fish]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "files", "marker"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"}, {"add", "."},
		{"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "v1"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = src
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return src
}

// makeBrokenGitProfile creates a local git repo laid out as a profile whose
// manifest fails validation (a package name that isn't Debian-shaped).
func makeBrokenGitProfile(t *testing.T) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "broken-profile")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "profile.yaml"), []byte("description: bad\npackages: [Not_Valid]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"}, {"add", "."},
		{"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "v1"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = src
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return src
}

// writeProfile hand-writes a profile directory directly under profilesRoot,
// without going through `profile add` (which requires a git repo).
func writeProfile(t *testing.T, profilesRoot, name, manifest string) {
	t.Helper()
	dir := filepath.Join(profilesRoot, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "profile.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
}

// withScratchConfig points the package configPath var at a scratch config
// file for the duration of the test and returns the directory containing it
// (profiles live under dir/profiles).
//
// Must be called *after* NewRootCmd(): registering the --config flag runs
// pflag's StringVar, which resets the bound variable to its default ("")
// at registration time. Calling this first would have that reset clobber
// the scratch path right back to empty, silently pointing every command at
// the real ~/.config/code-vm instead of the test fixture.
func withScratchConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir() // config dir; profiles live under dir/profiles
	// projectsRoot must be a separate tree: loadConfig refuses any mount
	// overlapping the profiles directory (MountsExcludeTree).
	projects := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte("instance: code-sandbox-clitest\nprojectsRoot: "+projects+"\ncpus: 1\nmemory: 1GiB\ndisk: 10GiB\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := configPath
	configPath = cfg
	t.Cleanup(func() { configPath = old })
	return dir
}

// appendConfig appends raw YAML to the scratch config file written by
// withScratchConfig, the same read-append-rewrite dance several tests below
// already did inline for activating profiles or setting vars.
func appendConfig(t *testing.T, extra string) {
	t.Helper()
	cfg := configPath
	base, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, append(base, []byte(extra)...), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestProfileAddClonesAndValidates(t *testing.T) {
	root := NewRootCmd()
	dir := withScratchConfig(t)
	src := makeGitProfile(t)

	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"profile", "add", src})
	if err := root.Execute(); err != nil {
		t.Fatalf("profile add: %v\n%s", err, out.String())
	}

	dst := filepath.Join(dir, "profiles", "team-fish")
	if info, err := os.Stat(dst); err != nil || !info.IsDir() {
		t.Fatalf("expected cloned profile at %s, err=%v", dst, err)
	}

	got := out.String()
	if !strings.Contains(got, "trusting its author") {
		t.Errorf("expected the trust warning in output, got:\n%s", got)
	}
	if !strings.Contains(got, "profiles:") {
		t.Errorf("expected the activation hint in output, got:\n%s", got)
	}
}

// makeGitProfileWithSecret creates a local git repo laid out as a valid
// profile that declares one secret and references it from a template, so
// `profile add` has something to warn about.
func makeGitProfileWithSecret(t *testing.T) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "secret-profile")
	if err := os.MkdirAll(filepath.Join(src, "templates"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "profile.yaml"), []byte(
		"description: fixture with a secret\nsecrets:\n  api-token:\n    description: an API token\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "templates", ".netrc"), []byte("${secret:api-token}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"}, {"add", "."},
		{"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "v1"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = src
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return src
}

func TestProfileAddWarnsAboutDeclaredSecrets(t *testing.T) {
	root := NewRootCmd()
	_ = withScratchConfig(t)
	src := makeGitProfileWithSecret(t)

	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"profile", "add", src})
	if err := root.Execute(); err != nil {
		t.Fatalf("profile add: %v\n%s", err, out.String())
	}

	got := out.String()
	if !strings.Contains(got, "declares secrets") || !strings.Contains(got, "api-token") {
		t.Errorf("expected a warning naming the declared secret, got:\n%s", got)
	}
	if !strings.Contains(got, "secrets.yaml") {
		t.Errorf("expected the warning to point at secrets.yaml, got:\n%s", got)
	}
}

func TestProfileAddRejectsInvalidBundle(t *testing.T) {
	root := NewRootCmd()
	dir := withScratchConfig(t)
	src := makeBrokenGitProfile(t)

	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"profile", "add", src})
	if err := root.Execute(); err == nil {
		t.Fatalf("expected an error for an invalid profile bundle; output:\n%s", out.String())
	}

	dst := filepath.Join(dir, "profiles", "broken-profile")
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("expected the cloned directory to be removed again, stat err = %v", err)
	}
}

func TestProfileAddRefusesExisting(t *testing.T) {
	// A single root, reused for both invocations: constructing a fresh
	// NewRootCmd() re-registers the --config flag, which resets configPath
	// to "" (see withScratchConfig's comment) and would silently point the
	// second add at the real ~/.config/code-vm instead of the fixture.
	root := NewRootCmd()
	_ = withScratchConfig(t)
	src := makeGitProfile(t)

	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"profile", "add", src})
	if err := root.Execute(); err != nil {
		t.Fatalf("first add: %v\n%s", err, out.String())
	}

	out.Reset()
	root.SetArgs([]string{"profile", "add", src})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected a second add of the same name to fail")
	}
	if !strings.Contains(err.Error(), "profile update") {
		t.Errorf("expected the error to mention `profile update`, got: %v", err)
	}
}

func TestProfileListShowsStatus(t *testing.T) {
	root := NewRootCmd()
	dir := withScratchConfig(t)
	profilesRoot := filepath.Join(dir, "profiles")
	writeProfile(t, profilesRoot, "alpha", "description: active one\npackages: [fish]\n")
	writeProfile(t, profilesRoot, "beta", "description: inactive one\npackages: [fish]\n")
	writeProfile(t, profilesRoot, "gamma", "description: broken\npackages: [Not_Valid]\n")
	writeProfile(t, profilesRoot, "delta", "description: has a secret\nsecrets:\n  a:\n    description: d\n")

	cfg := filepath.Join(dir, "config.yaml")
	base, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	activated := append(append([]byte{}, base...), []byte("profiles:\n  - alpha\n")...)
	if err := os.WriteFile(cfg, activated, 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"profile", "list"})
	if err := root.Execute(); err != nil {
		t.Fatalf("profile list: %v\n%s", err, out.String())
	}

	// Parse per-line rather than substring-matching the whole output: "active"
	// is itself a substring of "inactive".
	states := map[string]string{}
	lines := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		states[fields[0]] = fields[1]
		lines[fields[0]] = line
	}
	if states["alpha"] != "active" {
		t.Errorf("alpha state = %q, want %q; full output:\n%s", states["alpha"], "active", out.String())
	}
	if states["beta"] != "inactive" {
		t.Errorf("beta state = %q, want %q; full output:\n%s", states["beta"], "inactive", out.String())
	}
	if states["gamma"] != "invalid" {
		t.Errorf("gamma state = %q, want %q; full output:\n%s", states["gamma"], "invalid", out.String())
	}
	if !strings.Contains(lines["delta"], "[secrets: a]") {
		t.Errorf("delta row should carry a secrets marker, got: %q", lines["delta"])
	}
	if strings.Contains(lines["alpha"], "[secrets:") {
		t.Errorf("alpha row should not carry a secrets marker, got: %q", lines["alpha"])
	}
}

// The design spec's CLI section says `profile list` shows "git origin if
// any". A hand-authored profile (no .git) must show "local"; a git-cloned
// one must show its origin URL (here, the local path it was cloned from,
// since makeGitProfile uses a filesystem remote).
func TestProfileListShowsGitOrigin(t *testing.T) {
	root := NewRootCmd()
	dir := withScratchConfig(t)
	profilesRoot := filepath.Join(dir, "profiles")
	writeProfile(t, profilesRoot, "handmade", "description: local\npackages: [fish]\n")

	src := makeGitProfile(t)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"profile", "add", src})
	if err := root.Execute(); err != nil {
		t.Fatalf("profile add: %v\n%s", err, out.String())
	}

	out.Reset()
	root.SetArgs([]string{"profile", "list"})
	if err := root.Execute(); err != nil {
		t.Fatalf("profile list: %v\n%s", err, out.String())
	}

	lines := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 1 {
			continue
		}
		lines[fields[0]] = line
	}
	if !strings.Contains(lines["handmade"], "local") {
		t.Errorf("hand-made profile row should show \"local\", got: %q", lines["handmade"])
	}
	if !strings.Contains(lines["team-fish"], src) {
		t.Errorf("git-cloned profile row should show its origin %q, got: %q", src, lines["team-fish"])
	}
}

func TestProfileRemoveRefusesActive(t *testing.T) {
	root := NewRootCmd() // one root, reused for all three invocations
	dir := withScratchConfig(t)
	src := makeGitProfile(t)

	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"profile", "add", src})
	if err := root.Execute(); err != nil {
		t.Fatalf("add: %v\n%s", err, out.String())
	}

	cfg := filepath.Join(dir, "config.yaml")
	base, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	activated := append(append([]byte{}, base...), []byte("profiles:\n  - team-fish\n")...)
	if err := os.WriteFile(cfg, activated, 0o600); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	root.SetArgs([]string{"profile", "remove", "team-fish"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected remove to refuse a profile listed as active in the config")
	}

	dst := filepath.Join(dir, "profiles", "team-fish")
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("profile directory should still exist after a refused remove: %v", err)
	}

	// Deactivate it, then remove should succeed and delete the directory.
	if err := os.WriteFile(cfg, base, 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	root.SetArgs([]string{"profile", "remove", "team-fish"})
	if err := root.Execute(); err != nil {
		t.Fatalf("remove after deactivation: %v\n%s", err, out.String())
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("expected the profile directory to be removed, stat err = %v", err)
	}
}

func TestProfileUpdatePullsAndSkipsNonGit(t *testing.T) {
	root := NewRootCmd() // one root, reused for the add and the update
	dir := withScratchConfig(t)
	src := makeGitProfile(t)

	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"profile", "add", src})
	if err := root.Execute(); err != nil {
		t.Fatalf("add: %v\n%s", err, out.String())
	}

	// Commit a v2 marker in the source repo for update to pull.
	if err := os.WriteFile(filepath.Join(src, "files", "marker"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "."},
		{"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "v2"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = src
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// A hand-made, non-git profile directory must be skipped, not fail the run.
	profilesRoot := filepath.Join(dir, "profiles")
	writeProfile(t, profilesRoot, "manual", "description: manual\npackages: [fish]\n")

	out.Reset()
	root.SetArgs([]string{"profile", "update"})
	if err := root.Execute(); err != nil {
		t.Fatalf("update: %v\n%s", err, out.String())
	}

	got := out.String()
	if !strings.Contains(got, "manual") || !strings.Contains(got, "skipped") {
		t.Errorf("expected the hand-made profile noted as skipped, got:\n%s", got)
	}

	marker, err := os.ReadFile(filepath.Join(profilesRoot, "team-fish", "files", "marker"))
	if err != nil {
		t.Fatal(err)
	}
	if string(marker) != "v2\n" {
		t.Errorf("marker = %q, want %q after update", marker, "v2\n")
	}
}

// A directory under the profiles root can end up with a name ValidateName
// rejects (hand-created, left over from something else) without that being
// anyone's "explicit input" in the traversal sense: update-all must skip it
// and keep going, not abort the batch the way an explicit bad argument does.
func TestProfileUpdateAllSkipsNonConformingDirectoryName(t *testing.T) {
	root := NewRootCmd() // one root, reused for the add and the update
	dir := withScratchConfig(t)
	src := makeGitProfile(t)

	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"profile", "add", src})
	if err := root.Execute(); err != nil {
		t.Fatalf("add: %v\n%s", err, out.String())
	}

	profilesRoot := filepath.Join(dir, "profiles")
	badDir := filepath.Join(profilesRoot, "bad_name!")
	if err := os.MkdirAll(badDir, 0o755); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	root.SetArgs([]string{"profile", "update"})
	if err := root.Execute(); err != nil {
		t.Fatalf("update-all must not abort for a non-conforming directory name: %v\n%s", err, out.String())
	}

	got := out.String()
	if !strings.Contains(got, "bad_name!") || !strings.Contains(got, "skipped") {
		t.Errorf("expected the non-conforming directory noted as skipped, got:\n%s", got)
	}
	if !strings.Contains(got, "team-fish") || !strings.Contains(got, "updated") {
		t.Errorf("expected the valid profile to still be updated, got:\n%s", got)
	}
}

// A profile name is joined straight into filesystem paths (add's clone
// destination, update's pull directory, remove's RemoveAll target), so a
// traversal-shaped name must be rejected before any of that happens.

func TestProfileAddRejectsTraversalName(t *testing.T) {
	root := NewRootCmd()
	dir := withScratchConfig(t)
	src := makeGitProfile(t)

	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"profile", "add", src, "../../escape"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected add to reject a traversal-shaped name")
	}
	if !strings.Contains(err.Error(), "profile name must look like") {
		t.Errorf("expected the name-validation error, got: %v", err)
	}

	// Nothing should have been created: validation runs before profilesDir()
	// is even touched, let alone the clone destination.
	profilesRoot := filepath.Join(dir, "profiles")
	if _, statErr := os.Stat(profilesRoot); !os.IsNotExist(statErr) {
		t.Errorf("expected no profiles directory to be created, stat err = %v", statErr)
	}
	escaped := filepath.Clean(filepath.Join(profilesRoot, "../../escape"))
	if _, statErr := os.Stat(escaped); !os.IsNotExist(statErr) {
		t.Errorf("expected nothing created at %s, stat err = %v", escaped, statErr)
	}
}

func TestProfileUpdateRejectsTraversalName(t *testing.T) {
	root := NewRootCmd() // one root, reused for the add and the update
	dir := withScratchConfig(t)
	src := makeGitProfile(t)

	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"profile", "add", src})
	if err := root.Execute(); err != nil {
		t.Fatalf("add: %v\n%s", err, out.String())
	}

	out.Reset()
	root.SetArgs([]string{"profile", "update", "../../escape"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected update to reject a traversal-shaped name")
	}
	if !strings.Contains(err.Error(), "profile name must look like") {
		t.Errorf("expected the name-validation error, got: %v", err)
	}

	// The legitimate profile must be untouched: names are validated before
	// the per-name loop runs `git pull` against anything.
	marker, err := os.ReadFile(filepath.Join(dir, "profiles", "team-fish", "files", "marker"))
	if err != nil {
		t.Fatal(err)
	}
	if string(marker) != "v1\n" {
		t.Errorf("marker = %q, want %q (unchanged: reject must happen before any pull)", marker, "v1\n")
	}
}

func TestProfileRemoveRejectsTraversalName(t *testing.T) {
	root := NewRootCmd()
	dir := withScratchConfig(t)

	// A sibling directory outside the profiles tree, standing in for the
	// arbitrary host path a traversal-shaped name could otherwise reach via
	// os.RemoveAll.
	victim := filepath.Join(dir, "victim")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(victim, "keepme"), []byte("do not delete\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"profile", "remove", "../victim"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected remove to reject a traversal-shaped name")
	}
	if !strings.Contains(err.Error(), "profile name must look like") {
		t.Errorf("expected the name-validation error, got: %v", err)
	}

	if _, err := os.Stat(filepath.Join(victim, "keepme")); err != nil {
		t.Errorf("victim directory should have survived the rejected remove, stat err = %v", err)
	}
}

// installFakeClient substitutes the package's limactl client with a recorder.
func installFakeClient(t *testing.T, status string) *recordingRunner {
	t.Helper()
	r := &recordingRunner{statusOut: status}
	old := newClient
	newClient = func() lima.Client { return lima.Client{R: r} }
	t.Cleanup(func() { newClient = old })
	return r
}

func ranAny(calls [][]string, substr string) bool {
	for _, c := range calls {
		if strings.Contains(strings.Join(c, " "), substr) {
			return true
		}
	}
	return false
}

func TestProfileApplyPushesAndRuns(t *testing.T) {
	root := NewRootCmd()
	dir := withScratchConfig(t)
	pdir := filepath.Join(dir, "profiles", "p")
	if err := os.MkdirAll(filepath.Join(pdir, "files"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pdir, "profile.yaml"), []byte("domains: [example.net]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pdir, "files", "marker"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Activate it: append to the scratch config written by withScratchConfig.
	cfg := configPath
	b, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, append(b, []byte("profiles:\n  - p\n")...), 0o600); err != nil {
		t.Fatal(err)
	}

	r := installFakeClient(t, "Running")
	root.SetArgs([]string{"profile", "apply"})
	if err := root.Execute(); err != nil {
		t.Fatalf("profile apply: %v", err)
	}
	for _, want := range []string{
		"rm -rf /usr/local/share/sandbox-profiles",
		"install -d -m 0755 /usr/local/share/sandbox-profiles",
		"env SANDBOX_PROFILES_STRICT=1 /usr/local/lib/sandbox/apply-profiles.sh",
	} {
		if !ranAny(r.calls, want) {
			t.Errorf("missing guest command %q in %v", want, r.calls)
		}
	}
	// manifest.env + marker + files.list pushed, plus the allowlist fragment:
	// each travels through one `limactl copy`. Content is staged via host temp
	// files, so the domain itself is asserted at the session layer, not here.
	copies := 0
	for _, c := range r.calls {
		if len(c) > 0 && c[0] == "copy" {
			copies++
		}
	}
	if copies != 4 {
		t.Errorf("copies = %d, want 4 (manifest.env, marker, files.list, allowlist fragment)", copies)
	}
}

// Resolution (LoadSecrets/ResolveSecrets/ResolveVars) needs nothing from the
// guest, so an unmapped secret must fail apply before PushProfiles stages a
// new profile tree or ApplyAllowlist makes its domains live — otherwise a
// failed apply leaves a half-applied profile (new tree, live domains, no
// templates) whose hooks still run on the next boot.
func TestProfileApplyFailsBeforeTouchingGuestOnUnmappedSecret(t *testing.T) {
	root := NewRootCmd()
	dir := withScratchConfig(t)
	pdir := filepath.Join(dir, "profiles", "p")
	if err := os.MkdirAll(filepath.Join(pdir, "templates"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pdir, "profile.yaml"),
		[]byte("secrets:\n  tok:\n    suggest: gopass show -o t\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pdir, "templates", ".npmrc"), []byte("${secret:tok}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := configPath
	b, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, append(b, []byte("profiles:\n  - p\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	// No secrets.yaml is written at all: "tok" is unmapped.

	r := installFakeClient(t, "Running")
	root.SetArgs([]string{"profile", "apply"})
	err = root.Execute()
	if err == nil || !strings.Contains(err.Error(), "gopass show -o t") {
		t.Fatalf("profile apply = %v, want an unmapped-secret error with the suggest snippet", err)
	}
	// "status" is the only guest call resolution's own precondition check
	// makes; nothing that mutates guest state (push, rm -rf, allowlist) may
	// have run.
	for _, unwanted := range []string{
		"rm -rf /usr/local/share/sandbox-profiles",
		"install -d -m 0755 /usr/local/share/sandbox-profiles",
		"squid -k reconfigure",
		"apply-profiles.sh",
	} {
		if ranAny(r.calls, unwanted) {
			t.Errorf("guest call %q must not happen before resolution succeeds, calls=%v", unwanted, r.calls)
		}
	}
	if copies := func() int {
		n := 0
		for _, c := range r.calls {
			if len(c) > 0 && c[0] == "copy" {
				n++
			}
		}
		return n
	}(); copies != 0 {
		t.Errorf("no file may be staged into the guest before resolution succeeds, copies=%d calls=%v", copies, r.calls)
	}
}

func TestProfileApplyRequiresRunningVM(t *testing.T) {
	root := NewRootCmd()
	withScratchConfig(t)
	installFakeClient(t, "Stopped")
	root.SetArgs([]string{"profile", "apply"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("apply on a stopped VM = %v, want a not-running error", err)
	}
}
