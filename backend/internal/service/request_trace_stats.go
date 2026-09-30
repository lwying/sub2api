package service

// RequestTraceQueryStats counts only persisted Traces matching one executed query.
// Capture runtime counters live on a separate process-scoped status endpoint.
type RequestTraceQueryStats struct {
	MatchedTotal int64                          `json:"matched_total"`
	Status       RequestTraceQueryStatusCounts  `json:"status"`
	Capture      RequestTraceQueryCaptureCounts `json:"capture"`
	Usage        RequestTraceQueryUsageCounts   `json:"usage"`
}

type RequestTraceQueryStatusCounts struct {
	OK       int64 `json:"2xx"`
	Redirect int64 `json:"3xx"`
	Client   int64 `json:"4xx"`
	Server   int64 `json:"5xx"`
	Other    int64 `json:"other"`
}

type RequestTraceQueryCaptureCounts struct {
	NotObserved int64 `json:"not_observed"`
	Stored      int64 `json:"stored"`
	Partial     int64 `json:"partial"`
	WriteFailed int64 `json:"write_failed"`
}

type RequestTraceQueryUsageCounts struct {
	Linked   int64 `json:"linked"`
	Unlinked int64 `json:"unlinked"`
}
