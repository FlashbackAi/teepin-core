-- What a model was actually seen to do. One row per catalog model holding the
-- latest capability report from pkg/modelprobe: metadata the backend gave for
-- free (context window, output limit) and, per capability (tools, vision,
-- audio), whether a real test of it passed. The catalog's own supports_*
-- columns stay what an operator DECLARED; this is the evidence, and where it
-- is conclusive it decides what the model may be used for.
CREATE TABLE inference.model_probe_reports (
    model_route TEXT        PRIMARY KEY REFERENCES inference.models(model_route) ON DELETE CASCADE,
    ran_at      TIMESTAMPTZ NOT NULL,
    report      JSONB       NOT NULL
);
