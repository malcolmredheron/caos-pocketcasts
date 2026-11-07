# Pocket Casts tool

`caos-tools/pocketcasts` reads and changes the user's Pocket Casts account. Run
it with `run_tool(path="<source tree>/caos-tools/pocketcasts", arguments={...})`
after importing this repo; `tool_help` on that path is the authority on the
parameters. README.md covers the secrets it needs (`pocketcasts-email`,
`pocketcasts-password`); "secret ... is not granted" means they are missing or
not granted to this tool.

Every value is a string. Operations (`op`):

| op | does |
|---|---|
| `list_podcasts` | subscriptions with folder and title. `folder` filters. `with-latest=true` adds each podcast's latest episode date and episode count (one request per podcast). |
| `list_folders` | folders with podcast counts, and how many podcasts have no folder. |
| `search` | search the Pocket Casts directory (`query`); gives uuids to subscribe with. A feed URL as `query` gives that feed's uuid (private feeds too). |
| `subscribe` | `podcast` = uuid(s) or feed URL(s), optional `folder`. |
| `unsubscribe` | `podcast` = uuid or title (several, comma-separated). Dry run unless `apply=true`. |
| `move_podcast` | `podcast`, `folder` (name, uuid, or `none`). |
| `create_folder` / `rename_folder` / `delete_folder` | `name`, `folder`. Deleting moves its podcasts to the top level; dry run unless `apply=true`. |
| `list_episodes` | one `podcast`; filters below. Shows number, date, state (unplayed, in-progress, played, archived), uuid, title. |
| `archive` / `unarchive` | one `podcast`, selected by filters; dry run unless `apply=true`. |

Episode filters (list_episodes, archive, unarchive): `from-number`,
`to-number` (inclusive), `title-regex`, `published-after` (inclusive),
`published-before` (exclusive), both `YYYY-MM-DD`, `only`
(`archived`/`unarchived`, list only), `episodes` (uuids). Archive and unarchive
refuse to run with no filter unless `all=true`. Episode numbers come from the
feed if it has them, else from the title (`#102`, `Episode 102`, or a leading
number like Easy German's `701: ...`); `number-regex` overrides that with a
regex whose first group is the number. Episodes with no parsable number never
match a number filter, so check the dry run's "N of M have a number" line.

## Rules that matter

- **Destructive operations are two-step.** Call without `apply`, show the user
  what it says it would do, get their go-ahead, then repeat the identical call
  with `apply=true`. For "unsubscribe from podcasts with nothing recent",
  that means listing the candidates, asking, and only then unsubscribing.
- Check what a dry run says before applying: the matched count, the number
  range, and the first and last rows. A number parsed from a title can be wrong
  (a title that starts with a year, say).
- A write is read back and reported `confirmed` or `NOT CONFIRMED`. On
  `NOT CONFIRMED` or a failure after a write, re-read (`list_*`) before
  retrying; a write may have arrived.
- The tool's description of an ambiguous podcast name is an error that lists
  the candidates. Use a uuid then.

## Worked examples

**Archive episodes 102 to 465 of Easy German.**

```text
{"op":"archive","podcast":"Easy German","from-number":"102","to-number":"465"}
```

Read the dry run (should say numbers 102-465 and a count near 364 minus any
already archived), then repeat with `"apply":"true"`. If `Easy German` matches
several shows (there are others with "Easy German" in the name), use the uuid
from `list_podcasts`.

**Unsubscribe from podcasts with no episodes in the last year.**
`{"op":"list_podcasts","with-latest":"true"}`, pick those whose
`latest_episode` is before a year ago (compute the date from today's date),
show that list to the user, and after they confirm call `unsubscribe` with the
uuids comma-separated and `apply=true`. Podcasts whose latest date shows `?`
could not be checked; list them separately, do not unsubscribe them.

**Suggest better folders for podcasts in "Uncategorized".**
`{"op":"list_folders"}` for the existing folders, then
`{"op":"list_podcasts","folder":"none"}` for podcasts with no folder (or
`"folder":"Uncategorized"` if that is a real folder). Propose a folder for
each from its title and the existing folder names, preferring existing
folders. Move only after the user agrees, with `move_podcast` (several
podcasts can go to one folder in one call).
