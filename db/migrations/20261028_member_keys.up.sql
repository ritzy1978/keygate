-- Each team member gets a key of their own. Until now a member used the
-- owner's key, so removing a member left them holding a key that still
-- worked. A member's key is made the first time they open the portal
-- after accepting the invite, and is cleared when they are removed;
-- like a license key it is found by its hash, and is kept the way
-- licenses keep theirs: the plain key, plus its ciphertext when license
-- keys are encrypted.
ALTER TABLE seats ADD COLUMN IF NOT EXISTS member_key TEXT;
ALTER TABLE seats ADD COLUMN IF NOT EXISTS member_key_hash TEXT;
ALTER TABLE seats ADD COLUMN IF NOT EXISTS member_key_encrypted BYTEA;
CREATE UNIQUE INDEX IF NOT EXISTS idx_seats_member_key_hash
    ON seats (member_key_hash) WHERE member_key_hash IS NOT NULL;

-- A team is now its owner, who manages it, and members, who use the
-- license; there are no team admins. An admin until now becomes a
-- member, keeping their seat. The role column and its check stay as
-- they are, should the role come back.
UPDATE seats SET role = 'member' WHERE role = 'admin';

-- Whose device an activation is, and whose machine a floating session
-- is: the member whose key made it, or NULL for the license's key.
-- Removing the member removes both. A device stays whoever's activated
-- it. Everything existing was made with the license's key, so every row
-- starts NULL: the foreign keys are
-- added NOT VALID, as there is nothing to check, which saves a scan
-- under the lock this file holds; new rows are checked all the same.
-- The partial indexes, which removing a member deletes by, still read
-- each table once while it is locked.
ALTER TABLE activations ADD COLUMN IF NOT EXISTS seat_id TEXT;
ALTER TABLE activations DROP CONSTRAINT IF EXISTS activations_seat_id_fkey;
ALTER TABLE activations ADD CONSTRAINT activations_seat_id_fkey
    FOREIGN KEY (seat_id) REFERENCES seats(id) ON DELETE CASCADE NOT VALID;
CREATE INDEX IF NOT EXISTS idx_activations_seat ON activations (seat_id) WHERE seat_id IS NOT NULL;

-- Until now members used the license's key, so a device activated with
-- it may well be a member's machine. Each such device may move, once,
-- to the member whose own key activates it; one activated with the
-- license's key from now on is the owner's and stays so. Existing rows
-- take true and new ones false: a constant default added with the
-- column fills existing rows without rewriting the table.
ALTER TABLE activations ADD COLUMN IF NOT EXISTS predates_member_keys BOOLEAN NOT NULL DEFAULT true;
ALTER TABLE activations ALTER COLUMN predates_member_keys SET DEFAULT false;

ALTER TABLE floating_sessions ADD COLUMN IF NOT EXISTS seat_id TEXT;
ALTER TABLE floating_sessions DROP CONSTRAINT IF EXISTS floating_sessions_seat_id_fkey;
ALTER TABLE floating_sessions ADD CONSTRAINT floating_sessions_seat_id_fkey
    FOREIGN KEY (seat_id) REFERENCES seats(id) ON DELETE CASCADE NOT VALID;
CREATE INDEX IF NOT EXISTS idx_floating_sessions_seat ON floating_sessions (seat_id) WHERE seat_id IS NOT NULL;
