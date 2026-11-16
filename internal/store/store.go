package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/richaction/onebox-collector/internal/model"
	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS notifications (
  id TEXT PRIMARY KEY, account TEXT NOT NULL, token TEXT NOT NULL,
  created INTEGER NOT NULL, retry_at INTEGER NOT NULL, attempts INTEGER NOT NULL DEFAULT 0,
  state TEXT NOT NULL DEFAULT 'received', note TEXT
);
CREATE INDEX IF NOT EXISTS notifications_due ON notifications(state, retry_at);
CREATE TABLE IF NOT EXISTS cursors (account TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS messages (
  channel TEXT NOT NULL, msg_id TEXT NOT NULL, sender_id TEXT NOT NULL,
  msg_type TEXT NOT NULL, send_time INTEGER NOT NULL, date TEXT NOT NULL, payload BLOB NOT NULL,
  prepared BLOB, state TEXT NOT NULL DEFAULT 'received', level INTEGER NOT NULL DEFAULT 0, note TEXT,
  retry_count INTEGER NOT NULL DEFAULT 0, retry_at INTEGER NOT NULL,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
  PRIMARY KEY(channel, msg_id)
);
CREATE INDEX IF NOT EXISTS messages_due ON messages(state, retry_at);
CREATE INDEX IF NOT EXISTS messages_date ON messages(date, state);
CREATE TABLE IF NOT EXISTS sync_days (
  date TEXT PRIMARY KEY, due INTEGER NOT NULL, attempts INTEGER NOT NULL DEFAULT 0, revision INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE IF NOT EXISTS resolve_cache (
  cache_key TEXT PRIMARY KEY, payload BLOB NOT NULL, expires_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS official_fetch (
  url_hash TEXT PRIMARY KEY, status TEXT NOT NULL, fetched_at INTEGER NOT NULL
);
`

type DB struct {
	*sql.DB
	dir, dsn string
}

func Open(dir string) (*DB, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(dir, "collector.sqlite"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	name, err := filepath.Abs(file.Name())
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return nil, errors.Join(err, closeErr)
	}
	dsn := url.URL{Scheme: "file", Path: name, RawQuery: "_pragma=busy_timeout(2500)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)"}
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &DB{DB: db, dir: filepath.Dir(name), dsn: dsn.String()}, nil
}

type Notification struct {
	ID, Account, Token string
	Attempts           int
	Created            int64
}

func (db *DB) Notify(ctx context.Context, account, token string, now time.Time) error {
	id := fmt.Sprintf("%x", sha256.Sum256([]byte(account+"\x00"+token)))
	_, err := db.ExecContext(ctx, `INSERT INTO notifications(id,account,token,created,retry_at)
VALUES(?,?,?,?,?) ON CONFLICT DO NOTHING`, id, account, token, now.Unix(), now.Unix())
	return err
}

func (db *DB) NextNotification(ctx context.Context, now time.Time) (Notification, error) {
	var n Notification
	err := db.QueryRowContext(ctx, `SELECT id,account,token,attempts,created FROM notifications
WHERE state='received' AND retry_at<=? ORDER BY created,rowid LIMIT 1`, now.Unix()).Scan(&n.ID, &n.Account, &n.Token, &n.Attempts, &n.Created)
	return n, err
}

func (db *DB) Cursor(ctx context.Context, account string) (string, error) {
	var cursor string
	err := db.QueryRowContext(ctx, "SELECT value FROM cursors WHERE account=?", account).Scan(&cursor)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return cursor, err
}

// SavePage advances the cursor only when all messages on the page are durable.
func (db *DB) SavePage(ctx context.Context, n Notification, cursor string, final bool, messages []model.Message, now time.Time) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, m := range messages {
		payload, err := json.Marshal(m)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO messages(channel,msg_id,sender_id,msg_type,send_time,date,payload,retry_at,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`, m.Channel, m.ID, m.SenderID, m.Type, m.Sent.Unix(), m.Date(), payload, now.Unix(), now.Unix(), now.Unix()); err != nil {
			return err
		}
	}
	if cursor != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO cursors(account,value) VALUES(?,?) ON CONFLICT(account) DO UPDATE SET value=excluded.value`, n.Account, cursor); err != nil {
			return err
		}
	}
	if final {
		if _, err := tx.ExecContext(ctx, "UPDATE notifications SET state='done',token='',note=NULL WHERE id=?", n.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

var retryDelays = [...]time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute}

func Exhausted(attempts int) bool { return attempts >= len(retryDelays) }

func retry(attempts int, now time.Time) (state string, next int64) {
	if Exhausted(attempts) {
		return "failed", 0
	}
	return "received", now.Add(retryDelays[attempts]).Unix()
}

func (db *DB) RetryNotification(ctx context.Context, n Notification, now time.Time, cause error) error {
	state, at := retry(n.Attempts, now)
	_, err := db.ExecContext(ctx, "UPDATE notifications SET state=?,retry_at=?,attempts=attempts+1,note=?,token=CASE WHEN ?='failed' THEN '' ELSE token END WHERE id=?",
		state, at, cause.Error(), state, n.ID)
	return err
}

type Task struct {
	Message  model.Message
	Attempts int
	Prepared *model.Result
}

func (db *DB) NextMessage(ctx context.Context, now time.Time) (Task, error) {
	var t Task
	var payload, prepared []byte
	err := db.QueryRowContext(ctx, `SELECT payload,retry_count,prepared FROM messages WHERE state='received' AND retry_at<=?
ORDER BY send_time,rowid LIMIT 1`, now.Unix()).Scan(&payload, &t.Attempts, &prepared)
	if err != nil {
		return t, err
	}
	if len(prepared) > 0 {
		t.Prepared = new(model.Result)
		if err := json.Unmarshal(prepared, t.Prepared); err != nil {
			return t, err
		}
	}
	return t, json.Unmarshal(payload, &t.Message)
}

func (db *DB) RetryMessage(ctx context.Context, t Task, now time.Time, cause error) error {
	state, at := retry(t.Attempts, now)
	_, err := db.ExecContext(ctx, `UPDATE messages SET state=?,retry_at=?,retry_count=retry_count+1,updated_at=?,note=? WHERE channel=? AND msg_id=?`,
		state, at, now.Unix(), cause.Error(), t.Message.Channel, t.Message.ID)
	return err
}

func (db *DB) Prepare(ctx context.Context, m model.Message, result model.Result) error {
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, "UPDATE messages SET prepared=? WHERE channel=? AND msg_id=?", data, m.Channel, m.ID)
	return err
}

func (db *DB) Written(ctx context.Context, m model.Message, result model.Result, sync bool, now time.Time) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	state := "synced"
	if sync {
		state = "written"
		if _, err := tx.ExecContext(ctx, `INSERT INTO sync_days(date,due) VALUES(?,?) ON CONFLICT(date) DO UPDATE SET revision=revision+1`, m.Date(), now.Unix()); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET state=?,level=?,note=?,updated_at=?,prepared=NULL WHERE channel=? AND msg_id=?`,
		state, result.Level, result.Note, now.Unix(), m.Channel, m.ID); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) Replay(ctx context.Context, channel, id string, now time.Time) (bool, error) {
	res, err := db.ExecContext(ctx, `UPDATE messages SET state='received',retry_at=?,retry_count=0,updated_at=?,note=NULL
WHERE channel=? AND msg_id=? AND state='failed'`, now.Unix(), now.Unix(), channel, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

type SyncDay struct {
	Date     string
	Attempts int
	Revision int64
}

func (db *DB) NextSync(ctx context.Context, now time.Time) (SyncDay, error) {
	var day SyncDay
	err := db.QueryRowContext(ctx, "SELECT date,attempts,revision FROM sync_days WHERE due<=? ORDER BY due LIMIT 1", now.Unix()).Scan(&day.Date, &day.Attempts, &day.Revision)
	return day, err
}

func (db *DB) FinishSync(ctx context.Context, day SyncDay, now time.Time, failed bool) error {
	if failed {
		_, due := retry(day.Attempts, now)
		attempt := day.Attempts + 1
		if due == 0 {
			due, attempt = now.Add(6*time.Hour).Unix(), 0
		}
		_, err := db.ExecContext(ctx, "UPDATE sync_days SET due=?,attempts=? WHERE date=? AND revision=?", due, attempt, day.Date, day.Revision)
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	deleted, err := tx.ExecContext(ctx, "DELETE FROM sync_days WHERE date=? AND revision=?", day.Date, day.Revision)
	if err != nil {
		return err
	}
	n, err := deleted.RowsAffected()
	if err != nil {
		return err
	}
	if n == 1 {
		if _, err := tx.ExecContext(ctx, "UPDATE messages SET state='synced',updated_at=? WHERE date=? AND state='written'", now.Unix(), day.Date); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (db *DB) CacheGet(ctx context.Context, key string, target any) (bool, error) {
	var value []byte
	err := db.QueryRowContext(ctx, "SELECT payload FROM resolve_cache WHERE cache_key=? AND expires_at>?", key, time.Now().Unix()).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(value, target)
}

func (db *DB) CachePut(ctx context.Context, key string, value any, ttl time.Duration) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO resolve_cache VALUES(?,?,?) ON CONFLICT(cache_key) DO UPDATE SET payload=excluded.payload,expires_at=excluded.expires_at`,
		key, data, time.Now().Add(ttl).Unix())
	return err
}

func (db *DB) OfficialStatus(ctx context.Context, link string) (string, error) {
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(link)))
	var status string
	err := db.QueryRowContext(ctx, "SELECT status FROM official_fetch WHERE url_hash=? AND fetched_at>?", key, time.Now().Add(-time.Hour).Unix()).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return status, err
}

func (db *DB) SetOfficialStatus(ctx context.Context, link, status string) error {
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(link)))
	_, err := db.ExecContext(ctx, `INSERT INTO official_fetch VALUES(?,?,?) ON CONFLICT(url_hash) DO UPDATE SET status=excluded.status,fetched_at=excluded.fetched_at`,
		key, status, time.Now().Unix())
	return err
}

func (db *DB) Prune(ctx context.Context, now time.Time) error {
	cutoff := now.AddDate(0, 0, -7).Unix()
	if _, err := db.ExecContext(ctx, "DELETE FROM messages WHERE state='synced' AND updated_at<?", cutoff); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, "DELETE FROM notifications WHERE state='done' AND created<?", cutoff); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, "DELETE FROM resolve_cache WHERE expires_at<=?", now.Unix())
	return err
}
