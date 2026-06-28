-- Registry credentials table for Harbor integration

CREATE TABLE IF NOT EXISTS auth.registry_credentials (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id UUID NOT NULL REFERENCES auth.projects(id) ON DELETE CASCADE,
    harbor_project_name VARCHAR(255) NOT NULL UNIQUE,
    robot_account_id VARCHAR(255) NOT NULL,
    robot_account_name VARCHAR(255) NOT NULL,
    docker_config_json TEXT NOT NULL, -- Encrypted robot account token
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    revoked_at TIMESTAMP WITH TIME ZONE
);

CREATE INDEX idx_registry_credentials_project ON auth.registry_credentials(project_id) WHERE revoked_at IS NULL;
CREATE INDEX idx_registry_credentials_harbor_project ON auth.registry_credentials(harbor_project_name);

-- Trigger for updated_at
CREATE TRIGGER update_registry_credentials_updated_at BEFORE UPDATE ON auth.registry_credentials
    FOR EACH ROW EXECUTE FUNCTION auth.update_updated_at_column();

-- Add comment
COMMENT ON TABLE auth.registry_credentials IS 'Stores Harbor registry credentials for TEEPIN projects';
