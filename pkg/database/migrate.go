// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package database

import (
	"database/sql"
	"errors"
	"fmt"
	"log"

	"github.com/golang-migrate/migrate/v4"
	migratepg "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	"github.com/FlashbackAi/teepin-core/migrations"
)

// Migrate applies every pending schema migration embedded in the
// binary. It is safe to call on every startup: already-applied
// versions are skipped, and concurrent runners are serialized by
// golang-migrate's advisory lock.
func Migrate(db *sql.DB) error {
	source, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("failed to load embedded migrations: %w", err)
	}

	driver, err := migratepg.WithInstance(db, &migratepg.Config{})
	if err != nil {
		return fmt.Errorf("failed to init migration driver: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", source, "postgres", driver)
	if err != nil {
		return fmt.Errorf("failed to init migrator: %w", err)
	}

	if err := m.Up(); err != nil {
		if errors.Is(err, migrate.ErrNoChange) {
			log.Println("✅ Database schema up to date")
			return nil
		}
		return fmt.Errorf("migration failed: %w", err)
	}

	version, dirty, _ := m.Version()
	log.Printf("✅ Database migrated to version %d (dirty=%v)", version, dirty)
	return nil
}
