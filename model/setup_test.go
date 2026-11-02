package model

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

func TestSetupPreservesLongVersionAcrossMigrations(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			var driver gorm.Dialector
			switch dialect {
			case "sqlite":
				driver = sqlite.Open(":memory:")
			case "mysql":
				dsn := os.Getenv("TEST_MYSQL_DSN")
				if dsn == "" {
					t.Skip("TEST_MYSQL_DSN is not configured")
				}
				driver = mysql.Open(dsn)
			case "postgres":
				dsn := os.Getenv("TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("TEST_POSTGRES_DSN is not configured")
				}
				driver = postgres.New(postgres.Config{DSN: dsn, PreferSimpleProtocol: true})
			}
			db, err := gorm.Open(driver, &gorm.Config{
				NamingStrategy: schema.NamingStrategy{
					TablePrefix: fmt.Sprintf("setup_version_%d_", time.Now().UnixNano()),
				},
			})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			sqlDB.SetMaxOpenConns(1)
			t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
			t.Cleanup(func() { require.NoError(t, db.Migrator().DropTable(&Setup{})) })
			require.NoError(t, db.AutoMigrate(&Setup{}))

			original := Setup{Version: strings.Repeat("release-", 12) + "20261102", InitializedAt: 1793620800}
			require.NoError(t, db.Create(&original).Error)
			require.NoError(t, db.AutoMigrate(&Setup{}))
			var stored Setup
			require.NoError(t, db.First(&stored, original.ID).Error)
			require.Equal(t, original, stored)
		})
	}
}
