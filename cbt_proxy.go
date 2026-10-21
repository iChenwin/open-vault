package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"regexp"
	"time"
)

const cbtRequestLimit = 65536
const cbtResponseLimit = 8 << 20

var cbtGenerationID = regexp.MustCompile(`^[a-f0-9]{32}$`)
var cbtTargetName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,80}$`)

// cbtAPI is reached only after the normal web API authentication. Its transport
// can dial only the configured Unix socket; incoming hosts, headers and queries
// cannot select an upstream address or a different privileged endpoint.
func (s *Server) cbtAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	allowed := map[string]string{
		"/api/cbt/status":      http.MethodGet,
		"/api/cbt/runs":        http.MethodGet,
		"/api/cbt/head":        http.MethodGet,
		"/api/cbt/generations": http.MethodGet,
		"/api/cbt/backup":      http.MethodPost,
		"/api/cbt/verify":      http.MethodPost,
		"/api/cbt/restore":     http.MethodPost,
	}
	method, ok := allowed[r.URL.Path]
	if !ok {
		jsonOut(w, http.StatusNotFound, map[string]string{"error": "unknown CBT endpoint"})
		return
	}
	if r.Method != method {
		w.Header().Set("Allow", method)
		jsonOut(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if r.URL.RawQuery != "" {
		jsonOut(w, http.StatusBadRequest, map[string]string{"error": "CBT endpoints do not accept query parameters"})
		return
	}
	if s.cfg.CBTSocket == "" {
		if r.URL.Path == "/api/cbt/status" {
			jsonOut(w, http.StatusOK, map[string]bool{"configured": false, "busy": false})
		} else {
			jsonOut(w, http.StatusServiceUnavailable, map[string]string{"error": "CBT root service is not configured"})
		}
		return
	}
	if !filepath.IsAbs(s.cfg.CBTSocket) {
		jsonOut(w, http.StatusServiceUnavailable, map[string]string{"error": "CBT requires an absolute Unix socket path"})
		return
	}
	if r.ContentLength > cbtRequestLimit {
		jsonOut(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "CBT request body exceeds 65536 bytes"})
		return
	}
	var body []byte
	if r.Body != nil {
		var err error
		body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, cbtRequestLimit))
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				jsonOut(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "CBT request body exceeds 65536 bytes"})
			} else {
				jsonOut(w, http.StatusBadRequest, map[string]string{"error": "cannot read CBT request body"})
			}
			return
		}
	}
	if method == http.MethodGet && len(bytes.TrimSpace(body)) != 0 {
		jsonOut(w, http.StatusBadRequest, map[string]string{"error": "CBT GET requests must not have a body"})
		return
	}
	if method == http.MethodPost {
		var err error
		body, err = cbtProxyBody(r.URL.Path, body)
		if err != nil {
			jsonOut(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}
	timeout := 60 * time.Second
	if method == http.MethodPost {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	transport := &http.Transport{
		Proxy:             nil,
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", s.cfg.CBTSocket)
		},
		ResponseHeaderTimeout:  timeout,
		MaxResponseHeaderBytes: 65536,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport:     transport,
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	path := r.URL.Path[len("/api/cbt"):]
	upstream, err := http.NewRequestWithContext(ctx, method, "http://cbt.local"+path, bytes.NewReader(body))
	if err != nil {
		jsonOut(w, http.StatusBadGateway, map[string]string{"error": "cannot construct CBT root service request"})
		return
	}
	upstream.Header.Set("Accept", "application/json")
	if method == http.MethodPost {
		upstream.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(upstream)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			jsonOut(w, http.StatusGatewayTimeout, map[string]string{"error": "CBT root service timed out; an operation may already be accepted, inspect runs before retrying"})
		} else {
			jsonOut(w, http.StatusServiceUnavailable, map[string]string{"error": "CBT root service is unavailable; file backup remains available"})
		}
		return
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, cbtResponseLimit+1))
	if err != nil || len(data) > cbtResponseLimit || !json.Valid(data) || response.StatusCode < 200 || (response.StatusCode >= 300 && response.StatusCode < 400) {
		jsonOut(w, http.StatusBadGateway, map[string]string{"error": "invalid CBT root service response"})
		return
	}
	// Never copy Set-Cookie, Location, authorization or other upstream headers.
	jsonOut(w, response.StatusCode, json.RawMessage(data))
}

func cbtProxyBody(path string, body []byte) ([]byte, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		body = []byte("{}")
	}
	decode := func(target any) error {
		d := json.NewDecoder(bytes.NewReader(body))
		d.DisallowUnknownFields()
		if err := d.Decode(target); err != nil {
			return errors.New("invalid CBT request object")
		}
		if d.Decode(new(any)) != io.EOF {
			return errors.New("expected one CBT request object")
		}
		return nil
	}
	if path == "/api/cbt/restore" {
		var input struct {
			Generation string `json:"generation"`
			TargetName string `json:"target_name"`
		}
		if err := decode(&input); err != nil {
			return nil, err
		}
		if !cbtGenerationID.MatchString(input.Generation) || !cbtTargetName.MatchString(input.TargetName) {
			return nil, errors.New("restore requires a 32-character generation ID and a safe target_name of 1–80 letters, digits, underscores or hyphens")
		}
		return json.Marshal(input)
	}
	if path == "/api/cbt/verify" {
		var input struct {
			Generation string `json:"generation,omitempty"`
		}
		if err := decode(&input); err != nil {
			return nil, err
		}
		if input.Generation != "" && !cbtGenerationID.MatchString(input.Generation) {
			return nil, errors.New("verify generation must contain 32 lowercase hexadecimal characters")
		}
		return json.Marshal(input)
	}
	var input struct{}
	if err := decode(&input); err != nil {
		return nil, err
	}
	return []byte("{}"), nil
}
