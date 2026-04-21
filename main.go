package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

type Config struct {
	ListenAddr          string
	DefaultBackend      string
	AllowDynamicBackend bool
	DataDir             string
	RetentionDays       int
	MaxRequestBytes     int64
	MaxCaptureBytes     int64
	RequestTimeout      time.Duration
	PollBackendMetrics  bool
	PollInterval        time.Duration
}

func loadConfig() Config {
	return Config{
		ListenAddr:          getEnv("LISTEN_ADDR", ":9091"),
		DefaultBackend:      strings.TrimRight(getEnv("DEFAULT_BACKEND_URL", "http://host.docker.internal:8080"), "/"),
		AllowDynamicBackend: getEnvBool("ALLOW_DYNAMIC_BACKEND", true),
		DataDir:             getEnv("DATA_DIR", "/app/data"),
		RetentionDays:       getEnvInt("RETENTION_DAYS", 14),
		MaxRequestBytes:     getEnvInt64("MAX_REQUEST_BYTES", 32*1024*1024),
		MaxCaptureBytes:     getEnvInt64("MAX_CAPTURE_BYTES", 32*1024*1024),
		RequestTimeout:      time.Duration(getEnvInt("REQUEST_TIMEOUT_SECONDS", 600)) * time.Second,
		PollBackendMetrics:  getEnvBool("POLL_BACKEND_METRICS", true),
		PollInterval:        time.Duration(getEnvInt("POLL_INTERVAL_SECONDS", 10)) * time.Second,
	}
}

type Server struct {
	cfg    Config
	db     *sql.DB
	client *http.Client
	hub    *EventHub
	active atomic.Int64
}

type RequestRecord struct {
	ID               string    `json:"id"`
	CreatedAt        time.Time `json:"created_at"`
	Method           string    `json:"method"`
	Path             string    `json:"path"`
	Query            string    `json:"query"`
	ClientIP         string    `json:"client_ip"`
	BackendURL       string    `json:"backend_url"`
	Model            string    `json:"model"`
	IsStreaming      bool      `json:"is_streaming"`
	StatusCode       int       `json:"status_code"`
	ErrorText        string    `json:"error_text"`
	RequestBytes     int64     `json:"request_bytes"`
	ResponseBytes    int64     `json:"response_bytes"`
	PromptTokens     int64     `json:"prompt_tokens"`
	CompletionTokens int64     `json:"completion_tokens"`
	TotalTokens      int64     `json:"total_tokens"`
	PromptMs         float64   `json:"prompt_ms"`
	CompletionMs     float64   `json:"completion_ms"`
	TotalMs          float64   `json:"total_ms"`
	FirstByteMs      float64   `json:"first_byte_ms"`
	ChunksCount      int64     `json:"chunks_count"`
	RequestRawPath   string    `json:"request_raw_path"`
	ResponseRawPath  string    `json:"response_raw_path"`
	UserAgent        string    `json:"user_agent"`
	PromptTokPerSec  float64   `json:"prompt_tok_per_sec"`
	DecodeTokPerSec  float64   `json:"decode_tok_per_sec"`
	TotalTokPerSec   float64   `json:"total_tok_per_sec"`
}

type RequestFilter struct {
	Path       string
	Model      string
	Method     string
	Search     string
	StatusCode int
	SinceHours int
	Streaming  *bool
	ErrorsOnly bool
	WithTokens bool
}

type EventHub struct {
	mu      sync.Mutex
	clients map[chan string]struct{}
}

func NewEventHub() *EventHub {
	return &EventHub{clients: map[chan string]struct{}{}}
}

func (h *EventHub) Subscribe() chan string {
	ch := make(chan string, 64)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *EventHub) Unsubscribe(ch chan string) {
	h.mu.Lock()
	if _, ok := h.clients[ch]; ok {
		delete(h.clients, ch)
		close(ch)
	}
	h.mu.Unlock()
}

func (h *EventHub) Broadcast(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	msg := string(b)

	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.clients {
		select {
		case ch <- msg:
		default:
		}
	}
}

func main() {
	cfg := loadConfig()

	if err := validateBackendURL(cfg.DefaultBackend); err != nil {
		log.Fatalf("invalid DEFAULT_BACKEND_URL: %v", err)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		log.Fatalf("mkdir data dir: %v", err)
	}

	dbPath := filepath.Join(cfg.DataDir, "monitor.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if err := initDB(db); err != nil {
		log.Fatalf("init db: %v", err)
	}
	if err := normalizeDB(db); err != nil {
		log.Fatalf("normalize db: %v", err)
	}

	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    true,
	}

	s := &Server{
		cfg: cfg,
		db:  db,
		client: &http.Client{
			Transport: transport,
			Timeout:   cfg.RequestTimeout,
		},
		hub: NewEventHub(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.cleanupLoop(ctx)
	if cfg.PollBackendMetrics {
		go s.backendMetricsLoop(ctx)
	}

	log.Printf("llama.cpp Router Monitor listening on %s, backend=%s", cfg.ListenAddr, cfg.DefaultBackend)
	if err := http.ListenAndServe(cfg.ListenAddr, s); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/_monitor") {
		s.handleMonitor(w, r)
		return
	}
	s.handleProxy(w, r)
}

func (s *Server) handleMonitor(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/_monitor")
	if p == "" {
		p = "/"
	}

	switch {
	case p == "/":
		writeJSON(w, http.StatusOK, map[string]any{
			"name":    "llama.cpp Router Monitor",
			"version": "1.0.0",
			"endpoints": []string{
				"/_monitor/health",
				"/_monitor/live",
				"/_monitor/stats?hours=24",
				"/_monitor/requests?limit=100&offset=0",
				"/_monitor/request/{id}",
				"/_monitor/raw/{id}/{request|response}",
				"/_monitor/events",
				"/_monitor/backend-metrics?limit=200",
				"/_monitor/ui",
			},
		})
	case p == "/ui" || strings.HasPrefix(p, "/ui/"):
		s.handleUI(w, r, p)
	case p == "/health":
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "time": time.Now().UTC(), "active_connections": s.active.Load()})
	case p == "/stats":
		hours := getQueryInt(r, "hours", 24)
		f := parseRequestFilter(r)
		stats, err := s.getStats(hours, f)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, stats)
	case p == "/requests":
		limit := getQueryInt(r, "limit", 100)
		offset := getQueryInt(r, "offset", 0)
		f := parseRequestFilter(r)

		recs, err := s.getRequests(limit, offset, f)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": recs, "limit": limit, "offset": offset, "filters": f})
	case p == "/events":
		s.handleEvents(w, r)
	case p == "/live":
		writeJSON(w, http.StatusOK, map[string]any{"active_connections": s.active.Load(), "time": time.Now().UTC()})
	case p == "/backend-metrics":
		limit := getQueryInt(r, "limit", 200)
		items, err := s.getBackendMetrics(limit)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items, "limit": limit})
	case strings.HasPrefix(p, "/request/"):
		id := strings.TrimPrefix(p, "/request/")
		if r.Method == http.MethodDelete {
			if err := s.deleteRequestByID(id); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
					return
				}
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			s.hub.Broadcast(map[string]any{"kind": "request_deleted", "id": id, "time": time.Now().UTC()})
			writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "id": id})
			return
		}
		rec, err := s.getRequestByID(id)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, rec)
	case strings.HasPrefix(p, "/raw/"):
		s.handleRaw(w, p)
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown monitor endpoint"})
	}
}

func parseRequestFilter(r *http.Request) RequestFilter {
	f := RequestFilter{
		Path:       strings.TrimSpace(r.URL.Query().Get("path")),
		Model:      strings.TrimSpace(r.URL.Query().Get("model")),
		Method:     strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("method"))),
		Search:     strings.TrimSpace(r.URL.Query().Get("q")),
		StatusCode: getQueryInt(r, "status", 0),
		SinceHours: getQueryInt(r, "since_hours", 0),
		ErrorsOnly: getQueryBool(r, "errors_only", false),
		WithTokens: getQueryBool(r, "with_tokens", false),
	}
	if stream, ok := getOptionalQueryBool(r, "stream"); ok {
		f.Streaming = &stream
	}
	return f
}

func (s *Server) handleRaw(w http.ResponseWriter, p string) {
	parts := strings.Split(strings.TrimPrefix(p, "/raw/"), "/")
	if len(parts) != 2 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "use /_monitor/raw/{request_id}/{request|response}"})
		return
	}

	id := parts[0]
	kind := parts[1]
	rec, err := s.getRequestByID(id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	var rel string
	if kind == "request" {
		rel = rec.RequestRawPath
	} else if kind == "response" {
		rel = rec.ResponseRawPath
	} else {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "part must be request or response"})
		return
	}
	if rel == "" {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "raw payload not available"})
		return
	}

	fullPath := filepath.Clean(filepath.Join(s.cfg.DataDir, rel))
	data, err := readGzipFile(fullPath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	if json.Valid(data) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	w.Header().Set("X-Request-ID", id)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) handleUI(w http.ResponseWriter, r *http.Request, p string) {
	rel := strings.TrimPrefix(p, "/ui")
	if rel == "" || rel == "/" {
		http.ServeFile(w, r, filepath.Join("web", "index.html"))
		return
	}
	clean := filepath.Clean(strings.TrimPrefix(rel, "/"))
	if strings.Contains(clean, "..") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid path"})
		return
	}
	http.ServeFile(w, r, filepath.Join("web", clean))
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "streaming unsupported"})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch := s.hub.Subscribe()
	defer s.hub.Unsubscribe(ch)

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-ch:
			_, _ = fmt.Fprintf(w, "event: request\ndata: %s\n\n", msg)
			flusher.Flush()
		case <-heartbeat.C:
			_, _ = w.Write([]byte(": keepalive\n\n"))
			flusher.Flush()
		}
	}
}

func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	requestID := newID()
	clientIP := getClientIP(r)
	backendURL, trimmedQuery, err := s.selectBackend(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	if s.cfg.MaxRequestBytes > 0 && r.ContentLength > s.cfg.MaxRequestBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "request is too large"})
		return
	}

	requestBody, err := readWithLimit(r.Body, s.cfg.MaxRequestBytes)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, errTooLarge) {
			code = http.StatusRequestEntityTooLarge
		}
		writeJSON(w, code, map[string]any{"error": err.Error()})
		return
	}

	isStreaming, model := detectRequestMeta(requestBody)
	reqRawPath, err := s.saveRawPayload(requestID, "request", requestBody)
	if err != nil {
		log.Printf("save request raw failed: %v", err)
	}

	if err := s.insertRequest(RequestRecord{
		ID:             requestID,
		CreatedAt:      started.UTC(),
		Method:         r.Method,
		Path:           r.URL.Path,
		Query:          trimmedQuery,
		ClientIP:       clientIP,
		BackendURL:     backendURL,
		Model:          model,
		IsStreaming:    isStreaming,
		RequestBytes:   int64(len(requestBody)),
		RequestRawPath: reqRawPath,
		UserAgent:      r.UserAgent(),
	}); err != nil {
		log.Printf("insert request failed: %v", err)
	}

	target := backendURL + r.URL.Path
	if trimmedQuery != "" {
		target += "?" + trimmedQuery
	}

	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(requestBody))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	copyRequestHeaders(outReq.Header, r.Header)
	outReq.Header.Set("X-Proxy-Request-ID", requestID)
	outReq.ContentLength = int64(len(requestBody))

	s.active.Add(1)
	s.hub.Broadcast(map[string]any{"kind": "active", "active_connections": s.active.Load(), "time": time.Now().UTC()})
	defer func() {
		s.active.Add(-1)
		s.hub.Broadcast(map[string]any{"kind": "active", "active_connections": s.active.Load(), "time": time.Now().UTC()})
	}()

	resp, err := s.client.Do(outReq)
	if err != nil {
		total := float64(time.Since(started).Milliseconds())
		_ = s.finishRequest(requestID, RequestRecord{StatusCode: 502, ErrorText: err.Error(), TotalMs: total})
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "request_id": requestID})
		return
	}
	defer resp.Body.Close()

	for k, vals := range resp.Header {
		if isHopByHopHeader(k) {
			continue
		}
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("X-Proxy-Request-ID", requestID)
	w.WriteHeader(resp.StatusCode)

	var firstByteMs float64
	var copied int64
	var chunks int64
	capturer := newLimitedBuffer(s.cfg.MaxCaptureBytes)

	if isStreamingResponse(resp, isStreaming) {
		copied, firstByteMs, chunks, err = streamCopySSE(w, resp.Body, capturer, started)
	} else {
		copied, firstByteMs, err = streamCopy(w, resp.Body, capturer, started)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("copy response failed req=%s: %v", requestID, err)
	}

	respBytes := capturer.Bytes()
	respRawPath, saveErr := s.saveRawPayload(requestID, "response", respBytes)
	if saveErr != nil {
		log.Printf("save response raw failed: %v", saveErr)
	}

	meta := parseResponseMeta(resp.Header, respBytes)
	finalModel := model
	if meta.Model != "" {
		finalModel = meta.Model
	}
	totalMs := float64(time.Since(started).Milliseconds())

	if err := s.finishRequest(requestID, RequestRecord{
		Model:            finalModel,
		StatusCode:       resp.StatusCode,
		ResponseBytes:    copied,
		PromptTokens:     meta.PromptTokens,
		CompletionTokens: meta.CompletionTok,
		TotalTokens:      meta.TotalTokens,
		PromptMs:         meta.PromptMs,
		CompletionMs:     meta.CompletionMs,
		TotalMs:          totalMs,
		FirstByteMs:      firstByteMs,
		ChunksCount:      chunks,
		ResponseRawPath:  respRawPath,
	}); err != nil {
		log.Printf("finish request failed: %v", err)
	}

	s.hub.Broadcast(map[string]any{
		"kind":               "request",
		"id":                 requestID,
		"time":               time.Now().UTC(),
		"path":               r.URL.Path,
		"method":             r.Method,
		"status_code":        resp.StatusCode,
		"total_ms":           totalMs,
		"first_byte_ms":      firstByteMs,
		"prompt_tokens":      meta.PromptTokens,
		"completion_tokens":  meta.CompletionTok,
		"total_tokens":       meta.TotalTokens,
		"model":              finalModel,
		"response_bytes":     copied,
		"chunks_count":       chunks,
		"backend_url":        backendURL,
		"active_connections": s.active.Load(),
	})
}

func (s *Server) selectBackend(r *http.Request) (backend string, query string, err error) {
	backend = s.cfg.DefaultBackend
	vals := r.URL.Query()
	if s.cfg.AllowDynamicBackend {
		if b := strings.TrimSpace(r.Header.Get("X-Backend-URL")); b != "" {
			backend = strings.TrimRight(b, "/")
		} else if b := strings.TrimSpace(vals.Get("backend")); b != "" {
			backend = strings.TrimRight(b, "/")
			vals.Del("backend")
		}
	}
	if err = validateBackendURL(backend); err != nil {
		return "", "", fmt.Errorf("invalid backend URL: %w", err)
	}
	return backend, vals.Encode(), nil
}

func isStreamingResponse(resp *http.Response, reqStreaming bool) bool {
	if reqStreaming {
		return true
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	return strings.Contains(ct, "text/event-stream")
}

type responseMeta struct {
	Model        string
	PromptTokens int64
	CompletionTok int64
	TotalTokens  int64
	PromptMs     float64
	CompletionMs float64
}

func streamCopy(w http.ResponseWriter, src io.Reader, capture *limitedBuffer, started time.Time) (copied int64, firstByteMs float64, err error) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	seenFirst := false
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			if !seenFirst {
				seenFirst = true
				firstByteMs = float64(time.Since(started).Milliseconds())
			}
			chunk := buf[:n]
			wn, werr := w.Write(chunk)
			if wn > 0 {
				copied += int64(wn)
				_, _ = capture.Write(chunk[:wn])
				if flusher != nil {
					flusher.Flush()
				}
			}
			if werr != nil {
				return copied, firstByteMs, werr
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return copied, firstByteMs, nil
			}
			return copied, firstByteMs, rerr
		}
	}
}

func streamCopySSE(w http.ResponseWriter, src io.Reader, capture *limitedBuffer, started time.Time) (copied int64, firstByteMs float64, chunks int64, err error) {
	flusher, _ := w.(http.Flusher)
	reader := bufio.NewReader(src)
	seenFirst := false
	for {
		line, rerr := reader.ReadBytes('\n')
		if len(line) > 0 {
			if !seenFirst {
				seenFirst = true
				firstByteMs = float64(time.Since(started).Milliseconds())
			}
			if bytes.HasPrefix(line, []byte("data:")) {
				trimmed := strings.TrimSpace(strings.TrimPrefix(string(line), "data:"))
				if trimmed != "" && trimmed != "[DONE]" {
					chunks++
				}
			}
			wn, werr := w.Write(line)
			if wn > 0 {
				copied += int64(wn)
				_, _ = capture.Write(line[:wn])
				if flusher != nil {
					flusher.Flush()
				}
			}
			if werr != nil {
				return copied, firstByteMs, chunks, werr
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return copied, firstByteMs, chunks, nil
			}
			return copied, firstByteMs, chunks, rerr
		}
	}
}

func parseResponseMeta(headers http.Header, body []byte) responseMeta {
	var meta responseMeta
	ct := headers.Get("Content-Type")
	mediatype, _, _ := mime.ParseMediaType(ct)
	if strings.Contains(mediatype, "json") {
		parseJSONResponseMeta(body, &meta)
		return meta
	}
	if strings.Contains(mediatype, "text/event-stream") {
		parseSSEResponseMeta(body, &meta)
	}
	return meta
}

func parseJSONResponseMeta(body []byte, meta *responseMeta) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return
	}
	if v, ok := m["model"].(string); ok {
		meta.Model = v
	}

	if usage, ok := m["usage"].(map[string]any); ok {
		meta.PromptTokens = toInt64(usage["prompt_tokens"])
		meta.CompletionTok = toInt64(usage["completion_tokens"])
		meta.TotalTokens = toInt64(usage["total_tokens"])
	}
	if timings, ok := m["timings"].(map[string]any); ok {
		if meta.PromptTokens == 0 {
			meta.PromptTokens = toInt64(timings["prompt_n"])
		}
		if meta.CompletionTok == 0 {
			meta.CompletionTok = toInt64(timings["predicted_n"])
		}
		meta.PromptMs = toFloat64(timings["prompt_ms"])
		meta.CompletionMs = toFloat64(timings["predicted_ms"])
	}
	if meta.PromptTokens == 0 {
		meta.PromptTokens = toInt64(m["tokens_evaluated"])
	}
	if meta.CompletionTok == 0 {
		meta.CompletionTok = toInt64(m["tokens_predicted"])
	}
	if meta.TotalTokens == 0 {
		meta.TotalTokens = meta.PromptTokens + meta.CompletionTok
	}
	if meta.PromptMs == 0 {
		meta.PromptMs = toFloat64(m["tokens_evaluated_ms"])
	}
	if meta.CompletionMs == 0 {
		meta.CompletionMs = toFloat64(m["tokens_predicted_ms"])
	}
}

func parseSSEResponseMeta(body []byte, meta *responseMeta) {
	scanner := bufio.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		parseJSONResponseMeta([]byte(payload), meta)
		if meta.Model != "" && meta.TotalTokens > 0 {
			return
		}
	}
}

func detectRequestMeta(body []byte) (isStreaming bool, model string) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return false, ""
	}
	isStreaming, _ = m["stream"].(bool)
	if v, ok := m["model"].(string); ok {
		model = v
	}
	return
}

func (s *Server) saveRawPayload(requestID string, kind string, data []byte) (string, error) {
	if len(data) == 0 {
		return "", nil
	}
	dateDir := time.Now().UTC().Format("2006-01-02")
	relDir := filepath.Join("raw", dateDir)
	fullDir := filepath.Join(s.cfg.DataDir, relDir)
	if err := os.MkdirAll(fullDir, 0o755); err != nil {
		return "", err
	}
	fileName := fmt.Sprintf("%s-%s.gz", requestID, kind)
	relPath := filepath.Join(relDir, fileName)
	fullPath := filepath.Join(s.cfg.DataDir, relPath)

	f, err := os.Create(fullPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	gw := gzip.NewWriter(f)
	if _, err := gw.Write(data); err != nil {
		_ = gw.Close()
		return "", err
	}
	if err := gw.Close(); err != nil {
		return "", err
	}
	return relPath, nil
}

func readGzipFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gr, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gr.Close()
	return io.ReadAll(gr)
}

func initDB(db *sql.DB) error {
	stmts := []string{
		`PRAGMA journal_mode = WAL;`,
		`PRAGMA synchronous = NORMAL;`,
		`CREATE TABLE IF NOT EXISTS requests (
			id TEXT PRIMARY KEY,
			created_at DATETIME NOT NULL,
			method TEXT NOT NULL,
			path TEXT NOT NULL,
			query TEXT,
			client_ip TEXT,
			backend_url TEXT,
			model TEXT,
			is_streaming INTEGER NOT NULL DEFAULT 0,
			status_code INTEGER NOT NULL DEFAULT 0,
			error_text TEXT,
			request_bytes INTEGER NOT NULL DEFAULT 0,
			response_bytes INTEGER NOT NULL DEFAULT 0,
			prompt_tokens INTEGER NOT NULL DEFAULT 0,
			completion_tokens INTEGER NOT NULL DEFAULT 0,
			total_tokens INTEGER NOT NULL DEFAULT 0,
			prompt_ms REAL NOT NULL DEFAULT 0,
			completion_ms REAL NOT NULL DEFAULT 0,
			total_ms REAL NOT NULL DEFAULT 0,
			first_byte_ms REAL NOT NULL DEFAULT 0,
			chunks_count INTEGER NOT NULL DEFAULT 0,
			request_raw_path TEXT,
			response_raw_path TEXT,
			user_agent TEXT
		);`,
		`CREATE INDEX IF NOT EXISTS idx_requests_created_at ON requests(created_at DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_requests_path ON requests(path);`,
		`CREATE INDEX IF NOT EXISTS idx_requests_status_code ON requests(status_code);`,
		`CREATE TABLE IF NOT EXISTS backend_metrics (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			created_at DATETIME NOT NULL,
			backend_url TEXT NOT NULL,
			metric_name TEXT NOT NULL,
			metric_value REAL NOT NULL
		);`,
		`CREATE INDEX IF NOT EXISTS idx_backend_metrics_created_at ON backend_metrics(created_at DESC);`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

func normalizeDB(db *sql.DB) error {
	_, err := db.Exec(`UPDATE requests SET
		query = COALESCE(query, ''),
		client_ip = COALESCE(client_ip, ''),
		backend_url = COALESCE(backend_url, ''),
		model = COALESCE(model, ''),
		error_text = COALESCE(error_text, ''),
		request_raw_path = COALESCE(request_raw_path, ''),
		response_raw_path = COALESCE(response_raw_path, ''),
		user_agent = COALESCE(user_agent, '')
		WHERE
			query IS NULL OR
			client_ip IS NULL OR
			backend_url IS NULL OR
			model IS NULL OR
			error_text IS NULL OR
			request_raw_path IS NULL OR
			response_raw_path IS NULL OR
			user_agent IS NULL`)
	return err
}

func (s *Server) insertRequest(rec RequestRecord) error {
	_, err := s.db.Exec(`INSERT INTO requests (
		id, created_at, method, path, query, client_ip, backend_url, model,
		is_streaming, request_bytes, request_raw_path, user_agent
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.ID, rec.CreatedAt.Format(time.RFC3339Nano), rec.Method, rec.Path, rec.Query,
		rec.ClientIP, rec.BackendURL, rec.Model, boolToInt(rec.IsStreaming),
		rec.RequestBytes, rec.RequestRawPath, rec.UserAgent,
	)
	return err
}

func (s *Server) finishRequest(id string, rec RequestRecord) error {
	_, err := s.db.Exec(`UPDATE requests SET
		model = ?,
		status_code = ?,
		error_text = ?,
		response_bytes = ?,
		prompt_tokens = ?,
		completion_tokens = ?,
		total_tokens = ?,
		prompt_ms = ?,
		completion_ms = ?,
		total_ms = ?,
		first_byte_ms = ?,
		chunks_count = ?,
		response_raw_path = ?
		WHERE id = ?`,
		rec.Model, rec.StatusCode, rec.ErrorText, rec.ResponseBytes,
		rec.PromptTokens, rec.CompletionTokens, rec.TotalTokens,
		rec.PromptMs, rec.CompletionMs, rec.TotalMs, rec.FirstByteMs,
		rec.ChunksCount, rec.ResponseRawPath, id,
	)
	return err
}

func (s *Server) getRequests(limit int, offset int, f RequestFilter) ([]RequestRecord, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	query := `SELECT
		id, created_at, method, path, query, client_ip, backend_url, model,
		is_streaming, status_code, error_text, request_bytes, response_bytes,
		prompt_tokens, completion_tokens, total_tokens,
		prompt_ms, completion_ms, total_ms, first_byte_ms, chunks_count,
		request_raw_path, response_raw_path, user_agent
		FROM requests WHERE 1=1`
	args := make([]any, 0, 16)

	query, args = appendRequestFilterSQL(query, args, f, true)

	query += ` ORDER BY created_at DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]RequestRecord, 0, limit)
	for rows.Next() {
		rec, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		rec = enrichRequestRates(rec)
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *Server) getRequestByID(id string) (RequestRecord, error) {
	row := s.db.QueryRow(`SELECT
		id, created_at, method, path, query, client_ip, backend_url, model,
		is_streaming, status_code, error_text, request_bytes, response_bytes,
		prompt_tokens, completion_tokens, total_tokens,
		prompt_ms, completion_ms, total_ms, first_byte_ms, chunks_count,
		request_raw_path, response_raw_path, user_agent
		FROM requests WHERE id = ?`, id)
	rec, err := scanRequest(row)
	if err != nil {
		return rec, err
	}
	return enrichRequestRates(rec), nil
}

func (s *Server) deleteRequestByID(id string) error {
	rec, err := s.getRequestByID(id)
	if err != nil {
		return err
	}
	for _, rel := range []string{rec.RequestRawPath, rec.ResponseRawPath} {
		if rel == "" {
			continue
		}
		fullPath := filepath.Clean(filepath.Join(s.cfg.DataDir, rel))
		relCheck, relErr := filepath.Rel(s.cfg.DataDir, fullPath)
		if relErr != nil || strings.HasPrefix(relCheck, "..") {
			return fmt.Errorf("refusing to delete path outside data dir")
		}
		if err := os.Remove(fullPath); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	res, err := s.db.Exec(`DELETE FROM requests WHERE id = ?`, id)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Server) getStats(hours int, f RequestFilter) (map[string]any, error) {
	if hours <= 0 {
		hours = 24
	}
	if f.SinceHours > 0 {
		hours = f.SinceHours
	}
	since := time.Now().UTC().Add(-time.Duration(hours) * time.Hour).Format(time.RFC3339Nano)

	var totalRequests int64
	var promptTokens int64
	var completionTokens int64
	var totalTokens int64
	var lifetimeRequests int64
	var lifetimeTokens int64
	var matchingRequests int64
	var matchingTokens int64
	var reqBytes int64
	var respBytes int64
	var avgPromptMs float64
	var avgCompletionMs float64
	var avgTotalMs float64
	var avgFirstByteMs float64
	var errorsCount int64
	var streamCount int64

	query := `SELECT
		COUNT(*),
		COALESCE(SUM(prompt_tokens),0),
		COALESCE(SUM(completion_tokens),0),
		COALESCE(SUM(total_tokens),0),
		COALESCE(SUM(request_bytes),0),
		COALESCE(SUM(response_bytes),0),
		COALESCE(AVG(prompt_ms),0),
		COALESCE(AVG(completion_ms),0),
		COALESCE(AVG(total_ms),0),
		COALESCE(AVG(first_byte_ms),0),
		COALESCE(SUM(CASE WHEN status_code >= 400 OR status_code = 0 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(is_streaming),0)
		FROM requests WHERE created_at >= ?`
	args := []any{since}
	query, args = appendRequestFilterSQL(query, args, f, false)

	err := s.db.QueryRow(query, args...).Scan(
		&totalRequests,
		&promptTokens,
		&completionTokens,
		&totalTokens,
		&reqBytes,
		&respBytes,
		&avgPromptMs,
		&avgCompletionMs,
		&avgTotalMs,
		&avgFirstByteMs,
		&errorsCount,
		&streamCount,
	)
	if err != nil {
		return nil, err
	}
	if err := s.db.QueryRow(`SELECT
		COUNT(*),
		COALESCE(SUM(total_tokens),0)
		FROM requests`).Scan(&lifetimeRequests, &lifetimeTokens); err != nil {
		return nil, err
	}
	matchQuery := `SELECT
		COUNT(*),
		COALESCE(SUM(total_tokens),0)
		FROM requests WHERE 1=1`
	matchArgs := []any{}
	matchQuery, matchArgs = appendRequestFilterSQL(matchQuery, matchArgs, f, true)
	if err := s.db.QueryRow(matchQuery, matchArgs...).Scan(&matchingRequests, &matchingTokens); err != nil {
		return nil, err
	}

	secs := float64(hours * 3600)
	rpm := float64(totalRequests) / float64(hours*60)
	promptTokensPerSec := float64(promptTokens) / secs
	decodeTokensPerSec := float64(completionTokens) / secs
	tokensPerSec := float64(totalTokens) / secs
	errorRate := 0.0
	if totalRequests > 0 {
		errorRate = float64(errorsCount) / float64(totalRequests)
	}

	return map[string]any{
		"hours":                   hours,
		"active_connections":      s.active.Load(),
		"total_requests":          totalRequests,
		"total_prompt_tokens":     promptTokens,
		"total_completion_tokens": completionTokens,
		"total_tokens":            totalTokens,
		"matching_total_requests": matchingRequests,
		"matching_total_tokens":   matchingTokens,
		"lifetime_total_requests": lifetimeRequests,
		"lifetime_total_tokens":   lifetimeTokens,
		"total_request_bytes":     reqBytes,
		"total_response_bytes":    respBytes,
		"avg_prompt_ms":           avgPromptMs,
		"avg_completion_ms":       avgCompletionMs,
		"avg_total_ms":            avgTotalMs,
		"avg_first_byte_ms":       avgFirstByteMs,
		"requests_per_minute":     rpm,
		"prompt_tokens_per_second": promptTokensPerSec,
		"decode_tokens_per_second": decodeTokensPerSec,
		"tokens_per_second":       tokensPerSec,
		"errors_count":            errorsCount,
		"error_rate":              errorRate,
		"streaming_requests":      streamCount,
	}, nil
}

func (s *Server) getBackendMetrics(limit int) ([]map[string]any, error) {
	if limit <= 0 || limit > 5000 {
		limit = 200
	}
	rows, err := s.db.Query(`SELECT created_at, backend_url, metric_name, metric_value
		FROM backend_metrics ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]map[string]any, 0, limit)
	for rows.Next() {
		var createdAt string
		var backendURL string
		var metricName string
		var metricValue float64
		if err := rows.Scan(&createdAt, &backendURL, &metricName, &metricValue); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"created_at":   createdAt,
			"backend_url":  backendURL,
			"metric_name":  metricName,
			"metric_value": metricValue,
		})
	}
	return out, rows.Err()
}

func (s *Server) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	s.cleanup()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.cleanup()
		}
	}
}

func (s *Server) cleanup() {
	if s.cfg.RetentionDays <= 0 {
		return
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -s.cfg.RetentionDays).Format(time.RFC3339Nano)
	if _, err := s.db.Exec(`DELETE FROM requests WHERE created_at < ?`, cutoff); err != nil {
		log.Printf("cleanup requests failed: %v", err)
	}
	if _, err := s.db.Exec(`DELETE FROM backend_metrics WHERE created_at < ?`, cutoff); err != nil {
		log.Printf("cleanup backend_metrics failed: %v", err)
	}

	rawRoot := filepath.Join(s.cfg.DataDir, "raw")
	entries, err := os.ReadDir(rawRoot)
	if err != nil && !os.IsNotExist(err) {
		log.Printf("cleanup raw read failed: %v", err)
		return
	}
	cutoffDate := time.Now().UTC().AddDate(0, 0, -s.cfg.RetentionDays)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		d, err := time.Parse("2006-01-02", entry.Name())
		if err != nil {
			continue
		}
		if d.Before(cutoffDate) {
			_ = os.RemoveAll(filepath.Join(rawRoot, entry.Name()))
		}
	}
}

func appendRequestFilterSQL(query string, args []any, f RequestFilter, includeSince bool) (string, []any) {
	if f.Path != "" {
		query += ` AND path LIKE ?`
		args = append(args, "%"+f.Path+"%")
	}
	if f.Model != "" {
		query += ` AND model LIKE ?`
		args = append(args, "%"+f.Model+"%")
	}
	if f.Method != "" {
		query += ` AND method = ?`
		args = append(args, f.Method)
	}
	if f.StatusCode > 0 {
		query += ` AND status_code = ?`
		args = append(args, f.StatusCode)
	}
	if includeSince && f.SinceHours > 0 {
		since := time.Now().UTC().Add(-time.Duration(f.SinceHours) * time.Hour).Format(time.RFC3339Nano)
		query += ` AND created_at >= ?`
		args = append(args, since)
	}
	if f.Streaming != nil {
		query += ` AND is_streaming = ?`
		args = append(args, boolToInt(*f.Streaming))
	}
	if f.ErrorsOnly {
		query += ` AND (status_code >= 400 OR status_code = 0 OR error_text != '')`
	}
	if f.WithTokens {
		query += ` AND total_tokens > 0`
	}
	if f.Search != "" {
		query += ` AND (
			id LIKE ? OR path LIKE ? OR query LIKE ? OR client_ip LIKE ? OR model LIKE ? OR error_text LIKE ?
		)`
		pattern := "%" + f.Search + "%"
		args = append(args, pattern, pattern, pattern, pattern, pattern, pattern)
	}
	return query, args
}

func (s *Server) backendMetricsLoop(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.pollBackendMetrics(ctx)
		}
	}
}

func (s *Server) pollBackendMetrics(ctx context.Context) {
	u := s.cfg.DefaultBackend + "/metrics"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := s.client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return
	}
	metrics := parsePrometheusText(string(body))
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := s.db.Begin()
	if err != nil {
		return
	}
	for name, value := range metrics {
		_, _ = tx.Exec(`INSERT INTO backend_metrics (created_at, backend_url, metric_name, metric_value) VALUES (?, ?, ?, ?)`, now, s.cfg.DefaultBackend, name, value)
	}
	_ = tx.Commit()
}

func parsePrometheusText(text string) map[string]float64 {
	out := make(map[string]float64)
	lines := strings.Split(text, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := fields[0]
		value, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			continue
		}
		out[name] = value
	}
	return out
}

type scanner interface {
	Scan(dest ...any) error
}

func scanRequest(s scanner) (RequestRecord, error) {
	var rec RequestRecord
	var createdAt string
	var streamInt int
	var query sql.NullString
	var clientIP sql.NullString
	var backendURL sql.NullString
	var model sql.NullString
	var errorText sql.NullString
	var requestRawPath sql.NullString
	var responseRawPath sql.NullString
	var userAgent sql.NullString
	err := s.Scan(
		&rec.ID,
		&createdAt,
		&rec.Method,
		&rec.Path,
		&query,
		&clientIP,
		&backendURL,
		&model,
		&streamInt,
		&rec.StatusCode,
		&errorText,
		&rec.RequestBytes,
		&rec.ResponseBytes,
		&rec.PromptTokens,
		&rec.CompletionTokens,
		&rec.TotalTokens,
		&rec.PromptMs,
		&rec.CompletionMs,
		&rec.TotalMs,
		&rec.FirstByteMs,
		&rec.ChunksCount,
		&requestRawPath,
		&responseRawPath,
		&userAgent,
	)
	if err != nil {
		return rec, err
	}
	rec.Query = query.String
	rec.ClientIP = clientIP.String
	rec.BackendURL = backendURL.String
	rec.Model = model.String
	rec.ErrorText = errorText.String
	rec.RequestRawPath = requestRawPath.String
	rec.ResponseRawPath = responseRawPath.String
	rec.UserAgent = userAgent.String
	t, err := time.Parse(time.RFC3339Nano, createdAt)
	if err == nil {
		rec.CreatedAt = t
	}
	rec.IsStreaming = streamInt == 1
	return rec, nil
}

func enrichRequestRates(rec RequestRecord) RequestRecord {
	if rec.PromptMs > 0 {
		rec.PromptTokPerSec = float64(rec.PromptTokens) / (rec.PromptMs / 1000.0)
	}
	if rec.CompletionMs > 0 {
		rec.DecodeTokPerSec = float64(rec.CompletionTokens) / (rec.CompletionMs / 1000.0)
	}
	if rec.TotalMs > 0 {
		rec.TotalTokPerSec = float64(rec.TotalTokens) / (rec.TotalMs / 1000.0)
	}
	return rec
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func copyRequestHeaders(dst http.Header, src http.Header) {
	for k, vals := range src {
		if isHopByHopHeader(k) || strings.EqualFold(k, "Host") || strings.EqualFold(k, "Content-Length") {
			continue
		}
		for _, v := range vals {
			dst.Add(k, v)
		}
	}
}

func isHopByHopHeader(name string) bool {
	switch strings.ToLower(name) {
	case "connection", "proxy-connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailers", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

func getClientIP(r *http.Request) string {
	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[0])
	}
	if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
		return xr
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

var errTooLarge = errors.New("body exceeds max size")

func readWithLimit(rc io.ReadCloser, maxBytes int64) ([]byte, error) {
	defer rc.Close()
	if maxBytes <= 0 {
		return io.ReadAll(rc)
	}
	limited := io.LimitReader(rc, maxBytes+1)
	b, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > maxBytes {
		return nil, errTooLarge
	}
	return b, nil
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func toInt64(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case float32:
		return int64(x)
	case int:
		return int64(x)
	case int64:
		return x
	case int32:
		return int64(x)
	case json.Number:
		i, _ := x.Int64()
		return i
	case string:
		i, _ := strconv.ParseInt(x, 10, 64)
		return i
	default:
		return 0
	}
}

func toFloat64(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case int32:
		return float64(x)
	case json.Number:
		f, _ := x.Float64()
		return f
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	default:
		return 0
	}
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func getEnv(name string, fallback string) string {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return fallback
	}
	return v
}

func getEnvInt(name string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return fallback
	}
	i, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return i
}

func getEnvInt64(name string, fallback int64) int64 {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return fallback
	}
	i, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return fallback
	}
	return i
}

func getEnvBool(name string, fallback bool) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(name)))
	if v == "" {
		return fallback
	}
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func getQueryInt(r *http.Request, name string, fallback int) int {
	v := strings.TrimSpace(r.URL.Query().Get(name))
	if v == "" {
		return fallback
	}
	i, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return i
}

func getQueryBool(r *http.Request, name string, fallback bool) bool {
	v := strings.TrimSpace(strings.ToLower(r.URL.Query().Get(name)))
	if v == "" {
		return fallback
	}
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func getOptionalQueryBool(r *http.Request, name string) (bool, bool) {
	v := strings.TrimSpace(strings.ToLower(r.URL.Query().Get(name)))
	if v == "" {
		return false, false
	}
	switch v {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	default:
		return false, false
	}
}

type limitedBuffer struct {
	max int64
	buf bytes.Buffer
}

func newLimitedBuffer(max int64) *limitedBuffer {
	return &limitedBuffer{max: max}
}

func (lb *limitedBuffer) Write(p []byte) (int, error) {
	if lb.max <= 0 {
		return lb.buf.Write(p)
	}
	remaining := lb.max - int64(lb.buf.Len())
	if remaining <= 0 {
		return len(p), nil
	}
	if int64(len(p)) > remaining {
		_, _ = lb.buf.Write(p[:remaining])
		return len(p), nil
	}
	_, _ = lb.buf.Write(p)
	return len(p), nil
}

func (lb *limitedBuffer) Bytes() []byte {
	return lb.buf.Bytes()
}

func validateBackendURL(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("backend URL is empty")
	}
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		return fmt.Errorf("backend URL must start with http:// or https://")
	}
	return nil
}
