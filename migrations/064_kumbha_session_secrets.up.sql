-- Secrets a customer enters for a Kumbha build (an API key the app needs, a
-- database URL). The value goes from the customer's browser to the control
-- plane and is stored here SEALED (AES-256-GCM, see pkg/kumbha/secrets.go).
-- The build agent never receives a value: it can ask for a secret by name,
-- and the control plane injects the stored value into the deployed app's
-- environment when the app is created or redeployed.
--
-- One row per (session, name); saving again replaces the value.
CREATE TABLE billing.kumbha_session_secrets (
    session_id UUID        NOT NULL REFERENCES billing.inference_sessions(id) ON DELETE CASCADE,
    name       TEXT        NOT NULL,
    sealed     BYTEA       NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (session_id, name)
);
