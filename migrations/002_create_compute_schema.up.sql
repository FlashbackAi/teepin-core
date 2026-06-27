-- Compute schema: instances, types
CREATE SCHEMA IF NOT EXISTS compute;

-- Instance types (predefined)
CREATE TABLE compute.instance_types (
    id VARCHAR(50) PRIMARY KEY,
    name VARCHAR(255) NOT NULL,
    description TEXT,
    gpu_vram_gb INTEGER,
    cpu_units INTEGER NOT NULL,
    memory_gb INTEGER NOT NULL,
    price_per_hour DECIMAL(10, 4) NOT NULL,
    available BOOLEAN DEFAULT TRUE,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

-- Insert default types
INSERT INTO compute.instance_types (id, name, description, gpu_vram_gb, cpu_units, memory_gb, price_per_hour) VALUES
('gpu.h100.mig-1g', 'H100 MIG 10GB', 'NVIDIA H100 with 10GB VRAM', 10, 4, 16, 1.00),
('gpu.h100.mig-2g', 'H100 MIG 20GB', 'NVIDIA H100 with 20GB VRAM', 20, 8, 32, 2.00),
('gpu.h100.mig-4g', 'H100 MIG 40GB', 'NVIDIA H100 with 40GB VRAM', 40, 16, 64, 4.00),
('gpu.h100.full', 'H100 Full GPU', 'NVIDIA H100 with 80GB VRAM', 80, 32, 128, 8.00),
('cpu.small', 'CPU Small', '2 vCPUs, 4GB RAM', NULL, 2, 4, 0.10),
('cpu.medium', 'CPU Medium', '4 vCPUs, 8GB RAM', NULL, 4, 8, 0.20),
('cpu.large', 'CPU Large', '8 vCPUs, 16GB RAM', NULL, 8, 16, 0.40);

-- Instances (running workloads)
CREATE TABLE compute.instances (
    id VARCHAR(50) PRIMARY KEY,
    project_id UUID NOT NULL REFERENCES auth.projects(id) ON DELETE CASCADE,
    user_id UUID NOT NULL REFERENCES auth.users(id) ON DELETE SET NULL,
    name VARCHAR(255) NOT NULL,
    image VARCHAR(500) NOT NULL,
    instance_type_id VARCHAR(50) REFERENCES compute.instance_types(id),
    status VARCHAR(50) NOT NULL,
    gpu_vram_gb INTEGER,
    cpu_units INTEGER NOT NULL,
    memory_gb INTEGER NOT NULL,
    endpoint VARCHAR(500),
    internal_ip VARCHAR(50),
    k8s_pod_name VARCHAR(255),
    k8s_namespace VARCHAR(255) DEFAULT 'teepin',
    env_vars JSONB,
    ports JSONB,
    labels JSONB,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    started_at TIMESTAMP WITH TIME ZONE,
    terminated_at TIMESTAMP WITH TIME ZONE
);

CREATE INDEX idx_instances_project ON compute.instances(project_id) WHERE terminated_at IS NULL;
CREATE INDEX idx_instances_status ON compute.instances(status);
CREATE INDEX idx_instances_user ON compute.instances(user_id);
CREATE INDEX idx_instances_created_at ON compute.instances(created_at);

-- Trigger for updated_at
CREATE TRIGGER update_instance_types_updated_at BEFORE UPDATE ON compute.instance_types
    FOR EACH ROW EXECUTE FUNCTION auth.update_updated_at_column();

CREATE TRIGGER update_instances_updated_at BEFORE UPDATE ON compute.instances
    FOR EACH ROW EXECUTE FUNCTION auth.update_updated_at_column();
