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
	"sort"
	"strconv"
	"strings"
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
		render(os.Stdout, st)
	case "watch":
		watch(c)
	case "show":
		show(c, args)
	case "log":
		logCmd(c, args)
	case "pause", "cancel", "resume", "release", "ack", "top", "retry":
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
		for _, id := range fs.Args() {
			var r map[string]string
			if err := c.do("POST", "/api/items/"+id+"/"+cmd, body, &r); err != nil {
				fmt.Fprintln(os.Stderr, "gaffer:", err)
				continue
			}
			fmt.Printf("#%s -> %s\n", id, r["ok"])
		}
	case "drain", "undrain":
		if len(args) != 1 {
			die(fmt.Errorf("%s HOST", cmd))
		}
		die(c.do("POST", "/api/hosts/"+args[0]+"/"+cmd, "", nil))
		fmt.Printf("%s %sed\n", args[0], cmd)
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
	default:
		usage()
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `gaffer: run work, watch progress, survive reboots

  gaffer submit KIND [flags] [--] ARGS...    queue a job of a kind (kinds/<KIND>/kind.toml)
  gaffer run [flags] -- CMD...               queue an ad-hoc shell command
  gaffer ls [-a] [--json]                    board: needs-you first, then running, queued, history
  gaffer watch                               the board, live
  gaffer show ID / log ID [-f]               detail + events / console log
  gaffer pause|resume|cancel|retry [--resume]|release|ack|top ID...
  gaffer drain|undrain HOST
  gaffer track TITLE...                      observe work gaffer doesn't run; prints an id
  gaffer emit [ID] LINE...                   ::progress / ::phase / ::checkpoint / ::needs-you / ::done / ::fail
                                             (inside a gaffer job: no ID, goes to the job's own log)
  gaffer controller | agent                  the daemons
`)
	os.Exit(2)
}

func submit(c *client, cmd string, args []string) {
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	title := fs.String("title", "", "")
	host := fs.String("host", "", "pin to an agent")
	needs := fs.String("needs", "", "slot label (gpu, cpu…)")
	prio := fs.Int("prio", 100, "lower runs first")
	held := fs.String("held", "", "queue held, with this reason")
	onInt := fs.String("on-interrupt", "", "resume | restart | hold")
	rx := fs.String("progress-regex", "", "run only: regex with done/total groups")
	stall := fs.Int("stall", 0, "seconds without progress before stalled")
	review := fs.Bool("review", false, "finish in needs-you instead of done")
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
	}
	if *needs != "" {
		spec.Needs = *needs
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
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if spec.Env == nil {
			spec.Env = map[string]string{}
		}
		spec.Env[k] = v
	}
	var r map[string]int64
	die(c.do("POST", "/api/items", map[string]any{"spec": spec, "priority": *prio, "held": *held}, &r))
	fmt.Printf("#%d queued  %s\n", r["id"], spec.Title)
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
	kv("attempt", fmt.Sprintf("%d (interrupts %d)", it.Attempt, it.Retries))
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
	kv("reason", it.Reason)
	kv("created", clockMS(it.Created))
	if it.Started > 0 {
		kv("started", clockMS(it.Started))
	}
	if it.Finished > 0 {
		kv("finished", clockMS(it.Finished)+"  ("+fmtDur(float64(it.Finished-it.Started)/1000)+")")
	}
	kv("log", fmt.Sprintf("%d bytes  (gaffer log %d)", it.LogSize, it.ID))
	fmt.Println("  events:")
	for _, e := range d.Events {
		fmt.Printf("    %s  %-11s %s\n", clockMS(e.TS), e.Kind, e.Msg)
	}
}

func logCmd(c *client, args []string) {
	fs := flag.NewFlagSet("log", flag.ExitOnError)
	follow := fs.Bool("f", false, "follow")
	n := fs.Int("n", 40, "lines of history")
	fs.Parse(args)
	if fs.NArg() != 1 {
		die(fmt.Errorf("log [-f] ID"))
	}
	id := fs.Arg(0)
	get := func(off int64) ([]byte, int64) {
		req, _ := http.NewRequest("GET", strings.TrimRight(c.cfg.Controller, "/")+"/api/items/"+id+"/log?offset="+strconv.FormatInt(off, 10), nil)
		req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
		res, err := c.hc.Do(req)
		die(err)
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		size, _ := strconv.ParseInt(res.Header.Get("X-Log-Size"), 10, 64)
		return b, size
	}
	b, size := get(0)
	lines := strings.Split(string(b), "\n")
	if len(lines) > *n+1 {
		lines = lines[len(lines)-*n-1:]
	}
	os.Stdout.WriteString(strings.Join(lines, "\n"))
	off := size
	for *follow {
		time.Sleep(time.Second)
		b, size := get(off)
		os.Stdout.Write(b)
		off = max(off, size)
	}
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
			render(&b, st)
			fmt.Print("\033[H\033[2J" + time.Now().Format("15:04:05") + "  gaffer watch (^C to quit)\n\n" + b.String())
		}
		res.Body.Close()
		time.Sleep(time.Second)
	}
}

// ---------------------------------------------------------------- rendering

func render(w io.Writer, st proto.Status) {
	for _, h := range st.Hosts {
		state := "online"
		if !h.Online {
			state = "OFFLINE " + fmtDur(float64(st.Now-h.Seen)/1000) + " ago"
		}
		if h.Drained {
			state += "  DRAINED " + h.Note
		}
		labels := make([]string, 0, len(h.Slots))
		for l := range h.Slots {
			labels = append(labels, l)
		}
		sort.Strings(labels)
		slots := []string{}
		for _, l := range labels {
			slots = append(slots, fmt.Sprintf("%s %d/%d", l, h.Used[l], h.Slots[l]))
		}
		fmt.Fprintf(w, "%-10s %-8s %s\n", h.Host, state, strings.Join(slots, "  "))
	}
	if len(st.Items) == 0 {
		fmt.Fprintln(w, "\n(nothing)")
		return
	}
	fmt.Fprintf(w, "\n%4s  %-10s %-9s %-8s %-8s %-44s %s\n", "ID", "STATE", "HOST", "KIND", "STEP", "PROGRESS", "ETA / DONE")
	for _, it := range st.Items {
		state := it.State
		if it.Stalled {
			state = "STALLED"
		}
		when := ""
		switch {
		case it.ETA > 0:
			when = fmtDur(it.ETA) + " (" + clock(time.Now().Add(time.Duration(it.ETA)*time.Second)) + ")"
		case it.Finished > 0:
			when = clockMS(it.Finished)
		}
		step := it.StepName
		if len(it.Spec.Steps) > 1 && (it.State == proto.SRunning || it.State == proto.SStopping) {
			step = fmt.Sprintf("%d/%d %s", it.Step+1, len(it.Spec.Steps), it.StepName)
		}
		fmt.Fprintf(w, "%4d  %-10s %-9s %-8s %-8s %-44s %s\n", it.ID, state, cut(it.Host, 9), cut(it.Kind, 8), cut(step, 8),
			cut(progText(it, 10), 44), when)
		fmt.Fprintf(w, "%16s%s\n", "", cut(it.Title, 100))
		if it.Attention != "" {
			fmt.Fprintf(w, "%16s→ needs you: %s\n", "", cut(it.Attention, 100))
		}
		if it.Reason != "" && it.State != proto.SDone {
			fmt.Fprintf(w, "%16s%s\n", "", cut(it.Reason, 110))
		}
	}
}

func unit(it proto.Item) string {
	if it.Progress != nil && it.Progress.Unit != "" {
		return it.Progress.Unit
	}
	return "units"
}

func progText(it proto.Item, width int) string {
	p := it.Progress
	if p == nil {
		if it.State == proto.SRunning {
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
		s += blocks[rem] + strings.Repeat(" ", w-full-1)
		if rem == 0 {
			s = strings.Repeat("█", full) + strings.Repeat("·", w-full)
		}
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
