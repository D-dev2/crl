package shell

import (
	"bytes"
	"strings"
	"testing"
)

func newTestEditor(t *testing.T) (*editor, *bytes.Buffer) {
	t.Helper()
	var out bytes.Buffer
	return newEditor(&out, "crl > "), &out
}

func TestEditorTypingBackspaceAndSubmit(t *testing.T) {
	e, out := newTestEditor(t)
	e.DrawPrompt()
	e.Feed([]byte("crl.ho"))
	e.Feed([]byte("\x7fost"))
	if got := e.Text(); got != "crl.host" {
		t.Fatalf("texte = %q", got)
	}
	res := e.Feed([]byte("\r"))
	if len(res.Lines) != 1 || res.Lines[0] != "crl.host" {
		t.Fatalf("lignes = %#v", res.Lines)
	}
	if e.Text() != "" {
		t.Fatalf("tampon non vidé : %q", e.Text())
	}
	if !strings.Contains(out.String(), "crl > crl.host") {
		t.Fatalf("redessin absent :\n%q", out.String())
	}
}

func TestEditorLongPasteWritesPromptOnce(t *testing.T) {
	e, out := newTestEditor(t)
	e.DrawPrompt()
	out.Reset()

	paste := strings.Repeat("eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.", 30)
	e.Feed([]byte(paste))
	if got := e.Text(); got != paste {
		t.Fatalf("collage tronqué : %d caractères au lieu de %d", len(got), len(paste))
	}
	if got := out.String(); got != paste {
		t.Fatalf("le collage ne doit être écrit qu'une fois, sans prompt :\n%q", got)
	}
}

func TestEditorUTF8SplitAcrossReads(t *testing.T) {
	e, _ := newTestEditor(t)
	e.Feed([]byte("crl.host "))
	e.Feed([]byte{0xc3})
	if got := e.Text(); got != "crl.host " {
		t.Fatalf("octet UTF-8 incomplet affiché trop tôt : %q", got)
	}
	e.Feed([]byte{0xa9})
	if got := e.Text(); got != "crl.host é" {
		t.Fatalf("caractère UTF-8 incomplet : %q", got)
	}
	e.Feed([]byte{0xff})
	if got := e.Text(); got != "crl.host é" {
		t.Fatalf("octet invalide accepté : %q", got)
	}
}

func TestEditorCursorInsertionAndSplitEscape(t *testing.T) {
	e, _ := newTestEditor(t)
	e.Feed([]byte("ac"))
	e.Feed([]byte("\x1b["))
	e.Feed([]byte("D"))
	e.Feed([]byte("b"))
	if got := e.Text(); got != "abc" {
		t.Fatalf("texte après insertion = %q", got)
	}
	e.Feed([]byte("\x05")) // Ctrl-E
	e.Feed([]byte("d"))
	if got := e.Text(); got != "abcd" {
		t.Fatalf("texte après Ctrl-E = %q", got)
	}
	e.Feed([]byte("\x1b[3~")) // Suppr en fin de ligne : sans effet
	if got := e.Text(); got != "abcd" {
		t.Fatalf("suppression inattendue = %q", got)
	}
	e.Feed([]byte("\x01")) // Ctrl-A
	e.Feed([]byte("\x04")) // Ctrl-D supprime le caractère sous le curseur
	if got := e.Text(); got != "bcd" {
		t.Fatalf("texte après Ctrl-D = %q", got)
	}
}

func TestEditorHistoryNavigation(t *testing.T) {
	e, _ := newTestEditor(t)
	e.Feed([]byte("first\r"))
	e.Feed([]byte("second\r"))

	e.Feed([]byte("\x1b[A"))
	if got := e.Text(); got != "second" {
		t.Fatalf("premier haut = %q", got)
	}
	e.Feed([]byte("\x1b[A"))
	if got := e.Text(); got != "first" {
		t.Fatalf("deuxième haut = %q", got)
	}
	e.Feed([]byte("\x1b[A"))
	if got := e.Text(); got != "first" {
		t.Fatalf("dépassement haut = %q", got)
	}
	e.Feed([]byte("\x1b[B"))
	if got := e.Text(); got != "second" {
		t.Fatalf("premier bas = %q", got)
	}
	e.Feed([]byte("\x1b[B"))
	if got := e.Text(); got != "" {
		t.Fatalf("retour à la saisie vide = %q", got)
	}

	e.Feed([]byte("partiel"))
	e.Feed([]byte("\x1b[A"))
	if got := e.Text(); got != "second" {
		t.Fatalf("saisie partielle non sauvegardée : %q", got)
	}
	e.Feed([]byte("\x1b[B"))
	if got := e.Text(); got != "partiel" {
		t.Fatalf("saisie partielle non restaurée : %q", got)
	}
	if len(e.history) != 2 {
		t.Fatalf("historique = %#v", e.history)
	}
}

func TestEditorCtrlCAndCtrlD(t *testing.T) {
	e, out := newTestEditor(t)
	e.Feed([]byte("abandon"))
	res := e.Feed([]byte("\x03"))
	if !res.Interrupted || len(res.Lines) != 0 {
		t.Fatalf("résultat Ctrl-C = %#v", res)
	}
	if e.Text() != "" {
		t.Fatalf("tampon après Ctrl-C = %q", e.Text())
	}
	if len(e.history) != 0 {
		t.Fatalf("Ctrl-C a ajouté à l'historique : %#v", e.history)
	}
	if !strings.Contains(out.String(), "^C") {
		t.Fatalf("écho Ctrl-C absent :\n%q", out.String())
	}

	empty, _ := newTestEditor(t)
	res = empty.Feed([]byte("\x04"))
	if !res.EOF || len(res.Lines) != 0 {
		t.Fatalf("résultat Ctrl-D sur ligne vide = %#v", res)
	}

	nonEmpty, _ := newTestEditor(t)
	nonEmpty.Feed([]byte("x"))
	res = nonEmpty.Feed([]byte("\x04"))
	if res.EOF {
		t.Fatal("Ctrl-D en fin de ligne ne doit pas terminer le shell")
	}
	if nonEmpty.Text() != "x" {
		t.Fatalf("texte modifié par Ctrl-D en fin de ligne = %q", nonEmpty.Text())
	}
}

func TestEditorNotifyRedrawsWithoutLosingInput(t *testing.T) {
	e, out := newTestEditor(t)
	e.DrawPrompt()
	e.Feed([]byte("crl.sho"))
	out.Reset()

	e.Notify([]string{"[Mint] #1 GET /x", "seconde ligne\x00"})
	text := out.String()
	if !strings.Contains(text, "[Mint] #1 GET /x") {
		t.Fatalf("notification absente :\n%q", text)
	}
	if strings.Contains(text, "\x00") {
		t.Fatalf("caractère de contrôle non filtré :\n%q", text)
	}
	if !strings.Contains(text, "crl > crl.sho") {
		t.Fatalf("prompt non redessiné :\n%q", text)
	}
	if e.Text() != "crl.sho" {
		t.Fatalf("saisie perdue : %q", e.Text())
	}

	out.Reset()
	e.Notify(nil)
	if out.Len() != 0 {
		t.Fatalf("Notify vide a écrit :\n%q", out.String())
	}
}

func TestEditorWordAndLineEditing(t *testing.T) {
	e, _ := newTestEditor(t)
	e.Feed([]byte("one two three"))
	e.Feed([]byte("\x17")) // Ctrl-W
	if got := e.Text(); got != "one two " {
		t.Fatalf("texte après Ctrl-W = %q", got)
	}
	e.Feed([]byte("\x15")) // Ctrl-U
	if got := e.Text(); got != "" {
		t.Fatalf("texte après Ctrl-U = %q", got)
	}

	e.Feed([]byte("abcdef"))
	e.Feed([]byte("\x01")) // Ctrl-A
	e.Feed([]byte("\x06")) // Ctrl-F
	e.Feed([]byte("\x06")) // Ctrl-F
	e.Feed([]byte("\x0b")) // Ctrl-K
	if got := e.Text(); got != "ab" {
		t.Fatalf("texte après Ctrl-K = %q", got)
	}

	e.Feed([]byte("\x0c")) // Ctrl-L
	if got := e.Text(); got != "ab" {
		t.Fatalf("texte perdu après Ctrl-L = %q", got)
	}
}

func TestSanitizeTerminal(t *testing.T) {
	got := sanitizeTerminal("a\nb\r\x00c\td\x1b[31m")
	if strings.ContainsAny(got, "\n\r\x00\t\x1b") {
		t.Fatalf("caractères de contrôle restants : %q", got)
	}
	if !strings.Contains(got, "a b") {
		t.Fatalf("saut de ligne non remplacé : %q", got)
	}
}

func TestSanitizeTerminalKeepANSI(t *testing.T) {
	got := sanitizeTerminalKeepANSI("\x1b[1;35m[Mint]\x1b[0m #1\x00\nGET\x1b[2J")
	want := "\x1b[1;35m[Mint]\x1b[0m #1 GET"
	if got != want {
		t.Fatalf("sanitizeTerminalKeepANSI = %q, want %q", got, want)
	}
}
