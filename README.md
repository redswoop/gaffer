# gaffer

Run any kind of long work on owned machines, see how far along it is, and have it survive the machine
going down. One controller, an agent per machine, one static Go binary for all of it. Kinds of work are
a manifest plus scripts in whatever language the work wants; the harness itself is only Go.

```
 box A                                     box B
 ┌────────────────────────────┐            ┌──────────────────┐
 │ gaffer controller          │◄── HTTP ───│ gaffer agent     │  dials out; keeps running jobs
 │  sqlite: items, events     │   reports  │  job dirs        │  while the controller is away
 │  scheduler, alerts (ntfy)  │── assign ─►│  tails, polls    │
 │  API + SSE                 │            └──────────────────┘
 │ gaffer agent (local)       │
 └────────────────────────────┘
```

```sh
gaffer submit KIND [flags] [--] ARGS...   # queue a job of a kind (kinds/<KIND>/kind.toml)
gaffer run [flags] -- CMD...              # an ad-hoc command, same treatment
gaffer ls / watch                         # the board: questions first, then review, failures, stalls, running, queued
gaffer show ID / log ID [-f]              # detail + event history / console log (kept on the controller)
gaffer pause|resume|cancel|retry [--resume]|release|ack|top ID...
gaffer drain|undrain HOST
gaffer track TITLE / emit ID LINE         # work gaffer doesn't run, reporting in
```

Submit flags: `--host H` (pin), `--needs LABEL` (slot type, default `cpu`), `--prio N` (lower first),
`--held REASON`, `--on-interrupt resume|restart|hold`, `--stall SEC`, `--review`, `-e K=V`, `--title`.

## Progress

Every source is folded into one record (`done / total / unit / phase / msg / eta`) and the controller
derives rate and ETA from the samples (slope over the last 10 min) unless the job states its own ETA.

1. **Push, line protocol.** Anything a step prints to stdout/stderr:
   ```
   ::progress done=12432 total=33379 unit=frames msg="chunk 42/109"    (also: ::progress 42/109, 0.45, 45%)
   ::phase vae-decode
   ::msg waiting for the NAS
   ::checkpoint seg=7 done=11840          any k=v; handed back on resume
   ::needs-you pick a checkpoint: e3 / e5 / e8
   ```
   `gaffer emit LINE` inside a job writes the same thing into the job's log.
2. **Push from outside.** `gaffer emit ID LINE` to the controller (observed work, other machines);
   `::done` and `::fail REASON` end an observed item.
3. **Regex.** `progress_regex` in the kind for logs that don't speak the protocol. Named groups
   `done total pct eta eta_min msg phase`; two unnamed groups = done, total.
4. **Poll.** `progress_poll` runs every `poll_every` seconds in the job's environment and prints protocol
   lines. Print nothing when there's nothing to say (a `done=0` would overwrite pushed progress).

Only a *change* moves the progress clock, so a job (or a poll) repeating a stuck value goes **stalled**
after `stall_after` seconds (default 15 min): alive but not moving, which a pid check never sees.

## Surviving things

The design point is `kill -9` at any instant, because a shutdown gives a few seconds of warning at best.

- **Steps run detached.** Each step runs under `gaffer _shim` started with `setsid`: its own session, its
  output appended straight to the job log, its identity (`boot_id + pid + start time`) and exit code
  written to the job dir. The agent only watches files, so restarting, upgrading or crashing the agent
  never touches a running step, and neither a reboot nor pid reuse can make a dead job look alive.
- **Everything is on disk, and reconciled.** Agents re-read their job dirs on start: alive → adopt;
  exit code → finish; neither → interrupted. The controller is level-triggered off agent reports, so a
  lost message, either side restarting or the network going away converges instead of drifting. An
  assignment is re-sent until the agent reports it, and agents ignore attempts they already have.
- **Interrupted → the kind's policy.**
  - `resume` (default): step boundaries are free resume points (died in `verify` → rerun `verify`, not
    the render). Inside a step, the job resumes from its last `::checkpoint` if it printed one: it gets
    `W_RESUME=1`, `W_CHECKPOINT` (JSON) and `W_CKPT_<KEY>` per key, plus the same `W_STATE` dir, so a job
    with a checkpoint goes back to the host that holds its state.
  - `restart`: from step 0 with a clean `W_STATE`.
  - `hold`: park it for a human.
  After `max_retries` interrupts (default 3) it fails instead of looping.
- **Pause is a stop at a checkpoint, not a kill.** TERM to the job's process group, `stop_grace` seconds
  to print a `::checkpoint` and exit, then KILL. The slot is free; `resume` continues from there.
- **Circuit breaker.** Two failures in a row on a host drains it (`undrain` after fixing).
- **Alerts** (ntfy, if configured): failed, needs-you, ready for review, interrupted, stalled, host
  unreachable for 5 min, host rebooted, breaker.

## Kinds

`kinds/<name>/kind.toml` plus the scripts it runs. The spec is resolved at submit time and stored with
the job, so editing a kind never changes a queued job. Each agent sets `W_KIND_DIR` to its own copy.

```toml
title = "fake {{1}} units"        # {{args}}, {{1}}..{{9}}
on_interrupt = "resume"           # resume | restart | hold
needs = "gpu"                     # slot label; agents advertise slots = { gpu = 1, cpu = 4 }
stop_grace = 30
stall_after = 600
review = true                     # finish in needs-you; `gaffer ack` = done
fits = '$W_KIND_DIR/fits'         # nonzero exit = not now (VRAM, a lease); output = the reason; retried in 30 s
progress_regex = '...'
progress_poll = '$W_KIND_DIR/poll "$@"'
poll_every = 15

[[steps]]
name = "render"
run = '$W_KIND_DIR/render "$@"'   # bash -c; "$@" = the job's args
```

Step environment: `W_ID W_ATTEMPT W_STEP W_STEP_NAME W_JOBDIR W_STATE W_LOG W_HOST W_KIND_DIR W_RESUME`
(+ `W_CHECKPOINT`, `W_CKPT_*` on resume), the kind's `env`, `-e` from submit, and `gaffer` on `PATH`.

`kinds/fake` is the test kind: every progress path, checkpoints, TERM handling, and
`FAKE_FAIL / FAKE_HANG / FAKE_ASK` to make it fail, wedge or ask.

## Config

`~/.config/gaffer/config.toml` (or `$GAFFER_CONFIG`):

```toml
controller = "http://100.x.y.z:8097"   # what agents and the CLI talk to
listen = "auto:8097"                   # controller bind: auto = tailscale IP, else 127.0.0.1
data = "~/.local/share/gaffer"         # db + central logs (default)
token_file = "~/.config/gaffer/token"  # created by the controller on first start; copy to agents
ntfy = "http://ntfy.example:8093/gaffer"
kinds = "~/src/gaffer/kinds"
[agent]
host = "boxa"
dir = "~/.local/share/gaffer/agent"
slots = { gpu = 1, cpu = 4 }
```

## Build and test

```sh
CGO_ENABLED=0 go build -o gaffer .     # Go 1.27+; pure Go (modernc sqlite), so the binary is static
go test ./... && ./test/e2e.sh
```

`test/e2e.sh` brings up a controller and two agents in a scratch dir and kills each piece in turn: agent
`kill -9` mid-step (adopted, same attempt), job `kill -9` (resumed from checkpoint), simulated reboot
(new boot id), pause/resume through a TERM checkpoint, failure + breaker, stall, a question mid-run,
controller `kill -9` (job unaffected, full log arrives), restart policy, observed work, two hosts.

## Deploy

`./deploy.sh` builds, installs the controller + agent as systemd user units on this host, and pushes
the binary, kinds, config and token to each host in `REMOTES` (settings in an untracked `deploy.local`;
see the top of the script). Remote agents need no root: they dial out to the controller over HTTP, and
a `* * * * * gaffer agent` cron line is a fine watchdog because an agent refuses to run twice.
`demo/tour.sh` is a three-minute guided demo that runs as a gaffer job.

## Not yet

Web board + ntfy action buttons, kind actions (`ship`, `redo`), server-side kind resolution on
`POST /api/items` + a versioned API doc, several kind directories (public + private), history-based ETA
priors before a job reports, job-dir GC on agents.
