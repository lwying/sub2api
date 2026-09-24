package dto

type KeyBillingSnapshotSettings struct {
	Enabled       bool `json:"enabled"`
	MaxStaleHours int  `json:"max_stale_hours"`
}
