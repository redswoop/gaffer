# gaffer

Run any kind of long work on owned machines, see how far along it is, have it survive the machine going
down, and have it step aside the moment you want the machine yourself. One controller, an agent per
machine, one static Go binary for all of it. Kinds of work are a manifest plus scripts in whatever language
the work wants; the harness itself is only Go.

```
 box A                                     box B
 ┌────────────────────────────┐            ┌──────────────────────┐
 │ gaffer controller          │◄── HTTP ───│ gaffer agent         │  dials out; keeps running jobs
 │  sqlite: items, events     │   reports  │  job dirs, leases    │  while the controller is away
 │  scheduler, alerts (ntfy)  │── assign ─►│  tails, polls, probes│
 │  API + SSE                 │            │  unix socket: lease  │
 │ gaffer agent (local)       │            └──────────────────────┘
 └────────────────────────────┘
```

```sh
gaffer submit KIND [flags] [--] ARGS...   # queue a job of a kind (kinds/<KIND>/kind.toml)
gaffer run [flags] -- CMD...              # an ad-hoc command, same treatment
gaffer ls / watch                         # the board: questions first, then review, failures, stalls, running, yielded, queued
gaffer show ID / log ID [-f]              # detail + event history / console log (kept on the controller; -f ends when it settles)
gaffer wait [--events] [--any] ID...      # block until they settle or one needs you (see Waiting)
gaffer pause|resume|cancel|retry [--resume]|ack|top ID...
                                          # ack: a needs-you is answered, a failure seen (off FAILED, kept)
gaffer lease RES [--holder N] [--class C] [--ttl 30m] [--wait] [--exclusive] [--release] [-- CMD...]
                                          # take a resource on this box for something that isn't a job (see Leases)
gaffer leases                             # who holds what on this box, and what the probe sees
gaffer host hold|release|drain HOST       # hold: no new work lands there (running jobs go on); release undoes it;
                                          # drain: hold + pause everything running there
gaffer shutdown                           # pause every job on this box and wait for it: the OS shutdown hook
gaffer track TITLE / emit ID LINE         # work gaffer doesn't run, reporting in
gaffer do ACTION ID / gaffer ACTION ID    # an action the item's kind or observer offers: gaffer ship 13
gaffer kinds [--dirs]                     # the kinds this config sees, first dir wins
```

Work runs on the box you start it on: gaffer is a board, not a dispatcher. To run something on another
machine, ssh there and `gaffer run` / `gaffer submit` there; that box's agent runs it and the board shows it.
`run` also carries your `PATH`, `VIRTUAL_ENV` and `CONDA_PREFIX`, so `gaffer run -- python x.py` behaves like
typing `python x.py`. Run flags: `--progress tqdm|rsync|ffmpeg|pct` (presets; rsync wants
`--info=progress2`) or `--progress-regex RX`.

Submit flags: `--host H` (another box's agent instead of this one's), `--needs LABEL` (slot type, default `cpu`),
`--prio N` (lower first; default 100, or the kind's `priority`; see Priority), `--shares C` (a lease class this job
runs alongside, repeatable), `--held REASON`, `--on-interrupt resume|restart|hold`, `--stall SEC`, `--review`,
`--quiet` (board only, no ntfy pushes), `-e K=V`, `--title`.

Servers are not jobs: something meant to stay up sits on the board as STALLED forever. gaffer is for work that ends.

## Waiting

`gaffer wait ID...` is for scripts and agents that start work and want to hear back without polling. It
returns when every item has settled (done, failed, canceled, needs-you), or as soon as one is in trouble
(failed, a question, review, held, stalled), or with `--any` when the first one settles. It prints a line
per item and the log tail of the ones that need a look, and exits with the worst outcome: `0` done,
`1` failed, `2` needs you, `3` canceled, `4` unknown id, `124` `--timeout DUR` ran out. A yielded item is
neither settled nor trouble: it is coming back by itself.

`--events` streams a line per meaningful change (state, step, a question, a stall, progress in 10% steps)
for something reading lines as they come, and keeps going through trouble until everything settles. Each
round re-reads every item, so a controller restart only delays the answer.

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

## Leases: the machine is yours first

A slot label (`gpu`) is a resource on a host. A running job step holds it; so can a **lease**, which is how
anything that isn't a job (a chat model's server, a render UI, a game) says it wants the card, and for how
long. The agent owns leases (they are files in its dir, so they survive a restart and work with the
controller away); `gaffer lease` talks to this box's agent over a unix socket, never to the controller.

```sh
gaffer lease gpu --holder qwen --class llm --ttl 30m          # take it, or refresh it: idempotent per holder
gaffer lease gpu --holder qwen --class llm --ttl 30m --wait   # ...and return once it is actually free (<= --timeout, 90 s)
gaffer lease gpu --holder game --exclusive -- steam %command% # process-bound: held while the command's scope lives
gaffer lease gpu --holder qwen --release
gaffer leases
```

- **First grant → yield.** Every running step on this host that needs the resource stops (TERM, the kind's
  `stop_grace`, KILL: the pause path, at its last checkpoint) and the item goes **yielded**: a state of its
  own on the board, between running and queued, that says who has the card. It is not an interrupt (no
  retry counted), and nothing starts on the resource while the lease holds.
- **Gone → back by itself.** A TTL lease expires when nobody refreshes it for `--ttl`; a process lease ends
  with its command. Thirty seconds after the last lease on the resource is gone (a holder that comes right
  back costs nothing) the yielded items are requeued ahead of their priority class and resume from their
  checkpoints. A human `pause` on a yielded item wins and sticks; `resume` on one means "queued now, no
  debounce" (it still waits for the lease to go, unless it shares it).
- **`shares`.** A kind or step that declares `shares = ["llm"]` runs alongside a lease of class `llm`: it is
  a co-holder, never bumped by it, never kept from starting by it (a job that uses the local model while a
  person also talks to it). A lease taken from inside a job (`W_ID` set) never bumps that job.
- **Leases never bump leases.** An exclusive lease is refused while anything else holds the resource and
  refuses everything after it; it bumps every job, shared or not. Two non-exclusive leases of different
  classes coexist; a job must share every live class to run.
- **Idle residents.** With a `probe` and `release` hook configured for the resource (below), `--wait` means
  "no step of gaffer's is left on it and the probe sees nothing foreign above `idle_mib`": the release hook
  is run once a minute to clear what is idle (cached models, an idle server), told who is waiting so it can
  spare them. The same hook runs before a job takes an unleased resource that residents still occupy, told
  what the step shares. Without hooks, `--wait` returns when the steps are gone.
- Exit codes: `0` granted (and free, with `--wait`), `1` refused / timed out / gone, `2` usage, `3` no agent here.
  On a timeout the output names what is still on the resource (`#309 still stopping (KILL in 12s)`,
  `comfy-main 9.8G (not gaffer's)`); a TTL lease stays, a process lease is released and the command not run.

Who calls it is up to the host: an LLM proxy on each request (holder `qwen`, class `llm`, refreshing when the
answer ends), a render UI's prompt hook (holder `comfy`, 10 min, and its observer while the queue has work), a
game launcher (`--exclusive -- CMD`). None of that is in this repo; the hooks and callers live with the machine.

## Priority

Lower `--prio` runs first (default 100, or the kind's `priority`). The queue is one list ordered by priority,
then by "already had the slot and yielded it", then by age. With the slots full, a queued job whose priority
is lower than a running holder's by at least `preempt_gap` (50; `preempt_gap = -1` in the config turns it off)
**bumps** the least urgent holder of its slot: the holder yields exactly as for a lease (`yielding gpu to #321
(digest)`) and is requeued first when #321 leaves the slot. One bump in flight per resource per host; never an
action, never a kind with `on_interrupt = restart` (its run would be lost), never while a lease holds the
resource. A would-be preemptor whose victim yielded but which then didn't start (its `fits` said no) waits ten
minutes before bumping anything again. So: interactive use (leases) > scheduled jobs (`--prio 50`) > background
work (100).

## Observers: work gaffer doesn't run

Renders in another queue, agent sessions, a ComfyUI queue: an **observer** is a script an agent runs every
`every` seconds that prints the *whole current truth* of one source, and the controller reconciles, the
way it does with agent reports. `::item KEY attrs...` starts an item; the protocol lines after it apply
to that item:

```
::item 42 title="scene 3" host=boxb state=running stall=1800
::progress done=88 total=92 unit=chunks eta=276
::item 43 title="scene 4" state=queued reason="waiting for the GPU"
::item 41 title="scene 2" state=needs-you
::needs-you review: watch it, then ship it
```

- Attributes: `title`, `state` (`running queued paused held needs-you done failed canceled`, default
  running), `host` (default: the agent running the observer), `kind` (default: the observer's name),
  `reason`, `stall=SECONDS` (default never: most sources have nothing to say about stalls), `quiet=1`,
  `started=` / `finished=` (unix seconds, the source's own times instead of when gaffer first saw it),
  `actions=ship,redo` (what's on offer for this item right now; see Actions).
- **A key missing from a snapshot is gone** (done, "gone from NAME"). A failed run (nonzero exit,
  timeout) changes nothing: the board shows the source as FAILING, and as SILENT after three periods
  without a report, both with one alert.
- The source owns the item, so `pause`, `resume` and `retry` are refused (do that in the source). `ack` and
  `cancel` (= dismiss) work and stick for as long as the source keeps reporting the same thing; an item
  that starts running again comes back.
- Observers go in the agent's config (they are per machine), the scripts anywhere:
  `observers/comfyui` here is an example (it also keeps Comfy's gpu lease fresh while the queue has work).

`quiet` (an item attribute, a kind field, `--quiet`) keeps an item on the board without ntfy pushes, for
sources that already notify on their own.

## Actions

`gaffer ship 13` (= `gaffer do ship 13`) runs an action on an item. The command comes from whoever owns
the item: `actions = { ship = "$W_KIND_DIR/ship" }` in a kind.toml (offered once the job isn't running),
or `actions = { ship = "..." }` on an `[[agent.observe]]` entry (offered when the snapshot says
`actions=ship` on that item). An action is a **job of its own**: detached, logged, on the board, alerting
on failure, run on the host that owns the item, with `W_ITEM`, `W_KEY`, `W_SOURCE`, `W_ITEM_HOST` (and
`W_KIND_DIR` of the item's kind). Two rules make it safe for things like shipping:

- One unfinished action per item; a second is refused.
- An interrupted action is **held**, never retried, never bumped. Whether it took effect is the owner's
  business: the next snapshot (or a look) says. An action's exit code should mean what it says: wrap tools
  that print an error and exit 0.
- A failed action is filed away by itself (`acked`, an event says why) once the source reports its item
  `done`: the thing it was for happened. A human `ack` on the item doesn't count.
- A kind item sitting in review is filed (`done`) when an action on it succeeds: the action was the answer
  to the review. An observed item isn't; its source's next snapshot says what the action did.

## Surviving things

The design point is `kill -9` at any instant, because a shutdown gives a few seconds of warning at best.

- **Steps run detached.** Each step runs under `gaffer _shim` started with `setsid`: its own session, its
  output appended straight to the job log, its identity (`boot_id + pid + start time`) and exit code
  written to the job dir. The agent only watches files, so restarting, upgrading or crashing the agent
  never touches a running step, and neither a reboot nor pid reuse can make a dead job look alive.
  Where `systemd-run --user` works, each step also gets a transient scope of its own
  (`gaffer-HOST-ID-ATTEMPT-STEP.scope`): its own cgroup, so systemd-oomd takes one hungry job, not the
  agent and everything else it runs, and ordered before the shutdown hook so the hook gets to pause it.
- **Everything is on disk, and reconciled.** Agents re-read their job dirs (and leases) on start: alive →
  adopt; exit code → finish; neither → interrupted. The controller is level-triggered off agent reports, so a
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
  Any exit code works; exit 0 after TERM means "yes, paused", never "step done". Actions can't be
  paused (they run to the end; `cancel` stops one). **Every pause has an author**, and the board says it:
  one by you (`pause`, `host drain`) stays paused until you `resume`; one by the shutdown hook, or a
  yield to a lease or a more urgent job, is undone by itself when its reason has passed.
- **Shutdown.** `deploy/gaffer-shutdown.service` runs `gaffer shutdown` as the system goes down, before
  the agent is stopped: it asks the agent (a file in its dir, no network, which is already going) to
  pause every running job, and blocks until they have (up to `TimeoutStopSec`). While that file is fresh
  the host reports itself held, so nothing new lands on it. The pauses are on disk and reach the
  controller after the reboot; the jobs come back queued (`resuming after HOST came back`), at their
  checkpoints, by themselves. An aborted shutdown (the box never went down) resumes them too, a couple
  of minutes after the hook is gone. Two backstops for when the hook doesn't run (power cut, `reboot
  -f`, a job started outside the agent's unit): a step killed by TERM/HUP/INT that gaffer didn't send is
  interrupted, not failed; and an exit 0 from before the reboot with no TERM of gaffer's on record is
  interrupted, not done (the OS's shutdown TERM makes a stopped step look finished).
- **Circuit breaker.** Two failures in a row on a host holds it (`gaffer host release` after fixing).
- **Alerts** (ntfy, if configured): failed, needs-you, ready for review, interrupted, stalled, host
  unreachable for 5 min, host rebooted, breaker.

## Kinds

`kinds/<name>/kind.toml` plus the scripts it runs. The spec is resolved at submit time and stored with
the job, so editing a kind never changes a queued job. Each agent sets `W_KIND_DIR` to its own copy.

```toml
title = "fake {{1}} units"        # {{args}}, {{1}}..{{9}}
on_interrupt = "resume"           # resume | restart | hold
needs = "gpu"                     # slot label; agents advertise slots = { gpu = 1, cpu = 4 }; a step may override
shares = ["llm"]                  # lease classes this job runs alongside (it uses the local model itself)
priority = 50                     # default --prio for the kind (0 = 100)
stop_grace = 30
stall_after = 600
review = true                     # finish in needs-you; `gaffer ack` = done
fits = '$W_KIND_DIR/fits'         # before a step that needs a slot: nonzero exit = not now (VRAM); output = the
                                  # reason; retried every 30 s, head of the queue first
progress_regex = '...'
progress_poll = '$W_KIND_DIR/poll "$@"'
poll_every = 15

[[steps]]
name = "render"
run = '$W_KIND_DIR/render "$@"'   # bash -c; "$@" = the job's args

[[steps]]
name = "verify"
run = '$W_KIND_DIR/verify "$@"'
needs = "cpu"                     # holds a cpu slot, not the gpu: the next render starts while this checks
```

A job occupies the slot of the step it is on, so a kind whose later steps only need `cpu` hands the
`gpu` back as soon as the render step ends. `fits` is for the kind's own limits (does this file fit this
card); who else wants the resource is gaffer's business (leases, the release hook).

Step environment: `W_ID W_ATTEMPT W_STEP W_STEP_NAME W_JOBDIR W_STATE W_LOG W_HOST W_KIND_DIR W_RESUME`
(+ `W_CHECKPOINT`, `W_CKPT_*` on resume), the kind's `env`, `-e` from submit, and `gaffer` on `PATH`.

`kinds/fake` is the test kind: every progress path, checkpoints, TERM handling, and
`FAKE_FAIL / FAKE_HANG / FAKE_ASK / FAKE_TERM_IGNORE / FAKE_FITS_SLEEP` to make it fail, wedge, ask, ignore TERM or dawdle in fits.

## Config

`~/.config/gaffer/config.toml` (or `$GAFFER_CONFIG`):

```toml
controller = "http://100.x.y.z:8097"   # what agents and the CLI talk to
listen = "auto:8097"                   # controller bind: auto = tailscale IP, else 127.0.0.1
data = "~/.local/share/gaffer"         # db + central logs (default)
token_file = "~/.config/gaffer/token"  # created by the controller on first start; copy to agents
ntfy = "http://ntfy.example:8093/gaffer"
kinds = ["~/src/gaffer/kinds", "~/src/private-kinds"]   # or one dir; a kind's name resolves to the first dir that has it
preempt_gap = 50                       # a queued job this much more urgent bumps a running one (-1: never)
[agent]
host = "boxa"
dir = "~/.local/share/gaffer/agent"
slots = { gpu = 1, cpu = 4 }
path = ["/opt/tools/bin"]              # in front of PATH for every step, observer, action (a unit's PATH is bare)
[agent.resources.gpu]                  # optional, per slot label: see Leases
probe = "~/src/gaffer/resources/gpu-probe"   # prints `used=MiB total=MiB`, then `PID NAME MiB` per process
release = "~/.local/bin/gaffer-gpu-release"  # clears idle residents; gets GAFFER_RES, GAFFER_HOLDERS, GAFFER_WAITING_FOR, GAFFER_SHARES
idle_mib = 1500
every = 10
[[agent.observe]]                      # repeatable; see Observers
name = "comfyui"
run = "~/src/gaffer/observers/comfyui"
every = 15
```

Kinds that aren't public belong in a dir of their own (or next to the tool they drive), listed in `kinds`.
`deploy.sh` pushes every dir to the remotes in the same order. `resources/gpu-probe` is the NVIDIA probe;
a release hook is machine-specific (what is idle there) and lives with the machine.

## The board

`gaffer ls`: one line per host (`boxa cpu 2/4 gpu 1/1 (qwen llm, idle 3m)`: who has a resource when it
isn't just jobs, and for how long it has been quiet), observers, then NEEDS YOU / FAILED / RUNNING /
YIELDED / QUEUED / PAUSED / HELD, each row with the one thing worth knowing and the command to type
(`→ gaffer resume 12`). Queued rows say why (`behind #310 for gpu`, `gpu: qwen (llm) has it`, `waiting:
boxa: 4.7 GiB VRAM free, need 21.6`). `gaffer show ID` adds priority, who paused it, what it yielded to and
when the lease expires, time actually spent running over all attempts, and the event history with runs of
the same event folded (`assigned ×497`).

## Build and test

```sh
CGO_ENABLED=0 go build -ldflags "-X gaffer/internal/agent.Version=$(git describe --always --dirty --tags)" -o gaffer .
go test ./... && ./test/e2e.sh         # Go 1.27+; pure Go (modernc sqlite), so the binary is static
```

`test/e2e.sh` brings up a controller and two agents in a scratch dir and kills each piece in turn: agent
`kill -9` mid-step (adopted, same attempt), job `kill -9` (resumed from checkpoint), simulated reboot
(new boot id), a step's unwatched exit 0 before a reboot, pause/resume through a TERM checkpoint, failure + breaker +
ack of a failure, stall, a question mid-run, a step killed by a stray HUP, a fits check saying not now (no attempt, no
event spam, the queue head first when it fits again), a step that needs a different slot than its kind, host
hold/release/drain, the shutdown hook with the controller unreachable and the jobs resuming by themselves after the
reboot (a pause by hand staying paused), the hook holding the host meanwhile, leases (a lease yields the running gpu
job and it comes back from its checkpoint when the lease goes, TTL expiry, a process lease dying with its command,
a sharing job as co-holder, exclusive refused, leases surviving an agent kill -9, a pause by hand beating a yield,
a lease landing during fits, `--wait` timing out honestly), resource hooks (the release hook before a gpu job, not
under a lease, a foreign holder named, a dead pid ignored), priority preemption (the 50-point gap, the bumped job
back first, a restart-policy job never bumped, a preemptor that can't start backing off, one bump in flight),
controller `kill -9` (job unaffected, full log arrives), restart policy, observed work, a kind from a
second kinds dir, observers (snapshot in, progress follows, ack sticks, a failing observer changes
nothing, gone = done), actions (a job on the owner's host, refused when not on offer or already
running, held when interrupted, a failure filed once the source says done, kind actions), two hosts.

## Deploy

`./deploy.sh` builds (version from `git describe`), installs the controller + agent as systemd user units on
this host, and pushes the binary, kinds, config, token and the agent + shutdown units to each host in
`REMOTES` (settings in an untracked `deploy.local`; see the top of the script; `REMOTE_CONF_EXTRA` carries a
remote's `[agent.resources.*]` block). Remote agents need no root: they dial out to the controller over HTTP.
`gaffer ls --json` shows the version each host runs. `demo/tour.sh` is a three-minute guided demo that runs
as a gaffer job.

## Not yet

A `--force` resume that runs a yielded job under a live lease; a stale-holder alert for a lease nobody has
refreshed in hours; fractional resources (VRAM amounts rather than one slot); web board + ntfy action buttons
(actions exist; the buttons would call `POST /api/items/ID/do`); server-side kind resolution on `POST
/api/items` + a versioned API doc; history-based ETA priors before a job reports; job-dir GC on agents.
