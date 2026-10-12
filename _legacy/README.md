# _legacy: first-build code waiting to be rebuilt

**Not compiled.** Go ignores directories whose name starts with `_`, so nothing here is built, vetted, linted or tested.

This is the code of the first plan (MySQL, local users) that a later rebuild phase adapts. It was parked here in Phase 00.1 so the build stays green while the phases that replace it are not done yet. Paths mirror where the code lived before (`internal/platform/...`).

| Code | Rebuilt in | What happens to it |
|---|---|---|
| `internal/platform/database`, `db/embed.go`, `sqlc.yaml` | 00.2 | Rewritten for pgx + `WithTenant` |
| `internal/platform/testkit`, `pkg/platform/httpx/health_test.go` | 00.2 | Rewritten for Postgres, `siapp_app` and the Personel stub (00.5) |
| `internal/platform/audit`, `internal/platform/idem` | 00.4 | Audit adapted (tenant + RLS); idem replaced by the `Idempotency-Key` middleware |
| `internal/platform/auth` (`tokens.go`, `middleware.go`, `sessions.go`, `revocations.go`, `cleanup.go`) | 01.1, 01.2 | Kept and adapted |
| `internal/platform/auth` (`service.go`, `seed.go`, `handler.go`, the local-password login) | 01.1 | Dropped: login goes through Personalia |
| `internal/app` | 01.1 | Its wiring returns in `cmd/api` and testkit |
| `cmd/seed`, `cmd/worker` | 01.1, 01.2 | Rebuilt for the new identity model |

The full first build (with the users, passwords and mail code that was deleted on 2026-10-09) is the tag **`mysql-plan-final`**: `git show mysql-plan-final:<path>`.

**Delete this whole directory** once Phase 01.3 is done.
