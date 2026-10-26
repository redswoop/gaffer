package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"gaffer/internal/progress"
	"gaffer/internal/proto"
)

// job is one item's directory on this host: job.json (this struct), log (every attempt appended),
// state/ (W_STATE, the kind's own checkpoint data), pid.A.S and rc.A.S per attempt and step.
type job struct {
	ID         int64          `json:"id"`
	Attempt    int            `json:"attempt"`
	Spec       proto.Spec     `json:"spec"`
	KindDir    string         `json:"kind_dir"`
	Host       string         `json:"host"`
	State      string         `json:"state"`
	Step       int            `json:"step"`
	Resume     bool           `json:"resume"`
	ResumeStep int            `json:"resume_step"`
	StopMode   string         `json:"stop_mode,omitempty"`
	StopAt     int64          `json:"stop_at,omitempty"`
	TermSent   bool           `json:"term_sent,omitempty"`
	KillSent   bool           `json:"kill_sent,omitempty"`
	SpawnedAt  int64          `json:"spawned_at"`
	RC         int            `json:"rc"`
	Reason     string         `json:"reason,omitempty"`
	Started    int64          `json:"started"`
	Finished   int64          `json:"finished,omitempty"`
	Acked      bool           `json:"acked,omitempty"`
	TailOff    int64          `json:"tail_off"`
	P          progress.State `json:"p"`

	dir      string
	rx       *regexp.Regexp
	dirty    bool
	polling  bool
	lastPoll int64
}

func loadJob(dir string) (*job, error) {
	b, err := os.ReadFile(filepath.Join(dir, "job.json"))
	if err != nil {
		return nil, err
	}
	j := &job{}
	if err := json.Unmarshal(b, j); err != nil {
		return nil, fmt.Errorf("%s: %w", dir, err)
	}
	j.dir = dir
	j.compile()
	return j, nil
}

func (j *job) compile() {
	j.rx = nil
	if j.Spec.ProgressRegex != "" {
		j.rx, _ = regexp.Compile(j.Spec.ProgressRegex)
	}
}

func (j *job) save() error { j.dirty = false; return writeJSON(filepath.Join(j.dir, "job.json"), j) }

func (j *job) logPath() string { return filepath.Join(j.dir, "log") }

func (j *job) logf(format string, a ...any) {
	f, err := os.OpenFile(j.logPath(), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return
	}
	fmt.Fprintf(f, format+"\n", a...)
	f.Close()
}

// env is what a step, the poll command and the fits check see.
func (j *job) env(dir string, step int) []string {
	exe, _ := os.Executable()
	env := []string{}
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "W_") && !strings.HasPrefix(e, "PATH=") {
			env = append(env, e)
		}
	}
	cp := j.P.Checkpoint
	resume := j.Resume && step == j.ResumeStep && cp != nil && cp.Step == step
	env = append(env,
		"PATH="+filepath.Dir(exe)+":"+os.Getenv("PATH"),
		"W_ID="+strconv.FormatInt(j.ID, 10),
		"W_ATTEMPT="+strconv.Itoa(j.Attempt),
		"W_STEP="+strconv.Itoa(step),
		"W_STEP_NAME="+j.Spec.Steps[step].Name,
		"W_JOBDIR="+dir,
		"W_STATE="+filepath.Join(dir, "state"),
		"W_LOG="+filepath.Join(dir, "log"),
		"W_HOST="+j.Host,
		"W_KIND_DIR="+j.KindDir,
		"W_RESUME="+map[bool]string{true: "1", false: "0"}[resume],
	)
	if resume {
		b, _ := json.Marshal(cp.Data)
		env = append(env, "W_CHECKPOINT="+string(b), "W_CKPT_DONE="+strconv.FormatFloat(cp.Done, 'f', -1, 64))
		for k, v := range cp.Data {
			env = append(env, "W_CKPT_"+strings.ToUpper(k)+"="+v)
		}
	}
	for k, v := range j.Spec.Env {
		env = append(env, k+"="+v)
	}
	return env
}

// fold runs one line through the progress parser; only a change in what the job says moves the
// progress timestamp, so a poll repeating a stuck value still goes stale.
func (j *job) fold(line string, now int64) {
	var before proto.Progress
	had := j.P.Progress != nil
	if had {
		before = *j.P.Progress
	}
	cp, att := j.P.Checkpoint, j.P.Attention
	if !j.P.Line(line, j.rx, now) {
		return
	}
	if cp != j.P.Checkpoint && j.P.Checkpoint != nil {
		j.P.Checkpoint.Step = j.Step
	}
	if p := j.P.Progress; had && p != nil {
		after := *p
		after.TS = before.TS
		if after == before {
			p.TS = before.TS
			if cp == j.P.Checkpoint && att == j.P.Attention {
				return
			}
		}
	}
	j.dirty = true
}

// tail parses new complete lines of the log (\n or \r, so tqdm bars count).
func (j *job) tail(now int64) {
	f, err := os.Open(j.logPath())
	if err != nil {
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() <= j.TailOff {
		return
	}
	buf := make([]byte, min(st.Size()-j.TailOff, 4<<20))
	n, _ := f.ReadAt(buf, j.TailOff)
	buf = buf[:n]
	end := bytes.LastIndexAny(buf, "\n\r")
	if end < 0 {
		if n < 4<<20 {
			return // partial line; wait for the rest
		}
		end = n - 1
	}
	for _, l := range strings.FieldsFunc(string(buf[:end+1]), func(r rune) bool { return r == '\n' || r == '\r' }) {
		j.fold(l, now)
	}
	j.TailOff += int64(end + 1)
	j.dirty = true
}

func (j *job) logSize() int64 {
	st, err := os.Stat(j.logPath())
	if err != nil {
		return 0
	}
	return st.Size()
}

func (j *job) readLog(off int64, max int) []byte {
	f, err := os.Open(j.logPath())
	if err != nil {
		return nil
	}
	defer f.Close()
	buf := make([]byte, max)
	n, err := f.ReadAt(buf, off)
	if err != nil && err != io.EOF {
		return nil
	}
	return buf[:n]
}

// lastError picks the most telling line near the end of the log for a failure reason.
func (j *job) lastError() string {
	size := j.logSize()
	off := max(0, size-16384)
	lines := strings.FieldsFunc(string(j.readLog(off, 16384)), func(r rune) bool { return r == '\n' || r == '\r' })
	var last string
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" || strings.HasPrefix(l, "::") || strings.HasPrefix(l, "──── gaffer") {
			continue
		}
		if last == "" {
			last = l
		}
		if regexp.MustCompile(`(?i)error|traceback|fatal|killed|out of memory|exception`).MatchString(l) {
			return trunc(l, 200)
		}
	}
	return trunc(last, 200)
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func (j *job) report() proto.JobReport {
	r := proto.JobReport{ID: j.ID, Attempt: j.Attempt, State: j.State, Step: j.Step, RC: j.RC, Reason: j.Reason,
		Progress: j.P.Progress, Checkpoint: j.P.Checkpoint, Attention: j.P.Attention, LogSize: j.logSize(),
		Started: j.Started, Finished: j.Finished}
	if j.Step < len(j.Spec.Steps) {
		r.StepName = j.Spec.Steps[j.Step].Name
	}
	if j.State == proto.JRunning {
		if id, ok := readIdent(pidPath(j.dir, j.Attempt, j.Step)); ok {
			r.PID = id.PID
		}
	}
	return r
}
