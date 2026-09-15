# nad_remote

Pure-Python control for NAD network amplifiers, reverse-engineered from the
unmaintained **NAD Remote** Android app (`com.nadelectronics.nad.remote`,
versionCode 28). Built for headless / IoT use — no Android, no old phone, no GUI.

Verified live against a **NAD C338** (firmware 1.76). Also implements the
protocols for the **C368 / C388** and the **D 7050**, decoded from the same APK.

## What it controls

Power on/off · volume (absolute dB + relative steps) · input source · mute ·
status readback (model, firmware, power, source, volume, mute).

## Requirements

- Python 3.8+
- Optional: `zeroconf` for mDNS auto-discovery (`pip install zeroconf`).
  Without it, discovery falls back to a TCP port sweep of your `/24`.

The amp must have **network standby enabled** on the unit
(Settings → *Control Setup / Network Standby → On*) or it won't answer while off.

## CLI

```bash
# find amps on the LAN
python nad_remote.py discover

# everything below auto-discovers if you omit --host
python nad_remote.py --host 192.168.1.20 status
python nad_remote.py --host 192.168.1.20 sources
python nad_remote.py --host 192.168.1.20 power on
python nad_remote.py --host 192.168.1.20 volume -45      # dB
python nad_remote.py --host 192.168.1.20 volume -         # step down
python nad_remote.py --host 192.168.1.20 source Coax1     # token, name, or index
python nad_remote.py --host 192.168.1.20 mute on
```

## Python API

```python
from nad_remote import NADAmp, discover

# explicit host (fastest, best for a fixed IoT install — give the amp a DHCP reservation)
with NADAmp("192.168.1.20") as amp:      # model auto-detected on connect
    amp.power_on()
    amp.set_volume(-45)                    # real dB, rounded to 0.5
    amp.set_source("Coax1")                # or "Coaxial 1", or index 4
    amp.mute(False)
    print(amp.get_status().as_dict())

# or discover
amp = NADAmp.autodiscover()
```

`get_status()` returns a `NADStatus` with `.power` (bool), `.mute` (bool),
`.source` (friendly name), `.volume_db` (float), `.model`, `.version`.

### Sources

| Model        | On-wire form            | Names |
|--------------|-------------------------|-------|
| C338         | token (`Coax1`, `Opt1`) | Streaming, BT/Wi-Fi, TV, Phono, Coaxial 1/2, Optical 1/2 |
| C368 / C388  | 1-based index           | Optical 1/2, Coaxial 1/2, Phono, Line 1/2, Bluetooth (varies by config) |
| D7050        | 0-based index           | Coaxial 1/2, Optical 1/2, Computer, AirPlay, Dock, Bluetooth |

`amp.set_source()` accepts the friendly name, the wire token, or the integer index.

## IoT integration

Everything is a plain TCP call, so it drops into any pipeline. Minimal MQTT bridge:

```python
import json, paho.mqtt.client as mqtt
from nad_remote import NADAmp

HOST = "192.168.1.20"

def on_message(cli, _u, msg):
    cmd = msg.payload.decode()
    with NADAmp(HOST) as amp:
        if cmd == "on":        amp.power_on()
        elif cmd == "off":     amp.power_off()
        elif cmd.startswith("vol:"):    amp.set_volume(float(cmd[4:]))
        elif cmd.startswith("src:"):    amp.set_source(cmd[4:])
        cli.publish("nad/state", json.dumps(amp.get_status().as_dict()))

c = mqtt.Client(); c.on_message = on_message
c.connect("localhost"); c.subscribe("nad/cmd"); c.loop_forever()
```

Notes for reliability in an always-on service:
- The amp accepts **one control connection at a time**. Open, act, read, close —
  don't hold the socket open across the whole daemon if the NAD app / other
  integrations may also connect. (This library opens per-`with`-block.)
- After `power_on()` the C338 reboots its control interface and goes silent for
  ~1–2 s. `get_status()` already retries across that gap.
- Give the amp a static DHCP lease so `--host` never changes.

## Auto-follow a Samsung TV (`main.py`)

Turns the amp on when the TV turns on — switches it to the TV input and sets a
fixed reference level so the **TV** controls actual loudness — and powers it off
when the TV goes to standby. **Fully local, no SmartThings cloud, no token.**

How it detects TV power: it reads the TV's own Tizen endpoint
`http://<tv-ip>:8001/api/v2/` → `device.PowerState` (`on` / `standby`). A
Samsung-TV-as-SmartThings-hub keeps its network alive in standby, so this is
always reachable and accurate. (Verified on a KQ55SF85AEXKR.)

```bash
python main.py --status     # show TV + amp state
python main.py --once       # reconcile a single time
python main.py              # run the daemon (Ctrl-C to stop)
```

Configure via env vars (or edit the constants at the top of the file):

| Var | Default | Meaning |
|-----|---------|---------|
| `TV_IP` | `192.168.1.10` | Samsung TV address |
| `AMP_IP` | `192.168.1.20` | NAD amp address |
| `AMP_SOURCE` | `TV` | input to select on power-on (`None` to leave it) |
| `AMP_REFERENCE_DB` | `-20` | fixed level set on power-on (see note below) |
| `POWER_OFF_WITH_TV` | `1` | also power the amp off on TV standby |
| `POLL_INTERVAL` | `3` | seconds between TV polls |

**About `AMP_REFERENCE_DB` and "TV controls the volume":** for the TV's volume to
actually change loudness, the TV must attenuate its **optical/digital** output.
The C338 has no HDMI, so HDMI-ARC/CEC volume passthrough is not available — only
optical/coax. Some Samsung TVs output **fixed full-scale** over optical and their
volume slider does nothing downstream. Test yours: if the TV volume has no effect,
your amp reference *is* your volume — set `AMP_REFERENCE_DB` to a comfortable
level and control volume via this tool instead. Starts at a safe **−20 dB** (0 dB
is near-max on a C338); raise toward 0 if the TV can't get loud enough.

### Install as a service (systemd, on your always-on Linux host)

```bash
sudo mkdir -p /opt/nad-remote
sudo cp nad_remote.py main.py /opt/nad-remote/
sudo cp tv-amp-sync.service /etc/systemd/system/
sudoedit /etc/systemd/system/tv-amp-sync.service   # set User + the Environment= values
sudo systemctl daemon-reload
sudo systemctl enable --now tv-amp-sync
journalctl -u tv-amp-sync -f                        # watch it
```

### Alternative: Home Assistant

If you run Home Assistant, you already have a Samsung TV integration that exposes
the TV's power as an entity — skip the polling and drive the amp from an
automation. Register the amp as a shell command and trigger on the TV entity:

```yaml
# configuration.yaml
shell_command:
  amp_tv_on: "python3 /config/nad/nad_remote.py --host 192.168.1.20 source TV && \
              python3 /config/nad/nad_remote.py --host 192.168.1.20 power on && \
              python3 /config/nad/nad_remote.py --host 192.168.1.20 volume -20"
  amp_off:   "python3 /config/nad/nad_remote.py --host 192.168.1.20 power off"

# automations.yaml
- alias: Amp follows TV
  triggers:
    - trigger: state
      entity_id: media_player.living_room_tv
      to: "on"
  actions:
    - action: shell_command.amp_tv_on
- alias: Amp off with TV
  triggers:
    - trigger: state
      entity_id: media_player.living_room_tv
      to: "off"
  actions:
    - action: shell_command.amp_off
```

### Alternative: native SmartThings Routine (no extra host)

If you have no always-on host, the only zero-extra-hardware path is a **SmartThings
Edge LAN driver** running on the TV-hub that exposes the amp as a device; then a
SmartThings *Routine* ("When TV turns on → turn on Amp") handles the rule. The
C338 text protocol (below) ports cleanly to Lua. Caveat: this requires your TV-hub
to allow custom Edge driver channels (model/firmware dependent), and note that
SmartThings personal access tokens now expire after 24 h, so a token-polling
script is **not** a durable option — use the local daemon above or an Edge driver.

## Protocol reference (for future maintenance)

Decoded from the APK; kept here so this survives without the app.

### Text protocol — C338 / C368 / C388 / M32 / AVR receivers
- **TCP port 30001.** ASCII. Requests terminated with CR (`\r`); responses are
  `Key=Value` terminated with CR+LF.
- Set: `Main.Power=On`, `Main.Volume=-45.0`, `Main.Source=Coax1`, `Main.Mute=On`
- Relative: `Main.Volume+`, `Main.Volume-`
- Query: append `?` — `Main.Power?` → `Main.Power=On`
- Volume is **real dB** (C338 range ≈ −80.0 … +12.0, 0.5 steps). No auth/handshake.
- Discovery: mDNS `_googlecast._tcp.local.`, TXT record `md` contains `C338`,
  `fn` = friendly name.

### Binary protocol — D 7050
- **TCP port 50001.** 5-byte packets: `[0x00, 0x01, 0x02, <msg>, <value>]`.
- Query: `[0x00, 0x01, 0x02, 0x02, <msg>]`. Device pushes `[0x00,0x01,n,<msg>,<val>]`.
- msg codes: SOURCE=3, VOLUME=4, POWER=9, MUTE=10, VERSION=11 (QUERY prefix=2).
- Values: power/mute 0/1; source 0-based index; volume 0–200 (step 0.5 dB, −90…+10).
- Discovery: mDNS `_JB2XiOSApp._tcp.local.`

## Files

- `nad_remote.py` — the amp control library + CLI (single file, no required deps).
- `main.py` — daemon that follows the Samsung TV's power state (local).
- `docker-compose.yml` — containerised daemon (mounts this folder, runs `main.py`).
- `tv-amp-sync.service` — systemd unit, if you would rather not use Docker.
- `README.md` — this file.
- `LICENSE` — MIT.
