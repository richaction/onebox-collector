package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/richaction/onebox-collector/internal/channel/wecomkf"
	"github.com/richaction/onebox-collector/internal/model"
	"github.com/richaction/onebox-collector/internal/store"
	"github.com/richaction/onebox-collector/internal/vault"
)

type Media interface {
	FetchMedia(context.Context, string) (io.ReadCloser, error)
}

type KF interface {
	Media
	Pull(ctx context.Context, account, token, cursor string) (wecomkf.Page, error)
}

type VideoResolver interface {
	Resolve(context.Context, model.Message, bool) (model.Result, bool)
}

type ArticleResolver interface {
	Resolve(context.Context, model.Message, string) model.Result
}

type Syncer interface {
	Sync(context.Context, string) error
}

type Service struct {
	DB       *store.DB
	Vault    *vault.Writer
	KF       KF
	Douyin   VideoResolver
	Official ArticleResolver
	Sync     Syncer
	Log      *slog.Logger
	metrics  metrics
}

// Pulling callback notifications stays independent of slower media/Markdown tasks.
func (s *Service) PullOne(ctx context.Context, now time.Time) (bool, error) {
	n, err := s.DB.NextNotification(ctx, now)
	if err != nil {
		return false, noWork(err)
	}
	if err := s.pullPage(ctx, n, now); err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		s.Log.Warn("notification failed", "account", n.Account, "attempt", n.Attempts+1, "terminal", store.Exhausted(n.Attempts), "error", err)
		return true, s.DB.RetryNotification(ctx, n, now, err)
	}
	return true, nil
}

func (s *Service) pullPage(ctx context.Context, n store.Notification, now time.Time) error {
	cursor, err := s.DB.Cursor(ctx, n.Account)
	if err != nil {
		return err
	}
	token := n.Token
	if now.Sub(time.Unix(n.Created, 0)) >= 9*time.Minute {
		token = ""
	}
	page, err := s.KF.Pull(ctx, n.Account, token, cursor)
	if err != nil {
		return err
	}
	messages := make([]model.Message, 0, len(page.Raw))
	for _, raw := range page.Raw {
		msg, keep, err := wecomkf.Adapt(raw)
		if err != nil {
			return fmt.Errorf("E_ADAPT: %w", err)
		}
		if keep {
			messages = append(messages, msg)
		}
	}
	return s.DB.SavePage(ctx, n, page.Cursor, !page.More, messages, now)
}

func noWork(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}

func (s *Service) WriteOne(ctx context.Context, now time.Time) (bool, error) {
	t, err := s.DB.NextMessage(ctx, now)
	if err != nil {
		return false, noWork(err)
	}
	start := time.Now()
	result, err := s.process(ctx, t)
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if err != nil {
		s.Log.Warn("message failed", "msg_id", t.Message.ID, "type", t.Message.Type, "attempt", t.Attempts+1,
			"terminal", store.Exhausted(t.Attempts), "error", err)
		return true, s.DB.RetryMessage(ctx, t, now, err)
	}
	if err := s.DB.Written(ctx, t.Message, result, s.Sync != nil, now); err != nil {
		return false, err
	}
	if t.Message.Type == model.Douyin {
		s.metrics.observeDouyin(result, time.Since(start))
	}
	s.Log.Info("note written", "msg_id", t.Message.ID, "date", t.Message.Date(), "type", t.Message.Type,
		"level", result.Level, "reason", result.Reason)
	return true, nil
}

func (s *Service) process(ctx context.Context, t store.Task) (model.Result, error) {
	m := t.Message
	if t.Prepared == nil {
		result, err := s.resolve(ctx, t)
		if err != nil {
			return result, err
		}
		if err := s.DB.Prepare(ctx, m, result); err != nil {
			return result, err
		}
		t.Prepared = &result
	}
	if err := ctx.Err(); err != nil {
		return *t.Prepared, err
	}
	if err := s.Vault.Append(m, *t.Prepared); err != nil {
		return *t.Prepared, fmt.Errorf("E_WRITE: %w", err)
	}
	return *t.Prepared, nil
}

func (s *Service) resolve(ctx context.Context, t store.Task) (model.Result, error) {
	m := t.Message
	var result model.Result
	switch m.Type {
	case model.Image:
		var err error
		result.Media, err = s.channelImage(ctx, m.Date(), m.MediaID)
		if err != nil {
			if !errors.Is(err, vault.ErrAssetSize) && !errors.Is(err, vault.ErrAssetFormat) && !errors.Is(err, vault.ErrLowSpace) {
				return result, fmt.Errorf("download image: %w", err)
			}
			result.Note = "图片未归档：" + err.Error()
		}
	case model.Douyin:
		var retry bool
		result, retry = s.Douyin.Resolve(ctx, m, t.Attempts > 0)
		if retry && !store.Exhausted(t.Attempts) {
			return result, errors.New(result.Note)
		}
	case model.Official:
		var cover string
		if m.CoverID != "" {
			cover, _ = s.channelImage(ctx, m.Date(), m.CoverID)
		}
		result = s.Official.Resolve(ctx, m, cover)
	}
	return result, ctx.Err()
}

func (s *Service) channelImage(ctx context.Context, date, id string) (string, error) {
	if id == "" {
		return "", vault.ErrAssetFormat
	}
	body, err := s.KF.FetchMedia(ctx, id)
	if err != nil {
		return "", err
	}
	defer body.Close()
	path, _, err := s.Vault.StoreAsset(date, "image", body)
	return path, err
}

func (s *Service) RunPuller(ctx context.Context) {
	s.run(ctx, func(now time.Time) (bool, error) { return s.PullOne(ctx, now) })
}

func (s *Service) RunWriter(ctx context.Context) {
	s.run(ctx, func(now time.Time) (bool, error) { return s.WriteOne(ctx, now) })
}

func (s *Service) RunMaintenance(ctx context.Context) {
	timer := time.NewTicker(time.Minute)
	defer timer.Stop()
	var lastHour time.Time
	var backupDate string
	lowSpace := false
	for ctx.Err() == nil {
		now := time.Now()
		if free, ratio, err := s.Vault.Space(); err == nil {
			s.metrics.free.Store(free)
			if ratio < 0.10 && !lowSpace {
				s.Log.Warn("Vault disk below 10% available", "available_bytes", free)
			}
			lowSpace = ratio < 0.10
		}
		if lastHour.IsZero() || now.Sub(lastHour) >= time.Hour {
			if err := s.DB.Prune(ctx, now); err != nil && ctx.Err() == nil {
				s.Log.Error("prune state", "error", err)
			}
			lastHour = now
		}
		date := now.In(model.Shanghai).Format(time.DateOnly)
		if backupDate != date {
			if err := s.DB.Backup(ctx, now); err == nil {
				backupDate = date
			} else if ctx.Err() == nil {
				s.Log.Error("SQLite backup failed", "error", err)
			}
		}
		select {
		case <-ctx.Done():
		case <-timer.C:
		}
	}
}

func (s *Service) RunSyncer(ctx context.Context) {
	s.run(ctx, func(now time.Time) (bool, error) {
		day, err := s.DB.NextSync(ctx, now)
		if err != nil {
			return false, noWork(err)
		}
		syncCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		err = s.Sync.Sync(syncCtx, day.Date)
		cancel()
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if err != nil {
			s.Log.Warn("WebDAV sync failed", "date", day.Date, "attempt", day.Attempts+1, "error", err)
		}
		return true, s.DB.FinishSync(ctx, day, time.Now(), err != nil)
	})
}

func (s *Service) run(ctx context.Context, step func(time.Time) (bool, error)) {
	for ctx.Err() == nil {
		didWork, err := step(time.Now())
		if err == nil && didWork {
			continue
		}
		if err != nil && ctx.Err() == nil {
			s.Log.Error("worker", "error", err)
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
		case <-timer.C:
		}
		timer.Stop()
	}
}
