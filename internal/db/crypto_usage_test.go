package db

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func setupCryptoUsageDB(t *testing.T) (*Repo, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.sqlite")
	database, err := OpenDatabase(dbPath)
	if err != nil {
		t.Fatalf("OpenDatabase: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	_, err = database.Exec(`
		CREATE TABLE IF NOT EXISTS providerConnections (
			id TEXT PRIMARY KEY,
			provider TEXT NOT NULL,
			authType TEXT NOT NULL,
			name TEXT,
			email TEXT,
			priority INTEGER DEFAULT 0,
			isActive INTEGER DEFAULT 1,
			data TEXT NOT NULL,
			createdAt TEXT NOT NULL,
			updatedAt TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS usageDaily (
			dateKey TEXT PRIMARY KEY,
			data TEXT NOT NULL
		);
	`)
	if err != nil {
		t.Fatalf("create schema: %v", err)
	}
	return NewRepo(database), dbPath
}

func TestProtectAndUnprotectConnectionData_AESGCM(t *testing.T) {
	t.Setenv("CREDENTIAL_ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

	raw := `{"apiKey":"sk-secret-123","accessToken":"tok-abc","refreshToken":"ref-xyz","projectId":"proj-1","backoffLevel":2}`
	protected, err := ProtectConnectionData(raw)
	if err != nil {
		t.Fatalf("ProtectConnectionData: %v", err)
	}
	if strings.Contains(protected, "sk-secret-123") || strings.Contains(protected, "tok-abc") || strings.Contains(protected, "ref-xyz") {
		t.Fatalf("plaintext secret leaked in protected JSON: %s", protected)
	}
	if !strings.Contains(protected, `"projectId":"proj-1"`) {
		t.Fatalf("expected non-secret projectId preserved in JSON: %s", protected)
	}

	unprotected, err := UnprotectConnectionData(protected)
	if err != nil {
		t.Fatalf("UnprotectConnectionData: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(unprotected), &got); err != nil {
		t.Fatalf("unmarshal unprotected: %v", err)
	}
	if got["apiKey"] != "sk-secret-123" || got["accessToken"] != "tok-abc" || got["refreshToken"] != "ref-xyz" || got["projectId"] != "proj-1" {
		t.Fatalf("unexpected round-trip map: %+v", got)
	}

	// Missing encryption key must refuse encrypted values instead of passing ciphertext.
	t.Setenv("CREDENTIAL_ENCRYPTION_KEY", "")
	if _, err := UnprotectConnectionData(protected); !errors.Is(err, ErrMissingCredentialKey) {
		t.Fatalf("expected ErrMissingCredentialKey, got %v", err)
	}
	// Legacy plaintext remains readable when key is unset.
	legacy, err := UnprotectConnectionData(raw)
	if err != nil || legacy != raw {
		t.Fatalf("expected legacy plaintext passthrough, got %q err=%v", legacy, err)
	}
}

func TestMigrateEncryptConnections_MigratesAndPreservesModelLock(t *testing.T) {
	repo, _ := setupCryptoUsageDB(t)
	if _, err := repo.RawDB().Exec(
		`INSERT INTO providerConnections (id, provider, authType, name, isActive, data, createdAt, updatedAt) VALUES ('c1', 'antigravity', 'oauth', 'test', 1, '{"accessToken":"tok-live","refreshToken":"ref-live","projectId":"p1"}', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
	); err != nil {
		t.Fatalf("seed plaintext connection: %v", err)
	}

	t.Setenv("CREDENTIAL_ENCRYPTION_KEY", "my-passphrase-for-linux-server")
	migrated, err := MigrateEncryptConnections(repo.RawDB())
	if err != nil {
		t.Fatalf("MigrateEncryptConnections: %v", err)
	}
	if migrated != 1 {
		t.Fatalf("expected 1 migrated row, got %d", migrated)
	}

	var storedRaw string
	if err := repo.RawDB().QueryRow("SELECT data FROM providerConnections WHERE id = 'c1'").Scan(&storedRaw); err != nil {
		t.Fatalf("query stored raw: %v", err)
	}
	if strings.Contains(storedRaw, "tok-live") || strings.Contains(storedRaw, "ref-live") {
		t.Fatalf("expected encrypted secrets in SQLite, got %s", storedRaw)
	}

	// LockConnectionModel uses SQLite json_set on top of encrypted connection data.
	if err := repo.LockConnectionModel("c1", "gemini-3-flash", 60, 1); err != nil {
		t.Fatalf("LockConnectionModel: %v", err)
	}
	locked, err := repo.IsConnectionModelLocked("c1", "gemini-3-flash")
	if err != nil || !locked {
		t.Fatalf("expected model lock active, locked=%v err=%v", locked, err)
	}

	conn, err := repo.GetProviderConnectionByID("c1")
	if err != nil {
		t.Fatalf("GetProviderConnectionByID: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(conn.Data), &decoded); err != nil {
		t.Fatalf("decode decrypted data: %v", err)
	}
	if decoded["accessToken"] != "tok-live" || decoded["refreshToken"] != "ref-live" {
		t.Fatalf("expected decrypted tokens after lock, got %+v", decoded)
	}
}

func TestMergeUsageDaily_ConcurrentMultiHandleAtomic(t *testing.T) {
	repo1, dbPath := setupCryptoUsageDB(t)
	db2, err := OpenDatabase(dbPath)
	if err != nil {
		t.Fatalf("OpenDatabase second handle: %v", err)
	}
	defer db2.Close()
	repo2 := NewRepo(db2)

	const dateKey = "2026-11-20"
	const workersPerRepo = 10

	var wg sync.WaitGroup
	errs := make(chan error, workersPerRepo*2)
	runWorker := func(r *Repo) {
		defer wg.Done()
		err := r.MergeUsageDaily(dateKey, func(existing string) (string, error) {
			m := map[string]int{"requests": 0, "promptTokens": 0}
			if existing != "" {
				_ = json.Unmarshal([]byte(existing), &m)
			}
			m["requests"]++
			m["promptTokens"] += 10
			b, err := json.Marshal(m)
			return string(b), err
		})
		if err != nil {
			errs <- err
		}
	}

	for i := 0; i < workersPerRepo; i++ {
		wg.Add(2)
		go runWorker(repo1)
		go runWorker(repo2)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("MergeUsageDaily concurrent error: %v", err)
	}

	raw, err := repo1.GetUsageDaily(dateKey)
	if err != nil {
		t.Fatalf("GetUsageDaily: %v", err)
	}
	var got map[string]int
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("unmarshal merged usage: %v", err)
	}
	if got["requests"] != workersPerRepo*2 || got["promptTokens"] != workersPerRepo*2*10 {
		t.Fatalf("lost atomic increments: got %+v, want requests=%d promptTokens=%d", got, workersPerRepo*2, workersPerRepo*2*10)
	}
}
