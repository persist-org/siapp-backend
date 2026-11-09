package notifications

import (
	"strconv"
	"strings"

	"siapp/internal/db"
	"siapp/pkg/platform/timeutil"
)

// labels are the words of a text that change with the business line. They come from the alias
// table of persist-ecosystem/docs/00-overview/01-product-overview.md §3.1: the labels follow
// the line of the project, they are not hard-coded for security.
type labels struct {
	danru  string // DANRU / Ka-Regu, or the cleaning line's Supervisor
	member string // the team role MEMBER
	team   string // regu or tim
	patrol string // Patroli, or Rute Pembersihan
	guard  string // Jaga, or Zona Kerja Area
	report string // Laporan, or Laporan Kebersihan
	panic  string // Sinyal Tombol Panik, or Sinyal Panik
}

// labelsFor returns the labels of a line. An empty or unknown line gets the security ones: the
// line is a property of the project, and a caller that has none to give is a security caller.
func labelsFor(line BusinessLine) labels {
	switch line {
	case LineCleaning:
		return labels{
			danru: "Supervisor", member: "Anggota Tim", team: "Tim", patrol: "Rute Pembersihan",
			guard: "Zona Kerja Area", report: "Laporan Kebersihan", panic: "Sinyal Panik",
		}
	case LineSecurity:
	}
	return labels{
		danru: "DANRU", member: "Anggota", team: "Regu", patrol: "Patroli",
		guard: "Jaga", report: "Laporan", panic: "Sinyal Tombol Panik",
	}
}

// Render returns the title and the body of a notification, in Indonesian, with the labels of
// v.BusinessLine. It is total: it never returns an empty title or body, whatever v holds.
//
// The switch has no default on purpose. The exhaustive linter then fails the build for a Type
// that has no case, so a type cannot ship with a generic text by accident. The generic text at
// the end is for a value this build does not know, which only a database newer than the code
// can produce.
func Render(t Type, v Vars) (title, body string) {
	l := labelsFor(v.BusinessLine)
	actor := orDefault(v.ActorName, "Seorang anggota")
	date := v.Date

	switch t {
	case TypeLeaveRequested:
		return "Pengajuan izin baru",
			actor + " mengajukan izin" + onDate(" untuk ", date) + ". Menunggu keputusan Anda."
	case TypeAbsenceMarked:
		return "Anda ditandai mangkir",
			"Anda ditandai mangkir" + onDate(" pada ", date) + by(" oleh "+l.danru+" ", v.ActorName) + "."
	case TypeReportSubmitted:
		return l.report + " baru",
			actor + " mengirim " + strings.ToLower(l.report) + with(" (", v.Subject, ")") + onTeam(l, v.TeamName) + "."
	case TypeInstructionReceived:
		return "Instruksi baru",
			"Instruksi baru dari " + orDefault(v.ActorName, "atasan Anda") + with(": ", v.Subject) + "."
	case TypeInstructionFeedback:
		return "Balasan instruksi",
			actor + " membalas instruksi Anda" + with(": ", v.Subject) + "."
	case TypeTeamChanged:
		return "Perpindahan " + strings.ToLower(l.team),
			"Anda dipindahkan ke " + strings.ToLower(l.team) + " " + orDefault(v.TeamName, "lain") + onDate(" mulai ", date) + "."
	case TypePositionChanged:
		return "Pergantian posisi", positionChangedBody(l, v)
	case TypePatrolSubmitted:
		return l.patrol + " menunggu persetujuan",
			actor + " mengirim " + strings.ToLower(l.patrol) + with(" ", v.Subject) + ". Menunggu persetujuan Anda."
	case TypePatrolApproved:
		return l.patrol + " disetujui",
			l.patrol + with(" ", v.Subject) + " Anda telah disetujui" + by(" oleh ", v.ActorName) + "."
	case TypeGuardSubmitted:
		return l.guard + " menunggu persetujuan",
			actor + " mengirim " + strings.ToLower(l.guard) + with(" ", v.Subject) + ". Menunggu persetujuan Anda."
	case TypeGuardApproved:
		return l.guard + " disetujui",
			l.guard + with(" ", v.Subject) + " Anda telah disetujui" + by(" oleh ", v.ActorName) + "."
	case TypePanicTriggered:
		return l.panic,
			actor + " mengirim " + strings.ToLower(l.panic) + with(" (", v.Subject, ")") + onTeam(l, v.TeamName) + ". Segera beri bantuan."
	case TypePanicResolved:
		return l.panic + " selesai",
			l.panic + by(" dari ", v.ActorName) + " telah ditandai selesai."
	case TypeOfflineSubmissionSent:
		return "Data offline terkirim",
			orDefault(v.Subject, "Data") + " yang Anda simpan saat offline sudah terkirim."
	case TypeLeaveDecided:
		if v.Approved {
			return "Izin disetujui", "Izin Anda" + onDate(" untuk ", date) + " telah disetujui."
		}
		return "Izin ditolak", "Izin Anda" + onDate(" untuk ", date) + " ditolak."
	case TypeLeaveCancelled:
		return "Pengajuan izin dibatalkan",
			actor + " membatalkan pengajuan izin" + onDate(" untuk ", date) + "."
	case TypeSwapAssigned:
		return "Tukar jaga",
			"Anda mendapat tukar jaga" + by(" dengan ", v.ActorName) + onDate(" pada ", date) + "."
	}
	return "Notifikasi baru", "Ada pembaruan untuk Anda."
}

// positionChangedBody is the one text that has two causes: a new team role in a regu (organization,
// Phase 02.2), whose word follows the business line, and a new position from Personel, whose name
// is Personel's own.
func positionChangedBody(l labels, v Vars) string {
	switch v.TeamRole {
	case db.TeamRoleDANRU:
		return "Peran Anda di " + strings.ToLower(l.team) + with(" ", v.TeamName) + " sekarang " + l.danru + onDate(" mulai ", v.Date) + "."
	case db.TeamRoleMEMBER:
		return "Peran Anda di " + strings.ToLower(l.team) + with(" ", v.TeamName) + " sekarang " + l.member + onDate(" mulai ", v.Date) + "."
	}
	if v.PositionName != "" {
		return "Posisi Anda sekarang " + v.PositionName + "."
	}
	return "Posisi Anda telah diperbarui."
}

func orDefault(s, fallback string) string {
	if s = strings.TrimSpace(s); s != "" {
		return s
	}
	return fallback
}

// with returns the parts joined, when the middle one is not empty: with(": ", subject) is
// ": Cuti" or "". An optional closing part follows the value.
func with(parts ...string) string {
	value := strings.TrimSpace(parts[1])
	if value == "" {
		return ""
	}
	out := parts[0] + value
	if len(parts) > 2 {
		out += parts[2]
	}
	return out
}

// by is with() for a name; the prefix carries its own trailing space when it needs one.
func by(prefix, name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	return strings.TrimRight(prefix, " ") + " " + name
}

func onTeam(l labels, teamName string) string {
	return with(" di "+strings.ToLower(l.team)+" ", teamName)
}

// onDate returns the prefix and the date in Indonesian ("30 Oktober 2026"), or "" for the zero
// Date.
func onDate(prefix string, d timeutil.Date) string {
	if d.IsZero() {
		return ""
	}
	t := d.Time()
	return prefix + strconv.Itoa(t.Day()) + " " + monthNames[t.Month()-1] + " " + strconv.Itoa(t.Year())
}

var monthNames = [12]string{
	"Januari", "Februari", "Maret", "April", "Mei", "Juni",
	"Juli", "Agustus", "September", "Oktober", "November", "Desember",
}
