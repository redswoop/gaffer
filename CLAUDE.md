# gaffer

General-purpose work runner + status board: one controller, an agent per machine, one static Go binary.
Kinds of work = `kinds/<name>/kind.toml` + scripts in any language; the harness itself is Go only (no
Python/JS in the harness, by design). Read README.md for the model, the `::` progress protocol and the
resilience rules before changing anything.

- **This repo is public.** Nothing machine-specific, personal or private goes in it: hostnames, IPs,
  tailnet names, home paths, tokens. Machine settings live in the untracked `deploy.local` and
  `~/.config/gaffer/`. Kinds for private work live in a separate private directory/repo, not `kinds/`.
- Build `CGO_ENABLED=0 go build -o gaffer .`; prove changes with `go test ./... && ./test/e2e.sh` (kills every
  piece with -9 in a scratch dir; must stay green). Add an e2e case for any new failure mode you handle.
- Design rules: agents own processes, the controller only reacts to agent reports (level-triggered, never
  event replay); every state transition is on disk before it happens; never kill by pattern, only by the
  recorded process group; a kind's own logic stays in its scripts, not in Go.
