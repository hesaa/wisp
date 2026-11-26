package store

import (
	"database/sql"
	"errors"
)

// LinkPreview is a locally cached OpenGraph/HTML preview (or negative 'none' marker) keyed by URL.
type LinkPreview struct {
	URL, Status, Canonical, Title, Desc string
	Thumb                               []byte
	FetchedAt                           int64
}

// GetLinkPreview returns the cached preview for url; ok is false when not present.
func (d *DB) GetLinkPreview(url string) (LinkPreview, bool, error) {
	var p LinkPreview
	err := d.sql.QueryRow(
		`SELECT url, status, canonical, title, desc, thumb, fetched_at FROM link_previews WHERE url = ?`,
		url,
	).Scan(&p.URL, &p.Status, &p.Canonical, &p.Title, &p.Desc, &p.Thumb, &p.FetchedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return LinkPreview{}, false, nil
	}
	if err != nil {
		return LinkPreview{}, false, err
	}
	return p, true, nil
}

// PutLinkPreview inserts or updates a cached link preview entry.
func (d *DB) PutLinkPreview(p LinkPreview) error {
	_, err := d.sql.Exec(
		`INSERT INTO link_previews (url, status, canonical, title, desc, thumb, fetched_at)
		VALUES (?, ?, ?, ?, ?, COALESCE(?, x''), ?)
		ON CONFLICT(url) DO UPDATE SET
			status = excluded.status,
			canonical = excluded.canonical,
			title = excluded.title,
			desc = excluded.desc,
			thumb = excluded.thumb,
			fetched_at = excluded.fetched_at`,
		p.URL, p.Status, p.Canonical, p.Title, p.Desc, p.Thumb, p.FetchedAt,
	)
	return err
}

// ClearLinkPreviews removes all cached link preview entries.
func (d *DB) ClearLinkPreviews() error {
	_, err := d.sql.Exec(`DELETE FROM link_previews`)
	return err
}
