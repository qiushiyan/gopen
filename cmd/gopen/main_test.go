package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/gopen/internal/resolve"
)

// Every test runs against a temporary HOME and global Git config, a bare local
// origin reached through remote.origin.pushurl while remote.origin.url keeps a
// github.com shape, a stub gh and a stub browser on a PATH that holds no real
// gh. Nothing reaches the network, the real gh, or a real browser.

const base = "https://github.com/acme/widget"

func TestMain(m *testing.M) {
	// A test that forgets the fixture must still never reach a real browser.
	os.Setenv("GOPEN_BROWSER", "/nonexistent/gopen-test-browser")
	os.Exit(m.Run())
}

type fixture struct {
	t                  *testing.T
	home, repo, origin string
	stub               string // gh and browser stubs, and their logs
	stdout, stderr     bytes.Buffer
	stdin              string
	interactive        bool
}

func setup(t *testing.T) *fixture {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, home: home, repo: filepath.Join(home, "widget"), origin: filepath.Join(home, "origin.git"), stub: filepath.Join(home, "stub")}
	tools := filepath.Join(home, "tools")
	for _, d := range []string{f.stub, tools} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(realGit, filepath.Join(tools, "git")); err != nil {
		t.Fatal(err)
	}
	f.write("stub/gh", "#!/bin/sh\n"+
		"printf '%s\\n' \"$*\" >> \""+f.stub+"/gh.log\"\n"+
		"[ -f \""+f.stub+"/hang\" ] && sleep 5\n"+
		"[ -f \""+f.stub+"/fail\" ] && { echo 'gh: could not resolve host' >&2; exit 1; }\n"+
		"[ -f \""+f.stub+"/pr\" ] && cat \""+f.stub+"/pr\"\n"+
		"exit 0\n", 0o755)
	f.write("stub/browser", "#!/bin/sh\nprintf '%s\\n' \"$1\" >> \""+f.stub+"/browser.log\"\n", 0o755)
	f.write(".gitconfig", "[user]\n\tname = Test\n\temail = test@example.invalid\n[advice]\n\tdetachedHead = false\n", 0o644)

	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("PATH", f.stub+":"+tools+":/usr/bin:/bin")
	t.Setenv("GOPEN_BROWSER", filepath.Join(f.stub, "browser"))
	for _, key := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "BROWSER"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	if p, err := exec.LookPath("gh"); err != nil || p != filepath.Join(f.stub, "gh") {
		t.Fatalf("gh must resolve to the stub, got %q %v", p, err)
	}

	f.git(home, "init", "-q", "--bare", "-b", "main", f.origin)
	f.git(home, "init", "-q", "-b", "main", f.repo)
	f.git(f.repo, "remote", "add", "origin", "git@github.com:acme/widget.git")
	f.git(f.repo, "config", "remote.origin.pushurl", f.origin)
	f.write("widget/README.md", "# widget\n", 0o644)
	f.write("widget/src/app/main.go", "package main\n", 0o644)
	f.git(f.repo, "add", ".")
	f.git(f.repo, "commit", "-qm", "initial")
	f.git(f.repo, "push", "-q", "-u", "origin", "main")
	f.git(f.repo, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	t.Chdir(f.repo)
	return f
}

func (f *fixture) write(rel, body string, mode os.FileMode) {
	f.t.Helper()
	p := filepath.Join(f.home, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) git(dir string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %s: %v", args, out, err)
	}
	return strings.TrimSpace(string(out))
}

// pushed creates a branch with its own commit and pushes it to origin.
func (f *fixture) pushed(branch string) {
	f.t.Helper()
	f.git(f.repo, "switch", "-qc", branch)
	f.git(f.repo, "commit", "-q", "--allow-empty", "-m", branch)
	f.git(f.repo, "push", "-q", "-u", "origin", branch)
}

func (f *fixture) setPR(url string) { f.write("stub/pr", url+"\n", 0o644) }

func (f *fixture) flag(name string, on bool) {
	f.t.Helper()
	p := filepath.Join(f.stub, name)
	if on {
		f.write("stub/"+name, "", 0o644)
	} else {
		os.Remove(p)
	}
}

func (f *fixture) lines(name string) []string {
	b, err := os.ReadFile(filepath.Join(f.stub, name))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		f.t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func (f *fixture) reset() {
	os.Remove(filepath.Join(f.stub, "gh.log"))
	os.Remove(filepath.Join(f.stub, "browser.log"))
}

func (f *fixture) call(args ...string) int {
	f.stdout.Reset()
	f.stderr.Reset()
	return run(context.Background(), args, strings.NewReader(f.stdin), f.interactive, &f.stdout, &f.stderr)
}

// expect runs gopen and checks the exit code and the one-line stdout.
func (f *fixture) expect(code int, url string, args ...string) {
	f.t.Helper()
	rc := f.call(args...)
	want := ""
	if url != "" {
		want = url + "\n"
	}
	if rc != code || f.stdout.String() != want {
		f.t.Fatalf("gopen %v: exit %d stdout %q; want exit %d stdout %q\nstderr: %s", args, rc, f.stdout.String(), code, want, f.stderr.String())
	}
}

func (f *fixture) cache(branch string) string {
	cmd := exec.Command("git", "config", "--get", "branch."+branch+".gopen-pr")
	cmd.Dir = f.repo
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}

func TestPRThreadOpened(t *testing.T) {
	f := setup(t)
	f.pushed("feat/search")
	f.setPR(base + "/pull/7")
	f.expect(0, base+"/pull/7")
	if got := f.lines("browser.log"); len(got) != 1 || got[0] != base+"/pull/7" {
		t.Fatalf("browser: %q", got)
	}
	if got := f.lines("gh.log"); len(got) != 1 || got[0] != "pr list --head feat/search --state open --limit 1 --json url --jq .[0].url // empty" {
		t.Fatalf("gh: %q", got)
	}
	sha := f.git(f.repo, "rev-parse", "refs/remotes/origin/feat/search")
	if c := strings.Fields(f.cache("feat/search")); len(c) != 3 || c[0] != sha || c[2] != base+"/pull/7" {
		t.Fatalf("cache: %q", c)
	}
}

func TestBranchTreeWithoutPR(t *testing.T) {
	f := setup(t)
	f.pushed("feat/search")
	f.expect(0, base+"/tree/feat/search")
	if c := strings.Fields(f.cache("feat/search")); len(c) != 3 || c[2] != "-" {
		t.Fatalf("no-PR answer not cached: %q", c)
	}
	// --tree skips the lookup even when a PR exists.
	f.setPR(base + "/pull/7")
	f.reset()
	f.expect(0, base+"/tree/feat/search", "--tree")
	if got := f.lines("gh.log"); got != nil {
		t.Fatalf("--tree ran gh: %q", got)
	}
}

func TestDefaultBranchSkipsLookup(t *testing.T) {
	f := setup(t)
	f.setPR(base + "/pull/7")
	f.expect(0, base+"/tree/main")
	if got := f.lines("gh.log"); got != nil {
		t.Fatalf("default branch ran gh: %q", got)
	}
}

func TestRenamedBranchFollowsUpstream(t *testing.T) {
	f := setup(t)
	f.pushed("feat/old")
	f.git(f.repo, "branch", "-m", "feat/new") // keeps upstream origin/feat/old
	f.setPR(base + "/pull/9")
	f.expect(0, base+"/pull/9")
	if got := f.lines("gh.log"); len(got) != 1 || !strings.Contains(got[0], "--head feat/old ") {
		t.Fatalf("gh: %q", got)
	}
	if f.cache("feat/new") == "" {
		t.Fatal("cache belongs to the local branch name")
	}
}

func TestDetachedHeadOpensCommit(t *testing.T) {
	f := setup(t)
	f.pushed("feat/search")
	f.git(f.repo, "switch", "-q", "--detach", "HEAD~1")
	short := f.git(f.repo, "rev-parse", "--short", "HEAD")
	f.setPR(base + "/pull/7")
	f.expect(0, base+"/tree/"+short)
	if got := f.lines("gh.log"); got != nil {
		t.Fatalf("detached HEAD ran gh: %q", got)
	}
}

func TestPRCache(t *testing.T) {
	f := setup(t)
	f.pushed("feat/search")
	f.setPR(base + "/pull/7")
	f.expect(0, base+"/pull/7")

	// A fresh hit makes no call, even though the answer has changed.
	f.setPR(base + "/pull/8")
	f.expect(0, base+"/pull/7")
	if n := len(f.lines("gh.log")); n != 1 {
		t.Fatalf("cache hit ran gh: %d calls", n)
	}
	f.expect(0, base+"/pull/8", "--refresh")
	if n := len(f.lines("gh.log")); n != 2 {
		t.Fatalf("--refresh: %d calls", n)
	}

	// An entry at the TTL is stale.
	sha := f.git(f.repo, "rev-parse", "refs/remotes/origin/feat/search")
	old := time.Now().Add(-resolve.CacheTTL).Unix()
	f.git(f.repo, "config", "branch.feat/search.gopen-pr", fmt.Sprintf("%s %d %s/pull/1", sha, old, base))
	f.setPR(base + "/pull/10")
	f.expect(0, base+"/pull/10")

	// A new push moves the key sha, so the cached answer is not served.
	f.git(f.repo, "config", "branch.feat/search.gopen-pr", fmt.Sprintf("%s %d %s/pull/1", sha, time.Now().Unix(), base))
	f.expect(0, base+"/pull/1")
	f.git(f.repo, "commit", "-q", "--allow-empty", "-m", "more")
	f.git(f.repo, "push", "-q")
	f.expect(0, base+"/pull/10")

	// An entry the zsh function wrote is read the same way.
	newSHA := f.git(f.repo, "rev-parse", "refs/remotes/origin/feat/search")
	if c := strings.Fields(f.cache("feat/search")); len(c) != 3 || c[0] != newSHA {
		t.Fatalf("cache: %q", c)
	}
	f.git(f.repo, "config", "branch.feat/search.gopen-pr", newSHA+" "+strconv.FormatInt(time.Now().Unix(), 10)+" -")
	f.expect(0, base+"/tree/feat/search")
}

func TestPRLookupFailureFallsBackUncached(t *testing.T) {
	f := setup(t)
	f.pushed("feat/search")
	f.flag("fail", true)
	f.expect(0, base+"/tree/feat/search")
	if !strings.Contains(f.stderr.String(), "PR lookup failed") || f.cache("feat/search") != "" {
		t.Fatalf("stderr %q cache %q", f.stderr.String(), f.cache("feat/search"))
	}
	f.flag("fail", false)
	f.setPR(base + "/pull/7")
	f.expect(0, base+"/pull/7")

	// A hanging gh is bounded, and its children with it.
	f.flag("hang", true)
	defer func(d time.Duration) { resolve.DefaultPRTimeout = d }(resolve.DefaultPRTimeout)
	resolve.DefaultPRTimeout = 300 * time.Millisecond
	start := time.Now()
	f.expect(0, base+"/tree/feat/search", "--refresh")
	if elapsed := time.Since(start); elapsed > 3*time.Second || !strings.Contains(f.stderr.String(), "timed out") {
		t.Fatalf("hang: %s %q", elapsed, f.stderr.String())
	}
}

func TestWithoutGH(t *testing.T) {
	f := setup(t)
	f.pushed("feat/search")
	os.Remove(filepath.Join(f.stub, "gh"))
	if _, err := exec.LookPath("gh"); err == nil {
		t.Fatal("a gh is still on PATH")
	}
	f.expect(0, base+"/tree/feat/search")
	if f.stderr.Len() != 0 || f.cache("feat/search") != "" {
		t.Fatalf("stderr %q cache %q", f.stderr.String(), f.cache("feat/search"))
	}
}

func TestPrintDoesNotOpen(t *testing.T) {
	f := setup(t)
	f.pushed("feat/search")
	f.setPR(base + "/pull/7")
	f.expect(0, base+"/pull/7", "--print")
	f.expect(0, base+"/pull/7", "-n")
	if got := f.lines("browser.log"); got != nil {
		t.Fatalf("--print opened %q", got)
	}
}

func TestNotOnOrigin(t *testing.T) {
	f := setup(t)
	f.git(f.repo, "switch", "-qc", "feat/local")
	f.git(f.repo, "commit", "-q", "--allow-empty", "-m", "local")
	onOrigin := func() bool {
		return exec.Command("git", "--git-dir", f.origin, "rev-parse", "--verify", "--quiet", "refs/heads/feat/local").Run() == nil
	}

	// No terminal: exit 3, nothing printed, opened, or pushed. --print and -y
	// together still never push.
	for _, args := range [][]string{nil, {"--print"}, {"-n", "-y"}} {
		f.expect(3, "", args...)
		if !strings.Contains(f.stderr.String(), "'feat/local' is not on origin") {
			t.Fatalf("stderr: %q", f.stderr.String())
		}
	}
	if f.lines("browser.log") != nil || f.lines("gh.log") != nil || onOrigin() {
		t.Fatal("exit 3 must not open, look up, or push")
	}

	// A terminal asks; declining opens the repository home without pushing.
	f.interactive, f.stdin = true, "n\n"
	f.expect(0, base)
	if !strings.Contains(f.stderr.String(), "Push and open a PR? [y/N]") || onOrigin() {
		t.Fatalf("decline: %q", f.stderr.String())
	}

	// Accepting pushes -u and opens the PR-create page.
	f.stdin = "y\n"
	f.reset()
	f.expect(0, base+"/compare/feat/local?expand=1")
	if !onOrigin() || f.git(f.repo, "rev-parse", "--abbrev-ref", "@{upstream}") != "origin/feat/local" {
		t.Fatal("not pushed with upstream")
	}
	if got := f.lines("browser.log"); len(got) != 1 || got[0] != base+"/compare/feat/local?expand=1" {
		t.Fatalf("browser: %q", got)
	}
	// Pushed, it resolves like any branch on origin.
	f.interactive, f.stdin = false, ""
	f.expect(0, base+"/tree/feat/local")
}

func TestYesPushesWithoutTerminal(t *testing.T) {
	f := setup(t)
	f.git(f.repo, "switch", "-qc", "feat/local")
	f.expect(0, base+"/compare/feat/local?expand=1", "-y")
	if f.git(f.origin, "rev-parse", "refs/heads/feat/local") != f.git(f.repo, "rev-parse", "HEAD") {
		t.Fatal("not pushed")
	}
	// With a path, -y opens the file on the just-pushed branch.
	f.git(f.repo, "switch", "-qc", "feat/other")
	f.expect(0, base+"/blob/feat/other/README.md?plain=1#L1", "-y", "README.md:1")

	// gopen.pushArgs carries a repository's push flags: with --no-verify a
	// failing pre-push hook is skipped; without it the hook still runs.
	hooks := filepath.Join(f.home, "hooks")
	if err := os.MkdirAll(hooks, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "pre-push"), []byte("#!/bin/sh\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	f.git(f.repo, "config", "core.hooksPath", hooks)
	f.git(f.repo, "switch", "-qc", "feat/hooked")
	f.expect(1, "", "-y")
	f.git(f.repo, "config", "--add", "gopen.pushArgs", "--no-verify")
	f.expect(0, base+"/compare/feat/hooked?expand=1", "-y")
	if !strings.Contains(f.stderr.String(), "git push --no-verify (gopen.pushArgs)") {
		t.Fatalf("stderr: %q", f.stderr.String())
	}
	f.git(f.repo, "config", "--unset-all", "gopen.pushArgs")
	f.git(f.repo, "config", "--unset", "core.hooksPath")

	// A failed push exits 1 with nothing on stdout.
	f.git(f.repo, "switch", "-qc", "feat/broken")
	f.git(f.repo, "config", "remote.origin.pushurl", filepath.Join(f.home, "missing.git"))
	f.expect(1, "", "-y")
	if !strings.Contains(f.stderr.String(), "push failed") {
		t.Fatalf("stderr: %q", f.stderr.String())
	}
}

func TestPathAndLine(t *testing.T) {
	f := setup(t)
	f.pushed("feat/search")
	f.setPR(base + "/pull/7")
	f.expect(0, base+"/blob/feat/search/src/app/main.go#L42", "-n", "src/app/main.go:42")
	f.expect(0, base+"/blob/feat/search/src/app/main.go#L3-L9", "-n", "src/app/main.go:3-9")
	f.expect(0, base+"/blob/feat/search/README.md", "-n", "README.md")
	f.expect(0, base+"/tree/feat/search/src", "-n", "src")
	f.expect(0, base+"/tree/feat/search", "-n", ".")
	f.expect(0, base+"/blob/feat/search/src/app/main.go#L1", "-n", filepath.Join(f.repo, "src/app/main.go:1"))
	if got := f.lines("gh.log"); got != nil {
		t.Fatalf("a path must skip the PR lookup: %q", got)
	}

	// Relative paths start from the current directory, not the root.
	t.Chdir(filepath.Join(f.repo, "src", "app"))
	f.expect(0, base+"/blob/feat/search/src/app/main.go#L2", "-n", "main.go:2")
	f.expect(0, base+"/blob/feat/search/README.md", "-n", "../../README.md")
	// An absolute path through a symlinked directory still lands in the repo.
	link := filepath.Join(f.home, "link")
	if err := os.Symlink(f.repo, link); err != nil {
		t.Fatal(err)
	}
	f.expect(0, base+"/blob/feat/search/README.md", "-n", filepath.Join(link, "README.md"))

	for _, arg := range []string{"../../../outside", "missing.go", "/etc/hosts", "../app:3", "main.go:0", "main.go:9-2"} {
		f.expect(2, "", "-n", arg)
	}

	// A detached HEAD points the path at the commit.
	t.Chdir(f.repo)
	f.git(f.repo, "switch", "-q", "--detach")
	short := f.git(f.repo, "rev-parse", "--short", "HEAD")
	f.expect(0, base+"/blob/"+short+"/src/app/main.go#L5", "src/app/main.go:5")
	if got := f.lines("browser.log"); len(got) != 1 {
		t.Fatalf("browser: %q", got)
	}
}

func TestOriginShapes(t *testing.T) {
	f := setup(t)
	for _, origin := range []string{
		"git@github.com-personal:acme/widget.git",
		"ssh://git@github.com/acme/widget.git",
		"https://github.com/acme/widget.git",
	} {
		f.git(f.repo, "remote", "set-url", "origin", origin)
		f.expect(0, base+"/tree/main", "-n")
	}
	f.git(f.repo, "remote", "set-url", "origin", "git@gitlab.com:acme/widget.git")
	f.expect(1, "", "-n")
	if !strings.Contains(f.stderr.String(), "not a github.com remote") {
		t.Fatalf("stderr: %q", f.stderr.String())
	}
	f.git(f.repo, "remote", "remove", "origin")
	f.expect(1, "", "-n")

	t.Chdir(f.home)
	f.expect(1, "", "-n")
	if !strings.Contains(f.stderr.String(), "not in a git repository") {
		t.Fatalf("stderr: %q", f.stderr.String())
	}
}

func TestUsageAndBrowserFailure(t *testing.T) {
	f := setup(t)
	f.expect(2, "", "--bogus")
	f.expect(2, "", "README.md", "src")
	if rc := f.call("--help"); rc != 0 || !strings.Contains(f.stdout.String(), "Exit codes:") {
		t.Fatalf("help: %d", rc)
	}
	// A failed opener exits 1, but the URL has already reached stdout.
	t.Setenv("GOPEN_BROWSER", "/usr/bin/false")
	f.expect(1, base+"/tree/main")
}
