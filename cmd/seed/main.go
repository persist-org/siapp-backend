// Command seed prepares a development environment and bootstraps the first admin.
//
//	seed                                      print who can log in (the Personalia dev accounts)
//	seed grant-admin --org <id> --nik <nik>   make a person the first ORG_ADMIN of an organization
//
// For identity it seeds nothing: people come from Persist Personalia, and SiAPP only caches
// them. Without arguments it prints the accounts of the real kpi-personalia-api seed
// (db/seed/40_siapp.sql, which personeltest mirrors), so a developer knows who can log in.
// Later phases add the masters and a demo organization.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	// Embeds the timezone database, like cmd/api, for the same distroless image.
	_ "time/tzdata"

	"siapp/pkg/access"
	"siapp/pkg/identity"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/config"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/personel"
	"siapp/pkg/platform/personel/personeltest"
)

const grantTimeout = time.Minute

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return printAccounts()
	}
	switch args[0] {
	case "grant-admin":
		return grantAdmin(args[1:])
	default:
		return fmt.Errorf("unknown command %q; use: seed | seed grant-admin --org <id> --nik <nik>", args[0])
	}
}

// grantAdmin makes a person the first ORG_ADMIN of an organization.
//
// ⚠ This is the ONLY way to create an admin without an existing one: every admin endpoint
// needs an admin, so the first has to come from outside the API. It runs as the database
// OWNER role (MIGRATE_DSN), inside the organization's tenant transaction (row-level security
// still applies to the owner, FORCE ROW LEVEL SECURITY), and is audited as a grant with no
// granting admin.
//
// A person who has never signed in is fetched from Personalia by NIK first (People.Ensure),
// so the first admin can be set up before anyone has logged in. A NIK that does not belong
// to the organization is refused: the lookup is scoped to it.
func grantAdmin(args []string) error {
	fs := flag.NewFlagSet("grant-admin", flag.ContinueOnError)
	org := fs.Int64("org", 0, "organization id (Personalia company id)")
	nik := fs.String("nik", "", "NIK of the person to make ORG_ADMIN")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *org <= 0 || strings.TrimSpace(*nik) == "" {
		return fmt.Errorf("usage: seed grant-admin --org <id> --nik <nik>")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.MigrateDSN == "" {
		return fmt.Errorf("MIGRATE_DSN (the owner role) is required to grant the first admin")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, grantTimeout)
	defer cancel()

	pool, err := database.Open(ctx, cfg.MigrateDSN)
	if err != nil {
		return err
	}
	defer pool.Close()
	client, err := personel.New(personel.Config{BaseURL: cfg.PersonelBaseURL, Key: cfg.PersonelIntegrationKey})
	if err != nil {
		return fmt.Errorf("personel client: %w", err)
	}

	personnelID, err := identity.NewPeople(pool, client, clock.System{}).Ensure(ctx, *org, *nik)
	if err != nil {
		return fmt.Errorf("find the person (NIK %s in organization %d): %w", *nik, *org, err)
	}
	if err := access.GrantFirstAdmin(ctx, pool, clock.System{}, *org, personnelID); err != nil {
		return err
	}
	fmt.Printf("done: personnel %d of organization %d is ORG_ADMIN\n", personnelID, *org)
	return nil
}

func printAccounts() error {
	fmt.Println("SiAPP signs people in with their Personalia account. These accounts come from")
	fmt.Println("kpi-personalia-api's dev seed (run its `make seed` first). Every password is:")
	fmt.Printf("\n    %s\n\n", personeltest.Password)
	fmt.Println("Web login needs a web role: grant the first admin with")
	fmt.Println("    go run ./cmd/seed grant-admin --org 900101 --nik SIAPPA0001")
	fmt.Println()

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
