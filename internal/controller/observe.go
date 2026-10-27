package controller

import (
	"fmt"
	"strconv"
	"strings"

	"gaffer/internal/progress"
	"gaffer/internal/proto"
)

// An observer is to work gaffer doesn't run what an agent is to work it does: every run it reports the
// whole truth of its source, and the items follow. A key missing from a good snapshot is gone (done);
// a failed or silent observer changes nothing, it just shows as such.

var snapStates = map[string]bool{proto.SQueued: true, proto.SRunning: true, proto.SPaused: true, proto.SHeld: true,
	proto.SNeedsYou: true, proto.SDone: true, proto.SFailed: true, proto.SCanceled: true}

func terminal(state string) bool {
	return state == proto.SDone || state == proto.SFailed || state == proto.SCanceled
}

type snapItem struct {
	key   string
	attrs map[string]string
	ps    progress.State
}

// parseSnapshot reads `::item KEY k=v...` headers; the protocol lines after one apply to that item.
func parseSnapshot(out string, t int64) []*snapItem {
	var items []*snapItem
	var cur *snapItem
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if rest, ok := strings.CutPrefix(l, "::item"); ok && (rest == "" || rest[0] == ' ') {
			toks := progress.Tokens(rest)
			if len(toks) == 0 {
				cur = nil
				continue
			}
			cur = &snapItem{key: toks[0], attrs: map[string]string{}}
			for _, tok := range toks[1:] {
				if k, v, ok := strings.Cut(tok, "="); ok {
					cur.attrs[k] = v
				}
			}
			items = append(items, cur)
			continue
		}
		if cur != nil {
			cur.ps.Line(l, nil, t)
		}
	}
	return items
}

func (c *Controller) observe(name string, req proto.ObserveReq) {
	c.mu.Lock()
	defer func() {
		if c.changed {
			c.changed = false
			c.notify()
		}
		c.mu.Unlock()
	}()
	t := nowMS()
	var alerted bool
	known := c.db.QueryRow("SELECT alerted FROM sources WHERE name=?", name).Scan(&alerted) == nil
	if !req.OK {
		c.db.Exec(`INSERT INTO sources(name, host, every, seen, ok, error, alerted) VALUES(?,?,?,?,0,?,1)
			ON CONFLICT(name) DO UPDATE SET host=excluded.host, every=excluded.every, seen=excluded.seen, ok=0,
			error=excluded.error, alerted=1`, name, req.Host, req.Every, t, req.Error)
		if !alerted {
			c.alert("gaffer: observer "+name+" failing", req.Host+": "+req.Error, "default", "warning")
		}
		c.changed = true
		return
	}
	c.db.Exec(`INSERT INTO sources(name, host, every, seen, ok, error, alerted, actions) VALUES(?,?,?,?,1,'',0,?)
		ON CONFLICT(name) DO UPDATE SET host=excluded.host, every=excluded.every, seen=excluded.seen, ok=1, error='',
		alerted=0, actions=excluded.actions`, name, req.Host, req.Every, t, js(req.Actions))
	if alerted && known {
		c.alert("gaffer: observer "+name+" back", "", "low", "white_check_mark")
	}
	seen := map[string]bool{}
	for _, s := range parseSnapshot(req.Out, t) {
		if seen[s.key] {
			continue // first one wins
		}
		seen[s.key] = true
		c.observed(name, req.Host, s, t)
	}
	for _, it := range c.query("WHERE source=? AND state NOT IN ('done','canceled','failed')", name) {
		if !seen[it.Key] {
			it.State, it.Reason, it.Attention, it.Finished = proto.SDone, "gone from "+name, "", t
			c.event(it.ID, "gone", "no longer reported by "+name)
			c.put(it)
		}
	}
}

// observed folds one snapshot entry into its item. The source is the truth, except that a human's
// dismissal (cancel) or ack sticks for as long as the source keeps saying the same thing.
func (c *Controller) observed(source, host string, s *snapItem, t int64) {
	state := s.attrs["state"]
	if !snapStates[state] {
		state = proto.SRunning
	}
	attention := s.ps.Attention
	if state == proto.SNeedsYou && attention == "" {
		attention = "review"
	}
	if h := s.attrs["host"]; h != "" {
		host = h
	}
	stall := -1 // most sources have nothing to say about stalls; opt in with stall=SECONDS
	if v, err := strconv.Atoi(s.attrs["stall"]); err == nil {
		stall = v
	}
	quiet := s.attrs["quiet"] == "1" || s.attrs["quiet"] == "true"
	var it *rec
	if rs := c.query("WHERE source=? AND ext_key=?", source, s.key); len(rs) > 0 {
		it = rs[0]
	}
	if it == nil { // new: no alerts, a first snapshot of a backlog isn't news
		kind := s.attrs["kind"]
		if kind == "" {
			kind = source
		}
		title := s.attrs["title"]
		if title == "" {
			title = source + " " + s.key
		}
		id := c.insert(proto.Spec{Kind: kind, Title: title, StallAfter: stall, Quiet: quiet}, state, s.attrs["reason"], 100, true)
		if it = c.get(id); it == nil {
			return
		}
		it.Source, it.Key, it.Host, it.Attention, it.Progress, it.Offers = source, s.key, host, attention, s.ps.Progress, s.attrs["actions"]
		if terminal(state) || state == proto.SNeedsYou {
			it.Finished = t
		}
		stamp(it, s.attrs)
		c.put(it)
		return
	}
	stamp(it, s.attrs)
	dismissed := it.State == proto.SCanceled && state != proto.SRunning && state != proto.SQueued
	acked := it.State == proto.SDone && state == proto.SNeedsYou && attention == it.AttentionAcked
	if dismissed || acked {
		return
	}
	if v := s.attrs["title"]; v != "" {
		it.Title, it.Spec.Title = v, v
	}
	if v := s.attrs["kind"]; v != "" {
		it.Kind, it.Spec.Kind = v, v
	}
	it.Host, it.Spec.StallAfter, it.Spec.Quiet, it.Reason, it.Offers = host, stall, quiet, s.attrs["reason"], s.attrs["actions"]
	if state != it.State {
		c.event(it.ID, state, strings.TrimSpace(it.State+" -> "+state+" "+it.Reason))
		switch {
		case state == proto.SFailed:
			c.alertFor(it, "gaffer: FAILED #"+itoa(it.ID)+" ("+source+")", it.Title+"\n"+it.Reason, "high", "warning")
		case active(state) || state == proto.SQueued:
			it.Finished = 0
		}
		if terminal(state) || state == proto.SNeedsYou {
			it.Finished = t
		}
		stamp(it, s.attrs)
		it.Acked = 0 // a fresh failure is news again
		if state == proto.SDone {
			c.mootActions(it, source)
		}
		if state == proto.SRunning && it.Started == 0 {
			it.Started = t
		}
		it.State = state
	}
	p := s.ps.Progress
	if p != nil && it.Progress != nil {
		was := *it.Progress
		was.TS = p.TS
		if was == *p {
			p.TS = it.Progress.TS // unchanged: the progress clock stays put, so a stuck source stalls
		}
	}
	if p == nil {
		it.Progress = nil
	}
	if it.AttentionAcked != "" && attention != it.AttentionAcked {
		it.AttentionAcked = ""
	}
	c.fold(it, p, nil, attention)
	c.put(it)
}

// mootActions files away failed actions on an item its source now says is done: whatever went wrong
// (a wrapper's after-check, say), the thing the action was for happened. Only the source's word counts,
// never a human's ack.
func (c *Controller) mootActions(it *rec, source string) {
	for _, a := range c.query("WHERE parent=? AND state='failed' AND acked=0", it.ID) {
		a.Acked = nowMS()
		c.event(a.ID, "moot", "#"+itoa(it.ID)+" is done per "+source)
		c.put(a)
	}
}

// stamp takes the source's own started= / finished= (unix seconds) over the time gaffer first saw it.
func stamp(it *rec, attrs map[string]string) {
	if v, err := strconv.ParseFloat(attrs["started"], 64); err == nil && v > 0 {
		it.Started = int64(v * 1000)
	}
	if v, err := strconv.ParseFloat(attrs["finished"], 64); err == nil && v > 0 && it.Finished > 0 {
		it.Finished = int64(v * 1000)
	}
}

func (c *Controller) sources(t int64) []proto.Source {
	out := []proto.Source{}
	rows, err := c.db.Query("SELECT name, host, every, seen, ok, error FROM sources ORDER BY name")
	if err != nil {
		return out
	}
	for rows.Next() {
		var s proto.Source
		rows.Scan(&s.Name, &s.Host, &s.Every, &s.Seen, &s.OK, &s.Error)
		s.Silent = t-s.Seen > silentAfter(s.Every)
		out = append(out, s)
	}
	rows.Close()
	for i := range out {
		c.db.QueryRow("SELECT COUNT(*) FROM items WHERE source=? AND state NOT IN ('done','canceled')", out[i].Name).Scan(&out[i].Items)
	}
	return out
}

// retireSources drops the observers a host no longer runs (taken out of its config), and their items
// with them: a removed source is gone on purpose, not silent.
func (c *Controller) retireSources(host string, running []string, t int64) {
	keep := map[string]bool{}
	for _, n := range running {
		keep[n] = true
	}
	var gone []string
	rows, err := c.db.Query("SELECT name FROM sources WHERE host=?", host)
	if err != nil {
		return
	}
	for rows.Next() {
		var n string
		rows.Scan(&n)
		if !keep[n] {
			gone = append(gone, n)
		}
	}
	rows.Close()
	for _, n := range gone {
		for _, it := range c.query("WHERE source=? AND state NOT IN ('done','canceled','failed')", n) {
			it.State, it.Reason, it.Attention, it.Finished = proto.SDone, n+" observer removed", "", t
			c.event(it.ID, "gone", "the "+n+" observer was removed from "+host)
			c.put(it)
		}
		c.db.Exec("DELETE FROM sources WHERE name=?", n)
		logf("observer %s retired (no longer configured on %s)", n, host)
		c.changed = true
	}
}

func silentAfter(every int) int64 { return max(3*int64(every)*1000, 120*1000) }

// watchSources alerts once per silence; the next good report clears it (and says so).
func (c *Controller) watchSources(t int64) {
	for _, s := range c.sources(t) {
		if !s.Silent {
			continue
		}
		var alerted bool
		c.db.QueryRow("SELECT alerted FROM sources WHERE name=?", s.Name).Scan(&alerted)
		if !alerted {
			c.alert("gaffer: observer "+s.Name+" silent", fmt.Sprintf("no report from %s for %s", s.Host, dur(t-s.Seen)), "default", "hourglass")
			c.db.Exec("UPDATE sources SET alerted=1 WHERE name=?", s.Name)
		}
	}
}
