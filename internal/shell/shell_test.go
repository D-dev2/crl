package shell

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"crl-cli/internal/mint"
	"crl-cli/internal/request"
)

func newTestShell(t *testing.T, input string) (*Shell, *bytes.Buffer, *mint.Manager, *mint.History, *request.Builder) {
	t.Helper()
	hist := mint.NewHistory(mint.DefaultHistoryConfig())
	mgr, err := mint.NewManager(mint.DefaultConfig(), hist)
	if err != nil {
		t.Fatal(err)
	}
	builder := request.New()
	var out bytes.Buffer
	sh, err := New(Options{
		In:      strings.NewReader(input),
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

func addCapture(t *testing.T, hist *mint.History) {
	t.Helper()
	body := []byte(`{"username":"demo"}`)
	hist.AddRequest(mint.BridgeEvent{
		Type:      "request",
		FlowID:    "flow-1",
		Timestamp: time.Unix(1700000000, 0),
		Method:    "POST",
		URL:       "http://127.0.0.1:5002/users/v1/login?next=%2Fhome&x=1&x=&token=supersecret",
		Headers: []mint.Header{
			{Name: "Authorization", Value: "Bearer supersecret"},
			{Name: "Cookie", Value: "session=abc"},
			{Name: "Content-Type", Value: "application/json"},
			{Name: "X-Test", Value: "demo"},
		},
		Body:     body,
		BodySize: int64(len(body)),
	})
	hist.AddResponse(mint.BridgeEvent{
		Type:       "response",
		FlowID:     "flow-1",
		Timestamp:  time.Unix(1700000000, 0).Add(24 * time.Millisecond),
		StatusCode: 200,
		Reason:     "OK",
		Headers: []mint.Header{
			{Name: "Set-Cookie", Value: "session=abc"},
			{Name: "Content-Type", Value: "application/json"},
		},
		Body:     []byte(`{"ok":true}`),
		BodySize: 11,
		Duration: 24 * time.Millisecond,
	})
}

func TestMintRequestsFilterAndHistory(t *testing.T) {
	sh, out, _, hist, _ := newTestShell(t, "")
	addCapture(t, hist)
	hist.AddRequest(mint.BridgeEvent{
		Type: "request", FlowID: "flow-2", Timestamp: time.Now(),
		Method: "GET", URL: "http://other.test/admin", BodySize: 0,
	})

	if exit := sh.Execute("mint.requests"); exit {
		t.Fatal("unexpected exit")
	}
	text := out.String()
	for _, want := range []string{"MÉTHODE", "127.0.0.1:5002", "/users/v1/login", "200", "PENDING", "other.test"} {
		if !strings.Contains(text, want) {
			t.Fatalf("mint.requests missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "supersecret") {
		t.Fatalf("requests leaked secret:\n%s", text)
	}

	out.Reset()
	sh.Execute("mint.filter 127.0.0.1")
	if !strings.Contains(out.String(), "1 capture(s)") {
		t.Fatalf("filter result:\n%s", out.String())
	}
	out.Reset()
	sh.Execute("mint.requests")
	if strings.Contains(out.String(), "other.test") {
		t.Fatalf("filter not applied:\n%s", out.String())
	}
	out.Reset()
	sh.Execute("mint.filter clear")
	if len(hist.List()) != 2 {
		t.Fatal("filter clear deleted history")
	}
	out.Reset()
	sh.Execute("mint.history")
	if !strings.Contains(out.String(), "other.test") {
		t.Fatalf("history output:\n%s", out.String())
	}
}

func TestMintShowIncludesFullHeaders(t *testing.T) {
	sh, out, _, hist, _ := newTestShell(t, "")
	addCapture(t, hist)
	sh.Execute("mint.request.show 1")
	text := out.String()
	for _, want := range []string{"Capture #1", "Bearer supersecret", "Cookie: session=abc", "Set-Cookie: session=abc", "token=supersecret", "200 OK", "24ms", "/users/v1/login"} {
		if !strings.Contains(text, want) {
			t.Fatalf("show missing %q:\n%s", want, text)
		}
	}
}

func TestMintSelectAndUse(t *testing.T) {
	sh, out, _, hist, builder := newTestShell(t, "")
	addCapture(t, hist)

	sh.Execute("mint.select 1")
	if !strings.Contains(out.String(), "sélectionnée") {
		t.Fatalf("select output:\n%s", out.String())
	}
	if _, ok := hist.Selected(); !ok {
		t.Fatal("selection not stored")
	}
	out.Reset()
	sh.Execute("mint.use 1 --force")
	if !strings.Contains(out.String(), "importée") || !strings.Contains(out.String(), "Aucune requête envoyée") {
		t.Fatalf("use output:\n%s", out.String())
	}
	if builder.URL() != "http://127.0.0.1:5002/users/v1/login?next=%2Fhome&x=1&x=&token=supersecret" {
		t.Fatalf("URL not preserved: %q", builder.URL())
	}
	if builder.Method() != "POST" {
		t.Fatalf("method = %q", builder.Method())
	}
	if string(builder.Body()) != `{"username":"demo"}` {
		t.Fatalf("body = %q", builder.Body())
	}
	headers := builder.Headers()
	if len(headers) != 4 {
		t.Fatalf("headers = %#v", headers)
	}
	if headers[0].Value != "Bearer supersecret" {
		t.Fatalf("real header value not preserved: %#v", headers[0])
	}
	out.Reset()
	sh.Execute("crl.show")
	if !strings.Contains(out.String(), "Bearer supersecret") || !strings.Contains(out.String(), "session=abc") {
		t.Fatalf("crl.show did not show full headers:\n%s", out.String())
	}
}

func TestMintUseRefusesIncompleteRequest(t *testing.T) {
	sh, out, _, hist, builder := newTestShell(t, "")
	hist.AddRequest(mint.BridgeEvent{
		Type: "request", FlowID: "flow-big", Timestamp: time.Now(),
		Method: "POST", URL: "http://example.test/upload",
		Body: []byte("part"), BodySize: 10000, BodyTruncated: true,
	})
	sh.Execute("mint.use 1 --force")
	if !strings.Contains(out.String(), "refusée") || !strings.Contains(out.String(), "incomplet") {
		t.Fatalf("expected refusal:\n%s", out.String())
	}
	if builder.URL() != "" {
		t.Fatalf("builder modified: %q", builder.URL())
	}
}

func TestMintUsePendingResponseWarns(t *testing.T) {
	sh, out, _, hist, builder := newTestShell(t, "")
	hist.AddRequest(mint.BridgeEvent{
		Type: "request", FlowID: "flow-pending", Timestamp: time.Now(),
		Method: "GET", URL: "http://example.test/pending",
	})
	sh.Execute("mint.use 1 --force")
	if !strings.Contains(out.String(), "réponse n'avait pas encore") {
		t.Fatalf("missing pending warning:\n%s", out.String())
	}
	if builder.URL() != "http://example.test/pending" {
		t.Fatalf("import failed: %q", builder.URL())
	}
}

func TestMintUseAsksBeforeReplacing(t *testing.T) {
	sh, out, _, hist, builder := newTestShell(t, "n\n")
	addCapture(t, hist)
	_ = builder.SetURL("http://previous.test/")

	sh.Execute("mint.use 1")
	if !strings.Contains(out.String(), "annulée") {
		t.Fatalf("expected cancellation:\n%s", out.String())
	}
	if builder.URL() != "http://previous.test/" {
		t.Fatalf("builder changed despite cancellation: %q", builder.URL())
	}

	out.Reset()
	sh.Execute("mint.use 1")
	// Le scanner a déjà consommé "n" ; une deuxième exécution sans nouvelle
	// entrée annule également, ce qui est le comportement sûr.
	if builder.URL() != "http://previous.test/" {
		t.Fatalf("builder changed without confirmation: %q", builder.URL())
	}
}

func TestMintClearConfirmation(t *testing.T) {
	sh, out, _, hist, _ := newTestShell(t, "n\n")
	addCapture(t, hist)
	sh.Execute("mint.clear")
	if !strings.Contains(out.String(), "annulé") {
		t.Fatalf("expected cancellation:\n%s", out.String())
	}
	if len(hist.List()) != 1 {
		t.Fatal("history cleared despite cancellation")
	}

	sh2, out2, _, hist2, _ := newTestShell(t, "y\n")
	addCapture(t, hist2)
	sh2.Execute("mint.clear")
	if !strings.Contains(out2.String(), "Historique effacé") {
		t.Fatalf("expected clearing:\n%s", out2.String())
	}
	if len(hist2.List()) != 0 {
		t.Fatal("history not cleared")
	}
}

func TestMintClearYesFlag(t *testing.T) {
	sh, out, _, hist, _ := newTestShell(t, "")
	addCapture(t, hist)
	sh.Execute("mint.clear --yes")
	if !strings.Contains(out.String(), "Historique effacé") {
		t.Fatalf("clear output:\n%s", out.String())
	}
	if len(hist.List()) != 0 {
		t.Fatal("history not cleared")
	}
}

func TestMintProxyStatusAndConfig(t *testing.T) {
	sh, out, _, _, _ := newTestShell(t, "")
	sh.Execute("mint.proxy status")
	text := out.String()
	for _, want := range []string{"État", "arrêté", "127.0.0.1:8080", "Captures", "Communication"} {
		if !strings.Contains(text, want) {
			t.Fatalf("status missing %q:\n%s", want, text)
		}
	}
	out.Reset()
	sh.Execute("mint.proxy.config")
	configText := out.String()
	for _, want := range []string{
		"État Mint", "ARRÊTÉ", "Processus", "Communication", "inactive",
		"Proxy utilisé par CRL", "Paramètres Mint", "Corps max/capture", "2.0 Mio",
		"loopback uniquement", "non affiché", "ne modifie aucune configuration réelle",
	} {
		if !strings.Contains(configText, want) {
			t.Fatalf("config output missing %q:\n%s", want, configText)
		}
	}
	out.Reset()
	sh.Execute("mint.proxy.port 0")
	if !strings.Contains(out.String(), "invalide") {
		t.Fatalf("invalid port accepted:\n%s", out.String())
	}
}

func TestProxyPriorityInShow(t *testing.T) {
	sh, out, _, _, builder := newTestShell(t, "")
	_ = builder.SetURL("http://example.test/")
	_ = builder.SetProxy("http://explicit.test:3128")
	sh.Execute("crl.show")
	if !strings.Contains(out.String(), "explicite") || !strings.Contains(out.String(), "explicit.test:3128") {
		t.Fatalf("explicit proxy not shown:\n%s", out.String())
	}
	out.Reset()
	sh.Execute("crl.proxy none")
	sh.Execute("crl.show")
	if !strings.Contains(out.String(), "désactivé") {
		t.Fatalf("none proxy not shown:\n%s", out.String())
	}
	out.Reset()
	sh.Execute("crl.proxy auto")
	sh.Execute("crl.show")
	if !strings.Contains(out.String(), "non configuré") {
		t.Fatalf("auto proxy not shown:\n%s", out.String())
	}
}

func TestCRLCommandsShowFullHeaders(t *testing.T) {
	sh, out, _, _, builder := newTestShell(t, "")
	sh.Execute("crl.host http://127.0.0.1:5002/a%2Fb?x=1&x=2&empty=&token=abc")
	sh.Execute("crl.method PUT")
	sh.Execute("crl.head Authorization: Bearer verysecret")
	sh.Execute("crl.json {\"a\":1}")
	sh.Execute("crl.show")
	text := out.String()
	if !strings.Contains(text, "Bearer verysecret") || !strings.Contains(text, "token=abc") {
		t.Fatalf("full header or URL values missing:\n%s", text)
	}
	if builder.URL() != "http://127.0.0.1:5002/a%2Fb?x=1&x=2&empty=&token=abc" {
		t.Fatalf("URL changed: %q", builder.URL())
	}
	out.Reset()
	sh.Execute("crl.table")
	if !strings.Contains(out.String(), "MÉTHODE") || !strings.Contains(out.String(), "PUT") {
		t.Fatalf("table output:\n%s", out.String())
	}
}

func TestJSONClearAliasesAndReset(t *testing.T) {
	sh, out, _, _, builder := newTestShell(t, "")
	sh.Execute("crl.method POST")
	sh.Execute("crl.head X-Keep: yes")
	sh.Execute(`crl.json {"a":1}`)
	sh.Execute("crl.json clear")
	if builder.BodySet() || builder.BodySize() != 0 || builder.Method() != "POST" || builder.Headers()[0].Value != "yes" {
		t.Fatalf("clear modified unrelated request state: method=%q headers=%#v bodySet=%v", builder.Method(), builder.Headers(), builder.BodySet())
	}
	sh.Execute("crl.show")
	if !strings.Contains(out.String(), "Corps (vide / non défini)") {
		t.Fatalf("empty body not reported:\n%s", out.String())
	}
	sh.Execute(`crl.json {"b":2}`)
	if !builder.BodySet() || string(builder.Body()) != `{"b":2}` || builder.ContentType() != "application/json" {
		t.Fatalf("new JSON not configured: %#v", builder)
	}
	sh.Execute("crl.body.clear")
	if builder.BodySet() {
		t.Fatal("crl.body.clear did not clear body")
	}
}

func TestMintWatchOffQueuesEventsAndMintRequestsRemainAvailable(t *testing.T) {
	hist := mint.NewHistory(mint.DefaultHistoryConfig())
	mgr, err := mint.NewManager(mint.DefaultConfig(), hist)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	notifications := make(chan mint.Notification, 2)
	var out bytes.Buffer
	sh, err := New(Options{In: strings.NewReader(""), Out: &out, Manager: mgr, Notifications: notifications})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sh.Close)
	sh.Execute("mint.watch off")
	notifications <- mint.Notification{Kind: "request", ID: 7, Method: "GET", URL: "http://example.test/path"}
	sh.drainNotifications(false)
	if len(sh.pendingEvents) != 1 {
		t.Fatalf("queued events = %d", len(sh.pendingEvents))
	}
	sh.Execute("mint.events")
	if len(sh.pendingEvents) != 0 || !strings.Contains(out.String(), "#7") {
		t.Fatalf("events not displayed/cleared:\n%s", out.String())
	}
	addCapture(t, hist)
	out.Reset()
	sh.Execute("mint.requests")
	if !strings.Contains(out.String(), "/users/v1/login") {
		t.Fatalf("watch off hid stored captures:\n%s", out.String())
	}
}

func TestReportMintCaptureShowsObservedHeaderDifferences(t *testing.T) {
	sh, out, _, hist, builder := newTestShell(t, "")
	started := time.Now()
	_ = builder.SetURL("http://example.test/path")
	_ = builder.SetMethod("GET")
	_ = builder.SetHeader("Authorization", "Bearer prepared")
	hist.AddRequest(mint.BridgeEvent{
		Type: "request", FlowID: "diff-flow", Timestamp: started,
		Method: "GET", URL: "http://example.test/path",
		Headers: []mint.Header{{Name: "Authorization", Value: "Bearer captured"}, {Name: "X-Extra", Value: "seen"}},
	})
	sh.reportMintCapture(started)
	for _, want := range []string{"Capture réellement observée", "Authorization", "X-Extra", "valeur préparée absente ou différente"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q:\n%s", want, out.String())
		}
	}
}

func TestReset(t *testing.T) {
	sh, out, _, _, builder := newTestShell(t, "")
	_ = builder.SetURL("http://example.test/")
	sh.Execute("reset")
	if builder.URL() != "" {
		t.Fatal("reset did not clear URL")
	}
	if !strings.Contains(out.String(), "réinitialisé") {
		t.Fatalf("reset output:\n%s", out.String())
	}
}

func TestLaunchUsesCurl(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl absent")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer replay-token" || r.Header.Get("Cookie") != "session=replay-cookie" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("X-Server", "test")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("hello-from-server"))
	}))
	defer srv.Close()

	sh, out, _, hist, _ := newTestShell(t, "")
	hist.AddRequest(mint.BridgeEvent{
		Type: "request", FlowID: "replay-flow", Timestamp: time.Now(),
		Method: "POST", URL: srv.URL + "/x?y=1",
		Headers: []mint.Header{
			{Name: "Authorization", Value: "Bearer replay-token"},
			{Name: "Cookie", Value: "session=replay-cookie"},
			{Name: "Content-Type", Value: "application/json"},
		},
		Body: []byte(`{"a":1}`), BodySize: 7,
	})
	sh.Execute("mint.use 1 --force")
	sh.Execute("crl.launch")
	text := out.String()
	for _, want := range []string{"201", "X-Server: test", "hello-from-server"} {
		if !strings.Contains(text, want) {
			t.Fatalf("launch output missing %q:\n%s", want, text)
		}
	}
}

func TestLaunchTempResponseIsPrivateAndCleaned(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl absent")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("temp-check"))
	}))
	defer srv.Close()

	sh, _, _, _, builder := newTestShell(t, "")
	_ = builder.SetURL(srv.URL)
	sh.Execute("crl.launch")
	dir := sh.lastResponseDir
	if dir == "" {
		t.Fatal("répertoire temporaire non enregistré")
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("droits du répertoire = %v, err = %v", info, err)
	}
	for _, name := range []string{"headers.txt", "body.bin"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s absent : %v", name, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s droits = %o", name, info.Mode().Perm())
		}
	}
	sh.Close()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("répertoire temporaire non supprimé : %v", err)
	}
}

func TestLaunchHTTPResponseLayoutAndPrettyJSON(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl absent")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "session=secret")
		_, _ = w.Write([]byte(`{"b":2,"a":[1,2]}`))
	}))
	defer srv.Close()

	sh, out, _, _, builder := newTestShell(t, "")
	_ = builder.SetURL(srv.URL + "/json?token=secret")
	sh.Execute("crl.launch")
	text := out.String()
	for _, want := range []string{
		"Requête envoyée", "Réponse HTTP", "Statut", "200 OK", "application/json",
		"En-têtes de réponse", "Corps de la réponse — JSON", "\"b\": 2", "Corps complet",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("launch output missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, `{"b":2`) {
		t.Fatalf("JSON not pretty-printed:\n%s", text)
	}
	if !strings.Contains(text, "Set-Cookie: session=secret") {
		t.Fatalf("full response header missing:\n%s", text)
	}
	if strings.Contains(text, "\033[") {
		t.Fatalf("launch emitted ANSI on a non-terminal buffer:\n%s", text)
	}
}

func TestLaunchOutputModes(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl absent")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"mode":"test"}`))
	}))
	defer srv.Close()

	sh, out, _, _, builder := newTestShell(t, "")
	_ = builder.SetURL(srv.URL)

	sh.Execute("crl.output raw")
	sh.Execute("crl.launch")
	if !strings.Contains(out.String(), `{"mode":"test"}`) {
		t.Fatalf("raw output not preserved:\n%s", out.String())
	}

	out.Reset()
	sh.Execute("crl.output pretty")
	sh.Execute("crl.launch")
	if !strings.Contains(out.String(), "\"mode\": \"test\"") {
		t.Fatalf("pretty output missing indentation:\n%s", out.String())
	}

	out.Reset()
	sh.Execute("crl.output invalid")
	if !strings.Contains(out.String(), "invalide") {
		t.Fatalf("invalid output mode accepted:\n%s", out.String())
	}
}

func TestLaunchHTTPErrorHTML(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl absent")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`<!doctype html><html><body><h1>400 Bad Request</h1><p>Invalid input</p></body></html>`))
	}))
	defer srv.Close()

	sh, out, _, _, builder := newTestShell(t, "")
	_ = builder.SetURL(srv.URL)
	sh.Execute("crl.launch")
	text := out.String()
	for _, want := range []string{"400 Bad Request", "Corps de la réponse — HTML", "<h1>", "Invalid input", "Erreur HTTP côté client"} {
		if !strings.Contains(text, want) {
			t.Fatalf("HTML error response missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Corps de la réponse — JSON") {
		t.Fatalf("HTML response presented as JSON:\n%s", text)
	}
}

func TestLaunchHTTPStatusDiagnostics(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl absent")
	}
	for _, code := range []int{200, 400, 401, 403, 404} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Diagnostic", "visible")
				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(code)
				_, _ = w.Write([]byte("body-" + strconv.Itoa(code)))
			}))
			defer srv.Close()
			sh, out, _, _, builder := newTestShell(t, "")
			_ = builder.SetURL(srv.URL + "/path?q=1")
			_ = builder.SetMethod("GET")
			sh.Execute("crl.launch")
			text := out.String()
			for _, want := range []string{strconv.Itoa(code), "GET", "/path?q=1", "X-Diagnostic: visible", "body-" + strconv.Itoa(code), "Durée"} {
				if !strings.Contains(text, want) {
					t.Fatalf("missing %q:\n%s", want, text)
				}
			}
			if code == 403 && !strings.Contains(text, "ne prouve pas que l'authentification est absente") {
				t.Fatalf("403 diagnostic overclaims:\n%s", text)
			}
			if code == 404 && !strings.Contains(text, "vérifiez le chemin, les paramètres") {
				t.Fatalf("404 diagnostic missing:\n%s", text)
			}
		})
	}
}

func TestLaunchConnectionFailureIsNotAnHTTPStatus(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl absent")
	}
	port := freeLoopbackPort(t)
	sh, out, _, _, builder := newTestShell(t, "")
	_ = builder.SetURL("http://127.0.0.1:" + strconv.Itoa(port) + "/")
	sh.Execute("crl.proxy none")
	sh.Execute("crl.launch")
	text := out.String()
	for _, want := range []string{"Erreur de connexion", "connexion impossible", "aucune réponse HTTP reçue"} {
		if !strings.Contains(text, want) {
			t.Fatalf("connection failure missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Réponse HTTP") {
		t.Fatalf("connection failure invented an HTTP response:\n%s", text)
	}
}

func TestUnknownCommand(t *testing.T) {
	sh, out, _, _, _ := newTestShell(t, "")
	sh.Execute("mint.unknown")
	if !strings.Contains(out.String(), "inconnue") {
		t.Fatalf("unknown command output:\n%s", out.String())
	}
	out.Reset()
	sh.Execute("crl.unknown")
	if !strings.Contains(out.String(), "inconnue") {
		t.Fatalf("unknown crl output:\n%s", out.String())
	}
}

func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func TestShellRemainsUsableAfterMintStartFailure(t *testing.T) {
	hist := mint.NewHistory(mint.DefaultHistoryConfig())
	cfg := mint.DefaultConfig()
	cfg.MitmdumpPath = "crl-cli-mitmdump-absent-for-shell-test"
	mgr, err := mint.NewManager(cfg, hist)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	sh, err := New(Options{
		In:      strings.NewReader(""),
		Out:     &out,
		Builder: request.New(),
		Manager: mgr,
	})
	if err != nil {
		t.Fatal(err)
	}

	sh.Execute("mint.proxy on")
	text := out.String()
	if !strings.Contains(text, "Échec") || !strings.Contains(text, "reste utilisable") {
		t.Fatalf("échec Mint mal signalé :\n%s", text)
	}
	if st := mgr.Status(); st.State != mint.StateFailed {
		t.Fatalf("état après échec = %s", st.State)
	}

	// Le shell doit rester pleinement utilisable après l'échec du proxy.
	out.Reset()
	sh.Execute("crl.host http://example.test/after-failure")
	if !strings.Contains(out.String(), "after-failure") {
		t.Fatalf("crl.host inutilisable après échec :\n%s", out.String())
	}
	out.Reset()
	sh.Execute("crl.show")
	if !strings.Contains(out.String(), "GET") || !strings.Contains(out.String(), "example.test") {
		t.Fatalf("crl.show inutilisable après échec :\n%s", out.String())
	}
	out.Reset()
	sh.Execute("mint.proxy status")
	statusText := out.String()
	if !strings.Contains(statusText, "en erreur") || !strings.Contains(statusText, "introuvable") {
		t.Fatalf("statut d'échec incomplet :\n%s", statusText)
	}
	out.Reset()
	sh.Execute("mint.requests")
	if !strings.Contains(out.String(), "Aucune capture") {
		t.Fatalf("mint.requests inutilisable après échec :\n%s", out.String())
	}
	if exit := sh.Execute("help"); exit {
		t.Fatal("help a quitté le shell")
	}
}

// TestRealMintAutoProxyInShell vérifie le chemin complet du shell : démarrage
// réel de mitmdump, curl lancé sans crl.proxy explicite, capture du trafic
// loopback par le proxy Mint, puis arrêt sans perte de l'historique.
func TestRealMintAutoProxyInShell(t *testing.T) {
	if testing.Short() {
		t.Skip("test d'intégration désactivé en mode -short")
	}
	if _, err := exec.LookPath("mitmdump"); err != nil {
		t.Skip("mitmdump absent : test d'intégration réel ignoré")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl absent")
	}

	cfg := mint.DefaultConfig()
	cfg.Port = freeLoopbackPort(t)
	cfg.StartTimeout = 25 * time.Second
	hist := mint.NewHistory(mint.DefaultHistoryConfig())
	mgr, err := mint.NewManager(cfg, hist)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()

	var out bytes.Buffer
	sh, err := New(Options{
		In:      strings.NewReader(""),
		Out:     &out,
		Builder: request.New(),
		Manager: mgr,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sh.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("hello-from-mint"))
	}))
	defer server.Close()

	sh.Execute("mint.proxy on")
	if !strings.Contains(out.String(), "Proxy actif") {
		t.Fatalf("démarrage Mint échoué :\n%s", out.String())
	}
	if st := mgr.Status(); st.State != mint.StateRunning {
		t.Fatalf("état = %s", st.State)
	}

	out.Reset()
	sh.Execute("mint.proxy.config")
	configText := out.String()
	for _, want := range []string{"ACTIF", "connectée", "Mint (automatique)", "Addon", "Captures"} {
		if !strings.Contains(configText, want) {
			t.Fatalf("configuration Mint active incomplète (%q) :\n%s", want, configText)
		}
	}

	out.Reset()
	sh.Execute("crl.host " + server.URL + "/auto-proxy?token=secret")
	sh.Execute("crl.launch")
	launchText := out.String()
	if !strings.Contains(launchText, "hello-from-mint") {
		t.Fatalf("curl via proxy automatique a échoué :\n%s", launchText)
	}
	if !strings.Contains(launchText, cfg.ListenHost+":"+strconv.Itoa(cfg.Port)) {
		t.Fatalf("proxy Mint automatique non annoncé :\n%s", launchText)
	}
	if strings.Contains(launchText, "token=secret") {
		t.Fatalf("secret affiché par crl.launch :\n%s", launchText)
	}

	var capture mint.Capture
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, c := range hist.List() {
			if strings.Contains(c.Request.URL, "/auto-proxy") {
				capture = c
			}
		}
		if capture.ID != 0 && capture.Response != nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if capture.ID == 0 {
		t.Fatalf("trafic loopback non capturé : %#v", hist.List())
	}
	if capture.Request.Method != "GET" {
		t.Fatalf("méthode capturée = %q", capture.Request.Method)
	}
	if capture.Response == nil || capture.Response.StatusCode != 200 {
		t.Fatalf("réponse capturée = %#v", capture.Response)
	}

	out.Reset()
	sh.Execute("mint.proxy off")
	if !strings.Contains(out.String(), "Proxy arrêté") {
		t.Fatalf("arrêt Mint :\n%s", out.String())
	}
	if st := mgr.Status(); st.State != mint.StateStopped {
		t.Fatalf("état après arrêt = %s", st.State)
	}
	if len(hist.List()) == 0 {
		t.Fatal("captures perdues après arrêt du proxy")
	}
}
