-- How a Git app is built: its Dockerfile, Railpack (no Dockerfile needed), or the
-- Dockerfile when there is one and Railpack otherwise.
ALTER TABLE apps ADD COLUMN builder text NOT NULL DEFAULT 'auto' CHECK (builder IN ('auto', 'dockerfile', 'railpack'));

-- Preview deploys: a Git app with previews on gets an app of its own for each pull request
-- to its branch, at <preview app name>.<preview_domain>, removed when the request closes.
-- The parent row goes only after the agent removed it, so previews are deleted with it
-- explicitly; SET NULL only covers a row removed some other way.
ALTER TABLE apps ADD COLUMN previews boolean NOT NULL DEFAULT false;
ALTER TABLE apps ADD COLUMN preview_domain text;
ALTER TABLE apps ADD COLUMN preview_of uuid REFERENCES apps (id) ON DELETE SET NULL;
ALTER TABLE apps ADD COLUMN preview_number integer;
ALTER TABLE apps ADD CONSTRAINT apps_preview_check CHECK (preview_of IS NULL OR preview_number IS NOT NULL);
CREATE UNIQUE INDEX apps_preview_idx ON apps (preview_of, preview_number) WHERE preview_of IS NOT NULL;

-- A deployment of a pull request fetches this ref instead of the branch.
ALTER TABLE deployments ADD COLUMN ref text;

-- Customers may have Git apps only when their package says so.
ALTER TABLE packages ADD COLUMN git_apps boolean NOT NULL DEFAULT false;
