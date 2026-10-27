// Package agent runs jobs on one host. It owns the processes and the job directories; the controller
// only ever sees reports. Everything an agent knows is on disk, so killing it (or the controller, or
// the network) at any moment loses nothing: on restart it re-reads the job dirs and reconciles.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gaffer/internal/config"
	"gaffer/internal/progress"
	"gaffer/internal/proto"
)

const Version = "0.1.0"

type pollResult struct {
	id      int64
	attempt int
	out     string
}

type Agent struct {
	cfg     *config.Config
	host    string
	dir     string
	exe     string
	jobs    map[int64]*job
	logHave map[int64]int64
	polls   chan pollResult
	client  *http.Client
	synced  bool
	obsLast map[string]int64
	obsBusy map[string]bool
	obsDone chan string
}

func now() int64 { return time.Now().UnixMilli() }

func Run(cfg *config.Config) error {
	a := &Agent{cfg: cfg, host: cfg.Agent.Host, dir: cfg.Agent.Dir, jobs: map[int64]*job{}, logHave: map[int64]int64{},
		polls: make(chan pollResult, 16), client: &http.Client{Timeout: 15 * time.Second},
		obsLast: map[string]int64{}, obsBusy: map[string]bool{}, obsDone: make(chan string, 16)}
	a.exe, _ = os.Executable()
	if len(cfg.Agent.Path) > 0 {
		os.Setenv("PATH", strings.Join(cfg.Agent.Path, ":")+":"+os.Getenv("PATH"))
	}
	if err := os.MkdirAll(filepath.Join(a.dir, "jobs"), 0o755); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(a.dir, "agent.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("another agent holds %s", lock.Name())
	}
	dirs, _ := filepath.Glob(filepath.Join(a.dir, "jobs", "*"))
	for _, d := range dirs {
		if j, err := loadJob(d); err == nil {
			a.jobs[j.ID] = j
		}
	}
	log.Printf("agent %s up (boot %.8s), %d job dirs, controller %s", a.host, bootID(), len(a.jobs), cfg.Controller)
	lastSync := int64(0)
	for {
		t := now()
		a.drainPolls(t)
		a.observe(t)
		for _, j := range a.jobs {
			a.tick(j, t)
			if j.dirty && (j.State != proto.JRunning || t-j.lastSave() > 2000) {
				j.save()
			}
		}
		if t-lastSync >= 2000 {
			lastSync = t
			if err := a.sync(); err != nil {
				log.Printf("sync: %v", err)
			}
		}
		time.Sleep(time.Second)
	}
}

func (j *job) lastSave() int64 {
	st, err := os.Stat(filepath.Join(j.dir, "job.json"))
	if err != nil {
		return 0
	}
	return st.ModTime().UnixMilli()
}

// tick advances one running job by looking at the files its shim leaves.
func (a *Agent) tick(j *job, t int64) {
	if j.State != proto.JRunning {
		return
	}
	j.tail(t)
	if rc, ok := readRC(j); ok {
		a.stepFinished(j, rc, t)
		return
	}
	id, ok := readIdent(pidPath(j.dir, j.Attempt, j.Step))
	if !ok {
		if t-j.SpawnedAt < 15000 {
			return // the shim is still starting
		}
		a.end(j, proto.JInterrupted, "step never started (agent died while launching it?)", t)
		return
	}
	if id.alive() {
		if j.StopMode != "" {
			if !j.TermSent {
				syscall.Kill(-id.PID, syscall.SIGTERM)
				j.TermSent, j.dirty = true, true
				log.Printf("#%d %s: TERM to group %d", j.ID, j.StopMode, id.PID)
			} else if grace := int64(max(j.Spec.StopGrace, 1)) * 1000; t-j.StopAt > grace && !j.KillSent {
				syscall.Kill(-id.PID, syscall.SIGKILL)
				j.KillSent, j.dirty = true, true
				log.Printf("#%d: KILL to group %d after %ds grace", j.ID, id.PID, grace/1000)
			}
		}
		a.maybePoll(j, t)
		return
	}
	if rc, ok := readRC(j); ok { // exited between the two checks
		a.stepFinished(j, rc, t)
		return
	}
	j.tail(t)
	if j.StopMode != "" {
		a.end(j, proto.JStopped, "stopped ("+j.StopMode+")", t)
		return
	}
	why := "process gone without an exit code (kill -9? OOM killer?)"
	if id.Boot != bootID() {
		why = "machine rebooted"
	}
	a.end(j, proto.JInterrupted, why, t)
}

func readRC(j *job) (int, bool) {
	b, err := os.ReadFile(rcPath(j.dir, j.Attempt, j.Step))
	if err != nil {
		return 0, false
	}
	rc, err := strconv.Atoi(strings.TrimSpace(string(b)))
	return rc, err == nil
}

func (a *Agent) stepFinished(j *job, rc int, t int64) {
	j.tail(t)
	name := j.Spec.Steps[j.Step].Name
	if rc != 0 {
		j.RC = rc
		if j.StopMode != "" {
			a.end(j, proto.JStopped, fmt.Sprintf("stopped (%s) in %s, rc %d", j.StopMode, name, rc), t)
		} else {
			a.end(j, proto.JFailed, fmt.Sprintf("%s exited %d: %s", name, rc, j.lastError()), t)
		}
		return
	}
	if cp := j.P.Checkpoint; cp != nil && cp.Step == j.Step {
		j.P.Checkpoint = nil // a finished step's checkpoint means nothing to the next one
	}
	if j.Step+1 >= len(j.Spec.Steps) {
		j.RC = 0
		a.end(j, proto.JDone, "", t)
		return
	}
	j.Step++
	if j.StopMode != "" {
		a.end(j, proto.JStopped, "stopped ("+j.StopMode+") between steps", t)
		return
	}
	a.spawn(j, t)
}

func (a *Agent) end(j *job, state, reason string, t int64) {
	j.State, j.Reason, j.Finished, j.dirty = state, reason, t, true
	j.save()
	log.Printf("#%d attempt %d: %s %s", j.ID, j.Attempt, state, reason)
}

func (a *Agent) spawn(j *job, t int64) {
	j.SpawnedAt, j.TermSent, j.KillSent = t, false, false
	os.Remove(rcPath(j.dir, j.Attempt, j.Step))
	os.Remove(pidPath(j.dir, j.Attempt, j.Step))
	j.save() // before the start: a restart in between must know a shim may exist
	cmd := exec.Command(a.exe, "_shim", j.dir, strconv.Itoa(j.Attempt), strconv.Itoa(j.Step))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		a.end(j, proto.JFailed, "spawn: "+err.Error(), t)
		return
	}
	go cmd.Wait() // reap; adopted shims (after an agent restart) aren't ours to wait for
	log.Printf("#%d attempt %d: step %d %s (shim %d)", j.ID, j.Attempt, j.Step, j.Spec.Steps[j.Step].Name, cmd.Process.Pid)
}

// start handles an assignment. Idempotent: the controller re-sends it until it sees the attempt reported.
func (a *Agent) start(as proto.Assign) {
	t := now()
	j := a.jobs[as.ID]
	if j != nil && (j.Attempt >= as.Attempt || j.State == proto.JRunning) {
		return
	}
	dir := filepath.Join(a.dir, "jobs", strconv.FormatInt(as.ID, 10))
	if j == nil {
		j = &job{ID: as.ID, dir: dir}
	} else {
		j.TailOff = j.logSize() // don't re-read earlier attempts
	}
	j.Attempt, j.Spec, j.Host, j.State, j.Step = as.Attempt, as.Spec, a.host, proto.JRunning, as.Step
	j.KindDir = a.cfg.KindDir(as.Spec.Kind)
	j.Resume, j.ResumeStep = as.Resume, as.Step
	j.StopMode, j.StopAt, j.RC, j.Reason, j.Started, j.Finished, j.Acked = "", 0, 0, "", t, 0, false
	j.P.Attention = ""
	os.MkdirAll(filepath.Join(dir, "state"), 0o755)
	if as.Resume {
		if as.Checkpoint != nil && (j.P.Checkpoint == nil || j.P.Checkpoint.TS < as.Checkpoint.TS) {
			j.P.Checkpoint = as.Checkpoint
		}
	} else {
		j.P = progress.State{}
		os.RemoveAll(filepath.Join(dir, "state"))
		os.MkdirAll(filepath.Join(dir, "state"), 0o755)
	}
	j.compile()
	a.jobs[as.ID] = j
	how := "fresh"
	if cp := j.P.Checkpoint; as.Resume && cp != nil && cp.Step == as.Step {
		how = fmt.Sprintf("resume from checkpoint %v", cp.Data)
	} else if as.Step > 0 {
		how = "from step " + j.Spec.Steps[as.Step].Name
	}
	j.logf("──── gaffer: #%d attempt %d on %s, %s, %s ────", j.ID, j.Attempt, a.host, how, time.Now().Format("Mon 15:04:05"))
	if j.Spec.Fits != "" {
		if out, ok := a.runAux(j, j.Spec.Fits, 60*time.Second); !ok {
			lines := strings.Split(strings.TrimSpace(out), "\n")
			a.end(j, proto.JRejected, trunc(lines[len(lines)-1], 200), t)
			return
		}
	}
	a.spawn(j, t)
}

// auxCmd builds a kind's fits / poll command in the job's environment (on the main loop, so the
// goroutine that runs it never reads the job).
func (a *Agent) auxCmd(j *job, script string, timeout time.Duration) (*exec.Cmd, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	cmd := exec.CommandContext(ctx, "bash", append([]string{"-c", script, "gaffer-aux"}, j.Spec.Args...)...)
	cmd.Dir = j.dir
	cmd.Env = j.env(j.dir, min(j.Step, len(j.Spec.Steps)-1))
	return cmd, cancel
}

func (a *Agent) runAux(j *job, script string, timeout time.Duration) (string, bool) {
	cmd, cancel := a.auxCmd(j, script, timeout)
	defer cancel()
	out, err := cmd.CombinedOutput()
	return string(out), err == nil
}

func (a *Agent) maybePoll(j *job, t int64) {
	if j.Spec.ProgressPoll == "" || j.polling {
		return
	}
	every := int64(max(j.Spec.PollEvery, 5)) * 1000
	if t-j.lastPoll < every {
		return
	}
	j.polling, j.lastPoll = true, t
	id, att := j.ID, j.Attempt
	cmd, cancel := a.auxCmd(j, j.Spec.ProgressPoll, 30*time.Second)
	go func() {
		defer cancel()
		out, _ := cmd.CombinedOutput()
		a.polls <- pollResult{id, att, string(out)}
	}()
}

func (a *Agent) drainPolls(t int64) {
	for {
		select {
		case r := <-a.polls:
			if j := a.jobs[r.id]; j != nil {
				j.polling = false
				if j.Attempt == r.attempt && j.State == proto.JRunning {
					for _, l := range strings.Split(r.out, "\n") {
						j.fold(l, t)
					}
				}
			}
		default:
			return
		}
	}
}

// observe starts each due observer; the goroutine posts its result straight to the controller. A
// failed run is reported as a failure, never as an empty snapshot (which would mean "all gone").
func (a *Agent) observe(t int64) {
	for drained := false; !drained; {
		select {
		case name := <-a.obsDone:
			a.obsBusy[name] = false
		default:
			drained = true
		}
	}
	for _, o := range a.cfg.Agent.Observe {
		every := max(o.Every, 2)
		if o.Name == "" || o.Run == "" || a.obsBusy[o.Name] || t-a.obsLast[o.Name] < int64(every)*1000 {
			continue
		}
		a.obsBusy[o.Name], a.obsLast[o.Name] = true, t
		exe := a.exe
		go func() {
			defer func() { a.obsDone <- o.Name }()
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(max(every, 60))*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "bash", "-c", o.Run)
			cmd.Env = append(os.Environ(), "PATH="+filepath.Dir(exe)+":"+os.Getenv("PATH"), "W_HOST="+a.host)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			req := proto.ObserveReq{Host: a.host, Every: every, OK: true, Actions: o.Actions}
			if err := cmd.Run(); err != nil {
				lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
				req.OK, req.Error = false, trunc(strings.TrimSpace(err.Error()+": "+lines[len(lines)-1]), 300)
			} else {
				req.Out = stdout.String()
			}
			body, _ := json.Marshal(req)
			hr, _ := http.NewRequest("POST", strings.TrimRight(a.cfg.Controller, "/")+"/api/observe/"+o.Name, bytes.NewReader(body))
			hr.Header.Set("Authorization", "Bearer "+a.cfg.Token)
			hr.Header.Set("Content-Type", "application/json")
			if res, err := a.client.Do(hr); err != nil {
				log.Printf("observe %s: %v", o.Name, err)
			} else {
				res.Body.Close()
			}
		}()
	}
}

func (a *Agent) sync() error {
	req := proto.SyncReq{Host: a.host, BootID: bootID(), Version: Version, Slots: a.cfg.Agent.Slots, Jobs: []proto.JobReport{},
		Observers: []string{}}
	for _, o := range a.cfg.Agent.Observe {
		req.Observers = append(req.Observers, o.Name)
	}
	ids := make([]int64, 0, len(a.jobs))
	for id, j := range a.jobs {
		if !j.Acked {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(x, y int) bool { return ids[x] < ids[y] })
	budget := 1 << 20
	for _, id := range ids {
		j := a.jobs[id]
		req.Jobs = append(req.Jobs, j.report())
		if have, ok := a.logHave[id]; ok && budget > 0 {
			if b := j.readLog(have, min(256<<10, budget)); len(b) > 0 {
				req.Logs = append(req.Logs, proto.LogChunk{ID: id, Offset: have, Data: b})
				budget -= len(b)
			}
		}
	}
	body, _ := json.Marshal(req)
	hr, _ := http.NewRequest("POST", strings.TrimRight(a.cfg.Controller, "/")+"/api/agent/sync", bytes.NewReader(body))
	hr.Header.Set("Authorization", "Bearer "+a.cfg.Token)
	hr.Header.Set("Content-Type", "application/json")
	res, err := a.client.Do(hr)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("controller: %s", res.Status)
	}
	var resp proto.SyncResp
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		return err
	}
	if !a.synced {
		a.synced = true
		log.Printf("synced with controller")
	}
	t := now()
	for id, h := range resp.LogHave {
		a.logHave[id] = h
	}
	for _, s := range resp.Stop {
		if j := a.jobs[s.ID]; j != nil && j.State == proto.JRunning && j.StopMode == "" {
			j.StopMode, j.StopAt = s.Mode, t
			j.save()
			log.Printf("#%d: %s requested", j.ID, s.Mode)
		}
	}
	for _, ack := range resp.Ack {
		if j := a.jobs[ack.ID]; j != nil && j.Attempt == ack.Attempt && j.State != proto.JRunning {
			j.Acked = true
			j.save()
			delete(a.logHave, ack.ID)
		}
	}
	for _, as := range resp.Start {
		a.start(as)
	}
	return nil
}
