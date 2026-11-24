# Codex model overrides (fork)

The proxy learns which Codex (ChatGPT account) models exist from two upstream
catalogs at https://github.com/router-for-me/models, refreshed every 3 hours:

- `models.json` lists the models of each plan (`codex-free`, `codex-team`,
  `codex-plus`, `codex-pro`). Requests for models missing from the list fail
  with `unknown provider for model ...`.
- `codex_client_models.json` holds the Codex client templates (service tiers,
  base instructions and so on) behind `/v1/models?client_version=...`.

`models.codex-overrides` fills gaps until upstream catches up. Upstream always
wins: each part of an override steps aside once the catalog provides it, so a
stale entry is harmless.

```yaml
models:
  codex-overrides:
    - id: gpt-6.3-sol            # model upstream does not list yet
      like: gpt-6.2-sol          # clone limits, thinking levels and template
      display-name: "GPT 6.3 Sol" # optional
    - id: gpt-6.2-astra          # existing model missing a speed tier
      add-service-tiers: [ultrafast]
```

Each entry needs `id` and at least one of `like` or `add-service-tiers`. `like`
must differ from `id`, and each `id` may appear only once. Invalid lists are
rejected when the config loads.

## Model entries (`like`)

- **Model list.** If no Codex plan lists `id`, every plan that lists `like` gets
  a copy of `like` under the new ID. The copy uses `display-name`, or a name
  derived from the ID (`gpt-6.3-sol` becomes `GPT 6.3 Sol`). Codex OAuth
  accounts then route `id` to the codex provider and send it upstream
  unchanged. Once any plan lists `id`, the copy is dropped everywhere.
- **Client catalog.** If the templates lack `id`, a copy of the `like` template
  is added with a new `slug` and `display_name`. It is dropped once upstream
  ships a template for `id`.
- If `like` is missing too, that part is inactive. A warning is logged and
  nothing fails.

The two parts are independent: upstream may list the model before it ships the
template, or the reverse.

## Tier entries (`add-service-tiers`)

Each tier the Codex client entry for `id` lacks is added in the shape upstream
uses: `{"id": "ultrafast", "name": "Ultrafast", "description": ""}` is appended to
`service_tiers` and `ultrafast` to `additional_speed_tiers`. Upstream lists
the `priority` service tier as the `fast` speed tier, so `fast` and `priority`
both map to that pair. Tiers upstream already lists are left unchanged. With
`like`, the tiers go on the cloned template.

## When overrides apply

Overrides apply at startup, on every config reload (file watcher or management
API), and after every upstream catalog refresh. A change republishes the Codex
client catalog, which bumps its revision so cached templates rebuild, and
re-registers models for Codex auths. Overrides do not apply in Home mode, which
serves its own catalogs.

## Management API

`GET /v8/management/codex-model-overrides` lists each entry with its state:

```json
{"overrides":[{"id":"gpt-6.3-sol","like":"gpt-6.2-sol","display-name":"GPT 6.3 Sol","add-service-tiers":[],
  "state":{"model":"active","catalog":"active","service-tiers":{}}}]}
```

The states are `active` (the override supplies it), `upstream` (upstream already
provides it, so the override is redundant), `inactive` (it cannot apply, for
example because `like` is missing, or in Home mode) and `none` (not requested).

`PUT /v8/management/codex-model-overrides` takes `{"overrides":[...]}` with the
same entry fields and no `state`. It validates the list, replaces it, saves
`config.yaml` and applies it at once. The response has the same shape as GET.
`{"overrides":[]}` clears the list.
