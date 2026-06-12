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
