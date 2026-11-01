package request

import "testing"

func TestFollowableRedirectStatuses(t *testing.T) {
	for _, code := range []int{301, 302, 303, 307, 308} {
		if !FollowableRedirect(code) {
			t.Errorf("FollowableRedirect(%d) = false", code)
		}
	}
	for _, code := range []int{300, 304, 305, 306, 309, 200, 400} {
		if FollowableRedirect(code) {
			t.Errorf("FollowableRedirect(%d) = true", code)
		}
	}
}

func TestRedirectMethodBody(t *testing.T) {
	cases := []struct {
		code       int
		method     string
		wantMethod string
		wantBody   bool
	}{
		{301, "POST", "GET", false},
		{301, "PUT", "GET", false},
		{301, "GET", "GET", true},
		{301, "HEAD", "HEAD", true},
		{302, "POST", "GET", false},
		{302, "PATCH", "GET", false},
		{303, "POST", "GET", false},
		{303, "HEAD", "HEAD", false},
		{307, "POST", "POST", true},
		{307, "PUT", "PUT", true},
		{308, "POST", "POST", true},
		{308, "PATCH", "PATCH", true},
	}
	for _, tc := range cases {
		method, keepBody := RedirectMethodBody(tc.code, tc.method)
		if method != tc.wantMethod || keepBody != tc.wantBody {
			t.Errorf("RedirectMethodBody(%d, %s) = (%s, %v), want (%s, %v)",
				tc.code, tc.method, method, keepBody, tc.wantMethod, tc.wantBody)
		}
	}
}

func TestResolveRedirect(t *testing.T) {
	cases := []struct {
		base     string
		location string
		want     string
	}{
		{"http://example.test/admin/", "/user/login", "http://example.test/user/login"},
		{"http://example.test/admin/page", "next", "http://example.test/admin/next"},
		{"https://example.test/a/b?x=1", "../c?y=2", "https://example.test/c?y=2"},
		{"http://example.test/a", "https://other.test/z", "https://other.test/z"},
	}
	for _, tc := range cases {
		got, err := ResolveRedirect(tc.base, tc.location)
		if err != nil {
			t.Fatalf("ResolveRedirect(%q, %q) : %v", tc.base, tc.location, err)
		}
		if got != tc.want {
			t.Errorf("ResolveRedirect(%q, %q) = %q, want %q", tc.base, tc.location, got, tc.want)
		}
	}

	for _, location := range []string{"", "javascript:alert(1)", "ftp://example.test/file"} {
		if _, err := ResolveRedirect("http://example.test/", location); err == nil {
			t.Errorf("ResolveRedirect a accepté %q", location)
		}
	}
}

func TestSameOrigin(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"http://example.test/a", "http://example.test:80/b", true},
		{"https://example.test/a", "https://example.test:443/b", true},
		{"http://example.test/a", "https://example.test/b", false},
		{"http://example.test/a", "http://example.test:8080/b", false},
		{"http://example.test/a", "http://other.test/b", false},
		{"http://127.0.0.1/a", "http://127.0.0.1/b", true},
	}
	for _, tc := range cases {
		if got := SameOrigin(tc.a, tc.b); got != tc.want {
			t.Errorf("SameOrigin(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestRedirectKeyIgnoresFragmentAndDefaultPort(t *testing.T) {
	if got, want := RedirectKey("HTTP://Example.TEST:80/a?x=1#frag"), RedirectKey("http://example.test/a?x=1"); got != want {
		t.Fatalf("RedirectKey = %q, want %q", got, want)
	}
	if RedirectKey("http://example.test/a?x=1") == RedirectKey("http://example.test/a?x=2") {
		t.Fatal("les requêtes différentes ne doivent pas être considérées comme une boucle")
	}
}

func TestStripCrossOriginCredentials(t *testing.T) {
	b := New()
	if err := b.SetURL("http://example.test/"); err != nil {
		t.Fatal(err)
	}
	for _, h := range []Header{
		{Name: "Authorization", Value: "Bearer secret"},
		{Name: "Proxy-Authorization", Value: "Basic secret"},
		{Name: "Cookie", Value: "session=secret"},
		{Name: "Cookie2", Value: "old=1"},
		{Name: "X-Api-Key", Value: "secret"},
		{Name: "X-Test", Value: "kept"},
		{Name: "Content-Type", Value: "application/json"},
	} {
		if err := b.AddHeader(h.Name, h.Value); err != nil {
			t.Fatal(err)
		}
	}
	StripCrossOriginCredentials(b)
	for _, h := range b.Headers() {
		switch h.Name {
		case "Authorization", "Proxy-Authorization", "Cookie", "Cookie2", "X-Api-Key":
			t.Errorf("%s n'a pas été supprimé", h.Name)
		}
	}
	if len(b.Headers()) != 2 {
		t.Fatalf("en-têtes restants = %#v", b.Headers())
	}
	if got := b.Headers()[0]; got.Name != "X-Test" || got.Value != "kept" {
		t.Fatalf("en-tête non sensible modifié : %#v", got)
	}
}

func TestDropBodyHeaders(t *testing.T) {
	b := New()
	for _, h := range []Header{
		{Name: "Content-Type", Value: "application/json"},
		{Name: "Content-Encoding", Value: "gzip"},
		{Name: "Content-Language", Value: "fr"},
		{Name: "Content-Length", Value: "10"},
		{Name: "Transfer-Encoding", Value: "chunked"},
		{Name: "X-Test", Value: "kept"},
	} {
		if err := b.AddHeader(h.Name, h.Value); err != nil {
			t.Fatal(err)
		}
	}
	DropBodyHeaders(b)
	if len(b.Headers()) != 1 || b.Headers()[0].Name != "X-Test" {
		t.Fatalf("en-têtes restants = %#v", b.Headers())
	}
}
