package profile

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// git runs git in dir and returns its trimmed stdout, failing the test on
// error. The fixed identity and disabled signing keep fixtures independent
// of the host's git config.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir,
		"-c", "user.email=t@t", "-c", "user.name=t",
		"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Fatalf("git %v: %v\n%s", args, err, ee.Stderr)
		}
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// newRepo initialises a non-bare repo, writes files (path -> content), runs
// prepare for anything beyond plain files, and commits everything on branch
// main. It returns the repo path, usable as a clone URL.
func newRepo(t *testing.T, files map[string]string, prepare func(dir string)) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	for rel, content := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if prepare != nil {
		prepare(dir)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "init")
	git(t, dir, "branch", "-M", "main")
	return dir
}

// makeSourceRepo builds the standard source fixture: SKILL.md, an executable
// bin/run, an empty file, a symlink link -> SKILL.md, and secret.txt marked
// export-ignore, committed on main and tagged v1.
func makeSourceRepo(t *testing.T) string {
	t.Helper()
	dir := newRepo(t, map[string]string{
		"SKILL.md":       "# skill\n",
		"bin/run":        "#!/bin/sh\necho run\n",
		"empty":          "",
		"secret.txt":     "do not ship\n",
		".gitattributes": "secret.txt export-ignore\n",
	}, func(dir string) {
		if err := os.Chmod(filepath.Join(dir, "bin", "run"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("SKILL.md", filepath.Join(dir, "link")); err != nil {
			t.Fatal(err)
		}
	})
	git(t, dir, "tag", "v1")
	return dir
}

// mirror creates the bare mirror checkout Load resolves against.
func mirror(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, filepath.Dir(dst), "clone", "-q", "--mirror", src, dst)
}

// untar decodes a gzipped tar archive into its headers by entry name.
func untar(t *testing.T, b []byte) map[string]*tar.Header {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	out := map[string]*tar.Header{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name] = h
	}
}

func TestLoadResolvesAndArchivesSource(t *testing.T) {
	dir := t.TempDir()
	src := makeSourceRepo(t)
	writeProfile(t, dir, "p", "sources:\n  - {repo: "+src+", into: .claude/skills/s, ref: main}\n", nil)
	mirror(t, src, checkoutDir(dir, "p", "s"))
	p, err := Load(dir, "p")
	if err != nil {
		t.Fatal(err)
	}
	s := p.Sources[0]
	if s.Unavailable != "" || len(s.Commit) != 40 || len(s.Archive) == 0 {
		t.Fatalf("source = %+v", s)
	}
	entries := untar(t, s.Archive)
	if _, ok := entries["SKILL.md"]; !ok {
		t.Error("SKILL.md missing")
	}
	if h, ok := entries["bin/run"]; !ok || h.Mode&0o111 == 0 {
		t.Error("executable bit lost")
	}
	if h, ok := entries["empty"]; !ok || h.Size != 0 {
		t.Error("empty tracked file must ship")
	}
	if h, ok := entries["link"]; !ok || h.Typeflag != tar.TypeSymlink || h.Linkname != "SKILL.md" {
		t.Error("symlink must be archived as a link")
	}
	if _, ok := entries["secret.txt"]; ok {
		t.Error("export-ignore'd file must not ship")
	}
	if _, ok := entries[".git/HEAD"]; ok {
		t.Error(".git must not ship")
	}
	if UnavailableWarnings([]Profile{p}) != nil {
		t.Error("a resolved source must not warn")
	}
}

func TestSanitisedSourceNameKeysCheckoutAndGuestFiles(t *testing.T) {
	// The name derived from .oh-my-zsh names the checkout directory, the
	// guest archive file and its sources.list line alike.
	dir := t.TempDir()
	src := makeSourceRepo(t)
	writeProfile(t, dir, "p", "sources:\n  - {repo: "+src+", into: .oh-my-zsh}\n", nil)
	if fails, _ := syncProfile(t, dir, "p"); fails != nil {
		t.Fatalf("failures: %v", fails)
	}
	if _, err := os.Stat(filepath.Join(SourcesDir(dir), "p", "oh-my-zsh", "HEAD")); err != nil {
		t.Fatalf("checkout not at the sanitised name: %v", err)
	}
	p, err := Load(dir, "p")
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]string{}
	for _, f := range GuestFiles([]Profile{p}) {
		paths[f.Path] = f.Content
	}
	if _, ok := paths[GuestRoot+"/p/sources/oh-my-zsh.tar.gz.b64"]; !ok {
		t.Errorf("no archive at the sanitised name among %v", paths)
	}
	if got := paths[GuestRoot+"/p/sources.list"]; got != "oh-my-zsh .oh-my-zsh\n" {
		t.Errorf("sources.list = %q", got)
	}
}

func TestLoadMarksMissingCheckoutUnavailable(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "p", "sources:\n  - {repo: https://example.invalid/r, into: .claude/skills/s}\n", nil)
	p, err := Load(dir, "p")
	if err != nil {
		t.Fatal(err)
	}
	s := p.Sources[0]
	if s.Unavailable != "never fetched" || s.Archive != nil || s.Commit != "" {
		t.Fatalf("source = %+v", s)
	}
	want := []string{"profile p: source s unavailable (never fetched); run code-vm profile update p"}
	if got := UnavailableWarnings([]Profile{p}); !reflect.DeepEqual(got, want) {
		t.Errorf("UnavailableWarnings = %q, want %q", got, want)
	}
}

func TestLoadMarksUnresolvableRefUnavailable(t *testing.T) {
	dir := t.TempDir()
	src := makeSourceRepo(t)
	writeProfile(t, dir, "p", "sources:\n  - {repo: "+src+", into: .claude/skills/s, ref: nope}\n", nil)
	mirror(t, src, checkoutDir(dir, "p", "s"))
	p, err := Load(dir, "p")
	if err != nil {
		t.Fatal(err)
	}
	s := p.Sources[0]
	if s.Unavailable != "ref nope not found in checkout" || s.Archive != nil || s.Commit != "" {
		t.Fatalf("source = %+v", s)
	}
}

func TestLoadResolvesTagAndCommitAndDefault(t *testing.T) {
	dir := t.TempDir()
	src := makeSourceRepo(t)
	head := git(t, src, "rev-parse", "HEAD")
	refs := map[string]string{"tag": "v1", "sha": head, "default": ""}
	for name, ref := range refs {
		spec := "sources:\n  - {repo: " + src + ", into: .claude/skills/s"
		if ref != "" {
			spec += ", ref: " + ref
		}
		writeProfile(t, dir, name, spec+"}\n", nil)
		mirror(t, src, checkoutDir(dir, name, "s"))
	}
	for name := range refs {
		p, err := Load(dir, name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := p.Sources[0].Commit; got != head {
			t.Errorf("%s: Commit = %q (unavailable: %q), want %s", name, got, p.Sources[0].Unavailable, head)
		}
	}
}

func TestForbiddenEntry(t *testing.T) {
	// The into rule already rejects ".claude" itself, so the archive check is
	// defense in depth; pin it directly.
	if got := forbiddenEntry(".claude", []string{"skills/x", "settings.json"}); got != ".claude/settings.json" {
		t.Errorf("forbiddenEntry = %q", got)
	}
	if got := forbiddenEntry(".claude/skills/s", []string{"settings.json"}); got != "" {
		t.Errorf("a settings.json inside a skill dir is fine, got %q", got)
	}
}

func TestLoadShipsSettingsJsonInsideSkillDir(t *testing.T) {
	dir := t.TempDir()
	src := newRepo(t, map[string]string{"settings.json": "{}\n"}, nil)
	writeProfile(t, dir, "p", "sources:\n  - {repo: "+src+", into: .claude/skills/s}\n", nil)
	mirror(t, src, checkoutDir(dir, "p", "s"))
	p, err := Load(dir, "p")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := untar(t, p.Sources[0].Archive)["settings.json"]; !ok {
		t.Error("settings.json inside a skill dir must ship")
	}
}

func TestArchiveRejectsOversize(t *testing.T) {
	dir := t.TempDir()
	src := newRepo(t, nil, func(dir string) {
		// Random bytes are incompressible, so gzip cannot bring them under the cap.
		b := make([]byte, 9<<20)
		if _, err := rand.Read(b); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "blob"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	})
	writeProfile(t, dir, "p", "sources:\n  - {repo: "+src+", into: tools/s}\n", nil)
	mirror(t, src, checkoutDir(dir, "p", "s"))
	_, err := Load(dir, "p")
	// 9 MiB of incompressible data passes the cap while git is still writing:
	// archive stops git mid-stream rather than buffering the whole tar.
	if !errors.Is(err, errOverCap) || !strings.Contains(err.Error(), "over the") {
		t.Fatalf("err = %v, want an oversize error", err)
	}
}

// gzip shrinks a repetitive blob a thousandfold, so the compressed cap alone
// would let gigabytes through to the guest's disk; the raw tar stream has
// its own cap, checked in the same pass.
func TestArchiveRejectsOversizeUncompressed(t *testing.T) {
	dir := t.TempDir()
	src := newRepo(t, nil, func(dir string) {
		// 70 MiB of zeros compresses to a few tens of KiB, well under
		// MaxArchiveBytes.
		if err := os.WriteFile(filepath.Join(dir, "blob"), make([]byte, 70<<20), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	writeProfile(t, dir, "p", "sources:\n  - {repo: "+src+", into: tools/s}\n", nil)
	mirror(t, src, checkoutDir(dir, "p", "s"))
	_, err := Load(dir, "p")
	if !errors.Is(err, errOverRawCap) || !strings.Contains(err.Error(), "uncompressed cap") {
		t.Fatalf("err = %v, want an uncompressed-size error", err)
	}
}

// A manifest that now names a different repository must not keep resolving
// against the old checkout's objects: a commit pinned from the previous
// repository would otherwise install content the new one never had. The
// new repository is cloned beside the old checkout and swapped in only on
// success, so a failing clone still leaves the last good checkout.
func TestSyncSourcesReclonesWhenRepoChanges(t *testing.T) {
	dir := t.TempDir()
	repoA := newRepo(t, map[string]string{"a.txt": "from A\n"}, nil)
	repoB := newRepo(t, map[string]string{"b.txt": "from B\n"}, nil)
	pinnedA := git(t, repoA, "rev-parse", "HEAD")
	manifest := func(repo, ref string) string {
		return "sources:\n  - {repo: " + repo + ", into: tools/s, ref: " + ref + "}\n"
	}

	writeProfile(t, dir, "p", manifest(repoA, pinnedA), nil)
	if fails, _ := syncProfile(t, dir, "p"); len(fails) != 0 {
		t.Fatalf("initial sync: %v", fails)
	}
	if got := loadedCommit(t, dir, "p", 0); got != pinnedA {
		t.Fatalf("commit = %s, want %s", got, pinnedA)
	}

	// Same pin, different repository: A's commit is not in B, so the sync
	// must fail rather than serve it from the leftover objects.
	writeProfile(t, dir, "p", manifest(repoB, pinnedA), nil)
	fails, _ := syncProfile(t, dir, "p")
	if len(fails) != 1 {
		t.Fatalf("a commit the new repository lacks must fail the sync, got %v", fails)
	}
	p, err := Load(dir, "p")
	if err != nil {
		t.Fatal(err)
	}
	if p.Sources[0].Unavailable == "" {
		t.Fatalf("the old repository's commit must not resolve from the new checkout, got %+v", p.Sources[0])
	}
	if got := originOf(checkoutDir(dir, "p", "s")); got != repoB {
		t.Errorf("checkout origin = %q, want the new repository %q", got, repoB)
	}

	// Tracking B's branch resolves B's content.
	writeProfile(t, dir, "p", manifest(repoB, "main"), nil)
	if fails, _ := syncProfile(t, dir, "p"); len(fails) != 0 {
		t.Fatalf("sync against B: %v", fails)
	}
	p, err = Load(dir, "p")
	if err != nil {
		t.Fatal(err)
	}
	commit := p.Sources[0].Commit
	if want := git(t, repoB, "rev-parse", "HEAD"); commit != want {
		t.Errorf("commit = %s, want B's HEAD %s", commit, want)
	}
	if _, ok := untar(t, p.Sources[0].Archive)["b.txt"]; !ok {
		t.Error("the archive must come from the new repository")
	}

	// A change to a repository that cannot be cloned keeps the last good
	// checkout: nothing is swapped out before the clone succeeded.
	writeProfile(t, dir, "p", manifest(filepath.Join(t.TempDir(), "nope"), "main"), nil)
	if fails, _ := syncProfile(t, dir, "p"); len(fails) != 1 {
		t.Fatalf("a failing clone must be reported, got %v", fails)
	}
	if got := loadedCommit(t, dir, "p", 0); got != commit {
		t.Errorf("after a failed reclone the previous checkout must still resolve, got %s want %s", got, commit)
	}
	if leftovers, _ := filepath.Glob(checkoutDir(dir, "p", "s") + ".*"); len(leftovers) != 0 {
		t.Errorf("no sibling checkouts may linger, got %v", leftovers)
	}
}

func TestLoadNeverResolvesParentRepo(t *testing.T) {
	// The config dir is itself a repo (a versioned ~/.config) and the
	// checkout dir exists but is no git dir (an interrupted clone): git must
	// not discover the parent repo and archive it.
	config := newRepo(t, map[string]string{"dotfile": "x\n"}, nil)
	dir := filepath.Join(config, "profiles")
	writeProfile(t, dir, "p", "sources:\n  - {repo: https://example.invalid/r, into: tools/s}\n", nil)
	if err := os.MkdirAll(checkoutDir(dir, "p", "s"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A git archive run from a subdirectory archives only that (empty)
	// subdirectory, which would mask the bug as a hard error; pin resolution
	// directly as well as the end result.
	if c, err := resolveCommit(checkoutDir(dir, "p", "s"), ""); !errors.Is(err, errRefNotFound) {
		t.Fatalf("resolveCommit = %q, %v; want errRefNotFound", c, err)
	}
	p, err := Load(dir, "p")
	if err != nil {
		t.Fatal(err)
	}
	if s := p.Sources[0]; s.Unavailable != "checkout incomplete" || s.Commit != "" || s.Archive != nil {
		t.Fatalf("source = %+v, want it unavailable as an incomplete checkout, not resolved against the parent repo", s)
	}
}

func TestResolveCommitPrecedenceAndAnnotatedTag(t *testing.T) {
	src := makeSourceRepo(t)
	first := git(t, src, "rev-parse", "HEAD")
	// Tag "x" at the first commit, branch "x" at a second one: the branch wins.
	git(t, src, "tag", "x")
	git(t, src, "commit", "-q", "--allow-empty", "-m", "second")
	second := git(t, src, "rev-parse", "HEAD")
	git(t, src, "branch", "x")
	git(t, src, "tag", "-a", "v2", "-m", "annotated")
	m := filepath.Join(t.TempDir(), "m")
	mirror(t, src, m)
	for ref, want := range map[string]string{"x": second, "v1": first, "v2": second} {
		if got, err := resolveCommit(m, ref); err != nil || got != want {
			t.Errorf("resolveCommit(%q) = %q, %v; want %s", ref, got, err, want)
		}
	}
}

func TestResolveCommitIgnoresInheritedGitDir(t *testing.T) {
	src := makeSourceRepo(t)
	want := git(t, src, "rev-parse", "HEAD")
	m := filepath.Join(t.TempDir(), "m")
	mirror(t, src, m)
	other := newRepo(t, map[string]string{"f": "x\n"}, nil)
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	if got, err := resolveCommit(m, ""); err != nil || got != want {
		t.Errorf("resolveCommit = %q, %v; want the mirror's %s", got, err, want)
	}
}

func TestArchiveIgnoresHostConfiguredFilters(t *testing.T) {
	// A source repo selects a filter by .gitattributes; the host's git config
	// (global file, or config injected through the environment) defines what
	// it runs. archive must ship the raw blob and never run the command.
	src := newRepo(t, map[string]string{"f": "raw\n", ".gitattributes": "f filter=t\n"}, nil)
	m := filepath.Join(t.TempDir(), "m")
	mirror(t, src, m)
	commit := git(t, src, "rev-parse", "HEAD")

	tmp := t.TempDir()
	marker := filepath.Join(tmp, "marker")
	script := filepath.Join(tmp, "smudge")
	body := "#!/bin/sh\ntouch '" + marker + "'\ncat >/dev/null\necho smudged\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(tmp, "gitconfig")
	if err := os.WriteFile(global, []byte("[filter \"t\"]\n\tsmudge = "+script+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "filter.t.smudge")
	t.Setenv("GIT_CONFIG_VALUE_0", script)
	t.Setenv("GIT_CONFIG_PARAMETERS", "'filter.t.smudge'='"+script+"'")

	b, _, err := archive(m, commit)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	var got string
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Name == "f" {
			c, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			got = string(c)
		}
	}
	if got != "raw\n" {
		t.Errorf("archived f = %q, want the raw blob", got)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the configured smudge command ran (marker stat: %v)", err)
	}
}

// syncProfile loads the manifest of profile name and syncs its sources,
// returning the failures and the progress output.
func syncProfile(t *testing.T, dir, name string) ([]SyncFailure, string) {
	t.Helper()
	p, err := LoadManifest(dir, name)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	return SyncSources(context.Background(), dir, p, &out), out.String()
}

// loadedCommit loads profile name and returns the commit of its source i,
// failing the test when the source is unavailable.
func loadedCommit(t *testing.T, dir, name string, i int) string {
	t.Helper()
	p, err := Load(dir, name)
	if err != nil {
		t.Fatal(err)
	}
	s := p.Sources[i]
	if s.Unavailable != "" {
		t.Fatalf("source %s unavailable: %s", s.Name, s.Unavailable)
	}
	return s.Commit
}

func TestSyncSourcesClonesThenTracksBranch(t *testing.T) {
	dir := t.TempDir()
	src := makeSourceRepo(t)
	c1 := git(t, src, "rev-parse", "HEAD")
	writeProfile(t, dir, "p", "sources:\n  - {repo: "+src+", into: .claude/skills/s, ref: main}\n", nil)
	fails, out := syncProfile(t, dir, "p")
	if fails != nil {
		t.Fatalf("failures: %v", fails)
	}
	if _, err := os.Stat(checkoutDir(dir, "p", "s")); err != nil {
		t.Fatalf("checkout missing: %v", err)
	}
	if want := "p: source s → " + c1[:12] + "\n"; out != want {
		t.Errorf("progress = %q, want %q", out, want)
	}
	if got := loadedCommit(t, dir, "p", 0); got != c1 {
		t.Errorf("Commit = %s, want %s", got, c1)
	}
	git(t, src, "commit", "-q", "--allow-empty", "-m", "c2")
	c2 := git(t, src, "rev-parse", "HEAD")
	if fails, _ := syncProfile(t, dir, "p"); fails != nil {
		t.Fatalf("failures: %v", fails)
	}
	if got := loadedCommit(t, dir, "p", 0); got != c2 {
		t.Errorf("after sync Commit = %s, want the branch's new tip %s", got, c2)
	}
}

func TestSyncSourcesKeepsPinnedCommit(t *testing.T) {
	dir := t.TempDir()
	src := makeSourceRepo(t)
	c1 := git(t, src, "rev-parse", "HEAD")
	writeProfile(t, dir, "p", "sources:\n  - {repo: "+src+", into: .claude/skills/s, ref: "+c1+"}\n", nil)
	git(t, src, "commit", "-q", "--allow-empty", "-m", "c2")
	if fails, _ := syncProfile(t, dir, "p"); fails != nil {
		t.Fatalf("failures: %v", fails)
	}
	if got := loadedCommit(t, dir, "p", 0); got != c1 {
		t.Errorf("Commit = %s, want the pinned %s", got, c1)
	}
}

func TestSyncSourcesUnreachableKeepsCheckoutAndContinues(t *testing.T) {
	dir := t.TempDir()
	src := makeSourceRepo(t)
	missing := filepath.Join(t.TempDir(), "nope")
	manifest := func(repoA string) string {
		return "sources:\n  - {repo: " + repoA + ", into: tools/a}\n  - {repo: " + src + ", into: tools/b}\n"
	}
	writeProfile(t, dir, "p", manifest(missing), nil)
	fails, _ := syncProfile(t, dir, "p")
	if len(fails) != 1 || fails[0].Source != "a" || fails[0].Profile != "p" || fails[0].Repo != missing || fails[0].Err == nil {
		t.Fatalf("failures = %+v, want one for source a", fails)
	}
	if want := "profile p: source a (" + missing + "): "; !strings.HasPrefix(fails[0].Error(), want) {
		t.Errorf("Error() = %q, want prefix %q", fails[0].Error(), want)
	}
	if _, err := os.Stat(checkoutDir(dir, "p", "a")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a failed clone must leave no checkout behind, stat: %v", err)
	}
	if _, err := os.Stat(checkoutDir(dir, "p", "b")); err != nil {
		t.Errorf("the second source must still sync: %v", err)
	}

	// Once a has a checkout, a repo that becomes unreachable (set-url to a
	// missing path) fails the sync but keeps the old objects resolvable.
	writeProfile(t, dir, "p", manifest(src), nil)
	if fails, _ := syncProfile(t, dir, "p"); fails != nil {
		t.Fatalf("failures: %v", fails)
	}
	head := git(t, src, "rev-parse", "HEAD")
	writeProfile(t, dir, "p", manifest(missing), nil)
	if fails, _ := syncProfile(t, dir, "p"); len(fails) != 1 || fails[0].Source != "a" {
		t.Fatalf("failures = %+v, want one for source a", fails)
	}
	if got := loadedCommit(t, dir, "p", 0); got != head {
		t.Errorf("Commit = %s, want the kept %s", got, head)
	}
}

func TestSyncSourcesFetchesUnadvertisedCommit(t *testing.T) {
	// A pinned commit no ref reaches after the mirror fetch is fetched by id.
	dir := t.TempDir()
	src := makeSourceRepo(t)
	git(t, src, "config", "uploadpack.allowAnySHA1InWant", "true")
	git(t, src, "commit", "-q", "--allow-empty", "-m", "dangling")
	pinned := git(t, src, "rev-parse", "HEAD")
	git(t, src, "reset", "-q", "--hard", "HEAD~1")
	writeProfile(t, dir, "p", "sources:\n  - {repo: file://"+src+", into: tools/s, ref: "+pinned+"}\n", nil)
	if fails, _ := syncProfile(t, dir, "p"); fails != nil {
		t.Fatalf("failures: %v", fails)
	}
	if got := loadedCommit(t, dir, "p", 0); got != pinned {
		t.Errorf("Commit = %s, want %s", got, pinned)
	}
	if got := pins(t, checkoutDir(dir, "p", "s")); got != pinRefs+pinned {
		t.Errorf("pins = %q, want %s", got, pinRefs+pinned)
	}
}

// pins lists the pin refs of the mirror at m, one per line.
func pins(t *testing.T, m string) string {
	t.Helper()
	return git(t, m, "for-each-ref", "--format=%(refname)", pinRefs)
}

func TestSyncSourcesPinSurvivesUpstreamBranchDeletion(t *testing.T) {
	// The pinned commit is reachable only via a side branch at first sync;
	// once the branch is deleted upstream and pruned locally, only the pin
	// ref keeps it alive, even through an aggressive gc.
	dir := t.TempDir()
	src := makeSourceRepo(t)
	git(t, src, "checkout", "-q", "-b", "side")
	git(t, src, "commit", "-q", "--allow-empty", "-m", "side")
	pinned := git(t, src, "rev-parse", "HEAD")
	git(t, src, "checkout", "-q", "main")
	manifest := func(ref string) string {
		return "sources:\n  - {repo: " + src + ", into: tools/s, ref: " + ref + "}\n"
	}
	writeProfile(t, dir, "p", manifest(pinned), nil)
	if fails, _ := syncProfile(t, dir, "p"); fails != nil {
		t.Fatalf("failures: %v", fails)
	}
	git(t, src, "branch", "-q", "-D", "side")
	m := checkoutDir(dir, "p", "s")
	if fails, _ := syncProfile(t, dir, "p"); fails != nil {
		t.Fatalf("failures: %v", fails)
	}
	if out := git(t, m, "for-each-ref", "refs/heads/side"); out != "" {
		t.Fatalf("side branch not pruned: %s", out)
	}
	if got := pins(t, m); got != pinRefs+pinned {
		t.Fatalf("pins = %q, want %s", got, pinRefs+pinned)
	}
	git(t, m, "-c", "gc.reflogExpire=now", "-c", "gc.reflogExpireUnreachable=now", "gc", "-q", "--prune=now")
	if got := loadedCommit(t, dir, "p", 0); got != pinned {
		t.Errorf("Commit = %s, want %s", got, pinned)
	}
	// Switching to a branch drops the pin.
	writeProfile(t, dir, "p", manifest("main"), nil)
	if fails, _ := syncProfile(t, dir, "p"); fails != nil {
		t.Fatalf("failures: %v", fails)
	}
	if got := pins(t, m); got != "" {
		t.Errorf("pins = %q, want none", got)
	}
}

func TestSyncSourcesTakesOnlyBranchesAndTags(t *testing.T) {
	// Forge namespaces (refs/pull/*) never reach the checkout, on clone or
	// on fetch.
	dir := t.TempDir()
	src := makeSourceRepo(t)
	head := git(t, src, "rev-parse", "HEAD")
	git(t, src, "update-ref", "refs/pull/1/head", head)
	writeProfile(t, dir, "p", "sources:\n  - {repo: "+src+", into: tools/s}\n", nil)
	m := checkoutDir(dir, "p", "s")
	for i := range 2 {
		if fails, _ := syncProfile(t, dir, "p"); fails != nil {
			t.Fatalf("sync %d failures: %v", i, fails)
		}
		if out := git(t, m, "for-each-ref", "refs/pull/"); out != "" {
			t.Fatalf("sync %d fetched forge refs: %s", i, out)
		}
		if out := git(t, m, "for-each-ref", "--format=%(refname)", "refs/heads/", "refs/tags/"); out != "refs/heads/main\nrefs/tags/v1" {
			t.Fatalf("sync %d refs = %q", i, out)
		}
		git(t, src, "update-ref", "refs/pull/2/head", head)
	}
}

func TestSyncSourcesClonesWithoutHardlinks(t *testing.T) {
	dir := t.TempDir()
	src := makeSourceRepo(t)
	// The source borrows objects from another repository; a local clone
	// would copy that alternates file and keep reading from there.
	alt := newRepo(t, map[string]string{"other": "x\n"}, nil)
	srcObjects := filepath.Join(src, ".git", "objects")
	if err := os.WriteFile(filepath.Join(srcObjects, "info", "alternates"), []byte(filepath.Join(alt, ".git", "objects")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeProfile(t, dir, "p", "sources:\n  - {repo: "+src+", into: tools/s}\n", nil)
	if fails, _ := syncProfile(t, dir, "p"); fails != nil {
		t.Fatalf("failures: %v", fails)
	}
	m := filepath.Join(checkoutDir(dir, "p", "s"), "objects")
	if _, err := os.Stat(filepath.Join(m, "info", "alternates")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the source's alternates file reached the checkout (stat: %v)", err)
	}
	// Any file of the source's object store, under any name: a transport
	// clone packs objects, so the paths need not match.
	var srcFiles []os.FileInfo
	err := filepath.WalkDir(srcObjects, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		fi, err := os.Stat(p)
		srcFiles = append(srcFiles, fi)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	err = filepath.WalkDir(m, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		a, err := os.Stat(p)
		if err != nil {
			return err
		}
		checked++
		for _, b := range srcFiles {
			if os.SameFile(a, b) {
				t.Errorf("%s is hardlinked to the source repository", p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 || len(srcFiles) == 0 {
		t.Fatal("no object files to compare; the test checks nothing")
	}
	if got, want := loadedCommit(t, dir, "p", 0), git(t, src, "rev-parse", "HEAD"); got != want {
		t.Errorf("Commit = %s, want %s", got, want)
	}
}

func TestLocalRepoPath(t *testing.T) {
	for repo, want := range map[string]string{
		"https://github.com/o/r.git": "",
		"git@github.com:o/r.git":     "",
		"host:path":                  "",
		"ssh://git@host/o/r.git":     "",
		"file:x/r.git":               "",
		"file:///srv/r.git":          "/srv/r.git",
		"file://localhost/srv/r.git": "/srv/r.git",
		"file:///home/u/pro%6aects":  "/home/u/projects",
		"/abs/r":                     "/abs/r",
		"./rel":                      "./rel",
		"rel/path":                   "rel/path",
		"rel/a:b":                    "rel/a:b",
	} {
		got, ok, err := LocalRepoPath(repo)
		if err != nil || ok != (want != "") || got != want {
			t.Errorf("LocalRepoPath(%q) = %q, %v, %v; want %q, %v", repo, got, ok, err, want, want != "")
		}
	}
	if _, ok, err := LocalRepoPath("file:///x/%zz"); !ok || err == nil {
		t.Errorf("an undecodable file URL must be local and an error, got %v, %v", ok, err)
	}
	// git would open /srv/r.git for either of these, ignoring the host; a
	// host that is not plainly this machine must be refused, not resolved.
	for _, repo := range []string{"file://evil.example/srv/r.git", "file://localhost"} {
		if _, ok, err := LocalRepoPath(repo); !ok || err == nil {
			t.Errorf("LocalRepoPath(%q) must be local and an error, got %v, %v", repo, ok, err)
		}
	}
}

func TestSyncSourcesNeverResolvesRepoAgainstWorkingDirectory(t *testing.T) {
	// git prefers an existing directory relative to its working directory
	// over an scp-like URL; one planted where code-vm runs must not be
	// cloned in place of the remote.
	cwd := t.TempDir()
	t.Chdir(cwd)
	planted := filepath.Join(cwd, "git@example.invalid:o", "r.git")
	if err := os.MkdirAll(filepath.Dir(planted), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, cwd, "clone", "-q", "--bare", makeSourceRepo(t), planted)
	t.Setenv("GIT_SSH_COMMAND", "false")
	dir := t.TempDir()
	writeProfile(t, dir, "p", "sources:\n  - {repo: \"git@example.invalid:o/r.git\", into: tools/s}\n", nil)
	fails, _ := syncProfile(t, dir, "p")
	if len(fails) != 1 {
		t.Fatalf("failures = %v, want the remote to be unreachable", fails)
	}
	if _, err := os.Stat(checkoutDir(dir, "p", "s")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the planted directory was cloned (stat: %v)", err)
	}
}

func TestSyncSourcesFollowsDefaultBranchChange(t *testing.T) {
	// A source without a ref tracks the remote's default branch, also after
	// the remote switches it.
	dir := t.TempDir()
	src := makeSourceRepo(t)
	writeProfile(t, dir, "p", "sources:\n  - {repo: "+src+", into: tools/s}\n", nil)
	if fails, _ := syncProfile(t, dir, "p"); fails != nil {
		t.Fatalf("failures: %v", fails)
	}
	git(t, src, "checkout", "-q", "-b", "dev")
	git(t, src, "commit", "-q", "--allow-empty", "-m", "dev")
	dev := git(t, src, "rev-parse", "HEAD")
	if fails, _ := syncProfile(t, dir, "p"); fails != nil {
		t.Fatalf("failures: %v", fails)
	}
	if got := loadedCommit(t, dir, "p", 0); got != dev {
		t.Errorf("Commit = %s, want the new default branch's %s", got, dev)
	}
}

func TestSyncSourcesKeepsCheckoutWhenGitFails(t *testing.T) {
	// git failing on a healthy mirror for an unrelated reason (here a
	// malformed global config) must fail the sync, never remove the mirror.
	dir := t.TempDir()
	src := makeSourceRepo(t)
	head := git(t, src, "rev-parse", "HEAD")
	writeProfile(t, dir, "p", "sources:\n  - {repo: "+src+", into: tools/s}\n", nil)
	if fails, _ := syncProfile(t, dir, "p"); fails != nil {
		t.Fatalf("failures: %v", fails)
	}
	bad := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(bad, []byte("[broken\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", bad)
	fails, _ := syncProfile(t, dir, "p")
	if len(fails) != 1 || !strings.Contains(fails[0].Error(), "config") {
		t.Fatalf("failures = %v, want one naming the bad config", fails)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	if got := loadedCommit(t, dir, "p", 0); got != head {
		t.Errorf("Commit = %s, want the kept %s", got, head)
	}
}

func TestSyncSourcesRejectsInvalidNames(t *testing.T) {
	dir := t.TempDir()
	p := Profile{Name: "p", Sources: []Source{{Name: "..", Repo: "https://example.invalid/r"}}}
	fails := SyncSources(context.Background(), dir, p, io.Discard)
	if len(fails) != 1 || fails[0].Source != ".." {
		t.Fatalf("failures = %v, want one for the invalid name", fails)
	}
	if _, err := os.Stat(SourcesDir(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("nothing may be created for an invalid name, stat: %v", err)
	}
}

func TestSyncSourcesReplacesHalfCreatedCheckout(t *testing.T) {
	// An empty directory at the checkout path is what a clone killed before
	// it could clean up leaves behind; it holds nothing worth keeping.
	dir := t.TempDir()
	src := makeSourceRepo(t)
	head := git(t, src, "rev-parse", "HEAD")
	writeProfile(t, dir, "p", "sources:\n  - {repo: "+src+", into: tools/s}\n", nil)
	if err := os.MkdirAll(checkoutDir(dir, "p", "s"), 0o755); err != nil {
		t.Fatal(err)
	}
	if fails, _ := syncProfile(t, dir, "p"); fails != nil {
		t.Fatalf("failures: %v", fails)
	}
	if got := loadedCommit(t, dir, "p", 0); got != head {
		t.Errorf("Commit = %s, want %s", got, head)
	}
}

func TestSyncSourcesReportsUnresolvableRef(t *testing.T) {
	dir := t.TempDir()
	src := makeSourceRepo(t)
	writeProfile(t, dir, "p", "sources:\n  - {repo: "+src+", into: tools/s, ref: nope}\n", nil)
	fails, _ := syncProfile(t, dir, "p")
	if len(fails) != 1 || !errors.Is(fails[0].Err, errRefNotFound) {
		t.Fatalf("failures = %+v, want a ref-not-found failure", fails)
	}
}

func TestPruneSourcesRemovesUndeclared(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "p", "sources:\n  - {repo: https://example.invalid/r, into: tools/a}\n", nil)
	root := filepath.Join(SourcesDir(dir), "p")
	// .stray does not conform to ValidateName, so prune leaves it alone.
	for _, d := range []string{"a", "b", ".stray"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	p, err := LoadManifest(dir, "p")
	if err != nil {
		t.Fatal(err)
	}
	removed, err := PruneSources(dir, p)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(removed, []string{"b"}) {
		t.Errorf("removed = %q, want [b]", removed)
	}
	for d, want := range map[string]bool{"a": true, "b": false, ".stray": true} {
		if _, err := os.Stat(filepath.Join(root, d)); (err == nil) != want {
			t.Errorf("%s exists = %v, want %v", d, err == nil, want)
		}
	}
}

func TestPruneSourcesRemovesProfileDirWithoutSources(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(SourcesDir(dir), "p")
	if err := os.MkdirAll(filepath.Join(root, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := Profile{Name: "p"}
	if _, err := PruneSources(dir, p); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("profile sources dir must be gone, stat: %v", err)
	}
	// A profile with no sources dir at all is not an error.
	if removed, err := PruneSources(dir, p); err != nil || removed != nil {
		t.Errorf("PruneSources = %q, %v", removed, err)
	}
}

func TestSyncSourcesNamesRemoteOriginDespiteUserDefault(t *testing.T) {
	// The clone keeps the user's config, where clone.defaultRemoteName may
	// rename the remote; every later sync addresses it as origin.
	dir := t.TempDir()
	src := makeSourceRepo(t)
	global := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(global, []byte("[clone]\n\tdefaultRemoteName = upstream\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	writeProfile(t, dir, "p", "sources:\n  - {repo: "+src+", into: tools/s}\n", nil)
	if fails, _ := syncProfile(t, dir, "p"); fails != nil {
		t.Fatalf("clone failures: %v", fails)
	}
	c2 := commitFile(t, src)
	if fails, _ := syncProfile(t, dir, "p"); fails != nil {
		t.Fatalf("fetch failures: %v", fails)
	}
	if got := loadedCommit(t, dir, "p", 0); got != c2 {
		t.Errorf("Commit = %s, want %s", got, c2)
	}
}

// commitFile adds an empty commit to the repo at dir and returns its id.
func commitFile(t *testing.T, dir string) string {
	t.Helper()
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "next")
	return git(t, dir, "rev-parse", "HEAD")
}

func TestArchiveIgnoresHostAttributesFiles(t *testing.T) {
	// The user's global attributes file sits at a default location under
	// XDG_CONFIG_HOME, so no config selects it: an export-ignore there must
	// not change what ships.
	src := newRepo(t, map[string]string{"f": "raw\n"}, nil)
	m := filepath.Join(t.TempDir(), "m")
	mirror(t, src, m)
	commit := git(t, src, "rev-parse", "HEAD")

	xdg := t.TempDir()
	if err := os.MkdirAll(filepath.Join(xdg, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(xdg, "git", "attributes"), []byte("* export-ignore\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", xdg)

	_, names, err := archive(m, commit)
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	if !reflect.DeepEqual(names, []string{"f"}) {
		t.Errorf("archived entries = %v, want [f]", names)
	}
}
