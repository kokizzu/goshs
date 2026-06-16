package tui

import (
	"fmt"
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
	m := newModel(&options.Options{IP: "0.0.0.0", Port: 8000, Webroot: "/srv", DNS: true, DNSPort: 8053, SMB: true, SMBPort: 445}, nil, nil, nil, nil)
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
	m := newModel(&options.Options{Webroot: "/srv"}, nil, nil, nil, nil)
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
