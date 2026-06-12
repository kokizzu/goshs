package tui

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// This file is the Go counterpart of the smart content decoder in
// assets/js/src/collab.js. It gives the TUI's HTTP detail view the same
// usability the browser Collab tab has: JSON is pretty-printed, and base64 /
// JWT / form-encoded values found in the URL parameters, headers or body are
// decoded inline. Behaviour intentionally mirrors the JS so an operator sees
// the same output in either interface.

var (
	reBase64Std     = regexp.MustCompile(`^[A-Za-z0-9+/]+=*$`)
	reBase64URLSafe = regexp.MustCompile(`^[A-Za-z0-9\-_]+=*$`)
	reBearer        = regexp.MustCompile(`(?i)^Bearer\s+`)
	reBasic         = regexp.MustCompile(`(?i)^Basic\s+`)
	reAuthScheme    = regexp.MustCompile(`^([A-Za-z0-9\-]+)\s+(.+)$`)
)

// smartDecode transforms a captured value the way the Collab tab does: it
// pretty-prints JSON (recursively decoding base64/JWT string leaves), decodes
// a standalone JWT or base64 value, or expands form-encoded bodies. It returns
// the rendered text and a short tag naming the transformation applied (empty
// when the value was left untouched).
func smartDecode(raw string) (text, tag string) {
	t := strings.TrimSpace(raw)
	if t == "" {
		return raw, ""
	}
	if out, tg := tryDecodeValue(t); tg != "" {
		return out, tg
	}
	if strings.Contains(t, "=") && !strings.HasPrefix(t, "{") && !strings.HasPrefix(t, "[") {
		if out, any := decodeForm(t); any {
			return out, "form-decoded"
		}
	}
	return raw, ""
}

// tryDecodeValue attempts JSON, then JWT, then base64 (possibly wrapping JSON)
// on a single value. tag is empty when nothing matched.
func tryDecodeValue(raw string) (text, tag string) {
	if js, ok := prettyJSON(raw); ok {
		return js, "JSON"
	}
	if jwt, ok := tryJWT(raw); ok {
		return jwt, "JWT"
	}
	if b64, ok := tryBase64(raw); ok {
		if js, ok := prettyJSON(b64); ok {
			return js, "base64 → JSON"
		}
		return b64, "base64"
	}
	return raw, ""
}

// prettyJSON indents a JSON value and recursively decodes any base64/JWT
// string leaves, matching walkJSON in collab.js. ok is false when the input is
// not JSON.
func prettyJSON(s string) (string, bool) {
	t := strings.TrimSpace(s)
	if t == "" || (t[0] != '{' && t[0] != '[') {
		return "", false
	}
	var v any
	if err := json.Unmarshal([]byte(t), &v); err != nil {
		return "", false
	}
	out, err := json.MarshalIndent(walkJSON(v), "", "  ")
	if err != nil {
		return "", false
	}
	return string(out), true
}

func walkJSON(node any) any {
	switch n := node.(type) {
	case []any:
		for i := range n {
			n[i] = walkJSON(n[i])
		}
		return n
	case map[string]any:
		for k, v := range n {
			n[k] = walkJSON(v)
		}
		return n
	case string:
		return decodeStringLeaf(n)
	}
	return node
}

func decodeStringLeaf(s string) any {
	if len(s) < 8 {
		return s
	}
	if jwt, ok := tryJWT(s); ok {
		var parsed any
		if json.Unmarshal([]byte(jwt), &parsed) == nil {
			return map[string]any{"__decoded": "JWT", "__value": parsed}
		}
		return map[string]any{"__decoded": "JWT", "__value": jwt}
	}
	if b64, ok := tryBase64(s); ok {
		tb := strings.TrimSpace(b64)
		if len(tb) > 0 && (tb[0] == '{' || tb[0] == '[') {
			var nested any
			if json.Unmarshal([]byte(tb), &nested) == nil {
				return map[string]any{"__decoded": "base64→JSON", "__value": walkJSON(nested)}
			}
		}
		return map[string]any{"__decoded": "base64", "__value": b64}
	}
	return s
}

// tryBase64 decodes s if it is valid (standard or URL-safe) base64 that yields
// mostly-printable text. ok is false otherwise. Mirrors tryBase64 in
// collab.js, including the 10% non-printable rejection threshold.
func tryBase64(s string) (string, bool) {
	if len(s) < 8 {
		return "", false
	}
	clean := strings.NewReplacer("\r", "", "\n", "").Replace(s)
	isStd := reBase64Std.MatchString(clean)
	isURLSafe := reBase64URLSafe.MatchString(clean)
	if !isStd && !isURLSafe {
		return "", false
	}
	norm := strings.NewReplacer("-", "+", "_", "/").Replace(clean)
	if rem := len(norm) % 4; rem != 0 {
		norm += strings.Repeat("=", 4-rem)
	}
	decoded, err := base64.StdEncoding.DecodeString(norm)
	if err != nil {
		return "", false
	}
	bad := 0
	for _, c := range decoded {
		if c < 9 || (c > 13 && c < 32) || c >= 127 {
			bad++
		}
	}
	if len(decoded) > 0 && float64(bad)/float64(len(decoded)) > 0.1 {
		return "", false
	}
	return string(decoded), true
}

// tryJWT decodes the header and payload of a three-part JWT. ok is false when
// s is not a well-formed JWT with JSON segments. Mirrors tryJWT in collab.js.
func tryJWT(s string) (string, bool) {
	parts := strings.Split(reBearer.ReplaceAllString(strings.TrimSpace(s), ""), ".")
	if len(parts) != 3 {
		return "", false
	}
	header, ok := decodeJWTSegment(parts[0])
	if !ok {
		return "", false
	}
	payload, ok := decodeJWTSegment(parts[1])
	if !ok {
		return "", false
	}
	out, err := json.MarshalIndent(map[string]any{
		"header":    header,
		"payload":   payload,
		"signature": parts[2],
	}, "", "  ")
	if err != nil {
		return "", false
	}
	return string(out), true
}

func decodeJWTSegment(seg string) (any, bool) {
	norm := strings.NewReplacer("-", "+", "_", "/").Replace(seg)
	if rem := len(norm) % 4; rem != 0 {
		norm += strings.Repeat("=", 4-rem)
	}
	data, err := base64.StdEncoding.DecodeString(norm)
	if err != nil {
		return nil, false
	}
	var v any
	if json.Unmarshal(data, &v) != nil {
		return nil, false
	}
	return v, true
}

// decodeForm expands a form-encoded body (key=value&key=value), decoding each
// value independently. any reports whether at least one value was transformed.
func decodeForm(body string) (text string, any bool) {
	pairs := strings.Split(body, "&")
	lines := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		eq := strings.Index(pair, "=")
		if eq == -1 {
			lines = append(lines, urlUnescape(pair))
			continue
		}
		k := urlUnescape(pair[:eq])
		v := urlUnescape(pair[eq+1:])
		if out, tag := tryDecodeValue(v); tag != "" {
			any = true
			lines = append(lines, k+" ["+tag+"] = "+inlineOrIndent(out, "  "))
		} else {
			lines = append(lines, k+" = "+v)
		}
	}
	return strings.Join(lines, "\n\n"), any
}

// decodeAuthHeader applies scheme-aware decoding to an Authorization header,
// mirroring decodeAuthHeader in collab.js.
func decodeAuthHeader(value string) (text, tag string) {
	v := strings.TrimSpace(value)
	switch {
	case reBearer.MatchString(v):
		if jwt, ok := tryJWT(v); ok {
			return jwt, "JWT"
		}
		token := reBearer.ReplaceAllString(v, "")
		if b64, ok := tryBase64(token); ok {
			return b64, "Bearer → base64"
		}
		return v, ""
	case reBasic.MatchString(v):
		token := reBasic.ReplaceAllString(v, "")
		if b64, ok := tryBase64(token); ok {
			return b64, "Basic auth"
		}
		return v, ""
	}
	if mm := reAuthScheme.FindStringSubmatch(v); mm != nil {
		scheme, token := mm[1], mm[2]
		if b64, ok := tryBase64(token); ok {
			return b64, scheme + " → base64"
		}
		if strings.Contains(token, ",") {
			parts := strings.Split(token, ",")
			for i := range parts {
				parts[i] = "  " + strings.TrimSpace(parts[i])
			}
			return strings.Join(parts, "\n"), scheme
		}
		return v, ""
	}
	if b64, ok := tryBase64(v); ok {
		return b64, "base64"
	}
	return v, ""
}

// formatHeader renders a single header line, decoding its value the way
// fmtHeaders does in collab.js: the Authorization header gets the scheme-aware
// decoder, every other header gets the generic value decoder. Multi-line
// decoded output is indented under the header name.
func formatHeader(key, value string) string {
	var text, tag string
	if strings.EqualFold(key, "authorization") {
		text, tag = decodeAuthHeader(value)
	} else {
		text, tag = tryDecodeValue(value)
	}
	if tag == "" {
		return key + ": " + value
	}
	return key + " [" + tag + "]: " + inlineOrIndent(text, "    ")
}

// sortedKeys returns the keys of a header map in case-insensitive order so the
// detail view is stable between renders.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return strings.ToLower(keys[i]) < strings.ToLower(keys[j])
	})
	return keys
}

// indentBlock prefixes every line of s with indent.
func indentBlock(s, indent string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = indent + lines[i]
	}
	return strings.Join(lines, "\n")
}

// urlUnescape percent-decodes a form field, falling back to the raw value
// when it is not valid URL encoding.
func urlUnescape(s string) string {
	if dec, err := url.QueryUnescape(s); err == nil {
		return dec
	}
	return s
}

// inlineOrIndent keeps single-line values inline; multi-line values are
// prefixed with a newline and the given indent on each line, matching the
// web UI's layout.
func inlineOrIndent(s, indent string) string {
	if !strings.Contains(s, "\n") {
		return s
	}
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = indent + lines[i]
	}
	return "\n" + strings.Join(lines, "\n")
}
