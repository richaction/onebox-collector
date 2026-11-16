package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/richaction/onebox-collector/internal/model"
)

func TestBackupSnapshotContainsPendingNotifications(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Date(2026, 11, 16, 0, 0, 0, 0, model.Shanghai)
	if err := db.Notify(t.Context(), "kf", "token", now); err != nil {
		t.Fatal(err)
	}
	if err := db.Backup(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	if err := db.Backup(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	snapshot, err := sql.Open("sqlite", filepath.Join(dir, "backups", "2026-11-16.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = snapshot.Close() })
	var account string
	if err := snapshot.QueryRow("SELECT account FROM notifications").Scan(&account); err != nil || account != "kf" {
		t.Fatalf("%q %v", account, err)
	}
}
