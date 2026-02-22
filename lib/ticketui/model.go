// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package ticketui

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/bureau-foundation/bureau/lib/ticketindex"
)

// Tab identifies which data view is active.
type Tab int

const (
	// TabReady shows tickets available for work, grouped by parent epic.
	TabReady Tab = iota
	// TabBlocked shows tickets with unsatisfied dependencies.
	TabBlocked
	// TabAll shows every ticket regardless of status.
	TabAll
)

// FocusRegion identifies which pane has keyboard focus.
type FocusRegion int

const (
	// FocusList means navigation keys move the ticket list cursor.
	FocusList FocusRegion = iota
	// FocusDetail means navigation keys scroll the detail viewport.
	FocusDetail
	// FocusFilter means keystrokes go to the filter input.
	FocusFilter
	// FocusDetailSearch means keystrokes go to the detail pane
	// search input (activated by / when the detail pane has focus).
	FocusDetailSearch
	// FocusDropdown means a dropdown overlay (status or priority)
	// is active. All keyboard input routes to the dropdown until
	// the user selects an option or dismisses it.
	FocusDropdown
	// FocusTitleEdit means the title in the detail header is being
	// edited inline. Character input modifies the title buffer,
	// enter submits, escape cancels.
	FocusTitleEdit
	// FocusNoteModal means the note input modal is active. All
	// keyboard input routes to the textarea inside the modal.
	// Ctrl+D submits, Escape cancels.
	FocusNoteModal
)

// Split ratio bounds and step size.
const (
	splitRatioMin  = 0.20
	splitRatioMax  = 0.80
	splitRatioStep = 0.05

	// doubleClickThreshold is the maximum interval between two clicks
	// on the splitter divider to count as a double-click.
	doubleClickThreshold = 400 * time.Millisecond

	// ungroupedEpicID is a sentinel used as the EpicID for the
	// "ungrouped" header so it participates in collapse/expand like
	// real epic groups.
	ungroupedEpicID = "_ungrouped"
)

// sourceEventMsg wraps a Source Event for delivery through the
// bubbletea message loop.
type sourceEventMsg struct {
	event Event
}

// heatTickMsg is sent periodically to drive the heat decay animation.
// While any tickets are hot, a new tick is scheduled after each one.
type heatTickMsg struct{}

// clipboardFadeMsg is sent after a short delay to clear the clipboard
// feedback notice from the status bar.
type clipboardFadeMsg struct{}

// mutationResultMsg is sent when an asynchronous mutation call completes.
// On success, err is nil and the subscribe stream delivers the update.
// On error, err is displayed in the status bar.
type mutationResultMsg struct {
	err error
}

// mutationErrorFadeMsg is sent after a delay to clear the mutation
// error notice from the status bar.
type mutationErrorFadeMsg struct{}

// clipboardFadeDelay is how long the "Copied" notice stays visible.
const clipboardFadeDelay = 2 * time.Second

// copyToClipboard writes text to the system clipboard via the OSC 52
// terminal escape sequence. Writes directly to /dev/tty to bypass
// bubbletea's managed output — OSC 52 is invisible (no screen effect)
// so it's safe to write alongside the TUI renderer.
//
// Uses BEL (\x07) as the OSC terminator rather than ST (\x1b\\)
// because BEL is a single byte that survives intact through layered
// terminal environments (SSH, tmux, screen). ST's two-byte escape
// can be misinterpreted by intermediate layers.
//
// When tmux is detected (via $TMUX or $TERM prefix), sends the OSC 52
// both via tmux DCS passthrough (for allow-passthrough configurations)
// and directly (for set-clipboard configurations). This covers both
// tmux forwarding modes; duplicate clipboard sets are harmless.
//
// After a short delay, sends clipboardFadeMsg to clear the UI notice.
func copyToClipboard(text string) tea.Cmd {
	return tea.Batch(
		func() tea.Msg {
			tty, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
			if err != nil {
				return nil
			}
			defer tty.Close()

			encoded := base64.StdEncoding.EncodeToString([]byte(text))
			osc52 := fmt.Sprintf("\x1b]52;c;%s\x07", encoded)

			// Detect tmux: TMUX env var (local tmux), or TERM prefix
			// (forwarded through SSH from a local tmux session).
			inTmux := os.Getenv("TMUX") != "" ||
				strings.HasPrefix(os.Getenv("TERM"), "tmux") ||
				strings.HasPrefix(os.Getenv("TERM"), "screen")

			if inTmux {
				// tmux DCS passthrough: escapes are doubled inside
				// the DCS wrapper. Uses BEL as OSC terminator to
				// avoid double-escaping ST. Requires tmux
				// allow-passthrough on.
				fmt.Fprintf(tty, "\x1bPtmux;\x1b%s\x1b\\", osc52)
			}

			// Direct OSC 52: works without tmux, or with tmux
			// set-clipboard on/external (tmux intercepts and forwards).
			tty.WriteString(osc52)
			return nil
		},
		tea.Tick(clipboardFadeDelay, func(time.Time) tea.Msg {
			return clipboardFadeMsg{}
		}),
	)
}

// ListItem is a single row in the rendered list. It is either a group
// header (for the ready tab's epic grouping) or a ticket entry.
type ListItem struct {
	// IsHeader is true for epic group headers and false for ticket rows.
	IsHeader bool

	// For headers: the epic ticket ID.
	EpicID string
	// For headers: the epic title and progress.
	EpicTitle string
	EpicDone  int
	EpicTotal int
	Collapsed bool
	// For headers: health metrics (nil on non-ready tabs or ungrouped).
	EpicHealth *ticketindex.EpicHealthStats

	// For ticket rows: the ticket entry.
	Entry ticketindex.Entry
}

// navPosition records the viewer state at a point in time so
// back/forward navigation can restore the previous view.
type navPosition struct {
	tab          Tab
	ticketID     string
	scrollOffset int
}

// Model is the top-level bubbletea model for the ticket viewer TUI.
type Model struct {
	source Source
	theme  Theme
	keys   KeyMap

	// Terminal dimensions (set by WindowSizeMsg).
	width  int
	height int
	ready  bool

	// Active tab and filter.
	activeTab Tab
	filter    FilterModel

	// List state. items is the displayed list (may include group headers
	// for the ready tab). entries is the underlying ticket data before
	// grouping. stats comes from the source snapshot.
	entries      []ticketindex.Entry
	items        []ListItem
	stats        ticketindex.Stats
	cursor       int
	scrollOffset int
	selectedID   string // Stable focus: track selection by ticket ID.

	// Two-pane layout.
	focusRegion          FocusRegion
	priorFocus           FocusRegion // Saved focus when entering filter mode.
	splitRatio           float64     // Fraction of width for the list pane.
	detailPane           DetailPane  // Right pane: scrollable ticket detail.
	collapsedEpics       map[string]bool
	draggingSplitter     bool      // True while the user is dragging the divider.
	draggingListScroll   bool      // True while dragging the list scrollbar thumb.
	draggingDetailScroll bool      // True while dragging the detail scrollbar thumb.
	lastSplitterClick    time.Time // For double-click detection on the divider.

	// Tab bar click regions. Each entry maps a tab to its X range in
	// the header line so mouse clicks on Y=0 can switch tabs.
	tabHitRanges []tabHitRange

	// Navigation history. Populated by navigateToTicket so that
	// back/forward (backspace, mouse4/mouse5) can restore the view.
	navHistory []navPosition // Back stack.
	navForward []navPosition // Forward stack (cleared on new navigation).

	// Filter match highlighting: maps ticket ID to matched rune
	// positions in the title. Populated by applyFilter when the
	// filter uses fuzzy matching; nil when no filter is active.
	filterHighlights map[string][]int

	// Hover tooltip: shown when the mouse rests on a clickable ticket
	// reference in the detail pane (graph nodes, dependency entries,
	// autolinked inline references). Nil when no tooltip is visible.
	tooltip *tooltipState

	// Clipboard feedback: set when a right-click copies a ticket ID
	// to the system clipboard via OSC 52. Cleared after a short delay
	// so the "Copied" indicator disappears.
	clipboardNotice string

	// Mutation state. Active when the source implements Mutator.
	activeDropdown  *DropdownOverlay // Non-nil when a dropdown overlay is visible.
	operatorUserID  string           // Matrix user ID for auto-assignment on in_progress transitions.
	mutationError   string           // Briefly displayed in the status bar after a failed mutation.
	titleEditBuffer []rune           // Rune buffer for inline title editing; nil when not editing.
	titleEditCursor int              // Cursor position within titleEditBuffer.
	titleEditID     string           // Ticket ID being title-edited.
	noteModal       *NoteModal       // Non-nil when the note input modal is visible.

	// Live update animation.
	heatTracker  *HeatTracker // Tracks recently-changed tickets for glow animation.
	eventChannel <-chan Event // Source event subscription; nil if no live updates.
	tickRunning  bool         // True when the heat animation tick timer is active.
}

// tabHitRange maps a horizontal span in the header to a tab.
type tabHitRange struct {
	startX int // Inclusive.
	endX   int // Exclusive.
	tab    Tab
}

// NewModel creates a Model connected to the given ticket data source.
// Loads the Ready view by default (the primary use case).
func NewModel(source Source) Model {
	snapshot := source.Ready()

	model := Model{
		source:         source,
		theme:          DefaultTheme,
		keys:           DefaultKeyMap,
		activeTab:      TabReady,
		entries:        snapshot.Entries,
		stats:          snapshot.Stats,
		splitRatio:     0.50,
		detailPane:     NewDetailPane(DefaultTheme),
		collapsedEpics: make(map[string]bool),
		heatTracker:    NewHeatTracker(),
		eventChannel:   source.Subscribe(),
	}

	model.rebuildItems()

	// Initialize stable focus on the first selectable item.
	if len(model.items) > 0 {
		model.cursor = 0
		if model.cursor < len(model.items) && !model.items[model.cursor].IsHeader {
			model.selectedID = model.items[model.cursor].Entry.ID
		}
	}

	return model
}

// SetOperatorID sets the Matrix user ID of the current operator. Used
// for auto-assignment when transitioning tickets to in_progress via
// the status dropdown. Call this after NewModel and before running the
// bubbletea program. Only meaningful when the source implements Mutator.
func (model *Model) SetOperatorID(userID string) {
	model.operatorUserID = userID
}

// Init implements tea.Model. Starts listening for source events if the
// event channel is available (set up in NewModel).
func (model Model) Init() tea.Cmd {
	if model.eventChannel == nil {
		return nil
	}
	return listenForSourceEvent(model.eventChannel)
}

// listenForSourceEvent returns a tea.Cmd that blocks until an event
// arrives on the source channel, then delivers it as a sourceEventMsg.
func listenForSourceEvent(channel <-chan Event) tea.Cmd {
	return func() tea.Msg {
		event, ok := <-channel
		if !ok {
			return nil
		}
		return sourceEventMsg{event: event}
	}
}

// Update implements tea.Model. Routes keyboard events based on the
// current focus region and handles layout changes.
func (model Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.KeyMsg:
		// Any keyboard input dismisses the hover tooltip.
		model.tooltip = nil

		// When filter is active, route all input to the filter first,
		// except for Esc (clear) and Enter (confirm and return to list).
		if model.focusRegion == FocusFilter {
			return model.handleFilterKeys(message)
		}
		// When detail search is active, route all input to the search.
		if model.focusRegion == FocusDetailSearch {
			return model.handleDetailSearchKeys(message)
		}
		// When a dropdown overlay is active, route all input to it.
		if model.focusRegion == FocusDropdown {
			return model.handleDropdownKeys(message)
		}
		// When title is being edited inline, route all input to the editor.
		if model.focusRegion == FocusTitleEdit {
			return model.handleTitleEditKeys(message)
		}
		// When the note modal is active, route all input to it.
		if model.focusRegion == FocusNoteModal {
			return model.handleNoteModalKeys(message)
		}

		switch {
		case key.Matches(message, model.keys.Quit):
			return model, tea.Quit

		case key.Matches(message, model.keys.FocusToggle):
			if model.focusRegion == FocusList {
				model.focusRegion = FocusDetail
			} else {
				model.focusRegion = FocusList
			}

		case key.Matches(message, model.keys.SplitGrow):
			model.splitRatio += splitRatioStep
			if model.splitRatio > splitRatioMax {
				model.splitRatio = splitRatioMax
			}
			model.updatePaneSizes()

		case key.Matches(message, model.keys.SplitShrink):
			model.splitRatio -= splitRatioStep
			if model.splitRatio < splitRatioMin {
				model.splitRatio = splitRatioMin
			}
			model.updatePaneSizes()

		case key.Matches(message, model.keys.TabReady):
			model.switchTab(TabReady)

		case key.Matches(message, model.keys.TabBlocked):
			model.switchTab(TabBlocked)

		case key.Matches(message, model.keys.TabAll):
			model.switchTab(TabAll)

		case key.Matches(message, model.keys.FilterActivate):
			if model.focusRegion == FocusDetail {
				// In detail pane: activate detail search.
				model.focusRegion = FocusDetailSearch
				model.detailPane.search.Active = true
				model.updatePaneSizes()
			} else {
				// In list pane (or anywhere else): activate list filter.
				model.priorFocus = model.focusRegion
				model.focusRegion = FocusFilter
				model.filter.Active = true
				// Reset list position to the top so the user sees
				// results from the beginning as they type.
				model.cursor = 0
				model.scrollOffset = 0
				model.updatePaneSizes()
			}

		case key.Matches(message, model.keys.NavigateBack):
			model.navigateBack()

		case key.Matches(message, model.keys.FilterClear):
			if model.focusRegion == FocusDetail && model.detailPane.search.Input != "" {
				model.detailPane.search.Clear()
				model.detailPane.applySearchHighlighting()
				model.updatePaneSizes()
			} else if model.filter.Input != "" {
				model.filter.Clear()
				model.refreshFromSource()
			}

		default:
			if model.focusRegion == FocusList {
				model.handleListKeys(message)
			} else {
				model.handleDetailKeys(message)
			}
		}

	case tea.MouseMsg:
		if cmd := model.handleMouse(message); cmd != nil {
			return model, cmd
		}

	case clipboardFadeMsg:
		model.clipboardNotice = ""

	case dropdownSelectMsg:
		return model.handleDropdownSelect(message)

	case mutationResultMsg:
		if message.err != nil {
			model.mutationError = message.err.Error()
			return model, tea.Tick(clipboardFadeDelay, func(time.Time) tea.Msg {
				return mutationErrorFadeMsg{}
			})
		}

	case mutationErrorFadeMsg:
		model.mutationError = ""

	case tea.WindowSizeMsg:
		model.width = message.Width
		model.height = message.Height
		model.ready = true
		model.updatePaneSizes()
		model.computeTabHitRanges()
		model.syncDetailPane()

	case sourceEventMsg:
		return model.handleSourceEvent(message)

	case heatTickMsg:
		return model.handleHeatTick()
	}
	return model, nil
}

// contentStartY returns the Y coordinate where the content area
// begins. The top chrome line is always exactly 1 row: either the
// tab bar (normal) or the filter bar (when filter is active). The
// filter bar replaces the tab bar rather than pushing content down.
func (model Model) contentStartY() int {
	return 1
}

// handleMouse routes mouse events to the appropriate pane based on the
// mouse position. Scroll wheel scrolls whichever pane the cursor is
// over. Clicks in the list select the clicked row. Dragging the
// divider resizes the split. Scrollbar clicks and drags scroll the
// corresponding pane. Right-click on a hoverable ticket reference
// copies the ticket ID to the system clipboard.
func (model *Model) handleMouse(message tea.MouseMsg) tea.Cmd {
	listWidth := model.listWidth()
	contentStart := model.contentStartY()
	listScrollX := listWidth - 1     // Right edge of the list pane.
	dividerX := listWidth            // The divider column.
	detailScrollX := model.width - 1 // Rightmost column.

	inContentArea := message.Y >= contentStart && message.Y < model.height-2
	inListPane := message.X >= 0 && message.X < listScrollX
	onListScroll := message.X == listScrollX
	onDivider := message.X == dividerX
	inDetailPane := message.X > dividerX && message.X < detailScrollX
	onDetailScroll := message.X == detailScrollX

	// Motion events with no button held: update hover tooltip.
	// This fires before drag handling because drags also produce
	// motion events (with a button held), and those should continue
	// to be handled by the drag logic below.
	if message.Action == tea.MouseActionMotion && message.Button == tea.MouseButtonNone {
		model.updateHoverTooltip(message, inDetailPane && inContentArea)
		return nil
	}

	// Any non-motion interaction dismisses the tooltip, dropdown,
	// and title edit.
	if model.tooltip != nil {
		model.tooltip = nil
	}
	if model.activeDropdown != nil {
		model.dismissDropdown()
	}
	if model.focusRegion == FocusTitleEdit {
		model.cancelTitleEdit()
	}
	if model.noteModal != nil {
		model.noteModal = nil
		model.focusRegion = FocusDetail
	}

	// Handle active drags — motion updates position, release ends drag.
	if model.draggingSplitter || model.draggingListScroll || model.draggingDetailScroll {
		if message.Action == tea.MouseActionRelease {
			model.draggingSplitter = false
			model.draggingListScroll = false
			model.draggingDetailScroll = false
			return nil
		}
		switch {
		case model.draggingSplitter:
			model.setSplitFromMouseX(message.X)
		case model.draggingListScroll:
			model.scrollListToY(message.Y - contentStart)
		case model.draggingDetailScroll:
			model.scrollDetailToY(message.Y - contentStart)
		}
		return nil
	}

	switch message.Button {
	case tea.MouseButtonWheelUp:
		if !inContentArea {
			return nil
		}
		if inListPane || onListScroll || onDivider {
			model.scrollListUp(1)
		} else if inDetailPane || onDetailScroll {
			model.detailPane.viewport.LineUp(3)
		}

	case tea.MouseButtonWheelDown:
		if !inContentArea {
			return nil
		}
		if inListPane || onListScroll || onDivider {
			model.scrollListDown(1)
		} else if inDetailPane || onDetailScroll {
			model.detailPane.viewport.LineDown(3)
		}

	case tea.MouseButtonLeft:
		if message.Action != tea.MouseActionPress {
			return nil
		}
		// Tab clicks: header row (Y=0) maps X to tab labels.
		if message.Y == 0 {
			for _, hit := range model.tabHitRanges {
				if message.X >= hit.startX && message.X < hit.endX {
					model.switchTab(hit.tab)
					return nil
				}
			}
			return nil
		}
		if !inContentArea {
			return nil
		}
		// Scrollbar clicks: jump to position and start drag tracking.
		if onListScroll {
			model.focusRegion = FocusList
			model.draggingListScroll = true
			model.scrollListToY(message.Y - contentStart)
			return nil
		}
		if onDetailScroll {
			model.focusRegion = FocusDetail
			model.draggingDetailScroll = true
			model.scrollDetailToY(message.Y - contentStart)
			return nil
		}
		if onDivider {
			now := time.Now()
			if now.Sub(model.lastSplitterClick) <= doubleClickThreshold {
				// Double-click: toggle the split. If the list has
				// more than half the width, collapse it to 1/4;
				// otherwise expand it to 3/4.
				if model.splitRatio > 0.50 {
					model.splitRatio = 0.25
				} else {
					model.splitRatio = 0.75
				}
				model.updatePaneSizes()
				model.lastSplitterClick = time.Time{} // Reset to prevent triple-click toggling.
				return nil
			}
			model.lastSplitterClick = now
			model.draggingSplitter = true
			return nil
		}
		if inListPane {
			model.handleListClick(message.Y - contentStart)
		} else if inDetailPane {
			model.focusRegion = FocusDetail
			// X relative to the detail content area: subtract
			// list pane, divider, and left padding.
			contentRelativeX := message.X - model.listWidth() - 1 - 1

			// Check for header click targets (status, priority, title)
			// in the fixed header area above the scrollable body.
			headerRelativeY := message.Y - contentStart
			if headerRelativeY >= 0 && headerRelativeY < detailHeaderLines {
				field := model.detailPane.HeaderTarget(headerRelativeY, contentRelativeX)
				if field != "" {
					if cmd := model.handleHeaderClick(field, message.X, message.Y); cmd != nil {
						return cmd
					}
					return nil
				}
			}

			// Check for clickable dependency/child/parent entries
			// in the detail body area.
			bodyRelativeY := message.Y - contentStart - detailHeaderLines
			if bodyRelativeY >= 0 && bodyRelativeY < model.detailPane.viewport.Height {
				ticketID := model.detailPane.ClickTarget(bodyRelativeY, contentRelativeX)
				if ticketID != "" {
					model.navigateToTicket(ticketID)
				}
			}
		}

	case tea.MouseButtonRight:
		if message.Action != tea.MouseActionRelease {
			return nil
		}
		// Right-click on a hoverable ticket reference copies the
		// ticket ID to the system clipboard via OSC 52.
		if !inDetailPane || !inContentArea {
			return nil
		}
		bodyRelativeY := message.Y - contentStart - detailHeaderLines
		contentRelativeX := message.X - model.listWidth() - 1 - 1
		if bodyRelativeY >= 0 && bodyRelativeY < model.detailPane.viewport.Height {
			ticketID := model.detailPane.ClickTarget(bodyRelativeY, contentRelativeX)
			if ticketID != "" {
				model.clipboardNotice = ticketID
				return copyToClipboard(ticketID)
			}
		}

	case tea.MouseButtonBackward:
		if message.Action == tea.MouseActionPress {
			model.navigateBack()
		}

	case tea.MouseButtonForward:
		if message.Action == tea.MouseActionPress {
			model.navigateForward()
		}
	}
	return nil
}

// scrollListToY sets the list scroll offset based on a Y position
// within the content area (0 = top of content). Maps the Y coordinate
// proportionally to the full item range.
func (model *Model) scrollListToY(relativeY int) {
	visible := model.visibleHeight()
	totalItems := len(model.items)
	if totalItems <= visible || visible <= 0 {
		return
	}

	// Map Y position to scroll offset.
	maxOffset := totalItems - visible
	offset := 0
	if visible > 0 {
		offset = relativeY * maxOffset / (visible - 1)
	}
	if offset < 0 {
		offset = 0
	}
	if offset > maxOffset {
		offset = maxOffset
	}
	model.scrollOffset = offset

	// Move cursor into the visible range.
	if model.cursor < model.scrollOffset {
		model.cursor = model.scrollOffset
	}
	if model.cursor >= model.scrollOffset+visible {
		model.cursor = model.scrollOffset + visible - 1
	}
	model.syncDetailPane()
}

// scrollDetailToY sets the detail viewport scroll based on a Y
// position within the content area.
func (model *Model) scrollDetailToY(relativeY int) {
	totalLines := model.detailPane.viewport.TotalLineCount()
	visible := model.detailPane.viewport.Height
	if totalLines <= visible || visible <= 0 {
		return
	}

	// The scrollbar spans the full pane height but the viewport body
	// starts below the fixed header. Remap clicks in the header zone
	// to the body region.
	bodyRelativeY := relativeY - detailHeaderLines
	if bodyRelativeY < 0 {
		bodyRelativeY = 0
	}

	maxOffset := totalLines - visible
	offset := 0
	if visible > 1 {
		offset = bodyRelativeY * maxOffset / (visible - 1)
	}
	if offset < 0 {
		offset = 0
	}
	if offset > maxOffset {
		offset = maxOffset
	}
	model.detailPane.viewport.SetYOffset(offset)
}

// updateHoverTooltip checks whether the mouse is hovering over a
// clickable ticket reference in the detail pane body and updates
// the tooltip state accordingly. Called on motion events with no
// button held. The inDetailBody parameter indicates whether the
// mouse position is within the detail pane's content area.
func (model *Model) updateHoverTooltip(message tea.MouseMsg, inDetailBody bool) {
	if !inDetailBody {
		model.tooltip = nil
		return
	}

	contentStart := model.contentStartY()
	bodyRelativeY := message.Y - contentStart - detailHeaderLines
	contentRelativeX := message.X - model.listWidth() - 1 - 1

	if bodyRelativeY < 0 || bodyRelativeY >= model.detailPane.viewport.Height {
		model.tooltip = nil
		return
	}

	target := model.detailPane.HoverTarget(bodyRelativeY, contentRelativeX)
	if target == nil {
		model.tooltip = nil
		return
	}

	// Don't re-lookup if already showing tooltip for this ticket.
	if model.tooltip != nil && model.tooltip.ticketID == target.TicketID {
		return
	}

	content, exists := model.source.Get(target.TicketID)
	if !exists {
		model.tooltip = nil
		return
	}

	// Render tooltip to determine its dimensions for positioning.
	tooltipLines := renderTooltip(target.TicketID, content, model.theme, tooltipMaxWidth)
	tooltipHeight := len(tooltipLines)
	tooltipWidth := 0
	if len(tooltipLines) > 0 {
		tooltipWidth = ansi.StringWidth(tooltipLines[0])
	}

	// Position: above the hover target if room, otherwise below.
	anchorY := message.Y - tooltipHeight - 1
	if anchorY < 0 {
		anchorY = message.Y + 1
	}

	// Horizontal: aligned to the mouse X, clamped to screen bounds.
	anchorX := message.X
	if anchorX+tooltipWidth > model.width {
		anchorX = model.width - tooltipWidth
	}
	if anchorX < 0 {
		anchorX = 0
	}

	// Compute screen coordinates of the hovered target for bold highlighting.
	// The detail pane content starts after: list pane + divider + left padding.
	detailContentStartX := model.listWidth() + 1 + 1
	hoverScreenY := message.Y
	hoverScreenX0 := detailContentStartX + target.StartX
	hoverScreenX1 := detailContentStartX + target.EndX

	model.tooltip = &tooltipState{
		ticketID:      target.TicketID,
		content:       content,
		anchorX:       anchorX,
		anchorY:       anchorY,
		hoverScreenY:  hoverScreenY,
		hoverScreenX0: hoverScreenX0,
		hoverScreenX1: hoverScreenX1,
	}
}

// setSplitFromMouseX updates the split ratio based on the mouse X
// coordinate, clamped to the configured min/max bounds.
func (model *Model) setSplitFromMouseX(mouseX int) {
	if model.width <= 0 {
		return
	}
	ratio := float64(mouseX) / float64(model.width)
	if ratio < splitRatioMin {
		ratio = splitRatioMin
	}
	if ratio > splitRatioMax {
		ratio = splitRatioMax
	}
	model.splitRatio = ratio
	model.updatePaneSizes()
}

// scrollListUp moves the cursor up by count items, skipping headers.
func (model *Model) scrollListUp(count int) {
	for step := 0; step < count; step++ {
		model.moveCursorUp()
	}
	model.ensureCursorVisible()
	model.syncDetailPane()
}

// scrollListDown moves the cursor down by count items, skipping headers.
func (model *Model) scrollListDown(count int) {
	for step := 0; step < count; step++ {
		model.moveCursorDown()
	}
	model.ensureCursorVisible()
	model.syncDetailPane()
}

// handleListClick processes a mouse click at the given row offset
// within the content area. Selects the clicked item, or toggles
// collapse on headers.
func (model *Model) handleListClick(rowOffset int) {
	// Clicking anywhere in the list pane focuses it, even on empty
	// space below the last item.
	model.focusRegion = FocusList

	itemIndex := model.scrollOffset + rowOffset
	if itemIndex < 0 || itemIndex >= len(model.items) {
		return
	}

	item := model.items[itemIndex]
	if item.IsHeader {
		// Click on header toggles collapse.
		if item.EpicID != "" {
			model.collapsedEpics[item.EpicID] = !model.collapsedEpics[item.EpicID]
			model.rebuildItems()
			model.restoreSelection()
		}
		return
	}

	// Click on a ticket row selects it.
	model.cursor = itemIndex
	model.syncDetailPane()
}

// navigateToTicket selects the given ticket in the list. If the ticket
// is visible on the current tab, the cursor moves to it directly. If
// not, switches to the All tab (which contains every ticket) and
// selects it there. Does nothing if the ticket is not in the index.
//
// Pushes the current position onto the navigation history so
// back (backspace / mouse4) can return to where the user was.
func (model *Model) navigateToTicket(ticketID string) {
	// Save current position for back navigation.
	model.navHistory = append(model.navHistory, navPosition{
		tab:          model.activeTab,
		ticketID:     model.selectedID,
		scrollOffset: model.scrollOffset,
	})
	// New navigation clears the forward stack.
	model.navForward = model.navForward[:0]

	// Check the current items list first.
	for index, item := range model.items {
		if !item.IsHeader && item.Entry.ID == ticketID {
			model.cursor = index
			model.selectedID = ticketID
			model.focusRegion = FocusList
			model.ensureCursorVisible()
			model.syncDetailPane()
			return
		}
	}

	// Not on current tab — switch to All which includes every ticket.
	// Set selectedID before refreshing so restoreSelection finds it.
	model.selectedID = ticketID
	model.activeTab = TabAll
	model.filter.Clear()
	model.refreshFromSource()
	model.focusRegion = FocusList
}

// navigateBack restores the previous position from the navigation
// history stack. The current position is pushed onto the forward
// stack so navigateForward can return to it. Does nothing if there
// is no history.
func (model *Model) navigateBack() {
	if len(model.navHistory) == 0 {
		return
	}

	// Save current position to forward stack.
	model.navForward = append(model.navForward, navPosition{
		tab:          model.activeTab,
		ticketID:     model.selectedID,
		scrollOffset: model.scrollOffset,
	})

	// Pop from history.
	position := model.navHistory[len(model.navHistory)-1]
	model.navHistory = model.navHistory[:len(model.navHistory)-1]

	model.restoreNavPosition(position)
}

// navigateForward restores the next position from the forward stack
// (populated by navigateBack). The current position is pushed onto
// the history stack. Does nothing if there is no forward history.
func (model *Model) navigateForward() {
	if len(model.navForward) == 0 {
		return
	}

	// Save current position to history stack.
	model.navHistory = append(model.navHistory, navPosition{
		tab:          model.activeTab,
		ticketID:     model.selectedID,
		scrollOffset: model.scrollOffset,
	})

	// Pop from forward.
	position := model.navForward[len(model.navForward)-1]
	model.navForward = model.navForward[:len(model.navForward)-1]

	model.restoreNavPosition(position)
}

// restoreNavPosition switches to the tab and ticket recorded in the
// given position, restoring the scroll offset as closely as possible.
func (model *Model) restoreNavPosition(position navPosition) {
	model.selectedID = position.ticketID
	model.activeTab = position.tab
	model.filter.Clear()
	model.refreshFromSource()

	// Override the scroll position that refreshFromSource computed
	// with the saved one, then re-clamp to keep the cursor visible.
	model.scrollOffset = position.scrollOffset
	model.ensureCursorVisible()
	model.focusRegion = FocusList
}

// handleFilterKeys processes keystrokes when the filter input has focus.
func (model Model) handleFilterKeys(message tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(message, model.keys.Quit):
		// ctrl+c always quits, even in filter mode.
		if message.Type == tea.KeyCtrlC {
			return model, tea.Quit
		}
		// 'q' is a regular character in filter mode.
		model.filter.HandleRune('q')
		model.applyFilter()
		return model, nil

	case key.Matches(message, model.keys.FilterClear):
		// Esc: if there's filter text, clear it; if already empty, exit filter mode.
		if model.filter.Input != "" {
			model.filter.Clear()
			model.updatePaneSizes()
			model.refreshFromSource()
		} else {
			model.filter.Active = false
			model.updatePaneSizes()
			model.focusRegion = model.priorFocus
		}
		return model, nil

	case message.Type == tea.KeyEnter:
		// Confirm filter and return focus to the list.
		model.filter.Active = false
		model.focusRegion = FocusList
		return model, nil

	case message.Type == tea.KeyBackspace:
		if model.filter.HandleBackspace() {
			model.applyFilter()
		}
		return model, nil

	case message.Type == tea.KeyRunes || message.Type == tea.KeySpace:
		for _, r := range message.Runes {
			model.filter.HandleRune(r)
		}
		model.applyFilter()
		return model, nil
	}

	return model, nil
}

// handleDetailSearchKeys processes keystrokes when the detail search
// input has focus. Follows the same pattern as handleFilterKeys:
// regular characters go to the input, Esc clears/exits, Enter
// confirms and returns to detail navigation.
func (model Model) handleDetailSearchKeys(message tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(message, model.keys.Quit):
		if message.Type == tea.KeyCtrlC {
			return model, tea.Quit
		}
		// 'q' is a regular character in search mode.
		model.detailPane.search.HandleRune('q')
		model.detailPane.applySearchHighlighting()
		return model, nil

	case key.Matches(message, model.keys.FilterClear):
		// Esc: if there's search text, clear it and remove highlights;
		// if already empty, exit search mode.
		if model.detailPane.search.Input != "" {
			model.detailPane.search.Clear()
			model.detailPane.applySearchHighlighting()
			model.updatePaneSizes()
		} else {
			model.detailPane.search.Active = false
			model.updatePaneSizes()
		}
		model.focusRegion = FocusDetail
		return model, nil

	case message.Type == tea.KeyEnter:
		// Confirm search and return to detail navigation.
		model.detailPane.search.Active = false
		model.updatePaneSizes()
		model.focusRegion = FocusDetail
		// Jump to the first match if the user just typed a query.
		if model.detailPane.search.MatchCount() > 0 {
			model.detailPane.ScrollToCurrentMatch()
		}
		return model, nil

	case message.Type == tea.KeyBackspace:
		if model.detailPane.search.HandleBackspace() {
			model.detailPane.applySearchHighlighting()
		}
		return model, nil

	case message.Type == tea.KeyRunes || message.Type == tea.KeySpace:
		for _, character := range message.Runes {
			model.detailPane.search.HandleRune(character)
		}
		model.detailPane.applySearchHighlighting()
		return model, nil
	}

	return model, nil
}

// switchTab changes the active tab and reloads data from the source.
func (model *Model) switchTab(tab Tab) {
	if model.activeTab == tab {
		return
	}
	model.activeTab = tab
	model.filter.Clear()
	model.refreshFromSource()
}

// refreshFromSource reloads entries from the source for the active tab
// and rebuilds the item list.
func (model *Model) refreshFromSource() {
	var snapshot Snapshot
	switch model.activeTab {
	case TabReady:
		snapshot = model.source.Ready()
	case TabBlocked:
		snapshot = model.source.Blocked()
	case TabAll:
		snapshot = model.source.All()
	}

	model.stats = snapshot.Stats

	// Apply filter if there's active filter text.
	if model.filter.Input != "" {
		results := model.filter.ApplyFuzzy(snapshot.Entries, model.source)
		model.entries = make([]ticketindex.Entry, len(results))
		model.filterHighlights = make(map[string][]int, len(results))
		for index, result := range results {
			model.entries[index] = result.Entry
			if len(result.TitlePositions) > 0 {
				model.filterHighlights[result.Entry.ID] = result.TitlePositions
			}
		}
	} else {
		model.entries = snapshot.Entries
		model.filterHighlights = nil
	}

	model.rebuildItems()
	model.restoreSelection()
	model.ensureCursorVisible()
	model.syncDetailPane()
}

// applyFilter re-filters the current source data without reloading.
func (model *Model) applyFilter() {
	var snapshot Snapshot
	switch model.activeTab {
	case TabReady:
		snapshot = model.source.Ready()
	case TabBlocked:
		snapshot = model.source.Blocked()
	case TabAll:
		snapshot = model.source.All()
	}

	model.stats = snapshot.Stats

	if model.filter.Input != "" {
		results := model.filter.ApplyFuzzy(snapshot.Entries, model.source)
		model.entries = make([]ticketindex.Entry, len(results))
		model.filterHighlights = make(map[string][]int, len(results))
		for index, result := range results {
			model.entries[index] = result.Entry
			if len(result.TitlePositions) > 0 {
				model.filterHighlights[result.Entry.ID] = result.TitlePositions
			}
		}
	} else {
		model.entries = snapshot.Entries
		model.filterHighlights = nil
	}

	model.rebuildItems()

	// When actively filtering, snap to the top of the list so the
	// highest-scored matches are visible as the user types. Without
	// this, the scroll offset from the pre-filter list persists and
	// the user sees an arbitrary slice of filtered results.
	if model.filter.Input != "" {
		model.cursor = 0
		model.scrollOffset = 0
		if len(model.items) > 0 && !model.items[0].IsHeader {
			model.selectedID = model.items[0].Entry.ID
		}
	} else {
		model.restoreSelection()
	}
	model.ensureCursorVisible()
	model.syncDetailPane()
}

// rebuildItems constructs the items list from entries. For the ready
// tab, tickets are grouped by parent epic. For other tabs, items are
// a flat list of ticket entries.
func (model *Model) rebuildItems() {
	if model.activeTab != TabReady {
		model.items = make([]ListItem, len(model.entries))
		for index, entry := range model.entries {
			model.items[index] = ListItem{Entry: entry}
		}
		return
	}

	// Group ready tickets by parent epic.
	model.items = model.buildGroupedItems()
}

// buildGroupedItems constructs the ready view with epic grouping.
//
// Algorithm:
//   - Collect entries that have a parent epic, grouped by parent ID
//   - Sort epic groups by the epic's priority (ascending), then the
//     epic's creation time
//   - Within each group, entries are already sorted by priority then
//     creation time (from the source)
//   - Ungrouped entries (no parent) appear at the bottom
func (model *Model) buildGroupedItems() []ListItem {
	type epicInfo struct {
		priority  int
		createdAt string
		title     string
	}

	type epicGroup struct {
		epicID      string
		epicContent *epicInfo
		children    []ticketindex.Entry
	}

	groups := make(map[string]*epicGroup)
	var ungrouped []ticketindex.Entry

	for _, entry := range model.entries {
		if entry.Content.Parent == "" {
			// No parent — check if this entry IS an epic with children.
			// Epics themselves don't appear as rows; they appear as headers.
			if entry.Content.Type == "epic" {
				continue
			}
			ungrouped = append(ungrouped, entry)
			continue
		}

		parentID := entry.Content.Parent
		group, exists := groups[parentID]
		if !exists {
			parentContent, parentExists := model.source.Get(parentID)
			info := &epicInfo{}
			if parentExists {
				info.priority = parentContent.Priority
				info.createdAt = parentContent.CreatedAt
				info.title = parentContent.Title
			} else {
				// Dangling parent reference — use a high priority so it
				// sorts to the end, and show the ID as the title.
				info.priority = 99
				info.title = parentID
			}
			group = &epicGroup{
				epicID:      parentID,
				epicContent: info,
			}
			groups[parentID] = group
		}
		group.children = append(group.children, entry)
	}

	// Sort epic groups by epic priority, then creation time.
	sortedGroups := make([]*epicGroup, 0, len(groups))
	for _, group := range groups {
		sortedGroups = append(sortedGroups, group)
	}
	slices.SortFunc(sortedGroups, func(a, b *epicGroup) int {
		if a.epicContent.priority != b.epicContent.priority {
			return a.epicContent.priority - b.epicContent.priority
		}
		return strings.Compare(a.epicContent.createdAt, b.epicContent.createdAt)
	})

	var items []ListItem

	for _, group := range sortedGroups {
		total, closed := model.source.ChildProgress(group.epicID)
		health := model.source.EpicHealth(group.epicID)

		collapsed := model.collapsedEpics[group.epicID]
		items = append(items, ListItem{
			IsHeader:   true,
			EpicID:     group.epicID,
			EpicTitle:  group.epicContent.title,
			EpicDone:   closed,
			EpicTotal:  total,
			Collapsed:  collapsed,
			EpicHealth: &health,
		})

		if !collapsed {
			for _, child := range group.children {
				items = append(items, ListItem{Entry: child})
			}
		}
	}

	// Ungrouped tickets at the bottom.
	if len(ungrouped) > 0 {
		collapsed := model.collapsedEpics[ungroupedEpicID]
		items = append(items, ListItem{
			IsHeader:  true,
			EpicID:    ungroupedEpicID,
			EpicTitle: "ungrouped",
			EpicDone:  0,
			EpicTotal: len(ungrouped),
			Collapsed: collapsed,
		})
		if !collapsed {
			for _, entry := range ungrouped {
				items = append(items, ListItem{Entry: entry})
			}
		}
	}

	return items
}

// restoreSelection attempts to find the previously selected ticket ID
// in the rebuilt items list and move the cursor there. If not found,
// clamps the cursor to a valid position.
func (model *Model) restoreSelection() {
	if model.selectedID == "" {
		model.cursor = 0
		return
	}

	for index, item := range model.items {
		if !item.IsHeader && item.Entry.ID == model.selectedID {
			model.cursor = index
			return
		}
	}

	// Selected ticket is no longer in the list. Clamp cursor.
	model.cursor = model.clampedIndex(model.cursor)
}

// clampedIndex returns position clamped to valid item bounds.
func (model *Model) clampedIndex(position int) int {
	if len(model.items) == 0 {
		return 0
	}
	if position < 0 {
		return 0
	}
	if position >= len(model.items) {
		return len(model.items) - 1
	}
	return position
}

// handleListKeys processes navigation keys when the list has focus.
func (model *Model) handleListKeys(message tea.KeyMsg) {
	prevCursor := model.cursor

	switch {
	case key.Matches(message, model.keys.Up):
		model.moveCursorUp()

	case key.Matches(message, model.keys.Down):
		model.moveCursorDown()

	case key.Matches(message, model.keys.PageUp):
		target := model.cursor - model.visibleHeight()
		if target < 0 {
			target = 0
		}
		model.cursor = model.clampedIndex(target)

	case key.Matches(message, model.keys.PageDown):
		target := model.cursor + model.visibleHeight()
		if len(model.items) > 0 && target >= len(model.items) {
			target = len(model.items) - 1
		}
		model.cursor = model.clampedIndex(target)

	case key.Matches(message, model.keys.Home):
		model.cursor = 0

	case key.Matches(message, model.keys.End):
		if len(model.items) > 0 {
			model.cursor = len(model.items) - 1
		}

	case key.Matches(message, model.keys.Left):
		model.collapseOrGoToParent()

	case key.Matches(message, model.keys.Right):
		model.expandOrEnterFirstChild()

	case message.Type == tea.KeyEnter:
		// Toggle epic collapse on header items.
		if model.cursor < len(model.items) && model.items[model.cursor].IsHeader {
			epicID := model.items[model.cursor].EpicID
			if epicID != "" {
				model.collapsedEpics[epicID] = !model.collapsedEpics[epicID]
				model.rebuildItems()
				model.restoreSelection()
			}
		}
	}

	model.ensureCursorVisible()

	// Update detail pane if selection changed.
	if model.cursor != prevCursor {
		model.syncDetailPane()
	}
}

// moveCursorUp moves the cursor to the previous item (headers and
// ticket rows are both selectable).
func (model *Model) moveCursorUp() {
	if model.cursor > 0 {
		model.cursor--
	}
}

// moveCursorDown moves the cursor to the next item (headers and
// ticket rows are both selectable).
func (model *Model) moveCursorDown() {
	if model.cursor < len(model.items)-1 {
		model.cursor++
	}
}

// collapseOrGoToParent handles the Left key in the list:
//   - On an expanded header: collapse it
//   - On a child ticket: collapse the parent group (cursor stays on the header)
//   - On a collapsed header: no-op
func (model *Model) collapseOrGoToParent() {
	if model.cursor < 0 || model.cursor >= len(model.items) {
		return
	}

	item := model.items[model.cursor]

	// Find the epic ID to collapse: either this header or the nearest
	// parent header above the current position.
	epicID := ""
	if item.IsHeader {
		epicID = item.EpicID
	} else {
		for index := model.cursor - 1; index >= 0; index-- {
			if model.items[index].IsHeader {
				epicID = model.items[index].EpicID
				break
			}
		}
	}

	if epicID == "" || model.collapsedEpics[epicID] {
		return
	}

	model.collapsedEpics[epicID] = true
	model.rebuildItems()
	// Place cursor on the collapsed header.
	for index, rebuilt := range model.items {
		if rebuilt.IsHeader && rebuilt.EpicID == epicID {
			model.cursor = index
			break
		}
	}
}

// expandOrEnterFirstChild handles the Right key in the list:
//   - On a collapsed header: expand it
//   - On an expanded header: move cursor to first child
//   - On a ticket row: no-op
func (model *Model) expandOrEnterFirstChild() {
	if model.cursor < 0 || model.cursor >= len(model.items) {
		return
	}

	item := model.items[model.cursor]
	if !item.IsHeader {
		return
	}

	if item.EpicID != "" && model.collapsedEpics[item.EpicID] {
		// Expand.
		model.collapsedEpics[item.EpicID] = false
		model.rebuildItems()
		model.restoreSelection()
		// Move cursor to the first child after this header.
		for index := 0; index < len(model.items); index++ {
			if model.items[index].IsHeader && model.items[index].EpicID == item.EpicID {
				if index+1 < len(model.items) && !model.items[index+1].IsHeader {
					model.cursor = index + 1
				}
				return
			}
		}
		return
	}

	// Already expanded — move to first child.
	if model.cursor+1 < len(model.items) && !model.items[model.cursor+1].IsHeader {
		model.cursor = model.cursor + 1
	}
}

// handleDetailKeys processes navigation keys when the detail pane has focus.
func (model *Model) handleDetailKeys(message tea.KeyMsg) {
	switch {
	case key.Matches(message, model.keys.Up):
		model.detailPane.viewport.LineUp(1)
	case key.Matches(message, model.keys.Down):
		model.detailPane.viewport.LineDown(1)
	case key.Matches(message, model.keys.PageUp):
		model.detailPane.ScrollUp()
	case key.Matches(message, model.keys.PageDown):
		model.detailPane.ScrollDown()
	case key.Matches(message, model.keys.Home):
		model.detailPane.viewport.GotoTop()
	case key.Matches(message, model.keys.End):
		model.detailPane.viewport.GotoBottom()

	// Search match navigation: only active when there's a search query.
	// When no search is active, 'n' opens the note modal (if the source
	// supports mutations).
	case key.Matches(message, model.keys.SearchNext):
		if model.detailPane.search.Input != "" {
			model.detailPane.search.NextMatch()
			model.detailPane.applySearchHighlighting()
			model.detailPane.ScrollToCurrentMatch()
		} else {
			model.openNoteModal()
		}
	case key.Matches(message, model.keys.SearchPrevious):
		if model.detailPane.search.Input != "" {
			model.detailPane.search.PreviousMatch()
			model.detailPane.applySearchHighlighting()
			model.detailPane.ScrollToCurrentMatch()
		}
	}
}

// handleDropdownKeys processes keyboard input when a dropdown overlay
// is active (FocusDropdown). Up/down navigate options, enter selects,
// escape dismisses. All other keys are consumed to prevent them from
// reaching the underlying panes.
func (model Model) handleDropdownKeys(message tea.KeyMsg) (tea.Model, tea.Cmd) {
	if model.activeDropdown == nil {
		model.focusRegion = FocusDetail
		return model, nil
	}

	switch {
	case key.Matches(message, model.keys.Quit):
		if message.Type == tea.KeyCtrlC {
			return model, tea.Quit
		}
		// 'q' dismisses the dropdown rather than quitting.
		model.dismissDropdown()

	case key.Matches(message, model.keys.FilterClear):
		// Escape dismisses the dropdown.
		model.dismissDropdown()

	case key.Matches(message, model.keys.Up):
		model.activeDropdown.MoveUp()

	case key.Matches(message, model.keys.Down):
		model.activeDropdown.MoveDown()

	case message.Type == tea.KeyEnter:
		selected := model.activeDropdown.Selected()
		msg := dropdownSelectMsg{
			field:    model.activeDropdown.Field,
			ticketID: model.activeDropdown.TicketID,
			value:    selected.Value,
		}
		model.dismissDropdown()
		return model.handleDropdownSelect(msg)
	}

	return model, nil
}

// dismissDropdown closes the active dropdown overlay and returns
// keyboard focus to the detail pane.
func (model *Model) dismissDropdown() {
	model.activeDropdown = nil
	model.focusRegion = FocusDetail
}

// handleHeaderClick processes a click on a header field (status,
// priority, or title). Opens the appropriate mutation UI if the source
// supports mutations. Returns a tea.Cmd if one is needed, or nil.
func (model *Model) handleHeaderClick(field string, screenX, screenY int) tea.Cmd {
	mutator, ok := model.source.(Mutator)
	if !ok {
		return nil
	}
	_ = mutator // Used by the select handler, not directly here.

	if model.selectedID == "" {
		return nil
	}

	content, exists := model.source.Get(model.selectedID)
	if !exists {
		return nil
	}

	switch field {
	case "status":
		options := StatusTransitions(content.Status)
		if len(options) == 0 {
			return nil
		}
		model.activeDropdown = &DropdownOverlay{
			Options:  options,
			Cursor:   0,
			AnchorX:  screenX,
			AnchorY:  screenY + 1, // Below the clicked field.
			Field:    "status",
			TicketID: model.selectedID,
		}
		model.focusRegion = FocusDropdown

	case "priority":
		model.activeDropdown = &DropdownOverlay{
			Options:  PriorityOptions(),
			Cursor:   content.Priority, // Pre-select current priority.
			AnchorX:  screenX,
			AnchorY:  screenY + 1,
			Field:    "priority",
			TicketID: model.selectedID,
		}
		model.focusRegion = FocusDropdown

	case "title":
		titleRunes := []rune(content.Title)
		model.titleEditBuffer = titleRunes
		model.titleEditCursor = len(titleRunes)
		model.titleEditID = model.selectedID
		model.focusRegion = FocusTitleEdit
		model.updateTitleEditHeader()
	}

	return nil
}

// handleDropdownSelect dispatches the mutation for a dropdown selection.
// The mutation runs in a background goroutine; results arrive as a
// mutationResultMsg. On success, the subscribe stream updates the
// local index automatically.
func (model Model) handleDropdownSelect(message dropdownSelectMsg) (tea.Model, tea.Cmd) {
	mutator, ok := model.source.(Mutator)
	if !ok {
		return model, nil
	}

	switch message.field {
	case "status":
		assignee := ""
		if message.value == "in_progress" {
			assignee = model.operatorUserID
		}
		return model, func() tea.Msg {
			err := mutator.UpdateStatus(context.Background(), message.ticketID, message.value, assignee)
			return mutationResultMsg{err: err}
		}

	case "priority":
		priority, err := strconv.Atoi(message.value)
		if err != nil {
			return model, func() tea.Msg {
				return mutationResultMsg{err: fmt.Errorf("invalid priority value: %s", message.value)}
			}
		}
		return model, func() tea.Msg {
			err := mutator.UpdatePriority(context.Background(), message.ticketID, priority)
			return mutationResultMsg{err: err}
		}
	}

	return model, nil
}

// handleTitleEditKeys processes keyboard input during inline title
// editing. Regular characters are inserted at the cursor. Backspace
// deletes behind the cursor. Left/right move the cursor. Home/end
// jump to the start/end. Enter submits the new title. Escape cancels
// and restores the original title.
func (model Model) handleTitleEditKeys(message tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(message, model.keys.Quit):
		if message.Type == tea.KeyCtrlC {
			return model, tea.Quit
		}
		// 'q' cancels the title edit rather than quitting.
		model.cancelTitleEdit()

	case key.Matches(message, model.keys.FilterClear):
		// Escape cancels the edit.
		model.cancelTitleEdit()

	case message.Type == tea.KeyEnter:
		return model.submitTitleEdit()

	case message.Type == tea.KeyBackspace:
		if model.titleEditCursor > 0 {
			model.titleEditBuffer = append(
				model.titleEditBuffer[:model.titleEditCursor-1],
				model.titleEditBuffer[model.titleEditCursor:]...)
			model.titleEditCursor--
			model.updateTitleEditHeader()
		}

	case message.Type == tea.KeyDelete:
		if model.titleEditCursor < len(model.titleEditBuffer) {
			model.titleEditBuffer = append(
				model.titleEditBuffer[:model.titleEditCursor],
				model.titleEditBuffer[model.titleEditCursor+1:]...)
			model.updateTitleEditHeader()
		}

	case message.Type == tea.KeyLeft:
		if model.titleEditCursor > 0 {
			model.titleEditCursor--
			model.updateTitleEditHeader()
		}

	case message.Type == tea.KeyRight:
		if model.titleEditCursor < len(model.titleEditBuffer) {
			model.titleEditCursor++
			model.updateTitleEditHeader()
		}

	case message.Type == tea.KeyHome || message.Type == tea.KeyCtrlA:
		model.titleEditCursor = 0
		model.updateTitleEditHeader()

	case message.Type == tea.KeyEnd || message.Type == tea.KeyCtrlE:
		model.titleEditCursor = len(model.titleEditBuffer)
		model.updateTitleEditHeader()

	case message.Type == tea.KeyRunes || message.Type == tea.KeySpace:
		for _, character := range message.Runes {
			// Insert at cursor position.
			model.titleEditBuffer = append(model.titleEditBuffer, 0)
			copy(model.titleEditBuffer[model.titleEditCursor+1:], model.titleEditBuffer[model.titleEditCursor:])
			model.titleEditBuffer[model.titleEditCursor] = character
			model.titleEditCursor++
		}
		model.updateTitleEditHeader()
	}

	return model, nil
}

// cancelTitleEdit restores the original title and exits edit mode.
func (model *Model) cancelTitleEdit() {
	model.titleEditBuffer = nil
	model.titleEditID = ""
	model.focusRegion = FocusDetail
	// Re-render the detail pane to restore the original title display.
	model.syncDetailPane()
}

// submitTitleEdit sends the title mutation and exits edit mode.
func (model Model) submitTitleEdit() (tea.Model, tea.Cmd) {
	newTitle := strings.TrimSpace(string(model.titleEditBuffer))
	ticketID := model.titleEditID

	model.titleEditBuffer = nil
	model.titleEditID = ""
	model.focusRegion = FocusDetail

	if newTitle == "" {
		// Empty title: cancel the edit silently.
		model.syncDetailPane()
		return model, nil
	}

	// Check if title actually changed.
	content, exists := model.source.Get(ticketID)
	if exists && content.Title == newTitle {
		model.syncDetailPane()
		return model, nil
	}

	mutator, ok := model.source.(Mutator)
	if !ok {
		model.syncDetailPane()
		return model, nil
	}

	model.syncDetailPane()
	return model, func() tea.Msg {
		err := mutator.UpdateTitle(context.Background(), ticketID, newTitle)
		return mutationResultMsg{err: err}
	}
}

// updateTitleEditHeader re-renders the detail header with the current
// title edit buffer and a visible cursor so the user sees their edits
// in real time.
func (model *Model) updateTitleEditHeader() {
	if !model.detailPane.hasEntry {
		return
	}

	// Build the title with a block cursor at the edit position.
	// Characters before the cursor are rendered normally; the cursor
	// character uses reverse video; characters after are normal.
	cursorStyle := lipgloss.NewStyle().Reverse(true)
	var titleWithCursor string
	if model.titleEditCursor >= len(model.titleEditBuffer) {
		// Cursor at the end: show a trailing block cursor.
		titleWithCursor = string(model.titleEditBuffer) + cursorStyle.Render(" ")
	} else {
		before := string(model.titleEditBuffer[:model.titleEditCursor])
		atCursor := string(model.titleEditBuffer[model.titleEditCursor : model.titleEditCursor+1])
		after := string(model.titleEditBuffer[model.titleEditCursor+1:])
		titleWithCursor = before + cursorStyle.Render(atCursor) + after
	}

	// Create a modified entry with the cursor-enhanced title.
	editedEntry := model.detailPane.entry
	editedEntry.Content.Title = titleWithCursor

	renderer := NewDetailRenderer(model.theme, model.detailPane.contentWidth())
	var score *ticketindex.TicketScore
	if model.activeTab == TabReady {
		ticketScore := model.source.Score(editedEntry.ID, model.detailPane.renderTime)
		score = &ticketScore
	}
	headerResult := renderer.RenderHeader(editedEntry, score)
	model.detailPane.header = headerResult.Rendered
	model.detailPane.headerClickTargets = headerResult.Targets
}

// openNoteModal opens the note input modal for the currently selected
// ticket. Does nothing if no ticket is selected or if the source
// does not support mutations.
func (model *Model) openNoteModal() {
	if model.selectedID == "" {
		return
	}
	if _, ok := model.source.(Mutator); !ok {
		return
	}

	modal := NewNoteModal(model.selectedID, model.theme)
	model.noteModal = &modal
	model.focusRegion = FocusNoteModal
}

// handleNoteModalKeys processes keyboard input when the note modal is
// active. Ctrl+D submits the note, Escape cancels. All other input
// goes to the textarea.
func (model Model) handleNoteModalKeys(message tea.KeyMsg) (tea.Model, tea.Cmd) {
	if model.noteModal == nil {
		model.focusRegion = FocusDetail
		return model, nil
	}

	switch {
	case message.Type == tea.KeyCtrlC:
		return model, tea.Quit

	case message.Type == tea.KeyEsc:
		model.noteModal = nil
		model.focusRegion = FocusDetail
		return model, nil

	case message.Type == tea.KeyCtrlD:
		// Submit the note.
		body := strings.TrimSpace(model.noteModal.Value())
		ticketID := model.noteModal.TicketID
		model.noteModal = nil
		model.focusRegion = FocusDetail

		if body == "" {
			return model, nil
		}

		mutator, ok := model.source.(Mutator)
		if !ok {
			return model, nil
		}

		return model, func() tea.Msg {
			err := mutator.AddNote(context.Background(), ticketID, body)
			return mutationResultMsg{err: err}
		}

	default:
		// Forward to textarea.
		model.noteModal.Update(message)
		return model, nil
	}
}

// syncDetailPane updates the detail pane content to reflect the
// currently selected ticket. Uses the current wall clock for
// staleness display — this is a user-triggered action (navigation),
// not a tight loop.
func (model *Model) syncDetailPane() {
	if model.cursor < 0 || model.cursor >= len(model.items) {
		model.detailPane.Clear()
		return
	}

	now := time.Now()
	item := model.items[model.cursor]
	if item.IsHeader {
		// Show the epic ticket in the detail pane if this header
		// corresponds to a real epic (not the ungrouped sentinel).
		if item.EpicID != "" && item.EpicID != ungroupedEpicID {
			content, ok := model.source.Get(item.EpicID)
			if ok {
				model.selectedID = item.EpicID
				model.detailPane.SetContent(model.source, ticketindex.Entry{
					ID:      item.EpicID,
					Content: content,
				}, now)
				return
			}
		}
		model.detailPane.Clear()
		return
	}

	model.selectedID = item.Entry.ID
	model.detailPane.SetContent(model.source, item.Entry, now)
}

// handleSourceEvent processes a live event from the source (ticket
// created, updated, or removed). Refreshes the snapshot, ignites the
// heat tracker, and schedules the animation tick if not already running.
func (model Model) handleSourceEvent(message sourceEventMsg) (tea.Model, tea.Cmd) {
	event := message.event
	now := time.Now()

	// Ignite heat for the changed ticket.
	kind := HeatPut
	if event.Kind == "remove" {
		kind = HeatRemove
	}
	model.heatTracker.Ignite(event.TicketID, kind, now)

	// Refresh the view from the source (which already has the update
	// applied — Put/Remove on IndexSource update the index before
	// dispatching the event).
	model.refreshFromSource()

	// If the detail pane is showing the changed ticket, re-render it.
	if model.selectedID == event.TicketID {
		model.syncDetailPane()
	}

	// Build the set of commands: always re-listen for the next event,
	// and start the heat tick if it isn't already running.
	commands := []tea.Cmd{listenForSourceEvent(model.eventChannel)}
	if !model.tickRunning {
		model.tickRunning = true
		commands = append(commands, scheduleHeatTick())
	}

	return model, tea.Batch(commands...)
}

// handleHeatTick processes a heat animation tick. If any tickets are
// still hot, schedules another tick; otherwise stops the timer.
func (model Model) handleHeatTick() (tea.Model, tea.Cmd) {
	now := time.Now()
	if model.heatTracker.HasHot(now) {
		return model, scheduleHeatTick()
	}
	model.tickRunning = false
	return model, nil
}

// scheduleHeatTick returns a tea.Cmd that sends a heatTickMsg after
// the animation tick interval.
func scheduleHeatTick() tea.Cmd {
	return tea.Tick(heatTickInterval, func(time.Time) tea.Msg {
		return heatTickMsg{}
	})
}

// updatePaneSizes recalculates pane dimensions after a resize or
// split ratio change.
func (model *Model) updatePaneSizes() {
	contentHeight := model.visibleHeight()
	// 1 column for the vertical divider between panes.
	detailWidth := model.width - model.listWidth() - 1
	if detailWidth < 10 {
		detailWidth = 10
	}
	model.detailPane.SetSize(detailWidth, contentHeight)
}

// listWidth returns the width of the list pane in columns.
func (model Model) listWidth() int {
	return int(float64(model.width) * model.splitRatio)
}

// View implements tea.Model. Renders the full TUI frame with two panes.
func (model Model) View() string {
	if !model.ready {
		return "Loading..."
	}

	if len(model.items) == 0 && model.filter.Input == "" {
		return model.renderEmpty()
	}

	var sections []string

	// Top chrome line: either the tab bar or the filter bar. The
	// filter bar replaces the tab bar so the layout doesn't shift.
	filterView := model.filter.View(model.theme, model.width)
	if filterView != "" {
		sections = append(sections, filterView)
	} else {
		sections = append(sections, model.renderHeader())
	}

	// Two-pane content area with vertical divider.
	listView := model.renderListPane()
	divider := model.renderDivider()
	detailFocused := model.focusRegion == FocusDetail
	detailView := model.detailPane.View(detailFocused)
	contentArea := lipgloss.JoinHorizontal(lipgloss.Top, listView, divider, detailView)
	sections = append(sections, contentArea)

	// Bottom separator.
	separator := lipgloss.NewStyle().
		Foreground(model.theme.BorderColor).
		Render(strings.Repeat("─", model.width))
	sections = append(sections, separator)

	// Help bar.
	sections = append(sections, model.renderHelp())

	output := strings.Join(sections, "\n")

	// Overlay hover effects if a tooltip is active: bold the hovered
	// link target, then splice the tooltip box on top.
	if model.tooltip != nil {
		output = overlayBold(output,
			model.tooltip.hoverScreenY,
			model.tooltip.hoverScreenX0,
			model.tooltip.hoverScreenX1)
		tooltipLines := renderTooltip(
			model.tooltip.ticketID, model.tooltip.content,
			model.theme, tooltipMaxWidth)
		output = overlayTooltip(output, tooltipLines,
			model.tooltip.anchorX, model.tooltip.anchorY)
	}

	// Overlay dropdown if active.
	if model.activeDropdown != nil {
		dropdownLines := model.activeDropdown.Render(model.theme)
		output = overlayTooltip(output, dropdownLines,
			model.activeDropdown.AnchorX, model.activeDropdown.AnchorY)
	}

	// Overlay note modal if active.
	if model.noteModal != nil {
		modalLines, anchorX, anchorY := model.noteModal.Render(model.width, model.height)
		output = overlayTooltip(output, modalLines, anchorX, anchorY)
	}

	return output
}

// renderListPane renders the ticket list with proper column layout.
func (model Model) renderListPane() string {
	listWidth := model.listWidth()

	// Always reserve 1 column for the focus indicator so content
	// stays at a fixed position regardless of focus state.
	focused := model.focusRegion == FocusList
	rowWidth := listWidth - 1

	renderer := NewListRenderer(model.theme, rowWidth)

	visible := model.visibleHeight()
	if visible < 0 {
		visible = 0
	}

	now := time.Now()
	var rows []string
	for index := model.scrollOffset; index < model.scrollOffset+visible && index < len(model.items); index++ {
		item := model.items[index]
		selected := index == model.cursor
		var row string
		if item.IsHeader {
			row = renderer.RenderGroupHeader(item, rowWidth, selected)
		} else {
			// On the Ready tab, compute and pass scoring info for
			// leverage/urgency indicators. Other tabs pass nil.
			var score *ticketindex.TicketScore
			if model.activeTab == TabReady {
				ticketScore := model.source.Score(item.Entry.ID, now)
				score = &ticketScore
			}
			row = renderer.RenderRow(item.Entry, selected, score, model.filterHighlights[item.Entry.ID])
			// Apply heat tint for recently-changed tickets (selection
			// highlight takes priority so we skip hot styling there).
			if !selected {
				ticketID := item.Entry.ID
				if heat := model.heatTracker.Heat(ticketID, now); heat > 0 {
					accentColor := model.theme.HotAccentPut
					if model.heatTracker.Kind(ticketID) == HeatRemove {
						accentColor = model.theme.HotAccentRemove
					}
					row = lipgloss.NewStyle().
						Background(accentColor).
						Width(rowWidth).
						MaxWidth(rowWidth).
						Render(row)
				}
			}
		}
		rows = append(rows, row)
	}

	// Pad empty rows.
	rendered := len(rows)
	if rendered < visible {
		emptyStyle := lipgloss.NewStyle().Width(rowWidth)
		for padding := rendered; padding < visible; padding++ {
			rows = append(rows, emptyStyle.Render(""))
		}
	}

	// Right scrollbar: shows scroll position and focus state.
	scrollbar := renderScrollbar(
		model.theme, visible,
		len(model.items), visible, model.scrollOffset,
		focused,
	)

	contentStyle := lipgloss.NewStyle().
		Width(rowWidth).
		Height(visible)

	return lipgloss.JoinHorizontal(lipgloss.Top,
		contentStyle.Render(strings.Join(rows, "\n")),
		scrollbar,
	)
}

// renderDivider renders the single-column vertical divider between the
// list and detail panes. The divider is draggable for resizing.
func (model Model) renderDivider() string {
	visible := model.visibleHeight()
	if visible < 0 {
		visible = 0
	}

	color := model.theme.BorderColor
	if model.draggingSplitter {
		color = model.theme.StatusInProgress
	}

	dividerStyle := lipgloss.NewStyle().
		Foreground(color)

	lines := make([]string, visible)
	for index := range lines {
		lines[index] = "│"
	}

	return dividerStyle.Width(1).Height(visible).Render(strings.Join(lines, "\n"))
}

// visibleHeight returns the number of list rows that fit between the
// chrome elements. Derived from contentStartY (chrome above) plus the
// bottom separator (1) and help bar (1).
func (model Model) visibleHeight() int {
	return model.height - model.contentStartY() - 2
}

// ensureCursorVisible adjusts scrollOffset so the cursor is within
// the visible window.
func (model *Model) ensureCursorVisible() {
	visible := model.visibleHeight()
	if visible <= 0 {
		return
	}

	// Clamp scrollOffset so we never scroll past the end of the list.
	// This handles tab switches where the new list is shorter than the
	// old scrollOffset.
	maxOffset := len(model.items) - visible
	if maxOffset < 0 {
		maxOffset = 0
	}
	if model.scrollOffset > maxOffset {
		model.scrollOffset = maxOffset
	}

	// Ensure the cursor is within the visible window.
	if model.cursor < model.scrollOffset {
		model.scrollOffset = model.cursor
	}
	if model.cursor >= model.scrollOffset+visible {
		model.scrollOffset = model.cursor - visible + 1
	}
}

// renderEmpty renders the empty state when no tickets match.
func (model Model) renderEmpty() string {
	text := "No tickets found."
	if stater, ok := model.source.(LoadingStater); ok {
		state := stater.LoadingState()
		if state != "caught_up" {
			text = loadingStateLabel(state)
		}
	}

	messageStyle := lipgloss.NewStyle().
		Foreground(model.theme.FaintText)

	return lipgloss.Place(
		model.width, model.height,
		lipgloss.Center, lipgloss.Center,
		messageStyle.Render(text),
	)
}

// tabDefs is the fixed list of tab definitions used by both the
// header renderer and the hit range computation.
var tabDefs = []struct {
	label string
	tab   Tab
}{
	{"1:Ready", TabReady},
	{"2:Blocked", TabBlocked},
	{"3:All", TabAll},
}

// computeTabHitRanges calculates the X positions of each tab label
// in the header line. Called on window resize so mouse clicks on
// the header can switch tabs.
func (model *Model) computeTabHitRanges() {
	model.tabHitRanges = model.tabHitRanges[:0]
	cursor := 3 // Leading "───"

	for index, tabDef := range tabDefs {
		cursor++ // Space before label.
		labelStart := cursor
		cursor += lipgloss.Width(tabDef.label)

		model.tabHitRanges = append(model.tabHitRanges, tabHitRange{
			startX: labelStart,
			endX:   cursor,
			tab:    tabDef.tab,
		})

		cursor++ // Space after label.

		// Separator between tabs (3 chars) and after last tab (1 char).
		if index == len(tabDefs)-1 {
			cursor++
		} else {
			cursor += 3
		}
	}
}

// renderHeader renders the combined tab bar + separator as a single
// line in the btop style: tab labels embedded in a horizontal rule
// with stats on the right.
//
// Example: ─── 1:Ready ─── 2:Blocked ─── 3:All ─── 12 shown  3 open ─
func (model Model) renderHeader() string {
	separatorStyle := lipgloss.NewStyle().
		Foreground(model.theme.BorderColor)
	activeStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(model.theme.HeaderForeground)
	inactiveStyle := lipgloss.NewStyle().
		Foreground(model.theme.FaintText)
	statsStyle := lipgloss.NewStyle().
		Foreground(model.theme.FaintText)

	sep := separatorStyle.Render("─")

	// Build the left portion: ─── Label ─── Label ─── Label ─
	leftParts := sep + sep + sep // Leading "───"
	cursor := 3

	for index, tabDef := range tabDefs {
		leftParts += " "
		cursor++

		if model.activeTab == tabDef.tab {
			leftParts += activeStyle.Render(tabDef.label)
		} else {
			leftParts += inactiveStyle.Render(tabDef.label)
		}
		cursor += lipgloss.Width(tabDef.label)

		leftParts += " "
		cursor++

		sepCount := 3
		if index == len(tabDefs)-1 {
			sepCount = 1
		}
		for range sepCount {
			leftParts += sep
			cursor++
		}
	}

	// Stats on the right.
	viewCount := len(model.entries)
	open := model.stats.ByStatus["open"]
	inProgress := model.stats.ByStatus["in_progress"]
	closed := model.stats.ByStatus["closed"]
	statsText := fmt.Sprintf(
		"%d shown  %d open  %d active  %d closed",
		viewCount, open, inProgress, closed)
	statsRendered := statsStyle.Render(statsText)
	statsWidth := lipgloss.Width(statsText)

	// Fill the gap between tabs and stats with separator chars,
	// leaving 1 space on each side of the stats.
	rightPortion := " " + statsRendered + " " + sep
	rightWidth := 1 + statsWidth + 1 + 1

	fillCount := model.width - cursor - rightWidth
	if fillCount < 1 {
		fillCount = 1
	}
	fill := ""
	for range fillCount {
		fill += sep
	}

	return leftParts + fill + rightPortion
}

// statusIconString returns the single-character status indicator for a
// ticket status. Returns empty string for "open" and unknown statuses
// so callers can omit the icon entirely rather than reserving space.
func statusIconString(status string) string {
	switch status {
	case "in_progress":
		return "●"
	case "blocked":
		return "◐"
	case "closed":
		return "✓"
	default:
		return ""
	}
}

// renderHelp renders the bottom help bar with key hints and position.
func (model Model) renderHelp() string {
	style := lipgloss.NewStyle().Foreground(model.theme.HelpText)

	focusIndicator := "LIST"
	switch model.focusRegion {
	case FocusDetail:
		focusIndicator = "DETAIL"
	case FocusFilter:
		focusIndicator = "FILTER"
	case FocusDetailSearch:
		focusIndicator = "SEARCH"
	case FocusDropdown:
		focusIndicator = "SELECT"
	case FocusTitleEdit:
		focusIndicator = "EDIT"
	case FocusNoteModal:
		focusIndicator = "NOTE"
	}

	help := fmt.Sprintf(" [%s] q quit  ↑↓ navigate  ←→ collapse/expand  BS back  Tab focus  ]/[ resize  1/2/3 tabs  / filter",
		focusIndicator)

	// Show search match hint when detail has an active search.
	if model.focusRegion == FocusDetail && model.detailPane.search.Input != "" {
		matchCount := model.detailPane.search.MatchCount()
		if matchCount > 0 {
			help += fmt.Sprintf("  n/N search (%d/%d)",
				model.detailPane.search.CurrentIndex()+1, matchCount)
		} else {
			help += "  (no matches)"
		}
	}

	totalItems := 0
	for _, item := range model.items {
		if !item.IsHeader {
			totalItems++
		}
	}

	if len(model.items) > model.visibleHeight() {
		position := ""
		if model.scrollOffset == 0 {
			position = "top"
		} else if model.scrollOffset+model.visibleHeight() >= len(model.items) {
			position = "bottom"
		} else {
			percent := float64(model.scrollOffset) / float64(len(model.items)-model.visibleHeight()) * 100
			position = fmt.Sprintf("%d%%", int(percent))
		}
		help += fmt.Sprintf("  [%s] %d/%d", position, model.cursor+1, totalItems)
	} else if totalItems > 0 {
		// Find the 1-based position among selectable items.
		selectablePosition := 0
		for index := 0; index <= model.cursor && index < len(model.items); index++ {
			if !model.items[index].IsHeader {
				selectablePosition++
			}
		}
		help += fmt.Sprintf("  %d/%d", selectablePosition, totalItems)
	}

	// Show loading state for service-backed sources. The source
	// optionally implements LoadingStater to report stream progress.
	if stater, ok := model.source.(LoadingStater); ok {
		state := stater.LoadingState()
		if state != "caught_up" {
			loadingStyle := lipgloss.NewStyle().
				Foreground(model.theme.StatusColor("in_progress")).
				Bold(true)
			label := loadingStateLabel(state)
			help += "  " + loadingStyle.Render(label)
		}
	}

	// Show clipboard feedback when a right-click copy just happened.
	if model.clipboardNotice != "" {
		clipStyle := lipgloss.NewStyle().
			Foreground(model.theme.StatusColor("closed")).
			Bold(true)
		help += "  " + clipStyle.Render("Copied: "+model.clipboardNotice)
	}

	// Show mutation error when a mutation call failed.
	if model.mutationError != "" {
		errorStyle := lipgloss.NewStyle().
			Foreground(model.theme.StatusColor("blocked")).
			Bold(true)
		help += "  " + errorStyle.Render("Error: "+model.mutationError)
	}

	return style.Render(help)
}
