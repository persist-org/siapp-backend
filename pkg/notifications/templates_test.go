package notifications_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"siapp/internal/db"
	"siapp/pkg/notifications"
	"siapp/pkg/platform/timeutil"
)

var oct30 = timeutil.NewDate(2026, time.October, 30)

func fullVars(line notifications.BusinessLine) notifications.Vars {
	return notifications.Vars{
		BusinessLine: line, ActorName: "Budi", TeamName: "1K-IBMI-S1", TeamRole: db.TeamRoleDANRU,
		PositionName: "Kepala Shift", Subject: "Pos 1", Date: oct30, Approved: true,
	}
}

func TestAllTypesListsEveryTypeOnce(t *testing.T) {
	seen := map[notifications.Type]bool{}
	for _, typ := range notifications.AllTypes() {
		require.False(t, seen[typ], "%s is listed twice", typ)
		seen[typ] = true
	}
	require.Len(t, seen, 16, "the 12 types of Tabel 7 and the 4 the manual's flows imply")
}

// The unit test the plan asks for: every type renders a non-empty title and body in both
// business lines, whatever the caller managed to fill in.
func TestEveryTypeRendersInBothBusinessLines(t *testing.T) {
	lines := []notifications.BusinessLine{notifications.LineSecurity, notifications.LineCleaning, ""}
	for _, typ := range notifications.AllTypes() {
		for _, line := range lines {
			for name, vars := range map[string]notifications.Vars{
				"full":  fullVars(line),
				"empty": {BusinessLine: line},
			} {
				title, body := notifications.Render(typ, vars)
				where := string(typ) + "/" + string(line) + "/" + name
				require.NotEmpty(t, title, where)
				require.NotEmpty(t, body, where)
				for _, text := range []string{title, body} {
					require.Equal(t, strings.TrimSpace(text), text, "%s: stray space at an end of %q", where, text)
					require.NotContains(t, text, "  ", "%s: a hole where a field was left out: %q", where, text)
					require.NotContains(t, text, "()", where)
					require.NotContains(t, text, " .", where)
					require.NotContains(t, text, "%!", where)
				}
			}
		}
	}
}

func TestTitlesTellTheTypesApart(t *testing.T) {
	titles := map[string]notifications.Type{}
	for _, typ := range notifications.AllTypes() {
		title, _ := notifications.Render(typ, fullVars(notifications.LineSecurity))
		other, clash := titles[title]
		require.False(t, clash, "%s and %s share the title %q", typ, other, title)
		titles[title] = typ
	}
}

// The labels follow the project's business line (the alias table of the product overview §3.1).
func TestLabelsFollowTheBusinessLine(t *testing.T) {
	cases := []struct {
		name       string
		typ        notifications.Type
		vars       notifications.Vars
		security   [2]string // title, body
		cleaning   [2]string
		sameOnBoth bool
	}{
		{
			name: "the DANRU becomes the Supervisor", typ: notifications.TypePositionChanged,
			vars:     notifications.Vars{TeamRole: db.TeamRoleDANRU, TeamName: "R1"},
			security: [2]string{"Pergantian posisi", "Peran Anda di regu R1 sekarang DANRU."},
			cleaning: [2]string{"Pergantian posisi", "Peran Anda di tim R1 sekarang Supervisor."},
		},
		{
			name: "the member becomes a team member", typ: notifications.TypePositionChanged,
			vars:     notifications.Vars{TeamRole: db.TeamRoleMEMBER, TeamName: "R1"},
			security: [2]string{"Pergantian posisi", "Peran Anda di regu R1 sekarang Anggota."},
			cleaning: [2]string{"Pergantian posisi", "Peran Anda di tim R1 sekarang Anggota Tim."},
		},
		{
			name: "a position from Personel is Personel's own word", typ: notifications.TypePositionChanged,
			vars:       notifications.Vars{PositionName: "Kepala Shift"},
			security:   [2]string{"Pergantian posisi", "Posisi Anda sekarang Kepala Shift."},
			sameOnBoth: true,
		},
		{
			name: "regu becomes tim", typ: notifications.TypeTeamChanged,
			vars:     notifications.Vars{TeamName: "R2"},
			security: [2]string{"Perpindahan regu", "Anda dipindahkan ke regu R2."},
			cleaning: [2]string{"Perpindahan tim", "Anda dipindahkan ke tim R2."},
		},
		{
			name: "the DANRU who marked the absence", typ: notifications.TypeAbsenceMarked,
			vars:     notifications.Vars{ActorName: "Budi", Date: oct30},
			security: [2]string{"Anda ditandai mangkir", "Anda ditandai mangkir pada 30 Oktober 2026 oleh DANRU Budi."},
			cleaning: [2]string{"Anda ditandai mangkir", "Anda ditandai mangkir pada 30 Oktober 2026 oleh Supervisor Budi."},
		},
		{
			name: "patrol becomes a cleaning route", typ: notifications.TypePatrolSubmitted,
			vars:     notifications.Vars{ActorName: "Budi", Subject: "Pos 1"},
			security: [2]string{"Patroli menunggu persetujuan", "Budi mengirim patroli Pos 1. Menunggu persetujuan Anda."},
			cleaning: [2]string{"Rute Pembersihan menunggu persetujuan", "Budi mengirim rute pembersihan Pos 1. Menunggu persetujuan Anda."},
		},
		{
			name: "guard becomes a work zone", typ: notifications.TypeGuardApproved,
			vars:     notifications.Vars{ActorName: "Andi", Subject: "Gerbang"},
			security: [2]string{"Jaga disetujui", "Jaga Gerbang Anda telah disetujui oleh Andi."},
			cleaning: [2]string{"Zona Kerja Area disetujui", "Zona Kerja Area Gerbang Anda telah disetujui oleh Andi."},
		},
		{
			name: "the panic signal", typ: notifications.TypePanicTriggered,
			vars:     notifications.Vars{ActorName: "Sari", Subject: "Medis", TeamName: "R1"},
			security: [2]string{"Sinyal Tombol Panik", "Sari mengirim sinyal tombol panik (Medis) di regu R1. Segera beri bantuan."},
			cleaning: [2]string{"Sinyal Panik", "Sari mengirim sinyal panik (Medis) di tim R1. Segera beri bantuan."},
		},
		{
			name: "the panic ends", typ: notifications.TypePanicResolved,
			vars:     notifications.Vars{ActorName: "Sari"},
			security: [2]string{"Sinyal Tombol Panik selesai", "Sinyal Tombol Panik dari Sari telah ditandai selesai."},
			cleaning: [2]string{"Sinyal Panik selesai", "Sinyal Panik dari Sari telah ditandai selesai."},
		},
		{
			name: "a report is a cleaning report", typ: notifications.TypeReportSubmitted,
			vars:     notifications.Vars{ActorName: "Budi", Subject: "Pencurian", TeamName: "R1"},
			security: [2]string{"Laporan baru", "Budi mengirim laporan (Pencurian) di regu R1."},
			cleaning: [2]string{"Laporan Kebersihan baru", "Budi mengirim laporan kebersihan (Pencurian) di tim R1."},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vars := tc.vars
			vars.BusinessLine = notifications.LineSecurity
			title, body := notifications.Render(tc.typ, vars)
			require.Equal(t, tc.security, [2]string{title, body}, "security")

			vars.BusinessLine = notifications.LineCleaning
			title, body = notifications.Render(tc.typ, vars)
			want := tc.cleaning
			if tc.sameOnBoth {
				want = tc.security
			}
			require.Equal(t, want, [2]string{title, body}, "cleaning")

			// A caller with no business line, or one this build does not know, is a security caller.
			for _, line := range []notifications.BusinessLine{"", "janitorial"} {
				vars.BusinessLine = line
				title, body = notifications.Render(tc.typ, vars)
				require.Equal(t, tc.security, [2]string{title, body}, "line %q", line)
			}
		})
	}
}

// A field the caller leaves out drops its phrase, and the sentence still reads.
func TestRenderDropsWhatIsMissing(t *testing.T) {
	cases := []struct {
		name        string
		typ         notifications.Type
		vars        notifications.Vars
		title, body string
	}{
		{"leave request with nothing", notifications.TypeLeaveRequested, notifications.Vars{},
			"Pengajuan izin baru", "Seorang anggota mengajukan izin. Menunggu keputusan Anda."},
		{"leave request with a name and a date", notifications.TypeLeaveRequested, notifications.Vars{ActorName: "Budi", Date: oct30},
			"Pengajuan izin baru", "Budi mengajukan izin untuk 30 Oktober 2026. Menunggu keputusan Anda."},
		{"a name of only spaces is no name", notifications.TypeLeaveRequested, notifications.Vars{ActorName: "   "},
			"Pengajuan izin baru", "Seorang anggota mengajukan izin. Menunggu keputusan Anda."},
		{"leave approved", notifications.TypeLeaveDecided, notifications.Vars{Approved: true, Date: oct30},
			"Izin disetujui", "Izin Anda untuk 30 Oktober 2026 telah disetujui."},
		{"leave declined, no date", notifications.TypeLeaveDecided, notifications.Vars{},
			"Izin ditolak", "Izin Anda ditolak."},
		{"moved to a regu with no name", notifications.TypeTeamChanged, notifications.Vars{},
			"Perpindahan regu", "Anda dipindahkan ke regu lain."},
		{"a role change with neither a role nor a position", notifications.TypePositionChanged, notifications.Vars{},
			"Pergantian posisi", "Posisi Anda telah diperbarui."},
		{"an instruction from nobody in particular", notifications.TypeInstructionReceived, notifications.Vars{},
			"Instruksi baru", "Instruksi baru dari atasan Anda."},
		{"an instruction with a title", notifications.TypeInstructionReceived, notifications.Vars{ActorName: "Budi", Subject: "Ganti shift"},
			"Instruksi baru", "Instruksi baru dari Budi: Ganti shift."},
		{"feedback", notifications.TypeInstructionFeedback, notifications.Vars{ActorName: "Budi"},
			"Balasan instruksi", "Budi membalas instruksi Anda."},
		{"an offline submission with nothing", notifications.TypeOfflineSubmissionSent, notifications.Vars{},
			"Data offline terkirim", "Data yang Anda simpan saat offline sudah terkirim."},
		{"an offline report", notifications.TypeOfflineSubmissionSent, notifications.Vars{Subject: "Laporan"},
			"Data offline terkirim", "Laporan yang Anda simpan saat offline sudah terkirim."},
		{"a swap", notifications.TypeSwapAssigned, notifications.Vars{ActorName: "Budi", Date: timeutil.NewDate(2026, time.December, 1)},
			"Tukar jaga", "Anda mendapat tukar jaga dengan Budi pada 1 Desember 2026."},
		{"a panic from an unknown person", notifications.TypePanicTriggered, notifications.Vars{},
			"Sinyal Tombol Panik", "Seorang anggota mengirim sinyal tombol panik. Segera beri bantuan."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			title, body := notifications.Render(tc.typ, tc.vars)
			require.Equal(t, tc.title, title)
			require.Equal(t, tc.body, body)
		})
	}
}

func TestRenderNamesEveryMonthInIndonesian(t *testing.T) {
	want := []string{"Januari", "Februari", "Maret", "April", "Mei", "Juni", "Juli", "Agustus", "September", "Oktober", "November", "Desember"}
	for i, name := range want {
		date := timeutil.NewDate(2026, time.Month(i+1), 7)
		_, body := notifications.Render(notifications.TypeLeaveDecided, notifications.Vars{Approved: true, Date: date})
		require.Equal(t, "Izin Anda untuk 7 "+name+" 2026 telah disetujui.", body)
	}
}

// A type this build does not know can only come from a database newer than the code. It still
// gets a text, so that the inbox never shows an empty row.
func TestRenderHasATextForAnUnknownType(t *testing.T) {
	title, body := notifications.Render(notifications.Type("FROM_THE_FUTURE"), notifications.Vars{})
	require.Equal(t, "Notifikasi baru", title)
	require.Equal(t, "Ada pembaruan untuk Anda.", body)
}
