# ai-summary

Posts a rolled-up Slack digest of inbox activity using a locally-installed AI CLI (`claude`, `gemini` or `opencode`). Different from `slack-notify` (one Slack message per email): `ai-summary` emits **one rolled-up summary per trigger**.

Three trigger modes:

- **`per_run`** — fire after each filter pass; summarize the new emails buffered during that pass.
- **`interval`** — fire every N minutes (e.g. `10m`); summarize the current state of the configured mailbox(es) when something changed.
- **`cron`** — fire on a wall-clock schedule (e.g. `@daily`, `0 8 * * 1-5`); same dirty gate as `interval`, but anchored to specific times.

In `interval` and `cron` modes the plugin pulls mailbox state via host RPC and only posts when the mailbox actually changed since the last fire — no noisy "nothing new" digests.

## Hidden labels

`labels.yml` defines per-label visibility via the `hidden: true` flag. Hidden labels are filtered out **both** before the dirty fingerprint is computed (so a hidden-only flip doesn't trigger a spurious digest) **and** before labels are surfaced to the AI prompt (so internal markers stay internal). The deny-list is refreshed via `host.list_labels` on each scheduled tick.

## Install

```bash
go install github.com/golang-mail/ai-summary@latest
```

No third-party dependencies — pure stdlib.

## Register the plugin

Add to `~/.go-mail/plugins.yml`:

```yaml
plugins:
  - name: ai-summary
    command: ["/path/to/ai-summary"]
    transport: socket   # default; called out for clarity
    timeout: 60s        # accommodates the AI CLI on flush
    config:
      backend: "claude"      # "claude" | "gemini" | "opencode"
      claude_command: ["claude", "-p", "--model", "claude-sonnet-4-6"]
      gemini_command: ["gemini", "-p"]
      opencode_command: ["opencode", "run"]   # add "--model", "provider/model#variant" (opencode v2 syntax; v1's --variant flag is gone) to override the default; "--format json" is appended automatically
      ai_timeout: "45s"
      mode: "per_run"        # "per_run" | "interval" | "cron"
      # schedule is required for interval/cron modes. Examples:
      #   "10m"          — every 10 minutes (interval)
      #   "@hourly"      — top of every hour (cron)
      #   "@daily"       — once a day at midnight (cron)
      #   "0 8 * * 1-5"  — 08:00 weekdays (cron)
      # schedule: "10m"
      # mailboxes is consulted only by interval/cron modes — the mailbox(es)
      # whose state to summarize. Filter routing is still by webhook key.
      # mailboxes:
      #   - {account: "work", mailbox: "INBOX"}
      # instructions replaces the default digest structure guidance
      # ("group by sender or topic, N bullets…") with your own. Slack mrkdwn
      # formatting rules still apply. Omit to keep the default structure.
      # instructions: |
      #   Use exactly three sections: Urgent, FYI, Ignore.
      #   One line per email, newest first. No emoji.
      include_body_chars: 600    # per-email body slice in prompts (per_run only); 0 = subject + from + visible labels
      include_labels: true
      min_batch: 1               # below this, post a one-liner without the AI call
      max_batch: 100             # cap on per_run buffer per webhook key
      username: "go-mail digest"
      icon_url: "https://example.com/avatar.png"
      default_webhook: "general"
      webhooks:
        general:  "https://hooks.slack.com/services/T.../B.../xxx"
        alerts:   "https://hooks.slack.com/services/T.../B.../yyy"
```

To run multiple trigger modes in parallel, register the plugin multiple times with different `name:` values (`ai-summary-pulse`, `ai-summary-morning`, etc.) — one process per mode keeps the buffer/timer state model trivial.

## Use it from filters

Filters route emails to a webhook key by setting `value:` on the `plugin:ai-summary` condition. In `per_run` mode the matched emails feed into the digest buffer. In `interval` / `cron` modes the filter is still the wiring point (so the plugin can be referenced from filter actions list to confirm placement), but `evaluate` is a no-op — those modes pull state from `host.list_emails` at tick time.

```yaml
filters:
  - name: ai-digest-inbox
    mailboxes: ["INBOX"]
    conditions:
      - field: plugin:ai-summary
        operator: matches
        value: "general"
    actions: []
```

`ai-summary` declares `run_last: true` by default — same as `slack-notify`. Cleanup filters (move, delete) fire first within a dependency level so the digest reflects the post-cleanup mailbox, not the raw arrival batch.

## Failure modes

| Failure | Behavior |
|---|---|
| AI CLI not on PATH or exits non-zero | Log; post a deterministic fallback listing so the digest still goes out |
| AI CLI times out (`ai_timeout`) | Log; post fallback listing |
| Slack post fails | Log; drop the digest (next trigger rolls it up anyway) |
| `host.list_labels` fails on tick | Log; reuse the previous deny-list (label visibility doesn't update this tick) |
| Plugin crashes mid-flush | Host respawns; in-flight `per_run` buffer is lost (acceptable — these are summaries) |

## Trigger summary

```
per_run:   evaluate → buffer → batch_complete event → flush per key
interval:  scheduled_tick → host.list_emails → fingerprint → if changed, summarize+post
cron:      scheduled_tick → host.list_emails → fingerprint → if changed, summarize+post
```

The dirty fingerprint is computed over `(sort(uids), sort(flags), sort(visible labels))` — flag-reorder by the IMAP server doesn't cause a flap, and hidden labels are excluded so backend-only label edits don't trigger digests the user would find surprising.
