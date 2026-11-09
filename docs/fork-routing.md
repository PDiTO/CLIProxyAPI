# Quota-aware routing fork

This fork preserves upstream `main` and adds `routing.strategy: soonest-reset-first`.
Set it through `/v8/management/config/routing/strategy` or the YAML config. Keep
`routing.session-affinity: true` to retain usable session bindings. The existing
TTL, subagent inheritance, rebind policy, explicit auth pinning and WebSocket
continuation behavior still apply. A new ranking does not move a usable session.

## Selection

The manager first checks provider/model support, disabled state, token validity,
existing cooldowns and request restrictions. Explicit higher priority still wins
among usable accounts. Within that tier, known usable subscription quota sorts
before unknown quota, then by earliest reset and auth ID. WebSocket requests
retain upstream's preference for WebSocket-capable credentials within the tier.

Consumption ordering uses the **longest applicable subscription window**. For
Codex this means the main `rate_limit.primary_window` and `secondary_window`,
using `limit_window_seconds` rather than assuming primary always means five hours.
A weekly window drives ordering when present. An exhausted applicable short or
weekly window blocks use until reset. Codex `allowed: false` or `limit_reached:
true` conservatively exhausts that rate-limit group's returned windows.
Additional pools apply only when their normalized `limit_name` exactly matches
the requested upstream model; a matched named pool replaces the default pool. Code-review limits and credits never rank ordinary
inference. Claude uses `five_hour`, `seven_day`, and applicable `seven_day_opus`
or `seven_day_sonnet` windows. Other Claude usage groups are not interpreted.

Only finite, nonnegative numeric utilization, valid reset timestamps and bounded
window lengths are accepted. Missing, malformed, future-dated observations,
observations older than 15 minutes, elapsed resets and unsupported providers fall
back to deterministic fill-first. An elapsed reset does not establish renewed
quota. A failed refresh retains the previous successful observation until stale.
A malformed partial response cannot replace the last complete observation.
Unsupported provider credentials and API keys remain eligible through the same
fallback. SIWC inference uses a different credential partition, so ChatGPT usage
is not applied to non-Codex SIWC traffic.

## Background refresh

A service-owned worker runs without the dashboard. It probes enabled OAuth
credentials at `https://chatgpt.com/backend-api/wham/usage` for Codex and
`https://api.anthropic.com/api/oauth/usage` for Claude. Original ChatGPT OAuth
credentials authenticate Codex probes, including accounts with nested SIWC.
Existing executor transports supply headers, proxies and TLS behavior.

Every account is due at startup and after five minutes. A 30-second discovery
sweep finds added credentials; oldest attempts go first so never-observed accounts
are not starved by already-ranked accounts. At most four probes run concurrently.
Slow probes can delay refresh; no network deadlines are added. Service shutdown
cancels the worker and its requests. No inference request or status read polls
quota. Probes never acquire or refresh tokens, update auth files, clear cooldowns
or change session bindings. Credential replacement invalidates cached quota, and
an old credential's result cannot become the replacement's observation.

## Authenticated status contract

`GET /v8/management/routing/status?provider=codex&model=gpt-5.4`
uses the existing management key and access restrictions. There is no v0 alias.
The optional `transport` is `http` by default or `websocket`. Codex status predicts
a Codex client's original OAuth partition and identifies this as
`request_profile: "codex"`. The response includes:

```json
{
  "schema_version": 1,
  "strategy": "soonest-reset-first",
  "provider": "codex",
  "model": "gpt-5.4",
  "transport": "http",
  "request_profile": "codex",
  "generated_at": "2026-11-09T04:00:00Z",
  "prediction_scope": "new_session",
  "next_auth_index": "opaque-credential-index",
  "fallback_reason": "",
  "session_affinity": true,
  "refresh_interval_seconds": 300,
  "max_age_seconds": 900,
  "accounts": [
    {
      "auth_index": "opaque-credential-index",
      "priority": 0,
      "eligible": true,
      "skipped_reason": "",
      "rank": 1,
      "quota_state": "fresh",
      "observed_at": "2026-11-09T03:59:00Z",
      "age_seconds": 60,
      "window": {
        "key": "secondary_window",
        "resets_at": "2026-11-10T00:00:00Z",
        "used_percent": 45
      }
    }
  ]
}
```

Indices match the credentials API. Eligible account ranks include lower priority
fallbacks. Skipped accounts have rank zero. Reasons include `disabled`,
`model_unsupported`, `unavailable`, `cooldown` and `quota_exhausted`. Quota states
are `fresh`, `missing`, `stale`, `malformed`, `expired` and `unsupported`.
`refresh_error`, when present, is a fixed diagnostic code, never an upstream body.
`fallback_reason` is `quota_unavailable` when the predicted account has unknown
quota, or `no_eligible_accounts` when the pool cannot serve the request.

Other selectors return `strategy_not_predictable` without a next prediction.
Home or a plugin scheduler reports `home_authoritative` or `plugin_scheduler`
and suppresses prediction because it can override local ordering. This is a
snapshot for a new unbound session, not a report of an actual bound session or
request with special credential restrictions. No session identifier, token,
filename, email address, raw error or upstream body appears in the response.
The dashboard must handle 404 from stock/older backends and must not label a local
sort as the backend's actual selection.

## Offline candidate validation and restart safety

Stock upstream has no equivalent checker. This fork supports both Go flag forms:

```sh
./cli-proxy-api --capabilities
./cli-proxy-api -check-config -config /path/to/candidate.yaml
```

Capabilities are one JSON line with `schema_version: 1`, `config_check: true`,
`routing_status_path`, `routing_strategies`, `version`, `commit` and
`safe_drain: false`. The checker parses v8 and legacy configs, rejects unknown
routing strategies and returns `{"valid":true}` or a nonzero exit code. It never
starts a listener, bootstraps plugins, loads `.env`, reads auth files, acquires or
refreshes credentials, or rewrites config. Errors omit config contents.
It validates local parsing and supported strategy, not upstream availability.

Stage and checksum the candidate first, verify these capabilities, then validate
the proposed config with that candidate before replacing the installed binary or
config. Keep a copy of the working binary outside mise's prune directory.
Rollback to it if startup fails. This backend does **not** implement safe drain.
Shutdown immediately closes active HTTP requests and WebSockets, including idle
sockets that retain continuation state. Defer restart until no connections are
established and no request has arrived for five minutes. Persistent sockets may
defer indefinitely; disconnect/drain needs a separate operator decision.

## Fork sync and releases

`.github/workflows/pd-sync-release.yml` runs every six hours, on main pushes and
by manual dispatch. PRs run the same verification script. Sync merges
`router-for-me/CLIProxyAPI:main`, runs formatting, all tests, the required build
and offline checks, then stages the entire release before pushing the merge.
It never resets or force-pushes custom commits. A failure creates or updates the
fork's failure issue and action summary. The previous published release remains
available. Publication uploads all assets to a draft before marking it latest.
Unchanged commits do not produce duplicate releases. The upstream tag release
workflow is restricted to the upstream repository so fork tags cannot publish
partial upstream-style releases.

A workflow-changing upstream merge requires a repository `SYNC_TOKEN` secret,
with Contents and Workflows write access to **PDiTO/CLIProxyAPI**. Only the push
step receives it. Other publishing uses the built-in repository token. Without
that secret a workflow-changing push fails visibly and keeps the last release.
Create/renew the secret through GitHub's secret UI; never commit it.

Tags are `pd-v<upstream-version>-<UTC YYYYMMDDHHMMSS>-<12-char fork SHA>`.
The timestamp precedes the hash so consecutive builds of one upstream version
sort chronologically. Assets are:

- `CLIProxyAPI_<full-tag>_linux_amd64.tar.gz`
- `checksums.txt`
- `routing-capabilities.json`
- `release-metadata.json`

The archive contains `cli-proxy-api`, `config.example.yaml`, both upstream
READMEs, LICENSE and the two metadata files. Metadata preserves the upstream
repository, base tag, upstream commit, fork commit and build time. The Linux
amd64 build uses CGO disabled for portability; dynamic shared-library plugins
are unavailable. Other upstream executors and WebSocket support remain built in.

Use only `github:PDiTO/CLIProxyAPI` in mise, with `version_prefix = "pd-v"`,
`version_regex = "^pd-v[0-9]+\\.[0-9]+\\.[0-9]+-[0-9]{14}-[0-9a-f]+$"` and
`asset_pattern = "CLIProxyAPI_*_linux_amd64.tar.gz"`. The GitHub backend supports
these options and uses explicit asset patterns without platform autodetection.
Never fall back to the upstream binary when a fork update fails. Fleet owns
minimum release age, staging, rollback, restart deferral and deployment.

The verification gate also includes two small upstream fixes discovered while
running it: Claude setup-token flags now use the metadata writers' existing lock,
and the Home heartbeat test allows failure detection plus the retry backoff.
These do not change the Home protocol or require a Home repository update.
