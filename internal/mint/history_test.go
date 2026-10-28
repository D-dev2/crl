package mint

import (
	"strings"
	"testing"
	"time"
)

func reqEvent(flow, rawURL, method string, body []byte) BridgeEvent {
	return BridgeEvent{
		Type:      "request",
		FlowID:    flow,
		Timestamp: time.Unix(1700000000, 0),
		Method:    method,
		URL:       rawURL,
		Headers:   []Header{{Name: "Authorization", Value: "Bearer secret"}},
		Body:      body,
		BodySize:  int64(len(body)),
	}
}

func respEvent(flow string, status int, body []byte, duration time.Duration) BridgeEvent {
	return BridgeEvent{
		Type:       "response",
		FlowID:     flow,
		Timestamp:  time.Unix(1700000000, 0).Add(duration),
		StatusCode: status,
		Body:       body,
		BodySize:   int64(len(body)),
		Duration:   duration,
	}
}

func TestAddRequestAndResponseSameCapture(t *testing.T) {
	h := NewHistory(DefaultHistoryConfig())
	c, isNew := h.AddRequest(reqEvent("flow-1", "http://127.0.0.1:5002/users?x=1&x=2", "POST", []byte(`{"a":1}`)))
	if !isNew || c.ID != 1 {
		t.Fatalf("capture = %#v, new=%v", c, isNew)
	}
	if c.Request.Host != "127.0.0.1:5002" || c.Request.Path != "/users" || c.Request.RawQuery != "x=1&x=2" {
		t.Fatalf("URL split wrong: %#v", c.Request)
	}
	if c.Response != nil || c.StatusLabel() != "PENDING" {
		t.Fatalf("pending state wrong: %#v", c)
	}
	if ok, _ := c.Importable(); !ok {
		t.Fatal("pending request should be importable")
	}

	c = h.AddResponse(respEvent("flow-1", 200, []byte(`{"ok":true}`), 24*time.Millisecond))
	if c.ID != 1 || c.Response == nil || c.Response.StatusCode != 200 {
		t.Fatalf("response not associated: %#v", c)
	}
	if c.Duration != 24*time.Millisecond || c.StatusLabel() != "200" {
		t.Fatalf("duration/status wrong: %#v", c)
	}
	req, resp := h.Counts()
	if req != 1 || resp != 1 {
		t.Fatalf("counts = %d/%d", req, resp)
	}

	all := h.List()
	if len(all) != 1 {
		t.Fatalf("expected one capture, got %d", len(all))
	}
}

func TestSelectManyKeepsOrderedPersistentSelection(t *testing.T) {
	h := NewHistory(DefaultHistoryConfig())
	h.AddRequest(reqEvent("a", "http://a.test/", "GET", nil))
	h.AddRequest(reqEvent("b", "http://b.test/", "POST", []byte("x")))
	got, err := h.SelectMany([]int{2, 1, 2})
	if err != nil || len(got) != 2 || got[0].ID != 2 || got[1].ID != 1 {
		t.Fatalf("selection = %#v, %v", got, err)
	}
	if got := h.SelectedMany(); len(got) != 2 || got[0].ID != 2 {
		t.Fatalf("persistent selection = %#v", got)
	}
	if _, err := h.SelectMany([]int{99}); err == nil {
		t.Fatal("unknown ID accepted")
	}
	if got := h.SelectedMany(); len(got) != 2 {
		t.Fatalf("failed selection changed state: %#v", got)
	}
}

func TestDuplicateRequestUpdatesFlow(t *testing.T) {
	h := NewHistory(DefaultHistoryConfig())
	h.AddRequest(reqEvent("flow-1", "http://example.test/old", "GET", []byte("old")))
	h.AddResponse(respEvent("flow-1", 200, []byte("old-response"), 10*time.Millisecond))
	c, isNew := h.AddRequest(reqEvent("flow-1", "http://example.test/new?q=1", "PUT", []byte("new")))
	if isNew || c.ID != 1 {
		t.Fatalf("duplicate created a new capture: %#v new=%v", c, isNew)
	}
	if c.Request.URL != "http://example.test/new?q=1" || string(c.Request.Body) != "new" {
		t.Fatalf("request not updated: %#v", c.Request)
	}
	if c.Response == nil || string(c.Response.Body) != "old-response" {
		t.Fatalf("response lost during update: %#v", c.Response)
	}
	if len(h.List()) != 1 {
		t.Fatalf("expected 1 capture, got %d", len(h.List()))
	}
	if h.TotalBodyBytes() != int64(len("new")+len("old-response")) {
		t.Fatalf("body accounting = %d", h.TotalBodyBytes())
	}
}

func TestResponseBeforeRequest(t *testing.T) {
	h := NewHistory(DefaultHistoryConfig())
	c := h.AddResponse(respEvent("flow-early", 201, []byte("created"), 5*time.Millisecond))
	if c.Request.Received {
		t.Fatal("placeholder should not claim a received request")
	}
	if ok, reason := c.Importable(); ok || !strings.Contains(reason, "pas encore") {
		t.Fatalf("placeholder importable=%v reason=%q", ok, reason)
	}

	c, isNew := h.AddRequest(reqEvent("flow-early", "http://example.test/items", "POST", []byte("x")))
	if isNew || c.ID != 1 {
		t.Fatalf("late request should update placeholder, got %#v new=%v", c, isNew)
	}
	if c.Response == nil || c.Response.StatusCode != 201 {
		t.Fatalf("response lost: %#v", c.Response)
	}
	if ok, _ := c.Importable(); !ok {
		t.Fatal("capture should be importable after request arrives")
	}
}

func TestTransportError(t *testing.T) {
	h := NewHistory(DefaultHistoryConfig())
	h.AddRequest(reqEvent("flow-err", "https://example.test/x", "GET", nil))
	c := h.AddError("flow-err", "connection refused", time.Unix(1700000001, 0))
	if c.Err != "connection refused" || c.StatusLabel() != "ERROR" {
		t.Fatalf("error state = %#v", c)
	}
	if ok, _ := c.Importable(); !ok {
		t.Fatal("request with transport error is still importable")
	}
	if len(c.IncompleteReasons()) == 0 {
		t.Fatal("expected incomplete reason")
	}
}

func TestTruncatedRequestNotImportable(t *testing.T) {
	h := NewHistory(DefaultHistoryConfig())
	ev := reqEvent("flow-big", "http://example.test/upload", "POST", []byte("part"))
	ev.BodySize = 10_000
	ev.BodyTruncated = true
	c, _ := h.AddRequest(ev)
	ok, reason := c.Importable()
	if ok || !strings.Contains(reason, "incomplet") {
		t.Fatalf("importable=%v reason=%q", ok, reason)
	}
	reasons := strings.Join(c.IncompleteReasons(), " ")
	if !strings.Contains(reasons, "tronqué") {
		t.Fatalf("reasons = %q", reasons)
	}
}

func TestFilterDoesNotDeleteHistory(t *testing.T) {
	h := NewHistory(DefaultHistoryConfig())
	h.AddRequest(reqEvent("f1", "http://alpha.test/users", "GET", nil))
	h.AddRequest(reqEvent("f2", "http://beta.test/admin", "GET", nil))
	h.AddRequest(reqEvent("f3", "http://alpha.test/items?q=1", "GET", nil))

	h.SetFilter("alpha")
	filtered := h.Filtered()
	if len(filtered) != 2 {
		t.Fatalf("filtered = %d", len(filtered))
	}
	h.SetFilter("")
	if len(h.List()) != 3 {
		t.Fatalf("filter clear deleted history: %d", len(h.List()))
	}
	h.SetFilter("q=1")
	if len(h.Filtered()) != 1 {
		t.Fatalf("query filter failed")
	}
}

func TestSelectionStableAndEviction(t *testing.T) {
	cfg := DefaultHistoryConfig()
	cfg.MaxCaptures = 2
	h := NewHistory(cfg)
	h.AddRequest(reqEvent("f1", "http://example.test/1", "GET", nil))
	h.AddRequest(reqEvent("f2", "http://example.test/2", "GET", nil))
	if _, ok := h.Select(1); !ok {
		t.Fatal("select failed")
	}
	h.AddRequest(reqEvent("f3", "http://example.test/3", "GET", nil))
	if _, ok := h.Get(1); ok {
		t.Fatal("oldest capture should have been evicted")
	}
	if _, ok := h.Selected(); ok {
		t.Fatal("selection should be cleared when selected capture is evicted")
	}
	if _, ok := h.Select(3); !ok {
		t.Fatal("recent capture should exist")
	}
	h.AddResponse(respEvent("f3", 200, nil, 1*time.Millisecond))
	if c, ok := h.Selected(); !ok || c.ID != 3 || c.Response == nil {
		t.Fatalf("selection lost after response update: %#v %v", c, ok)
	}
}

func TestMaxTotalBodyBytes(t *testing.T) {
	cfg := DefaultHistoryConfig()
	cfg.MaxCaptures = 100
	cfg.MaxTotalBodyBytes = 10
	h := NewHistory(cfg)
	h.AddRequest(reqEvent("f1", "http://example.test/1", "POST", []byte("123456")))
	h.AddRequest(reqEvent("f2", "http://example.test/2", "POST", []byte("abcdef")))
	if len(h.List()) != 1 {
		t.Fatalf("expected memory eviction, got %d captures", len(h.List()))
	}
	if h.TotalBodyBytes() > cfg.MaxTotalBodyBytes {
		t.Fatalf("total = %d > %d", h.TotalBodyBytes(), cfg.MaxTotalBodyBytes)
	}
}

func TestClearKeepsIDsMonotonic(t *testing.T) {
	h := NewHistory(DefaultHistoryConfig())
	h.AddRequest(reqEvent("f1", "http://example.test/1", "GET", nil))
	h.Select(1)
	h.Clear()
	if len(h.List()) != 0 {
		t.Fatal("clear failed")
	}
	if _, ok := h.Selected(); ok {
		t.Fatal("selection should be cleared")
	}
	c, _ := h.AddRequest(reqEvent("f2", "http://example.test/2", "GET", nil))
	if c.ID != 2 {
		t.Fatalf("IDs should not be reused, got %d", c.ID)
	}
}

func TestSnapshotIsIndependent(t *testing.T) {
	h := NewHistory(DefaultHistoryConfig())
	h.AddRequest(reqEvent("f1", "http://example.test/1", "GET", nil))
	c, _ := h.Get(1)
	c.Request.Headers[0].Value = "changed"
	again, _ := h.Get(1)
	if again.Request.Headers[0].Value == "changed" {
		t.Fatal("snapshot shares header slice")
	}
}

func TestPathAndQuery(t *testing.T) {
	h := NewHistory(DefaultHistoryConfig())
	c, _ := h.AddRequest(reqEvent("f1", "http://example.test/a%2Fb?x=1&x=&y=2", "GET", nil))
	if got := c.PathAndQuery(); got != "/a%2Fb?x=1&x=&y=2" {
		t.Fatalf("PathAndQuery = %q", got)
	}
}

func TestValidateURL(t *testing.T) {
	for _, raw := range []string{
		"http://example.test/",
		"https://127.0.0.1:8443/x?y=1",
	} {
		if err := ValidateURL(raw); err != nil {
			t.Fatalf("ValidateURL(%q) = %v", raw, err)
		}
	}
	for _, raw := range []string{"", "ftp://example.test/", "http:///x", "http://a b/"} {
		if err := ValidateURL(raw); err == nil {
			t.Fatalf("ValidateURL(%q) should fail", raw)
		}
	}
}
