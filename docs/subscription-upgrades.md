# Experimental subscription upgrades

Codex2API can expose an admin-only, quote-first workflow for two individual
ChatGPT subscription transitions:

- Plus (`plus`) to Pro 5x (`chatgptprolite`, observed entitlement `prolite`)
- Pro 5x (`prolite`) to Pro 20x (`chatgptpro`, observed entitlement `pro`)

This integration uses undocumented ChatGPT web backend endpoints. It is not a
stable OpenAI API contract and may change without notice. It is disabled by
default and must not be used for automatic or batch upgrades.

## Enable the API

Turn on **Settings → Subscription upgrades (experimental) → Enable subscription
upgrade API** in the admin UI. The switch is persisted in
`system_settings.subscription_upgrades_enabled` and takes effect immediately on
the instance that saved it, with no restart.

The environment variable below only supplies the initial value for a deployment
whose switch has never been saved:

```dotenv
CODEX2API_SUBSCRIPTION_UPGRADES_ENABLED=true
```

Once an administrator saves the switch from the admin UI, the stored value is
authoritative and the environment variable can no longer re-enable the feature.
A capability that spends real money must always be switchable off from the
interface.

While the feature is off, every subscription upgrade endpoint returns 404. When
it is on, all endpoints remain behind the existing admin authentication
middleware and nothing else. Note that an admin key is therefore the only thing
standing between a caller and a real charge.

In a multi-instance deployment the switch is applied in-process by the instance
that handled the save; other instances pick up the stored value when they next
start. This matches how the rest of the settings behave today.

## API flow

Read the current subscription:

```http
GET /api/admin/accounts/{id}/subscription
```

Create a short-lived preview:

```http
POST /api/admin/accounts/{id}/subscription/upgrade-quotes
Content-Type: application/json

{
  "target_plan": "chatgptpro",
  "currency": "PHP"
}
```

The response includes the exact amount currently due in minor units, recurring
price, tax, line items, renewal date, payment-method presence, and a two-minute
`quote_id`. `silent_reauthorization_available` reports whether Codex2API already
holds a separate Web Session that can be tried if the paid update invalidates
the current OAuth credential family. It never exposes that session credential.

After a human reviews that preview, submit a bounded confirmation:

```http
POST /api/admin/accounts/{id}/subscription/upgrades
Idempotency-Key: a-unique-value-for-this-one-upgrade
Content-Type: application/json

{
  "quote_id": "...",
  "currency": "PHP",
  "max_amount_minor": 350000,
  "confirmed": true
}
```

Immediately before the paid POST, Codex2API re-reads the current plan and
refreshes the upstream quote. It rejects the operation if the currency changes,
the plan changes, the account becomes delinquent, or the fresh amount is above
the confirmed cap.

Read the durable operation journal with:

```http
GET /api/admin/subscription-upgrades/{operation_id}
```

Reconcile an operation that did not reach a terminal state with:

```http
POST /api/admin/subscription-upgrades/{operation_id}/verify
```

Reconciliation is strictly read-only: it re-reads the upstream subscription and
promotes the operation to `succeeded` once the target entitlement is visible. It
never repeats the paid POST. This is also the only way to clear an operation
that is stuck in `submitting` because the process died mid-submission.

The journal stores the amount, currency, transition, sanitized status, and a
SHA-256 hash of the idempotency key. It never stores OAuth tokens, cookies,
payment method IDs, Stripe secrets, or card data.

## Payment and verification states

- `succeeded`: the paid request was accepted and the target entitlement was
  observed.
- `requires_user_action`: the upstream requested 3DS or another payment action;
  Codex2API stops and does not retry.
- `verification_pending`: the paid request was accepted, but the target
  entitlement is not visible yet. Reconcile with read-only requests; do not
  repeat payment.
- `verification_requires_reauthentication`: the paid request was accepted, but
  the OAuth credential family was subsequently rejected with a 401. Reauthorize
  the account, then verify the existing operation; do not repeat payment. Only a
  401 maps here: a 403 from chatgpt.com is usually a Cloudflare challenge rather
  than credential invalidation, so it falls to `verification_pending` instead.
- `ambiguous_transport`: the connection failed after submission may have begun.
  Reconcile the subscription and billing state manually; do not retry the paid
  POST automatically.

An `Idempotency-Key` is unique per account. Repeating the same request returns
the existing operation and never sends a second paid POST.

Independently of the idempotency key, an account may have only one submission in
flight at a time. The pre-submit journal row is written with status `submitting`
under a partial unique index on `account_id`, so a second submission — even from
another instance, and even with a different idempotency key — is rejected with
409 before any payment is attempted. Reconcile the outstanding operation first.

## Login preservation

Saving and restoring the same access token or refresh token cannot avoid login
when the upstream invalidates that credential family server-side. Codex2API can
silently recover only when the account also has a separate, still-valid ChatGPT
Web Session. In that case it attempts one normal account refresh and repeats
only the read-only entitlement verification. It never repeats the paid POST.

If no valid Web Session exists, `verification_requires_reauthentication` is the
correct terminal state until an administrator logs the account in again.

## Sanitized canary record (2026-08-27)

A single confirmed Pro 5x to Pro 20x canary was performed before this feature
was implemented:

- localized recurring price: PHP 9,990/month, including 12% VAT;
- refreshed amount due: PHP 3,451.96;
- paid update endpoint returned HTTP 200 exactly once;
- the existing access and refresh tokens were rejected afterward;
- no separate Web Session was stored, so manual reauthentication was required;
- after reauthentication, the upstream subscription reported `plan_type=pro`.

This outcome is why an accepted payment followed by a 401 is represented as
`verification_requires_reauthentication`, not as payment failure.

## Upstream contract observed by the canary

```text
GET  /backend-api/subscriptions?account_id={workspace_id}
GET  /backend-api/subscriptions/update/preview?account_id={workspace_id}&updated_plan={plan}
GET  /backend-api/checkout_pricing_config/configs/{currency}
POST /backend-api/subscriptions/update
```

The generic `/backend-api/payments/checkout` endpoint is not used for upgrades
of an already-paid subscription.
