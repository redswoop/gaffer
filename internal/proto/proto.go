// Package proto holds the types the controller, the agents and the CLI exchange.
package proto

// Step is one resumable unit of a job: `bash -c Run <name> <args...>`. A job that dies between steps
// picks up at the next step without asking the kind; resuming inside a step is the kind's business
// (::checkpoint + W_RESUME).
type Step struct {
	Name string `json:"name" toml:"name"`
	Run  string `json:"run" toml:"run"`
}

// Spec is everything an agent needs to run a job. It is resolved from the kind manifest at submit
// time and stored with the item, so editing a kind never changes a job that is already queued.
type Spec struct {
	Kind          string            `json:"kind"`
	Title         string            `json:"title"`
	Args          []string          `json:"args,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
	Cwd           string            `json:"cwd,omitempty"`
	Steps         []Step            `json:"steps"`
	OnInterrupt   string            `json:"on_interrupt,omitempty"` // resume (default) | restart | hold
	Needs         string            `json:"needs,omitempty"`        // slot label; "" = cpu
	Host          string            `json:"host,omitempty"`         // pin to one agent
	StopGrace     int               `json:"stop_grace,omitempty"`   // seconds between TERM and KILL
	StallAfter    int               `json:"stall_after,omitempty"`  // seconds without progress -> stalled
	ProgressRegex string            `json:"progress_regex,omitempty"`
	ProgressPoll  string            `json:"progress_poll,omitempty"`
	PollEvery     int               `json:"poll_every,omitempty"`
	Fits          string            `json:"fits,omitempty"` // run on the agent before starting; nonzero = not now
	Review        bool              `json:"review,omitempty"`
	MaxRetries    int               `json:"max_retries,omitempty"` // interrupts tolerated before failing
}

// Progress is the one shape every source (::progress lines, log regex, poll command) is folded into.
type Progress struct {
	Done  float64 `json:"done"`
	Total float64 `json:"total,omitempty"`
	Unit  string  `json:"unit,omitempty"`
	Phase string  `json:"phase,omitempty"`
	Msg   string  `json:"msg,omitempty"`
	ETA   float64 `json:"eta,omitempty"` // seconds left, only when the job states it
	TS    int64   `json:"ts"`            // unix ms the agent last saw it change
}

// Checkpoint is the last `::checkpoint k=v ...` a job printed. It is handed back on resume as W_CHECKPOINT.
type Checkpoint struct {
	Data map[string]string `json:"data"`
	Done float64           `json:"done,omitempty"`
	Step int               `json:"step"` // the step it belongs to; cleared when that step finishes
	TS   int64             `json:"ts"`
}

// Assign asks an agent to start (or restart / resume) a job. Re-sent every sync until the agent reports
// that attempt, so a lost response or an agent crash can't drop it; agents ignore attempts they have.
type Assign struct {
	ID         int64       `json:"id"`
	Attempt    int         `json:"attempt"`
	Step       int         `json:"step"`
	Resume     bool        `json:"resume"`
	Checkpoint *Checkpoint `json:"checkpoint,omitempty"`
	Spec       Spec        `json:"spec"`
}

type Stop struct {
	ID   int64  `json:"id"`
	Mode string `json:"mode"` // pause | cancel
}

// Agent-side job states.
const (
	JRunning     = "running"
	JDone        = "done"
	JFailed      = "failed"
	JStopped     = "stopped"     // exited after a stop request
	JInterrupted = "interrupted" // the process vanished without an exit code (reboot, kill -9, OOM killer)
	JRejected    = "rejected"    // the kind's fits check said not now
)

// JobReport is the agent's view of one job attempt. Agents report every attempt the controller hasn't
// acknowledged yet; the controller is level-triggered off these, never off a stream of events.
type JobReport struct {
	ID         int64       `json:"id"`
	Attempt    int         `json:"attempt"`
	State      string      `json:"state"`
	Step       int         `json:"step"`
	StepName   string      `json:"step_name,omitempty"`
	PID        int         `json:"pid,omitempty"`
	RC         int         `json:"rc"`
	Reason     string      `json:"reason,omitempty"`
	Progress   *Progress   `json:"progress,omitempty"`
	Checkpoint *Checkpoint `json:"checkpoint,omitempty"`
	Attention  string      `json:"attention,omitempty"` // last ::needs-you
	LogSize    int64       `json:"log_size"`
	Started    int64       `json:"started,omitempty"`
	Finished   int64       `json:"finished,omitempty"`
}

type LogChunk struct {
	ID     int64  `json:"id"`
	Offset int64  `json:"offset"`
	Data   []byte `json:"data"`
}

type SyncReq struct {
	Host    string         `json:"host"`
	BootID  string         `json:"boot_id"`
	Version string         `json:"version"`
	Slots   map[string]int `json:"slots"`
	Jobs    []JobReport    `json:"jobs"`
	Logs    []LogChunk     `json:"logs,omitempty"`
}

type SyncResp struct {
	Start   []Assign        `json:"start,omitempty"`
	Stop    []Stop          `json:"stop,omitempty"`
	Ack     []AckRef        `json:"ack,omitempty"`      // attempts the controller has recorded as final
	LogHave map[int64]int64 `json:"log_have,omitempty"` // bytes of each job's log the controller holds
}

type AckRef struct {
	ID      int64 `json:"id"`
	Attempt int   `json:"attempt"`
}

// Item is the controller's record, as the API and CLI see it.
type Item struct {
	ID         int64       `json:"id"`
	Kind       string      `json:"kind"`
	Title      string      `json:"title"`
	State      string      `json:"state"`
	Host       string      `json:"host,omitempty"`
	Attempt    int         `json:"attempt"`
	Retries    int         `json:"retries"`
	Step       int         `json:"step"`
	StepName   string      `json:"step_name,omitempty"`
	Priority   int         `json:"priority"`
	Reason     string      `json:"reason,omitempty"`
	Attention  string      `json:"attention,omitempty"`
	Observed   bool        `json:"observed,omitempty"`
	Progress   *Progress   `json:"progress,omitempty"`
	Checkpoint *Checkpoint `json:"checkpoint,omitempty"`
	Created    int64       `json:"created"`
	Started    int64       `json:"started,omitempty"`
	Finished   int64       `json:"finished,omitempty"`
	Updated    int64       `json:"updated"`
	Spec       Spec        `json:"spec"`
	// derived at read time
	Rate    float64 `json:"rate,omitempty"`     // units per second
	ETA     float64 `json:"eta,omitempty"`      // seconds left
	Stalled bool    `json:"stalled,omitempty"`  // running, but no progress for stall_after
	LogSize int64   `json:"log_size,omitempty"` // bytes the controller holds
}

// Controller-side item states.
const (
	SQueued   = "queued"
	SHeld     = "held"
	SAssigned = "assigned"
	SRunning  = "running"
	SStopping = "stopping"
	SPaused   = "paused"
	SNeedsYou = "needs-you"
	SDone     = "done"
	SFailed   = "failed"
	SCanceled = "canceled"
)

type Host struct {
	Host     string         `json:"host"`
	BootID   string         `json:"boot_id"`
	Version  string         `json:"version"`
	Slots    map[string]int `json:"slots"`
	Used     map[string]int `json:"used"`
	Seen     int64          `json:"seen"`
	Online   bool           `json:"online"`
	Drained  bool           `json:"drained"`
	Note     string         `json:"note,omitempty"`
	Failures int            `json:"failures"`
}

type Status struct {
	Hosts []Host `json:"hosts"`
	Items []Item `json:"items"`
	Now   int64  `json:"now"`
}
