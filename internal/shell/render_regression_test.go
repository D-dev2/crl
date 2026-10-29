package shell

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
	"time"

	"crl-cli/internal/mint"
)

func TestTrackingWriterBlockNormalizesSpacing(t *testing.T) {
	var out bytes.Buffer
	w := &trackingWriter{w: &out}
	w.beginBlock()
	if _, err := w.Write([]byte("\n[CRL] résultat\n")); err != nil {
		t.Fatal(err)
	}
	w.finishBlock()
	if got, want := out.String(), "\r\n[CRL] résultat\n\r\n"; got != want {
		t.Fatalf("sortie commande = %q, attendu %q", got, want)
	}

	out.Reset()
	w.beginBlock()
	w.finishBlock()
	if out.Len() != 0 {
		t.Fatalf("une commande silencieuse a produit %q", out.String())
	}
}

func TestMintReplayRendersEmptyErrorAndLargeBodies(t *testing.T) {
	sh, out, _, hist, _ := newTestShell(t, "")
	srv := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/empty":
			w.WriteHeader(http.StatusNoContent)
		case "/error":
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("panne-serveur"))
		case "/large":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write(bytes.Repeat([]byte("x"), 40<<10))
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer srv.Close()

	longURL := srv.URL + "/long?" + strings.Repeat("segment=abcdefghij&", 18) + "fin=1"
	for i, rawURL := range []string{srv.URL + "/empty", srv.URL + "/error", srv.URL + "/large", longURL} {
		hist.AddRequest(mint.BridgeEvent{
			Type: "request", FlowID: string(rune('a' + i)), Timestamp: time.Now(),
			Method: "GET", URL: rawURL,
		})
	}

	sh.Execute("mint.select 1 2 3 4")
	sh.Execute("mint.replay")
	text := out.String()
	for _, want := range []string{
		"Résultats du rejeu parallèle",
		"204 No Content",
		"(corps vide)",
		"500 Internal Server Error",
		"panne-serveur",
		"Succès HTTP : 3",
		"Erreurs HTTP : 1",
		"Affichage limité",
		"mint.replay.body 3 32768",
		"fin=1",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("sortie de rejeu sans %q :\n%s", want, text)
		}
	}
	if strings.Contains(text, "Erreur réseau") {
		t.Fatalf("une réponse HTTP valide a été classée comme erreur réseau :\n%s", text)
	}
	// Le corps d'une 204 ne doit pas être remplacé par un contenu inventé.
	if !strings.Contains(text, "Corps — vide (0 octets)") {
		t.Fatalf("nature du corps vide absente :\n%s", text)
	}
}
