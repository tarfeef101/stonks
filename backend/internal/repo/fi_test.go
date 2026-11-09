// FI institution + credential store tests (plan step 9.1, migration
// 0010): one shared postgres:18 testcontainer (newTestEnv) with
// subtests covering the global institution CRUD, the per-user
// credential round trip (BOTH secrets sealed as ONE pack each),
// list order by institution code, per-user isolation, the not-found
// sentinels, and the 23505 duplicate rules.
package repo

import (
	"errors"
	"strings"
	"testing"
	"time"

	"finance-tracker/internal/crypto"
)

func TestFIStore(t *testing.T) {
	env := newTestEnv(t, "fia-a", "fia-b")
	inst := NewInstitutionStore(env.pool)
	if inst == nil {
		t.Fatal("non-nil pool must give non-nil store")
	}
	if NewInstitutionStore(nil) != nil {
		t.Fatal("nil pool must give nil store")
	}
	creds := NewFICredentialStore(env.pool)
	if creds == nil {
		t.Fatal("non-nil pool must give non-nil store")
	}
	if NewFICredentialStore(nil) != nil {
		t.Fatal("nil pool must give nil store")
	}
	a := env.user(t, "fia-a")
	b := env.user(t, "fia-b")
	ctx := env.ctx
	aid, bid := a.row.ID, b.row.ID

	t.Run("institution create + get + get by code", func(t *testing.T) {
		zeta, err := inst.Create(ctx, InstitutionIn{Code: "fia-zeta", Name: "Zeta Bank"})
		if err != nil {
			t.Fatalf("create zeta: %v", err)
		}
		alpha, err := inst.Create(ctx, InstitutionIn{Code: "fia-alpha", Name: "Alpha Bank"})
		if err != nil {
			t.Fatalf("create alpha: %v", err)
		}
		// status defaults to 'active' (0010 CHECK).
		if alpha.Status != "active" || zeta.Status != "active" {
			t.Fatalf("default status = %q / %q, want active", alpha.Status, zeta.Status)
		}
		if got, err := inst.Get(ctx, alpha.ID); err != nil || got != alpha {
			t.Fatalf("get: got %+v (err %v), want %+v", got, err, alpha)
		}
		if got, err := inst.GetByCode(ctx, "fia-alpha"); err != nil || got != alpha {
			t.Fatalf("get by code: got %+v (err %v), want %+v", got, err, alpha)
		}
		if _, err := inst.Get(ctx, zeroUUID); !errors.Is(err, ErrInstitutionNotFound) {
			t.Fatalf("get zeroUUID: err = %v, want ErrInstitutionNotFound", err)
		}
		if _, err := inst.GetByCode(ctx, "fia-none"); !errors.Is(err, ErrInstitutionNotFound) {
			t.Fatalf("get by code none: err = %v, want ErrInstitutionNotFound", err)
		}
	})

	t.Run("institution list orders by code", func(t *testing.T) {
		out, err := inst.List(ctx)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		// Zeta was created first; code order must put alpha first.
		if len(out) != 2 || out[0].Code != "fia-alpha" || out[1].Code != "fia-zeta" {
			t.Fatalf("list: %+v", out)
		}
	})

	t.Run("institution duplicate code is ErrInstitutionCodeTaken", func(t *testing.T) {
		if _, err := inst.Create(ctx, InstitutionIn{Code: "fia-alpha", Name: "Duplicate"}); !errors.Is(err, ErrInstitutionCodeTaken) {
			t.Fatalf("duplicate code: err = %v, want ErrInstitutionCodeTaken", err)
		}
	})

	var credA, credZ Credential
	t.Run("credential create + get round trip seals both secrets", func(t *testing.T) {
		alpha, err := inst.GetByCode(ctx, "fia-alpha")
		if err != nil {
			t.Fatalf("lookup alpha: %v", err)
		}
		token, refresh := "fia-access-token", "fia-refresh-token"
		created, err := creds.Create(ctx, aid, FICredentialIn{
			InstitutionID: alpha.ID,
			Token:         token,
			RefreshToken:  refresh,
		}, a.dek)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if created.UserID != aid || created.InstitutionID != alpha.ID {
			t.Fatalf("created: %+v", created)
		}
		// The join carries the institution code + name for the wire DTO.
		if created.InstitutionCode != "fia-alpha" || created.InstitutionName != "Alpha Bank" {
			t.Fatalf("created join: %+v", created)
		}
		if created.LastSyncedAt != nil {
			t.Fatalf("fresh credential has last_synced_at = %v, want nil", created.LastSyncedAt)
		}
		credA = created
		if got, err := creds.Get(ctx, aid, created.ID, a.dek); err != nil || got != created {
			t.Fatalf("get: got %+v (err %v), want %+v", got, err, created)
		}

		// pack != plaintext: neither stored pack contains the secret's
		// bytes (the mortgage 7.1 no-plaintext rule, per secret).
		var tokenText, refreshText string
		if err := env.pool.QueryRow(ctx,
			`SELECT token_encrypted::text, refresh_token_encrypted::text
		       FROM fi_credentials WHERE id = $1`,
			created.ID).Scan(&tokenText, &refreshText); err != nil {
			t.Fatalf("read packs: %v", err)
		}
		if strings.Contains(tokenText, token) {
			t.Fatal("token_encrypted contains the plaintext token")
		}
		if strings.Contains(refreshText, refresh) {
			t.Fatal("refresh_token_encrypted contains the plaintext refresh token")
		}
		// open == plaintext: the stored packs open back to the exact
		// secrets under the user's own DEK.
		var tokenPack, refreshPack []byte
		if err := env.pool.QueryRow(ctx,
			`SELECT token_encrypted, refresh_token_encrypted
		       FROM fi_credentials WHERE id = $1`,
			created.ID).Scan(&tokenPack, &refreshPack); err != nil {
			t.Fatalf("read pack bytes: %v", err)
		}
		if plain, err := crypto.Open(a.dek, tokenPack); err != nil || string(plain) != token {
			t.Fatalf("token pack open = %q (err %v), want the plaintext token", string(plain), err)
		}
		if plain, err := crypto.Open(a.dek, refreshPack); err != nil || string(plain) != refresh {
			t.Fatalf("refresh pack open = %q (err %v), want the plaintext refresh token", string(plain), err)
		}
	})

	t.Run("mark synced stamps last_synced_at", func(t *testing.T) {
		at := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
		if err := creds.MarkSynced(ctx, aid, credA.ID, at); err != nil {
			t.Fatalf("mark synced: %v", err)
		}
		got, err := creds.Get(ctx, aid, credA.ID, a.dek)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.LastSyncedAt == nil || !got.LastSyncedAt.Equal(at) {
			t.Fatalf("last_synced_at = %v, want %v", got.LastSyncedAt, at)
		}
		// Scoping: neither the other user's id nor a zero id may be
		// stamped.
		if err := creds.MarkSynced(ctx, bid, credA.ID, at); !errors.Is(err, ErrFICredentialNotFound) {
			t.Fatalf("mark synced foreign: err = %v, want ErrFICredentialNotFound", err)
		}
		if err := creds.MarkSynced(ctx, aid, zeroUUID, at); !errors.Is(err, ErrFICredentialNotFound) {
			t.Fatalf("mark synced zeroUUID: err = %v, want ErrFICredentialNotFound", err)
		}
	})

	t.Run("credential list orders by institution code, per-user", func(t *testing.T) {
		zeta, err := inst.GetByCode(ctx, "fia-zeta")
		if err != nil {
			t.Fatalf("lookup zeta: %v", err)
		}
		credZ, err = creds.Create(ctx, aid, FICredentialIn{
			InstitutionID: zeta.ID,
			Token:         "fia-zeta-token",
			RefreshToken:  "fia-zeta-refresh",
		}, a.dek)
		if err != nil {
			t.Fatalf("create zeta credential: %v", err)
		}
		out, err := creds.List(ctx, aid, a.dek)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		// Alpha sorts before zeta even though the zeta credential was
		// created second (ORDER BY i.code, not created_at).
		if len(out) != 2 || out[0].InstitutionCode != "fia-alpha" || out[1].InstitutionCode != "fia-zeta" {
			t.Fatalf("list: %+v", out)
		}
		if out[1].ID != credZ.ID {
			t.Fatalf("list zeta row = %s, want %s", out[1].ID, credZ.ID)
		}
		// Per-user scoping: user b sees none of a's credentials.
		if others, err := creds.List(ctx, bid, b.dek); err != nil || len(others) != 0 {
			t.Fatalf("list b: got %+v (err %v), want empty", others, err)
		}
	})

	t.Run("credential not-found sentinels", func(t *testing.T) {
		// A foreign user's id is not disclosed (same rule as mortgages).
		if _, err := creds.Get(ctx, bid, credA.ID, b.dek); !errors.Is(err, ErrFICredentialNotFound) {
			t.Fatalf("get foreign: err = %v, want ErrFICredentialNotFound", err)
		}
		if _, err := creds.Get(ctx, aid, zeroUUID, a.dek); !errors.Is(err, ErrFICredentialNotFound) {
			t.Fatalf("get zeroUUID: err = %v, want ErrFICredentialNotFound", err)
		}
		if err := creds.Delete(ctx, bid, credA.ID); !errors.Is(err, ErrFICredentialNotFound) {
			t.Fatalf("delete foreign: err = %v, want ErrFICredentialNotFound", err)
		}
		if err := creds.Delete(ctx, aid, zeroUUID); !errors.Is(err, ErrFICredentialNotFound) {
			t.Fatalf("delete zeroUUID: err = %v, want ErrFICredentialNotFound", err)
		}
		// The row survived the failed foreign delete.
		if got, err := creds.Get(ctx, aid, credA.ID, a.dek); err != nil || got.ID != credA.ID {
			t.Fatalf("get after foreign delete: %+v (err %v)", got, err)
		}
	})

	t.Run("duplicate credential is ErrFICredentialExists", func(t *testing.T) {
		alpha, err := inst.GetByCode(ctx, "fia-alpha")
		if err != nil {
			t.Fatalf("lookup alpha: %v", err)
		}
		if _, err := creds.Create(ctx, aid, FICredentialIn{
			InstitutionID: alpha.ID,
			Token:         "dup-token",
			RefreshToken:  "dup-refresh",
		}, a.dek); !errors.Is(err, ErrFICredentialExists) {
			t.Fatalf("duplicate credential: err = %v, want ErrFICredentialExists", err)
		}
		// UNIQUE(user_id, institution_id): the SAME institution is
		// fine for a different user.
		if _, err := creds.Create(ctx, bid, FICredentialIn{
			InstitutionID: alpha.ID,
			Token:         "b-token",
			RefreshToken:  "b-refresh",
		}, b.dek); err != nil {
			t.Fatalf("create b credential: %v", err)
		}
	})

	t.Run("delete removes, then ErrFICredentialNotFound", func(t *testing.T) {
		if err := creds.Delete(ctx, aid, credA.ID); err != nil {
			t.Fatalf("delete: %v", err)
		}
		out, err := creds.List(ctx, aid, a.dek)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(out) != 1 || out[0].InstitutionCode != "fia-zeta" {
			t.Fatalf("list after delete: %+v", out)
		}
		if _, err := creds.Get(ctx, aid, credA.ID, a.dek); !errors.Is(err, ErrFICredentialNotFound) {
			t.Fatalf("get deleted: err = %v, want ErrFICredentialNotFound", err)
		}
		if err := creds.Delete(ctx, aid, credA.ID); !errors.Is(err, ErrFICredentialNotFound) {
			t.Fatalf("delete again: err = %v, want ErrFICredentialNotFound", err)
		}
	})
}

// TestFICredentialListAll covers the 9.2 worker-enumeration path:
// every user's ACTIVE-institution credentials, ordered by user then
// institution code, packs ciphertext (never decrypted here), disabled
// institutions skipped silently, last_synced_at round trip.
func TestFICredentialListAll(t *testing.T) {
	env := newTestEnv(t, "fi-all-a", "fi-all-b")
	inst := NewInstitutionStore(env.pool)
	creds := NewFICredentialStore(env.pool)
	a := env.user(t, "fi-all-a")
	b := env.user(t, "fi-all-b")
	ctx := env.ctx

	alpha, err := inst.Create(ctx, InstitutionIn{Code: "fi-all-alpha", Name: "Alpha All"})
	if err != nil {
		t.Fatalf("create alpha: %v", err)
	}
	zeta, err := inst.Create(ctx, InstitutionIn{Code: "fi-all-zeta", Name: "Zeta All"})
	if err != nil {
		t.Fatalf("create zeta: %v", err)
	}
	off, err := inst.Create(ctx, InstitutionIn{Code: "fi-all-off", Name: "Off Bank"})
	if err != nil {
		t.Fatalf("create off: %v", err)
	}

	// user a: two ACTIVE credentials; user b: one ACTIVE. The disabled
	// institution's credential must never appear in ListAll.
	credAAlpha, err := creds.Create(ctx, a.row.ID, FICredentialIn{
		InstitutionID: alpha.ID, Token: "fi-all-a-alpha-token", RefreshToken: "fi-all-a-alpha-refresh",
	}, a.dek)
	if err != nil {
		t.Fatalf("create a/alpha: %v", err)
	}
	if _, err := creds.Create(ctx, a.row.ID, FICredentialIn{
		InstitutionID: zeta.ID, Token: "fi-all-a-zeta-token", RefreshToken: "fi-all-a-zeta-refresh",
	}, a.dek); err != nil {
		t.Fatalf("create a/zeta: %v", err)
	}
	credBAlpha, err := creds.Create(ctx, b.row.ID, FICredentialIn{
		InstitutionID: alpha.ID, Token: "fi-all-b-alpha-token", RefreshToken: "fi-all-b-alpha-refresh",
	}, b.dek)
	if err != nil {
		t.Fatalf("create b/alpha: %v", err)
	}
	if _, err := creds.Create(ctx, b.row.ID, FICredentialIn{
		InstitutionID: off.ID, Token: "fi-all-b-off-token", RefreshToken: "fi-all-b-off-refresh",
	}, b.dek); err != nil {
		t.Fatalf("create b/off: %v", err)
	}
	if _, err := env.pool.Exec(ctx,
		`UPDATE fi_institutions SET status = 'disabled' WHERE id = $1`, off.ID); err != nil {
		t.Fatalf("disable off: %v", err)
	}

	out, err := creds.ListAll(ctx)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("list all: got %d rows, want 3 (disabled institution skipped)", len(out))
	}
	// Order: by user_id first, institution code second (user_id is a
	// random uuid, so assert the ordering property, not the user order).
	for i := 1; i < len(out); i++ {
		if out[i-1].UserID > out[i].UserID {
			t.Fatalf("rows not ordered by user_id: %q before %q", out[i-1].UserID, out[i].UserID)
		}
		if out[i-1].UserID == out[i].UserID && out[i-1].InstitutionCode >= out[i].InstitutionCode {
			t.Fatalf("rows not ordered by institution code within user %q", out[i].UserID)
		}
	}
	wantPairs := map[string]map[string]bool{
		a.row.ID: {"fi-all-alpha": true, "fi-all-zeta": true},
		b.row.ID: {"fi-all-alpha": true},
	}
	seen := map[string]map[string]bool{a.row.ID: {}, b.row.ID: {}}
	for _, r := range out {
		// The join carries code + name for the job's notification titles.
		wantName := map[string]string{"fi-all-alpha": "Alpha All", "fi-all-zeta": "Zeta All"}[r.InstitutionCode]
		if r.InstitutionName != wantName {
			t.Fatalf("row %s: institution name = %q, want %q", r.InstitutionCode, r.InstitutionName, wantName)
		}
		if !wantPairs[r.UserID][r.InstitutionCode] {
			t.Fatalf("unexpected row: user %s institution %q", r.UserID, r.InstitutionCode)
		}
		seen[r.UserID][r.InstitutionCode] = true
	}
	if !seen[a.row.ID]["fi-all-alpha"] || !seen[a.row.ID]["fi-all-zeta"] || !seen[b.row.ID]["fi-all-alpha"] {
		t.Fatalf("missing rows: %+v", out)
	}
	// Ciphertext: the returned packs hold no plaintext and open ONLY
	// under the owning user's DEK (the job opens them per-user).
	for _, r := range out {
		ownerDek, otherDek := a.dek, b.dek
		if r.UserID == b.row.ID {
			ownerDek, otherDek = b.dek, a.dek
		}
		if _, err := crypto.Open(ownerDek, r.TokenPack); err != nil {
			t.Fatalf("token pack for user %s does not open under its own DEK: %v", r.UserID, err)
		}
		if _, err := crypto.Open(ownerDek, r.RefreshTokenPack); err != nil {
			t.Fatalf("refresh pack for user %s does not open under its own DEK: %v", r.UserID, err)
		}
		if _, err := crypto.Open(otherDek, r.TokenPack); err == nil {
			t.Fatalf("token pack for user %s opened under the OTHER user's DEK", r.UserID)
		}
		if _, err := crypto.Open(otherDek, r.RefreshTokenPack); err == nil {
			t.Fatalf("refresh pack for user %s opened under the OTHER user's DEK", r.UserID)
		}
	}
	// last_synced_at: nil until the first MarkSynced, then stamped and
	// carried back by ListAll (the job's dedup/telemetry surface).
	for _, r := range out {
		if r.LastSyncedAt != nil {
			t.Fatalf("fresh row %s has last_synced_at = %v, want nil", r.ID, r.LastSyncedAt)
		}
	}
	at := time.Date(2026, 11, 8, 9, 5, 0, 0, time.UTC)
	if err := creds.MarkSynced(ctx, a.row.ID, credAAlpha.ID, at); err != nil {
		t.Fatalf("mark synced: %v", err)
	}
	if err := creds.MarkSynced(ctx, b.row.ID, credBAlpha.ID, at); err != nil {
		t.Fatalf("mark synced b: %v", err)
	}
	out, err = creds.ListAll(ctx)
	if err != nil {
		t.Fatalf("list all after sync: %v", err)
	}
	for _, r := range out {
		if r.ID == credAAlpha.ID || r.ID == credBAlpha.ID {
			if r.LastSyncedAt == nil || !r.LastSyncedAt.Equal(at) {
				t.Fatalf("row %s last_synced_at = %v, want %v", r.ID, r.LastSyncedAt, at)
			}
		} else {
			if r.LastSyncedAt != nil {
				t.Fatalf("row %s last_synced_at = %v, want nil", r.ID, r.LastSyncedAt)
			}
		}
	}
}
