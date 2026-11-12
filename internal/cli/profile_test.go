package cli

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/wetransform/code-vm/internal/config"
	"github.com/wetransform/code-vm/internal/lima"
	"github.com/wetransform/code-vm/internal/profile"
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
	if !strings.Contains(got, "manual") || !strings.Contains(got, "sources only") {
		t.Errorf("expected the hand-made profile noted as sources only, got:\n%s", got)
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
	newClient = func(string) lima.Client { return lima.Client{R: r} }
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
		// The recording runner answers the hash probe with its status
		// string, so the applier never matches and is swapped in.
		"mv -T \"$4\" \"$1\"",
		"env SANDBOX_PROFILES_STRICT=1 /usr/local/lib/sandbox/apply-profiles.sh",
	} {
		if !ranAny(r.calls, want) {
			t.Errorf("missing guest command %q in %v", want, r.calls)
		}
	}
	// manifest.env + marker + files.list pushed, plus the allowlist fragment
	// and the reinstalled applier: each travels through one `limactl copy`.
	// Content is staged via host temp files, so the domain itself is
	// asserted at the session layer, not here.
	copies := 0
	for _, c := range r.calls {
		if len(c) > 0 && c[0] == "copy" {
			copies++
		}
	}
	if copies != 5 {
		t.Errorf("copies = %d, want 5 (manifest.env, marker, files.list, allowlist fragment, applier)", copies)
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

// Lima reports Running before sandbox-boot.sh has finished provisioning the
// profile tree and running the applier; apply must refuse on the missing
// boot marker before its first guest write, or PushProfiles would wipe the
// tree the boot is iterating and trip the boot's error trap.
func TestProfileApplyWaitsForBootBeforeTouchingGuest(t *testing.T) {
	root := NewRootCmd()
	dir := withScratchConfig(t)
	writeProfile(t, filepath.Join(dir, "profiles"), "p", "domains: [example.net]\n")
	appendConfig(t, "profiles:\n  - p\n")

	r := installFakeClient(t, "Running")
	r.failOn = "test -f /run/sandbox-boot-complete"
	root.SetArgs([]string{"profile", "apply"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "not finished booting") {
		t.Fatalf("expected a boot-in-progress refusal, got %v", err)
	}
	for _, unwanted := range []string{
		"rm -rf /usr/local/share/sandbox-profiles",
		"squid -k reconfigure",
		"apply-profiles.sh",
		"copy",
	} {
		if ranAny(r.calls, unwanted) {
			t.Errorf("guest call %q must not happen while the boot is in progress, calls=%v", unwanted, r.calls)
		}
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

// gitCLI runs git in dir and returns its trimmed output, failing the test on
// error. The fixed identity and disabled signing keep fixtures independent
// of the host's git config.
func gitCLI(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir,
		"-c", "user.email=t@t", "-c", "user.name=t",
		"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// makeSourceRepoCLI mirrors makeSourceRepo in internal/profile (test helpers
// do not cross packages): SKILL.md, an executable bin/run, an empty file, a
// symlink link -> SKILL.md, and secret.txt marked export-ignore, committed
// on main and tagged v1. It returns the repo path, usable as a clone URL.
func makeSourceRepoCLI(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitCLI(t, dir, "init", "-q")
	for rel, content := range map[string]string{
		"SKILL.md":       "# skill\n",
		"bin/run":        "#!/bin/sh\necho run\n",
		"empty":          "",
		"secret.txt":     "do not ship\n",
		".gitattributes": "secret.txt export-ignore\n",
	} {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(dir, "bin", "run"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("SKILL.md", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	gitCLI(t, dir, "add", "-A")
	gitCLI(t, dir, "commit", "-q", "-m", "init")
	gitCLI(t, dir, "branch", "-M", "main")
	gitCLI(t, dir, "tag", "v1")
	return dir
}

// commitSource adds a commit to the source repo at dir and returns its id.
func commitSource(t *testing.T, dir, msg string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("# "+msg+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCLI(t, dir, "commit", "-q", "-am", msg)
	return gitCLI(t, dir, "rev-parse", "HEAD")
}

// makeGitProfileWithSource creates a git repo laid out as a profile whose
// manifest declares one source pulled from repo into .claude/skills/s.
func makeGitProfileWithSource(t *testing.T, repo string) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "with-source")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "description: fixture with a source\nsources:\n  - repo: " + repo + "\n    into: .claude/skills/s\n"
	if err := os.WriteFile(filepath.Join(src, "profile.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCLI(t, src, "init", "-q")
	gitCLI(t, src, "add", ".")
	gitCLI(t, src, "commit", "-q", "-m", "v1")
	return src
}

// executor is the part of a cobra root runRoot drives.
type executor interface {
	SetOut(io.Writer)
	SetErr(io.Writer)
	SetArgs([]string)
	Execute() error
}

// runRoot executes root with args, capturing stdout and stderr together.
func runRoot(root executor, args ...string) (string, error) {
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestProfileAddIntoGeneratesWrapperAndSyncs(t *testing.T) {
	root := NewRootCmd()
	dir := withScratchConfig(t)
	src := makeSourceRepoCLI(t)

	out, err := runRoot(root, "profile", "add", src, "--into", ".claude/skills/s", "--ref", "main", "wrap")
	if err != nil {
		t.Fatalf("profile add --into: %v\n%s", err, out)
	}
	manifest, err := os.ReadFile(filepath.Join(dir, "profiles", "wrap", "profile.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`into: ".claude/skills/s"`, `ref: "main"`} {
		if !strings.Contains(string(manifest), want) {
			t.Errorf("wrapper manifest lacks %q:\n%s", want, manifest)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "sources", "wrap", "s", "HEAD")); err != nil {
		t.Errorf("expected a synced checkout: %v", err)
	}
	if !strings.Contains(out, "trusting its author") || !strings.Contains(out, "sources are pulled") {
		t.Errorf("expected the trust warning with the sources sentence, got:\n%s", out)
	}
}

func TestProfileAddRefRequiresInto(t *testing.T) {
	root := NewRootCmd()
	dir := withScratchConfig(t)
	src := makeSourceRepoCLI(t)

	if out, err := runRoot(root, "profile", "add", src, "--ref", "main", "wrap"); err == nil {
		t.Fatalf("expected --ref without --into to fail; output:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "profiles", "wrap")); !os.IsNotExist(err) {
		t.Errorf("nothing should be written on a usage error, stat err = %v", err)
	}
}

// A YAML keyword as --ref must stay the literal ref: written bare, `null`
// would decode as no ref and silently unpin the source.
func TestProfileAddIntoKeepsYAMLKeywordRef(t *testing.T) {
	root := NewRootCmd()
	dir := withScratchConfig(t)
	src := makeSourceRepoCLI(t)

	// No such ref exists, so the sync fails; the profile is installed anyway.
	if out, err := runRoot(root, "profile", "add", src, "--into", "tools/s", "--ref", "null", "wrap"); err != nil {
		t.Fatalf("profile add: %v\n%s", err, out)
	}
	m, err := profile.LoadManifest(filepath.Join(dir, "profiles"), "wrap")
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Sources[0].Ref; got != "null" {
		t.Errorf("Ref = %q, want the literal ref null", got)
	}
}

// --into is validated by the manifest rules once written; an invalid value
// leaves nothing behind.
func TestProfileAddIntoRejectsInvalidInto(t *testing.T) {
	root := NewRootCmd()
	dir := withScratchConfig(t)
	src := makeSourceRepoCLI(t)

	out, err := runRoot(root, "profile", "add", src, "--into", "tools/s\": [x # y", "wrap")
	if err == nil || !strings.Contains(err.Error(), "only [a-zA-Z0-9._/-] is allowed") {
		t.Fatalf("expected the manifest's into rule to reject the value, got %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "profiles", "wrap")); !os.IsNotExist(err) {
		t.Errorf("an invalid wrapper must be removed again, stat err = %v", err)
	}
}

func TestProfileAddSyncsSourcesOfRealProfile(t *testing.T) {
	root := NewRootCmd()
	dir := withScratchConfig(t)
	src := makeGitProfileWithSource(t, makeSourceRepoCLI(t))

	out, err := runRoot(root, "profile", "add", src)
	if err != nil {
		t.Fatalf("profile add: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "sources", "with-source", "s", "HEAD")); err != nil {
		t.Errorf("expected the declared source to be synced: %v\n%s", err, out)
	}
	if !strings.Contains(out, "sources are pulled") {
		t.Errorf("expected the sources sentence in the trust warning, got:\n%s", out)
	}
}

// An unreachable source must not block installing the rest of the profile:
// it stays unavailable until profile update succeeds.
func TestProfileAddUnreachableSourceStillInstalls(t *testing.T) {
	root := NewRootCmd()
	dir := withScratchConfig(t)
	src := makeGitProfileWithSource(t, filepath.Join(t.TempDir(), "missing"))

	out, err := runRoot(root, "profile", "add", src)
	if err != nil {
		t.Fatalf("profile add with an unreachable source: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "profiles", "with-source")); err != nil {
		t.Errorf("expected the profile to be installed: %v", err)
	}
	if !strings.Contains(out, "stays unavailable") {
		t.Errorf("expected the failed source to be reported, got:\n%s", out)
	}
}

func TestProfileUpdateSyncsPrunesAndReportsFailures(t *testing.T) {
	root := NewRootCmd() // one root, reused for every invocation
	dir := withScratchConfig(t)
	profilesRoot := filepath.Join(dir, "profiles")
	src := makeSourceRepoCLI(t)

	if out, err := runRoot(root, "profile", "add", src, "--into", ".claude/skills/s", "wrap"); err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}

	c2 := commitSource(t, src, "c2")
	out, err := runRoot(root, "profile", "update")
	if err != nil {
		t.Fatalf("update: %v\n%s", err, out)
	}
	if !strings.Contains(out, "sources only") {
		t.Errorf("expected the wrapper noted as sources only, got:\n%s", out)
	}
	p, err := profile.Load(profilesRoot, "wrap")
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Sources[0].Commit; got != c2 {
		t.Errorf("commit after update = %s, want %s", got, c2)
	}

	// Moving the source renames it: the old checkout is pruned and the new
	// one is created.
	writeProfile(t, profilesRoot, "wrap", "description: wrapper\nsources:\n  - repo: "+src+"\n    into: other/t\n")
	if out, err := runRoot(root, "profile", "update", "wrap"); err != nil {
		t.Fatalf("update after rename: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "sources", "wrap", "s")); !os.IsNotExist(err) {
		t.Errorf("expected sources/wrap/s pruned, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sources", "wrap", "t", "HEAD")); err != nil {
		t.Errorf("expected sources/wrap/t synced: %v", err)
	}

	// An unreachable repo fails the update but never discards the checkout.
	writeProfile(t, profilesRoot, "wrap", "description: wrapper\nsources:\n  - repo: "+filepath.Join(t.TempDir(), "missing")+"\n    into: other/t\n")
	out, err = runRoot(root, "profile", "update")
	if err == nil {
		t.Fatalf("expected update to fail on an unreachable source; output:\n%s", out)
	}
	if !strings.Contains(out, "could not be updated") {
		t.Errorf("expected a failure summary, got:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "sources", "wrap", "t", "HEAD")); err != nil {
		t.Errorf("a failed fetch must keep the checkout: %v", err)
	}
}

// Without a name, one invalid local profile (inactive, not a git clone)
// must not stop the others from updating; the run still exits non-zero.
func TestProfileUpdateAllContinuesPastInvalidProfile(t *testing.T) {
	root := NewRootCmd()
	dir := withScratchConfig(t)
	profilesRoot := filepath.Join(dir, "profiles")
	src := makeSourceRepoCLI(t)

	if out, err := runRoot(root, "profile", "add", src, "--into", ".claude/skills/s", "wrap"); err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	// Sorted before "wrap", so it is reached first.
	writeProfile(t, profilesRoot, "broken", "description: bad\npackages: [Not_Valid]\n")
	c2 := commitSource(t, src, "c2")

	out, err := runRoot(root, "profile", "update")
	if err == nil {
		t.Fatalf("expected update to fail on the invalid profile; output:\n%s", out)
	}
	if !strings.Contains(out, "broken: invalid: ") || strings.Contains(out, "invalid after update") {
		t.Errorf("expected the non-git profile reported as invalid (no pull happened), got:\n%s", out)
	}
	if !strings.Contains(out, "1 profile(s) invalid: broken") || !strings.Contains(err.Error(), "1 profile(s) invalid") {
		t.Errorf("expected an invalid-profile summary, got err %v and:\n%s", err, out)
	}
	if !strings.Contains(out, "wrap: updated") {
		t.Errorf("expected the valid profile still updated, got:\n%s", out)
	}
	p, err := profile.Load(profilesRoot, "wrap")
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Sources[0].Commit; got != c2 {
		t.Errorf("commit after update = %s, want %s", got, c2)
	}

	// Named explicitly, an invalid profile still aborts.
	out, err = runRoot(root, "profile", "update", "broken")
	if err == nil || !strings.Contains(err.Error(), "profile broken is invalid: ") {
		t.Errorf("expected a hard error for the named invalid profile, got %v\n%s", err, out)
	}
}

// A local source repository the agent can write (inside a shared directory)
// is refused before git ever runs in it; the same repository outside every
// mount syncs.
func TestProfileSourceRefusesRepoInsideSharedDirectory(t *testing.T) {
	root := NewRootCmd()
	dir := withScratchConfig(t)
	profilesRoot := filepath.Join(dir, "profiles")
	c, _, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(c.ProjectsRoot, "skills")
	if err := os.Rename(makeSourceRepoCLI(t), shared); err != nil {
		t.Fatal(err)
	}

	want := "repository path " + config.CanonicalizeExisting(shared) + " lies inside the shared directory " +
		config.CanonicalizeExisting(c.ProjectsRoot) + ", which the agent can write; " +
		"sources must come from a path or URL the agent cannot modify"
	const hint = "move the repository outside the shared directories or use a remote URL"

	// A wrapper's URL is the user's own argument: refused outright,
	// nothing written.
	for _, repo := range []string{shared, "file://" + shared} {
		out, err := runRoot(root, "profile", "add", repo, "--into", "tools/s", "wrap")
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), hint) {
			t.Errorf("add %s: err = %v, want the refusal with its hint\n%s", repo, err, out)
		}
		for _, p := range []string{filepath.Join(profilesRoot, "wrap"), filepath.Join(dir, "sources", "wrap")} {
			if _, err := os.Stat(p); !os.IsNotExist(err) {
				t.Errorf("add %s: nothing may be written for a refused wrapper, %s stat err = %v", repo, p, err)
			}
		}
	}

	// A bundle's source: the profile installs, the source is refused and
	// never synced, by add and by update. A fresh root: cobra keeps the
	// --into of the runs above, and registering its flags resets configPath.
	cfg := configPath
	root = NewRootCmd()
	configPath = cfg
	out, err := runRoot(root, "profile", "add", makeGitProfileWithSource(t, shared))
	if err != nil {
		t.Fatalf("profile add: %v\n%s", err, out)
	}
	if !strings.Contains(out, want) || !strings.Contains(out, hint) || strings.Contains(out, "until profile update succeeds") {
		t.Errorf("add: want the refusal with its hint, got:\n%s", out)
	}
	out, err = runRoot(root, "profile", "update", "with-source")
	if err == nil || !strings.Contains(out, want) {
		t.Errorf("update: err = %v, want the refusal in:\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "sources", "with-source")); !os.IsNotExist(err) {
		t.Errorf("no checkout may be created for a refused source, stat err = %v", err)
	}

	outside := filepath.Join(t.TempDir(), "skills")
	if err := os.Rename(shared, outside); err != nil {
		t.Fatal(err)
	}
	if out, err := runRoot(root, "profile", "add", outside, "--into", "tools/s", "wrap"); err != nil {
		t.Fatalf("profile add outside: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "sources", "wrap", "s", "HEAD")); err != nil {
		t.Errorf("a repository outside the mounts must sync: %v", err)
	}
	if out, err := runRoot(root, "profile", "update", "wrap"); err != nil {
		t.Errorf("update outside: %v\n%s", err, out)
	}
	if _, err := profile.Load(profilesRoot, "wrap"); err != nil {
		t.Fatal(err)
	}
}

func TestCheckRepo(t *testing.T) {
	base := t.TempDir()
	projects := filepath.Join(base, "projects")
	extra := filepath.Join(base, "extra")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{filepath.Join(projects, "sub"), filepath.Join(projects, "skills"), filepath.Join(extra, "x"), filepath.Join(outside, "ok")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// outside/link -> projects/sub: "link/../skills" is projects/skills to
	// the kernel, outside/skills to a lexical clean.
	if err := os.Symlink(filepath.Join(projects, "sub"), filepath.Join(outside, "link")); err != nil {
		t.Fatal(err)
	}
	// projects/out -> outside/ok: safe today, retargetable by the agent.
	if err := os.Symlink(filepath.Join(outside, "ok"), filepath.Join(projects, "out")); err != nil {
		t.Fatal(err)
	}
	c := config.Config{ProjectsRoot: projects, ExtraMounts: []string{extra}}
	for repo, want := range map[string]string{
		"https://example.invalid/r.git":                  "",
		"git@example.invalid:o/r.git":                    "",
		"file:x/r.git":                                   "",
		filepath.Join(outside, "ok"):                     "",
		"file://" + filepath.Join(outside, "ok"):         "",
		filepath.Join(projects, "skills"):                "lies inside the shared directory " + projects,
		filepath.Join(extra, "x"):                        "lies inside the shared directory " + extra,
		"file://" + base + "/pro%6aects/skills":          "lies inside the shared directory " + projects,
		"file://" + base + "/pro%zzects/skills":          "invalid URL escape",
		filepath.Join(outside, "link") + "/../skills":    `contains a ".." segment`,
		filepath.Join(projects, "out"):                   "lies inside the shared directory " + projects,
		"rel/path":                                       "is relative",
		"./ok":                                           "is relative",
		filepath.Join(outside, "missing"):                "does not resolve",
		"file://" + filepath.Join(projects, "nope", "r"): "does not resolve",
	} {
		err := checkRepo(c, repo, "sources")
		if want == "" {
			if err != nil {
				t.Errorf("checkRepo(%q) = %v, want accepted", repo, err)
			}
			continue
		}
		var r *repoRefusal
		if err == nil || !errors.As(err, &r) || !strings.Contains(err.Error(), want) {
			t.Errorf("checkRepo(%q) = %v, want a refusal containing %q", repo, err, want)
		}
	}
}

// The profile bundle itself is held to the same rule: add refuses a clone
// URL inside a shared directory, and update refuses to pull from an origin
// that is one.
func TestProfileBundleRefusesRepoInsideSharedDirectory(t *testing.T) {
	root := NewRootCmd()
	dir := withScratchConfig(t)
	profilesRoot := filepath.Join(dir, "profiles")
	c, _, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(c.ProjectsRoot, "team-fish")
	if err := os.Rename(makeGitProfile(t), shared); err != nil {
		t.Fatal(err)
	}
	want := "lies inside the shared directory " + config.CanonicalizeExisting(c.ProjectsRoot) +
		", which the agent can write; profiles must come from a path or URL the agent cannot modify"

	out, err := runRoot(root, "profile", "add", shared)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("add: err = %v, want the refusal\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(profilesRoot, "team-fish")); !os.IsNotExist(err) {
		t.Errorf("nothing may be cloned for a refused bundle, stat err = %v", err)
	}

	// A clone whose origin was later pointed into a shared directory.
	if out, err := runRoot(root, "profile", "add", makeGitProfile(t), "a-moved"); err != nil {
		t.Fatalf("add a-moved: %v\n%s", err, out)
	}
	if out, err := runRoot(root, "profile", "add", makeGitProfile(t), "b-kept"); err != nil {
		t.Fatalf("add b-kept: %v\n%s", err, out)
	}
	gitCLI(t, filepath.Join(profilesRoot, "a-moved"), "remote", "set-url", "origin", shared)
	out, err = runRoot(root, "profile", "update")
	if err == nil || !strings.Contains(err.Error(), "could not be pulled: a-moved") {
		t.Errorf("update: err = %v, want a-moved recorded as not pulled\n%s", err, out)
	}
	if !strings.Contains(out, "a-moved: pull failed: ") || !strings.Contains(out, want) || !strings.Contains(out, "b-kept: updated") {
		t.Errorf("update: want a-moved refused and b-kept updated, got:\n%s", out)
	}
	if out, err := runRoot(root, "profile", "update", "a-moved"); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("update a-moved: err = %v, want the refusal\n%s", err, out)
	}
}

// Without a name, a profile whose pull fails is reported and skipped; the
// others still update, and the run exits non-zero naming it.
func TestProfileUpdateAllContinuesPastFailedPull(t *testing.T) {
	root := NewRootCmd()
	dir := withScratchConfig(t)
	profilesRoot := filepath.Join(dir, "profiles")
	gone := makeGitProfile(t)
	kept := makeGitProfile(t)
	if out, err := runRoot(root, "profile", "add", gone, "a-gone"); err != nil {
		t.Fatalf("add a-gone: %v\n%s", err, out)
	}
	if out, err := runRoot(root, "profile", "add", kept, "b-kept"); err != nil {
		t.Fatalf("add b-kept: %v\n%s", err, out)
	}
	// Rewriting the only commit upstream makes the --ff-only pull fail.
	gitCLI(t, gone, "commit", "-q", "--amend", "-m", "rewritten")
	if err := os.WriteFile(filepath.Join(kept, "files", "marker"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCLI(t, kept, "commit", "-q", "-am", "v2")

	out, err := runRoot(root, "profile", "update")
	if err == nil || !strings.Contains(err.Error(), "1 profile(s) could not be pulled: a-gone") {
		t.Fatalf("err = %v, want one naming the unpulled a-gone\n%s", err, out)
	}
	if !strings.Contains(out, "a-gone: pull failed: ") || strings.Contains(out, "a-gone: updated") {
		t.Errorf("expected a-gone reported as not pulled and not updated, got:\n%s", out)
	}
	if !strings.Contains(out, "b-kept: updated") {
		t.Errorf("expected b-kept still updated, got:\n%s", out)
	}
	marker, err := os.ReadFile(filepath.Join(profilesRoot, "b-kept", "files", "marker"))
	if err != nil {
		t.Fatal(err)
	}
	if string(marker) != "v2\n" {
		t.Errorf("b-kept marker = %q, want v2 after update", marker)
	}

	// Named explicitly, a failed pull still aborts.
	if out, err := runRoot(root, "profile", "update", "a-gone"); err == nil || !strings.Contains(err.Error(), "git pull") {
		t.Errorf("expected a hard error for the named profile's pull, got %v\n%s", err, out)
	}
}

// Sync failures collected from an earlier profile are still reported when a
// later profile turns out invalid.
func TestProfileUpdateReportsSyncFailuresAlongsideInvalidProfile(t *testing.T) {
	root := NewRootCmd()
	dir := withScratchConfig(t)
	profilesRoot := filepath.Join(dir, "profiles")
	missing := filepath.Join(t.TempDir(), "missing")
	writeProfile(t, profilesRoot, "a-wrap", "description: wrapper\nsources:\n  - repo: "+missing+"\n    into: tools/s\n")
	writeProfile(t, profilesRoot, "z-broken", "description: bad\npackages: [Not_Valid]\n")

	out, err := runRoot(root, "profile", "update")
	if err == nil {
		t.Fatalf("expected update to fail; output:\n%s", out)
	}
	for _, want := range []string{"z-broken: invalid: ", "1 source(s) could not be updated:", "profile a-wrap: source s (" + missing + ")"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(err.Error(), "1 profile(s) invalid; 1 source(s) could not be updated") {
		t.Errorf("err = %v, want both counts", err)
	}
}

func TestProfileListShowsSourceLines(t *testing.T) {
	root := NewRootCmd()
	dir := withScratchConfig(t)
	profilesRoot := filepath.Join(dir, "profiles")
	src := makeSourceRepoCLI(t)

	if out, err := runRoot(root, "profile", "add", src, "--into", ".claude/skills/s", "wrap"); err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	writeProfile(t, profilesRoot, "never", "description: never synced\nsources:\n  - repo: "+src+"\n    into: .claude/skills/n\n    ref: v1\n")

	out, err := runRoot(root, "profile", "list")
	if err != nil {
		t.Fatalf("list: %v\n%s", err, out)
	}
	commitRe := regexp.MustCompile(`\b[0-9a-f]{12}\b`)
	var synced, unsynced bool
	states := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] != "source" {
			states[fields[0]] = fields[1]
		}
		if strings.Contains(line, "source") && strings.Contains(line, "~/.claude/skills/s") && commitRe.MatchString(line) {
			synced = true
		}
		if strings.Contains(line, "~/.claude/skills/n") && strings.Contains(line, "unavailable: never fetched") {
			unsynced = true
		}
	}
	if !synced {
		t.Errorf("expected a source line with a 12-hex commit for wrap, got:\n%s", out)
	}
	if !unsynced {
		t.Errorf("expected an unavailable source line for never, got:\n%s", out)
	}
	if states["never"] != "inactive" {
		t.Errorf("never state = %q, want inactive; output:\n%s", states["never"], out)
	}
}

func TestProfileRemoveDeletesSources(t *testing.T) {
	root := NewRootCmd()
	dir := withScratchConfig(t)
	src := makeSourceRepoCLI(t)

	if out, err := runRoot(root, "profile", "add", src, "--into", ".claude/skills/s", "wrap"); err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	if out, err := runRoot(root, "profile", "remove", "wrap"); err != nil {
		t.Fatalf("remove: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "sources", "wrap")); !os.IsNotExist(err) {
		t.Errorf("expected sources/wrap removed, stat err = %v", err)
	}
}
