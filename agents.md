# Working on gopen

Personal CLI that opens the current checkout on GitHub. The README owns the
design and the output contract. Keep changes proportional to demonstrated
failures or requested features, and the code free of third-party dependencies.

- Contract changes cross repos: stdout (one URL), the exit codes, and the flags
  are consumed by `~/dotfiles/tmux/.config/tmux/scripts/tmux-gopen.sh`
  (prefix g) and the `_gopen` completion in `~/dotfiles/zsh/.config/zsh/git.zsh`.
  The cache key `branch.<name>.gopen-pr` and its three-field value must stay
  readable for entries already in users' repositories.
- Tests need temporary homes, Git config, and repositories, a stub `gh` on a
  PATH with no real one, and `GOPEN_BROWSER` set to a stub. Never run the real
  `gh`, open a browser, or reach the network; isolate tmux checks on a private
  socket (`tmux -L <name>`).
- Consumers call `gopen` through PATH. `make install` installs it in
  `~/.local/bin`. Run `make check` before shipping; source edits alone leave
  callers on the old binary.
- Model-facing help, results, or errors: follow
  `~/dotfiles/claude/.claude/skills/prompt-engineering/SKILL.md`.
- Library/CLI documentation questions: follow `~/.agents/skills/find-docs/SKILL.md`.
