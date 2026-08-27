-- +goose Up
-- Optional self-asserted origin instance for a community (hostname only, e.g.
-- lemmy.world). Native communities carry their own instance domain; bridged
-- communities (Tidepool) carry the platform they were bridged FROM, which the
-- DNS handle (comicstrips.lemmy-world.tdpl.io) cannot express losslessly.
-- Clients render !name@origin. The community consumer validates the value
-- against BridgeTrust / the verified handle before it lands here; NULL means
-- the record carried none (or it was dropped) and readers fall back to the
-- handle-derived display form.
ALTER TABLE communities ADD COLUMN origin TEXT;

-- +goose Down
ALTER TABLE communities DROP COLUMN origin;
