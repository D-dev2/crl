package shell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
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

func newTestHTTPServer(t *testing.T, handler http.Handler) *httptest.Server {
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

func TestMintShowMasksSecrets(t *testing.T) {
	sh, out, _, hist, _ := newTestShell(t, "")
	addCapture(t, hist)
	sh.Execute("mint.request.show 1")
	text := out.String()
	for _, want := range []string{"Capture #1", "Bearer ***", "Cookie: ***", "Set-Cookie: ***", "token=***", "200 OK", "24ms", "/users/v1/login"} {
		if !strings.Contains(text, want) {
			t.Fatalf("show missing %q:\n%s", want, text)
		}
	}
}

func TestMintShowMasksProxyAndCustomHeaderValues(t *testing.T) {
	sh, out, _, hist, _ := newTestShell(t, "")
	hist.AddRequest(mint.BridgeEvent{
		Type: "request", FlowID: "custom-headers", Timestamp: time.Now(),
		Method: "GET", URL: "http://example.test/custom",
		Headers: []mint.Header{
			{Name: "Proxy-Authorization", Value: "Basic proxy-value"},
			{Name: "X-Custom-Token", Value: "custom-value"},
		},
	})
	sh.Execute("mint.request.show 1")
	text := out.String()
	for _, want := range []string{"Proxy-Authorization: Basic ***", "X-Custom-Token: ***"} {
		if !strings.Contains(text, want) {
			t.Fatalf("valeur complète %q absente :\n%s", want, text)
		}
	}
}

func TestMintSelectAndUse(t *testing.T) {
	sh, out, _, hist, builder := newTestShell(t, "")
	addCapture(t, hist)

	sh.Execute("mint.select 1")
	if !strings.Contains(out.String(), "Sélection persistante") {
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
	if !strings.Contains(out.String(), "Bearer ***") || !strings.Contains(out.String(), "Cookie: ***") {
		t.Fatalf("crl.show did not mask secret headers:\n%s", out.String())
	}
}

func TestSelectedRequestsShowAndEditIndependently(t *testing.T) {
	sh, out, _, hist, _ := newTestShell(t, "")
	for i := 1; i <= 3; i++ {
		hist.AddRequest(mint.BridgeEvent{Type: "request", FlowID: fmt.Sprintf("multi-%d", i), Method: "POST", URL: fmt.Sprintf("http://example.test/%d?token=private", i), Timestamp: time.Now(), Headers: []mint.Header{{Name: "Authorization", Value: "Bearer private"}, {Name: "X-Index", Value: strconv.Itoa(i)}, {Name: "Content-Type", Value: "application/json"}}, Body: []byte(fmt.Sprintf(`{"role":"user","n":%d}`, i)), BodySize: int64(len(fmt.Sprintf(`{"role":"user","n":%d}`, i)))})
	}
	sh.Execute("mint.select 1 2 3")
	out.Reset()
	sh.Execute("crl.show")
	text := out.String()
	for _, want := range []string{"REQUEST #1", "REQUEST #2", "REQUEST #3", "Authorization: Bearer ***", "\"role\": \"user\"", "token=***"} {
		if !strings.Contains(text, want) {
			t.Fatalf("crl.show missing %q:\n%s", want, text)
		}
	}
	sh.Execute("crl.method PATCH set 1")
	sh.Execute(`crl.head set 2 X-Note "two words"`)
	sh.Execute(`crl.json 2 set role = "admin"`)
	if sh.requestEdits[1].Method() != "PATCH" {
		t.Fatalf("method #1 = %q", sh.requestEdits[1].Method())
	}
	if sh.requestEdits[2].Method() != "POST" {
		t.Fatalf("method #2 changed unexpectedly: %q", sh.requestEdits[2].Method())
	}
	if sh.requestEdits[3].Method() != "POST" || !strings.Contains(string(sh.requestEdits[3].Body()), `"n":3`) {
		t.Fatalf("requête #3 modifiée involontairement: method=%s body=%s", sh.requestEdits[3].Method(), sh.requestEdits[3].Body())
	}
	for _, h := range sh.requestEdits[2].Headers() {
		if h.Name == "X-Note" && h.Value != "two words" {
			t.Fatalf("quoted header value = %q", h.Value)
		}
	}
	sh.Execute("crl.head show 2")
	if !strings.Contains(out.String(), "X-Note: two words") {
		t.Fatalf("target header inspection failed:\n%s", out.String())
	}
	sh.Execute("crl.head delete 2 X-Note")
	for _, h := range sh.requestEdits[2].Headers() {
		if h.Name == "X-Note" {
			t.Fatal("target header delete failed")
		}
	}
	if !strings.Contains(string(sh.requestEdits[2].Body()), `"role": "admin"`) {
		t.Fatalf("targeted JSON edit missing: %s", sh.requestEdits[2].Body())
	}
	beforeInvalid := string(sh.requestEdits[2].Body())
	sh.Execute(`crl.json 2 set role = not-json`)
	if string(sh.requestEdits[2].Body()) != beforeInvalid {
		t.Fatal("invalid JSON edit changed the working request")
	}
	if err := sh.requestEdits[3].SetJSON(`[ {"role":"user"} ]`); err != nil {
		t.Fatal(err)
	}
	sh.Execute(`crl.json 3 set 0.role = "admin"`)
	if !strings.Contains(string(sh.requestEdits[3].Body()), `"role": "admin"`) {
		t.Fatalf("array path update failed: %s", sh.requestEdits[3].Body())
	}
	sh.Execute("crl.head set selected Accept application/json")
	for id := 1; id <= 3; id++ {
		if sh.requestEdits[id] == nil {
			t.Fatalf("collective header missing request #%d", id)
		}
		found := false
		for _, h := range sh.requestEdits[id].Headers() {
			if h.Name == "Accept" {
				found = true
			}
		}
		if !found {
			t.Fatalf("Accept missing from #%d", id)
		}
	}
	sh.Execute("mint.select remove 2")
	if got := len(hist.SelectedMany()); got != 2 {
		t.Fatalf("selection count after remove = %d", got)
	}
	sh.Execute("mint.select clear")
	if got := len(hist.SelectedMany()); got != 0 {
		t.Fatalf("selection count after clear = %d", got)
	}
}

func TestJSONFieldEditPreservesLargeInteger(t *testing.T) {
	sh, out, _, _, builder := newTestShell(t, "")
	if err := builder.SetJSON(`{"id":9007199254740993,"role":"user"}`); err != nil {
		t.Fatal(err)
	}
	sh.Execute(`crl.json set role = "admin"`)
	if !strings.Contains(string(builder.Body()), "9007199254740993") || !strings.Contains(string(builder.Body()), `"role": "admin"`) {
		t.Fatalf("JSON update corrupted sibling data: %s\n%s", builder.Body(), out.String())
	}
}

func TestMintUseKeepsSelectionSeparateFromActiveRequest(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl absent")
	}
	gotPath := make(chan string, 1)
	srv := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { gotPath <- r.URL.Path; w.WriteHeader(http.StatusOK) }))
	defer srv.Close()
	sh, _, _, hist, builder := newTestShell(t, "")
	for i, path := range []string{"/loaded", "/selected"} {
		hist.AddRequest(mint.BridgeEvent{Type: "request", FlowID: fmt.Sprintf("separate-%d", i), Method: "GET", URL: srv.URL + path, Timestamp: time.Now()})
	}
	sh.Execute("mint.select 2")
	sh.Execute("mint.use 1 --force")
	if selected, ok := hist.Selected(); !ok || selected.ID != 2 {
		t.Fatalf("mint.use changed selection: %#v", selected)
	}
	if builder.URL() != srv.URL+"/loaded" {
		t.Fatalf("active request was not loaded: %s", builder.URL())
	}
	sh.Execute("crl.launch active")
	select {
	case path := <-gotPath:
		if path != "/loaded" {
			t.Fatalf("active launch sent selected request path %q", path)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("active launch did not reach server")
	}
}

func TestMintReplayRunsSelectedRequestsInParallel(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl absent")
	}
	var started atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started.Add(1)
		if r.Header.Get("Authorization") != "Bearer kept" {
			t.Errorf("Authorization lost during replay: %#v", r.Header)
		}
		time.Sleep(250 * time.Millisecond)
		if r.URL.Path == "/two" {
			w.WriteHeader(http.StatusUnauthorized)
		} else {
			w.WriteHeader(http.StatusOK)
		}
		_, _ = w.Write([]byte(r.URL.Path))
	})
	ln, listenErr := net.Listen("tcp4", "127.0.0.1:0")
	if listenErr != nil {
		t.Skipf("loopback unavailable in this sandbox: %v", listenErr)
	}
	srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: handler}}
	srv.Start()
	defer srv.Close()
	sh, out, _, hist, _ := newTestShell(t, "")
	for i, path := range []string{"/one", "/two"} {
		hist.AddRequest(mint.BridgeEvent{Type: "request", FlowID: strconv.Itoa(i), Method: "GET", URL: srv.URL + path, Timestamp: time.Now(), Headers: []mint.Header{{Name: "Authorization", Value: "Bearer kept"}}})
	}
	sh.Execute("mint.select 1 2")
	start := time.Now()
	sh.Execute("mint.replay")
	elapsed := time.Since(start)
	if started.Load() != 2 {
		t.Fatalf("started %d requests", started.Load())
	}
	if elapsed >= 450*time.Millisecond {
		t.Fatalf("batch took %s, expected parallel responses", elapsed)
	}
	for _, want := range []string{"#1", "#2", "200 OK", "401 Unauthorized", "/one", "/two", "Succès HTTP : 1", "Erreurs HTTP : 1"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in replay result:\n%s", want, out.String())
		}
	}
	sh.Execute("mint.replay.body 1")
	if !strings.Contains(out.String(), "Corps de réponse #1") {
		t.Fatalf("replayed body not retained for detailed inspection:\n%s", out.String())
	}
	if original, ok := hist.Get(1); !ok || original.Request.Method != "GET" {
		t.Fatalf("replay modified original capture: %#v", original)
	}
}

func TestMintReplayReportsTimeoutAndKeepsOtherErrors(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl absent")
	}
	started := make(chan struct{}, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		time.Sleep(250 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback unavailable in this sandbox: %v", err)
	}
	srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: handler}}
	srv.Start()
	defer srv.Close()
	dead, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback unavailable in this sandbox: %v", err)
	}
	deadURL := "http://" + dead.Addr().String() + "/refused"
	_ = dead.Close()
	sh, out, _, hist, _ := newTestShell(t, "")
	for i, rawURL := range []string{srv.URL + "/timeout", deadURL} {
		hist.AddRequest(mint.BridgeEvent{Type: "request", FlowID: strconv.Itoa(i), Method: "GET", URL: rawURL, Timestamp: time.Now()})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	sh.ctx = ctx
	sh.Execute("mint.select 1,2")
	sh.Execute("mint.replay")
	select {
	case <-started:
	default:
		t.Fatal("delayed request never started")
	}
	text := out.String()
	if !strings.Contains(text, "#1") || !strings.Contains(text, "#2") || !strings.Contains(text, "deadline exceeded") || !strings.Contains(text, "exit status 7") {
		t.Fatalf("batch errors not attributed individually:\n%s", text)
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

func TestCRLCommandsMaskSecrets(t *testing.T) {
	sh, out, _, _, builder := newTestShell(t, "")
	sh.Execute("crl.host http://127.0.0.1:5002/a%2Fb?x=1&x=2&empty=&token=abc")
	sh.Execute("crl.method PUT")
	sh.Execute("crl.head Authorization: Bearer verysecret")
	sh.Execute("crl.head Proxy-Authorization: Basic proxysecret")
	sh.Execute("crl.head X-Custom-Token: customsecret")
	sh.Execute("crl.json {\"a\":1}")
	sh.Execute("crl.show")
	text := out.String()
	for _, want := range []string{"Bearer ***", "Basic ***", "***", "token=***"} {
		if !strings.Contains(text, want) {
			t.Fatalf("full header or URL value %q missing:\n%s", want, text)
		}
	}
	if builder.URL() != "http://127.0.0.1:5002/a%2Fb?x=1&x=2&empty=&token=abc" {
		t.Fatalf("URL changed: %q", builder.URL())
	}
	out.Reset()
	sh.Execute("crl.table")
	table := out.String()
	if !strings.Contains(table, "MÉTHODE") || !strings.Contains(table, "PUT") {
		t.Fatalf("table output:\n%s", table)
	}
	if !strings.Contains(table, "Authorization") || !strings.Contains(table, "crl.show affiche le détail complet") {
		t.Fatalf("table ne résume pas les en-têtes :\n%s", table)
	}
	for _, secret := range []string{"verysecret", "proxysecret", "customsecret"} {
		if strings.Contains(table, secret) {
			t.Fatalf("le tableau récapitulatif expose %q :\n%s", secret, table)
		}
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
	if builder.ContentType() != "application/json" {
		t.Fatalf("Content-Type JSON supprimé par clear : %q", builder.ContentType())
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
	if builder.ContentType() != "application/json" {
		t.Fatalf("crl.body.clear a supprimé Content-Type : %q", builder.ContentType())
	}
}

func TestMintWatchOffNotificationDoesNotTouchActivePrompt(t *testing.T) {
	sh, _, _, _, _ := newTestShell(t, "")
	var out bytes.Buffer
	sh.editor = newEditor(&out, "crl > ")
	sh.editor.DrawPrompt()
	sh.editor.Feed([]byte("hel"))
	out.Reset()
	sh.mintWatch = false

	sh.handleNotification(mint.Notification{Kind: "request", ID: 9, Method: "GET", URL: "http://example.test/"})
	if out.Len() != 0 {
		t.Fatalf("mint.watch off a écrit dans le prompt :\n%q", out.String())
	}
	if len(sh.pendingEvents) != 1 || sh.editor.Text() != "hel" {
		t.Fatalf("événement non mis en attente ou saisie perdue : events=%d texte=%q", len(sh.pendingEvents), sh.editor.Text())
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
	sh.drainNotifications()
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

func TestPendingMintEventsAreBounded(t *testing.T) {
	sh, out, _, _, _ := newTestShell(t, "")
	sh.mintWatch = false
	for i := 0; i < maxPendingEvents+3; i++ {
		sh.queuePendingEvent(mint.Notification{Kind: "request", ID: i + 1, Method: "GET", URL: "http://example.test/" + strconv.Itoa(i)})
	}
	if len(sh.pendingEvents) != maxPendingEvents {
		t.Fatalf("file = %d, attendu %d", len(sh.pendingEvents), maxPendingEvents)
	}
	if sh.pendingDropped != 3 {
		t.Fatalf("événements abandonnés = %d, attendu 3", sh.pendingDropped)
	}
	// On ne réaffiche pas les 1024 événements : seul le compte d'abandons est
	// vérifié après vidage interne de la file.
	sh.pendingEvents = nil
	out.Reset()
	sh.cmdMintEvents()
	if !strings.Contains(out.String(), "3 événement(s) ancien(s) ont été abandonnés") {
		t.Fatalf("abandons non signalés :\n%s", out.String())
	}
	if sh.pendingDropped != 0 {
		t.Fatalf("compteur d'abandons non remis à zéro : %d", sh.pendingDropped)
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
	for _, want := range []string{"Capture récente de même méthode et URL", "trafic concurrent", "Authorization", "X-Extra", "valeur préparée absente ou différente"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q:\n%s", want, out.String())
		}
	}
}

func TestReportMintCaptureWaitsForAsyncBridge(t *testing.T) {
	sh, out, _, hist, builder := newTestShell(t, "")
	_ = builder.SetURL("http://example.test/late")
	started := time.Now()
	go func() {
		time.Sleep(40 * time.Millisecond)
		hist.AddRequest(mint.BridgeEvent{
			Type: "request", FlowID: "late-flow", Timestamp: started,
			Method: "GET", URL: "http://example.test/late",
			Headers: []mint.Header{{Name: "X-Late", Value: "yes"}},
		})
	}()
	sh.reportMintCapture(started)
	if !strings.Contains(out.String(), "Capture récente de même méthode et URL") || !strings.Contains(out.String(), "X-Late") {
		t.Fatalf("capture asynchrone non corrélée :\n%s", out.String())
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
	srv := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

func TestLaunchUsesExactUTF8BodyLength(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl absent")
	}
	body := "café 🐈\n" + strings.Repeat("界", 8192)
	received := make(chan struct {
		body   string
		length int64
	}, 1)
	srv := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		received <- struct {
			body   string
			length int64
		}{string(data), r.ContentLength}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	sh, _, _, _, builder := newTestShell(t, "")
	if err := builder.SetURL(srv.URL); err != nil {
		t.Fatal(err)
	}
	_ = builder.AddHeader("Content-Length", "1")
	builder.SetBodyText(body)
	sh.Execute("crl.launch")
	got := <-received
	if got.body != body {
		t.Fatalf("request body changed: got %d bytes, want %d", len(got.body), len(body))
	}
	if got.length != int64(len([]byte(body))) {
		t.Fatalf("Content-Length = %d, want %d bytes", got.length, len([]byte(body)))
	}
}

func TestMintAuthHeadersSurviveSelectionImportAndReplay(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl absent")
	}
	srv := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/secure" || r.URL.Query().Get("token") != "abc" {
			t.Errorf("requête inattendue : %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.Header.Get("Authorization") != "Bearer auth-secret" ||
			r.Header.Get("Cookie") != "session=abc" ||
			r.Header.Get("X-Api-Key") != "api-secret" {
			t.Errorf("en-têtes d'authentification altérés : %#v", r.Header)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("X-Replay", "ok")
		_, _ = w.Write([]byte("auth-ok"))
	}))
	defer srv.Close()

	sh, out, _, hist, builder := newTestShell(t, "")
	hist.AddRequest(mint.BridgeEvent{
		Type: "request", FlowID: "auth-flow", Timestamp: time.Now(),
		Method: "POST", URL: srv.URL + "/secure?token=abc",
		Headers: []mint.Header{
			{Name: "Authorization", Value: "Bearer auth-secret"},
			{Name: "Cookie", Value: "session=abc"},
			{Name: "X-Api-Key", Value: "api-secret"},
			{Name: "Proxy-Authorization", Value: "Basic proxy-secret"},
			{Name: "Content-Type", Value: "application/json"},
		},
		Body: []byte(`{"ok":true}`), BodySize: 11,
	})
	hist.AddResponse(mint.BridgeEvent{
		Type: "response", FlowID: "auth-flow", Timestamp: time.Now(),
		StatusCode: 200, Reason: "OK", Duration: 5 * time.Millisecond,
	})

	sh.Execute("mint.select 1")
	if !strings.Contains(out.String(), "Sélection persistante") && !strings.Contains(out.String(), "sélectionnée") {
		t.Fatalf("sélection absente :\n%s", out.String())
	}
	out.Reset()
	sh.Execute("mint.request.show 1")
	detail := out.String()
	for _, want := range []string{"Bearer auth-secret", "session=abc", "api-secret", "proxy-secret"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("valeur %q absente de la capture :\n%s", want, detail)
		}
	}

	out.Reset()
	sh.Execute("mint.use --force")
	if !strings.Contains(out.String(), "importée") {
		t.Fatalf("importation absente :\n%s", out.String())
	}
	headerValue := func(name string) string {
		for _, h := range builder.Headers() {
			if strings.EqualFold(h.Name, name) {
				return h.Value
			}
		}
		return ""
	}
	if headerValue("Authorization") != "Bearer auth-secret" ||
		headerValue("Cookie") != "session=abc" ||
		headerValue("X-Api-Key") != "api-secret" {
		t.Fatalf("en-têtes perdus à l'import : %#v", builder.Headers())
	}

	out.Reset()
	sh.Execute("crl.show")
	if !strings.Contains(out.String(), "Bearer auth-secret") {
		t.Fatalf("crl.show ne restitue pas l'authentification :\n%s", out.String())
	}

	out.Reset()
	sh.Execute("crl.launch")
	text := out.String()
	for _, want := range []string{"200 OK", "X-Replay: ok", "auth-ok"} {
		if !strings.Contains(text, want) {
			t.Fatalf("rejeu sans %q :\n%s", want, text)
		}
	}
}

func TestLaunchTempResponseIsPrivateAndCleaned(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl absent")
	}
	srv := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	srv := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	srv := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	srv := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			srv := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			if (code == 401 || code == 403) && (!strings.Contains(text, "crl.show") || !strings.Contains(text, "mint.request.show")) {
				t.Fatalf("%d diagnostic ne guide pas vers les en-têtes préparés et capturés:\n%s", code, text)
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

func TestHelpListsRecentCRLAndMintCommands(t *testing.T) {
	sh, out, _, _, _ := newTestShell(t, "")
	sh.Execute("help")
	for _, command := range []string{
		"crl.ca", "crl.table", "crl.output", "mint.watch", "mint.events",
		"mint.history", "mint.filter", "mint.use", "mint.help",
	} {
		if !strings.Contains(out.String(), command) {
			t.Errorf("help does not list %s:\n%s", command, out.String())
		}
	}
	out.Reset()
	sh.Execute("mint.help")
	for _, command := range []string{"mint.watch", "mint.events", "mint.filter", "mint.use"} {
		if !strings.Contains(out.String(), command) {
			t.Errorf("mint.help does not list %s:\n%s", command, out.String())
		}
	}
}

func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
			t.Skipf("environnement sans accès aux sockets loopback : %v", err)
		}
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

	server := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	if !strings.Contains(launchText, "token=secret") {
		t.Fatalf("URL complète absente de crl.launch :\n%s", launchText)
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
