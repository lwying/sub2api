package repository

import (
	"io"
	"net/http"
	"strings"
)

// fixUpstreamResponse 构造一个供传输层测试使用的上游响应。
//
// 它原先定义在已随票据 10 退役删除的 http_upstream_diagnostic_test.go 中，但 Trace 的
// 传输层测试（http_upstream_trace_test.go）仍在使用，因此搬到这里单独保存。
func fixUpstreamResponse(req *http.Request, status int, body string) *http.Response {
	response := &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       http.NoBody,
		Request:    req,
	}
	if body != "" {
		response.Body = io.NopCloser(strings.NewReader(body))
	}
	return response
}
