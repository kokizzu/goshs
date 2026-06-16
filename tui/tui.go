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
	"goshs.de/goshs/v2/goshsversion"
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
	name      string
	icon      string
	rows      []eventRow
	sel       int
	top       int
	detailTop int // scroll offset within the open detail body
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
	detailH  int       // last rendered detail viewport height (for paging)
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
			{name: "HTTP", icon: "🌐"},
			{name: "DNS", icon: "📡"},
			{name: "SMB", icon: "🔑"},
			{name: "LDAP", icon: "📇"},
			{name: "SMTP", icon: "✉️"},
			{name: "SHELLS", icon: "🐚"},
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
			p.detailTop = 0
		}
	case "down", "j":
		p := m.panes[m.active]
		if p.sel < len(p.rows)-1 {
			p.sel++
			p.detailTop = 0
		}
	case "home", "g":
		// Jump to the newest event (row 0). In detail view this surfaces any
		// events that arrived while the operator was reading an older one.
		p := m.panes[m.active]
		p.sel = 0
		p.detailTop = 0
	case "end", "G":
		p := m.panes[m.active]
		if len(p.rows) > 0 {
			p.sel = len(p.rows) - 1
			p.detailTop = 0
		}
	case "pgup", "ctrl+u":
		// Scroll the open detail body up; the view clamps the offset.
		p := m.panes[m.active]
		p.detailTop -= m.detailStep()
		if p.detailTop < 0 {
			p.detailTop = 0
		}
	case "pgdown", "ctrl+d", " ":
		m.panes[m.active].detailTop += m.detailStep()
	case "enter":
		p := m.panes[m.active]
		if len(p.rows) > 0 {
			m.detail = !m.detail
			p.detailTop = 0
		}
	case "esc":
		m.detail = false
	}
	return m, nil
}

// detailStep is how many lines pgup/pgdown scroll the detail body by — close
// to a full page based on the last rendered detail height.
func (m *model) detailStep() int {
	if m.detailH > 1 {
		return m.detailH - 1
	}
	return 10
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
	accent := lipgloss.Color(nord14) // green: 2xx
	if e.Status >= 400 {
		accent = lipgloss.Color(nord11) // red: 4xx/5xx
	} else if e.Status >= 300 {
		accent = lipgloss.Color(nord9) // blue: 3xx
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
	m.addRow(paneDNS, eventRow{summary: summary, detail: detail, accent: lipgloss.Color(nord8)})
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
	m.addRow(paneSMTP, eventRow{summary: summary, detail: b.String(), accent: lipgloss.Color(nord13)})
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
	m.addRow(paneSMB, eventRow{summary: summary, detail: b.String(), accent: lipgloss.Color(nord15)})
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
	m.addRow(paneLDAP, eventRow{summary: summary, detail: b.String(), accent: lipgloss.Color(nord9)})
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
			p.add(eventRow{summary: summary, detail: detail, accent: lipgloss.Color(nord11)})
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

// Nord palette (https://www.nordtheme.com) — the same theme the web UI uses,
// so the TUI and browser dashboards share a look.
const (
	nord0  = "#2E3440" // polar night (background)
	nord1  = "#3B4252" // status-bar background
	nord3  = "#4C566A" // muted / dim
	nord4  = "#D8DEE9" // snow storm (foreground)
	nord6  = "#ECEFF4" // brightest foreground
	nord8  = "#88C0D0" // frost (primary accent)
	nord9  = "#81A1C1" // frost (3xx / ldap)
	nord11 = "#BF616A" // aurora red (errors / shells)
	nord13 = "#EBCB8B" // aurora yellow (smtp)
	nord14 = "#A3BE8C" // aurora green (2xx)
	nord15 = "#B48EAD" // aurora purple (smb)
)

var (
	bannerStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(nord8))
	bannerSubtle  = lipgloss.NewStyle().Foreground(lipgloss.Color(nord3))
	tabActive     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(nord0)).Background(lipgloss.Color(nord8)).Padding(0, 1)
	tabInactive   = lipgloss.NewStyle().Foreground(lipgloss.Color(nord4)).Background(lipgloss.Color(nord1)).Padding(0, 1)
	selectedStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(nord0)).Background(lipgloss.Color(nord8))
	dimStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color(nord3))
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(nord8))
	statusStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color(nord4)).Background(lipgloss.Color(nord1))
	controlsStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(nord6)).Background(lipgloss.Color(nord3))
	scrollThumb   = lipgloss.NewStyle().Foreground(lipgloss.Color(nord8))
	scrollTrack   = lipgloss.NewStyle().Foreground(lipgloss.Color(nord3))
)

// bannerArt is the goshs figlet logo, mirroring logger.PrintBanner so the
// dashboard greets the operator with the same brand mark as the CLI.
var bannerArt = []string{
	`  __ _  ___  ___| |__  ___ `,
	` / _` + "`" + ` |/ _ \/ __| '_ \/ __|`,
	`| (_| | (_) \__ \ | | \__ \`,
	` \__, |\___/|___/_| |_|___/`,
	`  __/ |`,
	` |___/`,
}

func (m *model) View() string {
	if m.width == 0 || m.height == 0 {
		return "starting goshs dashboard..."
	}
	banner := m.banner()
	tabs := m.tabs()
	status := m.statusBar()
	chrome := lipgloss.Height(banner) + lipgloss.Height(tabs) + lipgloss.Height(status)
	bodyH := m.height - chrome
	if bodyH < 1 {
		bodyH = 1
	}
	body := m.bodyView(bodyH)
	return strings.Join([]string{banner, tabs, body, status}, "\n")
}

// banner renders the goshs logo at the top. On short terminals it collapses to
// a single line so the event body keeps as much room as possible.
func (m *model) banner() string {
	if m.height < 18 || m.width < 30 {
		return bannerStyle.Render("goshs ") + bannerSubtle.Render(goshsversion.GoshsVersion)
	}
	var b strings.Builder
	for i, ln := range bannerArt {
		b.WriteString(bannerStyle.Render(ln))
		if i == len(bannerArt)-1 {
			b.WriteString(bannerSubtle.Render("   " + goshsversion.GoshsVersion))
		} else {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func (m *model) tabs() string {
	var cells []string
	for i, p := range m.panes {
		label := fmt.Sprintf("%s %s(%d)", p.icon, p.name, len(p.rows))
		if i == m.active {
			cells = append(cells, tabActive.Render(label))
		} else {
			cells = append(cells, tabInactive.Render(label))
		}
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, cells...)
}

// statusBar renders the bottom chrome: a wrapping info bar of server facts and
// a context-sensitive controls line, both filling the terminal width.
func (m *model) statusBar() string {
	info := statusStyle.Width(m.width).Render(strings.Join(m.statusSegments(), "   "))
	controls := controlsStyle.Width(m.width).Render(trunc(m.controlsHint(), m.width))
	return info + "\n" + controls
}

// statusSegments collects the server facts shown in the status bar. Most come
// straight from the parsed options; the live tunnel URL and shared-link count
// live on the FileServer at runtime and are not surfaced here.
func (m *model) statusSegments() []string {
	o := m.opts
	scheme := "http"
	if o.SSL {
		scheme = "https"
	}
	var seg []string
	add := func(s string) { seg = append(seg, s) }

	add("🏷 " + goshsversion.GoshsVersion)
	add(fmt.Sprintf("🔗 %s://%s:%d", scheme, o.IP, o.Port))
	add("📁 " + o.Webroot)
	if o.UploadFolder != "" && o.UploadFolder != o.Webroot {
		add("📥 " + o.UploadFolder)
	}
	if !m.deadline.IsZero() {
		add("⏳ ttl " + remaining(m.deadline))
	}

	if o.Username != "" {
		add("🔒 auth " + o.Username)
	} else if o.BasicAuth != "" || o.CertAuth != "" {
		add("🔒 auth on")
	}
	if o.DropUser != "" {
		add("👤 " + o.DropUser)
	}

	var flags []string
	if o.ReadOnly {
		flags = append(flags, "read-only")
	}
	if o.UploadOnly {
		flags = append(flags, "upload-only")
	}
	if o.NoDelete {
		flags = append(flags, "no-delete")
	}
	if len(flags) > 0 {
		add("🚫 " + strings.Join(flags, ","))
	}

	if o.WebDav {
		add(fmt.Sprintf("🗂 webdav :%d", o.WebDavPort))
	}
	if o.CLI {
		add("⌨ cli")
	}
	if o.Tunnel {
		add("🌍 tunnel")
	}
	if o.DNS {
		add(fmt.Sprintf("📡 dns :%d", o.DNSPort))
	}
	if o.SMTP {
		add(fmt.Sprintf("✉ smtp :%d", o.SMTPPort))
	}
	if o.SMB {
		s := fmt.Sprintf("🔑 smb :%d", o.SMBPort)
		if o.SMBDomain != "" || o.SMBShare != "" {
			s += fmt.Sprintf(" %s\\%s", o.SMBDomain, o.SMBShare)
		}
		add(s)
	}
	if o.LDAP {
		add(fmt.Sprintf("📇 ldap :%d", o.LDAPPort))
	}
	if o.Catcher {
		add("🐚 catcher")
	}
	return seg
}

func (m *model) controlsHint() string {
	if m.detail {
		return "↑↓ event · PgUp/PgDn scroll · g/G newest/oldest · esc close · q quit"
	}
	return "⇄ Tab/←→ panes · ↑↓ scroll · ⏎ detail · g/G top/bottom · q quit"
}

// bodyView renders exactly h lines for the active pane so the status bar stays
// pinned to the bottom of the screen. The list view gets a vertical scrollbar
// in the rightmost column whenever the events overflow the viewport.
func (m *model) bodyView(h int) string {
	p := m.panes[m.active]
	if m.detail {
		return m.detailView(p, h)
	}
	if len(p.rows) == 0 {
		return padLines([]string{dimStyle.Render("  (no events yet)")}, h)
	}

	// Keep the selected row within the visible window.
	if p.sel < p.top {
		p.top = p.sel
	}
	if p.sel >= p.top+h {
		p.top = p.sel - h + 1
	}
	if p.top < 0 {
		p.top = 0
	}

	bar := scrollColumn(len(p.rows), p.top, h)
	contentW := m.width
	if bar != nil {
		contentW = m.width - 1
	}
	if contentW < 1 {
		contentW = 1
	}

	end := p.top + h
	if end > len(p.rows) {
		end = len(p.rows)
	}
	var lines []string
	for i := p.top; i < end; i++ {
		line := trunc(p.rows[i].summary, contentW)
		st := lipgloss.NewStyle().Foreground(p.rows[i].accent)
		if i == p.sel {
			st = selectedStyle
		}
		lines = append(lines, st.Width(contentW).Render(line))
	}
	for len(lines) < h {
		lines = append(lines, strings.Repeat(" ", contentW))
	}
	if bar != nil {
		for i := range lines {
			lines[i] += scrollCell(bar, i)
		}
	}
	return strings.Join(lines, "\n")
}

// scrollColumn returns a per-row mask marking where the scrollbar thumb sits,
// or nil when all content fits (total <= viewH) and no bar is needed.
func scrollColumn(total, top, viewH int) []bool {
	if viewH <= 0 || total <= viewH {
		return nil
	}
	mask := make([]bool, viewH)
	thumb := viewH * viewH / total
	if thumb < 1 {
		thumb = 1
	}
	pos := 0
	if maxTop := total - viewH; maxTop > 0 {
		pos = (viewH - thumb) * top / maxTop
	}
	for i := 0; i < thumb && pos+i < viewH; i++ {
		mask[pos+i] = true
	}
	return mask
}

// scrollCell renders one cell of the scrollbar track for row i.
func scrollCell(bar []bool, i int) string {
	if i < len(bar) && bar[i] {
		return scrollThumb.Render("█")
	}
	return scrollTrack.Render("│")
}

// padLines pads (or clips) a slice of rendered lines to exactly h lines and
// joins them, so a section fills its reserved height.
func padLines(lines []string, h int) string {
	for len(lines) < h {
		lines = append(lines, "")
	}
	if len(lines) > h {
		lines = lines[:h]
	}
	return strings.Join(lines, "\n")
}

// padRight pads a plain (ANSI-free) string with spaces to w display columns.
func padRight(s string, w int) string {
	if n := len([]rune(s)); n < w {
		return s + strings.Repeat(" ", w-n)
	}
	return s
}

// detailView renders the inspected event in exactly h lines: a title line
// (with a position badge) plus a scrollable, hard-wrapped body. The badge
// surfaces that newer events have arrived while the operator was reading this
// one — rows are newest-first, so any row above the selection (index < p.sel)
// is newer. The count ticks up live as traffic comes in even though the
// inspected event stays anchored; ↑ (or home/g) jumps to them.
func (m *model) detailView(p *pane, h int) string {
	if p.sel >= len(p.rows) {
		return padLines(nil, h)
	}
	badge := fmt.Sprintf("%d/%d", p.sel+1, len(p.rows))
	if p.sel > 0 {
		badge += fmt.Sprintf(" · ↑ %d newer", p.sel)
	}
	title := titleStyle.Render(fmt.Sprintf("%s %s detail  %s", p.icon, p.name, badge))

	width := m.width - 1 // reserve the rightmost column for the scrollbar
	if width < 1 {
		width = 1
	}
	body := strings.Split(hardWrap(p.rows[p.sel].detail, width), "\n")

	bodyH := h - 1 // title consumes one line
	if bodyH < 1 {
		bodyH = 1
	}
	m.detailH = bodyH

	// Clamp the scroll offset to the content and write it back so the paging
	// keys stay bounded across renders.
	maxTop := len(body) - bodyH
	if maxTop < 0 {
		maxTop = 0
	}
	if p.detailTop > maxTop {
		p.detailTop = maxTop
	}
	if p.detailTop < 0 {
		p.detailTop = 0
	}

	bar := scrollColumn(len(body), p.detailTop, bodyH)
	end := p.detailTop + bodyH
	if end > len(body) {
		end = len(body)
	}
	lines := []string{title}
	for i := p.detailTop; i < end; i++ {
		ln := body[i]
		if bar != nil {
			ln = padRight(ln, width) + scrollCell(bar, i-p.detailTop)
		}
		lines = append(lines, ln)
	}
	return padLines(lines, h)
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

// --- helpers ----------------------------------------------------------------

// tstamp formats an event time for the one-line summaries. It includes the
// full date (year as well) plus the time so that, on long-running instances,
// events from previous days or years remain distinguishable at a glance.
func tstamp(t time.Time) string { return t.Format("2006-01-02 15:04:05") }

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
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n == 1 {
		return string(r[:1])
	}
	return string(r[:n-1]) + "…"
}
