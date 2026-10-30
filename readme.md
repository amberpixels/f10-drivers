<p align="center">
  <img src="logo.svg" alt="f10-drivers" width="362">
</p>

<div align="center">

Tracker drivers for [f10](https://github.com/amberpixels/f10), configured per project instead of written per project.

[![License: MIT](https://img.shields.io/badge/license-MIT-yellow.svg)](LICENSE)

</div>

---

[f10](https://github.com/amberpixels/f10) is a task pipeline for coding agents: capture, plan, ship.
It reaches GitHub and GitLab issues through `gh` and `glab`, and any other tracker through an executable at `.f10/driver` that speaks its [driver contract](https://github.com/amberpixels/f10/blob/main/docs/driver-contract.md).

`f10-drivers` is that executable, written once per tracker.
A project carries a one-line shim and a TOML file, no code.

| tracker | verbs |
|---|---|
| Notion, REST API | `read`, `url`, `search`, `list`, `index`, `branch`, `children`, `create`, `comment`, `update` |
| Notion, hosted MCP server | the same |

## Install

```sh
go install github.com/amberpixels/f10-drivers/cmd/f10-drivers@latest
f10-drivers --version
```

## Use it in a project

`.f10/driver`, executable:

```sh
#!/bin/sh
exec f10-drivers notion "$@"
```

The config is read from `.f10/driver.toml`. With out-of-tree storage (`~/.f10/<project>/`), pass it explicitly:

```sh
exec f10-drivers notion --config "$(dirname "$0")/driver.toml" "$@"
```

In `.f10/instructions/project.md`:

```yaml
tracker:
  kind: notion
  id: MKT-###
```

`f10 task create` and `/f10:capture` then create tasks through the driver.

## Notion

Pick how the driver reaches Notion with `transport` in `driver.toml`:

- `api` (default): the REST API with an internal integration's token.
- `mcp`: Notion's hosted MCP server, as the logged-in user. Use it when the workspace does not allow integrations.

Either way, the database needs a **Unique ID** property with the project's prefix (`MKT`).

**API setup.** Create an integration at notion.so/profile/integrations, put its token in an env var (`NOTION_TOKEN` by default, never in the repo), and share the database with it (`...` menu, Connections).

**MCP setup.** Set `transport = "mcp"` and run `f10-drivers notion login` once.
It authorizes in the browser and stores the tokens in the OS keychain, or in a `0600` file under the user config dir when there is no keychain.
Tokens refresh on their own; a missing or revoked login fails with a message pointing at `login`.
`f10-drivers notion logout` revokes it.

### `driver.toml`

```toml
prefix = "MKT"                    # task ids are MKT-<number>
branch = "{id}/{slug}"            # {id}, {number}, {slug}

[notion]
# transport = "mcp"               # "api" by default
token_env = "NOTION_TOKEN"        # api only
database = "https://www.notion.so/team/Tasks-1a2b3c4d00004000800000000000db01"
# data_source = "..."             # only when the database has several
done = ["Done", "Cancelled"]      # status options that close a task

[notion.properties]
id = "ID"                         # required: the Unique ID property
# title = "Name"                  # the title property by default
status = "Status"                 # status or select
priority = "Priority"             # select or status
assignee = "Assignee"             # people
labels = "Tags"                   # multi_select or select
parent = "Parent task"            # relation; without it, children declines
after = "Blocked by"              # relation; the task this one waits for
result = "Result"                 # any other key: a property update --set <key>=<value> writes

[notion.priority]                 # extra option names per level
high = ["High", "P1"]
```

Only `prefix`, `notion.database` (or `data_source`) and `notion.properties.id` are required.
The driver checks the config against the database: unknown keys, missing or mistyped properties, `done` values that are not status options, or a `prefix` Notion does not use all fail with a message naming the problem.

### Verbs

| verb | answer |
|---|---|
| `read <n>` | title, status, priority, labels, assignee, the body as markdown, then page comments |
| `url <n>` | the page url |
| `search <q>` | tasks whose title contains `q` |
| `list` | the first 100 open tasks |
| `index` | the database url |
| `branch <n>` | the `branch` template, filled |
| `children <n>` | tasks whose `parent` is this one |
| `create` | the new task's id, from `# <title>` and a markdown body on stdin |
| `comment <n>` | the new comment's id, from markdown on stdin, posted as a page comment |
| `update <n>` | the task's id, after setting the properties its flags name |

`create` takes `--status`, `--label`, `--priority <level>`, `--parent <n>` and `--after <n>`, mapped through `[notion.properties]`.
Values are checked against the schema before the page is made; unknown or unconfigured flags are dropped with a warning.
It is one request, retried only on a 429: after a 5xx or a lost connection, check the database before running it again.
Over the API, read verbs also retry a transient 5xx.

`update` takes the same `--status`, `--label` and `--priority <level>`, plus `--assignee <email|name>` and `--set <key>=<value>` for any other key under `[notion.properties]`: a url, text, number, checkbox (`true`/`false`), date (`YYYY-MM-DD` or RFC 3339), select, status, multi-select (comma-separated option names), email or phone property.
`--label` replaces the task's labels, `--assignee` replaces its people, `--set` on a multi-select replaces its options, and an empty `--set <key>=` clears the property.
Every value is checked before the request, and unlike `create`, an unknown or unconfigured flag fails the update with nothing sent.
The values are absolute, so `update` retries a 429 and a transient 5xx like a read.
Over the API, `--assignee <email>` needs the integration's "read user information including email addresses" capability; a name matching several users fails naming them.

`comment` keeps paragraphs, links, bold, italic, strikethrough and inline code; headings, quotes, lists, tables and code blocks become plain text.
Like `create`, it is one request, retried only on a 429: after a 5xx or a lost connection, check the task's comments before running it again.

### Over MCP

The MCP tools return text meant for an LLM, not a versioned API, so the driver checks each answer's shape and fails naming the tool if it changes. Differences from the API:

- `children` declines (exit 3) past 100 subtasks.
- `list` and `search` rows carry `created` only if the database has a Created time property.
- Urls are `app.notion.com/p/...` links.
- Below the Business plan, Notion allows a few dozen queries an hour per workspace; past that, verbs fail with `usage_limit_reached`. Use `api` for heavy use.

On both transports, images stay links (f10 downloads them), a truncated body gets a pointer to the full page, and comments on single blocks are not read.

## Conformance

`conformance.Check` runs every verb through a driver's process boundary and checks each answer against the contract, including a `create`, a `comment` and an `update`, each followed by a `read`, so point it at a fake or a scratch database.
The Notion driver passes it over both transports, against the fakes in `notion/notiontest`. A new tracker passes its own fixtures to the same check.

## Development

```sh
just test    # go test ./...
just lint    # standardgo
just build   # bin/f10-drivers
```

Tests use `F10_DRIVERS_NOTION_API` and `F10_DRIVERS_NOTION_MCP` (loopback urls only) to reach the fakes, `F10_DRIVERS_TOKEN_STORE=file` to skip the keychain, and `F10_DRIVERS_NO_BROWSER` to make `login` only print its url.

## License

MIT
