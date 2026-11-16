package core

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/richaction/onebox-collector/internal/channel/wecomkf"
	"github.com/richaction/onebox-collector/internal/model"
	"github.com/richaction/onebox-collector/internal/resolver"
	"github.com/richaction/onebox-collector/internal/store"
	"github.com/richaction/onebox-collector/internal/vault"
	"github.com/richaction/onebox-collector/internal/webdav"
)

type fixture struct {
	Name       string
	Raw        json.RawMessage
	Type       model.Type
	Date, File string
	Contains   []string
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func fakeHTTP(handler http.HandlerFunc) *http.Client {
	return &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		handler(w, r)
		response := w.Result()
		response.Request = r
		return response, nil
	})}
}

const corp = "ww-test"
const token = "callback-token"

var key = bytes.Repeat([]byte{42}, 32)

func signed(t *testing.T, message, recipient string) (string, url.Values) {
	t.Helper()
	plain := make([]byte, 20)
	binary.BigEndian.PutUint32(plain[16:], uint32(len(message)))
	plain = append(plain, []byte(message+recipient)...)
	padding := 32 - len(plain)%32
	plain = append(plain, bytes.Repeat([]byte{byte(padding)}, padding)...)
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	cipher.NewCBCEncrypter(block, key[:16]).CryptBlocks(plain, plain)
	encrypted := base64.StdEncoding.EncodeToString(plain)
	fields := []string{token, "1794791520", "nonce", encrypted}
	slices.Sort(fields)
	signature := fmt.Sprintf("%x", sha1.Sum([]byte(strings.Join(fields, ""))))
	return encrypted, url.Values{"msg_signature": {signature}, "timestamp": {"1794791520"}, "nonce": {"nonce"}}
}

func TestEncryptedCallbackToVaultAndWebDAV(t *testing.T) {
	input, err := os.ReadFile("../../spec/fixtures/wecomkf.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []fixture
	if err := json.Unmarshal(input, &fixtures); err != nil {
		t.Fatal(err)
	}
	var raws []json.RawMessage
	for _, f := range fixtures {
		msg, keep, err := wecomkf.Adapt(f.Raw)
		if err != nil || !keep || msg.Type != f.Type || msg.Date() != f.Date {
			t.Fatalf("%s: got type %s and date %s, keep=%v, error=%v", f.Name, msg.Type, msg.Date(), keep, err)
		}
		raws = append(raws, f.Raw)
	}
	raws = append(raws, json.RawMessage(`{"msgid":"agent-message","origin":4,"send_time":1794792600,"msgtype":"text","text":{"content":"AGENT_ONLY"}}`))
	pulls, tokenCalls, downloads := 0, 0, 0
	png := []byte("\x89PNG\r\n\x1a\nfixture")
	wecom := fakeHTTP(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cgi-bin/gettoken":
			tokenCalls++
			fmt.Fprint(w, `{"errcode":0,"access_token":"access","expires_in":7200}`)
		case "/cgi-bin/kf/sync_msg":
			if r.URL.Query().Get("access_token") != "access" {
				t.Error("missing API access token")
			}
			pulls++
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			if request["open_kfid"] != "kf-1" {
				t.Error("wrong kf account")
			}
			switch request["cursor"] {
			case nil:
				if request["token"] != "read-1" {
					t.Error("missing event token")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"msg_list": raws[:3], "has_more": 1, "next_cursor": "page-1"})
			case "page-1":
				_ = json.NewEncoder(w).Encode(map[string]any{"msg_list": raws[3:], "has_more": 0, "next_cursor": "page-2"})
			case "page-2":
				_ = json.NewEncoder(w).Encode(map[string]any{"msg_list": raws[:1], "has_more": 0, "next_cursor": "page-3"})
			default:
				t.Errorf("unexpected cursor: %v", request["cursor"])
				http.Error(w, "wrong cursor", 400)
			}
		case "/cgi-bin/media/get":
			if r.URL.Query().Get("media_id") != "image-media" {
				t.Error("wrong media id")
			}
			downloads++
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(png)
		default:
			http.NotFound(w, r)
		}
	})
	assets := vault.New(t.TempDir(), 1024)
	state := t.TempDir()
	db, err := store.Open(state)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	base, _ := url.Parse("https://dav.invalid/vault/")
	remote := make(map[string][]byte)
	putCount := 0
	dav := fakeHTTP(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		blob, exists := remote[path]
		switch r.Method {
		case "MKCOL":
			w.WriteHeader(http.StatusCreated)
		case "PROPFIND":
			w.WriteHeader(http.StatusMultiStatus)
			fmt.Fprint(w, `<multistatus xmlns="DAV:">`)
			for name := range remote {
				if strings.HasSuffix(name, " (冲突副本).md") {
					fmt.Fprintf(w, "<response><href>%s</href></response>", (&url.URL{Path: name}).EscapedPath())
				}
			}
			fmt.Fprint(w, "</multistatus>")
		case "MOVE":
			destination, err := url.Parse(r.Header.Get("Destination"))
			if err != nil || !exists || !strings.Contains(destination.Path, "/.conflicts/") || r.Header.Get("Overwrite") != "F" {
				t.Error("unexpected conflict archive request")
				http.Error(w, "wrong request", 400)
				return
			}
			remote[destination.Path] = blob
			delete(remote, path)
			w.WriteHeader(http.StatusCreated)
		case http.MethodPut:
			if !exists && r.Header.Get("If-None-Match") != "*" {
				t.Error("new file PUT is unguarded")
			}
			if exists && r.Header.Get("If-Match") != `"known"` {
				t.Error("existing file PUT is unguarded")
			}
			putCount++
			remote[path], _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
		case http.MethodGet, http.MethodHead:
			if !exists {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("ETag", `"known"`)
			if r.Method == http.MethodGet {
				_, _ = w.Write(blob)
			}
		default:
			t.Errorf("unexpected WebDAV method %s", r.Method)
			http.NotFound(w, r)
		}
	})
	s := &Service{
		DB: db, Vault: assets, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		KF:       &wecomkf.Client{Base: "https://wecom.invalid/cgi-bin", Corp: corp, Secret: "secret", HTTP: wecom},
		Douyin:   &resolver.Douyin{HTTP: fakeHTTP(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }), Assets: assets, Cache: db},
		Official: &resolver.Official{Assets: assets, History: db},
		Sync:     &webdav.Client{Base: base, HTTP: dav, Vault: assets},
	}
	cb, err := wecomkf.NewCallback(corp, token, base64.RawStdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	handler := s.Public("/webhook/kf", cb)
	echo, q := signed(t, "echo+你好", corp)
	q.Set("echostr", echo)
	response := callWebhook(handler, http.MethodGet, q, nil)
	if response.Code != 200 || response.Body.String() != "echo+你好" {
		t.Fatalf("GET: %d %q", response.Code, response.Body.String())
	}
	q.Set("msg_signature", "bad")
	if got := callWebhook(handler, http.MethodGet, q, nil); got.Code != 403 {
		t.Fatalf("bad signature: %d", got.Code)
	}
	event := func(readToken string) string {
		return `<xml><ToUserName>ww-test</ToUserName><MsgType>event</MsgType><Event>kf_msg_or_event</Event><Token>` + readToken + `</Token><OpenKfId>kf-1</OpenKfId></xml>`
	}
	encoded, q := signed(t, "invalid XML", corp)
	if got := callWebhook(handler, http.MethodPost, q, envelope(encoded)); got.Code != 400 {
		t.Fatalf("bad XML: %d", got.Code)
	}
	encoded, q = signed(t, event("foreign"), "different-corp")
	if got := callWebhook(handler, http.MethodPost, q, envelope(encoded)); got.Code != 403 {
		t.Fatalf("bad recipient: %d", got.Code)
	}
	encoded, q = signed(t, event("read-1"), corp)
	for range 2 {
		if got := callWebhook(handler, http.MethodPost, q, envelope(encoded)); got.Code != 200 || got.Body.String() != "success" {
			t.Fatalf("POST: %d %q", got.Code, got.Body.String())
		}
	}
	if pulls != 0 {
		t.Fatal("callback made an upstream request before acknowledging")
	}
	if _, err := os.Stat(assets.NotePath(fixtures[0].Date)); !os.IsNotExist(err) {
		t.Fatal("callback created a note before background processing")
	}
	now := time.Now().Add(time.Second)
	ctx := t.Context()
	requireSteps(t, ctx, now, s.PullOne, 2)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(state)
	if err != nil {
		t.Fatal(err)
	}
	s.DB = db
	s.Douyin.(*resolver.Douyin).Cache = db
	s.Official.(*resolver.Official).History = db
	requireSteps(t, ctx, now, s.WriteOne, len(fixtures))

	encoded, q = signed(t, event("read-2"), corp)
	if got := callWebhook(handler, http.MethodPost, q, envelope(encoded)); got.Code != 200 {
		t.Fatalf("duplicate sync POST: %d", got.Code)
	}
	requireSteps(t, ctx, now, s.PullOne, 1)
	requireSteps(t, ctx, now, s.WriteOne, 0)
	if tokenCalls != 1 || pulls != 3 || downloads != 2 {
		t.Fatalf("API calls: token=%d pull=%d media=%d", tokenCalls, pulls, downloads)
	}

	date := fixtures[0].Date
	note, media, err := assets.ReadDay(date)
	if err != nil {
		t.Fatal(err)
	}
	if len(media) != 1 {
		t.Fatalf("expected one deduplicated image, got %v", media)
	}
	text := string(note)
	if strings.Count(text, "<!-- onebox:") != len(fixtures) || strings.Contains(text, "AGENT_ONLY") {
		t.Fatalf("unexpected entries: %s", text)
	}
	for _, f := range fixtures {
		if filepath.Base(assets.NotePath(f.Date)) != f.File {
			t.Fatalf("%s: wrong path", f.Name)
		}
		for _, want := range f.Contains {
			if !strings.Contains(text, want) {
				t.Errorf("%s: missing %q in %s", f.Name, want, text)
			}
		}
	}
	day, err := db.NextSync(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Sync.Sync(ctx, day.Date); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishSync(ctx, day, now, false); err != nil {
		t.Fatal(err)
	}
	if err := s.Sync.Sync(ctx, day.Date); err != nil {
		t.Fatal(err)
	}
	remoteNote := "/vault/" + date + "/" + date + ".md"
	if putCount != 2 || !bytes.Equal(remote[remoteNote], note) {
		t.Fatalf("sync PUT count %d", putCount)
	}
	copyName := "/vault/" + date + "/" + date + " (冲突副本).md"
	remote[copyName] = append(bytes.Clone(note), []byte("## 10:02\n手工追加的内容\n")...)
	if err := s.Sync.Sync(ctx, date); err != nil {
		t.Fatal(err)
	}
	updated, _, err := assets.ReadDay(date)
	if err != nil || !bytes.Contains(updated, []byte("手工追加的内容")) || !bytes.Equal(remote[remoteNote], updated) {
		t.Fatalf("conflict merge: %s, %v", updated, err)
	}
	if _, exists := remote[copyName]; exists {
		t.Fatal("conflict copy not archived")
	}
	if putCount != 3 {
		t.Fatalf("conflict PUT count %d", putCount)
	}
}

func callWebhook(handler http.Handler, method string, q url.Values, body io.Reader) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(method, "/webhook/kf?"+q.Encode(), body))
	return w
}

func envelope(s string) io.Reader {
	return strings.NewReader("<xml><Encrypt>" + s + "</Encrypt></xml>")
}

func requireSteps(t *testing.T, ctx context.Context, now time.Time, step func(context.Context, time.Time) (bool, error), count int) {
	t.Helper()
	for i := 0; i <= count; i++ {
		did, err := step(ctx, now)
		if err != nil || did != (i < count) {
			t.Fatalf("step %d (expected %d): did=%v err=%v", i, count, did, err)
		}
	}
}
