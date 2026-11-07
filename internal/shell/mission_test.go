package shell

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crl-cli/internal/crlhistory"
	"crl-cli/internal/mint"
)

const vampiAddr = "127.0.0.1:5002"

func vampiReachable() bool {
	conn, err := net.DialTimeout("tcp", vampiAddr, 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// moduleRoot remonte depuis le répertoire du paquet de test jusqu'à la
// racine du module (celui qui contient go.mod).
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod introuvable en remontant depuis le paquet de test")
		}
		dir = parent
	}
}

func withCRLHist(t *testing.T, sh *Shell) {
	t.Helper()
	store, err := crlhistory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sh.crlHist = store
	t.Cleanup(func() { _ = store.Close() })
}

func buildCRLBin(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "crl")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/crl")
	cmd.Dir = moduleRoot(t)
	if err := cmd.Run(); err != nil {
		t.Fatalf("build binaire crl : %v", err)
	}
	return bin
}

// Test 1: crl -c depuis le terminal Linux.
func TestMissionTerminalDashC(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl absent")
	}
	if !vampiReachable() {
		t.Skipf("VAmPI joindre sur %s", vampiAddr)
	}
	bin := buildCRLBin(t)
	cmd := exec.Command(bin, "-c", `crl.host http://127.0.0.1:5002/ & crl.method GET & crl.launch`)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("crl -c a échoué : %v\nstderr:%s", err, errOut.String())
	}
	text := out.String()
	for _, want := range []string{"200 OK", "VAmPI the Vulnerable API"} {
		if !strings.Contains(text, want) {
			t.Fatalf("crl -c terminal manquant %q:\n%s", want, text)
		}
	}
}

// Test 1bis: crl --script depuis le terminal.
func TestMissionTerminalScriptFile(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl absent")
	}
	if !vampiReachable() {
		t.Skipf("VAmPI joindre sur %s", vampiAddr)
	}
	bin := buildCRLBin(t)
	script := filepath.Join(t.TempDir(), "audit.crl")
	if err := os.WriteFile(script, []byte("crl.host http://127.0.0.1:5002/\ncrl.method GET\ncrl.launch\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "--script", script)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("crl --script a échoué : %v", err)
	}
	if !strings.Contains(out.String(), "200 OK") {
		t.Fatalf("crl --script n'a pas exécuté la requête:\n%s", out.String())
	}
}

// Test 4 (terminal): crl --json depuis le terminal, stdout uniquement JSON.
func TestMissionTerminalJSONStdoutOnly(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl absent")
	}
	if !vampiReachable() {
		t.Skipf("VAmPI joindre sur %s", vampiAddr)
	}
	bin := buildCRLBin(t)
	script := filepath.Join(t.TempDir(), "audit.crl")
	if err := os.WriteFile(script, []byte("crl.host http://127.0.0.1:5002/\ncrl.method GET\ncrl.launch\n"), 0644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	cmd := exec.Command(bin, "--json", "--script", script)
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("crl --json --script a échoué : %v\nstderr:%s", err, errOut.String())
	}
	var results []struct {
		OK      bool   `json:"ok"`
		Status  int    `json:"status_code"`
		Command string `json:"command"`
		Network bool   `json:"network_error"`
	}
	if err := json.Unmarshal(out.Bytes(), &results); err != nil {
		t.Fatalf("stdout n'est pas du JSON valide : %v\nstdout:%s", err, out.String())
	}
	if len(results) != 1 || !results[0].OK || results[0].Status != 200 || results[0].Network {
		t.Fatalf("résultat JSON inattendu : %+v", results)
	}
	if strings.Contains(out.String(), "[CRL]") || strings.Contains(out.String(), "[Mint]") {
		t.Fatalf("stdout contient des diagnostics :\n%s", out.String())
	}
	_ = errOut
}

// Test 2: crl.script -c depuis le shell interactif CRL.
func TestMissionShellScriptDashC(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl absent")
	}
	if !vampiReachable() {
		t.Skipf("VAmPI joindre sur %s", vampiAddr)
	}
	sh, out, _, _, _ := newTestShell(t, "")
	cmd := `crl.script -c 'crl.host http://127.0.0.1:5002/ & crl.method GET & crl.launch'`
	if exit := sh.Execute(cmd); exit {
		t.Fatal("crl.script -c a quitté le shell")
	}
	text := out.String()
	if strings.Contains(text, "commande inconnue") {
		t.Fatalf("crl non reconnu depuis le shell:\n%s", text)
	}
	if !strings.Contains(text, "200 OK") {
		t.Fatalf("crl.script -c n'a pas exécuté la requête:\n%s", text)
	}
}

// Test 3: crl.script --script depuis le shell (fichier .crl).
func TestMissionShellScriptFile(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl absent")
	}
	if !vampiReachable() {
		t.Skipf("VAmPI joindre sur %s", vampiAddr)
	}
	sh, out, _, _, _ := newTestShell(t, "")
	script := filepath.Join(t.TempDir(), "audits", "test.crl")
	if err := os.MkdirAll(filepath.Dir(script), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("crl.host http://127.0.0.1:5002/\ncrl.method GET\ncrl.launch\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if exit := sh.Execute("crl.script --script " + script); exit {
		t.Fatal("crl.script --script a quitté le shell")
	}
	text := out.String()
	if !strings.Contains(text, "200 OK") {
		t.Fatalf("crl.script --script n'a pas exécuté le fichier:\n%s", text)
	}
}

// Test 4 (shell): crl.script --json depuis le shell, stdout JSON uniquement.
func TestMissionShellScriptJSON(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl absent")
	}
	if !vampiReachable() {
		t.Skipf("VAmPI joindre sur %s", vampiAddr)
	}
	sh, _, _, _, _ := newTestShell(t, "")
	withCRLHist(t, sh)
	sh.jsonMode = true
	script := filepath.Join(t.TempDir(), "audit.crl")
	if err := os.WriteFile(script, []byte("crl.host http://127.0.0.1:5002/\ncrl.method GET\ncrl.launch\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if exit := sh.Execute("crl.script --json --script " + script); exit {
		t.Fatal("crl.script --json a quitté le shell")
	}
	results := sh.ScriptResults()
	if len(results) != 1 || !results[0].OK || results[0].StatusCode != 200 || results[0].NetworkError {
		t.Fatalf("résultat JSON inattendu : %+v", results)
	}
}

// Test 5: création d'un dossier et d'un script depuis CRL.
func TestMissionFileManagement(t *testing.T) {
	sh, out, _, _, _ := newTestShell(t, "")
	root := t.TempDir()
	if exit := sh.Execute("crl.mkdir " + root + "/scripts"); exit {
		t.Fatal("crl.mkdir a quitté le shell")
	}
	if exit := sh.Execute("crl.touch " + root + "/scripts/audit.crl"); exit {
		t.Fatal("crl.touch a quitté le shell")
	}
	if _, err := os.Stat(root + "/scripts"); os.IsNotExist(err) {
		t.Fatal("dossier scripts non créé")
	}
	if _, err := os.Stat(root + "/scripts/audit.crl"); os.IsNotExist(err) {
		t.Fatal("fichier audit.crl non créé")
	}
	text := out.String()
	if !strings.Contains(text, "Répertoire créé") || !strings.Contains(text, "Fichier créé/actualisé") {
		t.Fatalf("sortie file management inattendue:\n%s", text)
	}

	out.Reset()
	if exit := sh.Execute("crl.ls " + root); exit {
		t.Fatal("crl.ls a quitté le shell")
	}
	if !strings.Contains(out.String(), "scripts") {
		t.Fatalf("crl.ls ne liste pas le dossier:\n%s", out.String())
	}

	out.Reset()
	if exit := sh.Execute("ls " + root); exit {
		t.Fatal("ls a quitté le shell")
	}
	if !strings.Contains(out.String(), "scripts") {
		t.Fatalf("alias ls ne liste pas le dossier:\n%s", out.String())
	}
}

// Test 6: crl.nano ouvre l'éditeur et restaure le terminal.
func TestMissionNano(t *testing.T) {
	dir := t.TempDir()
	fakeNano := filepath.Join(dir, "nano")
	if err := os.WriteFile(fakeNano, []byte("#!/bin/sh\nprintf 'edited-by-nano' > \"$1\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	oldPath := os.Getenv("PATH")
	os.Setenv("PATH", dir+string(os.PathListSeparator)+oldPath)
	defer os.Setenv("PATH", oldPath)

	target := filepath.Join(t.TempDir(), "audit.crl")
	sh, out, _, _, _ := newTestShell(t, "")
	sh.interactive = true
	sh.termFd = os.Stdin.Fd()
	sh.termRestore = func() {}
	sh.editor = &editor{out: sh.out}
	if exit := sh.Execute("crl.nano " + target); exit {
		t.Fatal("crl.nano a quitté le shell")
	}
	text := out.String()
	if !strings.Contains(text, "Édition terminée") {
		t.Fatalf("crl.nano n'a pas signalé la fin:\n%s", text)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("fichier cible non écrit par nano : %v", err)
	}
	if string(data) != "edited-by-nano" {
		t.Fatalf("nano n'a pas modifié le fichier: %q", string(data))
	}
	_ = io.Discard
	_ = text
}

// Test 7: le JSON d'exemple est conservé sans aucune altération.
func TestMissionJSONPreservedExactly(t *testing.T) {
	sh, _, _, _, builder := newTestShell(t, "")
	original := `{
  "add order": {
    "id \"id\"": 1
  },
  "add orders \"id\"": 1
}`
	if exit := sh.Execute("crl.json " + original); exit {
		t.Fatal("crl.json a quitté le shell")
	}
	if string(builder.Body()) != original {
		t.Fatalf("JSON altéré par crl.json:\n got:%q\nwant:%q", string(builder.Body()), original)
	}
	if !builder.BodySet() {
		t.Fatal("BodySet devrait être vrai")
	}
	if builder.ContentType() != "application/json" {
		t.Fatalf("Content-Type manquant: %q", builder.ContentType())
	}
}

// Test 7bis: édition par pointeur JSON sur une clé contenant espace et guillemets.
func TestMissionJSONPointerEdit(t *testing.T) {
	sh, _, _, _, builder := newTestShell(t, "")
	original := `{
  "add order": {
    "id \"id\"": 1
  },
  "add orders \"id\"": 1
}`
	if err := builder.SetJSON(original); err != nil {
		t.Fatal(err)
	}
	if exit := sh.Execute(`crl.json set /add order/id "id" = 2`); exit {
		t.Fatal("crl.json set JSON Pointer a quitté le shell")
	}
	var decoded map[string]any
	if err := json.Unmarshal(builder.Body(), &decoded); err != nil {
		t.Fatalf("corps invalide après édition pointeur: %v", err)
	}
	addOrder, ok := decoded["add order"].(map[string]any)
	if !ok {
		t.Fatalf("clé 'add order' absente: %#v", decoded)
	}
	if addOrder[`id "id"`] != float64(2) {
		t.Fatalf("pointeur JSON n'a pas modifié la valeur: %#v", addOrder)
	}
	if decoded["add orders \"id\""] != float64(1) {
		t.Fatalf("donnée sibling altérée: %#v", decoded)
	}
}

// Test 8: import d'une capture POST avec body, puis d'une capture GET sans body.
func TestMissionImportPostThenGet(t *testing.T) {
	sh, _, _, hist, builder := newTestShell(t, "")
	hist.AddRequest(mint.BridgeEvent{
		Type: "request", FlowID: "post-flow", Method: "POST",
		URL:     "http://127.0.0.1:5002/login",
		Headers: []mint.Header{{Name: "Content-Type", Value: "application/json"}},
		Body:    []byte(`{"u":1}`), BodySize: 4,
	})
	hist.AddRequest(mint.BridgeEvent{
		Type: "request", FlowID: "get-flow", Method: "GET",
		URL:      "http://127.0.0.1:5002/orders/6",
		BodySize: 0,
	})
	if exit := sh.Execute("mint.use 1 --force"); exit {
		t.Fatal("mint.use POST a quitté le shell")
	}
	if builder.Method() != "POST" || string(builder.Body()) != `{"u":1}` || !builder.BodySet() {
		t.Fatalf("POST non importé: method=%q body=%q set=%v", builder.Method(), string(builder.Body()), builder.BodySet())
	}
	if exit := sh.Execute("mint.use 2 --force"); exit {
		t.Fatal("mint.use GET a quitté le shell")
	}
	if builder.Method() != "GET" || builder.BodySet() || len(builder.Body()) != 0 || builder.URL() != "http://127.0.0.1:5002/orders/6" {
		t.Fatalf("GET n'a pas remplacé (body conservé): method=%q body=%q set=%v url=%q", builder.Method(), string(builder.Body()), builder.BodySet(), builder.URL())
	}
}

// Test 9: séparation Request Body / Response Body.
func TestMissionRequestVsResponseBody(t *testing.T) {
	sh, out, _, hist, builder := newTestShell(t, "")
	hist.AddRequest(mint.BridgeEvent{
		Type: "request", FlowID: "sep-flow", Method: "POST",
		URL:     "http://127.0.0.1:5002/login",
		Headers: []mint.Header{{Name: "Content-Type", Value: "application/json"}},
		Body:    []byte(`{"username":"demo"}`), BodySize: 16,
	})
	hist.AddResponse(mint.BridgeEvent{
		Type: "response", FlowID: "sep-flow", StatusCode: 200, Reason: "OK",
		Headers: []mint.Header{{Name: "Content-Type", Value: "application/json"}},
		Body:    []byte(`{"ok":true}`), BodySize: 7,
	})
	if exit := sh.Execute("mint.use 1 --force"); exit {
		t.Fatal("mint.use a quitté le shell")
	}
	if string(builder.Body()) != `{"username":"demo"}` {
		t.Fatalf("constructeur contient le mauvais corps: %q", string(builder.Body()))
	}
	if builder.ContentType() != "application/json" {
		t.Fatalf("Content-Type perdu: %q", builder.ContentType())
	}
	if exit := sh.Execute("crl.show"); exit {
		t.Fatal("crl.show a quitté le shell")
	}
	text := out.String()
	if strings.Contains(text, `"ok":true`) {
		t.Fatalf("corps de réponse présent dans le constructeur:\n%s", text)
	}
}

// Test 10: construction manuelle, import Mint, rejeu des requêtes.
func TestMissionManualBuildImportReplay(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl absent")
	}
	sh, out, _, hist, builder := newTestShell(t, "")
	srv := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		t.Logf("serveur a reçu %s %s body=%q", r.Method, r.URL.Path, string(body))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"received":true}`))
	}))
	defer srv.Close()

	if exit := sh.Execute(fmt.Sprintf("crl.host %s/orders", srv.URL)); exit {
		t.Fatal("crl.host a quitté le shell")
	}
	if exit := sh.Execute("crl.method POST"); exit {
		t.Fatal("crl.method a quitté le shell")
	}
	if exit := sh.Execute(`crl.json {"add order":1}`); exit {
		t.Fatal("crl.json a quitté le shell")
	}
	out.Reset()
	if exit := sh.Execute("crl.show"); exit {
		t.Fatal("crl.show a quitté le shell")
	}
	if string(builder.Body()) != `{"add order":1}` {
		t.Fatalf("corps JSON altéré par crl.show/construction: %q", string(builder.Body()))
	}
	if !strings.Contains(out.String(), `"add order":`) {
		t.Fatalf("construction manuelle absente de crl.show:\n%s", out.String())
	}

	out.Reset()
	if exit := sh.Execute("crl.launch active"); exit {
		t.Fatal("crl.launch active a quitté le shell")
	}
	if !strings.Contains(out.String(), "200 OK") {
		t.Fatalf("rejeu construction manuelle échoué:\n%s", out.String())
	}

	hist.AddRequest(mint.BridgeEvent{
		Type: "request", FlowID: "import-flow", Method: "GET",
		URL: srv.URL + "/users/v1",
		Headers: []mint.Header{
			{Name: "Authorization", Value: "Bearer supersecret"},
			{Name: "Content-Type", Value: "application/json"},
		},
	})
	if exit := sh.Execute("mint.use 1 --force"); exit {
		t.Fatal("mint.use a quitté le shell")
	}
	if builder.Method() != "GET" || builder.URL() != srv.URL+"/users/v1" || builder.BodySet() {
		t.Fatalf("import n'a pas remplacé le constructeur: method=%q url=%q bodySet=%v", builder.Method(), builder.URL(), builder.BodySet())
	}
	foundAuth := false
	for _, h := range builder.Headers() {
		if h.Name == "Authorization" && h.Value == "Bearer supersecret" {
			foundAuth = true
		}
	}
	if !foundAuth {
		t.Fatalf("Authorization perdu à l'import: %#v", builder.Headers())
	}
	out.Reset()
	if exit := sh.Execute("crl.launch active"); exit {
		t.Fatal("crl.launch active après import a quitté le shell")
	}
	if !strings.Contains(out.String(), "200 OK") {
		t.Fatalf("rejeu après import échoué:\n%s", out.String())
	}
}

// Test 10bis: crl.script -c s'arrête sur erreur (pas de requête suivante).
func TestMissionScriptStopsOnError(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl absent")
	}
	sh, _, _, _, _ := newTestShell(t, "")
	withCRLHist(t, sh)
	sh.jsonMode = true
	cmd := `crl.script --json -c 'crl.host http://127.0.0.1:1/ & crl.method GET & crl.launch & crl.host http://127.0.0.1:5002/ & crl.method GET & crl.launch'`
	if exit := sh.Execute(cmd); exit {
		t.Fatal("crl.script -c a quitté le shell")
	}
	results := sh.ScriptResults()
	if len(results) != 1 {
		t.Fatalf("attendu 1 résultat (arrêt sur erreur), obtenu %d : %+v", len(results), results)
	}
	if results[0].OK || !results[0].NetworkError {
		t.Fatalf("premier résultat attendu en erreur réseau: %+v", results[0])
	}
}

// Test 10ter: crl.script --json -c depuis le shell.
func TestMissionShellJSONDashC(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl absent")
	}
	if !vampiReachable() {
		t.Skipf("VAmPI joindre sur %s", vampiAddr)
	}
	sh, _, _, _, _ := newTestShell(t, "")
	withCRLHist(t, sh)
	sh.jsonMode = true
	cmd := `crl.script --json -c 'crl.host http://127.0.0.1:5002/ & crl.method GET & crl.launch'`
	if exit := sh.Execute(cmd); exit {
		t.Fatal("crl.script --json -c a quitté le shell")
	}
	results := sh.ScriptResults()
	if len(results) != 1 || !results[0].OK || results[0].StatusCode != 200 {
		t.Fatalf("résultat JSON inattendu : %+v", results)
	}
}
