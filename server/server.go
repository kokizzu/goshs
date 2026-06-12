package server

import (
	"context"

	"goshs.de/goshs/v2/catcher"
	"goshs.de/goshs/v2/clipboard"
	"goshs.de/goshs/v2/dnsserver"
	"goshs.de/goshs/v2/ftpserver"
	"goshs.de/goshs/v2/httpserver"
	"goshs.de/goshs/v2/ldapserver"
	"goshs.de/goshs/v2/logger"
	"goshs.de/goshs/v2/options"
	"goshs.de/goshs/v2/sftpserver"
	"goshs.de/goshs/v2/smbserver"
	"goshs.de/goshs/v2/smtpserver"
	"goshs.de/goshs/v2/utils"
	"goshs.de/goshs/v2/webhook"
	"goshs.de/goshs/v2/ws"
)

// Servers bundles the shared runtime handles a caller needs after StartAll:
// the graceful-shutdown function plus the live event hub and reverse-shell
// catcher manager that drive the optional TUI dashboard.
type Servers struct {
	Shutdown func(context.Context)
	Hub      *ws.Hub
	Catcher  *catcher.Manager
}

func StartAll(opts *options.Options) *Servers {
	// Init clipboard and hub
	clip := clipboard.New()
	hub := ws.NewHub(clip, opts.CLI)
	go hub.Run()

	// Whitelist and Webhook
	wl, wh := registerWhitelistWebhook(opts)

	// http
	httpSrv := httpserver.NewHttpServer(opts, hub, clip, wl, *wh)
	go httpSrv.Start("web")

	// webdav
	var webdavSrv *httpserver.FileServer
	if opts.WebDav {
		webdavSrv = httpserver.NewHttpServer(opts, hub, clip, wl, *wh)
		webdavSrv.WebdavPort = opts.WebDavPort
		go webdavSrv.Start("webdav")
	}

	if opts.DNS {
		dnsSrv := dnsserver.NewDNSServer(opts, hub, wh)
		go dnsSrv.Start()
	}

	if opts.SMTP {
		smtpServer := smtpserver.NewSMTP(opts, hub, wh)
		go smtpServer.Start()
	}

	if opts.SMB {
		smbServer := smbserver.NewSMBServer(opts, hub, wh)
		go smbServer.Start()
	}

	if opts.LDAP {
		ldapSrv := ldapserver.NewLDAPServer(opts, hub, wh)
		go ldapSrv.Start()
	}

	if opts.FTP {
		if opts.FTPSFTPMode {
			sftpSrv := sftpserver.NewSFTPServer(opts, wl, *wh)
			go sftpSrv.Start()
		} else {
			ftpSrv := ftpserver.NewFTPServer(opts, wl, *wh)
			go ftpSrv.Start()
		}
	}

	// Zeroconf mDNS
	if opts.MDNS {
		err := utils.RegisterZeroconfMDNS(opts.SSL, opts.Port, opts.WebDav, opts.WebDavPort, opts.SMTP, opts.SMTPPort, opts.DNS, opts.DNSPort, opts.SMB, opts.SMBPort, opts.LDAP, opts.LDAPPort, opts.FTP, opts.FTPSFTPMode, opts.FTPPort)
		if err != nil {
			logger.Warnf("error registering zeroconf mDNS: %+v", err)
		}
	}

	shutdown := func(ctx context.Context) {
		if err := httpSrv.Shutdown(ctx); err != nil {
			logger.Errorf("error shutting down HTTP server: %+v", err)
		}
		if webdavSrv != nil {
			if err := webdavSrv.Shutdown(ctx); err != nil {
				logger.Errorf("error shutting down WebDAV server: %+v", err)
			}
		}
	}

	return &Servers{
		Shutdown: shutdown,
		Hub:      hub,
		Catcher:  httpSrv.CatcherMgr,
	}
}

func registerWhitelistWebhook(opts *options.Options) (wl *httpserver.Whitelist, wh *webhook.Webhook) {
	// Parse IP whitelist
	enabled := false
	if opts.Whitelist != "" {
		logger.Infof("Whitelist activated: %+v", opts.Whitelist)
		enabled = true
	}

	// Register Whitelist
	wl, err := httpserver.NewIPWhitelist(opts.Whitelist, enabled, opts.TrustedProxies)
	if err != nil {
		logger.Warnf("Error parsing IP whitelist: %+v", err)
	}

	// Register webhook
	webh := webhook.Register(opts.WebhookEnabled, opts.WebhookURL, opts.WebhookProvider, opts.WebhookEventsParsed)

	return wl, webh
}
