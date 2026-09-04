# Home Assistant

Systemd2MQTT uses device-based MQTT discovery (Home Assistant 2024.11 or newer). Nothing to configure on the HA side beyond the MQTT integration: once the daemon publishes `homeassistant/device/<node_id>/config`, the device shows up under Settings, Devices, MQTT.

Everything below uses the example config (`node_id: office-kiosk`, device name `Office kiosk`). HA builds entity ids from the device name plus the entity name, so with another `device.name` the prefix changes.

## Entities

### Units

Each configured unit (and each template instance) gets four entities. For `grafana-kiosk.service`, `vlc@cam1.service` (Front door) and `vlc@cam2.service` (Parking):

| Entity | Type | What it does |
|---|---|---|
| `switch.office_kiosk_grafana_kiosk` | switch | ON starts the unit, OFF stops it. State is `ON` while the unit is `active`, `activating` or `reloading`. It only changes when systemd reports a new state, so a failed start snaps back to OFF instead of lying |
| `button.office_kiosk_restart_grafana_kiosk` | button | `systemctl restart` |
| `sensor.office_kiosk_grafana_kiosk_state` | diagnostic sensor | `active (running)`, `inactive (dead)`, `failed (failed)`. Its attributes carry `unit`, `scope`, `load_state`, `last_job`, `last_job_result`, `last_job_error`, `last_job_at`, `changed_at` |
| `binary_sensor.office_kiosk_grafana_kiosk_problem` | diagnostic problem | ON when the unit is `failed` or does not exist (`not-found`) |

Same four for `switch.office_kiosk_front_door` and `switch.office_kiosk_parking`.

Unit entities are unavailable while the daemon is offline or while the systemd manager for their scope is unreachable, and only then.

### Camera select

`select.office_kiosk_camera` comes from the exclusive `vlc@.service` template. Options are `off`, `Front door`, `Parking`. Picking one stops every other instance first (one by one, waiting for each stop job), then starts the chosen one. `off` stops them all.

The state is derived from what is really running: the label of the single active instance, `off` when none. Starting an instance by hand on the kiosk flips the select on its own. If two instances are active at the same time (two switches turned on together), the select shows the first one in config order and the daemon logs a warning. Use the select in automations and keep the individual switches for manual poking.

### Display

| Entity | Type | What it does |
|---|---|---|
| `switch.office_kiosk_display` | switch | ON writes `PowerSaveMode` 0, OFF writes the configured `display.off_mode` (3 = off by default). State is ON only when the mode is 0. Attributes: `power_save_mode`, `mode`, `off_mode` |
| `select.office_kiosk_display_power_mode` | config select | `on`, `standby`, `suspend`, `off`, the raw Mutter modes 0 to 3 |

Both go unavailable when Mutter reports mode -1 (no outputs, monitor unplugged, session not up). Moving the mouse on the kiosk wakes the display and the switch follows within a second.

### Daemon diagnostics

| Entity | Type | What it does |
|---|---|---|
| `binary_sensor.office_kiosk_session_bus` | connectivity | the user systemd manager is reachable. A `binary_sensor.office_kiosk_system_bus` appears only when a `scope: system` unit is configured |
| `sensor.office_kiosk_version` | sensor | daemon version, with `commit`, `started_at`, `hostname`, `bus_user_online`, `display_online`, `mqtt_reconnects` as attributes |
| `sensor.office_kiosk_started` | timestamp | when the daemon started |
| `button.office_kiosk_restart_systemd2mqtt` | restart button | the daemon exits cleanly and systemd starts it again |

That is 19 entities for the example config.

## Example automations

### Display off at night, on in the morning

```yaml
automation:
  - alias: Office kiosk display off at night
    triggers:
      - trigger: time
        at: "22:30:00"
    actions:
      - action: switch.turn_off
        target:
          entity_id: switch.office_kiosk_display

  - alias: Office kiosk display on in the morning
    triggers:
      - trigger: time
        at: "07:30:00"
    conditions:
      - condition: state
        entity_id: binary_sensor.workday_sensor
        state: "on"
    actions:
      - action: switch.turn_on
        target:
          entity_id: switch.office_kiosk_display
```

If your monitor behaves better with a lighter mode, use the select instead:

```yaml
      - action: select.select_option
        target:
          entity_id: select.office_kiosk_display_power_mode
        data:
          option: suspend
```

### Show the front door camera on motion, back to Grafana after

Uses the Camera select so the previous feed is stopped before the new one starts, and turns the display on in case it was off.

```yaml
automation:
  - alias: Office kiosk show front door on motion
    triggers:
      - trigger: state
        entity_id: binary_sensor.front_door_motion
        to: "on"
    actions:
      - action: switch.turn_on
        target:
          entity_id: switch.office_kiosk_display
      - action: select.select_option
        target:
          entity_id: select.office_kiosk_camera
        data:
          option: Front door
      - delay: "00:02:00"
      - action: select.select_option
        target:
          entity_id: select.office_kiosk_camera
        data:
          option: "off"
```

VLC runs fullscreen on top of grafana-kiosk, so stopping the camera is enough to get the dashboard back; grafana-kiosk keeps running underneath.

### Restart grafana-kiosk when it hangs

```yaml
automation:
  - alias: Office kiosk restart grafana-kiosk nightly
    triggers:
      - trigger: time
        at: "04:00:00"
    actions:
      - action: button.press
        target:
          entity_id: button.office_kiosk_restart_grafana_kiosk

  - alias: Office kiosk notify when a unit fails
    triggers:
      - trigger: state
        entity_id:
          - binary_sensor.office_kiosk_grafana_kiosk_problem
          - binary_sensor.office_kiosk_front_door_problem
          - binary_sensor.office_kiosk_parking_problem
        to: "on"
    actions:
      - action: notify.mobile_app_phone
        data:
          title: Office kiosk
          message: >-
            {{ trigger.to_state.name }}:
            {{ state_attr(trigger.entity_id.replace('_problem', '_state').replace('binary_sensor.', 'sensor.'), 'last_job_error') }}
```

## Removing the old command_line switches

The old `kiosk-http-automation-server` integration looked like this in `configuration.yaml`:

```yaml
command_line:
  - switch:
      name: Office kiosk display
      command_on: curl -s http://10.1.0.64:8080/display/on
      command_off: curl -s http://10.1.0.64:8080/display/off
  - switch:
      name: Office kiosk VLC
      command_on: curl -s http://10.1.0.64:8080/unit/vlc/start
      command_off: curl -s http://10.1.0.64:8080/unit/vlc/stop
```

1. Delete those blocks (and any `rest_command` or `shell_command` entries pointing at port 8080) from `configuration.yaml`.
2. Restart Home Assistant. The `switch.office_kiosk_display` and `switch.office_kiosk_vlc` command_line entities disappear.
3. Point automations, scripts and dashboards at the new entities:

| Old | New |
|---|---|
| `switch.office_kiosk_display` (command_line) | `switch.office_kiosk_display` (MQTT), same id once the old one is gone |
| `switch.office_kiosk_vlc` on | `select.office_kiosk_camera` with option `Front door` or `Parking` |
| `switch.office_kiosk_vlc` off | `select.office_kiosk_camera` with option `off` |
| `/unit/grafana-kiosk/restart` | `button.office_kiosk_restart_grafana_kiosk` |

If HA kept the old entity id around with a `_2` suffix on the new one, remove the old entity from Settings, Entities and rename the new one.

4. On the kiosk, stop and disable `kiosk-http-automation-server.service` (see the README).

## Notes

- Retained commands on `set` topics are ignored by the daemon, so publishing with the retain flag from a script does nothing useful. HA never does that for switches, selects or buttons.
- Everything the daemon publishes is retained, so HA sees the last known state right after its own restart, and the availability topics tell it whether that state is current.
- Removing a unit from the config and restarting leaves its retained topics on the broker. If you want them gone: `systemctl --user stop systemd2mqtt.service`, then `systemd2mqtt --clear-discovery` with the old config, edit the config, then `systemctl --user start systemd2mqtt.service`. HA drops the entities on the next discovery. The daemon has to be stopped first, a running one republishes its cached state right after the clear.
