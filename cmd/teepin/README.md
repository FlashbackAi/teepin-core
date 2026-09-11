# TEEPIN CLI

Command-line interface for TEEPIN GPU infrastructure.

## 🚀 Quick Start

### Build from Source

```bash
# From teepin-core directory
cd cmd/teepin
go build -o teepin

# Install globally
go install

# Or build for all platforms
GOOS=linux GOARCH=amd64 go build -o teepin-linux
GOOS=darwin GOARCH=amd64 go build -o teepin-darwin
GOOS=windows GOARCH=amd64 go build -o teepin.exe
```

### First-Time Setup

```bash
# 1. Initialize configuration
./teepin init

# 2. Login (for now, just creates a placeholder)
./teepin login --api-key dummy-key

# 3. Check version
./teepin version
```

## 📝 Available Commands

### Core Commands

```bash
# Initialize config
teepin init

# Authenticate
teepin login

# Deploy an instance
teepin deploy \
  --image pytorch/pytorch:latest \
  --gpu-vram 25GB \
  --name my-instance

# List instances
teepin list

# View logs
teepin logs inst-a82e7f3

# Show version
teepin version
```

### Object Storage (Teepin S3)

```bash
# Buckets
teepin storage buckets create my-bucket
teepin storage buckets list
teepin storage buckets delete my-bucket

# Objects
teepin storage objects put my-bucket photos/cat.jpg ./cat.jpg
teepin storage objects list my-bucket --prefix photos/
teepin storage objects get my-bucket photos/cat.jpg --outfile cat.jpg
teepin storage objects delete my-bucket photos/cat.jpg

# Mint a short-lived signed link (safe to hand to an end user/browser —
# it carries no API key)
teepin storage objects url my-bucket photos/cat.jpg --ttl 300 --disposition attachment
```

Every `storage` command except `objects url` sends your API key on the request — run these from a machine you control, never from anywhere a browser or mobile client could read the credential.

## 🔧 Configuration

Config file location: `~/.teepin/config.yaml`

```yaml
api_url: http://localhost:8080
default_region: us-west-1
output_format: table
```

Credentials location: `~/.teepin/credentials`

## 🎯 Testing with Local API

Make sure the API server is running:

```bash
# In another terminal
cd teepin-core
kubectl port-forward -n teepin-system svc/teepin-api 8080:80
```

Then use the CLI:

```bash
# Deploy an instance
./teepin deploy --image nginx:latest --gpu-vram 25GB

# List instances
./teepin list

# Expected output:
# ID             NAME              STATUS    GPU VRAM  CREATED
# --             ----              ------    --------  -------
# inst-a82e7f3   instance-123456   Pending   25GB      2026-06-26 10:23
```

## 📚 Examples

### Basic Deployment

```bash
teepin deploy --image pytorch/pytorch:latest --gpu-vram 25GB
```

### Full Configuration

```bash
teepin deploy \
  --image pytorch/pytorch:latest \
  --name pytorch-training \
  --gpu-vram 25GB \
  --cpu 4 \
  --memory 16GB \
  --env EPOCHS=100 \
  --env BATCH_SIZE=32 \
  --port 8888:8888
```

### Detached Mode (Scripting)

```bash
# Get instance ID for scripting
INSTANCE_ID=$(teepin deploy --image nginx:latest --gpu-vram 10GB -d)
echo "Created instance: $INSTANCE_ID"
```

### JSON Output

```bash
# List instances in JSON format
teepin list -o json | jq '.instances[].id'
```

## 🐛 Troubleshooting

### "Error connecting to API"

Make sure the API server is running and accessible:

```bash
curl http://localhost:8080/health
```

### "Failed to create instance"

Check the API server logs:

```bash
kubectl logs -n teepin-system -l app=teepin-api
```

## 🚧 Coming Soon

Commands not yet implemented:
- `teepin describe INSTANCE_ID` - Detailed instance info
- `teepin stop/start/restart` - Instance lifecycle
- `teepin delete INSTANCE_ID` - Delete instance
- `teepin exec INSTANCE_ID` - SSH into instance
- `teepin types` - List instance types
- `teepin regions` - List available regions
- `teepin billing` - View billing info

See [CLI-SDK-SPEC.md](../../CLI-SDK-SPEC.md) for the full spec.
