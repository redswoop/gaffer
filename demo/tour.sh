#!/bin/bash
# demo/tour.sh: runs as a gaffer job on the host whose agent is the gaffer-agent user unit:
#   gaffer run --title "demo · tour" --host $(hostname -s) --stall -1 -- demo/tour.sh
# Waits for an
# ack, submits fake jobs on both hosts, then pauses, resumes and kills things while narrating on the board.
set -u
say() { echo "::msg $*"; echo "tour: $*"; }
step() { echo "::progress done=$1 total=8 unit=acts"; }
id_of() { sed -n 's/^#\([0-9]*\) queued.*/\1/p'; }
f() { gaffer ls --json | jq -r ".items[]|select(.id==$1)|$2"; }
at() { local until=$(( START + $1 )); while [ "$(date +%s)" -lt "$until" ]; do sleep 1; done; }

OTHER=$(gaffer ls --json | jq -r --arg me "$W_HOST" '[.hosts[]|select(.online and .host!=$me)][0].host // $me')

step 0
echo "::needs-you run \`gaffer watch\` in a terminal, then \`gaffer ack $W_ID\` to start the tour"
say "waiting for your go-ahead"
until [ -n "$(f "$W_ID" '.attention // ""')" ]; do sleep 1; done   # our question has reached the board
until [ -z "$(f "$W_ID" '.attention // ""')" ]; do sleep 2; done   # ...and you've acked it
echo "::clear-needs-you"
START=$(date +%s)

step 1; say "submitting six jobs on $W_HOST + $OTHER"
A=$(gaffer submit fake --title "demo · long render" -e FAKE_SLEEP=1 -- 150 | id_of)
B=$(gaffer submit fake --title "demo · render on $OTHER" --host "$OTHER" -e FAKE_SLEEP=1 -- 120 | id_of)
C=$(gaffer submit fake --title "demo · log-scraped progress (regex)" --host "$W_HOST" -e FAKE_SLEEP=1 -e FAKE_MODE=regex -- 120 | id_of)
D=$(gaffer submit fake --title "demo · asks you a question" -e FAKE_SLEEP=1 -e FAKE_ASK=20 -- 80 | id_of)
E=$(gaffer submit fake --title "demo · wedges at unit 15" --stall 25 -e FAKE_SLEEP=1 -e FAKE_HANG=15 -- 120 | id_of)
F=$(gaffer submit fake --title "demo · fails at unit 25" -e FAKE_SLEEP=1 -e FAKE_FAIL=25 -- 100 | id_of)

at 15; step 2; say "at +40s: pause #$A (TERM -> it checkpoints -> slot freed)"
at 40; gaffer pause "$A"; say "paused #$A; resuming it at +60s from its checkpoint"
at 60; gaffer resume "$A"; step 3; say "resumed #$A (#$D asked you something at ~+25s: \`gaffer ack $D\`)"
at 75; step 4; say "at +85s: kill -9 the $W_HOST agent; jobs keep running, systemd restarts it, it re-adopts them"
at 85; kill -9 "$(systemctl --user show -p MainPID --value gaffer-agent.service)"; say "agent killed at $(date +%T); watch the board keep moving"
at 105; step 5; say "at +115s: kill -9 #$C's process group (OOM killer stand-in); it resumes from its checkpoint"
at 115
J=$(dirname "$W_JOBDIR")/$C
kill -9 -"$(jq -r .pid "$J/pid.$(jq -r .attempt "$J/job.json").$(jq -r .step "$J/job.json")")" && say "killed #$C at $(date +%T)"
at 140; step 6; say "#$E has been wedged since unit 15 (STALLED); canceling it at +150s"
at 150; gaffer cancel "$E"
at 160; step 7; say "#$F failed on its own at unit 25 (see its reason line); \`gaffer show $F\` for the history"
at 175; step 8; say "tour done. Leftovers: ack the reviews as they land (\`gaffer ack ID\`), \`gaffer show $A\` for #$A's full history"
