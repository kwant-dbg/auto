package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Document is a fetched item ready to be snapshotted.
type Document struct {
	CanonicalURL     string
	ExternalID       string
	MIME             string
	Content          []byte
	PublishedAt      *time.Time
	AuthorHash       string
	OrgHint          string
	InjectionSuspect bool
}

// SaveResult reports what SaveDocument did.
type SaveResult struct {
	ID      int64
	New     bool // a new row was written (first sighting or changed content)
	Changed bool // content differs from the previous version of the same URL
}

// SaveDocument snapshots a document for a source. Unchanged content for a URL
// is a no-op; changed content becomes a new version linked to the previous
// one (SRC-4), which gives change tracking and page diffs for free.
func (s *Store) SaveDocument(ctx context.Context, sourceID int64, d Document) (SaveResult, error) {
	if !d.InjectionSuspect {
		d.InjectionSuspect = LooksLikeInjection(d.Content)
	}
	hash, rel, err := s.Blobs.Put(d.Content)
	if err != nil {
		return SaveResult{}, err
	}

	var res SaveResult
	err = pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var prevID int64
		var prevHash string
		err := tx.QueryRow(ctx, `
			SELECT id, content_hash FROM raw_document
			WHERE source_id = $1 AND canonical_url = $2
			ORDER BY fetched_at DESC, id DESC
			LIMIT 1
			FOR UPDATE`, sourceID, d.CanonicalURL).Scan(&prevID, &prevHash)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return err
		case prevHash == hash:
			res = SaveResult{ID: prevID}
			return nil
		default:
			res.Changed = true
		}

		var prev *int64
		if prevID != 0 {
			prev = &prevID
		}
		err = tx.QueryRow(ctx, `
			INSERT INTO raw_document
				(source_id, canonical_url, external_id, content_hash, blob_path, mime, byte_size,
				 published_at, prev_version_id, author_hash, org_hint, injection_suspect)
			VALUES ($1, $2, NULLIF($3, ''), $4, $5, $6, $7, $8, $9, NULLIF($10, ''), NULLIF($11, ''), $12)
			ON CONFLICT (source_id, canonical_url, content_hash) DO NOTHING
			RETURNING id`,
			sourceID, d.CanonicalURL, d.ExternalID, hash, rel, d.MIME, len(d.Content),
			d.PublishedAt, prev, d.AuthorHash, d.OrgHint, d.InjectionSuspect,
		).Scan(&res.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			// Content reverted to an older version: reuse that row.
			res = SaveResult{}
			return tx.QueryRow(ctx, `SELECT id FROM raw_document
				WHERE source_id = $1 AND canonical_url = $2 AND content_hash = $3`,
				sourceID, d.CanonicalURL, hash).Scan(&res.ID)
		}
		if err != nil {
			return err
		}
		res.New = true
		return nil
	})
	return res, err
}

// AuthorHash is the stored, non-reversible form of a public author handle,
// used for independence counting (PRB-4). Every writer uses this one form so
// documents saved by research tools and by collectors agree.
func AuthorHash(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return ""
	}
	return Hash([]byte("author:" + name))[:16]
}
