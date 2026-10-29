//go:build linux

package shell

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"
	"unsafe"

	"crl-cli/internal/mint"
	"crl-cli/internal/request"
)

func setPTYSize(t *testing.T, f interface{ Fd() uintptr }, rows, cols uint16) {
	t.Helper()
	ws := struct {
		Row, Col, Xpixel, Ypixel uint16
	}{Row: rows, Col: cols}
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&ws))); errno != 0 {
		// Le redimensionnement est un bonus du test : certaines plateformes ou
		// environnements de CI refusent TIOCSWINSZ sans invalider le reste.
		t.Logf("redimensionnement PTY ignoré : %v", errno)
	}
}

// vtScreen est un mini-émulateur VT utilisé uniquement pour vérifier que les
// rafraîchissements remplacent bien le panneau au lieu d'en empiler plusieurs.
// Il couvre les séquences réellement émises par l'éditeur et par le shell.
type vtScreen struct {
	w, h       int
	row, col   int
	cells      [][]rune
	savedRow   int
	savedCol   int
	savedValid bool
}

func newVTScreen(w, h int) *vtScreen {
	s := &vtScreen{w: w, h: h, cells: make([][]rune, h)}
	for i := range s.cells {
		s.cells[i] = make([]rune, w)
		for j := range s.cells[i] {
			s.cells[i][j] = ' '
		}
	}
	return s
}

func (s *vtScreen) feed(raw string) {
	for i := 0; i < len(raw); {
		if raw[i] == 0x1b {
			if i+1 < len(raw) && raw[i+1] == '[' {
				j := i + 2
				for j < len(raw) && (raw[j] < 0x40 || raw[j] > 0x7e) {
					j++
				}
				if j >= len(raw) {
					return
				}
				params := raw[i+2 : j]
				s.csi(params, raw[j])
				i = j + 1
				continue
			}
			i++
			if i < len(raw) {
				i++
			}
			continue
		}
		r, size := utf8.DecodeRuneInString(raw[i:])
		i += size
		switch r {
		case '\r':
			s.col = 0
		case '\n':
			s.lineFeed()
		case '\b':
			if s.col > 0 {
				s.col--
			}
		case '\t':
			next := (s.col + 8) &^ 7
			for s.col < next {
				s.put(' ')
			}
		default:
			s.put(r)
		}
	}
}

func (s *vtScreen) csi(params string, final byte) {
	mode, count := 1, 0
	clean := strings.TrimLeft(params, "?")
	if clean != "" {
		parts := strings.Split(clean, ";")
		if n, err := strconv.Atoi(parts[0]); err == nil {
			mode = n
		}
		if n, err := strconv.Atoi(parts[len(parts)-1]); err == nil {
			count = n
		}
	}
	switch final {
	case 'A':
		s.row -= max(1, mode)
		if s.row < 0 {
			s.row = 0
		}
	case 'B':
		s.row += max(1, mode)
		if s.row >= s.h {
			s.row = s.h - 1
		}
	case 'C':
		s.col += max(1, mode)
		if s.col >= s.w {
			s.col = s.w - 1
		}
	case 'D':
		s.col -= max(1, mode)
		if s.col < 0 {
			s.col = 0
		}
	case 'H', 'f':
		s.row, s.col = 0, 0
		parts := strings.Split(clean, ";")
		if len(parts) > 0 {
			if n, err := strconv.Atoi(parts[0]); err == nil && n > 0 {
				s.row = minInt(n-1, s.h-1)
			}
		}
		if len(parts) > 1 {
			if n, err := strconv.Atoi(parts[1]); err == nil && n > 0 {
				s.col = minInt(n-1, s.w-1)
			}
		}
	case 'J':
		if mode == 2 {
			for r := range s.cells {
				for c := range s.cells[r] {
					s.cells[r][c] = ' '
				}
			}
			s.row, s.col = 0, 0
			return
		}
		s.clearFromCursor()
	case 'K':
		for c := s.col; c < s.w; c++ {
			s.cells[s.row][c] = ' '
		}
	case 's':
		s.savedRow, s.savedCol, s.savedValid = s.row, s.col, true
	case 'u':
		if s.savedValid {
			s.row, s.col = s.savedRow, s.savedCol
		}
	case 'h', 'l', 'm':
		// Modes privés et styles ANSI : sans effet sur le contenu.
	default:
		_ = count
	}
}

func (s *vtScreen) put(r rune) {
	if s.col >= s.w {
		s.col = 0
		s.lineFeed()
	}
	if s.row < 0 || s.row >= s.h || s.col < 0 || s.col >= s.w {
		return
	}
	s.cells[s.row][s.col] = r
	s.col++
}

func (s *vtScreen) lineFeed() {
	s.row++
	if s.row >= s.h {
		copy(s.cells, s.cells[1:])
		last := make([]rune, s.w)
		for i := range last {
			last[i] = ' '
		}
		s.cells[s.h-1] = last
		s.row = s.h - 1
	}
}

func (s *vtScreen) clearFromCursor() {
	for c := s.col; c < s.w; c++ {
		s.cells[s.row][c] = ' '
	}
	for r := s.row + 1; r < s.h; r++ {
		for c := 0; c < s.w; c++ {
			s.cells[r][c] = ' '
		}
	}
}

func (s *vtScreen) text() string {
	lines := make([]string, s.h)
	for i, row := range s.cells {
		lines[i] = strings.TrimRight(string(row), " ")
	}
	return strings.Join(lines, "\n")
}

func screenFrom(raw string) *vtScreen {
	s := newVTScreen(100, 24)
	s.feed(raw)
	return s
}

func waitPTYTextSince(t *testing.T, got *ptyCollector, from int, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		text := got.String()
		if from <= len(text) && strings.Contains(text[from:], want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("sortie PTY sans %q après l'offset %d :\n%q", want, from, got.String())
}

func previousLineIsBlank(text, prompt string) bool {
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.Contains(lines[i], prompt) {
			if i == 0 {
				return false
			}
			return strings.TrimSpace(lines[i-1]) == ""
		}
	}
	return false
}

func TestPTYJSONEditorKeepsOneCompactFrameAndRestoresPrompt(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	master, slave := openPTY(t)
	defer master.Close()
	defer slave.Close()

	hist := mint.NewHistory(mint.DefaultHistoryConfig())
	mgr, err := mint.NewManager(mint.DefaultConfig(), hist)
	if err != nil {
		t.Fatal(err)
	}
	capture := mint.BridgeEvent{
		Type: "request", FlowID: "editor", Timestamp: time.Now(),
		Method: "POST", URL: "http://example.test/edit",
		Headers: []mint.Header{{Name: "Content-Type", Value: "application/json"}},
		Body:    []byte(`{}`), BodySize: 2,
	}
	hist.AddRequest(capture)
	sh, err := New(Options{In: slave, Out: slave, Builder: request.New(), Manager: mgr})
	if err != nil {
		t.Fatal(err)
	}
	got := &ptyCollector{}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, e := master.Read(buf)
			if n > 0 {
				got.write(buf[:n])
			}
			if e != nil {
				return
			}
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sh.Run(ctx) }()
	waitPTYLine(t, got, "crl [GET] [Mint: OFF] >", 3*time.Second)

	if _, err := master.Write([]byte("crl.json 1 edit\r")); err != nil {
		t.Fatal(err)
	}
	waitPTYText(t, got, "CRL JSON EDITOR", 3*time.Second)

	screen := screenFrom(got.String())
	if n := strings.Count(screen.text(), "CRL JSON EDITOR"); n != 1 {
		t.Fatalf("éditeur affiché %d fois après ouverture :\n%s", n, screen.text())
	}
	if n := strings.Count(screen.text(), "~"); n > 3 {
		t.Fatalf("cadre inutilement haut (%d lignes ~) pour un JSON court :\n%s", n, screen.text())
	}

	// Un redimensionnement doit reconstruire le cadre à la nouvelle largeur.
	setPTYSize(t, slave, 30, 70)
	if _, err := master.Write([]byte("\x1b[D")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	screen = screenFrom(got.String())
	if n := strings.Count(screen.text(), "CRL JSON EDITOR"); n != 1 {
		t.Fatalf("éditeur dupliqué après redimensionnement :\n%s", screen.text())
	}
	topWidth := 0
	for _, line := range strings.Split(screen.text(), "\n") {
		if strings.HasPrefix(line, "╭") && strings.Contains(line, "─") {
			topWidth = utf8.RuneCountInString(line)
			break
		}
	}
	if topWidth != 70 {
		t.Fatalf("largeur du cadre après redimensionnement = %d, attendu 70 :\n%s", topWidth, screen.text())
	}

	// Place le curseur avant le '}' et insère un champ tout en gardant un JSON
	// valide. Plusieurs frappes successives ne doivent pas empiler les cadres.
	if _, err := master.Write([]byte("\"x\":1")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	screen = screenFrom(got.String())
	if n := strings.Count(screen.text(), "CRL JSON EDITOR"); n != 1 {
		t.Fatalf("éditeur dupliqué après saisie :\n%s", screen.text())
	}
	if !strings.Contains(screen.text(), `{"x":1}`) {
		t.Fatalf("saisie absente du cadre :\n%s", screen.text())
	}
	if _, err := master.Write([]byte("\x13")); err != nil { // Ctrl+S
		t.Fatal(err)
	}
	waitPTYText(t, got, "Corps de la requête #1 modifié", 3*time.Second)
	waitPTYLine(t, got, "crl [GET] [Mint: OFF] >", 3*time.Second)
	screen = screenFrom(got.String())
	if strings.Contains(screen.text(), "CRL JSON EDITOR") {
		t.Fatalf("cadre non effacé après validation :\n%s", screen.text())
	}
	if !previousLineIsBlank(screen.text(), "crl [GET] [Mint: OFF] >") {
		t.Fatalf("prompt collé au résultat après validation :\n%s", screen.text())
	}
	if b, err := sh.requestBuilderFor(mustCapture(t, hist, 1)); err != nil {
		t.Fatal(err)
	} else if got, want := string(b.Body()), `{"x":1}`; got != want {
		t.Fatalf("corps édité = %q, attendu %q", got, want)
	}

	// Esc doit restaurer le corps précédent et laisser le terminal propre.
	mark := len(got.String())
	if _, err := master.Write([]byte("crl.json 1 edit\r")); err != nil {
		t.Fatal(err)
	}
	waitPTYTextSince(t, got, mark, "CRL JSON EDITOR", 3*time.Second)
	if _, err := master.Write([]byte("\x1b")); err != nil {
		t.Fatal(err)
	}
	waitPTYText(t, got, "Édition annulée", 3*time.Second)
	waitPTYLine(t, got, "crl [GET] [Mint: OFF] >", 3*time.Second)
	screen = screenFrom(got.String())
	if strings.Contains(screen.text(), "CRL JSON EDITOR") {
		t.Fatalf("cadre non effacé après annulation :\n%s", screen.text())
	}
	if b, err := sh.requestBuilderFor(mustCapture(t, hist, 1)); err != nil {
		t.Fatal(err)
	} else if got, want := string(b.Body()), `{"x":1}`; got != want {
		t.Fatalf("Esc a modifié le corps : %q, attendu %q", got, want)
	}

	if _, err := master.Write([]byte("exit\r")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shell : %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("le shell ne s'est pas arrêté après exit")
	}
}

func mustCapture(t *testing.T, hist *mint.History, id int) mint.Capture {
	t.Helper()
	c, ok := hist.Get(id)
	if !ok {
		t.Fatalf("capture #%d absente", id)
	}
	return c
}

func TestPTYLaunchAndReplayStartResultOnNewLineAndKeepPromptSeparate(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	master, slave := openPTY(t)
	defer master.Close()
	defer slave.Close()

	srv := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	hist := mint.NewHistory(mint.DefaultHistoryConfig())
	mgr, err := mint.NewManager(mint.DefaultConfig(), hist)
	if err != nil {
		t.Fatal(err)
	}
	hist.AddRequest(mint.BridgeEvent{
		Type: "request", FlowID: "replay", Timestamp: time.Now(),
		Method: "GET", URL: srv.URL + "/captured?token=secret",
	})
	sh, err := New(Options{In: slave, Out: slave, Builder: request.New(), Manager: mgr})
	if err != nil {
		t.Fatal(err)
	}
	got := &ptyCollector{}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, e := master.Read(buf)
			if n > 0 {
				got.write(buf[:n])
			}
			if e != nil {
				return
			}
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sh.Run(ctx) }()
	waitPTYLine(t, got, "crl [GET] [Mint: OFF] >", 3*time.Second)

	// crl.launch actif : la sortie doit commencer après la ligne de commande
	// et le prompt ne doit pas être collé au dernier en-tête/corps.
	if _, err := master.Write([]byte("crl.host " + srv.URL + "/active\r")); err != nil {
		t.Fatal(err)
	}
	waitPTYText(t, got, "URL configurée", 3*time.Second)
	waitPTYLine(t, got, "crl [GET] [Mint: OFF] >", 3*time.Second)
	if _, err := master.Write([]byte("crl.launch\r")); err != nil {
		t.Fatal(err)
	}
	waitPTYText(t, got, "Corps de la réponse", 5*time.Second)
	waitPTYLine(t, got, "crl [GET] [Mint: OFF] >", 5*time.Second)
	launchScreen := screenFrom(got.String())
	if !previousLineIsBlank(launchScreen.text(), "crl [GET] [Mint: OFF] >") {
		t.Fatalf("prompt collé à crl.launch :\n%s", launchScreen.text())
	}

	// crl.launch <id> utilise le même moteur de rejeu : titre, résumé, tableau,
	// en-têtes, corps et séparateur final doivent être présents.
	if _, err := master.Write([]byte("crl.launch 1\r")); err != nil {
		t.Fatal(err)
	}
	waitPTYText(t, got, "RÉPONSE #1", 5*time.Second)
	waitPTYText(t, got, "En-têtes de réponse", 5*time.Second)
	waitPTYText(t, got, "Corps — JSON", 5*time.Second)
	waitPTYLine(t, got, "crl [GET] [Mint: OFF] [Envoyées: 1/1] >", 5*time.Second)
	replayScreen := screenFrom(got.String())
	if !previousLineIsBlank(replayScreen.text(), "crl [GET] [Mint: OFF] [Envoyées: 1/1] >") {
		t.Fatalf("prompt collé au rejeu :\n%s", replayScreen.text())
	}
	for _, want := range []string{"Résumé", "Résultats", "RÉPONSE #1", "Statut", "Corps — JSON"} {
		if !strings.Contains(got.String(), want) {
			t.Fatalf("rendu de rejeu sans %q :\n%s", want, got.String())
		}
	}

	if _, err := master.Write([]byte("exit\r")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shell : %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("le shell ne s'est pas arrêté après exit")
	}
}
