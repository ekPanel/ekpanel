-- Apps installed from the service catalogue remember which template and version they came
-- from. Their Compose file is the template's, which customers may run but not change.
ALTER TABLE apps ADD COLUMN template text;
ALTER TABLE apps ADD COLUMN template_version integer;
ALTER TABLE apps ADD CHECK ((template IS NULL) = (template_version IS NULL));
ALTER TABLE apps ADD CHECK (template IS NULL OR kind = 'compose');
