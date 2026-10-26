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
	Ntfy       string `toml:"ntfy"` // full topic URL; empty = no pushes
	Kinds      string `toml:"kinds"`
	Agent      struct {
		Host  string         `toml:"host"`
		Dir   string         `toml:"dir"`
		Slots map[string]int `toml:"slots"`
	} `toml:"agent"`
	Token string `toml:"-"`
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
	c.Data, c.TokenFile, c.Kinds, c.Agent.Dir = expand(c.Data), expand(c.TokenFile), expand(c.Kinds), expand(c.Agent.Dir)
	if c.Kinds == "" {
		exe, _ := os.Executable()
		c.Kinds = filepath.Join(filepath.Dir(exe), "kinds")
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
	StopGrace     int               `toml:"stop_grace"`
	StallAfter    int               `toml:"stall_after"`
	ProgressRegex string            `toml:"progress_regex"`
	ProgressPoll  string            `toml:"progress_poll"`
	PollEvery     int               `toml:"poll_every"`
	Fits          string            `toml:"fits"`
	Review        bool              `toml:"review"`
	MaxRetries    int               `toml:"max_retries"`
}

func (c *Config) LoadKind(name string) (*Kind, error) {
	k := &Kind{}
	p := filepath.Join(c.Kinds, name, "kind.toml")
	if _, err := toml.DecodeFile(p, k); err != nil {
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
		OnInterrupt: k.OnInterrupt, Needs: k.Needs, StopGrace: k.StopGrace, StallAfter: k.StallAfter,
		ProgressRegex: k.ProgressRegex, ProgressPoll: k.ProgressPoll, PollEvery: k.PollEvery, Fits: k.Fits,
		Review: k.Review, MaxRetries: k.MaxRetries}
}
