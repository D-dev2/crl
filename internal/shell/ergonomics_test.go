package shell

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"crl-cli/internal/display"
	"crl-cli/internal/mint"
	"crl-cli/internal/request"
)

func TestPromptCompactAdaptiveAndLive(t *testing.T) {
	sh, _, _, hist, builder := newTestShell(t, "")
	addCapture(t, hist)
	hist.AddRequest(mint.BridgeEvent{
		Type: "request", FlowID: "flow-2", Timestamp: time.Now(),
		Method: "GET", URL: "http://example.test/second",
	})
	if exit := sh.Execute("mint.select 1 2"); exit {
		t.Fatal("mint.select a demandé la sortie")
	}
	if err := builder.SetURL("https://example.test/very/long/path?token=secret"); err != nil {
		t.Fatal(err)
	}
	if err := builder.SetMethod("PUT"); err != nil {
		t.Fatal(err)
	}
	if err := builder.AddHeader("X-Test", "1"); err != nil {
		t.Fatal(err)
	}
	builder.SetBodyText("payload")

	for _, width := range []int{50, 80, 120} {
		t.Run(strconv.Itoa(width), func(t *testing.T) {
			t.Setenv("COLUMNS", strconv.Itoa(width))
			got := sh.promptText()
			if strings.ContainsAny(got, "\r\n") {
				t.Fatalf("invite multiligne à %d colonnes : %q", width, got)
			}
			if !strings.HasSuffix(got, "$") {
				t.Fatalf("invite sans $ final : %q", got)
			}
			if w := display.VisibleWidth(got); w > width {
				t.Fatalf("invite de %d colonnes dépasse %d : %q", w, width, got)
			}
			for _, want := range []string{"crl", "[SEL 2", "PUT", "H1", "B"} {
				if !strings.Contains(got, want) {
					t.Fatalf("invite à %d colonnes sans %q : %q", width, want, got)
				}
			}
			if !strings.Contains(got, "[MINT OFF]") && !strings.Contains(got, "[M:OFF]") {
				t.Fatalf("invite à %d colonnes sans état Mint : %q", width, got)
			}
			if width == 50 && (!strings.Contains(got, "GET") || !strings.Contains(got, "POST")) {
				t.Fatalf("méthodes sélectionnées absentes à 50 colonnes : %q", got)
			}
			if strings.Contains(got, "example.test") || strings.Contains(got, "token") {
				t.Fatalf("invite contenant une URL ou un secret : %q", got)
			}
			if width >= 80 && !strings.Contains(got, "GET×1") {
				t.Fatalf("ventilation des méthodes absente à %d colonnes : %q", width, got)
			}
		})
	}

	t.Setenv("COLUMNS", "80")
	before := sh.promptText()
	if exit := sh.Execute("crl.head X-Builder: oui"); exit {
		t.Fatal("crl.head a demandé la sortie")
	}
	after := sh.promptText()
	if before == after || !strings.Contains(after, "H2") {
		t.Fatalf("invite non actualisée après ajout d'en-tête : avant=%q après=%q", before, after)
	}
	sh.Execute("crl.head.clear X-Builder")
	if got := sh.promptText(); !strings.Contains(got, "H1") || strings.Contains(got, "H2") {
		t.Fatalf("invite non actualisée après suppression d'en-tête : %q", got)
	}
	sh.Execute("crl.body clear")
	if got := sh.promptText(); strings.Contains(got, " B]") {
		t.Fatalf("invite indique encore un corps : %q", got)
	}
	sh.Execute("mint.select remove 2")
	if got := sh.promptText(); !strings.Contains(got, "[SEL 1") {
		t.Fatalf("invite non actualisée après réduction de sélection : %q", got)
	}
	sh.Execute("mint.select clear")
	if got := sh.promptText(); !strings.Contains(got, "[SEL 0]") {
		t.Fatalf("invite non actualisée après sélection vide : %q", got)
	}
}

func TestBuilderAndCaptureCommandsStayIndependent(t *testing.T) {
	sh, out, _, hist, builder := newTestShell(t, "")
	addCapture(t, hist)
	hist.AddRequest(mint.BridgeEvent{
		Type: "request", FlowID: "flow-2", Timestamp: time.Now(),
		Method: "GET", URL: "http://example.test/two",
		Headers: []mint.Header{{Name: "X-Keep", Value: "keep"}},
		Body:    []byte("old"), BodySize: 3,
	})
	sh.Execute("mint.select 1 2")

	sh.Execute("crl.host https://builder.test/path")
	sh.Execute("crl.method POST")
	if builder.URL() != "https://builder.test/path" || builder.Method() != "POST" {
		t.Fatalf("constructeur non configuré : %s %s", builder.Method(), builder.URL())
	}
	b1, err := sh.requestBuilderFor(mustCapture(t, hist, 1))
	if err != nil {
		t.Fatal(err)
	}
	if b1.URL() != "http://127.0.0.1:5002/users/v1/login?next=%2Fhome&x=1&x=&token=supersecret" {
		t.Fatalf("la sélection a modifié l'URL de la capture 1 : %s", b1.URL())
	}
	if len(sh.history.SelectedMany()) != 2 {
		t.Fatal("la sélection a été modifiée par une commande du constructeur")
	}

	sh.Execute("crl.host set 1 add https://capture.test/new?x=1")
	sh.Execute("crl.method set 1 add PATCH")
	sh.Execute("crl.head set 1 add sec-fetch-dest: iframe")
	sh.Execute("crl.head set 1 add X-Time: 12:30:45")
	sh.Execute("crl.head set 1 add X-Note: two  words")
	sh.Execute("crl.cookie set 1 add session=nouveau")
	sh.Execute("crl.cookie set 1 add theme=dark")
	sh.Execute("crl.param set 1 add q=hello")
	sh.Execute("crl.json set 1 add {\"x\":1}")

	if builder.URL() != "https://builder.test/path" || builder.Method() != "POST" || builder.BodySize() != 0 {
		t.Fatalf("une commande ciblée a modifié le constructeur : %s %s corps=%d", builder.Method(), builder.URL(), builder.BodySize())
	}
	if b1.URL() != "https://capture.test/new?x=1&q=hello" {
		t.Fatalf("URL de capture 1 = %q", b1.URL())
	}
	if b1.Method() != "PATCH" {
		t.Fatalf("méthode de capture 1 = %q", b1.Method())
	}
	if got := headerValueOf(b1.Headers(), "sec-fetch-dest"); got != "iframe" {
		t.Fatalf("sec-fetch-dest = %q", got)
	}
	if got := headerValueOf(b1.Headers(), "X-Time"); got != "12:30:45" {
		t.Fatalf("X-Time = %q", got)
	}
	if got := headerValueOf(b1.Headers(), "X-Note"); got != "two  words" {
		t.Fatalf("X-Note = %q", got)
	}
	if got := headerValueOf(b1.Headers(), "Cookie"); got != "session=nouveau; theme=dark" {
		t.Fatalf("Cookie = %q", got)
	}
	if got := string(b1.Body()); got != `{"x":1}` {
		t.Fatalf("corps de capture 1 = %q", got)
	}
	b2, err := sh.requestBuilderFor(mustCapture(t, hist, 2))
	if err != nil {
		t.Fatal(err)
	}
	if b2.URL() != "http://example.test/two" || b2.Method() != "GET" || string(b2.Body()) != "old" {
		t.Fatalf("capture 2 modifiée : %s %s %q", b2.Method(), b2.URL(), b2.Body())
	}

	sh.Execute("crl.head set 1 remove sec-fetch-dest")
	sh.Execute("crl.head set 1 remove X-Time")
	sh.Execute("crl.head set 1 remove X-Note")
	sh.Execute("crl.cookie set 1 remove session")
	sh.Execute("crl.cookie set 1 remove theme")
	sh.Execute("crl.param set 1 remove q")
	sh.Execute("crl.body set 1 remove")
	sh.Execute("crl.method set 1 remove")
	sh.Execute("crl.host set 1 remove")

	if got := headerValueOf(b1.Headers(), "sec-fetch-dest"); got != "" {
		t.Fatalf("sec-fetch-dest toujours présent : %q", got)
	}
	if got := headerValueOf(b1.Headers(), "Cookie"); got != "" {
		t.Fatalf("cookie toujours présent : %q", got)
	}
	if b1.Method() != "GET" || b1.URL() != "" || b1.BodySize() != 0 {
		t.Fatalf("capture 1 mal nettoyée : %s %q corps=%d", b1.Method(), b1.URL(), b1.BodySize())
	}
	if got := headerValueOf(b1.Headers(), "X-Test"); got != "demo" {
		t.Fatalf("les autres en-têtes de la capture ne sont pas préservés : X-Test=%q", got)
	}
	if len(sh.history.SelectedMany()) != 2 {
		t.Fatal("la sélection a été modifiée par les commandes ciblées")
	}
	if !strings.Contains(out.String(), "Modifications") {
		t.Fatalf("résumé des modifications absent :\n%s", out.String())
	}
}

func TestReplayUsesOnlySelectionOrExplicitTarget(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(paths)
	}
	lastPath := func() string {
		mu.Lock()
		defer mu.Unlock()
		if len(paths) == 0 {
			return ""
		}
		return paths[len(paths)-1]
	}
	pathSnapshot := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...)
	}
	sh, out, _, hist, builder := newTestShell(t, "")
	for _, path := range []string{"/one", "/two", "/three"} {
		hist.AddRequest(mint.BridgeEvent{Type: "request", FlowID: path, Timestamp: time.Now(), Method: "GET", URL: srv.URL + path})
	}

	sh.Execute("mint.select 1 2 3")
	sh.Execute("crl.launch 2")
	if count() != 1 {
		t.Fatalf("crl.launch 2 a envoyé %d requête(s)", count())
	}
	if len(sh.history.SelectedMany()) != 3 {
		t.Fatal("un rejeu individuel a modifié la sélection")
	}

	sh.Execute("crl.launch")
	if count() != 4 {
		t.Fatalf("crl.launch n'a pas rejoué les 3 captures sélectionnées : %d", count())
	}
	out.Reset()
	sh.Execute("crl.launch all")
	if !strings.Contains(out.String(), "n'est pas une cible valide") || !strings.Contains(out.String(), "mint.select all") {
		t.Fatalf("crl.launch all n'a pas été refusé clairement :\n%s", out.String())
	}
	if count() != 4 {
		t.Fatal("crl.launch all a envoyé des requêtes")
	}
	if len(sh.history.SelectedMany()) != 3 {
		t.Fatal("crl.launch all a modifié la sélection")
	}

	sh.Execute("mint.select remove 2")
	if len(sh.history.SelectedMany()) != 2 {
		t.Fatal("mint.select remove 2 n'a pas retiré uniquement la capture 2")
	}
	sh.Execute("mint.replay")
	if count() != 6 {
		t.Fatalf("mint.replay n'a pas rejoué les deux captures restantes : %d", count())
	}
	sh.Execute("mint.select clear")
	if len(sh.history.SelectedMany()) != 0 {
		t.Fatal("mint.select clear n'a pas vidé la sélection")
	}
	out.Reset()
	sh.Execute("mint.replay")
	if !strings.Contains(out.String(), "aucune capture sélectionnée") {
		t.Fatalf("mint.replay sans sélection aurait dû échouer :\n%s", out.String())
	}
	if count() != 6 {
		t.Fatal("mint.replay sans sélection a envoyé une requête")
	}

	_ = builder.SetURL(srv.URL + "/active")
	sh.Execute("crl.launch")
	if count() != 7 || lastPath() != "/active" {
		t.Fatalf("crl.launch sans sélection n'a pas envoyé le constructeur : %v", pathSnapshot())
	}
	sh.Execute("mint.select 1 2 3")
	sh.Execute("crl.launch 1")
	if len(sh.history.SelectedMany()) != 3 {
		t.Fatal("crl.launch 1 a modifié la sélection")
	}
	if count() != 8 || lastPath() != "/one" {
		t.Fatalf("crl.launch 1 n'a pas envoyé uniquement la capture 1 : %v", pathSnapshot())
	}
}

func TestChainedCommandsAndURLAmperstand(t *testing.T) {
	var mu sync.Mutex
	type received struct {
		method, path, query, header, body string
	}
	var got []received
	srv := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, received{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, header: r.Header.Get("X-Chain"), body: string(body)})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer func() {
		srv.CloseClientConnections()
		srv.Close()
	}()

	sh, out, _, _, _ := newTestShell(t, "")
	line := "crl.host " + srv.URL + "/chain?a=1&b=2 & crl.method POST & crl.head X-Chain: yes & crl.body hello & crl.launch"
	sh.Execute(line)
	mu.Lock()
	if len(got) != 1 {
		t.Fatalf("enchaînement : %d requête(s) reçue(s)\n%s", len(got), out.String())
	}
	request := got[0]
	mu.Unlock()
	if request.method != "POST" || request.path != "/chain" || request.query != "a=1&b=2" || request.header != "yes" || request.body != "hello" {
		t.Fatalf("requête enchaînée incorrecte : %+v", request)
	}

	out.Reset()
	sh.Execute("crl.host ftp://example.test & crl.launch")
	if !strings.Contains(out.String(), "Erreur") {
		t.Fatalf("l'erreur d'URL n'a pas été affichée :\n%s", out.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatal("l'enchaînement a continué après une commande en échec")
	}
}

func TestANSISequencesAreNeverShownAsText(t *testing.T) {
	if got := sanitizeTerminal("avant\x1b[31mrouge\x1b[0m\naprès"); got != "avantrouge après" {
		t.Fatalf("sanitizeTerminal = %q", got)
	}
	if got := sanitizeTerminal("a\x1b]0;titre\x07b\x1b[2Jc"); got != "abc" {
		t.Fatalf("séquences OSC/CSI non supprimées : %q", got)
	}
	if got := sanitizeTerminalKeepANSI("\x1b[31mrouge\x1b[0m\x1b[2J"); got != "\x1b[31mrouge\x1b[0m" {
		t.Fatalf("sanitizeTerminalKeepANSI = %q", got)
	}

	sh, out, _, hist, builder := newTestShell(t, "")
	hist.AddRequest(mint.BridgeEvent{
		Type: "request", FlowID: "ansi", Timestamp: time.Now(), Method: "POST",
		URL:     "http://example.test/ansi",
		Headers: []mint.Header{{Name: "X-Test", Value: "\x1b[31mheader\x1b[0m"}},
		Body:    []byte("avant\x1b[31mrouge\x1b[0m après"), BodySize: 20,
	})
	sh.Execute("mint.show 1")
	sh.Execute("crl.show 1")
	text := out.String()
	if strings.Contains(text, "[31m") || strings.Contains(text, "\x1b") {
		t.Fatalf("séquence ANSI affichée comme du texte :\n%q", text)
	}
	if !strings.Contains(text, "header") {
		t.Fatalf("contenu utile perdu :\n%s", text)
	}

	srv := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("réponse\x1b[31mrouge\x1b[0m"))
	}))
	defer srv.Close()
	if err := builder.SetURL(srv.URL); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	sh.Execute("crl.launch")
	launchText := out.String()
	if strings.Contains(launchText, "[31m") || strings.Contains(launchText, "\x1b") {
		t.Fatalf("ANSI affiché comme texte par crl.launch :\n%q", launchText)
	}

	hist.AddRequest(mint.BridgeEvent{Type: "request", FlowID: "ansi-replay", Timestamp: time.Now(), Method: "GET", URL: srv.URL})
	sh.Execute("mint.select 2")
	out.Reset()
	sh.Execute("mint.replay")
	replayText := out.String()
	if strings.Contains(replayText, "[31m") || strings.Contains(replayText, "\x1b") {
		t.Fatalf("ANSI affiché comme texte par mint.replay :\n%q", replayText)
	}
}

func TestHelpDocumentsBuilderCapturesAndArguments(t *testing.T) {
	sh, out, _, _, _ := newTestShell(t, "")
	sh.Execute("help")
	text := out.String()
	for _, want := range []string{
		"CONSTRUCTEUR ACTIF", "CAPTURES MINT", "CIBLES ET VALEURS", "crl.cookie", "crl.param",
		"crl.host set <id> add|set <url>", "crl.launch all", "<cible>", "<id>", "<ids>",
		"<nom>=<valeur>", "<JSON>", "selected", "mint.replay",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("aide générale sans %q :\n%s", want, text)
		}
	}
	out.Reset()
	sh.Execute("crl.help")
	crlHelp := out.String()
	for _, want := range []string{"Constructeur actif", "Captures Mint", "sélection", "Enchaînement"} {
		if !strings.Contains(crlHelp, want) {
			t.Fatalf("crl.help sans %q :\n%s", want, crlHelp)
		}
	}
	out.Reset()
	sh.Execute("mint.help")
	mintHelp := out.String()
	for _, want := range []string{"mint.select", "mint.replay", "mint.requests"} {
		if !strings.Contains(mintHelp, want) {
			t.Fatalf("mint.help sans %q :\n%s", want, mintHelp)
		}
	}
}

func headerValueOf(headers []request.Header, name string) string {
	for _, header := range headers {
		if strings.EqualFold(header.Name, name) {
			return header.Value
		}
	}
	return ""
}

func TestTargetListsAndHeaderParsing(t *testing.T) {
	sh, _, _, hist, _ := newTestShell(t, "")
	addCapture(t, hist)
	hist.AddRequest(mint.BridgeEvent{
		Type: "request", FlowID: "flow-2", Timestamp: time.Now(),
		Method: "GET", URL: "http://example.test/two",
	})

	sh.Execute(`crl.head set 1 2 add X-Multi: un:deux`)
	sh.Execute("crl.head set 1,2 add X-Spaces: deux  espaces")
	sh.Execute(`crl.host set 1 2 add https://multi.test/x?keep=1`)
	sh.Execute(`crl.method set 1 2 add PATCH`)
	sh.Execute(`crl.cookie set 1 2 add session=abc`)
	sh.Execute(`crl.param set 1 2 add q=hello world`)
	sh.Execute(`crl.json set 1 2 add {"a":1}`)

	for _, id := range []int{1, 2} {
		b, err := sh.requestBuilderFor(mustCapture(t, hist, id))
		if err != nil {
			t.Fatal(err)
		}
		if got := headerValueOf(b.Headers(), "X-Multi"); got != "un:deux" {
			t.Fatalf("capture %d X-Multi = %q", id, got)
		}
		if got := headerValueOf(b.Headers(), "X-Spaces"); got != "deux  espaces" {
			t.Fatalf("capture %d X-Spaces = %q", id, got)
		}
		if b.Method() != "PATCH" || b.URL() != "https://multi.test/x?keep=1&q=hello+world" {
			t.Fatalf("capture %d = %s %s", id, b.Method(), b.URL())
		}
		if got := headerValueOf(b.Headers(), "Cookie"); got != "session=abc" {
			t.Fatalf("capture %d Cookie = %q", id, got)
		}
		if got := string(b.Body()); got != `{"a":1}` {
			t.Fatalf("capture %d corps = %q", id, got)
		}
	}
}

func TestPromptKeepsSelectionMethodsAtFiftyColumns(t *testing.T) {
	sh, _, _, hist, _ := newTestShell(t, "")
	for _, method := range []string{"GET", "POST", "PUT"} {
		hist.AddRequest(mint.BridgeEvent{
			Type: "request", FlowID: method, Timestamp: time.Now(),
			Method: method, URL: "http://example.test/" + strings.ToLower(method),
		})
	}
	sh.Execute("mint.select 1 2 3")
	t.Setenv("COLUMNS", "50")
	got := sh.promptText()
	for _, want := range []string{"[SEL 3", "GET", "POST", "PUT"} {
		if !strings.Contains(got, want) {
			t.Fatalf("invite à 50 colonnes sans %q : %q", want, got)
		}
	}
	if w := display.VisibleWidth(got); w > 50 {
		t.Fatalf("invite de %d colonnes : %q", w, got)
	}
}

func TestCRLSetGrammar(t *testing.T) {
	sh, out, _, hist, builder := newTestShell(t, "")
	for i, path := range []string{"/one", "/two", "/three"} {
		hist.AddRequest(mint.BridgeEvent{
			Type:      "request",
			FlowID:    fmt.Sprintf("f%d", i+1),
			Timestamp: time.Now(),
			Method:    "GET",
			URL:       "http://example.test" + path,
			Headers:   []mint.Header{{Name: "X-Keep", Value: "keep"}},
		})
	}
	sh.Execute("mint.select 1 2 3")

	// 1. crl.method set 1 add GET
	sh.Execute("crl.method set 1 add GET")
	out.Reset()
	// 2. crl.method set 1 set POST
	sh.Execute("crl.method set 1 set POST")
	out.Reset()
	// 3. crl.method set 1 remove (remise à GET)
	sh.Execute("crl.method set 1 remove")
	out.Reset()

	// Le constructeur actif n'a pas été touché.
	if builder.URL() != "" || builder.Method() != "GET" {
		t.Fatalf("constructeur actif modifié : %s %s", builder.Method(), builder.URL())
	}
	b1, err := sh.requestBuilderFor(mustCapture(t, hist, 1))
	if err != nil {
		t.Fatal(err)
	}
	if b1.Method() != "GET" {
		t.Fatalf("méthode capture 1 = %q", b1.Method())
	}
	if len(sh.history.SelectedMany()) != 3 {
		t.Fatal("la sélection a été modifiée par les commandes ciblées")
	}

	// 4. En-têtes : ajout, remplacement, suppression.
	sh.Execute(`crl.head set 1 add X-Test: valeur`)
	sh.Execute(`crl.head set 1 set X-Test: nouvelle`)
	sh.Execute(`crl.head set 1 add X-Debug: étape: détail`)
	out.Reset()
	sh.Execute(`crl.head set 1 remove X-Debug`)
	out.Reset()
	b1, err = sh.requestBuilderFor(mustCapture(t, hist, 1))
	if err != nil {
		t.Fatal(err)
	}
	if got := headerValueOf(b1.Headers(), "X-Test"); got != "nouvelle" {
		t.Fatalf("X-Test = %q", got)
	}
	if got := headerValueOf(b1.Headers(), "X-Debug"); got != "" {
		t.Fatalf("X-Debug toujours présent : %q", got)
	}
	if got := headerValueOf(b1.Headers(), "X-Keep"); got != "keep" {
		t.Fatalf("autre en-tête perdu : %q", got)
	}

	// 5. Paramètres : ajout et suppression.
	sh.Execute(`crl.param set 1 add token abc123`)
	sh.Execute(`crl.param set 1 set token xyz789`)
	out.Reset()
	sh.Execute(`crl.param set 1 remove token`)
	out.Reset()
	b1, err = sh.requestBuilderFor(mustCapture(t, hist, 1))
	if err != nil {
		t.Fatal(err)
	}
	u := b1.URL()
	if strings.Contains(u, "token") {
		t.Fatalf("paramètre token encore présent : %s", u)
	}

	// 6. Une nouvelle requête peut être créée pendant qu'une sélection existe.
	sh.Execute("crl.host https://nouveau.test/path")
	sh.Execute("crl.method PUT")
	if builder.URL() != "https://nouveau.test/path" || builder.Method() != "PUT" {
		t.Fatalf("constructeur non configuré pendant sélection : %s %s", builder.Method(), builder.URL())
	}
	if len(sh.history.SelectedMany()) != 3 {
		t.Fatal("la sélection a été détournée par le constructeur")
	}

	// 7. La sélection reste intacte après un rejeu individuel (sans curl, on vérifie l'état).
	out.Reset()
	sh.Execute("crl.launch 2")
	if len(sh.history.SelectedMany()) != 3 {
		t.Fatal("crl.launch 2 a modifié la sélection")
	}

	// 8. Le prompt reste compact et sur une seule ligne à plusieurs largeurs.
	for _, width := range []int{50, 80, 120} {
		t.Setenv("COLUMNS", strconv.Itoa(width))
		got := sh.promptText()
		if strings.ContainsAny(got, "\r\n") {
			t.Fatalf("invite multiligne à %d : %q", width, got)
		}
		if !strings.HasSuffix(got, "$") {
			t.Fatalf("invite sans $ final à %d : %q", width, got)
		}
		if w := display.VisibleWidth(got); w > width {
			t.Fatalf("invite dépasse %d colonnes : %q", w, got)
		}
	}

	// 9. Espacement entre commandes successives (chaîne &).
	out.Reset()
	sh.Execute("crl.host https://chain.test/x & crl.method POST & crl.head set 1 add X-Chain: oui")
	if !strings.Contains(out.String(), "Modifications") {
		t.Fatalf("chaîne & non traitée :\n%s", out.String())
	}
	if len(sh.history.SelectedMany()) != 3 {
		t.Fatal("la sélection a été modifiée par la chaîne &")
	}
}
