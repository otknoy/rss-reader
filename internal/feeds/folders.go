package feeds

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// Folder groups subscriptions at one level.
type Folder struct {
	ID   int64
	Name string
}

// ErrFolderName indicates an empty folder name.
var ErrFolderName = errors.New("フォルダー名を入力してください")

var (
	ErrFolderNotFound     = errors.New("folder not found")
	ErrFolderNameConflict = errors.New("folder name already in use")
	ErrInvalidFolderOrder = errors.New("invalid folder order")
)

func folderConstraintError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23503":
			return fmt.Errorf("%w: %w", ErrFolderNotFound, err)
		case "23505":
			return fmt.Errorf("%w: %w", ErrFolderNameConflict, err)
		}
	}
	return err
}

// Folders lists folders in display order, followed by unpositioned folders by name.
func (s *Store) Folders(ctx context.Context) ([]Folder, error) {
	return listFolders(ctx, s.db)
}

func listFolders(ctx context.Context, db queryer) (folders []Folder, err error) {
	rows, err := db.QueryContext(ctx, "SELECT id,name FROM feeds.folders ORDER BY sort_position NULLS LAST,name,id")
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var folder Folder
		if err := rows.Scan(&folder.ID, &folder.Name); err != nil {
			return nil, err
		}
		folders = append(folders, folder)
	}
	return folders, rows.Err()
}

// ErrFolderOrderChanged indicates the caller's displayed order is stale.
var ErrFolderOrderChanged = errors.New("folder order changed")

// SetFolderOrder saves a sidebar order if it still matches the caller's original order.
func (s *Store) SetFolderOrder(ctx context.Context, previous, ids []int64) (err error) {
	if !sameFolderIDs(previous, ids) {
		return ErrInvalidFolderOrder
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if rollbackErr := tx.Rollback(); !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, rollbackErr)
		}
	}()
	if _, err := tx.ExecContext(ctx, "LOCK TABLE feeds.folders IN SHARE ROW EXCLUSIVE MODE"); err != nil {
		return err
	}

	current, err := listFolders(ctx, tx)
	if err != nil {
		return err
	}
	if len(current) != len(previous) {
		return ErrFolderOrderChanged
	}
	for i, folder := range current {
		if folder.ID != previous[i] {
			return ErrFolderOrderChanged
		}
	}

	if _, err := tx.ExecContext(ctx, `UPDATE feeds.folders SET sort_position = array_position($1::bigint[], id)`, ids); err != nil {
		return err
	}
	return tx.Commit()
}

func sameFolderIDs(previous, ids []int64) bool {
	if len(previous) == 0 || len(previous) != len(ids) {
		return false
	}
	remaining := make(map[int64]bool, len(previous))
	for _, id := range previous {
		if id <= 0 || remaining[id] {
			return false
		}
		remaining[id] = true
	}
	for _, id := range ids {
		if !remaining[id] {
			return false
		}
		delete(remaining, id)
	}
	return true
}

// SaveFolder creates a folder or renames an existing one.
func (s *Store) SaveFolder(ctx context.Context, id int64, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return ErrFolderName
	}
	if id == 0 {
		_, err := s.db.ExecContext(ctx, "INSERT INTO feeds.folders(name) VALUES($1)", name)
		return folderConstraintError(err)
	}
	return folderConstraintError(folderMutation(s.db.ExecContext(ctx, "UPDATE feeds.folders SET name=$2 WHERE id=$1", id, name)))
}

// DeleteFolder returns its subscriptions to the unfiled list.
func (s *Store) DeleteFolder(ctx context.Context, id int64) error {
	return folderMutation(s.db.ExecContext(ctx, "DELETE FROM feeds.folders WHERE id=$1", id))
}

// Move changes a subscription folder, with zero meaning unfiled.
func (s *Store) Move(ctx context.Context, id, folder int64) error {
	return folderConstraintError(folderMutation(s.db.ExecContext(
		ctx,
		"UPDATE feeds.subscriptions SET folder_id=NULLIF($2::bigint,0) WHERE id=$1 AND active",
		id,
		folder,
	)))
}

func folderMutation(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && count == 0 {
		return sql.ErrNoRows
	}
	return err
}

// ClosedFolders lists closed folders, with zero representing the unfiled list.
func (s *Store) ClosedFolders(ctx context.Context) (closed map[int64]bool, err error) {
	rows, err := s.db.QueryContext(ctx, "SELECT COALESCE(folder_id, 0) FROM feeds.closed_folders")
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	closed = make(map[int64]bool)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		closed[id] = true
	}
	return closed, rows.Err()
}

// SetFolderOpen persists the sidebar state of a folder.
func (s *Store) SetFolderOpen(ctx context.Context, id int64, open bool) error {
	if open {
		_, err := s.db.ExecContext(ctx, "DELETE FROM feeds.closed_folders WHERE folder_id IS NOT DISTINCT FROM NULLIF($1::bigint, 0)", id)
		return err
	}
	_, err := s.db.ExecContext(ctx, "INSERT INTO feeds.closed_folders(folder_id) VALUES(NULLIF($1::bigint, 0)) ON CONFLICT DO NOTHING", id)
	return folderConstraintError(err)
}

// ActiveIDsInFolder returns active subscriptions in a folder, including the unfiled folder.
func (s *Store) ActiveIDsInFolder(ctx context.Context, id int64) (ids []int64, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT s.id
		FROM (
			SELECT id FROM feeds.folders WHERE id = $1
			UNION ALL SELECT 0 WHERE $1 = 0
		) f
		LEFT JOIN feeds.subscriptions s
			ON s.active AND s.folder_id IS NOT DISTINCT FROM NULLIF(f.id, 0)`, id)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()

	ids = []int64{}
	found := false
	for rows.Next() {
		found = true
		var feedID sql.NullInt64
		if err := rows.Scan(&feedID); err != nil {
			return nil, err
		}
		if feedID.Valid {
			ids = append(ids, feedID.Int64)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !found {
		return nil, sql.ErrNoRows
	}
	return ids, nil
}
