// dockerns は Docker コンテナーへの接続な名前解決を行う HTTP / SOCKS v5 プロキシーサーバー及び DNS サーバー。
//
// ルーティングに関する設定は etcd 上に保存して使用する。
//
//  # 「ホスト名が ^.*\.my-service\.com$ の正規表現に一致したら my_container_name へ接続する」というルーティング情報を master アカウントに追加する。
//  # 0.regexp_name の 0 は優先順位で、複数のルーティング情報がある場合に値が大きいほど優先される。regexp_name は管理上の設定名なので何でも構わない。
//  curl -L http://172.17.42.1:4001/v2/keys/proxy/master/my_container_name/0.regexp_name -X PUT -d value='^.*\.my-service\.com$'
//
// 上記のようなルーティング情報が保存されている状態で、以下のようにして dockerns を起動する。
//
//  # Docker Remote API は /var/run/docker.sock 経由でアクセス
//  # etcd は http://172.17.42.1:4001 経由でアクセス
//  # HTTP は 80 番ポート、SOCKS は 1080 番ポート、DNS は 53 番ポートで待ち受け
//  dockerns -docker=unix:///var/run/docker.sock: -etcd=http://172.17.42.1:4001 -http=:80 -socks=:1080 -dns=:53
//
// この状態で HTTP や SOCKS v5 プロトコルでアクセスすると、プロキシーのユーザー名とパスワードを要求される。
//
//  ユーザー名(アカウント名)
//      master
//  パスワード
//      任意の文字列 (dockerns 起動時に -password オプションで指定した場合はその文字列)
//
// 接続しようとした先が正規表現に一致している場合は本来の接続先ではなく指定されたコンテナーへの接続としてすり替えられる。
//
// dockerns の起動中に Docker のコンテナーが起動／終了されたり etcd のルーティング情報が変化した場合には随時設定が再構築される。
//
// 有効なオプションは以下の通り。
//
//  -d
//      デバッグモード。
//  -reverse
//      HTTP サーバーでリバースプロキシーモードを有効にする。
//      有効にするためには -account オプションで有効なアカウント名を指定する必要がある。
//  -account=""
//      アカウント名。
//      常に特定のアカウントを使用する場合はここでアカウント名を指定するとユーザー認証が不要になる。
//  -realm="Proxy"
//      HTTP プロキシーで使用されるレルム。
//  -password=""
//      HTTP / SOCKS v5 プロキシーで使用するパスワード。
//      省略した場合は任意の文字列を入力すれば通過できる。
//  -docker=""
//      Docker Remote API にアクセスするためのアドレスを指定する。
//      省略した場合は Docker Remote API は使用せずに起動する。
//      例: 'http://172.17.42.1:4243', 'unix:///path/to/docker.sock:'
//  -etcd="http://172.17.42.1:4001"
//      etcd にアクセスするためのアドレスを指定する。
//  -routes="/proxy"
//      プロキシールーティング情報が etcd 上のどこを基点に保存されているのかを指定する。
//  -http=""
//      HTTP プロキシーが待ち受けるアドレスを :80 のような形で指定する。省略した場合は待ち受けない。
//  -socks=""
//      SOCKS v5 プロキシーが待ち受けるアドレスを :1080 のような形で指定する。省略した場合は待ち受けない。
//  -dns=""
//      DNS サーバが待ち受けるアドレスを :53 のような形で指定する。省略した場合は待ち受けない。
//      使用するためには -account でアカウント名を適切に渡す必要がある。
//  -ns="8.8.8.8:53"
//      DNS サーバが自分自身で解決できなかったリクエストを転送する先のネームサーバー。
//  -fakemx=""
//      -ns で指定されたサーバーからの応答を返す前に MX レコードの内容を書き換える場合に指定する。
package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/mimoto-xxxxxx/dockerns/accounts"
	"github.com/mimoto-xxxxxx/dockerns/dns"
	"github.com/mimoto-xxxxxx/dockerns/proxy"
)

func main() {
	var (
		debug         = flag.Bool("d", false, "debug mode")
		reverse       = flag.Bool("reverse", false, "enable reverse http proxy mode")
		account       = flag.String("account", "", "account")
		realm         = flag.String("realm", "Proxy", "realm for proxy server")
		proxyPassword = flag.String("password", "", "password for proxy server")
		dockerAddress = flag.String("docker", "", "docker remote api address")
		etcdAddress   = flag.String("etcd", "http://172.17.42.1:4001", "etcd address")
		etcdRoot      = flag.String("routes", "/proxy", "etcd routes information root")
		httpService   = flag.String("http", "", "HTTP service address (e.g., ':80')")
		socksService  = flag.String("socks", "", "SOCKSv5 service address (e.g., ':1080')")
		dnsService    = flag.String("dns", "", "DNS service address (e.g., ':53')")
		nameServer    = flag.String("ns", "8.8.8.8:53", "secondary name server (e.g., '8.8.8.8:53')")
		fakeMX        = flag.String("fakemx", "", "enable mx record poisoning(e.g., 'localhost.localdomain.')")
	)

	flag.Parse()

	ac := accounts.New(*dockerAddress, *etcdAddress, *etcdRoot)
	ac.Verbose = *debug

	end := make(chan struct{})

	c := make(chan os.Signal)
	signal.Notify(c, os.Interrupt)
	go func() {
		for _ = range c {
			end <- struct{}{}
		}
	}()

	go func() {
		// 初回の設定読み込みが成功するまで待機する
		log.Println("building routing table")

		err := ac.Reload()
		for err != nil {
			time.Sleep(time.Second)
			log.Println("wait...")
			err = ac.Reload()
		}

		go ac.Watch()

		if *httpService != "" {
			go func() {
				if *reverse && *account != "" {
					s := proxy.NewRevHTTP(ac, *account)
					if err := s.ListenAndServe(*httpService); err != nil {
						log.Println("ListenAndServe(RevHTTP):", err)
					}
				} else {
					s := proxy.NewHTTP(ac)
					s.AccountName = *account
					s.Password = *proxyPassword
					s.Realm = *realm
					if err := s.ListenAndServe(*httpService); err != nil {
						log.Println("ListenAndServe(HTTP):", err)
					}
				}
				end <- struct{}{}
			}()
		}
		if *socksService != "" {
			go func() {
				s := proxy.NewSOCKS(ac)
				s.AccountName = *account
				if err := s.ListenAndServe(*socksService); err != nil {
					log.Println("ListenAndServe(SOCKS):", err)
				}
				end <- struct{}{}
			}()
		}
		if *dnsService != "" {
			go func() {
				s := dns.New(ac)
				s.AccountName = *account
				s.NameServer = *nameServer
				s.FakeMX = *fakeMX
				if err := s.ListenAndServe(*dnsService); err != nil {
					log.Println("ListenAndServe(DNS):", err)
				}
				end <- struct{}{}
			}()
		}
	}()
	<-end
}
