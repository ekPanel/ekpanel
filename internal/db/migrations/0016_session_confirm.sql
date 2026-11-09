-- Some actions (opening a terminal) need the password again: a session confirmed this way
-- is trusted for them until confirmed_until.
ALTER TABLE sessions ADD COLUMN confirmed_until timestamptz;
