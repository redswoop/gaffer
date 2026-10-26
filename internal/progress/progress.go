// Package progress folds every way a job can say how it's doing into one proto.Progress.
//
// The line protocol (stdout/stderr of any step, `gaffer emit`, or the output of a kind's poll command):
//
//	::progress done=12432 total=33379 unit=frames msg="chunk 42/109" [eta=1800] [phase=render]
//	::progress 42/109        ::progress 0.45        ::progress 45%
//	::phase vae-decode
//	::msg waiting for the NAS
//	::checkpoint seg=7 done=11840      (any k=v; done= is the progress value it resumes at)
//	::needs-you pick a checkpoint: e3 / e5 / e8
//
// A kind can also give a regex for logs that don't speak the protocol. Named groups done, total, pct,
// eta, msg, phase are used; with two unnamed groups the first is done and the second total.
package progress

import (
	"regexp"
	"strconv"
	"strings"

	"gaffer/internal/proto"
)

type State struct {
	Progress   *proto.Progress
	Checkpoint *proto.Checkpoint
	Attention  string
}

// Line folds one log line into s. It reports whether anything changed.
func (s *State) Line(line string, rx *regexp.Regexp, now int64) bool {
	t := strings.TrimSpace(line)
	if strings.HasPrefix(t, "::") {
		return s.command(t[2:], now)
	}
	if rx != nil {
		return s.regex(t, rx, now)
	}
	return false
}

func (s *State) prog(now int64) *proto.Progress {
	if s.Progress == nil {
		s.Progress = &proto.Progress{}
	}
	s.Progress.TS = now
	return s.Progress
}

func (s *State) command(c string, now int64) bool {
	verb, rest, _ := strings.Cut(c, " ")
	rest = strings.TrimSpace(rest)
	switch verb {
	case "progress":
		p := s.prog(now)
		for _, tok := range Tokens(rest) {
			k, v, kv := strings.Cut(tok, "=")
			if !kv {
				positional(p, tok)
				continue
			}
			setField(p, k, v)
		}
		return true
	case "phase":
		s.prog(now).Phase = rest
		return true
	case "msg":
		s.prog(now).Msg = rest
		return true
	case "checkpoint":
		cp := &proto.Checkpoint{Data: map[string]string{}, TS: now}
		for _, tok := range Tokens(rest) {
			k, v, _ := strings.Cut(tok, "=")
			if k == "done" {
				cp.Done, _ = strconv.ParseFloat(v, 64)
				continue
			}
			cp.Data[k] = v
		}
		if cp.Done == 0 && s.Progress != nil {
			cp.Done = s.Progress.Done
		}
		s.Checkpoint = cp
		return true
	case "needs-you":
		s.Attention = rest
		return true
	case "clear-needs-you":
		s.Attention = ""
		return true
	}
	return false
}

// positional handles `::progress 42/109`, `0.45` and `45%`.
func positional(p *proto.Progress, tok string) {
	if a, b, ok := strings.Cut(tok, "/"); ok {
		p.Done, _ = strconv.ParseFloat(a, 64)
		p.Total, _ = strconv.ParseFloat(b, 64)
		return
	}
	if pct, ok := strings.CutSuffix(tok, "%"); ok {
		p.Done, _ = strconv.ParseFloat(pct, 64)
		p.Total, p.Unit = 100, "%"
		return
	}
	if f, err := strconv.ParseFloat(tok, 64); err == nil {
		p.Done, p.Total = f, 1
	}
}

func setField(p *proto.Progress, k, v string) {
	f, _ := strconv.ParseFloat(v, 64)
	switch k {
	case "done":
		p.Done = f
	case "total":
		p.Total = f
	case "pct":
		p.Done, p.Total = f, 100
	case "eta":
		p.ETA = f
	case "eta_min":
		p.ETA = f * 60
	case "unit":
		p.Unit = v
	case "msg":
		p.Msg = v
	case "phase":
		p.Phase = v
	}
}

func (s *State) regex(line string, rx *regexp.Regexp, now int64) bool {
	m := rx.FindStringSubmatch(line)
	if m == nil {
		return false
	}
	p := s.prog(now)
	named := false
	for i, name := range rx.SubexpNames() {
		if name == "" || i >= len(m) || m[i] == "" {
			continue
		}
		named = true
		setField(p, name, m[i])
	}
	if !named && len(m) >= 3 {
		p.Done, _ = strconv.ParseFloat(m[1], 64)
		p.Total, _ = strconv.ParseFloat(m[2], 64)
	}
	return true
}

// Tokens splits on spaces, keeping "double quoted" runs together (quotes removed, \" escapes honored).
func Tokens(s string) []string {
	var out []string
	var b strings.Builder
	inq, have := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && inq && i+1 < len(s):
			i++
			b.WriteByte(s[i])
		case c == '"':
			inq, have = !inq, true
		case c == ' ' && !inq:
			if have {
				out = append(out, b.String())
				b.Reset()
				have = false
			}
		default:
			b.WriteByte(c)
			have = true
		}
	}
	if have {
		out = append(out, b.String())
	}
	return out
}
