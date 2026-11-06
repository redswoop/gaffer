package cli

import "fmt"

// Quantities with a byte unit (unit=B or bytes, what a download or copy reports) print as 335 MiB, not 3.35132398e+08.
// Any other unit keeps the plain %g it always had.

func isBytes(unit string) bool { return unit == "B" || unit == "bytes" }

func humanBytes(v float64) string {
	u := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	i := 0
	for v >= 1024 && i < len(u)-1 {
		v /= 1024
		i++
	}
	switch {
	case i == 0:
		return fmt.Sprintf("%.0f B", v)
	case v >= 100:
		return fmt.Sprintf("%.0f %s", v, u[i])
	case v >= 10:
		return fmt.Sprintf("%.1f %s", v, u[i])
	}
	return fmt.Sprintf("%.2f %s", v, u[i])
}

// count is "N unit" for a total-less count.
func count(v float64, unit string) string {
	if isBytes(unit) {
		return humanBytes(v)
	}
	return fmt.Sprintf("%g %s", v, unit)
}

// ofTotal is "done/total unit" for counts and progress lines.
func ofTotal(done, total float64, unit string) string {
	if isBytes(unit) {
		return humanBytes(done) + " / " + humanBytes(total)
	}
	return fmt.Sprintf("%g/%g %s", done, total, unit)
}

// rate is "N unit/s".
func rate(r float64, unit string) string {
	if isBytes(unit) {
		return humanBytes(r) + "/s"
	}
	return fmt.Sprintf("%.3g %s/s", r, unit)
}
