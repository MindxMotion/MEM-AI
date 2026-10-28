package model

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestErrorLogQueueRejectsOverflowAndShutdownWithoutBlocking(t *testing.T) {
	writer := newErrorLogWriter(1)
	require.True(t, writer.enqueue(Log{RequestId: "first"}))
	assert.False(t, writer.enqueue(Log{RequestId: "overflow"}))
	assert.EqualValues(t, 1, writer.accepted.Load())
	assert.EqualValues(t, 1, writer.dropped.Load())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, writer.stop(ctx), context.Canceled)
	assert.False(t, writer.enqueue(Log{RequestId: "after-stop"}))
	assert.EqualValues(t, 2, writer.dropped.Load())
	assert.Equal(t, "first", (<-writer.queue).RequestId)
	_, open := <-writer.queue
	assert.False(t, open)
}

func TestRecordErrorLogSnapshotsContextAndBoundsMetadata(t *testing.T) {
	previous := relayErrorLogs
	relayErrorLogs = newErrorLogWriter(4)
	t.Cleanup(func() { relayErrorLogs = previous })
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Request.RemoteAddr = "192.0.2.20:9000"
	c.Set("username", "original-user")
	c.Set(common.RequestIdKey, "error-request")
	c.Set(common.UpstreamRequestIdKey, "upstream-request")
	common.SetContextKey(c, constant.ContextKeyUserSetting, dto.UserSetting{RecordIpLog: true})
	other := NewLogOther()
	other.SetPublic("status_code", 503)
	other.SetPublic("error_code", "shell_api_error")
	other.SetPublic("request_path", c.Request.URL.Path)
	other.SetAdmin("request_policy", []map[string]any{{"decision": "retry"}})
	RecordErrorLog(c, 7, 13, "test-model", "token-label", "masked failure", 9, 2, true, "default", other)
	c.Set("username", "reused-user")
	c.Set(common.RequestIdKey, "reused-request")
	other.SetPublic("status_code", 400)
	log := <-relayErrorLogs.queue
	assert.Equal(t, "original-user", log.Username)
	assert.Equal(t, "error-request", log.RequestId)
	assert.Equal(t, "upstream-request", log.UpstreamRequestId)
	assert.Equal(t, "192.0.2.20", log.Ip)
	assert.Equal(t, LogTypeError, log.Type)
	assert.Equal(t, 13, log.ChannelId)
	assert.Contains(t, log.Other, `"status_code":503`)
	assert.Contains(t, log.Other, `"request_policy"`)
	assert.NotContains(t, log.Other, "reused")

	other.SetAdmin("oversized", strings.Repeat("x", 40000))
	other.SetPublic("request_path", "/v1/"+strings.Repeat("x", 40000))
	common.SetContextKey(c, constant.ContextKeyUserSetting, dto.UserSetting{RecordIpLog: false})
	RecordErrorLog(c, 7, 13, "test-model", "label", strings.Repeat("错", 1500), 9, 2, true, "default", other)
	log = <-relayErrorLogs.queue
	assert.Empty(t, log.Ip)
	assert.True(t, utf8.ValidString(log.Content))
	assert.LessOrEqual(t, len(log.Content), 2051)
	assert.LessOrEqual(t, len(log.Other), 32*1024)
	assert.Contains(t, log.Other, `"diagnostics_truncated":true`)
	assert.Contains(t, log.Other, `"status_code":400`)
	assert.NotContains(t, log.Other, `"oversized"`)
}

func TestErrorLogAsyncDatabaseMatrix(t *testing.T) {
	dialect := os.Getenv("TEST_ERROR_LOG_DIALECT")
	if dialect == "" {
		dialect = "sqlite"
	}
	var driver gorm.Dialector
	switch dialect {
	case "sqlite":
		driver = sqlite.Open(":memory:")
	case "mysql":
		require.NotEmpty(t, os.Getenv("TEST_MYSQL_DSN"))
		driver = mysql.Open(os.Getenv("TEST_MYSQL_DSN"))
	case "postgres":
		require.NotEmpty(t, os.Getenv("TEST_POSTGRES_DSN"))
		driver = postgres.Open(os.Getenv("TEST_POSTGRES_DSN"))
	default:
		t.Fatalf("unsupported test dialect %q", dialect)
	}
	db, err := gorm.Open(driver, &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	const table = "async_relay_error_log_fixture"
	db = db.Table(table).Session(&gorm.Session{})
	require.NoError(t, db.AutoMigrate(&Log{}))
	t.Cleanup(func() { require.NoError(t, db.Migrator().DropTable(table)) })
	prior := Log{Type: LogTypeConsume, RequestId: "existing-record", Content: "preserved"}
	require.NoError(t, db.Create(&prior).Error)
	require.NoError(t, db.AutoMigrate(&Log{}))

	writer := newErrorLogWriter(4)
	entered, release := make(chan struct{}), make(chan struct{})
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register("test:block_error_insert", func(tx *gorm.DB) {
		if entry, ok := tx.Statement.Dest.(*Log); ok && entry.RequestId == "queued-request" {
			close(entered)
			<-release
		}
	}))
	defer db.Callback().Create().Remove("test:block_error_insert")
	writer.start(db)
	require.True(t, writer.enqueue(Log{Type: LogTypeError, UserId: 7, ChannelId: 13, RequestId: "queued-request", Other: `{"status_code":503,"admin_info":{"use_channel":["13"],"request_policy":[{"decision":{"action":"retry"}}]}}`}))
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("database writer did not start")
	}
	require.True(t, writer.enqueue(Log{Type: LogTypeError, RequestId: "accepted-while-database-blocked"}))
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, writer.stop(ctx))
	assert.EqualValues(t, 2, writer.written.Load())
	assert.EqualValues(t, 0, writer.failed.Load())
	var records []Log
	require.NoError(t, db.Order("id").Find(&records).Error)
	require.Len(t, records, 3)
	assert.Equal(t, prior.Id, records[0].Id)
	assert.Equal(t, "preserved", records[0].Content)
	assert.Equal(t, "queued-request", records[1].RequestId)
	assert.Equal(t, 13, records[1].ChannelId)
	assert.Contains(t, records[1].Other, `"action":"retry"`)
	assert.Equal(t, "accepted-while-database-blocked", records[2].RequestId)
	assert.True(t, db.Migrator().HasIndex(table, "idx_logs_request_id"))

	failing := newErrorLogWriter(1)
	failing.start(db.Table("missing_async_log_fixture"))
	require.True(t, failing.enqueue(Log{RequestId: "failed-write"}))
	require.NoError(t, failing.stop(ctx))
	assert.EqualValues(t, 1, failing.failed.Load())
	assert.EqualValues(t, 0, failing.written.Load())

	outcomeDB := db.Session(&gorm.Session{NewDB: true}).Table("relay_outcome_fixture").Session(&gorm.Session{})
	require.NoError(t, outcomeDB.AutoMigrate(&RelayOutcome{}))
	t.Cleanup(func() { require.NoError(t, outcomeDB.Migrator().DropTable("relay_outcome_fixture")) })
	oldOutcome := RelayOutcome{
		RequestID: "older-outcome", CreatedAt: time.Now().Add(-31 * 24 * time.Hour).Unix(),
		Outcome: "success", Authenticated: false, FirstResponseMS: -1,
	}
	require.NoError(t, outcomeDB.Create(&oldOutcome).Error)
	require.NoError(t, outcomeDB.AutoMigrate(&RelayOutcome{}))
	require.NoError(t, outcomeDB.AutoMigrate(&RelayOutcome{}))
	outcomes := newDiagnosticWriter[RelayOutcome](4, "async relay outcomes")
	outcomes.start(outcomeDB)
	require.True(t, outcomes.enqueue(RelayOutcome{
		RequestID: "terminal-request", CreatedAt: time.Now().Unix(), ModelName: "test-model",
		Endpoint: "/v1/chat/completions", ClientFamily: "workbuddy", Origin: "api",
		Outcome: "failure", Reason: "upstream_stream_failed", HTTPStatus: 200, ErrorStatus: 500,
		Stream: true, ResponseStarted: true, Attempts: 2, FeaturesKnown: true, HasTools: true,
		RequestSize: "4k_32k", FirstResponseMS: 123,
	}))
	require.NoError(t, outcomes.stop(ctx))
	assert.EqualValues(t, 1, outcomes.written.Load())
	var storedOutcome RelayOutcome
	require.NoError(t, outcomeDB.Where("request_id = ?", "terminal-request").First(&storedOutcome).Error)
	assert.Equal(t, "failure", storedOutcome.Outcome)
	assert.Equal(t, "workbuddy", storedOutcome.ClientFamily)
	assert.Equal(t, 2, storedOutcome.Attempts)
	assert.Equal(t, 200, storedOutcome.HTTPStatus)
	assert.Equal(t, 500, storedOutcome.ErrorStatus)
	assert.True(t, storedOutcome.Stream)
	assert.True(t, storedOutcome.HasTools)
	assert.False(t, storedOutcome.Authenticated)
	assert.EqualValues(t, 123, storedOutcome.FirstResponseMS)
	require.Error(t, outcomeDB.Create(&storedOutcome).Error, "a request must never become two terminal samples")
	require.NoError(t, DeleteRelayOutcomesBefore(outcomeDB, time.Now().Add(-30*24*time.Hour).Unix()))
	var outcomeCount int64
	require.NoError(t, outcomeDB.Model(&RelayOutcome{}).Count(&outcomeCount).Error)
	assert.EqualValues(t, 1, outcomeCount, "retention deletes old observations but preserves current requests")
	assert.True(t, outcomeDB.Migrator().HasIndex("relay_outcome_fixture", "idx_relay_outcome_created_at"))
	require.NoError(t, db.Where("request_id = ?", "existing-record").First(&prior).Error)
	assert.Equal(t, "preserved", prior.Content, "new telemetry must not alter billing/error records")

	versionSQL := "SELECT version()"
	if dialect == "sqlite" {
		versionSQL = "SELECT sqlite_version()"
	}
	var version string
	require.NoError(t, db.Raw(versionSQL).Scan(&version).Error)
	t.Logf("error log database: %s %s; existing records and indexes preserved after two migrations", dialect, version)
}
