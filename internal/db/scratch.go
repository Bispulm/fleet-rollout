package db

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
)

// scratchTimeout bounds creating or dropping a scratch database.
const scratchTimeout = 30 * time.Second

// CreateScratch creates an empty database on the server at serverURL, named prefix and a
// random suffix. It returns the new database's URL, and a function that drops it, which works
// even after ctx has ended, so that an interrupted run still cleans up.
func CreateScratch(ctx context.Context, serverURL, prefix string) (string, func() error, error) {
	suffix := make([]byte, 8)
	_, _ = rand.Read(suffix)
	name := prefix + hex.EncodeToString(suffix)
	parsed, err := url.Parse(serverURL)
	if err != nil {
		return "", nil, fmt.Errorf("parse database URL: %w", err)
	}

	setup, cancel := context.WithTimeout(ctx, scratchTimeout)
	defer cancel()
	admin, err := pgx.Connect(setup, serverURL)
	if err != nil {
		return "", nil, fmt.Errorf("connect to database server: %w", err)
	}
	defer func() { _ = admin.Close(context.WithoutCancel(setup)) }()
	if _, err := admin.Exec(setup, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		return "", nil, fmt.Errorf("create scratch database: %w", err)
	}

	drop := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), scratchTimeout)
		defer cancel()
		admin, err := pgx.Connect(ctx, serverURL)
		if err != nil {
			return fmt.Errorf("drop scratch database %s: %w", name, err)
		}
		defer func() { _ = admin.Close(ctx) }()
		if _, err := admin.Exec(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			return fmt.Errorf("drop scratch database %s: %w", name, err)
		}
		return nil
	}
	parsed.Path = "/" + name
	return parsed.String(), drop, nil
}
