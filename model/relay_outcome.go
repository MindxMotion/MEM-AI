package model

import (
	"context"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

// RelayOutcome is a non-billing, terminal HTTP request observation. It contains
// no prompt text, credentials, user identifiers, IPs or upstream error messages.
type RelayOutcome struct {
	RequestID       string `gorm:"primaryKey;type:varchar(64)"`
	CreatedAt       int64  `gorm:"index:idx_relay_outcome_created_at"`
	ModelName       string `gorm:"type:varchar(128)"`
	ChannelID       int
	Endpoint        string `gorm:"type:varchar(64)"`
	ClientFamily    string `gorm:"type:varchar(32)"`
	Origin          string `gorm:"type:varchar(16)"`
	Authenticated   bool
	Outcome         string `gorm:"type:varchar(16)"`
	Reason          string `gorm:"type:varchar(64)"`
	HTTPStatus      int
	ErrorStatus     int
	ResponseStarted bool
	Stream          bool
	StreamEnd       string `gorm:"type:varchar(32)"`
	Attempts        int
	LatencyMS       int64
	FirstResponseMS int64
	RequestSize     string `gorm:"type:varchar(16)"`
	FeaturesKnown   bool
	HasTools        bool
	HasImages       bool
	Structured      bool
	Reasoning       bool
}

var relayOutcomes = newDiagnosticWriter[RelayOutcome](1024, "async relay outcomes")

func RecordRelayOutcome(outcome RelayOutcome) bool {
	return relayOutcomes.enqueue(outcome)
}

func StartRelayOutcomeWriter() {
	select {
	case <-relayOutcomes.done:
		relayOutcomes = newDiagnosticWriter[RelayOutcome](1024, "async relay outcomes")
	default:
	}
	relayOutcomes.maintain = func(database *gorm.DB) error {
		return DeleteRelayOutcomesBefore(database, time.Now().Add(-30*24*time.Hour).Unix())
	}
	relayOutcomes.start(DB)
	common.SysLog(common.LogText("async relay outcomes enabled: capacity=1024 workers=1 write_timeout=3s retention_days=30"))
}

func StopRelayOutcomeWriter(ctx context.Context) error {
	return relayOutcomes.stop(ctx)
}

// Each hourly cleanup removes at most 500 old observations, never a table scan
// or an unbounded delete in the request path.
func DeleteRelayOutcomesBefore(database *gorm.DB, cutoff int64) error {
	var ids []string
	if err := database.Model(&RelayOutcome{}).Where("created_at < ?", cutoff).
		Order("created_at").Limit(500).Pluck("request_id", &ids).Error; err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	return database.Where("request_id IN ? AND created_at < ?", ids, cutoff).Delete(&RelayOutcome{}).Error
}
