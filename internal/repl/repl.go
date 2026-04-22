package repl

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/chzyer/readline"
	"github.com/fatih/color"
	"github.com/rchandnaWUSTL/terraform-dev/internal/agent"
	"github.com/rchandnaWUSTL/terraform-dev/internal/config"
	"github.com/rchandnaWUSTL/terraform-dev/internal/provider"
	"github.com/rchandnaWUSTL/terraform-dev/internal/tools"
)

var (
	bold         = color.New(color.Bold)
	cyan         = color.New(color.FgCyan)
	green        = color.New(color.FgGreen)
	red          = color.New(color.FgRed)
	yellow       = color.New(color.FgYellow)
	white        = color.New(color.FgWhite)
	dimWhite     = color.New(color.FgWhite, color.Faint)
	boldWhite    = color.New(color.FgWhite, color.Bold)
	tfPurple     = color.New(color.Attribute(38), color.Attribute(5), color.Attribute(99), color.Bold)  // HashiCorp Terraform #7B42BC
	packerBlue   = color.New(color.Attribute(38), color.Attribute(5), color.Attribute(39), color.Bold)  // HashiCorp Packer #02A8EF
	waypointTeal = color.New(color.Attribute(38), color.Attribute(5), color.Attribute(44))              // HashiCorp Waypoint #14C6CB
	vaultYellow  = color.New(color.Attribute(38), color.Attribute(5), color.Attribute(220))             // HashiCorp Vault #FFCF25
	boundaryPink = color.New(color.Attribute(38), color.Attribute(5), color.Attribute(203))             // HashiCorp Boundary #EC585D
)

type REPL struct {
	cfg       *config.Config
	ag        *agent.Agent
	prov      provider.Provider
	org       string
	workspace string

	mu              sync.Mutex
	lastPlanSummary json.RawMessage
	lastRunID       string
	discardedRuns   map[string]bool
	stdinReader     *bufio.Reader
}

func New(cfg *config.Config, prov provider.Provider, org, workspace string) *REPL {
	return &REPL{
		cfg:           cfg,
		ag:            agent.New(cfg, prov),
		prov:          prov,
		org:           org,
		workspace:     workspace,
		discardedRuns: map[string]bool{},
		stdinReader:   bufio.NewReader(os.Stdin),
	}
}

func (r *REPL) Run() error {
	color.NoColor = false
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
		boundaryPink.Printf("Unknown command: %s\n", parts[0])
		fmt.Println("Type /help for available commands.")
	}
	return false
}

func (r *REPL) ask(userMsg string) {
	ctx := context.Background()

	var sawToolResult atomic.Bool
	var spin *toolSpinner
	ch, err := r.ag.Ask(ctx, userMsg, r.org, r.workspace,
		func(ev agent.ToolCallEvent) {
			spin = startToolSpinner(ev)
		},
		func(name string, result *tools.CallResult) {
			if spin != nil {
				spin.finish(name, result)
				spin = nil
			} else {
				printToolResult(name, result)
			}
			sawToolResult.Store(true)
			r.recordToolResult(name, result)
		},
		r.approveMutation,
	)
	if err != nil {
		boundaryPink.Printf("Error: %v\n", err)
		return
	}

	fmt.Println()
	var buf strings.Builder
	firstLine := true
	flushLine := func(line string) {
		clean := stripMarkdown(line)
		if clean == "" && firstLine {
			return
		}
		if firstLine && sawToolResult.Load() {
			fmt.Println()
		}
		white.Printf("  %s\n", clean)
		firstLine = false
	}
	for chunk := range ch {
		if chunk.Err != nil {
			if buf.Len() > 0 {
				flushLine(buf.String())
				buf.Reset()
			}
			boundaryPink.Printf("Error: %v\n", chunk.Err)
			return
		}
		if chunk.Done {
			break
		}
		buf.WriteString(chunk.Text)
		for {
			s := buf.String()
			idx := strings.IndexByte(s, '\n')
			if idx < 0 {
				break
			}
			flushLine(s[:idx])
			buf.Reset()
			buf.WriteString(s[idx+1:])
		}
	}
	if buf.Len() > 0 {
		flushLine(buf.String())
	}
	fmt.Println()
}

// recordToolResult captures the last successful plan_summary and run_create
// payloads so the approval gate can warn on destructive plans and trigger a
// matching discard when the user cancels an apply.
func (r *REPL) recordToolResult(name string, result *tools.CallResult) {
	if result == nil || result.Err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch name {
	case "_hcp_tf_plan_summary":
		r.lastPlanSummary = result.Output
	case "_hcp_tf_run_create":
		if id := extractRunID(result.Output); id != "" {
			r.lastRunID = id
		}
	case "_hcp_tf_run_discard":
		if id := result.Args["run_id"]; id != "" {
			r.discardedRuns[id] = true
		}
	}
}

// approveMutation prompts the user before a mutating tool executes. It returns
// true when the user types "yes". For apply operations on plans with pending
// destroys, a second confirmation is required. On cancellation of an apply,
// any previously-created run is discarded synchronously so it does not remain
// pending in HCP Terraform.
func (r *REPL) approveMutation(name string, args map[string]string) bool {
	// Already-discarded runs get an automatic pass on follow-up discard calls
	// so the agent's "call discard on cancel" rule doesn't double-prompt.
	if name == "_hcp_tf_run_discard" {
		r.mu.Lock()
		discarded := r.discardedRuns[args["run_id"]]
		r.mu.Unlock()
		if discarded {
			return true
		}
	}

	action := describeAction(name, args)
	fmt.Println()
	vaultYellow.Printf("  ⚠ This will %s. Type 'yes' to confirm or anything else to cancel.\n", action)

	if name == "_hcp_tf_run_apply" {
		if destroys := r.destroysFromLastPlan(); destroys > 0 {
			boundaryPink.Printf("  ✗ This plan will destroy %d resource(s). Type 'yes' again to confirm destruction.\n", destroys)
		}
	}

	if !r.readYes() {
		r.onMutationCancelled(name, args)
		return false
	}

	if name == "_hcp_tf_run_apply" && r.destroysFromLastPlan() > 0 {
		boundaryPink.Println("  ✗ Confirm destruction.")
		if !r.readYes() {
			r.onMutationCancelled(name, args)
			return false
		}
	}

	return true
}

func (r *REPL) readYes() bool {
	fmt.Print("  > ")
	line, err := r.stdinReader.ReadString('\n')
	if err != nil {
		return false
	}
	return strings.TrimSpace(line) == "yes"
}

// onMutationCancelled prints the cancellation marker and, if an apply is being
// cancelled after a run was created, synchronously discards that run.
func (r *REPL) onMutationCancelled(name string, args map[string]string) {
	r.mu.Lock()
	runID := r.lastRunID
	alreadyDiscarded := runID != "" && r.discardedRuns[runID]
	r.mu.Unlock()

	if name == "_hcp_tf_run_apply" && runID != "" && !alreadyDiscarded {
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(r.cfg.TimeoutSeconds)*time.Second)
		defer cancel()
		discard := tools.Call(ctx, "_hcp_tf_run_discard", map[string]string{
			"run_id":  runID,
			"comment": "cancelled at approval gate",
		}, r.cfg.TimeoutSeconds)
		printToolResult("_hcp_tf_run_discard", discard)
		if discard.Err == nil {
			r.mu.Lock()
			r.discardedRuns[runID] = true
			r.mu.Unlock()
		}
	}
	boundaryPink.Println("  Cancelled.")
}

func (r *REPL) destroysFromLastPlan() int {
	r.mu.Lock()
	raw := r.lastPlanSummary
	r.mu.Unlock()
	if len(raw) == 0 {
		return 0
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return 0
	}
	for _, key := range []string{"destroy", "destroys", "resource_destructions", "ResourceDestructions"} {
		if n, ok := toInt(m[key]); ok {
			return n
		}
	}
	return 0
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	case string:
		var x int
		if _, err := fmt.Sscanf(n, "%d", &x); err == nil {
			return x, true
		}
	}
	return 0, false
}

func extractRunID(raw json.RawMessage) string {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	for _, key := range []string{"ID", "id", "run_id", "RunID"} {
		if s, ok := m[key].(string); ok && strings.HasPrefix(s, "run-") {
			return s
		}
	}
	return ""
}

func describeAction(name string, args map[string]string) string {
	switch name {
	case "_hcp_tf_run_create":
		if ws := args["workspace"]; ws != "" {
			return fmt.Sprintf("create a new run in %s", ws)
		}
		return "create a new run"
	case "_hcp_tf_run_apply":
		if ws := args["workspace"]; ws != "" {
			return fmt.Sprintf("apply the pending run in %s", ws)
		}
		return "apply the pending run"
	case "_hcp_tf_run_discard":
		return "discard the pending run"
	}
	return "perform a mutation"
}

var spinnerFrames = []string{"|", "/", "-", "\\"}

type toolSpinner struct {
	stop chan struct{}
	done chan struct{}
	name string
	args string
}

func startToolSpinner(ev agent.ToolCallEvent) *toolSpinner {
	argParts := make([]string, 0, len(ev.Args))
	for k, v := range ev.Args {
		argParts = append(argParts, fmt.Sprintf("%s=%s", k, v))
	}
	s := &toolSpinner{
		stop: make(chan struct{}),
		done: make(chan struct{}),
		name: ev.Name,
		args: strings.Join(argParts, " "),
	}
	s.render(spinnerFrames[0])
	go func() {
		defer close(s.done)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		i := 0
		for {
			select {
			case <-s.stop:
				return
			case <-ticker.C:
				i = (i + 1) % len(spinnerFrames)
				s.render(spinnerFrames[i])
			}
		}
	}()
	return s
}

func (s *toolSpinner) render(frame string) {
	fmt.Print("\r\033[2K  ")
	tfPurple.Print(frame)
	fmt.Print(" ")
	tfPurple.Print(s.name)
	if s.args != "" {
		dimWhite.Printf("  %s", s.args)
	}
}

func (s *toolSpinner) finish(name string, result *tools.CallResult) {
	close(s.stop)
	<-s.done
	fmt.Print("\r\033[2K")
	printToolResult(name, result)
}

func printToolResult(name string, result *tools.CallResult) {
	duration := result.Duration.Round(time.Millisecond)
	if result.Err != nil {
		boundaryPink.Printf("  ✗ %s (%s): %s\n", name, duration, result.Err.Message)
		return
	}

	preview := truncateJSON(result.Output, 120)
	waypointTeal.Printf("  ✓ %s", name)
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

var (
	reMdHeader     = regexp.MustCompile(`^\s*#+\s*`)
	reMdBullet     = regexp.MustCompile(`^\s*[-*+]\s+`)
	reMdBold       = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	reMdItalic     = regexp.MustCompile(`(^|[^*])\*([^*\s][^*]*[^*\s]|[^*\s])\*([^*]|$)`)
	reMdCode       = regexp.MustCompile("`([^`]+)`")
	reMdTableSep   = regexp.MustCompile(`^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)+\|?\s*$`)
	reMdTableRow   = regexp.MustCompile(`^\s*\|.*\|\s*$`)
	reMdBlockquote = regexp.MustCompile(`^\s*>\s*`)
	reMdWhitespace = regexp.MustCompile(`[ \t]{2,}`)
)

// stripMarkdown removes markdown syntax from a single line, returning
// plain prose. Returns "" for purely structural lines (e.g. table separators)
// so callers can drop them.
func stripMarkdown(line string) string {
	s := line
	if reMdTableSep.MatchString(s) {
		return ""
	}
	s = reMdHeader.ReplaceAllString(s, "")
	s = reMdBlockquote.ReplaceAllString(s, "")
	s = reMdBullet.ReplaceAllString(s, "")
	s = reMdBold.ReplaceAllString(s, "$1")
	s = reMdItalic.ReplaceAllString(s, "$1$2$3")
	s = reMdCode.ReplaceAllString(s, "$1")
	if reMdTableRow.MatchString(s) {
		cells := strings.Split(strings.Trim(s, " \t|"), "|")
		for i, c := range cells {
			cells[i] = strings.TrimSpace(c)
		}
		s = strings.Join(cells, "  ")
	}
	s = reMdWhitespace.ReplaceAllString(s, " ")
	return strings.TrimRight(s, " \t")
}

func printBanner(cfg *config.Config) {
	tfRows := []string{
		"  ████████╗███████╗██████╗ ██████╗  █████╗ ███████╗ ██████╗ ██████╗ ███╗   ███╗",
		"  ╚══██╔══╝██╔════╝██╔══██╗██╔══██╗██╔══██╗██╔════╝██╔═══██╗██╔══██╗████╗ ████║",
		"     ██║   █████╗  ██████╔╝██████╔╝███████║█████╗  ██║   ██║██████╔╝██╔████╔██║",
		"     ██║   ██╔══╝  ██╔══██╗██╔══██╗██╔══██║██╔══╝  ██║   ██║██╔══██╗██║╚██╔╝██║",
		"     ██║   ███████╗██║  ██║██║  ██║██║  ██║██║     ╚██████╔╝██║  ██║██║ ╚═╝ ██║",
		"     ╚═╝   ╚══════╝╚═╝  ╚═╝╚═╝  ╚═╝╚═╝  ╚═╝╚═╝      ╚═════╝ ╚═╝  ╚═╝╚═╝     ╚═╝",
	}
	devRows := []string{
		"██████╗ ███████╗██╗   ██╗",
		"██╔══██╗██╔════╝██║   ██║",
		"██║  ██║█████╗  ██║   ██║",
		"██║  ██║██╔══╝  ╚██╗ ██╔╝",
		"██████╔╝███████╗ ╚████╔╝ ",
		"╚═════╝ ╚══════╝  ╚═══╝  ",
	}

	fmt.Println()
	for i := range tfRows {
		tfPurple.Print(tfRows[i])
		packerBlue.Println(devRows[i])
	}
	fmt.Println()
	white.Println("  AI-powered development for infrastructure-as-code")
	dimWhite.Println("  v0.1.0 • Type /help for commands")
	fmt.Println()

	sepWidth := utf8.RuneCountInString(tfRows[0] + devRows[0])
	dimWhite.Println(strings.Repeat("-", sepWidth))
	fmt.Println()
	mode := "readonly"
	if !cfg.Readonly {
		mode = "apply"
	}
	dimWhite.Printf("  model: %s  |  mode: %s  |  type /help for commands\n", cfg.Model, mode)
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
