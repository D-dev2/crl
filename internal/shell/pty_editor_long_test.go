//go:build linux

package shell

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"crl-cli/internal/mint"
	"crl-cli/internal/request"
)

func editorFrameComplete(text string) bool {
	from := strings.LastIndex(text, "\033[J")
	if from < 0 {
		from = 0
	} else {
		from += len("\033[J")
	}
	return strings.Contains(text[from:], "╰")
}

// waitEditorIdle attend un cadre complet et une courte période sans nouvelle
// écriture. Cela évite de photographier un rafraîchissement au milieu de son
// écriture ou juste avant le traitement d'une frappe encore en file.
func waitEditorIdle(t *testing.T, got *ptyCollector, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	lastLen, quiet := -1, 0
	for time.Now().Before(deadline) {
		text := got.String()
		if len(text) == lastLen && editorFrameComplete(text) {
			quiet++
			if quiet >= 10 {
				return
			}
		} else {
			lastLen, quiet = len(text), 0
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("dernier rafraîchissement de l'éditeur incomplet ou instable :\n%q", got.String())
}

func assertSingleEditorFrame(t *testing.T, screen *vtScreen, title string) {
	t.Helper()
	text := screen.text()
	if n := strings.Count(text, title); n != 1 {
		t.Fatalf("%s affiché %d fois :\n%s", title, n, text)
	}
	if n := strings.Count(text, "╭"); n != 2 {
		t.Fatalf("bord supérieur affiché %d fois :\n%s", n, text)
	}
	if n := strings.Count(text, "╰"); n != 2 {
		t.Fatalf("bord inférieur affiché %d fois :\n%s", n, text)
	}
	if n := strings.Count(text, "├"); n != 4 {
		t.Fatalf("séparateurs du cadre = %d, attendu 4 :\n%s", n, text)
	}
}

func TestPTYBodyEditorLongCRLFRendersOneStableFrameAndPreservesBytes(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	master, slave := openPTY(t)
	defer master.Close()
	defer slave.Close()

	hist := mint.NewHistory(mint.DefaultHistoryConfig())
	mgr, err := mint.NewManager(mint.DefaultConfig(), hist)
	if err != nil {
		t.Fatal(err)
	}
	original := "{\r\n\t\"clé\": \"valeur accentuée très longue " + strings.Repeat("segment ", 40) + "\",\r\n" +
		"\t\"contrôle\": \"avant\x00après\",\r\n" +
		"\t\"fin\": true\r\n}\r\n"
	hist.AddRequest(mint.BridgeEvent{
		Type: "request", FlowID: "long-body", Timestamp: time.Now(),
		Method: "POST", URL: "http://example.test/body",
		Body: []byte(original), BodySize: int64(len(original)),
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
	waitPTYLine(t, got, testPrompt, 3*time.Second)

	if _, err := master.Write([]byte("crl.body set 1 edit\r")); err != nil {
		t.Fatal(err)
	}
	waitPTYText(t, got, "CRL BODY EDITOR", 3*time.Second)
	waitPTYText(t, got, "Ctrl+S : valider", 3*time.Second)
	screen := screenFrom(got.String())
	assertSingleEditorFrame(t, screen, "CRL BODY EDITOR")
	if strings.Contains(screen.text(), "\x00") {
		t.Fatalf("octet NUL brut envoyé au terminal :\n%s", screen.text())
	}

	// Frappes successives et déplacements : chaque rafraîchissement doit
	// remplacer le panneau, pas en ajouter un nouveau.
	for i := 0; i < 60; i++ {
		keys := []string{"\x1b[A", "\x1b[B", "\x1b[D", "\x1b[C", "x", "\x7f"}
		if _, err := master.Write([]byte(keys[i%len(keys)])); err != nil {
			t.Fatal(err)
		}
		time.Sleep(3 * time.Millisecond)
	}
	waitEditorIdle(t, got, 3*time.Second)
	screen = screenFrom(got.String())
	assertSingleEditorFrame(t, screen, "CRL BODY EDITOR")
	if strings.Count(screen.text(), "Cible :") != 1 {
		t.Fatalf("ligne de cible dupliquée :\n%s", screen.text())
	}

	// Esc restaure le corps d'origine, octet pour octet.
	if _, err := master.Write([]byte("\x1b")); err != nil {
		t.Fatal(err)
	}
	waitPTYText(t, got, "Édition annulée", 3*time.Second)
	waitPTYLine(t, got, testPrompt, 3*time.Second)
	if b, err := sh.requestBuilderFor(mustCapture(t, hist, 1)); err != nil {
		t.Fatal(err)
	} else if !bytes.Equal(b.Body(), []byte(original)) {
		t.Fatalf("Esc a modifié le corps : %q", b.Body())
	}
	if strings.Contains(screenFrom(got.String()).text(), "CRL BODY EDITOR") {
		t.Fatalf("cadre non effacé après annulation :\n%s", screenFrom(got.String()).text())
	}

	// Une validation sans modification doit sauvegarder exactement les mêmes
	// octets, sans retour chariot ni caractère de contrôle ajouté par le rendu.
	mark := len(got.String())
	if _, err := master.Write([]byte("crl.body set 1 edit\r")); err != nil {
		t.Fatal(err)
	}
	waitPTYTextSince(t, got, mark, "CRL BODY EDITOR", 3*time.Second)
	if _, err := master.Write([]byte("\x13")); err != nil {
		t.Fatal(err)
	}
	waitPTYText(t, got, "Corps de la requête #1 modifié", 3*time.Second)
	if b, err := sh.requestBuilderFor(mustCapture(t, hist, 1)); err != nil {
		t.Fatal(err)
	} else if !bytes.Equal(b.Body(), []byte(original)) {
		t.Fatalf("validation sans modification a altéré le corps : %q", b.Body())
	}

	// Une insertion réelle doit être sauvegardée telle quelle, sans convertir
	// les CRLF existants ni ajouter de caractère de rendu.
	waitPTYLine(t, got, testPrompt, 3*time.Second)
	mark = len(got.String())
	if _, err := master.Write([]byte("crl.body set 1 edit\r")); err != nil {
		t.Fatal(err)
	}
	waitPTYTextSince(t, got, mark, "CRL BODY EDITOR", 3*time.Second)
	if _, err := master.Write([]byte("X\x13")); err != nil {
		t.Fatal(err)
	}
	waitPTYTextSince(t, got, mark, "Corps de la requête #1 modifié", 3*time.Second)
	if b, err := sh.requestBuilderFor(mustCapture(t, hist, 1)); err != nil {
		t.Fatal(err)
	} else if gotBody := string(b.Body()); gotBody != original+"X" {
		t.Fatalf("insertion sauvegardée = %q, attendu %q", gotBody, original+"X")
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

func TestPTYJSONEditorLongContentScrollsAndSavesExactly(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	master, slave := openPTY(t)
	defer master.Close()
	defer slave.Close()

	hist := mint.NewHistory(mint.DefaultHistoryConfig())
	mgr, err := mint.NewManager(mint.DefaultConfig(), hist)
	if err != nil {
		t.Fatal(err)
	}
	var doc strings.Builder
	doc.WriteString("{\r\n")
	longValue := strings.Repeat("valeur-longue-éàü ", 30)
	for i := 0; i < 90; i++ {
		if i > 0 {
			doc.WriteString(",\r\n")
		}
		fmt.Fprintf(&doc, "\t\"champ%02d\": \"%s\"", i, longValue)
	}
	doc.WriteString("\r\n}\r\n")
	original := doc.String()
	hist.AddRequest(mint.BridgeEvent{
		Type: "request", FlowID: "long-json", Timestamp: time.Now(),
		Method: "POST", URL: "http://example.test/json",
		Body: []byte(original), BodySize: int64(len(original)),
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
	waitPTYLine(t, got, testPrompt, 3*time.Second)

	if _, err := master.Write([]byte("crl.json set 1 edit\r")); err != nil {
		t.Fatal(err)
	}
	waitPTYText(t, got, "CRL JSON EDITOR", 3*time.Second)
	waitPTYText(t, got, "Ctrl+S : valider", 3*time.Second)
	screen := screenFrom(got.String())
	assertSingleEditorFrame(t, screen, "CRL JSON EDITOR")
	if n := strings.Count(screen.text(), "~"); n > int(textAreaMaxRows) {
		t.Fatalf("trop de lignes de remplissage (%d) :\n%s", n, screen.text())
	}

	// Défilement vertical et horizontal, avec de nombreuses frappes sans
	// modification du document.
	for i := 0; i < 50; i++ {
		keys := []string{"\x1b[A", "\x1b[A", "\x1b[D", "\x1b[B", "\x1b[H", "\x1b[F"}
		if _, err := master.Write([]byte(keys[i%len(keys)])); err != nil {
			t.Fatal(err)
		}
		time.Sleep(3 * time.Millisecond)
	}
	waitEditorIdle(t, got, 3*time.Second)
	screen = screenFrom(got.String())
	assertSingleEditorFrame(t, screen, "CRL JSON EDITOR")

	// Un redimensionnement doit reconstruire un seul cadre à la nouvelle largeur.
	setPTYSize(t, slave, 24, 70)
	resizeMark := len(got.String())
	if _, err := master.Write([]byte("\x1b[D")); err != nil {
		t.Fatal(err)
	}
	waitPTYTextSince(t, got, resizeMark, "CRL JSON EDITOR", 3*time.Second)
	waitPTYTextSince(t, got, resizeMark, strings.Repeat("─", 68), 3*time.Second)
	waitEditorIdle(t, got, 3*time.Second)
	// On rejoue uniquement le dernier cadre complet. Cela évite qu'un reliquat
	// du cadre précédent, encore en cours de lecture, ne fasse défiler le cadre
	// redimensionné dans l'émulateur VT.
	latest := got.String()
	frameStart := strings.LastIndex(latest, "\033[J")
	if frameStart >= 0 {
		frameStart += len("\033[J")
	} else {
		frameStart = 0
	}
	resizedScreen := newVTScreen(70, 24)
	resizedScreen.feed(latest[frameStart:])
	assertSingleEditorFrame(t, resizedScreen, "CRL JSON EDITOR")
	topWidth := 0
	for _, line := range strings.Split(resizedScreen.text(), "\n") {
		if strings.HasPrefix(line, "╭") && strings.Contains(line, "─") {
			topWidth = utf8.RuneCountInString(line)
			break
		}
	}
	if topWidth != 70 {
		t.Fatalf("largeur du cadre après redimensionnement = %d, attendu 70 :\n%s", topWidth, resizedScreen.text())
	}

	if _, err := master.Write([]byte("\x13")); err != nil {
		t.Fatal(err)
	}
	waitPTYText(t, got, "Corps de la requête #1 modifié", 3*time.Second)
	waitPTYLine(t, got, testPrompt, 3*time.Second)
	if b, err := sh.requestBuilderFor(mustCapture(t, hist, 1)); err != nil {
		t.Fatal(err)
	} else if string(b.Body()) != original {
		t.Fatalf("contenu JSON enregistré différent de l'original (%d octets contre %d)", len(b.Body()), len(original))
	}
	if strings.Contains(screenFrom(got.String()).text(), "CRL JSON EDITOR") {
		t.Fatalf("cadre non effacé après validation :\n%s", screenFrom(got.String()).text())
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
