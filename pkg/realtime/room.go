// Package realtime is the WebSocket layer (Phase 05.3): authenticated connections, rooms, and a
// fan-out through Redis so that every API instance reaches the connections it holds.
package realtime

import (
	"regexp"
	"strconv"
	"strings"
)

// The room channels.
const (
	ChannelPanic         = "panic"
	ChannelTracking      = "tracking"
	ChannelNotifications = "notifications"
)

// The room scopes.
const (
	ScopeTeam      = "team"
	ScopeProject   = "project"
	ScopePersonnel = "personnel"
)

// codePattern is what a team or project code looks like (1K-IBMI-S1, 1K-IBMI-S). The pattern
// is narrower than a free string on purpose: a room name is built from client input.
var codePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`)

// Room is a parsed room name: "team.{code}.panic", "project.{code}.tracking" or
// "personnel.{id}.notifications".
type Room struct {
	Scope   string
	Subject string
	Channel string
}

// String is the room's name on the wire.
func (r Room) String() string { return r.Scope + "." + r.Subject + "." + r.Channel }

// PersonalRoom is where a person receives notification.created.
func PersonalRoom(personnelID int64) string {
	return Room{Scope: ScopePersonnel, Subject: strconv.FormatInt(personnelID, 10), Channel: ChannelNotifications}.String()
}

// TeamRoom and ProjectRoom name the panic and tracking rooms of a regu and a project.
func TeamRoom(code, channel string) string { return Room{ScopeTeam, code, channel}.String() }

// ProjectRoom is the room of a project for a channel.
func ProjectRoom(code, channel string) string { return Room{ScopeProject, code, channel}.String() }

// ParseRoom reads a room name. ok is false for anything that is not exactly one of the five
// room patterns, so an unknown room never reaches the authorizer.
func ParseRoom(name string) (Room, bool) {
	parts := strings.Split(name, ".")
	if len(parts) != 3 {
		return Room{}, false
	}
	r := Room{Scope: parts[0], Subject: parts[1], Channel: parts[2]}
	switch {
	case (r.Scope == ScopeTeam || r.Scope == ScopeProject) && (r.Channel == ChannelPanic || r.Channel == ChannelTracking):
		return r, codePattern.MatchString(r.Subject)
	case r.Scope == ScopePersonnel && r.Channel == ChannelNotifications:
		id, err := strconv.ParseInt(r.Subject, 10, 64)
		return r, err == nil && id > 0 && strconv.FormatInt(id, 10) == r.Subject
	}
	return Room{}, false
}
