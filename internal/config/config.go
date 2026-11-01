// Package config reads ~/.config/gaffer/config.toml (or $GAFFER_CONFIG) and the kind manifests.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"gaffer/internal/proto"
)

type Config struct {
	Controller string `toml:"controller"` // URL agents and the CLI talk to
	Listen     string `toml:"listen"`     // controller bind; "auto:PORT" = tailscale IP, else 127.0.0.1
	Data       string `toml:"data"`       // controller: db + logs
	TokenFile  string `toml:"token_file"`
	Ntfy       string `toml:"ntfy"`  // full topic URL; empty = no pushes
	Kinds      Dirs   `toml:"kinds"` // one dir or a list; a kind's name resolves to the first dir that has it
	// PreemptGap: a queued job bumps a running one off a full slot when its priority number is lower by at
	// least this much (default 50; negative turns job-vs-job preemption off). Leases always bump.
	PreemptGap int `toml:"preempt_gap"`
	Agent      struct {
		Host      string              `toml:"host"`
		Dir       string              `toml:"dir"`
		Slots     map[string]int      `toml:"slots"`
		Path      Dirs                `toml:"path"` // put in front of PATH for everything the agent runs (a unit's PATH is bare)
		Observe   []Observer          `toml:"observe"`
		Resources map[string]Resource `toml:"resources"` // per slot label: how to see who has it and how to clear idle residents
	} `toml:"agent"`
	Token string `toml:"-"`
}

// Resource is the agent's hooks for one slot label (gpu, say). `probe` prints `used=N total=M` and then one
// `PID NAME AMOUNT` line per process using it; `release` is run before a job takes the resource when nothing
// holds a lease on it and the probe still shows more than idle_mib in foreign use (cached models, an
// idle chat server): it gets GAFFER_RES, GAFFER_HOLDERS (lease holders, holder:class, comma-separated)
// and, when run for `gaffer lease --wait`, GAFFER_WAITING_FOR (that holder).
type Resource struct {
	Probe   string `toml:"probe"`
	Release string `toml:"release"`
	IdleMiB int    `toml:"idle_mib"` // foreign use at or below this is nothing to clear (default 0)
	Every   int    `toml:"every"`    // probe period, seconds (default 10)
}

// Gap is the effective preemption gap: 0 in the file means the default, negative means off.
func (c *Config) Gap() int {
	if c.PreemptGap == 0 {
		return 50
	}
	return c.PreemptGap
}

// Dirs takes `kinds = "/a"` as well as `kinds = ["/a", "/b"]`.
type Dirs []string

func (d *Dirs) UnmarshalTOML(v any) error {
	switch x := v.(type) {
	case string:
		*d = Dirs{x}
	case []any:
		for _, e := range x {
			s, ok := e.(string)
			if !ok {
				return fmt.Errorf("kinds: want strings, got %T", e)
			}
			*d = append(*d, s)
		}
	default:
		return fmt.Errorf("kinds: want a string or a list, got %T", v)
	}
	return nil
}

// Observer is a script an agent runs every `every` seconds to report work gaffer doesn't run: its
// output is a full snapshot of that source (::item lines, see README), reconciled by the controller.
type Observer struct {
	Name    string            `toml:"name"`
	Run     string            `toml:"run"`
	Every   int               `toml:"every"`
	Actions map[string]string `toml:"actions"` // offered per item by the snapshot's actions=a,b
}

func Path() string {
	if p := os.Getenv("GAFFER_CONFIG"); p != "" {
		return p
	}
	return filepath.Join(home(), ".config/gaffer/config.toml")
}

func home() string { h, _ := os.UserHomeDir(); return h }

func expand(p string) string {
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home(), p[2:])
	}
	return p
}

func Load() (*Config, error) {
	c := &Config{Controller: "http://127.0.0.1:8097", Listen: "auto:8097", Data: "~/.local/share/gaffer",
		TokenFile: "~/.config/gaffer/token"}
	if _, err := toml.DecodeFile(Path(), c); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("%s: %w", Path(), err)
	}
	c.Data, c.TokenFile, c.Agent.Dir = expand(c.Data), expand(c.TokenFile), expand(c.Agent.Dir)
	for i := range c.Kinds {
		c.Kinds[i] = expand(c.Kinds[i])
	}
	if len(c.Kinds) == 0 {
		exe, _ := os.Executable()
		c.Kinds = Dirs{filepath.Join(filepath.Dir(exe), "kinds")}
	}
	if c.Agent.Host == "" {
		c.Agent.Host, _ = os.Hostname()
		c.Agent.Host = strings.ToLower(c.Agent.Host)
	}
	if c.Agent.Dir == "" {
		c.Agent.Dir = filepath.Join(c.Data, "agent")
	}
	if len(c.Agent.Slots) == 0 {
		c.Agent.Slots = map[string]int{"cpu": 2}
	}
	if b, err := os.ReadFile(c.TokenFile); err == nil {
		c.Token = strings.TrimSpace(string(b))
	}
	if t := os.Getenv("GAFFER_TOKEN"); t != "" {
		c.Token = t
	}
	return c, nil
}

// EnsureToken creates the shared token on first controller start.
func (c *Config) EnsureToken() error {
	if c.Token != "" {
		return nil
	}
	b := make([]byte, 24)
	rand.Read(b)
	c.Token = hex.EncodeToString(b)
	if err := os.MkdirAll(filepath.Dir(c.TokenFile), 0o700); err != nil {
		return err
	}
	return os.WriteFile(c.TokenFile, []byte(c.Token+"\n"), 0o600)
}

// ListenAddr resolves "auto:PORT" to the tailscale IPv4 (tailnet-only), falling back to loopback.
func (c *Config) ListenAddr() string {
	host, port, ok := strings.Cut(c.Listen, ":")
	if !ok || host != "auto" {
		return c.Listen
	}
	if out, err := exec.Command("tailscale", "ip", "-4").Output(); err == nil {
		if ip := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0]); ip != "" {
			return ip + ":" + port
		}
	}
	return "127.0.0.1:" + port
}

// Kind is a kinds/<name>/kind.toml manifest. Step commands usually reference $W_KIND_DIR, which each
// agent sets to its own copy of the kind directory.
type Kind struct {
	Title         string            `toml:"title"` // {{args}}, {{1}}, {{2}}… substituted
	Steps         []proto.Step      `toml:"steps"`
	Env           map[string]string `toml:"env"`
	OnInterrupt   string            `toml:"on_interrupt"`
	Needs         string            `toml:"needs"`
	Shares        []string          `toml:"shares"`   // lease classes this kind's steps co-exist with (a step may override)
	Priority      int               `toml:"priority"` // default --prio for this kind (0 = 100)
	StopGrace     int               `toml:"stop_grace"`
	StallAfter    int               `toml:"stall_after"`
	ProgressRegex string            `toml:"progress_regex"`
	ProgressPoll  string            `toml:"progress_poll"`
	PollEvery     int               `toml:"poll_every"`
	Fits          string            `toml:"fits"`
	Review        bool              `toml:"review"`
	Quiet         bool              `toml:"quiet"`
	Actions       map[string]string `toml:"actions"`
	MaxRetries    int               `toml:"max_retries"`
}

// KindDir is the first kinds dir that has <name>/kind.toml ("" if none does).
func (c *Config) KindDir(name string) string {
	for _, d := range c.Kinds {
		if _, err := os.Stat(filepath.Join(d, name, "kind.toml")); err == nil {
			return filepath.Join(d, name)
		}
	}
	return ""
}

func (c *Config) LoadKind(name string) (*Kind, error) {
	k := &Kind{}
	dir := c.KindDir(name)
	if dir == "" {
		return nil, fmt.Errorf("kind %s: not in %s", name, strings.Join(c.Kinds, ", "))
	}
	if _, err := toml.DecodeFile(filepath.Join(dir, "kind.toml"), k); err != nil {
		return nil, fmt.Errorf("kind %s: %w", name, err)
	}
	if len(k.Steps) == 0 {
		return nil, fmt.Errorf("kind %s: no steps", name)
	}
	return k, nil
}

// Spec resolves a kind + args into a self-contained job spec.
func (k *Kind) Spec(name string, args []string) proto.Spec {
	title := k.Title
	if title == "" {
		title = name + " {{args}}"
	}
	title = strings.ReplaceAll(title, "{{args}}", strings.Join(args, " "))
	for i := 9; i >= 1; i-- {
		v := ""
		if i <= len(args) {
			v = args[i-1]
		}
		title = strings.ReplaceAll(title, fmt.Sprintf("{{%d}}", i), v)
	}
	return proto.Spec{Kind: name, Title: strings.TrimSpace(title), Args: args, Env: k.Env, Steps: k.Steps,
		OnInterrupt: k.OnInterrupt, Needs: k.Needs, Shares: k.Shares, Priority: k.Priority, StopGrace: k.StopGrace, StallAfter: k.StallAfter,
		ProgressRegex: k.ProgressRegex, ProgressPoll: k.ProgressPoll, PollEvery: k.PollEvery, Fits: k.Fits,
		Review: k.Review, Quiet: k.Quiet, Actions: k.Actions, MaxRetries: k.MaxRetries}
}
