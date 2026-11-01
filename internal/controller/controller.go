// Package controller owns the queue. It never touches a process: agents report what is true on their
// host, and every decision here is made off those reports (level-triggered), so a lost message, a
// restart of either side or a long network outage converges instead of drifting.
package controller

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gaffer/internal/config"
	"gaffer/internal/proto"
)

const (
	offlineAfter      = 30 * 1000
	offlineAlertAfter = 5 * 60 * 1000
	defaultStall      = 15 * 60
	defaultRetries    = 3
	breakerFailures   = 2
)

func envSeconds(name string, def int64) int64 {
	if s := os.Getenv(name); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return int64(n) * 1000
		}
	}
	return def * 1000
}

// rejectBackoff is how long a fits rejection keeps an item off that host, ms. Every rejected item on a
// host retries on the same beat (expiry aligned to this grid), head of the queue first. Tests shrink it.
var rejectBackoff = envSeconds("GAFFER_REJECT_BACKOFF", 30)

// yieldDebounce: a lease must be gone this long before the items that yielded to it are requeued, so a
// holder that comes right back (a chat reply) doesn't cost a stop and a restart.
var yieldDebounce = envSeconds("GAFFER_YIELD_DEBOUNCE", 30)

// preemptBackoff: an urgent item whose victim yielded but which then didn't start itself (its fits said
// no) waits this long before it may bump anything again, instead of bouncing the victim every beat.
var preemptBackoff = envSeconds("GAFFER_PREEMPT_BACKOFF", 600)

// abortedShutdown: a job paused for a shutdown on a boot that is still running (the shutdown was aborted)
// resumes after this long with the host no longer holding. Longer than the hook's two-minute freshness.
var abortedShutdown = envSeconds("GAFFER_SHUTDOWN_ABORT", 150)

func nowMS() int64            { return time.Now().UnixMilli() }
func logf(f string, a ...any) { log.Printf(f, a...) }

type Controller struct {
	cfg       *config.Config
	db        *sql.DB
	mu        sync.Mutex
	changed   bool
	subMu     sync.Mutex
	subs      map[chan struct{}]struct{}
	leaseGone map[string]int64 // host/res -> when its last lease was first seen gone (debounce)
}

func Run(cfg *config.Config) error {
	if err := os.MkdirAll(filepath.Join(cfg.Data, "logs"), 0o755); err != nil {
		return err
	}
	if err := cfg.EnsureToken(); err != nil {
		return err
	}
	db, err := openDB(filepath.Join(cfg.Data, "gaffer.db"))
	if err != nil {
		return err
	}
	c := &Controller{cfg: cfg, db: db, subs: map[chan struct{}]struct{}{}, leaseGone: map[string]int64{}}
	go c.monitor()
	addr := cfg.ListenAddr()
	logf("controller on http://%s (db %s)", addr, filepath.Join(cfg.Data, "gaffer.db"))
	return http.ListenAndServe(addr, c.routes())
}

func (c *Controller) logPath(id int64) string {
	return filepath.Join(c.cfg.Data, "logs", fmt.Sprintf("%d.log", id))
}

func (c *Controller) logSize(id int64) int64 {
	st, err := os.Stat(c.logPath(id))
	if err != nil {
		return 0
	}
	return st.Size()
}

func (c *Controller) appendLog(ch proto.LogChunk) {
	if ch.Offset != c.logSize(ch.ID) || len(ch.Data) == 0 {
		return // out of step; the agent resends from log_have
	}
	f, err := os.OpenFile(c.logPath(ch.ID), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return
	}
	f.Write(ch.Data)
	f.Close()
}

func active(state string) bool {
	return state == proto.SAssigned || state == proto.SRunning || state == proto.SStopping
}

// sync is the whole agent protocol: fold the reports in, re-send what the agent should be doing,
// hand out new work, acknowledge finished attempts once their full log is here.
func (c *Controller) sync(req proto.SyncReq) proto.SyncResp {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := nowMS()
	resp := proto.SyncResp{LogHave: map[int64]int64{}}
	var prevBoot, prevLeases string
	var offlineAlerted bool
	c.db.QueryRow("SELECT boot_id, offline_alerted, leases FROM hosts WHERE host=?", req.Host).Scan(&prevBoot, &offlineAlerted, &prevLeases)
	leases := prevLeases
	if req.Leases != nil {
		leases = js(req.Leases)
	}
	c.db.Exec(`INSERT INTO hosts(host, boot_id, version, slots, seen, hold, leases, resources) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(host) DO UPDATE SET
		boot_id=excluded.boot_id, version=excluded.version, slots=excluded.slots, seen=excluded.seen, offline_alerted=0,
		hold=excluded.hold, leases=excluded.leases, resources=excluded.resources`,
		req.Host, req.BootID, req.Version, js(req.Slots), t, req.Hold, leases, js(req.Resources))
	if prevBoot != "" && prevBoot != req.BootID {
		c.alert("gaffer: "+req.Host+" rebooted", "agent is back; jobs paused for the shutdown resume by themselves, interrupted ones follow their kind's policy", "default", "arrows_counterclockwise")
	} else if offlineAlerted {
		c.alert("gaffer: "+req.Host+" back online", "", "low", "white_check_mark")
	}
	if leases != prevLeases {
		c.changed = true
	}

	if req.Observers != nil {
		c.retireSources(req.Host, req.Observers, t)
	}
	for _, ch := range req.Logs {
		c.appendLog(ch)
	}
	reported := map[int64]bool{}
	for _, r := range req.Jobs {
		resp.LogHave[r.ID] = c.logSize(r.ID)
		it := c.get(r.ID)
		if it == nil {
			resp.Ack = append(resp.Ack, proto.AckRef{ID: r.ID, Attempt: r.Attempt})
			continue
		}
		if r.Attempt != it.Attempt {
			if r.State == proto.JRunning { // an attempt we no longer want is still running: stop it
				resp.Stop = append(resp.Stop, proto.Stop{ID: r.ID, Mode: "cancel"})
				c.event(r.ID, "stale", fmt.Sprintf("attempt %d still running on %s; stopping it", r.Attempt, req.Host))
			} else {
				resp.Ack = append(resp.Ack, proto.AckRef{ID: r.ID, Attempt: r.Attempt})
			}
			continue
		}
		reported[r.ID] = true
		if r.State == proto.JRunning {
			c.running(it, r, req.Host)
			if it.StopMode != "" {
				s := proto.Stop{ID: it.ID, Mode: it.StopMode}
				if it.StopMode == "yield" {
					s.Res, s.For = it.YieldRes, yieldFor(it.YieldTo)
				}
				resp.Stop = append(resp.Stop, s)
			}
			continue
		}
		if active(it.State) && it.Host == req.Host && resp.LogHave[r.ID] >= r.LogSize { // settled = whole log here
			c.finished(it, r, req)
		}
		if resp.LogHave[r.ID] >= r.LogSize {
			resp.Ack = append(resp.Ack, proto.AckRef{ID: r.ID, Attempt: r.Attempt})
		}
	}

	// what we think is on this host but the agent didn't mention
	for _, it := range c.query("WHERE host=? AND state IN ('assigned','running','stopping') AND observed=0", req.Host) {
		if reported[it.ID] {
			continue
		}
		if it.State == proto.SAssigned {
			resp.Start = append(resp.Start, c.assignment(it))
			continue
		}
		c.interrupted(it, req.Host, "the agent has no record of it (job dir lost?)", nil, it.Step, "", "")
	}

	c.comeBack(req, t)
	resp.Start = append(resp.Start, c.schedule(req)...)
	if c.changed {
		c.changed = false
		c.notify()
	}
	return resp
}

// comeBack requeues, by itself, what stopped for a reason that has passed: a pause for a shutdown once
// the host is up on a new boot, a yield once the lease is gone (and stayed gone for the debounce) or
// the job it yielded to has left the slot. A human's pause is nobody's to undo.
func (c *Controller) comeBack(req proto.SyncReq, t int64) {
	for _, it := range c.query("WHERE host=? AND state='paused' AND paused_by='shutdown' AND observed=0", req.Host) {
		why := "paused for a shutdown; " + req.Host + " is back"
		if it.PausedBoot == req.BootID { // same boot: the box hasn't gone down. An aborted shutdown, once the hook is long gone
			if req.Hold != "" || t-it.Updated < abortedShutdown {
				continue
			}
			why = "paused for a shutdown that didn't happen"
		}
		it.State, it.Resume, it.Reason, it.PausedBy, it.PausedBoot = proto.SQueued, true, "resuming: "+why, "", ""
		c.event(it.ID, "resumed", why)
		c.put(it)
	}
	if req.Leases == nil {
		return // an agent that doesn't speak leases never yields anything
	}
	leased := byRes(req.Leases)
	for _, it := range c.query("WHERE host=? AND state='yielded' AND observed=0", req.Host) {
		if strings.HasPrefix(it.YieldTo, "#") { // yielded to a job: back when that job has left the slot
			p := c.get(yieldFor(it.YieldTo))
			switch {
			case p != nil && active(p.State) && p.Host == req.Host:
				continue // it has the slot
			case p != nil && p.State == proto.SQueued:
				// still queued: this sync's schedule() hands it the slot (comeBack runs first). Unless its fits
				// just said no, or it has been waiting far too long: then it bumped for nothing; back off.
				rejected := p.RejectHost == req.Host && t < p.RejectUntil
				if !rejected && t-it.YieldedAt < 2*rejectBackoff {
					continue
				}
				p.PreemptUntil = t + preemptBackoff
				c.event(p.ID, "preempt backoff", fmt.Sprintf("#%d yielded %s for it but it didn't start; it waits %s before bumping again", it.ID, it.YieldRes, dur(preemptBackoff)))
				c.put(p)
			}
			c.requeueYielded(it, "resuming: "+it.YieldTo+" left the "+it.YieldRes)
			continue
		}
		key := fmt.Sprintf("%s/%s/%d", req.Host, it.YieldRes, it.ID)
		if blocking(it, leased[it.YieldRes]) {
			delete(c.leaseGone, key)
			continue
		}
		if c.leaseGone[key] == 0 {
			c.leaseGone[key] = t
		}
		if t-c.leaseGone[key] < yieldDebounce {
			continue
		}
		delete(c.leaseGone, key)
		c.requeueYielded(it, "resuming: "+it.YieldTo+" left the "+it.YieldRes)
	}
}

func (c *Controller) requeueYielded(it *rec, why string) {
	it.State, it.Resume, it.Reason, it.PausedBy = proto.SQueued, true, why, ""
	c.event(it.ID, "requeued", why)
	c.put(it)
}

// blocking: some lease on the resource still keeps this item off it (exclusive, or a class it doesn't share).
func blocking(it *rec, ls []proto.Lease) bool {
	for _, l := range ls {
		if l.Exclusive || !it.Spec.SharesAll(it.Step, []string{l.Class}) {
			return true
		}
	}
	return false
}

func yieldFor(yieldTo string) int64 {
	n, _ := strconv.ParseInt(strings.TrimPrefix(yieldTo, "#"), 10, 64)
	return n
}

func byRes(ls []proto.Lease) map[string][]proto.Lease {
	m := map[string][]proto.Lease{}
	for _, l := range ls {
		m[l.Res] = append(m[l.Res], l)
	}
	return m
}

func classes(ls []proto.Lease) []string {
	var out []string
	for _, l := range ls {
		out = append(out, l.Class)
	}
	return out
}

// leaseText names the holders of a resource for the board: "qwen (llm)" or "qwen (llm), comfy (comfy)".
func leaseText(ls []proto.Lease) string {
	parts := make([]string, 0, len(ls))
	for _, l := range ls {
		s := l.Holder
		if l.Class != "" && l.Class != l.Holder {
			s += " (" + l.Class + ")"
		}
		if l.Exclusive {
			s += " exclusive"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ", ")
}

func (c *Controller) assignment(it *rec) proto.Assign {
	a := proto.Assign{ID: it.ID, Attempt: it.Attempt, Step: it.Step, Resume: it.Resume, Spec: it.Spec}
	if it.Resume {
		a.Checkpoint = it.Checkpoint
	}
	return a
}

func (c *Controller) running(it *rec, r proto.JobReport, host string) {
	if !active(it.State) {
		return
	}
	if it.State == proto.SAssigned {
		it.State, it.Reason, it.RejectHost, it.RejectUntil, it.YieldedAt, it.PreemptUntil = proto.SRunning, "", "", 0, 0, 0
		if it.StopMode != "" {
			it.State = proto.SStopping
		}
		c.event(it.ID, "running", fmt.Sprintf("attempt %d on %s, step %s", it.Attempt, host, r.StepName))
	}
	if it.Started == 0 {
		it.Started = r.Started
	}
	if it.Step != r.Step {
		c.event(it.ID, "step", r.StepName)
	}
	it.Step, it.StepName, it.Host = r.Step, r.StepName, host
	if r.StopMode != "" && it.StopMode == "" && it.State == proto.SRunning { // the agent is stopping it on its own
		it.State, it.Reason = proto.SStopping, "stopping ("+r.StopMode+", "+strings.TrimPrefix(r.StopBy, "lease:")+")"
	}
	// a yield for a resource the job has since handed back (its step moved on) is moot: let it run
	if it.StopMode == "yield" && it.YieldRes != "" && it.Spec.NeedsAt(it.Step) != it.YieldRes && r.StopMode == "" {
		c.event(it.ID, "yield moot", fmt.Sprintf("on step %s (%s) now, the %s is free anyway", r.StepName, it.Spec.NeedsAt(it.Step), it.YieldRes))
		it.StopMode, it.State, it.YieldRes, it.YieldTo = "", proto.SRunning, "", ""
	}
	c.fold(it, r.Progress, r.Checkpoint, r.Attention)
	c.put(it)
}

// fold takes a progress snapshot (from an agent or an emit) into the item.
func (c *Controller) fold(it *rec, p *proto.Progress, cp *proto.Checkpoint, attention string) {
	if p != nil {
		moved := it.Progress == nil || it.Progress.Done != p.Done
		if it.Progress != nil && p.Done < it.Progress.Done {
			c.db.Exec("DELETE FROM samples WHERE item=?", it.ID) // went backwards (restart): old rate is meaningless
		}
		if moved {
			var last int64
			c.db.QueryRow("SELECT COALESCE(MAX(ts),0) FROM samples WHERE item=?", it.ID).Scan(&last)
			if nowMS()-last >= 10000 || it.Progress == nil {
				c.db.Exec("INSERT INTO samples(item, ts, done) VALUES(?,?,?)", it.ID, p.TS, p.Done)
			}
		}
		if it.StallAlerted && (it.Progress == nil || p.TS != it.Progress.TS) {
			it.StallAlerted = false
		}
		it.Progress = p
	}
	if cp != nil {
		it.Checkpoint = cp
	}
	if attention != it.Attention {
		if attention != "" && attention != it.AttentionAcked {
			it.Attention = attention
			c.event(it.ID, "needs-you", attention)
			c.alertFor(it, "gaffer: #"+itoa(it.ID)+" needs you", it.Title+"\n"+attention, "high", "raising_hand")
		} else if attention == "" {
			it.Attention, it.AttentionAcked = "", ""
		}
	}
}

// ran adds an attempt's running time to the item's total (what `show` prints as "ran").
func ran(it *rec, r proto.JobReport) {
	if r.Started > 0 && r.Finished > r.Started {
		it.RunMS += r.Finished - r.Started
	}
}

func (c *Controller) finished(it *rec, r proto.JobReport, req proto.SyncReq) {
	host := req.Host
	c.fold(it, r.Progress, r.Checkpoint, r.Attention)
	it.Step, it.StepName = r.Step, r.StepName
	switch r.State {
	case proto.JDone:
		ran(it, r)
		it.State, it.Finished, it.Reason, it.Checkpoint, it.StopMode = proto.SDone, r.Finished, "", nil, ""
		c.db.Exec("UPDATE hosts SET failures=0 WHERE host=?", host)
		it.Attention = "" // a mid-run question nobody answered is moot once the job is over
		if it.Spec.Review {
			it.State, it.Attention = proto.SNeedsYou, "review"
			c.alertFor(it, "gaffer: #"+itoa(it.ID)+" ready for review", it.Title, "default", "eyes")
		}
		c.event(it.ID, it.State, "finished on "+host)
	case proto.JFailed:
		ran(it, r)
		it.State, it.Finished, it.Reason, it.StopMode, it.Attention = proto.SFailed, r.Finished, r.Reason, "", ""
		c.event(it.ID, "failed", r.Reason)
		c.alertFor(it, "gaffer: FAILED #"+itoa(it.ID)+" on "+host, it.Title+"\n"+r.Reason+"\ngaffer log "+itoa(it.ID), "high", "warning")
		c.breaker(host)
	case proto.JStopped:
		ran(it, r)
		c.stopped(it, r, req)
		c.event(it.ID, it.State, r.Reason)
		it.StopMode = ""
	case proto.JInterrupted:
		ran(it, r)
		c.interrupted(it, host, r.Reason, r.Checkpoint, r.Step, r.StopMode, r.StopBy)
		return
	case proto.JRejected:
		// Not now, says the kind's fits check: nothing started, so this wasn't an attempt. Every rejected
		// item on the host retries on the same beat (expiry aligned to the backoff grid), so when the
		// resource frees the head of the queue gets it, not whichever item's backoff happened to expire first.
		it.Attempt--
		it.State, it.RejectHost, it.RejectUntil = proto.SQueued, host, (nowMS()/rejectBackoff+1)*rejectBackoff
		c.waiting(it, "waiting: "+host+": "+r.Reason)
		if it.StopMode != "" {
			it.State, it.Reason, it.StopMode = map[string]string{"cancel": proto.SCanceled, "pause": proto.SPaused, "yield": proto.SQueued}[it.StopMode], "", ""
			if it.State == proto.SPaused {
				it.PausedBy = "human"
			}
		}
		it.Host = ""
	}
	if it.State != proto.SQueued {
		c.childEvent(it)
	}
	c.put(it)
}

// stopped decides what a stop that completed was: a cancel, a pause (and whose), or a yield. The order
// matters: a human's pause or cancel wins over anything the agent was doing on its own.
func (c *Controller) stopped(it *rec, r proto.JobReport, req proto.SyncReq) {
	host := req.Host
	boot := r.StopBoot
	if boot == "" {
		boot = req.BootID
	}
	switch {
	case it.StopMode == "cancel":
		it.State, it.Finished, it.Reason, it.Attention = proto.SCanceled, r.Finished, "canceled", ""
		return
	case it.StopMode == "pause":
		it.State, it.Reason, it.PausedBy, it.PausedBoot = proto.SPaused, "paused on "+host+" by you", "human", boot
	case r.StopMode == "yield" || it.StopMode == "yield":
		it.State, it.PausedBy, it.YieldedAt, it.YieldRes = proto.SYielded, "yield", nowMS(), it.Spec.NeedsAt(it.Step)
		it.YieldTo = yieldTarget(it, r, req)
		it.Reason = "yielding " + it.YieldRes + " to " + it.YieldTo + " on " + host
	default: // the agent stopped it on its own: the shutdown hook (or an older agent, which only did that)
		it.State, it.Reason, it.PausedBy, it.PausedBoot = proto.SPaused, "paused on "+host+" for a shutdown", "shutdown", boot
	}
	it.Resume = true
	if it.Checkpoint != nil || it.State == proto.SYielded {
		it.Affinity = host
	}
}

// yieldTarget names what a job yielded to: "#321" (the controller's own preemption) or the lease holder
// the agent named, dressed with its class from the agent's current lease list.
func yieldTarget(it *rec, r proto.JobReport, req proto.SyncReq) string {
	if strings.HasPrefix(it.YieldTo, "#") {
		return it.YieldTo
	}
	if strings.HasPrefix(r.StopBy, "#") {
		return r.StopBy
	}
	holder := strings.TrimPrefix(r.StopBy, "lease:")
	for _, l := range req.Leases {
		if l.Holder == holder {
			return leaseText([]proto.Lease{l})
		}
	}
	if holder == "" {
		return "a lease"
	}
	return holder
}

// interrupted applies the kind's policy to a job that vanished: resume where it was (step boundary,
// plus the kind's checkpoint if it printed one), restart from scratch, or hold for a human.
func (c *Controller) interrupted(it *rec, host, why string, cp *proto.Checkpoint, step int, stopMode, stopBy string) {
	if cp != nil {
		it.Checkpoint = cp
	}
	it.Retries++
	maxR := it.Spec.MaxRetries
	if maxR == 0 {
		maxR = defaultRetries
	}
	stopping, by := it.StopMode, ""
	if stopping == "" && stopMode != "" { // the agent was stopping it on its own (shutdown hook, a lease)
		stopping, by = stopMode, stopBy
	}
	it.Host, it.StopMode = "", ""
	switch stopping { // it died while being stopped: that's the stop
	case "pause":
		it.State, it.Reason, it.Resume, it.PausedBy = proto.SPaused, "paused ("+why+")", true, "human"
		if by == "shutdown" {
			it.Reason, it.PausedBy = "paused for a shutdown ("+why+")", "shutdown"
		}
		it.Retries--
		c.put(it)
		return
	case "yield":
		it.State, it.Resume, it.PausedBy, it.YieldedAt, it.YieldRes = proto.SYielded, true, "yield", nowMS(), it.Spec.NeedsAt(step)
		if it.YieldTo == "" {
			it.YieldTo = strings.TrimPrefix(by, "lease:")
		}
		it.Reason, it.Host, it.Affinity = "yielding "+it.YieldRes+" to "+it.YieldTo+" ("+why+")", host, host
		it.Retries--
		c.put(it)
		return
	case "cancel":
		it.State, it.Reason, it.Finished, it.Attention = proto.SCanceled, "canceled", nowMS(), ""
		c.put(it)
		return
	}
	policy := it.Spec.OnInterrupt
	switch {
	case it.Retries > maxR:
		it.State, it.Reason, it.Finished = proto.SFailed, fmt.Sprintf("interrupted %d times; last: %s", it.Retries, why), nowMS()
		c.alertFor(it, "gaffer: FAILED #"+itoa(it.ID), it.Title+"\n"+it.Reason, "high", "warning")
	case policy == "hold":
		it.State, it.Reason = proto.SHeld, "interrupted on "+host+": "+why+" (hold policy; `gaffer resume`)"
		it.Resume, it.Step = true, step
	case policy == "restart":
		it.State, it.Reason, it.Resume, it.Step, it.Checkpoint, it.Affinity = proto.SQueued, "restarting: "+why, false, 0, nil, ""
	default:
		it.State, it.Resume, it.Step = proto.SQueued, true, step
		how := "at step " + it.Spec.Steps[min(step, len(it.Spec.Steps)-1)].Name
		if it.Checkpoint != nil && it.Checkpoint.Step == step {
			how += fmt.Sprintf(" from checkpoint %v", it.Checkpoint.Data)
			it.Affinity = host
		}
		it.Reason = "resuming " + how + " (" + why + ")"
	}
	c.event(it.ID, "interrupted", host+": "+why+" -> "+it.State)
	if it.State != proto.SFailed {
		c.alertFor(it, "gaffer: #"+itoa(it.ID)+" interrupted on "+host, it.Title+"\n"+it.Reason, "default", "zap")
	}
	if it.State != proto.SQueued {
		c.childEvent(it)
	}
	c.put(it)
}

func (c *Controller) breaker(host string) {
	var n int
	c.db.QueryRow("UPDATE hosts SET failures=failures+1 WHERE host=? RETURNING failures", host).Scan(&n)
	if n >= breakerFailures {
		c.db.Exec("UPDATE hosts SET drained=1, note=? WHERE host=?", fmt.Sprintf("held after %d failures in a row", n), host)
		c.alert("gaffer: "+host+" held", fmt.Sprintf("%d failures in a row; fix, then `gaffer host release %s`", n, host), "high", "rotating_light")
	}
}

// waiting restates why a queued item isn't running: one `waiting` event per change of reason, however
// often the beat comes round, so a retry every 30 s doesn't flood the history. The text must not tick
// (no "idle 3m" in it); the ticking parts live on the host line of the board.
func (c *Controller) waiting(it *rec, text string) {
	if it.Reason == text {
		return
	}
	it.Reason = text
	c.event(it.ID, "waiting", strings.TrimPrefix(text, "waiting: "))
	c.put(it)
}

// schedule hands queued work to this host: priority then age (an item that already had the slot and
// yielded it goes first within its priority), while it has free slots of the label a job needs and no
// lease on that label it can't share. A job pinned by host or checkpoint affinity only goes there. Then,
// with the slots full, the most urgent waiting job may bump the least urgent holder (preempt_gap).
func (c *Controller) schedule(req proto.SyncReq) []proto.Assign {
	var held bool // the `drained` column: held by hand or by the breaker
	c.db.QueryRow("SELECT drained FROM hosts WHERE host=?", req.Host).Scan(&held)
	if held || req.Hold != "" { // or pausing everything for a shutdown: nothing new lands there
		return nil
	}
	used := map[string]int{}
	holders := map[string][]*rec{}
	for _, it := range c.query("WHERE host=? AND state IN ('assigned','running','stopping') AND observed=0", req.Host) {
		n := it.Spec.NeedsAt(it.Step)
		used[n]++
		holders[n] = append(holders[n], it)
	}
	leased := byRes(req.Leases)
	t := nowMS()
	// yielded work whose lease is gone is about to be requeued (after the debounce): its slot is spoken for,
	// or a queued item would slip in ahead of the job that already had it
	coming := map[string]bool{}
	for _, y := range c.query("WHERE host=? AND state='yielded' AND observed=0", req.Host) {
		if !strings.HasPrefix(y.YieldTo, "#") && !blocking(y, leased[y.YieldRes]) {
			coming[y.YieldRes] = true
		}
	}
	var out []proto.Assign
	var blocked []*rec // wanted a full slot here; preemption candidates in queue order
	for _, it := range c.query("WHERE state='queued' AND observed=0 ORDER BY priority, CASE WHEN yielded_at>0 THEN 0 ELSE 1 END, yielded_at, id") {
		n := it.Spec.NeedsAt(it.Step)
		if (it.Spec.Host != "" && it.Spec.Host != req.Host) || (it.Affinity != "" && it.Affinity != req.Host) {
			continue
		}
		if ls := leased[n]; len(ls) > 0 && !it.Spec.SharesAll(it.Step, classes(ls)) {
			c.waiting(it, n+": "+leaseText(ls)+" has it")
			continue
		}
		if it.RejectHost == req.Host && t < it.RejectUntil {
			continue
		}
		if coming[n] && it.YieldedAt == 0 {
			c.waiting(it, "behind yielded work coming back to the "+n)
			continue
		}
		if req.Slots[n] <= used[n] {
			if first := firstRunning(holders[n]); first != nil {
				c.waiting(it, "behind #"+itoa(first.ID)+" for "+n)
			}
			blocked = append(blocked, it)
			continue
		}
		it.Attempt++
		retry := it.RejectHost == req.Host // another go after a fits rejection: already on the record
		it.State, it.Host = proto.SAssigned, req.Host
		if !retry {
			it.Reason = ""
		}
		c.put(it)
		if !retry {
			c.event(it.ID, "assigned", fmt.Sprintf("attempt %d to %s", it.Attempt, req.Host))
		}
		out = append(out, c.assignment(it))
		used[n]++
		holders[n] = append(holders[n], it)
	}
	c.preempt(blocked, holders, leased, t)
	return out
}

func firstRunning(hs []*rec) *rec {
	var first *rec
	for _, h := range hs {
		if h.State == proto.SRunning && (first == nil || h.ID < first.ID) {
			first = h
		}
	}
	return first
}

// preempt: the most urgent blocked job (that isn't in preempt backoff) bumps the least urgent running
// holder of its slot when the gap in priority is at least preempt_gap. One bump in flight per resource
// per host; never an action, never a kind that would restart from scratch, never while a lease holds
// the resource.
func (c *Controller) preempt(blocked []*rec, holders map[string][]*rec, leased map[string][]proto.Lease, t int64) {
	gap := c.cfg.Gap()
	if gap < 0 {
		return
	}
	done := map[string]bool{}
	for _, cand := range blocked {
		n := cand.Spec.NeedsAt(cand.Step)
		if done[n] || len(leased[n]) > 0 || t < cand.PreemptUntil {
			continue
		}
		done[n] = true // the most urgent waiting job speaks for the resource this beat
		var victim *rec
		inflight := false
		for _, h := range holders[n] {
			if h.StopMode == "yield" {
				inflight = true
			}
			if h.State != proto.SRunning || h.StopMode != "" || h.Parent != 0 || h.Spec.OnInterrupt == "hold" || h.Spec.OnInterrupt == "restart" {
				continue
			}
			if victim == nil || h.Priority > victim.Priority || (h.Priority == victim.Priority && h.ID > victim.ID) {
				victim = h
			}
		}
		if inflight || victim == nil || victim.Priority < cand.Priority+gap {
			continue
		}
		victim.StopMode, victim.State, victim.YieldRes, victim.YieldTo = "yield", proto.SStopping, n, "#"+itoa(cand.ID)
		c.event(victim.ID, "yield", fmt.Sprintf("yielding %s to #%d (%s, priority %d vs %d)", n, cand.ID, cand.Title, cand.Priority, victim.Priority))
		c.put(victim)
		cand.PreemptUntil = t + preemptBackoff // provisional: cleared when it actually runs
		c.waiting(cand, "waiting for #"+itoa(victim.ID)+" to yield "+n)
		c.put(cand)
	}
}

// monitor raises the alerts nobody asks for: hosts gone quiet, jobs that stopped moving.
func (c *Controller) monitor() {
	for range time.Tick(10 * time.Second) {
		c.mu.Lock()
		t := nowMS()
		rows, _ := c.db.Query("SELECT host, seen, offline_alerted FROM hosts")
		type h struct {
			host    string
			seen    int64
			alerted bool
		}
		var hs []h
		for rows != nil && rows.Next() {
			var x h
			rows.Scan(&x.host, &x.seen, &x.alerted)
			hs = append(hs, x)
		}
		if rows != nil {
			rows.Close()
		}
		for _, x := range hs {
			if t-x.seen > offlineAlertAfter && !x.alerted {
				var n int
				c.db.QueryRow("SELECT COUNT(*) FROM items WHERE host=? AND state IN ('assigned','running','stopping')", x.host).Scan(&n)
				if n > 0 {
					c.alert("gaffer: "+x.host+" unreachable", fmt.Sprintf("no agent sync for %s; %d job(s) on it", dur(t-x.seen), n), "high", "warning")
				}
				c.db.Exec("UPDATE hosts SET offline_alerted=1 WHERE host=?", x.host)
			}
		}
		for _, it := range c.query("WHERE state IN ('running','stopping') AND stall_alerted=0") {
			if c.stalled(it, t) {
				it.StallAlerted = true
				c.put(it)
				c.event(it.ID, "stalled", "no progress for "+dur(t-c.lastMove(it)))
				c.alertFor(it, "gaffer: #"+itoa(it.ID)+" stalled", it.Title+"\nno progress for "+dur(t-c.lastMove(it)), "default", "hourglass")
			}
		}
		c.watchSources(t)
		c.changed = false
		c.mu.Unlock()
		c.notify() // also keeps ETAs and host liveness fresh for watchers
	}
}

func (c *Controller) lastMove(it *rec) int64 {
	if it.Progress != nil && it.Progress.TS > 0 {
		return it.Progress.TS
	}
	if it.Started > 0 {
		return it.Started
	}
	return it.Updated
}

func (c *Controller) stalled(it *rec, t int64) bool {
	if it.State != proto.SRunning && it.State != proto.SStopping {
		return false
	}
	after := it.Spec.StallAfter
	if after < 0 {
		return false
	}
	if after == 0 {
		after = defaultStall
	}
	return t-c.lastMove(it) > int64(after)*1000
}

// derive fills rate / ETA / stalled. Rate is the slope over the last 10 minutes of samples (or since
// the first one, early on); a job's own ETA wins when it states one.
func (c *Controller) derive(it *rec, t int64) proto.Item {
	out := it.Item
	out.Actions = c.offers(it)
	out.Stalled = c.stalled(it, t)
	out.LogSize = c.logSize(it.ID)
	p := it.Progress
	if p == nil {
		return out
	}
	var t0 int64
	var d0 float64
	err := c.db.QueryRow("SELECT ts, done FROM samples WHERE item=? AND ts>=? ORDER BY ts LIMIT 1", it.ID, t-600000).Scan(&t0, &d0)
	if err != nil || p.TS-t0 < 30000 {
		c.db.QueryRow("SELECT ts, done FROM samples WHERE item=? ORDER BY ts LIMIT 1", it.ID).Scan(&t0, &d0)
	}
	if t0 > 0 && p.TS-t0 >= 5000 && p.Done > d0 {
		out.Rate = (p.Done - d0) / (float64(p.TS-t0) / 1000)
	}
	if active(it.State) {
		switch {
		case p.ETA > 0:
			out.ETA = max(0, p.ETA-float64(t-p.TS)/1000)
		case p.Total > 0 && out.Rate > 0:
			out.ETA = max(0, (p.Total-p.Done)/out.Rate-float64(t-p.TS)/1000)
		}
	}
	return out
}

func (c *Controller) status(all bool) proto.Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := nowMS()
	st := proto.Status{Now: t, Hosts: []proto.Host{}, Sources: c.sources(t), Items: []proto.Item{}}
	rows, err := c.db.Query("SELECT host, boot_id, version, slots, seen, drained, note, failures, hold, leases, resources FROM hosts ORDER BY host")
	if err == nil {
		for rows.Next() {
			var h proto.Host
			var slots, hold, leases, resources string
			rows.Scan(&h.Host, &h.BootID, &h.Version, &slots, &h.Seen, &h.Held, &h.Note, &h.Failures, &hold, &leases, &resources)
			json.Unmarshal([]byte(slots), &h.Slots)
			json.Unmarshal([]byte(leases), &h.Leases)
			json.Unmarshal([]byte(resources), &h.Resources)
			h.Online = t-h.Seen < offlineAfter
			if hold != "" && h.Online {
				h.Held, h.Note = true, "pausing everything for a "+hold
			}
			h.Used = map[string]int{}
			st.Hosts = append(st.Hosts, h)
		}
		rows.Close()
	}
	where := "WHERE (state NOT IN ('done','canceled') AND NOT (state='failed' AND acked>0)) OR finished > ?"
	cut := t - 24*3600*1000
	if all {
		cut = 0
	}
	for _, it := range c.query(where, cut) {
		st.Items = append(st.Items, c.derive(it, t))
		for i := range st.Hosts {
			if st.Hosts[i].Host == it.Host && active(it.State) && !it.Observed {
				st.Hosts[i].Used[it.Spec.NeedsAt(it.Step)]++
			}
		}
	}
	for i := range st.Hosts { // a leased resource reads as in use even with no job on it
		for res, ls := range byRes(st.Hosts[i].Leases) {
			if len(ls) > 0 && st.Hosts[i].Used[res] < st.Hosts[i].Slots[res] {
				st.Hosts[i].Used[res] = st.Hosts[i].Slots[res]
			}
		}
	}
	sort.SliceStable(st.Items, func(a, b int) bool {
		ra, rb := rank(st.Items[a]), rank(st.Items[b])
		if ra != rb {
			return ra < rb
		}
		if st.Items[a].Priority != st.Items[b].Priority {
			return st.Items[a].Priority < st.Items[b].Priority
		}
		return st.Items[a].ID < st.Items[b].ID
	})
	return st
}

// rank orders the board: a running job asking a question (it may be blocked on you), then finished
// work waiting for review, failures, stalls, what's moving, what stepped aside, what's waiting, history.
func rank(it proto.Item) int {
	switch {
	case it.Attention != "" && active(it.State):
		return 0
	case it.State == proto.SNeedsYou:
		return 1
	case it.State == proto.SFailed && it.Acked == 0:
		return 2
	case it.Stalled:
		return 3
	case active(it.State):
		return 4
	case it.State == proto.SYielded:
		return 5
	case it.State == proto.SQueued:
		return 6
	case it.State == proto.SPaused || it.State == proto.SHeld:
		return 7
	}
	return 8
}

// alertFor is an item's push, unless the item is quiet (something else already pings for it).
func (c *Controller) alertFor(it *rec, title, msg, prio, tags string) {
	if it.Spec.Quiet {
		logf("quiet #%d %s", it.ID, title)
		return
	}
	c.alert(title, msg, prio, tags)
}

func (c *Controller) alert(title, msg, prio, tags string) {
	logf("ALERT %s: %s", title, strings.ReplaceAll(msg, "\n", " | "))
	if c.cfg.Ntfy == "" {
		return
	}
	go func() {
		req, _ := http.NewRequest("POST", c.cfg.Ntfy, strings.NewReader(msg))
		req.Header.Set("Title", title)
		req.Header.Set("Priority", prio)
		req.Header.Set("Tags", tags)
		if res, err := (&http.Client{Timeout: 10 * time.Second}).Do(req); err != nil {
			logf("ntfy: %v", err)
		} else {
			res.Body.Close()
		}
	}()
}

func (c *Controller) notify() {
	c.subMu.Lock()
	defer c.subMu.Unlock()
	for ch := range c.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func itoa(n int64) string { return fmt.Sprint(n) }

func dur(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	if d >= time.Hour {
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}
