package ftpserver

import (
	"crypto/tls"
	"fmt"
	"net"
	"strconv"

	ftplib "github.com/fclairamb/ftpserverlib"
	"github.com/spf13/afero"
	"goshs.de/goshs/v2/httpserver"
	"goshs.de/goshs/v2/logger"
	"goshs.de/goshs/v2/options"
	"goshs.de/goshs/v2/webhook"
)

type FTPServer struct {
	IP        string
	Port      int
	Root      string
	Username  string
	Password  string
	ReadOnly  bool
	NoDelete  bool
	Webhook   webhook.Webhook
	Whitelist *httpserver.Whitelist
}

func NewFTPServer(opts *options.Options, wl *httpserver.Whitelist, wh webhook.Webhook) *FTPServer {
	return &FTPServer{
		IP:        opts.IP,
		Port:      opts.FTPPort,
		Root:      opts.Webroot,
		Username:  opts.Username,
		Password:  opts.Password,
		ReadOnly:  opts.ReadOnly,
		NoDelete:  opts.NoDelete,
		Webhook:   wh,
		Whitelist: wl,
	}
}

func (s *FTPServer) Start() error {
	driver := &mainDriver{srv: s}
	srv := ftplib.NewFtpServer(driver)
	logger.Infof("Starting FTP server on %s:%d", s.IP, s.Port)
	return srv.ListenAndServe()
}

func (s *FTPServer) HandleWebhookSend(action, path, ip string, blocked bool) {
	var message string
	if blocked {
		message = fmt.Sprintf("[FTP] BLOCKED %s - [%s] - \"%s\"", ip, action, path)
	} else {
		message = fmt.Sprintf("[FTP] %s - [%s] - \"%s\"", ip, action, path)
	}
	logger.HandleWebhookSend(message, "ftp", s.Webhook)
}

// mainDriver implements ftplib.MainDriver
type mainDriver struct {
	srv *FTPServer
}

func (d *mainDriver) GetSettings() (*ftplib.Settings, error) {
	return &ftplib.Settings{
		ListenAddr:              net.JoinHostPort(d.srv.IP, strconv.Itoa(d.srv.Port)),
		Banner:                  "goshs FTP server ready",
		ActiveTransferPortNon20: true,
	}, nil
}

func (d *mainDriver) ClientConnected(cc ftplib.ClientContext) (string, error) {
	clientIP := cc.RemoteAddr().String()
	if !isAllowedIP(cc.RemoteAddr(), d.srv.Whitelist) {
		logger.Warnf("[FTP] [WHITELIST] Access denied for %s", clientIP)
		return "", fmt.Errorf("access denied")
	}
	logger.Infof("[FTP] Client connected from %s", clientIP)
	return "goshs FTP server", nil
}

func (d *mainDriver) ClientDisconnected(cc ftplib.ClientContext) {
	logger.Infof("[FTP] Client disconnected: %s", cc.RemoteAddr())
}

func (d *mainDriver) AuthUser(cc ftplib.ClientContext, user, pass string) (ftplib.ClientDriver, error) {
	if d.srv.Username != "" || d.srv.Password != "" {
		if user != d.srv.Username || pass != d.srv.Password {
			logger.Warnf("[FTP] Auth failed for user '%s' from %s", user, cc.RemoteAddr())
			d.srv.HandleWebhookSend("AUTH", user, cc.RemoteAddr().String(), true)
			return nil, fmt.Errorf("invalid credentials")
		}
	}
	logger.Infof("[FTP] User '%s' authenticated from %s", user, cc.RemoteAddr())
	d.srv.HandleWebhookSend("AUTH", user, cc.RemoteAddr().String(), false)

	base := afero.NewBasePathFs(afero.NewOsFs(), d.srv.Root)
	if d.srv.ReadOnly {
		return afero.NewReadOnlyFs(base), nil
	}
	if d.srv.NoDelete {
		return &noDeleteFs{Fs: base}, nil
	}
	return base, nil
}

func (d *mainDriver) GetTLSConfig() (*tls.Config, error) {
	return nil, nil
}

// noDeleteFs wraps afero.Fs and blocks remove operations.
type noDeleteFs struct {
	afero.Fs
}

func (fs *noDeleteFs) Remove(name string) error {
	return fmt.Errorf("delete not allowed")
}

func (fs *noDeleteFs) RemoveAll(path string) error {
	return fmt.Errorf("delete not allowed")
}

func isAllowedIP(addr net.Addr, wl *httpserver.Whitelist) bool {
	if !wl.Enabled {
		return true
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		host = addr.String()
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, n := range wl.Networks {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
