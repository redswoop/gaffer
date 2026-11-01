// Package cli is the human side: submit, look, steer.
package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gaffer/internal/config"
	"gaffer/internal/proto"
)

type client struct {
	cfg *config.Config
	hc  *http.Client
}

func newClient(cfg *config.Config) *client {
	return &client{cfg, &http.Client{Timeout: 30 * time.Second}}
}

func (c *client) do(method, path string, body any, out any) error {
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		j, _ := json.Marshal(b)
		rd = bytes.NewReader(j)
	}
	req, _ := http.NewRequest(method, strings.TrimRight(c.cfg.Controller, "/")+path, rd)
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	res, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("controller %s: %w", c.cfg.Controller, err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		b, _ := io.ReadAll(res.Body)
		return fmt.Errorf("%s", strings.TrimSpace(string(b)))
	}
	if out != nil {
		return json.NewDecoder(res.Body).Decode(out)
	}
	return nil
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func die(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "gaffer:", err)
		os.Exit(1)
	}
}

func Main(cfg *config.Config, cmd string, args []string) {
	c := newClient(cfg)
	switch cmd {
	case "submit", "run":
		submit(c, cmd, args)
	case "ls":
		fs := flag.NewFlagSet("ls", flag.ExitOnError)
		all := fs.Bool("a", false, "include finished items older than a day")
		asJSON := fs.Bool("json", false, "raw status")
		fs.Parse(args)
		var st proto.Status
		die(c.do("GET", "/api/status"+map[bool]string{true: "?all=1", false: ""}[*all], nil, &st))
		if *asJSON {
			json.NewEncoder(os.Stdout).Encode(st)
			return
		}
		render(os.Stdout, st, termStyle(os.Stdout, *all))
	case "watch":
		watch(c)
	case "show":
		show(c, args)
	case "log":
		logCmd(c, args)
	case "wait":
		wait(c, args)
	case "shutdown":
		shutdown(cfg, args)
	case "lease":
		leaseCmd(cfg, args)
	case "leases":
		leasesCmd(cfg, args)
	case "host":
		if len(args) != 2 || (args[0] != "hold" && args[0] != "release" && args[0] != "drain") {
			die(fmt.Errorf("host hold|release|drain HOST"))
		}
		hostCmd(c, args[0], args[1])
	case "pause", "cancel", "resume", "ack", "top", "retry":
		fs := flag.NewFlagSet(cmd, flag.ExitOnError)
		resume := fs.Bool("resume", false, "retry: keep the step + checkpoint instead of starting over")
		fs.Parse(args)
		if fs.NArg() == 0 {
			die(fmt.Errorf("%s ID...", cmd))
		}
		body := ""
		if *resume {
			body = "resume"
		}
		failed := false
		for _, id := range fs.Args() {
			var r map[string]string
			if err := c.do("POST", "/api/items/"+id+"/"+cmd, body, &r); err != nil {
				fmt.Fprintln(os.Stderr, "gaffer:", err)
				failed = true
				continue
			}
			fmt.Printf("#%s -> %s\n", id, r["ok"])
		}
		if failed {
			os.Exit(1)
		}
	case "track":
		fs := flag.NewFlagSet("track", flag.ExitOnError)
		kind := fs.String("kind", "observed", "")
		host := fs.String("host", "", "where it runs (display only)")
		stall := fs.Int("stall", 0, "seconds without an emit before it's stalled (0 = default, -1 = never)")
		fs.Parse(args)
		var r map[string]int64
		die(c.do("POST", "/api/track", map[string]any{"title": strings.Join(fs.Args(), " "), "kind": *kind, "host": *host, "stall_after": *stall}, &r))
		fmt.Println(r["id"])
	case "emit":
		emit(c, args)
	case "do":
		if len(args) < 2 {
			die(fmt.Errorf("do ACTION ID..."))
		}
		doAction(c, args[0], args[1:])
	case "kinds":
		fs := flag.NewFlagSet("kinds", flag.ExitOnError)
		dirs := fs.Bool("dirs", false, "just the kinds dirs, in lookup order")
		fs.Parse(args)
		if *dirs {
			for _, d := range cfg.Kinds {
				fmt.Println(d)
			}
			return
		}
		seen := map[string]string{}
		for _, d := range cfg.Kinds {
			ms, _ := filepath.Glob(filepath.Join(d, "*", "kind.toml"))
			for _, m := range ms {
				name, dir := filepath.Base(filepath.Dir(m)), filepath.Dir(m)
				if first, ok := seen[name]; ok {
					fmt.Printf("%-14s %s  (shadowed by %s)\n", name, dir, first)
					continue
				}
				seen[name] = dir
				fmt.Printf("%-14s %s\n", name, dir)
			}
		}
	default:
		if len(args) > 0 && allIDs(args) { // `gaffer ship 13` = `gaffer do ship 13`
			doAction(c, cmd, args)
			return
		}
		usage()
	}
}

func allIDs(args []string) bool {
	for _, a := range args {
		if _, err := strconv.Atoi(a); err != nil {
			return false
		}
	}
	return true
}

func doAction(c *client, name string, ids []string) {
	failed := false
	for _, id := range ids {
		var r map[string]string
		if err := c.do("POST", "/api/items/"+id+"/do", name, &r); err != nil {
			fmt.Fprintln(os.Stderr, "gaffer:", err)
			failed = true
			continue
		}
		fmt.Printf("#%s: %s queued\n", id, r["ok"])
	}
	if failed {
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `gaffer: run work, watch progress, survive reboots

  gaffer submit KIND [flags] [--] ARGS...    queue a job of a kind (kinds/<KIND>/kind.toml), on this box
  gaffer run [flags] -- CMD...               queue an ad-hoc shell command, on this box, with your PATH
  gaffer ls [-a] [--json]                    board: needs-you first, then running, queued, history
  gaffer watch                               the board, live
  gaffer show ID / log ID [-f]               detail + events / console log (-f ends when it settles)
  gaffer wait [--events] [--any] ID...       block until they settle, or one needs you; exit 0 done,
                                             1 failed, 2 needs you, 3 canceled, 124 --timeout
  gaffer pause|resume|cancel|retry [--resume]|ack|top ID...
  gaffer lease RES [--holder N] [--class C] [--ttl 30m] [--wait] [--exclusive] [--release] [-- CMD...]
                                             take a resource on this box for something that isn't a job (a chat
                                             model, a render UI, a game): its jobs step aside and come back
  gaffer leases                              who holds what here, and what the probe sees
  gaffer host hold|release|drain HOST        hold: no new work lands there (running jobs go on); release undoes
                                             it; drain: hold + pause everything running there
  gaffer shutdown [--wait SEC]               pause every job on this box and wait for it: the OS shutdown hook
  gaffer track TITLE...                      observe work gaffer doesn't run; prints an id
  gaffer emit [ID] LINE...                   ::progress / ::phase / ::checkpoint / ::needs-you / ::done / ::fail
                                             (inside a gaffer job: no ID, goes to the job's own log)
  gaffer do ACTION ID... / gaffer ACTION ID... an action the item's kind or observer offers (ship, …)
  gaffer kinds [--dirs]                      the kinds this config sees, first dir wins
  gaffer controller | agent                  the daemons
`)
	os.Exit(2)
}

func submit(c *client, cmd string, args []string) {
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	title := fs.String("title", "", "")
	host := fs.String("host", "", "run on this agent instead of the one on this box")
	needs := fs.String("needs", "", "slot label (gpu, cpu…)")
	prio, prioSet := 100, false
	fs.Func("prio", "lower runs first (default 100, or the kind's); a job 50+ below a running one's takes its slot", func(s string) error {
		n, err := strconv.Atoi(s)
		prio, prioSet = n, true
		return err
	})
	var shares multi
	fs.Var(&shares, "shares", "lease class this job can run alongside (repeatable): a Qwen job shares llm")
	held := fs.String("held", "", "queue held, with this reason")
	onInt := fs.String("on-interrupt", "", "resume | restart | hold")
	rx := fs.String("progress-regex", "", "run only: regex with done/total groups")
	preset := fs.String("progress", "", "run only: tqdm | rsync (--info=progress2) | ffmpeg | pct")
	stall := fs.Int("stall", 0, "seconds without progress before stalled")
	review := fs.Bool("review", false, "finish in needs-you instead of done")
	quiet := fs.Bool("quiet", false, "board only, no ntfy pushes")
	var env multi
	fs.Var(&env, "e", "K=V (repeatable)")
	kind := ""
	if cmd == "submit" { // `submit KIND [flags] [--] ARGS`: flag parsing stops at the first non-flag
		if len(args) == 0 || strings.HasPrefix(args[0], "-") {
			die(fmt.Errorf("submit KIND [flags] [--] ARGS..."))
		}
		kind, args = args[0], args[1:]
	}
	fs.Parse(args)
	var spec proto.Spec
	if cmd == "run" {
		if fs.NArg() == 0 {
			die(fmt.Errorf("run [flags] -- CMD..."))
		}
		line := shellJoin(fs.Args())
		if *preset != "" {
			r, ok := progressPresets[*preset]
			if !ok {
				die(fmt.Errorf("--progress %s: one of tqdm, rsync, ffmpeg, pct (or --progress-regex)", *preset))
			}
			*rx = r
		}
		spec = proto.Spec{Kind: "shell", Title: line, Steps: []proto.Step{{Name: "run", Run: line}}, ProgressRegex: *rx}
		if wd, err := os.Getwd(); err == nil {
			spec.Cwd = wd
		}
	} else {
		k, err := c.cfg.LoadKind(kind)
		die(err)
		spec = k.Spec(kind, fs.Args())
	}
	if *title != "" {
		spec.Title = *title
	}
	if *host != "" {
		spec.Host = *host
	} else if spec.Host == "" { // work runs where it was started: gaffer is a board, not a dispatcher
		spec.Host = c.cfg.Agent.Host
	}
	if cmd == "run" && spec.Host == c.cfg.Agent.Host { // here, so behave like typing it: your PATH and venv
		for _, k := range []string{"PATH", "VIRTUAL_ENV", "CONDA_PREFIX"} {
			if v := os.Getenv(k); v != "" {
				if spec.Env == nil {
					spec.Env = map[string]string{}
				}
				spec.Env[k] = v
			}
		}
	}
	if *needs != "" {
		spec.Needs = *needs
	}
	if len(shares) > 0 {
		spec.Shares = shares
	}
	if *onInt != "" {
		spec.OnInterrupt = *onInt
	}
	if *stall != 0 {
		spec.StallAfter = *stall
	}
	if *review {
		spec.Review = true
	}
	if *quiet {
		spec.Quiet = true
	}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if spec.Env == nil {
			spec.Env = map[string]string{}
		}
		spec.Env[k] = v
	}
	var st proto.Status
	if c.do("GET", "/api/status", nil, &st) == nil && !slices.ContainsFunc(st.Hosts, func(h proto.Host) bool { return h.Host == spec.Host }) {
		fmt.Fprintf(os.Stderr, "gaffer: note: no agent %q has reported in; this waits until one does\n", spec.Host)
	}
	body := map[string]any{"spec": spec, "held": *held}
	if prioSet {
		body["priority"] = prio
	}
	var r map[string]int64
	die(c.do("POST", "/api/items", body, &r))
	fmt.Printf("#%d queued  %s\n", r["id"], spec.Title)
}

// progressPresets are --progress-regex for tools everyone runs bare.
var progressPresets = map[string]string{
	"tqdm":   `(?P<done>\d+)/(?P<total>\d+) \[`,
	"rsync":  `(?P<pct>\d+)%\s+[\d.]+\S*B/s`,
	"ffmpeg": `frame=\s*(?P<done>\d+)`,
	"pct":    `(?P<pct>\d+(?:\.\d+)?)%`,
}

func shellJoin(a []string) string {
	if len(a) == 1 {
		return a[0] // one arg = a shell line already
	}
	out := make([]string, len(a))
	for i, s := range a {
		if s != "" && strings.IndexFunc(s, func(r rune) bool {
			return !(r == '/' || r == '.' || r == '-' || r == '_' || r == '=' || r == ':' || r == ',' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'))
		}) < 0 {
			out[i] = s
		} else {
			out[i] = "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return strings.Join(out, " ")
}

func emit(c *client, args []string) {
	var id string
	if len(args) > 0 {
		if _, err := strconv.Atoi(args[0]); err == nil {
			id, args = args[0], args[1:]
		}
	}
	text := strings.Join(args, " ")
	if text == "" {
		b, _ := io.ReadAll(os.Stdin)
		text = string(b)
	}
	if !strings.HasPrefix(strings.TrimSpace(text), "::") {
		text = "::" + text // `gaffer emit progress 3/10` works too
	}
	if id == "" {
		if lp := os.Getenv("W_LOG"); lp != "" { // inside a job: into its own log, which the agent tails
			f, err := os.OpenFile(lp, os.O_WRONLY|os.O_APPEND, 0o644)
			die(err)
			fmt.Fprintln(f, strings.TrimRight(text, "\n"))
			f.Close()
			return
		}
		die(fmt.Errorf("emit ID LINE (no W_LOG: not inside a gaffer job)"))
	}
	var r map[string]string
	die(c.do("POST", "/api/items/"+id+"/emit", text, &r))
}

func show(c *client, args []string) {
	if len(args) != 1 {
		die(fmt.Errorf("show ID"))
	}
	var d struct {
		Item   proto.Item `json:"item"`
		Events []struct {
			TS   int64  `json:"ts"`
			Kind string `json:"kind"`
			Msg  string `json:"msg"`
		} `json:"events"`
		Leases []proto.Lease `json:"leases"`
	}
	die(c.do("GET", "/api/items/"+args[0], nil, &d))
	it := d.Item
	fmt.Printf("#%d %s  [%s]  %s\n", it.ID, it.Title, it.Kind, it.State)
	kv := func(k, v string) {
		if v != "" {
			fmt.Printf("  %-11s %s\n", k, v)
		}
	}
	kv("host", it.Host)
	if it.Source != "" {
		kv("observed", it.Source+" "+it.Key)
	} else {
		kv("attempt", fmt.Sprintf("%d (interrupts %d)", it.Attempt, it.Retries))
		kv("priority", fmt.Sprint(it.Priority))
	}
	if n := it.Spec.NeedsAt(it.Step); n != "cpu" || len(it.Spec.SharesAt(it.Step)) > 0 {
		s := n
		if sh := it.Spec.SharesAt(it.Step); len(sh) > 0 {
			s += ", shares " + strings.Join(sh, " ")
		}
		kv("needs", s)
	}
	switch {
	case it.State == proto.SYielded:
		y := "to " + it.YieldTo
		if it.YieldedAt > 0 {
			y += " since " + clockMS(it.YieldedAt)
		}
		for _, l := range d.Leases {
			if l.Res == it.Spec.NeedsAt(it.Step) && strings.HasPrefix(it.YieldTo, l.Holder) {
				if l.Expires > 0 {
					y += fmt.Sprintf("; lease expires %s unless refreshed", clockMS(l.Expires))
				} else {
					y += fmt.Sprintf("; lease lives with pid %d", l.PID)
				}
			}
		}
		if strings.HasPrefix(it.YieldTo, "#") {
			y += "; resumes when it leaves the slot"
		} else {
			y += "; resumes ~30 s after the lease goes"
		}
		kv("yielded", y)
	case it.State == proto.SPaused && it.PausedBy == "shutdown":
		kv("paused by", "the shutdown hook; resumes by itself when "+it.Host+" is back")
	case it.State == proto.SPaused:
		kv("paused by", "you  (gaffer resume "+fmt.Sprint(it.ID)+")")
	case it.State == proto.SStopping && it.StopMode != "":
		kv("stopping", it.StopMode)
	}
	steps := []string{}
	for i, s := range it.Spec.Steps {
		mark := s.Name
		if i == it.Step && it.State != proto.SDone {
			mark = "[" + s.Name + "]"
		}
		steps = append(steps, mark)
	}
	kv("steps", strings.Join(steps, " → "))
	kv("progress", progText(it, 40))
	if it.ETA > 0 {
		kv("eta", fmtDur(it.ETA)+" ("+clock(time.Now().Add(time.Duration(it.ETA)*time.Second))+")")
	}
	if it.Rate > 0 {
		kv("rate", fmt.Sprintf("%.3g %s/s", it.Rate, unit(it)))
	}
	if it.Checkpoint != nil {
		kv("checkpoint", fmt.Sprintf("step %d %v @ %g", it.Checkpoint.Step, it.Checkpoint.Data, it.Checkpoint.Done))
	}
	kv("needs you", it.Attention)
	if len(it.Actions) > 0 {
		kv("actions", strings.Join(it.Actions, ", ")+fmt.Sprintf("   (gaffer %s %d)", it.Actions[0], it.ID))
	}
	if it.Parent != 0 {
		kv("acts on", fmt.Sprintf("#%d", it.Parent))
	}
	kv("reason", it.Reason)
	kv("created", clockMS(it.Created))
	if it.Started > 0 {
		kv("started", clockMS(it.Started))
	}
	if it.Finished > 0 {
		kv("finished", clockMS(it.Finished))
	}
	if it.RunMS > 0 {
		ran := "ran " + fmtDur(float64(it.RunMS)/1000)
		if it.Attempt > 1 {
			ran += fmt.Sprintf(" over %d attempts", it.Attempt)
		}
		kv("time", ran)
	}
	kv("log", fmt.Sprintf("%d bytes  (gaffer log %d)", it.LogSize, it.ID))
	fmt.Println("  events:")
	// a run of the same event (500 `assigned` while a fits said no every beat) folds into one line
	attempt := regexp.MustCompile(`attempt \d+`)
	for i := 0; i < len(d.Events); {
		e := d.Events[i]
		j := i + 1
		for j < len(d.Events) && d.Events[j].Kind == e.Kind && attempt.ReplaceAllString(d.Events[j].Msg, "attempt N") == attempt.ReplaceAllString(e.Msg, "attempt N") {
			j++
		}
		if j-i >= 3 {
			fmt.Printf("    %s  %-11s %s  ×%d, until %s\n", clockMS(e.TS), e.Kind, e.Msg, j-i, clockMS(d.Events[j-1].TS))
		} else {
			for k := i; k < j; k++ {
				fmt.Printf("    %s  %-11s %s\n", clockMS(d.Events[k].TS), d.Events[k].Kind, d.Events[k].Msg)
			}
		}
		i = j
	}
}

func logCmd(c *client, args []string) {
	fs := flag.NewFlagSet("log", flag.ExitOnError)
	follow := fs.Bool("f", false, "follow, until the item settles")
	n := fs.Int("n", 40, "lines of history")
	fs.Parse(args)
	if fs.NArg() != 1 {
		die(fmt.Errorf("log [-f] ID"))
	}
	id := fs.Arg(0)
	b, size, err := c.logFrom(id, 0)
	die(err)
	lines := strings.Split(string(b), "\n")
	if len(lines) > *n+1 {
		lines = lines[len(lines)-*n-1:]
	}
	os.Stdout.WriteString(strings.Join(lines, "\n"))
	off := size
	for *follow {
		time.Sleep(time.Second)
		done := settled(itemNow(c, id)) // read before the log: a settled item's log is complete
		b, size, err := c.logFrom(id, off)
		if err != nil { // controller restarting: keep following
			continue
		}
		os.Stdout.Write(b)
		off = max(off, size)
		if done {
			return
		}
	}
}

// logFrom is the item's console log from byte off, and the size the controller holds.
func (c *client) logFrom(id string, off int64) ([]byte, int64, error) {
	req, _ := http.NewRequest("GET", strings.TrimRight(c.cfg.Controller, "/")+"/api/items/"+id+"/log?offset="+strconv.FormatInt(off, 10), nil)
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	res, err := c.hc.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	size, _ := strconv.ParseInt(res.Header.Get("X-Log-Size"), 10, 64)
	return b, size, nil
}

func watch(c *client) {
	req, _ := http.NewRequest("GET", strings.TrimRight(c.cfg.Controller, "/")+"/api/stream", nil)
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	for {
		res, err := (&http.Client{}).Do(req)
		if err != nil {
			fmt.Printf("\033[H\033[2Jcontroller unreachable: %v (retrying)\n", err)
			time.Sleep(3 * time.Second)
			continue
		}
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			if !strings.HasPrefix(sc.Text(), "event:") {
				continue
			}
			var st proto.Status
			if err := c.do("GET", "/api/status", nil, &st); err != nil {
				continue
			}
			var b bytes.Buffer
			render(&b, st, termStyle(os.Stdout, false))
			fmt.Print("\033[H\033[2J" + time.Now().Format("15:04:05") + "  gaffer watch (^C to quit)\n\n" + b.String())
		}
		res.Body.Close()
		time.Sleep(time.Second)
	}
}

// ---------------------------------------------------------------- rendering

func unit(it proto.Item) string {
	if it.Progress != nil && it.Progress.Unit != "" {
		return it.Progress.Unit
	}
	return "units"
}

func progText(it proto.Item, width int) string {
	p := it.Progress
	if p == nil {
		if it.State == proto.SRunning && !it.Observed {
			return "(no progress reported yet)"
		}
		return ""
	}
	var parts []string
	if p.Total > 0 {
		f := min(1, p.Done/p.Total)
		parts = append(parts, bar(f, width), fmt.Sprintf("%3.0f%%", f*100))
		if p.Unit != "%" {
			parts = append(parts, fmt.Sprintf("%g/%g %s", p.Done, p.Total, unit(it)))
		}
	} else if p.Done > 0 {
		parts = append(parts, fmt.Sprintf("%g %s", p.Done, unit(it)))
	}
	if p.Phase != "" {
		parts = append(parts, "["+p.Phase+"]")
	}
	if p.Msg != "" {
		parts = append(parts, p.Msg)
	}
	return strings.Join(parts, " ")
}

func bar(f float64, w int) string {
	blocks := []string{"", "▏", "▎", "▍", "▌", "▋", "▊", "▉"}
	full := int(f * float64(w))
	rem := int((f*float64(w) - float64(full)) * 8)
	s := strings.Repeat("█", full)
	if full < w {
		if rem > 0 {
			return s + blocks[rem] + strings.Repeat("·", w-full-1)
		}
		s += strings.Repeat("·", w-full)
	}
	return s
}

func cut(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func fmtDur(sec float64) string {
	d := time.Duration(sec) * time.Second
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
}

func clock(t time.Time) string {
	if y, m, d := t.Date(); y == time.Now().Year() && m == time.Now().Month() && d == time.Now().Day() {
		return t.Format("15:04")
	}
	return t.Format("Mon 15:04")
}

func clockMS(ms int64) string { return clock(time.UnixMilli(ms)) }

// hostCmd: hold (no new work there), release, drain (hold + pause what runs there), through the controller.
func hostCmd(c *client, cmd, host string) {
	if cmd == "release" {
		die(c.do("POST", "/api/hosts/"+host+"/release", "", nil))
		fmt.Printf("%s released\n", host)
		return
	}
	die(c.do("POST", "/api/hosts/"+host+"/hold", "", nil))
	fmt.Printf("%s held\n", host)
	if cmd == "hold" {
		return
	}
	var st proto.Status
	die(c.do("GET", "/api/status", nil, &st))
	for _, it := range st.Items {
		if it.Host != host || it.Source != "" || it.Parent != 0 {
			continue // actions run to the end; observed items aren't ours to stop
		}
		switch it.State {
		case proto.SAssigned, proto.SRunning, proto.SStopping:
			var r map[string]string
			if err := c.do("POST", fmt.Sprintf("/api/items/%d/pause", it.ID), "", &r); err != nil {
				fmt.Fprintf(os.Stderr, "gaffer: #%d: %v\n", it.ID, err)
				continue
			}
			fmt.Printf("#%d -> %s\n", it.ID, r["ok"])
		}
	}
}

// shutdown is the OS shutdown hook, and works with the network already gone: it asks this box's agent
// (a file in its dir) to pause every running job, then blocks until none is running, so systemd waits.
// The agent records the pauses on disk and reports them when it next reaches the controller, after the
// reboot if need be; they come back by themselves once the host does. Nothing to do if no agent is up.
func shutdown(cfg *config.Config, args []string) {
	fs := flag.NewFlagSet("shutdown", flag.ExitOnError)
	wait := fs.Int("wait", 60, "seconds to wait for the jobs to stop")
	fs.Parse(args)
	dir := cfg.Agent.Dir
	if dir == "" {
		die(fmt.Errorf("no [agent] dir in the config: this box runs no jobs"))
	}
	if f, err := os.OpenFile(filepath.Join(dir, "agent.lock"), os.O_RDWR, 0); err == nil {
		if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil { // we got it: no agent holds it
			fmt.Println("no agent running here; nothing to pause")
			return
		}
	} else {
		fmt.Println("no agent dir here; nothing to pause")
		return
	}
	running := func() (n int, ids []string) {
		dirs, _ := filepath.Glob(filepath.Join(dir, "jobs", "*", "job.json"))
		for _, p := range dirs {
			var j struct {
				ID    int64  `json:"id"`
				State string `json:"state"`
			}
			if b, err := os.ReadFile(p); err == nil && json.Unmarshal(b, &j) == nil && j.State == "running" {
				n++
				ids = append(ids, "#"+strconv.FormatInt(j.ID, 10))
			}
		}
		return
	}
	n, ids := running()
	if n == 0 {
		fmt.Println("nothing running here; safe to go down")
		return
	}
	pauseAll := filepath.Join(dir, "pause-all")
	die(os.WriteFile(pauseAll, []byte(time.Now().Format(time.RFC3339)+"\n"), 0o644))
	fmt.Printf("pausing %s\n", strings.Join(ids, " "))
	deadline := time.Now().Add(time.Duration(*wait) * time.Second)
	for {
		time.Sleep(time.Second)
		// the file stays: the host reports itself held while it is fresh (two minutes), and the agent removes
		// it when it next starts (after the reboot). An aborted shutdown lifts the hold by itself.
		if left, ids := running(); left == 0 {
			fmt.Printf("%d paused, safe to go down; they resume by themselves when this box is back\n", n)
			return
		} else if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "gaffer: %s still stopping after %ds; going down anyway (interrupted, they follow their kind's policy)\n", strings.Join(ids, " "), *wait)
			os.Exit(1)
		}
	}
}
