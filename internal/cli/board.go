package cli

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
	"unsafe"

	"gaffer/internal/proto"
)

// The board: one line per item, grouped by what it wants from you, sized to the terminal.

type style struct {
	color bool
	width int
	all   bool // history too
}

func (s style) c(code, txt string) string {
	if !s.color || txt == "" {
		return txt
	}
	return "\033[" + code + "m" + txt + "\033[0m"
}
func (s style) bold(t string) string   { return s.c("1", t) }
func (s style) dim(t string) string    { return s.c("2", t) }
func (s style) red(t string) string    { return s.c("31", t) }
func (s style) yellow(t string) string { return s.c("33", t) }
func (s style) green(t string) string  { return s.c("32", t) }

func termStyle(f *os.File, all bool) style {
	s := style{width: 120, all: all}
	var ws struct{ Row, Col, X, Y uint16 }
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws))); e == 0 && ws.Col > 0 {
		s.width, s.color = int(ws.Col), os.Getenv("NO_COLOR") == ""
	} else if n, err := fmt.Sscan(os.Getenv("COLUMNS"), &s.width); n != 1 || err != nil {
		s.width = 120
	}
	return s
}

func runes(s string) int { return utf8.RuneCountInString(s) }

// pad cuts or pads to exactly n runes.
func pad(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if runes(s) > n {
		return cut(s, n)
	}
	return s + strings.Repeat(" ", n-runes(s))
}

// padMid is pad for titles: it cuts the middle, because the end ("Scene 4", "e8") is often what tells
// two items apart.
func padMid(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return pad(s, n)
	}
	head := (n - 1) * 6 / 10
	return string(r[:head]) + "…" + string(r[len(r)-(n-1-head):])
}

type section struct {
	name  string
	paint func(string) string
	items []proto.Item
}

func render(w io.Writer, st proto.Status, s style) {
	header(w, st, s)
	secs := []*section{
		{name: "NEEDS YOU", paint: s.yellow}, {name: "FAILED", paint: s.red}, {name: "RUNNING", paint: s.green},
		{name: "QUEUED", paint: s.bold}, {name: "PAUSED / HELD", paint: s.bold}, {name: "RECENT", paint: s.dim},
	}
	var history []proto.Item
	for _, it := range st.Items {
		switch {
		case it.Attention != "" && (active(it.State) || it.State == proto.SNeedsYou), it.State == proto.SNeedsYou:
			secs[0].items = append(secs[0].items, it)
		case it.State == proto.SFailed && it.Acked == 0:
			secs[1].items = append(secs[1].items, it)
		case active(it.State):
			secs[2].items = append(secs[2].items, it)
		case it.State == proto.SQueued:
			secs[3].items = append(secs[3].items, it)
		case it.State == proto.SPaused || it.State == proto.SHeld:
			secs[4].items = append(secs[4].items, it)
		default:
			history = append(history, it)
		}
	}
	sort.SliceStable(history, func(a, b int) bool { return history[a].Finished > history[b].Finished })
	if s.all {
		secs[5].items = history
	}
	// one set of column widths for the whole board, so sections line up
	idW, kindW, hostW, titleW := 2, 0, 0, 0
	for _, sec := range secs {
		for _, it := range sec.items {
			idW, kindW = max(idW, len(fmt.Sprint(it.ID))), max(kindW, runes(it.Kind))
			hostW, titleW = max(hostW, runes(it.Host)), max(titleW, runes(it.Title))
		}
	}
	kindW, hostW = min(kindW, 10), min(hostW, 10)
	if s.width < 110 {
		kindW = 0 // narrow: the kind is the first thing to go
	}
	cols := progCols{}
	for _, it := range secs[2].items {
		c, e := counts(it), eta(it, s)
		cols.count, cols.eta = max(cols.count, runes(c)), max(cols.eta, visible(e))
	}
	fixed := 2 + idW + 2 + kindW + 2 + hostW + 2
	if kindW == 0 {
		fixed -= 2
	}
	need := 30 // the detail gets room for its aligned progress columns first, the title takes the rest
	if len(secs[2].items) > 0 {
		need = max(need, 17+cols.count+2+cols.eta)
	}
	titleW = max(16, min(titleW, 48, s.width-fixed-need))
	detailW := max(20, s.width-fixed-titleW-2)
	for _, sec := range secs {
		if len(sec.items) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n%s %s\n", sec.paint(sec.name), s.dim(fmt.Sprint(len(sec.items))))
		for _, it := range sec.items {
			kind := s.dim(pad(it.Kind, kindW)) + "  "
			if kindW == 0 {
				kind = ""
			}
			line := fmt.Sprintf("  %*d  %s%s  %s  ", idW, it.ID, kind, padMid(it.Title, titleW), s.dim(pad(it.Host, hostW)))
			fmt.Fprintln(w, line+detail(it, detailW, cols, s))
		}
	}
	if !s.all && len(history) > 0 {
		n := map[string]int{}
		for _, it := range history {
			n[it.State]++
		}
		parts := []string{}
		for _, k := range []string{proto.SDone, proto.SFailed, proto.SCanceled} {
			if n[k] > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", n[k], k))
			}
		}
		fmt.Fprintf(w, "\n%s\n", s.dim("last 24h: "+strings.Join(parts, ", ")+"  (gaffer ls -a)"))
	}
	if len(st.Items) == 0 {
		fmt.Fprintln(w, "\n(nothing)")
	}
}

func active(state string) bool {
	return state == proto.SAssigned || state == proto.SRunning || state == proto.SStopping
}

// header: hosts and observers on one line while they're fine; anything wrong gets its own line.
func header(w io.Writer, st proto.Status, s style) {
	var ok, bad []string
	for _, h := range st.Hosts {
		labels := make([]string, 0, len(h.Slots))
		for l := range h.Slots {
			labels = append(labels, l)
		}
		sort.Strings(labels)
		slots := []string{}
		for _, l := range labels {
			slots = append(slots, fmt.Sprintf("%s %d/%d", l, h.Used[l], h.Slots[l]))
		}
		desc := h.Host + " " + s.dim(strings.Join(slots, " "))
		switch {
		case !h.Online:
			bad = append(bad, s.red(fmt.Sprintf("%s OFFLINE %s", h.Host, fmtDur(float64(st.Now-h.Seen)/1000))))
		case h.Drained:
			bad = append(bad, s.yellow(h.Host+" DRAINED: "+h.Note)+" "+s.dim("(gaffer undrain "+h.Host+")"))
		default:
			ok = append(ok, desc)
		}
	}
	var srcs []string
	for _, src := range st.Sources {
		switch {
		case src.Silent:
			bad = append(bad, s.yellow(fmt.Sprintf("observer %s SILENT %s", src.Name, fmtDur(float64(st.Now-src.Seen)/1000)))+
				s.dim(fmt.Sprintf(" (every %ds on %s)", src.Every, src.Host)))
		case !src.OK:
			bad = append(bad, s.red("observer "+src.Name+" FAILING")+s.dim(" on "+src.Host+": ")+cut(src.Error, 80))
		default:
			srcs = append(srcs, src.Name)
		}
	}
	line := strings.Join(ok, s.dim("  ·  "))
	if len(srcs) > 0 {
		line += s.dim("    observing ") + strings.Join(srcs, " ")
	}
	fmt.Fprintln(w, line)
	for _, b := range bad {
		fmt.Fprintln(w, "  "+b)
	}
}

// detail is the one thing worth reading about an item, by state.
func detail(it proto.Item, w int, cols progCols, s style) string {
	switch {
	case it.Attention != "" || it.State == proto.SNeedsYou:
		a := it.Attention
		if a == "" {
			a = "review"
		}
		hint := ""
		if len(it.Actions) > 0 {
			hint = fmt.Sprintf("  → gaffer %s %d", strings.Join(it.Actions, "|"), it.ID)
		}
		if runes(a)+runes(hint) > w {
			a = cut(a, max(10, w-runes(hint)))
		}
		return s.yellow(a) + s.dim(hint)
	case it.State == proto.SFailed && it.Acked == 0:
		hint := fmt.Sprintf("  → gaffer retry|ack %d", it.ID)
		if it.Source != "" {
			hint = fmt.Sprintf("  → gaffer ack %d", it.ID) // the source owns retries
		}
		return s.red(cut(it.Reason, max(10, w-runes(hint)))) + s.dim(hint)
	case active(it.State):
		return progressDetail(it, w, cols, s)
	case it.State == proto.SQueued, it.State == proto.SPaused, it.State == proto.SHeld:
		r := it.Reason
		if r == "" && it.Progress != nil && it.Progress.Total > 0 {
			r = fmt.Sprintf("at %.0f%%", 100*it.Progress.Done/it.Progress.Total)
		}
		return s.dim(cut(r, w))
	}
	when := ""
	if it.Finished > 0 {
		when = clockMS(it.Finished)
	}
	r := it.Reason
	if (it.State == proto.SDone && it.Source == "") || r == it.State {
		r = ""
	}
	return s.dim(cut(strings.TrimSpace(fmt.Sprintf("%-9s %s  %s", it.State, when, r)), w))
}

// progCols are the widths shared by every running row, so counts and ETAs line up.
type progCols struct{ count, eta int }

func counts(it proto.Item) string {
	p := it.Progress
	switch {
	case p == nil || p.Unit == "%":
		return ""
	case p.Total > 0:
		return fmt.Sprintf("%g/%g %s", p.Done, p.Total, unit(it))
	case p.Done > 0:
		return fmt.Sprintf("%g %s", p.Done, unit(it))
	}
	return ""
}

func eta(it proto.Item, s style) string {
	if it.ETA <= 0 {
		return ""
	}
	return fmtDur(it.ETA) + s.dim(" → "+clock(time.Now().Add(time.Duration(it.ETA)*time.Second)))
}

func progressDetail(it proto.Item, w int, cols progCols, s style) string {
	out := ""
	if it.Stalled {
		out += s.red("STALLED") + "  "
	}
	if len(it.Spec.Steps) > 1 {
		out += s.dim(fmt.Sprintf("%d/%d %s", it.Step+1, len(it.Spec.Steps), it.StepName)) + "  "
	}
	p := it.Progress
	if p != nil && p.Total > 0 {
		f := min(1, p.Done/p.Total)
		out += bar(f, 10) + fmt.Sprintf(" %3.0f%%  ", f*100)
	} else if counts(it) == "" && it.ETA <= 0 {
		cols = progCols{} // nothing to line up: just the message
	} else if cols.count > 0 || cols.eta > 0 {
		out += strings.Repeat(" ", 17)
	}
	if cols.count > 0 {
		c := counts(it)
		out += strings.Repeat(" ", cols.count-runes(c)) + c + "  "
	}
	if cols.eta > 0 {
		e := eta(it, s)
		out += e + strings.Repeat(" ", cols.eta-visible(e)) + "  "
	}
	if p != nil {
		msg := strings.TrimSpace(p.Phase + " " + p.Msg)
		if p.Phase != "" && p.Msg != "" {
			msg = p.Phase + ": " + p.Msg
		}
		if room := w - visible(out); msg != "" && room >= 8 {
			out += s.dim(cut(msg, room))
		}
	}
	if strings.TrimSpace(out) == "" && !it.Observed {
		return s.dim("(no progress reported yet)")
	}
	return strings.TrimRight(out, " ")
}

// visible is the rune count without ANSI escapes.
func visible(x string) int {
	n, esc := 0, false
	for _, r := range x {
		switch {
		case r == '\033':
			esc = true
		case esc && r == 'm':
			esc = false
		case !esc:
			n++
		}
	}
	return n
}
