package proxy

import (
	"log"
	"net/http"
	"net/http/httputil"
	"os"

	"github.com/mimoto-xxxxxx/dockerns/accounts"
)

// RevHTTP は HTTP リバースプロキシ。
type RevHTTP struct {
	Logger *log.Logger
	rp     *httputil.ReverseProxy
}

// NewRevHTTP は新しい HTTP リバースプロキシを作成する。
func NewRevHTTP(accounts *accounts.Accounts, accountName string) *RevHTTP {
	return &RevHTTP{
		Logger: log.New(os.Stderr, "", log.LstdFlags),
		rp: &httputil.ReverseProxy{
			Director: func(req *http.Request) {
				a := accounts.Get(accountName)
				if a == nil {
					req.URL.Host = "0.0.0.0"
					return
				}
				req.URL.Host = a.Routes.ReplaceHost(req.URL.Host)
				req.Header.Add("X-Real-IP", req.RemoteAddr)
			},
		},
	}
}

// ServeHTTP は http.Handler の実装。
func (r *RevHTTP) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	r.rp.ServeHTTP(rw, req)
}

// ListenAndServe は addr で Listen して通信の待受状態に入る。
func (r *RevHTTP) ListenAndServe(addr string) error {
	return http.ListenAndServe(addr, r.rp)
}
