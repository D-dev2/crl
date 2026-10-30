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

func TestTextAreaFrameIsCompactAndRedrawClearsPreviousFrame(t *testing.T) {
	var out bytes.Buffer
	a := newTextArea(&out, "JSON", "actif", []byte(`{}`))
	a.SetSize(100, 24)
	if a.rows != textAreaMinRows {
		t.Fatalf("hauteur pour un JSON court = %d, attendu %d", a.rows, textAreaMinRows)
	}
	a.Draw(palette{}, false)
	if got := strings.Count(out.String(), "~"); got > 2 {
		t.Fatalf("cadre compact avec %d lignes ~ : %q", got, out.String())
	}

	out.Reset()
	a.Feed([]byte("x"))
	a.Draw(palette{}, false)
	redraw := out.String()
	if !strings.Contains(redraw, "\033[J") || strings.Contains(redraw, "\033[u") {
		t.Fatalf("le redessin ne remplace pas le panneau précédent : %q", redraw)
	}
	if !strings.Contains(redraw, "Ctrl+S : valider") {
		t.Fatalf("redessin incomplet : %q", redraw)
	}

	long := newTextArea(&bytes.Buffer{}, "JSON", "actif", []byte(strings.Repeat("x\n", 100)))
	long.SetSize(100, 24)
	if long.rows < textAreaMinRows || long.rows > long.termHeight-textAreaFrameExtra || long.rows > textAreaMaxRows {
		t.Fatalf("hauteur non bornée : %d lignes pour un terminal de %d", long.rows, long.termHeight)
	}
	small := newTextArea(&bytes.Buffer{}, "JSON", "actif", []byte(`{}`))
	small.SetSize(20, 10)
	if small.width != 18 {
		t.Fatalf("largeur pour un terminal de 20 colonnes = %d", small.width)
	}
	small.Draw(palette{}, false)
}

func TestTextAreaRendersControlCharactersWithoutTerminalSideEffects(t *testing.T) {
	original := "entête\r\n\tligne accentuée éàü \x1b[31mrouge\x00fin\rdernière"
	var out bytes.Buffer
	a := newTextArea(&out, "BODY", "test", []byte(original))
	a.SetSize(70, 20)
	a.Draw(palette{}, false)
	got := out.String()

	if a.Text() != original {
		t.Fatalf("l'affichage a modifié le contenu : %q, attendu %q", a.Text(), original)
	}
	if strings.Contains(got, "\x00") || strings.Contains(got, "\t") {
		t.Fatalf("caractère de contrôle brut dans le panneau : %q", got)
	}
	if strings.Contains(got, "\x1b[31m") {
		t.Fatalf("séquence ANSI du contenu interprétée par le terminal : %q", got)
	}
	for i := 0; i < len(got); i++ {
		if got[i] != '\r' {
			continue
		}
		if i == 0 || (i+1 < len(got) && (got[i+1] == '\n' || got[i+1] == '\x1b')) {
			continue
		}
		t.Fatalf("retour chariot brut hors fin de ligne à l'offset %d : %q", i, got)
	}
	if !strings.Contains(got, "␍") || !strings.Contains(got, "·") {
		t.Fatalf("les contrôles ne sont pas rendus de façon visible : %q", got)
	}
}

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
