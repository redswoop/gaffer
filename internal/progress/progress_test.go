package progress

import (
	"regexp"
	"testing"
)

func TestProtocol(t *testing.T) {
	var s State
	s.Line(`::progress done=12 total=60 unit=frames msg="chunk 4/109"`, nil, 1)
	p := s.Progress
	if p.Done != 12 || p.Total != 60 || p.Unit != "frames" || p.Msg != "chunk 4/109" || p.TS != 1 {
		t.Fatalf("kv: %+v", p)
	}
	s.Line("  ::progress 42/109", nil, 2)
	if p.Done != 42 || p.Total != 109 || p.Msg != "chunk 4/109" {
		t.Fatalf("positional keeps msg: %+v", p)
	}
	s.Line("::progress 45%", nil, 3)
	if p.Done != 45 || p.Total != 100 {
		t.Fatalf("pct: %+v", p)
	}
	s.Line("::phase vae-decode", nil, 4)
	if p.Phase != "vae-decode" {
		t.Fatalf("phase: %+v", p)
	}
	s.Line("::checkpoint seg=7 path=\"a b\"", nil, 5)
	if c := s.Checkpoint; c.Data["seg"] != "7" || c.Data["path"] != "a b" || c.Done != 45 {
		t.Fatalf("checkpoint: %+v", c)
	}
	s.Line("::needs-you pick one: e3 / e5", nil, 6)
	if s.Attention != "pick one: e3 / e5" {
		t.Fatalf("needs-you: %q", s.Attention)
	}
	if s.Line("just a log line", nil, 7) {
		t.Fatal("plain line changed state")
	}
}

func TestRegex(t *testing.T) {
	var s State
	named := regexp.MustCompile(`\[chunk \d+/\d+\] frames (?P<done>\d+)/(?P<total>\d+).*?ETA (?P<eta>[\d.]+)`)
	s.Line("[chunk 3/109] frames 888/33379  2.1 fps  ETA 412.5 min", named, 1)
	if p := s.Progress; p.Done != 888 || p.Total != 33379 || p.ETA != 412.5 {
		t.Fatalf("named: %+v", p)
	}
	var s2 State
	s2.Line("frame 7 of 60", regexp.MustCompile(`frame (\d+) of (\d+)`), 1)
	if p := s2.Progress; p.Done != 7 || p.Total != 60 {
		t.Fatalf("unnamed: %+v", p)
	}
}
