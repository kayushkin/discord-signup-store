// Package restrictedclaude runs one Claude Code turn in a folder, confined to
// it. The avatar drawer and the event picture painter give Claude Code things
// strangers wrote — a photo, a comment, an event's description — which may try
// to steer it, so a turn runs --restricted with only the file tools, which
// Claude Code then confines to the folder: no shell, no web, no MCP servers,
// and no session kept.
package restrictedclaude

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Turn is how a turn is run.
type Turn struct {
	// ClaudePath is the Claude Code command.
	ClaudePath string
	// Model is the model to draw with; "" takes Claude Code's own default.
	Model string
	// MaximumBudgetUSD is the most one turn may spend, in dollars.
	MaximumBudgetUSD string
	// Timeout is how long one turn may take.
	Timeout time.Duration
}

// Run runs one turn in folder with prompt. An error carries the end of what
// Claude Code printed.
func (t Turn) Run(folder, prompt string) error {
	ctx, cancel := context.WithTimeout(context.Background(), t.Timeout)
	defer cancel()
	arguments := []string{"-p", "--restricted", "--tools", "Read,Write,Edit", "--strict-mcp-config",
		"--permission-mode", "acceptEdits", "--no-session-persistence", "--max-budget-usd", t.MaximumBudgetUSD}
	if t.Model != "" {
		arguments = append(arguments, "--model", t.Model)
	}
	command := exec.CommandContext(ctx, t.ClaudePath, append(arguments, prompt)...)
	command.Dir = folder
	output, err := command.CombinedOutput()
	if err != nil {
		tail := strings.TrimSpace(string(output))
		if len(tail) > 400 {
			tail = "…" + tail[len(tail)-400:]
		}
		return fmt.Errorf("Claude Code failed: %v: %s", err, tail)
	}
	return nil
}
