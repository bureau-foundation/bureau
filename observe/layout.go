// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package observe

import (
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// Layout describes the tmux session structure for an observation target.
// This is the Go representation of the m.bureau.layout Matrix state event
// content. It is used both for reading live tmux state and for applying
// desired state from Matrix.
type Layout struct {
	// Prefix is the tmux prefix key for this session (e.g., "C-a").
	// Empty means use the Bureau default (Ctrl-a).
	Prefix string `json:"prefix,omitempty"`

	// Windows is the ordered list of tmux windows in the session.
	Windows []Window `json:"windows"`
}

// Window describes a single tmux window containing one or more panes.
type Window struct {
	// Name is the tmux window name (displayed in the status bar).
	Name string `json:"name"`

	// Panes is the ordered list of panes in this window. The first pane
	// is the root; subsequent panes are splits from it.
	Panes []Pane `json:"panes"`
}

// Pane describes a single pane within a tmux window. Exactly one of
// Observe, Command, or Role must be set.
type Pane struct {
	// Observe is the principal localpart to observe in this pane
	// (e.g., "iree/amdgpu/pm"). When set, the pane runs bureau-observe
	// connected to this principal. Used in channel/room layouts for
	// composite views.
	Observe string `json:"observe,omitempty"`

	// Command is a shell command to run in this pane. Used for local
	// tooling (beads-tui, dashboards, shells) in composite layouts.
	Command string `json:"command,omitempty"`

	// Role identifies the pane's purpose in a principal's own layout
	// (e.g., "agent", "shell"). The launcher resolves roles to concrete
	// commands based on the agent template. Used in principal layouts.
	Role string `json:"role,omitempty"`

	// ObserveMembers dynamically populates panes from room membership.
	// When set, the daemon creates one pane per room member matching
	// the filter. Used in channel layouts for auto-scaling views.
	ObserveMembers *MemberFilter `json:"observe_members,omitempty"`

	// Split is the split direction from the previous pane: "horizontal"
	// or "vertical". Empty for the first pane in a window (which is not
	// a split).
	Split string `json:"split,omitempty"`

	// Size is the pane size as a percentage of the available space
	// (1-99). Empty or zero means tmux divides evenly.
	Size int `json:"size,omitempty"`
}

// MemberFilter selects room members for dynamic pane creation.
type MemberFilter struct {
	// Role filters members by their Bureau role (from the principal's
	// identity metadata). Empty means all members.
	Role string `json:"role,omitempty"`
}

// tmuxPane holds the raw data read from tmux list-panes for a single pane.
type tmuxPane struct {
	index   int
	width   int
	height  int
	left    int
	top     int
	command string
}

// ReadTmuxLayout reads the current layout of a tmux session and returns
// it as a Layout struct. Queries tmux for window list, pane arrangement,
// and running commands.
//
// serverSocket is the tmux server socket path (the -S flag).
// sessionName is the tmux session to read (e.g., "bureau/iree/amdgpu/pm").
func ReadTmuxLayout(serverSocket, sessionName string) (*Layout, error) {
	// Query window list.
	windowLines, err := tmuxCommand(serverSocket, "list-windows",
		"-t", sessionName,
		"-F", "#{window_index}\t#{window_name}")
	if err != nil {
		return nil, fmt.Errorf("listing windows for session %q: %w", sessionName, err)
	}

	layout := &Layout{}

	for _, line := range splitLines(windowLines) {
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("unexpected list-windows output: %q", line)
		}
		windowIndex := parts[0]
		windowName := parts[1]

		// Query panes for this window.
		paneTarget := sessionName + ":" + windowIndex
		paneLines, err := tmuxCommand(serverSocket, "list-panes",
			"-t", paneTarget,
			"-F", "#{pane_index}\t#{pane_width}\t#{pane_height}\t#{pane_left}\t#{pane_top}\t#{pane_current_command}")
		if err != nil {
			return nil, fmt.Errorf("listing panes for %q: %w", paneTarget, err)
		}

		panes, err := parsePaneLines(paneLines)
		if err != nil {
			return nil, fmt.Errorf("parsing panes for %q: %w", paneTarget, err)
		}

		window := Window{
			Name:  windowName,
			Panes: inferPaneSplits(panes),
		}
		layout.Windows = append(layout.Windows, window)
	}

	if len(layout.Windows) == 0 {
		return nil, fmt.Errorf("session %q has no windows", sessionName)
	}

	return layout, nil
}

// ApplyLayout creates or modifies a tmux session to match the given
// layout. Creates windows and panes as needed, sets split sizes, and
// starts commands. Existing panes running matching commands are left
// untouched; extra panes are closed; missing panes are created.
//
// serverSocket is the tmux server socket path (the -S flag).
// sessionName is the tmux session to modify (e.g., "bureau/iree/amdgpu/pm").
// If the session does not exist, it is created.
func ApplyLayout(serverSocket, sessionName string, layout *Layout) error {
	if len(layout.Windows) == 0 {
		return fmt.Errorf("layout has no windows")
	}

	// Check if the session already exists.
	sessionExists := false
	if _, err := tmuxCommand(serverSocket, "has-session", "-t", sessionName); err == nil {
		sessionExists = true
	}

	for windowIndex, window := range layout.Windows {
		if len(window.Panes) == 0 {
			return fmt.Errorf("window %q has no panes", window.Name)
		}

		var windowTarget string

		if windowIndex == 0 {
			if !sessionExists {
				// Create the session with the first window. The first
				// pane's command (if any) runs in the initial pane.
				firstCommand := resolveCommand(window.Panes[0])
				args := []string{"new-session", "-d", "-s", sessionName, "-n", window.Name,
					"-x", "160", "-y", "48"}
				if firstCommand != "" {
					args = append(args, firstCommand)
				}
				if _, err := tmuxCommand(serverSocket, args...); err != nil {
					return fmt.Errorf("creating session %q: %w", sessionName, err)
				}
			} else {
				// Session exists. Rename the current window.
				_, _ = tmuxCommand(serverSocket, "rename-window",
					"-t", sessionName, window.Name)
			}

			// Get the window target for this first window.
			indexLine, err := tmuxCommand(serverSocket, "list-windows",
				"-t", sessionName, "-F", "#{window_index}")
			if err != nil {
				return fmt.Errorf("listing windows after creation: %w", err)
			}
			lines := splitLines(indexLine)
			if len(lines) == 0 {
				return fmt.Errorf("no windows found after creating session %q", sessionName)
			}
			windowTarget = sessionName + ":" + lines[0]
		} else {
			// Create additional windows.
			firstCommand := resolveCommand(window.Panes[0])
			args := []string{"new-window", "-t", sessionName, "-n", window.Name}
			if firstCommand != "" {
				args = append(args, firstCommand)
			}
			if _, err := tmuxCommand(serverSocket, args...); err != nil {
				return fmt.Errorf("creating window %q: %w", window.Name, err)
			}

			// Get the index of the newly created window (the last one).
			indexLine, err := tmuxCommand(serverSocket, "list-windows",
				"-t", sessionName, "-F", "#{window_index}")
			if err != nil {
				return fmt.Errorf("listing windows: %w", err)
			}
			lines := splitLines(indexLine)
			if len(lines) == 0 {
				return fmt.Errorf("no windows found after creating window %q", window.Name)
			}
			windowTarget = sessionName + ":" + lines[len(lines)-1]
		}

		// Create additional panes via splits.
		for paneIndex := 1; paneIndex < len(window.Panes); paneIndex++ {
			pane := window.Panes[paneIndex]

			splitFlag := "-v" // vertical split (pane below)
			if pane.Split == "horizontal" {
				splitFlag = "-h" // horizontal split (pane to right)
			}

			command := resolveCommand(pane)
			args := []string{"split-window", "-t", windowTarget, splitFlag}
			if pane.Size > 0 && pane.Size < 100 {
				args = append(args, "-l", strconv.Itoa(pane.Size)+"%")
			}
			if command != "" {
				args = append(args, command)
			}

			if _, err := tmuxCommand(serverSocket, args...); err != nil {
				return fmt.Errorf("splitting pane in window %q: %w", window.Name, err)
			}
		}
	}

	return nil
}

// resolveCommand returns the shell command for a pane. For Command panes,
// returns the command string directly. For Observe panes, returns a
// bureau observe invocation. For Role panes, returns empty (the launcher
// resolves roles to commands separately).
func resolveCommand(pane Pane) string {
	if pane.Command != "" {
		return pane.Command
	}
	if pane.Observe != "" {
		return "bureau observe " + pane.Observe
	}
	return ""
}

// parsePaneLines parses the output of tmux list-panes -F into tmuxPane
// structs. The format is tab-separated:
// pane_index, pane_width, pane_height, pane_left, pane_top, pane_current_command
func parsePaneLines(output string) ([]tmuxPane, error) {
	var panes []tmuxPane
	for _, line := range splitLines(output) {
		parts := strings.SplitN(line, "\t", 6)
		if len(parts) != 6 {
			return nil, fmt.Errorf("unexpected list-panes output (expected 6 fields): %q", line)
		}

		index, err := strconv.Atoi(parts[0])
		if err != nil {
			return nil, fmt.Errorf("parsing pane index %q: %w", parts[0], err)
		}
		width, err := strconv.Atoi(parts[1])
		if err != nil {
			return nil, fmt.Errorf("parsing pane width %q: %w", parts[1], err)
		}
		height, err := strconv.Atoi(parts[2])
		if err != nil {
			return nil, fmt.Errorf("parsing pane height %q: %w", parts[2], err)
		}
		left, err := strconv.Atoi(parts[3])
		if err != nil {
			return nil, fmt.Errorf("parsing pane left %q: %w", parts[3], err)
		}
		top, err := strconv.Atoi(parts[4])
		if err != nil {
			return nil, fmt.Errorf("parsing pane top %q: %w", parts[4], err)
		}

		panes = append(panes, tmuxPane{
			index:   index,
			width:   width,
			height:  height,
			left:    left,
			top:     top,
			command: parts[5],
		})
	}

	return panes, nil
}

// inferPaneSplits takes panes read from tmux (sorted by visual position)
// and infers the split direction and size for each pane relative to the
// previous one. The first pane has no split direction.
//
// The heuristic compares each pane's position to the previous pane:
//   - Same top, different left → horizontal split (side by side)
//   - Same left, different top → vertical split (stacked)
//   - Otherwise → horizontal split (best guess for complex layouts)
//
// Size is calculated as the pane's dimension percentage of the total
// dimension along the split axis (pane dimension + previous pane dimension
// + 1 for the tmux separator border).
func inferPaneSplits(rawPanes []tmuxPane) []Pane {
	if len(rawPanes) == 0 {
		return nil
	}

	// Sort by visual position: top first, then left. This gives reading
	// order (top-to-bottom, left-to-right within the same row).
	sort.Slice(rawPanes, func(i, j int) bool {
		if rawPanes[i].top != rawPanes[j].top {
			return rawPanes[i].top < rawPanes[j].top
		}
		return rawPanes[i].left < rawPanes[j].left
	})

	panes := make([]Pane, len(rawPanes))

	// First pane: root, no split.
	panes[0] = Pane{
		Command: rawPanes[0].command,
	}

	for i := 1; i < len(rawPanes); i++ {
		current := rawPanes[i]
		previous := rawPanes[i-1]

		pane := Pane{
			Command: current.command,
		}

		if current.top == previous.top {
			// Same row → horizontal split (pane is to the right).
			pane.Split = "horizontal"
			// Size: this pane's width as percentage of the combined width
			// (both panes + 1 cell for the tmux separator).
			totalWidth := previous.width + current.width + 1
			pane.Size = (current.width * 100) / totalWidth
		} else if current.left == previous.left {
			// Same column → vertical split (pane is below).
			pane.Split = "vertical"
			// Size: this pane's height as percentage of the combined height.
			totalHeight := previous.height + current.height + 1
			pane.Size = (current.height * 100) / totalHeight
		} else {
			// Different row AND column — complex nested layout. Use
			// horizontal as default since it's the more common split for
			// side-by-side agent views.
			pane.Split = "horizontal"
			totalWidth := previous.width + current.width + 1
			pane.Size = (current.width * 100) / totalWidth
		}

		panes[i] = pane
	}

	return panes
}

// tmuxCommand runs a tmux command against the given server socket and
// returns the combined stdout/stderr output. All tmux interactions go
// through this function to ensure the -S flag is always present.
func tmuxCommand(serverSocket string, args ...string) (string, error) {
	fullArgs := append([]string{"-S", serverSocket}, args...)
	cmd := exec.Command("tmux", fullArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("tmux %s: %w (output: %s)",
			strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

// splitLines splits output into non-empty lines, trimming whitespace.
func splitLines(output string) []string {
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
