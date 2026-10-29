package model

import (
	"context"
	"errors"
	"slices"
	"time"
)

var ErrReliabilityWindowTooLarge = errors.New("request reliability window exceeds 100000 observations")

type ReliabilityMetrics struct {
	Requests     int      `json:"requests"`
	Succeeded    int      `json:"succeeded"`
	Rejected     int      `json:"rejected"`
	Failed       int      `json:"failed"`
	Cancelled    int      `json:"cancelled"`
	Unknown      int      `json:"unknown"`
	SuccessRate  *float64 `json:"success_rate"`
	LatencyP50MS *float64 `json:"latency_p50_ms"`
	LatencyP95MS *float64 `json:"latency_p95_ms"`
	FirstP50MS   *float64 `json:"first_p50_ms"`
	FirstP95MS   *float64 `json:"first_p95_ms"`
	FirstSamples int      `json:"first_samples"`
	latencies    []int64
	firstEvents  []int64
}

func (m *ReliabilityMetrics) add(row RelayOutcome) {
	m.Requests++
	switch row.Outcome {
	case "success":
		m.Succeeded++
		m.latencies = append(m.latencies, row.LatencyMS)
		if row.Stream && row.FirstResponseMS >= 0 {
			m.firstEvents = append(m.firstEvents, row.FirstResponseMS)
		}
	case "rejected":
		m.Rejected++
	case "failure", "incomplete":
		m.Failed++
	case "cancelled":
		m.Cancelled++
	default:
		m.Unknown++
	}
}

func (m *ReliabilityMetrics) finish() {
	if m.Requests > 0 {
		rate := float64(m.Succeeded) * 100 / float64(m.Requests)
		m.SuccessRate = &rate
	}
	m.FirstSamples = len(m.firstEvents)
	m.LatencyP50MS, m.LatencyP95MS = reliabilityPercentiles(m.latencies)
	m.FirstP50MS, m.FirstP95MS = reliabilityPercentiles(m.firstEvents)
	m.latencies, m.firstEvents = nil, nil
}

func reliabilityPercentiles(values []int64) (*float64, *float64) {
	if len(values) == 0 {
		return nil, nil
	}
	slices.Sort(values)
	middle := len(values) / 2
	p50 := float64(values[middle])
	if len(values)%2 == 0 {
		p50 = float64(values[middle-1])/2 + float64(values[middle])/2
	}
	p95 := float64(values[(95*len(values)+99)/100-1])
	return &p50, &p95
}

type ReliabilityChannel struct {
	ChannelID int    `json:"channel_id"`
	Name      string `json:"name"`
	ReliabilityMetrics
}

type ReliabilityDay struct {
	Day string `json:"day"`
	ReliabilityMetrics
}

type ReliabilityFailure struct {
	CreatedAt   int64  `json:"created_at"`
	ChannelID   int    `json:"channel_id"`
	ModelName   string `json:"model_name"`
	Reason      string `json:"reason"`
	HTTPStatus  int    `json:"http_status"`
	ErrorStatus int    `json:"error_status"`
	Attempts    int    `json:"attempts"`
	LatencyMS   int64  `json:"latency_ms"`
}

type RelayReliability struct {
	WindowStart    int64                `json:"window_start"`
	WindowEnd      int64                `json:"window_end"`
	OffsetMinutes  int                  `json:"offset_minutes"`
	Summary        ReliabilityMetrics   `json:"summary"`
	Channels       []ReliabilityChannel `json:"channels"`
	Daily          []ReliabilityDay     `json:"daily"`
	RecentFailures []ReliabilityFailure `json:"recent_failures"`
	LocalRequests  int                  `json:"local_requests"`
	RetryRequests  int                  `json:"retry_requests"`
	WriterDropped  uint64               `json:"writer_dropped"`
	WriterFailed   uint64               `json:"writer_failed"`
	WriterQueued   int                  `json:"writer_queued"`
}

// Channel statistics include only one-attempt requests: retries cannot be
// attributed to one provider using a terminal request observation alone.
func QueryRelayReliability(ctx context.Context, now time.Time, days, offsetMinutes int) (RelayReliability, error) {
	zone := time.FixedZone("report", offsetMinutes*60)
	local := now.In(zone)
	start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, zone).AddDate(0, 0, 1-days)
	result := RelayReliability{
		WindowStart: start.Unix(), WindowEnd: now.Unix(), OffsetMinutes: offsetMinutes,
		Channels: []ReliabilityChannel{}, Daily: []ReliabilityDay{}, RecentFailures: []ReliabilityFailure{},
		WriterDropped: relayOutcomes.dropped.Load(), WriterFailed: relayOutcomes.failed.Load(),
		WriterQueued: len(relayOutcomes.queue),
	}
	var rows []RelayOutcome
	err := DB.WithContext(ctx).Select("created_at", "model_name", "channel_id", "outcome", "reason",
		"http_status", "error_status", "attempts", "latency_ms", "stream", "first_response_ms").
		Where("created_at >= ? AND created_at <= ? AND authenticated = ? AND origin = ?",
			start.Unix(), now.Unix(), true, "api").
		Order("created_at DESC").Order("request_id").Limit(100001).Find(&rows).Error
	if err != nil {
		return result, err
	}
	if len(rows) > 100000 {
		return result, ErrReliabilityWindowTooLarge
	}
	byDay := make(map[string]*ReliabilityMetrics, days)
	for i := range days {
		day := start.AddDate(0, 0, i).Format(time.DateOnly)
		byDay[day] = &ReliabilityMetrics{}
	}
	byChannel := make(map[int]*ReliabilityChannel)
	for _, row := range rows {
		result.Summary.add(row)
		byDay[time.Unix(row.CreatedAt, 0).In(zone).Format(time.DateOnly)].add(row)
		switch {
		case row.Attempts == 0:
			result.LocalRequests++
		case row.Attempts > 1:
			result.RetryRequests++
		case row.Attempts == 1 && row.ChannelID > 0:
			channel := byChannel[row.ChannelID]
			if channel == nil {
				channel = &ReliabilityChannel{ChannelID: row.ChannelID}
				byChannel[row.ChannelID] = channel
			}
			channel.add(row)
		}
		if row.Outcome != "success" && len(result.RecentFailures) < 50 {
			result.RecentFailures = append(result.RecentFailures, ReliabilityFailure{
				CreatedAt: row.CreatedAt, ChannelID: row.ChannelID, ModelName: row.ModelName,
				Reason: row.Reason, HTTPStatus: row.HTTPStatus, ErrorStatus: row.ErrorStatus,
				Attempts: row.Attempts, LatencyMS: row.LatencyMS,
			})
		}
	}
	var channels []struct {
		ID   int
		Name string
	}
	if len(byChannel) > 0 {
		ids := make([]int, 0, len(byChannel))
		for id := range byChannel {
			ids = append(ids, id)
		}
		if err := DB.WithContext(ctx).Model(&Channel{}).Select("id", "name").Where("id IN ?", ids).Find(&channels).Error; err != nil {
			return result, err
		}
	}
	for _, channel := range channels {
		byChannel[channel.ID].Name = channel.Name
	}
	for _, channel := range byChannel {
		channel.finish()
		result.Channels = append(result.Channels, *channel)
	}
	slices.SortFunc(result.Channels, func(a, b ReliabilityChannel) int { return a.ChannelID - b.ChannelID })
	for i := range days {
		day := start.AddDate(0, 0, i).Format(time.DateOnly)
		metrics := byDay[day]
		metrics.finish()
		result.Daily = append(result.Daily, ReliabilityDay{Day: day, ReliabilityMetrics: *metrics})
	}
	result.Summary.finish()
	return result, nil
}
