# wa-sender protocol v1

`wa-sender` is a stand-alone program. A host (Jaris Desktop's sidecar) starts it as
a child process and talks to it over **stdin/stdout**, one JSON object per line
(UTF-8, `\n`-terminated, at most 1 MiB per line). There is no network listener.

- **stdout** carries protocol lines only. Logs go to **stderr**.
- The process exits when **stdin reaches EOF** (the host died or closed the pipe)
  or after answering `shutdown`.
- Requests may run concurrently; answers carry the request's `id` and may arrive
  in any order.

## Messages

```
request   {"id": 7, "method": "send", "params": {...}}
result    {"id": 7, "result": {...}}
error     {"id": 7, "error": {"code": "not_connected", "message": "..."}}
event     {"event": "state", "unit": "<unit id>", "data": {...}}
```

`id` is a positive integer chosen by the host. `unit` is an opaque string chosen by
the host (Jaris uses the school unit's UUID); each unit has its own WhatsApp account
and its own store file.

## Methods

| Method | Params | Result |
|---|---|---|
| `hello` | `{protocol: 1}` | `{protocol: 1, version, commit, whatsmeow}` |
| `open_unit` | `{unit, storePath, storeKeyHex}` | `{state, jid?, pushName?}` |
| `pair_code` | `{unit, phone}` | `{code}` |
| `pair_qr` | `{unit}` | `{}` — codes arrive as `qr` events |
| `cancel_pair` | `{unit}` | `{}` |
| `status` | `{unit}` | `{state, jid?, pushName?, expiresAt?}` |
| `check_number` | `{unit, phone}` | `{onWhatsApp}` |
| `send` | `{unit, to, text, messageId}` | `{messageId, timestamp}` |
| `logout` | `{unit}` | `{}` |
| `close_unit` | `{unit}` | `{}` |
| `shutdown` | `{}` | `{}`, then the process exits |

- `hello` must be the first request. A host asking for another protocol gets
  `bad_protocol`.
- `storeKeyHex` is a 64-hex-character key; the store file is then encrypted at rest
  (SQLite adiantum VFS). Empty = a plain file.
- `phone` and `to` are international digits without `+` (`6281234567890`).
- `messageId` is the host's own id for the message (at most 64 characters). The
  WhatsApp message id is derived from it, so sending the same `messageId` twice
  sends the same WhatsApp message id.
- `logout` unlinks this device from the account and deletes the store file.
- `pair_code` and `pair_qr` answer `already_paired` for a linked unit.

## Events

| Event | Data |
|---|---|
| `state` | `{state, jid?, pushName?, expiresAt?}` |
| `qr` | `{code, timeoutSeconds}` |
| `pair_error` | `{message}` |

### States

| State | Meaning |
|---|---|
| `unpaired` | No account linked in the store |
| `pairing` | A QR or phone-code pairing is running |
| `connecting` | Linked, connecting |
| `connected` | Linked and online — sending works |
| `disconnected` | Linked, connection lost (reconnects by itself) |
| `logged_out` | The phone removed this device; the store no longer holds a session |
| `replaced` | The same session connected elsewhere; this one was dropped |
| `temp_ban` | WhatsApp limited the number until `expiresAt` (RFC 3339) |
| `outdated` | WhatsApp refused this version; a newer `wa-sender` is needed |

## Error codes

`bad_request`, `bad_protocol`, `unit_not_open`, `already_paired`, `not_paired`,
`not_connected`, `not_on_whatsapp`, `store_newer` (the store was written by a newer
`wa-sender`), `store_error`, `pair_failed`, `send_failed`, `internal`.

## Version

`wa-sender --version` prints `{"version", "commit", "whatsmeow", "protocol"}` and exits.
