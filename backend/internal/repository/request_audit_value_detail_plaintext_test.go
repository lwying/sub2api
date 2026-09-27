//go:build unit

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestPlaintextValueDetailWithoutKeyFollowsUsage(t *testing.T) {
	db, mock, _ := newValueDetailSQLMock(t)
	repo := NewRequestAuditValueDetailRepository(db, nil)
	payload, err := service.EncodeRequestAuditValueDetailValues(service.RequestAuditValueDetailValues{Model: "claude-sonnet-4-5"})
	require.NoError(t, err)
	write := service.RequestAuditValueDetailWrite{
		UsageLogID: 91, State: service.RequestAuditValueDetailStateStored,
		Reason:        service.RequestAuditValueDetailRetained,
		StorageFormat: service.RequestAuditValueDetailStoragePlaintextUsageBound,
		Fields:        service.RequestAuditValueDetailFields{Route: "/v1/messages", Protocol: service.RequestAuditProtocolAnthropic},
		Payload:       payload, EntryCount: 1,
	}
	mock.ExpectQuery(`INSERT INTO request_audit_value_details`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(time.Now().UTC()))
	detail, err := repo.CreateRequestAuditValueDetail(context.Background(), write)
	require.NoError(t, err)
	require.True(t, detail.Stored)
	require.Zero(t, detail.KeyVersion)
	require.True(t, detail.ExpiresAt.IsZero())

	now := time.Now().UTC()
	mock.ExpectQuery(`SELECT storage_format, route, protocol, ciphertext, plaintext_payload, expires_at`).
		WillReturnRows(sqlmock.NewRows([]string{"storage_format", "route", "protocol", "ciphertext", "plaintext_payload", "expires_at"}).
			AddRow(service.RequestAuditValueDetailStoragePlaintextUsageBound, service.RequestAuditValueDetailRouteMessages, service.RequestAuditProtocolAnthropic, nil, payload, nil))
	values, err := repo.ReadRequestAuditValueDetailValues(context.Background(), 91, now.Add(365*24*time.Hour))
	require.NoError(t, err)
	require.Equal(t, "claude-sonnet-4-5", values.Model)
	require.NoError(t, mock.ExpectationsWereMet())
}

// 读侧的入站头值必须按**入站路由**的协议复核：Messages 入站 + Responses 出站这类转换里，
// 入站 wire 与出站 wire 是两个闭集，用出站协议解释入站头值会把合法取值判成读侧丢弃。
func TestPlaintextValueDetailReadUsesInboundRouteProtocol(t *testing.T) {
	db, mock, _ := newValueDetailSQLMock(t)
	repo := NewRequestAuditValueDetailRepository(db, nil)
	payload, err := service.EncodeRequestAuditValueDetailValues(service.RequestAuditValueDetailValues{
		Inbound: service.RequestAuditValueDetailInboundValues{
			RequestHeaders: map[string][]string{"Anthropic-Beta": {"claude-code-20250219"}},
		},
		Attempts: []service.RequestAuditValueDetailAttemptValues{{
			Index: 1, Protocol: service.RequestAuditProtocolOpenAIResp,
			ResponseHeaders: map[string][]string{"Retry-After": {"3"}},
		}},
	})
	require.NoError(t, err)
	now := time.Now().UTC()
	mock.ExpectQuery(`SELECT storage_format, route, protocol, ciphertext, plaintext_payload, expires_at`).
		WillReturnRows(sqlmock.NewRows([]string{"storage_format", "route", "protocol", "ciphertext", "plaintext_payload", "expires_at"}).
			AddRow(service.RequestAuditValueDetailStoragePlaintextUsageBound,
				service.RequestAuditValueDetailRouteMessages, service.RequestAuditProtocolOpenAIResp, nil, payload, nil))
	values, err := repo.ReadRequestAuditValueDetailValues(context.Background(), 92, now)
	require.NoError(t, err)
	require.Equal(t, []string{"claude-code-20250219"}, values.Inbound.RequestHeaders["Anthropic-Beta"],
		"入站是 Messages，入站头值必须按 Messages 闭集复核")
	require.Equal(t, []string{"3"}, values.Attempts[0].ResponseHeaders["Retry-After"])
	require.NoError(t, mock.ExpectationsWereMet())
}

// 旧密文行在写下的那一刻还没有「入站路由协议」这个事实，读侧必须保持原有单协议语义：
// 按路由重新解释历史行会让原本可读的头值被判成读侧丢弃而整份拒绝。
func TestLegacyCiphertextValueDetailKeepsSingleProtocolRule(t *testing.T) {
	db, mock, valueCipher := newValueDetailSQLMock(t)
	repo := NewRequestAuditValueDetailRepository(db, valueCipher)
	payload, err := service.EncodeRequestAuditValueDetailValues(service.RequestAuditValueDetailValues{
		Inbound: service.RequestAuditValueDetailInboundValues{
			RequestHeaders: map[string][]string{"Anthropic-Beta": {"claude-code-20250219"}},
		},
	})
	require.NoError(t, err)
	ciphertext, err := valueCipher.Encrypt(payload)
	require.NoError(t, err)
	now := time.Now().UTC()
	mock.ExpectQuery(`SELECT storage_format, route, protocol, ciphertext, plaintext_payload, expires_at`).
		WillReturnRows(sqlmock.NewRows([]string{"storage_format", "route", "protocol", "ciphertext", "plaintext_payload", "expires_at"}).
			AddRow(service.RequestAuditValueDetailStorageEncryptedV1,
				service.RequestAuditValueDetailRouteChatCompletions, service.RequestAuditProtocolAnthropic, ciphertext, nil, now.Add(time.Hour)))
	values, err := repo.ReadRequestAuditValueDetailValues(context.Background(), 93, now)
	require.NoError(t, err)
	require.Equal(t, []string{"claude-code-20250219"}, values.Inbound.RequestHeaders["Anthropic-Beta"],
		"旧密文行仍按写入时的单协议语义解读")
	require.NoError(t, mock.ExpectationsWereMet())
}
