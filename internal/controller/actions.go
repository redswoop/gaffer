package controller

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"gaffer/internal/proto"
)

// An action (`gaffer do ship 13`) is a job of its own: detached, logged, on the board, alerting on
// failure like any other. Its command comes from whoever owns the item (the kind, or the observer that
// reports it), it runs on the host that owns it, and an interrupted action is held, never retried: a
// ship must not happen twice by accident. Whether it took effect, the owner's next report says.

func (c *Controller) act(it *rec, name string) (string, error) {
	cmd, host, err := c.actionFor(it, name)
	if err != nil {
		return "", err
	}
	for _, ch := range c.query("WHERE parent=? AND state NOT IN ('done','failed','canceled')", it.ID) {
		return "", fmt.Errorf("#%d already has %s #%d (%s); let it finish or cancel it first", it.ID, ch.Spec.Steps[0].Name, ch.ID, ch.State)
	}
	spec := proto.Spec{Kind: it.Kind, Title: name + " · " + it.Title, Steps: []proto.Step{{Name: name, Run: cmd}},
		Host: host, OnInterrupt: "hold", Env: map[string]string{"W_ITEM": itoa(it.ID), "W_KEY": it.Key,
			"W_SOURCE": it.Source, "W_ITEM_HOST": it.Host}}
	var top int
	c.db.QueryRow("SELECT COALESCE(MIN(priority),100) FROM items WHERE state='queued'").Scan(&top)
	id := c.insert(spec, proto.SQueued, "", min(top, 100)-1, false) // you asked for it now: ahead of the queue
	ch := c.get(id)
	if ch == nil {
		return "", fmt.Errorf("couldn't queue %s", name)
	}
	ch.Parent = it.ID
	c.put(ch)
	c.event(it.ID, "action", fmt.Sprintf("%s -> #%d", name, id))
	return fmt.Sprintf("%s #%d", name, id), nil
}

func (c *Controller) actionFor(it *rec, name string) (cmd, host string, err error) {
	if it.Source != "" {
		var acts string
		c.db.QueryRow("SELECT actions, host FROM sources WHERE name=?", it.Source).Scan(&acts, &host)
		cmds := map[string]string{}
		json.Unmarshal([]byte(acts), &cmds)
		if !offered(it.Offers, name) {
			return "", "", fmt.Errorf("%s isn't on offer for #%d right now (%s offers: %s)", name, it.ID, it.Source, orNone(split(it.Offers)))
		}
		if cmds[name] == "" {
			return "", "", fmt.Errorf("the %s observer offers %s but its config has no command for it", it.Source, name)
		}
		return cmds[name], host, nil
	}
	if active(it.State) {
		return "", "", fmt.Errorf("#%d is %s; actions wait until it isn't running", it.ID, it.State)
	}
	if it.Spec.Actions[name] == "" {
		return "", "", fmt.Errorf("kind %s has no action %s (has: %s)", it.Kind, name, orNone(keys(it.Spec.Actions)))
	}
	return it.Spec.Actions[name], it.Host, nil
}

// offers is what the board and `show` list as doable now.
func (c *Controller) offers(it *rec) []string {
	if it.Source != "" {
		return split(it.Offers)
	}
	if active(it.State) || it.Parent != 0 {
		return nil
	}
	return keys(it.Spec.Actions)
}

// childEvent notes an action's outcome on the item it acted on.
func (c *Controller) childEvent(it *rec) {
	if it.Parent != 0 && len(it.Spec.Steps) > 0 {
		c.event(it.Parent, "action", strings.TrimSpace(fmt.Sprintf("%s #%d %s %s", it.Spec.Steps[0].Name, it.ID, it.State, it.Reason)))
	}
}

func offered(list, name string) bool {
	for _, a := range split(list) {
		if a == name {
			return true
		}
	}
	return false
}

func split(list string) []string {
	var out []string
	for _, a := range strings.Split(list, ",") {
		if a = strings.TrimSpace(a); a != "" {
			out = append(out, a)
		}
	}
	return out
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func orNone(xs []string) string {
	if len(xs) == 0 {
		return "none"
	}
	return strings.Join(xs, ", ")
}
