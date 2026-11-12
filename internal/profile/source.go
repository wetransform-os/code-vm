package profile

// Sources are delivered from host-side checkouts, never from anything the
// agent can reach: each source is a bare clone under SourcesDir, which lives
// in the host config dir and is not mounted into the VM, so the agent cannot
// plant hooks, config or refs that a later host-side git run would trust.
// It is cloned through the transport (--no-local), so a source on a local
// path shares no object files with the checkout and passes on no
// alternates. A local path inside a directory shared with the agent is
// refused before any sync (the cli checks LocalRepoPath results against the
// mounts): fetching runs git upload-pack in that repository under its own
// config, which the agent would control. Content leaves the
// checkout only as a `git archive` of a resolved commit: archive reads blobs
// from the object store, so a symlink in the repo is emitted as a link entry
// rather than followed on the host, and nothing outside the commit's tree
// (.git, export-ignore'd paths) ships. The archive is extracted later in the
// guest as the agent user, so a link inside it can at most point at what the
// agent could already read.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// MaxArchiveBytes caps a source archive's compressed size: archives travel
// into the guest with the rest of the profile payload.
const MaxArchiveBytes = 8 << 20

// errRefNotFound marks a ref that does not resolve in the checkout.
var errRefNotFound = errors.New("not found in checkout")

// SourcesDir is where source checkouts live: next to the profiles dir in
// the host config dir.
func SourcesDir(profilesDir string) string {
	return filepath.Join(filepath.Dir(profilesDir), "sources")
}

// checkoutDir is the bare clone for one source of one profile.
func checkoutDir(profilesDir, profile, source string) string {
	return filepath.Join(SourcesDir(profilesDir), profile, source)
}

// gitCmd builds a local git command against the bare checkout at dir: one
// that reads or writes only the checkout itself (rev-parse, archive,
// for-each-ref, update-ref, symbolic-ref). --git-dir, not -C: -C only
// changes directory and git then searches upwards, so a checkout dir that is
// not (yet) a repo inside a versioned config dir would resolve and archive
// the parent repo. Arguments go straight to exec, never through a shell;
// refs and repos are validated at manifest load so none can parse as an
// option. core.attributesFile is set on the command line because the
// global attributes file needs no config to apply: git reads
// $XDG_CONFIG_HOME/git/attributes (or ~/.config/git/attributes) by default,
// and an eol= or export-ignore rule there would change what archive ships.
func gitCmd(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", append([]string{"-c", "core.attributesFile=" + os.DevNull, "--git-dir=" + dir}, args...)...)
	cmd.Env = gitLocalEnviron()
	return cmd
}

// gitNetCmd builds a network git command (remote set-url, fetch,
// ls-remote) against the checkout at dir, bound to ctx. dir must be
// absolute. Like the clone, the command runs in dir's parent, never in the
// process's working directory: git prefers an existing directory relative
// to its working directory over an scp-like URL, so a directory named
// git@host:org/repo.git planted in the project code-vm was started from
// would turn a remote source into a local repository the agent controls.
// The parent of a checkout lies in the host config dir, which the agent
// cannot write.
func gitNetCmd(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--git-dir=" + dir}, args...)...)
	cmd.Env = gitEnviron()
	cmd.Dir = filepath.Dir(dir)
	return cmd
}

// repoLocationVars are git environment variables that point a command at a
// different repository, object store or index than the one named.
var repoLocationVars = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE",
	"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES",
}

// allowProtocol restricts the transports git may use. Local commands never
// touch the network, so for them it is defense in depth.
const allowProtocol = "GIT_ALLOW_PROTOCOL=file:git:http:https:ssh"

// gitEnviron is the environment of the network commands (clone, remote
// set-url, fetch, ls-remote): the process environment minus
// repoLocationVars, plus allowProtocol. These keep the user's git config,
// which is where credential helpers, ssh commands, proxies and URL rewrites
// live.
func gitEnviron() []string {
	return append(environWithout(func(k string) bool {
		return slices.Contains(repoLocationVars, k)
	}), allowProtocol)
}

// gitLocalEnviron is the environment of the local commands, which need
// nothing from the user's git config and must not take anything from it:
// git archive applies the filters a source repo's own .gitattributes
// selects, and the commands implementing them come from config. With the
// user's config in effect, a source marking files filter=lfs would make
// every code-vm command run git-lfs smudge on the host (network, credential
// helper) and fail in a bare checkout, and any other configured filter would
// run on, and rewrite, content an upstream author chose. Without global and
// system config, and without config injected through the environment,
// archive ships the raw blobs. Only the checkout's own config remains, which
// code-vm wrote and the agent cannot reach. The same goes for attributes:
// GIT_ATTR_NOSYSTEM drops the system attributes file, gitCmd points
// core.attributesFile at /dev/null for the global one, and an inherited
// GIT_ATTR_SOURCE (attributes read from another tree) is removed, so only
// the attributes committed in the source itself apply.
func gitLocalEnviron() []string {
	env := environWithout(func(k string) bool {
		return slices.Contains(repoLocationVars, k) ||
			k == "GIT_CONFIG" || k == "GIT_CONFIG_GLOBAL" || k == "GIT_CONFIG_SYSTEM" ||
			k == "GIT_CONFIG_NOSYSTEM" || k == "GIT_CONFIG_COUNT" || k == "GIT_CONFIG_PARAMETERS" ||
			strings.HasPrefix(k, "GIT_CONFIG_KEY_") || strings.HasPrefix(k, "GIT_CONFIG_VALUE_") ||
			k == "GIT_ATTR_NOSYSTEM" || k == "GIT_ATTR_SOURCE"
	})
	return append(env, "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_ATTR_NOSYSTEM=1", allowProtocol)
}

// environWithout returns the process environment minus the variables drop
// selects by name.
func environWithout(drop func(string) bool) []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if !drop(k) {
			out = append(out, kv)
		}
	}
	return out
}

// resolveCommit resolves ref to a full commit id in the checkout at dir. An
// empty ref means the checkout's HEAD, i.e. the remote's default branch. A
// full commit id (validateSources has lowercased it) is looked up only as
// an object, never as a ref name, so a branch or tag named like the hash
// cannot shadow the pin. A name is looked up as a tag, then a branch, in
// one for-each-ref call: git's own precedence, applied so that a branch
// added later under a tag's name cannot redirect it. Only a full commit id
// is a pin; a tag still moves when its owner re-points it. The error wraps
// errRefNotFound when nothing resolves.
func resolveCommit(dir, ref string) (string, error) {
	if ref == "" || FullCommitRe.MatchString(ref) {
		object := "HEAD"
		if ref != "" {
			object = ref
		}
		out, err := gitCmd(dir, "rev-parse", "--verify", "--quiet", object+"^{commit}").Output()
		if err == nil {
			return strings.TrimSpace(string(out)), nil
		}
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return "", fmt.Errorf("git rev-parse: %w", err)
		}
		return "", fmt.Errorf("ref %s %w", object, errRefNotFound)
	}
	// %(*objectname) is the peeled commit of an annotated tag and empty
	// otherwise, so after whitespace splitting the commit is the second
	// field either way. The patterns are prefixes and the output is sorted
	// by refname (heads before tags), so the exact names are picked out
	// here, tag first.
	out, err := gitOutput(gitCmd(dir, "for-each-ref", "--format=%(refname) %(*objectname) %(objectname)",
		"refs/tags/"+ref, "refs/heads/"+ref), "git for-each-ref")
	if err != nil {
		return "", err
	}
	commits := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if fields := strings.Fields(line); len(fields) >= 2 {
			commits[fields[0]] = fields[1]
		}
	}
	for _, name := range []string{"refs/tags/" + ref, "refs/heads/" + ref} {
		if c, ok := commits[name]; ok {
			return c, nil
		}
	}
	return "", fmt.Errorf("ref %s %w", ref, errRefNotFound)
}

// errOverCap marks an archive whose compressed size passed MaxArchiveBytes.
var errOverCap = fmt.Errorf("source archive is over the %d byte compressed cap", MaxArchiveBytes)

// MaxArchiveRawBytes caps what the guest will expand: gzip can shrink a
// repetitive blob a thousandfold, so the compressed cap alone would let a
// tree of a few gigabytes through to fill the VM's disk after the
// destination was wiped.
const MaxArchiveRawBytes = 64 << 20

// errOverRawCap marks an archive whose uncompressed tar stream passed
// MaxArchiveRawBytes.
var errOverRawCap = fmt.Errorf("source tree is over the %d byte uncompressed cap", MaxArchiveRawBytes)

// cappedReader fails with errOverRawCap once more than max bytes have been
// read through it.
type cappedReader struct {
	r    io.Reader
	max  int
	read int
}

func (c *cappedReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += n
	if c.read > c.max {
		return n, errOverRawCap
	}
	return n, err
}

// capBuffer is a bytes.Buffer that refuses to grow past max.
type capBuffer struct {
	bytes.Buffer
	max  int
	over bool
}

func (b *capBuffer) Write(p []byte) (int, error) {
	if b.over || b.Len()+len(p) > b.max {
		b.over = true
		return 0, errOverCap
	}
	return b.Buffer.Write(p)
}

// archive returns commit's tree from the checkout at dir as a gzipped tar,
// plus the archive's entry names. git's output is compressed as it streams,
// into a buffer capped at MaxArchiveBytes, so an oversize source never sits
// in memory whole; the entry names are read from the same raw stream that
// is compressed, so they are exactly those of the bytes that ship. The zero
// gzip header (no name, no mtime) keeps the bytes a function of the commit
// alone.
func archive(dir, commit string) ([]byte, []string, error) {
	cmd := gitCmd(dir, "archive", "--format=tar", commit)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("git archive: %w", err)
	}
	gz := &capBuffer{max: MaxArchiveBytes}
	zw := gzip.NewWriter(gz)
	raw := io.TeeReader(&cappedReader{r: stdout, max: MaxArchiveRawBytes}, zw)
	names, readErr := tarNames(raw)
	if readErr == nil {
		// The tar reader stops at the end-of-archive marker; the padding
		// after it is part of the stream too.
		_, readErr = io.Copy(io.Discard, raw)
	}
	if readErr != nil {
		// Stop git rather than leave it blocked writing to a pipe nobody
		// reads; Wait then reaps it and closes the pipe.
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	switch {
	case gz.over:
		return nil, nil, errOverCap
	case errors.Is(readErr, errOverRawCap):
		return nil, nil, errOverRawCap
	case readErr == nil && waitErr != nil:
		return nil, nil, fmt.Errorf("git archive: %w: %s", waitErr, strings.TrimSpace(stderr.String()))
	case readErr != nil:
		if waitErr != nil && stderr.Len() > 0 {
			return nil, nil, fmt.Errorf("git archive: %w: %s", waitErr, strings.TrimSpace(stderr.String()))
		}
		return nil, nil, fmt.Errorf("read source archive: %w", readErr)
	}
	if err := zw.Close(); err != nil {
		if gz.over {
			return nil, nil, errOverCap
		}
		return nil, nil, err
	}
	if len(names) == 0 {
		return nil, nil, errors.New("source archive is empty")
	}
	return gz.Bytes(), names, nil
}

// tarNames reads the tar stream r to its end-of-archive marker and returns
// its entry names.
func tarNames(r io.Reader) ([]string, error) {
	var names []string
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return names, nil
		}
		if err != nil {
			return nil, err
		}
		// git archive prepends a pax global header carrying the commit id;
		// it is metadata, not an entry.
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		names = append(names, h.Name)
	}
}

// forbiddenEntry returns the first archive entry that would land on a locked
// path once extracted at into, or "". The into rule already keeps sources
// away from those paths; this rechecks against the actual content.
func forbiddenEntry(into string, names []string) string {
	for _, n := range names {
		if rel := path.Join(into, n); forbiddenFiles[rel] {
			return rel
		}
	}
	return ""
}

// loadSources resolves and archives each of p's sources. A checkout that was
// never fetched, or that lacks the ref, leaves the source Unavailable rather
// than failing the load: the rest of the profile still applies, and the user
// is told to run profile update. Archive failures are hard errors.
func loadSources(profilesDir string, p *Profile) error {
	for i := range p.Sources {
		s := &p.Sources[i]
		dir := checkoutDir(profilesDir, p.Name, s.Name)
		if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
			s.Unavailable = "never fetched"
			continue
		} else if err != nil {
			return fmt.Errorf("source %s: %w", s.Name, err)
		}
		// A directory without HEAD is the remains of an interrupted clone:
		// saying its ref is missing would send the user hunting upstream,
		// while the next sync simply clones it again.
		if ok, err := isCheckout(dir); err != nil {
			return fmt.Errorf("source %s: %w", s.Name, err)
		} else if !ok {
			s.Unavailable = "checkout incomplete"
			continue
		}
		commit, err := resolveCommit(dir, s.Ref)
		if errors.Is(err, errRefNotFound) {
			s.Unavailable = err.Error()
			continue
		} else if err != nil {
			return fmt.Errorf("source %s: %w", s.Name, err)
		}
		b, names, err := archive(dir, commit)
		if err != nil {
			return fmt.Errorf("source %s: %w", s.Name, err)
		}
		if rel := forbiddenEntry(s.Into, names); rel != "" {
			return fmt.Errorf("source %s: archive contains %s; profiles may not ship the locked Claude settings", s.Name, rel)
		}
		s.Commit, s.Archive = commit, b
	}
	return nil
}

// UnavailableWarnings returns one line per unavailable source, in profile
// and manifest order.
func UnavailableWarnings(profiles []Profile) []string {
	var out []string
	for _, p := range profiles {
		for _, s := range p.Sources {
			if s.Unavailable != "" {
				out = append(out, fmt.Sprintf("profile %s: source %s unavailable (%s); run code-vm profile update %s",
					p.Name, s.Name, s.Unavailable, p.Name))
			}
		}
	}
	return out
}

// SyncFailure is a source that could not be brought up to date.
type SyncFailure struct {
	Profile, Source, Repo string
	Err                   error
}

func (f SyncFailure) Error() string {
	return fmt.Sprintf("profile %s: source %s (%s): %v", f.Profile, f.Source, f.Repo, f.Err)
}

func (f SyncFailure) Unwrap() error { return f.Err }

// ResolveRepoURL returns repo as git will actually open it: the user's
// url.<base>.insteadOf rules apply to every network command (which keep the
// user's config for credentials and transports), so a URL that reads as
// remote in the manifest may name a local path by the time git runs. The
// expansion runs with --get-url, which exits without contacting anything,
// from the filesystem root so the argument can never be read as a remote
// name of whatever repository the process happens to be in.
func ResolveRepoURL(repo string) (string, error) {
	cmd := exec.Command("git", "ls-remote", "--get-url", "--", repo)
	cmd.Env = gitEnviron()
	cmd.Dir = string(filepath.Separator)
	out, err := gitOutput(cmd, "git ls-remote --get-url")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// LocalRepoPath reports whether repo names a repository on the local
// filesystem, and returns the path git will open. It follows git's own
// rule: a URL with a scheme is remote except file://, whose path git
// percent-decodes, and so is returned decoded (an undecodable one is an
// error); a colon before the first slash makes the scp-like ssh form
// host:path (so file:x is remote too); anything else is a path, returned as
// written. A relative path is returned as is: what it resolves against is
// the caller's concern.
func LocalRepoPath(repo string) (string, bool, error) {
	if p, ok := strings.CutPrefix(repo, "file://"); ok {
		// file://host/path: git ignores the authority and opens /path, so
		// only the two spellings that plainly mean "this machine" are
		// accepted; any other host would silently turn into a local path.
		i := strings.Index(p, "/")
		if i < 0 {
			return "", true, fmt.Errorf("repository URL %s: file URL has no path", repo)
		}
		if host := p[:i]; host != "" && host != "localhost" {
			return "", true, fmt.Errorf("repository URL %s: file URL host %q is not supported; use file:///path or file://localhost/path", repo, host)
		}
		dec, err := url.PathUnescape(p[i:])
		if err != nil {
			return "", true, fmt.Errorf("repository URL %s: %w", repo, err)
		}
		return dec, true, nil
	}
	if strings.Contains(repo, "://") {
		return "", false, nil
	}
	colon, slash := strings.Index(repo, ":"), strings.Index(repo, "/")
	if colon >= 0 && (slash < 0 || colon < slash) {
		return "", false, nil
	}
	return repo, true, nil
}

// SyncSources clones or fetches the checkout of each of p's sources, in
// manifest order, and reports each resolved commit to out. Only profile add
// and update call it; Load resolves against whatever it left. A source that
// fails is collected and the rest still sync, so one unreachable repo does
// not hold back the others.
func SyncSources(ctx context.Context, profilesDir string, p Profile, out io.Writer) []SyncFailure {
	var fails []SyncFailure
	for _, s := range p.Sources {
		// The names become a path that a failed clone removes; manifest
		// loading validates them, but this is exported.
		err := ValidateName(p.Name)
		if err == nil {
			err = ValidateName(s.Name)
		}
		var commit, dir string
		if err == nil {
			// Absolute, so the network commands can run in its parent.
			dir, err = filepath.Abs(checkoutDir(profilesDir, p.Name, s.Name))
		}
		if err == nil {
			commit, err = syncSource(ctx, dir, s)
		}
		if err != nil {
			fails = append(fails, SyncFailure{Profile: p.Name, Source: s.Name, Repo: s.Repo, Err: err})
			continue
		}
		fmt.Fprintf(out, "%s: source %s → %s\n", p.Name, s.Name, commit[:12])
	}
	return fails
}

// pinRefs is the checkout-local namespace of refs that keep pinned commits
// reachable, so gc never drops a commit the manifest names by id once its
// branch is deleted upstream or when it was only fetchable by id.
const pinRefs = "refs/code-vm/pinned/"

// sourceRefspecs are the only refs a checkout takes from its remote:
// branches and tags, which are all a ref can name. Forge namespaces such as
// refs/pull/* never come along, and --prune, which only prunes within the
// refspecs fetched, can never reach pinRefs.
var sourceRefspecs = []string{"+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"}

// cloneCheckout creates the bare checkout for repo at dir, leaving nothing
// behind on failure (a partial clone would later pass for a checkout).
func cloneCheckout(ctx context.Context, repo, dir string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	// "--" ends option parsing; repos are also validated not to start with
	// "-".
	// --bare rather than --mirror: it takes branches and tags only, as later
	// fetches do. --no-local: a clone from a local path would otherwise copy
	// that repository's objects directory as is, objects/info/alternates
	// included, leaving the checkout reading objects from wherever the
	// source pointed it; the transport packs the objects instead.
	// --no-hardlinks keeps the same guarantee should the transport ever be
	// bypassed. --origin: the clone keeps the user's config, whose
	// clone.defaultRemoteName would otherwise rename the remote every later
	// sync addresses as origin.
	cmd := exec.CommandContext(ctx, "git", "clone", "--bare", "--no-local", "--no-hardlinks", "--origin", "origin", "--quiet", "--", repo, dir)
	cmd.Env = gitEnviron()
	// See gitNetCmd: never resolve the repo against the process's working
	// directory.
	cmd.Dir = filepath.Dir(dir)
	if err := runGit(cmd, "git clone"); err != nil {
		_ = os.RemoveAll(dir)
		return err
	}
	return nil
}

// originOf returns the checkout's origin URL, or "" when it has none.
func originOf(dir string) string {
	out, err := gitOutput(gitCmd(dir, "remote", "get-url", "origin"), "git remote get-url")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// replaceCheckout clones repo into a sibling of dir and swaps it in,
// removing the previous checkout only after the new one is in place. Stale
// siblings from an interrupted earlier swap are swept first.
func replaceCheckout(ctx context.Context, repo, dir string) error {
	for _, pattern := range []string{dir + ".new-*", dir + ".old-*"} {
		stale, _ := filepath.Glob(pattern)
		for _, p := range stale {
			_ = os.RemoveAll(p)
		}
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Errorf("generate checkout name: %w", err)
	}
	suffix := hex.EncodeToString(b[:])
	fresh, old := dir+".new-"+suffix, dir+".old-"+suffix
	if err := cloneCheckout(ctx, repo, fresh); err != nil {
		return err
	}
	if err := os.Rename(dir, old); err != nil {
		_ = os.RemoveAll(fresh)
		return err
	}
	if err := os.Rename(fresh, dir); err != nil {
		// Put the previous checkout back; nothing is lost either way.
		_ = os.Rename(old, dir)
		_ = os.RemoveAll(fresh)
		return err
	}
	return os.RemoveAll(old)
}

// syncSource brings the checkout at dir up to date with s.Repo and resolves
// s.Ref in it. The fetch moves every branch and tag and drops deleted ones:
// a branch or tag ref tracks the remote, a commit id stays pinned.
func syncSource(ctx context.Context, dir string, s Source) (string, error) {
	repo, err := isCheckout(dir)
	if err != nil {
		return "", err
	}
	switch {
	case !repo:
		// Never fetched, or the remains of a clone killed before git wrote
		// HEAD: no fetched objects to protect, so start over.
		if err := os.RemoveAll(dir); err != nil {
			return "", err
		}
		if err := cloneCheckout(ctx, s.Repo, dir); err != nil {
			return "", err
		}
	case originOf(dir) != s.Repo:
		// The manifest now names a different repository. Re-pointing the
		// old checkout would keep its objects, and a pinned commit from the
		// previous repository would go on resolving — and installing — as
		// if the new one contained it. Clone the new repository beside the
		// old checkout and swap it in only once the clone succeeded, so a
		// failed clone still leaves the last good checkout in place.
		if err := replaceCheckout(ctx, s.Repo, dir); err != nil {
			return "", err
		}
	default:
		// A failed fetch returns before anything else touches the
		// checkout: just as Load never fetches (nothing moves on start), a
		// failed sync never discards, so the last good objects keep
		// resolving.
		fetch := gitNetCmd(ctx, dir, append([]string{"fetch", "--prune", "--quiet", "origin"}, sourceRefspecs...)...)
		if err := runGit(fetch, "git fetch"); err != nil {
			return "", err
		}
		// Only a source without a ref resolves HEAD; the clone set it, and
		// it has to follow a later change of the remote's default branch.
		if s.Ref == "" {
			if err := syncDefaultBranch(ctx, dir); err != nil {
				return "", err
			}
		}
	}
	commit, err := resolveCommit(dir, s.Ref)
	if !FullCommitRe.MatchString(s.Ref) {
		if err != nil {
			return "", err
		}
		return commit, prunePins(dir, "")
	}
	if errors.Is(err, errRefNotFound) {
		// A pinned commit that no ref reaches (unmerged, or force-pushed
		// away) can only be fetched by id, and only where the server allows
		// unadvertised objects; hence best effort.
		fetchErr := runGit(gitNetCmd(ctx, dir, "fetch", "--quiet", "origin", s.Ref+":"+pinRefs+s.Ref), "git fetch")
		commit, err = resolveCommit(dir, s.Ref)
		if err != nil && fetchErr != nil {
			err = fmt.Errorf("%w; %w", err, fetchErr)
		}
	}
	if err != nil {
		return "", err
	}
	// Pin even a commit that a branch reaches today: the branch may be
	// deleted upstream, and the next fetch would prune it.
	if err := runGit(gitCmd(dir, "update-ref", pinRefs+commit, commit), "git update-ref"); err != nil {
		return "", err
	}
	return commit, prunePins(dir, pinRefs+commit)
}

// syncDefaultBranch points the checkout's HEAD at the remote's current
// default branch. Not git remote set-head --auto: that writes
// refs/remotes/origin/HEAD and requires the matching remote-tracking branch,
// neither of which a bare checkout has. A remote that does not advertise
// its HEAD (dumb HTTP) leaves HEAD as the clone set it.
func syncDefaultBranch(ctx context.Context, dir string) error {
	out, err := gitOutput(gitNetCmd(ctx, dir, "ls-remote", "--symref", "origin", "HEAD"), "git ls-remote")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(out), "\n") {
		rest, ok := strings.CutPrefix(line, "ref: ")
		if !ok {
			continue
		}
		target, name, _ := strings.Cut(rest, "\t")
		if name != "HEAD" || !strings.HasPrefix(target, "refs/heads/") {
			continue
		}
		return runGit(gitCmd(dir, "symbolic-ref", "HEAD", target), "git symbolic-ref")
	}
	return nil
}

// isCheckout reports whether dir holds a checkout, judged by its shape
// rather than by running git: a bare checkout always has a HEAD file, and git
// can fail on a healthy checkout for unrelated reasons (a malformed global
// config), which must never lead the caller to remove it.
func isCheckout(dir string) (bool, error) {
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return true, nil
}

// prunePins deletes the pin refs in the checkout at dir other than keep, so
// commits the manifest no longer names become collectable again.
func prunePins(dir, keep string) error {
	out, err := gitOutput(gitCmd(dir, "for-each-ref", "--format=%(refname)", pinRefs), "git for-each-ref")
	if err != nil {
		return err
	}
	for _, ref := range strings.Fields(string(out)) {
		if ref == keep {
			continue
		}
		if err := runGit(gitCmd(dir, "update-ref", "-d", ref), "git update-ref"); err != nil {
			return err
		}
	}
	return nil
}

// runGit runs cmd and folds git's stderr into the error, so the user sees
// git's own reason (auth, DNS, not found).
func runGit(cmd *exec.Cmd, what string) error {
	_, err := gitOutput(cmd, what)
	return err
}

// gitOutput is runGit returning cmd's stdout.
func gitOutput(cmd *exec.Cmd, what string) ([]byte, error) {
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %s", what, err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// PruneSources removes the checkouts under SourcesDir/<p.Name> that p no
// longer declares, and the whole directory when p declares no sources. It
// returns the removed checkout names. Entries that could not be a source
// name are skipped, not removed, as profile update skips stray directories:
// SyncSources never made them.
func PruneSources(profilesDir string, p Profile) ([]string, error) {
	if err := ValidateName(p.Name); err != nil {
		return nil, err
	}
	root := filepath.Join(SourcesDir(profilesDir), p.Name)
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	declared := map[string]bool{}
	for _, s := range p.Sources {
		declared[s.Name] = true
	}
	var removed []string
	for _, e := range entries {
		if !e.IsDir() || declared[e.Name()] || ValidateName(e.Name()) != nil {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, e.Name())); err != nil {
			return removed, err
		}
		removed = append(removed, e.Name())
	}
	if len(p.Sources) == 0 {
		if err := os.RemoveAll(root); err != nil {
			return removed, err
		}
	}
	return removed, nil
}
