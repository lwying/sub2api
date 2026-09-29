//go:build integration

package repository

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestRequestTraceSupportAfterMigration(t *testing.T) {
	ctx := context.Background()
	probe := NewRequestTraceSupportProbe(integrationDB)
	support, err := probe.ProbeRequestTraceSupport(ctx)
	require.NoError(t, err)
	require.True(t, support.Supported, "the migrated Trace table must have its cascading usage ownership")
	require.Equal(t, service.PlaintextCaptureSupportReasonSupported, support.Reason)
}

func TestRequestTraceSupportRejectsPartitionedAndUnownedShapes(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, ddl, want string
	}{
		{
			name: "missing trace table",
			ddl:  plaintextCaptureSupportUsageLogsDDL,
			want: service.PlaintextCaptureSupportReasonMissingOwnership,
		},
		{
			name: "trace lacks cascade",
			ddl: plaintextCaptureSupportUsageLogsDDL + `
				CREATE TABLE request_traces (id BIGINT PRIMARY KEY, usage_log_id BIGINT REFERENCES usage_logs(id));`,
			want: service.PlaintextCaptureSupportReasonMissingOwnership,
		},
		{
			name: "trace has cascade",
			ddl: plaintextCaptureSupportUsageLogsDDL + `
				CREATE TABLE request_traces (id BIGINT PRIMARY KEY, usage_log_id BIGINT REFERENCES usage_logs(id) ON DELETE CASCADE);`,
			want: service.PlaintextCaptureSupportReasonSupported,
		},
		{
			name: "partitioned usage",
			ddl:  plaintextCaptureSupportPartitionedUsageLogsDDL,
			want: service.PlaintextCaptureSupportReasonPartitionedUsageLogs,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx := plaintextCaptureSupportDDLTx(t, ctx, tc.ddl)
			probe := &requestTraceSupportProbe{q: tx}
			support, err := probe.ProbeRequestTraceSupport(ctx)
			require.NoError(t, err)
			require.Equal(t, tc.want, support.Reason)
			require.Equal(t, tc.want == service.PlaintextCaptureSupportReasonSupported, support.Supported)
		})
	}
}
