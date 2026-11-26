package postgres

import (
	"context"
	"database/sql"
	"errors"
	"rss-reader/internal/feeds"
)

// MergeOPML atomically merges the subscriptions parsed from an OPML document.
func (s *Store) MergeOPML(ctx context.Context, subscriptions []feeds.OPMLSubscription) (result feeds.ImportResult, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer func() {
		rollbackErr := tx.Rollback()
		if !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, rollbackErr)
		}
	}()

	for _, feed := range subscriptions {
		var id int64
		err := tx.QueryRowContext(ctx, `INSERT INTO feeds.subscriptions(url,title) VALUES($1,$2)
 ON CONFLICT(url) DO NOTHING RETURNING id`, feed.URL, feed.Title).Scan(&id)
		if err == nil && feed.Folder != "" {
			_, err = tx.ExecContext(ctx, `INSERT INTO feeds.folders(name) VALUES($1) ON CONFLICT(name) DO NOTHING`, feed.Folder)
			if err == nil {
				_, err = tx.ExecContext(
					ctx,
					`UPDATE feeds.subscriptions SET folder_id=(SELECT id FROM feeds.folders WHERE name=$2) WHERE id=$1`,
					id,
					feed.Folder,
				)
			}
		} else if errors.Is(err, sql.ErrNoRows) {
			err = tx.QueryRowContext(ctx, `UPDATE feeds.subscriptions SET active=true,next_fetch_at=NULL WHERE url=$1 AND NOT active RETURNING id`, feed.URL).Scan(&id)
		}

		if errors.Is(err, sql.ErrNoRows) {
			result.Existing++
			continue
		}
		if err != nil {
			return feeds.ImportResult{}, err
		}
		result.Added++
	}

	if err := tx.Commit(); err != nil {
		return feeds.ImportResult{}, err
	}
	return result, nil
}

// OPMLSnapshot reads folder membership and folder names from the same database snapshot.
func (s *Store) OPMLSnapshot(ctx context.Context) (subscriptions []feeds.Subscription, folders []feeds.Folder, err error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		rollbackErr := tx.Rollback()
		if !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, rollbackErr)
		}
	}()

	subscriptions, err = list(ctx, tx, false, nil)
	if err != nil {
		return nil, nil, err
	}
	folders, err = listFolders(ctx, tx)
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	return subscriptions, folders, nil
}
