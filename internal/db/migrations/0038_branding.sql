-- White-label: the panel's own name, color, logo and domain, and each reseller's.
-- reseller_id NULL is the panel's brand (at most one); a reseller's brand applies to the
-- reseller and their customers when the administrator allows it (resellers.white_label).
CREATE TABLE brands (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    reseller_id  uuid UNIQUE REFERENCES users (id) ON DELETE CASCADE,
    name         text NOT NULL DEFAULT '' CHECK (length(name) <= 60),
    color        text NOT NULL DEFAULT '' CHECK (color = '' OR color ~ '^#[0-9a-f]{6}$'),
    logo         bytea,                                    -- PNG, re-encoded by the panel
    logo_version integer NOT NULL DEFAULT 0,               -- in the logo's address, for caching
    domain       text UNIQUE,                              -- served by the panel's server
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX brands_panel_key ON brands ((true)) WHERE reseller_id IS NULL;

ALTER TABLE resellers ADD COLUMN white_label boolean NOT NULL DEFAULT false;

-- A brand's domain is a host name like a website's, so no website or app can take it.
ALTER TABLE site_hostnames ADD COLUMN brand_id uuid REFERENCES brands (id) ON DELETE CASCADE;
ALTER TABLE site_hostnames DROP CONSTRAINT site_hostnames_check;
ALTER TABLE site_hostnames ADD CONSTRAINT site_hostnames_one_owner CHECK (num_nonnulls(site_id, app_id, brand_id) = 1);
