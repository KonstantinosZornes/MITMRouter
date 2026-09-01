[Chinese](DESIGN.md)

# MITMRouter Detailed Design (Current Implementation)

> This document describes the actual implementation at the current repository HEAD. It is not a plan for a future version.
>
> MITMRouter is a local HTTP MITM traffic router. A client only needs to point its network proxy at the local machine. When required, the service decrypts HTTPS, identifies an API key or token in the request, derives a sticky identity from either a subscription account or a Marker, and reaches the target through a configured upstream egress.
>
> The current implementation also includes account-map synchronization, account-to-ordinary-proxy bindings, single-file SQLite storage, a Vue 3 admin console, access auditing, and mapping update records.

**Most important constraint:** MITMRouter only changes which egress connection is used. It does not proactively modify the target site's business URL, request headers, request body, response headers, or response body. Request IDs, routing information, and audit information stay inside the server. The normal framing differences imposed by HTTP/1.1, HTTP/2, and tunnel protocols are the exception.

---

## 1. The Big Picture: How a Request Is Routed

### 1.1 Two Kinds of Identity

The word “account” has two meanings in this system:

- **Marker:** a credential temporarily visible in a request, such as `Authorization: Bearer ...`, `x-api-key`, or `api-key`. If it cannot be matched to an account, the Marker itself is used to derive the sticky identity.
- **Subscription account:** a logical account obtained from CLIProxyAPI, Sub2API, or manual registration. It may be an email address, UUID, or caller-defined account name. The request's AT/RT is used only for lookup; after a match, routing is based on the logical account, so token rotation does not change the account's egress identity.

The core decision is:

```text
Receive request
  │
  ├─ Is the target allowed by ACL?
  │    ├─ No: return local 403; do not contact the target
  │    └─ Yes: continue with identity resolution
  │
  ├─ Classify the target Host as a built-in AI platform
  ├─ Extract a credential from headers/query, or from a specific body when needed
  ├─ Look up acct_map using sha256(platform + ":" + normalized credential)
  │    ├─ Hit: obtain platform/account and use account-level stickiness
  │    └─ Miss: use the credential itself as the Marker for legacy hashing
  │
  ├─ Does the mapped account have an egress binding?
  │    ├─ Yes: select a plain egress using sticky/random mode
  │    └─ No: select default_upstream and inject platform session parameters
  │
  └─ Forward directly or through the upstream, then stream the response back unchanged
```

### 1.2 Why MITM Is Required

A normal HTTP proxy can see only the CONNECT tunnel. The TLS content inside the tunnel is encrypted, so the proxy cannot see the target request's `Authorization` header or other credentials. Local TLS termination is therefore required before the router can identify an account and choose a sticky session for it.

The relationship with upstream platforms such as Resin is:

- MITMRouter performs local decryption, identity resolution, and routing;
- Resin, DataImpulse, Decodo, and 1024proxy provide the actual residential/datacenter egress and platform-side stickiness;
- MITMRouter writes its short session identity into the username portion of the upstream credential.

### 1.3 Current Capabilities

- HTTP proxy ingress: absolute-form plaintext requests and HTTPS `CONNECT`;
- SNI-based leaf certificate issuance after CONNECT, with either TLS MITM or blind-tunnel fallback;
- Upstream schemes `http://`, `https://`, `socks5://`, and `socks5h://`;
- Six built-in upstream platforms: DataImpulse, Decodo, 1024proxy, Resin, Generic, and Plain;
- Marker stickiness, subscription-account mapping, and sticky/random account-to-ordinary-proxy bindings;
- CLIProxyAPI/Sub2API API full synchronization; direct CPA file reads and direct Sub2API PostgreSQL incremental reads;
- Streaming passthrough for SSE, 1xx responses, trailers, and HTTP/1.1 `101 Switching Protocols` upgrades;
- SQLite access auditing, synchronization update records, and Prometheus text metrics;
- A Vue 3 admin console with Basic Settings, Upstream Egress, Account Management, Access Audit, and Updates pages.

---

## 2. Scope and Non-Goals

### 2.1 Functional Scope

1. The ingress listener defaults to `127.0.0.1:55666`; the admin console independently defaults to `127.0.0.1:55667`.
2. Inbound Basic authentication can be enabled; failed authentication returns the standard `407 Proxy Authentication Required` response.
3. HTTPS targets are MITM-parsed by default; ACL first decides whether a target may be accessed, then allowed traffic is routed as MITM or a blind tunnel according to its wire form.
4. Known AI Hosts use AT/RT/API-key fingerprints to look up subscription accounts. Unknown or unmapped credentials retain the legacy Marker-hash fallback.
5. Upstream injectors write the sticky identity into upstream credentials; a `plain` upstream does not inject session parameters.
6. All configuration, credential material, account mappings, and audit records that need persistence are stored in the SQLite database under the data directory. Runtime snapshots, failure counters, and event queues remain in memory. There are no YAML, `.env`, or JSON configuration files.
7. Changes made through the admin console become visible to the routing plane through snapshots or reloads without a restart, except that changing TLS paths requires a restart.

### 2.2 Non-Goals

- No SOCKS5 ingress; the local ingress speaks HTTP proxy semantics.
- No UDP, QUIC, or HTTP/3 forwarding. A client using UDP 443 bypasses this service.
- No workaround for certificate-pinning clients.
- No automatic retry of business requests. LLM generation requests may be non-idempotent, so retries remain the client's responsibility.
- No automatic failover across multiple default upstreams. Upstream failures are handled with controlled errors and identity-salt rotation.
- No multi-admin, RBAC, or complex permission model.
- No changes to CPA or Sub2API source code. Direct monitoring reads only a local file directory or PostgreSQL.
- Marker values, AT/RT values, upstream passwords, and complete DSNs are not written to audit records, metrics, or normal logs.

---

## 3. Overall Architecture

### 3.1 Traffic Plane

```text
                          MITMRouter process
┌────────────┐       ┌─────────────────────────────────────────────┐
│ curl / SDK │ HTTP  │ ingress listener :55666                     │
│ AI client  ├──────►│                                               │
└────────────┘       │  CONNECT                                      │
                      │    ├─ inbound authentication                  │
                      │    ├─ ACL deny → local 403                    │
                      │    ├─ ACL allow → hijack after 200 response   │
                      │    ├─ TLS ClientHello → TLS MITM              │
                      │    └─ otherwise → blind tunnel                │
                      │                                               │
                      │  absolute-form plaintext request              │
                      │    └─ authentication → forward                │
                      │                                               │
                      │  MITM/plaintext forward                       │
                      │    ├─ identity.Resolver                       │
                      │    ├─ acctmap.Registry lookup                  │
                      │    ├─ acctegress.Table lookup                  │
                      │    ├─ upstream.Table selection                 │
                      │    ├─ SessionInjector → egress URL             │
                      │    └─ http.Transport.RoundTrip + streaming     │
                      └───────────────────────┬─────────────────────┘
                                              │ HTTP CONNECT / SOCKS5 CONNECT
                                              ▼
                                    upstream egress or target site
```

Ordinary HTTP forwarding uses one shared `http.Transport`. Each request carries the selected egress URL in its context, and `Transport.Proxy` reads it from there. A missing egress URL means a direct connection.

A blind tunnel does not go through the HTTP handler. The router sends CONNECT to an HTTP upstream or SOCKS5 CONNECT to a SOCKS5 upstream, then copies bytes in both directions with two `io.Copy` calls while preserving TCP half-close behavior.

### 3.2 Management Plane

```text
Browser
  │  http(s)://<admin-addr>/ui/
  ▼
admin listener :55667
  ├─ /ui*       → Vue SPA from embed.FS
  ├─ /api/*     → management REST (a session is required except for login)
  └─ /metrics   → Prometheus text (enabled and session-protected)
                         │
                         ▼
              settings / upstreams / marker_salts / acct_map / sync_sources
              acct_egress / access_logs / sync_events / secrets
```

The ingress and admin console are two independent `http.Server` instances on two independent listening addresses:

- The ingress accepts only CONNECT and absolute-form requests. An origin-form browser request receives a static notice and does not expose `/api`, `/ui`, or `/metrics`.
- The admin console accepts only origin-form requests. CONNECT and absolute-form proxy requests receive `404 admin_no_ingress`.

### 3.3 Data and Control Flow

Configuration writes generally follow this path:

```text
Validate in management API
  → write a SQLite transaction
  → replace the settings/upstream/binding snapshot
  → subsequent requests see the new snapshot without a restart
```

Account-map writes follow this path:

```text
synchronizer or management API
  → write acct_map in a transaction and garbage-collect orphan acct_egress rows
  → full Reload of acctmap.Registry
  → OnMapChange rebuilds acctegress.Table
```

A source-level lock serializes full synchronization and direct incremental updates for the same source. Different sources can run in parallel. Registry reloads and binding-snapshot rebuilds are additionally serialized by the process-wide map-change lock, preventing an older reload from overwriting a newer in-memory state.

---

## 4. Startup, Storage, and Defaults

### 4.1 Startup Parameters

The startup form is:

```bash
mitmrouter \
  -data ./data \
  -addr 127.0.0.1:55666 \
  -admin-addr 127.0.0.1:55667 \
  -trace-file ./debug.trace \
  -log-level info
```

| Parameter | Default | Description |
|---|---|---|
| `-data` | `./data` | Data directory containing `router.db`; relative paths are relative to the process working directory |
| `-addr` | `127.0.0.1:55666` | Client HTTP ingress; takes effect on every start and is not stored in the database |
| `-admin-addr` | `127.0.0.1:55667` | Admin-console listener; takes effect on every start and is not stored in the database |
| `-trace-file` | Empty | Enables local plaintext tracing; contains sensitive information and is disabled by default |
| `-log-level` | `info` | `debug`, `info`, `warn`, or `error` |

Both listen parameters must be valid `host:port` values, use ports `1–65535`, and differ from each other. Listener addresses are no longer settings controlled by the admin console. Legacy `listen_addr` and `admin_addr` keys in an old database are ignored.

First startup performs these steps:

1. Create the data directory and tighten it to `0700`;
2. Create the SQLite schema, enable WAL, and serialize writes;
3. Seed default settings and a random `hash_salt`;
4. Generate or load the ECDSA P-256 root CA and store it in `secrets`;
5. Generate a random administrator password, store only its bcrypt hash, and print the plaintext once to the console;
6. Restore Marker dynamic salts, account mappings, and account-egress bindings;
7. Start the audit writer, update writer, synchronizer, and two HTTP listeners.

The ingress and admin listeners can each have their own TLS certificate/key pair. Filling both paths enables HTTPS-only mode for that listener. At the same paths, certificate files are checked by mtime/size every 60 seconds; a renewal is used for new handshakes, while a bad reload keeps the old certificate and only logs a warning. Changing the paths themselves requires a restart.

### 4.2 SQLite Runtime

The service uses the pure-Go `modernc.org/sqlite` driver and does not require CGO:

- the main write connection uses `MaxOpenConns=1` to serialize transactions;
- a read-only pool has up to four connections and uses `query_only`;
- `journal_mode=WAL`;
- `busy_timeout=5s`;
- the data directory is `0700`; `router.db`, WAL, and SHM files are `0600`.

The database is the only persistent state carrier. It contains configuration, egress credentials, the admin session key, the CA private key, account mappings, and audit data. Runtime snapshots are maintained in process memory.

### 4.3 Actual Table Schema

The current implementation has nine main tables. `store.ensureSchema` creates them idempotently and adds missing columns to older databases.

#### settings

```sql
CREATE TABLE settings (
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL,       -- JSON text
  updated_at INTEGER NOT NULL
);
```

#### upstreams

```sql
CREATE TABLE upstreams (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  name       TEXT NOT NULL UNIQUE,
  platform   TEXT NOT NULL,       -- dataimpulse/decodo/1024proxy/resin/generic/plain
  base_url   TEXT NOT NULL,       -- contains real egress credentials; masked in API output
  inject     TEXT,                -- JSON used only by generic
  enabled    INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
```

`plain` is just another row in `upstreams`, not a separate table. Its `inject` value must be empty.

#### access_logs

```sql
CREATE TABLE access_logs (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  ts             INTEGER NOT NULL,       -- unix ms
  req_id         TEXT NOT NULL DEFAULT '',
  method         TEXT NOT NULL,
  host           TEXT NOT NULL,
  path           TEXT NOT NULL,
  status         INTEGER NOT NULL,
  dur_ms         INTEGER NOT NULL,
  ttfb_ms        INTEGER,
  bytes_out      INTEGER NOT NULL DEFAULT 0,
  has_marker     INTEGER NOT NULL,
  account        TEXT NOT NULL DEFAULT '',
  account_fp     TEXT NOT NULL,
  upstream       TEXT NOT NULL,
  err            TEXT,
  internal_error TEXT
);
```

- `account` is the real logical account after an `acct_map` hit, not an AT/RT value; it is empty when unmapped;
- `account_fp` is the derived identity for the request; sticky upstreams and sticky bindings use it;
- `err` is a legacy column; new requests write `internal_error`;
- `ttfb_ms` can be NULL when no response header was submitted or when the row predates the metric.

Indexes are `idx_logs_ts(ts)` and `idx_logs_account(account_fp, ts)`.

#### marker_salts

```sql
CREATE TABLE marker_salts (
  marker_fp  TEXT PRIMARY KEY,   -- full SHA-256 fingerprint, never the Marker
  salt       INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
```

This table stores dynamic salt values, not the consecutive-failure counter. The failure counter exists only in the process-local LRU.

#### acct_map

```sql
CREATE TABLE acct_map (
  platform    TEXT NOT NULL,
  source      TEXT NOT NULL,       -- src:<id> / api
  source_type TEXT NOT NULL,       -- CLIProxyAPI / Sub2API / custom
  account     TEXT NOT NULL,       -- account identifier, normalized to lowercase
  at_fp       TEXT NOT NULL DEFAULT '',
  rt_fp       TEXT NOT NULL DEFAULT '',
  at_hint     TEXT NOT NULL DEFAULT '',
  rt_hint     TEXT NOT NULL DEFAULT '',
  updated_at  INTEGER NOT NULL,
  PRIMARY KEY(platform, source, account, rt_fp, source_type)
);
CREATE INDEX idx_acct_map_source ON acct_map(source);
```

AT/RT plaintext is never stored. `at_fp` and `rt_fp` are platform-namespaced credential fingerprints; `*_hint` contains only a short tail for display.

#### sync_sources

```sql
CREATE TABLE sync_sources (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  kind              TEXT NOT NULL,       -- cpa / sub2api
  name              TEXT NOT NULL UNIQUE,
  mode              TEXT NOT NULL DEFAULT 'api', -- legacy field; no longer used
  base_url          TEXT NOT NULL,
  direct_auth_dir   TEXT NOT NULL DEFAULT '',
  direct_db_secret  TEXT NOT NULL DEFAULT '',
  interval_s        INTEGER NOT NULL DEFAULT 600,
  enabled           INTEGER NOT NULL DEFAULT 1,
  last_sync_at      INTEGER,
  last_status       TEXT NOT NULL DEFAULT '',
  empty_streak      INTEGER NOT NULL DEFAULT 0,
  created_at        INTEGER NOT NULL,
  updated_at        INTEGER NOT NULL
);
```

`base_url` and the API key are used by scheduled API full synchronization. `direct_auth_dir` or `direct_db_secret` enables optional direct incremental reading. The API key is stored as `secrets[source_key_<id>]`; the PostgreSQL DSN is stored as `secrets[source_direct_db_<id>]`; the table stores only the secret key name.

#### acct_egress

```sql
CREATE TABLE acct_egress (
  platform   TEXT NOT NULL,
  account    TEXT NOT NULL,
  egress_id  INTEGER NOT NULL,       -- upstreams.id; must refer to plain
  mode       TEXT NOT NULL DEFAULT 'sticky', -- sticky / random
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY(platform, account, egress_id)
);
CREATE INDEX idx_acct_egress_egress ON acct_egress(egress_id);
```

Bindings do not include a `source` dimension. The same `(platform, account)` shares one binding even when it is present in multiple sources.

#### sync_events

```sql
CREATE TABLE sync_events (
  id      INTEGER PRIMARY KEY AUTOINCREMENT,
  ts      INTEGER NOT NULL,
  kind    TEXT NOT NULL,           -- direct_file/direct_incremental/api_sync/push/delete
  source  TEXT NOT NULL DEFAULT '',
  status  TEXT NOT NULL DEFAULT 'ok', -- ok / error
  summary TEXT NOT NULL DEFAULT '',
  detail  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_sync_events_ts ON sync_events(ts);
```

This table records account-map changes and is separate from `access_logs`. The nine tables are `settings`, `upstreams`, `access_logs`, `marker_salts`, `acct_map`, `sync_sources`, `acct_egress`, `sync_events`, and `secrets`.

#### secrets

```sql
CREATE TABLE secrets (
  key   TEXT PRIMARY KEY,
  value BLOB NOT NULL
);
```

Main keys include:

- `admin_password_bcrypt` and `session_hmac_key`;
- `ca_cert_pem` and `ca_key_pem`;
- `source_key_<id>`: a synchronization-source API key;
- `source_direct_db_<id>`: a Sub2API PostgreSQL DSN.

### 4.4 Defaults and Hot Reload

Effective first-install defaults are:

| Setting key | Default | Current semantics |
|---|---:|---|
| `listen_auth` | `""` | Empty means inbound Basic authentication is off |
| `default_upstream` | `""` | Empty means no upstream is configured and direct connection is allowed; it does not mean “choose the first” |
| `no_marker_policy` | `default_session` | No credential uses the fixed identity `default` |
| `marker_path_parts` | `[]` | Empty means try Marker rules on every path |
| `marker_headers` | `Authorization`, `x-api-key`, `api-key`, `x-goog-api-key` | Tried in this order |
| `hash_salt` | Random 32-byte hex | Changing it recomputes every identity |
| `sid_len` | `16` | Backend accepts `4–64` |
| `session_ttl_min` | `0` | `0` means do not interfere with platform TTLs |
| `salt_rotate_failure_threshold` | `2` | Consecutive rotatable failures; accepted range `1–100` |
| `block_private_targets` | `true` | Reject private, local, and other non-public targets |
| `acctmap_enabled` | `true` | When off, skip account mapping and use the Marker formula |
| `acl_whitelist` / `acl_blacklist` | `[]` | By default all targets are eligible for MITM parsing |
| `log_retention_days` | `30` | Retention period for audit and update records |
| `sync_empty_clear_threshold` | `3` | Clear a source only after N consecutive successful empty results |
| `metrics_enabled` | `false` | `/metrics` returns 404 by default |
| Four TLS path keys | `""` | Fill each certificate/key pair to enable that listener's TLS |

The forwarding path reads an immutable `settings.Holder` snapshot through `atomic.Value` without a lock. `log_retention_days`, `metrics_enabled`, and `sync_empty_clear_threshold` are operational settings read from the store/API as needed and are not part of the forwarding snapshot.

Saving settings validates TLS pairs and loadability, policy and numeric ranges, a non-empty Marker-header list, and ACL entries. A non-empty `default_upstream` must refer to an enabled upstream; an empty value explicitly means direct connection. After the routing settings are saved, the snapshot is replaced immediately. A TLS-path change returns `restart_required=true`. `acctmap_enabled` exists in the settings table but is not exposed by the current settings API or page; PUT settings preserves its old value so an older client cannot silently turn account mapping off.

---

## 5. Module Breakdown and Key Interfaces

### 5.1 Directory Structure

```text
MITMRouter/
├── cmd/mitmrouter/main.go          # bootstrap, two listeners, graceful shutdown
├── internal/
│   ├── acl/                        # target ACL compilation and matching
│   ├── acctegress/                 # immutable account↔ordinary-proxy binding snapshot
│   ├── acctmap/                    # account registry, fingerprints, Host→platform mapping
│   ├── api/                        # management REST, sessions, and CRUD
│   ├── certca/                     # root CA, SNI leaf certificates, cache
│   ├── httpnames/                  # shared credential-header constants
│   ├── identity/                   # header/query/body identity resolution
│   ├── marker/                     # Marker rules and dynamic-salt LRU
│   ├── metrics/                    # Prometheus text metrics
│   ├── reqid/                      # server-internal request IDs
│   ├── server/                     # ingress, MITM, blind tunnels, forwarding
│   ├── settings/                   # settings snapshots, validation, hot reload
│   ├── sticky/                     # pure hash derivation
│   ├── store/                      # SQLite schema, transactions, async writers
│   ├── syncer/                     # full synchronization and direct readers
│   ├── tlsreload/                  # mtime-based external certificate reload
│   ├── trace/                      # explicitly enabled plaintext troubleshooting trace
│   ├── upstream/                   # upstream entries and session injectors
│   └── webui/                      # //go:embed SPA file server
├── web/                            # Vue3 + Vite + Element Plus frontend
│   └── src/views/{Login,Settings,Upstreams,AcctMap,Audit,Updates}.vue
├── DEPLOY.md / DEPLOY.en.md
├── PARAMETERS.md / PARAMETERS.en.md
├── DESIGN.md / DESIGN.en.md
└── docs/
    ├── 001-function-test-plan.md
    ├── 002-transparent-forwarding-design.md
    ├── 003-identity-resolution-design.md
    ├── 004-stable-account-hash-design.md
    ├── 005-upstream-endpoints-and-auth-analysis.md
    ├── 006-sticky-session-credentials.md
    ├── 007-security-public-deployment-assessment.md
    ├── 008-benchmark-system.md
    ├── 009-performance-test-report.md
    ├── 010-bug-revalidation-report.md
    ├── 011-plain-binding-design.md
    ├── 012-credential-refresh-monitoring-design.md
    └── 013-update-log-design.md
```

### 5.2 Upstream Module

```go
type Upstream struct {
    ID       int64
    Name     string
    Platform string
    BaseURL  *url.URL       // original egress URL, including credentials
    Enabled  bool

    // generic-only fields
    UsernameTemplate string
    StaticPassword   string
}

type InjectParams struct {
    Account string // derived session identity or a fallback such as default
    TTLMin  int    // session_ttl_min; 0 means no intervention
    Country string // reserved; currently empty
}

type SessionInjector interface {
    Inject(base *url.URL, p InjectParams) (*url.URL, error)
}
```

Injectors register through `upstream.Register` and are obtained for a concrete entry with `InjectorFor(platform, upstream)`. Injectors have no shared mutable state and return URL copies.

### 5.3 Identity Resolution Module

```go
type Resolution struct {
    Credential string // request-memory only; never log it
    Platform   string
    Account    string // real account after an acct_map hit; empty otherwise
    Mapped     bool
    RuleID     string // non-sensitive rule name
}

type Resolver struct{}
func (r *Resolver) ResolveWithBody(
    req *http.Request,
    targetHost string,
    opts Options,
) (Resolution, io.ReadCloser)
```

`ResolveWithBody` also returns the exact body stream to forward. A body parser may read from the original stream early, but the return value replays the bytes it consumed. The parser does not replace the body on the original request.

### 5.4 Account-Mapping and Binding Snapshots

```go
type Registry struct { /* rows + AT/RT fingerprint indexes */ }
func (r *Registry) Lookup(fp string) (Entry, bool)
func (r *Registry) Reload(entries []Entry)

type Table struct { /* acctegress: immutable map[(platform,account)] */ }
func (t *Table) Lookup(platform, account string) (Binding, bool)
```

`acctmap.Registry` protects its in-memory indexes with an RWMutex and provides O(1) AT/RT fingerprint lookup. Writes do not update individual rows; they replace the whole registry. `acctegress.Table` is immutable. After a database write, it is rebuilt and atomically installed with `Server.SwapAcctEgress`.

### 5.5 Settings and Routing Service

```go
type Holder struct{ /* atomic.Value */ }
func (h *Holder) Current() Snapshot
func (h *Holder) Set(snap Snapshot)

func (s *Server) SwapUpstreams(*upstream.Table)
func (s *Server) SwapAcctEgress(*acctegress.Table)
func (s *Server) AttachAcctMap(*acctmap.Registry)
```

`Server` owns the shared `http.Transport`, CA, settings snapshot, upstream table, account registry, binding table, Marker-salt LRU, asynchronous audit channel, and optional trace writer.

---

## 6. Identity Resolution and Stickiness Algorithm

### 6.1 Host-to-Platform Mapping

Account mapping is not attempted blindly for every domain. `acctmap.PlatformForHost` lowercases and removes the port from the target Host, then applies these suffix mappings:

| Platform | Host suffixes |
|---|---|
| `openai` | `chatgpt.com`, `openai.com` |
| `anthropic` | `anthropic.com`, `claude.ai`, `claude.com` |
| `gemini` | `googleapis.com`, `ai.google.dev` |
| `grok` | `x.ai`, `grok.com` |
| `kimi` | `api.kimi.com`, `moonshot.cn` |
| `deepseek` | `deepseek.com` |
| `glm` | `bigmodel.cn`, `z.ai` |
| `qwen` | `dashscope.aliyuncs.com` |
| `iflow` | `apis.iflow.cn` |
| `ollama` | `ollama.com` |

An unknown Host returns an empty platform and cannot hit the account map. A custom platform can be stored through manual registration, but the current automatic router has no configurable arbitrary Host-to-platform rule.

### 6.2 Credential Normalization and Extraction Order

`NormalizeCred`:

1. trims leading and trailing whitespace;
2. removes matching single or double quotes;
3. strips a case-insensitive `Bearer ` or `Token ` prefix when present;
4. preserves the case of the remaining credential.

For a known AI Host, the generic extraction order is:

```text
Authorization
→ x-api-key
→ api-key
→ x-goog-api-key
→ URL query parameter key
```

This known-platform path does not use `marker_path_parts`, so a credential on any AI API path can identify its account. If no generic platform credential is found, `marker.Extract` is tried. It follows the configured `marker_headers`; `Authorization` accepts only the `Bearer` form. When `marker_path_parts` is non-empty, the URL path must contain at least one configured fragment. The fragments use ordinary substring matching and do not include the query.

The fixed resolution order is:

```text
1. known-platform header/query carriers
2. generic MarkerRules headers
3. a matching exact body rule
4. no identity: apply no_marker_policy
```

### 6.3 Special Body Parsing

The current implementation has one body rule:

```text
Host:         auth.x.ai
Method:       POST
Path:         contains /oauth2/token
Content-Type: application/x-www-form-urlencoded
Field:        refresh_token
```

Body parsing follows these rules:

- never read the body when a header/query credential was already found;
- read at most `64 KiB`;
- parse a copy and keep the bytes sent to the target unchanged;
- if the body is too large, malformed, or missing the field, continue forwarding and use the fallback identity;
- if several rules match on one Host, choose the rule with the longest `PathKey`; Host matching remains exact;
- the current implementation parses only the Grok OAuth refresh body and does not scan arbitrary JSON or other bodies.

### 6.4 acct_map Hits and the Fallback Formula

The credential lookup key is:

```text
credential' = NormalizeCred(credential)
fp          = lowercase_hex(SHA256(platform + ":" + credential'))
```

When `acctmap_enabled=true` and `fp` hits the registry:

```text
identity.key = platform + "/" + account
```

The AT and RT of one account, and credentials for the same account from different sources, produce the same `identity.key`. `account` is normalized to lowercase; the source instance is not part of the routing identity.

The request's derived session identity, also stored in the audit `account_fp` field, is:

```text
k          = markerSalts.Get(identity.key, credential, or a fallback key)
base_salt  = sticky.CombineSalt(hash_salt, k)

acct_map hit:
account_fp = Derive(base_salt + "#a", platform + "/" + account, sid_len)

mapping miss:
account_fp = Derive(base_salt, normalized credential, sid_len)
```

`Derive` is:

```text
lowercase_hex(SHA256(salt + marker))[:sid_len]
```

This is string-concatenation hashing, not HMAC. `sid_len` accepts `4–64`, with a default of 16. The `#a` namespace makes a mapped identity distinct from the legacy Marker hash.

### 6.5 No-Marker Fallback

| `no_marker_policy` | `account_fp` | Egress behavior |
|---|---|---|
| `default_session` | `default` until rotated; after rotation `Derive(base_salt+"#default", "default")` | default upstream or direct |
| `client_ip_session` | `Derive(base_salt+"#ip", clientIP)` | default upstream or direct |
| `direct` | `-` | forced direct connection; no upstream |

`default_session` and `client_ip_session` have separate dynamic-salt keys. `direct` does not create a dynamic-salt key.

### 6.6 Salt Rotation When an Upstream Is Unusable

This is an “escape to a new identity” mechanism, not a business-request retry:

- it applies only to requests sent through an upstream; direct connections do not rotate;
- with a Marker, rotation is keyed by the Marker or mapped account; without a Marker, it is keyed by the fixed fallback identity or source IP;
- TLS/certificate failures, TLS alerts, invalid TLS records, handshake-time EOF, and upstream proxy CONNECT errors are rotatable failures; a real target HTTP 5xx and an ordinary dial refusal are not;
- the integer salt is incremented only after `salt_rotate_failure_threshold` consecutive rotatable failures, defaulting to 2; a non-rotatable failure or any HTTP response clears the consecutive-failure count;
- the next derivation uses `hash_salt + "#k" + N`, producing a new `account_fp` and usually a new platform-side egress;
- the process uses a thread-safe LRU with capacity 10,000. Its key is a full `sha256(identity key)` and it never stores the plaintext identity;
- rotation events are written through a bounded queue of capacity 256 to `marker_salts`. Startup restores at most 10,000 rows ordered by recent activity. A full queue or database-write failure affects persistence only, not the current in-memory route;
- the metrics are `marker_salt_rotations_total` and `marker_salt_persist_dropped_total`.

The design guarantees deterministic identity derivation, not a permanent lock on one physical IP. A provider's session expiry, node exhaustion, or lease reclamation can still change the concrete egress IP.

---

## 7. Upstream Adapters and Route Selection

### 7.1 Six Upstream Platforms

| `platform` | Purpose | Injection behavior |
|---|---|---|
| `dataimpulse` | DataImpulse sticky egress | Remove old `sessid.*` from the username parameter area and append `sessid.<account>` |
| `decodo` | Decodo/Smartproxy sticky egress | Require the `user-` prefix, replace/insert `session-<account>`, and optionally replace `sessionduration` |
| `1024proxy` | 1024proxy sticky egress | Replace `sid-<account>` in flat parameters and optionally replace `t-<minutes>` |
| `resin` | Resin sticky egress | Keep the part before the first `.`, then produce `Platform.<account>` |
| `generic` | User-defined template | Replace `{user}`, `{sid}`, `{ttl_min}`, and `{country}` |
| `plain` | Ordinary proxy | Return `base_url` through an identity injector; use credentials as-is and inject no session parameters |

Every injector operates on a copy of the egress credential URL sent to the upstream proxy. It does not alter the target site's business URL, request headers, request body, or response content.

### 7.2 Dedicated Injector Rules

#### DataImpulse

```text
input:  login__cr.us
output: login__cr.us;sessid.<account>
```

- The parameter area uses `;` between parameters and `.` between key and value;
- existing `sessid.*` parameters are removed before the new one is appended;
- the correct key is `sessid`, not the community-circulated `sid`;
- `session_ttl_min` does not affect this `sessid` mechanism.

#### 1024proxy

```text
<apikey>-region-US-sid-old-t-5
→ <apikey>-region-US-sid-<account>-t-5
```

- Known keys are `region`, `st`, `city`, `asn`, `sid`, and `t`;
- only the token immediately after a known key is treated as its value, so hyphens inside the API key remain valid;
- when `session_ttl_min>0`, `t` is replaced with a value clamped to `1–120`;
- the base URL should normally include `-t-N`.

#### Decodo

```text
user-alice-country-us-session-old-sessionduration-90
→ user-alice-country-us-session-<account>-sessionduration-90
```

- The username must start with `user-`;
- known keys include `country`, `city`, `st`, `state`, `asn`, `session`, `sessionduration`, and `session_iplock`;
- when `session_ttl_min>0`, `sessionduration` is clamped to `1–1440`;
- Decodo sessions have provider-side idle-expiry semantics, so the concrete IP remains best effort.

#### Resin

```text
Default:<token>
→ Default.<account>:<token>
```

- The username must not be empty;
- the text before the first `.` is the Resin Platform; the old Account is discarded;
- Resin receives the Account string as-is and manages the lease, health switching, and expiry itself.

#### Generic

`inject` is stored as JSON, for example:

```json
{
  "username_template": "{user}-sessid-{sid}",
  "password": "static-password"
}
```

The only placeholders are `{user}`, `{sid}`, `{ttl_min}`, and `{country}`. Both save-time and runtime validation reject unclosed braces and unknown placeholders. `{ttl_min}` is allowed only when `session_ttl_min>0`. A non-empty `password` overrides the base URL password; if omitted, the base URL password is retained.

### 7.3 Protocols and Default Upstream

| Base URL scheme | Ordinary HTTP forwarding | Blind tunnel |
|---|---|---|
| `http://` | `http.Transport` proxy | TCP + HTTP CONNECT |
| `https://` | `http.Transport` proxy | TCP + TLS, then HTTP CONNECT |
| `socks5://` | SOCKS5 egress | custom SOCKS5 CONNECT |
| `socks5h://` | handled as SOCKS5; the target domain is resolved by the upstream | handled as SOCKS5 |

Default route selection:

1. If there is no credential and `no_marker_policy=direct`, use a direct connection;
2. if a mapped account has a binding, use its bound `plain` egress first;
3. otherwise read `default_upstream`;
4. `default_upstream=""` explicitly means no default upstream and therefore a direct connection;
5. a non-empty default that is missing, disabled, lacks an injector, or fails injection produces a controlled failure; it must not silently fall back to direct connection;
6. `plain` can itself be the default upstream, in which case unmapped traffic uses that ordinary proxy without session injection.

---

## 8. Ingress, ACL, and Transparent Forwarding

### 8.1 Ingress/Admin Separation

The ingress `Server.Handler` behaves as follows:

```text
CONNECT       → inbound authentication → hijack → TLS MITM or blind tunnel
absolute URL  → inbound authentication → forward
origin-form   → static Ingress Port notice
```

The admin `AdminHandler` behaves as follows:

```text
/ui*, /api/*, /metrics → management handlers
CONNECT, absolute URL  → 404 admin_no_ingress
other origin-form      → admin placeholder/entry page
```

### 8.2 Inbound Basic Authentication

Set `listen_auth` to `user:pass` to enable authentication. Both CONNECT and absolute-form requests check:

```text
Proxy-Authorization: Basic base64(user:pass)
```

A missing, malformed, or incorrect credential receives:

```text
407 Proxy Authentication Required
Proxy-Authenticate: Basic realm="sticky-mitm"
```

Credential comparison uses `subtle.ConstantTimeCompare`. CONNECT authenticates once; a decrypted business request inside the tunnel is not checked again. Failed inbound authentication consumes no upstream resources.

The admin-console password and `listen_auth` are separate credentials. Because `/api/settings` is authenticated, the current implementation returns `listen_auth` in plaintext and the page allows an administrator to edit or copy it. The response may also include an ingress URL containing the credentials. Do not disclose either value.

### 8.3 CONNECT, TLS MITM, and Blind Tunnels

CONNECT flow:

```text
CONNECT host:port
  → listen_auth check
  → ACL target check
       ├─ denied → local 403; do not hijack or dial the target
       └─ allowed → continue
  → block_private_targets pre-check
  → hijack TCP connection
  → send 200 Connection Established first
  → peek the first byte (30-second pre-handshake idle limit)
       ├─ first byte 0x16 and ACLIntercept(host)=true
       │    → tls.Server
       │    → obtain/sign a leaf certificate for SNI
       │    → ALPN h2 or http/1.1
       │    → create a new req_id for every inner request
       │    → forward
       └─ otherwise
            → blindTunnel
```

Root and leaf certificates:

- root CA: ECDSA P-256, valid for 10 years, stored in `secrets`;
- leaf certificate: ECDSA P-256, SAN is the target domain or IP, valid for 7 days;
- leaf cache: LRU capacity 4096; entries with less than 24 hours remaining are reissued;
- concurrent issuance for the same SNI is merged with `singleflight`;
- clients must trust the `ca.pem` or `ca.crt` downloaded from the admin console;
- admin-listener TLS uses an external certificate and serves a different purpose from the MITM root CA.

After a blind tunnel is established, it only copies bytes and does not read a Marker, so it uses the no-Marker fallback policy. HTTP and SOCKS5 upstream handshake stages have a 15-second timeout; MITM TLS handshakes have a 10-second timeout. The forwarding path has no response-header timeout, so long LLM generations are not cut off.

### 8.4 ACL: Decide Whether the Target Is Allowed, Then Whether to MITM

ACL entries support:

- a single IP;
- a CIDR network;
- an exact domain;
- a wildcard such as `*.example.com` (matches subdomains but not the base domain itself).

Each list accepts at most 500 entries. Matching is case-insensitive and normalizes whitespace, ports, IPv6 brackets, and a trailing root dot. The decision order is:

```text
blacklist hit                         → local 403; reject access
non-empty allowlist with no match     → local 403; reject access
otherwise                             → allow; MITM-parse HTTPS traffic
```

ACL controls target access and does not rewrite an allowed business request or response. A rejected target receives only a local 403; no DNS lookup, identity parsing, egress selection, or target connection is performed. The decision is made:

- once before hijacking a CONNECT connection; a rejected target never receives `200 Connection Established`;
- again in `forward`, before identity parsing, covering absolute-form plaintext requests and an inner Host that differs from the CONNECT target;
- for an allowed CONNECT, after peeking the first byte, to choose TLS termination or a blind tunnel.

Rejections are recorded as `acl_blocked` audit events; `status=0` means no target response was received.

ACL matching uses only the target's literal hostname/IP and does not perform DNS resolution. Runtime snapshots precompile the matcher. The management API rejects an entire save containing an invalid entry; loading an old hand-edited database skips invalid entries and logs a warning instead. If the original allowlist was non-empty but no valid entries remain, access is still denied for every target, so runtime tolerance cannot accidentally disable the allowlist.

### 8.5 Private-Network Target Protection

`block_private_targets` defaults to true and is independent of ACL. It rejects:

- `localhost` and loopback addresses;
- RFC1918 private, link-local, unspecified, and multicast addresses;
- CGNAT `100.64.0.0/10`;
- hostnames whose DNS results contain any of the above;
- other special targets classified as non-public by the target-address validation rules, including common cloud-metadata addresses.

A hostname is resolved before routing and every result must be public. Direct connections dial the already checked IP, reducing DNS-rebinding bypasses. A private target receives a locally generated 403 and is audited with `status=0` and `internal_error=private_target_blocked`. When protection is disabled, a literal private target uses the compatibility behavior of a direct connection rather than the configured upstream; this is a high-risk setting.

### 8.6 Ordinary Request Forwarding and “Do Not Modify Requests”

Forwarding creates an internal copy only for `http.Transport`:

```go
func cloneForwardRequest(r *http.Request, scheme, host string, ctx context.Context) *http.Request {
    out := r.Clone(ctx)
    out.URL.Scheme = scheme // transport-only routing field
    out.URL.Host = host     // transport-only routing field
    out.RequestURI = ""
    return out
}
```

The original business request remains unchanged. The internal copy retains:

- the method;
- path, RawPath, RawQuery, and ForceQuery;
- client-provided request headers;
- body and trailers.

When a body parser has read from the body, the outbound copy receives a replay reader containing the consumed prefix followed by the remaining original stream. The target therefore receives the same request-body bytes without loss or duplication. When no User-Agent was supplied, the copy explicitly prevents Go from adding its default User-Agent.

Response handling:

1. call `RoundTrip`;
2. copy the target's real status and all response-header values;
3. write the response body in 32 KiB chunks and flush each chunk;
4. never buffer an entire SSE response;
5. relay 1xx responses, including `103 Early Hints`, before the final response;
6. announce trailers before the body and copy their values after the body ends;
7. on `101 Switching Protocols`, hijack the connection and copy upgraded bytes in both directions;
8. trace and diagnostic wrappers observe bytes without changing them.

A request ID exists only in context, slog records, `access_logs.req_id`, and the internal trace association. MITMRouter does not add one to the target request or client response, and it does not specially remove a same-named header supplied by the client or target.

---

## 9. Account Mapping and Synchronization

### 9.1 `acct_map` Row Semantics

One row represents:

```text
one platform + one source instance + one source type + one account + one current credential set
```

The primary key is `(platform, source, account, rt_fp, source_type)`:

- an AT change with the same RT updates the row in place;
- an RT change replaces the old row for the same source/type;
- disappearance from a full snapshot deletes the row under that source;
- the same account from different sources or source types occupies separate rows;
- as long as the account remains in any source, its account-level egress binding remains.

`source` values are:

- `src:<id>` for a `sync_sources` instance;
- `api` for manual registration through the management API.

`source_type` is a display and extension name. Built-in values are `CLIProxyAPI` and `Sub2API`; manual pushes may use any non-empty custom value up to 64 characters.

### 9.2 API Full Synchronization

There are two source kinds, and multiple independent instances of either kind are supported:

| kind | Display name | Calls |
|---|---|---|
| `cpa` | CLIProxyAPI | `GET {base}/v0/management/auth-files`, then concurrent downloads from `/v0/management/auth-files/download?name=...`; authentication is `Authorization: Bearer <management-key>` |
| `sub2api` | Sub2API | `GET {base}/api/v1/admin/accounts/data`; authentication is `x-api-key: <admin-api-key>` |

Scheduler behavior:

- one 30-second tick loop;
- each source uses its own `interval_s`, with a minimum of 60 seconds;
- one synchronization attempt is made immediately at process startup;
- the admin console's “Sync Now” button sends a wake event for the selected source;
- a failed API fetch keeps the old mapping and does not clear an empty snapshot;
- a successful fetch updates `last_sync_at`/`last_status` and emits an `api_sync` update event.

CPA parsing:

- a provider in the list must be on the platform allowlist;
- auth JSON uses `type`/`provider`, `email`/`email_address`, `access_token`, and `refresh_token`;
- both top-level tokens and nested `tokens` tokens are supported;
- `codex/openai` maps to `openai`, `claude` to `anthropic`, `gemini/antigravity` to `gemini`, `xai/grok` to `grok`, and the implementation also supports `kimi`, `qwen`, and `iflow`;
- disabled files are still synchronized because disabled does not mean that the credentials no longer belong to the account;
- unrecognized files, files without an account, and files without AT/RT are skipped; a download or parse failure fails the full round and preserves the old snapshot.

Sub2API parsing:

- only `type=oauth` and `type=setup-token` are accepted;
- `credentials.email` is used as the account, falling back to `name`;
- `access_token` and `refresh_token` are read;
- `apikey`, `upstream`, Bedrock, and other types without usable AT/RT are skipped;
- only allowlisted platform mappings are accepted.

### 9.3 Empty-Snapshot Protection

A successful full request that parses to zero accounts does not immediately clear the source's old mapping:

```text
non-empty keep set                  → replace snapshot immediately; empty_streak=0
empty keep set and no old rows      → do not clear anything; reset count to 0
empty keep set and old rows exist   → increment empty_streak
                                      below threshold: keep old mapping
                                      at threshold: clear source and GC bindings
```

`sync_empty_clear_threshold` controls the threshold, defaulting to 3 and accepting `1–100`. A value of 1 means immediate clearing. Fetch or parse failures never enter this path, so they do not increment the counter or clear mappings. The counter is stored in `sync_sources.empty_streak` and survives a restart.

At the threshold, the source mapping is cleared and `empty_streak` remains at the threshold. Later empty snapshots do not repeat the cleanup; the next non-empty snapshot resets the counter. This guard covers only a completely empty snapshot. A non-empty snapshot that is missing some accounts is still aligned immediately to the returned full result.

### 9.4 Direct Incremental Reading: Sub2API PostgreSQL

For a `sub2api` source, the API field `direct_db_dsn` enables this feature. The database column is `direct_db_secret`, which stores only the secret-key name; the plaintext DSN is stored in `secrets[source_direct_db_<id>]`. Direct reading is layered on top of API full synchronization, not an either/or mode:

- the reader performs one initial round, then polls every 3 seconds;
- it first finds OAuth/setup-token accounts updated in the last 30 seconds:

```sql
SELECT id, updated_at
FROM accounts
WHERE updated_at >= clock_timestamp() - interval '30 seconds'
  AND type IN ('oauth', 'setup-token')
ORDER BY updated_at, id;
```

- it then reads email, AT, and RT directly from `accounts.credentials` for those IDs;
- it stores no cursor, old-token hash, or extra snapshot; the 30-second overlap permits duplicate processing;
- `ApplyAccountDelta` affects only the current source + platform + account: delete that account's old mapping, insert new fingerprints when AT or RT exists, and insert nothing when both are empty;
- direct reading does not discover hard deletes, renames, or platform changes; API full synchronization owns completeness;
- a successful incremental round does not overwrite `last_sync_at`/`last_status`. A round that applies accounts emits `direct_incremental`; a failure sets `last_status` to error and emits a failure event.

Database safety rules:

- a local `localhost`, loopback address, or Unix socket may use no TLS;
- a remote PostgreSQL connection must use `sslmode=verify-full` and may not skip certificate verification;
- use a read-only database account;
- the DSN is read only while opening the connection and is not returned in the management API, logs, or update records.

### 9.5 Direct Incremental Reading: CPA Auth Directory

For a `cpa` source, an absolute `direct_auth_dir` enables direct reading:

- the root directory itself may not be a symlink;
- startup does not import the whole directory; existing mappings are established by API full synchronization;
- fsnotify watches are added recursively, and newly created subdirectories are added at runtime;
- only regular `.json` files are processed; symlinks, non-regular files, and files larger than 2 MiB are skipped;
- `Create`, `Write`, and visible atomic-renames enter a bounded pending queue; each path is debounced for 200ms;
- file size and mtime are checked before and after reading to protect against partial writes;
- `type/provider`, email/email_address, and top-level or nested `tokens` AT/RT values are parsed;
- a valid change calls `ApplyAccountDelta`;
- empty files, malformed JSON, unknown provider/type, missing accounts, and missing credentials do not commit an update and do not turn the old mapping into a deletion;
- `Remove` and cleanup after deletion/rename are left to the next API full synchronization; the direct reader does not persist file-to-account relationships;
- watcher failures trigger a rebuild attempt; missed changes are eventually converged by API full synchronization;
- file-queue and watcher failures preserve old mappings and do not stop API full synchronization.

### 9.6 Source Lifecycle and Concurrency

A direct reader is created only when both conditions hold:

```text
source.enabled = true
and cpa has direct_auth_dir / sub2api has direct_db_secret
```

There is no global incremental switch and no mutually exclusive `api/direct` mode.

- filling a path starts the reader for that source;
- clearing a path stops the reader, closes the watcher/database connection, and leaves existing mappings unchanged;
- disabling a source stops its reader but keeps existing mappings;
- changing the path or kind stops the old reader, commits the configuration, and starts the reader for the new configuration;
- deleting a source stops the reader and waits for in-flight work before one transaction deletes the source, its mappings, and its secrets;
- API full synchronization and direct incremental work for the same source share a source lock and cannot overwrite one another;
- after a mapping write, the implementation always reloads `Registry` and calls `OnMapChange` to rebuild the binding snapshot.

### 9.7 Manual Registration and Credential Editing

The admin console or an external management caller can use:

```text
PUT /api/acctmap/{platform}/{account}
```

Request body:

```json
{
  "access_token": "...",
  "refresh_token": "...",
  "source_type": "CLIProxyAPI"
}
```

Semantics:

- `source` is fixed to `api`;
- `source_type` is required and may not exceed 64 characters;
- at least one of AT and RT is required;
- submitting only AT preserves the current RT, and submitting only RT preserves the current AT;
- an RT change replaces the old pushed row for the same account and source type;
- different source types and pulled sources do not affect each other;
- the server stores only fingerprints and short hints;
- after the write, it reloads the registry and binding snapshot and emits a `push` event.

Deletion endpoints:

```text
DELETE /api/acctmap/{platform}/{account}?source=
DELETE /api/acctmap/{platform}/{account}/tokens/{fp}
```

Account deletion can be limited to one source. Token deletion matches AT first and then RT; if both fields become empty, the row is deleted. Every deletion path garbage-collects orphan `acct_egress` rows in the same transaction.

---

## 10. Account-to-Ordinary-Proxy Binding

### 10.1 Binding Semantics

A `plain` entry is an ordinary proxy:

```text
platform = plain
base_url = http(s)://user:pass@host:port or socks5://...
inject   = empty
```

The binding key is `(platform, account)`, not a Marker and not a source. Only a logical account that hit `acct_map` can hit a binding. An unmapped raw Marker, a no-Marker request, and a blind tunnel continue through the existing default route. `no_marker_policy=direct` applies only when there is no credential; once that policy selects direct connection, account bindings are not checked.

Priority:

```text
private-target rejection
  → no credential + no_marker_policy=direct: direct connection
  → mapped account with a binding: select a bound plain egress
  → default_upstream + session injection
  → explicit empty default_upstream: direct connection
```

If a binding exists but every bound egress is missing or disabled, the router returns a controlled 502 with `internal_error=upstream_config`. It never silently falls back to the default sticky route or direct connection. A binding candidate is passed through the `plain` identity injector, which returns a URL copy without adding session parameters.

### 10.2 Sticky and Random Modes

All rows for one account share one `mode`:

- `sticky`: compute a Rendezvous/HRW score for every candidate plain egress and choose the highest score. The result is deterministic for the same salt, account, and candidate set, and does not drift after restart;
- `random`: choose uniformly from the candidate set with `math/rand/v2` for every request, without persisting a selection.

HRW score:

```text
score = BigEndianUint64(
          SHA256(salt + "\x00" + platform/account
                 + "\x00" + strconv(egress_id))[:8]
        )

salt = CombineSalt(hash_salt, dynamic_salt) + "#a"
       for a mapped account binding
```

With HRW, adding or removing an egress changes only accounts that selected the affected egress, instead of reshuffling the whole pool as modulo hashing would. When an account's dynamic salt rotates, a sticky binding automatically chooses again.

A plain account binding ignores `session_ttl_min` and does not add query parameters or username parameters to its URL. It still goes through the plain identity injector, whose implementation returns a copy unchanged.

### 10.3 Cascade Cleanup

Two rules cover all deletion paths:

1. deleting an `upstreams` row deletes all `acct_egress` rows that reference its ID in the same transaction;
2. every `acct_map` write path runs:

```sql
DELETE FROM acct_egress
WHERE NOT EXISTS (
  SELECT 1 FROM acct_map a
  WHERE a.platform = acct_egress.platform
    AND a.account  = acct_egress.account
);
```

Therefore:

- a binding remains while the account exists in any source;
- when the account disappears from every source, the binding is removed in the same transaction;
- empty-snapshot protection protects the account and its binding together;
- the binding snapshot is rebuilt after GC and does not continue using deleted bindings.

### 10.4 Binding Writes

Account direction:

```text
PUT /api/acctegress/{platform}/{account}
{"mode":"sticky", "egress_ids":[3,7]}
```

The implementation deletes all bindings for the account and inserts the new complete set. `egress_ids=[]` is equivalent to clearing the binding. The account must exist in `acct_map`; every ID must identify a `plain` entry. The entry may be disabled at save time.

Egress direction:

```text
PUT /api/acctegress/egress/{id}
{
  "accounts":[{"platform":"anthropic","account":"a@example.com"}],
  "mode":"random"
}
```

This replaces the account set associated with the egress: accounts absent from the list are removed from that egress, and listed accounts are associated with it. `mode` affects only newly associated accounts; existing accounts keep their own mode. If the list contains an unknown account, the whole request returns 404 and does not partially write.

---

## 11. Management API and Web Console

### 11.1 Management Session

- Cookie name: `sticky_session`;
- payload contains only `{exp}` and is base64url-encoded;
- signature is HMAC-SHA256 using `secrets.session_hmac_key`;
- validity is 7 days; the session is renewed when less than half of its lifetime remains;
- `HttpOnly` and `SameSite=Lax`; `Secure` is set when the admin console request arrived over TLS;
- changing the admin password creates a new HMAC key and immediately invalidates all old sessions;
- login failures use source-IP exponential backoff, up to 15 minutes, with at most 10,000 in-memory entries;
- the new password must have at least six characters, and only its bcrypt hash is stored.

Every management API route except `POST /api/auth/login`, including `POST /api/auth/logout`, requires a valid session. Errors use one envelope:

```json
{"error":{"code":"...","message":"..."}}
```

Common status meanings are 400 for invalid input, 401 for no session, 404 for a missing resource, 409 for a conflict, 429 for login backoff, 500 for an internal error, and 503 when a capability is not wired.

### 11.2 REST Routes

#### Authentication, Settings, and Certificates

| Method | Path | Purpose |
|---|---|---|
| POST | `/api/auth/login` | Log in with `{password}` and issue a Cookie |
| POST | `/api/auth/logout` | Delete the browser Cookie |
| GET | `/api/auth/me` | Check the current session |
| POST | `/api/auth/password` | Change the admin password and revoke old sessions |
| GET/PUT | `/api/settings` | Read/save Basic Settings |
| POST | `/api/settings/reset-salt` | Generate a new `hash_salt` and recompute all identities |
| GET | `/api/ca.pem` | Download the PEM root certificate |
| GET | `/api/ca.crt` | Download the DER root certificate |
| GET | `/metrics` | Return Prometheus text when enabled |

`GET /api/settings` also returns two read-only values:

- `ingress_url`: constructed from the current request Host, the ingress port, and the ingress TLS state;
- `ingress_url_auth`: when `listen_auth` is enabled, the URL with URL-encoded `user:pass` credentials.

The ingress and admin IP/port values remain startup parameters and cannot be changed with PUT.

#### Upstream Egress

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/upstreams` | List entries; replace `base_url` passwords with `____` and mask Generic passwords too |
| POST | `/api/upstreams` | Create an upstream |
| PUT | `/api/upstreams/{id}` | Edit an upstream; `____` or `__unchanged__` in a password field keeps the old password |
| DELETE | `/api/upstreams/{id}` | Delete an entry; the default is protected and bindings are cascaded |
| POST | `/api/upstreams/{id}/default` | Set the default upstream |
| POST | `/api/upstreams/{id}/test` | Probe egress IP/geography using `account=healthcheck` |

Save-time validation checks the scheme, host, platform-specific username rules, and Generic template. `plain` cannot carry an `inject` template.

#### Synchronization Sources and Account Mapping

| Method | Path | Purpose |
|---|---|---|
| GET/POST | `/api/sources` | List/create synchronization sources |
| PUT/DELETE | `/api/sources/{id}` | Edit/delete a source; deletion cascades mappings and secrets |
| POST | `/api/sources/{id}/test` | Test the API full-sync configuration |
| POST | `/api/sources/{id}/test?target=incremental` | Test the direct PostgreSQL reader or CPA watcher |
| POST | `/api/sources/{id}/sync` | Trigger an API full synchronization immediately |
| GET | `/api/acctmap` | Preview mappings with platform/account/source/source_type/binding filters and pagination |
| GET | `/api/acctmap/stats` | Return row count, distinct-account count, and platform/source statistics |
| PUT | `/api/acctmap/{platform}/{account}` | Manually register or edit AT/RT |
| DELETE | `/api/acctmap/{platform}/{account}` | Delete an account mapping; `source=` limits the source |
| DELETE | `/api/acctmap/{platform}/{account}/tokens/{fp}` | Delete the matching AT or RT fingerprint |

For a new source, the API full-sync `base_url` and `api_key` are required; direct incremental configuration is optional. During editing, an empty API key/DSN means keep the old value, and `direct_db_clear=true` clears the Sub2API DSN and stops its incremental reader. Normal source-list responses never return an API key or complete DSN; they expose only `direct_db_configured`. An incremental test failure returns the reader's operational error summary. In the production reader, DSN parsing, TLS, and ping errors are normalized so the DSN content is not returned.

#### Account Egress Bindings

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/acctegress` | Return bindings grouped by account and association counts for each egress |
| PUT | `/api/acctegress/{platform}/{account}` | Replace one account's binding set |
| DELETE | `/api/acctegress/{platform}/{account}` | Clear one account's binding |
| DELETE | `/api/acctegress` | Clear all bindings |
| PUT | `/api/acctegress/egress/{id}` | Replace the account set associated with one plain egress |

A normal paginated response is:

```json
{"items":[],"total":0,"page":1,"page_size":50}
```

The log and update-record endpoints accept at most 200 rows per page. The account-map in-memory filter accepts up to 2,000 rows per page, and the egress-association drawer uses server-side pagination instead of fetching the entire account database at once.

### 11.3 Actual Pages

After login, the left navigation has five business pages:

1. **Basic Settings — `Settings.vue`**
   - A read-only ingress URL banner; when inbound authentication is enabled, the copyable authenticated URL is shown first;
   - ingress authentication and ingress TLS paths;
   - an explanation that `-addr`/`-admin-addr` are startup parameters, plus admin TLS paths;
   - Marker path fragments and request headers;
   - salt, SID length, TTL, and salt-rotation threshold;
   - default upstream, no-Marker policy, and private-target protection;
   - ACL allowlist/blocklist;
   - audit retention, empty-snapshot threshold, and metrics;
   - `ca.pem`/`ca.crt` downloads and administrator-password change.

2. **Upstream Egress — `Upstreams.vue`**
   - Upstream CRUD, dynamic platform hints, masked passwords, enable/disable, default selection, and egress connectivity tests;
   - support for `plain` ordinary proxies;
   - a “Bind Accounts” drawer on `plain` rows, with server-side account search, cross-page selection, and a mode for newly associated accounts.

3. **Account Management — `AcctMap.vue`**
   - synchronization sources with API full sync, optional direct incremental path, full-sync interval, and latest status;
   - full/incremental tests, immediate synchronization, and source create/edit/delete;
   - mapping preview with platform, account, AT/RT tails, source, binding mode/egress count, and update time;
   - manual AT/RT registration and editing;
   - filtering by platform, source type, account, and binding state;
   - account-side binding to multiple plain egresses in sticky or random mode;
   - one-click clearing of all account-egress bindings.

4. **Access Audit — `Audit.vue`**
   - time, host/path keyword, account/session, upstream, and 2xx/4xx/5xx/error filters;
   - request ID, method, target, status, total duration, TTFB, outbound bytes, Marker presence, account/session, internal error, and upstream;
   - pagination, five-second auto-refresh, and manual clearing.

5. **Updates — `Updates.vue`**
   - synchronization-source, direct-file, manual-push, and deletion mapping changes;
   - filtering by time, kind, source, and success/failure;
   - human-readable summaries and optional details such as an absolute file path;
   - pagination, five-second auto-refresh, and manual clearing.

---

## 12. Auditing, Update Records, Metrics, and Security

### 12.1 Access Audit

For each HTTP request entering `forward`, the server constructs an `access_logs` event, including when a local failure response is generated, and attempts to enqueue it. The event can still be dropped if the asynchronous queue is full. Ordinary CONNECT blind tunnels do not create `access_logs`; connection-level events normally appear only in structured logs. A CONNECT rejected by ACL or private-target policy before the tunnel is established additionally attempts a `status=0` audit event.

Audit fields:

| Field | Meaning |
|---|---|
| `req_id` | 32-character random lowercase hex, server-internal only |
| `host/path` | Target host and path; query parameters are not stored |
| `status` | Real target/upstream HTTP status; 0 for a local failure before a response was received |
| `dur_ms` | Time from processing start to response completion/failure |
| `ttfb_ms` | Time from processing start to the first response header submitted to the client; NULL if none was submitted |
| `bytes_out` | Response bytes actually written to the client |
| `has_marker` | Whether a credential/Marker was resolved |
| `account` | Real logical account after a mapping hit; empty when unmapped |
| `account_fp` | Derived sticky identity; may be `default`, a hash, or `-` without a Marker |
| `upstream` | Selected upstream name, or `direct` for a direct connection |
| `internal_error` | MITMRouter's safe internal failure category |

`status` and `internal_error` combinations:

| Situation | status | internal_error |
|---|---:|---|
| Target returns real 2xx/4xx/5xx | real status | empty |
| DNS/dial/timeout/TLS/EOF before response headers | 0 | `dns` / `dial` / `timeout` / `tls` / `eof` |
| Default or bound upstream configuration error | 0 | `upstream_config` |
| Upstream HTTP CONNECT explicitly rejected | 0 | `upstream_connect_rejected` or `upstream_connect` |
| Local private-target policy rejects the target | 0 | `private_target_blocked` |
| Response body is interrupted after headers | real status | `upstream_response_eof` or `upstream_response_read` |
| Client disconnect causes a response-write failure | real status | `downstream_write` |
| Request context is canceled | 0 or already committed status | `canceled` |
| Other forwarding error | 0 | `transport` |

A locally generated failure response uses fixed text and contains no upstream URL, username, password, account, or raw error:

```json
{"error":{"code":"bad_gateway","message":"upstream unavailable"}}
```

A real 407, 4xx, or 5xx response from the target/upstream is passed through unchanged and is not converted into an internal error.

Audit writes use a channel with capacity 4,096. The writer flushes every 200ms or after 256 entries. A full queue drops entries and logs a warning without blocking forwarding. Graceful shutdown drains the queue for at most five seconds.

### 12.2 Account-Mapping Update Records

`sync_events` records these events:

| kind | Trigger |
|---|---|
| `direct_file` | CPA file change processed successfully; read/parse failures, including unknown type/provider, create failure events; recognized files with no account or no credential only produce a warning |
| `direct_incremental` | Sub2API incremental accounts applied or an incremental query fails |
| `api_sync` | API full synchronization succeeds, an empty snapshot is deferred, or synchronization fails |
| `push` | Manual account registration/edit |
| `delete` | Account or token deletion |

`summary` is a human-readable sentence and `detail` is supplementary information. Neither contains AT/RT plaintext or a complete credential fingerprint. Update events use their own 4,096-capacity channel and 200ms/256-entry batch writer, independent of the access-audit channel; producers never block when the queue is full. Both tables use `log_retention_days` and are cleaned by the background retention task at startup and every 24 hours thereafter.

### 12.3 Prometheus Metrics

When `metrics_enabled=false`, `/metrics` returns 404. When enabled, it still requires an admin session. The current zero-dependency registry exposes:

- `requests_total{upstream,has_marker}` (`upstream` is the configured entry name and `has_marker` is a boolean);
- `upstream_errors_total{upstream}` (the only label is `upstream`);
- `auth_failures_total`: admin-console login failures;
- `ingress_auth_failures_total`: ingress Basic-authentication failures;
- `active_connections`: active tunnels;
- `marker_salt_rotations_total`;
- `marker_salt_persist_dropped_total`.

Account values, Markers, tokens, complete URLs, and request parameters are not metric labels.

### 12.4 Logs and Plaintext Trace

stdout uses a `log/slog` JSON handler:

- normal requests are primarily logged at debug level;
- info/warn/error cover startup, configuration, synchronization, and failures;
- structured records may contain `req_id`, target Host, upstream name, status, and safe failure categories;
- Marker, AT/RT, upstream URL userinfo, API keys, and PostgreSQL passwords are not logged.

Plaintext tracing is enabled only by explicitly passing `-trace-file <path>`:

- request/response URLs, complete headers, and bodies are appended;
- data is recorded in streaming chunks rather than waiting for a complete body;
- the file is created/opened with mode `0600`;
- tracing is off by default and the file contains secrets, so it must be temporary and protected;
- a trace-write failure is remembered but does not interrupt forwarding and is not written to SQLite audit records.

### 12.5 Key Protection

- `router.db` and WAL/SHM files are `0600`; the data directory is `0700`;
- the database contains the CA private key, upstream credentials, admin password hash, and session key;
- anyone with the data directory can decrypt HTTPS traffic handled by this service; protect backups like private keys;
- the management API masks upstream and Generic passwords and returns only “configured” for synchronization-source API keys/DSNs;
- the current settings page intentionally lets an authenticated administrator see the saved inbound-authentication password for editing/copying, and the authenticated ingress URL must not be disclosed;
- private-target protection is on by default; non-loopback ingress without inbound authentication and non-loopback admin listening without TLS both generate warnings.

---

## 13. Testing, Limitations, and Current Status

### 13.1 Covered Test Types

Run the backend test suite with:

```bash
go test ./...
```

The current tests cover:

- Marker/account fingerprints, credential normalization, path and header extraction;
- Host-to-platform mapping, AT/RT dual indexes, mapping hits, and Marker fallback;
- body-parser limits, body replay, and Grok refresh tokens;
- `Derive`, salt combination, consecutive failures, and dynamic-salt persistence/restart recovery;
- DataImpulse, Decodo, 1024proxy, Resin, Generic, and Plain injector golden cases;
- inbound authentication, separation of the two listeners, CONNECT/MITM/blind tunnels, and half-close behavior;
- ACL allow/deny behavior, private-DNS bypass protection, and local rejection;
- transparent URL/header/body/response/trailer/1xx/101/SSE forwarding;
- distinguishing real HTTP 4xx/5xx from local forwarding failures, plus TTFB and request IDs in audit records;
- acct_map full snapshots, AT/RT partial pushes, source isolation, deletion, and GC;
- the CPA direct watcher, Sub2API direct incremental query, and remote PostgreSQL TLS validation;
- empty-snapshot thresholds, source lifecycle, synchronization update events, and asynchronous writers;
- plain account-binding sticky HRW, random mode, pool-change stability, all-disabled controlled failure, and cascade cleanup.

The complete functional test plan is [docs/001-function-test-plan.md](docs/001-function-test-plan.md).

### 13.2 Known Limitations

1. **Provider stickiness is best effort.** Decodo sessions have idle expiry, DataImpulse `sessid` lasts about half an hour on average, 1024proxy is limited by `t` and the plan range, and Resin has lease expiry and node-exhaustion cases. One `account_fp` does not mean one physical IP forever.
2. **API full synchronization is no faster than every 60 seconds.** Direct Sub2API reading uses a 30-second overlap and polls every three seconds. A larger database-clock difference can fall outside the overlap; API full synchronization remains the final fallback.
3. **Direct readers provide freshness, not completeness.** They do not own hard deletion, renaming, or platform changes; API full synchronization converges those changes. CPA direct reading has no file-to-account relation, so deleting a file does not immediately delete its mapping.
4. **CPA supports only the verified minimal JSON shape.** Custom plugin formats and one-file/multiple-virtual-account formats are skipped; the next API full synchronization converges the result.
5. **Source permissions matter.** The service process must be able to read the CPA directory, the Sub2API account must be read-only, and remote PostgreSQL must use `verify-full`.
6. **ACL controls target access.** A blacklist hit or an allowlist miss returns a local 403; `block_private_targets` is a separate private-network check, and an ACL allow does not bypass it.
7. **Automatic account mapping is limited to built-in Host platforms.** Custom platforms can be registered and stored, but the current code has no admin-configured arbitrary Host-to-platform table.
8. **Event queues are best effort.** Audit and update records use bounded asynchronous queues. A full queue can drop records; the audit path emits a warning, while update-event producers simply remain non-blocking.
9. **There is no business-request retry.** Local failures return 502/403 and the SDK decides whether to retry.
10. **HTTP/3/UDP are outside the path.** Disable UDP 443 at the client or system layer when all traffic must pass through this service.
11. **Certificate-pinning clients cannot be MITM-decrypted.** The client must trust this service's CA and must not pin the target certificate.
12. **Disabling private-target protection creates a serious SSRF risk.** Non-loopback deployments must also configure firewall rules, TLS, and inbound authentication.

### 13.3 Current Implementation Status

The implementation is no longer the original “Marker only, four tables, four pages” M1–M4 plan. It now includes:

- two separated listener planes and optional listener TLS;
- transparent streaming forwarding and internal request IDs;
- Marker-level and account-level stickiness;
- CPA/Sub2API API full synchronization;
- CPA file and Sub2API PostgreSQL direct incremental readers;
- `plain` egress and account bindings;
- empty-snapshot protection, update records, auditing, and metrics;
- authenticated-admin editing and copying of the saved inbound credentials.

Future features must continue to follow two principles:

1. **Deep-module principle:** callers should learn small interfaces while parsing, snapshots, concurrency, and failure semantics stay inside the module;
2. **Transparent-forwarding principle:** routing, auditing, tracing, and request IDs must not modify external business requests or responses.

Related designs:

- [Transparent forwarding](docs/002-transparent-forwarding-design.md)
- [Special body identity resolution](docs/003-identity-resolution-design.md)
- [Stable account hash](docs/004-stable-account-hash-design.md)
- [Ordinary proxy and account binding](docs/011-plain-binding-design.md)
- [AT/RT incremental monitoring](docs/012-credential-refresh-monitoring-design.md)
- [Account-map update records](docs/013-update-log-design.md)
- [Parameter guide](PARAMETERS.en.md)
- [Production deployment guide](DEPLOY.en.md)

---

## Appendix: Minimal Decision Pseudocode for a Request

The following is a flow sketch, not compilable code. The actual implementation is in `internal/server/ingress.go`, `internal/identity/resolver.go`, and the snapshot modules. `resolveOutboundDetailed` computes and returns `account_fp` internally; callers do not pass it back as an argument.

```text
forward(request):
  snap = settings.Current()
  body = request.Body

  if not snap.ACLAllowed(normalize(host)):
      writeLocal403(response, "acl_forbidden")
      emitAudit(status=0, internal_error="acl_blocked")
      return

  if snap.ACLIntercept(normalize(host)):
      resolved, body = Resolver.ResolveWithBody(request, host, {
          MarkerRules, AcctMapEnabled, AcctMap,
      })
      ident = mappedIdentity(resolved)  # only an acct_map hit has platform/account
  else:
      resolved = emptyResolution()     # reserved for allowed traffic that skips MITM
      ident = emptyIdentity()

  proxyURL, accountFP, upstreamName, reason, err =
      resolveOutboundDetailed(request.Context(), snap,
                              resolved.Credential, ident, clientIP, host)

  # Inside resolveOutboundDetailed:
  # 1. validate the private-target policy; on failure return a local 403;
  # 2. no credential + no_marker_policy=direct → direct, short-circuit bindings;
  # 3. mapped identity with a binding → choose an enabled plain egress using
  #    sticky/random mode, then use the plain identity injector for a URL copy;
  # 4. otherwise choose default_upstream; empty means direct, invalid non-empty
  #    configuration is a controlled failure;
  # 5. all other upstreams receive accountFP through their SessionInjector.

  if err != nil:
      writeForwardFailure(responseWriter, err)
      emitAudit(status=0, internal_error=classify(err))
      return

  out = cloneForwardRequest(request, scheme, host,
                            context.WithValue(request.Context(), proxyURL))
  out.Body = body  # replay body when parsed; business bytes stay unchanged
  response, err = transport.RoundTrip(out)
  if err != nil:
      writeForwardFailure(responseWriter, err)
      emitAudit(status=0, internal_error=classify(err))
      return

  relayResponseStream(responseWriter, response)
  emitAudit(accountFP, upstreamName, reason,
            response.status, response.headers, response.body_bytes)
```

The real implementation also handles 1xx responses, trailers, SSE, 101 upgrades, response-body read failures, and downstream write failures. Account bindings take precedence over the default upstream inside `resolveOutboundDetailed`, except that `no_marker_policy=direct` short-circuits bindings for requests without credentials.
