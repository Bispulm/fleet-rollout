-- +goose Up

-- A device now starts installing only once the server accepts its request, so an
-- assignment no longer carries a lease that lets it go ahead without contact.
ALTER TABLE assignments DROP COLUMN expires_at;
