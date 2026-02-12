# Deployment Engine UMC

The Deployment Engine is a UA-Managed Component (UMC) that handles deployment lifecycle management for Underleaf applications.

## Architecture

The Deployment Engine is extracted from the central UA kernel and runs as an independent UMC, communicating with the kernel via the syscall API. This provides:

- **Clean separation**: Deployment logic isolated from kernel
- **Scalability**: Can run multiple instances for distributed deployments
- **Flexibility**: Easy to upgrade or replace without touching kernel
- **Testability**: Can be tested in isolation

## Features

- **Compilation**: Convert deployment specs to execution graphs
- **Planning**: Generate execution plans from graphs
- **Reconciliation**: Execute plans and reconcile state
- **Container Management**: Direct Docker API access for container operations
- **Event Publishing**: Publish deployment events via kernel EventService syscall

## Getting Started

### Build

```bash
make build
```

### Run

```bash
make run
```

The engine communicates with the UA kernel via Unix domain socket at `/tmp/ua_kernel.sock`.

### Configuration

Create a `config.yaml`:

```yaml
kernel:
  socket: /tmp/ua_kernel.sock
  timeout: 30s
  
docker:
  socket: unix:///var/run/docker.sock
  
engine:
  max_workers: 10
  reconciliation_interval: 30s
```

## Syscalls Used

The Deployment Engine uses the following UA kernel syscalls:

- **EventService.EmitEvent**: Publish deployment events
- **EventService.SubscribeLocal**: Receive events (optional, for event-driven reconciliation)
- **ClusterService.GetKV**: Retrieve deployment state from cluster KV
- **ClusterService.PutKV**: Store deployment state

## Package Structure

```
├── cmd/serve/
│   └── main.go          # Launcher/Runtime entry point
├── internal/
│   ├── deployment/      # Handler orchestration
│   ├── compiler/        # Spec → Graph compilation
│   ├── runner/          # Graph execution via Docker
│   ├── recipe/          # Recipe resolution
│   └── syscall/         # Kernel syscall clients
└── README.md
```

## Development

### Dependencies

- Go 1.24+
- Docker (for testing container operations)
- UA Kernel running with syscall API

### Testing

```bash
make test
```

## Integration with UA

The UA agent supervises the Deployment Engine via config:

```yaml
managed_components:
  - name: deployment_engine
    binary_path: /usr/local/bin/deployment_engine
    health_endpoint: http://localhost:8080/health
    restart_policy: always
    granted_syscalls:
      - EventService
      - ClusterService
```

When the UA kernel receives deployment requests:
1. Routes request to MMA (Mycelium Mesh Agent)
2. MMA calls deployment_engine HTTP API
3. deployment_engine uses EventService syscall to publish events back to UA
4. UA forwards events to clients as needed

## Future Enhancements

- [ ] gRPC API for MMA integration (instead of HTTP)
- [ ] Helm chart deployment support
- [ ] Terraform state management
- [ ] GitOps integration
- [ ] Multi-cloud deployment
