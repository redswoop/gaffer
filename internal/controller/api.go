package controller

import (
	"bufio"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"gaffer/internal/progress"
	"gaffer/internal/proto"
)

func (c *Controller) routes() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("POST /api/agent/sync", func(w http.ResponseWriter, r *http.Request) {
		var req proto.SyncReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Host == "" {
			http.Error(w, "bad sync", 400)
			return
		}
		reply(w, c.sync(req))
	})
	m.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		reply(w, c.status(r.URL.Query().Get("all") != ""))
	})
	m.HandleFunc("GET /api/stream", c.stream)
	m.HandleFunc("POST /api/items", c.submit)
	m.HandleFunc("POST /api/track", c.track)
	m.HandleFunc("POST /api/observe/{name}", func(w http.ResponseWriter, r *http.Request) {
		var req proto.ObserveReq
		if err := json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(&req); err != nil || r.PathValue("name") == "" {
			http.Error(w, "bad observe", 400)
			return
		}
		c.observe(r.PathValue("name"), req)
		reply(w, map[string]bool{"ok": true})
	})
	m.HandleFunc("GET /api/items/{id}", func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		it := c.get(pathID(r))
		if it == nil {
			http.Error(w, "no such item", 404)
			return
		}
		type ev struct {
			TS   int64  `json:"ts"`
			Kind string `json:"kind"`
			Msg  string `json:"msg"`
		}
		evs := []ev{}
		rows, err := c.db.Query("SELECT ts, kind, msg FROM events WHERE item=? ORDER BY id", it.ID)
		if err == nil {
			for rows.Next() {
				var e ev
				rows.Scan(&e.TS, &e.Kind, &e.Msg)
				evs = append(evs, e)
			}
			rows.Close()
		}
		reply(w, map[string]any{"item": c.derive(it, nowMS()), "events": evs})
	})
	m.HandleFunc("GET /api/items/{id}/log", func(w http.ResponseWriter, r *http.Request) {
		id := pathID(r)
		off, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
		size := c.logSize(id)
		w.Header().Set("X-Log-Size", strconv.FormatInt(size, 10))
		f, err := os.Open(c.logPath(id))
		if err != nil || off >= size {
			return
		}
		defer f.Close()
		f.Seek(off, io.SeekStart)
		io.Copy(w, f)
	})
	m.HandleFunc("POST /api/items/{id}/{verb}", c.verb)
	m.HandleFunc("POST /api/hosts/{host}/{verb}", func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		h := r.PathValue("host")
		switch r.PathValue("verb") {
		case "drain":
			c.db.Exec("UPDATE hosts SET drained=1, note='drained by hand' WHERE host=?", h)
		case "undrain":
			c.db.Exec("UPDATE hosts SET drained=0, failures=0, note='' WHERE host=?", h)
		default:
			http.Error(w, "unknown verb", 400)
			return
		}
		c.notify()
		reply(w, map[string]string{"ok": h})
	})
	return c.auth(m)
}

func (c *Controller) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got == "" {
			got = r.URL.Query().Get("token")
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(c.cfg.Token)) != 1 {
			http.Error(w, "token", 401)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func pathID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id
}

type submitReq struct {
	Spec     proto.Spec `json:"spec"`
	Priority int        `json:"priority"`
	Held     string     `json:"held,omitempty"` // non-empty: queue held with this reason
}

func (c *Controller) submit(w http.ResponseWriter, r *http.Request) {
	var s submitReq
	if err := json.NewDecoder(r.Body).Decode(&s); err != nil || len(s.Spec.Steps) == 0 {
		http.Error(w, "need a spec with steps", 400)
		return
	}
	if s.Priority == 0 {
		s.Priority = 100
	}
	c.mu.Lock()
	state := proto.SQueued
	if s.Held != "" {
		state = proto.SHeld
	}
	id := c.insert(s.Spec, state, s.Held, s.Priority, false)
	c.mu.Unlock()
	c.notify()
	reply(w, map[string]int64{"id": id})
}

// track creates an observed item: gaffer never runs it, the work reports in with `gaffer emit`.
func (c *Controller) track(w http.ResponseWriter, r *http.Request) {
	var s struct {
		Title, Kind, Host string
		StallAfter        int `json:"stall_after"`
	}
	if err := json.NewDecoder(r.Body).Decode(&s); err != nil || s.Title == "" {
		http.Error(w, "need a title", 400)
		return
	}
	if s.Kind == "" {
		s.Kind = "observed"
	}
	c.mu.Lock()
	id := c.insert(proto.Spec{Kind: s.Kind, Title: s.Title, StallAfter: s.StallAfter}, proto.SRunning, "", 100, true)
	if it := c.get(id); it != nil && s.Host != "" {
		it.Host = s.Host
		c.put(it)
	}
	c.mu.Unlock()
	c.notify()
	reply(w, map[string]int64{"id": id})
}

func (c *Controller) verb(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer func() { c.mu.Unlock(); c.notify() }()
	it := c.get(pathID(r))
	if it == nil {
		http.Error(w, "no such item", 404)
		return
	}
	verb := r.PathValue("verb")
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	msg, err := c.apply(it, verb, string(body))
	if err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	c.event(it.ID, verb, msg)
	reply(w, map[string]string{"ok": msg})
}

// apply is every human (or script) action on an item. Each checks the state it may act on, so a
// command racing a state change fails loudly instead of clobbering it.
func (c *Controller) apply(it *rec, verb, body string) (string, error) {
	bad := func() (string, error) { return "", fmt.Errorf("#%d is %s; can't %s", it.ID, it.State, verb) }
	if it.Source != "" && verb != "ack" && verb != "cancel" && verb != "top" && verb != "do" {
		return "", fmt.Errorf("#%d is reported by the %s observer; %s it there (cancel just dismisses it here)", it.ID, it.Source, verb)
	}
	switch verb {
	case "pause":
		if it.Parent != 0 {
			return "", fmt.Errorf("#%d is an action; it runs to the end (cancel stops it)", it.ID)
		}
		switch it.State {
		case proto.SQueued, proto.SHeld:
			it.State = proto.SPaused
		case proto.SAssigned, proto.SRunning, proto.SStopping:
			if it.Observed {
				return bad()
			}
			it.StopMode = "pause"
			if it.State != proto.SAssigned {
				it.State = proto.SStopping
			}
		default:
			return bad()
		}
	case "cancel":
		switch it.State {
		case proto.SQueued, proto.SHeld, proto.SPaused, proto.SNeedsYou, proto.SFailed:
			it.State, it.Finished, it.Reason, it.Attention = proto.SCanceled, nowMS(), "canceled", ""
		case proto.SAssigned, proto.SRunning, proto.SStopping:
			if it.Observed {
				it.State, it.Finished, it.Reason, it.Attention = proto.SCanceled, nowMS(), "canceled", ""
				break
			}
			it.StopMode = "cancel"
			if it.State != proto.SAssigned {
				it.State = proto.SStopping
			}
		default:
			return bad()
		}
	case "resume", "release":
		if it.State != proto.SPaused && it.State != proto.SHeld {
			return bad()
		}
		it.State, it.Reason = proto.SQueued, ""
	case "retry":
		if it.State != proto.SFailed && it.State != proto.SCanceled && it.State != proto.SDone {
			return bad()
		}
		it.State, it.Reason, it.Retries, it.Finished, it.Affinity, it.Acked = proto.SQueued, "", 0, 0, "", 0
		if strings.TrimSpace(body) != "resume" {
			it.Step, it.Resume, it.Checkpoint, it.Progress = 0, false, nil, nil
			c.db.Exec("DELETE FROM samples WHERE item=?", it.ID)
		} else {
			it.Resume = true
		}
	case "ack":
		if it.State == proto.SFailed && it.Acked == 0 { // seen it: it stays failed, just off the FAILED list
			it.Acked = nowMS()
			break
		}
		if it.Attention == "" && it.State != proto.SNeedsYou {
			return bad()
		}
		it.AttentionAcked, it.Attention = it.Attention, ""
		if it.State == proto.SNeedsYou {
			it.State = proto.SDone
			if it.Finished == 0 {
				it.Finished = nowMS() // or it would never show again, not even in `ls -a`
			}
		}
	case "top":
		var m int
		c.db.QueryRow("SELECT COALESCE(MIN(priority),100) FROM items").Scan(&m)
		it.Priority = m - 1
	case "emit":
		return c.emit(it, body)
	case "do":
		return c.act(it, strings.TrimSpace(body))
	default:
		return "", fmt.Errorf("unknown verb %s", verb)
	}
	c.put(it)
	return it.State, nil
}

// emit folds protocol lines into an item from outside any agent (observed work, scripts on other
// machines). `::done`, `::fail REASON` and `::needs-you` end or flag it.
func (c *Controller) emit(it *rec, body string) (string, error) {
	if !active(it.State) && it.State != proto.SNeedsYou {
		return "", fmt.Errorf("#%d is %s", it.ID, it.State)
	}
	ps := progress.State{Attention: it.Attention}
	if it.Progress != nil {
		p := *it.Progress
		ps.Progress = &p
	}
	t := nowMS()
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		switch {
		case l == "::done":
			it.State, it.Finished, ps.Attention = proto.SDone, t, ""
		case strings.HasPrefix(l, "::fail"):
			it.State, it.Finished, it.Reason, ps.Attention = proto.SFailed, t, strings.TrimSpace(strings.TrimPrefix(l, "::fail")), ""
			c.alertFor(it, "gaffer: FAILED #"+itoa(it.ID), it.Title+"\n"+it.Reason, "high", "warning")
		default:
			ps.Line(l, nil, t)
		}
	}
	c.fold(it, ps.Progress, ps.Checkpoint, ps.Attention)
	c.put(it)
	if f, err := os.OpenFile(c.logPath(it.ID), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644); err == nil && it.Observed {
		f.WriteString(strings.TrimRight(body, "\n") + "\n")
		f.Close()
	}
	return it.State, nil
}

// stream is SSE: an event whenever anything changes (and every 10 s); clients refetch /api/status.
func (c *Controller) stream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no streaming", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	ch := make(chan struct{}, 1)
	c.subMu.Lock()
	c.subs[ch] = struct{}{}
	c.subMu.Unlock()
	defer func() { c.subMu.Lock(); delete(c.subs, ch); c.subMu.Unlock() }()
	fmt.Fprint(w, "event: change\ndata: {}\n\n")
	fl.Flush()
	last := time.Time{}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			if d := time.Since(last); d < 500*time.Millisecond {
				time.Sleep(500*time.Millisecond - d) // coalesce bursts
			}
			last = time.Now()
			fmt.Fprint(w, "event: change\ndata: {}\n\n")
			fl.Flush()
		}
	}
}
