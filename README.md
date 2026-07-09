# gomuks/push
This is a simple push gateway to let the [gomuks backend](https://github.com/tulir/gomuks)
send FCM push notifications to [gomuks android](https://github.com/gomuks/android).

The default instance is at push.gomuks.app, but it's probably possible to self-host by forking
the android app and using your own FCM credentials.

## Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `HOST` | (all interfaces) | Listen host for the push endpoint. |
| `PORT` | `8080` | Listen port for the push endpoint. |
| `FCM_CREDENTIALS_FILE` | — | Path to the Firebase service account JSON. |
| `FCM_PACKAGE_NAME` | — | Android package name FCM messages are restricted to. |
| `METRICS_LISTEN_ADDRESS` | `:9090` | Listen address for the Prometheus endpoint. Set to `-` to disable. |
| `METRICS_OWNER_LABEL` | `false` | Add an `owner` label to push metrics. See the warning below. |

## Metrics

Prometheus metrics are served at `/metrics` on a **separate listener** from the push
endpoint (`:9090` by default), so they are not reachable from wherever the push endpoint
is published. Keep that port on your internal network.

| Metric | Type | Labels |
| --- | --- | --- |
| `gomuks_push_pushes_total` | counter | `result` (`sent`, `invalid_token`, `error`), `priority` (`high`, `normal`) |
| `gomuks_push_pushes_rejected_total` | counter | `reason` (`invalid_json`, `invalid_owner`, `payload_too_large`, `content_too_large`, `not_found`) |
| `gomuks_push_fcm_send_duration_seconds` | histogram | `result` |
| `gomuks_push_push_payload_bytes` | histogram | — |
| `gomuks_push_http_requests_total` | counter | `handler`, `method`, `code` |
| `gomuks_push_http_request_duration_seconds` | histogram | `handler`, `method` |
| `gomuks_push_http_requests_in_flight` | gauge | — |
| `gomuks_push_build_info` | gauge | `revision`, `go_version` |
| `gomuks_push_pushes_by_owner_total` | counter | `owner`, `result` — only when `METRICS_OWNER_LABEL=true` |

Plus the standard Go runtime and process collectors.

### Why source IPs and push tokens are not labels

They are recorded in the structured logs instead, not in metrics:

- A **push token** is a bearer credential for the device it belongs to. Anything holding
  one can send notifications to that device through the FCM project. `/metrics` is
  normally unauthenticated, so a token label would publish every user's push credential.
- **Source IPs** and push tokens are both unbounded in cardinality. One time series per
  IP or per token means Prometheus memory grows without limit as clients come and go.

The access log already records `remote_addr`, `x_forwarded_for` and `user_agent`, and the
FCM result lines record `owner`, `push_token` and `message_id`. Both carry a `request_id`,
so a single request can be reconstructed across the two lines. Query them with Loki or
`jq` against `/var/log/gomuks-push.log` when you need per-identity detail.

`METRICS_OWNER_LABEL=true` adds an `owner` (Matrix user ID) label to
`gomuks_push_pushes_by_owner_total`. The owner set is bounded by the size of the
deployment rather than by the internet, so this is reasonable for a private gateway with
a known user set. **Leave it off for anything public** — it is one time series per user,
and it puts user IDs on an unauthenticated endpoint.

### Grafana

`grafana-dashboard.json` covers every metric above. Import it and pick a Prometheus data
source; the `job` and `instance` variables are populated from `gomuks_push_build_info`.
The "Top owners" panel stays empty unless `METRICS_OWNER_LABEL` is enabled.

## Container images

Images are published to `ghcr.io/<owner>/gomuks-push` on every push to `main` and on
`v*` tags, for `linux/amd64` and `linux/arm64`.

```sh
docker run --rm \
  -e FCM_CREDENTIALS_FILE=/creds.json \
  -e FCM_PACKAGE_NAME=app.gomuks.android \
  -v /path/to/creds.json:/creds.json:ro \
  -p 8080:8080 \
  ghcr.io/<owner>/gomuks-push:latest
```

Note that port 9090 is deliberately not published above; map it only onto a network you
trust.
