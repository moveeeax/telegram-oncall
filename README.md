# telegram-oncall

[![ci](https://github.com/moveeeax/telegram-oncall/actions/workflows/ci.yml/badge.svg)](https://github.com/moveeeax/telegram-oncall/actions/workflows/ci.yml)
[![Go 1.26+](https://img.shields.io/badge/go-1.26%2B-00ADD8?logo=go)](go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

On-call rotas, escalation timers and acknowledgement, delivered to Telegram.

Grafana OnCall was archived. PagerDuty starts at $21/user/month. A four-person
team that already lives in a Telegram group does not need a second incident
platform — it needs the pager to reach whoever is actually awake, to escalate
when nobody answers, and to leave an audit trail of who acknowledged what.

This repository holds the engine: alert ingest, routing, dedup, rotas and the
durable escalation state machine, plus `oncall`, a single-binary CLI that
exercises all of it offline. **No bot token, no account and no network access
are required to run anything in this repo.** See
[Scope](#scope-what-is-and-is-not-here) for what is deliberately missing.

## What it does

- **Ingest** — Alertmanager webhook v4, Grafana unified alerting, and a generic
  JSON shape, normalised into one alert model. Format is auto-detected.
- **Routing** — label equality, anchored label regexps, absent-label and
  severity matchers, with `continue: true` for fan-out to an audit chat.
- **Dedup and flap suppression** — one live incident per (fingerprint, route),
  enforced by a partial unique index; a fingerprint that fires more than N times
  in a window is recorded but not paged.
- **Rotas** — daily or weekly rotation over an ordered member list, per-rota
  timezone, and manual "I'm covering tonight" overrides. Handover is computed
  from calendar days in local time, so a DST change does not slide the pager by
  an hour or hand it to the wrong person.
- **Escalation state machine** — page the primary, wait, page the secondary,
  wait, @-ping the whole group. Every timer lives in SQLite, not in a goroutine:
  restarting mid-incident loses nothing, and each rung delivers exactly once
  even with several workers ticking the same database.
- **Ack / Resolve** — recorded in an append-only ledger with actor and
  timestamp, exportable as CSV or JSON.
- **Quiet hours** — per severity, with a critical class that always breaks
  through. Suppressed incidents are still recorded, so the audit trail is
  complete even when the phone stays quiet.
- **Heartbeats** — dead-man's switch endpoints. A cron that stops calling home
  pages the rota, once per outage.

## Install

Requires Go 1.26 or newer (CI tests 1.26 and 1.27). The SQLite driver is pure Go, so there is no cgo and
no system library to install.

```sh
go install github.com/moveeeax/telegram-oncall/cmd/oncall@latest
```

Or from a clone:

```sh
git clone https://github.com/moveeeax/telegram-oncall.git
cd telegram-oncall
go build -o oncall ./cmd/oncall
```

## Usage

Everything below runs against the checked-in files in [`examples/`](examples/)
and produces the output shown. CI runs these same commands on every push, so if
they stop working the build goes red.

### Replay a real incident through the escalation ladder

`simulate` runs the whole state machine on a fake clock: ingest, routing,
paging, escalation, ack, and the ledger that results.

```sh
go run ./cmd/oncall simulate \
  -c examples/oncall.yaml \
  -f examples/alertmanager-firing.json \
  -ack-after 3m -ack-by bob -until 30m
```

```text
simulating from 2026-03-13T22:41:07Z (workspace "acme-sre")

T+00:00  ingest  opened    route=database
T+00:00  [page] step=0 CRITICAL -> db-topic (@carol): Postgres replica is 412s behind primary — on call for rota "primary" (manual override: alice at a wedding)
            buttons: ack:1 | resolve:1
T+02:00  [escalate] step=1 CRITICAL -> db-topic (mention-all): Postgres replica is 412s behind primary — escalated to the whole group
            buttons: ack:1 | resolve:1
T+03:00  [ack] incident 1 acknowledged by bob — ladder stopped

incident 1: status=acked step=1 occurrences=1 acked_by=bob after 3m0s

ack ledger:
  2026-03-13T22:41:07Z  system   open       route=database escalation=db-fast severity=critical
  2026-03-13T22:41:07Z  system   page       step 0 -> rota:primary
  2026-03-13T22:43:07Z  system   escalate   step 1 -> target
  2026-03-13T22:44:07Z  bob      ack        acked at step 1
```

Note who got paged: not the person the weekly rotation would have picked, but
Carol, because the config has an override covering that night.

Drop `-ack-after` and watch the ladder run to the end. Persist the run with
`-db` and export the ledger afterwards:

```sh
go run ./cmd/oncall simulate -c examples/oncall.yaml \
  -f examples/alertmanager-firing.json -db /tmp/incidents.db -until 30m
go run ./cmd/oncall export -db /tmp/incidents.db -format csv
```

### Ask who is carrying the pager

```sh
go run ./cmd/oncall whoison -c examples/oncall.yaml -at 2026-03-13T22:41:07Z -next 2
```

```text
primary      carol (Carol Sato)  until 2026-03-14T08:00:00Z  [manual override: alice at a wedding]
  2026-03-09T09:00:00Z -> 2026-03-16T09:00:00Z  alice
  2026-03-16T09:00:00Z -> 2026-03-23T09:00:00Z  bob
secondary    carol (Carol Sato)  until 2026-03-16T09:00:00Z  [weekly rotation, period 9, member 1/3]
  2026-03-16T09:00:00Z -> 2026-03-23T09:00:00Z  alice
  2026-03-23T09:00:00Z -> 2026-03-30T08:00:00Z  bob
```

The last handover lands at `08:00Z` rather than `09:00Z` because Amsterdam moves
to summer time on 2026-03-29 and the rota hands over at 10:00 *local*.

### Dry-run routing for a payload you actually received

```sh
go run ./cmd/oncall route -c examples/oncall.yaml \
  -f examples/alertmanager-firing.json -at 2026-03-13T22:41:07Z
```

```text
alert Postgres replica is 412s behind primary  fingerprint=9f2a1c4b7d8e0a31 severity=critical status=firing source=alertmanager
  route=database target=db-topic(topic) escalation=db-fast
    severity "critical" breaks through quiet hours
    step 0: rota:primary           -> carol then wait 2m0s
    step 1: target                 -> the group chat
```

Pipe your own payload in with `-f -`. An alert that matches nothing is reported
as `UNROUTED` rather than silently dropped.

### Validate a workspace before applying it

```sh
go run ./cmd/oncall validate -c examples/oncall.yaml
```

```text
ok: workspace "acme-sre"
  3 target(s), 3 responder(s), 2 rota(s), 2 escalation(s), 3 route(s), 1 heartbeat(s)
  quiet hours 23:00-08:00 Europe/Amsterdam, suppress [info warning], breakthrough [critical]
```

Validation is strict — unknown YAML fields are rejected and every cross
reference is checked — and it reports *all* problems at once rather than
stopping at the first, because this file gets written the way Terraform gets
written.

## Configuration

One YAML document describes a workspace. See
[`examples/oncall.yaml`](examples/oncall.yaml) for a fully commented one.

| Block | Purpose |
| --- | --- |
| `targets` | where pages land: a `group`, a forum `topic` (`thread_id`), or a `dm` |
| `responders` | the humans, with their Telegram user ids |
| `rotas` | `daily`/`weekly` rotation, `handover_time`, `timezone`, `overrides` |
| `escalations` | ordered `steps`, each `notify: rota:<name>` / `responder:<id>` / `target`, with a `wait` |
| `routes` | `match` predicate to a `target` + `escalation`, optional `continue` |
| `quiet_hours` | `from`/`to` (may wrap midnight), `suppress` and `breakthrough` severities |
| `heartbeats` | dead-man's switches with `interval` and `grace` |
| `dedup` | `flap_window` and `flap_threshold` |

Every id in the example is an obvious placeholder. Real chat ids and bot tokens
belong in your own config, never in a repository.

## How the state machine stays honest

The interesting part is not the ladder, it is what happens when the process
dies, when two workers race, or when the same alert fires forty times.

- **Timers are rows, not goroutines.** `escalate.Engine.Tick` selects timers
  whose `fire_at` has passed. A process that was down for seven minutes fires
  them the moment it comes back. Covered by `TestRestartSafety`.
- **Claiming a timer is a conditional UPDATE.** `UPDATE timers SET state='fired'
  WHERE id=? AND state='pending'` — whoever gets `RowsAffected() == 1` owns it.
- **Delivery is guarded by a unique index** on `(incident_id, step)`, so even a
  torn retry cannot page twice. Covered by
  `TestExactlyOnceUnderConcurrentTicks`.
- **Dedup is a partial unique index**, not application bookkeeping: at most one
  incident per `(fingerprint, route)` in a live state. Resolving one frees the
  fingerprint for the next occurrence.
- **Ack cancels pending timers** and is idempotent; a second ack writes no
  second ledger row.

## Scope: what is and is not here

Present and working, entirely offline:

- alert ingest and normalisation, routing, dedup, flap suppression
- rota resolution with overrides and DST-stable handover
- the durable escalation state machine, ack/resolve, the audit ledger
- quiet hours, heartbeat expiry
- the `oncall` CLI and its test suite

Not here, because it needs a Telegram bot token and a running service — this is
the next slice, not a stub:

- the Telegram Bot API transport (`notify.Notifier` is the seam it plugs into,
  and `notify.Page.Buttons()` already defines the inline-keyboard callback
  contract)
- the HTTP webhook receiver, API-key auth and per-key rate limiting
- the callback handler that turns an inline button press into `Engine.Ack`
- multi-tenancy, billing and the merchant-of-record checkout

Deliberately out of scope for v1, not "coming later": voice calls and SMS;
Slack, Teams, Discord and email; a mobile app or web dashboard; SSO/SAML.
Telegram-only is the point.

## Development

```sh
go test ./...           # unit tests
go test -race ./...     # what CI runs
go vet ./...
gofmt -l .
```

The test suite uses the same fixtures the README does, including the documented
CLI output, so a change that breaks an example breaks the build.

## License

MIT. See [LICENSE](LICENSE).
