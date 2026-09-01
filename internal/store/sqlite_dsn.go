package store

import (
	"net/url"
	"runtime"
)

const sqlitePragmas = "_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"

// sqliteDSN returns a driver DSN that keeps native Windows filenames intact.
// modernc.org/sqlite treats a file: URI as an SQLite URI; a Windows path
// containing backslashes is not a valid URI path and can make WAL setup fail
// with the misleading "out of memory" result code.
func sqliteDSN(dbPath string) string {
	return sqliteDSNForOS(dbPath, runtime.GOOS)
}

func sqliteDSNForOS(dbPath, goos string) string {
	if goos == "windows" {
		return dbPath + "?" + sqlitePragmas
	}

	dsnURL := &url.URL{
		Scheme:   "file",
		Path:     dbPath,
		RawQuery: sqlitePragmas,
	}
	return dsnURL.String()
}
