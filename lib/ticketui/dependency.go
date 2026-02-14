// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticketui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/bureau-foundation/bureau/lib/schema"
)

// depNode holds the data for one node in the dependency graph. Each
// node is either a left (blocked-by) or right (blocks) neighbor of
// the center ticket, or the center ticket itself.
type depNode struct {
	ticketID string
	content  schema.TicketContent
	exists   bool // False when the ticket is not in the index.
}

// DependencyGraph renders a compact horizontal ASCII DAG showing the
// immediate dependency neighborhood of a ticket. The center ticket
// has its blocked-by nodes fanning in from the left and its blocks
// nodes fanning out to the right, connected with box-drawing chars.
//
// Layout for 3 left, 2 right:
//
//	● tkt-abc ─┐               ┌─ ● tkt-ghi
//	● tkt-def ─┼── tkt-center ─┤
//	● tkt-xyz ─┘               └─ ● tkt-jkl
//
// All neighbor nodes are clickable. Returns the rendered graph and
// click targets with line offsets relative to the graph (no header).
type DependencyGraph struct {
	theme Theme
	width int
}

// NewDependencyGraph creates a graph renderer for the given width.
func NewDependencyGraph(theme Theme, width int) DependencyGraph {
	return DependencyGraph{theme: theme, width: width}
}

// Render produces the dependency graph for the given ticket. Returns
// the rendered string and click targets for neighbor nodes. Returns
// empty string and nil targets if the ticket has no dependencies.
func (graph DependencyGraph) Render(centerID string, blockedBy []string, blocks []string, source Source) (string, []BodyClickTarget) {
	if len(blockedBy) == 0 && len(blocks) == 0 {
		return "", nil
	}

	// Resolve nodes.
	leftNodes := graph.resolveNodes(blockedBy, source)
	rightNodes := graph.resolveNodes(blocks, source)

	totalRows := max(len(leftNodes), len(rightNodes))
	if totalRows < 1 {
		totalRows = 1
	}

	// Vertical centering offsets.
	leftStart := (totalRows - len(leftNodes)) / 2
	rightStart := (totalRows - len(rightNodes)) / 2
	centerRow := totalRows / 2

	// Compute column widths.
	leftLabelWidth := graph.maxLabelWidth(leftNodes)
	rightLabelWidth := graph.maxLabelWidth(rightNodes)
	centerLabel := centerID

	// The fixed-width portions: left connector (2: " ─" before merge)
	// + merge column (1) + left arm (2: "─ ") + center label +
	// right arm (2: " ─") + split column (1) + right connector (2: "─ ").
	fixedWidth := 0
	if len(leftNodes) > 0 {
		fixedWidth += 2 + 1 + 2 // " ─" + merge + "─ "
	}
	if len(rightNodes) > 0 {
		fixedWidth += 2 + 1 + 2 // " ─" + split + "─ "
	}

	// Truncate labels if the graph exceeds the available width.
	totalWidth := leftLabelWidth + fixedWidth + lipgloss.Width(centerLabel) + rightLabelWidth
	if totalWidth > graph.width {
		// Shrink side labels proportionally, preserving the center.
		available := graph.width - fixedWidth - lipgloss.Width(centerLabel)
		if available < 0 {
			available = 0
		}
		half := available / 2
		if leftLabelWidth > half && rightLabelWidth > half {
			leftLabelWidth = half
			rightLabelWidth = available - half
		} else if leftLabelWidth > half {
			leftLabelWidth = available - rightLabelWidth
		} else {
			rightLabelWidth = available - leftLabelWidth
		}
		if leftLabelWidth < 0 {
			leftLabelWidth = 0
		}
		if rightLabelWidth < 0 {
			rightLabelWidth = 0
		}
	}

	connectorStyle := lipgloss.NewStyle().
		Foreground(graph.theme.BorderColor)

	// Compute the X positions of the label regions for click targets.
	// Left labels occupy columns [0, leftLabelWidth).
	// Right labels start after: left area + connectors + center + connectors.
	leftEndX := leftLabelWidth
	rightStartX := leftLabelWidth
	if len(leftNodes) > 0 {
		rightStartX += 3 // " ─" + merge char
	}
	centerAreaWidth := lipgloss.Width(centerLabel)
	if len(leftNodes) > 0 {
		centerAreaWidth += 2 // "─ " left arm
	}
	if len(rightNodes) > 0 {
		centerAreaWidth += 2 // " ─" right arm
	}
	rightStartX += centerAreaWidth
	if len(rightNodes) > 0 {
		rightStartX += 3 // split char + "─ "
	}
	rightEndX := rightStartX + rightLabelWidth

	var rows []string
	var targets []BodyClickTarget

	for row := 0; row < totalRows; row++ {
		var parts []string

		// Left label.
		leftIndex := row - leftStart
		hasLeftNode := leftIndex >= 0 && leftIndex < len(leftNodes)
		if len(leftNodes) > 0 {
			if hasLeftNode {
				label := graph.renderLabel(leftNodes[leftIndex], leftLabelWidth)
				parts = append(parts, label)
			} else {
				parts = append(parts, strings.Repeat(" ", leftLabelWidth))
			}
		}

		// Left connector + merge column.
		if len(leftNodes) > 0 {
			if hasLeftNode {
				parts = append(parts, connectorStyle.Render(" ─"))
			} else {
				parts = append(parts, "  ")
			}

			mergeChar := graph.mergeChar(row, leftStart, leftStart+len(leftNodes)-1, centerRow)
			parts = append(parts, connectorStyle.Render(string(mergeChar)))
		}

		// Center area.
		if row == centerRow {
			leftArm := ""
			rightArm := ""
			if len(leftNodes) > 0 {
				leftArm = connectorStyle.Render("─ ")
			}
			if len(rightNodes) > 0 {
				rightArm = connectorStyle.Render(" ─")
			}
			centerStyle := lipgloss.NewStyle().
				Bold(true).
				Foreground(graph.theme.HeaderForeground)
			parts = append(parts, leftArm+centerStyle.Render(centerLabel)+rightArm)
		} else {
			// Blank center: spaces matching the center label + arms.
			parts = append(parts, strings.Repeat(" ", centerAreaWidth))
		}

		// Split column + right connector.
		if len(rightNodes) > 0 {
			splitChar := graph.splitChar(row, rightStart, rightStart+len(rightNodes)-1, centerRow)
			parts = append(parts, connectorStyle.Render(string(splitChar)))

			rightIndex := row - rightStart
			hasRightNode := rightIndex >= 0 && rightIndex < len(rightNodes)
			if hasRightNode {
				parts = append(parts, connectorStyle.Render("─ "))
			} else {
				parts = append(parts, "  ")
			}

			// Right label.
			if hasRightNode {
				label := graph.renderLabel(rightNodes[rightIndex], rightLabelWidth)
				parts = append(parts, label)
			}
		}

		rowString := strings.Join(parts, "")
		rows = append(rows, rowString)

		// Click targets for left nodes.
		if hasLeftNode && leftNodes[leftIndex].exists {
			targets = append(targets, BodyClickTarget{
				Line:     row,
				TicketID: leftNodes[leftIndex].ticketID,
				StartX:   0,
				EndX:     leftEndX,
			})
		}

		// Click targets for right nodes.
		rightIndex := row - rightStart
		if rightIndex >= 0 && rightIndex < len(rightNodes) && rightNodes[rightIndex].exists {
			targets = append(targets, BodyClickTarget{
				Line:     row,
				TicketID: rightNodes[rightIndex].ticketID,
				StartX:   rightStartX,
				EndX:     rightEndX,
			})
		}
	}

	return strings.Join(rows, "\n"), targets
}

// resolveNodes looks up each ticket ID in the source and returns
// depNode values with resolved content.
func (graph DependencyGraph) resolveNodes(ticketIDs []string, source Source) []depNode {
	nodes := make([]depNode, len(ticketIDs))
	for index, ticketID := range ticketIDs {
		content, exists := source.Get(ticketID)
		nodes[index] = depNode{
			ticketID: ticketID,
			content:  content,
			exists:   exists,
		}
	}
	return nodes
}

// maxLabelWidth returns the maximum visual width among the rendered
// labels for a set of nodes.
func (graph DependencyGraph) maxLabelWidth(nodes []depNode) int {
	maxWidth := 0
	for _, node := range nodes {
		width := graph.labelWidth(node)
		if width > maxWidth {
			maxWidth = width
		}
	}
	return maxWidth
}

// labelWidth returns the visual width of a node's label (icon + ID).
func (graph DependencyGraph) labelWidth(node depNode) int {
	if !node.exists {
		return lipgloss.Width(node.ticketID + "?")
	}
	icon := statusIconString(node.content.Status)
	if icon == "" {
		icon = " "
	}
	return lipgloss.Width(icon + " " + node.ticketID)
}

// renderLabel renders a node's label (status icon + ticket ID),
// padded or truncated to the given width.
func (graph DependencyGraph) renderLabel(node depNode, targetWidth int) string {
	if !node.exists {
		text := node.ticketID + "?"
		if lipgloss.Width(text) > targetWidth {
			text = truncateString(text, targetWidth-1) + "…"
		}
		style := lipgloss.NewStyle().
			Foreground(graph.theme.FaintText).
			Width(targetWidth)
		return style.Render(text)
	}

	icon := statusIconString(node.content.Status)
	if icon == "" {
		icon = " "
	}

	statusStyle := lipgloss.NewStyle().
		Foreground(graph.theme.StatusColor(node.content.Status))
	idStyle := lipgloss.NewStyle().
		Foreground(graph.theme.StatusColor(node.content.Status))

	// When the target width is too narrow for icon + ID, truncate the
	// ID so the label fits. This happens when the graph must shrink
	// side labels to fit the terminal width.
	visibleID := node.ticketID
	iconWidth := lipgloss.Width(icon)
	separatorWidth := 1 // Space between icon and ID.
	availableForID := targetWidth - iconWidth - separatorWidth
	if availableForID > 0 && lipgloss.Width(visibleID) > availableForID {
		visibleID = truncateString(visibleID, availableForID-1) + "…"
	} else if availableForID <= 0 {
		visibleID = ""
	}

	label := statusStyle.Render(icon) + " " + idStyle.Render(visibleID)
	labelVisualWidth := lipgloss.Width(label)

	if labelVisualWidth < targetWidth {
		label += strings.Repeat(" ", targetWidth-labelVisualWidth)
	}
	return label
}

// mergeChar returns the box-drawing character for the left merge
// column at the given row. The merge bar spans from fanStart to
// fanEnd, and the horizontal line exits rightward on centerRow.
func (graph DependencyGraph) mergeChar(row, fanStart, fanEnd, centerRow int) rune {
	if row < fanStart || row > fanEnd {
		return ' '
	}
	// Single node — straight horizontal line.
	if fanStart == fanEnd {
		return '─'
	}

	hasUp := row > fanStart
	hasDown := row < fanEnd
	hasLeft := row >= fanStart && row <= fanEnd // Node connects from left.
	hasRight := row == centerRow                // Center line exits right.

	return boxDrawing(hasLeft, hasRight, hasUp, hasDown)
}

// splitChar returns the box-drawing character for the right split
// column at the given row. The split bar spans from fanStart to
// fanEnd, and the horizontal line enters from the left on centerRow.
func (graph DependencyGraph) splitChar(row, fanStart, fanEnd, centerRow int) rune {
	if row < fanStart || row > fanEnd {
		return ' '
	}
	// Single node — straight horizontal line.
	if fanStart == fanEnd {
		return '─'
	}

	hasUp := row > fanStart
	hasDown := row < fanEnd
	hasRight := row >= fanStart && row <= fanEnd // Node connects to right.
	hasLeft := row == centerRow                  // Center line enters from left.

	return boxDrawing(hasLeft, hasRight, hasUp, hasDown)
}

// boxDrawing returns the Unicode box-drawing light character that
// connects in the specified directions.
func boxDrawing(left, right, up, down bool) rune {
	switch {
	case left && right && up && down:
		return '┼'
	case left && right && up:
		return '┴'
	case left && right && down:
		return '┬'
	case left && up && down:
		return '┤'
	case right && up && down:
		return '├'
	case left && right:
		return '─'
	case up && down:
		return '│'
	case left && down:
		return '┐'
	case left && up:
		return '┘'
	case right && down:
		return '┌'
	case right && up:
		return '└'
	case left:
		return '─'
	case right:
		return '─'
	case up, down:
		return '│'
	default:
		return ' '
	}
}
