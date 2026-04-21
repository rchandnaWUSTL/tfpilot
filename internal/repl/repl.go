package repl

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/chzyer/readline"
	"github.com/fatih/color"
	"github.com/rchandnaWUSTL/terraform-dev/internal/agent"
	"github.com/rchandnaWUSTL/terraform-dev/internal/config"
	"github.com/rchandnaWUSTL/terraform-dev/internal/tools"
)

var (
	bold      = color.New(color.Bold)
	cyan      = color.New(color.FgCyan)
	green     = color.New(color.FgGreen)
	red       = color.New(color.FgRed)
	yellow    = color.New(color.FgYellow)
	white     = color.New(color.FgWhite)
	dimWhite  = color.New(color.FgWhite, color.Faint)
	magenta   = color.New(color.FgMagenta, color.Bold)
	boldWhite = color.New(color.FgWhite, color.Bold)
)

type REPL struct {
	cfg       *config.Config
	ag        *agent.Agent
	org       string
	workspace string
}

func New(cfg *config.Config, org, workspace string) *REPL {
	return &REPL{
		cfg:       cfg,
		ag:        agent.New(cfg),
		org:       org,
		workspace: workspace,
	}
}

func (r *REPL) Run() error {
	printBanner(r.cfg)

	rl, err := readline.NewEx(&readline.Config{
		Prompt:          cyan.Sprint("hcp-tf> "),
		HistoryFile:     historyPath(),
		InterruptPrompt: "^C",
		EOFPrompt:       "exit",
	})
	if err != nil {
		return fmt.Errorf("readline: %w", err)
	}
	defer rl.Close()

	for {
		line, err := rl.Readline()
		if err != nil {
			break
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "/") {
			if done := r.handleSlash(line); done {
				break
			}
			continue
		}

		r.ask(line)
	}
	return nil
}

func (r *REPL) handleSlash(cmd string) (exit bool) {
	parts := strings.Fields(cmd)
	switch parts[0] {
	case "/exit", "/quit":
		fmt.Println("Goodbye.")
		return true
	case "/help":
		printHelp()
	case "/reset":
		r.ag.Reset()
		dimWhite.Println("Conversation reset.")
	case "/org":
		if len(parts) > 1 {
			r.org = parts[1]
			green.Printf("org set to %s\n", r.org)
		} else {
			fmt.Printf("org: %s\n", r.org)
		}
	case "/workspace":
		if len(parts) > 1 {
			r.workspace = parts[1]
			green.Printf("workspace set to %s\n", r.workspace)
		} else {
			fmt.Printf("workspace: %s\n", r.workspace)
		}
	case "/mode":
		if r.cfg.Readonly {
			green.Println("mode: readonly (default)")
		} else {
			yellow.Println("mode: read-write")
		}
	default:
		red.Printf("Unknown command: %s\n", parts[0])
		fmt.Println("Type /help for available commands.")
	}
	return false
}

func (r *REPL) ask(userMsg string) {
	ctx := context.Background()

	ch, err := r.ag.Ask(ctx, userMsg, r.org, r.workspace,
		func(ev agent.ToolCallEvent) {
			printToolCall(ev)
		},
		func(name string, result *tools.CallResult) {
			printToolResult(name, result)
		},
	)
	if err != nil {
		red.Printf("Error: %v\n", err)
		return
	}

	fmt.Println()
	white.Print("  ")
	for chunk := range ch {
		if chunk.Err != nil {
			fmt.Println()
			red.Printf("Error: %v\n", chunk.Err)
			return
		}
		if chunk.Done {
			break
		}
		// Print inline, replacing newlines with newline + indent
		text := strings.ReplaceAll(chunk.Text, "\n", "\n  ")
		white.Print(text)
	}
	fmt.Print("\n\n")
}

func printToolCall(ev agent.ToolCallEvent) {
	argParts := make([]string, 0, len(ev.Args))
	for k, v := range ev.Args {
		argParts = append(argParts, fmt.Sprintf("%s=%s", k, v))
	}
	cyan.Printf("  ⟳ %s", ev.Name)
	if len(argParts) > 0 {
		dimWhite.Printf("  %s", strings.Join(argParts, " "))
	}
	fmt.Println()
}

func printToolResult(name string, result *tools.CallResult) {
	duration := result.Duration.Round(time.Millisecond)
	if result.Err != nil {
		red.Printf("  ✗ %s (%s): %s\n", name, duration, result.Err.Message)
		return
	}

	preview := truncateJSON(result.Output, 120)
	green.Printf("  ✓ %s", name)
	dimWhite.Printf(" (%s)  %s\n", duration, preview)
}

func truncateJSON(raw json.RawMessage, maxLen int) string {
	s := strings.ReplaceAll(string(raw), "\n", " ")
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxLen {
		return s[:maxLen] + "…"
	}
	return s
}

func printBanner(cfg *config.Config) {
	fmt.Println()
	magenta.Println(`  ████████╗███████╗██████╗ ██████╗  █████╗ ███████╗ ██████╗ ██████╗ ███╗   ███╗
  ╚══██╔══╝██╔════╝██╔══██╗██╔══██╗██╔══██╗██╔════╝██╔═══██╗██╔══██╗████╗ ████║
     ██║   █████╗  ██████╔╝██████╔╝███████║█████╗  ██║   ██║██████╔╝██╔████╔██║
     ██║   ██╔══╝  ██╔══██╗██╔══██╗██╔══██║██╔══╝  ██║   ██║██╔══██╗██║╚██╔╝██║
     ██║   ███████╗██║  ██║██║  ██║██║  ██║██║     ╚██████╔╝██║  ██║██║ ╚═╝ ██║
     ╚═╝   ╚══════╝╚═╝  ╚═╝╚═╝  ╚═╝╚═╝  ╚═╝╚═╝      ╚═════╝ ╚═╝  ╚═╝╚═╝     ╚═╝`)
	boldWhite.Println(`                              ██████╗ ███████╗██╗   ██╗
                              ██╔══██╗██╔════╝██║   ██║
                              ██║  ██║█████╗  ██║   ██║
                              ██║  ██║██╔══╝  ╚██╗ ██╔╝
                              ██████╔╝███████╗ ╚████╔╝
                              ╚═════╝ ╚══════╝  ╚═══╝  `)
	fmt.Println()
	dimWhite.Printf("  model: %s  |  mode: readonly  |  type /help for commands\n", cfg.Model)
	fmt.Println()
}

func printHelp() {
	bold.Println("Commands:")
	fmt.Println("  /org <name>        Set default org")
	fmt.Println("  /workspace <name>  Set default workspace")
	fmt.Println("  /mode              Show current mode")
	fmt.Println("  /reset             Clear conversation history")
	fmt.Println("  /help              Show this help")
	fmt.Println("  /exit              Exit")
	fmt.Println()
	bold.Println("Examples:")
	fmt.Println("  Is it safe to apply my latest prod changes to staging?")
	fmt.Println("  Describe the prod-us-east-1 workspace")
	fmt.Println("  Any of my workspaces drifted this week?")
	fmt.Println()
}

func historyPath() string {
	home, _ := os.UserHomeDir()
	dir := home + "/.terraform-dev"
	_ = os.MkdirAll(dir, 0700)
	return dir + "/history"
}
