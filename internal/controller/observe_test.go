package controller

import "testing"

func TestParseSnapshot(t *testing.T) {
	out := `::progress 1/2
::item 42 title="scene \"3\"" host=boxb state=running stall=1800
::progress done=88 total=92 unit=chunks
some chatter
::item 43 state=needs-you
::needs-you review: watch it
::item
::msg belongs to nobody
`
	items := parseSnapshot(out, 1000)
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}
	a, b := items[0], items[1]
	if a.key != "42" || a.attrs["title"] != `scene "3"` || a.attrs["host"] != "boxb" || a.attrs["stall"] != "1800" {
		t.Errorf("item 42: %q %v", a.key, a.attrs)
	}
	if p := a.ps.Progress; p == nil || p.Done != 88 || p.Total != 92 || p.Unit != "chunks" {
		t.Errorf("item 42 progress: %+v", p)
	}
	if b.key != "43" || b.ps.Attention != "review: watch it" || b.ps.Progress != nil {
		t.Errorf("item 43: %q %q %+v", b.key, b.ps.Attention, b.ps.Progress)
	}
}
