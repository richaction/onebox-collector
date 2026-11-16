package core

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/richaction/onebox-collector/internal/model"
	"github.com/richaction/onebox-collector/internal/store"
	"github.com/richaction/onebox-collector/internal/vault"
)

type unavailableVideo struct{ calls int }

func (v *unavailableVideo) Resolve(context.Context, model.Message, bool) (model.Result, bool) {
	v.calls++
	return model.Result{Level: 2, Title: "保留标题", Note: "视频下载失败"}, true
}

func TestRetryExhaustionAndPreparedRecovery(t *testing.T) {
	now := time.Now()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := t.Context()
	m := model.Message{Channel: "kf", ID: "retry-id", Sent: now, Type: model.Douyin, URL: "https://v.douyin.com/video"}
	if err := db.SavePage(ctx, store.Notification{}, "", false, []model.Message{m}, now); err != nil {
		t.Fatal(err)
	}
	writer := vault.New(t.TempDir(), 1024)
	video := &unavailableVideo{}
	s := Service{DB: db, Vault: writer, Douyin: video, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, delta := range []time.Duration{0, time.Minute, 6 * time.Minute, 36 * time.Minute} {
		requireSteps(t, ctx, now.Add(delta), s.WriteOne, 1)
	}
	if video.calls != 4 {
		t.Fatalf("retried %d times", video.calls-1)
	}
	body, _, err := writer.ReadDay(m.Date())
	if err != nil || !strings.Contains(string(body), m.URL) || !strings.Contains(string(body), "视频下载失败") || !strings.Contains(string(body), "保留标题") {
		t.Fatalf("lost final fallback: %s, %v", body, err)
	}

	m.ID = "prepared-id"
	result := model.Result{Level: 3, Note: "复用上次解析结果"}
	if err := db.SavePage(ctx, store.Notification{}, "", false, []model.Message{m}, now); err != nil {
		t.Fatal(err)
	}
	if err := db.Prepare(ctx, m, result); err != nil {
		t.Fatal(err)
	}
	if err := writer.Append(m, result); err != nil {
		t.Fatal(err)
	}
	// Simulate interruption after atomic rename but before DB.Written.
	requireSteps(t, ctx, now, s.WriteOne, 1)
	body, err = os.ReadFile(writer.NotePath(m.Date()))
	if err != nil || strings.Count(string(body), "复用上次解析结果") != 1 || video.calls != 4 {
		t.Fatalf("failed to recover committed note: %s, calls=%d error=%v", body, video.calls, err)
	}
}

func TestFailedMessageReplay(t *testing.T) {
	now := time.Now()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := t.Context()
	m := model.Message{Channel: "kf", ID: "terminal", Sent: now, Type: model.Image}
	if err := db.SavePage(ctx, store.Notification{}, "", false, []model.Message{m}, now); err != nil {
		t.Fatal(err)
	}
	for _, delta := range []time.Duration{0, time.Minute, 6 * time.Minute, 36 * time.Minute} {
		task, err := db.NextMessage(ctx, now.Add(delta))
		if err != nil {
			t.Fatal(err)
		}
		if err := db.RetryMessage(ctx, task, now.Add(delta), errors.New("failed")); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := db.Replay(ctx, "kf", "terminal", now); err != nil || !ok {
		t.Fatalf("replay %v %v", ok, err)
	}
	task, err := db.NextMessage(ctx, now)
	if err != nil || task.Attempts != 0 || task.Message.ID != "terminal" {
		t.Fatalf("queued task: %+v %v", task, err)
	}
}
