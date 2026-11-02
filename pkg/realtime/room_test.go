package realtime_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"siapp/pkg/realtime"
)

func TestParseRoom(t *testing.T) {
	valid := map[string]realtime.Room{
		"team.1K-IBMI-S1.panic":      {Scope: "team", Subject: "1K-IBMI-S1", Channel: "panic"},
		"team.1K-IBMI-S1.tracking":   {Scope: "team", Subject: "1K-IBMI-S1", Channel: "tracking"},
		"project.1K-IBMI-S.panic":    {Scope: "project", Subject: "1K-IBMI-S", Channel: "panic"},
		"project.1K-IBMI-S.tracking": {Scope: "project", Subject: "1K-IBMI-S", Channel: "tracking"},
		"personnel.42.notifications": {Scope: "personnel", Subject: "42", Channel: "notifications"},
	}
	for name, want := range valid {
		got, ok := realtime.ParseRoom(name)
		require.True(t, ok, name)
		require.Equal(t, want, got, name)
		require.Equal(t, name, got.String())
	}

	for _, name := range []string{
		"", "team", "team.X.panic.extra", "team..panic", "team.X.notifications", "project.X.notifications",
		"personnel.42.panic", "personnel.0.notifications", "personnel.-1.notifications", "personnel.042.notifications",
		"personnel.abc.notifications", "org.1.panic", "team.a b.panic", "team.a/b.panic", "team.*.panic",
		"team.1K-IBMI-S1.panic ", "team." + string(make([]byte, 70)) + ".panic",
	} {
		_, ok := realtime.ParseRoom(name)
		require.False(t, ok, "%q must not be a room", name)
	}
}
