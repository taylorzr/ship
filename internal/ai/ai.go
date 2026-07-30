package ai

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func detectTerminal() string {
	if t := os.Getenv("TERMINAL"); t != "" {
		return t
	}

	candidates := []string{
		"x-terminal-emulator",
		"gnome-terminal",
		"konsole",
		"xfce4-terminal",
		"lxterminal",
		"urxvt",
		"xterm",
		"kitty",
		"alacritty",
		"foot",
	}
	for _, c := range candidates {
		if _, err := exec.LookPath(c); err == nil {
			return c
		}
	}
	return ""
}

func reviewArgs(command, prompt string) []string {
	base := filepath.Base(command)
	if base == "opencode" {
		return []string{command, "--prompt", prompt}
	}
	return []string{command, prompt}
}

func LaunchReview(repo string, number int, command string) error {
	if strings.TrimSpace(command) == "" {
		return fmt.Errorf("no AI command configured")
	}
	prompt := fmt.Sprintf("review PR #%d in %s", number, repo)
	args := reviewArgs(command, prompt)

	var cmd *exec.Cmd
	if os.Getenv("KITTY_PID") != "" {
		launchArgs := append([]string{"@", "launch", "--type=window"}, args...)
		cmd = exec.Command("kitty", launchArgs...)
	} else {
		term := detectTerminal()
		if term == "" {
			return fmt.Errorf("no terminal found")
		}
		quoted := make([]string, len(args))
		for i, a := range args {
			quoted[i] = fmt.Sprintf("%q", a)
		}
		shellCmd := strings.Join(quoted, " ") + "; exec sh"
		cmd = exec.Command(term, "-e", "sh", "-c", shellCmd)
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Start()
}
