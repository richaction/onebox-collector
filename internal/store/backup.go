package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/richaction/onebox-collector/internal/model"
)

// Backup copies a consistent SQLite snapshot using a separate connection so a
// long read does not occupy the connection serving callback acknowledgements.
func (db *DB) Backup(ctx context.Context, now time.Time) error {
	dir := filepath.Join(db.dir, "backups")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	date := now.In(model.Shanghai).Format(time.DateOnly)
	target := filepath.Join(dir, date+".sqlite")
	if _, err := os.Stat(target); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temp, err := os.CreateTemp(dir, ".backup-")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if err := temp.Close(); err != nil {
		return err
	}
	conn, err := sql.Open("sqlite", db.dsn)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "VACUUM INTO ?", temp.Name()); err != nil {
		return err
	}
	file, err := os.OpenFile(temp.Name(), os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	err = errors.Join(file.Sync(), file.Close())
	if err != nil {
		return err
	}
	if err := os.Rename(temp.Name(), target); err != nil {
		return err
	}
	folder, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := errors.Join(folder.Sync(), folder.Close()); err != nil {
		return err
	}
	cutoff := now.In(model.Shanghai).AddDate(0, 0, -6).Format(time.DateOnly)
	files, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, file := range files {
		stamp, ok := strings.CutSuffix(file.Name(), ".sqlite")
		if !ok || !file.Type().IsRegular() || stamp >= cutoff {
			continue
		}
		if _, err := time.Parse(time.DateOnly, stamp); err != nil {
			continue
		}
		if err := os.Remove(filepath.Join(dir, file.Name())); err != nil {
			return err
		}
	}
	return nil
}
