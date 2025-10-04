package main

import (
	"bytes"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"

	"github.com/fatih/color"
)

// customRoundTripperは、リクエスト送信直前の最終調整（ヘッダー操作など）とロギングを担う
type customRoundTripper struct {
	logger  *slog.Logger
	proxied http.RoundTripper
	// 色付け用の関数
	reqColor  func(a ...interface{}) string
	respColor func(a ...interface{}) string
}

func (crt *customRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// 1. 最終調整：ReverseProxyが自動追加したX-Forwarded-Forヘッダーを削除
	req.Header.Del("X-Forwarded-For")

	// 2. 記録：送信直前のリクエストをログに出力
	//    この時点ではヘッダーが削除されているため、ログにも出力されない
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
		req.Body.Close()
	}
	req.Body = io.NopCloser(bytes.NewBuffer(body))

	logReq := req.Clone(req.Context())
	logReq.Body = io.NopCloser(bytes.NewBuffer(body))
	reqDump, _ := httputil.DumpRequestOut(logReq, true)

	crt.logger.Info("プロキシからサーバーへのリクエスト", "target", req.URL.String())
	fmt.Println("┌--- [プロキシからサーバーへのリクエスト内容] ---")
	fmt.Println(crt.reqColor(string(reqDump)))
	fmt.Println("└------------------------------------------")

	// 3. 本来の責務：実際にリクエストを送信する
	return crt.proxied.RoundTrip(req)
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	targetURL := flag.String("target", "", "転送先となるSOAPサーバの完全なURL (例: https://example.com/service)")
	listenAddr := flag.String("listen", ":8080", "プロキシが待受するアドレスとポート (例: :8080)")
	flag.Parse()

	if *targetURL == "" {
		logger.Error("必須の引数が指定されていません", "引数", "-target")
		flag.Usage()
		os.Exit(1)
	}

	target, err := url.Parse(*targetURL)
	if err != nil {
		logger.Error("無効なターゲットURLです", "url", *targetURL, "エラー", err)
		os.Exit(1)
	}

	// ★★★ 色付け用のオブジェクトを作成
	reqColorPrinter := color.New(color.FgCyan).SprintFunc()
	respColorPrinter := color.New(color.FgYellow).SprintFunc()

	// 実際に通信を行う、標準のTransportを作成
	baseTransport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}

	// 標準Transportを、自作のロギング用Transportでラップする
	customTransport := &customRoundTripper{
		logger:    logger,
		proxied:   baseTransport,
		reqColor:  reqColorPrinter,
		respColor: respColorPrinter,
	}

	// Director: リクエストを書き換える関数
	director := func(req *http.Request) {
		// 転送先情報をリクエストに設定
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.URL.Path = target.Path
		req.Host = target.Host // Hostヘッダーも明示的に設定
	}

	modifyResponse := func(resp *http.Response) error {
		var body []byte
		if resp.Body != nil {
			body, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
		}

		logger.Info("サーバーからのレスポンス", "status", resp.Status)

		logResp := *resp
		logResp.Body = io.NopCloser(bytes.NewBuffer(body))
		respDump, _ := httputil.DumpResponse(&logResp, true)

		fmt.Println("┌--- [サーバーからのレスポンス内容] ---")
		fmt.Println(respColorPrinter(string(respDump)))
		fmt.Println("└------------------------------------")

		resp.Body = io.NopCloser(bytes.NewBuffer(body))
		return nil
	}

	proxy := &httputil.ReverseProxy{
		Director:       director,
		Transport:      customTransport, // Transportに、自作したロギング用Transportを設定
		ModifyResponse: modifyResponse,
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body []byte
		// r.Bodyがnilでないことを確認してから読み込む
		if r.Body != nil {
			var err error
			body, err = io.ReadAll(r.Body)
			if err != nil {
				logger.Error("リクエストボディの読み込みに失敗しました", "エラー", err)
				http.Error(w, "Server Error", http.StatusInternalServerError)
				return
			}
			r.Body.Close()
		}

		// クライアントからのリクエスト内容をログに出力
		logger.Info("クライアントからのリクエスト", "method", r.Method, "path", r.URL.Path)
		logReq := r.Clone(r.Context()) // リクエストを複製
		logReq.Body = io.NopCloser(bytes.NewBuffer(body))
		reqDump, _ := httputil.DumpRequest(logReq, true)

		fmt.Println("┌--- [クライアントからのリクエスト内容] ---")
		fmt.Println(reqColorPrinter(string(reqDump)))
		fmt.Println("└--------------------------------------")

		// プロキシにリクエストを渡す
		r.Body = io.NopCloser(bytes.NewBuffer(body)) // bodyが空でも問題ない
		proxy.ServeHTTP(w, r)
	})

	logger.Info("プロキシサーバーを起動します", "待受アドレス", *listenAddr)
	logger.Info("リクエストを転送します", "転送先URL", *targetURL)

	if err := http.ListenAndServe(*listenAddr, handler); err != nil {
		logger.Error("サーバーの起動に失敗しました", "エラー", err)
		os.Exit(1)
	}
}
