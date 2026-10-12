# caos-pocketcasts

A [caos](https://github.com/Metta-AI/caos) tool for managing a Pocket Casts
account: list, add and remove podcasts and folders, move a podcast to a
different folder, list a podcast's episodes, and archive or unarchive them.

There is one tool, `caos-tools/pocketcasts`, selected by its `op` parameter. It
is a single stdlib-only Go program (`worker.go`) run on caos' `std/go` image.

## Where the API comes from

Pocket Casts has no public API. This talks to the endpoints the official
open-source Android app uses
([Automattic/pocket-casts-android](https://github.com/Automattic/pocket-casts-android),
`SyncService.kt` and `sync_api.proto`), which are also what the community
client libraries wrap. They can change without notice.

| what | endpoint |
|---|---|
| log in | `POST api.pocketcasts.com/user/login_pocket_casts` (JSON) |
| subscriptions and folders | `POST /user/podcast/list` |
| per-episode state (archived, played) | `POST /user/podcast/episodes` (JSON; `isDeleted` is "archived") |
| every write: subscribe, unsubscribe, folders, move, archive | `POST /user/sync/update` (protobuf `SyncUpdateRequest`) |
| episode titles and dates | `GET podcast-api.pocketcasts.com/podcast/full/<uuid>` (public, no login) |
| search | `POST podcast-api.pocketcasts.com/discover/search` (response shape inferred, matched by walking the JSON) |

The Go image has no module cache, so the protobuf encoding is hand-written.
`op=selftest` checks it against hand-computed bytes without needing an account.

**Verified against the real service:** `search`, the public episode feed
(Easy German: 728 episodes, `has_more_episodes: false`, titles like
`701: ...`), and the protobuf encoder (`selftest`).
**Not yet verified, because it needs the account's credentials:** login, the
JSON shape of `/user/podcast/list` and `/user/podcast/episodes`, and that
`sync/update` accepts these records. The tool reads back every write and says
`NOT CONFIRMED` rather than assuming, and it has a protobuf fallback for
`podcast/list`. Expect to fix details on the first real run.

## Secrets

The tool logs in with the account's email and password, read from two caos
secrets, `pocketcasts-email` and `pocketcasts-password`. Values never appear in
arguments, results or this repo; caos injects them into the worker at
`/secret/<name>`. The account must have a Pocket Casts password (an account
that only ever signed in with Google or Apple needs one set first).

The secrets live in your secret directory (`$CAOS_SECRETS_DIR` in caos'
README). **Pushing replaces the server's copy of the whole directory**, so add
these two files to the directory you already use (with `github-token` etc.)
rather than pushing a new one.

```text
# $CAOS_SECRETS_DIR/pocketcasts-email
value=you@example.com
reader:@@=git+https://github.com/malcolmredheron/caos-pocketcasts?ref=refs/heads/main&dir=caos-tools/pocketcasts
```

```text
# $CAOS_SECRETS_DIR/pocketcasts-password
value=<password>
reader:@@=git+https://github.com/malcolmredheron/caos-pocketcasts?ref=refs/heads/main&dir=caos-tools/pocketcasts
```

The value must be verbatim, with no trailing newline. To keep a password out
of an editor, write the file with `printf`:

```sh
cd "$CAOS_SECRETS_DIR"
printf 'value=%s\nreader:@@=%s\n' "$PASSWORD" \
  'git+https://github.com/malcolmredheron/caos-pocketcasts?ref=refs/heads/main&dir=caos-tools/pocketcasts' \
  > pocketcasts-password
```

(or `value:@=<file>` to read the value from a file). Then push:

```sh
nix run github:Metta-AI/caos#caos-cli -- secrets-push --dir="$CAOS_SECRETS_DIR" --server="$CAOS_SERVER"
```

A secret pushed later takes effect on the next request; no restart.

**What the grant means.** `ref=refs/heads/main` grants the secrets to whatever
`caos-tools/pocketcasts` is on `main`, so anyone who can push to this repo can
publish a version that reads your password. That is fine for a private repo
only you can write to. To grant just the code you have reviewed, use
`rev=<commit sha>` instead of `ref=refs/heads/main`, and update the line when
you review a newer commit.

The grant matches the tool as published here. Running an unpublished copy from
a working layer (`sources/caos-pocketcasts/99-dirty/...`) is not granted and
fails with "secret ... is not granted"; publish, then import the published
commit.

## Using it

In a caos session, import this repo and run the tool by path:

```text
import_source(source="https://github.com/malcolmredheron/caos-pocketcasts.git",
              into="sources/caos-pocketcasts/00-base")
tool_help(path="sources/caos-pocketcasts/00-base/caos-tools/pocketcasts")
run_tool(path="sources/caos-pocketcasts/00-base/caos-tools/pocketcasts",
         arguments={"op": "list_folders"})
```

See `AGENTS.md` for the operations and worked examples.
