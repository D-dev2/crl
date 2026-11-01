package shell

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"crl-cli/internal/mint"
	"crl-cli/internal/request"
)

func authNewTestShell(t *testing.T) (*Shell, *bytes.Buffer, *mint.Manager, *mint.History, *request.Builder) {
	t.Helper()
	hist := mint.NewHistory(mint.DefaultHistoryConfig())
	mgr, err := mint.NewManager(mint.DefaultConfig(), hist)
	if err != nil {
		t.Fatal(err)
	}
	builder := request.New()
	var out bytes.Buffer
	sh, err := New(Options{
		In:      strings.NewReader(""),
		Out:     &out,
		Builder: builder,
		Manager: mgr,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sh.Close)
	return sh, &out, mgr, hist, builder
}

// captureServer records requests and responds according to handler.
type captureServer struct {
	mu       sync.Mutex
	requests []http.Request
	bodies   [][]byte
	srv      *httptest.Server
}

func newCaptureServer(t *testing.T, handler http.HandlerFunc) *captureServer {
	t.Helper()
	s := &captureServer{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests = append(s.requests, *r.Clone(context.Background()))
		body, _ := io.ReadAll(r.Body)
		s.bodies = append(s.bodies, body)
		s.mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *captureServer) lastRequest() (http.Request, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) == 0 {
		return http.Request{}, false
	}
	return s.requests[len(s.requests)-1], true
}

func (s *captureServer) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func TestAuthBasicTransmitted(t *testing.T) {
	if !curlAvailable(t) {
		return
	}
	var gotAuth string
	srv := newCaptureServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, "basic-ok")
	})

	sh, out, _, _, builder := authNewTestShell(t)
	_ = builder.SetURL(srv.srv.URL + "/basic")
	_ = builder.SetHeader("Authorization", "Basic dXNlcjpwYXNz")
	sh.Execute("crl.launch")
	text := out.String()
	if !strings.Contains(text, "basic-ok") {
		t.Fatalf("basic response absent: %s", text)
	}
	if gotAuth != "Basic dXNlcjpwYXNz" {
		t.Fatalf("Authorization not transmitted: %q", gotAuth)
	}
}

func TestAuthBearerTransmitted(t *testing.T) {
	if !curlAvailable(t) {
		return
	}
	var gotAuth string
	srv := newCaptureServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, "bearer-ok")
	})

	sh, out, _, _, builder := authNewTestShell(t)
	_ = builder.SetURL(srv.srv.URL + "/bearer")
	_ = builder.SetHeader("Authorization", "Bearer faketoken123")
	sh.Execute("crl.launch")
	text := out.String()
	if !strings.Contains(text, "bearer-ok") {
		t.Fatalf("bearer response absent: %s", text)
	}
	if gotAuth != "Bearer faketoken123" {
		t.Fatalf("Authorization not transmitted: %q", gotAuth)
	}
}

func TestAuthMultipleCookiesTransmitted(t *testing.T) {
	if !curlAvailable(t) {
		return
	}
	var gotCookie string
	srv := newCaptureServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotCookie = r.Header.Get("Cookie")
		_, _ = io.WriteString(w, "cookies-ok")
	})

	sh, out, _, _, builder := authNewTestShell(t)
	_ = builder.SetURL(srv.srv.URL + "/cookies")
	_ = builder.SetHeader("Cookie", "session=abc; theme=dark")
	sh.Execute("crl.launch")
	text := out.String()
	if !strings.Contains(text, "cookies-ok") {
		t.Fatalf("cookies response absent: %s", text)
	}
	if gotCookie != "session=abc; theme=dark" {
		t.Fatalf("Cookie not transmitted: %q", gotCookie)
	}
}

func TestAuthSetCookiePreservedInHistory(t *testing.T) {
	if !curlAvailable(t) {
		return
	}
	srv := newCaptureServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "session=abc; Path=/")
		w.Header().Add("Set-Cookie", "theme=dark; Path=/")
		_, _ = io.WriteString(w, "setcookie-ok")
	})

	sh, out, _, hist, builder := authNewTestShell(t)
	_ = builder.SetURL(srv.srv.URL + "/setcookie")
	sh.Execute("crl.launch")
	text := out.String()
	if !strings.Contains(text, "setcookie-ok") {
		t.Fatalf("setcookie response absent: %s", text)
	}

	// Add the capture manually to history (as mint.use would import it) to
	// verify Set-Cookie is preserved and displayed.
	hist.AddRequest(mint.BridgeEvent{
		Type:      "request",
		FlowID:    "setcookie-flow",
		Timestamp: time.Now(),
		Method:    "GET",
		URL:       srv.srv.URL + "/setcookie",
		BodySize:  0,
	})
	hist.AddResponse(mint.BridgeEvent{
		Type:       "response",
		FlowID:     "setcookie-flow",
		Timestamp:  time.Now(),
		StatusCode: 200,
		Reason:     "OK",
		Headers: []mint.Header{
			{Name: "Set-Cookie", Value: "session=abc; Path=/"},
			{Name: "Set-Cookie", Value: "theme=dark; Path=/"},
			{Name: "Content-Type", Value: "text/plain"},
		},
		Body:     []byte("setcookie-ok"),
		BodySize: 13,
	})

	// Find the capture by URL.
	captures := hist.List()
	var found *mint.Capture
	for i := range captures {
		c := captures[i]
		if c.Request.URL == srv.srv.URL+"/setcookie" {
			found = &c
			break
		}
	}
	if found == nil {
		urls := make([]string, 0, len(captures))
		for _, c := range captures {
			urls = append(urls, c.Request.URL)
		}
		t.Fatalf("capture not found in history: %d captures, urls=%v, target=%q", len(captures), urls, srv.srv.URL+"/setcookie")
	}
	var hasSession, hasTheme bool
	for _, h := range found.Response.Headers {
		if strings.EqualFold(h.Name, "Set-Cookie") {
			if strings.Contains(h.Value, "session=abc") {
				hasSession = true
			}
			if strings.Contains(h.Value, "theme=dark") {
				hasTheme = true
			}
		}
	}
	if !hasSession {
		t.Fatalf("Set-Cookie session not preserved in history: %#v", found.Response.Headers)
	}
	if !hasTheme {
		t.Fatalf("Set-Cookie theme not preserved in history: %#v", found.Response.Headers)
	}
}

func TestAuthReplayPreservesCredentials(t *testing.T) {
	if !curlAvailable(t) {
		return
	}
	var gotAuth, gotCookie string
	srv := newCaptureServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCookie = r.Header.Get("Cookie")
		_, _ = io.WriteString(w, "replay-ok")
	})

	sh, out, _, hist, _ := authNewTestShell(t)
	hist.AddRequest(mint.BridgeEvent{
		Type:      "request",
		FlowID:    "replay-auth",
		Timestamp: time.Now(),
		Method:    "GET",
		URL:       srv.srv.URL + "/replay",
		Headers: []mint.Header{
			{Name: "Authorization", Value: "Bearer replay-secret"},
			{Name: "Cookie", Value: "sess=replay"},
			{Name: "X-Api-Key", Value: "key-replay"},
		},
		BodySize: 0,
	})
	hist.AddResponse(mint.BridgeEvent{
		Type:       "response",
		FlowID:     "replay-auth",
		Timestamp:  time.Now(),
		StatusCode: 200,
		Reason:     "OK",
		Body:       []byte("{}"),
		BodySize:   2,
	})

	sh.Execute("mint.select 1")
	out.Reset()
	sh.Execute("mint.replay")
	text := out.String()
	if !strings.Contains(text, "replay-ok") {
		t.Fatalf("replay response absent: %s", text)
	}
	if gotAuth != "Bearer replay-secret" {
		t.Fatalf("Authorization not replayed: %q", gotAuth)
	}
	if gotCookie != "sess=replay" {
		t.Fatalf("Cookie not replayed: %q", gotCookie)
	}
}

func TestAuthManualTokenModificationBeforeReplay(t *testing.T) {
	if !curlAvailable(t) {
		return
	}
	var gotAuth string
	srv := newCaptureServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, "modified-ok")
	})

	sh, out, _, hist, _ := authNewTestShell(t)
	hist.AddRequest(mint.BridgeEvent{
		Type:      "request",
		FlowID:    "modify-auth",
		Timestamp: time.Now(),
		Method:    "GET",
		URL:       srv.srv.URL + "/modify",
		Headers:   []mint.Header{{Name: "Authorization", Value: "Bearer old-token"}},
		BodySize:  0,
	})
	hist.AddResponse(mint.BridgeEvent{
		Type:       "response",
		FlowID:     "modify-auth",
		Timestamp:  time.Now(),
		StatusCode: 200,
		Reason:     "OK",
		Body:       []byte("{}"),
		BodySize:   2,
	})

	sh.Execute("mint.select 1")
	out.Reset()
	sh.Execute("crl.head set 1 add Authorization: Bearer new-token")
	out.Reset()
	sh.Execute("mint.replay")
	text := out.String()
	if !strings.Contains(text, "modified-ok") {
		t.Fatalf("modified replay response absent: %s", text)
	}
	if gotAuth != "Bearer new-token" {
		t.Fatalf("modified token not used: %q", gotAuth)
	}
}

func TestAuthSameOriginRedirectKeepsCredentials(t *testing.T) {
	if !curlAvailable(t) {
		return
	}
	var gotAuth2, gotCookie2 string
	srv := newCaptureServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			w.Header().Set("Location", "/target")
			w.WriteHeader(http.StatusFound)
			_, _ = io.WriteString(w, "redirect-body")
			return
		}
		gotAuth2 = r.Header.Get("Authorization")
		gotCookie2 = r.Header.Get("Cookie")
		_, _ = io.WriteString(w, "target-ok")
	})

	sh, out, _, _, builder := authNewTestShell(t)
	_ = builder.SetURL(srv.srv.URL + "/start")
	_ = builder.SetHeader("Authorization", "Bearer same-origin")
	_ = builder.SetHeader("Cookie", "session=same")
	sh.Execute("crl.launch")
	text := out.String()
	if !strings.Contains(text, "target-ok") {
		t.Fatalf("same-origin redirect not followed: %s", text)
	}
	if gotAuth2 != "Bearer same-origin" {
		t.Fatalf("Authorization not kept on same-origin redirect: %q", gotAuth2)
	}
	if gotCookie2 != "session=same" {
		t.Fatalf("Cookie not kept on same-origin redirect: %q", gotCookie2)
	}
}

func TestAuthCrossOriginRedirectDropsCredentialsByDefault(t *testing.T) {
	if !curlAvailable(t) {
		return
	}
	var gotAuth, gotCookie string
	target := newCaptureServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCookie = r.Header.Get("Cookie")
		_, _ = io.WriteString(w, "cross-ok")
	})
	start := newCaptureServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target.srv.URL+"/target")
		w.WriteHeader(http.StatusFound)
	})

	sh, out, _, _, builder := authNewTestShell(t)
	_ = builder.SetURL(start.srv.URL + "/start")
	_ = builder.SetHeader("Authorization", "Bearer cross-secret")
	_ = builder.SetHeader("Cookie", "session=cross")
	sh.Execute("crl.launch")
	text := out.String()
	if !strings.Contains(text, "cross-ok") {
		t.Fatalf("cross-origin redirect not followed: %s", text)
	}
	if gotAuth != "" {
		t.Fatalf("Authorization leaked cross-origin: %q", gotAuth)
	}
	if gotCookie != "" {
		t.Fatalf("Cookie leaked cross-origin: %q", gotCookie)
	}
}

func TestAuthCrossOriginRedirectSendsCredentialsWhenEnabled(t *testing.T) {
	if !curlAvailable(t) {
		return
	}
	var gotAuth, gotCookie string
	target := newCaptureServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCookie = r.Header.Get("Cookie")
		_, _ = io.WriteString(w, "cross-on-ok")
	})
	start := newCaptureServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target.srv.URL+"/target")
		w.WriteHeader(http.StatusFound)
	})

	sh, out, _, _, builder := authNewTestShell(t)
	_ = builder.SetURL(start.srv.URL + "/start")
	_ = builder.SetHeader("Authorization", "Bearer cross-on-secret")
	_ = builder.SetHeader("Cookie", "session=cross-on")
	sh.Execute("crl.redirect.credentials on")
	out.Reset()
	sh.Execute("crl.launch")
	text := out.String()
	if !strings.Contains(text, "cross-on-ok") {
		t.Fatalf("cross-origin redirect with credentials not followed: %s", text)
	}
	if gotAuth != "Bearer cross-on-secret" {
		t.Fatalf("Authorization not sent cross-origin when enabled: %q", gotAuth)
	}
	if gotCookie != "session=cross-on" {
		t.Fatalf("Cookie not sent cross-origin when enabled: %q", gotCookie)
	}
}

func TestAuthCredentialsPreservedInHistoryAndDisplayed(t *testing.T) {
	if !curlAvailable(t) {
		return
	}
	srv := newCaptureServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "history-ok")
	})

	sh, out, _, hist, builder := authNewTestShell(t)
	_ = builder.SetURL(srv.srv.URL + "/history")
	_ = builder.SetHeader("Authorization", "Bearer history-secret")
	_ = builder.SetHeader("Cookie", "session=history")
	_ = builder.SetHeader("X-Api-Key", "key-history")
	builder.SetBodyText(`{"token":"body-token"}`)
	sh.Execute("crl.launch")
	text := out.String()
	if !strings.Contains(text, "history-ok") {
		t.Fatalf("history response absent: %s", text)
	}

	// Add the capture manually to history to verify values are preserved and
	// displayed (direct curl launches are not recorded by Mint history).
	hist.AddRequest(mint.BridgeEvent{
		Type:      "request",
		FlowID:    "history-flow",
		Timestamp: time.Now(),
		Method:    "GET",
		URL:       srv.srv.URL + "/history",
		Headers: []mint.Header{
			{Name: "Authorization", Value: "Bearer history-secret"},
			{Name: "Cookie", Value: "session=history"},
			{Name: "X-Api-Key", Value: "key-history"},
		},
		Body:     []byte(`{"token":"body-token"}`),
		BodySize: int64(len(`{"token":"body-token"}`)),
	})
	hist.AddResponse(mint.BridgeEvent{
		Type:       "response",
		FlowID:     "history-flow",
		Timestamp:  time.Now(),
		StatusCode: 200,
		Reason:     "OK",
		Body:       []byte("history-ok"),
		BodySize:   10,
	})

	// crl.show should display full values.
	out.Reset()
	sh.Execute("crl.show")
	showText := out.String()
	for _, want := range []string{"Bearer history-secret", "session=history", "key-history", "body-token"} {
		if !strings.Contains(showText, want) {
			t.Fatalf("crl.show missing %q:\n%s", want, showText)
		}
	}
	if strings.Contains(showText, "***") {
		t.Fatalf("crl.show masks values:\n%s", showText)
	}

	// mint.request.show should also display full values.
	var captureID int
	for _, c := range hist.List() {
		if c.Request.URL == srv.srv.URL+"/history" {
			captureID = c.ID
			break
		}
	}
	if captureID == 0 {
		t.Fatal("capture not found in history")
	}
	out.Reset()
	sh.Execute("mint.request.show " + strconv.Itoa(captureID))
	detailText := out.String()
	for _, want := range []string{"Bearer history-secret", "session=history", "key-history"} {
		if !strings.Contains(detailText, want) {
			t.Fatalf("mint.request.show missing %q:\n%s", want, detailText)
		}
	}
	if strings.Contains(detailText, "***") {
		t.Fatalf("mint.request.show masks values:\n%s", detailText)
	}
}

func TestAuthRedirectCredentialsCommandState(t *testing.T) {
	sh, out, _, _, _ := authNewTestShell(t)
	sh.Execute("crl.redirect.credentials")
	if !strings.Contains(out.String(), "désactivé") {
		t.Fatalf("credentials should be disabled by default:\n%s", out.String())
	}
	if sh.redirectCredentials {
		t.Fatal("redirectCredentials should be false by default")
	}
	out.Reset()
	sh.Execute("crl.redirect.credentials on")
	if !strings.Contains(out.String(), "activé") {
		t.Fatalf("credentials on not applied:\n%s", out.String())
	}
	if !sh.redirectCredentials {
		t.Fatal("redirectCredentials should be true after on")
	}
	out.Reset()
	sh.Execute("crl.redirect.credentials off")
	if !strings.Contains(out.String(), "désactivé") {
		t.Fatalf("credentials off not applied:\n%s", out.String())
	}
	if sh.redirectCredentials {
		t.Fatal("redirectCredentials should be false after off")
	}
	out.Reset()
	sh.Execute("crl.redirect.credentials maybe")
	if !strings.Contains(out.String(), "usage") {
		t.Fatalf("invalid value accepted:\n%s", out.String())
	}
}
