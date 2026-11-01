package agent

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gaffer/internal/proto"
)

// A lease is a holder of a resource on this host that isn't a job step: an interactive user with a TTL it
// refreshes while it's active (a chat model, a render UI), or a process that holds it while it lives (a
// game). The agent is the authority: leases live in AGENTDIR/leases/RES/HOLDER.json, so they survive an
// agent restart and work with the controller down. The first grant on a resource stops every running step
// that needs it and doesn't share the lease's class (yield: the controller requeues it when the lease is
// gone); while a lease holds, no such step starts. Leases never bump leases: an exclusive one refuses
// everything else, and nothing can be granted next to an exclusive one.
type lease struct {
	Res       string `json:"res"`
	Holder    string `json:"holder"`
	Class     string `json:"class"`
	Exclusive bool   `json:"exclusive,omitempty"`
	Since     int64  `json:"since"`
	Refreshed int64  `json:"refreshed"`
	Expires   int64  `json:"expires,omitempty"` // 0: process-bound
	Boot      string `json:"boot"`
	Ident     *ident `json:"ident,omitempty"` // the holder process (process-bound)
	Scope     string `json:"scope,omitempty"` // its systemd scope, if any: the lease lives while that is active
	Job       int64  `json:"job,omitempty"`   // taken from inside this job, which is then a co-holder

	releasedAt int64 // when the release hook last ran for this holder's --wait (not persisted)
}

var holderRx = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,40}$`)

func (l *lease) key() string { return l.Res + "/" + l.Holder }

// text is the holder as the board names it: "qwen (llm)", "game exclusive".
func (l *lease) text() string {
	s := l.Holder
	if l.Class != "" && l.Class != l.Holder {
		s += " (" + l.Class + ")"
	}
	if l.Exclusive {
		s += " exclusive"
	}
	return s
}

func (l *lease) proto() proto.Lease {
	p := proto.Lease{Res: l.Res, Holder: l.Holder, Class: l.Class, Exclusive: l.Exclusive, Since: l.Since,
		Refreshed: l.Refreshed, Expires: l.Expires, Job: l.Job}
	if l.Ident != nil {
		p.PID = l.Ident.PID
	}
	return p
}

func (a *Agent) leasePath(l *lease) string {
	return filepath.Join(a.dir, "leases", l.Res, l.Holder+".json")
}

func (a *Agent) loadLeases() {
	files, _ := filepath.Glob(filepath.Join(a.dir, "leases", "*", "*.json"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		l := &lease{}
		if err := unmarshal(b, l); err != nil || l.Res == "" || l.Holder == "" {
			os.Remove(f)
			continue
		}
		a.leases[l.key()] = l
	}
}

func (a *Agent) saveLease(l *lease) error {
	if err := os.MkdirAll(filepath.Dir(a.leasePath(l)), 0o755); err != nil {
		return err
	}
	return writeJSON(a.leasePath(l), l)
}

func (a *Agent) dropLease(l *lease, why string) {
	delete(a.leases, l.key())
	os.Remove(a.leasePath(l))
	log.Printf("lease %s on %s %s", l.text(), l.Res, why)
}

// leaseList is every live lease, for the sync report. Never nil: nil would mean "an agent that doesn't
// know about leases".
func (a *Agent) leaseList() []proto.Lease {
	out := []proto.Lease{}
	for _, l := range a.leases {
		out = append(out, l.proto())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Res != out[j].Res {
			return out[i].Res < out[j].Res
		}
		return out[i].Since < out[j].Since
	})
	return out
}

func (a *Agent) leasesOn(res string) []*lease {
	var out []*lease
	for _, l := range a.leases {
		if l.Res == res {
			out = append(out, l)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since < out[j].Since })
	return out
}

func scopeActive(scope string) bool {
	out, _ := exec.Command("systemctl", "--user", "is-active", scope).Output()
	return strings.TrimSpace(string(out)) == "active"
}

// liveLease: a TTL lease until it expires; a process lease while its holder lives in this boot (the
// process, or the scope it started its command in).
func (a *Agent) liveLease(l *lease, t int64) (bool, string) {
	if l.Expires > 0 {
		if t >= l.Expires {
			return false, "expired (idle)"
		}
		return true, ""
	}
	if l.Boot != bootID() {
		return false, "from a previous boot"
	}
	if l.Ident != nil && l.Ident.alive() {
		return true, ""
	}
	if l.Scope != "" && scopeActive(l.Scope) {
		return true, ""
	}
	return false, "holder gone"
}

// enforceLeases, every tick: drop dead leases, and tell every running step that needs a leased resource
// it doesn't share to step aside. Level-triggered: a lease loaded from disk after a restart bumps what
// it should just the same, and a step that moved onto a leased resource gets caught on the next tick.
func (a *Agent) enforceLeases(t int64) {
	for _, l := range a.leases {
		if ok, why := a.liveLease(l, t); !ok {
			a.dropLease(l, why)
		}
	}
	if len(a.leases) == 0 {
		return
	}
	for _, j := range a.jobs {
		if j.State != proto.JRunning || j.StopMode != "" || j.Spec.OnInterrupt == "hold" {
			continue
		}
		n := j.Spec.NeedsAt(j.Step)
		if l := a.blockingLease(n, j.Spec, j.Step, j.ID); l != nil {
			a.stop(j, "yield", "lease:"+l.Holder, n, t)
			log.Printf("#%d: yielding %s to %s", j.ID, n, l.text())
		}
	}
}

// blockingLease: the lease on res that keeps this step off it, or nil. A lease taken from inside the job
// itself never blocks it; an exclusive lease blocks everything; otherwise the step must share every live
// class on the resource.
func (a *Agent) blockingLease(res string, spec proto.Spec, step int, job int64) *lease {
	for _, l := range a.leasesOn(res) {
		if job != 0 && l.Job == job {
			continue
		}
		if l.Exclusive || !spec.SharesAll(step, []string{l.Class}) {
			return l
		}
	}
	return nil
}

// acquire grants, refreshes or refuses a lease, on the loop.
func (a *Agent) acquire(req proto.LeaseReq, t int64) proto.LeaseResp {
	if req.Res == "" || !holderRx.MatchString(req.Holder) {
		return proto.LeaseResp{State: "refused", Reason: "need a resource and a holder name ([A-Za-z0-9_.-], up to 40)"}
	}
	if req.Class == "" {
		req.Class = req.Holder
	}
	key := req.Res + "/" + req.Holder
	if l := a.leases[key]; l != nil {
		l.Refreshed = t
		if req.TTLMS > 0 {
			l.Expires = t + req.TTLMS
		}
		a.saveLease(l)
		return a.leaseStatus(l, "refreshed", t, false) // a refresh is a heartbeat: nobody is waiting, don't go clearing things
	}
	for _, o := range a.leasesOn(req.Res) {
		if o.Exclusive || req.Exclusive {
			return proto.LeaseResp{State: "refused", Reason: fmt.Sprintf("%s: held by %s since %s", req.Res, o.text(), clock(o.Since))}
		}
	}
	l := &lease{Res: req.Res, Holder: req.Holder, Class: req.Class, Exclusive: req.Exclusive, Since: t, Refreshed: t,
		Boot: bootID(), Scope: req.Scope, Job: req.Job}
	if req.TTLMS > 0 {
		l.Expires = t + req.TTLMS
	} else {
		if req.PID == 0 && req.Scope == "" {
			return proto.LeaseResp{State: "refused", Reason: "a lease without a ttl needs a holder process"}
		}
		l.Ident = &ident{PID: req.PID, Boot: bootID(), Start: req.Start} // the holder is on this box: this boot, whatever it thinks
	}
	if err := a.saveLease(l); err != nil {
		return proto.LeaseResp{State: "refused", Reason: "can't record the lease: " + err.Error()}
	}
	a.leases[key] = l
	log.Printf("lease %s on %s granted", l.text(), l.Res)
	a.enforceLeases(t)
	resp := a.leaseStatus(l, "granted", t, true)
	for _, j := range a.jobs {
		if j.StopMode == "yield" && j.StopBy == "lease:"+l.Holder && j.State == proto.JRunning {
			resp.Bumping = append(resp.Bumping, j.ID)
		}
	}
	sort.Slice(resp.Bumping, func(i, k int) bool { return resp.Bumping[i] < resp.Bumping[k] })
	return resp
}

func (a *Agent) release(res, holder string) bool {
	l := a.leases[res+"/"+holder]
	if l == nil {
		return false
	}
	a.dropLease(l, "released")
	return true
}

// leaseStatus: is the resource actually free for this holder? No step of gaffer's that must yield to it
// is still alive, no process of such a step is still on it (the probe says), and foreign residents are
// at or below idle (with `clear`, the release hook is run once a minute to get them there: for a grant
// and for a --wait poll, not for a heartbeat refresh).
func (a *Agent) leaseStatus(l *lease, state string, t int64, clear bool) proto.LeaseResp {
	resp := proto.LeaseResp{State: state}
	p := l.proto()
	resp.Lease = &p
	mine := map[int64]bool{}
	for _, j := range a.jobs {
		if j.State != proto.JRunning || j.ID == l.Job || j.Spec.NeedsAt(j.Step) != l.Res {
			continue
		}
		if !l.Exclusive && j.Spec.SharesAll(j.Step, []string{l.Class}) {
			continue
		}
		mine[j.ID] = true
		switch {
		case j.StopMode == "" || j.Spec.OnInterrupt == "hold":
			resp.Pending = append(resp.Pending, fmt.Sprintf("#%d still running (%s)", j.ID, j.Spec.Steps[j.Step].Name))
		case j.KillSent:
			resp.Pending = append(resp.Pending, fmt.Sprintf("#%d killed, waiting for it to go", j.ID))
		default:
			left := int64(max(j.Spec.StopGrace, 1))*1000 - (t - j.StopAt)
			resp.Pending = append(resp.Pending, fmt.Sprintf("#%d still stopping (KILL in %ds)", j.ID, max(0, left/1000)))
		}
	}
	if pr := a.res[l.Res]; pr != nil {
		for _, pc := range pr.Procs {
			if j := a.jobOfPID(pc.PID); j != nil && mine[j.ID] && alivePID(pc.PID) {
				resp.Pending = append(resp.Pending, fmt.Sprintf("#%d's %s (pid %d) still on it", j.ID, pc.Name, pc.PID))
			}
		}
		if len(resp.Pending) == 0 {
			if clear && a.foreignMiB(l.Res) > a.idleMiB(l.Res) && (l.releasedAt == 0 || t-l.releasedAt > 60000) {
				if a.releaseIdle(l.Res, l.Holder, nil, t) {
					l.releasedAt = t
				}
			}
			if a.foreignMiB(l.Res) > a.idleMiB(l.Res) {
				for _, n := range a.foreignNames(l.Res) {
					resp.Pending = append(resp.Pending, n+" (not gaffer's)")
				}
			}
		}
	}
	resp.Ready = len(resp.Pending) == 0
	return resp
}
