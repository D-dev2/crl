package mint

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func testBridge(t *testing.T, maxBody int64) *Bridge {
	t.Helper()
	b, err := NewBridge(maxBody)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func bridgeClient() *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			Proxy: nil, // jamais de proxy pour le pont local
		},
	}
}

func postBridge(t *testing.T, b *Bridge, token string, payload []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, b.URL()+"/events", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := bridgeClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func waitEvent(t *testing.T, b *Bridge) BridgeEvent {
	t.Helper()
	select {
	case ev, ok := <-b.Events():
		if !ok {
			t.Fatal("event channel closed")
		}
		return ev
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for bridge event")
		return BridgeEvent{}
	}
}

func marshalEvent(t *testing.T, ev wireEvent) []byte {
	t.Helper()
	data, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestBridgeAuthorizedRequest(t *testing.T) {
	b := testBridge(t, 1024)
	payload := marshalEvent(t, wireEvent{
		Type:        "request",
		FlowID:      "flow-1",
		Timestamp:   time.Now().UTC().Format(time.RFC3339Nano),
		Method:      "post",
		URL:         "http://127.0.0.1:5002/users?x=1",
		HTTPVersion: "HTTP/1.1",
		Headers:     []Header{{Name: "Authorization", Value: "Bearer secret"}},
		BodyBase64:  base64.StdEncoding.EncodeToString([]byte("hello")),
		BodySize:    5,
	})
	resp := postBridge(t, b, b.token, payload)
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body=%s", resp.StatusCode, body)
	}
	ev := waitEvent(t, b)
	if ev.Type != "request" || ev.FlowID != "flow-1" || ev.Method != "POST" || ev.URL != "http://127.0.0.1:5002/users?x=1" {
		t.Fatalf("event = %#v", ev)
	}
	if string(ev.Body) != "hello" || ev.BodySize != 5 || len(ev.Headers) != 1 {
		t.Fatalf("body/headers = %#v", ev)
	}
}

func TestBridgeRejectsUnauthorized(t *testing.T) {
	b := testBridge(t, 1024)
	payload := marshalEvent(t, wireEvent{Type: "ready", Version: 1})
	for _, token := range []string{"", "wrong", b.token[:len(b.token)-1]} {
		resp := postBridge(t, b, token, payload)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("token %q: status = %d", token, resp.StatusCode)
		}
	}
	select {
	case ev := <-b.Events():
		t.Fatalf("unauthorized event leaked: %#v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestBridgeRejectsInvalidJSON(t *testing.T) {
	b := testBridge(t, 1024)
	for _, payload := range [][]byte{
		[]byte("{"),
		[]byte(`{"type":"request","flow_id":"x","url":"http://example.test/","method":"GET"} trailing`),
		[]byte(`[]`),
	} {
		resp := postBridge(t, b, b.token, payload)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("payload %q: status = %d", payload, resp.StatusCode)
		}
	}
}

func TestBridgeRejectsOversizedBody(t *testing.T) {
	b := testBridge(t, 16)
	payload := marshalEvent(t, wireEvent{
		Type:       "request",
		FlowID:     "flow-big",
		Method:     "POST",
		URL:        "http://example.test/",
		BodyBase64: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("x"), 17)),
		BodySize:   17,
	})
	resp := postBridge(t, b, b.token, payload)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestBridgeRejectsHugeHTTPPayload(t *testing.T) {
	b := testBridge(t, 1024)
	huge := append([]byte(`{"type":"request","flow_id":"x","padding":"`), bytes.Repeat([]byte("a"), 3<<20)...)
	huge = append(huge, []byte(`"}`)...)
	resp := postBridge(t, b, b.token, huge)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
}

func TestBridgeRejectsIncompleteBodyWithoutFlag(t *testing.T) {
	b := testBridge(t, 1024)
	payload := marshalEvent(t, wireEvent{
		Type:       "request",
		FlowID:     "flow-1",
		Method:     "POST",
		URL:        "http://example.test/",
		BodyBase64: base64.StdEncoding.EncodeToString([]byte("short")),
		BodySize:   500,
	})
	resp := postBridge(t, b, b.token, payload)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestBridgeValidation(t *testing.T) {
	b := testBridge(t, 1024)
	cases := []wireEvent{
		{Type: "request", FlowID: "", Method: "GET", URL: "http://example.test/"},
		{Type: "request", FlowID: "f", Method: "GET", URL: "ftp://example.test/"},
		{Type: "request", FlowID: "f", Method: "GE T", URL: "http://example.test/"},
		{Type: "response", FlowID: "f", StatusCode: 99},
		{Type: "response", FlowID: "f", StatusCode: 600},
		{Type: "unknown", FlowID: "f"},
		{Type: "request", FlowID: "f", Method: "GET", URL: "http://example.test/", Headers: []Header{{Name: "Bad Header", Value: "x"}}},
		{Type: "request", FlowID: "f", Method: "GET", URL: "http://example.test/", Headers: []Header{{Name: "X-Test", Value: "a\nb"}}},
	}
	for i, ev := range cases {
		resp := postBridge(t, b, b.token, marshalEvent(t, ev))
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("case %d (%#v): status = %d, want 400", i, ev, resp.StatusCode)
		}
	}
}

func TestBridgeSequentialEvents(t *testing.T) {
	b := testBridge(t, 1024)
	request := wireEvent{
		Type:       "request",
		FlowID:     "flow-seq",
		Method:     "GET",
		URL:        "http://example.test/a",
		BodyBase64: base64.StdEncoding.EncodeToString([]byte("body")),
		BodySize:   4,
	}
	response := wireEvent{
		Type:       "response",
		FlowID:     "flow-seq",
		StatusCode: 204,
		DurationMS: 12.5,
	}
	errorEvent := wireEvent{
		Type:   "error",
		FlowID: "flow-err",
		Error:  "connection reset",
	}
	for _, ev := range []wireEvent{request, response, errorEvent} {
		resp := postBridge(t, b, b.token, marshalEvent(t, ev))
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("status = %d", resp.StatusCode)
		}
	}
	if ev := waitEvent(t, b); ev.Type != "request" || ev.FlowID != "flow-seq" {
		t.Fatalf("event 1 = %#v", ev)
	}
	if ev := waitEvent(t, b); ev.Type != "response" || ev.StatusCode != 204 || ev.Duration != 12500*time.Microsecond {
		t.Fatalf("event 2 = %#v", ev)
	}
	if ev := waitEvent(t, b); ev.Type != "error" || !strings.Contains(ev.Error, "reset") {
		t.Fatalf("event 3 = %#v", ev)
	}
}

func TestBridgeCloseClosesEvents(t *testing.T) {
	b, err := NewBridge(1024)
	if err != nil {
		t.Fatal(err)
	}
	addr := b.Addr()
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case _, ok := <-b.Events():
		if ok {
			t.Fatal("event channel should be closed")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("events channel not closed")
	}
	if err := b.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if _, err := http.Get("http://" + addr + "/events"); err == nil {
		t.Fatal("bridge still listening after close")
	}
}

func TestBridgeOnlyLoopback(t *testing.T) {
	b := testBridge(t, 1024)
	host, _, err := strings.Cut(b.Addr(), ":")
	if !err {
		t.Fatalf("unexpected addr %q", b.Addr())
	}
	if host != "127.0.0.1" {
		t.Fatalf("bridge host = %q", host)
	}
	if len(b.token) != 64 {
		t.Fatalf("token length = %d", len(b.token))
	}
}
