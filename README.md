# nad-remote

Go control for NAD network amplifiers, reverse-engineered from the
unmaintained **NAD Remote** Android app (`com.nadelectronics.nad.remote`,
versionCode 28). Built for headless / IoT use — no Android, no old phone, no GUI.
Ships as two static binaries in a ~10 MB `scratch` Docker image, ready for a
Synology NAS.

Verified live against a **NAD C338** (firmware 1.76). Also implements the
protocols for the **C368 / C388** and the **D 7050**, decoded from the same APK.

## What it controls

Power on/off · volume (absolute dB + relative steps) · input source · mute ·
status readback (model, firmware, power, source, volume, mute).

## Layout

| Path | What |
|------|------|
| `nad/` | Go package: text + binary protocol clients, model-aware `Amp` facade, mDNS / TCP-sweep discovery |
| `cmd/nad-remote/` | CLI for manual control |
| `cmd/tv-amp-sync/` | daemon that follows a Samsung TV's power state |
| `Dockerfile` | multi-stage build → `scratch` image with both binaries |
| `docker-compose.yml` | the daemon as a Synology Container Manager project |
| `tv-amp-sync.service` | systemd unit, if you'd rather run the binary directly |

The amp must have **network standby enabled** on the unit
(Settings → *Control Setup / Network Standby → On*) or it won't answer while off.

## Run on Synology (Docker)

Pick the image architecture for your NAS: most models (`+`, `xs`, `DS2xx+`…)
are **Intel/AMD → `linux/amd64`**; a few value/`j` models are **ARM → `linux/arm64`**.
(*Control Panel → Info Center* shows the CPU.)

### Option A — let the NAS build it

1. Copy this folder to the NAS, e.g. `/volume1/docker/nad-remote`
   (File Station, or `scp -r . nas:/volume1/docker/nad-remote`).
2. Edit `docker-compose.yml`: set `TV_IP`, `AMP_IP` and the rest (see table below).
3. *Container Manager → Project → Create* → path = that folder → it picks up
   `docker-compose.yml`, builds the image and starts it.

Or over SSH: `cd /volume1/docker/nad-remote && sudo docker compose up -d --build`.

### Option B — build on your Mac, import on the NAS

```bash
docker build --platform linux/amd64 -t nad-remote:latest .     # or linux/arm64
docker save nad-remote:latest | gzip > nad-remote.tar.gz
```

*Container Manager → Image → Action → Import → nad-remote.tar.gz*, then create
the project from `docker-compose.yml` as in A — it uses the imported
`nad-remote:latest` instead of building.

### Operating it

```bash
sudo docker logs -f tv-amp-sync                                  # watch events
sudo docker exec tv-amp-sync tv-amp-sync --status                # TV + amp state
sudo docker exec tv-amp-sync nad-remote --host 192.168.1.20 status
sudo docker exec tv-amp-sync nad-remote discover                 # find amps
```

The compose file uses `network_mode: host` so the container sits directly on
your LAN (needed for mDNS discovery; plain TCP control would also work bridged).
Discovery sends one-shot mDNS queries from an ephemeral port, so it does not
fight DSM's own Bonjour service for UDP 5353.

## CLI (`nad-remote`)

```bash
nad-remote discover                                  # find amps on the LAN

# everything below auto-discovers if you omit --host
nad-remote --host 192.168.1.20 status
nad-remote --host 192.168.1.20 sources
nad-remote --host 192.168.1.20 power on
nad-remote --host 192.168.1.20 volume -45            # dB
nad-remote --host 192.168.1.20 volume -              # step down
nad-remote --host 192.168.1.20 source Coax1          # token, name, or index
nad-remote --host 192.168.1.20 mute on
```

Flags (`--host`, `--model`, `--port`, `--timeout`) go before the command.
Build locally with `go build ./cmd/...` (Go 1.26+).

## Go API

```go
import "github.com/aehrplo/nad-remote/nad"

// explicit host (fastest, best for a fixed IoT install — give the amp a DHCP reservation)
amp := nad.NewAmp("192.168.1.20", "", 0, 3*time.Second) // model auto-detected on connect
if err := amp.Connect(); err != nil { return err }
defer amp.Close()

amp.PowerOn()
amp.SetVolume(-45)        // real dB, rounded to 0.5
amp.SetSource("Coax1")    // or "Coaxial 1", or "4"
amp.SetMute(false)
st, _ := amp.GetStatus()  // json.Marshal(st) gives the same shape as `status`

// or discover
amp, err := nad.Autodiscover(5 * time.Second)
```

`GetStatus()` returns a `*nad.Status`; each field is a pointer and `nil` when
the amp didn't report it: `Power`, `Mute`, `Source` (friendly name),
`SourceToken`, `VolumeDB`, `Model`, `Version`. Errors wrap `nad.ErrNAD`;
connection failures also wrap `nad.ErrConnection`.

### Sources

| Model        | On-wire form            | Names |
|--------------|-------------------------|-------|
| C338         | token (`Coax1`, `Opt1`) | Streaming, BT/Wi-Fi, TV, Phono, Coaxial 1/2, Optical 1/2 |
| C368 / C388  | 1-based index           | Optical 1/2, Coaxial 1/2, Phono, Line 1/2, Bluetooth (varies by config) |
| D7050        | 0-based index           | Coaxial 1/2, Optical 1/2, Computer, AirPlay, Dock, Bluetooth |

`SetSource()` (and the CLI `source` command) accepts the friendly name, the wire token, or the 0-based index.

## Reliability notes

- The amp accepts **one control connection at a time**. Open, act, read, close —
  don't hold the socket open across a whole daemon if the NAD app / other
  integrations may also connect. (`tv-amp-sync` connects per action.)
- After `PowerOn()` the C338 reboots its control interface and goes silent for
  ~1–2 s. `GetStatus()` already retries across that gap.
- Give the amp a static DHCP lease so `AMP_IP` never changes.

## Auto-follow a Samsung TV (`tv-amp-sync`)

Turns the amp on when the TV turns on — switches it to the TV input and sets a
fixed reference level so the **TV** controls actual loudness — and powers it off
when the TV goes to standby. **Fully local, no SmartThings cloud, no token.**

How it detects TV power: it reads the TV's own Tizen endpoint
`http://<tv-ip>:8001/api/v2/` → `device.PowerState` (`on` / `standby`). A
Samsung-TV-as-SmartThings-hub keeps its network alive in standby, so this is
always reachable and accurate. (Verified on a KQ55SF85AEXKR.)

After a cold power-on the C338 ignores commands for ~3 s and then pushes its
remembered volume, which can overwrite a volume set too early. The daemon waits
for the amp to answer again, sets the volume, reads it back, and retries if the
restore won (logged as `volume retry n/5`).

```bash
tv-amp-sync --status     # show TV + amp state
tv-amp-sync --once       # reconcile a single time
tv-amp-sync              # run the daemon (Ctrl-C / docker stop to end)
```

Configure via env vars (`--tv`, `--amp`, `--reference-db` override for one-off tests):

| Var | Default | Meaning |
|-----|---------|---------|
| `TV_IP` | `192.168.1.10` | Samsung TV address |
| `AMP_IP` | `192.168.1.20` | NAD amp address |
| `AMP_MODEL` | *(auto)* | force `C338` / `C368` / `C388` / `D7050` |
| `AMP_SOURCE` | `TV` | input to select on power-on (empty or `none` to leave it) |
| `AMP_REFERENCE_DB` | `-20` | fixed level set on power-on (see note below) |
| `POWER_OFF_WITH_TV` | `1` | also power the amp off on TV standby (`0`/`false` to disable) |
| `POLL_INTERVAL` | `3` | seconds between TV polls |
| `TV_HTTP_TIMEOUT` | `2` | seconds for the TV HTTP request |
| `AMP_READY_TIMEOUT` | `12` | max seconds to wait for the amp after power-on |
| `VOLUME_SET_ATTEMPTS` | `5` | volume set+verify attempts |
| `VOLUME_TOLERANCE_DB` | `0.6` | how close the read-back must be |
| `TZ` | `UTC` | log timestamp time zone, e.g. `Asia/Seoul` |

**About `AMP_REFERENCE_DB` and "TV controls the volume":** for the TV's volume to
actually change loudness, the TV must attenuate its **optical/digital** output.
The C338 has no HDMI, so HDMI-ARC/CEC volume passthrough is not available — only
optical/coax. Some Samsung TVs output **fixed full-scale** over optical and their
volume slider does nothing downstream. Test yours: if the TV volume has no effect,
your amp reference *is* your volume — set `AMP_REFERENCE_DB` to a comfortable
level and control volume via this tool instead. Starts at a safe **−20 dB** (0 dB
is near-max on a C338); raise toward 0 if the TV can't get loud enough.

### Alternative: systemd (no Docker)

```bash
GOOS=linux GOARCH=arm64 go build -o bin/ ./cmd/...     # match the host's arch
sudo mkdir -p /opt/nad-remote && sudo cp bin/* /opt/nad-remote/
sudo cp tv-amp-sync.service /etc/systemd/system/
sudoedit /etc/systemd/system/tv-amp-sync.service       # set User + the Environment= values
sudo systemctl daemon-reload && sudo systemctl enable --now tv-amp-sync
journalctl -u tv-amp-sync -f
```

### Alternative: Home Assistant

If you run Home Assistant, you already have a Samsung TV integration that exposes
the TV's power as an entity — skip the polling and drive the amp from an
automation. Drop the `nad-remote` binary (built for HA's arch) into `/config/nad/`
and register it as a shell command:

```yaml
# configuration.yaml
shell_command:
  amp_tv_on: "/config/nad/nad-remote --host 192.168.1.20 source TV && \
              /config/nad/nad-remote --host 192.168.1.20 power on && \
              /config/nad/nad-remote --host 192.168.1.20 volume -20"
  amp_off:   "/config/nad/nad-remote --host 192.168.1.20 power off"

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
script is **not** a durable option — use the Docker daemon above or an Edge driver.

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

## Development

```bash
go test ./...      # protocol, retry, D7050 framing and mDNS parsing against local fakes
go vet ./...
```

The Python implementation this was ported from is in git history
(commit `030eb89`).
