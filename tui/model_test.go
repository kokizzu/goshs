package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"goshs.de/goshs/v2/options"
)

// newTestModel builds a minimal model with empty panes for unit testing the
// ingest/selection logic without a running bubbletea program.
func newTestModel() *model {
	return &model{
		panes: []*pane{
			{name: "HTTP"},
			{name: "DNS"},
		},
	}
}

func row(s string) eventRow { return eventRow{summary: s} }

func TestAddRowNewestFirst(t *testing.T) {
	m := newTestModel()
	m.addRow(paneHTTP, row("first"))
	m.addRow(paneHTTP, row("second"))
	p := m.panes[paneHTTP]
	if p.rows[0].summary != "second" {
		t.Fatalf("row 0 = %q, want second (newest first)", p.rows[0].summary)
	}
}

func TestAddRowRidesTopInListView(t *testing.T) {
	m := newTestModel()
	m.addRow(paneHTTP, row("a"))
	m.addRow(paneHTTP, row("b"))
	// Not in detail view and sitting at the top: selection stays on the newest.
	if got := m.panes[paneHTTP].sel; got != 0 {
		t.Fatalf("sel = %d, want 0 (riding the top)", got)
	}
}

func TestAddRowAnchorsSelectionWhenScrolled(t *testing.T) {
	m := newTestModel()
	for _, s := range []string{"a", "b", "c"} {
		m.addRow(paneHTTP, row(s))
	}
	p := m.panes[paneHTTP]
	// Operator scrolls down to inspect an older event.
	p.sel = 1
	want := p.rows[1].summary
	m.addRow(paneHTTP, row("d"))
	if p.rows[p.sel].summary != want {
		t.Fatalf("selection moved off its event: got %q, want %q", p.rows[p.sel].summary, want)
	}
}

func TestAddRowAnchorsSelectionInDetailView(t *testing.T) {
	m := newTestModel()
	m.addRow(paneHTTP, row("a"))
	m.addRow(paneHTTP, row("b"))
	p := m.panes[paneHTTP]
	// Open the detail view on the newest event (row 0).
	m.active = paneHTTP
	m.detail = true
	want := p.rows[0].summary
	m.addRow(paneHTTP, row("c"))
	if p.rows[p.sel].summary != want {
		t.Fatalf("detail view swapped events: got %q, want %q", p.rows[p.sel].summary, want)
	}
}

// TestViewFillsHeight guards the fixed-height layout: the rendered frame must
// be exactly m.height lines in both the list and detail views so the status
// bar stays pinned to the bottom of the screen regardless of event volume.
func TestViewFillsHeight(t *testing.T) {
	m := newModel(&options.Options{IP: "0.0.0.0", Port: 8000, Webroot: "/srv", DNS: true, DNSPort: 8053, SMB: true, SMBPort: 445}, nil, nil, nil, nil, nil)
	m.width, m.height = 100, 30
	for i := 0; i < 50; i++ {
		m.addRow(paneHTTP, row(fmt.Sprintf("event %d", i)))
	}
	m.active = paneHTTP

	if got := lipgloss.Height(m.View()); got != m.height {
		t.Fatalf("list view height = %d, want %d", got, m.height)
	}

	m.detail = true
	if got := lipgloss.Height(m.View()); got != m.height {
		t.Fatalf("detail view height = %d, want %d", got, m.height)
	}
}

// TestDetailScrollClamps verifies the detail scroll offset is bounded to the
// content during render, so paging past the end cannot strand the operator.
func TestDetailScrollClamps(t *testing.T) {
	m := newModel(&options.Options{Webroot: "/srv"}, nil, nil, nil, nil, nil)
	m.width, m.height = 80, 24
	m.addRow(paneHTTP, eventRow{summary: "x", detail: strings.Repeat("line\n", 100)})
	m.active = paneHTTP
	m.detail = true

	p := m.panes[paneHTTP]
	p.detailTop = 9999
	_ = m.View()

	if p.detailTop == 9999 {
		t.Fatalf("detailTop was not clamped to the content")
	}
	if m.detailH <= 0 {
		t.Fatalf("detailH not recorded for paging: %d", m.detailH)
	}
}

// TestStatusBarTunnelURL verifies the live tunnel URL is surfaced in the status
// bar once available, and shows a connecting placeholder until then.
func TestStatusBarTunnelURL(t *testing.T) {
	url := ""
	m := newModel(&options.Options{Webroot: "/srv", Tunnel: true}, nil, nil, nil, func() string { return url }, nil)

	if seg := strings.Join(m.statusSegments(), " "); !strings.Contains(seg, "tunnel (connecting)") {
		t.Fatalf("expected connecting placeholder before URL is up, got: %q", seg)
	}

	url = "https://abc123.tunnel.example"
	if seg := strings.Join(m.statusSegments(), " "); !strings.Contains(seg, url) {
		t.Fatalf("expected live tunnel URL in status bar, got: %q", seg)
	}
}

// TestExportPaneJSON verifies a per-pane export writes goshs-<proto>-log.json
// as a bare JSON array of the raw events, matching the web UI's exportHTTP().
func TestExportPaneJSON(t *testing.T) {
	dir := t.TempDir()
	cwd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer os.Chdir(cwd)

	m := newModel(&options.Options{Webroot: dir}, nil, nil, nil, nil, nil)
	m.addHTTP([]byte(`{"type":"http","source":"10.0.0.5:5000","method":"GET","status":200,"url":"/secret"}`))

	m.exportPane(paneHTTP)

	if !strings.HasPrefix(m.flash, "exported 1 HTTP event(s)") {
		t.Fatalf("unexpected flash after export: %q", m.flash)
	}

	data, err := os.ReadFile(filepath.Join(dir, "goshs-http-log.json"))
	if err != nil {
		t.Fatalf("read export: %v", err)
	}
	var events []map[string]any
	if err := json.Unmarshal(data, &events); err != nil {
		t.Fatalf("export is not a JSON array: %v\n%s", err, data)
	}
	if len(events) != 1 || events[0]["url"] != "/secret" || events[0]["type"] != "http" {
		t.Fatalf("unexpected export contents: %s", data)
	}
}

// TestExportAllJSON verifies the combined export mirrors the web UI's
// exportAllLogs() wrapper object: {generatedAt, http, dns, smtp, smb, ldap}.
func TestExportAllJSON(t *testing.T) {
	dir := t.TempDir()
	cwd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer os.Chdir(cwd)

	m := newModel(&options.Options{Webroot: dir}, nil, nil, nil, nil, nil)
	m.addHTTP([]byte(`{"type":"http","source":"10.0.0.5:5000","method":"GET","status":200,"url":"/a"}`))
	m.addDNS([]byte(`{"type":"dns","name":"evil.example","qtype":"A","source":"10.0.0.6:5300"}`))

	m.exportAll()

	data, err := os.ReadFile(filepath.Join(dir, "goshs-all-logs.json"))
	if err != nil {
		t.Fatalf("read export: %v", err)
	}
	var all struct {
		GeneratedAt string           `json:"generatedAt"`
		HTTP        []map[string]any `json:"http"`
		DNS         []map[string]any `json:"dns"`
		SMTP        []map[string]any `json:"smtp"`
		SMB         []map[string]any `json:"smb"`
		LDAP        []map[string]any `json:"ldap"`
	}
	if err := json.Unmarshal(data, &all); err != nil {
		t.Fatalf("export is not the expected object: %v\n%s", err, data)
	}
	if all.GeneratedAt == "" {
		t.Fatalf("missing generatedAt; got:\n%s", data)
	}
	if len(all.HTTP) != 1 || len(all.DNS) != 1 {
		t.Fatalf("expected 1 http + 1 dns event, got http=%d dns=%d", len(all.HTTP), len(all.DNS))
	}
}

// TestExportEmptyPane verifies exporting a pane with no events writes nothing
// and flashes a "nothing to export" notice instead.
func TestExportEmptyPane(t *testing.T) {
	dir := t.TempDir()
	cwd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer os.Chdir(cwd)

	m := newModel(&options.Options{Webroot: dir}, nil, nil, nil, nil, nil)
	m.exportPane(paneDNS)

	if m.flash != "nothing to export" {
		t.Fatalf("expected nothing-to-export flash, got %q", m.flash)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "*.json")); len(matches) != 0 {
		t.Fatalf("expected no file written, found %v", matches)
	}
}
