// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package matrix

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/lib/ref"
	"github.com/bureau-foundation/bureau/messaging"
)

// inspectParams holds the parameters for the matrix inspect command.
type inspectParams struct {
	cli.SessionConfig
	Room      string `json:"room"       desc:"room alias (#...) or room ID (!...)" required:"true"`
	EventType string `json:"event_type" flag:"type" desc:"filter state events to a specific type (supports trailing * for prefix match)"`
	cli.JSONOutput
}

// inspectResult is the structured output of the inspect command.
type inspectResult struct {
	RoomID        string               `json:"room_id"                   desc:"room's Matrix ID"`
	Alias         string               `json:"alias,omitempty"           desc:"canonical room alias"`
	Name          string               `json:"name,omitempty"            desc:"room display name"`
	Topic         string               `json:"topic,omitempty"           desc:"room topic"`
	RoomVersion   string               `json:"room_version,omitempty"    desc:"Matrix room version"`
	RoomType      string               `json:"room_type,omitempty"       desc:"room type (m.space for spaces, empty for regular rooms)"`
	Creator       string               `json:"creator,omitempty"         desc:"user who created the room"`
	MemberCount   int                  `json:"member_count"              desc:"number of joined members"`
	Members       []inspectMember      `json:"members"                   desc:"room members with power levels"`
	PowerLevels   *inspectPowerLevels  `json:"power_levels,omitempty"    desc:"room power level configuration"`
	SpaceChildren []inspectSpaceChild  `json:"space_children,omitempty"  desc:"child rooms (for spaces)"`
	SpaceParents  []inspectSpaceParent `json:"space_parents,omitempty"   desc:"parent spaces"`
	BureauState   []inspectStateGroup  `json:"bureau_state,omitempty"    desc:"Bureau protocol state events (m.bureau.*)"`
	MatrixState   []inspectStateGroup  `json:"matrix_state,omitempty"    desc:"standard Matrix state events"`
	OtherState    []inspectStateGroup  `json:"other_state,omitempty"     desc:"non-standard, non-Bureau state events"`
}

// inspectMember represents a room member with their power level.
type inspectMember struct {
	UserID      string `json:"user_id"                desc:"Matrix user ID"`
	DisplayName string `json:"display_name,omitempty" desc:"display name"`
	Membership  string `json:"membership"             desc:"membership state (join, invite, leave, ban)"`
	PowerLevel  int    `json:"power_level"            desc:"power level in this room"`
}

// inspectPowerLevels holds the parsed power level configuration.
type inspectPowerLevels struct {
	EventsDefault int            `json:"events_default" desc:"default power level required to send events"`
	StateDefault  int            `json:"state_default"  desc:"default power level required to send state events"`
	Ban           int            `json:"ban"            desc:"power level required to ban users"`
	Kick          int            `json:"kick"           desc:"power level required to kick users"`
	Invite        int            `json:"invite"         desc:"power level required to invite users"`
	Redact        int            `json:"redact"         desc:"power level required to redact events"`
	Users         map[string]int `json:"users,omitempty"  desc:"per-user power level overrides"`
	Events        map[string]int `json:"events,omitempty" desc:"per-event-type power level overrides"`
}

// inspectSpaceChild represents a child room in a space hierarchy.
type inspectSpaceChild struct {
	RoomID string `json:"room_id"          desc:"child room's Matrix ID"`
	Alias  string `json:"alias,omitempty"  desc:"child room alias"`
	Name   string `json:"name,omitempty"   desc:"child room display name"`
}

// inspectSpaceParent represents a parent space in the hierarchy.
type inspectSpaceParent struct {
	RoomID string `json:"room_id" desc:"parent space's Matrix ID"`
}

// inspectStateGroup groups state events by their event type.
type inspectStateGroup struct {
	EventType string              `json:"event_type" desc:"Matrix event type"`
	Events    []inspectStateEntry `json:"events"     desc:"state events of this type"`
}

// inspectStateEntry is a single state event within a group.
type inspectStateEntry struct {
	StateKey string         `json:"state_key" desc:"state key for this event"`
	Sender   string         `json:"sender"    desc:"user who sent this event"`
	Content  map[string]any `json:"content"   desc:"event content"`
}

// InspectCommand returns the "inspect" subcommand for examining room state.
func InspectCommand() *cli.Command {
	var params inspectParams

	return &cli.Command{
		Name:    "inspect",
		Summary: "Inspect a room's full state",
		Description: `Display a structured summary of a Matrix room's state, organized into
semantic sections: identity, members, power levels, space relationships,
Bureau protocol events, and standard Matrix events.

Unlike "bureau matrix state get" (which dumps raw JSON), inspect groups
and labels events by purpose, cross-references members with power levels,
and resolves space child aliases.

Use --type to filter the state event sections to a specific event type
or prefix (e.g., --type 'm.bureau.*').`,
		Usage: "bureau matrix inspect [flags] <room>",
		Examples: []cli.Example{
			{
				Description: "Inspect a room",
				Command:     "bureau matrix inspect --credential-file ./creds '#bureau/machine:bureau.local'",
			},
			{
				Description: "Inspect only Bureau state events",
				Command:     "bureau matrix inspect --credential-file ./creds --type 'm.bureau.*' '!room:bureau.local'",
			},
			{
				Description: "Inspect as JSON for programmatic use",
				Command:     "bureau matrix inspect --json --credential-file ./creds '#bureau/machine:bureau.local'",
			},
		},
		Annotations:    cli.ReadOnly(),
		Output:         func() any { return &inspectResult{} },
		Params:         func() any { return &params },
		RequiredGrants: []string{"command/matrix/inspect"},
		Run: func(args []string) error {
			if len(args) == 1 {
				params.Room = args[0]
			} else if len(args) > 1 {
				return cli.Validation("unexpected argument: %s\n\nusage: bureau matrix inspect [flags] <room>", args[1])
			}
			if params.Room == "" {
				return cli.Validation("room is required\n\nusage: bureau matrix inspect [flags] <room>")
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			session, err := params.SessionConfig.Connect(ctx)
			if err != nil {
				return cli.Internal("connect: %w", err)
			}

			roomID, err := resolveRoom(ctx, session, params.Room)
			if err != nil {
				return err
			}

			result, err := buildInspectResult(ctx, session, roomID, params.EventType)
			if err != nil {
				return err
			}

			if done, err := params.EmitJSON(result); done {
				return err
			}

			printInspectHuman(result)
			return nil
		},
	}
}

// buildInspectResult fetches room state and members, then organizes them into
// a structured inspectResult.
func buildInspectResult(ctx context.Context, session messaging.Session, roomID ref.RoomID, typeFilter string) (*inspectResult, error) {
	stateEvents, err := session.GetRoomState(ctx, roomID)
	if err != nil {
		return nil, cli.Internal("get room state: %w", err)
	}

	members, err := session.GetRoomMembers(ctx, roomID)
	if err != nil {
		return nil, cli.Internal("get room members: %w", err)
	}

	result := &inspectResult{RoomID: roomID.String()}

	// Collect state events into groups. We handle well-known types specially
	// (extracting identity fields, power levels, space relationships) and
	// group the rest by category.
	var powerLevelsRaw map[string]any
	bureauGroups := make(map[string][]inspectStateEntry)
	matrixGroups := make(map[string][]inspectStateEntry)
	otherGroups := make(map[string][]inspectStateEntry)

	for _, event := range stateEvents {
		stateKey := ""
		if event.StateKey != nil {
			stateKey = *event.StateKey
		}

		switch event.Type {
		case "m.room.create":
			if version, ok := event.Content["room_version"].(string); ok {
				result.RoomVersion = version
			}
			if roomType, ok := event.Content["type"].(string); ok {
				result.RoomType = roomType
			}
			if creator, ok := event.Content["creator"].(string); ok {
				result.Creator = creator
			}
		case "m.room.name":
			if name, ok := event.Content["name"].(string); ok {
				result.Name = name
			}
		case "m.room.canonical_alias":
			if alias, ok := event.Content["alias"].(string); ok {
				result.Alias = alias
			}
		case "m.room.topic":
			if topic, ok := event.Content["topic"].(string); ok {
				result.Topic = topic
			}
		case "m.room.power_levels":
			powerLevelsRaw = event.Content
		case "m.space.child":
			if stateKey != "" {
				result.SpaceChildren = append(result.SpaceChildren, inspectSpaceChild{
					RoomID: stateKey,
				})
			}
		case "m.space.parent":
			if stateKey != "" {
				result.SpaceParents = append(result.SpaceParents, inspectSpaceParent{
					RoomID: stateKey,
				})
			}
		case "m.room.member":
			// Members are handled via GetRoomMembers, skip here.
		default:
			entry := inspectStateEntry{
				StateKey: stateKey,
				Sender:   event.Sender,
				Content:  event.Content,
			}

			eventTypeString := event.Type.String()
			switch {
			case strings.HasPrefix(eventTypeString, "m.bureau."):
				bureauGroups[eventTypeString] = append(bureauGroups[eventTypeString], entry)
			case strings.HasPrefix(eventTypeString, "m."):
				matrixGroups[eventTypeString] = append(matrixGroups[eventTypeString], entry)
			default:
				otherGroups[eventTypeString] = append(otherGroups[eventTypeString], entry)
			}
		}
	}

	// Parse power levels.
	if powerLevelsRaw != nil {
		result.PowerLevels = parsePowerLevels(powerLevelsRaw)
	}

	// Build member list with power levels cross-referenced.
	userPowerLevels := make(map[string]int)
	defaultPowerLevel := 0
	if result.PowerLevels != nil {
		userPowerLevels = result.PowerLevels.Users
		defaultPowerLevel = result.PowerLevels.EventsDefault
		if usersDefault, ok := powerLevelsRaw["users_default"]; ok {
			if v, ok := usersDefault.(float64); ok {
				defaultPowerLevel = int(v)
			}
		}
	}

	joinedCount := 0
	for _, member := range members {
		powerLevel := defaultPowerLevel
		if pl, ok := userPowerLevels[member.UserID]; ok {
			powerLevel = pl
		}
		result.Members = append(result.Members, inspectMember{
			UserID:      member.UserID,
			DisplayName: member.DisplayName,
			Membership:  member.Membership,
			PowerLevel:  powerLevel,
		})
		if member.Membership == "join" {
			joinedCount++
		}
	}
	result.MemberCount = joinedCount

	// Sort members by power level descending, then user ID.
	sort.Slice(result.Members, func(i, j int) bool {
		if result.Members[i].PowerLevel != result.Members[j].PowerLevel {
			return result.Members[i].PowerLevel > result.Members[j].PowerLevel
		}
		return result.Members[i].UserID < result.Members[j].UserID
	})

	// Resolve space child aliases/names.
	for i := range result.SpaceChildren {
		childID, parseErr := ref.ParseRoomID(result.SpaceChildren[i].RoomID)
		if parseErr != nil {
			continue
		}
		childName, childAlias, _ := inspectRoomState(ctx, session, childID)
		result.SpaceChildren[i].Alias = childAlias
		result.SpaceChildren[i].Name = childName
	}

	// Apply type filter and build sorted state groups.
	typeFilters := parseTypeFilter(typeFilter)
	result.BureauState = buildStateGroups(bureauGroups, typeFilters)
	result.MatrixState = buildStateGroups(matrixGroups, typeFilters)
	result.OtherState = buildStateGroups(otherGroups, typeFilters)

	return result, nil
}

// parseTypeFilter converts a single type filter string into a filter slice
// suitable for matchesTypeFilter. Empty string means no filter.
func parseTypeFilter(filter string) []string {
	if filter == "" {
		return nil
	}
	return []string{filter}
}

// buildStateGroups converts a map of event-type → entries into a sorted slice
// of inspectStateGroup, filtered by type patterns.
func buildStateGroups(groups map[string][]inspectStateEntry, typeFilters []string) []inspectStateGroup {
	var result []inspectStateGroup
	for eventType, entries := range groups {
		if !matchesTypeFilter(eventType, typeFilters) {
			continue
		}
		// Sort entries within each group by state key.
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].StateKey < entries[j].StateKey
		})
		result = append(result, inspectStateGroup{
			EventType: eventType,
			Events:    entries,
		})
	}
	// Sort groups by event type name.
	sort.Slice(result, func(i, j int) bool {
		return result[i].EventType < result[j].EventType
	})
	return result
}

// parsePowerLevels extracts a structured inspectPowerLevels from the raw
// m.room.power_levels content map.
func parsePowerLevels(raw map[string]any) *inspectPowerLevels {
	pl := &inspectPowerLevels{}

	pl.EventsDefault = intFromContent(raw, "events_default")
	pl.StateDefault = intFromContent(raw, "state_default")
	pl.Ban = intFromContent(raw, "ban")
	pl.Kick = intFromContent(raw, "kick")
	pl.Invite = intFromContent(raw, "invite")
	pl.Redact = intFromContent(raw, "redact")

	if users, ok := raw["users"].(map[string]any); ok {
		pl.Users = make(map[string]int, len(users))
		for userID, level := range users {
			if v, ok := level.(float64); ok {
				pl.Users[userID] = int(v)
			}
		}
	}

	if events, ok := raw["events"].(map[string]any); ok {
		pl.Events = make(map[string]int, len(events))
		for eventType, level := range events {
			if v, ok := level.(float64); ok {
				pl.Events[eventType] = int(v)
			}
		}
	}

	return pl
}

// intFromContent extracts an integer from a content map by key, defaulting to 0.
func intFromContent(content map[string]any, key string) int {
	if v, ok := content[key].(float64); ok {
		return int(v)
	}
	return 0
}

// printInspectHuman prints the inspect result in human-readable format.
func printInspectHuman(result *inspectResult) {
	// Identity section.
	fmt.Fprintf(os.Stdout, "Room:     %s\n", result.RoomID)
	if result.Alias != "" {
		fmt.Fprintf(os.Stdout, "Alias:    %s\n", result.Alias)
	}
	if result.Name != "" {
		fmt.Fprintf(os.Stdout, "Name:     %s\n", result.Name)
	}
	if result.Topic != "" {
		fmt.Fprintf(os.Stdout, "Topic:    %s\n", result.Topic)
	}
	roomTypeLabel := "room"
	if result.RoomType == "m.space" {
		roomTypeLabel = "space"
	}
	if result.RoomVersion != "" {
		fmt.Fprintf(os.Stdout, "Type:     %s (v%s)\n", roomTypeLabel, result.RoomVersion)
	} else {
		fmt.Fprintf(os.Stdout, "Type:     %s\n", roomTypeLabel)
	}
	if result.Creator != "" {
		fmt.Fprintf(os.Stdout, "Creator:  %s\n", result.Creator)
	}
	fmt.Fprintln(os.Stdout)

	// Members section.
	fmt.Fprintf(os.Stdout, "Members (%d):\n", result.MemberCount)
	writer := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(writer, "  USER ID\tDISPLAY NAME\tMEMBERSHIP\tPOWER")
	for _, member := range result.Members {
		fmt.Fprintf(writer, "  %s\t%s\t%s\t%d\n",
			member.UserID, member.DisplayName, member.Membership, member.PowerLevel)
	}
	writer.Flush()
	fmt.Fprintln(os.Stdout)

	// Power levels section.
	if result.PowerLevels != nil {
		pl := result.PowerLevels
		fmt.Fprintln(os.Stdout, "Power Levels:")
		fmt.Fprintf(os.Stdout, "  events_default: %d    state_default: %d    ban: %d    kick: %d    invite: %d    redact: %d\n",
			pl.EventsDefault, pl.StateDefault, pl.Ban, pl.Kick, pl.Invite, pl.Redact)
		if len(pl.Events) > 0 {
			fmt.Fprintln(os.Stdout, "  Event overrides:")
			// Sort event type overrides for stable output.
			types := make([]string, 0, len(pl.Events))
			for eventType := range pl.Events {
				types = append(types, eventType)
			}
			sort.Strings(types)
			for _, eventType := range types {
				level := pl.Events[eventType]
				label := powerLevelLabel(level)
				fmt.Fprintf(os.Stdout, "    %-40s %d (%s)\n", eventType, level, label)
			}
		}
		fmt.Fprintln(os.Stdout)
	}

	// Space relationships.
	if len(result.SpaceChildren) > 0 {
		fmt.Fprintf(os.Stdout, "Space Children (%d):\n", len(result.SpaceChildren))
		childWriter := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(childWriter, "  ROOM ID\tALIAS\tNAME")
		for _, child := range result.SpaceChildren {
			fmt.Fprintf(childWriter, "  %s\t%s\t%s\n", child.RoomID, child.Alias, child.Name)
		}
		childWriter.Flush()
		fmt.Fprintln(os.Stdout)
	}

	if len(result.SpaceParents) > 0 {
		fmt.Fprintf(os.Stdout, "Space Parents (%d):\n", len(result.SpaceParents))
		for _, parent := range result.SpaceParents {
			fmt.Fprintf(os.Stdout, "  %s\n", parent.RoomID)
		}
		fmt.Fprintln(os.Stdout)
	}

	// State event sections.
	printStateSection(os.Stdout, "Bureau State", result.BureauState)
	printStateSection(os.Stdout, "Matrix State", result.MatrixState)
	printStateSection(os.Stdout, "Other State", result.OtherState)
}

// printStateSection prints a labeled section of grouped state events.
func printStateSection(out *os.File, label string, groups []inspectStateGroup) {
	if len(groups) == 0 {
		return
	}
	fmt.Fprintf(out, "%s:\n", label)
	for _, group := range groups {
		fmt.Fprintf(out, "  %s (%d %s):\n", group.EventType, len(group.Events), pluralEvent(len(group.Events)))
		for _, entry := range group.Events {
			content := abbreviateContent(entry.Content, 80)
			fmt.Fprintf(out, "    [%s]  sender=%s  %s\n", entry.StateKey, entry.Sender, content)
		}
	}
	fmt.Fprintln(out)
}

// pluralEvent returns "event" or "events" based on count.
func pluralEvent(count int) string {
	if count == 1 {
		return "event"
	}
	return "events"
}

// powerLevelLabel returns a human-readable label for a power level value.
func powerLevelLabel(level int) string {
	switch {
	case level <= 0:
		return "any member"
	case level <= 50:
		return "operator"
	default:
		return "admin"
	}
}
