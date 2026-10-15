package controller

import (
	"os"
	"testing"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// These DSNs must name disposable databases, never the live application database.
func TestUpstreamBalanceSettingsDatabaseRoundTrip(t *testing.T) {
	for _, engine := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(engine, func(t *testing.T) {
			var dialector gorm.Dialector
			switch engine {
			case "sqlite":
				dialector = sqlite.Open(":memory:")
			case "mysql":
				dsn := os.Getenv("BALANCE_TEST_MYSQL_DSN")
				if dsn == "" {
					t.Skip("disposable MySQL required")
				}
				dialector = mysql.Open(dsn)
			case "postgres":
				dsn := os.Getenv("BALANCE_TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("disposable PostgreSQL required")
				}
				dialector = postgres.Open(dsn)
			}
			// Never log SQL parameters or connection errors containing a DSN.
			db, err := gorm.Open(dialector, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
			require.True(t, err == nil, "disposable database connection failed")
			sqlDB, err := db.DB()
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
			require.NoError(t, db.AutoMigrate(&model.Channel{}))
			channel := model.Channel{Name: "balance-round-trip", Type: 14, Key: "synthetic-fixture",
				Status: 1, UsedQuota: 1234, Balance: 7.25,
				OtherSettings: `{"unrelated":{"keep":true}}`}
			require.NoError(t, db.Create(&channel).Error)
			t.Cleanup(func() { require.NoError(t, db.Unscoped().Delete(&channel).Error) })
			config := `{"unrelated":{"keep":true},"upstream_balance":{"profile":"legacy_subscription_usage","origin":"primary","unit":"USD","scope":"account","period":"cumulative"}}`
			require.NoError(t, db.Model(&channel).Update("settings", config).Error)
			// Re-entry preserves existing TEXT JSON without a schema conversion.
			require.NoError(t, db.AutoMigrate(&model.Channel{}))
			var saved model.Channel
			require.NoError(t, db.First(&saved, channel.Id).Error)
			require.Equal(t, config, saved.OtherSettings)
			require.Equal(t, channel.Key, saved.Key)
			require.Equal(t, channel.Type, saved.Type)
			require.Equal(t, channel.Status, saved.Status)
			require.Equal(t, channel.UsedQuota, saved.UsedQuota)
			require.Equal(t, channel.Balance, saved.Balance)
			profile, err := service.ReadUpstreamBalanceSettings(saved.OtherSettings)
			require.NoError(t, err)
			require.NotNil(t, profile)
			require.Equal(t, "account", profile.Scope)
			query := "SELECT version()"
			if engine == "sqlite" {
				query = "SELECT sqlite_version()"
			}
			var version string
			require.NoError(t, db.Raw(query).Scan(&version).Error)
			t.Logf("settings JSON round-trip and migration re-entry: %s %s", engine, version)
		})
	}
}
