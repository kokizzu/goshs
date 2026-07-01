package dnsserver

import (
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/miekg/dns"
	"goshs.de/goshs/v2/logger"
	"goshs.de/goshs/v2/options"
	"goshs.de/goshs/v2/webhook"
	"goshs.de/goshs/v2/ws"
)

type DNSServer struct {
	IP      string // IP to listen on
	ReplyIP string // IP to reply DNS queries
	Port    int
	Hub     *ws.Hub
	Silent  bool
	WebHook *webhook.Webhook

	udpConn net.PacketConn // bound by Bind, served by Start
	tcpLn   net.Listener   // bound by Bind, served by Start
}

func NewDNSServer(opts *options.Options, hub *ws.Hub, wh *webhook.Webhook) *DNSServer {
	replyIP := opts.DNSIP
	if replyIP == "" {
		replyIP = "0.0.0.0"
	}
	return &DNSServer{
		IP:      "0.0.0.0",
		ReplyIP: replyIP,
		Port:    opts.DNSPort,
		Hub:     hub,
		Silent:  opts.Silent,
		WebHook: wh,
	}
}

func (d *DNSServer) handler(w dns.ResponseWriter, r *dns.Msg) {
	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative = true

	for _, q := range r.Question {
		// Log query and push to websocket hub to be displayed in the UI
		event := ws.DNSEvent{
			Type:   "dns",
			Name:   q.Name,
			QType:  dns.TypeToString[q.Qtype],
			Source: w.RemoteAddr().String(),
			Time:   time.Now(),
		}
		eventBytes, err := json.Marshal(event)
		if err != nil {
			logger.Errorf("Error marshalling dns query event: %v", err)
			return
		}
		d.Hub.Broadcast <- eventBytes

		// If webhook is enabled, send the DNS query to the webhook endpoint
		logger.HandleWebhookSend(fmt.Sprintf("[DNS] - Source: %s - Type: %s - Query: %s", event.Source, event.QType, event.Name), "dns", *d.WebHook)

		switch q.Qtype {
		case dns.TypeA:
			m.Answer = append(m.Answer, &dns.A{
				Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 1},
				A:   net.ParseIP(d.ReplyIP).To4(),
			})
		case dns.TypeMX:
			m.Answer = append(m.Answer, &dns.MX{
				Hdr:        dns.RR_Header{Name: q.Name, Rrtype: dns.TypeMX, Class: dns.ClassINET, Ttl: 1},
				Preference: 10,
				Mx:         "mail." + q.Name,
			})
		case dns.TypeTXT:
			m.Answer = append(m.Answer, &dns.TXT{
				Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 1},
				Txt: []string{q.Name, "src=" + w.RemoteAddr().String()},
			})
		}
	}

	_ = w.WriteMsg(m)
}

// Bind acquires the UDP and TCP sockets so a port conflict is reported to the
// caller synchronously instead of a serving goroutine swallowing it.
func (d *DNSServer) Bind() error {
	addr := net.JoinHostPort(d.IP, strconv.Itoa(d.Port))
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return fmt.Errorf("DNS: failed to listen on udp %s: %w", addr, err)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		_ = pc.Close()
		return fmt.Errorf("DNS: failed to listen on tcp %s: %w", addr, err)
	}
	d.udpConn = pc
	d.tcpLn = ln
	return nil
}

func (d *DNSServer) Start() {
	// Bind lazily if a caller did not already do so via Bind.
	if d.udpConn == nil || d.tcpLn == nil {
		if err := d.Bind(); err != nil {
			logger.Fatalf("%+v", err)
		}
	}
	udpServer := &dns.Server{PacketConn: d.udpConn, Net: "udp", Handler: dns.HandlerFunc(d.handler)}
	tcpServer := &dns.Server{Listener: d.tcpLn, Net: "tcp", Handler: dns.HandlerFunc(d.handler)}
	logger.Infof("DNS server listening on udp/tcp %s:%d", d.IP, d.Port)

	go func() { _ = udpServer.ActivateAndServe() }()
	go func() { _ = tcpServer.ActivateAndServe() }()
}
