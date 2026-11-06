package cli

import "testing"

func TestQty(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{humanBytes(0), "0 B"},
		{humanBytes(335132398), "320 MiB"},
		{humanBytes(1503300328), "1.40 GiB"},
		{humanBytes(22533850592), "21.0 GiB"},
		{count(3.5, "frames"), "3.5 frames"},
		{count(2048, "B"), "2.00 KiB"},
		{ofTotal(12, 40, "frames"), "12/40 frames"},
		{ofTotal(548105171, 548105171, "bytes"), "523 MiB / 523 MiB"},
		{rate(4.79e7, "bytes"), "45.7 MiB/s"},
		{rate(2.5, "frames"), "2.5 frames/s"},
	} {
		if c.got != c.want {
			t.Errorf("got %q want %q", c.got, c.want)
		}
	}
}
