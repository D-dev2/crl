package shell

import (
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"crl-cli/internal/mint"
)

func TestRedirectFollowedByDefaultShowsInitialAndFinalResponses(t *testing.T) {
	if !curlAvailable(t) {
		return
	}
	srv := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/":
			w.Header().Set("X-Step", "initial")
			w.Header().Set("Location", "/user/login")
			w.WriteHeader(http.StatusFound)
			_, _ = io.WriteString(w, "redirect-body")
		case "/user/login":
			w.Header().Set("X-Step", "final")
			_, _ = io.WriteString(w, "final-body")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	sh, out, _, _, builder := newTestShell(t, "")
	_ = builder.SetURL(srv.URL + "/admin/")
	sh.Execute("crl.launch")
	text := out.String()

	for _, want := range []string{
		"RÉPONSE INITIALE · 302 FOUND",
		"Location",
		"/user/login",
		"X-Step: initial",
		"redirect-body",
		"RÉPONSE FINALE",
		"200 OK",
		"X-Step: final",
		"final-body",
		"Chaîne de redirections (1)",
		"GET",
		srv.URL + "/admin/",
		srv.URL + "/user/login",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("sortie de redirection incomplète (%q) :\n%s", want, text)
		}
	}
	if initial, final := strings.Index(text, "RÉPONSE INITIALE"), strings.Index(text, "RÉPONSE FINALE"); initial < 0 || final < 0 || initial > final {
		t.Fatalf("la réponse initiale doit précéder la réponse finale :\n%s", text)
	}
	if strings.Contains(text, "\033[") {
		t.Fatalf("couleurs ANSI émises hors terminal :\n%s", text)
	}
}

func TestRedirectDisabledStopsAtFirstResponse(t *testing.T) {
	if !curlAvailable(t) {
		return
	}
	srv := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/":
			w.Header().Set("Location", "/user/login")
			w.WriteHeader(http.StatusFound)
			_, _ = io.WriteString(w, "redirect-body")
		default:
			_, _ = io.WriteString(w, "final-body")
		}
	}))
	defer srv.Close()

	sh, out, _, _, builder := newTestShell(t, "")
	_ = builder.SetURL(srv.URL + "/admin/")

	sh.Execute("crl.redirect off")
	out.Reset()
	sh.Execute("crl.launch")
	text := out.String()
	for _, want := range []string{"302 Found", "Location", "redirect-body", "suivi automatique est désactivé"} {
		if !strings.Contains(text, want) {
			t.Fatalf("réponse 3xx incomplète (%q) :\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"final-body", "RÉPONSE FINALE"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("la redirection a été suivie malgré crl.redirect off (%q) :\n%s", unwanted, text)
		}
	}

	out.Reset()
	sh.Execute("crl.redirect on")
	out.Reset()
	sh.Execute("crl.launch")
	if !strings.Contains(out.String(), "final-body") || !strings.Contains(out.String(), "RÉPONSE FINALE") {
		t.Fatalf("le suivi n'a pas repris après crl.redirect on :\n%s", out.String())
	}
}

func TestRedirectChainKeepsEveryStepInOrder(t *testing.T) {
	if !curlAvailable(t) {
		return
	}
	srv := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/a":
			w.Header().Set("X-Step", "a")
			w.Header().Set("Location", "/b")
			w.WriteHeader(http.StatusMovedPermanently)
			_, _ = io.WriteString(w, "body-a")
		case "/b":
			w.Header().Set("X-Step", "b")
			w.Header().Set("Location", "/c")
			w.WriteHeader(http.StatusTemporaryRedirect)
			_, _ = io.WriteString(w, "body-b")
		case "/c":
			w.Header().Set("X-Step", "c")
			_, _ = io.WriteString(w, "body-c")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	sh, out, _, _, builder := newTestShell(t, "")
	_ = builder.SetURL(srv.URL + "/a")
	sh.Execute("crl.launch")
	text := out.String()

	for _, want := range []string{
		"RÉPONSE INITIALE · 301 MOVED PERMANENTLY",
		"RÉPONSE INTERMÉDIAIRE 2 · 307 TEMPORARY REDIRECT",
		"RÉPONSE FINALE",
		"body-a", "body-b", "body-c",
		"X-Step: a", "X-Step: b", "X-Step: c",
		"Chaîne de redirections (2)",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("étape de redirection absente (%q) :\n%s", want, text)
		}
	}
	for _, path := range []string{"/a", "/b", "/c"} {
		if !strings.Contains(text, srv.URL+path) {
			t.Fatalf("URL %s absente :\n%s", path, text)
		}
	}
	a, b, c := strings.Index(text, "body-a"), strings.Index(text, "body-b"), strings.Index(text, "body-c")
	if a < 0 || b < 0 || c < 0 || !(a < b && b < c) {
		t.Fatalf("ordre des corps incorrect :\n%s", text)
	}
}

func TestRedirectMethodAndBodyRules(t *testing.T) {
	if !curlAvailable(t) {
		return
	}
	cases := []struct {
		code       int
		wantMethod string
		wantBody   string
	}{
		{http.StatusMovedPermanently, "GET", ""},
		{http.StatusFound, "GET", ""},
		{http.StatusSeeOther, "GET", ""},
		{http.StatusTemporaryRedirect, "POST", "payload"},
		{http.StatusPermanentRedirect, "POST", "payload"},
	}
	for _, tc := range cases {
		t.Run(strconv.Itoa(tc.code), func(t *testing.T) {
			var gotMethod, gotBody, gotContentType string
			srv := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/start" {
					w.Header().Set("Location", "/target")
					w.WriteHeader(tc.code)
					return
				}
				gotMethod = r.Method
				body, _ := io.ReadAll(r.Body)
				gotBody = string(body)
				gotContentType = r.Header.Get("Content-Type")
				_, _ = io.WriteString(w, "ok")
			}))
			defer srv.Close()

			sh, out, _, _, builder := newTestShell(t, "")
			_ = builder.SetURL(srv.URL + "/start")
			_ = builder.SetMethod("POST")
			_ = builder.SetHeader("Content-Type", "application/json")
			builder.SetBodyText("payload")
			sh.Execute("crl.launch")

			if gotMethod != tc.wantMethod {
				t.Fatalf("méthode reçue = %q, attendu %q :\n%s", gotMethod, tc.wantMethod, out.String())
			}
			if gotBody != tc.wantBody {
				t.Fatalf("corps reçu = %q, attendu %q :\n%s", gotBody, tc.wantBody, out.String())
			}
			if tc.wantBody == "" && gotContentType != "" {
				t.Fatalf("Content-Type conservé avec un corps abandonné : %q\n%s", gotContentType, out.String())
			}
		})
	}
}

func TestRedirectCrossOriginDropsCredentials(t *testing.T) {
	if !curlAvailable(t) {
		return
	}
	var got http.Header
	target := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = io.WriteString(w, "cross-origin-ok")
	}))
	defer target.Close()
	start := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target.URL+"/target")
		w.WriteHeader(http.StatusFound)
	}))
	defer start.Close()

	sh, out, _, _, builder := newTestShell(t, "")
	_ = builder.SetURL(start.URL + "/start")
	for name, value := range map[string]string{
		"Authorization":       "Bearer secret",
		"Proxy-Authorization": "Basic secret",
		"Cookie":              "session=secret",
		"X-Api-Key":           "secret",
		"X-Test":              "kept",
	} {
		if err := builder.SetHeader(name, value); err != nil {
			t.Fatal(err)
		}
	}
	sh.Execute("crl.launch")
	if !strings.Contains(out.String(), "cross-origin-ok") {
		t.Fatalf("redirection cross-origin non suivie :\n%s", out.String())
	}
	for _, name := range []string{"Authorization", "Proxy-Authorization", "Cookie", "X-Api-Key"} {
		if value := got.Get(name); value != "" {
			t.Fatalf("%s transmis à une autre origine : %q", name, value)
		}
	}
	if got.Get("X-Test") != "kept" {
		t.Fatalf("en-tête non sensible perdu : %#v", got)
	}
}

func TestRedirectSameOriginKeepsCredentials(t *testing.T) {
	if !curlAvailable(t) {
		return
	}
	var gotAuthorization, gotCookie string
	srv := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			w.Header().Set("Location", "/target")
			w.WriteHeader(http.StatusFound)
			return
		}
		gotAuthorization = r.Header.Get("Authorization")
		gotCookie = r.Header.Get("Cookie")
		_, _ = io.WriteString(w, "same-origin-ok")
	}))
	defer srv.Close()

	sh, out, _, _, builder := newTestShell(t, "")
	_ = builder.SetURL(srv.URL + "/start")
	_ = builder.SetHeader("Authorization", "Bearer same")
	_ = builder.SetHeader("Cookie", "session=same")
	sh.Execute("crl.launch")
	if !strings.Contains(out.String(), "same-origin-ok") {
		t.Fatalf("redirection same-origin non suivie :\n%s", out.String())
	}
	if gotAuthorization != "Bearer same" || gotCookie != "session=same" {
		t.Fatalf("identifiants same-origin perdus : Authorization=%q Cookie=%q", gotAuthorization, gotCookie)
	}
}

func TestRedirectLoopIsDetected(t *testing.T) {
	if !curlAvailable(t) {
		return
	}
	var requests int32
	srv := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.Header().Set("Location", "/loop")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	sh, out, _, _, builder := newTestShell(t, "")
	_ = builder.SetURL(srv.URL + "/loop")
	sh.Execute("crl.launch")
	text := out.String()
	if !strings.Contains(text, "boucle de redirection détectée") {
		t.Fatalf("boucle non signalée :\n%s", text)
	}
	if n := atomic.LoadInt32(&requests); n != 1 {
		t.Fatalf("la boucle a été suivie %d fois au lieu d'être arrêtée immédiatement", n)
	}
}

func TestRedirectMaximumIsEnforced(t *testing.T) {
	if !curlAvailable(t) {
		return
	}
	var requests int32
	srv := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		n, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/chain/"))
		w.Header().Set("Location", "/chain/"+strconv.Itoa(n+1))
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	sh, out, _, _, builder := newTestShell(t, "")
	_ = builder.SetURL(srv.URL + "/chain/0")
	sh.Execute("crl.launch")
	text := out.String()
	if !strings.Contains(text, "limite de redirections atteinte (10)") {
		t.Fatalf("limite de redirections non signalée :\n%s", text)
	}
	if n := atomic.LoadInt32(&requests); n > 11 {
		t.Fatalf("%d requêtes envoyées malgré la limite de 10 redirections", n)
	}
}

func TestRedirectCommandState(t *testing.T) {
	sh, out, _, _, _ := newTestShell(t, "")
	sh.Execute("crl.redirect")
	if !strings.Contains(out.String(), "activé") {
		t.Fatalf("le suivi doit être activé par défaut :\n%s", out.String())
	}
	out.Reset()
	sh.Execute("crl.redirect off")
	if !strings.Contains(out.String(), "désactivé") {
		t.Fatalf("crl.redirect off non appliqué :\n%s", out.String())
	}
	if sh.followRedirects {
		t.Fatal("followRedirects doit être faux après crl.redirect off")
	}
	out.Reset()
	sh.Execute("crl.redirect on")
	if !strings.Contains(out.String(), "activé") || !sh.followRedirects {
		t.Fatalf("crl.redirect on non appliqué :\n%s", out.String())
	}
	out.Reset()
	sh.Execute("crl.redirect peut-être")
	if !strings.Contains(out.String(), "usage") {
		t.Fatalf("valeur invalide acceptée :\n%s", out.String())
	}
}

func TestReplayShowsRedirectResponsesBeforeFinal(t *testing.T) {
	if !curlAvailable(t) {
		return
	}
	srv := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/start":
			w.Header().Set("X-Step", "initial")
			w.Header().Set("Location", "/end")
			w.WriteHeader(http.StatusFound)
			_, _ = io.WriteString(w, "replay-intermediate")
		case "/end":
			w.Header().Set("X-Step", "final")
			_, _ = io.WriteString(w, "replay-final")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	sh, out, _, hist, _ := newTestShell(t, "")
	hist.AddRequest(mint.BridgeEvent{
		Type: "request", FlowID: "redirect-replay", Timestamp: time.Now(),
		Method: "GET", URL: srv.URL + "/start", BodySize: 0,
	})
	sh.Execute("mint.select 1")
	out.Reset()
	sh.Execute("mint.replay")
	text := out.String()

	for _, want := range []string{
		"RÉPONSE INITIALE · 302 FOUND",
		"X-Step: initial",
		"replay-intermediate",
		"RÉPONSE #1",
		"X-Step: final",
		"replay-final",
		"URL finale",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("rejeu de redirection incomplet (%q) :\n%s", want, text)
		}
	}
	initial := strings.Index(text, "RÉPONSE INITIALE")
	final := strings.Index(text, "RÉPONSE #1")
	if initial < 0 || final < 0 || initial > final {
		t.Fatalf("la réponse initiale doit précéder la réponse finale du rejeu :\n%s", text)
	}
}

func TestRedirectReplayDisabledStopsAtInitialResponse(t *testing.T) {
	if !curlAvailable(t) {
		return
	}
	srv := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			w.Header().Set("Location", "/end")
			w.WriteHeader(http.StatusFound)
			_, _ = io.WriteString(w, "replay-initial")
			return
		}
		_, _ = io.WriteString(w, "replay-final")
	}))
	defer srv.Close()

	sh, out, _, hist, _ := newTestShell(t, "")
	hist.AddRequest(mint.BridgeEvent{
		Type: "request", FlowID: "redirect-replay-off", Timestamp: time.Now(),
		Method: "GET", URL: srv.URL + "/start", BodySize: 0,
	})
	sh.Execute("crl.redirect off")
	sh.Execute("mint.select 1")
	out.Reset()
	sh.Execute("mint.replay")
	text := out.String()
	if !strings.Contains(text, "302 Found") || !strings.Contains(text, "replay-initial") {
		t.Fatalf("réponse initiale absente du rejeu :\n%s", text)
	}
	if strings.Contains(text, "replay-final") || strings.Contains(text, "RÉPONSE INITIALE") {
		t.Fatalf("le rejeu a suivi la redirection malgré crl.redirect off :\n%s", text)
	}
}

func curlAvailable(t *testing.T) bool {
	t.Helper()
	if _, err := execLookPath("curl"); err != nil {
		t.Skip("curl absent")
		return false
	}
	return true
}

// execLookPath est une variable pour garder les tests indépendants de l'ordre
// d'initialisation des helpers existants.
var execLookPath = func(file string) (string, error) {
	return exec.LookPath(file)
}

var _ = sync.Mutex{}
