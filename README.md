# gopen

A personal CLI that opens the current checkout on GitHub, branch-aware: the
open PR's thread when the branch has one, otherwise the branch's file tree, or
the commit when HEAD is detached. It prints the URL it opened, so shells, tmux,
editors, and agents share one resolver. Git and `gh` stay the authorities;
gopen owns only the choice of URL.

## Install and use

Requires Go 1.27+ and Git; `gh` (authenticated) enables the PR lookup.

```sh
make check
make install                 # installs to ~/.local/bin; keep it on PATH
gopen                        # PR thread, else the branch's tree
gopen --tree                 # the branch's files even when a PR is open
gopen --refresh              # re-check for a PR now (after opening or closing one)
gopen -n                     # print the URL without opening it
gopen -n src/app/main.go:42  # a link to line 42 on the current branch
gopen -y                     # branch not on origin: push -u, open the PR-create page
gopen --help
```

## Resolution

HEAD of the current directory's checkout decides, so a linked worktree opens
its own branch.

| HEAD | Opens |
|---|---|
| detached | the commit's tree, `/tree/<short-sha>` |
| branch with an open PR | the PR thread (skipped on the default branch) |
| branch on origin | the branch's tree, `/tree/<branch>` |
| branch not on origin | exit 3, or push and open `/compare/<branch>?expand=1` |

The deciding question is whether GitHub knows the ref, not whether the branch
tracks something, and it is answered from local refs without a network call:
a same-named `refs/remotes/origin/<branch>` first, then the branch's upstream
(a remote-tracking ref). Same-name wins because that is what `git push`
targets under `push.default=current`, and a renamed branch keeps its old
upstream, which would point at the old remote branch and its old PR. Refs are
only as fresh as the last fetch or push.

The origin URL becomes `https://github.com/<owner>/<repo>`. SSH shorthand
(`git@github.com:`), host aliases used for SSH key routing
(`git@github.com-personal:`), `ssh://git@github.com/`, and HTTPS URLs are
accepted, with any `.git` or trailing slash removed. Any other host exits 1.

## The PR lookup and its cache

The PR lookup is the one network call: `gh pr list --head <remote-branch>
--state open --limit 1`, run in the checkout, bounded at 10 seconds. Open PRs
only, because `gh pr view <branch>` also returns closed and merged PRs, the
stale link to avoid. It is skipped on the default branch (read from the local
`origin/HEAD`, or `main`/`master` without one), with `--tree`, with a path
argument, and when `gh` is not on PATH.

The answer is cached in the repository's Git config:

```
branch.<local-branch>.gopen-pr = "<pushed-sha> <epoch-seconds> <pr-url-or-dash>"
```

A hit needs the pushed tip (`origin/<remote-branch>`, else HEAD) to still match
and the entry to be younger than 10 minutes. Pushing new commits therefore
re-checks at once. A PR closed, merged, or opened without a push changes
nothing in Git, so the TTL bounds how long such a change goes unnoticed;
`--refresh` skips the cached answer. "No PR" is cached as `-`, so a PR-less
branch stays instant. A failed or timed-out lookup warns on stderr, is not
cached, and falls back to the branch's tree. The key and format are the ones
the earlier zsh `gopen` function wrote, so its entries remain valid.

## Paths and lines

A path argument opens that file or directory on the resolved ref instead of
the PR: `/blob/<ref>/<path>` for a file, `/tree/<ref>/<path>` for a directory.
The ref is the remote branch, or the short commit sha when detached. A `:n` or
`:n-m` suffix adds `#Ln` or `#Ln-Lm`; for files GitHub renders (Markdown,
notebooks, CSV, SVG, and similar) it also adds `?plain=1`, without which line
anchors do nothing. Relative paths start from the current directory; absolute
paths may pass through symlinks. The path must exist in the working tree and
lie inside the repository. A suffix that is not a line spec stays part of the
path.

The link names the branch, so it moves with the branch and assumes the file
matches what was pushed; a commit-pinned permalink is not offered.

## A branch not on origin

GitHub has nothing to show for an unpushed branch, so gopen needs a decision:

- `-y` pushes (`git push -u origin <branch>`) without asking, then opens the
  PR-create page, or the path on the just-pushed branch.
- With a terminal on stdin, gopen asks. Yes pushes as `-y` does; anything else
  opens the repository's home page.
- Without a terminal, or with `--print`, gopen opens and prints nothing and
  exits 3. `--print` never pushes, even with `-y`.

Exit 3 is the contract a non-interactive caller branches on to ask its own
way, as tmux's `confirm-before` does, then reruns with `-y`.

## Output and exit codes

On success stdout is exactly the URL, one line, whether it was opened or only
printed. Diagnostics, `git push` output, the push prompt, and the opener's
output go to stderr. Callers branch on the exit code, never on stderr wording.

| Code | Meaning |
|---|---|
| 0 | the URL was opened or printed |
| 1 | failure: not a git repository, no `origin`, origin not on github.com, push failed, no opener or the opener failed (the URL is still on stdout) |
| 2 | usage: unknown option, more than one path, a path outside the repository or missing from the working tree, a bad line range, a line on a directory |
| 3 | the branch is not on origin; nothing was opened, printed, or pushed |

## Opening the browser

`$GOPEN_BROWSER <url>` when set, else `$BROWSER <url>`, else `open` on macOS
or `xdg-open` elsewhere. `$BROWSER` outranks the platform opener because a
session that sets it means it: over SSH the office mini sets `browser-clip`,
which sends the URL to the laptop, where `open` would draw on the mini's
screen. Each variable names one executable, not a command line. Tests set `GOPEN_BROWSER` to a recording stub.

## Integration boundaries

- **tmux `prefix g`** runs `~/dotfiles/tmux/.config/tmux/scripts/tmux-gopen.sh`,
  which `cd`s to the pane's foreground path, calls `gopen` from PATH with stdin
  from `/dev/null`, shows the URL with `display-message`, and on exit 3 asks
  through `confirm-before` before calling `gopen -y`. The URL on stdout and exit
  code 3 are the contract it relies on.
- **zsh** keeps only the `_gopen` completion in
  `~/dotfiles/zsh/.config/zsh/git.zsh`; the flags it completes follow `--help`.
- **Editors and agents** use `gopen -n [path[:line]]` and read one URL from
  stdout.

`git push` runs as a plain subprocess, so shell functions that wrap `git`
(such as a `--no-verify` wrapper) do not apply to it. A repository that needs
push flags carries them in git config instead: every value of the multi-valued
`repo.pushArgs` is one argument inserted after `push`, and gopen names them on
stderr before pushing. The key is shared: the dotfiles zsh `git()` wrapper
reads it too, so git config is the only place a repository's push policy
lives. Dotfiles sets `repo.pushArgs = --no-verify` for the planlab clone and
its worktrees (`~/.config/git/planlab.gitconfig`, included by `gitdir`).

## Development

Go with the standard library only; Git and `gh` are subprocesses, never
reimplemented. `cmd/gopen` owns arguments, the push decision, output, exit
codes, and the browser; `internal/resolve` owns URL resolution, the PR cache,
path location, and the push itself. Subprocesses run in their own process
group, so the `gh` deadline also ends its children.

`make check` runs race-enabled tests, vet, and a formatting check. The tests
use a temporary `HOME` and global Git config, real repositories, and a bare
local origin reached through `remote.origin.pushurl` while `remote.origin.url`
keeps a github.com shape. A stub `gh` records its arguments and answers from a
file, and `GOPEN_BROWSER` points at a recording stub; PATH holds no real `gh`,
and nothing touches the network or a browser. `make install` builds beside the
destination and renames into place, so callers never run a partial binary.
