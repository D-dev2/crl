package shell

import (
	"bytes"
	"strings"
	"testing"

	"crl-cli/internal/termstyle"
)

func TestColorDisabledByDefaultOnBuffer(t *testing.T) {
	var out bytes.Buffer
	if colorEnabled(&out) {
		t.Fatal("les buffers ne doivent pas activer les couleurs")
	}
	sh, buf, _, _, _ := newTestShell(t, "")
	if sh.pal.Enabled {
		t.Fatal("le shell de test ne doit pas activer les couleurs")
	}
	sh.errorf("exemple")
	if !strings.Contains(buf.String(), "[CRL] Erreur : exemple") || strings.Contains(buf.String(), "\033[") {
		t.Fatalf("sortie sans couleur inattendue : %q", buf.String())
	}
}

func TestPaletteAndPromptColors(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	p := palette{Enabled: true}
	for name, got := range map[string]string{
		"method":  p.Method("DELETE"),
		"status":  p.StatusCode(503, "503"),
		"success": p.BoldGreen("ok"),
		"error":   p.BoldRed("ko"),
	} {
		if !strings.Contains(got, "\033[") || !strings.HasSuffix(got, termstyle.Reset) {
			t.Fatalf("style %s non coloré : %q", name, got)
		}
	}
	statuses := map[int]string{
		200: "\033[1;32m",
		301: "\033[1;36m",
		404: "\033[1;33m",
		500: "\033[1;31m",
	}
	for code, prefix := range statuses {
		if got := p.StatusCode(code, "x"); !strings.HasPrefix(got, prefix) {
			t.Fatalf("statut %d = %q, préfixe attendu %q", code, got, prefix)
		}
	}

	sh, _, _, _, _ := newTestShell(t, "")
	sh.pal.Enabled = true
	sh.lastFailed = false
	if got := sh.promptText(); !strings.Contains(got, "crl") || !strings.Contains(got, ">") {
		t.Fatalf("invite normale = %q", got)
	}
	sh.lastFailed = true
	if got := sh.promptText(); !strings.Contains(got, "crl") || !strings.Contains(got, ">") {
		t.Fatalf("invite d'erreur = %q", got)
	}

	sh.pal.Enabled = false
	sh.lastFailed = true
	if got := sh.promptText(); !strings.Contains(got, "crl") || strings.Contains(got, "\033[") {
		t.Fatalf("invite sans couleur = %q", got)
	}
}
