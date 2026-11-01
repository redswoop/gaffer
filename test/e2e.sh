#!/bin/bash
# test/e2e.sh: a controller + two agents in a scratch dir, then kill -9 each piece in turn and check
# the system converges. Run from the repo after `go build -o gaffer .`. Leaves the scratch dir for
# post-mortems (printed at the end).
set -u
ROOT=$(cd "$(dirname "$0")/.." && pwd)
T=${GAFFER_TEST_DIR:-$(mktemp -d -t gaffer-e2e.XXXX)}
PORT=${PORT:-18097}
BIN=$T/bin/gaffer
PASS=0 FAIL=0
BOOT=""   # set to simulate a reboot (agents and shims read GAFFER_BOOT_ID)
mkdir -p "$T/bin" && \cp -f "$ROOT/gaffer" "$BIN"

conf() { # conf HOST
  cat > "$T/$1.toml" <<EOT
controller = "http://127.0.0.1:$PORT"
listen = "127.0.0.1:$PORT"
data = "$T/ctl"
token_file = "$T/token"
kinds = ["$T/kinds", "$ROOT/kinds"]
[agent]
host = "$1"
dir = "$T/$1"
slots = { cpu = 2 }
EOT
}
conf alpha; conf beta
cat >> "$T/alpha.toml" <<EOT
[[agent.observe]]
name = "ext"
run = "$T/observe"
every = 2
actions = { ship = '$T/ship' }
EOT
mkdir -p "$T/kinds/hello"
cat > "$T/kinds/hello/kind.toml" <<'EOT'
title = "hello {{1}}"
actions = { again = '$W_KIND_DIR/say again' }
[[steps]]
name = "say"
run = '$W_KIND_DIR/say "$@"'
EOT
printf '#!/bin/bash\necho "hello $1 from $W_KIND_DIR"\n' > "$T/kinds/hello/say"
# the observer prints whatever obs.txt says, or fails while obs.fail exists
printf '#!/bin/bash\n[ -e %s/obs.fail ] && { echo "source unreachable" >&2; exit 1; }\ncat %s/obs.txt 2>/dev/null; true\n' "$T" "$T" > "$T/observe"
# the ship action: quick, except for key s2, which takes long enough to be killed mid-ship
printf '#!/bin/bash\necho "shipping $W_KEY for #$W_ITEM"\n[ "$W_KEY" = s2 ] && sleep 60\n[ "$W_KEY" != s3 ]\n' > "$T/ship"
chmod +x "$T/kinds/hello/say" "$T/observe" "$T/ship"
export GAFFER_CONFIG=$T/alpha.toml

start_controller() { setsid "$BIN" controller >> "$T/controller.log" 2>&1 & echo $! > "$T/controller.pid"
  for _ in $(seq 50); do [ -s "$T/token" ] && "$BIN" ls >/dev/null 2>&1 && return; sleep 0.1; done; echo "controller didn't come up"; exit 1; }
start_agent() { GAFFER_CONFIG=$T/$1.toml GAFFER_BOOT_ID=$BOOT setsid "$BIN" agent >> "$T/$1.log" 2>&1 & echo $! > "$T/$1.pid"; }
kill9() { kill -9 "$(cat "$T/$1.pid")" 2>/dev/null; sleep 0.3; }
cleanup() { for p in controller alpha beta; do kill9 $p; done
  for f in "$T"/*/jobs/*/pid.*; do [ -f "$f" ] && kill -9 -"$(jq -r .pid "$f")" 2>/dev/null; done; }
trap cleanup EXIT

g() { "$BIN" "$@"; }
submit() { g submit "$@" | sed -n 's/^#\([0-9]*\) queued.*/\1/p'; }
f() { g ls -a --json | jq -r ".items[]|select(.id==$1)|$2"; }
wait_for() { # wait_for ID JQ-EXPR-THAT-IS-TRUE [timeout]
  local i; for i in $(seq $((${3:-60} * 4))); do [ "$(f "$1" "$2")" = true ] && return 0; sleep 0.25; done
  echo "    timeout on #$1: $2  (state $(f "$1" .state), done $(f "$1" '.progress.done // 0'))"; return 1; }
state() { wait_for "$1" "(.state==\"$2\")" "${3:-60}"; }
done_ge() { wait_for "$1" "((.progress.done // 0) >= $2)" "${3:-60}"; }
check() { local d=$1; shift; if "$@"; then echo "ok    $d"; PASS=$((PASS+1)); else echo "FAIL  $d"; FAIL=$((FAIL+1)); fi; }
jobpg() { local id=$1 a; a=$(jq -r .attempt "$T/${2:-alpha}/jobs/$id/job.json"); s=$(jq -r .step "$T/${2:-alpha}/jobs/$id/job.json"); jq -r .pid "$T/${2:-alpha}/jobs/$id/pid.$a.$s"; }
eq() { [ "$1" = "$2" ] || { echo "    got '$1', want '$2'"; return 1; }; }
haslog() { g log -n 100000 "$1" | grep -q "$2" || { echo "    log of #$1 lacks '$2'"; return 1; }; }
# the resume point must be the last ::checkpoint printed before the interrupt, whatever the timing
resumed_at_last() { g log -n 100000 "$1" | awk '/^::checkpoint unit=/{split($2,a,"="); last=a[2]} /^──── gaffer: .* attempt 2 /{want=last} /^resuming at unit /{got=$4} END{if (want=="" || got!=want) {print "    resumed at \"" got "\", last checkpoint before the interrupt was \"" want "\""; exit 1}}'; }
lacks() { ! g log -n 100000 "$1" | grep -q "$2" || { echo "    log of #$1 has '$2'"; return 1; }; }

start_controller; start_agent alpha
echo "scratch: $T"

echo "-- progress: push, regex, poll"
a=$(submit fake --host alpha -e FAKE_SLEEP=0.2 -- 10)
b=$(submit fake --host alpha -e FAKE_SLEEP=0.2 -e FAKE_MODE=regex -- 10)
check "push protocol reaches review"  state "$a" needs-you 40
check "push progress is 10/10"        eq "$(f "$a" .progress.done)/$(f "$a" .progress.total)" 10/10
check "regex progress reaches review" state "$b" needs-you 40
check "regex progress is 10/10"       eq "$(f "$b" .progress.done)" 10
c=$(submit fake --host alpha -e FAKE_SLEEP=0.5 -e FAKE_MODE=poll -- 24)
check "poll progress shows up"        done_ge "$c" 3 30
check "rate + ETA derived"            wait_for "$c" '(.rate > 0 and .eta > 0)' 20
check "poll job reaches review"       state "$c" needs-you 40
check "ack: needs-you -> done"        eval 'g ack "$a" >/dev/null && eq "$(f "$a" .state)" done'

echo "-- agent kill -9 mid-step: job keeps running, agent re-adopts it"
j=$(submit fake --host alpha -e FAKE_SLEEP=0.3 -- 30)
done_ge "$j" 5 30; kill9 alpha; sleep 3; start_agent alpha
check "adopted job finishes"          state "$j" needs-you 60
check "same attempt, no interrupt"    eq "$(f "$j" .attempt)/$(f "$j" .retries)" 1/0

echo "-- job kill -9 (OOM killer): resumes from its checkpoint"
j=$(submit fake --host alpha -e FAKE_SLEEP=0.3 -- 30)
done_ge "$j" 12 30; kill -9 -"$(jobpg "$j")"
check "interrupted job finishes"      state "$j" needs-you 60
check "attempt 2, one interrupt"      eq "$(f "$j" .attempt)/$(f "$j" .retries)" 2/1
check "resumed at the last checkpoint" resumed_at_last "$j"

echo "-- simulated reboot: everything dies, agent comes back with a new boot id"
j=$(submit fake --host alpha -e FAKE_SLEEP=0.3 -- 30)
done_ge "$j" 7 30; kill9 alpha; kill -9 -"$(jobpg "$j")"; BOOT=simulated-reboot-1; sleep 1; start_agent alpha
check "rebooted job finishes"         state "$j" needs-you 60
check "events say machine rebooted"   eval 'g show "$j" | grep -q "machine rebooted"'
check "resumed at the last checkpoint" resumed_at_last "$j"

echo "-- shutdown without the hook: the OS TERMs the step, it exits 0, the box reboots"
j=$(submit fake --host alpha -e FAKE_SLEEP=0.3 -e FAKE_TERM_RC=0 -- 30)
done_ge "$j" 7 30; kill9 alpha; kill -TERM -"$(jobpg "$j")"; sleep 2; BOOT=simulated-reboot-2; start_agent alpha
check "unwatched exit 0 = interrupted" eval 'wait_for "$j" "(.attempt==2)" 30 && g show "$j" | grep -q "exited 0 unwatched"'
check "  job finishes on attempt 2"    state "$j" needs-you 60
check "  from the TERM checkpoint"     resumed_at_last "$j"

echo "-- a stop signal gaffer didn't send (logout, a stray kill): interrupted, not failed"
j=$(submit fake --host alpha -e FAKE_SLEEP=0.3 -- 30)
done_ge "$j" 7 30; kill -HUP -"$(jobpg "$j")"
check "HUP from outside = interrupted"  eval 'wait_for "$j" "(.attempt==2)" 30 && g show "$j" | grep -q "killed by SIGHUP from outside gaffer"'
check "  not a failure: host not held" eq "$(g ls --json | jq -r '.hosts[]|select(.host=="alpha")|.held')" false
check "  job finishes on attempt 2"    state "$j" needs-you 60

echo "-- hold / release / drain / shutdown"
g hold alpha >/dev/null
j=$(submit fake --host alpha -e FAKE_SLEEP=0.3 -- 30); sleep 3
check "held host takes nothing"       eq "$(f "$j" .state)/$(g ls --json | jq -r '.hosts[]|select(.host=="alpha")|.held')" "queued/true"
check "the board says so"             eval 'g ls | grep -q "alpha HELD: held by hand"'
g release alpha >/dev/null
check "released: it runs"             done_ge "$j" 3 30
check "drain pauses what runs there"  eval 'g drain alpha | grep -q "^#$j -> " && state "$j" paused 20'
check "  and holds the host"          eq "$(g ls --json | jq -r '.hosts[]|select(.host=="alpha")|.held')" true
g release alpha >/dev/null; p=$(f "$j" .progress.done); g resume "$j" >/dev/null
wait_for "$j" '(.attempt==2 and .state=="running")' 30 >/dev/null; done_ge "$j" $((p+2)) 30 >/dev/null
kill9 controller   # the network is the first thing to go at shutdown: the hook must not need it
check "shutdown pauses without the controller" eval 'timeout 30 "$BIN" shutdown 2>&1 | tee "$T/shutdown.out" | grep -q "1 paused, safe to go down"'
check "  nothing left running"        eq "$(jq -r .state "$T/alpha/jobs/$j/job.json")" stopped
kill9 alpha; BOOT=simulated-reboot-3; start_controller; start_agent alpha
check "  reported after the reboot: paused" state "$j" paused 30
g resume "$j" >/dev/null
check "  and resumes from there"      state "$j" needs-you 60
check "  inside the step"             haslog "$j" "resuming at unit"
check "shutdown with nothing running" eval '"$BIN" shutdown | grep -q "nothing running here"'
check "a second agent is refused"     eval 'GAFFER_CONFIG=$T/alpha.toml timeout 10 "$BIN" agent 2>&1 | grep -q "another agent holds"'

echo "-- pause = TERM, checkpoint, free the slot; resume picks up there"
j=$(submit fake --host alpha -e FAKE_SLEEP=0.3 -- 40)
done_ge "$j" 8 30; g pause "$j" >/dev/null
check "paused"                        state "$j" paused 20
check "checkpoint taken on TERM"      wait_for "$j" '((.checkpoint.data.unit|tonumber) >= 8)' 5
g resume "$j" >/dev/null
check "resumed job finishes"          state "$j" needs-you 60
check "resumed at the TERM checkpoint" haslog "$j" "resuming at unit"
j=$(submit fake --host alpha -e FAKE_SLEEP=0.3 -- 40)
done_ge "$j" 3 30; g pause "$j" >/dev/null; state "$j" paused 20 >/dev/null; g cancel "$j" >/dev/null
check "cancel from paused says canceled" eq "$(f "$j" .reason)" canceled

echo "-- exit 0 after TERM = paused, resumes inside the step"
j=$(submit fake --host alpha -e FAKE_SLEEP=0.3 -e FAKE_TERM_RC=0 -- 40)
done_ge "$j" 8 30; g pause "$j" >/dev/null
check "exit 0 on TERM pauses"            state "$j" paused 20
check "  in the step it stopped in"      eq "$(f "$j" .step)" 1
g resume "$j" >/dev/null
check "  resumed job finishes"           state "$j" needs-you 60
check "  from the TERM checkpoint"       haslog "$j" "resuming at unit"

echo "-- failure, then the circuit breaker holds the host"
x=$(submit fake --host alpha -e FAKE_SLEEP=0.1 -e FAKE_FAIL=3 -- 10)
check "failed"                        state "$x" failed 30
check "reason names the error"        eval 'f "$x" .reason | grep -q "fake failure at unit 3"'
y=$(submit fake --host alpha -e FAKE_SLEEP=0.1 -e FAKE_FAIL=2 -- 10)
state "$y" failed 30 >/dev/null
check "two in a row holds alpha"      eq "$(g ls --json | jq -r '.hosts[]|select(.host=="alpha")|.held')" true
g release alpha >/dev/null
check "retry from scratch"            eval 'g retry "$x" >/dev/null && wait_for "$x" "(.state==\"running\")" 20'
g cancel "$x" >/dev/null; state "$x" canceled 20 >/dev/null
check "ack files a failure, keeps it"   eval 'g ack "$y" >/dev/null && [ "$(f "$y" .state)" = failed ] && [ "$(f "$y" .acked)" -gt 0 ]'
check "acked failure leaves FAILED"   eval '! g ls | sed -n "/^FAILED/,/^\$/p" | grep -q "^ *$y "'
check "second ack is refused"         eval '! g ack "$y" >/dev/null 2>&1'

echo "-- stall: alive but not moving"
s=$(submit fake --host alpha --stall 4 -e FAKE_SLEEP=0.2 -e FAKE_HANG=3 -- 10)
check "flagged stalled"               wait_for "$s" '(.stalled==true)' 30
check "cancel stops it"               eval 'g cancel "$s" >/dev/null && state "$s" canceled 20'

echo "-- needs-you from inside a running job"
q=$(submit fake --host alpha -e FAKE_SLEEP=0.4 -e FAKE_ASK=3 -- 12)
check "attention raised mid-run"      wait_for "$q" '((.attention // "")|test("wants a decision"))' 30
check "board puts it first"           eval '[ "$(g ls --json | jq -r ".items[0].id")" = "$q" ]'
g ack "$q" >/dev/null
check "ack clears it, job continues"  wait_for "$q" '((.attention // "")=="" and .state=="running")' 10
check "then ends in review"           state "$q" needs-you 40
q=$(submit fake --host alpha -e FAKE_SLEEP=0.2 -e FAKE_ASK=2 -- 6)
state "$q" needs-you 40 >/dev/null
check "unanswered question -> review" eq "$(f "$q" .attention)" review

echo "-- controller kill -9 mid-job"
j=$(submit fake --host alpha -e FAKE_SLEEP=0.3 -- 30)
done_ge "$j" 5 30; kill9 controller; sleep 4; start_controller
check "job unaffected, finishes"      state "$j" needs-you 60
check "same attempt"                  eq "$(f "$j" .attempt)" 1
check "full log made it across"       haslog "$j" "verified"

echo "-- restart policy"
r=$(submit fake --host alpha --on-interrupt restart -e FAKE_SLEEP=0.3 -- 20)
done_ge "$r" 8 30; kill -9 -"$(jobpg "$r")"
check "restarted job finishes"        state "$r" needs-you 60
check "did not resume"                lacks "$r" "resuming at unit"

echo "-- observed work (gaffer doesn't run it)"
o=$(g track sab pull something)
g emit "$o" progress 3/10 unit=files
check "emit sets progress"            eq "$(f "$o" .progress.done)/$(f "$o" .progress.unit)" 3/files
g emit "$o" ::done
check "emit ::done finishes it"       eq "$(f "$o" .state)" done

echo "-- a kind from the second kinds dir"
h=$(submit hello --host alpha -- world)
check "runs from its own dir"         eval 'state "$h" done 30 && wait_for "$h" "(.log_size > 0)" 10 && haslog "$h" "hello world from $T/kinds/hello"'

echo "-- observers: the source's snapshot is the truth"
okey() { local i id; for i in $(seq 60); do id=$(g ls -a --json | jq -r ".items[]|select(.source==\"ext\" and .key==\"$1\")|.id"); [ -n "$id" ] && { echo "$id"; return; }; sleep 0.25; done; }
src() { g ls --json | jq -r ".sources[]|select(.name==\"ext\")|$1"; }
src_is() { local i; for i in $(seq 60); do [ "$(src "$1")" = "$2" ] && return 0; sleep 0.25; done; echo "    source $1 = $(src "$1")"; return 1; }
cat > "$T/obs.txt" <<'EOT'
::item r1 title="ext render" state=running
::progress done=5 total=10 unit=frames
::item v1 title="ext review" state=needs-you
EOT
o1=$(okey r1); o2=$(okey v1)
check "snapshot creates items"        eq "$(f "$o1" .progress.done)/$(f "$o1" .progress.total) $(f "$o2" .state)/$(f "$o2" .attention)" "5/10 needs-you/review"
check "observed work holds no slot"   eq "$(g ls --json | jq -r '.hosts[]|select(.host=="alpha")|.used.cpu // 0')" 0
check "pause is refused"              eval '! g pause "$o1" 2>/dev/null'
sed -i 's/done=5/done=8/' "$T/obs.txt"
check "progress follows the source"   done_ge "$o1" 8 15
g ack "$o2" >/dev/null; sleep 5
check "ack sticks across snapshots"   eq "$(f "$o2" .state)" done
touch "$T/obs.fail"
check "failing observer shows"        src_is .ok false
check "and changes nothing"           eq "$(f "$o1" .state)" running
check "error is on the board"         eval 'g ls | grep -q "FAILING on alpha: .*source unreachable"'
rm -f "$T/obs.fail"; sed -i '1,2d' "$T/obs.txt"
check "gone from the snapshot = done" wait_for "$o1" '(.state=="done" and .reason=="gone from ext")' 15
: > "$T/obs.txt"

echo "-- actions: a job of their own, on the owner's host, held if interrupted"
actid() { sed -n 's/.* #\([0-9]*\) queued$/\1/p'; }
cat > "$T/obs.txt" <<'EOT'
::item s1 title="ext shippable" state=needs-you actions=ship
::item s2 title="ext slow ship" state=needs-you actions=ship
::item r2 title="ext still rendering" state=running
::item s3 title="ext ship that half-works" state=needs-you actions=ship
EOT
p1=$(okey s1); p2=$(okey s2); p3=$(okey r2); p4=$(okey s3)
wait_for "$p4" '((.actions // [])|index("ship") != null)' 15 >/dev/null
a1=$(g ship "$p1" | actid)
check "ship runs as a job"            eval 'state "$a1" done 30 && wait_for "$a1" "(.log_size > 0)" 10 && haslog "$a1" "shipping s1 for #$p1"'
check "on the observer's host, linked" eq "$(f "$a1" .host)/$(f "$a1" .parent)" "alpha/$p1"
check "the item notes the outcome"    eval 'g show "$p1" | grep -q "ship #$a1 done"'
check "not on offer: refused"         eval '! g ship "$p3" 2>/dev/null'
a2=$(g ship "$p2" | actid)
check "a second ship is refused"      eval '! g ship "$p2" 2>/dev/null'
check "an action can't be paused"     eval '! g pause "$a2" 2>/dev/null'
wait_for "$a2" '(.state=="running")' 20 >/dev/null; sleep 1; kill -9 -"$(jobpg "$a2")"
check "interrupted action holds"      state "$a2" held 30
g cancel "$a2" >/dev/null
a4=$(g ship "$p4" | actid)
check "a failing ship fails"          state "$a4" failed 30
sed -i 's/^::item s3 .*/::item s3 title="ext ship that half-works" state=done/' "$T/obs.txt"
check "source says done: failure filed" wait_for "$a4" '((.acked // 0) > 0)' 15
check "and says why"                  eval 'g show "$a4" | grep -q "is done per ext"'
a3=$(g again "$h" | actid)
check "kind action, from its kind dir" eval 'state "$a3" done 30 && wait_for "$a3" "(.log_size > 0)" 10 && haslog "$a3" "hello again from $T/kinds/hello"'
a5=$(g ship "$b" | actid)
check "kind action on a review item"   state "$a5" done 30
check "  files the review: done"       eval 'wait_for "$b" "(.state==\"done\" and .reason==\"ship #$a5 done\")" 10'
check "  and says so"                  eval 'g show "$b" | grep -q "review closed by ship #$a5 done"'
: > "$T/obs.txt"

echo "-- an observer taken out of the config is retired, not silent"
echo '::item z1 title="ext leftover" state=running' > "$T/obs.txt"
z=$(okey z1)
sed -i '/^\[\[agent.observe\]\]/,$d' "$T/alpha.toml"
kill9 alpha; start_agent alpha
check "its items are done"            state "$z" done 20
check "the reason says so"            eq "$(f "$z" .reason)" "ext observer removed"
check "the source is gone"            eq "$(g ls --json | jq '[.sources[]|select(.name=="ext")]|length')" 0

echo "-- wait: block on many ids, exit code says how it went"
rc() { "$@" > "$T/wait.out" 2>&1; echo $?; }
w1=$(submit hello -- one); w2=$(submit hello -- two)
check "all done: 0"                   eq "$(rc timeout 30 "$BIN" wait "$w1" "$w2")" 0
check "a line per item"               eq "$(grep -c '^#[0-9]* done' "$T/wait.out")" 2
wf=$(submit fake --host alpha -e FAKE_SLEEP=0.1 -e FAKE_FAIL=3 -- 5)
ws=$(submit fake --host alpha -e FAKE_SLEEP=1 -- 60)
check "a failure returns first: 1"    eq "$(rc timeout 30 "$BIN" wait "$ws" "$wf")" 1
check "with the failed log's tail"    grep -q "│ error: fake failure at unit 3" "$T/wait.out"
check "the slow one still running"    eq "$(f "$ws" .state)" running
g cancel "$ws" >/dev/null
wq=$(submit fake --host alpha -e FAKE_SLEEP=0.3 -e FAKE_ASK=2 -- 40)
check "a mid-run question: 2"         eq "$(rc timeout 30 "$BIN" wait "$wq")" 2
check "says what it asks"             grep -q "ASKS: unit 2 wants a decision" "$T/wait.out"
g cancel "$wq" >/dev/null
check "canceled: 3"                   eq "$(state "$wq" canceled 20; rc "$BIN" wait "$wq")" 3
wk=$(submit fake --host alpha -e FAKE_SLEEP=0.3 -- 20)
( "$BIN" wait --events "$wk" > "$T/wait.ev" 2>&1; echo $? > "$T/wait.rc" ) &
done_ge "$wk" 3 >/dev/null; kill9 controller; sleep 2; start_controller
for _ in $(seq 120); do [ -s "$T/wait.rc" ] && break; sleep 0.25; done
check "survives a controller kill -9" eq "$(cat "$T/wait.rc" 2>/dev/null)" 2
check "events: says it lost it"       grep -q "controller unreachable" "$T/wait.ev"
check "events: and got it back"       grep -q "controller back" "$T/wait.ev"
check "events: ends in review"        grep -q "^#$wk needs-you" "$T/wait.ev"
check "events: not a flood"           eval '[ "$(wc -l < "$T/wait.ev")" -lt 30 ]'
wt=$(submit fake --host alpha -e FAKE_SLEEP=1 -- 60)
check "--timeout: 124"                eq "$(rc "$BIN" wait --timeout 2s "$wt")" 124
check "unknown id: 4"                 eq "$(rc "$BIN" wait 999999)" 4
check "log -f ends when it settles"   eval 'state "$wt" running 20 >/dev/null; g cancel "$wt" >/dev/null; timeout 30 "$BIN" log -f "$wt" >/dev/null'

echo "-- work runs where it was started; run behaves like typing it"
start_agent beta; sleep 3
rid() { g run "$@" | sed -n 's/^#\([0-9]*\) queued.*/\1/p'; }
ids=""; for _ in 1 2; do ids="$ids $(submit fake -e FAKE_SLEEP=0.2 -- 6)"; done
bids=""; for _ in 1 2; do bids="$bids $(GAFFER_CONFIG=$T/beta.toml submit fake -e FAKE_SLEEP=0.2 -- 6)"; done
xb=$(submit fake --host beta -e FAKE_SLEEP=0.2 -- 6)
for i in $ids $bids $xb; do state "$i" needs-you 60 >/dev/null; done
check "submitted on alpha: ran on alpha" eq "$(for i in $ids; do f "$i" .host; done | sort -u)" alpha
check "submitted on beta: ran on beta"   eq "$(for i in $bids; do f "$i" .host; done | sort -u)" beta
check "--host still says where"       eq "$(f "$xb" .host)" beta
mkdir -p "$T/mybin"; printf '#!/bin/bash\necho "found me; venv=$VIRTUAL_ENV"\n' > "$T/mybin/only-in-my-path"; chmod +x "$T/mybin/only-in-my-path"
r1=$(PATH=$T/mybin:$PATH VIRTUAL_ENV=/v/demo rid -- 'only-in-my-path && gaffer emit msg "emit still works"')
check "run carries PATH + venv"       eval 'state "$r1" done 30 && haslog "$r1" "found me; venv=/v/demo"'
check "and gaffer emit still found"   eval 'g show "$r1" | grep -q "emit still works" || haslog "$r1" "emit still works"'
r2=$(rid --progress tqdm -- 'printf " 37%%|###   | 3/8 [00:01<00:02, 2.1it/s]\r"; sleep 3')
check "--progress tqdm"               wait_for "$r2" '(.progress.done==3 and .progress.total==8)' 20
check "unknown preset refused"        eval '! g run --progress nope -- true 2>/dev/null'
check "no such agent: a note"         eval 'g run --host nowhere -- true 2>&1 | grep -q "no agent \"nowhere\""'

echo
g ls | head -12
echo
echo "$PASS passed, $FAIL failed   (scratch: $T)"
[ "$FAIL" = 0 ]
