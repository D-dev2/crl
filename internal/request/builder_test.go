package request

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetURLPreservesRawQuery(t *testing.T) {
	b := New()
	raw := "http://127.0.0.1:5002/a%2Fb?x=1&x=2&empty=&enc=%2F%3F"
	if err := b.SetURL(raw); err != nil {
		t.Fatal(err)
	}
	if b.URL() != raw {
		t.Fatalf("URL = %q, want %q", b.URL(), raw)
	}
}

func TestSetURLAddsScheme(t *testing.T) {
	b := New()
	if err := b.SetURL("127.0.0.1:5002/users"); err != nil {
		t.Fatal(err)
	}
	if b.URL() != "http://127.0.0.1:5002/users" {
		t.Fatalf("URL = %q", b.URL())
	}
}

func TestSetURLRejectsInvalid(t *testing.T) {
	for _, raw := range []string{"", "ftp://example.test/x", "http:///nohost", "http://a b/"} {
		b := New()
		if err := b.SetURL(raw); err == nil {
			t.Fatalf("SetURL(%q) should fail", raw)
		}
	}
}

func TestHeadersReplaceAndDuplicatePreservationOnImport(t *testing.T) {
	b := New()
	if err := b.AddHeader("X-Test", "one"); err != nil {
		t.Fatal(err)
	}
	if err := b.AddHeader("x-test", "two"); err != nil {
		t.Fatal(err)
	}
	headers := b.Headers()
	if len(headers) != 1 || headers[0].Value != "two" {
		t.Fatalf("headers = %#v", headers)
	}

	captured := CapturedRequest{
		URL:    "http://example.test/a?b=1",
		Method: "POST",
		Headers: []Header{
			{Name: "Cookie", Value: "a=1; session=first"},
			{Name: "Cookie", Value: "b=2 "},
			{Name: "Authorization", Value: "Bearer exact-token"},
			{Name: "Content-Type", Value: "application/json"},
		},
		Body:        []byte(`{"a":1}`),
		BodyPresent: true,
	}
	if err := b.Import(captured); err != nil {
		t.Fatal(err)
	}
	headers = b.Headers()
	if len(headers) != 4 || headers[0].Value != "a=1; session=first" || headers[1].Value != "b=2 " || headers[2].Value != "Bearer exact-token" {
		t.Fatalf("duplicate headers lost: %#v", headers)
	}
	if string(b.Body()) != `{"a":1}` {
		t.Fatalf("body = %q", b.Body())
	}
}

func TestImportRefusesTruncatedRequest(t *testing.T) {
	b := New()
	err := b.Import(CapturedRequest{
		URL:           "http://example.test/",
		Method:        "POST",
		BodyTruncated: true,
	})
	if err == nil || !strings.Contains(err.Error(), "tronqué") {
		t.Fatalf("expected truncation error, got %v", err)
	}
}

func TestImportPreservesExactBody(t *testing.T) {
	b := New()
	body := []byte("line1\r\nline2\x00binary")
	captured := CapturedRequest{
		URL:         "http://example.test/upload?x=%2F",
		Method:      "PUT",
		Body:        body,
		BodyPresent: true,
	}
	if err := b.Import(captured); err != nil {
		t.Fatal(err)
	}
	got := b.Body()
	if len(got) != len(body) || string(got) != string(body) {
		t.Fatalf("body altered: %q", got)
	}
}

func TestProxyPriority(t *testing.T) {
	b := New()

	res := b.ResolveProxy("http://127.0.0.1:8080", true)
	if res.URL != "http://127.0.0.1:8080" || res.None || !strings.Contains(res.Source, "Mint") {
		t.Fatalf("automatic mint proxy = %#v", res)
	}

	if err := b.SetProxy("http://explicit.test:3128"); err != nil {
		t.Fatal(err)
	}
	res = b.ResolveProxy("http://127.0.0.1:8080", true)
	if res.URL != "http://explicit.test:3128" || !strings.Contains(res.Source, "explicite") {
		t.Fatalf("explicit proxy should win: %#v", res)
	}

	if err := b.SetProxy("none"); err != nil {
		t.Fatal(err)
	}
	res = b.ResolveProxy("http://127.0.0.1:8080", true)
	if !res.None {
		t.Fatalf("explicit none should win: %#v", res)
	}

	b.ClearProxy()
	res = b.ResolveProxy("", false)
	if res.URL != "" || res.None {
		t.Fatalf("no proxy expected: %#v", res)
	}
}

func TestCurlArgsFidelity(t *testing.T) {
	b := New()
	if err := b.SetURL("http://127.0.0.1:5002/a%2Fb?x=1&x=2&empty="); err != nil {
		t.Fatal(err)
	}
	if err := b.SetMethod("post"); err != nil {
		t.Fatal(err)
	}
	if err := b.AddHeader("Content-Length", "999"); err != nil {
		t.Fatal(err)
	}
	if err := b.AddHeader("X-Test", "demo"); err != nil {
		t.Fatal(err)
	}
	b.SetBodyText("hello")

	args, err := b.CurlArgs(ProxyResolution{URL: "http://127.0.0.1:8080"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, "\x00")
	for _, want := range []string{
		"--globoff", "--path-as-is", "--proxy\x00http://127.0.0.1:8080", "--noproxy\x00",
		"-X\x00POST", "-H\x00X-Test: demo", "-H\x00Content-Type:", "--data-binary\x00@-",
		"http://127.0.0.1:5002/a%2Fb?x=1&x=2&empty=",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("curl args missing %q in %#v", want, args)
		}
	}
	if strings.Contains(joined, "Content-Length") {
		t.Fatalf("Content-Length should be skipped: %#v", args)
	}
}

func TestCurlArgsNoProxy(t *testing.T) {
	b := New()
	if err := b.SetURL("http://example.test/"); err != nil {
		t.Fatal(err)
	}
	args, err := b.CurlArgs(ProxyResolution{None: true})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--noproxy *") {
		t.Fatalf("missing --noproxy *: %s", joined)
	}
}

func TestParseHeaderLine(t *testing.T) {
	name, value, err := ParseHeaderLine("X-Test: a:b")
	if err != nil || name != "X-Test" || value != "a:b" {
		t.Fatalf("ParseHeaderLine = %q %q %v", name, value, err)
	}
	if _, _, err := ParseHeaderLine("invalid"); err == nil {
		t.Fatal("expected error")
	}
}

func TestJSONValidation(t *testing.T) {
	b := New()
	if err := b.SetJSON(`{"a":`); err == nil {
		t.Fatal("invalid JSON accepted")
	}
	if err := b.SetJSON(`{"a":1}`); err != nil {
		t.Fatal(err)
	}
	if b.ContentType() != "application/json" {
		t.Fatalf("Content-Type = %q", b.ContentType())
	}
}

func TestResetAndIsEmpty(t *testing.T) {
	b := New()
	if !b.IsEmpty() {
		t.Fatal("new builder should be empty")
	}
	_ = b.SetURL("http://example.test/")
	_ = b.AddHeader("X-Test", "1")
	b.SetBodyText("x")
	if b.IsEmpty() {
		t.Fatal("builder should not be empty")
	}
	b.Reset()
	if !b.IsEmpty() || b.URL() != "" || b.BodySet() {
		t.Fatalf("reset failed: %#v", b)
	}
}

func TestCAFileKeepsTLSVerification(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(path, []byte("-----BEGIN CERTIFICATE-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	b := New()
	if err := b.SetURL("https://example.test/secure"); err != nil {
		t.Fatal(err)
	}
	if err := b.SetCAFile(path); err != nil {
		t.Fatal(err)
	}
	args, err := b.CurlArgs(ProxyResolution{})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, "\x00")
	if !strings.Contains(joined, "--cacert\x00"+path) {
		t.Fatalf("--cacert absent : %#v", args)
	}
	for _, forbidden := range []string{"-k", "--insecure"} {
		for _, arg := range args {
			if arg == forbidden {
				t.Fatalf("vérification TLS désactivée par %q", forbidden)
			}
		}
	}

	b.ClearCAFile()
	args, _ = b.CurlArgs(ProxyResolution{})
	if strings.Contains(strings.Join(args, " "), "--cacert") {
		t.Fatalf("CA encore présente après effacement : %#v", args)
	}
	if err := b.SetCAFile(filepath.Join(dir, "absent.pem")); err == nil {
		t.Fatal("fichier CA absent accepté")
	}
	if err := b.SetCAFile(dir); err == nil {
		t.Fatal("répertoire accepté comme CA")
	}
}
