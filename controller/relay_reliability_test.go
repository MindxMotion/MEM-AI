package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequestReliabilityDatabaseMatrix(t *testing.T) {
	db := setupManageUserTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.RelayOutcome{}, &model.Channel{}))
	now := time.Date(2026, 10, 29, 4, 0, 0, 0, time.UTC)
	today, yesterday := now.Add(-time.Hour).Unix(), now.Add(-12*time.Hour-time.Minute).Unix()
	for _, channel := range []model.Channel{{Id: 13, Name: "BestMax"}, {Id: 14, Name: "ACE"}} {
		require.NoError(t, db.Create(&channel).Error)
	}
	rows := []model.RelayOutcome{
		{RequestID: "previous-day", CreatedAt: yesterday, ChannelID: 13, Outcome: "success", Attempts: 1, LatencyMS: 1000, Stream: true, FirstResponseMS: 100},
		{RequestID: "completed", CreatedAt: today, ChannelID: 13, Outcome: "success", Attempts: 1, LatencyMS: 3000, Stream: true, FirstResponseMS: 300},
		{RequestID: "timeout-http-200", CreatedAt: today, ChannelID: 13, Outcome: "failure", Reason: "timeout", HTTPStatus: 200, Attempts: 1, LatencyMS: 300000},
		{RequestID: "validation", CreatedAt: today, ChannelID: 13, Outcome: "rejected", Reason: "request_rejected", HTTPStatus: 400, ErrorStatus: 400, Attempts: 1},
		{RequestID: "disconnected", CreatedAt: today, ChannelID: 13, Outcome: "cancelled", Reason: "client_disconnected", HTTPStatus: 200, Attempts: 1},
		{RequestID: "local", CreatedAt: today, ChannelID: 13, Outcome: "rejected", Reason: "request_rejected", HTTPStatus: 403},
		{RequestID: "retry", CreatedAt: today, ChannelID: 13, Outcome: "success", Attempts: 2, LatencyMS: 7000},
		{RequestID: "ace", CreatedAt: today, ChannelID: 14, Outcome: "success", Attempts: 1, LatencyMS: 5000, Stream: true, FirstResponseMS: 200},
		{RequestID: "unknown", CreatedAt: today, ChannelID: 13, Outcome: "unknown", Attempts: 1},
		{RequestID: "incomplete", CreatedAt: today, ChannelID: 13, Outcome: "incomplete", Reason: "upstream_incomplete", HTTPStatus: 200, Attempts: 1},
		{RequestID: "probe", CreatedAt: today, ChannelID: 13, Outcome: "failure", Origin: "channel_test", Attempts: 1},
		{RequestID: "unauthenticated", CreatedAt: today, ChannelID: 13, Outcome: "failure", Attempts: 1},
		{RequestID: "too-old", CreatedAt: now.Add(-48 * time.Hour).Unix(), ChannelID: 13, Outcome: "failure", Attempts: 1},
	}
	for i := range rows {
		if rows[i].Origin == "" {
			rows[i].Origin = "api"
		}
		rows[i].Authenticated = rows[i].RequestID != "unauthenticated"
		rows[i].ModelName = "fixture-model"
	}
	require.NoError(t, db.Create(&rows).Error)
	result, err := model.QueryRelayReliability(context.Background(), now, 2, 480)
	require.NoError(t, err)
	assert.Equal(t, 10, result.Summary.Requests)
	assert.Equal(t, 4, result.Summary.Succeeded)
	assert.Equal(t, 2, result.Summary.Rejected)
	assert.Equal(t, 2, result.Summary.Failed)
	assert.Equal(t, 1, result.Summary.Cancelled)
	assert.Equal(t, 1, result.Summary.Unknown)
	require.NotNil(t, result.Summary.SuccessRate)
	assert.Equal(t, 40.0, *result.Summary.SuccessRate)
	require.NotNil(t, result.Summary.LatencyP50MS)
	assert.Equal(t, 4000.0, *result.Summary.LatencyP50MS)
	assert.Equal(t, 7000.0, *result.Summary.LatencyP95MS)
	assert.Equal(t, 200.0, *result.Summary.FirstP50MS)
	assert.Equal(t, 3, result.Summary.FirstSamples)
	assert.Equal(t, 1, result.LocalRequests)
	assert.Equal(t, 1, result.RetryRequests)
	require.Len(t, result.Channels, 2)
	assert.Equal(t, "BestMax", result.Channels[0].Name)
	assert.Equal(t, 7, result.Channels[0].Requests)
	assert.Equal(t, 2, result.Channels[0].Succeeded)
	assert.Equal(t, 2000.0, *result.Channels[0].LatencyP50MS)
	assert.Equal(t, 3000.0, *result.Channels[0].LatencyP95MS)
	assert.Equal(t, 100.0, *result.Channels[1].SuccessRate)
	require.Len(t, result.Daily, 2)
	assert.Equal(t, "2026-10-28", result.Daily[0].Day)
	assert.Equal(t, 1, result.Daily[0].Requests)
	assert.Equal(t, 9, result.Daily[1].Requests)
	require.Len(t, result.RecentFailures, 6)
	payload, err := common.Marshal(result)
	require.NoError(t, err)
	assert.NotContains(t, string(payload), "RequestID")
	assert.NotContains(t, string(payload), "request_id")
	assert.NotContains(t, string(payload), "unauthenticated")

	empty, err := model.QueryRelayReliability(context.Background(), now.Add(72*time.Hour), 1, -300)
	require.NoError(t, err)
	assert.Zero(t, empty.Summary.Requests)
	assert.Nil(t, empty.Summary.SuccessRate)
	assert.Nil(t, empty.Summary.LatencyP50MS)
	assert.Empty(t, empty.Channels)
	require.Len(t, empty.Daily, 1)
	assert.Zero(t, empty.Daily[0].Requests)
	assert.Nil(t, empty.Daily[0].SuccessRate)
	t.Logf("request reliability database matrix passed: %s", os.Getenv("TEST_MANAGE_USER_DIALECT"))
}

func TestRequestReliabilityRootAccessAndValidation(t *testing.T) {
	db := setupManageUserTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.RelayOutcome{}, &model.Channel{}))
	router := gin.New()
	router.Use(middleware.RequestId(), middleware.DisableCache())
	router.GET("/api/performance/reliability", middleware.RootAuth(), GetRelayReliability)
	anonymous := httptest.NewRecorder()
	router.ServeHTTP(anonymous, httptest.NewRequest(http.MethodGet, "/api/performance/reliability", nil))
	assert.Equal(t, http.StatusUnauthorized, anonymous.Code)
	for _, role := range []int{common.RoleCommonUser, common.RoleAdminUser, common.RoleRootUser} {
		user := model.User{Username: fmt.Sprintf("reliability-%d", role), Password: "fixture-password-hash",
			Role: role, Status: common.UserStatusEnabled, AuthVersion: 1, AffCode: fmt.Sprintf("reliability-%d", role)}
		require.NoError(t, db.Create(&user).Error)
		require.NoError(t, model.PublishUserAuthCache(user.Id))
		_, credential := createAccessTokenTestSession(t, user.Id, "reliability-session-"+user.Username)
		response := accessTokenRequest(router, http.MethodGet, "/api/performance/reliability", credential, "", "")
		if role != common.RoleRootUser {
			assert.Equal(t, http.StatusForbidden, response.Code)
			continue
		}
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		for _, query := range []string{"days=0", "days=31", "days=invalid", "offset_minutes=841", "offset_minutes=-841", "offset_minutes=invalid"} {
			response = accessTokenRequest(router, http.MethodGet, "/api/performance/reliability?"+query, credential, "", "")
			assert.Equal(t, http.StatusBadRequest, response.Code, query)
		}
	}
}
