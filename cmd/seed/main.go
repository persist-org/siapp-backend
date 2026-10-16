// Command seed prepares a development environment.
//
// For identity it seeds nothing: people come from Persist Personalia, and SiAPP only caches
// them when they log in. In development it prints the accounts of the real kpi-personalia-api
// seed (db/seed/40_siapp.sql, which personeltest mirrors), so a developer knows who can log
// in. Later phases add the masters and a demo organization.
package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"siapp/pkg/platform/personel/personeltest"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	fmt.Println("SiAPP signs people in with their Personalia account. These accounts come from")
	fmt.Println("kpi-personalia-api's dev seed (run its `make seed` first). Every password is:")
	fmt.Printf("\n    %s\n\n", personeltest.Password)

	accounts := []struct{ email, nik, note string }{
		{personeltest.EmailDanruSatu, personeltest.NIKDanruSatu, "PT Siaga Satu, team leader (DANRU), siapp-only account"},
		{personeltest.EmailAnggota1Satu, personeltest.NIKAnggota1Satu, "PT Siaga Satu, member, placed at the mall client"},
		{personeltest.EmailAnggota2Satu, personeltest.NIKAnggota2Satu, "PT Siaga Satu, member"},
		{personeltest.EmailSupervisorSatu, personeltest.NIKSupervisorSatu, "PT Siaga Satu, also uses the Personalia app (personalia domain)"},
		{personeltest.EmailNonaktifSatu, personeltest.NIKNonaktifSatu, "PT Siaga Satu, deactivated: login is refused (AUTH_ACCOUNT_DISABLED)"},
		{personeltest.EmailDanruDua, personeltest.NIKDanruDua, "PT Siaga Dua, team leader (DANRU): another tenant"},
		{personeltest.EmailAnggota1Dua, personeltest.NIKAnggota1Dua, "PT Siaga Dua, member"},
		{personeltest.EmailAnggota2Dua, personeltest.NIKAnggota2Dua, "PT Siaga Dua, member"},
		{personeltest.EmailNoNIK, "-", "no NIK: login is refused (AUTH_ACCOUNT_DISABLED)"},
	}
	var table strings.Builder
	w := tabwriter.NewWriter(&table, 0, 0, 2, ' ', 0)
	// A tabwriter over a strings.Builder cannot fail to write; only Flush reports.
	_, _ = fmt.Fprintln(w, "EMAIL\tNIK\tWHO")
	for _, a := range accounts {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", a.email, a.nik, a.note)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("format accounts: %w", err)
	}
	fmt.Print(table.String())
	return nil
}
