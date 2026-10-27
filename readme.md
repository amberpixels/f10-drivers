<p align="center">
  <img src="logo.svg" alt="f10-drivers" width="362">
</p>

<div align="center">

Tracker drivers for [f10](https://github.com/amberpixels/f10), configured per project instead of written per project.

[![License: MIT](https://img.shields.io/badge/license-MIT-yellow.svg)](LICENSE)

</div>

---

f10 reaches GitHub and GitLab issues through `gh` and `glab`.
Any other tracker goes through an executable at `.f10/driver` that answers f10's [driver contract](https://github.com/amberpixels/f10/blob/main/docs/driver-contract.md).
The contract is seven read verbs and one write verb, `create`, with answers on stdout and exit 3 for a verb the driver does not implement.

`f10-drivers` implements that contract once per tracker.
A project carries a one-line shim and a small TOML file, and no code.

| tracker | verbs |
|---|---|
| Notion | `read`, `url`, `search`, `list`, `index`, `branch`, `children`, `create` |

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

`f10 task create` and the capture step create tasks through the driver's `create`.
The **Create** adapter under `## Tracker` in `project.md` is only needed for a driver that declines it.

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
The driver fails with a message naming the problem on:

- an unknown key
- a priority level outside `urgent`, `high`, `medium`, `low`
- a property the database does not have, or one of the wrong type
- a `done` value that is not an option of the status property
- a `prefix` other than the one Notion gives the Unique ID property

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
| `create` | the new task's id (`MKT-57`), after making a page from the markdown on stdin and the flags below |

`create` reads `# <title>` then the body on stdin and sends the body to Notion as markdown, in one request.
Its flags map through `[notion.properties]`:

- `--status` and `--label` must be options of their property, matched case-insensitively. A select labels property takes one label.
- `--priority <level>` takes the first value `[notion.priority]` lists for the level that is an option, else the option named like the level.
- `--parent <n>` and `--after <n>` must be existing tasks. The body keeps its closing `After:` line, and `read` prints that line once.
- A flag whose property is not configured, or that `create` does not know, is dropped with a warning.

A value the schema does not allow fails before the page is created, naming the property and its options.
`create` retries only a 429. A 5xx or a lost connection is not retried, and the error says the task may have been created and names the database to check.

Images stay links, signed urls included: f10 downloads them itself.
A body Notion cuts short, or blocks it cannot render, gets a line in the answer pointing at the full page, and a warning on stderr.
`read` prints page-level comments; discussions on single blocks are not read.

## Conformance

`conformance.Check` runs every verb through a driver's process boundary and checks each answer against the contract.
That includes `create`, followed by a `read` of the new task, so point it at a fake or a scratch database; a driver that declines `create` passes.
It wants one line for a url or a branch, a JSON array of rows with the required fields, priorities in the vocabulary and RFC 3339 times.
A declined verb must exit 3; a missing task must exit non-zero with stderr.
The Notion driver runs it against a fake Notion API (`notion/notiontest`) through the same shim a project uses.
A new tracker passes its own fixtures to the same check.

## Development

```sh
just test    # go test ./...
just lint    # standardgo
just build   # bin/f10-drivers
```

Tests point the driver at a fake Notion API through `F10_DRIVERS_NOTION_API`, which accepts only a loopback url.

## License

MIT
