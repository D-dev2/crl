package mint

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"crl-cli/internal/request"
)

func newMintTestHTTPServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
			t.Skipf("environnement sans accès aux sockets loopback : %v", err)
		}
		t.Fatalf("impossible d'ouvrir un socket loopback pour le test : %v", err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Listener = listener
	server.Start()
	return server
}

func TestAddonPythonSyntax(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 absent")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "addon.py")
	if err := os.WriteFile(path, addonSource, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("python3", "-m", "py_compile", path).CombinedOutput()
	if err != nil {
		t.Fatalf("addon.py ne compile pas : %v\n%s", err, out)
	}
}

// TestRealMitmdumpCapture ne s'exécute que si mitmdump est réellement
// installé. Elle vérifie la capture, l'association requête/réponse et
// l'importation via un serveur HTTP local, sans dépendance Internet.
func TestRealMitmdumpCapture(t *testing.T) {
	if testing.Short() {
		t.Skip("test d'intégration désactivé en mode -short")
	}
	mitmdump, err := exec.LookPath("mitmdump")
	if err != nil {
		t.Skip("mitmdump absent : test d'intégration réel ignoré")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl absent")
	}

	cfg := DefaultConfig()
	cfg.Port = freePort(t)
	cfg.MitmdumpPath = mitmdump
	cfg.StartTimeout = 25 * time.Second

	hist := NewHistory(DefaultHistoryConfig())
	m, err := NewManager(cfg, hist)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	server := newMintTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	info, err := m.Start(ctx)
	if err != nil {
		t.Fatalf("démarrage mitmdump : %v", err)
	}

	curl := exec.Command("curl",
		"--globoff", "--path-as-is", "-sS", "-o", os.DevNull,
		"--max-time", "15",
		"--proxy", info.ProxyURL, "--noproxy", "",
		"-X", "POST",
		"-H", "X-Test: demo",
		"-H", "Content-Type: application/json",
		"--data-binary", `{"a":1}`,
		server.URL+"/capture/a%2Fb?x=1&x=2&empty=",
	)
	if out, err := curl.CombinedOutput(); err != nil {
		t.Fatalf("curl via Mint : %v\n%s", err, out)
	}

	waitFor(t, 10*time.Second, func() bool {
		req, resp := hist.Counts()
		return req >= 1 && resp >= 1
	})

	var capture Capture
	for _, c := range hist.List() {
		if strings.Contains(c.Request.URL, "/capture/a%2Fb") {
			capture = c
			break
		}
	}
	if capture.ID == 0 {
		t.Fatalf("capture introuvable : %#v", hist.List())
	}
	if capture.Request.Method != "POST" {
		t.Fatalf("method = %q", capture.Request.Method)
	}
	if capture.Request.RawQuery != "x=1&x=2&empty=" {
		t.Fatalf("query = %q", capture.Request.RawQuery)
	}
	if string(capture.Request.Body) != `{"a":1}` {
		t.Fatalf("body = %q", capture.Request.Body)
	}
	if capture.Response == nil || capture.Response.StatusCode != 200 {
		t.Fatalf("response = %#v", capture.Response)
	}

	headers := make([]request.Header, 0, len(capture.Request.Headers))
	for _, h := range capture.Request.Headers {
		headers = append(headers, request.Header{Name: h.Name, Value: h.Value})
	}
	builder := request.New()
	if err := builder.Import(request.CapturedRequest{
		URL:         capture.Request.URL,
		Method:      capture.Request.Method,
		Headers:     headers,
		Body:        capture.Request.Body,
		BodyPresent: len(capture.Request.Body) > 0,
	}); err != nil {
		t.Fatal(err)
	}
	if builder.URL() != capture.Request.URL {
		t.Fatalf("URL importée = %q, attendu %q", builder.URL(), capture.Request.URL)
	}
	if string(builder.Body()) != `{"a":1}` {
		t.Fatalf("corps importé = %q", builder.Body())
	}

	if err := m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}
