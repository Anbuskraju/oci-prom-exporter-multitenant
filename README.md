# OCI Prometheus Exporter

This Prometheus exporter collects metrics from Oracle Cloud Infrastructure (OCI) using the Go SDK.

## Features

- Multi-tenancy support using one API key
- Dynamic metric configuration (`metrics.yaml`)
- Subtree compartment traversal
- Prometheus `/metrics` endpoint

## Run

### Native (Local)

```bash
go run main.go
```

### Docker

```bash
docker build -t oci-exporter .
docker run -p 2112:2112 -v ~/.oci:/root/.oci oci-exporter
```

## Configs

- `config/tenants.yaml`: OCI tenancy and compartment config
- `config/metrics.yaml`: Namespaces and metric names to collect
