package model

import (
	"os"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// The same fixture is run on the latest released source to seed an upgrade,
// then on this revision. All DSNs must name disposable CI databases.
func TestUpstreamSyncStartupDatabase(t *testing.T) {
	mode := os.Getenv("SYNC_DATABASE_MODE")
	if mode == "" {
		t.Skip("requires a disposable startup/upgrade database")
	}
	require.Contains(t, []string{"seed", "fresh", "upgrade"}, mode)
	require.NotEmpty(t, os.Getenv("SYNC_SQL_DSN"))
	require.NotEmpty(t, os.Getenv("SYNC_LOG_SQL_DSN"))
	t.Setenv("SQL_DSN", os.Getenv("SYNC_SQL_DSN"))
	t.Setenv("LOG_SQL_DSN", os.Getenv("SYNC_LOG_SQL_DSN"))
	previousDB, previousLog := DB, LOG_DB
	previousMainType, previousLogType := common.MainDatabaseType(), common.LogDatabaseType()
	previousMaster, previousPath := common.IsMasterNode, common.SQLitePath
	common.IsMasterNode = true
	t.Cleanup(func() {
		DB, LOG_DB = previousDB, previousLog
		common.SetDatabaseTypes(previousMainType, previousLogType)
		common.IsMasterNode, common.SQLitePath = previousMaster, previousPath
		initCol()
	})

	profile := `{"upstream_balance":{"profile":"legacy_subscription_usage","origin":"primary","unit":"USD","scope":"account","period":"cumulative"},"plugin_config":{"region":"eu"}}`
	for start := range 2 {
		common.SQLitePath = os.Getenv("SYNC_SQLITE_PATH")
		require.NoError(t, InitDB())
		mainDB := DB
		if os.Getenv("SYNC_SQLITE_LOG_PATH") != "" {
			common.SQLitePath = os.Getenv("SYNC_SQLITE_LOG_PATH")
		}
		require.NoError(t, InitLogDB())
		common.SQLitePath = os.Getenv("SYNC_SQLITE_PATH")
		logDB := LOG_DB

		if start == 0 && mode != "upgrade" {
			require.NoError(t, DB.Create(&User{
				Id: 700001, Username: "sync-startup-user", Quota: 1 << 34, UsedQuota: 1234,
			}).Error)
			require.NoError(t, DB.Create(&Token{
				Id: 700001, UserId: 700001, Key: strings.Repeat("s", 64), Name: "sync-token", RemainQuota: 999,
			}).Error)
			require.NoError(t, DB.Create(&Channel{
				Id: 700001, Type: 14, Name: "sync-channel", Key: "synthetic-fixture-key",
				Status: common.ChannelStatusEnabled, Models: "sync-model", Group: "default",
				Balance: 7.25, UsedQuota: 1234, OtherSettings: profile,
			}).Error)
			require.NoError(t, LOG_DB.Create(&Log{
				Id: 700001, UserId: 700001, Type: 1, Quota: 11, TokenName: "sync-token",
			}).Error)
		}

		var user User
		require.NoError(t, DB.First(&user, 700001).Error)
		assert.Equal(t, "sync-startup-user", user.Username)
		assert.Equal(t, int(1<<34), user.Quota)
		assert.Equal(t, 1234, user.UsedQuota)
		var token Token
		require.NoError(t, DB.First(&token, 700001).Error)
		assert.Equal(t, strings.Repeat("s", 64), token.Key)
		assert.Equal(t, 999, token.RemainQuota)
		var channel Channel
		require.NoError(t, DB.First(&channel, 700001).Error)
		assert.Equal(t, "synthetic-fixture-key", channel.Key)
		assert.Equal(t, common.ChannelStatusEnabled, channel.Status)
		assert.Equal(t, 7.25, channel.Balance)
		assert.Equal(t, 1234, channel.UsedQuota)
		assert.Equal(t, profile, channel.OtherSettings)
		var log Log
		require.NoError(t, LOG_DB.First(&log, 700001).Error)
		assert.Equal(t, 11, log.Quota)
		assert.Equal(t, "sync-token", log.TokenName)
		assert.Error(t, DB.Create(&User{Username: user.Username}).Error)
		assert.Error(t, DB.Create(&Token{UserId: user.Id, Key: token.Key}).Error)

		var version string
		query := "SELECT version()"
		if common.UsingMainDatabase(common.DatabaseTypeSQLite) {
			query = "SELECT sqlite_version()"
		}
		require.NoError(t, DB.Raw(query).Scan(&version).Error)
		t.Logf("%s startup %d: %s %s; separate log database and existing records preserved", mode, start+1, common.MainDatabaseType(), version)
		for index, db := range []*gorm.DB{logDB, mainDB} {
			if index == 1 && logDB == mainDB {
				continue
			}
			sqlDB, err := db.DB()
			require.NoError(t, err)
			require.NoError(t, sqlDB.Close())
		}
	}
}
