package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Brand is a white-label: the panel's own (ResellerID nil) or a reseller's.
type Brand struct {
	ID          string    `json:"id"`
	ResellerID  *string   `json:"reseller_id"`
	Name        string    `json:"name"`
	Color       string    `json:"color"`
	HasLogo     bool      `json:"has_logo"`
	LogoVersion int       `json:"logo_version"`
	Domain      *string   `json:"domain"`
	UpdatedAt   time.Time `json:"updated_at"`
}

const brandColumns = `id, reseller_id, name, color, logo IS NOT NULL, logo_version, domain, updated_at`

func scanBrand(row pgx.Row) (Brand, error) {
	var b Brand
	err := row.Scan(&b.ID, &b.ResellerID, &b.Name, &b.Color, &b.HasLogo, &b.LogoVersion, &b.Domain, &b.UpdatedAt)
	return b, notFound(err)
}

// GetBrand returns the panel's brand (resellerID nil) or a reseller's; ErrNotFound if
// none was saved.
func (s *Store) GetBrand(ctx context.Context, resellerID *string) (Brand, error) {
	if resellerID == nil {
		return scanBrand(s.pool.QueryRow(ctx, `SELECT `+brandColumns+` FROM brands WHERE reseller_id IS NULL`))
	}
	return scanBrand(s.pool.QueryRow(ctx, `SELECT `+brandColumns+` FROM brands WHERE reseller_id = $1`, *resellerID))
}

// GetBrandByID returns a brand by its ID.
func (s *Store) GetBrandByID(ctx context.Context, id string) (Brand, error) {
	return scanBrand(s.pool.QueryRow(ctx, `SELECT `+brandColumns+` FROM brands WHERE id::text = $1`, id))
}

// BrandByDomain returns the brand whose domain is host, if it is in use: the panel's, or a
// reseller's who may use white-label.
func (s *Store) BrandByDomain(ctx context.Context, host string) (Brand, error) {
	return scanBrand(s.pool.QueryRow(ctx, `SELECT `+brandColumns+` FROM brands
		WHERE domain = $1 AND (reseller_id IS NULL OR (SELECT white_label FROM resellers WHERE user_id = brands.reseller_id))`, host))
}

// SaveBrand creates or changes the panel's brand or a reseller's (not its logo). The domain
// is claimed like a website's host name: ErrHostnameTaken if a website, app or another
// brand has it.
func (s *Store) SaveBrand(ctx context.Context, resellerID *string, name, color string, domain *string) (Brand, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Brand{}, err
	}
	defer tx.Rollback(ctx)
	var id string
	if resellerID == nil {
		err = tx.QueryRow(ctx, `SELECT id FROM brands WHERE reseller_id IS NULL`).Scan(&id)
	} else {
		err = tx.QueryRow(ctx, `SELECT id FROM brands WHERE reseller_id = $1`, *resellerID).Scan(&id)
	}
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		err = tx.QueryRow(ctx, `INSERT INTO brands (reseller_id, name, color) VALUES ($1, $2, $3) RETURNING id`,
			resellerID, name, color).Scan(&id)
	case err == nil:
		_, err = tx.Exec(ctx, `UPDATE brands SET name = $2, color = $3, updated_at = now() WHERE id = $1`, id, name, color)
	}
	if err != nil {
		return Brand{}, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM site_hostnames WHERE brand_id = $1`, id); err != nil {
		return Brand{}, err
	}
	if domain != nil {
		tag, err := tx.Exec(ctx, `INSERT INTO site_hostnames (hostname, brand_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, *domain, id)
		if err != nil {
			return Brand{}, err
		}
		if tag.RowsAffected() == 0 {
			return Brand{}, ErrHostnameTaken{Hostname: *domain}
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE brands SET domain = $2 WHERE id = $1`, id, domain); err != nil {
		return Brand{}, err
	}
	b, err := scanBrand(tx.QueryRow(ctx, `SELECT `+brandColumns+` FROM brands WHERE id = $1`, id))
	if err != nil {
		return Brand{}, err
	}
	return b, tx.Commit(ctx)
}

// SetBrandLogo replaces a brand's logo (a PNG), or removes it with nil.
func (s *Store) SetBrandLogo(ctx context.Context, id string, png []byte) (Brand, error) {
	return scanBrand(s.pool.QueryRow(ctx, `UPDATE brands SET logo = $2, logo_version = logo_version + 1, updated_at = now()
		WHERE id = $1 RETURNING `+brandColumns, id, png))
}

// BrandLogo returns a brand's logo, ErrNotFound if it has none.
func (s *Store) BrandLogo(ctx context.Context, id string) ([]byte, int, error) {
	var png []byte
	var version int
	err := s.pool.QueryRow(ctx, `SELECT logo, logo_version FROM brands WHERE id::text = $1 AND logo IS NOT NULL`, id).Scan(&png, &version)
	return png, version, notFound(err)
}

// PanelDomains lists the domains of the panel's brand and of resellers allowed white-label.
func (s *Store) PanelDomains(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT b.domain FROM brands b LEFT JOIN resellers r ON r.user_id = b.reseller_id
		WHERE b.domain IS NOT NULL AND (b.reseller_id IS NULL OR r.white_label) ORDER BY b.domain`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// BrandFor returns the brand a user sees: their reseller's (for a customer) or their own
// (for a reseller) when white-label is allowed, else the panel's. ok is false when the
// user sees the panel's brand.
func (s *Store) BrandFor(ctx context.Context, userID string) (b Brand, ok bool, err error) {
	b, err = scanBrand(s.pool.QueryRow(ctx, `SELECT `+brandColumns+` FROM brands
		WHERE reseller_id = (SELECT COALESCE(u.reseller_id, CASE WHEN u.role = 'reseller' THEN u.id END) FROM users u WHERE u.id = $1)
		  AND (SELECT white_label FROM resellers WHERE user_id = brands.reseller_id)`, userID))
	if errors.Is(err, ErrNotFound) {
		return Brand{}, false, nil
	}
	return b, err == nil, err
}

// EffectiveBrand is what someone sees: the panel's brand, or over it a reseller's (whose
// name and logo go together; the color and domain fall back to the panel's).
type EffectiveBrand struct {
	Name        string
	Color       string
	LogoID      string // brand whose logo is shown; "" for none
	LogoVersion int
	Domain      *string
	// Custom is false while everything is ekPanel's own.
	Custom bool
}

// EffectiveBrand returns the brand for a signed-in user, or (userID nil) for a visitor
// who opened the panel at host.
func (s *Store) EffectiveBrand(ctx context.Context, userID *string, host string) (EffectiveBrand, error) {
	e := EffectiveBrand{Name: "ekPanel"}
	panel, err := s.GetBrand(ctx, nil)
	switch {
	case err == nil:
		if panel.Name != "" {
			e.Name, e.Custom = panel.Name, true
		}
		if panel.HasLogo {
			e.LogoID, e.LogoVersion, e.Custom = panel.ID, panel.LogoVersion, true
		}
		e.Color, e.Domain = panel.Color, panel.Domain
		e.Custom = e.Custom || panel.Color != ""
	case !errors.Is(err, ErrNotFound):
		return e, err
	}
	var own Brand
	found := false
	switch {
	case userID != nil:
		if own, found, err = s.BrandFor(ctx, *userID); err != nil {
			return e, err
		}
	case host != "":
		own, err = s.BrandByDomain(ctx, host)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return e, err
		}
		found = err == nil && own.ResellerID != nil
	}
	if found && own.Name != "" {
		e.Name, e.Custom = own.Name, true
		e.LogoID, e.LogoVersion = "", 0
		if own.HasLogo {
			e.LogoID, e.LogoVersion = own.ID, own.LogoVersion
		}
		if own.Color != "" {
			e.Color = own.Color
		}
		if own.Domain != nil {
			e.Domain = own.Domain
		}
	}
	return e, nil
}
