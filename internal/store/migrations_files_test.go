package store_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var createTable = regexp.MustCompile(`(?im)^\s*CREATE TABLE\s`)

// migrationFiles returns the versions in migrations/ in apply order, and how
// many tables a fresh schema holds after applying them (theirs plus
// schema_migrations).
func migrationFiles(t *testing.T) ([]string, int) {
	t.Helper()
	names, err := filepath.Glob(filepath.Join("migrations", "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	var versions []string
	tables := 1
	for _, name := range names {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		versions = append(versions, strings.TrimSuffix(filepath.Base(name), ".sql"))
		tables += len(createTable.FindAll(b, -1))
	}
	return versions, tables
}
