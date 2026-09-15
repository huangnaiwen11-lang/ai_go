// local-generation-platform 是本地联调专用的生成中台模拟器。
// 它只监听回环地址，不读取生产配置、不访问外部网络，也不改变主站默认路由。
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

	generation "ai-business-service/internal/integrations/generation"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18000", "本地监听地址")
	key := flag.String("request-hmac-key", "local-generation-request-key-for-development-only-32", "本地请求 HMAC 密钥")
	callbackKey := flag.String("callback-hmac-key", "", "本地回调 HMAC 密钥；为空时复用请求密钥")
	callback := flag.Bool("callback", false, "是否向请求中的本地回调地址发送完成回调")
	flag.Parse()

	server := &http.Server{Addr: *addr, Handler: generation.NewLocalPlatformHandler(generation.LocalPlatformOptions{
		RequestHMACKey: *key,
		CallbackHMACKey: *callbackKey,
		Callback:       *callback,
		Now:            time.Now,
	})}
	slog.Info("本地生成中台模拟器已启动", "addr", *addr, "callback", *callback, "pid", os.Getpid())
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("本地生成中台模拟器异常退出", "error", err)
		os.Exit(1)
	}
}
