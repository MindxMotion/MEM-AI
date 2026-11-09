package common

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/glebarez/go-sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSQLiteDSNKeepsConcurrencyParamsForBarePaths(t *testing.T) {
	const params = "?_pragma=busy_timeout(30000)&_pragma=journal_mode(WAL)&_txlock=immediate"
	for _, tc := range []struct {
		name string
		path string
		want string
	}{
		{name: "default file name", path: "one-api.db", want: "one-api.db" + params},
		{name: "bare absolute path", path: "/data/new-api.db", want: "/data/new-api.db" + params},
		{name: "bare file URI", path: "file:/data/new-api.db", want: "file:/data/new-api.db" + params},
		{name: "in-memory database", path: ":memory:", want: ":memory:" + params},
		{name: "own query string", path: "/data/new-api.db?_pragma=busy_timeout(5000)", want: "/data/new-api.db?_pragma=busy_timeout(5000)"},
		{name: "file URI with query", path: "file:test?mode=memory&cache=shared", want: "file:test?mode=memory&cache=shared"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, sqliteDSN(tc.path))
		})
	}

}

func TestSQLiteDSNAppliesConcurrencyPragmas(t *testing.T) {
	for _, tc := range []struct {
		name        string
		query       string
		wantTimeout int
		wantJournal string
	}{
		{name: "bare path defaults", wantTimeout: 30000, wantJournal: "wal"},
		{name: "explicit query is preserved", query: "?_pragma=busy_timeout(5000)", wantTimeout: 5000, wantJournal: "delete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := sql.Open("sqlite", sqliteDSN(filepath.Join(t.TempDir(), "concurrency.db")+tc.query))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			var timeout int
			require.NoError(t, db.QueryRow("PRAGMA busy_timeout").Scan(&timeout))
			assert.Equal(t, tc.wantTimeout, timeout)
			var journal string
			require.NoError(t, db.QueryRow("PRAGMA journal_mode").Scan(&journal))
			assert.Equal(t, tc.wantJournal, journal)
		})
	}
}
