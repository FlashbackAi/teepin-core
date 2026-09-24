-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- A follow-up chat message can carry images and files, same as the
-- initial build prompt (which passes its own attachments straight to the
-- agent pod's launch env, never persisted — see pkg/kumbha/agent.go's
-- LaunchAgent). A queued follow-up has to survive until the agent's own
-- poll loop picks it up, so its attachments need a real column.
--
-- NULL, not an empty array, for a message with none — matches every other
-- optional-JSON column on this platform (nodeservices.config, etc.):
-- absence is a real, distinct state from "explicitly zero attachments."
ALTER TABLE billing.kumbha_messages
    ADD COLUMN attachments JSONB;
