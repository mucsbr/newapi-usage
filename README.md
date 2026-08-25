# newapi-usage

Read-only usage dashboard for a running NewAPI database.

It does not proxy traffic and does not modify NewAPI tables. It connects to the existing NewAPI database and shows usage by token/key:

- key name and key tail
- request count
- model list per key
- input tokens (`logs.prompt_tokens`)
- output tokens (`logs.completion_tokens`)
- cache read/write tokens from `logs.other`
- actual charged cost in CNY (`logs.quota`, `QuotaPerUnit`, and `USDExchangeRate`)
- per-model input/output/cache cost breakdown when the log contains ratio billing metadata
- request log list with model, channel, user, IP, and request ID

## Quick Start

```bash
cp .env.example .env
vim .env
docker compose up -d --build
```

Open:

```text
http://your-server-ip:8080
```

## Mainland China Build

The Docker build uses China-mainland friendly defaults:

- `APK_MIRROR=https://mirrors.aliyun.com/alpine`
- `GOPROXY=https://goproxy.cn,direct`
- `GOSUMDB=sum.golang.google.cn`

They can be changed in `.env`, or passed directly:

```bash
docker compose build \
  --build-arg APK_MIRROR=https://mirrors.aliyun.com/alpine \
  --build-arg GOPROXY=https://goproxy.cn,direct \
  --build-arg GOSUMDB=sum.golang.google.cn
```

To use official upstream sources instead:

```env
APK_MIRROR=
GOPROXY=https://proxy.golang.org,direct
GOSUMDB=sum.golang.org
```

## Configuration

Use `SQL_DSN` to point at the same database used by NewAPI.

PostgreSQL:

```env
SQL_DSN=postgresql://root:123456@postgres:5432/new-api?sslmode=disable
DB_DRIVER=postgres
NEWAPI_NETWORK=new-api_new-api-network
```

MySQL:

```env
SQL_DSN=root:123456@tcp(mysql:3306)/new-api?charset=utf8mb4&parseTime=true
DB_DRIVER=mysql
NEWAPI_NETWORK=new-api_new-api-network
```

SQLite:

```env
SQL_DSN=/data/one-api.db
DB_DRIVER=sqlite
```

For SQLite, mount the database file under `./data` or adjust the `docker-compose.yml` volume.

## Security

`ADMIN_PASSWORD` is required. The dashboard and data APIs require login; `/api/health` remains unauthenticated for Docker health checks.

```env
ADMIN_PASSWORD=replace-with-a-long-random-password
```

`SHOW_FULL_KEYS=false` by default. In this mode the service only displays token ID, token name, and the last 8 characters of the key.

Set `SHOW_FULL_KEYS=true` only on a trusted admin-only network.

## Audit Request Bodies

If OpenResty writes request bodies as JSONL, mount that directory and enable the audit importer:

```env
AUDIT_LOG_DIR=/home/asants/newapi/new-api/audit-logs
AUDIT_LOG_GLOB=/audit-logs/*.jsonl
AUDIT_INDEX_DSN=/var/lib/newapi-usage/audit.db
AUDIT_TIMEZONE=UTC
AUDIT_SCAN_INTERVAL_SECONDS=10
AUDIT_LOOKUP_WINDOW_SECONDS=120
AUDIT_MAX_LINES_PER_SCAN=50000
```

The JSONL record should contain request arrival time. The existing OpenResty Lua script can keep using `time = ngx.localtime()`:

```json
{"time":"2026-06-10 12:34:56","method":"POST","path":"/v1/chat/completions","headers":{"authorization":"Bearer sk-..."},"body":{"model":"gpt-4o","messages":[]}}
```

The importer also accepts Unix seconds/milliseconds and RFC3339 timestamps. For local-time strings from `ngx.localtime()`, parsing uses `AUDIT_TIMEZONE`.

If the JSONL `time` is 8 hours behind China time, OpenResty is writing UTC local time. Keep `TZ=Asia/Shanghai` for this service, but set:

```env
AUDIT_TIMEZONE=UTC
```

If OpenResty writes China local time, set `AUDIT_TIMEZONE=Asia/Shanghai`.

The importer stores an incremental cursor for each JSONL file in SQLite. It scans the glob periodically, imports new files from offset `0`, continues existing files from their last byte offset, and resets the cursor if a file is truncated or replaced.

The SQLite index stores request bodies plus token ID, key tail, key hash, model, request path, and source file position. It does not store the full API key.

Records with `record_type: "security_alert"` are stored separately from request bodies. They do not participate in request matching or create an extra conversation. The importer links each alert to the original OpenResty request by their shared `request_id`, so an alert appended after the response automatically appears on an already matched NewAPI usage log. Existing alert rows imported by older versions are split out in a one-time background migration when the original JSONL source line is still available.

Matching order in the UI:

1. `logs.token_id + (logs.created_at - logs.use_time)` against timestamped audit rows, with the same model ranked first. `AUDIT_LOOKUP_WINDOW_SECONDS` is applied around that estimated request start time.
2. `logs.request_id` to audit `request_id`, only if the audit JSONL explicitly contains a compatible request ID.
3. Latest rows with the same `token_id`, with the same model ranked first.

If the JSONL has no timestamp field, the service can still show same-token candidates, but it cannot uniquely identify the exact request when the same key has concurrent or high-frequency traffic. Add a timestamp such as `time`, `timestamp`, or `created_at` to your existing JSONL writer.

Only request bodies are shown. Model response text is not available unless the OpenResty audit layer also records response bodies.

## Channel Balance Cards

Optional cards can be shown in the "渠道余额" area. Configure only the providers you need.

Sub2API account card:

```env
SUB2API_BASE_URL=http://10.255.200.17:6780
SUB2API_API_KEY=admin-xxxxxxxxxxxxxxxx
SUB2API_LABEL=Sub2API
SUB2API_TIMEZONE=Asia/Shanghai
SUB2API_PAGE_SIZE=50
IKUN_API_BASE=https://api.ikuncode.cc
IKUN_ACCESS_TOKEN=your-ikun-access-token
IKUN_USER_ID=20378
IKUN_SUB2API_ACCOUNT_ID=275
IKUN_SUB2API_ACCOUNT_KEY=ikun
```

The card lists accounts from `/api/v1/admin/accounts`. If account-list metadata includes usage fields, those are shown as estimated remaining percentages. OAuth accounts also show a live refresh button that calls `/api/v1/admin/accounts/{id}/usage`.
When Ikun access is configured, the matching Sub2API account is enriched from `/api/user/self`: `quota` is shown as remaining balance, `used_quota` as used amount, and both quota fields are converted to CNY by dividing by `500000` and formatting to two decimals.

OpenCode Go Manager account card:

```env
OPENCODE_BASE_URL=http://host.docker.internal:9042
OPENCODE_USERNAME=admin
OPENCODE_PASSWORD=your-dashboard-password
OPENCODE_LABEL=OpenCode
```

The card loads the account list and pricing limits, then shows each account's estimated 5-hour, weekly, and monthly remaining USD quota. Enabled managed accounts have a refresh button that replaces the estimated values with live usage from `/dashboard/api/accounts/{id}/usage/refresh`.

XFYun MaaS coding-plan card:

```env
XFYUN_ENABLED=true
XFYUN_API_BASE=https://maas.xfyun.cn
XFYUN_LABEL=讯飞 MaaS
XFYUN_ACCOUNTS_PATH=/var/lib/newapi-usage/xfyun-accounts.json
XFYUN_PAGE_SIZE=20
```

The card stores one `ssoSessionId` as one account in `XFYUN_ACCOUNTS_PATH` and never returns the raw cookie value to the browser. Use the card's add/update controls to create or refresh accounts. It reads `/api/v1/gpt-finetune/coding-plan/list` and displays package, 5-hour, weekly, and daily quotas as remaining percentages.

## API

```text
GET /api/health
GET /api/summary?start=1710000000&end=1710086400
GET /api/keys?q=name&limit=100
GET /api/keys/{token_id}/models
GET /api/logs?token_id=123&type=success&page=1&page_size=100
GET /api/logs/{log_id}/audit
GET /api/audit/status
GET /api/channels/balance
GET /api/channels/sub2api/accounts/{account_id}/usage?force=true&timezone=Asia/Shanghai
POST /api/channels/opencode/accounts/{account_id}/usage/refresh
POST /api/channels/xfyun/accounts
PUT /api/channels/xfyun/accounts/{account_id}
DELETE /api/channels/xfyun/accounts/{account_id}
```

Time parameters are Unix timestamps in seconds.

## Self-Service Usage Page

Open `/self.html` and enter a New API key to view only that key's summary, model distribution, and metering logs. The key is matched directly against `tokens.key`, is kept only in the current browser page memory, and is never placed in a URL or returned by the API.

Self-service log responses deliberately omit request bodies, audit conversations, request IDs, IP addresses, user agents, and raw log metadata. The admin-only audit endpoint remains inaccessible without the management session.

```text
POST /api/self/login
GET /api/self/summary
GET /api/self/models
GET /api/self/logs
```

The three GET endpoints accept the key in `Authorization: Bearer sk-...` or `X-API-Key`.

## AI Request Review

Administrators can open `/review.html` to configure an OpenAI-compatible Chat Completions model and create background review jobs for selected keys, request models, and time ranges. After keys and a time range are selected, the page loads only models that actually occur in matching audit requests. The review API key is AES-GCM encrypted in the audit SQLite database; the local encryption key is generated next to the audit database with mode `0600`.

Review jobs compare each request's normalized message sequence with recent conversation branches. Only newly appended messages are sent to the review model, identical deltas reuse cached results, and an earlier risk is inherited while that message remains in the request history. Up to three jobs run concurrently; each job defaults to five parallel ready branches, with a global cap of ten outbound model requests. Requests in the same parent-child conversation chain remain ordered so inherited risk stays correct. The default mode reviews user messages only; user plus tool messages and all-message modes are also available.

The model is asked for a fixed JSON result using Chinese categories: 提示词注入、越狱绕过、凭据窃取、敏感信息泄露、恶意软件、网络攻击、欺诈、隐私侵犯、违法活动、滥用骚扰、其他风险. The client tries strict JSON Schema first, then JSON Object mode, then plain JSON prompting for compatible gateways. Reasoning effort can be omitted or set to `no_think`, `low`, or `high`; automatic mode retries with `no_think` when a provider explicitly requires that field.

```text
GET /api/review/config
PUT /api/review/config
POST /api/review/config/test
GET /api/review/keys
GET /api/review/models
GET /api/review/jobs
POST /api/review/jobs
GET /api/review/jobs/{job_id}
POST /api/review/jobs/{job_id}/pause
POST /api/review/jobs/{job_id}/resume
POST /api/review/jobs/{job_id}/cancel
DELETE /api/review/jobs/{job_id}
GET /api/review/jobs/{job_id}/results
GET /api/review/entries/{audit_entry_id}
```

If the compose network name is different, check it with:

```bash
docker network ls | grep new-api
```
