package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"gaffer/internal/proto"
)

// The agent's local API: a unix socket in its dir (0600), for `gaffer lease` / `gaffer leases`. Every
// handler hands its work to the loop as a closure and waits for the answer, so the loop stays the only
// thing that touches jobs and leases. A fits check can hold the loop for up to a minute; a caller that
// waits longer than that gets a 503 rather than hanging.

func SockPath(agentDir string) string { return filepath.Join(agentDir, "agent.sock") }

func (a *Agent) listen() error {
	p := SockPath(a.dir)
	os.Remove(p) // ours: the flock already says no other agent runs here
	ln, err := net.Listen("unix", p)
	if err != nil {
		return fmt.Errorf("agent socket: %w", err)
	}
	os.Chmod(p, 0o600)
	m := http.NewServeMux()
	m.HandleFunc("POST /lease", func(w http.ResponseWriter, r *http.Request) {
		var req proto.LeaseReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad lease request", 400)
			return
		}
		a.onLoop(w, func() any { return a.acquire(req, now()) })
	})
	m.HandleFunc("GET /lease/{res}/{holder}", func(w http.ResponseWriter, r *http.Request) {
		res, holder := r.PathValue("res"), r.PathValue("holder")
		a.onLoop(w, func() any {
			l := a.leases[res+"/"+holder]
			if l == nil {
				return proto.LeaseResp{State: "gone", Reason: "no lease by " + holder + " on " + res}
			}
			return a.leaseStatus(l, "held", now(), true)
		})
	})
	m.HandleFunc("DELETE /lease/{res}/{holder}", func(w http.ResponseWriter, r *http.Request) {
		res, holder := r.PathValue("res"), r.PathValue("holder")
		a.onLoop(w, func() any {
			if a.release(res, holder) {
				return proto.LeaseResp{State: "gone", Reason: "released"}
			}
			return proto.LeaseResp{State: "gone", Reason: "no lease by " + holder + " on " + res}
		})
	})
	m.HandleFunc("GET /leases", func(w http.ResponseWriter, r *http.Request) {
		a.onLoop(w, func() any { return a.leaseList() })
	})
	m.HandleFunc("GET /resources", func(w http.ResponseWriter, r *http.Request) {
		a.onLoop(w, func() any {
			out := a.resourceReports()
			if out == nil {
				out = map[string]proto.ResourceReport{}
			}
			return out
		})
	})
	go http.Serve(ln, m)
	return nil
}

var errBusy = errors.New("agent busy (a fits check is running); try again")

func (a *Agent) onLoop(w http.ResponseWriter, f func() any) {
	ch := make(chan any, 1)
	a.ops <- func() { ch <- f() }
	select {
	case v := <-ch:
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	case <-time.After(70 * time.Second):
		http.Error(w, errBusy.Error(), 503)
	}
}
