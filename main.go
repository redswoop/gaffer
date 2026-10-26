// gaffer: one controller, an agent per machine, any kind of work. See README.md.
package main

import (
	"fmt"
	"log"
	"os"
	"strconv"

	"gaffer/internal/agent"
	"gaffer/internal/cli"
	"gaffer/internal/config"
	"gaffer/internal/controller"
)

func main() {
	log.SetFlags(log.Ltime)
	if len(os.Args) < 2 {
		cli.Main(nil, "", nil)
	}
	cmd, args := os.Args[1], os.Args[2:]
	if cmd == "_shim" { // gaffer _shim JOBDIR ATTEMPT STEP (started by the agent, detached)
		if len(args) != 3 {
			os.Exit(2)
		}
		a, _ := strconv.Atoi(args[1])
		s, _ := strconv.Atoi(args[2])
		if err := agent.Shim(args[0], a, s); err != nil {
			fmt.Fprintln(os.Stderr, "shim:", err)
			os.Exit(1)
		}
		return
	}
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	switch cmd {
	case "controller":
		log.Fatal(controller.Run(cfg))
	case "agent":
		log.Fatal(agent.Run(cfg))
	case "version":
		fmt.Println("gaffer", agent.Version)
	default:
		cli.Main(cfg, cmd, args)
	}
}
