package main

import (
	"context"
	"fmt"

	"github.com/gogf/gf/v2/frame/g"
	"github.com/gogf/gf/v2/net/ghttp"
)

type Response struct {
	Message string      `json:"message" dc:"消息提示"`
	Data    interface{} `json:"data"    dc:"执行结果"`
}

type HelloReq struct {
	g.Meta `path:"/" method:"get" tags:"Test" summary:"Hello world test case"`
	Name   string `v:"required" json:"name" dc:"姓名"`
	Age    int    `v:"required" json:"age"  dc:"年龄"`
}
type HelloRes struct {
	Content string `json:"content" dc:"返回结果"`
}

type Hello struct{}

func (Hello) Say(ctx context.Context, req *HelloReq) (res *HelloRes, err error) {
	res = &HelloRes{
		Content: fmt.Sprintf(
			"Hello %s! Your Age is %d",
			req.Name,
			req.Age,
		),
	}
	return
}

func ResponseMiddleware(r *ghttp.Request) {
	r.Middleware.Next()

	var (
		msg string
		res = r.GetHandlerResponse()
		err = r.GetError()
	)
	if err != nil {
		msg = err.Error()
	} else {
		msg = "OK"
	}
	r.Response.WriteJson(Response{
		Message: msg,
		Data:    res,
	})
}

func main() {
	s := g.Server()
	s.Group("/", func(group *ghttp.RouterGroup) {
		group.Middleware(ResponseMiddleware)
		group.Bind(
			new(Hello),
		)
	})
	s.SetOpenApiPath("/api.json")
	s.SetSwaggerPath("/swagger")
	s.SetPort(8000)
	s.Run()
}
