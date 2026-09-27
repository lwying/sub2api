package routes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	adminhandler "github.com/Wei-Shaw/sub2api/internal/handler/admin"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 票据 08／09 的端到端回归：`body_format`／`header_format` 是**逐层**的来源事实，
// 不是「有没有载荷」的别名。
//
// 这些用例从真实 service 写入（因此来源事实由生产写路径产生），再经公开的管理员路由读出，
// 覆盖四种必须区分的组合：
//
//   - 新明文层开、该层整份跳过（无载荷）→ 仍是明文层，并且带它自己的受限原因码；
//     绝不能被说成 encrypted + not_observed（那会把「本次跳过」显示成「从未采集」）。
//   - 一行里旧密文层与新明文层并存 → 每层各自回答自己的格式与原因码。
//   - 新旧两层对同一次跳过给出**字面完全相同**的原因码 → 只能靠「哪一列写下了结论」区分，
//     不能靠原因码字符串猜（否则旧密文行会被错报成明文）。
//   - 从未被要求过的层 → 状态／原因必须是 not_observed，不是任何 skipped。

const errorDiagnosticProvenancePath = "/api/v1/admin/error-diagnostics/"

// errorDiagnosticRouteRepoStub 是只服务这几条用例的内存仓储。
//
// 它只镜像真实仓储在写入路径上的**收敛**（空状态／原因按未采集落库、stored 必须带载荷），
// 使断言反映的是服务侧写下的来源事实，而不是替身自己发明的事实。
type errorDiagnosticRouteRepoStub struct {
	records map[string]service.ErrorDiagnosticRecord
}

func newErrorDiagnosticRouteRepoStub() *errorDiagnosticRouteRepoStub {
	return &errorDiagnosticRouteRepoStub{records: map[string]service.ErrorDiagnosticRecord{}}
}

func (s *errorDiagnosticRouteRepoStub) CreateErrorDiagnostic(_ context.Context, write service.ErrorDiagnosticWrite, now time.Time) (service.ErrorDiagnosticRecord, error) {
	record := service.ErrorDiagnosticRecord{
		ID:                    write.ID,
		Protocol:              write.Attempt.Protocol,
		Stage:                 write.Attempt.Stage,
		AttemptIndex:          write.Attempt.AttemptIndex,
		UpstreamStatusCode:    write.Attempt.UpstreamStatusCode,
		BodyState:             write.BodyState,
		BodyReason:            write.BodyReason,
		HeaderState:           write.HeaderState,
		HeaderReason:          write.HeaderReason,
		HeaderEntryCount:      write.HeaderEntryCount,
		PlainRecord:           write.PlainRecord,
		PlainBodyState:        write.PlainBodyState,
		PlainBodyReason:       write.PlainBodyReason,
		PlainHeaderState:      write.PlainHeaderState,
		PlainHeaderReason:     write.PlainHeaderReason,
		PlainHeaderEntryCount: write.PlainHeaderEntryCount,
		CreatedAt:             now,
		MetadataExpiresAt:     now.Add(service.ErrorDiagnosticMetadataRetention),
	}
	// 与真实仓储一致：空状态／原因落成「未采集」，不会留下空字符串。
	if record.BodyState == "" {
		record.BodyState = service.ErrorDiagnosticBodyStateNotObserved
	}
	if record.BodyReason == "" {
		record.BodyReason = service.ErrorDiagnosticBodyNotObserved
	}
	if record.HeaderState == "" {
		record.HeaderState = service.ErrorDiagnosticHeaderStateNotObserved
	}
	if record.HeaderReason == "" {
		record.HeaderReason = service.ErrorDiagnosticHeaderNotObserved
	}
	if !write.PlainRecord {
		record.PlainBodyState = service.ErrorDiagnosticBodyStateNotObserved
		record.PlainBodyReason = service.ErrorDiagnosticBodyNotObserved
		record.PlainHeaderState = service.ErrorDiagnosticHeaderStateNotObserved
		record.PlainHeaderReason = service.ErrorDiagnosticHeaderNotObserved
	}
	if len(write.BodyCiphertext) > 0 {
		record.BodyStored = true
		record.BodyBytes = len(write.Attempt.Body)
		record.BodyExpiresAt = now.Add(service.ErrorDiagnosticBodyRetention)
	}
	if len(write.HeaderCiphertext) > 0 {
		record.HeaderStored = true
		record.HeaderExpiresAt = now.Add(service.ErrorDiagnosticHeaderRetention)
	}
	if len(write.PlainBodyPayload) > 0 {
		record.PlainBodyStored = true
		record.PlainBodyBytes = len(write.PlainBodyPayload)
	}
	if len(write.PlainHeaderPayload) > 0 {
		record.PlainHeaderStored = true
		record.PlainHeaderBytes = len(write.PlainHeaderPayload)
	}
	s.records[record.ID] = record
	return record, nil
}

func (s *errorDiagnosticRouteRepoStub) GetErrorDiagnostic(_ context.Context, id string) (service.ErrorDiagnosticRecord, error) {
	record, ok := s.records[id]
	if !ok {
		return service.ErrorDiagnosticRecord{}, service.ErrErrorDiagnosticNotFound
	}
	return record, nil
}

func (s *errorDiagnosticRouteRepoStub) ListErrorDiagnosticsByUsageLog(context.Context, int64, int) ([]service.ErrorDiagnosticRecord, error) {
	return nil, nil
}

func (s *errorDiagnosticRouteRepoStub) ListRecentErrorDiagnostics(context.Context, string, int) ([]service.ErrorDiagnosticRecord, error) {
	return nil, nil
}

func (s *errorDiagnosticRouteRepoStub) ListRecentErrorDiagnosticPage(context.Context, string, time.Time, int, int) ([]service.ErrorDiagnosticRecord, error) {
	out := make([]service.ErrorDiagnosticRecord, 0, len(s.records))
	for _, record := range s.records {
		out = append(out, record)
	}
	return out, nil
}

func (s *errorDiagnosticRouteRepoStub) CountRecentErrorDiagnostics(context.Context, string, time.Time) (int64, error) {
	return int64(len(s.records)), nil
}

func (s *errorDiagnosticRouteRepoStub) ReadErrorDiagnosticBody(context.Context, string, time.Time) ([]byte, error) {
	return nil, service.ErrErrorDiagnosticBodyGone
}

func (s *errorDiagnosticRouteRepoStub) ReadErrorDiagnosticHeaderValues(context.Context, string, time.Time) (service.ErrorDiagnosticHeaderValues, error) {
	return service.ErrorDiagnosticHeaderValues{}, service.ErrErrorDiagnosticHeaderValuesGone
}

func (s *errorDiagnosticRouteRepoStub) ClearExpiredErrorDiagnosticBodies(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}

func (s *errorDiagnosticRouteRepoStub) ClearExpiredErrorDiagnosticHeaderValues(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}

func (s *errorDiagnosticRouteRepoStub) DeleteExpiredErrorDiagnostics(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}

type errorDiagnosticRouteSettingsStub struct {
	settings service.ErrorDiagnosticSettings
}

func (s errorDiagnosticRouteSettingsStub) GetErrorDiagnosticSettings(context.Context) (service.ErrorDiagnosticSettings, error) {
	return s.settings, nil
}

// errorDiagnosticRouteCipherStub 只证明旧密文层有可用密钥：它必须真的加密，
// 否则旧密文层会落成 skipped_encryption_unavailable，就测不到「旧层写下了结论」。
type errorDiagnosticRouteCipherStub struct{}

func (errorDiagnosticRouteCipherStub) Encrypt(plaintext []byte) ([]byte, error) {
	return append([]byte("enc:"), plaintext...), nil
}

func (errorDiagnosticRouteCipherStub) Decrypt([]byte) ([]byte, error) {
	return nil, errors.New("route stub cipher never decrypts")
}

func (errorDiagnosticRouteCipherStub) KeyVersion() int { return 1 }

func newErrorDiagnosticProvenanceRouter(t *testing.T, settings service.ErrorDiagnosticSettings, withCipher bool) (*gin.Engine, *service.ErrorDiagnosticService) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	var cipher service.ErrorDiagnosticBodyCipher
	if withCipher {
		cipher = errorDiagnosticRouteCipherStub{}
	}
	diagnostics := service.NewErrorDiagnosticService(
		newErrorDiagnosticRouteRepoStub(), errorDiagnosticRouteSettingsStub{settings: settings}, cipher)
	handlers := &handler.Handlers{Admin: &handler.AdminHandlers{
		RequestErrorDiagnostic: adminhandler.NewRequestErrorDiagnosticHandler(diagnostics),
	}}
	router := gin.New()
	RegisterAdminRoutes(
		router.Group("/api/v1"), handlers,
		servermiddleware.AdminAuthMiddleware(func(c *gin.Context) { c.Next() }),
		servermiddleware.AuditLogMiddleware(func(c *gin.Context) { c.Next() }),
		servermiddleware.StepUpAuthMiddleware(func(c *gin.Context) { c.Next() }),
		nil, nil,
	)
	return router, diagnostics
}

func requireErrorDiagnosticDetail(t *testing.T, router *gin.Engine, id string) map[string]any {
	t.Helper()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, errorDiagnosticProvenancePath+id, nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	var payload struct {
		Data map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &payload))
	return payload.Data
}

// 新明文正文层开、整份跳过（**没有载荷**）：格式必须仍是明文层，状态与原因是它自己的
// 受限原因码——超限与不合格正文都不留存载荷，因此「没有载荷」不能被当成「不是明文层」。
func TestSkippedPlainBodyStaysOnThePlaintextLayer(t *testing.T) {
	for _, tc := range []struct {
		name       string
		attempt    service.ErrorDiagnosticAttempt
		wantReason string
	}{
		{
			name: "too large",
			attempt: service.ErrorDiagnosticAttempt{
				Protocol: service.ErrorDiagnosticProtocolMessages, Stage: service.ErrorDiagnosticStageWire,
				AttemptIndex: 1, UpstreamStatusCode: 500,
				// 传输层按策略扣住字节：体量超限，因此没有任何载荷可留。
				BodyVerdict: service.ErrorDiagnosticBodyVerdictTooLarge,
			},
			wantReason: service.ErrorDiagnosticBodySkippedTooLarge,
		},
		{
			name: "not text json",
			attempt: service.ErrorDiagnosticAttempt{
				Protocol: service.ErrorDiagnosticProtocolMessages, Stage: service.ErrorDiagnosticStageWire,
				AttemptIndex: 1, UpstreamStatusCode: 500,
				Body: []byte("not-json"), BodyReadComplete: true,
			},
			wantReason: service.ErrorDiagnosticBodySkippedNotTextJSON,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router, diagnostics := newErrorDiagnosticProvenanceRouter(t, service.ErrorDiagnosticSettings{
				Enabled: true, RiskAcknowledged: true, PlainBodyRetentionEnabled: true,
			}, false)

			record, err := diagnostics.RecordErrorDiagnostic(context.Background(), tc.attempt)
			require.NoError(t, err)
			require.Equal(t, service.ErrorDiagnosticBodyStateSkipped, record.PlainBodyState)
			require.False(t, record.PlainBodyStored)

			data := requireErrorDiagnosticDetail(t, router, record.ID)
			require.Equal(t, service.ErrorDiagnosticFormatPlaintext, data["body_format"])
			require.Equal(t, service.ErrorDiagnosticBodyStateSkipped, data["body_state"])
			require.Equal(t, tc.wantReason, data["reason"])

			// 头值层这次根本没有被要求过（两个头值开关都关）：它必须是「未采集」，
			// 而不是任何 skipped——未采集与采集失败是不同的事实。
			require.Equal(t, service.ErrorDiagnosticHeaderStateNotObserved, data["header_state"])
			require.Equal(t, service.ErrorDiagnosticHeaderNotObserved, data["header_reason"])
			require.Equal(t, service.ErrorDiagnosticFormatPlaintext, data["header_format"])
		})
	}
}

// 新明文 429 头值层开、取值整份不合格：格式与原因码同理必须来自明文层。
func TestInvalidPlainHeaderValuesStayOnThePlaintextLayer(t *testing.T) {
	router, diagnostics := newErrorDiagnosticProvenanceRouter(t, service.ErrorDiagnosticSettings{
		Enabled: true, RiskAcknowledged: true, PlainHeaderValueRetentionEnabled: true,
	}, false)

	record, err := diagnostics.RecordErrorDiagnostic(context.Background(), service.ErrorDiagnosticAttempt{
		Protocol: service.ErrorDiagnosticProtocolMessages, Stage: service.ErrorDiagnosticStageWire,
		AttemptIndex: 1, UpstreamStatusCode: http.StatusTooManyRequests,
		HeaderVerdict: service.ErrorDiagnosticHeaderVerdictInvalidValues,
	})
	require.NoError(t, err)
	require.Equal(t, service.ErrorDiagnosticHeaderStateSkipped, record.PlainHeaderState)

	data := requireErrorDiagnosticDetail(t, router, record.ID)
	require.Equal(t, service.ErrorDiagnosticFormatPlaintext, data["header_format"])
	require.Equal(t, service.ErrorDiagnosticHeaderStateSkipped, data["header_state"])
	require.Equal(t, service.ErrorDiagnosticHeaderSkippedInvalidValues, data["header_reason"])

	// 正文层这次没有正文可看：未采集，不是跳过。
	require.Equal(t, service.ErrorDiagnosticBodyStateNotObserved, data["body_state"])
	require.Equal(t, service.ErrorDiagnosticBodyNotObserved, data["reason"])
}

// 一行里旧密文正文层与新明文头值层并存，且**两层对同一次跳过给出字面完全相同的原因码**：
// 只有「哪一列写下了结论」能区分来源，按原因码猜会把旧密文正文错报成明文。
func TestMixedRowKeepsTheLegacyBodyEncryptedWhenBothLayersSkipWithTheSameReason(t *testing.T) {
	router, diagnostics := newErrorDiagnosticProvenanceRouter(t, service.ErrorDiagnosticSettings{
		Enabled: true, RiskAcknowledged: true,
		// 旧密文正文层开着，新明文只开了头值层。
		BodyRetentionEnabled: true, PlainHeaderValueRetentionEnabled: true,
	}, true)

	record, err := diagnostics.RecordErrorDiagnostic(context.Background(), service.ErrorDiagnosticAttempt{
		Protocol: service.ErrorDiagnosticProtocolMessages, Stage: service.ErrorDiagnosticStageWire,
		AttemptIndex: 1, UpstreamStatusCode: http.StatusTooManyRequests,
		Body: []byte("not-json"), BodyReadComplete: true,
		HeaderValues: service.ErrorDiagnosticHeaderValues{Response: map[string]string{"Retry-After": "30"}},
	})
	require.NoError(t, err)
	require.Equal(t, service.ErrorDiagnosticBodyStateSkipped, record.BodyState)
	require.Equal(t, service.ErrorDiagnosticBodySkippedNotTextJSON, record.BodyReason)
	require.Equal(t, record.BodyReason, record.PlainBodyReason,
		"两层共用同一份稳定原因码：字面量相同，来源只能由落在哪一列回答")

	data := requireErrorDiagnosticDetail(t, router, record.ID)
	require.Equal(t, service.ErrorDiagnosticFormatEncrypted, data["body_format"])
	require.Equal(t, service.ErrorDiagnosticBodyStateSkipped, data["body_state"])
	require.Equal(t, service.ErrorDiagnosticBodySkippedNotTextJSON, data["reason"])
	require.Equal(t, service.ErrorDiagnosticFormatPlaintext, data["header_format"])
	require.Equal(t, service.ErrorDiagnosticHeaderStateStored, data["header_state"])
	require.Equal(t, service.ErrorDiagnosticPlainHeaderRetained, data["header_reason"])
}

// 混合行里旧密文正文层真的留了正文：它保留自己的七天窗口与 retained 原因，
// 明文头值层不受影响。
func TestMixedRowKeepsTheLegacyBodySevenDayWindow(t *testing.T) {
	router, diagnostics := newErrorDiagnosticProvenanceRouter(t, service.ErrorDiagnosticSettings{
		Enabled: true, RiskAcknowledged: true,
		BodyRetentionEnabled: true, PlainHeaderValueRetentionEnabled: true,
	}, true)

	record, err := diagnostics.RecordErrorDiagnostic(context.Background(), service.ErrorDiagnosticAttempt{
		Protocol: service.ErrorDiagnosticProtocolMessages, Stage: service.ErrorDiagnosticStageWire,
		AttemptIndex: 1, UpstreamStatusCode: http.StatusTooManyRequests,
		Body: []byte(`{"model":"claude"}`), BodyReadComplete: true,
		HeaderValues: service.ErrorDiagnosticHeaderValues{Response: map[string]string{"Retry-After": "30"}},
	})
	require.NoError(t, err)
	require.True(t, record.BodyStored)

	data := requireErrorDiagnosticDetail(t, router, record.ID)
	require.Equal(t, service.ErrorDiagnosticFormatEncrypted, data["body_format"])
	require.Equal(t, service.ErrorDiagnosticBodyStateStored, data["body_state"])
	require.Equal(t, service.ErrorDiagnosticBodyRetained, data["reason"])
	// 旧密文层仍然有自己的七天窗口：混合不等于延长，也不等于改期。
	require.NotNil(t, data["body_expires_at"], "旧密文层仍然按七天窗口披露到期时刻")
	require.Equal(t, service.ErrorDiagnosticFormatPlaintext, data["header_format"])
	require.Equal(t, service.ErrorDiagnosticHeaderStateStored, data["header_state"])
	require.Nil(t, data["header_expires_at"], "明文层没有自有七天窗口")
}
