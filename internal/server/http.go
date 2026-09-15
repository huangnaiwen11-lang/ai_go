package server

import (
	stdhttp "net/http"

	"ai-business-service/internal/conf"

	kratosErrors "github.com/go-kratos/kratos/v3/errors"
	"github.com/go-kratos/kratos/v3/middleware/recovery"
	"github.com/go-kratos/kratos/v3/middleware/validate"
	"github.com/go-kratos/kratos/v3/transport/http"

	"go.einride.tech/aip/fieldbehavior"
	"google.golang.org/protobuf/proto"
)

// NewHTTPServer 创建 HTTP 传输层，并只注册不依赖业务模块的存活探针。
// 业务路由会随对应模块的 DTO、Usecase 和 Service 一起注册，避免出现示例接口残留。
func NewHTTPServer(c *conf.Server) *http.Server {
	var opts = []http.ServerOption{
		http.Filter(RequestIDFilter),
		http.ResponseEncoder(EncodeResponse),
		http.ErrorEncoder(EncodeError),
		http.NotFoundHandler(rootErrorHandler(kratosErrors.New(stdhttp.StatusNotFound, "NOT_FOUND", "Resource not found"))),
		http.MethodNotAllowedHandler(rootErrorHandler(kratosErrors.New(stdhttp.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "Method not allowed"))),
		http.Middleware(
			recovery.Recovery(),
			validate.Validator(func(req any) error {
				if msg, ok := req.(proto.Message); ok {
					if err := fieldbehavior.ValidateRequiredFields(msg); err != nil {
						return err
					}
				}
				return nil
			}),
		),
	}
	if c != nil && c.GetHttp() != nil {
		httpConfig := c.GetHttp()
		if httpConfig.Network != "" {
			opts = append(opts, http.Network(httpConfig.Network))
		}
		if httpConfig.Addr != "" {
			opts = append(opts, http.Address(httpConfig.Addr))
		}
		if httpConfig.Timeout != nil {
			opts = append(opts, http.Timeout(httpConfig.Timeout.AsDuration()))
		}
	}
	srv := http.NewServer(opts...)
	srv.Route("/").GET("/healthz", func(ctx http.Context) error {
		return EncodeResponse(ctx.Response(), ctx.Request(), "ok")
	})
	return srv
}

func rootErrorHandler(err error) stdhttp.Handler {
	return stdhttp.HandlerFunc(func(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
		EncodeError(writer, request, err)
	})
}
