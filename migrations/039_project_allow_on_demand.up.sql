-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- allow_on_demand is a per-project policy toggle: whether this project's
-- workloads may land on on-demand (home-node) capacity at all, as
-- opposed to reserved (datacenter) capacity only. Shared by both the
-- regular compute create flow (CreateInstance's home-placement branch,
-- pkg/api/server.go) and Kumbha's deploy flow (which creates instances
-- through that same handler) — one setting, not two, since both are the
-- same underlying placement decision.
--
-- Defaults to TRUE: today there is zero real reserved/datacenter
-- capacity in any deployment of this platform, so a FALSE default would
-- make every instance create fail immediately for every existing
-- project. This flips the default only once real reserved capacity
-- exists and a customer deliberately restricts themselves to it.
ALTER TABLE auth.projects
    ADD COLUMN allow_on_demand BOOLEAN NOT NULL DEFAULT TRUE;
