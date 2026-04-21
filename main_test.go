package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestParseResponseMetaJSON(t *testing.T) {
	body := []byte(`{
		"model":"llama-actual",
		"usage":{"prompt_tokens":481,"completion_tokens":712,"total_tokens":1193},
		"timings":{"prompt_ms":120.5,"predicted_ms":356.25}
	}`)
	meta := parseResponseMeta(http.Header{"Content-Type": []string{"application/json"}}, body)
	if meta.Model != "llama-actual" {
		t.Fatalf("model=%q", meta.Model)
	}
	if meta.PromptTokens != 481 || meta.CompletionTok != 712 || meta.TotalTokens != 1193 {
		t.Fatalf("unexpected tokens: %+v", meta)
	}
	if meta.PromptMs != 120.5 || meta.CompletionMs != 356.25 {
		t.Fatalf("unexpected timings: %+v", meta)
	}
}

func TestParseResponseMetaSSE(t *testing.T) {
	body := []byte(strings.Join([]string{
		`data: {"model":"stream-model","choices":[{"delta":{"content":"hi"}}]}`,
		`data: {"usage":{"prompt_tokens":12,"completion_tokens":34,"total_tokens":46},"timings":{"prompt_ms":20,"predicted_ms":80}}`,
		`data: [DONE]`,
		"",
	}, "\n"))
	meta := parseResponseMeta(http.Header{"Content-Type": []string{"text/event-stream"}}, body)
	if meta.Model != "stream-model" {
		t.Fatalf("model=%q", meta.Model)
	}
	if meta.PromptTokens != 12 || meta.CompletionTok != 34 || meta.TotalTokens != 46 {
		t.Fatalf("unexpected tokens: %+v", meta)
	}
}

func TestParseResponseMetaLlamaCppFallbackFields(t *testing.T) {
	body := []byte(`{
		"model":"llama-fallback",
		"tokens_evaluated": 77,
		"tokens_predicted": 123,
		"tokens_evaluated_ms": 910.5,
		"tokens_predicted_ms": 2222.25
	}`)
	meta := parseResponseMeta(http.Header{"Content-Type": []string{"application/json; charset=utf-8"}}, body)
	if meta.Model != "llama-fallback" {
		t.Fatalf("model=%q", meta.Model)
	}
	if meta.PromptTokens != 77 || meta.CompletionTok != 123 || meta.TotalTokens != 200 {
		t.Fatalf("unexpected tokens: %+v", meta)
	}
	if meta.PromptMs != 910.5 || meta.CompletionMs != 2222.25 {
		t.Fatalf("unexpected timings: %+v", meta)
	}
}

func TestHandleProxyNonStreamingLlamaCppJSON(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{
			"model":"llama.cpp-actual",
			"usage":{"prompt_tokens":11,"completion_tokens":22,"total_tokens":33},
			"timings":{"prompt_ms":50,"predicted_ms":125},
			"content":"ok"
		}`)
	}))
	defer backend.Close()

	svc, db, cleanup := newTestServer(t, backend.URL)
	defer cleanup()
	defer db.Close()

	proxy := httptest.NewServer(svc)
	defer proxy.Close()

	resp, err := proxy.Client().Post(proxy.URL+"/completion", "application/json", strings.NewReader(`{"model":"request-model","prompt":"hi"}`))
	if err != nil {
		t.Fatalf("proxy post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body), `"llama.cpp-actual"`) {
		t.Fatalf("unexpected response body: %s", string(body))
	}

	listResp, err := proxy.Client().Get(proxy.URL + "/_monitor/requests?limit=10")
	if err != nil {
		t.Fatalf("get requests: %v", err)
	}
	defer listResp.Body.Close()
	var payload struct {
		Items []RequestRecord `json:"items"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode requests: %v", err)
	}
	if len(payload.Items) == 0 {
		t.Fatal("expected at least one request")
	}
	rec := payload.Items[0]
	if rec.Model != "llama.cpp-actual" {
		t.Fatalf("model=%q", rec.Model)
	}
	if rec.TotalTokens != 33 || rec.PromptTokens != 11 || rec.CompletionTokens != 22 {
		t.Fatalf("unexpected tokens: %+v", rec)
	}
	if rec.ResponseRawPath == "" {
		t.Fatal("expected response raw path")
	}
}

func TestHandleProxyStreamingLifecycle(t *testing.T) {
	backendStarted := make(chan struct{}, 1)
	releaseBackend := make(chan struct{})

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"model\":\"backend-final\",\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
		flusher.Flush()
		select {
		case backendStarted <- struct{}{}:
		default:
		}
		<-releaseBackend
		_, _ = io.WriteString(w, "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":20,\"total_tokens\":30},\"timings\":{\"prompt_ms\":50,\"predicted_ms\":200}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer backend.Close()

	svc, db, cleanup := newTestServer(t, backend.URL)
	defer cleanup()
	defer db.Close()

	proxy := httptest.NewServer(svc)
	defer proxy.Close()

	client := proxy.Client()
	reqBody := `{"model":"request-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`

	reqDone := make(chan error, 1)
	go func() {
		resp, err := client.Post(proxy.URL+"/v1/chat/completions", "application/json", strings.NewReader(reqBody))
		if err != nil {
			reqDone <- err
			return
		}
		defer resp.Body.Close()
		_, err = io.ReadAll(resp.Body)
		reqDone <- err
	}()

	select {
	case <-backendStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("backend request did not start")
	}

	var liveID string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(proxy.URL + "/_monitor/requests?limit=10")
		if err != nil {
			t.Fatalf("load live requests: %v", err)
		}
		var payload struct {
			Items []RequestRecord `json:"items"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			resp.Body.Close()
			t.Fatalf("decode requests: %v", err)
		}
		resp.Body.Close()
		for _, item := range payload.Items {
			if item.Path == "/v1/chat/completions" {
				if item.StatusCode != 0 {
					t.Fatalf("expected in-flight status 0, got %d", item.StatusCode)
				}
				if item.ResponseRawPath != "" {
					t.Fatalf("expected empty response path while inflight, got %q", item.ResponseRawPath)
				}
				liveID = item.ID
				break
			}
		}
		if liveID != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if liveID == "" {
		t.Fatal("did not observe live request in monitor list")
	}

	close(releaseBackend)

	select {
	case err := <-reqDone:
		if err != nil {
			t.Fatalf("proxy request failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("proxy request did not finish")
	}

	resp, err := client.Get(proxy.URL + "/_monitor/request/" + liveID)
	if err != nil {
		t.Fatalf("load final request: %v", err)
	}
	defer resp.Body.Close()
	var rec RequestRecord
	if err := json.NewDecoder(resp.Body).Decode(&rec); err != nil {
		t.Fatalf("decode final request: %v", err)
	}
	if rec.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", rec.StatusCode)
	}
	if rec.Model != "backend-final" {
		t.Fatalf("model=%q", rec.Model)
	}
	if rec.TotalTokens != 30 || rec.PromptTokens != 10 || rec.CompletionTokens != 20 {
		t.Fatalf("unexpected tokens: %+v", rec)
	}
	if rec.ResponseRawPath == "" {
		t.Fatal("expected response raw path after completion")
	}
}

func TestDeleteRequestByIDRemovesRowAndRawFiles(t *testing.T) {
	svc, db, cleanup := newTestServer(t, "http://example.invalid")
	defer cleanup()
	defer db.Close()

	reqRaw, err := svc.saveRawPayload("req1", "request", []byte(`{"a":1}`))
	if err != nil {
		t.Fatalf("save request raw: %v", err)
	}
	respRaw, err := svc.saveRawPayload("req1", "response", []byte(`{"b":2}`))
	if err != nil {
		t.Fatalf("save response raw: %v", err)
	}
	err = svc.insertRequest(RequestRecord{
		ID:              "req1",
		CreatedAt:       time.Now().UTC(),
		Method:          http.MethodPost,
		Path:            "/v1/chat/completions",
		RequestRawPath:  reqRaw,
		ResponseRawPath: "",
	})
	if err != nil {
		t.Fatalf("insert request: %v", err)
	}
	err = svc.finishRequest("req1", RequestRecord{
		Model:           "m",
		StatusCode:      http.StatusOK,
		ResponseRawPath: respRaw,
	})
	if err != nil {
		t.Fatalf("finish request: %v", err)
	}

	if err := svc.deleteRequestByID("req1"); err != nil {
		t.Fatalf("delete request: %v", err)
	}

	if _, err := svc.getRequestByID("req1"); err == nil {
		t.Fatal("expected deleted row to be missing")
	}
	if _, err := os.Stat(filepath.Join(svc.cfg.DataDir, reqRaw)); !os.IsNotExist(err) {
		t.Fatalf("expected request raw to be removed, got err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(svc.cfg.DataDir, respRaw)); !os.IsNotExist(err) {
		t.Fatalf("expected response raw to be removed, got err=%v", err)
	}
}

func TestHandleRawReturnsSavedPayload(t *testing.T) {
	svc, db, cleanup := newTestServer(t, "http://example.invalid")
	defer cleanup()
	defer db.Close()

	reqRaw, err := svc.saveRawPayload("req2", "request", []byte(`{"hello":"world"}`))
	if err != nil {
		t.Fatalf("save raw: %v", err)
	}
	if err := svc.insertRequest(RequestRecord{
		ID:             "req2",
		CreatedAt:      time.Now().UTC(),
		Method:         http.MethodPost,
		Path:           "/v1/chat/completions",
		RequestRawPath: reqRaw,
	}); err != nil {
		t.Fatalf("insert request: %v", err)
	}

	rr := httptest.NewRecorder()
	svc.handleRaw(rr, "/raw/req2/request")
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := strings.TrimSpace(rr.Body.String()); got != `{"hello":"world"}` {
		t.Fatalf("raw body=%q", got)
	}
}

func TestGetStatsAggregatesRollingAndLifetime(t *testing.T) {
	svc, db, cleanup := newTestServer(t, "http://example.invalid")
	defer cleanup()
	defer db.Close()

	now := time.Now().UTC()
	records := []RequestRecord{
		{
			ID:               "recent-ok",
			CreatedAt:        now.Add(-10 * time.Minute),
			Method:           http.MethodPost,
			Path:             "/completion",
			StatusCode:       http.StatusOK,
			RequestBytes:     100,
			ResponseBytes:    200,
			PromptTokens:     10,
			CompletionTokens: 20,
			TotalTokens:      30,
			PromptMs:         100,
			CompletionMs:     200,
			TotalMs:          400,
			FirstByteMs:      150,
			IsStreaming:      false,
		},
		{
			ID:               "recent-live",
			CreatedAt:        now.Add(-5 * time.Minute),
			Method:           http.MethodPost,
			Path:             "/v1/chat/completions",
			StatusCode:       0,
			RequestBytes:     50,
			ResponseBytes:    0,
			PromptTokens:     0,
			CompletionTokens: 0,
			TotalTokens:      0,
			TotalMs:          0,
			FirstByteMs:      0,
			IsStreaming:      true,
		},
		{
			ID:               "old-ok",
			CreatedAt:        now.Add(-26 * time.Hour),
			Method:           http.MethodPost,
			Path:             "/completion",
			StatusCode:       http.StatusOK,
			RequestBytes:     70,
			ResponseBytes:    80,
			PromptTokens:     7,
			CompletionTokens: 8,
			TotalTokens:      15,
			PromptMs:         70,
			CompletionMs:     80,
			TotalMs:          170,
			FirstByteMs:      90,
			IsStreaming:      false,
		},
	}
	for _, rec := range records {
		if err := seedRequest(t, svc, rec); err != nil {
			t.Fatalf("seed request %s: %v", rec.ID, err)
		}
	}

	stats, err := svc.getStats(24, RequestFilter{})
	if err != nil {
		t.Fatalf("get stats: %v", err)
	}
	if stats["total_requests"].(int64) != 2 {
		t.Fatalf("rolling total_requests=%v", stats["total_requests"])
	}
	if stats["lifetime_total_requests"].(int64) != 3 {
		t.Fatalf("lifetime_total_requests=%v", stats["lifetime_total_requests"])
	}
	if stats["total_tokens"].(int64) != 30 {
		t.Fatalf("rolling total_tokens=%v", stats["total_tokens"])
	}
	if stats["lifetime_total_tokens"].(int64) != 45 {
		t.Fatalf("lifetime_total_tokens=%v", stats["lifetime_total_tokens"])
	}
	if stats["prompt_tokens_per_second"].(float64) <= 0 {
		t.Fatalf("prompt_tokens_per_second=%v", stats["prompt_tokens_per_second"])
	}
	if stats["decode_tokens_per_second"].(float64) <= 0 {
		t.Fatalf("decode_tokens_per_second=%v", stats["decode_tokens_per_second"])
	}
	if stats["errors_count"].(int64) != 1 {
		t.Fatalf("errors_count=%v", stats["errors_count"])
	}
	if stats["streaming_requests"].(int64) != 1 {
		t.Fatalf("streaming_requests=%v", stats["streaming_requests"])
	}
}

func TestGetRequestsWithTokensFilter(t *testing.T) {
	svc, db, cleanup := newTestServer(t, "http://example.invalid")
	defer cleanup()
	defer db.Close()

	now := time.Now().UTC()
	for _, rec := range []RequestRecord{
		{
			ID:               "with-tokens",
			CreatedAt:        now,
			Method:           http.MethodPost,
			Path:             "/completion",
			StatusCode:       http.StatusOK,
			PromptTokens:     10,
			CompletionTokens: 20,
			TotalTokens:      30,
		},
		{
			ID:          "no-tokens",
			CreatedAt:   now.Add(-time.Minute),
			Method:      http.MethodPost,
			Path:        "/completion",
			StatusCode:  http.StatusOK,
			TotalTokens: 0,
		},
	} {
		if err := seedRequest(t, svc, rec); err != nil {
			t.Fatalf("seed request %s: %v", rec.ID, err)
		}
	}

	items, err := svc.getRequests(20, 0, RequestFilter{WithTokens: true})
	if err != nil {
		t.Fatalf("get requests: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].ID != "with-tokens" {
		t.Fatalf("unexpected item id=%q", items[0].ID)
	}
}

func TestGetStatsRespectsFilters(t *testing.T) {
	svc, db, cleanup := newTestServer(t, "http://example.invalid")
	defer cleanup()
	defer db.Close()

	now := time.Now().UTC()
	for _, rec := range []RequestRecord{
		{
			ID:               "stream-with-tokens",
			CreatedAt:        now,
			Method:           http.MethodPost,
			Path:             "/v1/chat/completions",
			Model:            "m1",
			IsStreaming:      true,
			StatusCode:       http.StatusOK,
			PromptTokens:     20,
			CompletionTokens: 40,
			TotalTokens:      60,
		},
		{
			ID:          "plain-no-tokens",
			CreatedAt:   now,
			Method:      http.MethodPost,
			Path:        "/completion",
			Model:       "m2",
			IsStreaming: false,
			StatusCode:  http.StatusOK,
			TotalTokens: 0,
		},
	} {
		if err := seedRequest(t, svc, rec); err != nil {
			t.Fatalf("seed request %s: %v", rec.ID, err)
		}
	}

	streamTrue := true
	stats, err := svc.getStats(24, RequestFilter{Streaming: &streamTrue, WithTokens: true, Path: "/v1/chat"})
	if err != nil {
		t.Fatalf("get stats: %v", err)
	}
	if stats["total_requests"].(int64) != 1 {
		t.Fatalf("filtered total_requests=%v", stats["total_requests"])
	}
	if stats["matching_total_requests"].(int64) != 1 {
		t.Fatalf("matching_total_requests=%v", stats["matching_total_requests"])
	}
	if stats["total_tokens"].(int64) != 60 {
		t.Fatalf("filtered total_tokens=%v", stats["total_tokens"])
	}
	if stats["matching_total_tokens"].(int64) != 60 {
		t.Fatalf("matching_total_tokens=%v", stats["matching_total_tokens"])
	}
	if stats["streaming_requests"].(int64) != 1 {
		t.Fatalf("filtered streaming_requests=%v", stats["streaming_requests"])
	}
}

func TestCleanupDisabledWhenRetentionNonPositive(t *testing.T) {
	svc, db, cleanup := newTestServer(t, "http://example.invalid")
	defer cleanup()
	defer db.Close()

	svc.cfg.RetentionDays = 0
	if err := seedRequest(t, svc, RequestRecord{
		ID:         "old-record",
		CreatedAt:  time.Now().UTC().AddDate(0, 0, -30),
		Method:     http.MethodPost,
		Path:       "/completion",
		StatusCode: http.StatusOK,
	}); err != nil {
		t.Fatalf("seed request: %v", err)
	}

	svc.cleanup()

	if _, err := svc.getRequestByID("old-record"); err != nil {
		t.Fatalf("expected old record to remain, got %v", err)
	}
}

func newTestServer(t *testing.T, backendURL string) (*Server, *sql.DB, func()) {
	t.Helper()

	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "monitor.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := initDB(db); err != nil {
		t.Fatalf("init db: %v", err)
	}
	if err := normalizeDB(db); err != nil {
		t.Fatalf("normalize db: %v", err)
	}

	svc := &Server{
		cfg: Config{
			ListenAddr:          ":0",
			DefaultBackend:      backendURL,
			AllowDynamicBackend: true,
			DataDir:             dataDir,
			RetentionDays:       14,
			MaxRequestBytes:     2 << 20,
			MaxCaptureBytes:     2 << 20,
			RequestTimeout:      15 * time.Second,
		},
		db:     db,
		client: &http.Client{Timeout: 15 * time.Second},
		hub:    NewEventHub(),
	}

	cleanup := func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = os.Remove(filepath.Join(dataDir, "monitor.db-shm"))
		_ = os.Remove(filepath.Join(dataDir, "monitor.db-wal"))
		_ = db.PingContext(ctx)
	}
	return svc, db, cleanup
}

func seedRequest(t *testing.T, svc *Server, rec RequestRecord) error {
	t.Helper()
	if err := svc.insertRequest(RequestRecord{
		ID:             rec.ID,
		CreatedAt:      rec.CreatedAt,
		Method:         rec.Method,
		Path:           rec.Path,
		Query:          rec.Query,
		ClientIP:       rec.ClientIP,
		BackendURL:     rec.BackendURL,
		Model:          rec.Model,
		IsStreaming:    rec.IsStreaming,
		RequestBytes:   rec.RequestBytes,
		RequestRawPath: rec.RequestRawPath,
		UserAgent:      rec.UserAgent,
	}); err != nil {
		return err
	}
	return svc.finishRequest(rec.ID, rec)
}
