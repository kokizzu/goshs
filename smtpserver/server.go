package smtpserver

import (
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/emersion/go-smtp"
	"goshs.de/goshs/v2/logger"
	"goshs.de/goshs/v2/options"
	"goshs.de/goshs/v2/smtpattach"
	"goshs.de/goshs/v2/webhook"
	"goshs.de/goshs/v2/ws"
)

type SMTPServer struct {
	IP      string
	Port    int
	Hub     *ws.Hub
	WebHook *webhook.Webhook
	Domain  string

	ln net.Listener // bound by Bind, served by Start
}

func NewSMTP(opts *options.Options, hub *ws.Hub, wh *webhook.Webhook) *SMTPServer {
	return &SMTPServer{
		IP:      opts.IP,
		Port:    opts.SMTPPort,
		Domain:  opts.SMTPDomain,
		Hub:     hub,
		WebHook: wh,
	}
}

// Bind acquires the listening socket so a port conflict is reported to the
// caller synchronously instead of a serving goroutine swallowing it.
func (srv *SMTPServer) Bind() error {
	addr := net.JoinHostPort(srv.IP, strconv.Itoa(srv.Port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("SMTP: failed to listen on %s: %w", addr, err)
	}
	srv.ln = ln
	return nil
}

func (srv *SMTPServer) Start() {
	// Bind lazily if a caller did not already do so via Bind.
	if srv.ln == nil {
		if err := srv.Bind(); err != nil {
			logger.Fatalf("%+v", err)
		}
	}
	be := &Backend{Hub: srv.Hub, WebHook: srv.WebHook}
	s := smtp.NewServer(be)
	addr := net.JoinHostPort(srv.IP, strconv.Itoa(srv.Port))
	s.Addr = addr
	s.Domain = "goshs"
	s.AllowInsecureAuth = true // catch-all, no real auth needed
	if srv.Domain != "" {
		logger.Infof("SMTP catch-all listening on %s (restricting to @%s)", addr, srv.Domain)
	} else {
		logger.Infof("SMTP catch-all listening on %s (open relay)", addr)
	}
	go func() { _ = s.Serve(srv.ln) }()
	go smtpattach.PurgeLoop(1 * time.Hour)
}
