package cli

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"gaffer/internal/proto"
)

// Exit codes of `gaffer wait`, worst wins: a script (or a Claude) branches on them without parsing.
const (
	waitDone     = 0
	waitFailed   = 1
	waitNeedsYou = 2
	waitCanceled = 3
	waitUsage    = 4
	waitTimeout  = 124 // like timeout(1)
)

// settled: the item won't change again without someone acting on it.
func settled(it proto.Item) bool {
	switch it.State {
	case proto.SDone, proto.SFailed, proto.SCanceled, proto.SNeedsYou:
		return true
	}
	return false
}

// trouble: something a watcher should hear about now, even with other items still moving.
func trouble(it proto.Item) bool {
	return it.State == proto.SFailed || it.State == proto.SNeedsYou || it.State == proto.SHeld ||
		it.Stalled || (it.Attention != "" && active(it.State))
}

func waitCode(it proto.Item) int {
	switch {
	case it.State == proto.SNeedsYou || it.State == proto.SHeld || it.Stalled || it.Attention != "" && active(it.State):
		return waitNeedsYou
	case it.State == proto.SFailed:
		return waitFailed
	case it.State == proto.SCanceled:
		return waitCanceled
	}
	return waitDone
}

// waitLine is one item in one line, the unit of --events output and of the closing summary.
func waitLine(it proto.Item) string {
	s := fmt.Sprintf("#%d %-9s", it.ID, it.State)
	var parts []string
	switch {
	case it.Attention != "" && active(it.State):
		parts = append(parts, "ASKS: "+it.Attention)
	case it.Stalled:
		parts = append(parts, "STALLED")
	}
	if it.StepName != "" && len(it.Spec.Steps) > 1 {
		parts = append(parts, fmt.Sprintf("step %d/%d %s", it.Step+1, len(it.Spec.Steps), it.StepName))
	}
	if p := it.Progress; p != nil && p.Total > 0 && it.State != proto.SDone {
		parts = append(parts, fmt.Sprintf("%.0f%% %g/%g %s", min(1, p.Done/p.Total)*100, p.Done, p.Total, unit(it)))
	}
	if it.ETA > 0 && active(it.State) {
		parts = append(parts, "eta "+fmtDur(it.ETA))
	}
	if it.Reason != "" && !active(it.State) {
		parts = append(parts, it.Reason)
	}
	parts = append(parts, "· "+it.Title)
	return s + " " + strings.Join(parts, "  ")
}

// waitKey is what has to change for --events to print a line: state, step, question, stall, reason,
// and progress in 10% steps (msg churn and every-chunk ticks would flood a reader).
func waitKey(it proto.Item) string {
	dec := -1
	if p := it.Progress; p != nil && p.Total > 0 {
		dec = int(min(1, p.Done/p.Total) * 10)
	}
	return fmt.Sprintf("%s|%d|%s|%v|%s|%d|%s", it.State, it.Step, it.Attention, it.Stalled, it.Reason, dec, it.Host)
}

// wait blocks until items settle, level-triggered: each round reads every item's current state, so
// a controller restart or a missed change costs a round, never a wrong answer.
func wait(c *client, args []string) {
	fs := flag.NewFlagSet("wait", flag.ExitOnError)
	events := fs.Bool("events", false, "print a line per change (state, step, question, stall, every 10%); exit when all settle")
	anyOne := fs.Bool("any", false, "return as soon as any one item settles")
	timeout := fs.Duration("timeout", 0, "give up after this long (exit 124)")
	tail := fs.Int("n", 15, "log lines to show for items that failed or need you")
	every := fs.Duration("every", 2*time.Second, "")
	fs.Parse(args)
	if fs.NArg() == 0 || !allIDs(fs.Args()) {
		fmt.Fprintln(os.Stderr, "gaffer: wait [--events] [--any] [--timeout DUR] ID...")
		os.Exit(waitUsage)
	}
	ids := fs.Args()
	var deadline time.Time
	if *timeout > 0 {
		deadline = time.Now().Add(*timeout)
	}
	last := map[string]string{}
	items := map[string]proto.Item{}
	down := false
	for first := true; ; first = false {
		ok := true
		for _, id := range ids {
			var d struct {
				Item proto.Item `json:"item"`
			}
			if err := c.do("GET", "/api/items/"+id, nil, &d); err != nil {
				if strings.Contains(err.Error(), "no such item") {
					fmt.Fprintf(os.Stderr, "gaffer: #%s: no such item\n", id)
					os.Exit(waitUsage)
				}
				if *events && !down {
					fmt.Printf("%s controller unreachable, retrying: %v\n", time.Now().Format("15:04:05"), err)
				}
				down, ok = true, false
				break
			}
			items[id] = d.Item
		}
		if ok {
			if down && *events {
				fmt.Printf("%s controller back\n", time.Now().Format("15:04:05"))
			}
			down = false
			nSettled, nTrouble := 0, 0
			for _, id := range ids {
				it := items[id]
				if k := waitKey(it); *events && k != last[id] {
					fmt.Printf("%s %s\n", time.Now().Format("15:04:05"), waitLine(it))
				}
				last[id] = waitKey(it)
				if settled(it) {
					nSettled++
				}
				if trouble(it) {
					nTrouble++
				}
			}
			switch {
			case nSettled == len(ids),
				*anyOne && nSettled > 0,
				!*events && nTrouble > 0:
				os.Exit(waitSummary(c, ids, items, *tail, first))
			}
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			fmt.Printf("timed out after %s\n", *timeout)
			if !down {
				waitSummary(c, ids, items, 0, false)
			}
			os.Exit(waitTimeout)
		}
		time.Sleep(*every)
	}
}

// waitSummary prints every item's line, the log tail of the ones in trouble, and returns the worst code.
func waitSummary(c *client, ids []string, items map[string]proto.Item, tail int, already bool) int {
	code := waitDone
	worse := map[int]int{waitDone: 0, waitCanceled: 1, waitFailed: 2, waitNeedsYou: 3}
	if already {
		fmt.Println("(already settled when the wait started)")
	}
	for _, id := range ids {
		it := items[id]
		fmt.Println(waitLine(it))
		if cd := waitCode(it); settled(it) || trouble(it) {
			if worse[cd] > worse[code] {
				code = cd
			}
			if cd != waitDone && cd != waitCanceled && tail > 0 && !it.Observed {
				for _, l := range logTail(c, id, tail) {
					fmt.Println("    │ " + l)
				}
			}
		}
	}
	return code
}

func logTail(c *client, id string, n int) []string {
	b, _, _ := c.logFrom(id, 0)
	var lines []string // the words, not the :: protocol: that's already in the item's line
	for _, l := range strings.Split(string(b), "\n") {
		if l != "" && !strings.HasPrefix(l, "::") {
			lines = append(lines, l)
		}
	}
	return lines[max(0, len(lines)-n):]
}

// itemNow is the item as the controller sees it now (zero Item if it can't say).
func itemNow(c *client, id string) proto.Item {
	var d struct {
		Item proto.Item `json:"item"`
	}
	c.do("GET", "/api/items/"+id, nil, &d)
	return d.Item
}
