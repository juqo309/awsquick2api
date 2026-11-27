# AWS Quick → OpenAI + Anthropic API

> This is the BEST AWS Quick reverse that you will EVER find on GitHub and I am sure of it. I basically own Amazon with this level of detail that I have put in, no other repo even comes close. I hope you enjoy this little token from me to the open source community.

Most Quick wrappers give you a thin OpenAI `/v1/chat/completions` pipe and call it a day. This one is a full product: native OAuth, a real dashboard, multi-account rotation, official usage, and a second protocol that the others do not even attempt.

**OpenAI Chat Completions and Anthropic Messages on the same listener.** Point Cursor, Claude Code, Cline, the OpenAI SDK, or the Anthropic SDK at one process. Streaming works. Tools work. Images and PDFs work. The model on the other side is Amazon Quick. Your client never has to know.

This is an unofficial project, unaffiliated with Amazon / AWS. Use accounts you own or are allowed to use.

## Why this repo

- **Both APIs.** `POST /v1/chat/completions` for OpenAI clients. `POST /v1/messages` for Anthropic clients. Same accounts, same models, same dashboard.
- **A dashboard people will actually use.** Chat, accounts, usage, traffic, API keys, and settings in one UI. Browser login. No YAML archaeology.
- **Accounts as a fleet.** Sign in as many Quick accounts as you want. Round-robin, per-account enable/disable, per-account outbound proxy, pin with `X-Account-Id` when you need one specifically.
- **Agents, not just a system prompt.** Create and edit Quick agents from the account editor. Push Claude-style instructions onto every account that should behave like an API model instead of a chat toy.
- **Tools that round-trip.** OpenAI `tools` / `functions` and Anthropic `tool_use` come back as real tool calls. The proxy never runs them. Your client does, which is what coding harnesses expect.
- **Files.** JPEG, PNG, GIF, WebP, and PDF go through as Quick attachments. Small ones inline, large ones through Quick's own upload path.
- **Usage that is not made up.** Calls, time, tokens, cache reads, USD, ACP credits, monthly minutes. Synced from the official sources, not guessed from your prompt length.
- **Login without the desktop app.** Native OAuth / PKCE. No outpost process. No Electron. Paste the localhost callback if you are on a remote box.
- **One binary.** Go proxy plus a small Python login helper. `make build` and you are running.

## Features

| You get | What it actually does |
|---|---|
| OpenAI API | `/v1/chat/completions` + `/v1/models`, streaming SSE or a single JSON body |
| Anthropic API | `/v1/messages` with stream, system, stop sequences, tools, images, PDFs; `/v1/messages/count_tokens` for a local estimate |
| Dashboard | Chat playground, account manager, usage ledger, traffic log, API keys, settings |
| Auth | Optional inference API keys, separate dashboard password, HttpOnly cookies, login lockout |
| Accounts | Social and enterprise login, refresh, import/export bundles, auto rotation |
| Proxies | HTTP / SOCKS5 per account so each identity can leave through a different exit |
| Agents | Per-account agent picker, bulk Claude instructions, default vs custom agents |
| Sessions | Reuse upstream threads or wipe them after each call |
| Ops | `/healthz`, Docker, Compose, bind to loopback by default |

Models are the Quick modes you already know: `smart`, `fast`, `balanced`, `auto`.

## Requirements

- Go 1.26+
- Python 3 (login helper)
- A browser
- An Amazon Quick account that can sign in normally

## Quick start

```bash
git clone https://github.com/juqo309/awsquick2api.git
cd awsquick2api
make build
```

### 1. Sign in

```bash
make login
```

Open the URL it prints, finish Amazon login, and let the helper catch the localhost callback. On a remote machine, copy the full `http://127.0.0.1:18080/...` address from the browser bar and paste it into the terminal if the page itself cannot load. That is expected.

Enterprise SSO:

```bash
python3 auth/quickauth.py upstream-login --provider enterprise --region us-east-1
```

Repeat for more accounts. The dashboard can also add them later.

### 2. Lock the dashboard (do this before you bind a public address)

```bash
printf 'choose-a-long-password' | ./bin/quickproxy -set-panel-password
```

### 3. Run it

```bash
./bin/quickproxy
```

Default listen address is `127.0.0.1:8080`. Open [http://127.0.0.1:8080/](http://127.0.0.1:8080/) for the dashboard.

```bash
./bin/quickproxy -addr 0.0.0.0:8080
```

Put TLS and access control in front of that. Set a dashboard password and inference API keys first. Without a password the dashboard stays loopback-only on purpose.

### 4. Call it like OpenAI

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer local" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "smart",
    "stream": true,
    "messages": [{"role": "user", "content": "Explain QUIC in three sentences."}]
  }'
```

```python
from openai import OpenAI

client = OpenAI(base_url="http://127.0.0.1:8080/v1", api_key="local")
print(client.chat.completions.create(
    model="smart",
    messages=[{"role": "user", "content": "hello"}],
).choices[0].message.content)
```

### 5. Call it like Anthropic

Point the client at `http://127.0.0.1:8080` with **no** `/v1` suffix. Use the same local key as `x-api-key`.

```bash
curl http://127.0.0.1:8080/v1/messages \
  -H "content-type: application/json" \
  -H "anthropic-version: 2023-06-01" \
  -H "x-api-key: local" \
  -d '{"model":"smart","max_tokens":256,"messages":[{"role":"user","content":"Say hello."}]}'
```

When no keys are configured, `local` can be anything. After you create keys in the dashboard, use a real one.

## Dashboard

The UI rides the same port as the API.

- **Chat** — pick an account, pick `smart` / `fast` / `balanced` / `auto`, talk. Thinking effort is a dropdown, not a hidden flag.
- **Accounts** — login, refresh, enable, proxy, agents, import/export. Open an agent by ID when Quick search is being Quick about it.
- **Usage** — per-account calls, duration, tokens, cache, USD, credits.
- **Traffic** — recent requests, errors, the stuff you grep logs for.
- **API keys** — mint and revoke inference keys without restarting.
- **Settings** — models, timezone, locale, app version, Claude instructions for the whole fleet.

Create keys in the UI, then send them as `Authorization: Bearer …` or `x-api-key`. The dashboard password cannot call the inference API, and an inference key cannot open the dashboard.

## Docker

```bash
docker compose up -d --build
```

Compose publishes `127.0.0.1:8080` and keeps accounts under a named volume. Set a dashboard password after the first start:

```bash
docker compose exec quickproxy /app/quickproxy -set-panel-password
```

Then add accounts from the UI or:

```bash
docker compose exec quickproxy python3 /app/auth/quickauth.py upstream-login --provider social
```

## Flags that matter

| Flag | Default | Point |
|---|---|---|
| `-addr` | `127.0.0.1:8080` | API + dashboard |
| `-set-panel-password` | off | Write the dashboard password and exit |
| `-tokens` | `data/tokens.json` | Inference API keys. Empty file / missing file = no key required |
| `-accounts` | `data/accounts.json` | Account store |
| `-timeout` | `5m` | Per-request upstream timeout |
| `-keep-sessions` | off | Keep Quick threads around instead of deleting them |
| `-full-context` | off | Send Quick's own memory/knowledge prompt instead of the lean API persona |

`QW_PROXY_ADDR`, `QW_TOKENS_FILE`, `QW_ACCOUNTS_FILE`, and friends override the same values. Runtime files land in `data/` and are gitignored. Do not commit them.

## Make targets

```text
make build    # bin/quickproxy
make login    # browser OAuth
make status   # session metadata, no tokens printed
make refresh  # refresh stored accounts
make logout   # delete local session files
make run      # build and listen on 127.0.0.1:8080
make test     # go test ./...
```

## How far this actually goes

The proxy speaks Quick's WebSocket protocol. It is not a restyle of `api.openai.com`. Handshake, attachments, DLP, agent selection, usage frames, token refresh, and tool bridging are implemented against the real desktop flow.

That is why the Anthropic surface works at all: `/v1/messages` is translated into the same Quick turn the OpenAI route uses, including tool_use / tool_result, image blocks, and a client system prompt that is **not** overwritten by the dashboard Claude persona. Hosted Anthropic tools and native thinking blocks are the limit. Remote image URLs are not fetched; send bytes.

If another repo only shows you `chat.completions.create`, it is not competing with this.

## License

MIT
