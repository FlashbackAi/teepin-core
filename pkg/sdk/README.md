# TEEPIN Go SDK

Official Go client library for the TEEPIN API.

## Installation

```bash
go get github.com/teepin/teepin-core/pkg/sdk
```

## Quick Start

```go
package main

import (
    "context"
    "fmt"
    "log"

    "github.com/teepin/teepin-core/pkg/sdk"
)

func main() {
    // Create client
    client := sdk.NewClient(sdk.Config{
        BaseURL: "http://localhost:8080",
        APIKey:  "your-api-key", // Optional for now
    })

    ctx := context.Background()

    // Deploy an instance
    instance, err := client.Instances.Create(ctx, &sdk.CreateInstanceRequest{
        Name:     "pytorch-training",
        Image:    "pytorch/pytorch:latest",
        GPUVRAM:  "25GB",
        CPUUnits: 4,
        Memory:   "16GB",
        Env: map[string]string{
            "EPOCHS":     "100",
            "BATCH_SIZE": "32",
        },
    })
    if err != nil {
        log.Fatal(err)
    }

    fmt.Printf("Instance created: %s\n", instance.ID)
    fmt.Printf("Status: %s\n", instance.Status)
}
```

## Features

- ✅ **Type-safe** - Full type definitions for all API objects
- ✅ **Context-aware** - All operations accept `context.Context`
- ✅ **Error handling** - Rich error types with helper functions
- ✅ **Idiomatic Go** - Follows Go best practices
- ✅ **Well-documented** - Comprehensive documentation and examples

## Usage Examples

### List Instances

```go
instances, err := client.Instances.List(ctx)
if err != nil {
    log.Fatal(err)
}

for _, inst := range instances {
    fmt.Printf("%s: %s (%s)\n", inst.ID, inst.Name, inst.Status)
}
```

### Get Instance Details

```go
instance, err := client.Instances.Get(ctx, "inst-a82e7f3")
if err != nil {
    if sdk.IsNotFound(err) {
        fmt.Println("Instance not found")
    } else {
        log.Fatal(err)
    }
}

fmt.Printf("Instance: %s\n", instance.Name)
fmt.Printf("Status: %s\n", instance.Status)
fmt.Printf("GPU VRAM: %s\n", instance.GPUVRAM)
```

### Delete Instance

```go
err := client.Instances.Delete(ctx, "inst-a82e7f3")
if err != nil {
    log.Fatal(err)
}

fmt.Println("Instance deleted")
```

### List Instance Types

```go
types, err := client.Instances.ListTypes(ctx)
if err != nil {
    log.Fatal(err)
}

for _, t := range types {
    fmt.Printf("%s: %s - $%.2f/hr\n", t.Name, t.GPUVRAM, t.PricePerHour)
}
```

### Deploy with Port Mapping

```go
instance, err := client.Instances.Create(ctx, &sdk.CreateInstanceRequest{
    Name:     "web-app",
    Image:    "nginx:latest",
    GPUVRAM:  "10GB",
    CPUUnits: 2,
    Memory:   "8GB",
    Ports: []sdk.PortMapping{
        {Container: 80, Public: 8080, Protocol: "tcp"},
        {Container: 443, Public: 8443, Protocol: "tcp"},
    },
})
```

### Deploy with Labels

```go
instance, err := client.Instances.Create(ctx, &sdk.CreateInstanceRequest{
    Name:     "training-job",
    Image:    "pytorch/pytorch:latest",
    GPUVRAM:  "40GB",
    CPUUnits: 8,
    Memory:   "32GB",
    Labels: map[string]string{
        "team":        "ml-research",
        "project":     "image-classification",
        "environment": "production",
    },
})
```

### Custom HTTP Client

```go
import "net/http"

httpClient := &http.Client{
    Timeout: 60 * time.Second,
    Transport: &http.Transport{
        MaxIdleConns:        100,
        MaxIdleConnsPerHost: 10,
    },
}

client := sdk.NewClient(sdk.Config{
    BaseURL:    "https://api.teepin.cloud",
    APIKey:     "your-api-key",
    HTTPClient: httpClient,
})
```

## Error Handling

The SDK provides helper functions to check error types:

```go
err := client.Instances.Get(ctx, "inst-a82e7f3")
if err != nil {
    switch {
    case sdk.IsNotFound(err):
        fmt.Println("Instance not found")
    case sdk.IsBadRequest(err):
        fmt.Println("Invalid request parameters")
    case sdk.IsConflict(err):
        fmt.Println("Resource conflict")
    default:
        log.Fatal(err)
    }
}
```

## Configuration

### Environment Variables

```bash
export TEEPIN_API_URL=http://localhost:8080
export TEEPIN_API_KEY=your-api-key
```

### Config File

The SDK automatically reads from `~/.teepin/config.yaml`:

```yaml
api_url: http://localhost:8080
default_region: us-west-1
```

## Advanced Usage

### Concurrent Deployments

```go
package main

import (
    "context"
    "fmt"
    "sync"

    "github.com/teepin/teepin-core/pkg/sdk"
)

func main() {
    client := sdk.NewClient(sdk.Config{
        BaseURL: "http://localhost:8080",
    })

    ctx := context.Background()

    // Deploy 10 instances concurrently
    var wg sync.WaitGroup
    for i := 0; i < 10; i++ {
        wg.Add(1)
        go func(index int) {
            defer wg.Done()

            instance, err := client.Instances.Create(ctx, &sdk.CreateInstanceRequest{
                Name:     fmt.Sprintf("worker-%d", index),
                Image:    "pytorch/pytorch:latest",
                GPUVRAM:  "10GB",
                CPUUnits: 4,
                Memory:   "16GB",
            })
            if err != nil {
                fmt.Printf("Failed to create worker-%d: %v\n", index, err)
                return
            }

            fmt.Printf("Created %s\n", instance.ID)
        }(i)
    }

    wg.Wait()
    fmt.Println("All workers deployed")
}
```

### Retries and Timeouts

```go
import (
    "context"
    "time"
)

// Create context with timeout
ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()

// Retry logic
var instance *sdk.Instance
var err error
for i := 0; i < 3; i++ {
    instance, err = client.Instances.Create(ctx, &sdk.CreateInstanceRequest{
        Name:     "my-instance",
        Image:    "nginx:latest",
        GPUVRAM:  "25GB",
        CPUUnits: 4,
        Memory:   "16GB",
    })
    if err == nil {
        break
    }
    time.Sleep(time.Second * time.Duration(i+1))
}
```

## API Reference

### Client

```go
type Client struct {
    Instances *InstancesService
}

func NewClient(config Config) *Client
```

### InstancesService

```go
type InstancesService struct{}

// Create creates a new instance
func (s *InstancesService) Create(ctx context.Context, req *CreateInstanceRequest) (*Instance, error)

// List returns all instances
func (s *InstancesService) List(ctx context.Context) ([]Instance, error)

// Get returns a specific instance
func (s *InstancesService) Get(ctx context.Context, instanceID string) (*Instance, error)

// Delete deletes an instance
func (s *InstancesService) Delete(ctx context.Context, instanceID string) error

// ListTypes returns available instance types
func (s *InstancesService) ListTypes(ctx context.Context) ([]InstanceType, error)

// Metrics returns instance metrics (coming soon)
func (s *InstancesService) Metrics(ctx context.Context, instanceID string) (map[string]interface{}, error)

// Logs streams instance logs (coming soon)
func (s *InstancesService) Logs(ctx context.Context, instanceID string, follow bool) (<-chan string, error)
```

## Contributing

See [CONTRIBUTING.md](../../CONTRIBUTING.md) for details.

## License

Apache License 2.0
