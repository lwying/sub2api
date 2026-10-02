package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

// The export service walks one monotonic trace_id cursor at a time, so a page has
// to stay strictly ascending and never exceed the requested limit. The upper limit
// mirrors the export service's own accepted page size with headroom for callers
// that ask for a bigger page than the service itself uses.
const requestTraceExportSourceMaxLimit = 1000

// The export service refuses an approved detail with more than 1000 stages. Paging
// the stage read to the same bound keeps a single read bounded in memory while
// never rejecting a record that the export service itself would have accepted.
const requestTraceExportSourceMaxStages = 1000

var (
	requestTraceExportSourceIDShape = regexp.MustCompile(`^[0-9a-f]{32}$`)

	errRequestTraceExportSourceUnavailable = errors.New("request trace export source has no queryer")
)

// Selected exports add only their explicit bounded ID set; all query filters use
// requestTraceFilterWhere and requestTraceFilterArgs shared with list/stats.
const requestTraceExportFilterWhere = requestTraceFilterWhere + ` AND ($20::text[] IS NULL OR trace_id = ANY($20::text[]))`

func requestTraceExportFilterArgs(filter service.RequestTraceExportFilter) []any {
	args := requestTraceMetadataFilterArgs(filter)
	var selected any
	if len(filter.TraceIDs) > 0 {
		selected = pq.Array(filter.TraceIDs)
	}
	return append(args, selected)
}

// 导出详情次序必须与 Trace 列表一致：created_at DESC, id DESC。次序键是
// (created_at, 内部自增 id)，游标必须同时钉住这两个键——只钉 trace_id 会在
// created_at 并列时漏行或重放，只钉 created_at 会在并列时间戳上重复。内部 id
// 只出现在这个不透明游标里用于排序，绝不写入 JSONL、清单、日志或任何管理端可见
// 字段；外部 trace_id 才是导出的行标识。
const (
	requestTraceExportCursorPrefix = "v1:"
	// 有界：v1:<19 位微秒>:<19 位 id> 远小于此，超出即判为非法游标。
	requestTraceExportCursorMaxLen = 64
)

type requestTraceExportCursor struct {
	createdAtMicros int64
	internalID      int64
}

func encodeRequestTraceExportCursor(createdAt time.Time, internalID int64) string {
	return fmt.Sprintf("%s%d:%d", requestTraceExportCursorPrefix, createdAt.UTC().UnixMicro(), internalID)
}

// decodeRequestTraceExportCursor 严格解析游标：前缀、长度、段数、纯数字与正数
// 全部校验，任何不合规的串都判为非法，绝不猜测或部分接受。
func decodeRequestTraceExportCursor(token string) (requestTraceExportCursor, bool) {
	if len(token) == 0 || len(token) > requestTraceExportCursorMaxLen ||
		!strings.HasPrefix(token, requestTraceExportCursorPrefix) {
		return requestTraceExportCursor{}, false
	}
	parts := strings.Split(token[len(requestTraceExportCursorPrefix):], ":")
	if len(parts) != 2 || !requestTraceExportCursorDigits(parts[0]) || !requestTraceExportCursorDigits(parts[1]) {
		return requestTraceExportCursor{}, false
	}
	micros, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || micros <= 0 {
		return requestTraceExportCursor{}, false
	}
	internalID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || internalID <= 0 {
		return requestTraceExportCursor{}, false
	}
	return requestTraceExportCursor{createdAtMicros: micros, internalID: internalID}, true
}

func requestTraceExportCursorDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// strictlyBefore 报告 (createdAt, internalID) 是否严格排在游标之前（列表次序）。
func (c requestTraceExportCursor) strictlyBefore(createdAt time.Time, internalID int64) bool {
	cursorTime := time.UnixMicro(c.createdAtMicros).UTC()
	if createdAt.Before(cursorTime) {
		return true
	}
	return createdAt.Equal(cursorTime) && internalID < c.internalID
}

// requestTraceExportSource reads the same rows the session-only admin Trace detail
// reads, using parameterized SQL only. It owns no write path and no payload cache:
// every stage payload is bounded before it is handed to the export service, which
// is the single writer of the exported file.
type requestTraceExportSource struct {
	q sqlQueryer
}

func NewRequestTraceExportSource(db *sql.DB) service.RequestTraceExportSource {
	if db == nil {
		return &requestTraceExportSource{}
	}
	return &requestTraceExportSource{q: db}
}

var _ service.RequestTraceExportSource = (*requestTraceExportSource)(nil)

// NextTracePage 返回按 Trace 列表次序（created_at DESC, id DESC）排列的一页外部
// trace_id，以及续读下一页的游标。after 是上一页返回的游标，"" 表示从头开始；
// 它是本文件私有的不透明串，只钉 (created_at, 内部 id) 两个次序键，调用方原样
// 回传即可。因为游标不依赖"那一行仍然存在"，边遍历边被清理的记录不会让后续页
// 丢失或重复（枚举后消失的行由调用方按跳过计入）。
//
// 返回的 next 在空页时为 ""，表示遍历结束。页内次序必须严格递减：重复、乱序或
// 形状不对的行一律拒绝，而不是导出。
func (s *requestTraceExportSource) NextTracePage(ctx context.Context, filter service.RequestTraceExportFilter, after string, limit int) ([]string, string, error) {
	if s == nil || s.q == nil {
		return nil, "", errRequestTraceExportSourceUnavailable
	}
	if !requestTraceExportFilterValid(filter, limit) {
		return nil, "", service.ErrRequestTraceInvalidRecord
	}
	// 时间窗为空（from >= to）不是非法输入：条件本身自洽，只是没有任何记录能同时
	// 满足。返回空页，让"查询全部"以 0 行、清单"完整"收尾，与列表的 200 空集同一
	// 语义——服务层为"查询全部"盖的内部上界（任务创建时刻）会与一个落在未来的下界
	// 形成空区间，那是一个合法查询的结果，不是错误。调用方同时给出的倒置窗口仍由
	// HTTP/服务层在创建任务前拒绝，不会走到这里。
	if filter.CreatedFrom != nil && filter.CreatedTo != nil && !filter.CreatedFrom.Before(*filter.CreatedTo) {
		return nil, "", nil
	}
	var position requestTraceExportCursor
	hasPosition := false
	if after != "" {
		parsed, ok := decodeRequestTraceExportCursor(after)
		if !ok {
			return nil, "", service.ErrRequestTraceInvalidRecord
		}
		position, hasPosition = parsed, true
	}
	// 次序键与列表逐字一致：created_at DESC, id DESC。游标用行构造比较绑定参数，
	// 绝不把游标拼进语句。
	const query = `
		SELECT trace_id, created_at, id FROM request_traces
		WHERE ` + requestTraceExportFilterWhere + `
			AND ($21::timestamptz IS NULL OR (created_at, id) < ($21::timestamptz, $22::bigint))
		ORDER BY created_at DESC, id DESC
		LIMIT $23`
	var cursorCreatedAt, cursorInternalID any
	if hasPosition {
		cursorCreatedAt = time.UnixMicro(position.createdAtMicros).UTC()
		cursorInternalID = position.internalID
	}
	rows, err := s.q.QueryContext(ctx, query,
		append(requestTraceExportFilterArgs(filter), cursorCreatedAt, cursorInternalID, limit)...)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = rows.Close() }()
	ids := make([]string, 0, limit)
	next := ""
	var previous requestTraceExportCursor
	first := true
	for rows.Next() {
		var id string
		var createdAt time.Time
		var internalID int64
		if err := rows.Scan(&id, &createdAt, &internalID); err != nil {
			return nil, "", err
		}
		if !requestTraceExportSourceIDShape.MatchString(id) || internalID <= 0 {
			return nil, "", service.ErrRequestTraceInvalidRecord
		}
		// 页首必须严格早于游标、之后每一行必须严格早于前一行，否则游标会跳过或
		// 重放记录；这样的页一律拒绝而不是导出。
		if first && hasPosition && !position.strictlyBefore(createdAt, internalID) {
			return nil, "", service.ErrRequestTraceInvalidRecord
		}
		if !first && !previous.strictlyBefore(createdAt, internalID) {
			return nil, "", service.ErrRequestTraceInvalidRecord
		}
		first = false
		previous = requestTraceExportCursor{createdAtMicros: createdAt.UTC().UnixMicro(), internalID: internalID}
		ids = append(ids, id)
		next = encodeRequestTraceExportCursor(createdAt, internalID)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if len(ids) == 0 {
		next = ""
	}
	return ids, next, nil
}

func requestTraceExportFilterValid(filter service.RequestTraceExportFilter, limit int) bool {
	if limit <= 0 || limit > requestTraceExportSourceMaxLimit {
		return false
	}
	if filter.TraceID != "" && !requestTraceExportSourceIDShape.MatchString(filter.TraceID) {
		return false
	}
	switch filter.RouteFamily {
	case "", string(service.RequestTraceMessages), string(service.RequestTraceChatCompletions), string(service.RequestTraceResponses):
	default:
		return false
	}
	if filter.ClientStatus != nil && (*filter.ClientStatus < 0 || *filter.ClientStatus > 599) {
		return false
	}
	// 空时间窗（from >= to）由 NextTracePage 当成空结果，不在这里拒绝：它是服务层
	// 内部上界与调用方下界共同造成的合法空区间。
	return true
}

// ReadApprovedDetail loads a Trace that still exists and maps it into the typed
// allowlist the session-only admin detail API already discloses. Arbitrary stage
// metadata and raw payload bytes stay in the database: only the same bounded,
// redacted URL/header facts and UTF-8 payload text disclosed by the admin detail
// can be exported. A row deleted between paging and reading is reported as unavailable so the
// export counts it as skipped instead of resurrecting it.
func (s *requestTraceExportSource) ReadApprovedDetail(ctx context.Context, id string) (service.RequestTraceExportApprovedDetail, bool, error) {
	if s == nil || s.q == nil {
		return service.RequestTraceExportApprovedDetail{}, false, errRequestTraceExportSourceUnavailable
	}
	if !requestTraceExportSourceIDShape.MatchString(id) {
		return service.RequestTraceExportApprovedDetail{}, false, service.ErrRequestTraceInvalidRecord
	}
	// 信封字段与 Trace 详情同款同义：导出不能因为先前遗漏就少披露创建/完成/清理
	// 时间、请求时分组与客户端模型。列投影仍只读信封列，正文与任意 metadata 不出现。
	const envelopeQuery = `
		SELECT id, route_family, inbound_endpoint, capture_state, client_status, usage_log_id,
			created_at, completed_at, cleanup_after, group_id, requested_model, observed_platforms,
			user_id, api_key_id
		FROM request_traces WHERE trace_id = $1`
	var internalID int64
	var family, endpoint, state string
	var status int
	var usageLogID sql.NullInt64
	var createdAt, cleanupAfter time.Time
	var completedAt sql.NullTime
	var groupID sql.NullInt64
	var requestedModel sql.NullString
	var observedPlatforms []byte
	var userID, apiKeyID sql.NullInt64
	err := scanSingleRow(ctx, s.q, envelopeQuery, []any{id}, &internalID, &family, &endpoint, &state, &status, &usageLogID,
		&createdAt, &completedAt, &cleanupAfter, &groupID, &requestedModel, &observedPlatforms, &userID, &apiKeyID)
	if errors.Is(err, sql.ErrNoRows) {
		return service.RequestTraceExportApprovedDetail{}, false, nil
	}
	if err != nil {
		return service.RequestTraceExportApprovedDetail{}, false, err
	}
	detail := service.RequestTraceExportApprovedDetail{
		TraceID:         id,
		RouteFamily:     family,
		InboundEndpoint: endpoint,
		CaptureState:    state,
		ClientStatus:    status,
		CreatedAt:       createdAt,
		RequestedModel:  requestedModel.String,
		Stages:          make([]service.RequestTraceExportApprovedStage, 0, 8),
	}
	if usageLogID.Valid {
		linked := usageLogID.Int64
		detail.UsageLogID = &linked
	}
	if completedAt.Valid {
		completed := completedAt.Time
		detail.CompletedAt = &completed
	}
	// 与详情同一条规则：已关联使用记录的 Trace 随使用记录一起清理，不单独披露
	// cleanup_after；未关联的才带自己的清理时间。未观察到的事实保持零值。
	if detail.UsageLogID == nil {
		cleanup := cleanupAfter
		detail.CleanupAfter = &cleanup
	}
	if groupID.Valid {
		group := groupID.Int64
		detail.GroupID = &group
	}
	if userID.Valid {
		value := userID.Int64
		detail.UserID = &value
	}
	if apiKeyID.Valid {
		value := apiKeyID.Int64
		detail.APIKeyID = &value
	}
	// 平台历史与管理员详情同款同义：NULL 保持未知（缺席键），非 NULL 必须是去重、
	// 有界、token 合法的数组；损坏行按"不可用"处理，而不是尽力解释后导出。
	if len(observedPlatforms) > 0 {
		var platforms []string
		if err := json.Unmarshal(observedPlatforms, &platforms); err != nil || !service.ValidRequestTraceObservedPlatforms(platforms) {
			return service.RequestTraceExportApprovedDetail{}, false, service.ErrRequestTraceInvalidRecord
		}
		detail.ObservedPlatforms = platforms
	}
	if detail.UserID != nil || detail.APIKeyID != nil {
		labels := []service.RequestTrace{{UserID: detail.UserID, APIKeyID: detail.APIKeyID}}
		if err := hydrateRequestTraceLabels(ctx, s.q, labels); err != nil {
			return service.RequestTraceExportApprovedDetail{}, false, err
		}
		detail.UserEmail, detail.APIKeyName = labels[0].UserEmail, labels[0].APIKeyName
	}
	// The payload column is read as bytes and never exposed as bytes: it is bounded
	// one byte above the stage limit so an out-of-policy row fails closed instead of
	// being truncated into a misleading value, and it only becomes text when it is
	// valid UTF-8.
	const stagesQuery = `
		SELECT ordinal, stage, attempt_index, view_name, state, reason, observed_bytes,
			retained_bytes, dropped_events, redaction_unverified,
			substring(payload from 1 for $2::integer), substring(metadata::text from 1 for $4::integer)
		FROM request_trace_stages WHERE trace_id = $1 ORDER BY ordinal LIMIT $3`
	rows, err := s.q.QueryContext(ctx, stagesQuery, internalID,
		service.RequestTraceStagePayloadLimit+1, requestTraceExportSourceMaxStages+1, 4097)
	if err != nil {
		return service.RequestTraceExportApprovedDetail{}, false, err
	}
	defer func() { _ = rows.Close() }()
	var retainedPayload int64
	for rows.Next() {
		if len(detail.Stages) >= requestTraceExportSourceMaxStages {
			return service.RequestTraceExportApprovedDetail{}, false, service.ErrRequestTraceExportLimit
		}
		var stage service.RequestTraceExportApprovedStage
		var payload []byte
		var metadata string
		if err := rows.Scan(&stage.Ordinal, &stage.Stage, &stage.AttemptIndex, &stage.ViewName, &stage.State, &stage.Reason,
			&stage.ObservedBytes, &stage.RetainedBytes, &stage.DroppedEvents, &stage.RedactionUnverified, &payload, &metadata); err != nil {
			return service.RequestTraceExportApprovedDetail{}, false, err
		}
		if len(payload) > service.RequestTraceStagePayloadLimit || len(metadata) > requestTraceStageMetadataLimit {
			return service.RequestTraceExportApprovedDetail{}, false, service.ErrRequestTraceExportLimit
		}
		// The JSONB projection is decoded through the same typed rule used by
		// the repository and admin detail; no arbitrary metadata is exported.
		projection, err := decodeRequestTraceStageProjection(stage.Stage, []byte(metadata))
		if err != nil {
			return service.RequestTraceExportApprovedDetail{}, false, err
		}
		stage.Facts = projection.facts
		stage.Decision = projection.decision
		retainedPayload += int64(len(payload))
		// service.RequestTraceTotalBodyLimit is the collector's whole-trace body
		// budget, so a bigger payload set cannot be normal capture: it fails closed
		// rather than letting one trace exhaust the export's memory.
		if retainedPayload > service.RequestTraceTotalBodyLimit {
			return service.RequestTraceExportApprovedDetail{}, false, service.ErrRequestTraceExportLimit
		}
		if len(payload) > 0 && utf8.Valid(payload) {
			stage.PayloadText = string(payload)
		}
		detail.Stages = append(detail.Stages, stage)
	}
	if err := rows.Err(); err != nil {
		return service.RequestTraceExportApprovedDetail{}, false, err
	}
	return detail, true, nil
}
