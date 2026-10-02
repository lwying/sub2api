package repository

import (
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func validateRequestTraceFilter(filter service.RequestTraceListFilter) error {
	if (filter.TraceID != "" && !traceIDShape.MatchString(filter.TraceID)) ||
		(filter.RouteFamily != "" && !requestTraceFamilyValid(filter.RouteFamily)) ||
		(filter.ClientStatus != nil && (*filter.ClientStatus < 0 || *filter.ClientStatus > 599)) ||
		(filter.UsageLogID != nil && *filter.UsageLogID <= 0) ||
		(filter.AccountID != nil && *filter.AccountID <= 0) ||
		(filter.GroupID != nil && (*filter.GroupID <= 0 || filter.GroupUnknown != nil)) ||
		(filter.ModelUnknown != nil && filter.RequestedModel != "") ||
		(filter.PlatformUnknown != nil && filter.Platform != "") ||
		(filter.UserID != nil && (*filter.UserID <= 0 || filter.UserUnknown != nil)) ||
		(filter.APIKeyID != nil && (*filter.APIKeyID <= 0 || filter.APIKeyUnknown != nil)) ||
		(filter.Keyword != "" && !service.ValidRequestTraceKeyword(filter.Keyword)) {
		return service.ErrRequestTraceInvalidRecord
	}
	return nil
}

// All three readers (page, stats, export) answer the same metadata-only query.
// $1..$14 preserve the original filter order; $15..$19 are request-time owner
// and keyword criteria. Only export adds its selected-ID condition after these.
const requestTraceFilterWhere = `($1 = '' OR trace_id = $1)
    AND ($2 = '' OR route_family = $2)
    AND ($3::integer IS NULL OR client_status = $3)
    AND ($4::timestamptz IS NULL OR created_at >= $4)
    AND ($5::timestamptz IS NULL OR created_at < $5)
    AND ($6::boolean IS NULL OR (usage_log_id IS NOT NULL) = $6)
    AND ($7::bigint IS NULL OR usage_log_id = $7)
    AND ($8::bigint IS NULL OR EXISTS (
        SELECT 1 FROM request_trace_stages s WHERE s.trace_id = request_traces.id
          AND s.stage = 'wire_attempt' AND s.metadata @> jsonb_build_object('account_id', $8::bigint)))
    AND ($9::bigint IS NULL OR group_id = $9)
    AND ($10::boolean IS NULL OR (group_id IS NULL) = $10)
    AND ($11::text IS NULL OR lower(requested_model) = lower($11))
    AND ($12::boolean IS NULL OR (requested_model IS NULL) = $12)
    AND ($13::text IS NULL OR (
        EXISTS (SELECT 1 FROM request_trace_stages s2 WHERE s2.trace_id = request_traces.id
            AND s2.stage = 'wire_attempt' AND s2.metadata @> jsonb_build_object('platform', $13::text))
        OR observed_platforms @> jsonb_build_array($13::text)))
    AND ($14::boolean IS NULL OR ((NOT EXISTS (
        SELECT 1 FROM request_trace_stages s3 WHERE s3.trace_id = request_traces.id
          AND s3.stage = 'wire_attempt' AND s3.metadata ? 'platform')) AND observed_platforms IS NULL) = $14)
    AND ($15::bigint IS NULL OR user_id = $15)
    AND ($16::boolean IS NULL OR (user_id IS NULL) = $16)
    AND ($17::bigint IS NULL OR api_key_id = $17)
    AND ($18::boolean IS NULL OR (api_key_id IS NULL) = $18)
    AND ($19::text IS NULL OR (
        trace_id ILIKE $19 || '%' ESCAPE '\'
        OR inbound_endpoint ILIKE '%' || $19 || '%' ESCAPE '\'
        OR requested_model ILIKE '%' || $19 || '%' ESCAPE '\'
        OR EXISTS (SELECT 1 FROM users tu WHERE tu.id = request_traces.user_id AND tu.email ILIKE '%' || $19 || '%' ESCAPE '\')
        OR EXISTS (SELECT 1 FROM api_keys tk WHERE tk.id = request_traces.api_key_id AND tk.name ILIKE '%' || $19 || '%' ESCAPE '\')))`

// Escape LIKE wildcards while keeping typed '%' and '_' literal. SQL receives
// a bound pattern fragment; neither user input nor aliases are interpolated.
func requestTraceEscapedKeyword(keyword string) any {
	if keyword == "" {
		return nil
	}
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(keyword)
}

func requestTraceFilterArgs(traceID, route string, status *int, from, to *time.Time,
	linked *bool, usageID, accountID, groupID *int64, groupUnknown *bool, model string, modelUnknown *bool,
	platform string, platformUnknown *bool, userID *int64, userUnknown *bool, keyID *int64,
	keyUnknown *bool, keyword string) []any {
	var start, end any
	if from != nil {
		start = from.UTC()
	}
	if to != nil {
		end = to.UTC()
	}
	var linkedValue, groupMissing, modelMissing, platformMissing, userMissing, keyMissing any
	if linked != nil {
		linkedValue = *linked
	}
	if groupUnknown != nil {
		groupMissing = *groupUnknown
	}
	if modelUnknown != nil {
		modelMissing = *modelUnknown
	}
	if platformUnknown != nil {
		platformMissing = *platformUnknown
	}
	if userUnknown != nil {
		userMissing = *userUnknown
	}
	if keyUnknown != nil {
		keyMissing = *keyUnknown
	}
	return []any{traceID, route, status, start, end, linkedValue, usageID, accountID, groupID, groupMissing,
		nullIfEmptyTraceModel(model), modelMissing, nullIfEmptyTraceModel(platform), platformMissing,
		userID, userMissing, keyID, keyMissing, requestTraceEscapedKeyword(keyword)}
}

func requestTraceListFilterArgs(filter service.RequestTraceListFilter) []any {
	var from, to *time.Time
	if !filter.CreatedFrom.IsZero() {
		from = &filter.CreatedFrom
	}
	if !filter.CreatedTo.IsZero() {
		to = &filter.CreatedTo
	}
	return requestTraceFilterArgs(filter.TraceID, string(filter.RouteFamily), filter.ClientStatus, from, to,
		filter.UsageLinked, filter.UsageLogID, filter.AccountID, filter.GroupID, filter.GroupUnknown,
		filter.RequestedModel, filter.ModelUnknown, filter.Platform, filter.PlatformUnknown,
		filter.UserID, filter.UserUnknown, filter.APIKeyID, filter.APIKeyUnknown, filter.Keyword)
}

// requestTraceMetadataFilterArgs 把导出／删除筛选映射成 requestTraceFilterWhere 的
// $1..$19 绑定参数（不含导出所选 ID 的 $20）。列表、导出与手动清理共用同一份 clause
// 与同一份参数顺序，筛选语义不会各写一套而漂移。
func requestTraceMetadataFilterArgs(filter service.RequestTraceExportFilter) []any {
	return requestTraceFilterArgs(filter.TraceID, filter.RouteFamily, filter.ClientStatus,
		filter.CreatedFrom, filter.CreatedTo, filter.UsageLinked, filter.UsageLogID,
		filter.AccountID, filter.GroupID, filter.GroupUnknown, filter.RequestedModel,
		filter.ModelUnknown, filter.Platform, filter.PlatformUnknown,
		filter.UserID, filter.UserUnknown, filter.APIKeyID, filter.APIKeyUnknown, filter.Keyword)
}
