// Package accounts はプロキシのアカウント情報を管理する。
package accounts

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-etcd/etcd"
)

// Container は docker のコンテナを表す。コンテナ名にはリンクされた時の名前ではなく必ず独立した名前が割り当てられる。
type Container struct {
	Name      string //コンテナ名。
	IPAddress string //"172.17.0.2" のような形式。
}

// String はコンテナ情報を人間が読みやすい文字列として出力する。
func (c *Container) String() string {
	return fmt.Sprintf("%s(%s)", c.Name, c.IPAddress)
}

// Route はコンテナへのルーティング情報を表す。
// Name にはルーティングに対する任意の名称を保存することができる。
// Regexp に割り当てられた正規表現にホスト名が一致する場合はホスト名が Host に差し替えられる。
// Priority の値が大きいデータほど正規表現が優先的に評価される。
type Route struct {
	Name     string
	Priority int
	Host     string
	Regexp   *regexp.Regexp
}

// String はルーティング設定を人間が読みやすい文字列として出力する。
func (r *Route) String() string {
	return fmt.Sprintf("%v pr:%d -> %v", r.Regexp, r.Priority, r.Host)
}

// Routes は優先順位に応じて並び替えされた状態の Route の配列。
// sort.Interface を実装している。
type Routes []*Route

// Len は r の長さを返す。
func (r Routes) Len() int {
	return len(r)
}

// Less は r[i] のプライオリティが r[j] より小さい場合に true を返す。
func (r Routes) Less(i, j int) bool {
	return r[i].Priority < r[j].Priority
}

// Swap は r[i] と r[j] をすり替える。
func (r Routes) Swap(i, j int) {
	r[i], r[j] = r[j], r[i]
}

// ReplaceHost は host を該当するルーティング情報があれば差し替える。
// host に example.com:8080 のようなポート番号付きのものを渡した場合は分解した上で検索される。
func (r Routes) ReplaceHost(host string) string {
	parts := strings.SplitN(host, ":", 2)
	hostname := parts[0]
	hasPort := len(parts) == 2 && parts[1] != ""
	for _, route := range r {
		if route.Regexp.MatchString(hostname) {
			if hasPort {
				return route.Host + ":" + parts[1]
			}
			return route.Host
		}
	}
	return host
}

// Account は案件ごとの設定を格納した構造体。
// Routes は Priority の降順で並び替えられた状態で格納されている。
type Account struct {
	Name   string
	Routes Routes
}

// Accounts はアカウント情報の集合。
// accounts の string には Account.Name と同じ物を使用する。
type Accounts struct {
	accounts   map[string]Account
	m          sync.Mutex
	DockerAddr string
	EtcdAddr   string
	EtcdRoot   string
	Verbose    bool
}

// New は Accounts のインスタンスを新規作成する。
func New(dockerAddr, etcdAddr, etcdRoot string) *Accounts {
	return &Accounts{
		DockerAddr: dockerAddr,
		EtcdAddr:   etcdAddr,
		EtcdRoot:   etcdRoot,
		accounts:   make(map[string]Account),
	}
}

// httpGet は s の接頭辞が "unix:" の場合は UNIX ドメインソケットで HTTP リクエストを、
// そうでなければ http.Get(url) の結果を返す。
// UNIX ドメインソケットでのリクエストの場合は unix:///path/to/unix.sock:/request/path?param=value のような形式で渡す。
func httpGet(s string) (*http.Response, error) {
	if len(s) < 5 || (s[:5] != "unix:") {
		return http.Get(s)
	}

	u, err := url.Parse(s)
	if err != nil {
		return nil, err
	}
	parts := strings.SplitN(u.Path, ":", 2)
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid UNIX domain socket url format: %s", s)
	}
	host, path := parts[0], parts[1]

	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	req, err := http.NewRequest("GET", path, nil)
	if err != nil {
		return nil, err
	}

	conn, err := net.Dial("unix", host)
	if err != nil {
		return nil, err
	}

	return httputil.NewClientConn(conn, nil).Do(req)
}

// httpGetJson は url で指定されたリソースを取得し、それが JSON であると仮定した上で v へ展開する。
func httpGetJson(url string, v interface{}) error {
	res, err := httpGet(url)
	if err != nil {
		return err
	}

	defer res.Body.Close()

	if err = json.NewDecoder(res.Body).Decode(v); err != nil {
		return err
	}

	return nil
}

func getContainers(dockerAddr string) (map[string]*Container, error) {
	containers := make(map[string]*Container)

	// docker のコンテナ一覧を取得し、名前と IP の対応付けを行う。
	var containerList []struct {
		ID    string   `json:"Id"`
		Names []string `json:"Names"`
	}
	err := httpGetJson(dockerAddr+"/containers/json", &containerList)
	if err != nil {
		return nil, err
	}

	for _, containerItem := range containerList {
		// Name と IPAddress の値を得るため個々の詳細を問い合わせる。
		var container struct {
			Name            string `json:"Name"`
			NetworkSettings struct {
				IPAddress string `json:"IPAddress"`
			} `json:"NetworkSettings"`
		}
		err = httpGetJson(dockerAddr+"/containers/"+containerItem.ID+"/json", &container)
		if err != nil {
			return nil, err
		}

		// 名前は /hoge/mysql のようなリンク時の名前と
		// そのコンテナ本来の / が含まれていない名前の両方を登録しておく。
		c := &Container{
			IPAddress: container.NetworkSettings.IPAddress,
			Name:      container.Name[1:],
		}
		containers[c.Name] = c
		for _, n := range containerItem.Names {
			containers[n] = c
		}
	}

	return containers, nil
}

// Reload は Docker Remote API と etcd にアクセスしてルーティング情報を組み立てる。
// 設定された名前のコンテナが実際には存在しなかったり正規表現が不正な場合はメッセージを出力しつつもそれを除外した上で処理を続行する。
//
// etcd に対しては、例えば以下のような形式で設定を書き込んでおく。
//
//  # 例1: 「*.my-service.com は my_container_name の IP アドレスへのアクセスとして書き換える」というルーティング情報を master アカウントに追加する
//  curl -L http://172.17.42.1:4001/v2/keys/proxy/master/my_container_name.container/0.regexp_name -X PUT -d value='^.*\.my-service\.com$'
//  # 例2: 全ての道は Google に通ず
//  curl -L http://172.17.42.1:4001/v2/keys/proxy/master/www.google.com/600613.goog -X PUT -d value='.'
//
// `proxy` の部分はコマンドライン引数、`master` の部分はプロクシのユーザー名が使用される。
func (a *Accounts) Reload() error {
	var containers map[string]*Container
	if a.DockerAddr != "" {
		var err error
		containers, err = getContainers(a.DockerAddr)
		if err != nil {
			return err
		}
	}

	// etcd への登録情報を元に実際のルーティングを組み立てる。
	// "/proxy/アカウント名/接続先/0.正規表現の名前" で値部分が正規表現文字列。
	// 0 はプライオリティ。"0." を省略した場合はプライオリティ 0 として処理される。
	etcdClient := etcd.NewClient([]string{a.EtcdAddr})
	r, err := etcdClient.Get(a.EtcdRoot, false, true)
	if err != nil {
		if etcderr, ok := err.(etcd.EtcdError); ok && etcderr.ErrorCode == 100 {
			// routing information not found
			a.m.Lock()
			a.accounts = make(map[string]Account)
			a.m.Unlock()
			return nil
		}
		return err
	}

	accounts := make(map[string]Account)
	for _, aNode := range r.Node.Nodes {
		account := Account{
			Name: aNode.Key[strings.LastIndex(aNode.Key, "/")+1:],
		}
		for _, toNode := range aNode.Nodes {
			// 接続先を探す。
			host := toNode.Key[strings.LastIndex(toNode.Key, "/")+1:]

			// "foobar.container" の場合は Docker のコンテナへの接続とする。
			const SUFFIX = ".container"
			if len(host) > len(SUFFIX) && host[len(host)-len(SUFFIX):] == SUFFIX {
				containerName := host[:len(host)-len(SUFFIX)]
				if a.DockerAddr == "" {
					log.Println(
						"Docker Remote API not available:", containerName,
						"Account:", account,
					)
					continue
				}

				container, ok := containers[containerName]
				if !ok {
					log.Println(
						"Container not found:", containerName,
						"Account:", account,
					)
					continue
				}
				host = container.IPAddress
			}

			// コンテナに導くための正規表現をコンパイルする。
			for _, reNode := range toNode.Nodes {
				s := strings.SplitN(reNode.Key[strings.LastIndex(reNode.Key, "/")+1:], ".", 2)
				var priority int
				if len(s) < 2 {
					priority = 0
				} else {
					priority, err = strconv.Atoi(s[0])
					if err != nil {
						log.Println(
							"invalid priority value:", err,
							"Account:", account,
							"ConnectTo:", host,
							"RegExp:", reNode.Value,
						)
						continue
					}
				}

				re, err := regexp.Compile(reNode.Value)
				if err != nil {
					log.Println(
						"error at regexp.Compile:", err,
						"Account:", account,
						"ConnectTo:", host,
						"RegExp:", reNode.Value,
						"Priority:", priority,
					)
					continue
				}
				account.Routes = append(account.Routes, &Route{
					Name:     s[len(s)-1],
					Priority: priority,
					Host:     host,
					Regexp:   re,
				})
			}
		}

		sort.Sort(sort.Reverse(account.Routes))
		accounts[account.Name] = account
	}

	if a.Verbose {
		log.Println("new accounts:", accounts)
	}

	a.m.Lock()
	a.accounts = accounts
	a.m.Unlock()

	return nil
}

// get はアカウントリストを安全に取得する。
func (a *Accounts) get() map[string]Account {
	a.m.Lock()
	ret := a.accounts
	a.m.Unlock()

	return ret
}

// Get は accountName に対応するアカウント情報を取得する。
// 該当するアカウントが存在しない場合は nil を返す。
func (a *Accounts) Get(accountName string) *Account {
	if account, ok := a.get()[accountName]; ok {
		return &account
	}
	return nil
}

// Watch は etcd や docker を監視し、変更が見つかる度に自動的にルーティング情報を再構築する。
func (a *Accounts) Watch() error {
	recvEtcd := make(chan *etcd.Response)
	go a.watchEtcdEvent(recvEtcd)

	recvDocker := make(chan *dockerEvent)
	if a.DockerAddr != "" {
		go a.watchDockerEvent(recvDocker)
	}

	// 何か変更があった時はルーティングテーブルの再作成を1秒後にスケジューリングする。
	// 既にスケジューリングされている場合は t が入れ替わるため、結局一番最後のもののみが使用される。
	var t <-chan time.Time

	for {
		select {
		case r := <-recvEtcd:
			if a.Verbose {
				log.Println("etcd notify:", r)
			}
			t = time.After(time.Second)
		case r := <-recvDocker:
			if a.Verbose {
				log.Println("docker notify:", r)
			}
			t = time.After(time.Second)
		case <-t:
			if err := a.Reload(); err != nil {
				log.Println("watch:", err)
				break
			}
		}
	}
}

// watchEtcdEvent は etcd のイベントを検出する度に recv にイベント内容を投げる。
func (a *Accounts) watchEtcdEvent(recv chan *etcd.Response) error {
	etcdClient := etcd.NewClient([]string{a.EtcdAddr})
	for {
		_, err := etcdClient.Watch(a.EtcdRoot, 0, true, recv, nil)
		if err != nil {
			log.Println("watchEtcdEvent:", err)
			continue
		}
	}
}

// dockerEvent は docker のイベントストリーミングAPIで受信したデータを表現する構造体。
type dockerEvent struct {
	Status string `json:"status"`
	ID     string `json:"id"`
	From   string `json:"from"`
	Time   int64  `json:"time"`
}

// watchDockerEvent は docker のイベントを検出する度に recv にイベント内容を投げる。
func (a *Accounts) watchDockerEvent(recv chan<- *dockerEvent) error {
	for {
		func() {
			resp, err := httpGet(a.DockerAddr + "/events")
			if err != nil {
				log.Println("watchDockerEvent:", err)
				return
			}
			defer resp.Body.Close()

			// ストリームが途切れるまで JSON を読み取り随時 recv に流す。
			d := json.NewDecoder(resp.Body)
			for {
				de := new(dockerEvent)
				err = d.Decode(de)
				if err != nil {
					log.Println("watchDockerEvent:", err)
					break
				}
				recv <- de
			}
		}()
		time.Sleep(time.Second)
	}
}
