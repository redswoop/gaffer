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
	rejectBackoff     = 30 * 1000
	breakerFailures   = 2
)

func nowMS() int64            { return time.Now().UnixMilli() }
func logf(f string, a ...any) { log.Printf(f, a...) }

type Controller struct {
	cfg     *config.Config
	db      *sql.DB
	mu      sync.Mutex
	changed bool
	subMu   sync.Mutex
	subs    map[chan struct{}]struct{}
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
	c := &Controller{cfg: cfg, db: db, subs: map[chan struct{}]struct{}{}}
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

func needs(s proto.Spec) string {
	if s.Needs == "" {
		return "cpu"
	}
	return s.Needs
}

// sync is the whole agent protocol: fold the reports in, re-send what the agent should be doing,
// hand out new work, acknowledge finished attempts once their full log is here.
func (c *Controller) sync(req proto.SyncReq) proto.SyncResp {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := nowMS()
	resp := proto.SyncResp{LogHave: map[int64]int64{}}
	var prevBoot string
	var offlineAlerted bool
	c.db.QueryRow("SELECT boot_id, offline_alerted FROM hosts WHERE host=?", req.Host).Scan(&prevBoot, &offlineAlerted)
	c.db.Exec(`INSERT INTO hosts(host, boot_id, version, slots, seen) VALUES(?,?,?,?,?) ON CONFLICT(host) DO UPDATE SET
		boot_id=excluded.boot_id, version=excluded.version, slots=excluded.slots, seen=excluded.seen, offline_alerted=0`,
		req.Host, req.BootID, req.Version, js(req.Slots), t)
	if prevBoot != "" && prevBoot != req.BootID {
		c.alert("gaffer: "+req.Host+" rebooted", "agent is back; paused jobs wait for `gaffer resume`, interrupted ones follow their kind's policy", "default", "arrows_counterclockwise")
	} else if offlineAlerted {
		c.alert("gaffer: "+req.Host+" back online", "", "low", "white_check_mark")
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
				resp.Stop = append(resp.Stop, proto.Stop{ID: it.ID, Mode: it.StopMode})
			}
			continue
		}
		if active(it.State) && it.Host == req.Host && resp.LogHave[r.ID] >= r.LogSize { // settled = whole log here
			c.finished(it, r, req.Host)
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
		c.interrupted(it, req.Host, "the agent has no record of it (job dir lost?)", nil, it.Step)
	}

	resp.Start = append(resp.Start, c.schedule(req)...)
	if c.changed {
		c.changed = false
		c.notify()
	}
	return resp
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
		it.State = proto.SRunning
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

func (c *Controller) finished(it *rec, r proto.JobReport, host string) {
	c.fold(it, r.Progress, r.Checkpoint, r.Attention)
	it.Step, it.StepName = r.Step, r.StepName
	switch r.State {
	case proto.JDone:
		it.State, it.Finished, it.Reason, it.Checkpoint, it.StopMode = proto.SDone, r.Finished, "", nil, ""
		c.db.Exec("UPDATE hosts SET failures=0 WHERE host=?", host)
		it.Attention = "" // a mid-run question nobody answered is moot once the job is over
		if it.Spec.Review {
			it.State, it.Attention = proto.SNeedsYou, "review"
			c.alertFor(it, "gaffer: #"+itoa(it.ID)+" ready for review", it.Title, "default", "eyes")
		}
		c.event(it.ID, it.State, "finished on "+host)
	case proto.JFailed:
		it.State, it.Finished, it.Reason, it.StopMode, it.Attention = proto.SFailed, r.Finished, r.Reason, "", ""
		c.event(it.ID, "failed", r.Reason)
		c.alertFor(it, "gaffer: FAILED #"+itoa(it.ID)+" on "+host, it.Title+"\n"+r.Reason+"\ngaffer log "+itoa(it.ID), "high", "warning")
		c.breaker(host)
	case proto.JStopped:
		if it.StopMode == "cancel" {
			it.State, it.Finished, it.Reason, it.Attention = proto.SCanceled, r.Finished, "canceled", ""
		} else {
			it.State, it.Reason, it.Resume = proto.SPaused, "paused on "+host, true
			if it.Checkpoint != nil {
				it.Affinity = host
			}
		}
		c.event(it.ID, it.State, r.Reason)
		it.StopMode = ""
	case proto.JInterrupted:
		c.interrupted(it, host, r.Reason, r.Checkpoint, r.Step)
		return
	case proto.JRejected:
		it.State, it.Reason, it.RejectHost, it.RejectUntil = proto.SQueued, "waiting: "+host+": "+r.Reason, host, nowMS()+rejectBackoff
		if it.StopMode != "" {
			it.State, it.Reason, it.StopMode = map[string]string{"cancel": proto.SCanceled, "pause": proto.SPaused}[it.StopMode], "", ""
		}
		it.Host = ""
	}
	if it.State != proto.SQueued {
		c.childEvent(it)
	}
	c.put(it)
}

// interrupted applies the kind's policy to a job that vanished: resume where it was (step boundary,
// plus the kind's checkpoint if it printed one), restart from scratch, or hold for a human.
func (c *Controller) interrupted(it *rec, host, why string, cp *proto.Checkpoint, step int) {
	if cp != nil {
		it.Checkpoint = cp
	}
	it.Retries++
	maxR := it.Spec.MaxRetries
	if maxR == 0 {
		maxR = defaultRetries
	}
	stopping := it.StopMode
	it.Host, it.StopMode = "", ""
	switch stopping { // it died while we were stopping it: that's the stop
	case "pause":
		it.State, it.Reason, it.Resume = proto.SPaused, "paused ("+why+")", true
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
		c.alert("gaffer: "+host+" held", fmt.Sprintf("%d failures in a row; fix, then `gaffer release %s`", n, host), "high", "rotating_light")
	}
}

// schedule hands queued work to this host: priority then age, while it has free slots of the
// label a job needs. A job pinned by host or checkpoint affinity only goes there.
func (c *Controller) schedule(req proto.SyncReq) []proto.Assign {
	var held bool // the `drained` column: held by hand, by the breaker, or by a shutdown in progress
	c.db.QueryRow("SELECT drained FROM hosts WHERE host=?", req.Host).Scan(&held)
	if held {
		return nil
	}
	used := map[string]int{}
	for _, it := range c.query("WHERE host=? AND state IN ('assigned','running','stopping') AND observed=0", req.Host) {
		used[needs(it.Spec)]++
	}
	t := nowMS()
	var out []proto.Assign
	for _, it := range c.query("WHERE state='queued' AND observed=0 ORDER BY priority, id") {
		n := needs(it.Spec)
		if req.Slots[n] <= used[n] {
			continue
		}
		if (it.Spec.Host != "" && it.Spec.Host != req.Host) || (it.Affinity != "" && it.Affinity != req.Host) {
			continue
		}
		if it.RejectHost == req.Host && t < it.RejectUntil {
			continue
		}
		it.Attempt++
		it.State, it.Host, it.Reason = proto.SAssigned, req.Host, ""
		c.put(it)
		c.event(it.ID, "assigned", fmt.Sprintf("attempt %d to %s", it.Attempt, req.Host))
		out = append(out, c.assignment(it))
		used[n]++
	}
	return out
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
	rows, err := c.db.Query("SELECT host, boot_id, version, slots, seen, drained, note, failures FROM hosts ORDER BY host")
	if err == nil {
		for rows.Next() {
			var h proto.Host
			var slots string
			rows.Scan(&h.Host, &h.BootID, &h.Version, &slots, &h.Seen, &h.Held, &h.Note, &h.Failures)
			json.Unmarshal([]byte(slots), &h.Slots)
			h.Online = t-h.Seen < offlineAfter
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
				st.Hosts[i].Used[needs(it.Spec)]++
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
// work waiting for review, failures, stalls, what's moving, what's waiting, history.
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
	case it.State == proto.SQueued:
		return 5
	case it.State == proto.SPaused || it.State == proto.SHeld:
		return 6
	}
	return 7
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
