// Package catalog manages the file_catalog MySQL table.
package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/go-sql-driver/mysql"

	"diskmon/internal/model"
)

// Catalog wraps a *sql.DB and provides CRUD operations for file_catalog.
type Catalog struct {
	db *sql.DB
}

// New creates a Catalog backed by the given connection pool.
func New(db *sql.DB) *Catalog {
	return &Catalog{db: db}
}

// EnsurePartition creates the per-server partition if it does not already exist.
// Uses ALTER TABLE ... ADD PARTITION; no restart needed (online DDL in MySQL 8.0).
func (c *Catalog) EnsurePartition(ctx context.Context, serverID string) error {
	pname := partitionName(serverID)
	ddl := fmt.Sprintf(
		"ALTER TABLE file_catalog ADD PARTITION (PARTITION %s VALUES IN (?))", pname)
	_, err := c.db.ExecContext(ctx, ddl, serverID)
	if err != nil {
		if isPartitionExists(err) {
			return nil // already exists, that's fine
		}
		return fmt.Errorf("ensure partition %s: %w", pname, err)
	}
	return nil
}

// TruncateServer removes all rows for the given server before a full rescan.
// Uses TRUNCATE PARTITION for O(1) performance; falls back to DELETE if needed.
func (c *Catalog) TruncateServer(ctx context.Context, serverID string) error {
	pname := partitionName(serverID)
	_, err := c.db.ExecContext(ctx,
		fmt.Sprintf("ALTER TABLE file_catalog TRUNCATE PARTITION %s", pname))
	if err != nil {
		_, err = c.db.ExecContext(ctx,
			"DELETE FROM file_catalog WHERE server_id=?", serverID)
	}
	if err != nil {
		return fmt.Errorf("truncate server %s: %w", serverID, err)
	}
	return nil
}

// UpsertBatch inserts or updates entries in chunks of 500 rows.
// Uses multi-row INSERT ... ON DUPLICATE KEY UPDATE for efficiency.
func (c *Catalog) UpsertBatch(ctx context.Context, entries []model.FileEntry) error {
	if len(entries) == 0 {
		return nil
	}
	const chunkSize = 500
	for i := 0; i < len(entries); i += chunkSize {
		end := i + chunkSize
		if end > len(entries) {
			end = len(entries)
		}
		if err := c.upsertChunk(ctx, entries[i:end]); err != nil {
			return err
		}
	}
	return nil
}

func (c *Catalog) upsertChunk(ctx context.Context, entries []model.FileEntry) error {
	placeholder := strings.Repeat("(?,?,?,?,?,?,?,?),", len(entries))
	placeholder = placeholder[:len(placeholder)-1]

	query := `INSERT INTO file_catalog (server_id, volume, path, is_dir, size, ext, biz_key, updated_at)
VALUES ` + placeholder + `
ON DUPLICATE KEY UPDATE
    volume=VALUES(volume), is_dir=VALUES(is_dir), size=VALUES(size),
    ext=VALUES(ext), biz_key=VALUES(biz_key), updated_at=VALUES(updated_at),
    synced_at=NOW()`

	args := make([]any, 0, len(entries)*8)
	for _, e := range entries {
		args = append(args, e.ServerID, e.Volume, e.Path, e.IsDir, e.Size,
			nullStr(e.Ext), nullStr(e.BizKey), e.UpdatedAt)
	}
	_, err := c.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("upsert chunk: %w", err)
	}
	return nil
}

// Upsert inserts or updates a single entry.
func (c *Catalog) Upsert(ctx context.Context, e model.FileEntry) error {
	const query = `
INSERT INTO file_catalog (server_id, volume, path, is_dir, size, ext, biz_key, updated_at)
VALUES (?,?,?,?,?,?,?,?)
ON DUPLICATE KEY UPDATE
    volume=VALUES(volume), is_dir=VALUES(is_dir), size=VALUES(size),
    ext=VALUES(ext), biz_key=VALUES(biz_key), updated_at=VALUES(updated_at),
    synced_at=NOW()`
	_, err := c.db.ExecContext(ctx, query,
		e.ServerID, e.Volume, e.Path, e.IsDir, e.Size,
		nullStr(e.Ext), nullStr(e.BizKey), e.UpdatedAt)
	if err != nil {
		return fmt.Errorf("upsert %s: %w", e.Path, err)
	}
	return nil
}

// Delete removes a single file or directory entry.
func (c *Catalog) Delete(ctx context.Context, serverID, path string) error {
	_, err := c.db.ExecContext(ctx,
		"DELETE FROM file_catalog WHERE server_id=? AND path=?", serverID, path)
	if err != nil {
		return fmt.Errorf("delete %s: %w", path, err)
	}
	return nil
}

// Rename deletes the old path and upserts the new path in a single transaction.
func (c *Catalog) Rename(ctx context.Context, serverID, oldPath string, newEntry model.FileEntry) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin rename tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	_, err = tx.ExecContext(ctx,
		"DELETE FROM file_catalog WHERE server_id=? AND path=?", serverID, oldPath)
	if err != nil {
		return fmt.Errorf("rename delete %s: %w", oldPath, err)
	}

	const query = `
INSERT INTO file_catalog (server_id, volume, path, is_dir, size, ext, biz_key, updated_at)
VALUES (?,?,?,?,?,?,?,?)
ON DUPLICATE KEY UPDATE
    volume=VALUES(volume), is_dir=VALUES(is_dir), size=VALUES(size),
    ext=VALUES(ext), biz_key=VALUES(biz_key), updated_at=VALUES(updated_at),
    synced_at=NOW()`
	_, err = tx.ExecContext(ctx, query,
		newEntry.ServerID, newEntry.Volume, newEntry.Path, newEntry.IsDir, newEntry.Size,
		nullStr(newEntry.Ext), nullStr(newEntry.BizKey), newEntry.UpdatedAt)
	if err != nil {
		return fmt.Errorf("rename upsert %s: %w", newEntry.Path, err)
	}

	return tx.Commit()
}

// partitionName returns the MySQL partition name for a server.
// e.g. "server-a" → "p_server_a"
func partitionName(serverID string) string {
	safe := strings.NewReplacer("-", "_", ".", "_", " ", "_").Replace(serverID)
	return "p_" + strings.ToLower(safe)
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// isPartitionExists returns true for MySQL error 1517 (ER_SAME_NAME_PARTITION).
func isPartitionExists(err error) bool {
	if me, ok := err.(*mysql.MySQLError); ok {
		return me.Number == 1517
	}
	return false
}
