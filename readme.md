<div align="center">

### f10-drivers

Tracker drivers for [f10](https://github.com/amberpixels/f10), configured per project instead of written per project.

[![License: MIT](https://img.shields.io/badge/license-MIT-yellow.svg)](LICENSE)

</div>

---

f10 reaches GitHub and GitLab issues through `gh` and `glab`.
Any other tracker goes through an executable at `.f10/driver` that answers f10's [driver contract](https://github.com/amberpixels/f10/blob/main/docs/driver-contract.md): seven verbs, data on stdout, exit 3 for a verb it does not implement.

`f10-drivers` implements that contract once per tracker.
A project carries a one-line shim and a small TOML file, and no code.

| tracker | verbs |
|---|---|
| Notion | `read`, `url`, `search`, `list`, `index`, `branch`, `children` |

## Install

```sh
go install github.com/amberpixels/f10-drivers/cmd/f10-drivers@latest
```

## Use it in a project

`.f10/driver`, executable:

```sh
#!/bin/sh
exec f10-drivers notion "$@"
```

f10 runs the driver from the checkout root, so the config is read from `.f10/driver.toml`.
With out-of-tree storage (`~/.f10/<project>/`), point the shim at the file beside it:

```sh
#!/bin/sh
exec f10-drivers notion --config "$(dirname "$0")/driver.toml" "$@"
```

And in `.f10/instructions/project.md`, the tracker and its id format:

```yaml
tracker:
  kind: notion
  id: MKT-###
```

Creating tasks is not a driver verb.
It stays with the **Create** adapter under `## Tracker` in `project.md`; the Notion MCP connector fits it, since Notion takes blocks, not a markdown string.

## Notion

### Setup

1. Create an internal integration at notion.so/profile/integrations and copy its token into an env var (`NOTION_TOKEN` by default). Never put it in the repo.
2. Share the tasks database with the integration (the database's `...` menu, Connections).
3. Give the database a **Unique ID** property with the project's prefix (`MKT`). Its number is the task number f10 passes the driver.

### `driver.toml`

```toml
prefix = "MKT"                    # task ids are MKT-<number>
branch = "{id}/{slug}"            # optional; {id}, {number}, {slug}

[notion]
token_env = "NOTION_TOKEN"        # optional; the env var holding the token
database = "https://www.notion.so/team/Tasks-1a2b3c4d00004000800000000000db01"
# data_source = "..."             # only when the database has several
done = ["Done", "Cancelled"]      # optional; status options that close a task

[notion.properties]
id = "ID"                         # required: the Unique ID property
# title = "Name"                  # optional; the title property by default
status = "Status"                 # status or select
priority = "Priority"             # select or status
assignee = "Assignee"             # people
labels = "Tags"                   # multi_select or select
parent = "Parent task"            # relation; without it, children declines
after = "Blocked by"              # relation; the task this one waits for

[notion.priority]                 # optional; values per contract level, beyond
                                  # those already named urgent, high, medium, low
urgent = "Urgent"
high = ["High", "P1"]
medium = "Medium"
low = "Low"
```

Every key but `prefix`, `notion.database` (or `notion.data_source`) and `notion.properties.id` is optional.
An unknown key, a priority level outside `urgent`, `high`, `medium`, `low`, a property the database does not have, one of the wrong type, a `done` value that is not an option of the status property, or a `prefix` other than the one Notion gives the Unique ID property fails with a message naming it.

### What each verb answers

| verb | answer |
|---|---|
| `read <n>` | `# MKT-n: title`, the status, priority, labels and assignee, the body as Notion renders it to markdown, an `After:` line, then the page's comments |
| `url <n>` | the page's url |
| `search <q>` | tasks whose title contains `q`, any status |
| `list` | the first 100 tasks whose status is not in `done` |
| `index` | the database's url |
| `branch <n>` | the `branch` template filled from the task |
| `children <n>` | tasks whose `parent` relation holds this one; `state` is `closed` for a `done` status |

Images stay links, signed urls included: f10 downloads them itself.
A body Notion cuts short, or blocks it cannot render, gets a line in the answer pointing at the full page, and a warning on stderr.
`read` prints page-level comments; discussions on single blocks are not read.

## Conformance

`conformance.Check` runs every verb through a driver's process boundary and checks each answer against the contract: one line for a url or a branch, a JSON array of rows with the required fields, priorities in the vocabulary, RFC 3339 times, exit 3 for a declined verb, a non-zero exit with stderr for a missing task.
The Notion driver runs it against a fake Notion API (`notion/notiontest`) through the same shim a project uses; a new tracker passes its own fixtures to the same check.

## Development

```sh
just test    # go test ./...
just lint    # standardgo
just build   # bin/f10-drivers
```

Tests point the driver at a fake Notion API through `F10_DRIVERS_NOTION_API`, which accepts only a loopback url.

## License

MIT
