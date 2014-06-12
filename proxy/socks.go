package proxy

import (
	"fmt"
	"log"
	"os"

	"github.com/elazarl/goproxy"
	"github.com/oov/socks5"

	"github.com/mimoto-xxxxxx/dockerns/accounts"
)

// SOCKS は SOCKS5 プロトコルによるプロキシサーバ。
// AccountName を指定した場合は認証は行わずに接続できる。
type SOCKS struct {
	AccountName string
	Password    string
	Logger      *log.Logger
	accounts    *accounts.Accounts
	proxy       *goproxy.ProxyHttpServer
	socks       *socks5.Server
}

// authorize は username と password 正当なものであることを検証し、
// 成功した場合に該当するアカウント情報を返す。
func (s *SOCKS) authorize(username, password string) (*accounts.Account, error) {
	if s.Password != "" && password != s.Password {
		return nil, fmt.Errorf("password incorrect")
	}
	a := s.accounts.Get(username)
	if a == nil {
		return nil, fmt.Errorf("account not found")
	}
	return a, nil
}

// noauthorizeSOCKS は認証なし接続が行えるかテストする。
func (s *SOCKS) noauthorizeSOCKS(c *socks5.Conn) error {
	if s.AccountName == "" {
		return socks5.ErrAuthenticationFailed
	}

	a := s.accounts.Get(s.AccountName)
	if a == nil {
		if s.accounts.Verbose {
			log.Println("account not found:", s.AccountName)
		}
		return socks5.ErrAuthenticationFailed
	}

	c.Data = a
	return nil
}

// authorizeSOCKS は接続してきたユーザーが正しいユーザー名とパスワードを所持しているかテストする。
func (s *SOCKS) authorizeSOCKS(c *socks5.Conn, username, password []byte) error {
	if s.Password != "" && string(password) != s.Password {
		if s.accounts.Verbose {
			log.Println("password incorrect")
		}
		return socks5.ErrAuthenticationFailed
	}

	a := s.accounts.Get(string(username))
	if a == nil {
		if s.accounts.Verbose {
			log.Println("account not found:", string(username))
		}
		return socks5.ErrAuthenticationFailed
	}

	c.Data = a
	return nil
}

// NewSOCKS は SOCKS プロクシサーバーを新規作成する。
func NewSOCKS(accounts *accounts.Accounts) *SOCKS {
	s := &SOCKS{
		Logger:   log.New(os.Stderr, "", log.LstdFlags),
		accounts: accounts,
		socks:    socks5.New(),
	}

	s.socks.AuthNoAuthenticationRequiredCallback = s.noauthorizeSOCKS
	s.socks.AuthUsernamePasswordCallback = s.authorizeSOCKS
	s.socks.HandleConnectFunc(s.proxySOCKSConnect)
	return s
}

// ListenAndServe はサーバの Listen を開始する。
func (s *SOCKS) ListenAndServe(addr string) error {
	err := s.socks.ListenAndServe(addr)
	if err != nil {
		s.Logger.Println("proxy.ListenAndServe(SOCKS):", err)
	}
	return err
}

// proxySOCKSConnect は SOCKS5 プロクシの実装。
func (s *SOCKS) proxySOCKSConnect(c *socks5.Conn, host string) (newHost string, err error) {
	if account, ok := c.Data.(*accounts.Account); ok {
		newHost = account.Routes.ReplaceHost(host)
		if s.accounts.Verbose {
			s.Logger.Println("user:", account.Name, "host:", host, "newHost:", newHost)
		}
		return
	}
	return host, nil
}
