// Package resolve turns a checkout's HEAD into the GitHub URL that shows it:
// the open PR's thread, the branch's tree, or the detached commit's tree, or a
// file on that ref. Git stays the authority for repository state; gh answers
// the one question Git cannot, whether the branch has an open PR.
package resolve

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// CacheTTL bounds how long a cached PR answer is served. A PR closed, merged,
// or opened on an unchanged tip changes nothing in Git, so the pushed sha alone
// can never notice it; the TTL bounds how stale such an answer can get.
const CacheTTL = 600 * time.Second

// DefaultPRTimeout bounds the gh lookup in repositories returned by Open.
var DefaultPRTimeout = 10 * time.Second

// Repo is a checkout whose origin is on github.com.
type Repo struct {
	Dir  string // directory Git and gh run in (the caller's working directory)
	Base string // https://github.com/<owner>/<repo>

	Now       func() time.Time
	PRTimeout time.Duration // bound on the gh lookup, the only network call
}

// Request selects what to point at.
type Request struct {
	Tree    bool  // the branch's tree even when an open PR exists
	Refresh bool  // ignore the cached PR answer
	File    *File // a path on the ref instead of the ref itself
}

// File is a working-tree path, relative to the repository root.
type File struct {
	Path       string // slash-separated; "" is the root
	Dir        bool
	Start, End int // 1-based line range; 0 when absent, End == Start for one line
}

// Target is the outcome of Resolve.
type Target struct {
	URL     string
	Branch  string // the local branch; "" when HEAD is detached
	Warning string // non-fatal: the PR lookup failed and the tree was used

	// Unpushed: Branch has no counterpart on origin, so GitHub has nothing to
	// show for it and URL is empty. The caller decides whether to push.
	Unpushed bool
}

// Open checks that dir is inside a work tree whose origin is on github.com.
func Open(ctx context.Context, dir string) (*Repo, error) {
	if out, err := git(ctx, dir, "rev-parse", "--is-inside-work-tree"); err != nil || out != "true" {
		return nil, errors.New("not in a git repository")
	}
	origin, err := git(ctx, dir, "remote", "get-url", "origin")
	if err != nil {
		return nil, errors.New("no 'origin' remote")
	}
	base, ok := BaseURL(origin)
	if !ok {
		return nil, fmt.Errorf("origin is not a github.com remote (%s)", origin)
	}
	return &Repo{Dir: dir, Base: base, Now: time.Now, PRTimeout: DefaultPRTimeout}, nil
}

// sshShorthand matches git@github.com: and host aliases such as
// git@github.com-personal:, which route SSH keys through ~/.ssh/config.
var sshShorthand = regexp.MustCompile(`^git@github\.com(-[A-Za-z0-9_-]+)?:`)

// BaseURL rewrites an origin URL to https://github.com/<owner>/<repo>.
// It reports false for anything that is not a github.com repository.
func BaseURL(remote string) (string, bool) {
	u := strings.TrimSpace(remote)
	u = sshShorthand.ReplaceAllLiteralString(u, "https://github.com/")
	if rest, ok := strings.CutPrefix(u, "ssh://git@github.com/"); ok {
		u = "https://github.com/" + rest
	}
	u = strings.TrimSuffix(strings.TrimSuffix(u, "/"), ".git")
	rest, ok := strings.CutPrefix(u, "https://github.com/")
	if !ok {
		return "", false
	}
	owner, name, ok := strings.Cut(rest, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", false
	}
	return u, true
}

// Resolve applies the resolution order: detached HEAD -> the commit; a branch
// on origin -> its open PR (unless Tree, File, or the default branch), else its
// tree; a branch not on origin -> Unpushed.
func (r *Repo) Resolve(ctx context.Context, req Request) (Target, error) {
	branch, _ := git(ctx, r.Dir, "symbolic-ref", "--quiet", "--short", "HEAD")
	if branch == "" {
		sha, err := git(ctx, r.Dir, "rev-parse", "--short", "HEAD")
		if err != nil {
			return Target{}, errors.New("HEAD has no commit")
		}
		return Target{URL: r.RefURL(sha, req.File)}, nil
	}
	remote := r.remoteBranch(ctx, branch)
	if remote == "" {
		return Target{Branch: branch, Unpushed: true}, nil
	}
	t := Target{Branch: branch}
	if req.File == nil && !req.Tree && branch != r.defaultBranch(ctx, branch) {
		t.URL, t.Warning = r.pullRequest(ctx, branch, remote, req.Refresh)
	}
	if t.URL == "" {
		t.URL = r.RefURL(remote, req.File)
	}
	return t, nil
}

// remoteBranch names the branch's counterpart on the remote, or "" when there
// is none. Both checks are local refs, so this costs no network call. A
// same-named origin/<branch> wins over the upstream's name: that is what `git
// push` targets under push.default=current, and a renamed branch keeps its old
// upstream, which would point at the old remote branch and its old PR.
func (r *Repo) remoteBranch(ctx context.Context, branch string) string {
	if _, err := git(ctx, r.Dir, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branch); err == nil {
		return branch
	}
	up, err := git(ctx, r.Dir, "rev-parse", "--symbolic-full-name", "@{upstream}")
	if err != nil {
		return ""
	}
	rest, ok := strings.CutPrefix(up, "refs/remotes/")
	if !ok {
		return "" // a local upstream is not on any remote
	}
	_, name, _ := strings.Cut(rest, "/")
	return name
}

// defaultBranch reads origin/HEAD locally; without it, main and master count as
// default. A PR is never from the default branch, so its lookup is skipped.
func (r *Repo) defaultBranch(ctx context.Context, branch string) string {
	d, _ := git(ctx, r.Dir, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD")
	d = strings.TrimPrefix(d, "origin/")
	if d == "" && (branch == "main" || branch == "master") {
		d = branch
	}
	return d
}

// pullRequest returns the branch's open PR URL, or "" when it has none or gh is
// missing. The answer is cached in branch.<name>.gopen-pr as
// "<pushed-sha> <epoch> <url-or-dash>": a new push changes the sha and forces a
// re-check, the TTL bounds staleness, and "no PR" is cached too. A failed
// lookup is reported and not cached.
func (r *Repo) pullRequest(ctx context.Context, branch, remote string, refresh bool) (string, string) {
	gh, err := exec.LookPath("gh")
	if err != nil {
		return "", ""
	}
	now := r.Now().Unix()
	sha, err := git(ctx, r.Dir, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+remote)
	if err != nil || sha == "" {
		sha, _ = git(ctx, r.Dir, "rev-parse", "HEAD")
	}
	key := "branch." + branch + ".gopen-pr"
	if !refresh {
		if entry, err := git(ctx, r.Dir, "config", "--get", key); err == nil {
			if pr, ok := CacheHit(entry, sha, now); ok {
				return pr, ""
			}
		}
	}
	pr, err := r.lookupPR(ctx, gh, remote)
	if err != nil {
		return "", "PR lookup failed (" + err.Error() + "); using the branch tree"
	}
	value := pr
	if value == "" {
		value = "-"
	}
	_, _ = git(ctx, r.Dir, "config", key, fmt.Sprintf("%s %d %s", sha, now, value))
	return pr, ""
}

// CacheHit parses a cache entry and reports its PR URL ("" for a cached "no
// PR") when the entry matches sha and is younger than CacheTTL.
func CacheHit(entry, sha string, now int64) (string, bool) {
	f := strings.Fields(entry)
	if len(f) != 3 || f[0] != sha {
		return "", false
	}
	at, err := strconv.ParseInt(f[1], 10, 64)
	if err != nil || now-at >= int64(CacheTTL/time.Second) {
		return "", false
	}
	if f[2] == "-" {
		return "", true
	}
	return f[2], true
}

// lookupPR asks gh for the newest open PR whose head is the remote branch.
// Open PRs only: `gh pr view <branch>` also returns closed and merged ones,
// exactly the stale link to avoid.
func (r *Repo) lookupPR(ctx context.Context, gh, remote string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, r.PRTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, gh, "pr", "list", "--head", remote, "--state", "open",
		"--limit", "1", "--json", "url", "--jq", ".[0].url // empty")
	out, err := runBounded(cmd, r.Dir)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("gh timed out after %s", r.PRTimeout)
		}
		return "", err
	}
	pr, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	return pr, nil
}

// RefURL points at ref itself (its tree) or at a file or directory on it.
func (r *Repo) RefURL(ref string, f *File) string {
	if f == nil || f.Path == "" {
		return r.Base + "/tree/" + escapePath(ref)
	}
	kind := "blob"
	if f.Dir {
		kind = "tree"
	}
	u := r.Base + "/" + kind + "/" + escapePath(ref) + "/" + escapePath(f.Path)
	if f.Start > 0 {
		if rendered(f.Path) {
			u += "?plain=1" // GitHub renders these; line anchors need the source view
		}
		u += "#L" + strconv.Itoa(f.Start)
		if f.End > f.Start {
			u += "-L" + strconv.Itoa(f.End)
		}
	}
	return u
}

// CompareURL is the PR-create page for a just-pushed branch.
func (r *Repo) CompareURL(branch string) string {
	return r.Base + "/compare/" + escapePath(branch) + "?expand=1"
}

// Push runs `git push [repo.pushArgs…] -u origin <branch>` with its output on
// w. repo.pushArgs is a multi-valued git config key, one argument per value:
// a repository's own push flags, shared with the dotfiles zsh git() wrapper so
// git config is the policy's only home (dotfiles sets --no-verify for planlab,
// whose pre-push hook is only the git-lfs upload).
func (r *Repo) Push(ctx context.Context, branch string, stdin io.Reader, w io.Writer) error {
	args := []string{"push"}
	if extra, err := git(ctx, r.Dir, "config", "--get-all", "repo.pushArgs"); err == nil && extra != "" {
		flags := strings.Split(extra, "\n")
		fmt.Fprintf(w, "gopen: git push %s (repo.pushArgs)\n", strings.Join(flags, " "))
		args = append(args, flags...)
	}
	args = append(args, "-u", "origin", branch)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir, cmd.Stdin, cmd.Stdout, cmd.Stderr = r.Dir, stdin, w, w
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

// Locate resolves a path argument (relative to Dir, or absolute) to a File on
// the repository. The path must exist in the working tree and lie inside it.
func (r *Repo) Locate(ctx context.Context, arg string) (*File, error) {
	top, err := git(ctx, r.Dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, err
	}
	var rel string
	if filepath.IsAbs(arg) {
		realTop, err := filepath.EvalSymlinks(top)
		if err != nil {
			return nil, err
		}
		if rel, err = filepath.Rel(realTop, evalExisting(arg)); err != nil {
			return nil, err
		}
	} else {
		// Git computes the prefix from the physical directory, so symlinked
		// directories above the checkout do not matter.
		prefix, err := git(ctx, r.Dir, "rev-parse", "--show-prefix")
		if err != nil {
			return nil, err
		}
		rel = filepath.Join(prefix, arg)
	}
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return nil, fmt.Errorf("%s is outside the repository", arg)
	}
	if rel == "." {
		rel = ""
	}
	info, err := os.Stat(filepath.Join(top, rel))
	if err != nil {
		return nil, fmt.Errorf("no such path in the working tree: %s", arg)
	}
	return &File{Path: filepath.ToSlash(rel), Dir: info.IsDir()}, nil
}

// SplitLines separates a trailing :N or :N-M line suffix from a path argument.
// A suffix that is not a line spec stays part of the path.
func SplitLines(arg string) (path string, start, end int, err error) {
	path, spec, ok := strings.CutLast(arg, ":")
	if !ok || !isLineSpec(spec) {
		return arg, 0, 0, nil
	}
	a, b, isRange := strings.Cut(spec, "-")
	start, _ = strconv.Atoi(a)
	end = start
	if isRange {
		end, _ = strconv.Atoi(b)
	}
	if path == "" {
		return "", 0, 0, fmt.Errorf("%q names no path", arg)
	}
	if start < 1 || end < start {
		return "", 0, 0, fmt.Errorf("bad line range %q", spec)
	}
	return path, start, end, nil
}

func isLineSpec(s string) bool {
	a, b, isRange := strings.Cut(s, "-")
	return digits(a) && (!isRange || digits(b))
}

func digits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// evalExisting resolves symlinks in p, or in its parent when p itself does not
// exist, so /var and /private/var compare equal on macOS.
func evalExisting(p string) string {
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	if dir, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		return filepath.Join(dir, filepath.Base(p))
	}
	return filepath.Clean(p)
}

// rendered reports files GitHub shows rendered rather than as source.
func rendered(p string) bool {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".md", ".markdown", ".mdown", ".mkd", ".mkdn", ".mdx", ".rst", ".adoc", ".asciidoc",
		".org", ".textile", ".rdoc", ".pod", ".creole", ".mediawiki", ".wiki",
		".ipynb", ".csv", ".tsv", ".svg", ".geojson", ".topojson":
		return true
	}
	return false
}

// escapePath escapes each segment of a ref or path, keeping its slashes.
func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	out, err := runBounded(cmd, dir)
	if err != nil {
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return strings.TrimSuffix(out, "\n"), nil
}

// runBounded runs cmd in its own process group so a cancelled context also
// ends its children (an SSH or credential helper under gh or git).
func runBounded(cmd *exec.Cmd, dir string) (string, error) {
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err == syscall.ESRCH {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			first, _, _ := strings.Cut(msg, "\n")
			return "", fmt.Errorf("%w: %s", err, first)
		}
		return "", err
	}
	return stdout.String(), nil
}
