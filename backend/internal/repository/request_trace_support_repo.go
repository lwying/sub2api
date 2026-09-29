package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

var errRequestTraceSupportProbeUnavailable = errors.New("request trace support probe has no queryer")

type requestTraceSupportProbe struct {
	q sqlQueryer
}

// NewRequestTraceSupportProbe verifies that the trace table can cascade with usage.
func NewRequestTraceSupportProbe(db *sql.DB) service.RequestTraceSupportProbe {
	if db == nil {
		return &requestTraceSupportProbe{}
	}
	return &requestTraceSupportProbe{q: db}
}

var _ service.RequestTraceSupportProbe = (*requestTraceSupportProbe)(nil)

func (p *requestTraceSupportProbe) ProbeRequestTraceSupport(ctx context.Context) (service.PlaintextCaptureSupport, error) {
	if p == nil || p.q == nil {
		return service.PlaintextCaptureSupport{}, errRequestTraceSupportProbeUnavailable
	}
	const query = `
		WITH rels AS (
			SELECT to_regclass('usage_logs') AS usage_logs, to_regclass('request_traces') AS request_traces
		)
		SELECT
			rels.usage_logs IS NULL,
			rels.usage_logs IS NOT NULL AND EXISTS (
				SELECT 1 FROM pg_partitioned_table pt WHERE pt.partrelid = rels.usage_logs
			),
			rels.request_traces IS NOT NULL AND EXISTS (
				SELECT 1
				FROM pg_constraint con
				JOIN pg_attribute att ON att.attrelid = con.conrelid AND att.attnum = con.conkey[1]
				JOIN pg_attribute ref ON ref.attrelid = con.confrelid AND ref.attnum = con.confkey[1]
				WHERE con.conrelid = rels.request_traces AND con.contype = 'f'
				  AND con.confdeltype = 'c' AND con.confrelid = rels.usage_logs
				  AND array_length(con.conkey, 1) = 1
				  AND att.attname = 'usage_log_id' AND ref.attname = 'id'
			)
		FROM rels
	`
	var missing, partitioned, traceOwned bool
	if err := scanSingleRow(ctx, p.q, query, nil, &missing, &partitioned, &traceOwned); err != nil {
		return service.PlaintextCaptureSupport{}, err
	}
	switch {
	case missing:
		return service.PlaintextCaptureSupport{Reason: service.PlaintextCaptureSupportReasonUnknownDeployment}, nil
	case partitioned:
		return service.PlaintextCaptureSupport{Reason: service.PlaintextCaptureSupportReasonPartitionedUsageLogs}, nil
	case !traceOwned:
		return service.PlaintextCaptureSupport{Reason: service.PlaintextCaptureSupportReasonMissingOwnership}, nil
	default:
		return service.PlaintextCaptureSupport{Supported: true, Reason: service.PlaintextCaptureSupportReasonSupported}, nil
	}
}
