package agent

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// Shim is `gaffer _shim JOBDIR ATTEMPT STEP`. The agent starts it with setsid, so it leads its own
// session + process group and nothing ties it to the agent: an agent restart, crash or upgrade never
// touches a running step. It records who it is, runs the step with output appended to the job log,
// and records the exit code. Pausing = SIGTERM to the whole group; the step gets the signal, the shim
// just waits for it.
func Shim(dir string, attempt, step int) error {
	j, err := loadJob(dir)
	if err != nil {
		return err
	}
	if err := writeJSON(pidPath(dir, attempt, step), selfIdent()); err != nil {
		return err
	}
	signal.Notify(make(chan os.Signal, 1), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP) // caught, not ignored: exec resets it for the child

	log, err := os.OpenFile(filepath.Join(dir, "log"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	st := j.Spec.Steps[step]
	cmd := exec.Command("bash", append([]string{"-c", st.Run, "gaffer-" + st.Name}, j.Spec.Args...)...)
	cmd.Dir = j.Spec.Cwd
	if cmd.Dir == "" {
		cmd.Dir = dir
	}
	cmd.Env = j.env(dir, step)
	cmd.Stdout, cmd.Stderr = log, log
	rc, sig := 0, ""
	if err := cmd.Run(); err != nil {
		rc = 127
		if ee, ok := err.(*exec.ExitError); ok {
			rc = ee.ExitCode()
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				rc, sig = 128+int(ws.Signal()), unix.SignalName(ws.Signal()) // died by a signal, as opposed to exiting 143 by choice
			}
		} else {
			fmt.Fprintf(log, "gaffer: step %s did not start: %v\n", st.Name, err)
		}
	}
	tmp := rcPath(dir, attempt, step) + ".tmp"
	os.WriteFile(tmp, []byte(strconv.Itoa(rc)+"\n"+sig+"\n"), 0o644) // line 2: the signal name, if it died by one
	return os.Rename(tmp, rcPath(dir, attempt, step))
}

func pidPath(dir string, attempt, step int) string {
	return filepath.Join(dir, fmt.Sprintf("pid.%d.%d", attempt, step))
}

func rcPath(dir string, attempt, step int) string {
	return filepath.Join(dir, fmt.Sprintf("rc.%d.%d", attempt, step))
}
