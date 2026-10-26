package response

import (
	"strings"
	"testing"
	"time"
)

func TestParseHeaderDumpKeepsFinalResponse(t *testing.T) {
	dump := []byte("HTTP/1.1 100 Continue\r\n\r\n" +
		"HTTP/1.1 302 Found\r\nLocation: /next\r\nSet-Cookie: a=1\r\n\r\n" +
		"HTTP/2 200 OK\r\nContent-Type: application/json; charset=utf-8\r\n" +
		"X-Test: one\r\nX-Test: two\r\n\r\n")
	status, headers := ParseHeaderDump(dump)
	if status.Code != 200 || status.Reason != "OK" || status.Proto != "HTTP/2" {
		t.Fatalf("status = %#v", status)
	}
	if headers.ContentType() != "application/json" {
		t.Fatalf("content type = %q", headers.ContentType())
	}
	if got := headers.Get("x-test"); got != "one" {
		t.Fatalf("first X-Test = %q", got)
	}
	if len(headers) != 3 {
		t.Fatalf("headers = %#v", headers)
	}
}

func TestParseHeaderDumpIgnoresProxyConnect(t *testing.T) {
	dump := []byte("HTTP/1.1 200 Connection established\r\nProxy-Agent: mitmproxy\r\n\r\n")
	status, headers := ParseHeaderDump(dump)
	if status.Code != 0 || len(headers) != 0 {
		t.Fatalf("CONNECT proxy response treated as HTTP response: %#v %#v", status, headers)
	}

	dump = append(dump, []byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\n")...)
	status, headers = ParseHeaderDump(dump)
	if status.Code != 200 || headers.Get("Content-Type") != "text/plain" {
		t.Fatalf("final response not selected after CONNECT: %#v %#v", status, headers)
	}
}

func TestParseHeaderDumpWithoutStatus(t *testing.T) {
	status, headers := ParseHeaderDump(nil)
	if status.Code != 0 || len(headers) != 0 {
		t.Fatalf("unexpected result: %#v %#v", status, headers)
	}
}

func TestParseCurlMeta(t *testing.T) {
	raw := []byte("\n__CRL_STATUS__201\n__CRL_DURATION__0.024500\n" +
		"__CRL_SIZE__17\n__CRL_URL__http://example.test/final\n")
	meta := ParseCurlMeta(raw)
	if meta.HTTPCode != 201 || meta.Size != 17 || meta.EffectiveURL != "http://example.test/final" {
		t.Fatalf("meta = %#v", meta)
	}
	if meta.Duration != 24500*time.Microsecond {
		t.Fatalf("duration = %s", meta.Duration)
	}
}

func TestRenderBodyJSONModes(t *testing.T) {
	body := []byte(`{"b":2,"a":[1,2]}`)
	view := RenderBody(body, int64(len(body)), 4096, ModeAuto, "application/json")
	if view.Kind != BodyJSON || !strings.Contains(view.Text, "\n  \"b\": 2") {
		t.Fatalf("auto view = %#v", view)
	}
	raw := RenderBody(body, int64(len(body)), 4096, ModeRaw, "application/json")
	if raw.Text != string(body) {
		t.Fatalf("raw body changed: %q", raw.Text)
	}
}

func TestRenderBodyHTML(t *testing.T) {
	body := []byte(`<!doctype html><html><body><h1>400 Bad Request</h1><p>Invalid input</p></body></html>`)
	view := RenderBody(body, int64(len(body)), 4096, ModeAuto, "text/html; charset=utf-8")
	if view.Kind != BodyHTML {
		t.Fatalf("kind = %s", view.Kind)
	}
	if strings.Contains(view.Text, `"400"`) || !strings.Contains(view.Text, "<h1>") {
		t.Fatalf("HTML rendu comme JSON :\n%s", view.Text)
	}
	if !strings.Contains(view.Text, "\n    <h1>") {
		t.Fatalf("HTML non aéré :\n%s", view.Text)
	}
}

func TestRenderBodyTruncatedIsExplicit(t *testing.T) {
	body := []byte(`{"long":"` + strings.Repeat("x", 100) + `"}`)
	view := RenderBody(body, int64(len(body)), 20, ModePretty, "application/json")
	if !view.Truncated {
		t.Fatal("truncation not reported")
	}
	if strings.Contains(view.Text, "\n  \"long\"") {
		t.Fatalf("truncated body should not be reformatted:\n%s", view.Text)
	}
}

func TestRenderBodyBinary(t *testing.T) {
	body := []byte{0, 1, 2, 3}
	view := RenderBody(body, int64(len(body)), 4096, ModeAuto, "application/octet-stream")
	if view.Kind != BodyBinary || !strings.Contains(view.Text, "corps binaire") {
		t.Fatalf("binary view = %#v", view)
	}
}

func TestClassifyCurlFailure(t *testing.T) {
	if f := ClassifyCurlFailure(28, ""); !f.IsTimeout() || !strings.Contains(f.Label, "délai") {
		t.Fatalf("timeout = %#v", f)
	}
	if f := ClassifyCurlFailure(7, ""); !f.IsConnection() {
		t.Fatalf("connection = %#v", f)
	}
	if f := ClassifyCurlFailure(60, ""); !f.IsConnection() || !strings.Contains(f.Label, "certificat") {
		t.Fatalf("TLS = %#v", f)
	}
}
