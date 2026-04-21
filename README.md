# llama.cpp Router Monitor

[![Go](https://img.shields.io/badge/Go-1.24-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![Docker](https://img.shields.io/badge/Docker-ready-2496ED?logo=docker&logoColor=white)](https://www.docker.com/)
[![SQLite](https://img.shields.io/badge/SQLite-local-003B57?logo=sqlite&logoColor=white)](https://www.sqlite.org/)
[![llama.cpp](https://img.shields.io/badge/llama.cpp-local%20LLM-111111)](https://github.com/ggml-org/llama.cpp)
[![Streaming](https://img.shields.io/badge/SSE-streaming-1f6feb)](#what-it-does)

Monitor and inspect all traffic going through your local `llama.cpp` server.

Lightweight reverse proxy and monitoring UI for `llama.cpp` and OpenAI-compatible local inference servers.

It sits in front of your inference server, logs every request, stores raw payloads, measures latency and token throughput, and gives you a live web UI.

## What This Is For

`llama.cpp Router Monitor` is a tool for tracking **all requests to a local LLM** in a way that is easy to inspect, filter, and debug.

It is useful when you want to:

- debug local LLM traffic
- inspect prompts, streaming responses, timings, and token usage
- understand how agents behave step by step
- compare models, settings, and backends
- review failures and slow requests without digging through raw logs

## Screenshot

<!-- Replace this with a real PNG screenshot before publishing -->
![Screenshot](./docs/screenshot.png)

## What It Does

- Reverse-proxies requests to your `llama.cpp` server
- Captures request and response payloads
- Stores request history in SQLite
- Tracks:
  - active connections
  - TTFT
  - total latency
  - prompt/output token counts
  - prompt/output tokens per second
  - request/response sizes
  - errors
- Supports streaming and non-streaming responses
- Includes a live web UI with filtering and request inspection
- Cleans up old data automatically

## Why It Is Useful

Most local LLM setups tell you whether a request succeeded, but not **how** it behaved.

This project is meant to answer questions like:

- What exactly did the client send?
- What did the model return, including streaming output?
- How many prompt and output tokens were used?
- How fast was prompt ingestion vs output generation?
- Which requests were slow, failed, or behaved unexpectedly?
- What are my agents actually doing over time?

## Who It Is For

- people running `llama.cpp` locally
- developers building agent workflows
- anyone debugging prompts, tool calls, or request chains
- self-hosters who want visibility without adding heavy infrastructure

## Quick Start

### 1. Clone the repo

```bash
git clone https://github.com/dannychirkov/llama.cpp-router-monitor
cd llama-cpp-router-monitor
```

### 2. Start it

```bash
docker compose up -d --build
```

That is enough if your `llama.cpp` server is already reachable at:

```text
http://host.docker.internal:8080
```

### 3. Open the UI

```text
http://localhost:9091/_monitor/ui
```

### 4. Point your client to the proxy

Instead of sending requests directly to `llama.cpp`:

- old: `http://localhost:8080`
- new: `http://localhost:9091`

## Minimal Configuration

If your backend is not on `http://host.docker.internal:8080`, create a `.env` file:

```bash
cp .env.example .env
```

Then set:

```env
DEFAULT_BACKEND_URL=http://host.docker.internal:8080
```

## Windows Autostart

The container uses:

```yaml
restart: unless-stopped
```

So it comes back automatically when Docker starts.

To start it with Windows:

1. Open Docker Desktop
2. Go to `Settings -> General`
3. Enable `Start Docker Desktop when you sign in`

## One-Line Install For Existing Docker Users

```bash
git clone https://github.com/dannychirkov/llama.cpp-router-monitor && cd llama-cpp-router-monitor && docker compose up -d --build
```

## Runtime Model

The proxy keeps your existing API flow:

```text
client -> llama.cpp Router Monitor -> llama.cpp
```

It does not replace your inference server. It only sits in front of it.

## Privacy

This project is designed for local use.

- requests and responses are stored on your machine
- SQLite and raw payload files stay in `./data`
- nothing is sent anywhere unless you expose the service yourself

## Dynamic Backend Override

By default, requests go to `DEFAULT_BACKEND_URL`.

You can override the backend per request with:

- header:

```text
X-Backend-URL: http://host.docker.internal:8081
```

- or query parameter:

```text
?backend=http://host.docker.internal:8081
```

## Data Storage

Local data is stored in `./data`:

- `monitor.db` - SQLite database
- `raw/YYYY-MM-DD/*.gz` - raw request/response payloads

Old data is deleted automatically after `RETENTION_DAYS`.

If you want to keep everything indefinitely:

```env
RETENTION_DAYS=0
```

`0` or any negative value disables automatic cleanup.

## API Endpoints

- `GET /_monitor/health`
- `GET /_monitor/live`
- `GET /_monitor/stats?hours=24`
- `GET /_monitor/requests?limit=100&offset=0`
- `GET /_monitor/request/{id}`
- `DELETE /_monitor/request/{id}`
- `GET /_monitor/raw/{id}/request`
- `GET /_monitor/raw/{id}/response`
- `GET /_monitor/events`
- `GET /_monitor/backend-metrics?limit=200`
- `GET /_monitor/ui`

Supported request filters:

- `q`
- `path`
- `model`
- `method`
- `status`
- `since_hours`
- `stream`
- `errors_only`
- `with_tokens`

## Example Request

```bash
curl http://localhost:9091/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"local-model","messages":[{"role":"user","content":"hi"}],"stream":false}'
```

## Configuration

Main environment variables:

- `LISTEN_ADDR`
- `DEFAULT_BACKEND_URL`
- `ALLOW_DYNAMIC_BACKEND`
- `RETENTION_DAYS`
- `MAX_REQUEST_BYTES`
- `MAX_CAPTURE_BYTES`
- `REQUEST_TIMEOUT_SECONDS`
- `POLL_BACKEND_METRICS`
- `POLL_INTERVAL_SECONDS`
- `DATA_DIR`

See [`.env.example`](./.env.example) for defaults.

## Resource Usage Notes

To keep it lean:

- keep `MAX_CAPTURE_BYTES` reasonable, for example `8MB` to `32MB`
- disable backend metrics polling if you do not need it:
  - `POLL_BACKEND_METRICS=false`
- increase `POLL_INTERVAL_SECONDS` if `/metrics` does not need frequent polling

## Limitations

- some token and timing fields depend on what your backend actually returns
- raw payload capture can use noticeable disk space if retention is high
- this is a lightweight local monitor, not a full observability platform

## Roadmap

- charts for latency and throughput
- easier export of requests and metrics
- optional auth for shared environments
- better comparison across models and backends

## Publishing Notes

Before pushing to GitHub, replace:

- `./docs/screenshot.png`

You may also want to add:

- a license
- a changelog
- GitHub Actions for `go test`
- issue templates
