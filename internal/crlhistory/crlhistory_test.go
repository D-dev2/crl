package crlhistory

import (
	"os"
	"testing"
	"time"

	"crl-cli/internal/request"
)

func TestStoreAddListGetAndPersist(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	b := request.New()
	_ = b.SetURL("http://example.test/path?a=1&b=2")
	_ = b.SetMethod("POST")
	b.SetBody([]byte(`{"cle":"valeur"}`))
	b.AddHeader("Authorization", "Bearer secret")
	b.AddHeader("Cookie", "session=abc")

	id, err := store.Add(Execution{
		StartedAt:  time.Unix(1700000000, 0),
		Request:    FromBuilderRequest(b),
		StatusCode: 200,
		Duration:   5 * time.Millisecond,
		Response: &Message{
			Method:  "POST",
			URL:     "http://example.test/path?a=1&b=2",
			Headers: []Header{{Name: "Set-Cookie", Value: "session=abc"}},
			Body:    []byte(`{"ok":true}`),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != 1 {
		t.Fatalf("ID attendu 1, obtenu %d", id)
	}

	list := store.List()
	if len(list) != 1 || list[0].ID != 1 || list[0].StatusCode != 200 {
		t.Fatalf("liste inattendue: %+v", list)
	}
	got, ok := store.Get(1)
	if !ok || got.Request.URL != "http://example.test/path?a=1&b=2" {
		t.Fatalf("get #1 manquant ou incorrect: %+v", got)
	}
	if len(got.Request.Body) == 0 || string(got.Request.Body) != `{"cle":"valeur"}` {
		t.Fatalf("corps de requête perdu: %q", got.Request.Body)
	}
	if got.Response == nil || string(got.Response.Body) != `{"ok":true}` {
		t.Fatalf("corps de réponse perdu: %+v", got.Response)
	}
	// Les en-têtes sensibles (authentification, cookies) sont conservés tels quels.
	foundAuth := false
	foundCookie := false
	for _, h := range got.Request.Headers {
		if h.Name == "Authorization" && h.Value == "Bearer secret" {
			foundAuth = true
		}
		if h.Name == "Cookie" && h.Value == "session=abc" {
			foundCookie = true
		}
	}
	if !foundAuth || !foundCookie {
		t.Fatalf("en-têtes d'authentification/cookies perdus: %+v", got.Request.Headers)
	}

	// Réouverture : les données persistent sur disque.
	store.Close()
	store2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	list2 := store2.List()
	if len(list2) != 1 || list2[0].ID != 1 {
		t.Fatalf("persistence après réouverture ratée: %+v", list2)
	}
	got2, _ := store2.Get(1)
	if string(got2.Request.Body) != `{"cle":"valeur"}` || got2.StatusCode != 200 {
		t.Fatalf("données perdues après réouverture: %+v", got2)
	}
	store2.Close()

	// L'identifiant suivant est stable après réouverture.
	id2, err := store2Add(dir, Execution{StartedAt: time.Now(), Request: FromBuilderRequest(request.New())})
	if err != nil {
		t.Fatal(err)
	}
	if id2 != 2 {
		t.Fatalf("ID après réouverture attendu 2, obtenu %d", id2)
	}
	os.RemoveAll(dir)
}

func store2Add(dir string, ex Execution) (int, error) {
	store, err := Open(dir)
	if err != nil {
		return 0, err
	}
	defer store.Close()
	return store.Add(ex)
}
