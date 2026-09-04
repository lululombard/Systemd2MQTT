# Systemd2MQTT

A small Go daemon that exposes a whitelist of systemd units and the display power state of a Linux kiosk to Home Assistant over MQTT, with device-based discovery. Turn the screen off at night, start a camera feed when someone rings the door bell, restart grafana-kiosk when it hangs, all from HA entities with live state.

## Why

The office kiosk used to run a small Flask app (`kiosk-http-automation-server`) that HA drove with `command_line` switches and `curl`. That had three problems:

- Every Ubuntu upgrade broke its venv (distro Python 3.10 to 3.12 to 3.14), so it spent more time down than up.
- The HTTP API was unauthenticated and HA had no live state: the switches were optimistic and drifted from reality.
- Display control was `xset dpms`, which does nothing under Wayland. The kiosk is Wayland only now.

Systemd2MQTT replaces it with one static Go binary built by GitHub Actions. The kiosk only ever downloads a release, so distro upgrades cannot break it. Display power goes through Mutter's `org.gnome.Mutter.DisplayConfig` on the session bus, which is the supported way under GNOME Wayland.

## Features

- Start, stop and restart a configured list of systemd units from Home Assistant. User units (session bus) and system units (system bus, with a polkit rule) are both supported, chosen per unit with `scope:`.
- Template units (`vlc@.service`) with one instance per camera, plus an optional exclusive select that stops the other instances before starting the one you picked.
- Display on/off through Mutter's `PowerSaveMode`, plus a mode select (`on`, `standby`, `suspend`, `off`).
- Honest state. Switches only change when systemd reports a new `ActiveState`, never optimistically. A failed start snaps the switch back to OFF and lights a problem sensor.
- Per-scope availability: a dead bus greys out only its units, a missing monitor greys out only the display, the daemon itself never exits because something is unreachable.
- Home Assistant device-based MQTT discovery (HA 2024.11 or newer). Zero YAML on the HA side.
- MQTT over plain TCP or TLS (CA, optional client certificate), LWT, automatic reconnect with resubscribe.
- Runs as a systemd user unit with `Type=notify` and a watchdog. A broken config exits with code 2 and is not restarted in a loop.
- Retained commands are ignored, so a stale `ON` on the broker never starts a unit after a restart.

## What you get in Home Assistant

With the example config the MQTT integration shows one device, "Office kiosk", with 19 entities:

| Entity | What it does |
|---|---|
| `switch.office_kiosk_grafana_kiosk` | start/stop `grafana-kiosk.service` |
| `switch.office_kiosk_front_door`, `switch.office_kiosk_parking` | start/stop `vlc@cam1.service` and `vlc@cam2.service` |
| `select.office_kiosk_camera` | `off`, `Front door`, `Parking`: picks exactly one camera, stopping the others first |
| `button.office_kiosk_restart_grafana_kiosk` and friends | restart a unit |
| `switch.office_kiosk_display` | display on/off (`off` writes `display.off_mode`) |
| `select.office_kiosk_display_power_mode` | `on`, `standby`, `suspend`, `off` |
| `sensor.office_kiosk_grafana_kiosk_state` and friends | `active (running)` style diagnostic, with the full unit attributes |
| `binary_sensor.office_kiosk_grafana_kiosk_problem` and friends | ON when the unit is `failed` or `not-found` |
| `binary_sensor.office_kiosk_session_bus` | connectivity of the user systemd manager (a `system_bus` one appears when a `scope: system` unit is configured) |
| `sensor.office_kiosk_version`, `sensor.office_kiosk_started` | daemon version and start time |
| `button.office_kiosk_restart_systemd2mqtt` | restart the daemon itself |

See [docs/home-assistant.md](docs/home-assistant.md) for the full list and automation examples.

## Config

The daemon reads `~/.config/systemd2mqtt/config.yaml` (override with `--config` or `$SYSTEMD2MQTT_CONFIG`). Keep it mode 0600, it holds the broker password. Only `mqtt.broker` is required. Unknown keys are an error.

```yaml
node_id: office-kiosk
device: { name: Office kiosk, suggested_area: Office }
mqtt:
  broker: tcp://10.2.0.1:1883        # ssl://host:8883 for TLS, then set tls.ca_file (and cert/key for mTLS)
  username: systemd2mqtt
  password: "change-me"              # or password_file: /home/lululombard/.config/systemd2mqtt/mqtt_password
units:
  - { name: grafana-kiosk.service, scope: user, friendly_name: Grafana kiosk, icon: mdi:monitor-dashboard }
  # scope: system also works; it then needs the polkit rule from --print-polkit-rule
templates:
  - name: vlc@.service                # instances become vlc@cam1.service, vlc@cam2.service
    scope: user
    friendly_name: Camera
    icon: mdi:cctv
    exclusive: true                   # adds a "Camera" select (off / Front door / Parking); picking one stops the others first
    instances:
      - { name: cam1, friendly_name: Front door }
      - { name: cam2, friendly_name: Parking }
display:
  off_mode: off                      # standby|suspend|off, PowerSaveMode 1|2|3 ("suspend" = old xset behaviour)
```

### Reference

| Key | Default | Notes |
|---|---|---|
| `node_id` | sanitized hostname | `[a-z0-9_-]`, max 64. Used in every topic and in the discovery object id. Renaming it orphans retained topics, stop the daemon and run `--clear-discovery` with the old config first |
| `device.name` | `node_id` | device name in HA, also the prefix of every entity id |
| `device.manufacturer` | `lululombard` | |
| `device.model` | `Systemd2MQTT` | |
| `device.suggested_area` | empty | |
| `mqtt.broker` | required | `tcp://host:1883`, or `ssl://`, `tls://`, `mqtts://`, `wss://` for TLS. `ws://` also works |
| `mqtt.client_id` | `systemd2mqtt-<node_id>` | |
| `mqtt.username` | empty | |
| `mqtt.password` | empty | `password_file` wins when both are set |
| `mqtt.password_file` | empty | path to a file holding the password, trailing whitespace trimmed |
| `mqtt.base_topic` | `systemd2mqtt` | every topic lives under `<base_topic>/<node_id>` |
| `mqtt.keepalive` | `30s` | |
| `mqtt.qos` | `1` | 0, 1 or 2 |
| `mqtt.tls.ca_file` | empty | PEM CA bundle. Only used when the broker scheme is a TLS one, otherwise a warning is logged |
| `mqtt.tls.cert_file`, `mqtt.tls.key_file` | empty | client certificate for mTLS, both or neither |
| `mqtt.tls.insecure_skip_verify` | `false` | skips certificate checks, logs a warning |
| `homeassistant.enabled` | `true` | publish discovery and listen to the HA birth message |
| `homeassistant.discovery_prefix` | `homeassistant` | |
| `homeassistant.status_topic` | `homeassistant/status` | |
| `units` | `grafana-kiosk.service` and `vlc.service`, both `scope: user` | the default applies only when neither `units` nor `templates` is present. An explicit `units: []` means none |
| `units[].name` | required | full unit name, `foo.service` or an instance like `vlc@cam1.service` |
| `units[].scope` | required | `user` or `system` |
| `units[].id` | slug of the name | `vlc@cam1.service` becomes `vlc_cam1`. Must be unique |
| `units[].friendly_name` | name without `.service`, capitalised | entity name in HA |
| `units[].icon` | `mdi:cog-play` | |
| `units[].restart_button` | `true` | |
| `units[].problem_sensor` | `true` | |
| `templates[].name` | required | template unit name like `vlc@.service` |
| `templates[].scope`, `id`, `friendly_name`, `icon`, `restart_button`, `problem_sensor` | as for units | apply to every instance |
| `templates[].exclusive` | `false` | adds one select entity per template that runs at most one instance |
| `templates[].instances` | required | list of `cam1` strings or `{ name: cam1, friendly_name: Front door }` |
| `display.enabled` | `true` | |
| `display.off_mode` | `off` | what the switch OFF writes: `standby` (1), `suspend` (2) or `off` (3) |
| `display.expose_mode_select` | `true` | the `Display power mode` select |
| `systemd.poll_interval` | `30s` | full re-query of every unit, also the liveness check of the bus. Keep it around a minute at most |
| `systemd.job_timeout` | `60s` | how long a start/stop/restart may take before it is reported as `local-timeout` |
| `systemd.job_mode` | `replace` | systemd job mode, `replace` or `fail` |
| `log_level` | `info` | `debug`, `info`, `warn`, `error` |

Environment overrides: `SYSTEMD2MQTT_MQTT_BROKER`, `SYSTEMD2MQTT_MQTT_USERNAME`, `SYSTEMD2MQTT_MQTT_PASSWORD`.

### Template units and the exclusive select

Any `units[].name` may already be an instance (`vlc@cam1.service`). The `templates:` list is sugar for that: every instance is expanded into an ordinary unit with its own switch, restart button, state sensor and problem sensor, exactly like a plain unit. The id is the slug of the instance name (`vlc_cam1`), the friendly name is the instance's own or `<friendly_name> <instance>`.

`exclusive: true` adds one select per template whose options are `off` plus the instance labels. Picking an option stops every other instance one by one, waits for each stop job, then starts the chosen one. The select state is derived from the instances' real `ActiveState`, so starting an instance by hand with `systemctl --user start vlc@cam1.service` flips the select to `Front door` with no MQTT command involved. Turning two instance switches on at the same time still gives you two fullscreen VLC windows, only the select sequences them. Automations should use the select.

## MQTT topics

`R` is `<base_topic>/<node_id>`. Everything published is retained.

| Topic | Payload |
|---|---|
| `homeassistant/device/<node_id>/config` | discovery JSON |
| `R/availability` | `online` / `offline` (LWT) |
| `R/bus/<user\|system>/availability` | `online` / `offline` per systemd manager |
| `R/unit/<id>/state`, `R/unit/<id>/attributes` | `ON` iff ActiveState is `active`, `activating` or `reloading`; JSON with `unit`, `scope`, `active_state`, `sub_state`, `load_state`, `last_job`, `last_job_result`, `last_job_error`, `last_job_at`, `changed_at` |
| `R/template/<id>/state` | the active instance label or `off` (exclusive templates only) |
| `R/display/state`, `R/display/mode/state`, `R/display/attributes`, `R/display/availability` | `ON`/`OFF`, `on|standby|suspend|off`, `{power_save_mode, mode, off_mode}`, `offline` when Mutter reports -1 |
| `R/daemon/state` | `{version, commit, started_at, hostname, bus_user_online, bus_system_online, display_online, mqtt_reconnects}` |

Commands: `R/unit/<id>/set` (`ON`, `OFF`, `RESTART`), `R/template/<id>/set` (`off` or an instance label), `R/display/set` (`ON`/`OFF`), `R/display/mode/set` (`on|standby|suspend|off`), `R/daemon/restart`.

## Install on the kiosk

Everything runs as the logged-in user. `sudo` is only needed to retire the old system units and to install a polkit rule for `scope: system` units.

### 0. Prerequisites

Create an MQTT user on the broker with read/write on `systemd2mqtt/#` and `homeassistant/#`, then on the kiosk:

```sh
sudo apt install -y curl jq mosquitto-clients
```

### 1. Convert VLC to a user template unit

The old `/etc/systemd/system/vlc.service` (`User=lululombard`, `Environment=DISPLAY=:0`) does not work under Wayland. Replace it with the `vlc@.service` user template, one instance per camera. Stream URLs live outside the repo in `~/.config/vlc-cams/<instance>.env`.

```sh
sudo systemctl disable --now vlc.service
sudo mv /etc/systemd/system/vlc.service /etc/systemd/system/vlc.service.retired-wayland
sudo systemctl daemon-reload

mkdir -p -m 0700 ~/.config/vlc-cams
printf 'VLC_URL=rtsp://10.0.0.1:7447/your-stream-id\n' > ~/.config/vlc-cams/cam1.env
printf 'VLC_URL=rtsp://10.0.0.1:7447/other-stream-id\n' > ~/.config/vlc-cams/cam2.env
chmod 0600 ~/.config/vlc-cams/*.env

install -m 0644 deploy/vlc@.service ~/.config/systemd/user/vlc@.service
systemctl --user daemon-reload
systemctl --user start vlc@cam1.service   # proves VLC plays under Wayland
systemctl --user stop vlc@cam1.service
```

Adding a camera later is one more env file plus one line under `instances:` in `config.yaml`.

### 2. Install the release

```sh
curl -fsSL https://raw.githubusercontent.com/lululombard/Systemd2MQTT/main/deploy/install.sh | bash -s -- latest
```

Or from a checkout: `deploy/install.sh latest` (or a tag like `v1.0.0`). The script resolves the release through the GitHub API, downloads the tarball for your architecture and `checksums.txt`, verifies the sha256, installs the binary atomically to `~/.local/bin/systemd2mqtt`, copies `config.example.yaml` to `~/.config/systemd2mqtt/config.yaml` (0600) if none exists, installs the two user units, runs `daemon-reload` and restarts the daemon if it was running.

Already have the tarball (offline box, or you built it yourself)? Extract it and run `deploy/install.sh --local <dir>`.

### 3. Configure

```sh
$EDITOR ~/.config/systemd2mqtt/config.yaml       # broker, username, password
systemd2mqtt --check-config                      # prints the resolved config, password masked
systemd2mqtt --dump-discovery | head
```

### 4. Polkit rule (only for `scope: system` units)

User units need no rule. If you add a unit with `scope: system`, the daemon calls `StartUnit` on the system bus and polkit asks for admin authentication unless a rule allows it:

```sh
systemd2mqtt --print-polkit-rule > /tmp/10-systemd2mqtt.rules
sudo install -m 0644 -o root -g root /tmp/10-systemd2mqtt.rules /etc/polkit-1/rules.d/
```

The rule grants the current user `start`, `stop` and `restart` on exactly the configured system units (a prefix match for system-scope templates) and nothing else. Check it works without a prompt:

```sh
busctl call org.freedesktop.systemd1 /org/freedesktop/systemd1 org.freedesktop.systemd1.Manager StartUnit ss your-unit.service replace
busctl call org.freedesktop.systemd1 /org/freedesktop/systemd1 org.freedesktop.systemd1.Manager StartUnit ss cron.service replace   # must be denied
```

The committed `deploy/10-systemd2mqtt.rules` is the rendered output for the example config (comment-only, since it has no system units) and CI checks it stays in sync.

### 5. Run it

```sh
systemd2mqtt                                     # foreground smoke run, Ctrl-C to stop
systemctl --user enable --now systemd2mqtt.service
journalctl --user -u systemd2mqtt -f
mosquitto_sub -v -t 'systemd2mqtt/office-kiosk/#' -t 'homeassistant/device/office-kiosk/config'
```

The unit is `PartOf=graphical-session.target`, so it starts after autologin and stops on logout. That is on purpose: the display and VLC need the session anyway.

### 6. Retire the old HTTP server

```sh
sudo systemctl disable --now kiosk-http-automation-server.service
sudo systemctl reset-failed kiosk-http-automation-server.service
```

Then replace the `command_line` switches in HA with the new entities, see [docs/home-assistant.md](docs/home-assistant.md). Reboot once and check `systemctl --user is-active systemd2mqtt.service` after autologin.

### Upgrade

```sh
deploy/install.sh latest
```

No sudo, and the config is left alone.

## CLI

```
systemd2mqtt [--config PATH] [flag]
  --config PATH        config file (default ~/.config/systemd2mqtt/config.yaml or $SYSTEMD2MQTT_CONFIG)
  --version            print version, commit and build date
  --check-config       load and validate the config, print it with secrets masked, exit
  --dump-discovery     print the Home Assistant discovery payload as JSON, exit
  --print-polkit-rule  print the polkit rule for the configured system-scope units, exit
  --clear-discovery    blank every retained topic of this node on the broker, exit
```

Exit codes: 0 clean, 1 runtime error, 2 config error. `--check-config`, `--dump-discovery` and `--print-polkit-rule` never touch files or the network, CI runs them on the example config. The daemon and `--clear-discovery` also read `mqtt.password_file` and the `mqtt.tls.*` files before connecting and exit 2 when one is missing or unreadable.

## Troubleshooting

- **A unit did not start after a daemon restart even though the broker has `ON` retained on its `set` topic.** Working as intended. Retained messages on command topics are ignored so a stale command never replays; the journal says `ignoring retained command`. Clear it with `mosquitto_pub -r -n -t systemd2mqtt/<node>/unit/<id>/set`.
- **Display entities unavailable.** Mutter reports `PowerSaveMode = -1` when it has no outputs (monitor unplugged, session not up yet). The daemon publishes `display/availability offline` and refuses to write until the mode is known again. Check with `busctl --user get-property org.gnome.Mutter.DisplayConfig /org/gnome/Mutter/DisplayConfig org.gnome.Mutter.DisplayConfig PowerSaveMode`.
- **`last_job_result: error` with `Interactive authentication required` or `AccessDenied`.** That is polkit refusing a `scope: system` unit. Install the rule from `--print-polkit-rule` (step 4) and check the unit name in the rule matches the config.
- **The service is dead and `systemctl --user status` says exit code 2.** The config did not validate (unknown key, bad scope, missing broker), or `mqtt.password_file` / `mqtt.tls.*` points at a file that cannot be read. `RestartPreventExitStatus=2` stops the restart loop on purpose. Run `systemd2mqtt --check-config` (which does not open those files, so also check the paths in the journal line), fix it, then `systemctl --user restart systemd2mqtt`.
- **Switch snaps back to OFF right after turning it on.** The start job failed. The problem binary sensor turns on and `last_job_error` in the unit attributes has the reason. `systemctl --user status <unit>` on the kiosk has the rest.
- **Entities exist but everything is unavailable.** `R/availability` is the LWT. Either the daemon is not running or it lost the broker. `journalctl --user -u systemd2mqtt` shows the reconnect attempts.
- **Renamed `node_id` or a unit id and now HA shows duplicates.** Stop the daemon first, otherwise it republishes its cached state on the next reconcile and the retained topics come straight back: `systemctl --user stop systemd2mqtt.service`, then `systemd2mqtt --clear-discovery` with the old config, edit the config, then `systemctl --user start systemd2mqtt.service`.

## Development

Go 1.26 or newer.

```sh
make build              # static binary ./systemd2mqtt with version info from git
make test               # go test -race -count=1 ./...
make vet
make fmt                # fails when gofmt would change something
make print-polkit-rule  # renders the rule for config.example.yaml
make snapshot           # goreleaser build --snapshot --clean --single-target
```

CI (`.github/workflows/ci.yml`) runs on pull requests and pushes to `main`: tidy check, vet, race tests, amd64 and arm64 cross builds, `--check-config` and `--dump-discovery` on the example config, a diff of `--print-polkit-rule` against `deploy/10-systemd2mqtt.rules`, and a GoReleaser snapshot build.

Releasing is tagging:

```sh
git tag -a v1.0.0 -m v1.0.0
git push origin v1.0.0
```

`release.yml` runs the tests and `goreleaser release --clean`, which publishes a GitHub release with `systemd2mqtt_<version>_linux_{amd64,arm64}.tar.gz` (binary, LICENSE, README, `config.example.yaml`, `deploy/`) and `checksums.txt`. `install.sh latest` on the kiosk then picks it up.

## License

MIT, see [LICENSE](LICENSE).
