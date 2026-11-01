package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gaffer/internal/agent"
	"gaffer/internal/config"
	"gaffer/internal/proto"
)

// `gaffer lease RES ...` talks to this box's agent over its unix socket, never to the controller: a lease
// must work with the network gone. Exit codes: 0 granted (and, with --wait, the resource is actually
// free), 1 refused / timed out / lease gone, 2 usage, 3 no agent here.

const (
	leaseOK      = 0
	leaseRefused = 1
	leaseUsage   = 2
	leaseNoAgent = 3
)

type agentClient struct {
	hc   *http.Client
	sock string
}

func newAgentClient(cfg *config.Config) *agentClient {
	sock := agent.SockPath(cfg.Agent.Dir)
	return &agentClient{sock: sock, hc: &http.Client{Timeout: 90 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}}}}
}

func (a *agentClient) do(method, path string, body, out any) error {
	var rd *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	} else {
		rd = strings.NewReader("")
	}
	req, _ := http.NewRequest(method, "http://agent"+path, rd)
	res, err := a.hc.Do(req)
	if err != nil {
		if _, serr := os.Stat(a.sock); serr != nil {
			return fmt.Errorf("no agent running here (%s): %w", a.sock, err)
		}
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		var b strings.Builder
		json.NewDecoder(res.Body) // drain
		return fmt.Errorf("agent: %s%s", res.Status, b.String())
	}
	if out != nil {
		return json.NewDecoder(res.Body).Decode(out)
	}
	return nil
}

func leaseExit(err error) {
	fmt.Fprintln(os.Stderr, "gaffer:", err)
	if strings.Contains(err.Error(), "no agent running here") {
		os.Exit(leaseNoAgent)
	}
	os.Exit(leaseRefused)
}

func leaseCmd(cfg *config.Config, args []string) {
	fs := flag.NewFlagSet("lease", flag.ExitOnError)
	holder := fs.String("holder", "", "who holds it (default: your user name, or the command's name)")
	class := fs.String("class", "", "what kind of use; jobs whose kind `shares` it keep running (default: the holder)")
	ttl := fs.Duration("ttl", 30*time.Minute, "how long the lease lasts without being refreshed (not with -- CMD)")
	wait := fs.Bool("wait", false, "return only once the resource is actually free (implied with -- CMD)")
	timeout := fs.Duration("timeout", 90*time.Second, "give up waiting after this long")
	exclusive := fs.Bool("exclusive", false, "refuse every other lease (and every job) while held")
	release := fs.Bool("release", false, "give the lease up")
	res := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		res, args = args[0], args[1:]
	}
	fs.Parse(args)
	rest := fs.Args()
	if res == "" && len(rest) > 0 {
		res, rest = rest[0], rest[1:]
	}
	if res == "" {
		fmt.Fprintln(os.Stderr, "gaffer: lease RES [--holder NAME] [--class C] [--ttl DUR] [--wait] [--exclusive] [--release] [-- CMD...]")
		os.Exit(leaseUsage)
	}
	cmd := rest
	if *holder == "" {
		if len(cmd) > 0 {
			*holder = filepath.Base(cmd[0])
		} else if u := os.Getenv("USER"); u != "" {
			*holder = u
		} else {
			*holder = "someone"
		}
	}
	ac := newAgentClient(cfg)
	if *release {
		var r proto.LeaseResp
		if err := ac.do("DELETE", "/lease/"+res+"/"+*holder, nil, &r); err != nil {
			leaseExit(err)
		}
		fmt.Printf("%s: %s (%s)\n", res, r.Reason, *holder)
		return
	}
	req := proto.LeaseReq{Res: res, Holder: *holder, Class: *class, Exclusive: *exclusive, TTLMS: ttl.Milliseconds()}
	if id, err := strconv.ParseInt(os.Getenv("W_ID"), 10, 64); err == nil {
		req.Job = id // from inside a job: that job is a co-holder, never bumped by its own lease
	}
	scope := ""
	if len(cmd) > 0 {
		req.TTLMS = 0
		req.PID, req.Boot, req.Start = agent.SelfIdent()
		if exec.Command("systemd-run", "--user", "--scope", "--quiet", "--collect", "true").Run() == nil {
			scope = fmt.Sprintf("gaffer-lease-%s-%s-%d", res, *holder, os.Getpid())
			req.Scope = scope + ".scope"
		}
		*wait = true
	}
	var r proto.LeaseResp
	if err := ac.do("POST", "/lease", req, &r); err != nil {
		leaseExit(err)
	}
	if r.State == "refused" {
		fmt.Fprintf(os.Stderr, "gaffer: %s refused: %s\n", res, r.Reason)
		os.Exit(leaseRefused)
	}
	fmt.Println(leaseLine(res, r))
	if *wait && !r.Ready {
		deadline := time.Now().Add(*timeout)
		lastPost := time.Now()
		for !r.Ready {
			if time.Now().After(deadline) {
				fmt.Fprintf(os.Stderr, "gaffer: %s not free after %s: %s\n", res, timeout, strings.Join(r.Pending, "; "))
				if len(cmd) > 0 {
					ac.do("DELETE", "/lease/"+res+"/"+*holder, nil, nil)
				}
				os.Exit(leaseRefused)
			}
			time.Sleep(time.Second)
			if time.Since(lastPost) > 10*time.Second { // keep it fresh: a lease can't expire under its own waiter
				lastPost = time.Now()
				if err := ac.do("POST", "/lease", req, &r); err != nil {
					leaseExit(err)
				}
				continue
			}
			if err := ac.do("GET", "/lease/"+res+"/"+*holder, nil, &r); err != nil {
				leaseExit(err)
			}
			if r.State == "gone" {
				fmt.Fprintf(os.Stderr, "gaffer: the lease went away while waiting (%s)\n", r.Reason)
				os.Exit(leaseRefused)
			}
		}
		fmt.Printf("%s: free\n", res)
	}
	if len(cmd) == 0 {
		return
	}
	// process-bound: run the command, hand the lease back when it exits (or we're told to stop)
	var c *exec.Cmd
	if scope != "" {
		c = exec.Command("systemd-run", append([]string{"--user", "--scope", "--quiet", "--collect", "--unit", scope, "--"}, cmd...)...)
	} else {
		c = exec.Command(cmd[0], cmd[1:]...)
	}
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	if err := c.Start(); err != nil {
		ac.do("DELETE", "/lease/"+res+"/"+*holder, nil, nil)
		fmt.Fprintln(os.Stderr, "gaffer:", err)
		os.Exit(leaseRefused)
	}
	go func() {
		for s := range sig {
			c.Process.Signal(s)
		}
	}()
	err := c.Wait()
	ac.do("DELETE", "/lease/"+res+"/"+*holder, nil, nil)
	fmt.Fprintf(os.Stderr, "gaffer: %s released (%s exited)\n", res, filepath.Base(cmd[0]))
	if ee, ok := err.(*exec.ExitError); ok {
		os.Exit(ee.ExitCode())
	} else if err != nil {
		os.Exit(leaseRefused)
	}
}

func leaseLine(res string, r proto.LeaseResp) string {
	l := r.Lease
	s := res + ": " + r.State
	if l != nil {
		s += " to " + l.Holder
		if l.Class != "" && l.Class != l.Holder {
			s += " (" + l.Class + ")"
		}
		if l.Exclusive {
			s += ", exclusive"
		}
		if l.Expires > 0 {
			s += " until " + clockMS(l.Expires)
		} else if l.PID > 0 {
			s += " while pid " + strconv.Itoa(l.PID) + " lives"
		}
	}
	if len(r.Bumping) > 0 {
		ids := make([]string, len(r.Bumping))
		for i, id := range r.Bumping {
			ids[i] = "#" + strconv.FormatInt(id, 10)
		}
		s += "; yielding " + strings.Join(ids, " ")
	}
	if !r.Ready && len(r.Pending) > 0 {
		s += "; still on it: " + strings.Join(r.Pending, ", ")
	}
	return s
}

func leasesCmd(cfg *config.Config, args []string) {
	fs := flag.NewFlagSet("leases", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "raw")
	fs.Parse(args)
	ac := newAgentClient(cfg)
	var ls []proto.Lease
	if err := ac.do("GET", "/leases", nil, &ls); err != nil {
		leaseExit(err)
	}
	var rs map[string]proto.ResourceReport
	ac.do("GET", "/resources", nil, &rs)
	if *asJSON {
		json.NewEncoder(os.Stdout).Encode(map[string]any{"leases": ls, "resources": rs})
		return
	}
	now := time.Now().UnixMilli()
	for _, l := range ls {
		life := "until " + clockMS(l.Expires) + " unless refreshed"
		if l.Expires == 0 {
			life = fmt.Sprintf("while pid %d lives", l.PID)
		}
		ex := ""
		if l.Exclusive {
			ex = " exclusive"
		}
		fmt.Printf("%-6s %s (%s)%s  since %s, refreshed %s ago, %s\n", l.Res, l.Holder, l.Class, ex, clockMS(l.Since), fmtDur(float64(now-l.Refreshed)/1000), life)
	}
	names := make([]string, 0, len(rs))
	for n := range rs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		r := rs[n]
		fmt.Printf("%-6s %s of %s in use, steady for %s", n, gib(r.Used), gib(r.Total), fmtDur(float64(r.Idle)/1000))
		if len(r.Holders) > 0 {
			fmt.Printf(": %s", strings.Join(r.Holders, ", "))
		}
		fmt.Println()
	}
	if len(ls) == 0 && len(rs) == 0 {
		fmt.Println("no leases, no resource probes configured")
	}
}

func gib(mib int) string {
	if mib >= 1024 {
		return fmt.Sprintf("%.1fG", float64(mib)/1024)
	}
	return fmt.Sprintf("%dM", mib)
}
