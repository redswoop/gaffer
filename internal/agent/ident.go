package agent

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"time"
)

// A process is identified by boot id + pid + kernel start time, so neither a reboot nor pid reuse
// can make a dead job look alive.
type ident struct {
	PID   int    `json:"pid"`
	Boot  string `json:"boot"`
	Start uint64 `json:"start"`
}

func bootID() string {
	if b := os.Getenv("GAFFER_BOOT_ID"); b != "" { // tests simulate a reboot with this
		return b
	}
	b, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return strings.TrimSpace(string(b))
}

// procStat returns the process state letter and its start time (clock ticks since boot).
func procStat(pid int) (state string, start uint64, ok bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", 0, false
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')') // comm may contain spaces and parens
	if i < 0 {
		return "", 0, false
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 20 {
		return "", 0, false
	}
	start, _ = strconv.ParseUint(f[19], 10, 64)
	return f[0], start, true
}

func selfIdent() ident {
	_, st, _ := procStat(os.Getpid())
	return ident{PID: os.Getpid(), Boot: bootID(), Start: st}
}

// SelfIdent is this process's identity, for `gaffer lease -- CMD` to hand the agent.
func SelfIdent() (pid int, boot string, start uint64) {
	id := selfIdent()
	return id.PID, id.Boot, id.Start
}

func unmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func clock(ms int64) string { return time.UnixMilli(ms).Format("15:04") }

// alive: same boot, same process (start time matches), not a zombie.
func (id ident) alive() bool {
	if id.PID <= 0 || id.Boot != bootID() {
		return false
	}
	st, start, ok := procStat(id.PID)
	return ok && start == id.Start && st != "Z" && st != "X"
}

func readIdent(path string) (ident, bool) {
	var id ident
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &id) != nil {
		return id, false
	}
	return id, true
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
