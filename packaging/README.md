# Packaging

Service templates and OS packaging assets for running `octo serve` as a
long-lived daemon.

## Running octo serve as a service

`octo serve` writes structured logs (Go `slog`, text format) to **stderr**;
the level is set by `OCTO_LOG_LEVEL` (`debug` | `info` | `warn` | `error`,
default `info`). It does not manage log files itself — the service manager
captures stderr (systemd → the journal, launchd → `StandardErrorPath`).

Run it under your init system with `--no-supervisor`: the init system is the
supervisor. octo's own restart path (binary upgrade, startup-only config
change) exits the worker with code **42**; `Restart=always` (systemd) /
`KeepAlive` (launchd) respawn it from the on-disk binary, so the upgrade takes
effect on restart.

### Linux — systemd (user service)

[`systemd/octo.service`](systemd/octo.service):

```sh
cp packaging/systemd/octo.service ~/.config/systemd/user/octo.service
$EDITOR ~/.config/systemd/user/octo.service     # fix the ExecStart path
$EDITOR ~/.octo/serve.env && chmod 600 ~/.octo/serve.env   # API key, access key
systemctl --user daemon-reload
systemctl --user enable --now octo
journalctl --user -u octo -f
```

### macOS — launchd (user agent)

[`launchd/dev.octo-agent.serve.plist`](launchd/dev.octo-agent.serve.plist):

```sh
cp packaging/launchd/dev.octo-agent.serve.plist ~/Library/LaunchAgents/
$EDITOR ~/Library/LaunchAgents/dev.octo-agent.serve.plist   # fix path + env
launchctl load ~/Library/LaunchAgents/dev.octo-agent.serve.plist
tail -f /tmp/octo-serve.log
```

> Both templates default to a loopback bind. Exposing the server on a LAN/public
> address requires `-addr :8080` (or similar) **and** an access key — see
> [SECURITY.md](../SECURITY.md).

## Windows installer

The double-click `octo-setup.exe` is built from
[`windows/octo.iss`](windows/octo.iss) by the release workflow. See the README's
Install section.
