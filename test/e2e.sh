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
kinds = "$ROOT/kinds"
[agent]
host = "$1"
dir = "$T/$1"
slots = { cpu = 2 }
EOT
}
conf alpha; conf beta
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

echo "-- pause = TERM, checkpoint, free the slot; resume picks up there"
j=$(submit fake --host alpha -e FAKE_SLEEP=0.3 -- 40)
done_ge "$j" 8 30; g pause "$j" >/dev/null
check "paused"                        state "$j" paused 20
check "checkpoint taken on TERM"      wait_for "$j" '((.checkpoint.data.unit|tonumber) >= 8)' 5
g resume "$j" >/dev/null
check "resumed job finishes"          state "$j" needs-you 60
check "resumed at the TERM checkpoint" haslog "$j" "resuming at unit"

echo "-- failure, then the circuit breaker drains the host"
x=$(submit fake --host alpha -e FAKE_SLEEP=0.1 -e FAKE_FAIL=3 -- 10)
check "failed"                        state "$x" failed 30
check "reason names the error"        eval 'f "$x" .reason | grep -q "fake failure at unit 3"'
y=$(submit fake --host alpha -e FAKE_SLEEP=0.1 -e FAKE_FAIL=2 -- 10)
state "$y" failed 30 >/dev/null
check "two in a row drains alpha"     eq "$(g ls --json | jq -r '.hosts[]|select(.host=="alpha")|.drained')" true
g undrain alpha >/dev/null
check "retry from scratch"            eval 'g retry "$x" >/dev/null && wait_for "$x" "(.state==\"running\")" 20'
g cancel "$x" >/dev/null; state "$x" canceled 20 >/dev/null

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

echo "-- two agents share the queue"
start_agent beta; sleep 3
ids=""; for _ in 1 2 3 4; do ids="$ids $(submit fake -e FAKE_SLEEP=0.3 -- 12)"; done
sleep 4
check "both hosts got work"           eq "$(for i in $ids; do f "$i" .host; done | sort -u | grep -c .)" 2
for i in $ids; do state "$i" needs-you 60 >/dev/null; done
check "all four finished"             eq "$(for i in $ids; do f "$i" .state; done | sort -u)" needs-you

echo
g ls | head -12
echo
echo "$PASS passed, $FAIL failed   (scratch: $T)"
[ "$FAIL" = 0 ]
