# LindaFlow

Local-first M-Pesa transaction screening and merchant fraud-alert MVP.

## Included

- Go HTTP API and in-process low-latency rule scorer
- Daraja OAuth token handling
- STK Push initiation and STK callback receiver
- STK Push query
- C2B URL registration, validation and confirmation callbacks
- Durable local JSONL transaction journal
- PWA shell caching and IndexedDB offline transaction queue
- Dashboard with risk score, decision, reasons and transaction history
- Docker deployment

## Run locally

Requires Go 1.22+.

```sh
cp .env.example .env
go run ./cmd/lindaflow
```

Open http://localhost:8080.

The app starts without Daraja credentials. Local assessment and local transaction capture work without Safaricom connectivity. Daraja operations require valid credentials and a public HTTPS callback URL.

## Daraja setup

1. Create a Safaricom Daraja app and obtain Consumer Key and Consumer Secret.
2. Configure the correct shortcode, passkey and transaction type for your product.
3. Set `DARAJA_BASE_URL` to the sandbox or production host.
4. Set `CALLBACK_BASE_URL` to a publicly reachable HTTPS origin.
5. Configure the environment variables in `.env`.
6. Register C2B URLs using `POST /api/mpesa/register-c2b` where C2B is enabled for your shortcode.
7. Test with Safaricom sandbox credentials and test numbers before requesting production access.

Endpoints:
- `POST /api/assess`
- `GET|POST /api/transactions`
- `POST /api/mpesa/stk-push`
- `POST /api/mpesa/stk-query`
- `POST /api/mpesa/register-c2b`
- `POST /api/mpesa/stk-callback`
- `POST /api/mpesa/c2b-validation`
- `POST /api/mpesa/c2b-confirmation`

## Offline behavior

The dashboard shell is cached by the service worker. A transaction entered while offline is assessed locally and stored in IndexedDB. When connectivity returns, queued events are sent to the local API.

Offline mode cannot contact Safaricom, confirm settlement, initiate an STK Push, or guarantee that a payment occurred. A locally queued transaction is not proof of payment. Always verify a payment using a trusted Daraja callback or transaction query before releasing goods.

## Latency design

The assessment endpoint uses in-process Go rules and avoids network calls on the scoring path. It reads a short recent-transaction window from the local journal. The Daraja call is a separate action and has an 8-second client timeout; it is not part of local risk scoring.

## Important production hardening

This is an integration-ready MVP scaffold, not a certified production payment platform. Before production:
- Replace the JSONL journal with PostgreSQL/TimescaleDB and Redis velocity counters.
- Add Redpanda event streaming and a separately deployed ML inference service.
- Add merchant authentication, role-based access control, key rotation, rate limiting and request signing.
- Validate callback authenticity using Safaricom-supported controls and network allowlists where available; do not assume callback URLs are authenticated merely because they are obscure.
- Add idempotency and deduplication for callbacks and transaction events.
- Encrypt sensitive data at rest, minimize phone-number retention, and implement Kenyan Data Protection Act controls.
- Add structured logs, OpenTelemetry, metrics, alerting, backups and disaster recovery.
- Train and validate a LightGBM/XGBoost model on representative, labeled data before enabling automated blocking.
- Review C2B validation response semantics and transaction type against the exact Daraja product enabled for the business.

The initial rules are illustrative and must be calibrated against labeled transaction data. A low score does not prove a transaction is safe.
