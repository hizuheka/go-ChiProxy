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
	"time"

	"github.com/fatih/color"
)

// customRoundTripperは、リクエストとレスポンスのロギングと、通信の実行を担う
type customRoundTripper struct {
	logger    *slog.Logger
	proxied   http.RoundTripper
	reqColor  func(a ...interface{}) string
	respColor func(a ...interface{}) string
}

// RoundTrip は http.RoundTripper インターフェースを実装します。
func (crt *customRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	startTime := time.Now()

	// ReverseProxyが自動追加したX-Forwarded-Forヘッダーを削除
	req.Header.Del("X-Forwarded-For")

	// --- リクエストのロギング ---
	var reqBody []byte
	if req.Body != nil {
		var err error
		reqBody, err = io.ReadAll(req.Body)
		if err != nil {
			crt.logger.Error("リクエストボディの読み込みに失敗しました", "エラー", err)
			return nil, err
		}
	}
	// オリジナルのリクエストのボディを復元。これは後ほどサーバに転送されるため。
	req.Body = io.NopCloser(bytes.NewBuffer(reqBody))

	// ログ出力専用にリクエストを複製（クローン）する
	logReq := req.Clone(req.Context())
	// 複製したリクエストにも、新しいボディを設定する
	logReq.Body = io.NopCloser(bytes.NewBuffer(reqBody))

	// 複製したリクエストをダンプする（こちらのボディだけが消費される）
	reqDump, err := httputil.DumpRequestOut(logReq, true)
	if err != nil {
		crt.logger.Error("リクエストのダンプに失敗しました", "エラー", err)
	} else {
		crt.logger.Info("プロキシからサーバーへのリクエスト", "method", req.Method, "target", req.URL.String())
		fmt.Println("┌--- [プロキシからサーバーへのリクエスト内容] ---")
		fmt.Println(crt.reqColor(string(reqDump)))
		fmt.Println("└------------------------------------------")
	}

	// --- 実際にリクエストを送信 ---
	// オリジナルのリクエスト（ボディは未読の状態）を渡す
	resp, err := crt.proxied.RoundTrip(req)
	duration := time.Since(startTime)

	// ネットワークエラーなど、レスポンスが取得できなかった場合のエラーハンドリング
	if err != nil {
		crt.logger.Error("ターゲットへのリクエストが失敗しました", "エラー", err, "処理時間", duration)
		return nil, err
	}

	// --- レスポンスのロギング ---
	var respBody []byte
	if resp.Body != nil {
		var readErr error
		respBody, readErr = io.ReadAll(resp.Body)
		if readErr != nil {
			crt.logger.Error("レスポンスボディの読み込みに失敗しました", "エラー", readErr)
			return resp, readErr
		}
	}
	// オリジナルのレスポンスのボディを復元。これは最終的にクライアントに返されるため。
	resp.Body = io.NopCloser(bytes.NewBuffer(respBody))

	// ログ出力用にレスポンスのシャローコピーを作成
	logResp := *resp
	// コピーしたレスポンスに、新しいボディを設定する
	logResp.Body = io.NopCloser(bytes.NewBuffer(respBody))

	// コピーしたレスポンスをダンプする（こちらのボディだけが消費される）
	respDump, err := httputil.DumpResponse(&logResp, true)
	if err != nil {
		crt.logger.Error("レスポンスのダンプに失敗しました", "エラー", err)
	} else {
		crt.logger.Info("サーバーからのレスポンス", "status", resp.Status, "処理時間", duration)
		fmt.Println("┌--- [サーバーからのレスポンス内容] ---")
		fmt.Println(crt.respColor(string(respDump)))
		fmt.Println("└------------------------------------")
	}

	// オリジナルのレスポンス（ボディは未読の状態）を返す
	return resp, nil
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

	reqColorPrinter := color.New(color.FgCyan).SprintFunc()
	respColorPrinter := color.New(color.FgYellow).SprintFunc()

	// 実際に通信を行う、標準のTransportを作成
	baseTransport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // insecureフラグを適用
	}

	// 標準Transportを、自作のロギング用Transportでラップする
	customTransport := &customRoundTripper{
		logger:    logger,
		proxied:   baseTransport,
		reqColor:  reqColorPrinter,
		respColor: respColorPrinter,
	}

	director := func(req *http.Request) {
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.URL.Path = target.Path
		req.Host = target.Host
	}

	proxy := &httputil.ReverseProxy{
		Director:  director,
		Transport: customTransport, // 自作したロギング用Transportを設定
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			logger.Error("プロキシ処理中にエラーが発生しました", "エラー", err)
			http.Error(w, "プロキシ エラー", http.StatusBadGateway)
		},
	}

	// このハンドラはクライアントからの初回リクエストのみをログに出力する
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body []byte
		if r.Body != nil {
			var err error
			body, err = io.ReadAll(r.Body)
			if err != nil {
				logger.Error("クライアントからのリクエストボディ読み込みに失敗しました", "エラー", err)
				http.Error(w, "Server Error", http.StatusInternalServerError)
				return
			}
			r.Body.Close()
		}

		logger.Info("クライアントからのリクエストを受信", "method", r.Method, "path", r.URL.Path, "remote_addr", r.RemoteAddr)
		logReq := r.Clone(r.Context())
		logReq.Body = io.NopCloser(bytes.NewBuffer(body))
		reqDump, err := httputil.DumpRequest(logReq, true)
		if err != nil {
			logger.Error("クライアントリクエストのダンプに失敗しました", "エラー", err)
		} else {
			fmt.Println("┌--- [クライアントからのリクエスト内容] ---")
			fmt.Println(reqColorPrinter(string(reqDump)))
			fmt.Println("└--------------------------------------")
		}

		r.Body = io.NopCloser(bytes.NewBuffer(body))
		proxy.ServeHTTP(w, r)
	})

	logger.Info("プロキシサーバーを起動します", "待受アドレス", *listenAddr)
	logger.Info("リクエストを転送します", "転送先URL", *targetURL)

	if err := http.ListenAndServe(*listenAddr, handler); err != nil {
		logger.Error("サーバーの起動に失敗しました", "エラー", err)
		os.Exit(1)
	}
}
