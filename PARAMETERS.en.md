[中文](PARAMETERS.md)

# MITMRouter Parameter Configuration Guide

> This guide is for people deploying the system. It explains how to fill in every input field and startup parameter, which values to use, and what happens when a value is wrong, rather than explaining the code.
>
> Scope: the implementation on the repository's `main` branch.

---

## 0. Bottom line: where to configure parameters

MITMRouter **has no YAML, `.env`, or JSON configuration files**. Runtime configuration is ultimately stored in a SQLite file in the data directory:

```text
<data directory>/router.db
```

Parameters fall into five categories:

| Parameter location | Typical parameters | Saved to database | When it takes effect |
|---|---|---:|---|
| Startup command | `-data`, `-addr`, `-admin-addr`, `-trace-file`, `-log-level` | No | When the process starts; restart after changing it |
| Admin console “Basic settings” | TLS paths, ingress authentication, Marker rules, stickiness, ACL, maintenance settings | Yes | Most changes take effect immediately after saving; TLS path changes require a restart |
| Admin console “Upstream egress” | Name, platform, `base_url`, Generic template, enabled state | Yes | Takes effect immediately after saving |
| Admin console “Account management” | Sync source, account, AT/RT, account egress binding | Yes | Takes effect immediately after saving; sync sources run at their configured interval |
| Admin console “Access audit / update history” | Time range, keywords, pagination, filter conditions | No (query conditions only) | When querying or auto-refreshing |

The most important distinctions are:

- **The listening IP and port can only be changed through startup parameters**, not in the admin console;
- **Upstream credentials, the administrator password hash, and the MITM root CA private key are stored in `router.db`**; protect this file as key material;
- Upstream passwords, sync-source API keys, and Generic passwords shown in the admin console are masked; do not treat `____` as the real password when filling it in;
- When the default whitelist is empty, HTTPS targets are decrypted by MITM by default, so clients usually need to install the root CA downloaded from the admin console;
- This configuration document describes configuration only; it does not change the URL, request headers, response headers, or response body of external business requests.

## Contents

- [1. Recommended configuration order](#1-recommended-configuration-order)
- [2. Runtime command-line parameters](#2-runtime-command-line-parameters)
- [3. Three things you must know after first startup](#3-three-things-you-must-know-after-first-startup)
- [4. Detailed basic settings](#4-detailed-basic-settings)
- [5. Upstream egress parameters](#5-upstream-egress-parameters-name-platform-base_url-inject-enabled)
- [6. How to configure the six upstream platforms](#6-how-to-configure-the-six-upstream-platforms)
- [7. Account management: sync-source parameters](#7-account-management-sync-source-parameters)
- [8. Manual account registration parameters](#8-manual-account-registration-parameters)
- [9. Account ↔ outbound binding parameters](#9-account-outbound-binding-parameters)
- [10. Mapping between account platforms and target hosts](#10-mapping-between-account-platforms-and-target-hosts)
- [11. Access Audit page query parameters](#11-access-audit-page-query-parameters)
- [12. Update History page query parameters](#12-update-history-page-query-parameters)
- [13. Management API automation reference](#13-management-api-automation-reference)
- [14. Build script parameters (not runtime configuration)](#14-build-script-parameters-not-runtime-configuration)
- [15. Test-tool environment variables (not production service configuration)](#15-test-tool-environment-variables-not-production-service-configuration)
- [16. Common configuration combinations](#16-common-configuration-combinations)
- [17. Common errors and troubleshooting order](#17-common-errors-and-troubleshooting-order)
- [18. Runtime limitations you must not ignore](#18-runtime-limitations-you-must-not-ignore)
- [19. Parameter source index](#19-parameter-source-index)
- [20. A final checklist you can follow](#20-a-final-checklist-you-can-follow)

---

## 1. Recommended configuration order

Do not change every switch at the outset. Configure the system in the following order.

### 1.1 Minimal configuration for local use

```text
1. Start the program
2. Record the administrator password printed only once at first startup
3. Open the admin console
4. Download ca.pem and install it in the client trust store
5. Add an upstream egress
6. Set it as the default upstream
7. Point the client proxy to the ingress address
8. Verify the egress IP with “Test upstream” and a real request
```

Minimal startup command:

```bash
./mitmrouter -data ./data
```

Default addresses:

```text
Client ingress: 127.0.0.1:55666
Admin console:  127.0.0.1:55667
```

### 1.2 Recommended order for public or LAN deployment

If the ingress or admin console must listen on a non-loopback address, do not simply change it to `0.0.0.0`. Use this order:

```text
1. Prepare firewall rules first
2. Configure listen_tls_cert / listen_tls_key for the ingress
3. Configure admin_tls_cert / admin_tls_key for the admin console
4. Configure listen_auth (Basic authentication for the ingress)
5. Restrict the sources allowed to reach admin-addr to the operations network
6. Restart and confirm that both ports use the expected HTTP/HTTPS mode
7. Then connect clients or containers
```

The admin console uses its own session Cookie authentication; `listen_auth` authenticates the **client ingress**, not the admin-console login. For public deployment, consider both separately.

---

## 2. Runtime command-line parameters

Runtime parameters come from `cmd/mitmrouter`. The complete form is:

```bash
./mitmrouter \
  -data ./data \
  -addr 127.0.0.1:55666 \
  -admin-addr 127.0.0.1:55667 \
  -log-level info
```

`-trace-file` is recommended only temporarily for troubleshooting; see below.

### 2.1 `-data`: Data directory

| Item | Description |
|---|---|
| Parameter name | `-data` |
| Default value | `./data` |
| Type | Directory path string |
| Required | No |
| Effective time | At every startup |

**What to enter:**

Enter a directory that the program can create, read, and write, and that is exclusively or primarily used by this service. For example:

```bash
-data ./data
-data /opt/mitmrouter/data
-data /var/lib/mitmrouter
```

A relative path is relative to the process's current working directory, not the directory containing the binary. In systemd, an absolute path is recommended.

**What the directory contains:**

```text
/opt/mitmrouter/data/
└── router.db       # SQLite main database containing configuration, keys, CA, audit data, and account mappings
```

SQLite may also create these files while running:

```text
router.db-wal
router.db-shm
```

At startup, the program automatically:

- creates the data directory if it does not exist;
- restricts the data directory permissions to `0700`;
- restricts `router.db` and the WAL/SHM file permissions to `0600`;
- creates tables and performs idempotent schema completion.

**Do not enter these:**

```bash
-data ./router.db       # do not use a file name as the directory
-data /tmp               # production keys should not be stored in a temporary directory
```

Using a completely new `-data` directory is equivalent to creating a new instance: the administrator password, root CA, salt, and all database state will be generated again. The old data directory will not be automatically merged into the new one.

**Backup advice:**

`router.db` contains the CA private key, upstream real credentials, and account mappings, so it must not be copied to a public location like an ordinary log. Before backing it up, preferably stop the service or use a SQLite-aware backup method; do not casually copy a WAL database that is being written.

### 2.2 `-addr`: Client ingress listening address

| Item | Description |
|---|---|
| Parameter name | `-addr` |
| Default value | `127.0.0.1:55666` |
| Type | `host:port` |
| Saved to database | No |
| Effective time | At every startup |

This port is used as an HTTP proxy by curl, SDKs, containers, or other clients. It carries:

- HTTP absolute-form requests;
- HTTPS `CONNECT`;
- Local MITM decryption after `CONNECT`;
- inbound `Proxy-Authorization: Basic ...` authentication (when `listen_auth` is configured).

**Common values:**

```bash
# Allow access only from the local machine; the safest default
-addr 127.0.0.1:55666

# Listen on all IPv4 interfaces; recommended only after configuring a firewall, TLS, and authentication
-addr 0.0.0.0:55666

# Equivalent to listening on one of all local addresses; suitable for simple cases, but an explicit address is clearer
-addr :55666

# Listen on all IPv6 addresses; IPv6 addresses must be enclosed in square brackets
-addr '[::]:55666'
```

**Format requirements:**

- Must parse as `host:port`;
- the port must be from `1` through `65535`;
- must not be exactly the same as `-admin-addr`;
- restart the process after changing the command.

**Relationship between listening address and protocol:**

`-addr` determines only where to listen; it does not determine HTTP versus HTTPS. Whether the ingress is TLS-only is determined by these admin-console settings:

```text
listen_tls_cert
listen_tls_key
```

- If both paths are empty: the ingress accepts plain HTTP, and clients use a proxy URL such as `http://...`;
- if both paths are set: the ingress enforces HTTPS-only, and clients use a proxy URL such as `https://...`; plain-text connections are rejected during the TLS handshake.

**Risk warning:**

When `-addr` binds to a non-loopback address and `listen_auth` is empty, the program logs a risk warning. Any machine that can reach the port may then use it as a proxy.

### 2.3 `-admin-addr`: Admin-console listening address

| Item | Description |
|---|---|
| Parameter name | `-admin-addr` |
| Default value | `127.0.0.1:55667` |
| Type | `host:port` |
| Saved to database | No |
| Effective time | At every startup |

This port carries only:

- the `/ui/` admin-console pages;
- the `/api/*` management API;
- `/metrics` (when enabled, and still requiring login).

Clients must not use the admin-console port as a proxy. The admin-console port rejects `CONNECT` and absolute-form proxy requests; the ingress port does not expose admin-console capabilities as proxy requests.

**Common values:**

```bash
# Access only from a local browser
-admin-addr 127.0.0.1:55667

# Listen on all IPv4 addresses; admin-console TLS is required, and a firewall must restrict the source addresses
-admin-addr 0.0.0.0:55667

# Listen only on a specific LAN address
-admin-addr 192.168.1.10:55667
```

**Hard requirement relative to the ingress:**

```text
-addr must not be exactly the same as -admin-addr
```

If the admin console binds to a non-loopback address without `admin_tls_cert` / `admin_tls_key` configured, the program issues a warning. The admin-console login Cookie should not be transmitted over an untrusted plain-text network.

Changing the admin-console address does not write it to `router.db`. If systemd's `ExecStart` still uses the old address, saving settings in the web interface will not change where the listener binds; you must change the startup command or service file and restart it.

### 2.4 `-trace-file`: Plaintext request/response trace file

| Item | Description |
|---|---|
| Parameter name | `-trace-file` |
| Default value | Empty string; disabled |
| Type | File path |
| Effective time | File is opened at startup |
| Usage recommendation | For short-term local troubleshooting only |

For example:

```bash
-trace-file /tmp/mitmrouter-trace.log
```

When enabled, it appends:

- The request method and complete URL;
- All request headers;
- The request body;
- The response status;
- All response headers;
- Every data chunk of a streaming response body.

It **does not redact data** and may contain:

- API Key;
- Bearer Token;
- Refresh Token;
- User questions, model inputs, and model outputs;
- Cookie;
- Other business secrets.

The file is created or tightened with `0600` permissions and is appended to by default rather than overwriting old content. After troubleshooting, you should:

```text
1. Stop or restart the program with -trace-file removed
2. Delete the trace file
3. Clean up backups, terminal history, and log-collection copies according to your organization's security requirements
```

Do not treat it as a routine audit log, and do not leave it enabled in production for an extended period. Ordinary `-log-level debug` does not enable plaintext tracing; request and response bodies are written only when `-trace-file` is explicitly specified.

### 2.5 `-log-level`: Runtime log level

| Input value | Meaning |
|---|---|
| `debug` | Most detailed; suitable for troubleshooting |
| `info` | Default; normal runtime information |
| `warn` | Warning |
| `warning` | Compatibility spelling of `warn` |
| `error` | Errors only |

Examples:

```bash
-log-level info
-log-level debug
```

Comparison is case-insensitive and ignores whitespace at both ends. Any other value causes the program to fail at startup, for example:

```bash
-log-level verbose   # unsupported
```

Program logs use the fingerprint of the Marker rather than the plaintext Marker, but do not disclose logs carelessly for that reason: logs may still contain the target host, the cause of an error, upstream names, paths, and topology information.

The standard help option can also be used when running the program:

```bash
./mitmrouter -h
```

It only displays command-line help and does not modify any configuration.

---

## 3. Three things you must know after first startup

### 3.1 One-time admin password

When the program is used with a brand-new data directory for the first time, it generates a random administrator password and prints it once to the console:

```text
Admin password: <random string>
```

The database stores only a bcrypt hash; the plaintext password cannot be recovered from the database. Change the password immediately after the first login.

Current requirements for a new administrator password:

- The old password must be correct;
- The new password must be at least 6 characters long;
- After a successful change, the old admin session becomes invalid and you must log in again;
- The admin session is valid for a fixed 7 days and currently cannot be adjusted in the admin console.

### 3.2 MITM root CA and listener TLS certificates are different things

The project has two completely different types of certificates:

| Certificate | Purpose | Where configured |
|---|---|---|
| MITM root CA (`ca.pem` / `ca.crt`) | Trusted by clients and used to issue leaf certificates for target sites locally | Generated automatically by the program; downloaded from the admin console |
| Ingress/admin-console server certificate | Protects the two listener ports' own TLS connections | Four TLS paths on the Basic settings page |

If the client has not installed the MITM root CA, accessing an intercepted and decrypted HTTPS target will usually result in a self-signed certificate or unknown CA error.

Download endpoints:

```text
GET /api/ca.pem   # PEM text format
GET /api/ca.crt   # DER/CRT format; Windows can usually import it directly
```

### 3.3 The default ACL intercepts all HTTPS targets

The default values are:

```text
acl_whitelist = []
acl_blacklist = []
```

This means target access is unrestricted; for HTTPS targets:

```text
Empty whitelist + no blacklist match = allow access and MITM-decrypt
```

Therefore, before the first verification, you usually need to download and install `ca.pem`. If you want to allow only a small number of domains, fill in the whitelist; see Section 4.15.

---

## 4. Detailed basic settings

The admin console is usually available at:

```text
http://127.0.0.1:55667/ui/
```

If admin-console TLS is enabled, use:

```text
https://<admin-console-host>:<admin-console-port>/ui/
```

The IP and port shown in Basic Settings are read-only: they come from the startup parameters and are not editable fields.

### 4.1 Ingress address (`ingress_url`) — read-only; do not fill it in

The “Ingress address” shown at the top of the admin console is generated dynamically from:

- the port from `-addr`;
- the hostname used by the current request;
- whether ingress TLS is enabled.

For example, if the startup argument is:

```bash
-addr 0.0.0.0:55666
```

The page may show:

```text
http://127.0.0.1:55666
```

Or, when HTTPS is enabled:

```text
https://router.example.com:55666
```

This is only a hint to copy into a client or SDK configuration; it cannot change the listening port through `PUT /api/settings`.

### 4.2 Authenticated address (`ingress_url_auth`) — read-only; do not fill it in

When `listen_auth` is enabled, the page shows an address with the username and password embedded in the proxy URL, for example:

```text
http://proxy-user:proxy-pass@127.0.0.1:55666
```

It can be pasted directly into a client configuration that supports proxy URLs. However, because it contains real credentials:

- Do not share screenshots externally;
- do not commit it to Git;
- do not paste it into a public issue;
- do not put it in uncontrolled shell history or CI logs.

If credentials contain URL-special characters, the generated address is URL-encoded. A safer approach is to use the client’s separate proxy username and password fields.

### 4.3 `listen_auth`: Basic authentication for the client ingress

#### How to fill it in

The page splits this into two input fields:

```text
Username: <user>
Password:  <pass>
```

The underlying stored form is:

```text
user:pass
```

For example:

```text
Username: router-client
Password:  a random long password
```

This is equivalent to storing:

```text
router-client:<a random long password>
```

#### Value rules

| Input | Result |
|---|---|
| Leave both username and password empty | Disable ingress authentication |
| Fill in both username and password | Enable ingress authentication |
| Fill in only one | The page blocks saving; the API returns a validation error |
| Password contains a colon | It can be part of the password at the protocol level; do not put the colon in the username, to avoid confusion when reading and URL-encoding |

An empty value is suitable for a loopback-only listener:

```text
-addr 127.0.0.1:55666 + listen_auth empty
```

For a non-loopback listener, enabling authentication is strongly recommended:

```text
-addr 0.0.0.0:55666 + listen_auth non-empty + firewall + ingress TLS
```

#### How requests are authenticated

The client must send the following in the proxy request:

```http
Proxy-Authorization: Basic base64("Username:Password")
```

Authentication failure returns the standard:

```text
407 Proxy Authentication Required
```

Note that this is **ingress authentication**. A 407 returned by the upstream proxy is a different matter and usually means that the upstream `base_url` credentials are incorrect.

#### Display while editing

A saved password is displayed directly in the logged-in admin console:

```text
Username: router-client
Password:  <the original password>
```

You can change the password directly or copy it into the client’s username/password configuration. Do not share, commit to Git, or post publicly any page or ingress address containing real credentials.

### 4.4 `listen_tls_cert` / `listen_tls_key`: the ingress TLS certificate pair

These are the paths to the ingress server’s own TLS certificate and private key, not the MITM root CA paths.

Absolute paths are recommended:

```text
Certificate PEM: /opt/mitmrouter/certs/fullchain.pem
Private key PEM: /opt/mitmrouter/certs/privkey.pem
```

Other valid PEM certificates may also be used, for example:

```text
/etc/letsencrypt/live/proxy.example.com/fullchain.pem
/etc/letsencrypt/live/proxy.example.com/privkey.pem
```

#### Must be provided as a pair

| Certificate path | Private key path | Result |
|---|---|---|
| Empty | Empty | Plain HTTP ingress |
| Non-empty | Non-empty | HTTPS-only ingress |
| Non-empty | Empty | Saving is rejected |
| Empty | Non-empty | Saving is rejected |

When saving, the program attempts to load the certificate and private key. The following problems prevent saving:

- the file does not exist;
- the file cannot be read;
- the PEM format is invalid;
- the certificate and private key do not match;
- the files are not a parseable certificate pair.

An expired certificate or one that is not yet valid is a warning and does not prevent saving based on its time status. However, clients will encounter certificate problems after a restart, so renew or correct it promptly.

#### Activation semantics

- Changing the paths: after saving succeeds, the page says that a restart is required; before the restart, the listener continues using the old paths;
- with unchanged paths but changed file contents: the program automatically hot-reloads based on the file modification time, usually taking effect within about 60 seconds;
- when both fields are filled in, that port no longer accepts plain HTTP.

#### Client address change

When ingress TLS is enabled, change the client proxy configuration from:

```text
http://127.0.0.1:55666
```

to:

```text
https://127.0.0.1:55666
```

Do not fill in the certificate only on the server while continuing to have clients use `http://`.

### 4.5 `admin_tls_cert` / `admin_tls_key`: the admin console TLS certificate pair

The procedure is exactly the same as for the ingress certificate pair, but these settings affect only the `-admin-addr` listening port:

```text
Admin console certificate PEM: /opt/mitmrouter/certs/fullchain.pem
Admin console private key PEM: /opt/mitmrouter/certs/privkey.pem
```

The admin console can share a certificate pair with the ingress, provided that the certificate SAN covers the actual hostname used for access. Alternatively, two different certificate pairs can be used.

#### When configuration is required

- If the admin console is used only on `127.0.0.1`: a plain-text admin console is acceptable, but access through an SSH tunnel or another secure method is still recommended;
- if the admin console binds to an internal or public address: configuration is strongly recommended;
- if administrator passwords and session cookies must be transmitted across machines: HTTPS should be used.

Changing the paths requires a restart; certificate renewal at unchanged paths can be hot-reloaded. Admin console TLS and ingress TLS are two independent switches: one can be enabled while the other is disabled, but in production they should normally be considered together.

### 4.6 `marker_path_parts`: Marker path fragments (optional)

#### Default value

```json
[]
```

An empty array means:

```text
Apply the Marker extraction rules to all request paths
```

This is the recommended default because the same account continues to use the same sticky identity when accessing different API paths.

#### How to fill it in

The page is a multi-value tag selector. Press Enter after each value to add it, for example:

```text
/v1/
/chat/completions
/responses
```

Each fragment must begin with `/`:

```text
Correct: /v1/
Correct: /oauth/
Incorrect: v1/
Incorrect: chat
```

#### Matching method

This is **simple substring containment matching**, not a regular expression:

```text
If URL.Path contains any fragment, it is a match
```

For example, with this configuration:

```text
/v1/
```

These paths match:

```text
/v1/models
/v1/chat/completions
/proxy/v1/responses
```

These do not match:

```text
/models
/chat/completions
```

Multiple fragments use OR semantics; matching any one is sufficient. The query string is not part of `URL.Path`; do not include `?key=...` in a path fragment.

#### Do not over-restrict it

If you enter:

```text
/chat/completions
```

The same credential may fail to yield a Marker when accessing `/v1/models`, `/v1/embeddings`, or other paths, and consequently fall back to the no-Marker policy. Unless you are certain that only particular paths are needed, leaving this empty is less error-prone.

#### Exception for built-in AI platforms

For built-in AI targets recognized by the code, the system first reads the platform account mapping from fixed credential carriers; this built-in path does not depend on `marker_path_parts`. Custom hosts or traffic not recognized as a built-in platform mainly rely on the generic Marker rules here.

### 4.7 `marker_headers`: Marker request-header list

#### Default value

The current default contains four headers:

```text
Authorization
x-api-key
api-key
x-goog-api-key
```

The page permits multiple selections and also permits entering custom header names.

#### Processing order

The program reads the list in order:

```text
The value of the first non-empty request header = Marker
```

Therefore, when a request contains multiple headers, list order affects identity selection. Generally, put the most stable header that best represents the account first.

#### Special rule for Authorization

Only the following form is treated as a Marker:

```http
Authorization: Bearer sk-example
```

The following are not used by the generic Marker extractor:

```http
Authorization: Basic ...
Authorization: Token ...
Authorization: just-a-value
```

Other configured headers (for example, `x-api-key`) are read at their original value.

#### Custom-header example

If your client uses:

```http
X-Workspace-Key: workspace-abc
```

You can add the following to the list:

```text
X-Workspace-Key
```

Header lookup is usually case-insensitive in HTTP, but entering the name as it appears in the client makes troubleshooting easier.

#### Cannot be empty

When saving, the admin console requires the Header list to contain at least one item. If you do not want a default header class to participate in extraction, replace it with your own header instead of clearing the list.

#### Built-in AI credential carriers

For a recognized AI platform, account mapping also checks:

```text
Authorization: Bearer ...
x-api-key: ...
api-key: ...
x-goog-api-key: ...
URL query parameter ?key=...
```

This is built-in behavior, not a switch that can be completely disabled through `marker_headers`. Do not assume that removing `Authorization` on the page prevents a known platform from using it.

### 4.8 `hash_salt`: Global stickiness salt

#### Can it be entered directly?

No. The page displays it read-only and provides:

```text
Reset salt
```

The program generates a random salt on first startup and saves it. Normally, do not change it.

#### What happens when the salt is reset

Sticky identities are derived approximately as follows:

```text
account = truncated hexadecimal result of SHA256(hash_salt + identity input)
```

After clicking “Reset salt” and confirming:

- The derived session identity of every Marker changes;
- The derived session identity of every mapped account changes;
- The `session/sid` seen by upstream platforms changes;
- Accounts are usually assigned new egress IPs;
- Existing sticky relationships are no longer preserved.

This is suitable for replacing all egresses, handling provider-side risk controls, or migrating sessions, but not for routine experimentation. The page asks for a second confirmation precisely because it affects all accounts.

### 4.9 `sid_len`: Derived session ID length

| Item | Value |
|---|---|
| Default value | `16` |
| Currently valid range | `4` to `64` |
| Unit | Number of hexadecimal characters |

For example, when `sid_len=16`, a derived value looks like:

```text
8c12a0f4d7e91b20
```

It is not a byte count; it is the length of the final output string.

**How to choose:**

- `16`: Recommended default; sufficient to distinguish accounts at common scales;
- `24` or `32`: For larger account populations or when a provider prefers longer session IDs;
- `4`: Suitable only for very small tests; not recommended for production;
- `64`: Uses the complete SHA-256 hexadecimal result; usually unnecessary.

**Note:**

The current code validates `4–64`. Some older design text in the repository says `8–32` or `8–64`; follow the current page and API range of `4–64`.

Changing `sid_len` changes the identity string length and may therefore cause upstream sessions to be placed in new buckets; do not adjust it frequently during peak hours.

### 4.10 `session_ttl_min`: Upstream session duration (minutes)

| Item | Value |
|---|---|
| Default value | `0` |
| Page input range | `0–1440` |
| API save validation range | `0–10080` |
| Unit | Minutes |

`0` means:

```text
Do not actively modify an existing TTL parameter in the upstream URL
```

In most cases, keep `0` until the upstream works normally, then set it according to the provider's requirements.

**Important: the page and API limits differ slightly.** The current maximum in the page's numeric input is `1440`, while the automated API's backend validation allows up to `10080`. Do not bypass the page merely to enter a larger number; each provider has a lower maximum of its own, and a Generic template may receive this number unchanged.

#### Actual behavior by platform

| Upstream platform | `session_ttl_min=0` | `session_ttl_min>0` |
|---|---|---|
| Decodo | Preserve the existing `sessionduration` in `base_url` | Write/replace `sessionduration`, limited to `1–1440` |
| 1024proxy | Preserve the existing `t` in `base_url` | Write/replace `t`, limited to `1–120` |
| DataImpulse | Do not inject TTL | The current built-in injector still ignores this setting; an existing `sessttl` is preserved |
| Resin | Not applicable | Not applicable |
| Generic | Rejected if the template contains `{ttl_min}`, because 0 is forbidden | Replace `{ttl_min}` with the current integer |
| plain | Meaningless | Ignored |

For example, if you enter `30`:

- Decodo uses `sessionduration-30`;
- 1024proxy uses `t-30`;
- Generic replaces `{ttl_min}` with `30`;
- DataImpulse does not automatically add `sessttl` because of this setting.

#### Hidden impact of changing TTL

If a Generic template has already been saved:

```json
{"username_template":"{user}-session-{sid}-ttl-{ttl_min}"}
```

You cannot change `session_ttl_min` back to `0`, because `{ttl_min}` is forbidden when the value is 0. When saving settings, the program rechecks existing Generic entries and rejects an incompatible combination.

### 4.11 `salt_rotate_failure_threshold`: Consecutive failures before automatic upstream rotation

| Item | Value |
|---|---|
| Default value | `2` |
| Valid range | `1–100` |
| Meaning | Switch the dynamic salt after the same identity consecutively encounters upstream-unavailable errors that can trigger rotation |

This does not mean “rotate after any HTTP error occurs a certain number of times.” Only the following cases are primarily counted:

- Upstream TLS handshake or certificate failure;
- Failure to establish the upstream connection;
- The upstream proxy refuses to establish CONNECT;
- EOF / unexpected EOF from the peer before a response is received;
- Other transport-layer errors classified as indicating that the current upstream is unavailable.

A `4xx` or `5xx` response normally returned by the target is a genuine application response and is not treated as a reason to rotate upstream; receiving any HTTP response usually clears the previous consecutive-failure count.

#### How to choose

| Value | Suitable scenario | Trade-off |
|---:|---|---|
| `1` | Escape immediately when a provider node fails | A brief network interruption may also trigger rotation |
| `2` | Recommended default; balances responsiveness and stability | May tolerate one additional failure |
| `3–5` | Networks with occasional interruptions where greater stability is preferred | A bad node causes several more waits |
| Large value | Only suitable when deliberately prioritizing session continuity | A genuinely bad node takes longer to replace |

After the threshold is reached, the next request uses a new identity. Salt rotation is recorded per identity; it does not globally reset `hash_salt` on every rotation. Different Markers/accounts do not affect one another.

### 4.12 `default_upstream`: Default upstream

#### How to fill it

Select from the drop-down list an upstream name that has already been added and is **enabled**, for example:

```text
us-residential-1
```

You may also clear it to indicate that there is no default upstream.

#### Who uses it

The default upstream is used for:

- Requests with a Marker but no outbound binding for the account;
- Requests with a Marker whose account mapping does not match;
- Requests without a Marker when `no_marker_policy=default_session` or `client_ip_session`;
- Requests without a higher-priority outbound binding.

#### Validation and risks

If a name is entered, saving requires:

```text
The name exists, and the corresponding entry has enabled=true
```

The current default entry cannot be disabled or deleted directly; you must switch the default first. If manual database changes leave a dangling default name, the program will not silently connect directly. Instead, it returns a controlled 502 response to avoid accidentally exposing the local exit.

#### Meaning after clearing

After clearing `default_upstream`:

- Requests with no binding that would use the default route are handled as having “no upstream,” usually resulting in a direct connection;
- `no_marker_policy=direct` still results in a direct connection;
- Configured account outbound bindings still take priority over the default value;
- If your goal is that “all requests must pass through a proxy,” do not clear it.

### 4.13 `no_marker_policy`: What to do without a Marker

There are currently three values:

| Value | Page name | Behavior |
|---|---|---|
| `default_session` | Fixed identity `default` | All requests without a Marker share one default session identity |
| `client_ip_session` | By source IP | Derive the session identity from the client's source IP |
| `direct` | Direct connection without an upstream | Access the target directly without using a configured upstream proxy |

#### `default_session` (recommended default)

Suitable when:

- Only a small number of requests carry an API key;
- Requests without credentials should still use the same upstream session;
- The local proxy serves one primary client.

The drawback is that all requests without a Marker share one identity. If traffic without a Marker comes from multiple accounts, this option cannot distinguish those accounts.

#### `client_ip_session`

Suitable when:

- Multiple clients connect;
- Clients have no usable API key, but different source IPs can represent different users.

Note: If multiple devices connect through the same NAT, Docker bridge, or reverse proxy, they may share one source IP and therefore one session identity.

#### `direct`

Suitable when:

- Ordinary traffic without an identity should explicitly avoid the residential exit;
- Only traffic carrying a Marker should pass through the upstream;
- Debugging routing.

This applies only to requests “without a Marker.” Even when `direct` is selected, `block_private_targets=true` still prevents access to dangerous targets such as the local machine, private networks, and cloud metadata.

### 4.14 `block_private_targets`: Block access to local and private networks

Default value:

```text
true
```

It is recommended to keep this enabled in production.

When enabled, the program rejects:

- `localhost`;
- Loopback addresses such as `127.0.0.0/8`;
- RFC1918 private addresses;
- IPv6 private/local addresses;
- Link-local addresses;
- Unspecified addresses;
- Multicast addresses;
- `100.64.0.0/10` CGNAT addresses;
- Domain names that resolve to private addresses;
- Common cloud-provider link-local metadata addresses.

It checks not only a literal IP entered by the user, but also the target domain's DNS results, and requires all resolved addresses to be public to reduce opportunities to bypass the check with a domain name.

**This switch is a separate layer from the ACL:**

- The ACL first determines whether the target may be accessed;
- `block_private_targets` then determines whether the target is in a prohibited private-network range;
- An ACL allow does not permit private-network access, and a blacklist cannot bypass private-target protection;
- Even when a request passes through an upstream proxy, the target itself is checked by local security controls first.

When disabled, clients able to connect to MITMRouter may access `localhost`, private-network services, Docker networks, or cloud metadata. Consider disabling it only in a fully isolated environment where private-network access is explicitly required and other security boundaries are in place.

### 4.15 `acl_whitelist`: Target access allowlist

This is a **target-host access allowlist**. It controls which targets may enter the forwarding path.

#### When empty

```text
[] = no allowlist restriction; all targets are allowed, and HTTPS targets are MITM-parsed by default
```

#### When non-empty

Only targets matching the allowlist are allowed; unmatched targets receive a local `403` and are not contacted.
An allowlisted HTTPS target still follows the normal MITM path; this list does not rewrite requests.

For example, to allow and MITM-parse only OpenAI and Anthropic:

```text
api.openai.com
*.openai.com
api.anthropic.com
```

#### Four supported forms

| Form | Example | Meaning |
|---|---|---|
| Single IPv4/IPv6 address | `1.2.3.4`, `::1` | Match this IP |
| CIDR network | `10.0.0.0/8`, `2001:db8::/32` | Match this address range |
| Exact domain | `api.openai.com` | Match only this domain |
| Wildcard domain | `*.openai.com` | Match a subdomain at any level, but not `openai.com` itself |

#### Matching details

- Matching is case-insensitive;
- Whitespace at both ends is removed;
- A trailing root dot on a domain is removed;
- Do not include a scheme: `https://api.openai.com` is invalid;
- Do not include a port: `api.openai.com:443` is invalid;
- Do not include a path or query string;
- Domain matching uses only the target's literal hostname/IP; domain entries and IP entries are not converted into one another;
- A single list may contain at most 500 entries.

#### Examples

```text
Correct: api.openai.com
Correct: *.openai.com
Correct: 10.0.0.0/8
Correct: [IPv6 addresses do not need a port when entered]
Wrong: https://api.openai.com
Wrong: api.openai.com:443
Wrong: /v1/chat/completions
```

### 4.16 `acl_blacklist`: Target access blacklist

The priority is simple: a blacklist match rejects access, even when the target also matches the allowlist:

```text
A blacklist match → local 403; reject access
```

The overall decision order is:

```text
1. Blacklist match                     → local 403; reject access
2. Whitelist is non-empty and no match → local 403; reject access
3. All other cases                     → allow; MITM-parse HTTPS traffic
```

Therefore, the blacklist always takes precedence over the whitelist. For example:

```text
Whitelist: *.example.com
Blacklist: private.example.com
```

When accessing `private.example.com`, the client receives a local 403; the target is not contacted or MITM'd.

This is suitable for the following scenarios:

- Excluding a target that should not be accessed from an allowed range;
- Blocking a stale or misconfigured target;
- Narrowing the ingress access range together with the allowlist.

Again: both lists are access controls; `block_private_targets` is a separate private-network safety check.

### 4.17 `log_retention_days`: audit and update record retention

| Item | Value |
|---|---|
| Default value | `30` |
| Valid range | `1–3650` |
| Unit | days |

The program performs cleanup once at startup and then approximately once a day. The cleanup covers:

- Access audit records;
- Account-mapping update records.

It does not delete:

- Upstream configuration;
- The current account-mapping snapshot;
- The Marker dynamic salt;
- The MITM CA;
- The administrator password hash.

#### How to choose

| Days | Suitable scenarios |
|---:|---|
| `1–7` | Disk is tight; only short-term troubleshooting is needed |
| `30` | Recommended default for routine operations |
| `90–180` | Longer-term trend analysis and problem investigation |
| `365` and above | Compliance or long-term auditing; first confirm database growth and access controls |

Audit records contain metadata only; they do not store request bodies or request headers. If full-content troubleshooting is needed, temporarily use `-trace-file`; do not treat the retention period as a switch for recording plaintext traffic.

### 4.18 `metrics_enabled`: Prometheus `/metrics`

Disabled by default:

```text
false
```

When enabled:

```text
GET /metrics
```

provides Prometheus text-format metrics, and still requires an authenticated admin-console session. When disabled, access returns 404 rather than exposing the metrics.

Use it as follows:

```text
Enable: Prometheus/monitoring is in use and admin-console access is restricted
Disable: Metrics are not needed, or the admin-console exposure boundary is not ready
```

There are currently no additional port, path, username, or label configuration options. Metrics use the management plane corresponding to `-admin-addr`, not the client entry point.

### 4.19 `sync_empty_clear_threshold`: empty-sync snapshot protection threshold

| Item | Value |
|---|---|
| Default value | `3` |
| Valid range | `1–100` |
| Scope | When a full API sync returns an empty account list |

When a sync source connects successfully but returns 0 accounts, the program does not immediately delete existing mappings; instead, it counts consecutive empty snapshots.

For example, with a threshold of `3`:

```text
1st successful response with 0 accounts → retain old accounts; record 1/3
2nd successful response with 0 accounts → retain old accounts; record 2/3
3rd successful response with 0 accounts → only then clear this source's account mappings
```

If a sync fails because of a connection failure, HTTP error, or parse failure, it is not counted as an empty snapshot, and the failure does not clear the accounts.

#### How to choose

| Value | Behavior |
|---:|---|
| `1` | Clear immediately; suitable when an empty source list means there truly are no accounts |
| `3` | Recommended default; tolerates transient disconnects or upstream anomalies |
| `5–10` | The source occasionally returns an empty list and a more conservative policy is desired |
| Large values | Stronger protection, but genuine account deletion is also delayed |

When an account actually disappears from the mapping table, its associated account egress bindings are also removed recursively. Thus, this threshold protects not only account mappings but also their egress bindings.

### 4.20 `acctmap_enabled`: account-mapping switch (not currently a page input)

Default value:

```text
true
```

This is a database-compatibility setting, but the current basic-settings page does not expose a switch for it. The settings-save API also preserves the current value and will not silently disable it because an old client omitted the field.

When enabled:

```text
After a matching AT/RT fingerprint that was synchronized or registered manually, derive a sticky identity from “platform + account”
```

This means that after AT rotation, the egress identity can remain unchanged as long as it still belongs to the same account.

When disabled:

```text
Return to pure Marker-hash logic; a change in the token itself may change the egress identity
```

For normal use, do not manually modify this key or edit `router.db` directly. If you are not using account mapping, keep the default value.

---

## 5. Upstream egress parameters: `name`, `platform`, `base_url`, `inject`, `enabled`

Location: Admin console → **Upstream egress** → Add/Edit.

Each upstream entry represents an available HTTP/HTTPS/SOCKS5 egress proxy. Filling in this section does not rewrite the business request target URL, business request headers, or response content; it determines which egress the router connects to and whether a sticky identity is injected into the egress proxy username.

### 5.1 `name`: Entry name

**How to fill it:**

Enter a unique name that is clear to you. For example:

```text
us-residential-main
us-residential-backup
decodo-rotating
plain-office-proxy
```

**Rules:**

- It cannot be empty when creating an entry;
- It must be unique;
- Letters, numbers, hyphens, and underscores are recommended;
- Avoid leading or trailing spaces;
- This is not the provider username; do not enter the complete `base_url` here;
- The name appears in the default route, audit records, and update history.

The name is only an administrative identifier and does not participate in platform credential injection. Renaming an entry does not change its platform session ID, but if it is the current default upstream, the program updates the default reference accordingly.

### 5.2 `platform`: Platform type; select the correct value

Current platform options:

```text
dataimpulse
decodo
1024proxy
resin
generic
plain
```

This field is not a decorative label; it selects how the upstream username is rewritten. If it is wrong, the most troublesome case is:

```text
Upstream authentication still succeeds, but session parameters are not injected using the provider's syntax
```

It may appear to work while stickiness has actually stopped working.

Selection principle:

| Your upstream | Select platform |
|---|---|
| DataImpulse | `dataimpulse` |
| Decodo (formerly Smartproxy) | `decodo` |
| 1024proxy | `1024proxy` |
| Resin gateway | `resin` |
| Another platform using a username template | `generic` |
| Ordinary HTTP/SOCKS5 proxy that needs no session injection | `plain` |

### 5.3 `base_url`: Upstream proxy address and credentials

#### Basic format

```text
<scheme>://[username:password@]<proxy-host>:<port>
```

Examples:

```text
http://user:pass@proxy.example.com:8080
https://user:pass@gateway.example.com:8443
socks5://user:pass@proxy.example.com:1080
socks5h://user:pass@gateway.example.com:1080
```

Schemes currently allowed by the code:

```text
http
https
socks5
socks5h
```

A host is required. When saving, the program checks that the URL parses, the host is non-empty, and the scheme is allowed. It does not check whether the provider domain, port, plan, or API key is genuinely valid; provider-specific values must be entered exactly as generated by the provider console.

#### URL encoding for username and password

The username and password are in the URL userinfo section. If real credentials contain any of these characters:

```text
@ : / ? # %
```

encode them according to URL userinfo rules. For example, do not write a password containing `@` directly as:

```text
http://user:p@ss@example.com:8080
```

Use the corresponding percent-encoded form:

```text
http://user:p%40ss@example.com:8080
```

The safest approach is to copy the proxy URL generated by the provider console rather than reconstructing it manually.

#### Meaning of each scheme

- `http://`: connect through the HTTP proxy protocol;
- `https://`: use a protected HTTPS connection to the upstream proxy;
- `socks5://`: connect through SOCKS5;
- `socks5h://`: accepted as input and normalized to SOCKS5 transport at runtime, with domain resolution delegated to the SOCKS5 egress.

The scheme describes the connection method to the upstream proxy, not the final target site's protocol. HTTPS targets are still handled through standard CONNECT/stream forwarding.

#### Masking when editing an existing entry

Passwords in the list are displayed as:

```text
http://user:____@proxy.example.com:8080
```

Saving this mask unchanged after editing makes the program retain the old password. Programmatic updates also support:

```text
____
__unchanged__
```

Both values mean “retain the old password”; neither is a new real password. New entries require real credentials; the mask does not generate a password automatically.

### 5.4 `inject`: Generic template

Only `platform=generic` uses `inject`. The page accepts JSON text, for example:

```json
{"username_template":"{user}-session-{sid}","password":"<optional static password>"}
```

#### Fields

| JSON field | Required | Description |
|---|---:|---|
| `username_template` | Yes | Template written to the final upstream username |
| `password` | No | When non-empty, overrides the password in `base_url`; when empty, retains the `base_url` password |

Only four placeholders are allowed:

| Placeholder | Replacement |
|---|---|
| `{user}` | The original username from `base_url` |
| `{sid}` | The derived session identity, usually a lowercase hexadecimal string |
| `{ttl_min}` | The integer in the basic setting `session_ttl_min` |
| `{country}` | Reserved by the current version; always empty in practice |

The template uses string replacement. It is not a Go/Python expression and cannot contain arbitrary variables or functions.

#### Valid examples

```json
{"username_template":"{user}-session-{sid}"}
```

```json
{"username_template":"{user}-session-{sid}-sessionduration-{ttl_min}"}
```

```json
{"username_template":"{user}__sessid.{sid}","password":"<static password>"}
```

The second example requires:

```text
session_ttl_min > 0
```

Otherwise saving reports that the template uses the disabled `{ttl_min}` placeholder.

#### Invalid examples

```json
{"username_template":"{username}-{sid}"}
```

`{username}` is not a supported placeholder.

```json
{"username_template":"{user}-{sid"}
```

The braces are not closed.

```json
{"username_template":"{user}}-{sid}"}
```

The braces do not match.

```json
{"username_template":"{user}-{ttl_min}"}
```

This cannot be saved when `session_ttl_min=0`.

#### Generic password precedence

```text
Non-empty inject.password → use inject.password
Empty or absent inject.password → retain the base_url password
```

When editing a Generic entry, `inject.password` is also shown as `____`. Retain it to preserve the old password; do not submit the mask as a real new password.

#### Other JSON fields

Only `username_template` and `password` have defined semantics at present. Do not put other fields from a provider's API documentation into `inject` unless you have confirmed that the Generic template depends only on username and password; unknown fields do not automatically produce additional behavior.

---

## 6. How to configure the six upstream platforms

The `<...>` items below are placeholders; replace them with the actual values from the provider console when deploying.

### 6.1 DataImpulse: `platform=dataimpulse`

#### Recommended format

HTTP example:

```text
base_url = http://<login>__cr.us:<password>@gw.dataimpulse.com:823
```

SOCKS5 example (use the port specified by the provider console):

```text
base_url = socks5://<login>__cr.us:<password>@gw.dataimpulse.com:824
```

If the console provides a different region, port, or parameter, follow the console. The code checks only the generic URL format and does not require the host to be `gw.dataimpulse.com`.

#### How the username is rewritten

Common DataImpulse username syntax:

```text
<login>__<key>.<value>;<key>.<value>
```

The program:

1. Finds `__` in the username;
2. Treats the part before the double underscore as the login name;
3. Removes any existing `sessid.*` segment;
4. Appends this at the end:

```text
sessid.<current sid>
```

For example, enter:

```text
<login>__cr.us
```

The username logically sent to the upstream is similar to:

```text
<login>__cr.us;sessid.8c12a0f4d7e91b20
```

Other existing parameters are preserved.

#### TTL notes

The built-in DataImpulse injector currently injects only `sessid` and **ignores `session_ttl_min`**. If `base_url` already contains `sessttl.*`, the program does not remove it, but it also does not automatically add or modify it from the global TTL.

Therefore:

- If you only want to use `sessid`, keeping the global TTL at `0` is clearest;
- If you need DataImpulse `sessttl`, write it directly into `base_url` using the provider syntax, and confirm that the port and plan support it;
- Do not assume that setting the global TTL to 30 automatically produces `sessttl.30`.

#### Common mistakes

```text
Treating __sid.xxx as the official sessid syntax
Putting sessionduration in the DataImpulse username
Putting the region code or account credentials after the wrong separator
```

When the platform selection is wrong, DataImpulse may still authenticate successfully, but the session parameters may not be effective.

### 6.2 Decodo: `platform=decodo`

#### Recommended format

```text
base_url = http://user-<login>-country-us:<password>@gate.decodo.com:7000
```

You may also retain additional location parameters generated by the console:

```text
base_url = http://user-<login>-country-us-city-chicago:<password>@gate.decodo.com:7000
```

For SOCKS5, you can use a form supported by the provider, for example:

```text
base_url = socks5h://user-<login>-country-us:<password>@gate.decodo.com:7000
```

#### Most important username rule

The username must begin with:

```text
user-
```

The following will be rejected:

```text
<login>-country-us
```

The program injects or replaces:

```text
-session-<current sid>
```

If `session_ttl_min>0`, it also injects or replaces:

```text
-sessionduration-<minutes>
```

For example, with a global TTL of `30`, the final logical form is similar to:

```text
user-<login>-country-us-session-8c12a0f4d7e91b20-sessionduration-30
```

#### TTL range

The built-in injection range supported by Decodo is `1–1440` minutes. The program caps larger values at 1440.

If the global TTL is `0`:

- An existing `sessionduration-*` is preserved;
- If none exists, the provider's default behavior is used;
- `session` is still injected because it is the key to sticky identity.

#### Provider behavior reminder

Decodo sessions have provider-side lifetime limits; common documentation mentions a default of about 10 minutes and expiration after 60 seconds of inactivity. The same sid in MITMRouter can only “make a best effort to keep the same provider session”; it cannot make a residential node that the provider has already reclaimed remain unchanged forever.

### 6.3 1024proxy: `platform=1024proxy`

#### Recommended format

It is recommended to copy the complete username generated by the console. A typical format is:

```text
base_url = socks5://<apikey>-region-US-sid-placeholder-t-5:<password>@us.1024proxy.io:3000
```

HTTP may use the same host:port, with only the scheme changed to the form required by the provider:

```text
base_url = http://<apikey>-region-US-sid-placeholder-t-5:<password>@us.1024proxy.io:3000
```

Use the host, port, region, and plan specified by the console.

#### How the username is rewritten

Common 1024proxy syntax:

```text
<apikey>-region-<CC>-sid-<sessid>-t-<minutes>
```

The program replaces or adds:

```text
sid-<current sid>
```

If `session_ttl_min>0`, it also replaces or adds:

```text
t-<minutes>
```

When the TTL is `0`, the program preserves the `base_url`'s existing `t` value. If there is none, the final behavior is determined by the provider, so in production it is recommended to retain the complete username generated by the console.

#### TTL range

The built-in injector limits the TTL to `1–120` minutes. Your plan may be narrower, such as `3–30` minutes for some port plans. The program does not know your specific plan; the provider decides whether the final value is accepted.

#### Parameters not to delete

If the console username contains:

```text
region-US
st-...
city-...
```

Do not arbitrarily delete location or routing parameters to “simplify” them. The program only recognizes and rewrites known session keys; other parts are generally preserved as-is.

### 6.4 Resin: `platform=resin`

#### Recommended format

```text
base_url = socks5://Default:<RESIN_TOKEN>@resin:2260
```

You may also use the actual Resin host and port from your deployment:

```text
base_url = http://<Platform>:<RESIN_TOKEN>@<resin-host>:<port>
```

#### Username rules

Common Resin usernames are:

```text
Platform
Platform.Account
```

MITMRouter takes the portion before the first `.` as the platform name, then writes its own identity:

```text
Platform.<current sid>
```

For example:

```text
base_url username: Default
```

It logically becomes:

```text
Default.8c12a0f4d7e91b20
```

If the original username is:

```text
Default.old-account
```

The old `old-account` is discarded and replaced with the current sid.

#### Username is required

The current Resin injector requires a non-empty username, and each injection requires a non-empty current account. Therefore, do not enter:

```text
socks5://:<RESIN_TOKEN>@resin:2260
```

If you need a regular Resin proxy with “credentials exactly as provided and no session injection,” consider using `plain`; however, it will not provide this project’s Resin session injection feature.

Enter the Resin token as the password; the program does not put the session ID in the password.

### 6.5 Generic: `platform=generic`

Use Generic when the provider is not one of the four built-in providers and puts the session parameter in the username.

Two parts must be entered:

```text
base_url: the provider's original proxy URL
inject:   JSON describing how to generate a new username from the original username and sid
```

For example, if the provider syntax is:

```text
<user>-session-<session_id>
```

You can enter:

```text
base_url = http://<user>:<password>@gateway.example.com:8080
inject = {"username_template":"{user}-session-{sid}"}
```

If the provider syntax is:

```text
<user>-session-<session_id>-ttl-<minutes>
```

enter:

```text
inject = {"username_template":"{user}-session-{sid}-ttl-{ttl_min}"}
```

and set the global `session_ttl_min` to a value greater than 0.

Generic does not:

- automatically recognize your provider's parameter names;
- automatically check region codes;
- automatically check whether the port matches the plan;
- automatically turn `{country}` into a particular country;
- automatically modify business request content.

You are responsible for all of these based on the provider documentation.

### 6.6 plain: `platform=plain`

`plain` means a regular proxy with no session injection.

#### Examples

```text
base_url = http://user:pass@proxy.example.com:8080
```

```text
base_url = socks5://user:pass@proxy.example.com:1080
```

#### Rules

- `inject` must be empty;
- Credentials and the URL are used exactly as provided;
- It does not inject `sid`, session, or TTL;
- `session_ttl_min` has no meaning for it;
- It can be used for ordinary fixed proxies, office egresses, or IP-allowlisted proxies.

The special purpose of `plain` is binding an account to multiple ordinary outbound connections. Only `plain` entries can appear in the account outbound-binding selector; see Section 8.

If you set a `plain` entry as the default upstream, all requests without a higher-priority binding use this ordinary proxy; it does not provide provider-side Marker-based stickiness.

---

## 7. Account management: sync-source parameters

Account management has two paths with different purposes:

```text
Full sync: periodically pull from the management APIs of CLIProxyAPI / Sub2API
Incremental sync: directly watch the CPA authentication-file directory, or read Sub2API PostgreSQL directly
```

Both can be configured simultaneously. Full sync is responsible for periodically aligning all data; the incremental path is responsible for detecting AT/RT changes more quickly.

### 7.1 When account mapping is needed

If the client directly uses a stable API Key, account management is usually unnecessary:

```text
API Key → Marker hash → sticky egress
```

If the client uses rotating Bearer AT/RT and you want:

```text
The egress to remain unchanged after the token rotation of the same subscription/account
```

you should map AT/RT to the real account:

```text
AT/RT fingerprint → platform + account → account-level sticky egress
```

### 7.2 Sync source `kind`: source type

There are currently only two values:

| Page name | Stored value | Purpose |
|---|---|---|
| CLIProxyAPI | `cpa` | Read through the CLIProxyAPI management API or authentication directory |
| Sub2API | `sub2api` | Read through the Sub2API management API or PostgreSQL |

Do not put the page display name `CLIProxyAPI` in the `kind` field; the programmatic API must use `cpa`.

### 7.3 Sync source `name`: source instance name

Enter a unique, recognizable instance name, for example:

```text
cpa-production
cpa-test
sub2api-main
sub2api-vps-1
```

**Rules:**

- Must not be empty.
- Must be unique.
- This identifies the sync-source instance; it is not the vendor account name.
- When a sync source is deleted, account mappings under that source name are deleted in cascade.
- Account-management update records show this name.

### 7.4 Sync-source `base_url`: Management API root URL

#### General rules

It must be a clean HTTP or HTTPS root URL:

```text
http://127.0.0.1:8317
https://sub2api.example.com
```

The program will:

- remove a trailing `/`;
- require the scheme to be `http` or `https`;
- require a host;
- reject usernames/passwords in the URL;
- reject a query;
- reject a fragment.

Therefore, do not enter values like these:

```text
https://user:pass@sub2api.example.com       # URL credentials; rejected
https://sub2api.example.com?key=secret       # query present; rejected
https://sub2api.example.com#admin            # fragment present; rejected
https://sub2api.example.com/api/v1/...       # usually enter the service root; the program builds the path itself
```

Enter the API key separately in `api_key`; do not put it in the URL.

### 7.5 Sync-source `api_key`: Management API key

#### Creating a source

This is required when creating a source. Enter:

- the CLIProxyAPI management key;
- the Sub2API admin API key.

Do not enter a client-facing business API key unless it is actually the key required by the corresponding management API.

#### Editing a source

When editing an existing source, an empty input means:

```text
Keep the original API key in the database
```

The page does not display the real key. Do not mistake “leave blank to keep unchanged” for clearing the key.

#### How the program uses it

CLIProxyAPI:

```http
Authorization: Bearer <management-key>
```

Sub2API:

```http
x-api-key: <admin-api-key>
```

The key is stored in the secrets table. It must not be placed in `base_url`, the source name, or query parameters.

### 7.6 CLIProxyAPI full-sync fields

Select:

```text
kind = cpa
```

Fill in:

```text
name      = cpa-production
base_url  = https://<CLIProxyAPI address>
api_key   = <management-key>
```

The program makes calls similar to:

```text
GET <base_url>/v0/management/auth-files
GET <base_url>/v0/management/auth-files/download?name=<file name>
```

Common providers/platforms it recognizes include:

| CLIProxyAPI provider/type | Normalized platform |
|---|---|
| `codex`, `openai` | `openai` |
| `claude`, `anthropic` | `anthropic` |
| `gemini`, `antigravity` | `gemini` |
| `xai`, `grok` | `grok` |
| `kimi` | `kimi` |
| `qwen` | `qwen` |
| `iflow` | `iflow` |

Unknown providers are skipped and do not enter the account mapping.

Authentication files must contain:

- an account identifier (usually `email`, `email_address`, or similar);
- at least one of `access_token` or `refresh_token`.

A file with no account or no usable credential does not create a mapping row.

### 7.7 Sub2API full-sync fields

Select:

```text
kind = sub2api
```

Fill in:

```text
name      = sub2api-main
base_url  = https://<Sub2API address>
api_key   = <admin-api-key>
```

The program calls:

```text
GET <base_url>/api/v1/admin/accounts/data
```

Authentication header:

```http
x-api-key: <admin-api-key>
```

The program includes only these two account types in the AT/RT mapping:

```text
oauth
setup-token
```

Types such as `apikey`, `upstream`, and `bedrock` that have no usable AT/RT mapping are skipped.

Supported primary platform normalization rules:

| Sub2API platform | Normalized platform |
|---|---|
| `openai`, `codex` | `openai` |
| `anthropic`, `claude` | `anthropic` |
| `gemini` | `gemini` |
| `grok`, `xai` | `grok` |
| `kimi`, `moonshot` | `kimi` |
| `deepseek` | `deepseek` |
| `zhipu`, `glm` | `glm` |
| `ollama` | `ollama` |

The account identifier is taken from the credential's email first; if unavailable, the account name is used. Account identifiers are normalized to lowercase.

### 7.8 `interval_s`: Full synchronization interval (seconds)

| Item | Value |
|---|---|
| New-source default | `600` seconds, or 10 minutes |
| Minimum | `60` seconds |
| Meaning | Minimum interval between full API synchronizations |

Recommended:

```text
600
```

If upstream credentials are updated very frequently, consider:

```text
120
300
```

Do not set this to a few seconds. The program raises values below 60 to 60 to avoid overwhelming the source with frequent requests.

This is the interval for **full synchronization**, not incremental synchronization:

- CPA directory increments are triggered by file events;
- Sub2API PostgreSQL increments are currently polled at a fixed interval of about 3 seconds;
- The page has no separate incremental interval parameter.

When editing an existing source, omitting this field from the programmatic request or sending `0` usually preserves the old value; when creating a source, a non-positive value falls back to the default of 600.

### 7.9 Sync-source `enabled`: Enable/disable

When enabled:

- It participates in scheduled full synchronization;
- If an incremental path is configured, the corresponding incremental reader starts;
- Synchronization results update the account mappings.

When disabled:

- Scheduled full synchronization stops;
- The corresponding incremental reader stops;
- Account mappings already written are not immediately deleted merely because the source was disabled.

If you want to completely clean up a source's data, delete the sync source; deletion also removes its mappings.

### 7.10 `direct_auth_dir`: CPA authentication-file directory (optional)

Only `kind=cpa` may specify this field.

Example:

```text
/opt/cliproxyapi/auths
```

#### Requirements

- Must be an absolute path;
- The directory must already exist;
- Must be a directory, not a file;
- The root directory must not be a symbolic link;
- The MITMRouter process must have permission to read and watch it.

Setting this field enables CPA incremental synchronization:

```text
Authentication file changes → read JSON → extract account and AT/RT → update mappings
```

The program recursively watches the directory and its subdirectories, with special attention to `.json` files. Files that are too large, empty, symbolic links, changed while being read, or contain JSON outside the supported formats are skipped and logged as errors; a temporarily malformed read is not treated as account deletion.

Clearing this path disables CPA incremental synchronization, but does not affect full API synchronization.

### 7.11 `direct_db_dsn`: Sub2API PostgreSQL connection string (optional)

Only `kind=sub2api` may specify this field.

#### Local/same-host database example

```text
postgres://readonly:<password>@127.0.0.1:5432/sub2api?sslmode=disable
```

You may also use `localhost` or a Unix socket form, depending on the pgx/PostgreSQL environment.

#### Remote database example

```text
postgres://readonly:<password>@db.example.com:5432/sub2api?sslmode=verify-full
```

The remote host must use a secure TLS configuration. The current code requires the remote DSN to have an effect equivalent to:

```text
sslmode=verify-full
```

It must also perform correct certificate verification and server-name validation. Do not use:

```text
sslmode=disable
sslmode=require without complete hostname verification
sslmode=verify-full when the certificate/SAN/DNS does not match
```

#### Database-account permissions

A read-only account is recommended; it only needs to read the relevant fields from Sub2API's `accounts` table. Incremental reads check:

- `updated_at`;
- `type`;
- `platform`;
- `deleted_at`;
- email, access_token, and refresh_token in `credentials`.

The DSN is stored in secrets and is not echoed back in the page.

#### Editing and clearing

When editing an existing Sub2API source:

- Leave the DSN input blank to preserve the saved DSN;
- Select “Clear saved connection string” to delete the DSN and disable incremental reading;
- `direct_db_clear=true` and a non-empty `direct_db_dsn` cannot be submitted together.

### 7.12 Incremental paths are not a global switch

There is currently no global `incremental_enabled` input. Whether an incremental reader runs is determined by both conditions:

```text
Sync source enabled=true
and the source has the corresponding incremental path configured
```

Therefore:

```text
cpa + direct_auth_dir non-empty       → CPA file increment
sub2api + direct_db_dsn non-empty     → Sub2API database increment
Path empty                            → No incremental reader runs
```

### 7.13 Empty-snapshot protection and sync sources

`sync_empty_clear_threshold` only protects against a full API synchronization returning an empty list. It does not change:

- Complete replacement by a normal non-empty snapshot;
- Retention behavior when an API request fails;
- How the incremental reader handles updates to individual accounts.

If you see a status like:

```text
ok: empty snapshot deferred 1/3
```

It means the connection succeeded but the empty list was protected; it does not mean that the current mappings have been deleted.

---

## 8. Manual Account Registration Parameters

Entry point: Management Console → Account Management → **Manual Registration**.

Manual registration is suitable when you:

- have no available synchronization source;
- only need to register a small number of accounts;
- want to push account credentials through the API;
- want to test account mappings and outbound bindings.

### 8.1 `platform`: Platform Owning the Account

Built-in options in the UI:

```text
openai
anthropic
gemini
grok
kimi
deepseek
glm
qwen
iflow
ollama
```

The UI allows custom input, but production configurations should use the canonical lowercase platform names defined in the code.

The platform must match the target host classification. For example:

```text
openai   → openai.com / chatgpt.com
anthropic → anthropic.com / claude.ai / claude.com
grok    → x.ai / grok.com
```

If you enter a custom platform name but the target domain is not recognized by a built-in platform, the program generally cannot map requests to that custom platform. Custom values are reserved mainly for extension scenarios; entering an arbitrary name does not make it work automatically.

### 8.2 `account`: Stable Account Identifier

Required. The recommended value is:

```text
user@example.com
```

It may also be:

```text
A subscription UUID
A provider account ID
A stable account string that you define
```

Do not use:

```text
The current access token
The current refresh token
A random value
```

This field is intended to continue identifying the same account after token rotation.

The program:

- removes leading and trailing whitespace;
- converts the value to lowercase;
- uses the combination of platform and account as part of the account-level identity.

Therefore, the following two inputs are treated as the same account:

```text
User@example.com
user@example.com
```

### 8.3 `source_type`: Credential Source Type

Required, with a maximum length of 64 characters. Built-in UI options:

```text
CLIProxyAPI
Sub2API
```

You may also enter a custom value, for example:

```text
manual-import
team-a
backup-export
```

It is mainly used to:

- group entries on the account management page;
- count mapping rows from different sources;
- track the origin of manual pushes.

It is not the name of an upstream proxy platform, nor a machine enum that must match `kind`. Use a meaningful name that operators can understand.

### 8.4 `access_token`: AT

Optional, but at least one of `access_token` and `refresh_token` must be provided.

Paste the actual token value directly, without quotation marks, and do not put it in the account name. Before saving, the program performs the necessary whitespace and common scheme normalization, then stores a fingerprint instead of the complete token in plaintext.

The UI does not echo the complete token; it shows only a suffix hint, for example:

```text
…a1b2
```

This is not a token that can be used to log in again; it only helps you confirm whether the credential has changed.

### 8.5 `refresh_token`: RT

The same rules apply as for AT:

- optional;
- RT may be provided by itself;
- at least one of AT and RT must be provided;
- a fingerprint and suffix hint are stored;
- after rotating RT, register the account again or update it through a synchronization source.

Typical input:

```text
platform   = openai
account    = user@example.com
source_type = manual-import
access_token = <AT>
refresh_token = <RT>
```

### 8.6 Manual Registration Does Not Display Tokens in Plaintext in the List

The account mapping table displays:

- platform;
- account;
- the trailing hints of the AT/RT fingerprints;
- source;
- update time;
- outbound binding.

Complete tokens must not appear in the management console list, audit records, or ordinary runtime logs. Treat the input field, browser autofill, reverse-proxy access logs, and terminal history as sensitive information as well.

### 8.7 Impact of Manually Deleting an Account

Deleting an account removes mapping rows matching that account. By default, rows from all sources are deleted; if the API `source` query parameter specifies a source, only rows from that source are deleted.

When the account no longer exists in any source:

```text
The account's plain outbound binding is also garbage-collected.
```

Before deleting an account, confirm whether its outbound binding semantics still need to be retained.

---

## 9. Account ↔ outbound binding parameters

Outbound binding uses ordinary proxy entries of type `plain` to assign an account on a platform to several ordinary egresses, either stably or randomly.

It takes precedence over `default_upstream`:

```text
Account binding hit      → use the bound plain outbound
Account not bound        → follow the default route
```

### 9.1 Why it must be `plain`

The binding feature itself manages which ordinary egress to select:

- `plain` preserves each proxy's original credentials;
- the binding layer selects among multiple `plain` outbounds;
- platforms such as DataImpulse and Decodo no longer inject session parameters.

Entries for other platforms do not appear in the binding selector. To use a provider's own username-based stickiness, configure it as the default upstream or an ordinary default route, rather than as a `plain` account binding.

### 9.2 `mode`: binding mode

There are only two values:

| Value | Page label | Behavior |
|---|---|---|
| `sticky` | Sticky | Stably choose one egress for the account from the selected set |
| `random` | Random | Randomly choose one enabled egress from the selected set on every request |

#### `sticky`

Suitable when you:

- want the account to keep a fixed egress IP;
- want the choice to drift as little as possible after a restart;
- have many egresses and want stable distribution across multiple lines.

The program selects an egress using a stable score based on the account and outbound ID. Changing the salt or the binding set may cause a new selection.

#### `random`

Suitable when you:

- do not require the same account to keep a fixed IP;
- want to distribute requests across multiple ordinary egresses;
- need simple load sharing.

Each request may use a different egress. If only one egress is enabled, random and sticky mode have no practical difference.

### 9.3 Account-side binding: `egress_ids`

When you click “Bind outbound” on an account row:

1. Select `sticky` or `random`;
2. Check one or more `plain` outbounds;
3. Save.

`egress_ids` is an array of database IDs for outbound entries, for example:

```json
{
  "mode": "sticky",
  "egress_ids": [3, 7, 9]
}
```

“Clear selected” on the page clears all bindings for the account, causing it to fall back to the default route.

### 9.4 Outbound-side binding: `accounts`

When you batch-select accounts through “Associated accounts” for a `plain` outbound, the API shape is:

```json
{
  "mode": "sticky",
  "accounts": [
    {"platform":"openai","account":"user@example.com"},
    {"platform":"anthropic","account":"claude@example.com"}
  ]
}
```

The current page supports:

- filtering by platform;
- fuzzy search by platform/account;
- pagination;
- showing selected only;
- selecting all on the current page;
- inverting the selection on the current page;
- preserving selections across pages.

The meaning of saving is:

```text
This plain outbound has exactly the account set submitted this time
```

Accounts not submitted this time are unbound from that outbound.

### 9.5 Effect of batch mode on existing bindings

When associating accounts in batch from the outbound side, the page's `mode`:

- applies to accounts newly added to that outbound;
- leaves the existing mode unchanged for accounts already bound to that outbound.

To change a mode that already exists for an account, open “Bind outbound” from the account side and save.

### 9.6 Relationship between disabling, deleting, and binding

- Deleting a `plain` outbound: account bindings that reference it are deleted in cascade;
- disabling a `plain` outbound: binding rows may remain temporarily, but at runtime it is not an available candidate;
- if all outbounds bound to an account are missing or disabled: the request fails in a controlled manner and does not silently fall back to the default upstream;
- when an account disappears from all sources: its account binding is garbage-collected;
- deleting a mapping row from one source does not necessarily immediately delete the account binding, as long as another source still retains the same platform/account.

Therefore, before disabling or deleting an outbound, first filter the affected bindings on the account management page.

---

## 10. Mapping between account platforms and target hosts

For an account mapping to take effect, the platform identified from the target host must match the registered `platform`. The built-in classifications currently include:

| Account platform | Example built-in target suffixes |
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

The approximate reading order for built-in platform credentials is:

```text
Authorization: Bearer ...
x-api-key: ...
api-key: ...
x-goog-api-key: ...
URL ?key=...
```

If none of these is present and the target URL matches a built-in body-parsing rule, the program may also read credentials from a specific request body; this rule is not a management-console parameter and currently mainly covers the Grok OAuth refresh-token path.

Unknown domains do not automatically imply a platform. For an unknown host:

- the generic `marker_headers` can still provide a plain Marker;
- there is usually no built-in platform available for matching the account table;
- requests can still use plain-Marker hash stickiness.

---

## 11. Access Audit Page Query Parameters

These are query conditions, not persistent configuration. Entry point: Admin Console → **Access Audit**.

### 11.1 Time range `range`

Page options:

| Value | Meaning |
|---|---|
| `1h` | The last 1 hour |
| `24h` | The last 24 hours, default |
| `7d` | The last 7 days |
| `all` | All time |

The program ultimately converts this into Unix millisecond timestamps for `from` / `to`.

### 11.2 Keyword `q`

Enter part of the target host or path, for example:

```text
openai.com
/v1/chat
```

This performs substring matching on the host/path, not regular-expression matching. The query checks both:

```text
host LIKE %q%
path LIKE %q%
```

### 11.3 Account or session `account`

This is an exact match. You can enter:

- The real account identifier;
- The complete `account_fp`/derived session ID.

This is not a fuzzy search. The short suffix shown on the page is only a hint; when troubleshooting, it is best to copy the complete value or filter directly by the real account.

### 11.4 Upstream `upstream`

This is an exact match on the upstream entry name, for example:

```text
us-residential-main
plain-office-proxy
```

Special values may also appear:

```text
direct
blind
```

### 11.5 Status `class`

Page options:

| Value | Meaning |
|---|---|
| `2xx` | A 200–299 response from the real target/upstream |
| `4xx` | A 400–499 response from the real target |
| `5xx` | A 500-or-higher response from the real target |
| `err` | An internal error classified by MITMRouter itself |

In the audit, `status=0` means that the request failed before a real HTTP response was received, for example because the upstream connection failed, a private-network target was blocked locally, or the configuration was invalid. The client may receive 502, but the audit does not disguise a failure generated locally as an upstream 5xx.

### 11.6 Pagination

| Parameter | Default | Effective behavior |
|---|---:|---|
| `page` | `1` | Values below 1 are treated as 1 |
| `page_size` | `50` | Values less than or equal to 0 or greater than 200 are treated as 50 |

“Auto-refresh” means that the page runs the query again every 5 seconds; it is not a server-side persistent setting.

---

## 12. Update History Page Query Parameters

Update history tracks changes to account mappings separately from the access audit.

### 12.1 `kind`

Current values:

| Value | Meaning |
|---|---|
| `direct_file` | A change to the CPA authentication file |
| `direct_incremental` | An incremental read from Sub2API PostgreSQL |
| `api_sync` | A full API synchronization from CLIProxyAPI/Sub2API |
| `push` | Manual registration or a programmatic push |
| `delete` | Deletion of an account/credential |

### 12.2 `status`

```text
ok
error
```

### 12.3 `source`

A synchronization source is displayed as:

```text
src:<database ID>
```

The page maps it to the synchronization source name. The source for manual registration is:

```text
api
```

### 12.4 Pagination and time range

The same as for the access audit:

```text
range = 1h | 24h | 7d | all
page >= 1
page_size <= 200, default 50
```

Update history and the access audit share the `log_retention_days` retention period, but clearing one page does not clear the other.

---

## 13. Management API automation reference

All management APIs require the session cookie obtained after administrator login. The frontend sends by default:

```http
Content-Type: application/json
Cookie: sticky_session=<session value>
```

Request bodies have two general limits:

- Approximately 1 MiB maximum;
- They must contain exactly one JSON value, with no second JSON value appended afterward.

### 13.1 Login

```http
POST /api/auth/login
Content-Type: application/json

{"password":"<administrator password>"}
```

After a successful login, the server sets the session cookie. Repeated failed logins trigger backoff by source IP; do not repeatedly guess the password with a loop script.

### 13.2 Change the administrator password

```http
POST /api/auth/password
Content-Type: application/json

{
  "old_password": "<old password>",
  "new_password": "<new password, at least 6 characters>"
}
```

After a successful change, the old session is revoked and you must log in again.

### 13.3 Read basic settings

```http
GET /api/settings
```

The following fields in the response are read-only informational fields:

```text
ingress_url
ingress_url_auth
hash_salt
```

The following fields are configuration fields:

```text
listen_tls_cert
listen_tls_key
admin_tls_cert
admin_tls_key
listen_auth
default_upstream
no_marker_policy
marker_path_parts
marker_headers
sid_len
session_ttl_min
salt_rotate_failure_threshold
log_retention_days
metrics_enabled
sync_empty_clear_threshold
block_private_targets
acl_whitelist
acl_blacklist
```

### 13.4 Save basic settings

```http
PUT /api/settings
Content-Type: application/json

{
  "listen_tls_cert": "",
  "listen_tls_key": "",
  "admin_tls_cert": "",
  "admin_tls_key": "",
  "listen_auth": "",
  "default_upstream": "us-residential-main",
  "no_marker_policy": "default_session",
  "marker_path_parts": [],
  "marker_headers": [
    "Authorization",
    "x-api-key",
    "api-key",
    "x-goog-api-key"
  ],
  "hash_salt": "<non-empty salt read from GET; do not omit>",
  "sid_len": 16,
  "session_ttl_min": 0,
  "salt_rotate_failure_threshold": 2,
  "log_retention_days": 30,
  "metrics_enabled": false,
  "sync_empty_clear_threshold": 3,
  "block_private_targets": true,
  "acl_whitelist": [],
  "acl_blacklist": []
}
```

Notes:

- This saves the complete settings object; do not treat it as an arbitrary-field PATCH;
- `hash_salt` must be non-empty; do not write an arbitrary new string;
- `marker_headers` cannot be an empty array;
- Every item in `marker_path_parts` must start with `/`;
- If non-empty, `default_upstream` must point to an enabled upstream;
- Even if included in the JSON, `acctmap_enabled` is not used as a new value by this save endpoint;
- For compatibility with older clients, `block_private_targets` may be omitted; omission means retaining its current value;
- `GET /api/settings` returns `listen_auth` verbatim in an authenticated admin-console session; `PUT /api/settings` remains compatible with `____` or `__unchanged__` to mean retaining the old password;
- If TLS paths change, `restart_required` in the response is `true`.

### 13.5 Reset the salt

```http
POST /api/settings/reset-salt
```

No request body is required. This regenerates the salt and causes all identities to be assigned new egresses.

### 13.6 Add an upstream

Plain upstream:

```http
POST /api/upstreams
Content-Type: application/json

{
  "name": "plain-office-proxy",
  "platform": "plain",
  "base_url": "http://user:pass@proxy.example.com:8080",
  "enabled": true
}
```

Decodo:

```json
{
  "name":"decodo-us",
  "platform":"decodo",
  "base_url":"http://user-login-country-us:password@gate.decodo.com:7000",
  "enabled":true
}
```

Generic:

```json
{
  "name":"generic-sticky",
  "platform":"generic",
  "base_url":"http://user:password@gateway.example.com:8080",
  "inject":{
    "username_template":"{user}-session-{sid}"
  },
  "enabled":true
}
```

When creating an upstream:

- `name` and `base_url` are required;
- `platform` must be a registered platform;
- `inject` must be empty or omitted for `plain`;
- `generic` must have a non-empty `inject.username_template`;
- If omitted, `enabled` defaults to `true`.

### 13.7 Edit an upstream

When editing, you may submit a complete object or use the current API's partial-field semantics:

```json
{
  "name":"decodo-us-new",
  "base_url":"http://user-login-country-us:____@gate.decodo.com:7000",
  "enabled":true
}
```

Here, `____` means retaining the old password. Generic's `inject.password` likewise supports retaining the masked value.

If changing the platform to `generic`, provide a valid template at the same time; if changing it to `plain`, remove `inject`.

### 13.8 Set as default and test

```http
POST /api/upstreams/<id>/default
```

```http
POST /api/upstreams/<id>/test
```

Setting an upstream as default requires the entry to be enabled. Testing uses the injected health-check identity and attempts to display the egress IP through an external IP/geolocation lookup service; a failed test does not necessarily mean the upstream cannot handle all business traffic, but you should usually fix the connection, authentication, or protocol error reported by the test first.

### 13.9 Adding a Sync Source

CLIProxyAPI:

```json
{
  "kind":"cpa",
  "name":"cpa-production",
  "base_url":"https://cpa.example.com",
  "api_key":"<management-key>",
  "interval_s":600,
  "enabled":true,
  "direct_auth_dir":"/opt/cliproxyapi/auths"
}
```

Sub2API:

```json
{
  "kind":"sub2api",
  "name":"sub2api-production",
  "base_url":"https://sub2api.example.com",
  "api_key":"<admin-api-key>",
  "interval_s":600,
  "enabled":true,
  "direct_db_dsn":"postgres://readonly:password@127.0.0.1:5432/sub2api?sslmode=disable"
}
```

### 13.10 Editing a Sync Source and Clearing the DSN

For an existing source, leaving `api_key` empty preserves the previous value:

```json
{
  "name":"sub2api-production",
  "api_key":"",
  "interval_s":600,
  "enabled":true
}
```

To clear the Sub2API incremental DSN:

```json
{
  "kind":"sub2api",
  "direct_db_clear":true
}
```

Do not send these fields together:

```json
{
  "direct_db_clear":true,
  "direct_db_dsn":"postgres://..."
}
```

### 13.11 Manually Registering an Account

The `<platform>` and `<account>` path parameters must be URL-encoded. Request body:

```http
PUT /api/acctmap/openai/user%40example.com
Content-Type: application/json

{
  "source_type":"manual-import",
  "access_token":"<AT>",
  "refresh_token":"<RT>"
}
```

Requirements:

- `platform` and `account` must be non-empty;
- `source_type` must be non-empty and no longer than 64 characters;
- at least one of `AT` and `RT` must be non-empty.

### 13.12 Account-to-Egress Binding

```http
PUT /api/acctegress/<platform>/<account>
Content-Type: application/json

{
  "mode":"sticky",
  "egress_ids":[3,7]
}
```

An empty array is equivalent to clearing the binding for that account:

```json
{"mode":"sticky","egress_ids":[]}
```

### 13.13 Bulk Egress-to-Account Binding

This may only be used with egress IDs whose `platform=plain`:

```http
PUT /api/acctegress/egress/<plain-upstream-id>
Content-Type: application/json

{
  "mode":"random",
  "accounts":[
    {"platform":"openai","account":"user@example.com"},
    {"platform":"grok","account":"grok@example.com"}
  ]
}
```

An unknown account causes the entire request to fail; the system does not silently save only part of it.

### 13.14 Audit Query API

```text
GET /api/logs
```

Available query parameters:

```text
from       Unix millisecond start time
to         Unix millisecond end time
q          host/path substring
account    exact real account or account_fp value
upstream   exact upstream name
class      2xx | 4xx | 5xx | err
page       page number
page_size  1–200, default 50
```

### 13.15 Update Record Query API

```text
GET /api/updates
```

Available query parameters:

```text
from
to
kind      direct_file | direct_incremental | api_sync | push | delete
source    src:<id> or api
status    ok | error
page
page_size
```

---

## 14. Build script parameters (not runtime configuration)

The following parameters belong to `scripts/build.sh`. They only affect build artifacts, are not written to `router.db`, and are not settings in the runtime administration console.

```bash
./scripts/build.sh [options]
```

| Parameter | Default | Usage |
|---|---|---|
| `--os OS` | `linux` | Target operating system, for example `linux` |
| `--arch ARCH` | `amd64` | Target architecture, for example `amd64` or `arm64` |
| `-o PATH` / `--output PATH` | `bin/mitmrouter-OS-ARCH` | Output binary path |
| `--debug` | Disabled | Preserve Go debug symbols; the default build strips symbols |
| `--skip-web` | Disabled | Reuse the existing `internal/webui/dist` instead of rebuilding the frontend |
| `-h` / `--help` | — | Show help |

Examples:

```bash
# Default Linux amd64 static binary
./scripts/build.sh

# Linux arm64
./scripts/build.sh --os linux --arch arm64

# Custom output path while preserving debug symbols
./scripts/build.sh --output ./mitmrouter --debug

# Reuse existing frontend build artifacts
./scripts/build.sh --skip-web
```

`--skip-web` requires `internal/webui/dist/index.html` to already exist; otherwise the build fails.

---

## 15. Test-tool environment variables (not production service configuration)

The production `mitmrouter` does not read environment variables as substitutes for the configuration above. The following environment variables found in the repository are used only by tests or live E2E tools.

### 15.1 Long-running test switches

```bash
MITMROUTER_RUN_BENCHMARK=1
MITMROUTER_RUN_LOAD=1
```

They only determine whether specific benchmark/load tests run; they do not change the production service's listening, routing, or upstream configuration.

### 15.2 Real-environment parameters for `tools/e2elive`

The following are required when running real two-source E2E tests:

```bash
E2E_SUB2API_URL=<Sub2API administration address>
E2E_SUB2API_KEY=<Sub2API administration key>
E2E_CPA_URL=<CLIProxyAPI administration address>
E2E_CPA_KEY=<CLIProxyAPI administration key>
```

For example:

```bash
E2E_SUB2API_URL=https://sub2api.example.com \
E2E_SUB2API_KEY='<secret>' \
E2E_CPA_URL=https://cpa.example.com \
E2E_CPA_KEY='<secret>' \
go run ./tools/e2elive
```

These variables are not read by the `mitmrouter` main program. Do not mistakenly enter them in the administration console's `listen_auth` or the upstream `base_url`.

---

## 16. Common configuration combinations

### 16.1 Local machine + API key + one residential upstream

```text
Startup:
  -data ./data
  -addr 127.0.0.1:55666
  -admin-addr 127.0.0.1:55667

Basic settings:
  listen_auth = empty (can be disabled when restricted to the local machine)
  marker_path_parts = empty
  marker_headers = default four items
  default_upstream = the provider entry you added
  no_marker_policy = default_session
  block_private_targets = true
  acl_whitelist = empty
  acl_blacklist = empty

Client:
  http://127.0.0.1:55666
```

Install `ca.pem` before first use because HTTPS is MITM'd by default.

### 16.2 Subscription AT/RT + account-level stickiness

```text
1. Add a cpa or sub2api sync source
2. Enter the service root in base_url; do not put the API path or key in the URL
3. Enter api_key
4. Start with 600 for interval_s
5. Test immediately, then sync immediately
6. Confirm that platform/account appears in the current account mapping
7. Select the sticky provider entry for default_upstream
8. Use a real request to check whether the egress remains the same after token rotation for the same account
```

If using plain egress binding:

```text
1. First add multiple egresses with platform=plain
2. Select sticky or random on the account
3. Select the plain egress set
4. After saving, the binding takes precedence over default_upstream
```

### 16.3 Public ingress, internal administration console

It is recommended that both the ingress and administration console listen on addresses reachable from the internal network/public network, while network policies isolate the administration plane:

```text
-addr       0.0.0.0:55666
-admin-addr 127.0.0.1:55667
```

Access the administration console externally through an SSH tunnel; configure the ingress as follows:

```text
listen_tls_cert/key = issued certificate pair
listen_auth         = random long username/password
```

This is simpler than directly exposing the administration console at `0.0.0.0:55667`.

### 16.4 MITM only specified targets

For example, allow only OpenAI targets and MITM-parse them; reject other targets:

```text
acl_whitelist =
  api.openai.com
  *.openai.com

acl_blacklist = empty
```

The client needs to trust the MITM CA only when accessing allowlisted targets. Note: targets that do not match receive a local 403 and are not forwarded.

### 16.5 Direct connection for traffic without an identity

```text
no_marker_policy = direct
```

This changes only requests without a Marker; requests with a Marker are still handled according to upstream and account bindings. Keeping `block_private_targets` enabled is still recommended.

---

## 17. Common Errors and Troubleshooting Order

### 17.1 The Admin UI Does Not Open After Startup

Check in order:

```text
1. Verify that -admin-addr is the address you are visiting
2. Do not confuse the client ingress port with the admin port
3. If admin TLS is enabled, use https:// rather than http://
4. Verify that certificate and private-key paths are readable and match
5. Verify that the listening port is not occupied
6. Verify that the firewall is not blocking access
```

### 17.2 The Client Receives 407

If it occurs immediately at ingress:

```text
Check listen_auth
Check that the client sends Proxy-Authorization
Check that username/password in the proxy URL are URL-encoded
```

For an upstream CONNECT 407 shown by audit or logs:

```text
Check username, password, and platform in upstream base_url
Do not use the management API Key as the upstream proxy password
```

### 17.3 The Client Reports a Self-Signed Certificate

```text
The client has not installed the ca.pem downloaded from MITMRouter
```

Confirm that the client uses this instance's CA, the container CA bundle is updated, and the process/container was restarted afterward. Do not mistake the listening TLS certificate for the MITM root CA. If the ACL allowlist is empty, nearly every HTTPS target requires this.

### 17.4 The Upstream Authenticates, but the Egress Is Not Sticky

```text
1. Verify platform
2. Verify that DataImpulse uses sessid syntax
3. Verify that the Decodo username starts with user-
4. Verify that 1024proxy retains console-generated region/t parameters
5. Verify that the Resin username is non-empty
6. Verify the Generic username_template
7. Verify session_ttl_min is compatible with Generic {ttl_min}
8. Check whether audit account_fp changes when credentials/accounts change
```

The wrong platform is the most common cause: an upstream validating only API Key and password may still appear to work with the wrong platform.

### 17.5 Field Errors When Saving Basic Settings

| Error type | Common cause |
|---|---|
| `invalid_listen_auth` | Only one credential is provided, or the password is empty |
| `invalid_tls_pair` | Certificate and private key are not paired, or parsing failed |
| `invalid_policy` | `no_marker_policy` is not one of the three enum values |
| `invalid_rules` | Header list is empty, or a path fragment does not start with `/` |
| `invalid_salt` | `hash_salt` was cleared |
| `invalid_sidlen` | `sid_len` is not in `4–64` |
| `invalid_ttl` | `session_ttl_min` is below 0 or exceeds the API limit |
| `invalid_acl` | Invalid IP/CIDR/domain/wildcard format, or over 500 entries |
| `invalid_retention` | Retention is outside `1–3650` |
| `invalid_sync_empty_clear_threshold` | Threshold is outside `1–100` |
| `unknown_upstream` | Default upstream does not exist or is disabled |

### 17.6 Generic Save Fails

```text
1. Verify inject is a valid JSON object
2. Verify username_template is non-empty
3. Check for unsupported placeholders
4. Check for unmatched curly braces
5. If the template contains {ttl_min}, verify session_ttl_min is not 0
6. Verify that platform is generic
```

### 17.7 The Sync Source Never Has Accounts

```text
1. Verify base_url is the service root address
2. Verify api_key is the management API Key, not a business token
3. Verify the CLIProxyAPI provider is in the supported mapping
4. Verify Sub2API type is oauth/setup-token
5. Verify Sub2API platform is in the supported mapping
6. Verify accounts contain email/name and AT/RT
7. Check “recent sync” and error details in update records
8. Check whether empty-snapshot protection has not reached its threshold
```

### 17.8 The Account Is Bound, but Requests Still Return 502

```text
Verify all bound egress entries use platform=plain
Verify plain entries were not disabled or deleted
Verify the bound account still exists in acct_map
Verify whether all bound egress entries are unavailable
```

When an explicit binding has no available candidate, the program does not silently bypass it for the default upstream; restore or adjust the binding first.

### 17.9 A Changed Setting Appears Ineffective

| Change | Restart required |
|---|---:|
| `-addr` | Yes, after changing the startup command |
| `-admin-addr` | Yes, after changing the startup command |
| TLS paths | Yes |
| Certificate contents at the same path | Usually no, hot-loaded in about 60 seconds |
| `listen_auth` | No, after saving |
| Default upstream | No |
| Marker rules | No |
| ACL | No |
| Upstream entries | No |
| Account mappings/bindings | No |
| Log retention, metrics, empty-snapshot threshold | No |

Do not edit `router.db` directly and expect the running in-memory snapshot to discover changes. Save runtime configuration through the admin UI/API.

---

## 18. Runtime Limitations You Must Not Ignore

These are not editable parameters, but affect expectations:

1. **Stickiness is best effort.** A reclaimed residential node, expired session, or offline node may give the same sid a new IP.
2. **UDP/QUIC is unsupported.** HTTP/3/UDP 443 may bypass this proxy; restrict UDP 443 at the system layer when needed.
3. **Certificate-pinned clients cannot normally be MITM'd.** Application certificate pinning may reject even when the system trusts the root CA.
4. **Full MITM by default requires client trust in the root CA.** This is not an upstream configuration error.
5. **`router.db` is highly sensitive.** It may allow recovery of the CA private key, upstream credential hashes/material, and other instance secrets.
6. **Listening and upstream addresses differ.** `-addr`/`-admin-addr` are local listeners; `base_url` is the external proxy this service connects to.
7. **The admin password and ingress authentication are separate credentials.** Changing one does not change the other.

---

## 19. Parameter Source Index

To verify the current implementation, see:

| Content | Code location |
|---|---|
| Runtime parameters | `cmd/mitmrouter/main.go` |
| Basic settings structure, defaults, loading | `internal/settings/settings.go` |
| Default database settings | `internal/store/store.go` |
| Basic settings API and validation | `internal/api/api.go` |
| Marker extraction | `internal/marker/extract.go` |
| ACL format and matching | `internal/acl/acl.go` |
| Common upstream validation and Generic templates | `internal/upstream/upstream.go`, `internal/upstream/generic.go` |
| DataImpulse | `internal/upstream/dataimpulse.go` |
| Decodo | `internal/upstream/decodo.go` |
| 1024proxy | `internal/upstream/c1024.go` |
| Resin | `internal/upstream/resin.go` |
| plain | `internal/upstream/plain.go` |
| Sync-source API | `internal/api/acctmap.go` |
| CLIProxyAPI sync | `internal/syncer/cpa.go` |
| Sub2API sync | `internal/syncer/sub2api.go` |
| CPA directory incremental sync | `internal/syncer/cpa_direct.go` |
| Sub2API PostgreSQL incremental sync | `internal/syncer/sub2api_direct.go` |
| Account mapping | `internal/acctmap/acctmap.go` |
| Account-egress binding API | `internal/api/acctegress.go` |
| Routing priority and private-network protection | `internal/server/ingress.go` |
| Admin UI fields and controls | `web/src/views/Settings.vue`, `Upstreams.vue`, `AcctMap.vue` |
| Audit query page | `web/src/views/Audit.vue` |
| Update records page | `web/src/views/Updates.vue` |
| Official provider syntax research | `docs/006-sticky-session-credentials.md` |
| Production deployment steps | `DEPLOY.en.md` |

If code differs from old documentation, prefer current page validation and source code, especially: `sid_len` range `4–64`; page/API `session_ttl_min` limits `1440`/`10080`; ACL blacklist matches and allowlist misses return a local 403 without contacting the target; DataImpulse does not inject `sessttl` from `session_ttl_min`; and `acctmap_enabled` is not editable in basic settings.

---

## 20. A Final Checklist You Can Follow

### Startup layer
- [ ] `-data` points to persistent storage and backups do not expose `router.db`;
- [ ] `-addr` and `-admin-addr` differ;
- [ ] Non-loopback ingress has authentication and a network boundary;
- [ ] Non-loopback admin UI has TLS or firewall/SSH-tunnel protection;
- [ ] `-trace-file` is not enabled long-term.

### Basic settings
- [ ] `listen_auth` username and password are both filled or both empty;
- [ ] The two ingress TLS paths are provided as a pair;
- [ ] The two admin-console TLS paths are provided as a pair;
- [ ] The correct instance's `ca.pem` is installed on the actual client;
- [ ] `marker_path_parts` is empty unless needed;
- [ ] `marker_headers` is non-empty and sensibly ordered;
- [ ] `sid_len` is `4–64`, usually `16`;
- [ ] `session_ttl_min` matches the provider plan and Generic template;
- [ ] `default_upstream` points to an enabled entry;
- [ ] `no_marker_policy` is one of the three expected values;
- [ ] `block_private_targets` stays enabled unless isolation is explicit;
- [ ] ACL allowlist/denylist access-rejection semantics are understood;
- [ ] `log_retention_days` matches disk and audit needs;
- [ ] Empty-snapshot protection threshold is not accidentally `1`.

### Upstreams
- [ ] `platform` matches the provider;
- [ ] `base_url` is the provider-console proxy URL;
- [ ] URL-special characters in username/password are encoded;
- [ ] Decodo username starts with `user-`;
- [ ] 1024proxy retains console `region`, `sid`, `t`, and related structure;
- [ ] Resin username is non-empty;
- [ ] Generic template is valid JSON with supported placeholders;
- [ ] `plain` entries have no `inject`;
- [ ] At least one upstream is enabled and its egress IP tested;
- [ ] The correct entry is the default.

### Account management
- [ ] Sync-source `base_url` has no userinfo, query, or fragment;
- [ ] API Key is the management-interface key;
- [ ] `interval_s >= 60`;
- [ ] CPA incremental directory is absolute, real, and not a symlink;
- [ ] Remote Sub2API DSN uses `sslmode=verify-full`;
- [ ] Manual accounts use a stable identifier, not a token;
- [ ] At least one of AT/RT is filled;
- [ ] Registered platform matches the target host's built-in classification;
- [ ] Account-egress bindings select only `plain` entries;
- [ ] Bound accounts are checked before disabling/deleting egress.

After completing this checklist, send a request from an actual client and verify it with “Access Audit” and “Update Records”; do not judge the business path correct solely because the page says “Saved.”

---

*This document only adds configuration documentation; it does not change MITMRouter's forwarding implementation.*
