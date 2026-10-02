//go:build unit

package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type unreadableTraceBody struct{ reads int }

func (b *unreadableTraceBody) Read([]byte) (int, error) { b.reads++; return 0, io.EOF }
func (b *unreadableTraceBody) Close() error             { return nil }

func TestRequestTraceMiddlewareCreatesIndependentIDWithoutReadingDeniedBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var traces []string
	var bodiesObserved []bool
	r := gin.New()
	r.POST("/v1/messages", RequestTraceMiddleware(func(c *gin.Context) bool { return true }, func(c *gin.Context, id string, hasBody bool) {
		traces = append(traces, id)
		bodiesObserved = append(bodiesObserved, hasBody)
	}), func(c *gin.Context) { c.JSON(http.StatusUnauthorized, gin.H{"error": "rejected"}) })

	for i := 0; i < 2; i++ {
		body := &unreadableTraceBody{}
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("no key"))
		req.Header.Set("X-Request-ID", "reused-client-id")
		req.Body = body
		r.ServeHTTP(w, req)
		require.Equal(t, http.StatusUnauthorized, w.Code)
		require.Zero(t, body.reads)
	}
	require.Len(t, traces, 2)
	require.NotEqual(t, traces[0], traces[1])
	require.True(t, regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(traces[0]))
	require.Equal(t, []bool{false, false}, bodiesObserved)
}

// 正文关闭时"仅元信息"是完整应采形态，可达到 stored；任何真实缺口或阶段外协议
// （unsupported）都必须失败关闭，且与缺口阶段出现的位置无关。
func TestRequestTraceStoredStateHandlesBodyDisabledAndUnsupported(t *testing.T) {
	metadata := service.RequestTraceStage{Stage: "client_metadata", State: service.RequestTraceNotObserved, Reason: "metadata_observed"}
	bodyDisabled := service.RequestTraceStage{Stage: "client_entry", State: service.RequestTraceNotObserved, Reason: "capture_body_disabled"}
	wireObserved := service.RequestTraceStage{Stage: "wire_attempt", State: service.RequestTraceNotObserved, Reason: "wire_observed"}
	wireError := service.RequestTraceStage{Stage: "wire_attempt", State: service.RequestTraceNotObserved, Reason: "transport_error"}
	wireUnsupported := service.RequestTraceStage{Stage: "wire_attempt", State: service.RequestTraceUnsupported, Reason: "wire_protocol_outside_phase1"}
	hijacked := service.RequestTraceStage{Stage: "client_response", State: service.RequestTraceNotObserved, Reason: "hijacked_unobservable"}
	storedBody := service.RequestTraceStage{Stage: "client_response", View: "downstream", State: service.RequestTraceStored, Reason: "retained", Payload: []byte(`{"ok":true}`)}
	truncated := service.RequestTraceStage{Stage: "client_response", View: "downstream", State: service.RequestTraceTruncated, Reason: "truncated"}

	cases := []struct {
		name        string
		stages      []service.RequestTraceStage
		bodyCapture bool
		want        bool
	}{
		{"body on with stored stage", []service.RequestTraceStage{metadata, storedBody}, true, true},
		{"body on without any stored stage", []service.RequestTraceStage{metadata, bodyDisabled}, true, false},
		{"body on stored then unsupported still fails", []service.RequestTraceStage{storedBody, wireUnsupported}, true, false},
		{"body on unsupported then stored still fails", []service.RequestTraceStage{wireUnsupported, storedBody}, true, false},
		{"body on stored then truncated fails", []service.RequestTraceStage{storedBody, truncated}, true, false},
		{"body off metadata only is stored", []service.RequestTraceStage{metadata, bodyDisabled, wireObserved}, false, true},
		{"body off with transport error is not stored", []service.RequestTraceStage{metadata, bodyDisabled, wireError}, false, false},
		{"body off with unsupported protocol is not stored", []service.RequestTraceStage{metadata, bodyDisabled, wireUnsupported}, false, false},
		{"body off hijacked is not stored", []service.RequestTraceStage{metadata, bodyDisabled, hijacked}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, requestTraceStoredState(tc.stages, tc.bodyCapture))
		})
	}
}

// 通用 Trace middleware 没有门控快照时保持既有默认（采集正文、硬上限）；
// 有快照时按入口快照读取正文开关与体积上限。
func TestRequestTraceBodyCaptureReadsEntrySnapshotOrDefaults(t *testing.T) {
	ctx := context.Background()
	require.True(t, requestTraceBodyCaptureAllowed(ctx), "无快照时保持默认采集正文")
	require.EqualValues(t, service.RequestTraceBodyLimit, requestTraceBodyMaxBytes(ctx))

	snapshot := withRequestTraceGateSnapshot(ctx, service.RequestTraceGate{
		CaptureAllowed: true,
		Scope: service.RequestTraceSettings{
			CaptureBody: false, BodyMaxBytes: 64 << 10,
		},
	})
	require.False(t, requestTraceBodyCaptureAllowed(snapshot), "快照关闭正文时必须停用正文采集")
	require.EqualValues(t, 64<<10, requestTraceBodyMaxBytes(snapshot))
}
