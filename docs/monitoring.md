# Monitoring

nvweaver serves metrics at `GET /metrics`, in Prometheus's text format, on the same address as the API. Prometheus scrapes it
directly. OpenTelemetry users scrape it with the OpenTelemetry Collector's `prometheus` receiver, and send it on from there:
nvweaver doesn't push OTLP itself, as that would need the OpenTelemetry SDK as a dependency (decision 47).

It's authenticated like the rest of the API. **Give the monitoring system a scoped token of its own,** which owns nothing, so it
can read everything and change nothing:

```sh
nvweaver token new monitoring --token-file monitoring.token | sudo tee -a /etc/nvweaver/tokens
```

Each scrape asks every pool how full it is, which on LVM runs `lvs`, as `GET /v1/pools` does. Every 30 seconds is plenty.

## Metrics

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `nvweaver_info` | gauge | `version`, `backend` | Always 1. Which version is running, on which target: `nvmet` or `spdk`. |
| `nvweaver_target_ready` | gauge | | 1 if the target can serve hosts. |
| `nvweaver_objects` | gauge | `kind` | How many pools, volumes, snapshots, subsystems, namespaces, hosts and ports there are. |
| `nvweaver_warnings` | gauge | | Warnings about the service and its pools, which `GET /v1/system` and `GET /v1/pools` list. |
| `nvweaver_intents_pending` | gauge | | Storage changes started but not finished. Normally 0, or briefly more. Lasting means one is stuck, and the next start finishes it. |
| `nvweaver_pool_up` | gauge | `pool`, `type` | 1 if the pool's storage can be found. |
| `nvweaver_pool_size_bytes` | gauge | `pool` | The pool's size. |
| `nvweaver_pool_used_bytes` | gauge | `pool` | What's really allocated. **A full thin pool gives hosts I/O errors.** |
| `nvweaver_pool_provisioned_bytes` | gauge | `pool` | The sum of its volumes' sizes, which thin storage lets exceed its size. |
| `nvweaver_pool_used_ratio` | gauge | `pool` | How full it is, from 0 to 1. |
| `nvweaver_pool_metadata_used_ratio` | gauge | `pool` | LVM thin pools only: how full its metadata is, from 0 to 1. **Full metadata stops the pool.** |
| `nvweaver_pool_warning_level_ratio` | gauge | `pool` | The pool's `warning_percent`, from 0 to 1, so alerts can follow what's set on each pool. |
| `nvweaver_pool_warnings` | gauge | `pool` | Warnings about the pool, which `GET /v1/pools/{pool}` lists. |
| `nvweaver_pool_volumes`, `nvweaver_pool_snapshots` | gauge | `pool` | Volumes and snapshots in the pool. |
| `nvweaver_http_requests_total` | counter | `method`, `route`, `code` | API requests. `route` is the pattern, such as `/v1/volumes/{volume}`, not the name asked for. |
| `nvweaver_http_request_duration_seconds` | histogram | `method`, `route` | How long they took, up to 300 s, as a clone can take minutes. |

Request counts start again from 0 when the service restarts, as Prometheus expects of a counter.

## Prometheus

```yaml
scrape_configs:
  - job_name: nvweaver
    scrape_interval: 30s
    scheme: https
    authorization:
      credentials_file: /etc/prometheus/nvweaver.token
    tls_config:
      ca_file: /etc/prometheus/nvweaver-ca.crt
    static_configs:
      - targets: ["storage.example:8420"]
```

## OpenTelemetry Collector

```yaml
receivers:
  prometheus:
    config:
      scrape_configs:
        - job_name: nvweaver
          scrape_interval: 30s
          scheme: https
          authorization:
            credentials_file: /etc/otelcol/nvweaver.token
          tls_config:
            ca_file: /etc/otelcol/nvweaver-ca.crt
          static_configs:
            - targets: ["storage.example:8420"]
exporters:
  otlphttp:
    endpoint: https://otel.example:4318
service:
  pipelines:
    metrics:
      receivers: [prometheus]
      exporters: [otlphttp]
```

## Alerts

Examples, in Prometheus's rule format. The first two matter most: a thin pool that fills up, in data or metadata, gives every
host using it I/O errors.

```yaml
groups:
  - name: nvweaver
    rules:
      - alert: NvweaverPoolFilling
        expr: nvweaver_pool_used_ratio >= on(pool) nvweaver_pool_warning_level_ratio and on(pool) nvweaver_pool_warning_level_ratio > 0
        for: 5m
        annotations:
          summary: "Pool {{ $labels.pool }} is {{ $value | humanizePercentage }} full"
      - alert: NvweaverPoolMetadataFilling
        expr: nvweaver_pool_metadata_used_ratio > 0.8
        for: 5m
        annotations:
          summary: "Pool {{ $labels.pool }}'s metadata is {{ $value | humanizePercentage }} full"
      - alert: NvweaverPoolMissing
        expr: nvweaver_pool_up == 0
        for: 2m
      - alert: NvweaverTargetNotReady
        expr: nvweaver_target_ready == 0
        for: 2m
      - alert: NvweaverDown
        expr: up{job="nvweaver"} == 0
        for: 2m
      - alert: NvweaverChangeStuck
        expr: nvweaver_intents_pending > 0
        for: 15m
      - alert: NvweaverWarnings
        expr: nvweaver_warnings > 0
        for: 15m
        annotations:
          summary: "nvweaver has warnings: see GET /v1/system and GET /v1/pools"
      - alert: NvweaverAPIErrors
        expr: sum(rate(nvweaver_http_requests_total{code=~"5.."}[5m])) > 0
        for: 10m
```

`NvweaverAPIErrors` counts 503s too, which a busy service gives and clients retry, so it waits 10 minutes.
