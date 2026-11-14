-- Suspended customers cannot sign in, and their websites and apps answer with a page that
-- says so. The reason is for the administrators.
ALTER TABLE users ADD COLUMN suspended_at timestamptz;
ALTER TABLE users ADD COLUMN suspend_reason text;

-- A monthly bandwidth limit (calendar month, UTC). With bandwidth_stop, websites and apps
-- answer with a page that says so once it is used up, until the next month.
ALTER TABLE packages ADD COLUMN bandwidth_gb int;
ALTER TABLE packages ADD COLUMN bandwidth_stop boolean NOT NULL DEFAULT false;
-- Set while a customer's websites are stopped for using up their bandwidth.
ALTER TABLE users ADD COLUMN bandwidth_blocked boolean NOT NULL DEFAULT false;
