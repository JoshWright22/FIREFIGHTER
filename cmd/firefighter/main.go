// Command firefighter lets AI agents share one computer's GPU, CPU and RAM while the human always comes first.
//
// It is a single binary: a local scheduler daemon, the CLI agents queue heavy work through
// (firefighter run), and an MCP server over stdio (firefighter mcp). See https://github.com/JoshWright22/FIREFIGHTER.
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/JoshWright22/firefighter/internal/client"
	"github.com/JoshWright22/firefighter/internal/mcp"
	"github.com/JoshWright22/firefighter/internal/ui"
)

var version = "dev"

const usage = `FIREFIGHTER: Lets several AI coding agents share one computer without slowing it down or getting in your way.
Agents queue their heavy jobs here; they take turns, and you always come first.

  firefighter run [--gpu] [--net] --minutes N [--vram GB] [--ram GB] [--urgent] [--name NAME] -- <command...>
                                 wait for a turn, run the command here, print a receipt
      --detach                   queue it and return a job id at once (then: firefighter wait <id>)
      --evict never|graceful|force   what may happen if the human needs resources back
      --spot                     leftovers only: cheapest, preempted first
  firefighter quote [--gpu] --minutes N [--vram GB] [--ram GB]
                                 the market: price now, start time and cost per bid, busy odds
  firefighter bid <id> <1-10>    change a waiting job's tier (5 normal, 10 urgent)
  firefighter amend <id> [--ram GB] [--vram GB] [--minutes N] [--urgent]  change a waiting job's declarations; it keeps its place and its waiting run
  firefighter wait <id>          block until a job ends; prints its receipt and log tail
  firefighter status [--json]    the human's reserve, what's running until when, the queue, budgets
  firefighter forecast [--json]  what the human and each known command are expected to use, as learned
  firefighter harvest [earned <usd> | reset]  the idle harvester's power bill, earnings and profit guard
  firefighter update [--check]   install the newest release now (the guard also does it by itself: update.auto)
  firefighter extend <id> <min>  lengthen a running job's lease
  firefighter cancel <id>        cancel a waiting or running job
  firefighter show <id> [--json] one job in full: declared and counted needs, place, why it waits
  firefighter adopt <pid> [--gpu] [--name NAME] [--minutes N]   govern a process already running
  firefighter plan "<what>" --when "<when>" --minutes N [--gpu PCT] [--vram GB] [--ram GB] [--done]
  firefighter doctor             check the install, the guard, agent rules, Docker, disks; prints the fix
  firefighter storage [claim --gb N | path | release [--delete]]  disk space the human allots agents
  firefighter config [show | set <key> <value> | unset <key> | presets | path]
  firefighter protocol           the pause/checkpoint helper for long GPU loops
  firefighter statusline [--json]  one line for a status bar (Claude Code, VS Code, tmux, T3 Code)
  firefighter events             NDJSON stream of status changes, for editor plugins
  firefighter setup              start at logon + teach every agent CLI (MCP + global rules)
  firefighter uninstall          undo setup
  firefighter daemon [--stop]    the guard itself (setup and the CLI start it for you)
  firefighter mcp                MCP server on stdio (setup registers it)
  firefighter version
`

func main() {
	client.Version = version
	if len(os.Args) < 2 {
		ui.PrintBanner()
		fmt.Fprint(os.Stderr, "\n"+usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") && cmd != "run" {
		fmt.Print(commandHelp(cmd))
		os.Exit(0)
	}
	var err error
	code := 0
	switch cmd {
	case "run":
		code, err = run(args)
	case "wait":
		code, err = wait(args)
	case "status":
		err = status(args)
	case "extend":
		err = extend(args)
	case "quote":
		err = quote(args)
	case "statusline":
		err = statusline(args)
	case "events":
		err = events(args)
	case "bid":
		err = rebid(args)
	case "amend":
		err = amend(args)
	case "cancel":
		err = act(args, "cancel")
	case "show":
		err = show(args)
	case "adopt":
		err = adopt(args)
	case "plan":
		err = plan(args)
	case "config":
		err = configCmd(args)
	case "forecast":
		err = forecast(args)
	case "harvest":
		err = harvestCmd(args)
	case "storage":
		err = storageCmd(args)
	case "doctor":
		code, err = doctor(args)
	case "update":
		err = updateCmd(args)
	case "protocol":
		fmt.Print(protocol)
	case "setup", "install":
		err = install(args)
	case "uninstall":
		err = uninstall(args)
	case "daemon":
		err = daemonCmd(args)
	case "mcp":
		err = mcp.Serve(version)
	case "version", "--version", "-v":
		fmt.Println("firefighter", version)
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		err = fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
		code = 2
	}
	if err != nil {
		ui.Say(ui.Hot, "%v", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// commandHelp is the usage lines for one command (all of them for an unknown one).
func commandHelp(cmd string) string {
	var out []string
	keep := false
	for _, l := range strings.Split(usage, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "firefighter ") {
			f := strings.Fields(l)
			keep = len(f) > 1 && (f[1] == cmd || (cmd == "install" && f[1] == "setup"))
		}
		if keep {
			out = append(out, l)
		}
	}
	if len(out) == 0 {
		return usage
	}
	return strings.Join(out, "\n") + "\n"
}
