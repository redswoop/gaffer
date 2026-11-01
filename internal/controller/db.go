package controller

import (
	"database/sql"
	"encoding/json"
	"strings"

	_ "modernc.org/sqlite"

	"gaffer/internal/proto"
)

const schema = `
CREATE TABLE IF NOT EXISTS items(id INTEGER PRIMARY KEY, kind TEXT, title TEXT, state TEXT, host TEXT DEFAULT '',
  attempt INT DEFAULT 0, retries INT DEFAULT 0, step INT DEFAULT 0, step_name TEXT DEFAULT '', priority INT DEFAULT 100,
  reason TEXT DEFAULT '', attention TEXT DEFAULT '', attention_acked TEXT DEFAULT '', observed INT DEFAULT 0,
  progress TEXT, checkpoint TEXT, resume INT DEFAULT 0, stop_mode TEXT DEFAULT '', affinity TEXT DEFAULT '',
  stall_alerted INT DEFAULT 0, reject_host TEXT DEFAULT '', reject_until INT DEFAULT 0,
  created INT, started INT DEFAULT 0, finished INT DEFAULT 0, updated INT, spec TEXT);
CREATE INDEX IF NOT EXISTS items_state ON items(state);
CREATE TABLE IF NOT EXISTS samples(item INT, ts INT, done REAL);
CREATE INDEX IF NOT EXISTS samples_item ON samples(item, ts);
CREATE TABLE IF NOT EXISTS events(id INTEGER PRIMARY KEY, item INT, ts INT, kind TEXT, msg TEXT);
CREATE INDEX IF NOT EXISTS events_item ON events(item, ts);
CREATE TABLE IF NOT EXISTS sources(name TEXT PRIMARY KEY, host TEXT, every INT, seen INT, ok INT, error TEXT DEFAULT '',
  alerted INT DEFAULT 0, actions TEXT DEFAULT '{}');
CREATE TABLE IF NOT EXISTS hosts(host TEXT PRIMARY KEY, boot_id TEXT, version TEXT, slots TEXT, seen INT,
  drained INT DEFAULT 0, failures INT DEFAULT 0, note TEXT DEFAULT '', offline_alerted INT DEFAULT 0);
`

// rec is an item plus the controller-only bookkeeping.
type rec struct {
	proto.Item
	AttentionAcked string
	Resume         bool
	Affinity       string // resume with a checkpoint must go back to the host that holds W_STATE
	Offers         string // observed: the actions the source offers on it now, comma-separated
	StallAlerted   bool
	RejectHost     string
	RejectUntil    int64
	PausedBoot     string // the boot a shutdown pause happened in: a new boot means the host is back
	YieldRes       string // yielded / stopping for a yield: the resource given up
	PreemptUntil   int64  // a would-be preemptor that didn't start after its victim yielded waits this long before bumping again
}

const cols = `id, kind, title, state, host, attempt, retries, step, step_name, priority, reason, attention, attention_acked,
  observed, progress, checkpoint, resume, stop_mode, affinity, stall_alerted, reject_host, reject_until, created, started,
  finished, updated, spec, source, ext_key, parent, offers, acked, paused_by, paused_boot, run_ms, yielded_at, yield_to,
  yield_res, preempt_until`

func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	for _, col := range []string{"items source TEXT DEFAULT ''", "items ext_key TEXT DEFAULT ''", "items parent INT DEFAULT 0",
		"items offers TEXT DEFAULT ''", "sources actions TEXT DEFAULT '{}'", "items acked INT DEFAULT 0",
		"items paused_by TEXT DEFAULT ''", "items paused_boot TEXT DEFAULT ''", "items run_ms INT DEFAULT 0",
		"items yielded_at INT DEFAULT 0", "items yield_to TEXT DEFAULT ''", "items yield_res TEXT DEFAULT ''",
		"items preempt_until INT DEFAULT 0", "hosts hold TEXT DEFAULT ''", "hosts leases TEXT DEFAULT '[]'",
		"hosts resources TEXT DEFAULT '{}'"} { // added after v0
		table, def, _ := strings.Cut(col, " ")
		db.Exec("ALTER TABLE " + table + " ADD COLUMN " + def) // fails harmlessly once it exists
	}
	if _, err := db.Exec("CREATE INDEX IF NOT EXISTS items_ext ON items(source, ext_key)"); err != nil {
		return nil, err
	}
	// a pause from before pauses had an author was a human's (or the hook's, which then needed a human anyway)
	if _, err := db.Exec("UPDATE items SET paused_by='human' WHERE state='paused' AND paused_by=''"); err != nil {
		return nil, err
	}
	return db, nil
}

type scanner interface{ Scan(...any) error }

func scanRec(s scanner) (*rec, error) {
	r := &rec{}
	var prog, cp, spec sql.NullString
	err := s.Scan(&r.ID, &r.Kind, &r.Title, &r.State, &r.Host, &r.Attempt, &r.Retries, &r.Step, &r.StepName, &r.Priority,
		&r.Reason, &r.Attention, &r.AttentionAcked, &r.Observed, &prog, &cp, &r.Resume, &r.StopMode, &r.Affinity,
		&r.StallAlerted, &r.RejectHost, &r.RejectUntil, &r.Created, &r.Started, &r.Finished, &r.Updated, &spec, &r.Source, &r.Key, &r.Parent, &r.Offers, &r.Acked,
		&r.PausedBy, &r.PausedBoot, &r.RunMS, &r.YieldedAt, &r.YieldTo, &r.YieldRes, &r.PreemptUntil)
	if err != nil {
		return nil, err
	}
	if prog.Valid && prog.String != "" && prog.String != "null" {
		r.Progress = &proto.Progress{}
		json.Unmarshal([]byte(prog.String), r.Progress)
	}
	if cp.Valid && cp.String != "" && cp.String != "null" {
		r.Checkpoint = &proto.Checkpoint{}
		json.Unmarshal([]byte(cp.String), r.Checkpoint)
	}
	if spec.Valid {
		json.Unmarshal([]byte(spec.String), &r.Spec)
	}
	return r, nil
}

func js(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func (c *Controller) get(id int64) *rec {
	r, err := scanRec(c.db.QueryRow("SELECT "+cols+" FROM items WHERE id=?", id))
	if err != nil {
		return nil
	}
	return r
}

func (c *Controller) query(where string, args ...any) []*rec {
	rows, err := c.db.Query("SELECT "+cols+" FROM items "+where, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []*rec
	for rows.Next() {
		if r, err := scanRec(rows); err == nil {
			out = append(out, r)
		}
	}
	return out
}

func (c *Controller) put(r *rec) {
	r.Updated = nowMS()
	names := strings.Split(strings.ReplaceAll(cols, "\n", ""), ",")
	set := make([]string, 0, len(names))
	for _, n := range names[1:] {
		set = append(set, strings.TrimSpace(n)+"=?")
	}
	var prog, cp any
	if r.Progress != nil {
		prog = js(r.Progress)
	}
	if r.Checkpoint != nil {
		cp = js(r.Checkpoint)
	}
	_, err := c.db.Exec("UPDATE items SET "+strings.Join(set, ", ")+" WHERE id=?",
		r.Kind, r.Title, r.State, r.Host, r.Attempt, r.Retries, r.Step, r.StepName, r.Priority, r.Reason, r.Attention,
		r.AttentionAcked, r.Observed, prog, cp, r.Resume, r.StopMode, r.Affinity, r.StallAlerted, r.RejectHost,
		r.RejectUntil, r.Created, r.Started, r.Finished, r.Updated, js(r.Spec), r.Source, r.Key, r.Parent, r.Offers, r.Acked,
		r.PausedBy, r.PausedBoot, r.RunMS, r.YieldedAt, r.YieldTo, r.YieldRes, r.PreemptUntil, r.ID)
	if err != nil {
		logf("put #%d: %v", r.ID, err)
	}
	c.changed = true
}

func (c *Controller) insert(spec proto.Spec, state, reason string, prio int, observed bool) int64 {
	t := nowMS()
	res, err := c.db.Exec(`INSERT INTO items(kind, title, state, reason, priority, observed, created, updated, spec, started)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, spec.Kind, spec.Title, state, reason, prio, observed, t, t, js(spec),
		map[bool]int64{true: t, false: 0}[observed])
	if err != nil {
		logf("insert: %v", err)
		return 0
	}
	id, _ := res.LastInsertId()
	c.event(id, "created", state+" "+spec.Title)
	c.changed = true
	return id
}

func (c *Controller) event(id int64, kind, msg string) {
	c.db.Exec("INSERT INTO events(item, ts, kind, msg) VALUES(?,?,?,?)", id, nowMS(), kind, msg)
}
