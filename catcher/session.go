package catcher

import (
	"net"
	"sync"
	"time"
)

type Session struct {
	ID         string
	ListenerID string
	RemoteAddr string

	mu     sync.Mutex
	conn   net.Conn
	closed bool
}

func newSession(id, listenerID, remoteAddr string, conn net.Conn) *Session {
	return &Session{
		ID:         id,
		ListenerID: listenerID,
		RemoteAddr: remoteAddr,
		conn:       conn,
	}
}

func (s *Session) Read(buf []byte) (int, error) {
	return s.conn.Read(buf)
}

func (s *Session) Write(buf []byte) (int, error) {
	return s.conn.Write(buf)
}

func (s *Session) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	if s.conn != nil {
		s.conn.Close()
	}
}

// SetReadDeadline sets (or clears, with the zero time) the read deadline on the
// underlying connection. The TUI uses this to interrupt a blocked Read when an
// operator detaches from a session, without tearing the connection down so it
// can be re-attached later (or kept alive for web clients).
func (s *Session) SetReadDeadline(t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.conn == nil {
		return nil
	}
	return s.conn.SetReadDeadline(t)
}

// LocalIP returns the local IP the remote connected to, i.e. the address on
// this host that is reachable from the victim. The TUI uses it to build a
// download URL (ConPtyShell) the victim can fetch even when the server is bound
// to a wildcard address like 0.0.0.0.
func (s *Session) LocalIP() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		return ""
	}
	if a, ok := s.conn.LocalAddr().(*net.TCPAddr); ok {
		return a.IP.String()
	}
	host, _, err := net.SplitHostPort(s.conn.LocalAddr().String())
	if err != nil {
		return ""
	}
	return host
}

func (s *Session) IsClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

type SessionInfo struct {
	ID         string `json:"id"`
	ListenerID string `json:"listenerId"`
	RemoteAddr string `json:"remoteAddr"`
}
