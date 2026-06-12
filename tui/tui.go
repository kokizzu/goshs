// Package tui implements the optional interactive terminal dashboard for
// goshs. It is a second, in-process consumer of the WebSocket hub: it
// subscribes to the same broadcast stream and history that the browser Collab
// tab uses and renders it as a live full-screen view, so an operator running
// goshs headless over SSH gets situational awareness without port-forwarding
// the web UI.
package tui

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"goshs.de/goshs/v2/catcher"
	"goshs.de/goshs/v2/options"
	"goshs.de/goshs/v2/ws"
)

// maxRows caps how many events are retained per pane so a long-running
// session cannot grow the TUI's memory unbounded — mirroring the hub's own
// ring-buffer policy.
const maxRows = 500

// Run starts the dashboard and blocks until the operator quits it (q or
// Ctrl+C) or the self-destruct timer fires on ttlC. It subscribes to the hub
// for the duration and unsubscribes on exit. The caller is responsible for
// shutting the servers down afterwards. ttlC may be nil when --ttl is unset.
func Run(opts *options.Options, hub *ws.Hub, mgr *catcher.Manager, ttlC <-chan time.Time) error {
	sub := hub.Subscribe()
	defer hub.Unsubscribe(sub)

	m := newModel(opts, hub, mgr, sub, ttlC)
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

// --- messages ---------------------------------------------------------------

type hubMsg []byte
type tickMsg time.Time
type subClosedMsg struct{}
type ttlExpiredMsg struct{}

func waitForEvent(sub chan []byte) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-sub
		if !ok {
			return subClosedMsg{}
		}
		return hubMsg(msg)
	}
}

func waitForTTL(ttlC <-chan time.Time) tea.Cmd {
	if ttlC == nil {
		return nil
	}
	return func() tea.Msg {
		<-ttlC
		return ttlExpiredMsg{}
	}
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// --- model ------------------------------------------------------------------

type eventRow struct {
	summary string
	detail  string
	accent  lipgloss.Color
}

type pane struct {
	name string
	rows []eventRow
	sel  int
	top  int
}

// add inserts a new event at the top of the pane so the newest event is
// always row 0, mirroring the Collab tab in the web UI. Selection adjustment
// is left to model.addRow, which knows whether the detail view is open.
func (p *pane) add(r eventRow) {
	p.rows = append([]eventRow{r}, p.rows...)
	if len(p.rows) > maxRows {
		p.rows = p.rows[:maxRows]
	}
}

const (
	paneHTTP = iota
	paneDNS
	paneSMB
	paneLDAP
	paneSMTP
	paneShells
)

type model struct {
	opts *options.Options
	hub  *ws.Hub
	mgr  *catcher.Manager
	sub  chan []byte

	ttl <-chan time.Time // self-destruct trigger; nil when --ttl unset

	panes    []*pane
	active   int
	width    int
	height   int
	deadline time.Time // zero when no --ttl set
	detail   bool      // detail view of the selected row is open
}

func newModel(opts *options.Options, hub *ws.Hub, mgr *catcher.Manager, sub chan []byte, ttlC <-chan time.Time) *model {
	var deadline time.Time
	if opts.TTL > 0 {
		deadline = time.Now().Add(opts.TTL)
	}
	return &model{
		opts:     opts,
		hub:      hub,
		mgr:      mgr,
		sub:      sub,
		ttl:      ttlC,
		deadline: deadline,
		panes: []*pane{
			{name: "HTTP"},
			{name: "DNS"},
			{name: "SMB"},
			{name: "LDAP"},
			{name: "SMTP"},
			{name: "SHELLS"},
		},
	}
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(waitForEvent(m.sub), waitForTTL(m.ttl), tick())
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case hubMsg:
		m.ingest([]byte(msg))
		return m, waitForEvent(m.sub)

	case subClosedMsg:
		return m, tea.Quit

	case ttlExpiredMsg:
		// Self-destruct fired; quitting returns control to main, which runs
		// the graceful shutdown.
		return m, tea.Quit

	case tickMsg:
		m.refreshShells()
		return m, tick()
	}
	return m, nil
}

func (m *model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "tab", "right", "l":
		m.detail = false
		m.active = (m.active + 1) % len(m.panes)
	case "shift+tab", "left", "h":
		m.detail = false
		m.active = (m.active - 1 + len(m.panes)) % len(m.panes)
	case "up", "k":
		p := m.panes[m.active]
		if p.sel > 0 {
			p.sel--
		}
	case "down", "j":
		p := m.panes[m.active]
		if p.sel < len(p.rows)-1 {
			p.sel++
		}
	case "enter":
		p := m.panes[m.active]
		if len(p.rows) > 0 {
			m.detail = !m.detail
		}
	case "esc":
		m.detail = false
	}
	return m, nil
}

// --- ingest -----------------------------------------------------------------

// addRow stores a new event in pane idx and keeps the operator anchored on
// whatever they were looking at. New events are prepended (row 0), so to hold
// the view steady the selection and scroll offset shift down with the older
// rows — except when sitting at the very top of the list view, where the
// selection rides the newest event as a live tail. While the detail view is
// open on this pane the selection always follows its event, so an incoming
// event never swaps out what is being inspected.
func (m *model) addRow(idx int, r eventRow) {
	p := m.panes[idx]
	p.add(r)
	if len(p.rows) == 1 {
		return
	}
	riding := p.sel == 0 && p.top == 0 && !(m.detail && idx == m.active)
	if riding {
		return
	}
	if p.sel < len(p.rows)-1 {
		p.sel++
	}
	p.top++
}

func (m *model) ingest(raw []byte) {
	var peek struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &peek); err != nil {
		return
	}

	switch peek.Type {
	case "catchup":
		m.ingestCatchup(raw)
	case "http":
		m.addHTTP(raw)
	case "dns":
		m.addDNS(raw)
	case "smtp":
		m.addSMTP(raw)
	case "smb":
		m.addSMB(raw)
	case "ldap":
		m.addLDAP(raw)
	}
	// catcherConnection events are intentionally ignored here: the SHELLS pane
	// is rebuilt from the catcher manager on every tick, which reflects both
	// connects and disconnects accurately.
}

func (m *model) ingestCatchup(raw []byte) {
	var cu struct {
		HTTP []json.RawMessage `json:"http"`
		DNS  []json.RawMessage `json:"dns"`
		SMTP []json.RawMessage `json:"smtp"`
		SMB  []json.RawMessage `json:"smb"`
		LDAP []json.RawMessage `json:"ldap"`
	}
	if err := json.Unmarshal(raw, &cu); err != nil {
		return
	}
	for _, e := range cu.HTTP {
		m.addHTTP(e)
	}
	for _, e := range cu.DNS {
		m.addDNS(e)
	}
	for _, e := range cu.SMTP {
		m.addSMTP(e)
	}
	for _, e := range cu.SMB {
		m.addSMB(e)
	}
	for _, e := range cu.LDAP {
		m.addLDAP(e)
	}
}

func (m *model) addHTTP(raw []byte) {
	var e ws.HTTPEvent
	if json.Unmarshal(raw, &e) != nil {
		return
	}
	accent := lipgloss.Color("2") // green
	if e.Status >= 400 {
		accent = lipgloss.Color("1") // red
	} else if e.Status >= 300 {
		accent = lipgloss.Color("4") // blue
	}
	summary := fmt.Sprintf("%s  %-15s %-6s %-3d %s",
		tstamp(e.Timestamp), host(e.Source), e.Method, e.Status, e.URL)

	var b strings.Builder
	fmt.Fprintf(&b, "Time:      %s\n", e.Timestamp.Format(time.RFC3339))
	fmt.Fprintf(&b, "Source:    %s\n", e.Source)
	fmt.Fprintf(&b, "Request:   %s %s -> %d\n", e.Method, e.URL, e.Status)
	fmt.Fprintf(&b, "UserAgent: %s\n", e.UserAgent)
	if e.Parameters != "" {
		b.WriteString("\nParameters:\n")
		if out, any := decodeForm(e.Parameters); any {
			b.WriteString(indentBlock(out, "  ") + "\n")
		} else {
			fmt.Fprintf(&b, "  %s\n", e.Parameters)
		}
	}
	if len(e.Headers) > 0 {
		b.WriteString("\nHeaders:\n")
		for _, k := range sortedKeys(e.Headers) {
			b.WriteString("  " + formatHeader(k, e.Headers[k]) + "\n")
		}
	}
	if e.Body != "" {
		b.WriteString("\nBody:\n")
		if text, tag := smartDecode(e.Body); tag != "" {
			fmt.Fprintf(&b, "[%s]\n%s\n", tag, text)
		} else {
			fmt.Fprintf(&b, "%s\n", e.Body)
		}
	}
	m.addRow(paneHTTP, eventRow{summary: summary, detail: b.String(), accent: accent})
}

func (m *model) addDNS(raw []byte) {
	var e ws.DNSEvent
	if json.Unmarshal(raw, &e) != nil {
		return
	}
	summary := fmt.Sprintf("%s  %-15s %-5s %s", tstamp(e.Time), host(e.Source), e.QType, e.Name)
	detail := fmt.Sprintf("Time:   %s\nSource: %s\nType:   %s\nName:   %s\n",
		e.Time.Format(time.RFC3339), e.Source, e.QType, e.Name)
	m.addRow(paneDNS, eventRow{summary: summary, detail: detail, accent: lipgloss.Color("6")})
}

func (m *model) addSMTP(raw []byte) {
	var e ws.SMTPEvent
	if json.Unmarshal(raw, &e) != nil {
		return
	}
	att := ""
	if len(e.Attachments) > 0 {
		att = fmt.Sprintf(" [%d att]", len(e.Attachments))
	}
	summary := fmt.Sprintf("%s  %-24s %s%s", tstamp(e.Timestamp), trunc(e.From, 24), e.Subject, att)

	var b strings.Builder
	fmt.Fprintf(&b, "Time:    %s\n", e.Timestamp.Format(time.RFC3339))
	fmt.Fprintf(&b, "From:    %s\n", e.From)
	fmt.Fprintf(&b, "To:      %s\n", strings.Join(e.To, ", "))
	if len(e.CC) > 0 {
		fmt.Fprintf(&b, "CC:      %s\n", strings.Join(e.CC, ", "))
	}
	fmt.Fprintf(&b, "Subject: %s\n", e.Subject)
	for _, a := range e.Attachments {
		fmt.Fprintf(&b, "Attach:  %s (%s, %d bytes)\n", a.Filename, a.ContentType, a.Size)
	}
	if e.Body != "" {
		fmt.Fprintf(&b, "\n%s\n", e.Body)
	}
	m.addRow(paneSMTP, eventRow{summary: summary, detail: b.String(), accent: lipgloss.Color("3")})
}

func (m *model) addSMB(raw []byte) {
	var e ws.NTLMEvent
	if json.Unmarshal(raw, &e) != nil {
		return
	}
	cracked := ""
	if e.CrackedPassword != "" {
		cracked = " cracked=" + e.CrackedPassword
	}
	user := e.Username
	if e.Domain != "" {
		user = e.Domain + "\\" + e.Username
	}
	summary := fmt.Sprintf("%s  %-15s %-9s %s%s", tstamp(e.Timestamp), host(e.Source), e.HashType, user, cracked)

	var b strings.Builder
	fmt.Fprintf(&b, "Time:        %s\n", e.Timestamp.Format(time.RFC3339))
	fmt.Fprintf(&b, "Source:      %s\n", e.Source)
	fmt.Fprintf(&b, "User:        %s\n", user)
	fmt.Fprintf(&b, "Workstation: %s\n", e.Workstation)
	fmt.Fprintf(&b, "Hash type:   %s (hashcat mode %s)\n", e.HashType, e.HashcatMode)
	if e.CrackedPassword != "" {
		fmt.Fprintf(&b, "Cracked:     %s\n", e.CrackedPassword)
	}
	fmt.Fprintf(&b, "\nHash:\n%s\n", e.Hash)
	m.addRow(paneSMB, eventRow{summary: summary, detail: b.String(), accent: lipgloss.Color("5")})
}

func (m *model) addLDAP(raw []byte) {
	var e ws.LDAPEvent
	if json.Unmarshal(raw, &e) != nil {
		return
	}
	var summary string
	var b strings.Builder
	fmt.Fprintf(&b, "Time:      %s\n", e.Timestamp.Format(time.RFC3339))
	fmt.Fprintf(&b, "Source:    %s\n", e.Source)
	fmt.Fprintf(&b, "Operation: %s\n", e.Operation)
	if e.Operation == "ntlm" {
		user := e.Username
		if e.Domain != "" {
			user = e.Domain + "\\" + e.Username
		}
		cracked := ""
		if e.CrackedPassword != "" {
			cracked = " cracked=" + e.CrackedPassword
		}
		summary = fmt.Sprintf("%s  %-15s ntlm  %-9s %s%s", tstamp(e.Timestamp), host(e.Source), e.HashType, user, cracked)
		fmt.Fprintf(&b, "User:      %s\n", user)
		fmt.Fprintf(&b, "Hash type: %s (hashcat mode %s)\n", e.HashType, e.HashcatMode)
		if e.CrackedPassword != "" {
			fmt.Fprintf(&b, "Cracked:   %s\n", e.CrackedPassword)
		}
		fmt.Fprintf(&b, "\nHash:\n%s\n", e.Hash)
	} else {
		summary = fmt.Sprintf("%s  %-15s %-6s %s", tstamp(e.Timestamp), host(e.Source), e.Operation, e.DN)
		fmt.Fprintf(&b, "DN:        %s\n", e.DN)
		if e.Password != "" {
			fmt.Fprintf(&b, "Password:  %s\n", e.Password)
		}
	}
	m.addRow(paneLDAP, eventRow{summary: summary, detail: b.String(), accent: lipgloss.Color("5")})
}

// refreshShells rebuilds the SHELLS pane from the catcher manager so it
// reflects the live set of active reverse-shell sessions.
func (m *model) refreshShells() {
	if m.mgr == nil {
		return
	}
	p := m.panes[paneShells]
	prevSel := p.sel
	p.rows = p.rows[:0]
	for _, ln := range m.mgr.GetListeners() {
		for _, s := range ln.Sessions {
			summary := fmt.Sprintf("listener %s:%d  session %s  %s",
				ln.IP, ln.Port, shortID(s.ID), s.RemoteAddr)
			detail := fmt.Sprintf("Listener:   %s:%d (%s)\nSession:    %s\nRemoteAddr: %s\n",
				ln.IP, ln.Port, ln.ID, s.ID, s.RemoteAddr)
			p.add(eventRow{summary: summary, detail: detail, accent: lipgloss.Color("1")})
		}
	}
	sort.SliceStable(p.rows, func(i, j int) bool { return p.rows[i].summary < p.rows[j].summary })
	if prevSel < len(p.rows) {
		p.sel = prevSel
	} else if len(p.rows) > 0 {
		p.sel = len(p.rows) - 1
	} else {
		p.sel = 0
	}
}

// --- view -------------------------------------------------------------------

var (
	headerStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(lipgloss.Color("6")).Padding(0, 1)
	tabActive     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(lipgloss.Color("7")).Padding(0, 1)
	tabInactive   = lipgloss.NewStyle().Foreground(lipgloss.Color("7")).Padding(0, 1)
	footerStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	selectedStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(lipgloss.Color("7"))
)

func (m *model) View() string {
	if m.width == 0 {
		return "starting goshs dashboard..."
	}
	var b strings.Builder
	b.WriteString(m.header())
	b.WriteString("\n")
	b.WriteString(m.tabs())
	b.WriteString("\n")
	b.WriteString(m.bodyView())
	b.WriteString("\n")
	b.WriteString(m.footer())
	return b.String()
}

func (m *model) header() string {
	scheme := "http"
	if m.opts.SSL {
		scheme = "https"
	}
	auth := "off"
	if m.opts.BasicAuth != "" || m.opts.CertAuth != "" {
		auth = "on"
	}
	parts := []string{
		fmt.Sprintf("goshs  %s://%s:%d", scheme, m.opts.IP, m.opts.Port),
		"auth:" + auth,
	}
	if !m.deadline.IsZero() {
		parts = append(parts, "ttl "+remaining(m.deadline))
	}
	for _, s := range collabBadges(m.opts) {
		parts = append(parts, s)
	}
	line := strings.Join(parts, "  ·  ")
	return headerStyle.Width(m.width).Render(trunc(line, m.width-2))
}

func collabBadges(o *options.Options) []string {
	var out []string
	if o.DNS {
		out = append(out, "dns")
	}
	if o.SMTP {
		out = append(out, "smtp")
	}
	if o.SMB {
		out = append(out, "smb")
	}
	if o.LDAP {
		out = append(out, "ldap")
	}
	if o.Catcher {
		out = append(out, "catcher")
	}
	return out
}

func (m *model) tabs() string {
	var cells []string
	for i, p := range m.panes {
		label := fmt.Sprintf("%s(%d)", p.name, len(p.rows))
		if i == m.active {
			cells = append(cells, tabActive.Render(label))
		} else {
			cells = append(cells, tabInactive.Render(label))
		}
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, cells...)
}

// bodyHeight is the number of rows available between the header/tabs and the
// footer (3 chrome lines above, 1 below).
func (m *model) bodyHeight() int {
	h := m.height - 4
	if h < 1 {
		return 1
	}
	return h
}

func (m *model) bodyView() string {
	p := m.panes[m.active]
	if m.detail {
		return m.detailView(p)
	}
	if len(p.rows) == 0 {
		return lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Render("  (no events yet)")
	}

	h := m.bodyHeight()
	// Keep the selected row within the visible window.
	if p.sel < p.top {
		p.top = p.sel
	}
	if p.sel >= p.top+h {
		p.top = p.sel - h + 1
	}

	var b strings.Builder
	end := p.top + h
	if end > len(p.rows) {
		end = len(p.rows)
	}
	for i := p.top; i < end; i++ {
		line := trunc(p.rows[i].summary, m.width-2)
		if i == p.sel {
			b.WriteString(selectedStyle.Width(m.width).Render(line))
		} else {
			b.WriteString(lipgloss.NewStyle().Foreground(p.rows[i].accent).Render(line))
		}
		if i < end-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}

func (m *model) detailView(p *pane) string {
	if p.sel >= len(p.rows) {
		return ""
	}
	title := lipgloss.NewStyle().Bold(true).Render(fmt.Sprintf("%s detail (esc/enter to close)", p.name))
	width := m.width - 2
	if width < 1 {
		width = 1
	}
	return title + "\n\n" + hardWrap(p.rows[p.sel].detail, width)
}

// hardWrap re-flows a detail body so no rendered line exceeds width. Unlike
// word wrapping it also breaks single long tokens (e.g. a captured NTLM hash,
// which contains no spaces) so they no longer run off the right edge.
func hardWrap(s string, width int) string {
	var out strings.Builder
	lines := strings.Split(s, "\n")
	for li, line := range lines {
		if li > 0 {
			out.WriteByte('\n')
		}
		for len([]rune(line)) > width {
			r := []rune(line)
			out.WriteString(string(r[:width]))
			out.WriteByte('\n')
			line = string(r[width:])
		}
		out.WriteString(line)
	}
	return out.String()
}

func (m *model) footer() string {
	hint := "tab/←→ switch · ↑↓/jk scroll · enter detail · q quit"
	return footerStyle.Width(m.width).Render(trunc(hint, m.width-1))
}

// --- helpers ----------------------------------------------------------------

// tstamp formats an event time for the one-line summaries. It includes the
// date as well as the time so that, on long-running instances, events from
// previous days remain distinguishable at a glance.
func tstamp(t time.Time) string { return t.Format("01-02 15:04:05") }

func host(addr string) string {
	if i := strings.LastIndex(addr, ":"); i > 0 {
		return addr[:i]
	}
	return addr
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func remaining(deadline time.Time) string {
	d := time.Until(deadline).Round(time.Second)
	if d < 0 {
		d = 0
	}
	return d.String()
}

func trunc(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}
