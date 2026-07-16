# TEEPIN Core

Open-source core of **Teepin Web Services (TWS)** — a cloud services platform built service by service, starting with GPU compute: exact VRAM allocation and transparent pricing for AI workloads.

[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](https://opensource.org/licenses/Apache-2.0)
[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8?logo=go)](https://go.dev/)

## Overview

TEEPIN (Teepin Web Services) is a cloud services platform. Its first service is GPU-accelerated compute, which enables developers to deploy containerized AI/ML workloads with:

- **Exact VRAM Allocation**: Request 25GB, get exactly 25GB (no over-provisioning)
- **NVIDIA MIG Support**: Hardware-isolated GPU slicing for multi-tenancy
- **Time-Slicing**: Custom VRAM allocations for non-standard sizes
- **Transparent Pricing**: $0.10 per GB-hour of GPU VRAM
- **Kubernetes-Native**: Built on industry-standard container orchestration
- **Developer-First**: CLI, SDKs (Go, Python, TypeScript), and REST API

## Features

### Current (v0.1.0)

- ✅ **GPU Instance Management**: Create, list, get, delete GPU instances
- ✅ **Exact VRAM Allocation**: Hybrid MIG + time-slicing for flexible sizing
- ✅ **REST API**: Full HTTP API for programmatic access
- ✅ **CLI Tool**: `teepin` command-line interface
- ✅ **Multi-Language SDKs**: Go, Python (sync/async), TypeScript
- ✅ **Local Development**: Kind-based testing environment

### Coming Soon (v0.2.0)

- 🔲 **Authentication**: User accounts, API keys, multi-tenancy
- 🔲 **Database Persistence**: PostgreSQL for instance state
- 🔲 **Billing & Metering**: Usage tracking and Stripe integration
- 🔲 **Container Registry**: Private Harbor registry per project
- 🔲 **Networking**: LoadBalancer, DNS, SSL certificates
- 🔲 **Web Console**: React-based dashboard

## Quick Start

### Prerequisites

- Docker (v24+)
- kubectl (v1.28+)
- Go (v1.21+)
- Kind (v0.20+) for local development

### Local Development

```bash
# Clone the repository
git clone https://github.com/FlashbackAi/teepin-core.git
cd teepin-core

# Set up local Kubernetes cluster
./scripts/local-setup.sh

# Deploy TEEPIN platform
make deploy-local

# Port-forward to access API
kubectl port-forward -n teepin svc/teepin-api 8080:8080
```

### Using the API

```bash
# Create a GPU instance
curl -X POST http://localhost:8080/v1/compute/instances \
  -H "Content-Type: application/json" \
  -d '{
    "name": "my-instance",
    "image": "pytorch/pytorch:latest",
    "gpu_vram": "25GB",
    "cpu_units": 4,
    "memory": "16GB"
  }'

# List instances
curl http://localhost:8080/v1/compute/instances

# Get instance details
curl http://localhost:8080/v1/compute/instances/inst-abc123

# Delete instance
curl -X DELETE http://localhost:8080/v1/compute/instances/inst-abc123
```

### Using the CLI

```bash
# Install CLI
go install github.com/FlashbackAi/teepin-core/cmd/teepin@latest

# Initialize configuration
teepin init --api-url http://localhost:8080

# Deploy an instance
teepin deploy \
  --name my-instance \
  --image pytorch/pytorch:latest \
  --gpu-vram 25GB \
  --cpu 4 \
  --memory 16GB

# List instances
teepin list

# View logs
teepin logs inst-abc123

# Delete instance
teepin delete inst-abc123
```

### Using the SDKs

**Go SDK**:
```go
import "github.com/FlashbackAi/teepin-go/teepin"

client := teepin.NewClient(teepin.Config{
    BaseURL: "http://localhost:8080",
})

instance, err := client.Instances.Create(ctx, &teepin.CreateInstanceRequest{
    Name:     "my-instance",
    Image:    "pytorch/pytorch:latest",
    GpuVram:  "25GB",
    CpuUnits: 4,
    Memory:   "16GB",
})
```

**Python SDK**:
```python
from teepin import Client

client = Client(base_url="http://localhost:8080")

instance = client.instances.create({
    "name": "my-instance",
    "image": "pytorch/pytorch:latest",
    "gpu_vram": "25GB",
    "cpu_units": 4,
    "memory": "16GB"
})
```

**TypeScript SDK**:
```typescript
import { TeepinClient } from '@teepin/sdk';

const client = new TeepinClient({
  baseUrl: 'http://localhost:8080'
});

const instance = await client.instances.create({
  name: 'my-instance',
  image: 'pytorch/pytorch:latest',
  gpuVram: '25GB',
  cpuUnits: 4,
  memory: '16GB'
});
```

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                     TEEPIN Platform                         │
├─────────────────────────────────────────────────────────────┤
│                                                             │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐    │
│  │   CLI Tool   │  │   REST API   │  │  Web Console │    │
│  │   (teepin)   │  │  (HTTP/JSON) │  │   (React)    │    │
│  └──────┬───────┘  └──────┬───────┘  └──────┬───────┘    │
│         │                  │                  │             │
│         └──────────────────┴──────────────────┘             │
│                            │                                │
│         ┌──────────────────▼──────────────────┐            │
│         │       API Server (Go/Gin)           │            │
│         │  - Instance Management              │            │
│         │  - GPU Allocation                   │            │
│         │  - Authentication (v0.2)            │            │
│         │  - Billing (v0.2)                   │            │
│         └──────────────┬──────────────────────┘            │
│                        │                                    │
│         ┌──────────────▼──────────────────┐                │
│         │    Kubernetes API Server        │                │
│         │  - Pod Scheduling               │                │
│         │  - GPU Device Plugins           │                │
│         │  - Resource Management          │                │
│         └──────────────┬──────────────────┘                │
│                        │                                    │
│         ┌──────────────▼──────────────────┐                │
│         │     Worker Nodes                │                │
│         │  - NVIDIA H100 GPUs (MIG)       │                │
│         │  - Customer Containers          │                │
│         └─────────────────────────────────┘                │
│                                                             │
└─────────────────────────────────────────────────────────────┘
```

## Project Structure

```
teepin-core/
├── cmd/
│   ├── api-server/       # Main API server
│   ├── teepin/           # CLI tool
│   ├── billing-service/  # Billing & metering (v0.2)
│   └── auth-service/     # Authentication (v0.2)
├── pkg/
│   ├── api/              # API handlers
│   ├── auth/             # Authentication logic
│   ├── billing/          # Billing & metering
│   ├── database/         # Database client
│   ├── gpu/              # GPU allocation
│   ├── models/           # Data models
│   ├── networking/       # DNS, SSL, LoadBalancer
│   └── sdk/              # Go SDK
├── deploy/
│   ├── kubernetes/       # K8s manifests
│   └── local/            # Local dev setup
├── migrations/           # Database migrations
├── scripts/              # Setup scripts
└── tests/                # Integration tests
```

## GPU Allocation Strategy

TEEPIN uses a **hybrid allocation approach** for maximum flexibility:

### MIG Profiles (Standard Sizes)
For standard VRAM requests (10GB, 20GB, 40GB, 80GB), TEEPIN uses NVIDIA MIG for hardware-level isolation:

```
Customer requests 20GB → Allocate 20GB MIG slice
Customer requests 40GB → Allocate 40GB MIG slice
```

### Time-Slicing (Custom Sizes)
For custom VRAM requests, TEEPIN uses software-based time-slicing:

```
Customer requests 25GB → Allocate exactly 25GB via time-slicing
Customer requests 15GB → Allocate exactly 15GB via time-slicing
```

**Key Benefits**:
- No over-allocation or waste
- Pay only for what you use
- Flexible sizing (any GB amount)
- Transparent pricing

## Pricing

Simple, transparent pricing based on actual resource allocation:

| Resource      | Unit          | Price       |
|---------------|---------------|-------------|
| GPU VRAM      | per GB-hour   | $0.10       |
| CPU           | per vCPU-hour | $0.05       |
| Memory        | per GB-hour   | $0.01       |

**Example**:
- Instance with 25GB VRAM, 4 vCPUs, 16GB RAM
- Cost: (25 × $0.10) + (4 × $0.05) + (16 × $0.01) = **$2.86/hour**

## Development

### Building

```bash
# Build API server
make build-api

# Build CLI
make build-cli

# Build all
make build
```

### Testing

```bash
# Run unit tests
make test

# Run integration tests
make test-integration

# Run all tests with coverage
make test-coverage
```

### Contributing

We welcome contributions! Please see [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines.

1. Fork the repository
2. Create a feature branch (`git checkout -b feature/amazing-feature`)
3. Commit your changes (`git commit -m 'Add amazing feature'`)
4. Push to the branch (`git push origin feature/amazing-feature`)
5. Open a Pull Request

## Roadmap

### Stage 1: Local Development (Current)
- ✅ API server with instance lifecycle
- ✅ GPU allocation logic
- ✅ CLI tool
- ✅ Multi-language SDKs
- 🔲 Authentication system
- 🔲 Database persistence
- 🔲 Billing & metering

### Stage 2: Production Deployment
- 🔲 Deploy on H100 hardware
- 🔲 Container registry (Harbor)
- 🔲 Networking (DNS, SSL)
- 🔲 Monitoring & alerting
- 🔲 First 3 customers onboarded

### Stage 3: Scale & Enterprise
- 🔲 Multi-node cluster (8x H100)
- 🔲 High availability
- 🔲 Auto-scaling
- 🔲 Web console
- 🔲 27+ customers

See [ROADMAP.md](ROADMAP.md) for detailed timeline.

## License

This project is licensed under the Apache License 2.0 - see the [LICENSE](LICENSE) file for details.

## Support

- **Documentation**: [docs.teepin.cloud](https://docs.teepin.cloud) (coming soon)
- **Issues**: [GitHub Issues](https://github.com/FlashbackAi/teepin-core/issues)
- **Discussions**: [GitHub Discussions](https://github.com/FlashbackAi/teepin-core/discussions)
- **Email**: support@teepin.cloud

## Acknowledgments

- Built on [Kubernetes](https://kubernetes.io/)
- Powered by [NVIDIA GPUs](https://www.nvidia.com/)
- Inspired by [Akash Network](https://akash.network/)

---

**TEEPIN** - GPU Cloud Infrastructure for AI/ML Workloads
