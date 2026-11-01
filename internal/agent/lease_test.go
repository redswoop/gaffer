package agent

import (
	"os"
	"path/filepath"
	"testing"

	"gaffer/internal/config"
	"gaffer/internal/proto"
)

func testAgent(t *testing.T) *Agent {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Agent.Host, cfg.Agent.Dir = "alpha", dir
	a := &Agent{cfg: cfg, host: "alpha", dir: dir, jobs: map[int64]*job{}, leases: map[string]*lease{}, res: map[string]*probed{}}
	return a
}

func gpuJob(a *Agent, id int64, shares ...string) *job {
	j := &job{ID: id, State: proto.JRunning, Spec: proto.Spec{Needs: "gpu", Shares: shares, Steps: []proto.Step{{Name: "run"}}},
		dir: filepath.Join(a.dir, "jobs", "x")}
	os.MkdirAll(j.dir, 0o755)
	a.jobs[id] = j
	return j
}

func TestLeaseGrantRefreshRefuse(t *testing.T) {
	a := testAgent(t)
	r := a.acquire(proto.LeaseReq{Res: "gpu", Holder: "qwen", Class: "llm", TTLMS: 1000}, 1000)
	if r.State != "granted" || r.Lease == nil || r.Lease.Expires != 2000 || !r.Ready {
		t.Fatalf("grant: %+v", r)
	}
	if r = a.acquire(proto.LeaseReq{Res: "gpu", Holder: "qwen", Class: "llm", TTLMS: 1000}, 1500); r.State != "refreshed" || r.Lease.Expires != 2500 {
		t.Fatalf("refresh: %+v", r)
	}
	if r = a.acquire(proto.LeaseReq{Res: "gpu", Holder: "game", Exclusive: true, TTLMS: 1000}, 1600); r.State != "refused" {
		t.Fatalf("exclusive next to a lease: %+v", r)
	}
	if r = a.acquire(proto.LeaseReq{Res: "gpu", Holder: "comfy", Class: "comfy", TTLMS: 1000}, 1600); r.State != "granted" {
		t.Fatalf("second class: %+v", r)
	}
	if r = a.acquire(proto.LeaseReq{Res: "gpu", Holder: "bad name!", TTLMS: 1}, 1); r.State != "refused" {
		t.Fatalf("bad holder: %+v", r)
	}
	if r = a.acquire(proto.LeaseReq{Res: "gpu", Holder: "p"}, 1); r.State != "refused" {
		t.Fatalf("no ttl, no process: %+v", r)
	}
	if n := len(a.leaseList()); n != 2 {
		t.Fatalf("leases: %d", n)
	}
}

func TestLeaseYieldsAndShares(t *testing.T) {
	a := testAgent(t)
	j := gpuJob(a, 1)
	s := gpuJob(a, 2, "llm")
	c := gpuJob(a, 3)
	c.Spec.Needs = "cpu"
	r := a.acquire(proto.LeaseReq{Res: "gpu", Holder: "qwen", Class: "llm", TTLMS: 1000}, 1000)
	if j.StopMode != "yield" || j.StopBy != "lease:qwen" || j.StopRes != "gpu" {
		t.Errorf("gpu job not yielded: %+v", j)
	}
	if s.StopMode != "" || c.StopMode != "" {
		t.Errorf("sharing or cpu job bumped: %q %q", s.StopMode, c.StopMode)
	}
	if len(r.Bumping) != 1 || r.Bumping[0] != 1 || r.Ready {
		t.Errorf("resp: %+v", r)
	}
	if l := a.blockingLease("gpu", s.Spec, 0, 2); l != nil {
		t.Errorf("sharing job blocked by %v", l)
	}
	if l := a.blockingLease(c.Spec.NeedsAt(0), c.Spec, 0, 3); l != nil {
		t.Errorf("cpu job blocked by %v", l)
	}
	a.acquire(proto.LeaseReq{Res: "gpu", Holder: "game", Class: "game", Exclusive: true, TTLMS: 1000}, 1000) // refused, nothing changes
	if s.StopMode != "" {
		t.Errorf("a refused exclusive lease bumped the sharing job")
	}
	// a lease taken from inside a job never bumps that job
	a.release("gpu", "qwen")
	j.StopMode, j.StopBy = "", ""
	a.acquire(proto.LeaseReq{Res: "gpu", Holder: "me", Class: "x", TTLMS: 1000, Job: 1}, 1000)
	if j.StopMode != "" {
		t.Errorf("own lease bumped the job")
	}
}

func TestLeaseLivenessAndDisk(t *testing.T) {
	a := testAgent(t)
	a.acquire(proto.LeaseReq{Res: "gpu", Holder: "qwen", Class: "llm", TTLMS: 1000}, 1000)
	a.acquire(proto.LeaseReq{Res: "gpu", Holder: "proc", PID: os.Getpid(), Boot: bootID(), Start: selfIdent().Start}, 1000)
	a.acquire(proto.LeaseReq{Res: "gpu", Holder: "dead", PID: 999999, Boot: bootID()}, 1000)
	a.acquire(proto.LeaseReq{Res: "gpu", Holder: "oldboot", PID: os.Getpid(), Boot: "another"}, 1000)
	b := testAgent(t)
	b.dir, b.cfg.Agent.Dir = a.dir, a.dir
	b.loadLeases() // the dead and the other-boot ones were dropped on the next enforce after their grant
	if len(b.leases) != 2 {
		t.Fatalf("loaded %d leases from disk, want 2", len(b.leases))
	}
	b.enforceLeases(1500)
	if _, ok := b.leases["gpu/qwen"]; !ok {
		t.Errorf("ttl lease dropped before expiry")
	}
	if _, ok := b.leases["gpu/proc"]; !ok {
		t.Errorf("live process lease dropped")
	}
	if _, ok := b.leases["gpu/dead"]; ok {
		t.Errorf("dead process lease kept")
	}
	if _, ok := b.leases["gpu/oldboot"]; ok {
		t.Errorf("lease from another boot kept")
	}
	b.enforceLeases(2001)
	if _, ok := b.leases["gpu/qwen"]; ok {
		t.Errorf("ttl lease kept past expiry")
	}
	if files, _ := filepath.Glob(filepath.Join(a.dir, "leases", "*", "*.json")); len(files) != 1 {
		t.Errorf("disk has %d lease files, want 1 (proc)", len(files))
	}
}

func TestProbeFold(t *testing.T) {
	a := testAgent(t)
	a.cfg.Agent.Resources = map[string]config.Resource{"gpu": {Probe: "true", IdleMiB: 1500}}
	a.res["gpu"] = &probed{}
	a.fold("gpu", "used=14000 total=32000\n10463 sunshine 508\n999999 ghost 7000\n", 1000)
	a.res["gpu"].Procs[0].PID = os.Getpid() // a live foreign process (this test) worth 508
	p := a.res["gpu"]
	if p.Used != 14000 || p.Total != 32000 || len(p.Procs) != 2 || p.UsedSince != 1000 {
		t.Fatalf("fold: %+v", p)
	}
	if got := a.foreignMiB("gpu"); got != 508 { // the dead pid's share is not foreign; neither is the driver's overhead
		t.Errorf("foreign = %d, want 508", got)
	}
	a.fold("gpu", "used=14100 total=32000\n", 5000)
	if p.UsedSince != 1000 {
		t.Errorf("a small wobble reset the idle clock")
	}
	a.fold("gpu", "used=4000 total=32000\n", 6000)
	if p.UsedSince != 6000 {
		t.Errorf("a real change didn't reset the idle clock")
	}
	r := a.resourceReports()["gpu"]
	if r.Used != 4000 || r.Total != 32000 {
		t.Errorf("report: %+v", r)
	}
}
