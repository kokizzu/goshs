package tui

import "testing"

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
