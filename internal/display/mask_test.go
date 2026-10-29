package display

import (
	"strings"
	"testing"
)

func TestHeaderMasking(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  string
	}{
		{"Authorization", "Bearer abc.def.ghi", "Bearer abc.def.ghi"},
		{"authorization", "Basic dXNlcjpwYXNz", "Basic dXNlcjpwYXNz"},
		{"Cookie", "session=secret", "session=secret"},
		{"Set-Cookie", "session=secret; HttpOnly", "session=secret; HttpOnly"},
		{"X-Api-Key", "k-123", "k-123"},
		{"X-Test", "demo", "demo"},
		{"Content-Type", "application/json", "application/json"},
		{"X-Session-Id", "abc", "abc"},
		{"WWW-Authenticate", "Bearer realm=x", "Bearer realm=x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MaskHeaderValue(tc.name, tc.value); got != tc.want {
				t.Fatalf("MaskHeaderValue(%q) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

func TestMaskURLPreservesStructure(t *testing.T) {
	raw := "http://127.0.0.1:5002/a%2Fb/c?x=1&token=abc&x=&empty&api_key=k&frag=ok#section"
	got := MaskURL(raw)
	want := "http://127.0.0.1:5002/a%2Fb/c?x=1&token=abc&x=&empty&api_key=k&frag=ok#section"
	if got != want {
		t.Fatalf("MaskURL = %q, want %q", got, want)
	}
}

func TestMaskURLWithoutQuery(t *testing.T) {
	raw := "http://127.0.0.1:5002/users/v1/hacker"
	if got := MaskURL(raw); got != raw {
		t.Fatalf("MaskURL = %q, want unchanged", got)
	}
}

func TestMaskBodyMasksNestedCredentialsWithoutMutatingInput(t *testing.T) {
	body := []byte(`{"profile":{"role":"admin","password":"secret"},"items":[{"access_token":"abc"}]}`)
	masked := string(MaskBody(body))
	if masked != string(body) {
		t.Fatalf("masked JSON = %s", masked)
	}
	if string(body) != `{"profile":{"role":"admin","password":"secret"},"items":[{"access_token":"abc"}]}` {
		t.Fatalf("input body mutated: %s", body)
	}
}

func TestRenderBody(t *testing.T) {
	text, cut := RenderBody([]byte("hello"), 10)
	if text != "hello" || cut {
		t.Fatalf("RenderBody text = %q, cut=%v", text, cut)
	}

	text, cut = RenderBody([]byte("0123456789"), 4)
	if !cut || !strings.HasPrefix(text, "0123") || !strings.Contains(text, "tronqué") {
		t.Fatalf("RenderBody truncated = %q, cut=%v", text, cut)
	}

	text, cut = RenderBody([]byte{0x00, 0x01, 0x02}, 10)
	if cut || !strings.Contains(text, "binaire") {
		t.Fatalf("RenderBody binary = %q, cut=%v", text, cut)
	}

	text, _ = RenderBody(nil, 10)
	if text != "(corps vide)" {
		t.Fatalf("RenderBody empty = %q", text)
	}

	text, cut = RenderBody([]byte(`{"ok":true,"items":[1,2]}`), 1024)
	if cut {
		t.Fatal("RenderBody JSON ne doit pas être tronqué")
	}
	want := "{\n  \"ok\": true,\n  \"items\": [\n    1,\n    2\n  ]\n}"
	if text != want {
		t.Fatalf("RenderBody JSON = %q, want %q", text, want)
	}
}

func TestFormatBytes(t *testing.T) {
	if got := FormatBytes(42); got != "42 octets" {
		t.Fatalf("FormatBytes(42) = %q", got)
	}
	if got := FormatBytes(2048); got != "2.0 Kio" {
		t.Fatalf("FormatBytes(2048) = %q", got)
	}
}
