-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

BEGIN;

-- Down-migrating with an existing NULL user_id row (e.g. a Kumbha-deployed
-- instance created after this migration went up) would fail the NOT NULL
-- backfill outright — that failure is correct: it means real data depends
-- on the column staying nullable, and rolling back would silently need to
-- either delete or misattribute those rows. Surface it, don't guess.
ALTER TABLE compute.instances
    ALTER COLUMN user_id SET NOT NULL;

COMMIT;
