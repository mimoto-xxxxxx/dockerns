// Package proxy は HTTP プロキシを作るためのパッケージ。
package proxy

import (
	"encoding/base64"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/elazarl/goproxy"
	"github.com/elazarl/goproxy/ext/auth"
	"github.com/oov/socks5"

	"github.com/mimoto-xxxxxx/dockerns/accounts"
)

// HTTP は HTTP プロトコルによるフォワードプロキシサーバ。
// AccountName を指定した場合は認証は行わずに接続できる。
type HTTP struct {
	AccountName string
	Password    string
	Realm       string
	Logger      *log.Logger
	accounts    *accounts.Accounts
	proxy       *goproxy.ProxyHttpServer
	api         *http.ServeMux
	socks       *socks5.Server
}

// authorizeAndReplaceHost はリクエストからプロクシ用のユーザー/パスワード情報を探し出し、
// 内容に問題がなければそのアカウントを使用して host を置換して返す。
// ただし s.AccountName に指定がある場合はそちらを優先する。
func (s *HTTP) authorizeAndReplaceHost(host string, r *http.Request) (user string, newHost string, err error) {
	if s.AccountName != "" {
		a := s.accounts.Get(s.AccountName)
		if a == nil {
			err = fmt.Errorf("account not found")
			return
		}

		newHost = a.Routes.ReplaceHost(host)
		user = s.AccountName
		return
	}

	authHeader := strings.SplitN(r.Header.Get("Proxy-Authorization"), " ", 2)
	r.Header.Del("Proxy-Authorization")
	if len(authHeader) != 2 {
		err = fmt.Errorf("valid 'Proxy-Authorization' header not found")
		return
	}
	if authHeader[0] != "Basic" {
		err = fmt.Errorf("proxy only supports 'Basic' authentication: %v", authHeader[0])
		return
	}

	userpassraw, err := base64.StdEncoding.DecodeString(authHeader[1])
	if err != nil {
		err = fmt.Errorf("could not decode 'Proxy-Authorization' header value: %v", err)
		return
	}
	userpass := strings.SplitN(string(userpassraw), ":", 2)
	if len(userpass) != 2 {
		err = fmt.Errorf("'Proxy-Authorization' header value is invalid format: %v", string(userpassraw))
		return
	}
	if s.Password != "" && userpass[1] != s.Password {
		err = fmt.Errorf("password incorrect")
		return
	}
	a := s.accounts.Get(userpass[0])
	if a == nil {
		err = fmt.Errorf("account not found")
		return
	}

	newHost = a.Routes.ReplaceHost(host)
	user = userpass[0]
	return
}

// NewHTTP は HTTP プロクシ兼 API サーバーを新規作成する。
func NewHTTP(accounts *accounts.Accounts) *HTTP {
	s := &HTTP{
		Realm:    "Proxy",
		Logger:   log.New(os.Stderr, "", log.LstdFlags),
		accounts: accounts,
		proxy:    goproxy.NewProxyHttpServer(),
		api:      http.NewServeMux(),
	}
	if s.accounts.Verbose {
		s.proxy.Verbose = s.accounts.Verbose
	}

	onReq := s.proxy.OnRequest()
	onReq.DoFunc(s.proxyHTTP)
	onReq.HandleConnectFunc(s.proxyHTTPConnect)
	return s
}

// ListenAndServe はサーバの Listen を開始する。
func (s *HTTP) ListenAndServe(addr string) error {
	err := http.ListenAndServe(addr, s)
	if err != nil {
		s.Logger.Println("HTTP.ListenAndServe:", err)
	}
	return err
}

// ServeHTTP は http.Handler の実装。
func (s *HTTP) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	if req.Method == "CONNECT" || req.URL.IsAbs() {
		s.proxy.ServeHTTP(rw, req)
		return
	}
	s.api.ServeHTTP(rw, req)
}

// proxyHTTP は HTTP プロトコルにおけるプロクシの実装。
func (s *HTTP) proxyHTTP(r *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
	user, newHost, err := s.authorizeAndReplaceHost(r.URL.Host, r)
	if err != nil {
		if s.accounts.Verbose {
			s.Logger.Println("proxyHTTP:", err)
		}
		return nil, auth.BasicUnauthorized(r, s.Realm)
	}

	if s.accounts.Verbose {
		s.Logger.Println("user:", user, "host:", r.URL.Host, "newHost:", newHost)
	}

	r.URL.Host = newHost
	r.Header.Add("X-Real-IP", r.RemoteAddr)
	r.Header.Add("X-Forwarded-For", r.RemoteAddr)

	return r, nil
}

// proxyHTTPConnect は汎用 HTTP プロクシの実装。
func (s *HTTP) proxyHTTPConnect(host string, ctx *goproxy.ProxyCtx) (*goproxy.ConnectAction, string) {
	user, newHost, err := s.authorizeAndReplaceHost(host, ctx.Req)
	if err != nil {
		if s.accounts.Verbose {
			s.Logger.Println("proxyHTTPConnect:", err)
		}
		ctx.Resp = auth.BasicUnauthorized(ctx.Req, s.Realm)
		return goproxy.RejectConnect, host
	}

	if s.accounts.Verbose {
		s.Logger.Println("user:", user, "host:", host, "newHost:", newHost)
	}

	return goproxy.OkConnect, newHost
}

// proxySOCKSConnect は SOCKS プロクシの実装。
func (s *HTTP) proxySOCKSConnect(c *socks5.Conn, host string) (newHost string, err error) {
	if account, ok := c.Data.(*accounts.Account); ok {
		newHost = account.Routes.ReplaceHost(host)
		if s.accounts.Verbose {
			s.Logger.Println("user:", account.Name, "host:", host, "newHost:", newHost)
		}
		return
	}
	return host, nil
}
