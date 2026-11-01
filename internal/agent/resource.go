package agent

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gaffer/internal/config"
	"gaffer/internal/proto"
)

// A resource hook pair per slot label ([agent.resources.gpu]): `probe` says who is on the resource,
// `release` clears idle residents off it. Both are scripts; the agent only parses `used=N total=M` and
// `PID NAME AMOUNT` lines and attributes pids to its own jobs (by their systemd scope, else their process
// group). Nothing in here knows what a GPU is.

type proc struct {
	PID  int
	Name string
	MiB  int
}

type probed struct {
	Used, Total int
	Procs       []proc
	At          int64 // when this reading was taken
	UsedSince   int64 // since when `used` has been about this (the "idle 3m" on the board)
	busy        bool
	lastRelease int64
}

func (a *Agent) resource(res string) (config.Resource, bool) {
	r, ok := a.cfg.Agent.Resources[res]
	return r, ok && r.Probe != ""
}

func (a *Agent) idleMiB(res string) int { return a.cfg.Agent.Resources[res].IdleMiB }

// probe runs each configured resource's probe on its period, off the loop, and folds the reading in.
func (a *Agent) probe(t int64) {
	for res, r := range a.cfg.Agent.Resources {
		if r.Probe == "" {
			continue
		}
		p := a.res[res]
		if p == nil {
			p = &probed{}
			a.res[res] = p
		}
		if p.busy || t-p.At < int64(max(r.Every, 2))*1000 {
			continue
		}
		p.busy = true
		script, name := r.Probe, res
		go func() {
			out, err := runHook(script, 10*time.Second, nil)
			a.ops <- func() {
				p := a.res[name]
				p.busy = false
				if err != nil {
					log.Printf("probe %s: %v", name, err)
					return
				}
				a.fold(name, out, now())
			}
		}()
	}
}

func runHook(script string, timeout time.Duration, env []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", script)
	cmd.Env = append(os.Environ(), env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		lines := strings.Split(strings.TrimSpace(errb.String()), "\n")
		return out.String(), fmt.Errorf("%v: %s", err, lines[len(lines)-1])
	}
	return out.String(), nil
}

var probeLine = regexp.MustCompile(`^(\d+)\s+(\S+)\s+(\d+)`)

// fold takes a probe's output: first line `used=N total=M`, then `PID NAME AMOUNT` per process.
func (a *Agent) fold(res, out string, t int64) {
	p := a.res[res]
	was := p.Used
	p.Procs = p.Procs[:0]
	for i, l := range strings.Split(strings.TrimSpace(out), "\n") {
		l = strings.TrimSpace(l)
		if i == 0 {
			for _, tok := range strings.Fields(l) {
				if k, v, ok := strings.Cut(tok, "="); ok {
					n, _ := strconv.Atoi(v)
					switch k {
					case "used":
						p.Used = n
					case "total":
						p.Total = n
					}
				}
			}
			continue
		}
		if m := probeLine.FindStringSubmatch(l); m != nil {
			pid, _ := strconv.Atoi(m[1])
			mib, _ := strconv.Atoi(m[3])
			p.Procs = append(p.Procs, proc{PID: pid, Name: m[2], MiB: mib})
		}
	}
	if p.At == 0 || abs(p.Used-was) > max(p.Total/20, 64) {
		p.UsedSince = t
	}
	p.At = t
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// jobOfPID: the job whose step this process belongs to, by its systemd scope (gaffer-HOST-ID-A-S), else
// by process group (the shim's pid); nil if it isn't gaffer's.
func (a *Agent) jobOfPID(pid int) *job {
	if b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cgroup"); err == nil {
		if m := regexp.MustCompile(`gaffer-` + regexp.QuoteMeta(a.host) + `-(\d+)-\d+-\d+\.scope`).FindStringSubmatch(string(b)); m != nil {
			id, _ := strconv.ParseInt(m[1], 10, 64)
			return a.jobs[id]
		}
	}
	pg := procPGID(pid)
	if pg <= 0 {
		return nil
	}
	for _, j := range a.jobs {
		if j.State != proto.JRunning {
			continue
		}
		if id, ok := readIdent(pidPath(j.dir, j.Attempt, j.Step)); ok && id.PID == pg {
			return j
		}
	}
	return nil
}

func procPGID(pid int) int {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 3 {
		return 0
	}
	n, _ := strconv.Atoi(f[2])
	return n
}

func alivePID(pid int) bool {
	st, _, ok := procStat(pid)
	return ok && st != "Z" && st != "X"
}

// foreignMiB: what the probe sees on the resource that isn't a job of ours.
func (a *Agent) foreignMiB(res string) int {
	p := a.res[res]
	if p == nil {
		return 0
	}
	mine := 0
	for _, pc := range p.Procs {
		if !alivePID(pc.PID) || a.jobOfPID(pc.PID) != nil { // a dead pid is a stale reading, not a resident
			mine += pc.MiB
		}
	}
	return max(0, p.Used-mine)
}

func (a *Agent) foreignNames(res string) []string {
	p := a.res[res]
	if p == nil {
		return nil
	}
	var out []string
	for _, pc := range p.Procs {
		if alivePID(pc.PID) && a.jobOfPID(pc.PID) == nil {
			out = append(out, fmt.Sprintf("%s %s", filepath.Base(pc.Name), gib(pc.MiB)))
		}
	}
	return out
}

func gib(mib int) string {
	if mib >= 1024 {
		return fmt.Sprintf("%.1fG", float64(mib)/1024)
	}
	return fmt.Sprintf("%dM", mib)
}

// releaseIdle runs the resource's release hook when nothing holds a lease on the resource that would mind
// and foreign use is above idle. The hook is told who is waiting (a lease holder) or what the starting step
// shares (its lease classes), so it can spare what is about to be used: a chat model's request keeps the
// model, a job that shares `llm` keeps it too. Synchronous: the caller is about to start something on the
// resource. At most once per 10 s per resource. Re-probes.
func (a *Agent) releaseIdle(res, waitingFor string, shares []string, t int64) bool {
	r, ok := a.resource(res)
	if !ok || r.Release == "" {
		return false
	}
	p := a.res[res]
	if p == nil || t-p.lastRelease < 10000 {
		return false
	}
	if waitingFor == "" && len(a.leasesOn(res)) > 0 {
		return false // someone has it on purpose
	}
	if a.foreignMiB(res) <= r.IdleMiB {
		return false
	}
	holders := []string{}
	for _, l := range a.leasesOn(res) {
		holders = append(holders, l.Holder+":"+l.Class)
	}
	env := []string{"GAFFER_RES=" + res, "GAFFER_HOLDERS=" + strings.Join(holders, ","), "GAFFER_WAITING_FOR=" + waitingFor,
		"GAFFER_SHARES=" + strings.Join(shares, " ")}
	p.lastRelease = t
	log.Printf("%s: %s in foreign use (idle %s); running the release hook", res, gib(a.foreignMiB(res)), gib(r.IdleMiB))
	if out, err := runHook(r.Release, 60*time.Second, env); err != nil {
		log.Printf("release %s: %v", res, err)
	} else if s := strings.TrimSpace(out); s != "" {
		log.Printf("release %s: %s", res, lastLine(s))
	}
	if out, err := runHook(r.Probe, 10*time.Second, nil); err == nil {
		a.fold(res, out, now())
	}
	return true
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// resourceReports is the probe view for the sync: used, total, idle time, and the holders in the order
// the board wants them: leases, jobs, then foreign processes.
func (a *Agent) resourceReports() map[string]proto.ResourceReport {
	if len(a.res) == 0 {
		return nil
	}
	out := map[string]proto.ResourceReport{}
	for res, p := range a.res {
		if p.At == 0 {
			continue
		}
		r := proto.ResourceReport{Used: p.Used, Total: p.Total, Idle: now() - p.UsedSince}
		for _, l := range a.leasesOn(res) {
			r.Holders = append(r.Holders, l.text())
		}
		seen := map[int64]bool{}
		var jobs []string
		for _, pc := range p.Procs {
			if j := a.jobOfPID(pc.PID); j != nil && !seen[j.ID] {
				seen[j.ID] = true
				jobs = append(jobs, "#"+strconv.FormatInt(j.ID, 10))
			}
		}
		sort.Strings(jobs)
		r.Holders = append(r.Holders, jobs...)
		if a.foreignMiB(res) > a.idleMiB(res) {
			r.Holders = append(r.Holders, a.foreignNames(res)...)
		}
		out[res] = r
	}
	return out
}
