# MITMRouter

[中文](README.md)

> **An ultra-high-performance local traffic router for identity-based egress selection**
>
> Built for high-concurrency LLM/API traffic, MITMRouter recognizes a Marker or subscription account in each request and selects the appropriate upstream egress. The client does not need to change its API address, SDK, or request code—just point its network egress at the local machine.

MITMRouter's MITM root certificate and private key are **generated automatically on the first start** and stored only in the local data directory. All runtime data and key material—including configuration, credentials, account mappings, audit records, and CA material—are kept in the local SQLite database `router.db`. There is no cloud database or external data-service dependency. The deployment owner controls the data and the security boundary; protect `router.db` like key material.

MITMRouter solves a specific problem: **the same account should use a stable egress to access an upstream API, different accounts should be routable independently, and no client should need to be reconfigured one by one.**

```text
Marker / subscription account
            │
            ▼
Stable routing identity
            │
            ▼
Upstream platform session / sid
            │
            ▼
As-stable-as-possible egress IP
```

- **Ultra-high-performance routing path**: designed for high concurrency; controlled HTTPS + TLS decryption + HTTP/2 measurements reached about **833–921 IOPS** at 64 concurrent requests;
- **No business-side client changes**: no Base URL changes, no SDK changes, and no changes to business request content;
- **Account-level stickiness**: the same Marker or subscription account continues to use the same logical egress;
- **Unified multi-egress orchestration**: supports DataImpulse, Decodo, 1024proxy, Resin, and generic templates;
- **Operational visibility**: built-in Web admin console, access audit, TTFB, update records, and Prometheus metrics;
- **Local-first, controllable security**: key material and runtime data do not depend on cloud storage and remain under the deployment owner's control;
- **Simple delivery**: single-file SQLite storage, single-binary delivery, and no extra database dependency.

## What Problem It Solves

In LLM/API workloads, egress selection is constrained by identity, stability, and the cost of changing clients at the same time:

| Business problem | Direct result | MITMRouter's approach |
|---|---|---|
| The same account needs a stable egress | Requests drift between egresses, increasing the risk of triggering risk controls or losing a session | Map the Marker/account to a stable routing identity, then generate a platform session/sid |
| Different accounts need isolation | All requests share one egress and cannot be distinguished | Calculate routes independently for each identity and support account-level egress bindings |
| Identity is hidden inside HTTPS | Only the encrypted tunnel is visible, so routing by API key is impossible | Decrypt and recognize the Marker locally according to the target rules |
| Tokens rotate | A rotated token can be mistaken for a new account, changing its egress | Decouple the subscription account from short-lived AT/RT tokens so rotation need not change the egress |
| Egress platforms use different formats | Each platform requires a separate integration | Manage platform templates and session-parameter injection through one routing layer |
| Runtime behavior is hard to inspect | Egresses, status codes, latency, and failure causes are difficult to troubleshoot | Manage them centrally in the console and record request results and TTFB in the audit |

Its responsibility is **routing decisions and egress orchestration**: deciding which egress connection a request uses. The project does not proactively rewrite the business URL, request headers, request body, response headers, or response body seen by the destination service.

## Core Capabilities

### 1. Marker Routing and Account-Level Stickiness

- Recognizes common credentials by default, including `Authorization: Bearer ...`, `x-api-key`, `api-key`, and `x-goog-api-key`;
- Supports restricting identity recognition by target host, path fragments, and request rules;
- If no subscription-account mapping is found, uses the Marker to calculate a stable identity, preserving pure Marker routing;
- When an account mapping matches, routes by the logical subscription account rather than the short-lived token;
- Stores the routing salt in local data, so a process restart does not arbitrarily reassign every identity;
- Supports deliberate salt rotation when all identities need to move to new egresses, as a risk-control and failure-escape mechanism.

### 2. No Business Configuration Changes on the Client

The client does not need to change the target API's Base URL, SDK, or request code. After the local CA is trusted, point the system or application's network egress at MITMRouter's route entry.

Whether a target may be accessed is decided by the target ACL first:

- A denied target receives a local `403` and is never contacted;
- An allowed HTTPS target is decrypted locally for routing decisions;
- The routing layer uses the recognized identity only to select an egress and does not proactively modify the allowed request URL, headers, response headers, or response body.

### 3. Unified Integration with Multiple Egress Platforms

MITMRouter converts its internal routing identity into the session format required by each egress platform:

| Upstream platform | Session format | Purpose |
|---|---|---|
| DataImpulse | `sessid.<account>` | Carries the routing identity in DataImpulse session parameters |
| Decodo | `session-<account>` | Uses the platform's session parameter to maintain a session |
| 1024proxy | `sid-<account>` | Uses the platform's sid parameter to maintain a session |
| Resin | `Platform.<account>` | Carries the routing identity in the platform username format |
| Generic | Custom template | Integrates with similarly styled session formats |
| Plain | No session-parameter injection | Plain egress mode; can be associated with sticky/random selection |

Upstream connection URLs support `http://`, `https://`, `socks5://`, and `socks5h://`. Platform sessions have their own lifetimes. MITMRouter can keep using the same session identifier, but the egress IP may still change after the platform expires or reclaims that session.

<img width="3000" height="1662" alt="image" src="https://github.com/user-attachments/assets/62a5f2eb-1c4c-4341-8a6c-d9e8373a40db" />

### 4. Account and Egress Orchestration

Account management can pull accounts from CLIProxyAPI, Sub2API, and other sync sources, or register them manually. Once an account matches, you can:

- Bind multiple plain-mode egresses;
- Set priorities for different egresses;
- Choose between sticky and random selection;
- Give account-level bindings priority over the default route;
- Cascade-clean bindings when an account is deleted.

This separates “which account is it?” from “which egress should it use?”, which is useful for token rotation, egress-pool switching, and multi-account isolation.

<img width="3000" height="1662" alt="image" src="https://github.com/user-attachments/assets/c48bc1c9-075c-4890-98a9-12c8a35157a2" />

### 5. Management, Audit, and Security Controls

- Built-in bilingual Vue3 admin console;
- Add, test, enable, disable, and set the default upstream egress;
- Test an upstream's egress IP and geographic location;
- Query access audits by time, keyword, account fingerprint, upstream, and status code;
- Record total duration and time to first byte (TTFB);
- Optional Prometheus `/metrics` endpoint;
- Target ACL, private-network protection, and cloud-metadata protection;
- Inbound authentication, HTTPS for the admin console, and sensitive-information protection in logs;
- Update records for tracking sync-source pulls, account mappings, and related status changes.

<img width="3000" height="1662" alt="image" src="https://github.com/user-attachments/assets/72dadbc3-01ab-41e7-a017-45c9c12b1bd3" />

## Routing Path

```text
AI / API client
        │ HTTP request or HTTPS CONNECT
        ▼
Local route entry :55666
        │
        ├─ Is the target allowed by ACL?
        │    ├─ No: local 403; do not contact the target
        │    └─ Yes: continue with local TLS decryption and identity recognition
        │
        ├─ Extract the Marker or subscription-account credential
        ├─ Calculate a stable routing identity / look up the account mapping
        ├─ If an account binding matches, select a bound egress
        └─ Otherwise, select the default egress and inject platform session parameters
        │
        ▼
Upstream egress platform
        │ DataImpulse / Decodo / 1024proxy / Resin / Generic / Plain
        ▼
Destination API
```

### Why HTTPS Routing Needs Local Decryption

After a CONNECT tunnel is established, the destination request is inside encrypted TLS content. The upstream egress can see only the tunnel connection; it cannot directly read `Authorization` or another Marker inside it, so the upstream platform alone cannot route by account.

MITMRouter terminates TLS locally under controlled conditions, recognizes the identity, makes the egress decision, and then establishes the upstream connection. This process exists for routing decisions: it does not change the business URL, business request headers, or request body received by the destination service, and it does not change the response headers or response body returned by that service.

## Quick Start

### 1. Build and Start

Building requires Go ≥ 1.22 and pnpm:

```bash
./scripts/build.sh
./bin/mitmrouter-linux-amd64 -data ./data
```

The first start generates a random admin password and prints it only in the startup log. Save it immediately.

Default listeners:

| Purpose | Address |
|---|---|
| Client route entry | `127.0.0.1:55666` |
| Admin console | `127.0.0.1:55667` |

Open `http://127.0.0.1:55667/ui` and sign in with the password generated on the first start.

### 2. Complete the Minimum Configuration in the Admin Console

Follow these steps to route the first request:

1. **Basic Settings**: confirm the Marker extraction rules and inbound authentication;
2. **Download the CA**: download `ca.pem` and install it in the trust store of the system or client that needs HTTPS identity recognition;
3. **Upstream Egress**: select a platform and enter `base_url` and credentials;
4. **Test the Egress**: verify connectivity, egress IP, and geographic location;
5. **Set as Default**: make the available egress the default upstream;
6. **Connect the Client**: point the client or system network egress at `http://127.0.0.1:55666`;
7. **Verify the Result**: after a request finishes, inspect the account fingerprint, upstream, status code, and duration in Access Audit.

If inbound authentication is enabled on the route entry, use an address with credentials, for example:

```text
http://user:pass@127.0.0.1:55666
```

### Startup Parameters

The README keeps only the parameters needed at startup. See the [parameter guide](PARAMETERS.en.md) for complete runtime configuration instructions.

| Parameter | Default | Description |
|---|---|---|
| `-data` | `./data` | Data directory containing `router.db`, the CA, and runtime data |
| `-addr` | `127.0.0.1:55666` | Client route-entry listener address |
| `-admin-addr` | `127.0.0.1:55667` | Admin-console listener address |
| `-trace-file` | Empty (disabled) | Local debugging: streams plaintext requests/responses to a file; not redacted, so use carefully in production |
| `-log-level` | `info` | `debug` / `info` / `warn` / `error`; normal logs do not record Markers or tokens |

Listener addresses are controlled only by startup parameters and require a restart after modification. Other runtime settings are stored in SQLite and managed through the admin console.

## What the Admin Console Does

The admin console is not just a parameter form; it is the unified entry point for routing rules, egresses, and runtime status.

| Page | Problem it solves |
|---|---|
| Basic Settings | Configure TLS, inbound authentication, Marker extraction, stickiness, TTL, ACL, private-target protection, audit retention, and metrics |
| Upstream Egress | Manage multiple egress platforms, view masked credentials, test egress IPs, enable/disable egresses, and set the default route |
| Account Management | Sync or register subscription accounts, associate accounts with egresses, handle token rotation, and apply account-level routing |
| Access Audit | Filter requests by time, keyword, account fingerprint, upstream, and status code; inspect total duration and TTFB |
| Update Records | Track sync-source pulls, account mappings, and related status changes |

### Target ACL: Decide Which Targets May Be Accessed

The ACL controls which targets may enter the forwarding path. Four entry forms are supported:

- Single IP: `1.2.3.4`;
- CIDR network: `10.0.0.0/8`;
- Exact domain: excludes subdomains;
- Wildcard domain: `*.openai.com`, matching subdomains at any depth but not the base domain.

Decision order:

```text
Blocklist match                  → local 403; reject access
Non-empty allowlist, no match    → local 403; reject access
All other targets                → allow; identity parsing and routing
```

ACL rejection happens before DNS, identity parsing, or an egress connection; allowed requests keep their original business semantics, with no URL, request-header, response-header, or response-body rewriting. Separately, the enabled-by-default `block_private_targets` protects the local machine, private networks, and cloud metadata addresses: even an ACL-allowed target returns `403` when it fails that check, and DNS results are checked to prevent bypasses.

## Performance Results

> **These are controlled measurements of MITMRouter's own routing path, not a performance promise for public networks, residential egresses, or real model services.**

With a local virtual upstream and 64 KiB uploaded and downloaded per request:

- A single HTTP request averages about **3.2–3.4 ms** end-to-end, at about **290–310 IOPS**;
- A single HTTPS CONNECT + local TLS decryption + HTTP/2 request averages about **4.5–5.5 ms** end-to-end, at about **180–224 IOPS**;
- At 64 concurrent HTTPS requests, throughput is about **833–921 IOPS**, with about **68–76 ms** average end-to-end latency—the best balance between throughput and latency in this run;
- At 256 concurrent HTTPS requests, the highest observed throughput was **1033 IOPS**, but average end-to-end latency exceeded **230 ms** and the slowest request exceeded **700 ms**;
- Every sample passed URL, request-header, request-body, response-body SHA-256, and sticky-ID checks: **errors = 0**.

### Results Summary

| Scenario | Concurrency | IOPS | Average TTFB | Average end-to-end latency |
|---|---:|---:|---:|---:|
| HTTP direct | 1 | 289.9–309.8 | 2.37–2.55 ms | 3.22–3.45 ms |
| HTTP direct | 8 | 579.4–637.2 | 9.32–10.37 ms | 12.50–13.76 ms |
| HTTP direct | 64 | 447.5–912.7 | 60.96–123.6 ms | 67.59–139.4 ms |
| HTTP direct | 256 | 563.3–805.9 | 236.1–369.1 ms | 292.4–405.7 ms |
| HTTPS + TLS decryption + HTTP/2 | 1 | 181.8–223.6 | 3.17–3.95 ms | 4.47–5.50 ms |
| HTTPS + TLS decryption + HTTP/2 | 8 | 528.1–617.8 | 8.71–10.29 ms | 12.91–15.04 ms |
| HTTPS + TLS decryption + HTTP/2 | 64 | 833.2–920.9 | 43.19–49.53 ms | 68.15–76.01 ms |
| HTTPS + TLS decryption + HTTP/2 | 256 | 994.8–1033 | 177.8–191.8 ms | 233.8–239.5 ms |

See the [full performance report](docs/009-performance-test-report.md) for the complete raw data.

## Security Boundaries and Known Limitations

- **Stickiness is best effort**: after a platform session expires or is reclaimed, the same account may receive a new egress IP;
- **UDP/QUIC is not handled**: clients using HTTP/3 may bypass this route; restrict UDP 443 at the system level if necessary;
- **Certificate pinning cannot be decrypted**: clients using certificate pinning are not suitable for local identity parsing;
- **Protect `router.db`**: it contains the CA private key, credential material, account mappings, and audit data; anyone possessing the database may be able to decrypt HTTPS traffic that passed through this route;
- **Use `-trace-file` carefully**: it writes plaintext requests and responses without redaction and is intended only for controlled debugging;
- **Harden public deployments**: when the admin console binds to a non-loopback address, configure HTTPS, restrict access, and set inbound authentication;
- **ACL controls target access**: a blacklist match or allowlist miss returns a local `403`; private-target protection is an independent safety check, and an ACL allow does not bypass it;
- **No external-service performance guarantee**: the benchmark in this README does not represent the capacity of real residential egresses, public networks, or model services.

## Development

```bash
go test ./...                                    # Backend tests
./scripts/build.sh                               # Build the frontend and compile a statically linked Linux amd64 binary
./scripts/build.sh --os linux --arch arm64       # Cross-compile
./scripts/build.sh --output ./mitmrouter --debug # Specify output location / debug build
```

## Related Documentation

- [Detailed design](DESIGN.en.md): current implementation, routing path, and security constraints;
- [Parameter guide](PARAMETERS.en.md): admin-console fields, startup parameters, and deployment-time instructions;
- [Production deployment guide](DEPLOY.en.md): VPS, systemd, TLS, and container CA trust;
- [Account outbound-binding design](docs/011-plain-binding-design.md): account-to-egress binding strategies;
- [Benchmark system](docs/008-benchmark-system.md): test protocol, integrity checks, and test-code responsibilities;
- [Performance report](docs/009-performance-test-report.md): complete raw data and test boundaries;
- [Platform credential-format research](docs/006-sticky-session-credentials.md): session formats for each egress platform.

## License

This project is released under the [MIT License](LICENSE).

## Friend Links

- [Linux.do](https://linux.do/)

MITMRouter's core path can be summarized as: **recognize the identity, choose the route, and connect through the right egress as stably as possible.**
