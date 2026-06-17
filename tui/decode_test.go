package tui

import (
	"strings"
	"testing"
)

func TestSmartDecodeJSON(t *testing.T) {
	out, tag := smartDecode(`{"a":1,"b":"hi"}`)
	if tag != "JSON" {
		t.Fatalf("tag = %q, want JSON", tag)
	}
	if !strings.Contains(out, "\n") || !strings.Contains(out, `"a": 1`) {
		t.Fatalf("expected pretty-printed JSON, got:\n%s", out)
	}
}

func TestSmartDecodeBase64(t *testing.T) {
	// base64 of "hello world!!" (>= 8 chars decoded, printable)
	out, tag := smartDecode("aGVsbG8gd29ybGQhIQ==")
	if tag != "base64" {
		t.Fatalf("tag = %q, want base64", tag)
	}
	if out != "hello world!!" {
		t.Fatalf("out = %q, want %q", out, "hello world!!")
	}
}

func TestSmartDecodeBase64JSON(t *testing.T) {
	// base64 of {"user":"admin"}
	out, tag := smartDecode("eyJ1c2VyIjoiYWRtaW4ifQ==")
	if tag != "base64 → JSON" {
		t.Fatalf("tag = %q, want base64 → JSON", tag)
	}
	if !strings.Contains(out, `"user": "admin"`) {
		t.Fatalf("expected decoded JSON, got:\n%s", out)
	}
}

func TestSmartDecodeBinaryRejected(t *testing.T) {
	// Valid base64 but decodes to binary — must be left untouched.
	if _, ok := tryBase64("////////"); ok {
		t.Fatal("binary base64 should be rejected")
	}
}

func TestSmartDecodeForm(t *testing.T) {
	// user=admin&token=<base64 of secret123>
	out, tag := smartDecode("user=admin&token=c2VjcmV0MTIz")
	if tag != "form-decoded" {
		t.Fatalf("tag = %q, want form-decoded", tag)
	}
	if !strings.Contains(out, "user = admin") || !strings.Contains(out, "secret123") {
		t.Fatalf("unexpected form output:\n%s", out)
	}
}

func TestSmartDecodePlain(t *testing.T) {
	if _, tag := smartDecode("just some plain text"); tag != "" {
		t.Fatalf("tag = %q, want empty", tag)
	}
}

func TestDecodeAuthHeaderBasic(t *testing.T) {
	// Basic dXNlcjpwYXNz -> user:pass
	out, tag := decodeAuthHeader("Basic dXNlcjpwYXNz")
	if tag != "Basic auth" || out != "user:pass" {
		t.Fatalf("got (%q, %q), want (user:pass, Basic auth)", out, tag)
	}
}

func TestFormatHeaderPlain(t *testing.T) {
	if got := formatHeader("Accept", "text/html"); got != "Accept: text/html" {
		t.Fatalf("got %q", got)
	}
}

func TestHTMLToText(t *testing.T) {
	in := `<html><head><style>p{color:red}</style><title>x</title></head>
		<body><h1>Hi there</h1><p>Click <a href="http://evil">here</a> now</p>
		<script>alert(1)</script></body></html>`
	out := htmlToText(in)
	for _, want := range []string{"Hi there", "Click", "here", "now"} {
		if !strings.Contains(out, want) {
			t.Fatalf("htmlToText dropped %q; got:\n%s", want, out)
		}
	}
	for _, bad := range []string{"color:red", "alert(1)", "<", ">"} {
		if strings.Contains(out, bad) {
			t.Fatalf("htmlToText leaked %q; got:\n%s", bad, out)
		}
	}
	// Block elements should introduce line breaks (heading separate from body).
	if !strings.Contains(out, "Hi there\n") {
		t.Fatalf("expected a line break after the heading; got:\n%s", out)
	}
}

func TestHTMLToTextPlainPassthrough(t *testing.T) {
	// No tags: content survives, whitespace is tidied.
	if got := htmlToText("just  plain   text"); got != "just plain text" {
		t.Fatalf("got %q", got)
	}
}

func TestHTMLToTextLink(t *testing.T) {
	out := htmlToText(`<p>Click <a href="http://evil.example/login">here</a></p>`)
	if !strings.Contains(out, "here") || !strings.Contains(out, "(http://evil.example/login)") {
		t.Fatalf("link text and href should both appear; got:\n%s", out)
	}
	// In-page anchors are noise and should be dropped.
	if got := htmlToText(`<a href="#top">top</a>`); strings.Contains(got, "(#top)") {
		t.Fatalf("in-page anchor href should be omitted; got:\n%s", got)
	}
}

func TestHTMLToTextImage(t *testing.T) {
	out := htmlToText(`<img src="http://track.example/p.gif" alt="logo">`)
	if !strings.Contains(out, "[image:") || !strings.Contains(out, "http://track.example/p.gif") || !strings.Contains(out, "logo") {
		t.Fatalf("image src and alt should appear; got:\n%s", out)
	}
}
