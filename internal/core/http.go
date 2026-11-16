package core

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/richaction/onebox-collector/internal/channel/wecomkf"
)

func (s *Service) Public(callbackPath string, callback *wecomkf.Callback) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+callbackPath, func(w http.ResponseWriter, r *http.Request) {
		body, err := callback.Verify(r.URL.Query())
		if err != nil {
			s.callbackError(w, r, err)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(body)
	})
	mux.HandleFunc("POST "+callbackPath, func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		defer func() { s.metrics.observeCallback(time.Since(start)) }()
		ctx, cancel := context.WithTimeout(r.Context(), 2500*time.Millisecond)
		defer cancel()
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256<<10))
		if err != nil {
			s.callbackError(w, r, wecomkf.ErrDecrypt)
			return
		}
		event, err := callback.Receive(r.URL.Query(), body)
		if err != nil {
			s.callbackError(w, r, err)
			return
		}
		if event.Kind == "event" && event.Name == "kf_msg_or_event" {
			if err := s.DB.Notify(ctx, event.Account, event.Token, start); err != nil {
				s.Log.Error("persist callback", "event", event.Name, "error", err)
				http.Error(w, "E_QUEUE", http.StatusInternalServerError)
				return
			}
		}
		s.Log.Debug("callback accepted", "event", event.Name, "elapsed_ms", time.Since(start).Milliseconds())
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "success")
	})
	return mux
}

func (s *Service) callbackError(w http.ResponseWriter, r *http.Request, err error) {
	code, status := "E_DECRYPT", http.StatusBadRequest
	switch {
	case errors.Is(err, wecomkf.ErrSign):
		code, status = "E_SIGN", http.StatusForbidden
	case errors.Is(err, wecomkf.ErrCorp):
		code, status = "E_CORP", http.StatusForbidden
	}
	s.Log.Warn("callback rejected", "method", r.Method, "code", code)
	http.Error(w, code, status)
}

func (s *Service) Internal(replayToken string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if s.DB.PingContext(ctx) != nil || s.Vault.Check() != nil {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ok")
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		s.metrics.write(w, s.Vault.BytesStored())
	})
	mux.HandleFunc("POST /admin/replay/{msg_id}", func(w http.ResponseWriter, r *http.Request) {
		if replayToken == "" {
			http.NotFound(w, r)
			return
		}
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(token), []byte(replayToken)) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		queued, err := s.DB.Replay(r.Context(), "kf", r.PathValue("msg_id"), time.Now())
		if err != nil {
			s.Log.Error("replay failed", "error", err)
			http.Error(w, "E_QUEUE", http.StatusInternalServerError)
			return
		}
		if !queued {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	return mux
}
