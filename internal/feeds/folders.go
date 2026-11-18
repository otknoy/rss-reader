package feeds

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// Folder groups subscriptions at one level.
type Folder struct {
	ID   int64
	Name string
}

// ErrFolderName indicates an empty folder name.
var ErrFolderName = errors.New("フォルダー名を入力してください")

// Folders lists folders by name.
func (s *Store) Folders(ctx context.Context) (folders []Folder, err error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id,name FROM feeds.folders ORDER BY name,id")
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

// SaveFolder creates a folder or renames an existing one.
func (s *Store) SaveFolder(ctx context.Context, id int64, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return ErrFolderName
	}
	if id == 0 {
		_, err := s.db.ExecContext(ctx, "INSERT INTO feeds.folders(name) VALUES($1)", name)
		return err
	}
	return folderMutation(s.db.ExecContext(ctx, "UPDATE feeds.folders SET name=$2 WHERE id=$1", id, name))
}

// DeleteFolder returns its subscriptions to the unfiled list.
func (s *Store) DeleteFolder(ctx context.Context, id int64) error {
	return folderMutation(s.db.ExecContext(ctx, "DELETE FROM feeds.folders WHERE id=$1", id))
}

// Move changes a subscription folder, with zero meaning unfiled.
func (s *Store) Move(ctx context.Context, id, folder int64) error {
	return folderMutation(s.db.ExecContext(ctx, "UPDATE feeds.subscriptions SET folder_id=NULLIF($2::bigint,0) WHERE id=$1 AND active", id, folder))
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
