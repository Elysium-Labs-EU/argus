# Jira Cloud setup

`--jira-issues` (see `argus supervise`), and the post-spawn `Transition`/
`Comment`/`Assign` hooks it drives, all authenticate against Jira Cloud with
the same credentials: `JIRA_BASE_URL` / `JIRA_EMAIL` / `JIRA_API_TOKEN`, or a
JSON file at `$JIRA_CONFIG_FILE` — else `~/.argus/jira.json`.

A dead or expired token used to only surface as a failed worker spawn minutes
into a run, with a bare `jira returned 401 Unauthorized` pointing at no fix.
`argus jira check` and `argus jira setup` exist so that failure is caught
before dispatch, not after.

## Set up credentials

```bash
argus jira setup
```

Prompts for `base_url` (a bare `acme.atlassian.net` is fine — argus resolves
the `api.atlassian.com/ex/jira/{cloudId}` form itself), `email`, and an API
token, writes them to `~/.argus/jira.json` (mode `0600`), then immediately
runs the same live check as `argus jira check` below and prints the result.

Create the token at
<https://id.atlassian.com/manage-profile/security/api-tokens>.

**Prefer a scoped API token over a classic one.** Atlassian offers two token
shapes:

* **API token with scopes** — site-bound, an explicit scope list. Least
  privilege: this is what argus needs and all it needs.
* **Classic API token** — full account access, no scoping. Broader than
  anything argus (or a compromised token) should have.

Neither token type can be rotated in place. An expired or compromised token
means create a new one and delete the old one — there is no "renew".

## Minimal scopes

argus makes five call shapes. The table below is each one's minimal scope,
both the classic (broad) name and the granular (scoped-token) name, verified
against Atlassian's own granular-scopes reference
(<https://developer.atlassian.com/cloud/jira/platform/scopes-for-oauth-2-3LO-and-forge-apps/>)
and, for the two write endpoints without an entry on that page, cross-checked
against Atlassian's per-endpoint OAuth scope tables.

| argus call | endpoint | capability | classic scope | granular scope |
|---|---|---|---|---|
| `FetchIssue` | `GET /rest/api/3/issue/{key}` | read an issue | `read:jira-work` | `read:issue:jira` |
| `Transition` (read) | `GET .../transitions` | view available transitions | `read:jira-work` | `read:issue.transition:jira` |
| `Transition` (write) | `POST .../transitions` | apply a transition | `write:jira-work` | `write:issue:jira` |
| `Comment` | `POST .../comment` | write a comment | `write:jira-work` | `write:comment:jira` |
| `Assign` | `PUT .../assignee` | change the assignee | `write:jira-work` | `write:issue:jira` |
| `Myself` | `GET /rest/api/3/myself` | read your own identity | `read:jira-user` | `read:jira-user` |

A scoped token covering everything above needs: **`read:jira-work`,
`write:jira-work`, `read:jira-user`** (classic-shaped names — Atlassian's
scoped-token creation UI accepts either the classic or granular form for
these). `read:jira-user` is deliberately not `read:me`: `read:me` is the
scope for Atlassian's separate account API
(`api.atlassian.com/me`), not Jira's own `/rest/api/3/myself` — using it
would reproduce the exact 401 this setup flow exists to avoid.

`Assign` has no dedicated "change assignee" scope in Atlassian's granular
catalog; `write:issue:jira` (create/update issues) is the covering scope, the
same one `Transition`'s write side uses.

## Validate credentials

```bash
argus jira check
```

Resolves credentials the same way `--jira-issues` does, then performs a real
`GET /rest/api/3/myself` through the same resolve+auth+tenant path every
other Jira call in argus uses — not a check that the configured fields are
merely non-empty. On success it prints the resolved account and the
`api.atlassian.com/ex/jira/{cloudId}` base, confirming tenant resolution
too. `argus doctor` runs this same check and folds a Jira line into its
checklist whenever Jira has anything configured at all.

On failure, `check` reports one of three categories — as far as the HTTP
response allows it to distinguish them:

* **misconfigured** — a missing field, a bad `base_url`, or an unresolvable
  `/_edge/tenant_info` lookup. Never reached Jira's own auth at all.
* **dead/revoked token** — a bare 401.
* **wrong site / missing scope** — a 403, or a 401 whose error body reads
  scope-shaped.

A bare 401 with an empty body is inherently ambiguous — Jira does not
distinguish "this token is dead" from "this token is missing a scope" in
that response — so `check` defaults it to the more common case (dead/revoked)
rather than asserting certainty it doesn't have.

## `created_at`

`argus jira setup` stamps an RFC3339 `created_at` into `~/.argus/jira.json`
when it writes the file. This is a hint, not an authoritative expiry:
Atlassian's API does not expose a real token expiry, so `created_at` only
answers "how old is this credential", never "when does it die". Config files
written before this field existed, or hand-authored ones, simply omit it.
