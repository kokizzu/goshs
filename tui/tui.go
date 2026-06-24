// Package tui implements the optional interactive terminal dashboard for
// goshs. It is a second, in-process consumer of the WebSocket hub: it
// subscribes to the same broadcast stream and history that the browser Collab
// tab uses and renders it as a live full-screen view, so an operator running
// goshs headless over SSH gets situational awareness without port-forwarding
// the web UI.
package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"goshs.de/goshs/v2/catcher"
	"goshs.de/goshs/v2/clipboard"
	"goshs.de/goshs/v2/goshsversion"
	"goshs.de/goshs/v2/options"
	"goshs.de/goshs/v2/smtpattach"
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
func Run(opts *options.Options, hub *ws.Hub, mgr *catcher.Manager, clip *clipboard.Clipboard, tunnelURL func() string, ttlC <-chan time.Time) error {
	sub := hub.Subscribe()
	defer hub.Unsubscribe(sub)

	m := newModel(opts, hub, mgr, sub, clip, tunnelURL, ttlC)
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

// row kinds for the SHELLS pane, distinguishing a listener row from a session
// row so key actions (attach, kill, stop, restart) target the right object.
const (
	rowEvent    = ""         // ordinary collab event (default)
	rowListener = "listener" // a catcher listener
	rowSession  = "session"  // a connected reverse-shell session under a listener
)

type eventRow struct {
	summary string
	detail  string
	accent  lipgloss.Color
	kind    string          // "" for events; rowListener/rowSession in the SHELLS pane
	raw     json.RawMessage // original event JSON, for export parity with the web UI
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
	paneGenerator
	paneClipboard
)

type model struct {
	opts *options.Options
	hub  *ws.Hub
	mgr  *catcher.Manager
	clip *clipboard.Clipboard
	sub  chan []byte

	tunnelURL func() string    // live public tunnel URL getter; nil when unavailable
	ttl       <-chan time.Time // self-destruct trigger; nil when --ttl unset

	panes    []*pane
	active   int
	width    int
	height   int
	deadline time.Time // zero when no --ttl set
	detail   bool      // detail view of the selected row is open
	detailH  int       // last rendered detail viewport height (for paging)

	flash       string    // transient status message (e.g. export result)
	flashExpiry time.Time // when the flash message should clear

	inputActive bool                 // single-line text input mode is open
	inputBuf    string               // text typed so far in input mode
	inputPrompt string               // label shown in the status bar while typing
	inputSubmit func(string) tea.Cmd // invoked with the trimmed buffer on enter

	// Reverse-shell generator state (GENERATOR pane). Mirrors the web UI
	// catcher generator: a selected payload template plus LHOST/LPORT and an
	// output encoding, all editable in place.
	genSel  int      // index into shellDB of the selected payload
	genIP   string   // LHOST substituted into the template
	genPort string   // LPORT substituted into the template
	genEnc  encoding // output encoding (none / url / base64)
}

func newModel(opts *options.Options, hub *ws.Hub, mgr *catcher.Manager, sub chan []byte, clip *clipboard.Clipboard, tunnelURL func() string, ttlC <-chan time.Time) *model {
	var deadline time.Time
	if opts.TTL > 0 {
		deadline = time.Now().Add(opts.TTL)
	}
	return &model{
		opts:      opts,
		hub:       hub,
		mgr:       mgr,
		clip:      clip,
		sub:       sub,
		tunnelURL: tunnelURL,
		ttl:       ttlC,
		deadline:  deadline,
		genIP:     defaultLHOST(opts),
		genPort:   "4444",
		panes: []*pane{
			{name: "HTTP", icon: "🌐"},
			{name: "DNS", icon: "📡"},
			{name: "SMB", icon: "🔑"},
			{name: "LDAP", icon: "📇"},
			{name: "SMTP", icon: "📨"},
			{name: "SHELLS", icon: "🐚"},
			{name: "GENERATOR", icon: "⚡"},
			{name: "CLIPBOARD", icon: "📋"},
		},
	}
}

// defaultLHOST picks a sensible LHOST for the generator: the bound IP when it is
// a concrete address, mirroring the server's --tpl-var LHOST derivation; a
// placeholder otherwise (e.g. when bound to 0.0.0.0) so the operator can edit it.
func defaultLHOST(opts *options.Options) string {
	if opts.IP != "" && opts.IP != "0.0.0.0" {
		return opts.IP
	}
	return "10.10.10.10"
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

	case shellClosedMsg:
		// Returned after detaching from (or losing) an attached session.
		m.refreshShells()
		if msg.err != nil {
			m.setFlash("shell session ended: " + msg.err.Error())
		} else {
			m.setFlash("returned from shell session")
		}
		return m, nil

	case ttlExpiredMsg:
		// Self-destruct fired; quitting returns control to main, which runs
		// the graceful shutdown.
		return m, tea.Quit

	case tickMsg:
		m.refreshShells()
		m.refreshClipboard()
		if !m.flashExpiry.IsZero() && time.Now().After(m.flashExpiry) {
			m.flash = ""
			m.flashExpiry = time.Time{}
		}
		return m, tick()
	}
	return m, nil
}

func (m *model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// While typing a new clipboard entry, keys feed the input buffer instead of
	// driving navigation.
	if m.inputActive {
		return m.handleInputKey(msg)
	}
	// The GENERATOR pane is a form, not an event log, so it owns selection and
	// edit keys; only keys it does not handle (pane switching, quit) fall
	// through to the shared navigation below.
	if m.active == paneGenerator {
		if cmd, handled := m.handleGeneratorKey(msg); handled {
			return m, cmd
		}
	}
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
	case "e":
		// Export the active pane to goshs-<proto>-log.json.
		m.exportPane(m.active)
	case "E":
		// Export all panes to goshs-all-logs.json.
		m.exportAll()
	case "a":
		// Clipboard: add an entry. Shells: start a listener. Both open the
		// single-line input prompt.
		switch m.active {
		case paneClipboard:
			if m.clip != nil {
				m.beginInput("add entry", func(text string) tea.Cmd { return m.addClipboardEntry(text) })
			}
		case paneShells:
			if m.mgr != nil {
				m.beginInput("start listener (ip:port or port)", func(text string) tea.Cmd {
					return m.startListener(text)
				})
			}
		}
	case "d":
		// Clipboard: delete the selected entry. Shells: stop the selected
		// listener.
		switch m.active {
		case paneClipboard:
			return m, m.deleteClipboardEntry()
		case paneShells:
			return m, m.shellDelete()
		}
	case "r":
		// Restart the selected listener on the same ip:port (shells pane only).
		if m.active == paneShells {
			return m, m.restartSelectedListener()
		}
	case "i":
		// Attach to the selected reverse-shell session (shells pane only).
		if m.active == paneShells {
			return m, m.attachSelectedSession()
		}
	case "u":
		// Stabilise the selected shell as a Unix PTY (shells pane only).
		if m.active == paneShells {
			return m, m.upgradeSelectedSession(false)
		}
	case "U":
		// Stabilise the selected shell via Windows ConPtyShell (shells pane only).
		if m.active == paneShells {
			return m, m.upgradeSelectedSession(true)
		}
	case "C":
		// Clear the whole clipboard (clipboard pane only).
		if m.active == paneClipboard {
			return m, m.clearClipboard()
		}
	case "s":
		// Save the selected mail's attachments to disk (SMTP pane only).
		if m.active == paneSMTP {
			m.saveSMTPAttachments()
		}
	case "enter":
		// On a reverse-shell session row, ⏎ attaches; otherwise toggle detail.
		if m.active == paneShells {
			if cmd := m.attachSelectedSession(); cmd != nil {
				return m, cmd
			}
		}
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
	case "refreshClipboard":
		// A web client (or our own mutation) changed the shared clipboard;
		// rebuild the pane immediately rather than waiting for the next tick.
		m.refreshClipboard()
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
	m.addRow(paneHTTP, eventRow{summary: summary, detail: b.String(), accent: accent, raw: raw})
}

func (m *model) addDNS(raw []byte) {
	var e ws.DNSEvent
	if json.Unmarshal(raw, &e) != nil {
		return
	}
	summary := fmt.Sprintf("%s  %-15s %-5s %s", tstamp(e.Time), host(e.Source), e.QType, e.Name)
	detail := fmt.Sprintf("Time:   %s\nSource: %s\nType:   %s\nName:   %s\n",
		e.Time.Format(time.RFC3339), e.Source, e.QType, e.Name)
	m.addRow(paneDNS, eventRow{summary: summary, detail: detail, accent: lipgloss.Color(nord8), raw: raw})
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
	if len(e.Attachments) > 0 {
		b.WriteString("         (press s to save attachments to disk)\n")
	}
	// Prefer the plaintext part; fall back to rendering the HTML body as text so
	// HTML-only mail is no longer blank in the TUI.
	switch {
	case e.Body != "":
		fmt.Fprintf(&b, "\n%s\n", e.Body)
	case e.HTMLBody != "":
		fmt.Fprintf(&b, "\n[rendered from HTML]\n%s\n", htmlToText(e.HTMLBody))
	}
	m.addRow(paneSMTP, eventRow{summary: summary, detail: b.String(), accent: lipgloss.Color(nord13), raw: raw})
}

// saveSMTPAttachments writes the selected mail's attachments to a
// goshs-attachments directory under the working dir, pulling the bytes from the
// in-process attachment store. Attachments may have been purged on a
// long-running instance, in which case they are skipped.
func (m *model) saveSMTPAttachments() {
	p := m.panes[paneSMTP]
	if len(p.rows) == 0 {
		return
	}
	var e ws.SMTPEvent
	if json.Unmarshal(p.rows[p.sel].raw, &e) != nil {
		m.setFlash("could not read selected mail")
		return
	}
	if len(e.Attachments) == 0 {
		m.setFlash("no attachments on this mail")
		return
	}

	dir, err := os.Getwd()
	if err != nil {
		dir = "."
	}
	dir = filepath.Join(dir, "goshs-attachments")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		m.setFlash("save failed: " + err.Error())
		return
	}

	saved := 0
	for _, att := range e.Attachments {
		a, ok := smtpattach.Get(att.ID)
		if !ok {
			continue // expired or purged from the store
		}
		// Prefix with a short ID so attachments sharing a filename (or matching
		// an existing file in the directory) cannot clobber one another.
		name := fmt.Sprintf("%s-%s", shortID(att.ID), safeFilename(att.Filename))
		if err := os.WriteFile(filepath.Join(dir, name), a.Data, 0o644); err != nil {
			m.setFlash("save failed: " + err.Error())
			return
		}
		saved++
	}
	if saved == 0 {
		m.setFlash("attachments expired — nothing saved")
		return
	}
	m.setFlash(fmt.Sprintf("saved %d attachment(s) → %s", saved, dir))
}

// safeFilename reduces an attachment filename to a single safe path component
// so a malicious name cannot escape the target directory.
func safeFilename(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	if name == "" || name == "." || name == ".." || name == string(filepath.Separator) {
		return "attachment"
	}
	return name
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
	m.addRow(paneSMB, eventRow{summary: summary, detail: b.String(), accent: lipgloss.Color(nord15), raw: raw})
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
	m.addRow(paneLDAP, eventRow{summary: summary, detail: b.String(), accent: lipgloss.Color(nord9), raw: raw})
}

// refreshShells rebuilds the SHELLS pane from the catcher manager. Each row is
// a listener (so idle listeners with no connections are still visible and
// selectable for stopping); the detail view lists that listener's sessions.
func (m *model) refreshShells() {
	if m.mgr == nil {
		return
	}
	p := m.panes[paneShells]
	prevSel := p.sel
	p.rows = p.rows[:0]
	listeners := m.mgr.GetListeners()
	sort.SliceStable(listeners, func(i, j int) bool {
		if listeners[i].Port != listeners[j].Port {
			return listeners[i].Port < listeners[j].Port
		}
		return listeners[i].IP < listeners[j].IP
	})
	for _, ln := range listeners {
		accent := lipgloss.Color(nord8) // listening, idle
		if len(ln.Sessions) > 0 {
			accent = lipgloss.Color(nord14) // has live sessions
		}
		summary := fmt.Sprintf("%-21s  %d session(s)", fmt.Sprintf("%s:%d", ln.IP, ln.Port), len(ln.Sessions))

		var d strings.Builder
		fmt.Fprintf(&d, "Listener: %s:%d\nID:       %s\nSessions: %d\n", ln.IP, ln.Port, ln.ID, len(ln.Sessions))
		for _, s := range ln.Sessions {
			fmt.Fprintf(&d, "  • %s  (session %s)\n", s.RemoteAddr, shortID(s.ID))
		}
		raw, _ := json.Marshal(ln)
		p.rows = append(p.rows, eventRow{summary: summary, detail: d.String(), accent: accent, kind: rowListener, raw: raw})

		// One row per connected session, so each shell is selectable to attach
		// to or kill individually.
		for _, s := range ln.Sessions {
			sraw, _ := json.Marshal(s)
			ssummary := fmt.Sprintf("    ↳ %-15s  session %s", s.RemoteAddr, shortID(s.ID))
			sdetail := fmt.Sprintf("Session:   %s\nRemote:    %s\nListener:  %s:%d\n\nPress ⏎/i to attach (Ctrl+] detaches), u upgrade to PTY,\nU ConPtyShell (Windows), d to kill.\n", s.ID, s.RemoteAddr, ln.IP, ln.Port)
			p.rows = append(p.rows, eventRow{summary: ssummary, detail: sdetail, accent: lipgloss.Color(nord14), kind: rowSession, raw: sraw})
		}
	}
	if prevSel < len(p.rows) {
		p.sel = prevSel
	} else if len(p.rows) > 0 {
		p.sel = len(p.rows) - 1
	} else {
		p.sel = 0
	}
}

// --- listeners --------------------------------------------------------------

// startListener parses "ip:port" or a bare "port" and starts a catcher
// listener, reporting the outcome via a flash message. On success it returns a
// command that broadcasts refreshCatcher so the web UI adopts the new listener.
func (m *model) startListener(addr string) tea.Cmd {
	if m.mgr == nil {
		return nil
	}
	ip, port, err := parseListenerAddr(addr)
	if err != nil {
		m.setFlash("invalid address: " + err.Error())
		return nil
	}
	info, err := m.mgr.StartListener(ip, port)
	if err != nil {
		m.setFlash("start listener failed: " + err.Error())
		return nil
	}
	m.refreshShells()
	m.setFlash(fmt.Sprintf("listener started on %s:%d", info.IP, info.Port))
	return m.broadcast("refreshCatcher")
}

// stopSelectedListener stops the listener under the selection in the SHELLS
// pane (identified by the ListenerInfo stored on the row). On success it
// returns a command that broadcasts refreshCatcher so the web UI drops it.
func (m *model) stopSelectedListener() tea.Cmd {
	if m.mgr == nil {
		return nil
	}
	p := m.panes[paneShells]
	if len(p.rows) == 0 {
		m.setFlash("no listener selected")
		return nil
	}
	var info catcher.ListenerInfo
	if json.Unmarshal(p.rows[p.sel].raw, &info) != nil || info.ID == "" {
		m.setFlash("could not identify listener")
		return nil
	}
	if err := m.mgr.StopListener(info.ID); err != nil {
		m.setFlash("stop listener failed: " + err.Error())
		return nil
	}
	m.refreshShells()
	m.setFlash(fmt.Sprintf("stopped listener %s:%d", info.IP, info.Port))
	return m.broadcast("refreshCatcher")
}

// shellDelete acts on the selected SHELLS row: a session row is killed, a
// listener row is stopped. Both broadcast refreshCatcher so the web UI follows.
func (m *model) shellDelete() tea.Cmd {
	if m.mgr == nil {
		return nil
	}
	p := m.panes[paneShells]
	if len(p.rows) == 0 {
		m.setFlash("nothing selected")
		return nil
	}
	row := p.rows[p.sel]
	if row.kind == rowSession {
		var info catcher.SessionInfo
		if json.Unmarshal(row.raw, &info) != nil || info.ID == "" {
			m.setFlash("could not identify session")
			return nil
		}
		if err := m.mgr.KillSession(info.ID); err != nil {
			m.setFlash("kill session failed: " + err.Error())
			return nil
		}
		m.refreshShells()
		m.setFlash("killed session " + shortID(info.ID))
		return m.broadcast("refreshCatcher")
	}
	return m.stopSelectedListener()
}

// restartSelectedListener stops the selected listener (or the parent of the
// selected session) and starts a fresh one on the same ip:port, then broadcasts
// refreshCatcher so the web UI re-adopts it.
func (m *model) restartSelectedListener() tea.Cmd {
	if m.mgr == nil {
		return nil
	}
	p := m.panes[paneShells]
	if len(p.rows) == 0 {
		m.setFlash("no listener selected")
		return nil
	}
	row := p.rows[p.sel]
	if row.kind != rowListener {
		m.setFlash("select a listener to restart")
		return nil
	}
	var info catcher.ListenerInfo
	if json.Unmarshal(row.raw, &info) != nil || info.ID == "" {
		m.setFlash("could not identify listener")
		return nil
	}
	if err := m.mgr.StopListener(info.ID); err != nil {
		m.setFlash("restart failed: " + err.Error())
		return nil
	}
	started, err := m.mgr.StartListener(info.IP, info.Port)
	if err != nil {
		m.setFlash("restart failed: " + err.Error())
		m.refreshShells()
		return m.broadcast("refreshCatcher")
	}
	m.refreshShells()
	m.setFlash(fmt.Sprintf("restarted listener on %s:%d", started.IP, started.Port))
	return m.broadcast("refreshCatcher")
}

// attachSelectedSession bridges the operator's terminal to the selected
// reverse-shell session. It returns nil (so callers can fall through to other
// behaviour, e.g. ⏎ toggling detail) when the selection is not a session row.
func (m *model) attachSelectedSession() tea.Cmd {
	if m.mgr == nil || m.active != paneShells {
		return nil
	}
	p := m.panes[paneShells]
	if len(p.rows) == 0 {
		return nil
	}
	row := p.rows[p.sel]
	if row.kind != rowSession {
		return nil
	}
	var info catcher.SessionInfo
	if json.Unmarshal(row.raw, &info) != nil || info.ID == "" {
		m.setFlash("could not identify session")
		return nil
	}
	sess := m.mgr.GetSession(info.ID)
	if sess == nil {
		m.setFlash("session no longer connected")
		return nil
	}
	return tea.Exec(&shellBridge{session: sess}, func(err error) tea.Msg {
		return shellClosedMsg{err: err}
	})
}

// upgradeSelectedSession stabilises the selected reverse-shell session,
// mirroring the web UI's upgrade buttons. The Unix path spawns a PTY via
// python/script; the Windows path pulls ConPtyShell from this server and
// hijacks the socket. Both run before attaching — the operator presses ⏎/i once
// the upgrade has landed. Returns nil so the key handler can fall through when
// the selection is not a session row.
func (m *model) upgradeSelectedSession(windows bool) tea.Cmd {
	if m.mgr == nil || m.active != paneShells {
		return nil
	}
	p := m.panes[paneShells]
	if len(p.rows) == 0 {
		return nil
	}
	row := p.rows[p.sel]
	if row.kind != rowSession {
		m.setFlash("select a session row to upgrade")
		return nil
	}
	var info catcher.SessionInfo
	if json.Unmarshal(row.raw, &info) != nil || info.ID == "" {
		m.setFlash("could not identify session")
		return nil
	}
	sess := m.mgr.GetSession(info.ID)
	if sess == nil {
		m.setFlash("session no longer connected")
		return nil
	}
	// The attached shell gets the whole terminal, so size the PTY to the full
	// dashboard dimensions (with sane fallbacks before the first resize).
	rows, cols := m.height, m.width
	if windows {
		upgradeWindows(sess, m.conPtyURL(sess), rows, cols)
		m.setFlash("sent ConPtyShell upgrade — press ⏎ to attach")
	} else {
		upgradeUnix(sess, rows, cols)
		m.setFlash("sent PTY upgrade — wait a moment, then ⏎ to attach")
	}
	return nil
}

// conPtyURL builds the ConPtyShell download URL for an upgrade. It prefers the
// local address the victim connected to (so it works even when goshs is bound
// to 0.0.0.0), falling back to the configured IP and finally to loopback.
func (m *model) conPtyURL(sess *catcher.Session) string {
	scheme := "http"
	if m.opts.SSL {
		scheme = "https"
	}
	host := sess.LocalIP()
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = m.opts.IP
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("%s://%s:%d/ConPtyShell.ps1?conpty", scheme, host, m.opts.Port)
}

// parseListenerAddr accepts "ip:port" or a bare "port" and returns the bind IP
// (defaulting to 0.0.0.0) and port.
func parseListenerAddr(s string) (string, int, error) {
	s = strings.TrimSpace(s)
	ip, portStr := "0.0.0.0", s
	if strings.Contains(s, ":") {
		host, p, err := net.SplitHostPort(s)
		if err != nil {
			return "", 0, err
		}
		if host != "" {
			ip = host
		}
		portStr = p
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, fmt.Errorf("port must be a number")
	}
	if port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("port out of range")
	}
	return ip, port, nil
}

// --- clipboard --------------------------------------------------------------

// refreshClipboard rebuilds the CLIPBOARD pane from the shared clipboard so it
// reflects entries added from the web UI as well as the TUI. Entries are kept
// newest-first (the clipboard prepends), matching the other panes.
func (m *model) refreshClipboard() {
	if m.clip == nil {
		return
	}
	p := m.panes[paneClipboard]
	prevSel := p.sel
	entries, _ := m.clip.GetEntries()
	p.rows = p.rows[:0]
	for _, e := range entries {
		summary := fmt.Sprintf("%s  %s", e.Time, oneLine(e.Content))
		detail := fmt.Sprintf("Time: %s\n\n%s\n", e.Time, e.Content)
		raw, _ := json.Marshal(e)
		p.rows = append(p.rows, eventRow{summary: summary, detail: detail, accent: lipgloss.Color(nord7), raw: raw})
	}
	if prevSel < len(p.rows) {
		p.sel = prevSel
	} else if len(p.rows) > 0 {
		p.sel = len(p.rows) - 1
	} else {
		p.sel = 0
	}
}

// beginInput opens the single-line text prompt with a label and a submit
// callback run (with the trimmed buffer) when the operator presses enter.
func (m *model) beginInput(prompt string, submit func(string) tea.Cmd) {
	m.inputActive = true
	m.inputBuf = ""
	m.inputPrompt = prompt
	m.inputSubmit = submit
	m.detail = false
}

// handleInputKey feeds keystrokes into the active text input and dispatches its
// submit callback on enter.
func (m *model) handleInputKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEnter:
		text := strings.TrimSpace(m.inputBuf)
		submit := m.inputSubmit
		m.inputActive, m.inputBuf, m.inputPrompt, m.inputSubmit = false, "", "", nil
		if text == "" || submit == nil {
			return m, nil
		}
		return m, submit(text)
	case tea.KeyEsc:
		m.inputActive, m.inputBuf, m.inputPrompt, m.inputSubmit = false, "", "", nil
	case tea.KeyBackspace, tea.KeyDelete:
		if r := []rune(m.inputBuf); len(r) > 0 {
			m.inputBuf = string(r[:len(r)-1])
		}
	case tea.KeySpace:
		m.inputBuf += " "
	case tea.KeyRunes:
		m.inputBuf += string(msg.Runes)
	}
	return m, nil
}

// addClipboardEntry stores a new shared-clipboard entry and tells web clients
// to re-fetch.
func (m *model) addClipboardEntry(text string) tea.Cmd {
	if m.clip == nil {
		return nil
	}
	if err := m.clip.AddEntry(text); err != nil {
		m.setFlash("clipboard add failed: " + err.Error())
		return nil
	}
	m.refreshClipboard()
	m.setFlash("added clipboard entry")
	return m.broadcast("refreshClipboard")
}

// deleteClipboardEntry removes the selected clipboard entry and tells web
// clients to re-fetch.
func (m *model) deleteClipboardEntry() tea.Cmd {
	if m.clip == nil {
		return nil
	}
	p := m.panes[paneClipboard]
	if len(p.rows) == 0 {
		return nil
	}
	// GetEntries and the pane share the same newest-first order, so the
	// selection index maps directly to the clipboard slice position.
	if err := m.clip.DeleteEntry(p.sel); err != nil {
		m.setFlash("clipboard delete failed: " + err.Error())
		return nil
	}
	m.refreshClipboard()
	m.setFlash("deleted clipboard entry")
	return m.broadcast("refreshClipboard")
}

// clearClipboard empties the shared clipboard and tells web clients to
// re-fetch.
func (m *model) clearClipboard() tea.Cmd {
	if m.clip == nil {
		return nil
	}
	if err := m.clip.ClearClipboard(); err != nil {
		m.setFlash("clipboard clear failed: " + err.Error())
		return nil
	}
	m.refreshClipboard()
	m.setFlash("cleared clipboard")
	return m.broadcast("refreshClipboard")
}

// broadcast notifies web clients (and our own subscription) of a state change
// of the given type (e.g. "refreshClipboard", "refreshCatcher") so they
// re-fetch. The send runs in a command so it never blocks the UI loop on the
// unbuffered Broadcast channel.
func (m *model) broadcast(typ string) tea.Cmd {
	if m.hub == nil {
		return nil
	}
	msg, err := json.Marshal(struct {
		Type string `json:"type"`
	}{typ})
	if err != nil {
		return nil
	}
	return func() tea.Msg {
		m.hub.Broadcast <- msg
		return nil
	}
}

// --- export -----------------------------------------------------------------

// exportPane writes a single pane's events to goshs-<proto>-log.json — a bare
// JSON array of the raw events, matching the web UI's per-tab export
// (exportHTTP/exportDNS/… in collab.js).
func (m *model) exportPane(idx int) {
	p := m.panes[idx]
	events := rawEvents(p)
	if len(events) == 0 {
		m.setFlash("nothing to export")
		return
	}
	name := fmt.Sprintf("goshs-%s-log.json", strings.ToLower(p.name))
	path, err := writeJSONExport(name, events)
	if err != nil {
		m.setFlash("export failed: " + err.Error())
		return
	}
	m.setFlash(fmt.Sprintf("exported %d %s event(s) → %s", len(events), p.name, path))
}

// exportAll writes goshs-all-logs.json — the same wrapper object the web UI's
// exportAllLogs() produces: {generatedAt, http, dns, smtp, smb, ldap}. As in
// the web UI the SHELLS pane is not part of the combined export (use its
// per-pane export for that).
func (m *model) exportAll() {
	all := struct {
		GeneratedAt string            `json:"generatedAt"`
		HTTP        []json.RawMessage `json:"http"`
		DNS         []json.RawMessage `json:"dns"`
		SMTP        []json.RawMessage `json:"smtp"`
		SMB         []json.RawMessage `json:"smb"`
		LDAP        []json.RawMessage `json:"ldap"`
	}{
		GeneratedAt: time.Now().Format(time.RFC3339),
		HTTP:        rawEvents(m.panes[paneHTTP]),
		DNS:         rawEvents(m.panes[paneDNS]),
		SMTP:        rawEvents(m.panes[paneSMTP]),
		SMB:         rawEvents(m.panes[paneSMB]),
		LDAP:        rawEvents(m.panes[paneLDAP]),
	}
	total := len(all.HTTP) + len(all.DNS) + len(all.SMTP) + len(all.SMB) + len(all.LDAP)
	if total == 0 {
		m.setFlash("nothing to export")
		return
	}
	path, err := writeJSONExport("goshs-all-logs.json", all)
	if err != nil {
		m.setFlash("export failed: " + err.Error())
		return
	}
	m.setFlash(fmt.Sprintf("exported %d event(s) → %s", total, path))
}

// rawEvents collects a pane's stored raw event JSON in on-screen order (newest
// first), skipping any rows without a raw payload.
func rawEvents(p *pane) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(p.rows))
	for _, r := range p.rows {
		if len(r.raw) > 0 {
			out = append(out, r.raw)
		}
	}
	return out
}

// writeJSONExport marshals v to pretty-printed JSON (2-space indent, matching
// the web UI's JSON.stringify(…, null, 2)) and writes it to name in the working
// directory, returning the absolute path.
func writeJSONExport(name string, v any) (string, error) {
	// Marshal compactly first, then re-indent so embedded json.RawMessage event
	// objects are expanded too while preserving their original key order.
	compact, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, compact, "", "  "); err != nil {
		return "", err
	}
	dir, err := os.Getwd()
	if err != nil {
		dir = "."
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, pretty.Bytes(), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// setFlash shows a transient status message for a few seconds; the tick handler
// clears it once flashExpiry passes.
func (m *model) setFlash(msg string) {
	m.flash = msg
	m.flashExpiry = time.Now().Add(4 * time.Second)
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
	nord7  = "#8FBCBB" // frost (clipboard)
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
	tabFiller     = lipgloss.NewStyle().Background(lipgloss.Color(nord1))
	selectedStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(nord0)).Background(lipgloss.Color(nord8))
	dimStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color(nord3))
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(nord8))
	statusStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color(nord4)).Background(lipgloss.Color(nord1))
	controlsStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(nord6)).Background(lipgloss.Color(nord3))
	flashStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(nord0)).Background(lipgloss.Color(nord14))
	inputStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(nord0)).Background(lipgloss.Color(nord13))
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

// --- generator --------------------------------------------------------------

// handleGeneratorKey processes keys specific to the GENERATOR pane and reports
// whether it consumed the event. Unhandled keys (tab/shift+tab, q, E, ...)
// return handled=false so the shared handler still drives pane switching,
// quitting and export-all.
func (m *model) handleGeneratorKey(msg tea.KeyMsg) (tea.Cmd, bool) {
	switch msg.String() {
	case "up", "k":
		if m.genSel > 0 {
			m.genSel--
		}
	case "down", "j":
		if m.genSel < len(shellDB)-1 {
			m.genSel++
		}
	case "home", "g":
		m.genSel = 0
	case "end", "G":
		m.genSel = len(shellDB) - 1
	case "i":
		m.beginInput("set LHOST", func(text string) tea.Cmd {
			if text != "" {
				m.genIP = text
			}
			return nil
		})
	case "p":
		m.beginInput("set LPORT", func(text string) tea.Cmd {
			if text != "" {
				m.genPort = text
			}
			return nil
		})
	case "n":
		m.genEnc = m.genEnc.next()
	case "enter", "e":
		// The form has no detail view and no per-pane log to export; swallow
		// these so ⏎ does not toggle detail and e does not write an empty file.
	default:
		return nil, false
	}
	return nil, true
}

// generatorView renders the GENERATOR pane: a selectable payload list on the
// left and the editable LHOST/LPORT/encoding fields plus the filled command and
// listener line on the right, in exactly h lines.
func (m *model) generatorView(h int) string {
	if len(shellDB) == 0 {
		return padLines(nil, h)
	}
	leftW := 26
	if max := m.width - 20; leftW > max {
		leftW = max
	}
	if leftW < 10 {
		leftW = 10
	}
	rightW := m.width - leftW - 1
	if rightW < 1 {
		rightW = 1
	}
	left := m.generatorList(leftW, h)
	right := m.generatorOutput(rightW, h)
	return lipgloss.JoinHorizontal(lipgloss.Top, left, sepColumn(h), right)
}

// generatorList renders the scrollable list of payload names, windowed so the
// selected entry stays visible, as exactly h lines of width w.
func (m *model) generatorList(w, h int) string {
	top := 0
	if m.genSel >= h {
		top = m.genSel - h + 1
	}
	end := top + h
	if end > len(shellDB) {
		end = len(shellDB)
	}
	var lines []string
	for i := top; i < end; i++ {
		marker, st := "  ", lipgloss.NewStyle().Foreground(lipgloss.Color(nord4))
		if i == m.genSel {
			marker, st = "▶ ", selectedStyle
		}
		lines = append(lines, st.Width(w).Render(trunc(marker+shellDB[i].name, w)))
	}
	blank := lipgloss.NewStyle().Width(w).Render("")
	for len(lines) < h {
		lines = append(lines, blank)
	}
	return strings.Join(lines, "\n")
}

// generatorOutput renders the right-hand panel: the LHOST/LPORT/encoding fields,
// the selected payload's name, the filled command (hard-wrapped), and the
// matching listener command, as exactly h lines of width w.
func (m *model) generatorOutput(w, h int) string {
	e := shellDB[m.genSel]
	cmd := generateCommand(e.tmpl, m.genIP, m.genPort, m.genEnc)

	field := func(k, v string) string { return dimStyle.Render(padRight(k, 7)) + trunc(v, w-7) }
	enc := m.genEnc.String()
	if strings.HasPrefix(e.tmpl, psB64Prefix) {
		enc = "powershell -e (encoding ignored)"
	}

	lines := []string{
		field("LHOST", m.genIP),
		field("LPORT", m.genPort),
		field("ENC", enc),
		"",
		titleStyle.Render(trunc("⚡ "+e.name, w)),
		"",
	}
	lines = append(lines, strings.Split(hardWrap(cmd, w), "\n")...)
	lines = append(lines, "", dimStyle.Render(trunc("Listener: nc -lvnp "+m.genPort, w)))
	return padLines(lines, h)
}

// sepColumn renders a 1-column vertical divider h lines tall.
func sepColumn(h int) string {
	line := dimStyle.Render("│")
	lines := make([]string, h)
	for i := range lines {
		lines[i] = line
	}
	return strings.Join(lines, "\n")
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
		// The GENERATOR pane is a form, not an event log, so it carries no count.
		label := fmt.Sprintf("%s %s(%d)", p.icon, p.name, len(p.rows))
		if i == paneGenerator {
			label = fmt.Sprintf("%s %s", p.icon, p.name)
		}
		if i == m.active {
			cells = append(cells, tabActive.Render(label))
		} else {
			cells = append(cells, tabInactive.Render(label))
		}
	}
	row := lipgloss.JoinHorizontal(lipgloss.Top, cells...)
	// Extend the bar to the full terminal width so it reads as a solid strip
	// rather than cutting off after the last tab.
	if pad := m.width - lipgloss.Width(row); pad > 0 {
		row += tabFiller.Render(strings.Repeat(" ", pad))
	}
	return row
}

// statusBar renders the bottom chrome: a wrapping info bar of server facts and
// a context-sensitive controls line, both filling the terminal width.
func (m *model) statusBar() string {
	info := statusStyle.Width(m.width).Render(strings.Join(m.statusSegments(), "   "))
	// The controls line is taken over by the add-entry prompt while typing, or
	// by a transient flash (e.g. an export result), so the operator sees the
	// current mode/outcome without leaving the view.
	line, style := m.controlsHint(), controlsStyle
	switch {
	case m.inputActive:
		line, style = m.inputPrompt+" (enter save · esc cancel): "+m.inputBuf+"▌", inputStyle
	case m.flash != "":
		line, style = m.flash, flashStyle
	}
	controls := style.Width(m.width).Render(trunc(line, m.width))
	return info + "\n" + controls
}

// statusSegments collects the server facts shown in the status bar. Most come
// straight from the parsed options; the tunnel URL is read live via the
// tunnelURL getter since it is assigned asynchronously once the tunnel is up.
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
		if url := m.tunnel(); url != "" {
			add("🌍 " + url)
		} else {
			add("🌍 tunnel (connecting)")
		}
	}
	if o.DNS {
		add(fmt.Sprintf("📡 dns :%d", o.DNSPort))
	}
	if o.SMTP {
		add(fmt.Sprintf("📨 smtp :%d", o.SMTPPort))
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
	if o.Template {
		tpl := "🧩 tpl"
		// Mirror the server's LHOST resolution: explicit --tpl-var wins, else the
		// bound single IP, else unset (templates using {{.LHOST}} will error).
		lhost := o.TemplateVarsParsed["LHOST"]
		if lhost == "" && o.IP != "" && o.IP != "0.0.0.0" {
			lhost = o.IP
		}
		if lhost != "" {
			tpl += " LHOST=" + lhost
		}
		// Show every other --tpl-var (LPORT and any arbitrary KEY=VALUE) so the
		// status line reflects the full template context, not just LHOST. LHOST is
		// handled above (it has extra IP-derivation logic), so skip it here.
		keys := make([]string, 0, len(o.TemplateVarsParsed))
		for k := range o.TemplateVarsParsed {
			if k == "LHOST" {
				continue
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			tpl += fmt.Sprintf(" %s=%s", k, o.TemplateVarsParsed[k])
		}
		add(tpl)
	}
	return seg
}

// tunnel returns the current public tunnel URL, or "" when no getter was
// supplied (e.g. in tests) or the tunnel has not connected yet.
func (m *model) tunnel() string {
	if m.tunnelURL == nil {
		return ""
	}
	return m.tunnelURL()
}

func (m *model) controlsHint() string {
	if m.detail {
		switch m.active {
		case paneSMTP:
			return "↑↓ event · PgUp/PgDn scroll · s save attachments · e/E export · esc close · q quit"
		case paneShells:
			return "↑↓ row · ⏎/i attach · u/U upgrade · a start · r restart · d stop/kill · esc close · q quit"
		}
		return "↑↓ event · PgUp/PgDn scroll · g/G newest/oldest · e/E export · esc close · q quit"
	}
	switch m.active {
	case paneGenerator:
		return "⇄ Tab/←→ panes · ↑↓ shell · i LHOST · p LPORT · n encoding · q quit"
	case paneClipboard:
		return "⇄ Tab/←→ panes · ↑↓ scroll · ⏎ view · a add · d delete · C clear · e export · q quit"
	case paneShells:
		return "⇄ Tab/←→ panes · ↑↓ select · ⏎/i attach · u/U upgrade · a start · r restart · d stop/kill · q quit"
	case paneSMTP:
		return "⇄ Tab/←→ panes · ↑↓ scroll · ⏎ detail · s save attachments · e/E export · q quit"
	}
	return "⇄ Tab/←→ panes · ↑↓ scroll · ⏎ detail · g/G top/bottom · e/E export · q quit"
}

// bodyView renders exactly h lines for the active pane so the status bar stays
// pinned to the bottom of the screen. The list view gets a vertical scrollbar
// in the rightmost column whenever the events overflow the viewport.
func (m *model) bodyView(h int) string {
	if m.active == paneGenerator {
		return m.generatorView(h)
	}
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

// oneLine collapses newlines and tabs to spaces so multi-line content (e.g. a
// clipboard entry) fits on a single summary row.
func oneLine(s string) string {
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool {
		return r == '\n' || r == '\r' || r == '\t'
	}), " ")
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
