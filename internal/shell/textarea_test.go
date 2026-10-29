package shell

import (
	"bytes"
	"strings"
	"testing"
)

func TestTextAreaMultilineCursorInsertAndDelete(t *testing.T) {
	a := newTextArea(&bytes.Buffer{}, "JSON", "actif", []byte("abc\ndef"))
	a.Feed([]byte("\x1b[A")) // haut, conserve la colonne 3
	a.Feed([]byte("\x1b[D")) // avant le c
	a.Feed([]byte("X"))
	if got, want := a.Text(), "abXc\ndef"; got != want {
		t.Fatalf("insertion au curseur = %q, attendu %q", got, want)
	}
	a.Feed([]byte("\x1b[3~"))
	if got, want := a.Text(), "abX\ndef"; got != want {
		t.Fatalf("suppression au curseur = %q, attendu %q", got, want)
	}
	a.Feed([]byte("\x7f"))
	if got, want := a.Text(), "ab\ndef"; got != want {
		t.Fatalf("retour arrière = %q, attendu %q", got, want)
	}
}

func TestTextAreaEscapeSequenceMayArriveInChunks(t *testing.T) {
	a := newTextArea(&bytes.Buffer{}, "JSON", "actif", []byte("ab"))
	a.Feed([]byte("\x1b["))
	a.Feed([]byte("D"))
	a.Feed([]byte("X"))
	if got, want := a.Text(), "aXb"; got != want {
		t.Fatalf("séquence flèche découpée : %q, attendu %q", got, want)
	}
	if a.EscapePending() {
		t.Fatal("séquence complète toujours en attente")
	}
}

func TestTextAreaNaturalNewlinesAndVerticalScroll(t *testing.T) {
	a := newTextArea(&bytes.Buffer{}, "JSON", "actif", nil)
	a.Feed([]byte("première\rdeuxième"))
	if got, want := a.Text(), "première\ndeuxième"; got != want {
		t.Fatalf("retour naturel = %q, attendu %q", got, want)
	}
	var out bytes.Buffer
	a = newTextArea(&out, "JSON", "actif", []byte(strings.Repeat("x\n", 12)+"fin"))
	a.SetSize(80, 14)
	a.Draw(palette{}, false)
	if a.top == 0 || a.top+a.rows <= a.cursorPositionRow() {
		t.Fatalf("zone de défilement top=%d rows=%d curseur=%d", a.top, a.rows, a.cursorPositionRow())
	}
	if !strings.Contains(out.String(), "Ctrl+S : valider") {
		t.Fatalf("panneau d'édition absent: %q", out.String())
	}
}

func (a *textArea) cursorPositionRow() int { row, _ := a.cursorPosition(); return row }

func TestTextAreaEscapeAndValidationPreserveOriginal(t *testing.T) {
	sh, _, _, _, b := newTestShell(t, "")
	if err := b.SetJSON(`{"role":"user"}`); err != nil {
		t.Fatal(err)
	}
	a := newTextArea(sh.out, "JSON", "actif", []byte(`{"role":}`))
	sh.textArea, sh.textKind, sh.textBuilder = a, "json", b
	sh.commitTextArea()
	if got := string(b.Body()); got != `{"role":"user"}` {
		t.Fatalf("JSON invalide appliqué : %s", got)
	}
	if sh.textArea == nil || !strings.Contains(sh.textArea.message, "ligne 1 colonne") || sh.textArea.Text() != `{"role":}` {
		t.Fatalf("texte invalide non conservé dans l'éditeur: %#v", sh.textArea)
	}
	sh.textArea.Feed([]byte("\x7f"))
	sh.textArea.Feed([]byte(`"admin"}`))
	sh.commitTextArea()
	if got := string(b.Body()); got != `{"role":"admin"}` {
		t.Fatalf("JSON corrigé non appliqué : %s", got)
	}
	a = newTextArea(sh.out, "JSON", "actif", []byte(`{"role":"ignored"}`))
	sh.textArea, sh.textKind, sh.textBuilder = a, "json", b
	sh.closeTextArea(false)
	if got := string(b.Body()); got != `{"role":"admin"}` {
		t.Fatalf("annulation a modifié le corps : %s", got)
	}
}

func TestValidateJSONReportsLineAndColumn(t *testing.T) {
	err := validateJSON([]byte("{\n  \"name\": ,\n}"))
	if err == nil || !strings.Contains(err.Error(), "ligne 2 colonne") {
		t.Fatalf("erreur de position = %v", err)
	}
}
