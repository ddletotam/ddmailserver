# Unified identity-keyed aggregation (calendars & contacts)

Design agreed 2026-07-15. Extends the mail aggregation model to calendars and
contacts so the desktop client works with **one writable calendar** and **one
address book**, never seeing the underlying sources.

## Principle

The server is the aggregator. It pulls from N external providers (as a
standard client) and presents one unified account per user. The desktop client
is thin: connected to our server it receives already-merged data; connected to
a foreign server it is a plain standard client with no aggregation.

Mail already implements this end to end and is the reference:
- server pulls N IMAP accounts → one local INBOX (dedup by Message-ID);
- the IMAP server face exposes only local (aggregated) folders;
- each message keeps `account_id` / `remote_folder` for reverse operations.

Calendars and contacts must take the same shape.

## The identity axis (primary)

Everything the client touches is keyed to an **identity** — a concrete email
address. Sources are a server-side detail that hangs *under* an identity; the
word "source" never reaches the client.

1. **No orphan sources.** Every `calendar_source` / `contact_source`, when
   configured on the server, MUST be associated with exactly one identity —
   even an ICS-URL source that has no address of its own. Association is
   mandatory at creation time.
2. **identity → sources is 1:N.** One address may hold several sources (e.g. a
   Yandex identity with its own CalDAV calendar plus an attached ICS feed).
3. **The client models only identities**, exactly like the mail composer's
   From picker.

## Writable calendar semantics

- **One surface.** The client reads and writes a single calendar entity through
  one endpoint. Heterogeneous events (different sources, different rw) arrive in
  one stream; the client does not group by source.
- **Per-event capability.** Each event carries `editable` / `deletable`, derived
  from its source's `can_write`. The client MAY show a lock, but may also just
  attempt the write and take a clean server rejection — capabilities are
  discovered through interaction, not pre-modeled.
- **Edit/delete routing.** The server routes a change to the event's origin
  source (known via `source_id`) and enforces rights there — mirrors mail
  flag/event reverse sync.
- **Create routing keyed by identity.** The client sends the From-identity
  explicitly (default assignable). The server picks the target source *within*
  that identity. If the identity has several writable sources, it needs a
  default-write-source rule per identity. If the target can't write → clean
  rejection, an expected outcome.
- **Capabilities reported per identity.** The server tells the client, for each
  identity, whether it can create / edit events (and, separately, contacts). The
  client builds its "create under…" picker only from writable identities.
- **Colour is server-assigned.** A source-blind client cannot colour events by
  origin, so colour becomes a server-side attribute set per source at
  configuration time and delivered already-resolved on each event. The client
  renders the colour it is given and no longer defaults/derives colour itself.
  (Supersedes the earlier client-side colour default.)

## The protocol matrix (both faces, dictated by the far end)

|                | our client            | standard client (Tbird/Outlook) |
|----------------|-----------------------|---------------------------------|
| **our server** | anything (WS/REST)    | standard (we are the server)    |
| **foreign srv**| standard (we are the client) | —                        |

Standard protocols are mandatory on any boundary with the outside world, in
both directions. Full freedom only when both ends are ours. The client must
handle N simultaneous mixed connections (some native, some standard).

## Current state (audited 2026-07-15)

**Done — mail plane:** server-side aggregation, standard IMAP/SMTP faces,
inbound IMAP/SMTP clients, reverse flag/delete sync.

**Inbound provider clients (server as client):** IMAP ✓, SMTP ✓, CalDAV ✓
(reverse sync), CardDAV ✓ (Google People + partial MS Graph). **LDAP client
MISSING** — this is the corporate-GAL ingestion path.

**Aggregator for cal/contacts:** events already returned as one stream across
sources; contacts cross-source search exists (`SearchContacts`). But calendars
remain per-source entities (no single writable surface), and contacts are not
wired to the desktop client at all.

**Standard faces out:** CalDAV server ✓ (per-source, no PROPPATCH), CardDAV
server ✓ (no `addressbook-query` → autocomplete falls back to full dump), LDAP
server ✓ but panic-prone (nmcclain string parsing) and **not part of this
design**.

**Desktop client:** NativeProvider has full calendar CRUD but no contacts
methods; ImapProvider rejects calendars ("requires a DDMail server"). So
"our client → foreign server" for cal/contacts is entirely absent.

## Build order

- **Phase 0 — retire the LDAP server face (port 10389).** Not in this design
  and a live remote crash (malformed BER panics the whole process). Block the
  port now; remove the server code in the deploy that lands Phase 2.
- **Phase 1 — contacts + writable calendar into the native client.**
  - Add mandatory identity association to `calendar_sources` / `contact_sources`.
  - Extend `/identities` with per-identity capability flags.
  - Add contacts methods to `MailProvider` + `/contacts`, `/contacts/search`
    (backed by `SearchContacts`).
  - Unified writable-calendar endpoint: one event stream with per-event
    `editable`/`deletable` and a server-resolved `colour`; create takes
    From-identity; server routes writes to the identity's default-write source
    and enforces rights.
  - Per-source colour set at source configuration; carried resolved on each
    event. Client stops defaulting colour.
- **Phase 2 — corporate GAL via an outbound LDAP client. DROPPED 2026-07-15 as
  not applicable.** Probing the user's accounts found NO externally-reachable
  LDAP (ports 389/636/3268 closed everywhere). The corporate directories are
  reached by other means: AppSec (Yandex 360) already syncs via Yandex CardDAV
  (a configured contact source); skiftrade is on-prem Exchange with EWS/OWA
  blocked externally — its GAL would need an EWS client after IT exposes EWS,
  a separate effort, not LDAP. Revisit only if a real LDAP endpoint appears.
- **Phase 3 — `addressbook-query` in the CardDAV server face.** Real
  server-side autocomplete for standard clients (Thunderbird/Outlook).
- **Phase 4 — standard CalDAV/CardDAV client providers in the desktop client.**
  Closes "our client → foreign server" for cal/contacts. Largest, least urgent
  (only matters for accounts not routed through our server).

Each phase ships independently.
