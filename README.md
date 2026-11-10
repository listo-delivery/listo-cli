# listo

The command-line tool for developers on the Listo Delivery API. Log in from the
terminal, receive your webhooks on your own computer, fire test events, tail your
API request log, and create, read and cancel deliveries.

**Test mode is the default.** Everything you run uses your test credential and
moves no money. Live mode needs `--live` and, for anything that writes, a
confirmation.

## Install

```sh
# npm
npm install -g listo-cli

# Homebrew
brew install listo-delivery/tap/listo

# Install script (installs to $HOME/.listo/bin, verifies the checksum)
curl -fsSL https://cli.listo.delivery/install.sh | sh
```

Check it worked:

```sh
listo version
```

Shell completion for bash, zsh, fish and PowerShell:

```sh
listo completion zsh > "${fpath[1]}/_listo"
```

## Update

Once a day, on a terminal, listo prints one line on stderr when a newer version
exists, naming the command that upgrades the way you installed it:

| Installed with     | Upgrade with         |
| ------------------ | -------------------- |
| npm                | `npm i -g listo-cli` |
| Homebrew           | `brew upgrade listo` |
| The install script | `listo update`       |

`listo update` replaces the binary itself for an install-script copy, after
checking the download against Listo's release signing key and its checksum. For
a copy installed with npm or Homebrew it prints that tool's command instead.
`listo update --check` only says whether a newer version exists. Set
`LISTO_NO_UPDATE_NOTIFIER=1` to silence the daily line; it is never printed with
`--json` or when the output is piped.

## Log in

```sh
listo login
```

The CLI prints a code, opens your browser on the developer dashboard and waits.
Compare the code on the page with the one in your terminal, then authorize. The
credential is stored in your operating system's credential store (the macOS
Keychain, the Windows Credential Manager, or the Secret Service on Linux). When no
store is available it falls back to a file readable only by you, and tells you
which it used.

One approval is one mode. `listo login` signs you in to test mode; run
`listo login --live` to add live mode. `listo logout` ends the session on this
computer, and you can also revoke the computer in the dashboard under connected
apps.

Already have a key? Use it instead of logging in:

```sh
export LISTO_API_KEY=lst_test_...
listo whoami
```

## Listen: receive webhooks on your computer

```sh
listo listen --forward-to http://localhost:3000/webhooks/listo
```

`listen` receives every event of your account live and forwards each one to your
local server, signed. It prints a signing secret that stays the same every time
you start it on this computer (`listo listen --print-secret` prints it and
exits). Set it in your app and verify the `X-Listo-Signature` header with the
same helper from the SDK you use in production.

```
Ready [TEST]. Your webhook signing secret is whsec_... (set LISTO_WEBHOOK_SECRET in your app)
12:04:11  delivery.status_changed  evt_3kd...  -> 200 in 41 ms
```

Use `--events delivery.status_changed,store.updated` to forward only some types.

## Trigger a test event

```sh
listo trigger delivery.status_changed
```

This records a sample event and sends it to your `listen` sessions. It does not
call your webhook URL unless you add `--deliver`.

## Tail your API logs

```sh
listo logs tail
listo logs tail --status failed
```

## Deliveries

```sh
listo deliveries create --data @order.json
listo deliveries get dlv_...
listo deliveries cancel dlv_... --reason customer_canceled
listo simulate returned
```

`deliveries create` in live mode (`--live`) first prices the trip, shows the
store, the drop-off and the amount, and asks you to confirm before booking
exactly that quote. Answer `y` to continue; anything else cancels. With no
terminal (a script, a pipe) the CLI never proceeds on its own: add `--yes`.

## Options on every command

| Option   | What it does                                                                  |
| -------- | ----------------------------------------------------------------------------- |
| `--live` | Use live mode (real money). Test mode is the default.                         |
| `--yes`  | Do not ask for confirmation.                                                  |
| `--json` | Print JSON. Always on when the output is not a terminal.                      |
| `--lang` | Output language, `es` or `en`. Default: your computer's language.             |

Output is JSON whenever it is piped, colour only appears on a terminal, and
`NO_COLOR` turns colour off. Exit codes: `0` success, `1` the API refused or the
operation failed, `2` you typed the command wrong.

## Environment variables

| Variable                    | Meaning                                                                                  |
| --------------------------- | ---------------------------------------------------------------------------------------- |
| `LISTO_API_KEY`             | An API key (`lst_test_...` or `lst_live_...`), used instead of a stored login.           |
| `LISTO_API_URL`             | The API address. Default `https://api.listo.delivery`.                                   |
| `LISTO_LOGIN_URL`           | The sign-in address. Default: the API address with `api` replaced by `login`.            |
| `LISTO_CABLE_URL`           | The live socket address. Default `wss://cable.listo.delivery/cable`.                     |
| `LISTO_LANG`                | Output language, `es` or `en`. `--lang` wins; then `LC_ALL`, `LC_MESSAGES`, `LANG`.      |
| `LISTO_CONFIG_DIR`          | Where the CLI keeps its files. Default: your OS config folder, under `listo`.            |
| `LISTO_NO_KEYRING`          | Set to 1 to keep credentials in the file instead of the OS credential store.             |
| `LISTO_NO_UPDATE_NOTIFIER`  | Set to 1 to silence the once-a-day notice that a new version exists.                     |
| `NO_COLOR`                  | Turns colour off.                                                                        |

## Development

The module is `github.com/listo-delivery/listo-cli` and needs Go 1.27.

```sh
go vet ./...
go test ./...
go build ./cmd/listo
```

The version is typed once, in the repository the maintainers release from, and
the release build sets it from the tag.

## Licence

MIT, Cheto Labs, S.A.P.I. de C.V.
