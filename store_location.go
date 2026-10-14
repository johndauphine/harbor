package main

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var errStoreLocationChanged = errors.New("This configuration was copied or moved from its original location. Preserve the original configuration and use a new empty database, then re-enter your connection credentials. Do not run copied configurations independently")

// A copied database retains private vault references. Binding before startup
// cleanup prevents another copy from retiring credentials used by the original.
// Old databases without this marker bind on their first current startup.
func (a *App) bindStoreLocation(db *sql.DB) error {
	f, err := os.Open(a.configFile)
	if err != nil {
		return fmt.Errorf("Read configuration location: %w", err)
	}
	path, err := filepath.Abs(a.configFile)
	if err != nil {
		f.Close()
		return err
	}
	canonical, err := canonicalStorePath(f, path)
	f.Close()
	if err != nil {
		return fmt.Errorf("Resolve configuration location: %w", err)
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("CREATE TABLE IF NOT EXISTS store_location(singleton INTEGER PRIMARY KEY CHECK(singleton=1),path TEXT NOT NULL)"); err != nil {
		return fmt.Errorf("Read configuration location binding: %w", err)
	}
	var stored string
	err = tx.QueryRow("SELECT path FROM store_location WHERE singleton=1").Scan(&stored)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err = tx.Exec("INSERT INTO store_location(singleton,path) VALUES(1,?)", canonical); err != nil {
			return err
		}
	case err != nil:
		return fmt.Errorf("Read configuration location binding: %w", err)
	case stored != canonical:
		// Case aliases on case-insensitive volumes can have different strings but
		// still identify the original inode. Never silently bind a different file.
		original, e1 := os.Stat(stored)
		current, e2 := os.Stat(canonical)
		if e1 != nil || e2 != nil || !os.SameFile(original, current) {
			return errStoreLocationChanged
		}
	}
	return tx.Commit()
}
