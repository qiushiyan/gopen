package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"

	"github.com/qiushiyan/gopen/internal/resolve"
)

// Exit codes are the contract callers branch on; stderr wording is not.
const (
	exitOK       = 0
	exitFail     = 1
	exitUsage    = 2
	exitUnpushed = 3
)

const help = `Usage: gopen [options] [path[:line[-line]]]

Open the current checkout on GitHub, branch-aware, and print the URL.

Resolution, from the HEAD of the current directory's checkout:
  detached HEAD          the commit's tree        /tree/<sha>
  branch with open PR    the PR thread            (gh; skipped on the default branch)
  branch on origin       the branch's tree        /tree/<branch>
  branch not on origin   exit 3, or push and open the PR-create page (-y)

"On origin" is decided from local refs: origin/<branch>, then the branch's
upstream. Only the PR lookup uses the network. Its answer is cached per branch
in git config (branch.<name>.gopen-pr) for 10 minutes, keyed on the pushed
tip, so a new push re-checks at once; --refresh re-checks now. A failed lookup
warns, is not cached, and falls back to the branch's tree.

A path opens that file or directory on the branch (or detached commit) instead
of the PR: /blob/<ref>/<path>, plus #L<n> or #L<n>-L<m> for a :n or :n-m
suffix. A relative path is resolved from the current directory; the path must
exist in the working tree.

Options:
  -n, --print    print the URL without opening a browser; never pushes
  -y, --yes      push a branch that is not on origin (git push -u origin;
                 git config repo.pushArgs adds flags, one per value)
                 without asking, then open the PR-create page, or the path
                 on the branch
      --tree     the branch's tree even when an open PR exists
      --refresh  re-query the PR, ignoring the cached answer
  -h, --help     show this help

Output: on success stdout is exactly the URL, one line, whether opened or
printed. Diagnostics, git push output, and the push prompt go to stderr.

A branch not on origin: with a terminal on stdin (and no --print or -y),
gopen asks before pushing, and declining opens the repository's home page.
Without a terminal, or with --print, it opens nothing and exits 3.

Browser: $GOPEN_BROWSER <url> when set, else $BROWSER <url> (the office mini
sets browser-clip over SSH), else open (macOS) or xdg-open.

Exit codes:
  0  the URL was opened or printed
  1  failure: not a git repository, no github.com origin, push or browser failed
  2  usage: unknown option, extra argument, bad path or line
  3  the branch is not on origin; nothing opened or printed (-y pushes it)

Examples:
  gopen                     the PR thread, else the branch's tree
  gopen --tree              the branch's files, not its PR
  gopen -n                  print the URL, e.g. to copy
  gopen -n src/main.go:42   a link to line 42 on the current branch
`

type options struct {
	print, yes, tree, refresh, help bool
	path                            string
}

func parse(args []string) (options, error) {
	var o options
	var positional []string
	flags := true
	for _, arg := range args {
		if flags && strings.HasPrefix(arg, "-") && arg != "-" {
			switch arg {
			case "--":
				flags = false
			case "-n", "--print":
				o.print = true
			case "-y", "--yes":
				o.yes = true
			case "--tree":
				o.tree = true
			case "--refresh":
				o.refresh = true
			case "-h", "--help":
				o.help = true
			default:
				return o, fmt.Errorf("unknown option %q", arg)
			}
			continue
		}
		positional = append(positional, arg)
	}
	if len(positional) > 1 {
		return o, fmt.Errorf("unexpected argument %q (one path at most)", positional[1])
	}
	if len(positional) == 1 {
		o.path = positional[0]
	}
	return o, nil
}

// run is main without process globals. interactive reports whether stdin is a
// terminal, the only case in which gopen asks before pushing.
func run(ctx context.Context, args []string, stdin io.Reader, interactive bool, stdout, stderr io.Writer) int {
	o, err := parse(args)
	if err != nil {
		fmt.Fprintf(stderr, "gopen: %v\nRun gopen --help for usage.\n", err)
		return exitUsage
	}
	if o.help {
		fmt.Fprint(stdout, help)
		return exitOK
	}
	fail := func(code int, format string, a ...any) int {
		fmt.Fprintf(stderr, "gopen: "+format+"\n", a...)
		return code
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fail(exitFail, "%v", err)
	}
	repo, err := resolve.Open(ctx, cwd)
	if err != nil {
		return fail(exitFail, "%v", err)
	}

	req := resolve.Request{Tree: o.tree, Refresh: o.refresh}
	if o.path != "" {
		p, start, end, err := resolve.SplitLines(o.path)
		if err != nil {
			return fail(exitUsage, "%v", err)
		}
		f, err := repo.Locate(ctx, p)
		if err != nil {
			return fail(exitUsage, "%v", err)
		}
		if f.Dir && start > 0 {
			return fail(exitUsage, "%s is a directory; a line needs a file", p)
		}
		f.Start, f.End = start, end
		req.File = f
	}

	t, err := repo.Resolve(ctx, req)
	if err != nil {
		return fail(exitFail, "%v", err)
	}
	if t.Warning != "" {
		fmt.Fprintf(stderr, "gopen: %s\n", t.Warning)
	}

	url := t.URL
	if t.Unpushed {
		push := o.yes
		switch {
		case o.print || (!o.yes && !interactive):
			return fail(exitUnpushed, "'%s' is not on origin; nothing on GitHub to open (gopen -y pushes it and opens the PR-create page)", t.Branch)
		case !o.yes:
			fmt.Fprintf(stderr, "gopen: '%s' isn't on origin. Push and open a PR? [y/N] ", t.Branch)
			reply, _ := bufio.NewReader(stdin).ReadString('\n')
			push = strings.HasPrefix(strings.ToLower(strings.TrimSpace(reply)), "y")
		}
		switch {
		case !push:
			url = repo.Base // declined: the repository's home page
		default:
			if err := repo.Push(ctx, t.Branch, stdin, stderr); err != nil {
				return fail(exitFail, "push failed: %v", err)
			}
			url = repo.CompareURL(t.Branch)
			if req.File != nil {
				url = repo.RefURL(t.Branch, req.File)
			}
		}
	}

	fmt.Fprintln(stdout, url)
	if o.print {
		return exitOK
	}
	if err := openBrowser(url, stderr); err != nil {
		return fail(exitFail, "%v; URL: %s", err, url)
	}
	return exitOK
}

// openBrowser hands url to $GOPEN_BROWSER, else $BROWSER, else the platform
// opener. $BROWSER outranks the platform because a session that sets it means
// it: over SSH the office mini sets browser-clip, which sends the URL to the
// laptop, where `open` would draw on the mini's own screen. The opener's
// output goes to stderr so stdout stays the URL.
func openBrowser(url string, stderr io.Writer) error {
	name, quiet := os.Getenv("GOPEN_BROWSER"), false
	if name == "" {
		name = os.Getenv("BROWSER")
	}
	if name == "" {
		opener := "xdg-open"
		if runtime.GOOS == "darwin" {
			opener = "open"
		}
		p, err := exec.LookPath(opener)
		if err != nil {
			return errors.New("no browser opener found")
		}
		name, quiet = p, opener == "xdg-open" // xdg-open relays browser chatter
	}
	cmd := exec.Command(name, url)
	if !quiet {
		cmd.Stdout, cmd.Stderr = stderr, stderr
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdin, isTerminal(os.Stdin.Fd()), os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
