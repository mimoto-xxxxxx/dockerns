// Package dns は簡易的な DNS サーバーの実装。
package dns

import (
	"fmt"
	"log"
	"net"
	"os"

	"github.com/miekg/dns"

	"github.com/mimoto-xxxxxx/dockerns/accounts"
)

// DNS は簡易的な DNS サーバ。
type DNS struct {
	AccountName string
	TTL         uint32
	NameServer  string
	Logger      *log.Logger
	accounts    *accounts.Accounts
}

// New は DNS サーバー用のインスタンスを新規作成する。
func New(accounts *accounts.Accounts) *DNS {
	return &DNS{
		TTL:        60,
		NameServer: "8.8.8.8:53",
		Logger:     log.New(os.Stderr, "", log.LstdFlags),
		accounts:   accounts,
	}
}

// ListenAndServe は DNS サーバとして Listen を開始する。
// addr に指定されたアドレスとポートを UDP と TCP の両方で待ち受ける。
func (d *DNS) ListenAndServe(addr string) error {
	go (&dns.Server{Addr: addr, Net: "tcp", Handler: d}).ListenAndServe()
	return (&dns.Server{Addr: addr, Net: "udp", Handler: d}).ListenAndServe()
}

// serveFilure は失敗時のレスポンスを返す。
func (d *DNS) serveFailure(err error, w dns.ResponseWriter, req *dns.Msg) {
	d.Logger.Println("dns:", err)
	ret := &dns.Msg{}
	ret.SetReply(req)
	ret.SetRcode(req, dns.RcodeServerFailure)
	ret.Authoritative = false
	ret.RecursionAvailable = true
	w.WriteMsg(ret)
}

// forward は予め指定されていたネームサーバーに req をリクエストし、そのレスポンスをそのまま返送する。
func (d *DNS) forward(w dns.ResponseWriter, req *dns.Msg) {
	network := "udp"
	if _, ok := w.RemoteAddr().(*net.TCPAddr); ok {
		network = "tcp"
	}

	c := &dns.Client{Net: network}
	for i := 0; i < 3; i++ {
		r, _, err := c.Exchange(req, d.NameServer)
		if err == nil {
			w.WriteMsg(r)
			return
		}
		d.Logger.Println("failure to forward request:", err)
	}
	d.Logger.Println("gave up")

	m := &dns.Msg{}
	m.SetReply(req)
	m.SetRcode(req, dns.RcodeServerFailure)
	w.WriteMsg(m)
}

// ServeDNS は DNS サーバーにきたリクエストを処理する。
func (d *DNS) ServeDNS(w dns.ResponseWriter, req *dns.Msg) {
	ac := d.accounts.Get(d.AccountName)
	if ac == nil {
		d.serveFailure(fmt.Errorf("account not found: %q", d.AccountName), w, req)
		return
	}

	if len(req.Question) == 0 {
		d.serveFailure(fmt.Errorf("no query"), w, req)
		return
	}

	q := req.Question[0]
	if len(q.Name) == 0 {
		d.serveFailure(fmt.Errorf("invalid request: name is empty"), w, req)
		return
	}

	domain := q.Name[:len(q.Name)-1]
	h := ac.Routes.ReplaceHost(domain)

	if h == domain {
		d.forward(w, req)
		return
	}

	rr := []dns.RR{}

	if q.Qtype == dns.TypeA {
		ip := net.ParseIP(h)
		if ip != nil {
			ipaddr, err := net.ResolveIPAddr("ip", h)
			if err != nil {
				d.serveFailure(err, w, req)
				return
			}
			ip = ipaddr.IP
		}
		rr = append(rr, &dns.A{
			Hdr: dns.RR_Header{
				Name:   q.Name,
				Rrtype: dns.TypeA,
				Class:  dns.ClassINET,
				Ttl:    d.TTL,
			},
			A: ip,
		})
	}

	if q.Qtype == dns.TypeTXT || q.Qtype == dns.TypeANY {
		rr = append(rr, &dns.TXT{
			Hdr: dns.RR_Header{
				Name:   q.Name,
				Rrtype: dns.TypeTXT,
				Class:  dns.ClassINET,
				Ttl:    d.TTL,
			},
			Txt: []string{"v=spf1 mx -all"},
		})
	}

	if q.Qtype == dns.TypeMX || q.Qtype == dns.TypeANY {
		rr = append(rr, &dns.MX{
			Hdr: dns.RR_Header{
				Name:   q.Name,
				Rrtype: dns.TypeMX,
				Class:  dns.ClassINET,
				Ttl:    d.TTL,
			},
			Mx:         q.Name,
			Preference: 1,
		})
	}

	m := &dns.Msg{}
	m.SetReply(req)
	m.RecursionAvailable = true
	m.Answer = rr
	if err := w.WriteMsg(m); err != nil {
		serveFailure(err, w, req)
		return
	}
}
