//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

// errorDiagnosticRepoFake 是 ErrorDiagnosticRepository 的内存实现，
// 记录调用次数与收到的写入决定，便于断言「没有写入」这类否定事实。
type errorDiagnosticRepoFake struct {
	created   []ErrorDiagnosticWrite
	createErr error

	records map[string]ErrorDiagnosticRecord

	// caller 模拟真实仓储持有的解密能力：存储的是密文，返回的是明文。
	caller       ErrorDiagnosticBodyCipher
	body         map[string][]byte
	bodyErr      error
	bodyReads    int
	cleared      int64
	deleted      int64
	clearErr     error
	deleteErr    error
	clearCalls   int
	deleteCalls  int
	getCalls     int
	listCalls    int
	countCalls   int
	backlogCalls int
	backlog      ErrorDiagnosticCleanupBacklog
	backlogErr   error
	listRecent   []string
}

func newErrorDiagnosticRepoFake() *errorDiagnosticRepoFake {
	return &errorDiagnosticRepoFake{
		records: map[string]ErrorDiagnosticRecord{},
		body:    map[string][]byte{},
	}
}

func (f *errorDiagnosticRepoFake) CreateErrorDiagnostic(_ context.Context, write ErrorDiagnosticWrite, now time.Time) (ErrorDiagnosticRecord, error) {
	if f.createErr != nil {
		return ErrorDiagnosticRecord{}, f.createErr
	}
	f.created = append(f.created, write)
	record := ErrorDiagnosticRecord{
		ID:                 write.ID,
		UsageLogID:         write.Attempt.UsageLogID,
		HasUsage:           write.Attempt.UsageLogID > 0,
		Protocol:           write.Attempt.Protocol,
		AttemptIndex:       write.Attempt.AttemptIndex,
		Stage:              write.Attempt.Stage,
		UpstreamStatusCode: write.Attempt.UpstreamStatusCode,
		BodyState:          write.BodyState,
		BodyReason:         write.BodyReason,
		CreatedAt:          now,
		MetadataExpiresAt:  now.Add(ErrorDiagnosticMetadataRetention),
	}
	if len(write.BodyCiphertext) > 0 {
		record.BodyExpiresAt = now.Add(ErrorDiagnosticBodyRetention)
		record.BodyBytes = len(write.Attempt.Body)
		record.BodyKeyVersion = write.BodyKeyVersion
		record.BodyStored = true
		f.body[write.ID] = write.BodyCiphertext
	}
	f.records[write.ID] = record
	return record, nil
}

func (f *errorDiagnosticRepoFake) GetErrorDiagnostic(_ context.Context, id string) (ErrorDiagnosticRecord, error) {
	f.getCalls++
	record, ok := f.records[id]
	if !ok {
		return ErrorDiagnosticRecord{}, ErrErrorDiagnosticNotFound
	}
	return record, nil
}

func (f *errorDiagnosticRepoFake) ListErrorDiagnosticsByUsageLog(_ context.Context, usageLogID int64, limit int) ([]ErrorDiagnosticRecord, error) {
	f.listCalls++
	var out []ErrorDiagnosticRecord
	for _, record := range f.records {
		if record.HasUsage && record.UsageLogID == usageLogID {
			out = append(out, record)
		}
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// sortedErrorDiagnosticIDs 让内存替身的遍历顺序稳定，避免与真实仓储的 ORDER BY 语义脱节。
func (f *errorDiagnosticRepoFake) sortedErrorDiagnosticIDs(protocol string) []string {
	ids := make([]string, 0, len(f.records))
	for id, record := range f.records {
		if protocol != "" && record.Protocol != protocol {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (f *errorDiagnosticRepoFake) ListRecentErrorDiagnostics(_ context.Context, protocol string, limit int) ([]ErrorDiagnosticRecord, error) {
	f.listCalls++
	f.listRecent = append(f.listRecent, protocol)
	out := make([]ErrorDiagnosticRecord, 0, len(f.records))
	for _, id := range f.sortedErrorDiagnosticIDs(protocol) {
		out = append(out, f.records[id])
	}
	return out, nil
}

func (f *errorDiagnosticRepoFake) ListRecentErrorDiagnosticPage(_ context.Context, protocol string, _ time.Time, offset, limit int) ([]ErrorDiagnosticRecord, error) {
	f.listCalls++
	f.listRecent = append(f.listRecent, protocol)
	out := make([]ErrorDiagnosticRecord, 0, len(f.records))
	for _, id := range f.sortedErrorDiagnosticIDs(protocol) {
		out = append(out, f.records[id])
	}
	if offset >= len(out) {
		return nil, nil
	}
	out = out[offset:]
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *errorDiagnosticRepoFake) CountRecentErrorDiagnostics(_ context.Context, protocol string, _ time.Time) (int64, error) {
	f.countCalls++
	var count int64
	for _, record := range f.records {
		if protocol != "" && record.Protocol != protocol {
			continue
		}
		count++
	}
	return count, nil
}

func (f *errorDiagnosticRepoFake) ReadErrorDiagnosticCleanupBacklog(_ context.Context, now time.Time) (ErrorDiagnosticCleanupBacklog, error) {
	f.backlogCalls++
	if f.backlogErr != nil {
		return ErrorDiagnosticCleanupBacklog{}, f.backlogErr
	}
	backlog := f.backlog
	if !backlog.OldestRecordOverdueAt.IsZero() && backlog.RecordsOverdue == 0 {
		backlog.RecordsOverdue = 1
	}
	if !backlog.OldestBodyOverdueAt.IsZero() && backlog.BodiesOverdue == 0 {
		backlog.BodiesOverdue = 1
	}
	return backlog, nil
}

func (f *errorDiagnosticRepoFake) ReadErrorDiagnosticBody(_ context.Context, id string, now time.Time) ([]byte, error) {
	f.bodyReads++
	if f.bodyErr != nil {
		return nil, f.bodyErr
	}
	record, ok := f.records[id]
	if !ok || !record.BodyReadableAt(now) {
		return nil, ErrErrorDiagnosticBodyGone
	}
	ciphertext, ok := f.body[id]
	if !ok {
		return nil, ErrErrorDiagnosticBodyGone
	}
	if f.caller == nil {
		return nil, ErrErrorDiagnosticBodyGone
	}
	plaintext, err := f.caller.Decrypt(ciphertext)
	if err != nil {
		return nil, ErrErrorDiagnosticBodyGone
	}
	return plaintext, nil
}

func (f *errorDiagnosticRepoFake) ClearExpiredErrorDiagnosticBodies(_ context.Context, _ time.Time, _ int) (int64, error) {
	f.clearCalls++
	if f.clearErr != nil {
		return 0, f.clearErr
	}
	return f.cleared, nil
}

func (f *errorDiagnosticRepoFake) DeleteExpiredErrorDiagnostics(_ context.Context, _ time.Time, _ int) (int64, error) {
	f.deleteCalls++
	if f.deleteErr != nil {
		return 0, f.deleteErr
	}
	return f.deleted, nil
}

type errorDiagnosticSettingsFixed struct {
	settings ErrorDiagnosticSettings
	err      error
}

func (s errorDiagnosticSettingsFixed) GetErrorDiagnosticSettings(context.Context) (ErrorDiagnosticSettings, error) {
	return s.settings, s.err
}

type errorDiagnosticCipherFake struct {
	version   int
	fail      bool
	encrypted [][]byte
}

func (c *errorDiagnosticCipherFake) Encrypt(plaintext []byte) ([]byte, error) {
	if c.fail {
		return nil, errors.New("boom")
	}
	out := append([]byte("enc:"), plaintext...)
	c.encrypted = append(c.encrypted, plaintext)
	return out, nil
}

func (c *errorDiagnosticCipherFake) Decrypt(ciphertext []byte) ([]byte, error) {
	if c.fail {
		return nil, errors.New("boom")
	}
	return bytes.TrimPrefix(ciphertext, []byte("enc:")), nil
}

func (c *errorDiagnosticCipherFake) KeyVersion() int { return c.version }

func enabledErrorDiagnostics() ErrorDiagnosticSettings {
	return ErrorDiagnosticSettings{Enabled: true, RiskAcknowledged: true, BodyRetentionEnabled: true}
}

func messagesAttempt() ErrorDiagnosticAttempt {
	return ErrorDiagnosticAttempt{
		Protocol:           ErrorDiagnosticProtocolMessages,
		AttemptIndex:       0,
		Stage:              ErrorDiagnosticStageWire,
		UpstreamStatusCode: 400,
	}
}

func newTestErrorDiagnosticService(t *testing.T, repo ErrorDiagnosticRepository, settings ErrorDiagnosticSettings, cipher ErrorDiagnosticBodyCipher) *ErrorDiagnosticService {
	t.Helper()
	if fake, ok := repo.(*errorDiagnosticRepoFake); ok {
		// 真实仓储用同一把密钥解密；测试替身照此建模。
		fake.caller = cipher
	}
	svc := NewErrorDiagnosticService(repo, errorDiagnosticSettingsFixed{settings: settings}, cipher)
	require.NotNil(t, svc)
	return svc
}

func TestErrorDiagnostic_DefaultOffSuppressesAllWrites(t *testing.T) {
	cases := map[string]ErrorDiagnosticSettings{
		"no settings at all":    {},
		"capture flag only":     {Enabled: true},
		"risk ack without flag": {RiskAcknowledged: true},
		"body flag without ack": {Enabled: true, BodyRetentionEnabled: true},
	}
	for name, settings := range cases {
		t.Run(name, func(t *testing.T) {
			repo := newErrorDiagnosticRepoFake()
			svc := newTestErrorDiagnosticService(t, repo, settings, &errorDiagnosticCipherFake{})

			_, err := svc.RecordErrorDiagnostic(context.Background(), messagesAttempt())
			require.ErrorIs(t, err, ErrErrorDiagnosticDisabled)
			require.Empty(t, repo.created, "关闭状态下不得写入任何行")
			require.False(t, svc.CaptureEnabled(context.Background()))

			snapshot := svc.Counters()
			require.EqualValues(t, 1, snapshot.Attempts)
			require.EqualValues(t, 1, snapshot.DisabledSuppressed)
			require.EqualValues(t, 0, snapshot.StoredRecords)
		})
	}
}

func TestErrorDiagnostic_SettingsReadFailureIsFailClosed(t *testing.T) {
	repo := newErrorDiagnosticRepoFake()
	svc := NewErrorDiagnosticService(repo, errorDiagnosticSettingsFixed{err: errors.New("settings down")}, nil)

	require.False(t, svc.CaptureEnabled(context.Background()))
	_, err := svc.RecordErrorDiagnostic(context.Background(), messagesAttempt())
	require.ErrorIs(t, err, ErrErrorDiagnosticDisabled)
	require.Empty(t, repo.created)
}

func TestErrorDiagnostic_NilSettingsReaderIsFailClosed(t *testing.T) {
	repo := newErrorDiagnosticRepoFake()
	svc := NewErrorDiagnosticService(repo, nil, &errorDiagnosticCipherFake{})
	require.False(t, svc.CaptureEnabled(context.Background()))
	_, err := svc.RecordErrorDiagnostic(context.Background(), messagesAttempt())
	require.ErrorIs(t, err, ErrErrorDiagnosticDisabled)
}

func TestErrorDiagnostic_RejectsNonAllowlistedAttempts(t *testing.T) {
	cases := map[string]ErrorDiagnosticAttempt{
		"uncovered protocol": func() ErrorDiagnosticAttempt {
			a := messagesAttempt()
			a.Protocol = "antigravity.generate"
			return a
		}(),
		"unsupported phase1 stage": func() ErrorDiagnosticAttempt {
			a := messagesAttempt()
			a.Stage = "client_entry"
			return a
		}(),
		"empty stage": func() ErrorDiagnosticAttempt {
			a := messagesAttempt()
			a.Stage = ""
			return a
		}(),
		"status below 4xx": func() ErrorDiagnosticAttempt {
			a := messagesAttempt()
			a.UpstreamStatusCode = 399
			return a
		}(),
		"status above 5xx": func() ErrorDiagnosticAttempt {
			a := messagesAttempt()
			a.UpstreamStatusCode = 600
			return a
		}(),
		"zero status is not a 4xx/5xx": func() ErrorDiagnosticAttempt {
			a := messagesAttempt()
			a.UpstreamStatusCode = 0
			return a
		}(),
		"negative attempt index": func() ErrorDiagnosticAttempt {
			a := messagesAttempt()
			a.AttemptIndex = -1
			return a
		}(),
		"attempt index above bound": func() ErrorDiagnosticAttempt {
			a := messagesAttempt()
			a.AttemptIndex = ErrorDiagnosticMaxAttemptIndex + 1
			return a
		}(),
		"negative usage log id": func() ErrorDiagnosticAttempt {
			a := messagesAttempt()
			a.UsageLogID = -1
			return a
		}(),
	}
	for name, attempt := range cases {
		t.Run(name, func(t *testing.T) {
			repo := newErrorDiagnosticRepoFake()
			svc := newTestErrorDiagnosticService(t, repo, enabledErrorDiagnostics(), &errorDiagnosticCipherFake{})

			_, err := svc.RecordErrorDiagnostic(context.Background(), attempt)
			require.ErrorIs(t, err, ErrErrorDiagnosticInvalidAttempt)
			require.Empty(t, repo.created, "不合格输入不得生成任何行，也不得伪造 4xx/5xx")
			require.EqualValues(t, 1, svc.Counters().RejectedAttempts)
		})
	}
}

func TestErrorDiagnostic_NoUsageAttemptIsStoredIndependently(t *testing.T) {
	repo := newErrorDiagnosticRepoFake()
	svc := newTestErrorDiagnosticService(t, repo, enabledErrorDiagnostics(), nil)

	attempt := messagesAttempt()
	attempt.UpstreamStatusCode = 500
	record, err := svc.RecordErrorDiagnostic(context.Background(), attempt)
	require.NoError(t, err)

	require.False(t, record.HasUsage)
	require.Zero(t, record.UsageLogID)
	require.Equal(t, ErrorDiagnosticBodyStateNotObserved, record.BodyState)
	require.Equal(t, ErrorDiagnosticBodyNotObserved, record.BodyReason)
	require.True(t, record.BodyExpiresAt.IsZero())
	require.Equal(t, record.CreatedAt.Add(ErrorDiagnosticMetadataRetention), record.MetadataExpiresAt)
	require.Len(t, repo.created, 1)
	require.Empty(t, repo.created[0].BodyCiphertext, "票 01 不得写入任何正文")
	require.True(t, ValidErrorDiagnosticID(record.ID))
}

func TestErrorDiagnostic_WriteFailureDoesNotReportSuccess(t *testing.T) {
	repo := newErrorDiagnosticRepoFake()
	repo.createErr = errors.New("insert failed")
	svc := newTestErrorDiagnosticService(t, repo, enabledErrorDiagnostics(), nil)

	_, err := svc.RecordErrorDiagnostic(context.Background(), messagesAttempt())
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrErrorDiagnosticDisabled)

	snapshot := svc.Counters()
	require.EqualValues(t, 1, snapshot.WriteFailures)
	require.EqualValues(t, 0, snapshot.StoredRecords, "整条写入失败不得伪称已有可查询诊断")
}

func TestErrorDiagnostic_IDsAreOpaqueUniqueAndShapeChecked(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		id, err := NewErrorDiagnosticID()
		require.NoError(t, err)
		require.True(t, ValidErrorDiagnosticID(id))
		require.False(t, seen[id], "尝试标识必须唯一")
		seen[id] = true
	}

	for _, invalid := range []string{
		"",
		strings.Repeat("a", ErrorDiagnosticIDLength-1),
		strings.Repeat("a", ErrorDiagnosticIDLength+1),
		strings.ToUpper(strings.Repeat("a", ErrorDiagnosticIDLength)),
		"zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz",
		"' OR 1=1 --",
	} {
		require.False(t, ValidErrorDiagnosticID(invalid), "%q 必须被拒绝", invalid)
	}
}

func TestErrorDiagnostic_GetRejectsMalformedIDsWithoutTouchingStorage(t *testing.T) {
	repo := newErrorDiagnosticRepoFake()
	svc := newTestErrorDiagnosticService(t, repo, enabledErrorDiagnostics(), nil)

	_, err := svc.GetErrorDiagnostic(context.Background(), "not-an-id")
	require.ErrorIs(t, err, ErrErrorDiagnosticNotFound)
	require.Zero(t, repo.getCalls, "形状不合法的 ID 不得进入存储层")
}

func TestErrorDiagnostic_GetHidesMetadataExpiredRecords(t *testing.T) {
	repo := newErrorDiagnosticRepoFake()
	svc := newTestErrorDiagnosticService(t, repo, enabledErrorDiagnostics(), nil)

	record, err := svc.RecordErrorDiagnostic(context.Background(), messagesAttempt())
	require.NoError(t, err)

	now := record.CreatedAt.Add(ErrorDiagnosticMetadataRetention).Add(time.Second)
	svc.now = func() time.Time { return now }

	_, err = svc.GetErrorDiagnostic(context.Background(), record.ID)
	require.ErrorIs(t, err, ErrErrorDiagnosticNotFound, "第 30 天后 API 不可读")
	require.True(t, record.ExpiredAt(now))
}

func TestErrorDiagnostic_ListLimitsAreBounded(t *testing.T) {
	require.Equal(t, ErrorDiagnosticDefaultListLimit, NormalizeErrorDiagnosticListLimit(0))
	require.Equal(t, ErrorDiagnosticDefaultListLimit, NormalizeErrorDiagnosticListLimit(-5))
	require.Equal(t, ErrorDiagnosticMaxListLimit, NormalizeErrorDiagnosticListLimit(ErrorDiagnosticMaxListLimit+1000))
	require.Equal(t, 7, NormalizeErrorDiagnosticListLimit(7))
}

func TestErrorDiagnostic_ListByUsageLogRejectsUnusableIdentifiers(t *testing.T) {
	repo := newErrorDiagnosticRepoFake()
	svc := newTestErrorDiagnosticService(t, repo, enabledErrorDiagnostics(), nil)

	records, err := svc.ListErrorDiagnosticsByUsageLog(context.Background(), 0, 10)
	require.NoError(t, err)
	require.Empty(t, records)
	require.Zero(t, repo.listCalls, "无效的 usage 关联不得查询存储层")
}

func TestErrorDiagnostic_ListRecentRejectsProtocolOutsideAllowlist(t *testing.T) {
	repo := newErrorDiagnosticRepoFake()
	svc := newTestErrorDiagnosticService(t, repo, enabledErrorDiagnostics(), nil)

	_, err := svc.ListRecentErrorDiagnostics(context.Background(), "antigravity.generate", 10)
	require.ErrorIs(t, err, ErrErrorDiagnosticInvalidAttempt)
	require.Zero(t, repo.listCalls)
}

func TestDecideErrorDiagnosticBody_CoversEveryExclusionReason(t *testing.T) {
	// 期望值全部写死，不由被测函数反推；输入是独立 sentinel 正文。
	sentinelBody := []byte(`{"model":"claude","messages":[{"role":"user","content":"sentinel"}]}`)

	cases := []struct {
		name        string
		attempt     ErrorDiagnosticAttempt
		allowed     bool
		cipherReady bool
		wantState   string
		wantReason  string
		wantRetain  bool
	}{
		{
			name:       "no body observed is the ticket 01 default",
			attempt:    ErrorDiagnosticAttempt{},
			wantState:  ErrorDiagnosticBodyStateNotObserved,
			wantReason: ErrorDiagnosticBodyNotObserved,
		},
		{
			name: "non text json",
			attempt: ErrorDiagnosticAttempt{
				Body: []byte{0x00, 0x01, 0xff}, BodyReadComplete: true,
			},
			wantState:  ErrorDiagnosticBodyStateSkipped,
			wantReason: ErrorDiagnosticBodySkippedNotTextJSON,
		},
		{
			name: "valid json shape but invalid utf8 is refused",
			attempt: ErrorDiagnosticAttempt{
				Body: append(append([]byte(`{"a":"`), 0xff), '"', '}'), BodyReadComplete: true,
			},
			wantState:  ErrorDiagnosticBodyStateSkipped,
			wantReason: ErrorDiagnosticBodySkippedNotTextJSON,
		},
		{
			name: "oversized body is not truncated into a claim of completeness",
			attempt: ErrorDiagnosticAttempt{
				Body:             append(exactlyOneMiBJSONBody(), ' '),
				BodyReadComplete: true,
			},
			wantState:  ErrorDiagnosticBodyStateSkipped,
			wantReason: ErrorDiagnosticBodySkippedTooLarge,
		},
		{
			name: "oversized is reported even when the read was also incomplete",
			attempt: ErrorDiagnosticAttempt{
				Body:             append(exactlyOneMiBJSONBody(), ' '),
				BodyReadComplete: false,
			},
			wantState:  ErrorDiagnosticBodyStateSkipped,
			wantReason: ErrorDiagnosticBodySkippedTooLarge,
		},
		{
			name: "incomplete read",
			attempt: ErrorDiagnosticAttempt{
				Body: sentinelBody, BodyReadComplete: false,
			},
			wantState:  ErrorDiagnosticBodyStateSkipped,
			wantReason: ErrorDiagnosticBodySkippedIncompleteRead,
		},
		{
			name: "image attachment",
			attempt: ErrorDiagnosticAttempt{
				Body:             []byte(`{"messages":[{"content":[{"type":"image","image_url":{"url":"https://x/y.png"}}]}]}`),
				BodyReadComplete: true,
			},
			wantState:  ErrorDiagnosticBodyStateSkipped,
			wantReason: ErrorDiagnosticBodySkippedAttachment,
		},
		{
			name: "inline base64 data uri counts as an attachment",
			attempt: ErrorDiagnosticAttempt{
				Body:             []byte(`{"content":"data:image/png;base64,QUFBQQ=="}`),
				BodyReadComplete: true,
			},
			wantState:  ErrorDiagnosticBodyStateSkipped,
			wantReason: ErrorDiagnosticBodySkippedAttachment,
		},
		{
			name: "known structured credential supplied by the client",
			attempt: ErrorDiagnosticAttempt{
				Body:             []byte(`{"model":"claude","fallback_credit_token":"sentinel-value"}`),
				BodyReadComplete: true,
			},
			wantState:  ErrorDiagnosticBodyStateSkipped,
			wantReason: ErrorDiagnosticBodySkippedKnownCredential,
		},
		{
			name: "platform injected authorization value",
			attempt: ErrorDiagnosticAttempt{
				Body:             []byte(`{"headers":{"Authorization":"Bearer sentinel"}}`),
				BodyReadComplete: true,
			},
			wantState:  ErrorDiagnosticBodyStateSkipped,
			wantReason: ErrorDiagnosticBodySkippedKnownCredential,
		},
		{
			name: "body retention not opted in",
			attempt: ErrorDiagnosticAttempt{
				Body: sentinelBody, BodyReadComplete: true,
			},
			allowed:     false,
			cipherReady: true,
			wantState:   ErrorDiagnosticBodyStateSkipped,
			wantReason:  ErrorDiagnosticBodySkippedRetentionDisabled,
		},
		{
			name: "no cipher means no retention",
			attempt: ErrorDiagnosticAttempt{
				Body: sentinelBody, BodyReadComplete: true,
			},
			allowed:     true,
			cipherReady: false,
			wantState:   ErrorDiagnosticBodyStateSkipped,
			wantReason:  ErrorDiagnosticBodySkippedEncryptionUnavailable,
		},
		{
			name: "credential exclusion outranks the policy gates",
			attempt: ErrorDiagnosticAttempt{
				Body: []byte(`{"api_key":"sk-sentinel"}`), BodyReadComplete: true,
			},
			allowed:     false,
			cipherReady: false,
			wantState:   ErrorDiagnosticBodyStateSkipped,
			wantReason:  ErrorDiagnosticBodySkippedKnownCredential,
		},
		{
			name: "eligible body is retained",
			attempt: ErrorDiagnosticAttempt{
				Body: sentinelBody, BodyReadComplete: true,
			},
			allowed:     true,
			cipherReady: true,
			wantState:   ErrorDiagnosticBodyStateStored,
			wantReason:  ErrorDiagnosticBodyRetained,
			wantRetain:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state, reason, retain := DecideErrorDiagnosticBody(tc.attempt, tc.allowed, tc.cipherReady)
			require.Equal(t, tc.wantState, state)
			require.Equal(t, tc.wantReason, reason)
			require.Equal(t, tc.wantRetain, retain)
		})
	}
}

func TestClassifyErrorDiagnosticBody_IsConservative(t *testing.T) {
	// 合格：纯文本 JSON。
	ok := ClassifyErrorDiagnosticBody([]byte(`{"model":"claude","messages":[{"role":"user","content":"hello"}]}`))
	require.True(t, ok.IsTextJSON)
	require.False(t, ok.HasAttachment)
	require.False(t, ok.HasKnownCredential)

	// 非 JSON、空、二进制、截断的 JSON、multipart 都不是文本 JSON。
	for name, body := range map[string][]byte{
		"empty":            nil,
		"not json":         []byte("POST /v1/messages HTTP/1.1"),
		"truncated json":   []byte(`{"model":"claude"`),
		"binary png":       {0x89, 0x50, 0x4e, 0x47},
		"multipart marker": []byte("--boundary\r\nContent-Disposition: form-data\r\n"),
	} {
		require.False(t, ClassifyErrorDiagnosticBody(body).IsTextJSON, name)
	}

	// 附件与凭据分别命中，且大小写不敏感。
	require.True(t, ClassifyErrorDiagnosticBody([]byte(`{"input_file":{"file_id":"f_1"}}`)).HasAttachment)
	require.True(t, ClassifyErrorDiagnosticBody([]byte(`{"X-API-Key":"sentinel"}`)).HasKnownCredential)
	require.True(t, ClassifyErrorDiagnosticBody([]byte(`{"client_secret":"sentinel"}`)).HasKnownCredential)

	// 自由文本里的普通单词不应误判成凭据字段。
	require.False(t, ClassifyErrorDiagnosticBody([]byte(`{"content":"the secret to good code is tests"}`)).HasKnownCredential)
}

func TestErrorDiagnostic_ExactlyOneMiBIsRetained(t *testing.T) {
	// 边界：恰好 1 MiB 的合法 JSON 合格；多 1 字节即不合格。
	atLimit := ErrorDiagnosticAttempt{
		Body: exactlyOneMiBJSONBody(), BodyReadComplete: true,
	}
	state, reason, retain := DecideErrorDiagnosticBody(atLimit, true, true)
	require.Equal(t, ErrorDiagnosticBodyStateStored, state)
	require.Equal(t, ErrorDiagnosticBodyRetained, reason)
	require.True(t, retain)

	over := ErrorDiagnosticAttempt{
		Body: append(exactlyOneMiBJSONBody(), ' '), BodyReadComplete: true,
	}
	state, reason, retain = DecideErrorDiagnosticBody(over, true, true)
	require.Equal(t, ErrorDiagnosticBodyStateSkipped, state)
	require.Equal(t, ErrorDiagnosticBodySkippedTooLarge, reason)
	require.False(t, retain)
}

// exactlyOneMiBJSONBody 造一段恰好 ErrorDiagnosticMaxBodyBytes 字节的合法 JSON，
// 作为与生产判定无关的独立 sentinel 输入。
func exactlyOneMiBJSONBody() []byte {
	head := []byte(`{"padding":"`)
	tail := []byte(`"}`)
	body := make([]byte, 0, ErrorDiagnosticMaxBodyBytes)
	body = append(body, head...)
	body = append(body, bytes.Repeat([]byte("a"), ErrorDiagnosticMaxBodyBytes-len(head)-len(tail))...)
	body = append(body, tail...)
	if len(body) != ErrorDiagnosticMaxBodyBytes {
		panic("sentinel body must be exactly ErrorDiagnosticMaxBodyBytes")
	}
	return body
}

func TestErrorDiagnostic_EncryptionFailureNeverFallsBackToPlaintext(t *testing.T) {
	repo := newErrorDiagnosticRepoFake()
	cipher := &errorDiagnosticCipherFake{fail: true, version: 3}
	svc := newTestErrorDiagnosticService(t, repo, enabledErrorDiagnostics(), cipher)

	attempt := messagesAttempt()
	attempt.Body = []byte(`{"model":"claude","messages":[]}`)
	attempt.BodyReadComplete = true

	record, err := svc.RecordErrorDiagnostic(context.Background(), attempt)
	require.NoError(t, err, "加密失败仍要保留安全元数据")
	require.Equal(t, ErrorDiagnosticBodyStateSkipped, record.BodyState)
	require.Equal(t, ErrorDiagnosticBodySkippedEncryptionUnavailable, record.BodyReason)
	require.Len(t, repo.created, 1)
	require.Empty(t, repo.created[0].BodyCiphertext, "不得明文回退")
	require.Zero(t, repo.created[0].BodyKeyVersion)
	require.EqualValues(t, 1, svc.Counters().BodySkipped)
}

func TestErrorDiagnostic_RetainedBodyIsEncryptedBeforeStorage(t *testing.T) {
	repo := newErrorDiagnosticRepoFake()
	cipher := &errorDiagnosticCipherFake{version: 7}
	svc := newTestErrorDiagnosticService(t, repo, enabledErrorDiagnostics(), cipher)

	plaintext := []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"}]}`)
	attempt := messagesAttempt()
	attempt.Body = plaintext
	attempt.BodyReadComplete = true

	record, err := svc.RecordErrorDiagnostic(context.Background(), attempt)
	require.NoError(t, err)
	require.Equal(t, ErrorDiagnosticBodyStateStored, record.BodyState)

	require.Len(t, repo.created, 1)
	stored := repo.created[0].BodyCiphertext
	require.NotEmpty(t, stored)
	require.NotEqual(t, plaintext, stored, "落库的必须是密文，不是原始字节")
	require.Equal(t, 7, repo.created[0].BodyKeyVersion)
	require.Equal(t, plaintext, cipher.encrypted[0], "加密的必须是调用方观察到的实际出站字节")
	require.Equal(t, record.CreatedAt.Add(ErrorDiagnosticBodyRetention), record.BodyExpiresAt)
	require.EqualValues(t, 1, svc.Counters().BodyStored)
}

func TestErrorDiagnostic_BodyReadIsExplicitAndFailsClosed(t *testing.T) {
	repo := newErrorDiagnosticRepoFake()
	cipher := &errorDiagnosticCipherFake{version: 1}
	svc := newTestErrorDiagnosticService(t, repo, enabledErrorDiagnostics(), cipher)

	attempt := messagesAttempt()
	attempt.Body = []byte(`{"a":1}`)
	attempt.BodyReadComplete = true
	record, err := svc.RecordErrorDiagnostic(context.Background(), attempt)
	require.NoError(t, err)

	body, err := svc.ReadErrorDiagnosticBody(context.Background(), record.ID)
	require.NoError(t, err)
	require.Equal(t, []byte(`{"a":1}`), body)
	require.EqualValues(t, 1, svc.Counters().BodyReads)
}

func TestErrorDiagnostic_BodyReadIsRefusedAfterBodyExpiry(t *testing.T) {
	repo := newErrorDiagnosticRepoFake()
	cipher := &errorDiagnosticCipherFake{version: 1}
	svc := newTestErrorDiagnosticService(t, repo, enabledErrorDiagnostics(), cipher)

	attempt := messagesAttempt()
	attempt.Body = []byte(`{"a":1}`)
	attempt.BodyReadComplete = true
	record, err := svc.RecordErrorDiagnostic(context.Background(), attempt)
	require.NoError(t, err)

	// 第 7 天之后：正文必须不可读，而元数据（未到 30 天）仍可读。
	svc.now = func() time.Time { return record.CreatedAt.Add(ErrorDiagnosticBodyRetention).Add(time.Second) }

	_, err = svc.ReadErrorDiagnosticBody(context.Background(), record.ID)
	require.ErrorIs(t, err, ErrErrorDiagnosticBodyGone)

	metadata, err := svc.GetErrorDiagnostic(context.Background(), record.ID)
	require.NoError(t, err)
	require.Equal(t, ErrorDiagnosticBodyStateExpired, metadata.BodyState)
	require.EqualValues(t, 1, svc.Counters().BodyReadDenied)
}

func TestErrorDiagnostic_BodyReadIsRefusedWithoutCipher(t *testing.T) {
	repo := newErrorDiagnosticRepoFake()
	svc := newTestErrorDiagnosticService(t, repo, enabledErrorDiagnostics(), nil)

	attempt := messagesAttempt()
	attempt.Body = []byte(`{"a":1}`)
	attempt.BodyReadComplete = true
	record, err := svc.RecordErrorDiagnostic(context.Background(), attempt)
	require.NoError(t, err)

	_, err = svc.ReadErrorDiagnosticBody(context.Background(), record.ID)
	require.ErrorIs(t, err, ErrErrorDiagnosticBodyGone)
	require.Zero(t, repo.bodyReads, "缺密钥时不得进入存储层尝试解密")
}

func TestErrorDiagnostic_BodyReadHidesCauseOnlyOutcome(t *testing.T) {
	// 从未留存、已清除、已到期在读取层必须给出同一个结果，避免成为存在性探针。
	repo := newErrorDiagnosticRepoFake()
	svc := newTestErrorDiagnosticService(t, repo, enabledErrorDiagnostics(), &errorDiagnosticCipherFake{})

	attempt := messagesAttempt()
	record, err := svc.RecordErrorDiagnostic(context.Background(), attempt)
	require.NoError(t, err)

	_, err = svc.ReadErrorDiagnosticBody(context.Background(), record.ID)
	require.ErrorIs(t, err, ErrErrorDiagnosticBodyGone)

	_, err = svc.ReadErrorDiagnosticBody(context.Background(), strings.Repeat("f", ErrorDiagnosticIDLength))
	require.ErrorIs(t, err, ErrErrorDiagnosticBodyGone)
}

func TestErrorDiagnostic_NilServiceAndNilRepoAreSafe(t *testing.T) {
	var svc *ErrorDiagnosticService
	require.False(t, svc.CaptureEnabled(context.Background()))
	require.Empty(t, svc.Counters())
	_, err := svc.RecordErrorDiagnostic(context.Background(), messagesAttempt())
	require.ErrorIs(t, err, ErrErrorDiagnosticUnavailable)
	_, err = svc.GetErrorDiagnostic(context.Background(), strings.Repeat("a", ErrorDiagnosticIDLength))
	require.ErrorIs(t, err, ErrErrorDiagnosticNotFound)
	_, err = svc.ReadErrorDiagnosticBody(context.Background(), strings.Repeat("a", ErrorDiagnosticIDLength))
	require.ErrorIs(t, err, ErrErrorDiagnosticBodyGone)

	noRepo := NewErrorDiagnosticService(nil, errorDiagnosticSettingsFixed{settings: enabledErrorDiagnostics()}, nil)
	_, err = noRepo.RecordErrorDiagnostic(context.Background(), messagesAttempt())
	require.ErrorIs(t, err, ErrErrorDiagnosticUnavailable)
	_, err = noRepo.ListRecentErrorDiagnostics(context.Background(), "", 10)
	require.ErrorIs(t, err, ErrErrorDiagnosticUnavailable)
}

func TestErrorDiagnostic_SettingsAccessors(t *testing.T) {
	require.False(t, (ErrorDiagnosticSettings{}).CaptureAllowed())
	require.True(t, (ErrorDiagnosticSettings{Enabled: true, RiskAcknowledged: true}).CaptureAllowed())
	require.False(t, (ErrorDiagnosticSettings{Enabled: true, RiskAcknowledged: true}).BodyCaptureAllowed())
	require.True(t, (ErrorDiagnosticSettings{Enabled: true, RiskAcknowledged: true, BodyRetentionEnabled: true}).BodyCaptureAllowed())
}

func TestDescribeBodyState_MapsReasonToNarrowState(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

	require.Equal(t, ErrorDiagnosticBodyStateStored,
		DescribeBodyState(ErrorDiagnosticBodyRetained, true, now.Add(time.Hour), now))
	require.Equal(t, ErrorDiagnosticBodyStateExpired,
		DescribeBodyState(ErrorDiagnosticBodyRetained, true, now.Add(-time.Hour), now))
	require.Equal(t, ErrorDiagnosticBodyStatePurged,
		DescribeBodyState(ErrorDiagnosticBodyRetained, false, now.Add(-time.Hour), now))
	require.Equal(t, ErrorDiagnosticBodyStateNotObserved,
		DescribeBodyState(ErrorDiagnosticBodyNotObserved, false, time.Time{}, now))
	require.Equal(t, ErrorDiagnosticBodyStateNotObserved,
		DescribeBodyState("", false, time.Time{}, now))
	require.Equal(t, ErrorDiagnosticBodyStateSkipped,
		DescribeBodyState(ErrorDiagnosticBodySkippedTooLarge, false, time.Time{}, now))
	require.Equal(t, ErrorDiagnosticBodyStateSkipped,
		DescribeBodyState(ErrorDiagnosticBodySkippedKnownCredential, false, time.Time{}, now))
}

func TestErrorDiagnostic_PagingIsBoundedAndRejectsOutsideProtocols(t *testing.T) {
	repo := newErrorDiagnosticRepoFake()
	svc := newTestErrorDiagnosticService(t, repo, enabledErrorDiagnostics(), nil)

	require.Equal(t, 0, NormalizeErrorDiagnosticOffset(-10))
	require.Equal(t, 0, NormalizeErrorDiagnosticOffset(0))
	require.Equal(t, 5, NormalizeErrorDiagnosticOffset(5))
	// 偏移不再被夹到单页条数：翻页必须能越过 200，否则会把上一页当成下一页。
	require.Equal(t, 800, NormalizeErrorDiagnosticOffset(800))
	require.Equal(t, ErrorDiagnosticMaxListOffset, NormalizeErrorDiagnosticOffset(ErrorDiagnosticMaxListOffset+1))
	require.Greater(t, ErrorDiagnosticMaxListOffset, ErrorDiagnosticMaxListLimit)

	// 偏移与条数都被收敛到有界范围。
	_, err := svc.ListRecentErrorDiagnosticPage(context.Background(), "", ErrorDiagnosticMaxListOffset*10, ErrorDiagnosticMaxListLimit*10)
	require.NoError(t, err)

	// 协议必须在白名单内，否则不查询存储层。
	before := repo.listCalls
	_, err = svc.ListRecentErrorDiagnosticPage(context.Background(), "antigravity.generate", 0, 10)
	require.ErrorIs(t, err, ErrErrorDiagnosticInvalidAttempt)
	_, err = svc.CountRecentErrorDiagnostics(context.Background(), "antigravity.generate")
	require.ErrorIs(t, err, ErrErrorDiagnosticInvalidAttempt)
	require.Equal(t, before, repo.listCalls)
	require.Zero(t, repo.countCalls)
}

func TestErrorDiagnostic_CountReflectsOnlyReadableRows(t *testing.T) {
	repo := newErrorDiagnosticRepoFake()
	svc := newTestErrorDiagnosticService(t, repo, enabledErrorDiagnostics(), nil)

	for i := 0; i < 3; i++ {
		attempt := messagesAttempt()
		attempt.AttemptIndex = i
		_, err := svc.RecordErrorDiagnostic(context.Background(), attempt)
		require.NoError(t, err)
	}

	total, err := svc.CountRecentErrorDiagnostics(context.Background(), "")
	require.NoError(t, err)
	require.EqualValues(t, 3, total)

	total, err = svc.CountRecentErrorDiagnostics(context.Background(), ErrorDiagnosticProtocolMessages)
	require.NoError(t, err)
	require.EqualValues(t, 3, total)

	// 分页返回的是同一批行，且不会重复。
	page, err := svc.ListRecentErrorDiagnosticPage(context.Background(), "", 0, 2)
	require.NoError(t, err)
	require.Len(t, page, 2)

	rest, err := svc.ListRecentErrorDiagnosticPage(context.Background(), "", 2, 2)
	require.NoError(t, err)
	require.Len(t, rest, 1)
	require.NotEqual(t, page[0].ID, rest[0].ID)

	// 偏移超出窗口时返回空，而不是重复第一页。
	empty, err := svc.ListRecentErrorDiagnosticPage(context.Background(), "", 50, 10)
	require.NoError(t, err)
	require.Empty(t, empty)
}

func TestErrorDiagnostic_PagingHidesMetadataExpiredRows(t *testing.T) {
	repo := newErrorDiagnosticRepoFake()
	svc := newTestErrorDiagnosticService(t, repo, enabledErrorDiagnostics(), nil)

	record, err := svc.RecordErrorDiagnostic(context.Background(), messagesAttempt())
	require.NoError(t, err)
	require.EqualValues(t, 1, svc.Counters().StoredRecords)

	svc.now = func() time.Time { return record.CreatedAt.Add(ErrorDiagnosticMetadataRetention).Add(time.Second) }

	page, err := svc.ListRecentErrorDiagnosticPage(context.Background(), "", 0, 10)
	require.NoError(t, err)
	require.Empty(t, page, "第 30 天后分页不得返回该行")
}

func TestDecideErrorDiagnosticBody_TransportVerdictWithheldBytes(t *testing.T) {
	// transport 在超限／不完整时扣住字节：必须由 verdict 说明，否则只能算「未观察到」。
	cases := []struct {
		name       string
		attempt    ErrorDiagnosticAttempt
		wantState  string
		wantReason string
	}{
		{
			name:       "not_requested means no opt-in",
			attempt:    ErrorDiagnosticAttempt{BodyVerdict: ErrorDiagnosticBodyVerdictNotRequested},
			wantState:  ErrorDiagnosticBodyStateNotObserved,
			wantReason: ErrorDiagnosticBodyNotObserved,
		},
		{
			name:       "too_large with withheld bytes still reports too_large",
			attempt:    ErrorDiagnosticAttempt{BodyVerdict: ErrorDiagnosticBodyVerdictTooLarge},
			wantState:  ErrorDiagnosticBodyStateSkipped,
			wantReason: ErrorDiagnosticBodySkippedTooLarge,
		},
		{
			name:       "incomplete with withheld bytes still reports incomplete_read",
			attempt:    ErrorDiagnosticAttempt{BodyVerdict: ErrorDiagnosticBodyVerdictIncomplete},
			wantState:  ErrorDiagnosticBodyStateSkipped,
			wantReason: ErrorDiagnosticBodySkippedIncompleteRead,
		},
		{
			name: "too_large outranks the policy gates",
			attempt: ErrorDiagnosticAttempt{
				BodyVerdict: ErrorDiagnosticBodyVerdictTooLarge,
			},
			wantState:  ErrorDiagnosticBodyStateSkipped,
			wantReason: ErrorDiagnosticBodySkippedTooLarge,
		},
		{
			name: "verdict wins over the completeness flag",
			attempt: ErrorDiagnosticAttempt{
				BodyVerdict: ErrorDiagnosticBodyVerdictTooLarge, BodyReadComplete: true,
			},
			wantState:  ErrorDiagnosticBodyStateSkipped,
			wantReason: ErrorDiagnosticBodySkippedTooLarge,
		},
		{
			name: "complete falls back to byte-derived qualification",
			attempt: ErrorDiagnosticAttempt{
				BodyVerdict: ErrorDiagnosticBodyVerdictComplete,
				Body:        []byte(`{"model":"claude"}`), BodyReadComplete: true,
			},
			wantState:  ErrorDiagnosticBodyStateStored,
			wantReason: ErrorDiagnosticBodyRetained,
		},
		{
			name: "empty verdict keeps byte-derived qualification",
			attempt: ErrorDiagnosticAttempt{
				Body: []byte(`{"model":"claude"}`), BodyReadComplete: true,
			},
			wantState:  ErrorDiagnosticBodyStateStored,
			wantReason: ErrorDiagnosticBodyRetained,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state, reason, retain := DecideErrorDiagnosticBody(tc.attempt, true, true)
			require.Equal(t, tc.wantState, state)
			require.Equal(t, tc.wantReason, reason)
			require.Equal(t, tc.wantState == ErrorDiagnosticBodyStateStored, retain)
		})
	}
}

func TestErrorDiagnostic_UnknownVerdictWithBytesIsRejected(t *testing.T) {
	// 未知 verdict 且夹带字节：服务不能为这些字节背书，整条写入都不成立。
	attempt := messagesAttempt()
	attempt.BodyVerdict = "made_up_verdict"
	attempt.Body = []byte(`{"model":"claude"}`)
	attempt.BodyReadComplete = true

	repo := newErrorDiagnosticRepoFake()
	svc := newTestErrorDiagnosticService(t, repo, enabledErrorDiagnostics(), &errorDiagnosticCipherFake{})

	_, err := svc.RecordErrorDiagnostic(context.Background(), attempt)
	require.ErrorIs(t, err, ErrErrorDiagnosticInvalidAttempt)
	require.Empty(t, repo.created)

	// 未知 verdict 但没有字节：等同于未观察到正文，元数据仍然落库。
	noBytes := messagesAttempt()
	noBytes.BodyVerdict = "made_up_verdict"
	record, err := svc.RecordErrorDiagnostic(context.Background(), noBytes)
	require.NoError(t, err)
	require.Equal(t, ErrorDiagnosticBodyStateNotObserved, record.BodyState)
	require.Equal(t, ErrorDiagnosticBodyNotObserved, record.BodyReason)

	require.True(t, ErrorDiagnosticBodyVerdictAllowed(""))
	require.True(t, ErrorDiagnosticBodyVerdictAllowed(ErrorDiagnosticBodyVerdictComplete))
	require.False(t, ErrorDiagnosticBodyVerdictAllowed("queue_overflow"))
}

func TestErrorDiagnostic_DroppedDiagnosticsAreCountedWithoutBody(t *testing.T) {
	// 有界转发器溢出时的非阻塞计数：不读设置、不访问存储、不阻塞调用方。
	repo := newErrorDiagnosticRepoFake()
	svc := newTestErrorDiagnosticService(t, repo, ErrorDiagnosticSettings{}, nil)

	for i := 0; i < 5; i++ {
		svc.RecordDroppedErrorDiagnostic()
	}
	snapshot := svc.Counters()
	require.EqualValues(t, 5, snapshot.Dropped)
	require.EqualValues(t, 0, snapshot.Attempts, "丢弃计数不得伪装成尝试")
	require.EqualValues(t, 0, snapshot.StoredRecords)
	require.Empty(t, repo.created)

	// nil 安全。
	var nilSvc *ErrorDiagnosticService
	nilSvc.RecordDroppedErrorDiagnostic()
}

// TestClassifyErrorDiagnosticBody_FoldsKeyNamingStyles 覆盖字段名折叠：
// 结构化凭据与附件字段必须不分 snake_case／camelCase／kebab-case 都能命中，
// 否则客户端用 camelCase 提交凭据时会被误判为可留存。
func TestClassifyErrorDiagnosticBody_FoldsKeyNamingStyles(t *testing.T) {
	credentialCases := []struct {
		name string
		body string
	}{
		{"snake access_token", `{"access_token":"sentinel"}`},
		{"camel accessToken", `{"accessToken":"sentinel"}`},
		{"kebab access-token", `{"access-token":"sentinel"}`},
		{"snake refresh_token", `{"refresh_token":"sentinel"}`},
		{"camel refreshToken", `{"refreshToken":"sentinel"}`},
		{"snake client_secret", `{"client_secret":"sentinel"}`},
		{"camel clientSecret", `{"clientSecret":"sentinel"}`},
		{"snake private_key", `{"private_key":"sentinel"}`},
		{"camel privateKey", `{"privateKey":"sentinel"}`},
		{"snake session_token", `{"session_token":"sentinel"}`},
		{"camel sessionToken", `{"sessionToken":"sentinel"}`},
		{"camel apiKey", `{"apiKey":"sentinel"}`},
		{"camel xApiKey", `{"xApiKey":"sentinel"}`},
		{"camel fallbackCreditToken", `{"fallbackCreditToken":"sentinel"}`},
		{"camel clientAssertion", `{"clientAssertion":"sentinel"}`},
		{"camel setCookie", `{"setCookie":"sentinel"}`},
		{"camel idToken", `{"idToken":"sentinel"}`},
		{"camel proxyAuthorization", `{"proxyAuthorization":"sentinel"}`},
		// 嵌套与数组里的凭据字段同样必须命中。
		{"nested camel", `{"headers":{"authorization":"sentinel"}}`},
		{"array element camel", `{"items":[{"accessToken":"sentinel"}]}`},
	}
	for _, tc := range credentialCases {
		t.Run("credential/"+tc.name, func(t *testing.T) {
			classification := ClassifyErrorDiagnosticBody([]byte(tc.body))
			require.True(t, classification.IsTextJSON, "仍是合法文本 JSON")
			require.True(t, classification.HasKnownCredential, "%s 必须被识别为已知结构化凭据", tc.body)
		})
	}

	attachmentCases := []struct {
		name string
		body string
	}{
		{"snake image_url", `{"image_url":"https://x/y.png"}`},
		{"camel imageUrl", `{"imageUrl":"https://x/y.png"}`},
		{"snake input_file", `{"input_file":{"file_id":"f"}}`},
		{"camel inputFile", `{"inputFile":{"fileId":"f"}}`},
		{"camel mediaType", `{"mediaType":"image/png"}`},
		{"camel inlineData", `{"inlineData":{"data":"x"}}`},
		{"camel imageBase64", `{"imageBase64":"AAAA"}`},
	}
	for _, tc := range attachmentCases {
		t.Run("attachment/"+tc.name, func(t *testing.T) {
			classification := ClassifyErrorDiagnosticBody([]byte(tc.body))
			require.True(t, classification.HasAttachment, "%s 必须被识别为附件", tc.body)
		})
	}

	// 自由文本里的普通单词不得被误判成结构化字段（键名比较只看键，不看值）。
	for _, body := range []string{
		`{"content":"please rotate the access token and the client secret"}`,
		`{"messages":[{"role":"user","content":"my privateKey is safe here"}]}`,
		`{"content":"authorization","meta":{"note":"a word"}}`,
	} {
		classification := ClassifyErrorDiagnosticBody([]byte(body))
		require.True(t, classification.IsTextJSON, body)
		require.False(t, classification.HasKnownCredential, "自由文本不得被当成结构化凭据字段：%s", body)
		require.False(t, classification.HasAttachment, body)
	}
}

// TestNormalizeErrorDiagnosticJSONKey 锁定折叠规则的形状。
func TestNormalizeErrorDiagnosticJSONKey(t *testing.T) {
	for input, want := range map[string]string{
		"access_token":       "accesstoken",
		"accessToken":        "accesstoken",
		"ACCESS_TOKEN":       "accesstoken",
		"access-token":       "accesstoken",
		"x-api-key":          "xapikey",
		"xApiKey":            "xapikey",
		"":                   "",
		"already_normalized": "alreadynormalized",
	} {
		require.Equal(t, want, NormalizeErrorDiagnosticJSONKey(input), input)
	}
}

// TestClassifyErrorDiagnosticBody_SnakeCaseLiteralsStillMatch 锁定旧回退路径覆盖过的那批输入：
// 按字面量做子串匹配的回退已被 fail-closed 取代，但 snake_case／kebab-case 的常见写法
// 仍必须由结构化判定命中——它们本来就是键名字面量与折叠规则的并集。
func TestClassifyErrorDiagnosticBody_SnakeCaseLiteralsStillMatch(t *testing.T) {
	// 合法 JSON 下正常路径即可命中；这里锁定「键名与值都可能出现」的最宽形式仍被拒绝。
	body := []byte(`{"api_key":"sentinel","nested":{"set-cookie":"sentinel"}}`)
	classification := ClassifyErrorDiagnosticBody(body)
	require.True(t, classification.IsTextJSON)
	require.True(t, classification.HasKnownCredential)
}

// TestClassifyErrorDiagnosticBody_DetectsEncodedMediaParts 覆盖编码媒体部件的结构化识别。
//
// ADR 0005 只允许留存「无附件／编码媒体」的文本 JSON，因此三类协议的媒体部件
// （图片、文件、音频）无论嵌套在多少层数组里，都必须在分类阶段被判为附件，
// 否则一份 1 MiB 以内、其余字段完全合格的请求会把 base64 媒体正文带进密文留存。
//
// 这里锁定的是**结构**（媒体类型判别值 + 编码载荷字段），不是对值和自由文本做子串扫描：
// 判别值必须命中已知媒体类型枚举或 MIME 前缀，普通字符串值（含用户正文里的
// “input_audio” 之类的词）不构成证据。
func TestClassifyErrorDiagnosticBody_DetectsEncodedMediaParts(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			// 已验证的漏检：input_audio 载体键与 data 载荷都不在旧名单里。
			name: "chat completions input audio",
			body: `{"model":"gpt-4o-audio-preview","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"AA==","format":"wav"}}]}]}`,
		},
		{
			name: "responses input audio",
			body: `{"model":"gpt-5","input":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"AA==","format":"wav"}}]}]}`,
		},
		{
			// 载体键是通用名（source）：只有 type 判别值 + 载荷字段的结构判定能识别。
			name: "anthropic image source base64 without media type",
			body: `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"AAAA"}}]}]}`,
		},
		{
			// 同上，但按引用提供媒体（url 判别值 + url 载荷）。
			name: "anthropic image source url",
			body: `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}}]}]}`,
		},
		{
			name: "anthropic document source base64",
			body: `{"messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"AAAA"}}]}]}`,
		},
		{
			name: "anthropic image source mime type",
			body: `{"messages":[{"role":"user","content":[{"type":"image","source":{"media_type":"image/jpeg","data":"AAAA"}}]}]}`,
		},
		{
			name: "chat completions file data without data uri",
			body: `{"messages":[{"role":"user","content":[{"type":"file","file":{"filename":"a.pdf","file_data":"JVBERi0xLjQK"}}]}]}`,
		},
		{
			name: "responses input image with data uri",
			body: `{"input":[{"type":"input_image","image_url":"data:image/png;base64,AA=="}]}`,
		},
		{
			name: "gemini inline data",
			body: `{"contents":[{"parts":[{"inline_data":{"mime_type":"image/png","data":"AA=="}}]}]}`,
		},
		{
			// 媒体部件埋在多层数组里：结构识别必须跟随嵌套，而不是只看顶层键。
			name: "deeply nested array media part",
			body: `{"messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"input_audio","input_audio":{"data":"AA=="}}]},{"role":"user","content":[{"type":"image","source":{"data":"AAAA","media_type":"image/webp"}}]}]}`,
		},
		{
			name: "responses input file url",
			body: `{"input":[{"type":"input_file","filename":"a.pdf","file_url":"https://example.com/a.pdf"}]}`,
		},
		{
			name: "media part behind unknown object key",
			body: `{"payload":{"items":[{"part":{"type":"input_file","file_data":"AAAA"}}]}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			classification := ClassifyErrorDiagnosticBody([]byte(tc.body))
			require.True(t, classification.IsTextJSON, "仍是合法文本 JSON")
			require.True(t, classification.HasAttachment, "编码媒体部件必须判为附件：%s", tc.body)
		})
	}
}

// TestClassifyErrorDiagnosticBody_TextOnlyBodiesStayEligible 锁定结构化媒体识别的误报边界：
// 只有普通字段名与自由文本、没有编码媒体载荷的请求必须仍然可留存，
// 否则「工具 schema 里有个叫 data/source/format 的属性」就会让诊断能力大面积失效。
func TestClassifyErrorDiagnosticBody_TextOnlyBodiesStayEligible(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "tool schema declares data source format properties",
			body: `{"model":"claude","tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":{"data":{"type":"string"},"source":{"type":"string"},"format":{"type":"string"},"type":{"type":"string"}}}}}]}`,
		},
		{
			name: "tool schema declares image and audio properties",
			body: `{"tools":[{"parameters":{"properties":{"image":{"type":"string"},"audio":{"type":"string"},"url":{"type":"string"}}}}]}`,
		},
		{
			name: "free text mentions media field names",
			body: `{"messages":[{"role":"user","content":"please summarize the input_audio and image_url fields, then check the base64 payload policy"}],"type":"text"}`,
		},
		{
			name: "tts audio config without payload",
			body: `{"model":"gpt-4o-audio-preview","modalities":["audio","text"],"audio":{"voice":"alloy","format":"mp3"}}`,
		},
		{
			name: "media type list without payload",
			body: `{"type":"image_url","description":"always allowed","detail":"auto"}`,
		},
		{
			name: "nested tool arguments as strings",
			body: `{"messages":[{"role":"tool","content":"{\"type\":\"input_audio\",\"input_audio\":{\"data\":\"AA==\"}}"}]}`,
		},
		{
			name: "ordinary metadata with source and data objects",
			body: `{"type":"json","script":{"data":{"source":"docs"},"format":"text"}}`,
		},
		{
			name: "plain text content parts",
			body: `{"messages":[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"input_text","text":"world"}]}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			classification := ClassifyErrorDiagnosticBody([]byte(tc.body))
			require.True(t, classification.IsTextJSON, "仍是合法文本 JSON")
			require.False(t, classification.HasAttachment, "普通文本 JSON 不得被判为附件：%s", tc.body)
			require.False(t, classification.HasKnownCredential, tc.body)
		})
	}

	// 结构化凭据检测必须在同一次遍历里继续生效，不能被新的媒体识别挤掉。
	credential := ClassifyErrorDiagnosticBody([]byte(`{"content":[{"type":"text","text":"hi"}],"headers":{"x-api-key":"sentinel"}}`))
	require.True(t, credential.HasKnownCredential)
	require.False(t, credential.HasAttachment)
}

// TestClassifyErrorDiagnosticBody_TopLevelScalarsStayEligible 锁定顶层标量输入：
// 它们整体是合法 JSON，不构成任何附件或凭据证据，且遍历必须不依赖栈非空。
func TestClassifyErrorDiagnosticBody_TopLevelScalarsStayEligible(t *testing.T) {
	for _, body := range []string{`"text"`, `"image"`, `123`, `true`, `null`, `[]`, `{}`, `[1,2,3]`} {
		classification := ClassifyErrorDiagnosticBody([]byte(body))
		require.True(t, classification.IsTextJSON, body)
		require.False(t, classification.HasAttachment, body)
		require.False(t, classification.HasKnownCredential, body)
	}
}

// TestErrorDiagnostic_NearOneMiBEmbeddedMediaPartIsNotStored 覆盖完整写入路径：
// 一份体量刚好在上限内、其余字段完全合格的请求，只要夹带编码媒体部件，
// 就必须以 skipped_attachment 收敛，且绝不写出任何密文。
func TestErrorDiagnostic_NearOneMiBEmbeddedMediaPartIsNotStored(t *testing.T) {
	body := nearOneMiBJSONBodyWithEmbeddedAudioPart(t)
	require.LessOrEqual(t, len(body), ErrorDiagnosticMaxBodyBytes)

	state, reason, retain := DecideErrorDiagnosticBody(ErrorDiagnosticAttempt{
		Body: body, BodyReadComplete: true,
	}, true, true)
	require.Equal(t, ErrorDiagnosticBodyStateSkipped, state)
	require.Equal(t, ErrorDiagnosticBodySkippedAttachment, reason)
	require.False(t, retain)

	repo := newErrorDiagnosticRepoFake()
	cipher := &errorDiagnosticCipherFake{version: 1}
	svc := newTestErrorDiagnosticService(t, repo, enabledErrorDiagnostics(), cipher)

	attempt := messagesAttempt()
	attempt.Body = body
	attempt.BodyReadComplete = true

	record, err := svc.RecordErrorDiagnostic(context.Background(), attempt)
	require.NoError(t, err)
	require.Equal(t, ErrorDiagnosticBodyStateSkipped, record.BodyState)
	require.Equal(t, ErrorDiagnosticBodySkippedAttachment, record.BodyReason)
	require.Len(t, repo.created, 1, "元数据仍要落库，只是不带正文")
	require.Empty(t, repo.created[0].BodyCiphertext, "编码媒体正文绝不落库")
	require.Empty(t, cipher.encrypted)
	require.EqualValues(t, 1, svc.Counters().BodySkipped)
}

// nearOneMiBJSONBodyWithEmbeddedAudioPart 造一份接近上限、含 input_audio 媒体部件的合法 JSON，
// 模拟「体量合格但夹带编码媒体」的真实出站正文。
func nearOneMiBJSONBodyWithEmbeddedAudioPart(t *testing.T) []byte {
	t.Helper()
	head := []byte(`{"model":"gpt-4o-audio-preview","padding":"`)
	tail := []byte(`","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"AA==","format":"wav"}}]}]}`)
	body := make([]byte, 0, ErrorDiagnosticMaxBodyBytes)
	body = append(body, head...)
	body = append(body, bytes.Repeat([]byte("a"), ErrorDiagnosticMaxBodyBytes-len(head)-len(tail))...)
	body = append(body, tail...)
	require.Len(t, body, ErrorDiagnosticMaxBodyBytes)
	require.True(t, utf8.Valid(body))
	require.True(t, json.Valid(body))
	return body
}

// TestErrorDiagnostic_ManyKeyBodyCannotSmuggleKnownCredential 覆盖已验证的绕过路径：
//
// 一份体量在 1 MiB 以内、合法且其余字段完全合格的正文，只要键 token 数超过遍历上限，
// 遍历就会中止；旧的实现此时退回「按字面量做子串匹配」的弱判定，而弱判定只认
// snake_case／kebab-case 的字面键名，于是 camelCase 的已知凭据（fallbackCreditToken）
// 被判为「不含凭据」，整份被加密留存、可被管理端解密查看。
//
// 修好之后，遍历被中止本身就是「不留存」的理由：上限路径一律 fail-closed，
// 不再有任何弱判定为它放行。
func TestErrorDiagnostic_ManyKeyBodyCannotSmuggleKnownCredential(t *testing.T) {
	cases := map[string]string{
		"camel case credential beyond the key cap": `,"fallbackCreditToken":"sentinel-credential"`,
		"snake case credential beyond the key cap": `,"fallback_credit_token":"sentinel-credential"`,
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			body := errorDiagnosticShortKeyJSONBody(errorDiagnosticMaxWalkedJSONKeys+1, extra)
			require.LessOrEqual(t, len(body), ErrorDiagnosticMaxBodyBytes, "绕过路径必须落在 1 MiB 上限内才成立")
			require.True(t, utf8.Valid(body))
			require.True(t, json.Valid(body), "必须是合法 JSON，否则会在更早的前置判定被拒")
			require.Contains(t, string(body), "sentinel-credential", "凭据确实在正文里")

			// 攻防前提：键数越过上限，凭据位于上限之后，遍历根本到不了它。
			_, result := errorDiagnosticClassifyJSONStructure(body)
			require.Equal(t, errorDiagnosticJSONWalkKeyCapExceeded, result)

			classification := ClassifyErrorDiagnosticBody(body)
			require.True(t, classification.IsTextJSON, "字节层面它仍是文本 JSON")
			require.True(t, classification.HasAttachment, "遍历中止即整份不合格，不得退回弱字面量判定")

			state, reason, retain := DecideErrorDiagnosticBody(ErrorDiagnosticAttempt{
				Body: body, BodyReadComplete: true,
			}, true, true)
			require.Equal(t, ErrorDiagnosticBodyStateSkipped, state)
			require.Equal(t, ErrorDiagnosticBodySkippedAttachment, reason)
			require.False(t, retain)

			// 完整写入路径：元数据照落库，密文与加密调用都不得出现。
			repo := newErrorDiagnosticRepoFake()
			cipher := &errorDiagnosticCipherFake{version: 1}
			svc := newTestErrorDiagnosticService(t, repo, enabledErrorDiagnostics(), cipher)

			attempt := messagesAttempt()
			attempt.Body = body
			attempt.BodyReadComplete = true
			record, err := svc.RecordErrorDiagnostic(context.Background(), attempt)
			require.NoError(t, err, "正文不合格仍要保留安全元数据")
			require.Equal(t, ErrorDiagnosticBodyStateSkipped, record.BodyState)
			require.Equal(t, ErrorDiagnosticBodySkippedAttachment, record.BodyReason)
			require.Len(t, repo.created, 1)
			require.Empty(t, repo.created[0].BodyCiphertext, "凭据绝不能被加密留存")
			require.Empty(t, cipher.encrypted, "不合格正文不得进入加密")
			require.EqualValues(t, 1, svc.Counters().BodySkipped)
		})
	}
}

// TestErrorDiagnostic_ManyKeyBodyCannotSmuggleStructuralMedia 是同一绕过路径的媒体版本：
// 媒体部件同样藏在键数上限之后，弱判定识别不了「通用载体键（source）+ 判别值（image/base64）」
// 这种结构，因此旧实现会把它当作普通文本 JSON 加密留存。
func TestErrorDiagnostic_ManyKeyBodyCannotSmuggleStructuralMedia(t *testing.T) {
	body := errorDiagnosticShortKeyJSONBody(
		errorDiagnosticMaxWalkedJSONKeys+1,
		`,"content":[{"type":"image","source":{"type":"base64","data":"QUFBQQ=="}}]`,
	)
	require.LessOrEqual(t, len(body), ErrorDiagnosticMaxBodyBytes)
	require.True(t, utf8.Valid(body))
	require.True(t, json.Valid(body))
	require.Contains(t, string(body), `"source":{"type":"base64"`, "媒体部件确实在正文里")

	_, result := errorDiagnosticClassifyJSONStructure(body)
	require.Equal(t, errorDiagnosticJSONWalkKeyCapExceeded, result)

	classification := ClassifyErrorDiagnosticBody(body)
	require.True(t, classification.IsTextJSON)
	require.True(t, classification.HasAttachment)

	repo := newErrorDiagnosticRepoFake()
	cipher := &errorDiagnosticCipherFake{version: 1}
	svc := newTestErrorDiagnosticService(t, repo, enabledErrorDiagnostics(), cipher)

	attempt := messagesAttempt()
	attempt.Body = body
	attempt.BodyReadComplete = true
	record, err := svc.RecordErrorDiagnostic(context.Background(), attempt)
	require.NoError(t, err)
	require.Equal(t, ErrorDiagnosticBodyStateSkipped, record.BodyState)
	require.Equal(t, ErrorDiagnosticBodySkippedAttachment, record.BodyReason)
	require.Len(t, repo.created, 1)
	require.Empty(t, repo.created[0].BodyCiphertext, "编码媒体正文绝不落库")
	require.Empty(t, cipher.encrypted)
}

// TestErrorDiagnostic_AtKeyCapStructureStillDecides 锁定上限自身的边界：
// 恰好等于上限的键数仍在正常路径里，普通文本 JSON 继续可留存、camelCase 凭据继续被
// **结构化判定**命中——fail-closed 只针对「遍历被迫中止」，不针对「键多但能遍历完」。
func TestErrorDiagnostic_AtKeyCapStructureStillDecides(t *testing.T) {
	clean := errorDiagnosticShortKeyJSONBody(errorDiagnosticMaxWalkedJSONKeys, "")
	require.LessOrEqual(t, len(clean), ErrorDiagnosticMaxBodyBytes)
	require.True(t, json.Valid(clean))
	_, result := errorDiagnosticClassifyJSONStructure(clean)
	require.Equal(t, errorDiagnosticJSONWalkComplete, result)

	classification := ClassifyErrorDiagnosticBody(clean)
	require.True(t, classification.IsTextJSON)
	require.False(t, classification.HasAttachment)
	require.False(t, classification.HasKnownCredential)

	state, reason, retain := DecideErrorDiagnosticBody(ErrorDiagnosticAttempt{
		Body: clean, BodyReadComplete: true,
	}, true, true)
	require.Equal(t, ErrorDiagnosticBodyStateStored, state)
	require.Equal(t, ErrorDiagnosticBodyRetained, reason)
	require.True(t, retain)

	// 上限内且能遍历完：凭据是靠结构判定命中的（键名折叠），不是因为上限放行。
	credential := errorDiagnosticShortKeyJSONBody(
		errorDiagnosticMaxWalkedJSONKeys-1,
		`,"fallbackCreditToken":"sentinel-credential"`,
	)
	_, result = errorDiagnosticClassifyJSONStructure(credential)
	require.Equal(t, errorDiagnosticJSONWalkComplete, result)
	classification = ClassifyErrorDiagnosticBody(credential)
	require.True(t, classification.HasKnownCredential)

	state, reason, retain = DecideErrorDiagnosticBody(ErrorDiagnosticAttempt{
		Body: credential, BodyReadComplete: true,
	}, true, true)
	require.Equal(t, ErrorDiagnosticBodyStateSkipped, state)
	require.Equal(t, ErrorDiagnosticBodySkippedKnownCredential, reason)
	require.False(t, retain)
}

// TestErrorDiagnosticClassifyJSONStructure_WalkFailureIsNeverComplete 锁定遍历的失败语义：
// 解码器无法走完正文时，结论必须是「拒绝」而不是「没找到证据」——否则上限之外的
// 任何中止都会退化成放行。
//
// 这条分支在公开接缝上不可达：ClassifyErrorDiagnosticBody 先要求 json.Valid 通过，
// 而校验器与解码器共用同一个嵌套上限（10000 层嵌套），所以这里直接调用遍历函数，
// 用一份超过嵌套上限的深正文覆盖它。将来若两处上限脱节，这里就是第一道报警。
func TestErrorDiagnosticClassifyJSONStructure_WalkFailureIsNeverComplete(t *testing.T) {
	deep := []byte(strings.Repeat("[", 10_001) + strings.Repeat("]", 10_001))
	require.False(t, json.Valid(deep), "该正文连 json.Valid 都过不了，这正是它在公开接缝上不可达的原因")

	walked, result := errorDiagnosticClassifyJSONStructure(deep)
	require.Equal(t, errorDiagnosticJSONWalkDecoderRejected, result)
	require.False(t, walked.IsTextJSON, "遍历失败时不得给出任何「合格」结论")
	require.False(t, walked.HasAttachment)
	require.False(t, walked.HasKnownCredential)
}

// errorDiagnosticShortKeyJSONBody 造一份合法 JSON：键 token 数为 keys，
// 末尾附加一段调用方给定的额外字段（extra 需自带前导逗号，可为空）。
//
// 键名刻意取极短且循环复用：要在 1 MiB 之内挤进超过 errorDiagnosticMaxWalkedJSONKeys 个键，
// 平均每个键值对只剩约 6 字节。JSON 允许重复键名，而遍历是按**键 token 计数**的——
// 这正是本组用例要覆盖的性质：决定遍历是否越过上限的是键的个数，不是键名的唯一性。
// 额外的字段写在键列表之后，因此在上限被触发的用例里，它位于遍历到不了的位置。
func errorDiagnosticShortKeyJSONBody(keys int, extra string) []byte {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	body := make([]byte, 0, keys*6+len(extra)+2)
	body = append(body, '{')
	for i := 0; i < keys; i++ {
		if i > 0 {
			body = append(body, ',')
		}
		body = append(body, '"', alphabet[i%len(alphabet)], '"', ':', '0')
	}
	body = append(body, extra...)
	body = append(body, '}')
	return body
}
