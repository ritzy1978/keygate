-- Members who were team admins before 20261028_member_keys.up.sql stay
-- members: which seats were admins is not kept, so that change is not
-- undone here.
DROP INDEX IF EXISTS idx_floating_sessions_seat;
ALTER TABLE floating_sessions DROP COLUMN IF EXISTS seat_id;
ALTER TABLE activations DROP COLUMN IF EXISTS predates_member_keys;
DROP INDEX IF EXISTS idx_activations_seat;
ALTER TABLE activations DROP COLUMN IF EXISTS seat_id;
DROP INDEX IF EXISTS idx_seats_member_key_hash;
ALTER TABLE seats DROP COLUMN IF EXISTS member_key_encrypted;
ALTER TABLE seats DROP COLUMN IF EXISTS member_key_hash;
ALTER TABLE seats DROP COLUMN IF EXISTS member_key;
