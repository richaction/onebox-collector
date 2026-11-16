package resolver

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/richaction/onebox-collector/internal/model"
	"github.com/richaction/onebox-collector/internal/store"
	"github.com/richaction/onebox-collector/internal/vault"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (fn roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func replay(handler http.HandlerFunc) *http.Client {
	return &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		handler(w, r)
		resp := w.Result()
		resp.Request = r
		return resp, nil
	})}
}

func TestDouyinReplay(t *testing.T) {
	html, err := os.ReadFile("testdata/douyin.html")
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	assets := vault.New(t.TempDir(), 1024)
	details, cdn := 0, 0
	d := &Douyin{Assets: assets, Cache: db, MaxDuration: time.Minute, HTTP: replay(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Host {
		case "v.douyin.com":
			if r.URL.Path == "/expired" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Location", "https://www.douyin.com/video/7581111111111111111")
			w.WriteHeader(http.StatusFound)
		case "www.iesdouyin.com":
			details++
			_, _ = w.Write(html)
		case "cdn.invalid":
			if r.URL.Path == "/cover.png" {
				fmt.Fprint(w, "\x89PNG\r\n\x1a\ncover")
				return
			}
			cdn++
			if r.Header.Get("Referer") != "https://www.douyin.com/video/7581111111111111111" {
				t.Error("missing referer")
			}
			if cdn == 1 {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			fmt.Fprint(w, "\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom")
		default:
			t.Errorf("unexpected URL %s", r.URL)
			http.NotFound(w, r)
		}
	})}
	msg := model.Message{Sent: time.Unix(1794792000, 0), URL: "https://v.douyin.com/short"}
	result, retry := d.Resolve(t.Context(), msg, false)
	if result.Level != 2 || !retry || result.Cover == "" || result.Title != "示例标题" || result.Author != "示例作者" {
		t.Fatalf("download failure: %+v retry=%v", result, retry)
	}
	result, retry = d.Resolve(t.Context(), msg, true)
	if result.Level != 1 || retry || !strings.HasSuffix(result.Media, ".mp4") {
		t.Fatalf("download success: %+v retry=%v", result, retry)
	}
	result, retry = d.Resolve(t.Context(), msg, false)
	if result.Level != 1 || retry || details != 2 {
		t.Fatalf("cache miss: %+v details=%d", result, details)
	}
	msg.URL = "https://v.douyin.com/expired"
	result, retry = d.Resolve(t.Context(), msg, false)
	if result.Level != 3 || retry || result.Reason != "短链失效" {
		t.Fatalf("expired: %+v", result)
	}
}

func TestOfficialReplay(t *testing.T) {
	html, err := os.ReadFile("testdata/official.html")
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	assets := vault.New(t.TempDir(), 1024)
	requests := 0
	o := &Official{Assets: assets, History: db, Enabled: true, Timeout: time.Second, MaxImages: 1, HTTP: replay(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch r.URL.Path {
		case "/s/article":
			_, _ = w.Write(html)
		case "/s/blocked":
			w.Header().Set("Location", "https://mp.weixin.qq.com/mp/verify")
			w.WriteHeader(http.StatusFound)
		case "/article.png":
			if r.Header.Get("Referer") != "https://mp.weixin.qq.com/s/article" {
				t.Error("missing referer")
			}
			fmt.Fprint(w, "\x89PNG\r\n\x1a\narticle")
		default:
			t.Errorf("unexpected URL %s", r.URL)
			http.NotFound(w, r)
		}
	})}
	msg := model.Message{Sent: time.Unix(1794792000, 0), URL: "https://mp.weixin.qq.com/s/article"}
	result := o.Resolve(t.Context(), msg, "")
	if result.Level != 2 || result.Title != "正文标题" || !strings.Contains(result.Body, "**文章正文**") ||
		!strings.Contains(result.Body, "![正文图](assets/") || strings.Contains(result.Body, "去除广告") ||
		!strings.Contains(result.Note, "数量上限") {
		t.Fatalf("archive: %+v", result)
	}
	msg.URL = "https://mp.weixin.qq.com/s/blocked"
	result = o.Resolve(t.Context(), msg, "")
	if result.Level != 1 || !strings.Contains(result.Note, "页面需要验证") {
		t.Fatalf("blocked: %+v", result)
	}
	result = o.Resolve(t.Context(), msg, "")
	if result.Level != 1 || !strings.Contains(result.Note, "近期") || requests != 3 {
		t.Fatalf("blocked cache: %+v requests=%d", result, requests)
	}
}
