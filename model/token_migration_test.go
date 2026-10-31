package model

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func requireTokenConstraintExists(t *testing.T, db *gorm.DB, constraintName string) {
	t.Helper()
	var count int64
	require.NoError(t, db.Raw(`
SELECT count(*)
FROM pg_catalog.pg_constraint
WHERE conrelid = to_regclass(?)
  AND conname = ?`, "tokens", constraintName).Scan(&count).Error)
	require.EqualValues(t, 1, count)
}

func requireTokenIndexExists(t *testing.T, db *gorm.DB, indexName string) {
	t.Helper()
	var count int64
	require.NoError(t, db.Raw(`
SELECT count(*)
FROM pg_catalog.pg_index AS index_meta
JOIN pg_catalog.pg_class AS index_class
  ON index_class.oid = index_meta.indexrelid
WHERE index_meta.indrelid = to_regclass(?)
  AND index_class.relname = ?`, "tokens", indexName).Scan(&count).Error)
	require.EqualValues(t, 1, count)
}

func testTokenKeyMigrationNonPostgreSQL(t *testing.T, db *gorm.DB) {
	t.Helper()
	tableName := fmt.Sprintf("token_migration_%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = db.Migrator().DropTable(tableName) })

	tableDB := db.Table(tableName)
	require.NoError(t, tableDB.AutoMigrate(&Token{}))
	require.NoError(t, tableDB.Create(&Token{UserId: 1, Key: "preserved-key"}).Error)

	for range 2 {
		require.NoError(t, migrateTokenKeyUniqueness(db))
		require.NoError(t, tableDB.AutoMigrate(&Token{}))
	}

	var preserved Token
	require.NoError(t, tableDB.Where(&Token{Key: "preserved-key"}).First(&preserved).Error)
	assert.Equal(t, 1, preserved.UserId)
	expectedIndex := db.NamingStrategy.IndexName(tableName, "key")
	assert.True(t, db.Migrator().HasIndex(tableName, expectedIndex))
}

func TestMigrateTokenKeyUniquenessSQLite(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	testTokenKeyMigrationNonPostgreSQL(t, db)
}

func TestTokenUserQuotaPermissionMigrationDatabaseMatrix(t *testing.T) {
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
				driver = postgres.Open(dsn)
			}
			db, err := gorm.Open(driver, &gorm.Config{})
			require.NoError(t, err)
			connection, err := db.DB()
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, connection.Close()) })
			for _, upgrade := range []bool{false, true} {
				t.Run(fmt.Sprintf("upgrade=%v", upgrade), func(t *testing.T) {
					table := fmt.Sprintf("quota_permission_%d", time.Now().UnixNano())
					tableDB := db.Table(table).Session(&gorm.Session{})
					t.Cleanup(func() { assert.NoError(t, db.Migrator().DropTable(table)) })
					require.NoError(t, tableDB.AutoMigrate(&Token{}))
					original := Token{
						UserId: 7, Key: "preserved-api-key", Name: "restricted",
						Status: 1, ExpiredTime: 12345, RemainQuota: 99,
						ModelLimitsEnabled: true, ModelLimits: "claude-opus-4-6",
						Group: "default",
					}
					if upgrade {
						// The deployed schema differs only by this new permission column.
						require.NoError(t, tableDB.Migrator().DropColumn(&Token{}, "AllowUserQuotaQuery"))
						require.NoError(t, tableDB.Omit("AllowUserQuotaQuery").Create(&original).Error)
					}
					indexes, err := tableDB.Migrator().GetIndexes(&Token{})
					require.NoError(t, err)
					for range 2 {
						require.NoError(t, tableDB.AutoMigrate(&Token{}))
					}
					if !upgrade {
						require.NoError(t, tableDB.Create(&original).Error)
					}
					var stored Token
					require.NoError(t, tableDB.First(&stored, original.Id).Error)
					assert.Equal(t, original, stored, "existing keys and restrictions survive without granting user-quota access")
					assert.False(t, stored.AllowUserQuotaQuery)
					for _, index := range indexes {
						assert.True(t, tableDB.Migrator().HasIndex(&Token{}, index.Name()))
					}
					assert.Error(t, tableDB.Create(&Token{UserId: 8, Key: original.Key}).Error, "key uniqueness survives both startups")
					require.NoError(t, tableDB.Where("id = ?", original.Id).Update("allow_user_quota_query", true).Error)
					require.NoError(t, tableDB.First(&stored, original.Id).Error)
					assert.True(t, stored.AllowUserQuotaQuery, "the permission remains explicitly opt-in for existing keys")
				})
			}
		})
	}
}

func TestMigrateTokenKeyUniquenessMySQL(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("TEST_MYSQL_DSN"))
	if dsn == "" {
		t.Skip("TEST_MYSQL_DSN is not configured")
	}

	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	testTokenKeyMigrationNonPostgreSQL(t, db)
}

func TestMigrateTokenKeyUniquenessPostgreSQL(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not configured")
	}

	db, err := gorm.Open(postgres.New(postgres.Config{
		DSN:                  dsn,
		PreferSimpleProtocol: true,
	}), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })

	tests := []struct {
		name                 string
		prepareOld           func(*testing.T, *gorm.DB)
		expectedError        string
		preservedConstraints []string
		preservedIndexes     []string
	}{
		{name: "fresh"},
		{
			name: "legacy_idx_constraint",
			prepareOld: func(t *testing.T, tx *gorm.DB) {
				t.Helper()
				require.NoError(t, tx.Migrator().DropIndex(&Token{}, tokenKeyIndex))
				require.NoError(t, tx.Exec(
					"ALTER TABLE ? ADD CONSTRAINT ? UNIQUE (?)",
					clause.Table{Name: "tokens"},
					clause.Column{Name: tokenKeyIndex},
					clause.Column{Name: "key"},
				).Error)
			},
		},
		{
			name: "gorm_generated_constraint",
			prepareOld: func(t *testing.T, tx *gorm.DB) {
				t.Helper()
				require.NoError(t, tx.Exec(
					"ALTER TABLE ? ADD CONSTRAINT ? UNIQUE (?)",
					clause.Table{Name: "tokens"},
					clause.Column{Name: gormTokenKeyConstraint},
					clause.Column{Name: "key"},
				).Error)
			},
		},
		{
			name: "postgres_default_constraint_without_target_index",
			prepareOld: func(t *testing.T, tx *gorm.DB) {
				t.Helper()
				require.NoError(t, tx.Migrator().DropIndex(&Token{}, tokenKeyIndex))
				require.NoError(t, tx.Exec(
					"ALTER TABLE ? ADD CONSTRAINT ? UNIQUE (?)",
					clause.Table{Name: "tokens"},
					clause.Column{Name: postgresTokenKeyConstraint},
					clause.Column{Name: "key"},
				).Error)
			},
		},
		{
			name: "non_conflicting_uniqueness_is_preserved",
			prepareOld: func(t *testing.T, tx *gorm.DB) {
				t.Helper()
				require.NoError(t, tx.Exec(
					"ALTER TABLE ? ADD CONSTRAINT ? UNIQUE (?)",
					clause.Table{Name: "tokens"},
					clause.Column{Name: postgresTokenKeyConstraint},
					clause.Column{Name: "key"},
				).Error)
				require.NoError(t, tx.Exec(
					"ALTER TABLE ? ADD CONSTRAINT ? UNIQUE (?, ?)",
					clause.Table{Name: "tokens"},
					clause.Column{Name: "keep_tokens_key_user_id"},
					clause.Column{Name: "key"},
					clause.Column{Name: "user_id"},
				).Error)
				require.NoError(t, tx.Exec(
					"CREATE UNIQUE INDEX ? ON ? (?) WHERE user_id > 0",
					clause.Column{Name: "keep_tokens_partial_key"},
					clause.Table{Name: "tokens"},
					clause.Column{Name: "key"},
				).Error)
			},
			preservedConstraints: []string{"keep_tokens_key_user_id"},
			preservedIndexes:     []string{"keep_tokens_partial_key"},
		},
		{
			name: "arbitrary_constraint_is_rejected",
			prepareOld: func(t *testing.T, tx *gorm.DB) {
				t.Helper()
				require.NoError(t, tx.Exec(
					"ALTER TABLE ? ADD CONSTRAINT ? UNIQUE (?)",
					clause.Table{Name: "tokens"},
					clause.Column{Name: "keep_tokens_key_unique"},
					clause.Column{Name: "key"},
				).Error)
			},
			expectedError:        "unsupported unique constraint",
			preservedConstraints: []string{"keep_tokens_key_unique"},
		},
		{
			name: "deferrable_constraint_is_rejected",
			prepareOld: func(t *testing.T, tx *gorm.DB) {
				t.Helper()
				require.NoError(t, tx.Migrator().DropIndex(&Token{}, tokenKeyIndex))
				require.NoError(t, tx.Exec(
					"ALTER TABLE ? ADD CONSTRAINT ? UNIQUE (?) DEFERRABLE INITIALLY DEFERRED",
					clause.Table{Name: "tokens"},
					clause.Column{Name: postgresTokenKeyConstraint},
					clause.Column{Name: "key"},
				).Error)
			},
			expectedError:        "unsupported definition",
			preservedConstraints: []string{postgresTokenKeyConstraint},
		},
		{
			name: "invalid_target_index_is_rejected",
			prepareOld: func(t *testing.T, tx *gorm.DB) {
				t.Helper()
				require.NoError(t, tx.Migrator().DropIndex(&Token{}, tokenKeyIndex))
				require.NoError(t, tx.Exec(
					"CREATE INDEX ? ON ? (?)",
					clause.Column{Name: tokenKeyIndex},
					clause.Table{Name: "tokens"},
					clause.Column{Name: "key"},
				).Error)
				require.NoError(t, tx.Exec(
					"ALTER TABLE ? ADD CONSTRAINT ? UNIQUE (?)",
					clause.Table{Name: "tokens"},
					clause.Column{Name: postgresTokenKeyConstraint},
					clause.Column{Name: "key"},
				).Error)
			},
			expectedError:        "unexpected definition",
			preservedConstraints: []string{postgresTokenKeyConstraint},
			preservedIndexes:     []string{tokenKeyIndex},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tx := db.Begin()
			require.NoError(t, tx.Error)
			t.Cleanup(func() { _ = tx.Rollback().Error })

			schemaName := fmt.Sprintf("token_migration_%d", time.Now().UnixNano())
			require.NoError(t, tx.Exec(
				"CREATE SCHEMA ?",
				clause.Table{Name: schemaName},
			).Error)
			require.NoError(t, tx.Exec(
				"SET LOCAL search_path TO ?",
				clause.Table{Name: schemaName},
			).Error)

			require.NoError(t, migrateTokenKeyUniqueness(tx))
			require.NoError(t, tx.AutoMigrate(&Token{}))
			original := Token{UserId: 1, Key: "preserved-key", Name: "preserve me"}
			require.NoError(t, tx.Create(&original).Error)
			if test.prepareOld != nil {
				test.prepareOld(t, tx)
			}

			if test.expectedError != "" {
				err := migrateTokenKeyUniqueness(tx)
				require.Error(t, err)
				assert.Contains(t, err.Error(), test.expectedError)
				for _, constraintName := range test.preservedConstraints {
					requireTokenConstraintExists(t, tx, constraintName)
				}
				for _, indexName := range test.preservedIndexes {
					requireTokenIndexExists(t, tx, indexName)
				}
				return
			}

			for range 2 {
				require.NoError(t, migrateTokenKeyUniqueness(tx))
				require.NoError(t, tx.AutoMigrate(&Token{}))
			}

			var preserved Token
			require.NoError(t, tx.First(&preserved, original.Id).Error)
			assert.Equal(t, original.Key, preserved.Key)
			assert.Equal(t, original.Name, preserved.Name)

			constraints, err := inspectTokenKeyUniqueConstraints(tx, "tokens")
			require.NoError(t, err)
			assert.Empty(t, constraints)
			targetIndex, err := inspectTokenKeyIndex(tx, "tokens")
			require.NoError(t, err)
			assert.True(t, targetIndex.standaloneValid)
			for _, constraintName := range test.preservedConstraints {
				requireTokenConstraintExists(t, tx, constraintName)
			}
			for _, indexName := range test.preservedIndexes {
				requireTokenIndexExists(t, tx, indexName)
			}

			duplicateError := tx.Transaction(func(duplicateTx *gorm.DB) error {
				return duplicateTx.Create(&Token{UserId: 2, Key: original.Key}).Error
			})
			require.Error(t, duplicateError)

			var totalRows int64
			require.NoError(t, tx.Model(&Token{}).Count(&totalRows).Error)
			assert.EqualValues(t, 1, totalRows)
		})
	}
}
