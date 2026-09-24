-- Copyright 2026 TEEPIN Project
-- Licensed under the Apache License, Version 2.0

-- Adds 'tinfoil_confidential' to inference.models' provider check
-- constraint (migration 053) — a hardware-attested confidential-inference
-- enclave reached via the Tinfoil client (pkg/inference.
-- TinfoilConfidentialProvider), not plain TLS. Distinct from
-- 'openai_compatible' even though both speak the OpenAI chat-completions
-- shape on the wire, because this provider's base_url is a bare enclave
-- hostname attested against a fixed, hardcoded repo, not an arbitrary
-- endpoint an operator can point anywhere. See
-- flashback-instructions.md at the repo root ("flashback" is only a
-- codename, ignore it) and [[confidential-inference-hosting-model]].

ALTER TABLE inference.models
    DROP CONSTRAINT IF EXISTS models_provider_check;

ALTER TABLE inference.models
    ADD CONSTRAINT models_provider_check
    CHECK (provider IN ('node', 'anthropic', 'openai_compatible', 'tinfoil_confidential'));
