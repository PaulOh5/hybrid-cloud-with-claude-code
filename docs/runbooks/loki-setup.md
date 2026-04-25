# Runbook — Loki/Promtail setup (deferred)

The services already log JSON-lines to stdout (`slog.NewJSONHandler`), so
shipping them is a matter of stitching Promtail in front of the existing
log files. Phase 10.3 deferred the heavyweight infra; this runbook is the
cookbook when we're ready.

## Goal
- 14-day retention.
- Common labels: `service`, `instance_id`, `user_id`.
- Search from the existing Grafana instance.

## Steps (for the next operator)

### 1. Add Loki + Promtail to the observability compose
`infra/docker-compose.observability.yml` already runs Prometheus + Grafana.
Append:

```yaml
loki:
  image: grafana/loki:3.2.1
  container_name: hybrid-loki
  ports:
    - "3100:3100"
  volumes:
    - ./loki/loki-config.yml:/etc/loki/local-config.yaml:ro
    - loki-data:/loki

promtail:
  image: grafana/promtail:3.2.1
  container_name: hybrid-promtail
  volumes:
    - ./promtail/promtail-config.yml:/etc/promtail/config.yml:ro
    - /home/paul/hybrid-cloud/logs:/var/log/hybrid:ro
  command: -config.file=/etc/promtail/config.yml
```

Add `loki-data:` to the `volumes:` section.

### 2. Write `infra/loki/loki-config.yml`
Standard Loki single-binary config; set retention to 14 d and the storage
backend to filesystem (sufficient for single-host).

### 3. Write `infra/promtail/promtail-config.yml`
- Three scrape jobs: main-api, ssh-proxy, compute-agent.
- Pipeline stages:
  - `json` — parse the slog output.
  - `labels` — promote `service`, `instance_id`, `user_id`.
  - `timestamp` — use the `time` field from slog.

### 4. Provision Grafana
Add Loki data source to `infra/grafana/provisioning/datasources/`. URL is
`http://loki:3100`.

### 5. Verify
- Generate a test event (create + destroy an instance).
- In Grafana → Explore → Loki, query `{service="main-api"} |= "instance"`.
- Filter to a specific instance: `{service="main-api"} | json | instance_id="<id>"`.

## Why this is deferred
Phase 1 ships single-host on h20a. The JSON logs are already searchable
with `journalctl` or `tail | jq`. Loki adds another stateful service to
operate, so it's worth the cost only when:
- multiple hosts ship logs, or
- post-mortems are blocked by inability to correlate across services.
