package store

import (
	"context"
	"time"

	"ekpanel/internal/agentproto"
)

// SetWordPressInfo records a scan of a site. A site found to have WordPress is marked as a
// WordPress site (also one installed without ekPanel); one where it is gone no longer is.
func (s *Store) SetWordPressInfo(ctx context.Context, siteID string, info agentproto.WordPressInfo) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE sites SET wp_info = $2, wp_scanned_at = now(),
			app = CASE WHEN $3 THEN 'wordpress' WHEN app = 'wordpress' THEN NULL ELSE app END,
			app_version = CASE WHEN $3 AND $4 <> '' THEN $4 WHEN $3 THEN app_version WHEN app = 'wordpress' THEN NULL ELSE app_version END
		WHERE id = $1`, siteID, info, info.Installed, info.Version)
	return err
}

func (s *Store) SetWordPressAuto(ctx context.Context, siteID string, auto WordPressAuto) error {
	_, err := s.pool.Exec(ctx, `UPDATE sites SET wp_auto = $2 WHERE id = $1`, siteID, auto)
	return err
}

func (s *Store) SetStagingOf(ctx context.Context, siteID, liveID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE sites SET staging_of = $2 WHERE id = $1`, siteID, liveID)
	return err
}

// StagingOf returns the staging copy of a live site.
func (s *Store) StagingOf(ctx context.Context, liveID string) (Site, error) {
	return scanSite(s.pool.QueryRow(ctx, `SELECT `+siteColumns+siteFrom+` WHERE st.staging_of = $1`, liveID))
}

// WordPressDue lists PHP sites whose WordPress was last looked at before since (or never),
// oldest first.
func (s *Store) WordPressDue(ctx context.Context, since time.Time, limit int) ([]Site, error) {
	return collectSites(s.pool.Query(ctx, `SELECT `+siteColumns+siteFrom+`
		WHERE st.kind = 'php' AND st.state = 'active' AND (st.wp_scanned_at IS NULL OR st.wp_scanned_at < $1)
		ORDER BY st.wp_scanned_at NULLS FIRST LIMIT $2`, since, limit))
}
