-- Billing schema: usage tracking, invoices
CREATE SCHEMA IF NOT EXISTS billing;

-- Usage records (metering)
CREATE TABLE billing.usage_records (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    project_id UUID NOT NULL REFERENCES auth.projects(id) ON DELETE CASCADE,
    instance_id VARCHAR(50) NOT NULL REFERENCES compute.instances(id) ON DELETE CASCADE,
    resource_type VARCHAR(100) NOT NULL,
    quantity DECIMAL(10, 4) NOT NULL,
    unit VARCHAR(50) NOT NULL,
    unit_price DECIMAL(10, 4) NOT NULL,
    total_cost DECIMAL(10, 4) NOT NULL,
    start_time TIMESTAMP WITH TIME ZONE NOT NULL,
    end_time TIMESTAMP WITH TIME ZONE NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_usage_project ON billing.usage_records(project_id);
CREATE INDEX idx_usage_instance ON billing.usage_records(instance_id);
CREATE INDEX idx_usage_start_time ON billing.usage_records(start_time);
CREATE INDEX idx_usage_created_at ON billing.usage_records(created_at);

-- Invoices
CREATE TABLE billing.invoices (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    project_id UUID NOT NULL REFERENCES auth.projects(id) ON DELETE CASCADE,
    invoice_number VARCHAR(50) UNIQUE NOT NULL,
    period_start DATE NOT NULL,
    period_end DATE NOT NULL,
    subtotal DECIMAL(10, 2) NOT NULL,
    tax DECIMAL(10, 2) DEFAULT 0,
    total DECIMAL(10, 2) NOT NULL,
    status VARCHAR(50) NOT NULL,
    stripe_invoice_id VARCHAR(255),
    paid_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_invoices_project ON billing.invoices(project_id);
CREATE INDEX idx_invoices_period ON billing.invoices(period_start, period_end);
CREATE INDEX idx_invoices_status ON billing.invoices(status);
CREATE INDEX idx_invoices_invoice_number ON billing.invoices(invoice_number);

-- Payment methods (Stripe)
CREATE TABLE billing.payment_methods (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    project_id UUID NOT NULL REFERENCES auth.projects(id) ON DELETE CASCADE,
    stripe_customer_id VARCHAR(255) NOT NULL,
    stripe_payment_method_id VARCHAR(255) NOT NULL,
    type VARCHAR(50) NOT NULL,
    last4 VARCHAR(4),
    brand VARCHAR(50),
    exp_month INTEGER,
    exp_year INTEGER,
    is_default BOOLEAN DEFAULT FALSE,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

CREATE INDEX idx_payment_methods_project ON billing.payment_methods(project_id);
CREATE INDEX idx_payment_methods_stripe_customer ON billing.payment_methods(stripe_customer_id);

-- Triggers for updated_at
CREATE TRIGGER update_invoices_updated_at BEFORE UPDATE ON billing.invoices
    FOR EACH ROW EXECUTE FUNCTION auth.update_updated_at_column();

CREATE TRIGGER update_payment_methods_updated_at BEFORE UPDATE ON billing.payment_methods
    FOR EACH ROW EXECUTE FUNCTION auth.update_updated_at_column();
