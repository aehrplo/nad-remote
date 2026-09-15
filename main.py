#!/usr/bin/env python3
"""
tv-amp-sync (main.py) — turn a NAD amp on/off in step with a Samsung TV, locally.

When the Samsung TV powers on, this powers on the NAD amp, switches it to the
TV input, and sets it to a fixed reference volume (so the *TV* controls actual
loudness via its digital audio out). When the TV goes to standby, the amp is
(optionally) powered off again.

No SmartThings cloud, no OAuth, no expiring token: TV power is read from the
TV's own local Tizen endpoint  http://<tv>:8001/api/v2/  -> device.PowerState
('on' | 'standby'). Because a Samsung-TV-as-SmartThings-hub keeps its network
alive in standby, that endpoint is always reachable and reports state reliably.

Amp control uses the sibling module `nad_remote.py`.

Run it as a long-lived service (see tv-amp-sync.service). Config is via env vars
or the constants below; CLI flags override for one-off testing:

    python main.py               # run the daemon
    python main.py --once        # reconcile a single time, then exit
    python main.py --status      # print TV + amp state and exit
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import time
import urllib.request
from typing import Optional

from nad_remote import NADAmp, NADError

# ---------------------------------------------------------------------------
# Configuration  (env var overrides the default; CLI flag overrides both)
# ---------------------------------------------------------------------------

TV_IP = os.environ.get("TV_IP", "192.168.1.10")
AMP_IP = os.environ.get("AMP_IP", "192.168.1.20")
AMP_MODEL = os.environ.get("AMP_MODEL") or None        # None = auto-detect

# Input to select on the amp when the TV turns on (None = don't change source).
# C338 tokens: Stream, Wireless, TV, Phono, Coax1, Coax2, Opt1, Opt2
AMP_SOURCE = os.environ.get("AMP_SOURCE", "TV") or None

# Fixed reference level set on the amp each time the TV turns on.
#
#   You chose "amp fixed, TV controls volume". This is the amp's fixed operating
#   point; the TV's digital audio-out level then rides on top of it.
#
#   NOTE ON LOUDNESS: on the C338, 0.0 dB is essentially maximum. This defaults
#   to a SAFE -20 dB. Tune it to your room:
#     * If the TV can't get loud enough at its max, raise this toward 0.
#     * If your TV's OPTICAL out is fixed/full-scale (many Samsung sets do NOT
#       attenuate optical), then this value effectively *is* your volume — set
#       it to a comfortable listening level and control volume here, not on the
#       TV. (HDMI-ARC/CEC volume passthrough isn't available: the C338 has no HDMI.)
AMP_REFERENCE_DB = float(os.environ.get("AMP_REFERENCE_DB", "-20"))

# Power the amp off when the TV goes to standby?
POWER_OFF_WITH_TV = os.environ.get("POWER_OFF_WITH_TV", "1") not in ("0", "false", "False", "")

POLL_INTERVAL = float(os.environ.get("POLL_INTERVAL", "3"))   # seconds
TV_HTTP_TIMEOUT = float(os.environ.get("TV_HTTP_TIMEOUT", "2"))

# After a cold power-on the C338 ignores commands for ~3 s, then PUSHES its
# remembered volume/source. Setting volume inside that window races the restore
# and is clobbered intermittently. So we wait until the amp answers again, then
# set volume and VERIFY it stuck (retrying if the restore overwrote it).
AMP_READY_TIMEOUT = float(os.environ.get("AMP_READY_TIMEOUT", "12"))   # max wait for responsiveness
VOLUME_SET_ATTEMPTS = int(os.environ.get("VOLUME_SET_ATTEMPTS", "5"))
VOLUME_TOLERANCE_DB = float(os.environ.get("VOLUME_TOLERANCE_DB", "0.6"))


def log(msg: str) -> None:
    # Unbuffered, timestamped — plays nicely with `journalctl`.
    print(f"[{time.strftime('%Y-%m-%d %H:%M:%S')}] {msg}", flush=True)


# ---------------------------------------------------------------------------
# TV power state (local, no cloud)
# ---------------------------------------------------------------------------

def tv_power_state(tv_ip: str = TV_IP, timeout: float = TV_HTTP_TIMEOUT) -> Optional[str]:
    """
    Return 'on', 'standby', or None (unreachable / unknown).

    None is deliberately distinct from 'standby' so a transient network blip
    never looks like a real power-off and spuriously kills the amp.
    """
    url = f"http://{tv_ip}:8001/api/v2/"
    try:
        with urllib.request.urlopen(url, timeout=timeout) as r:
            data = json.load(r)
    except Exception:
        return None
    ps = (data.get("device", {}) or {}).get("PowerState")
    if not ps:
        return None
    return str(ps).lower()


# ---------------------------------------------------------------------------
# Amp actions
# ---------------------------------------------------------------------------

def _wait_amp_ready(amp) -> None:
    """Block until the amp answers commands again after a cold power-on, so our
    volume set lands *after* the amp's own boot-time volume restore (not during
    it, where it gets clobbered)."""
    deadline = time.monotonic() + AMP_READY_TIMEOUT
    while time.monotonic() < deadline:
        if amp.is_responsive():
            time.sleep(1.0)      # let the boot-time state push fully land
            return
        time.sleep(0.5)
    log(f"WARNING: amp not responsive within {AMP_READY_TIMEOUT}s after power-on")


def _set_volume_verified(amp):
    """Set the reference volume and confirm it stuck, retrying if the amp's
    power-on restore overwrote it. Returns (ok, last_read_db)."""
    last = None
    for i in range(VOLUME_SET_ATTEMPTS):
        amp.set_volume(AMP_REFERENCE_DB)
        time.sleep(0.7)
        last = amp.get_volume_db()
        if last is not None and abs(last - AMP_REFERENCE_DB) <= VOLUME_TOLERANCE_DB:
            return True, last
        log(f"  volume retry {i + 1}/{VOLUME_SET_ATTEMPTS}: read {last} dB, want {AMP_REFERENCE_DB}")
        time.sleep(0.5)
    return False, last


def amp_on_tv_powered_on() -> None:
    """Power the amp, select the TV input, pin the fixed reference volume."""
    try:
        with NADAmp(AMP_IP, model=AMP_MODEL) as amp:
            st = amp.get_status()
            if not st.power:
                amp.power_on()
                _wait_amp_ready(amp)          # wait out the boot/volume-restore race
            if AMP_SOURCE:
                amp.set_source(AMP_SOURCE)
                time.sleep(0.3)
            ok, vol = _set_volume_verified(amp)
            src = amp.get_status().source
            flag = "" if ok else "  [UNCONFIRMED]"
            log(f"amp -> on | source={src} | volume={vol} dB{flag}")
    except NADError as e:
        log(f"amp on-action failed: {e}")


def amp_on_tv_standby() -> None:
    if not POWER_OFF_WITH_TV:
        return
    try:
        with NADAmp(AMP_IP, model=AMP_MODEL) as amp:
            if amp.get_status().power:
                amp.power_off()
                log("amp -> off (TV standby)")
    except NADError as e:
        log(f"amp off-action failed: {e}")


def reconcile(prev: Optional[str], cur: Optional[str]) -> None:
    """Act only on a real edge into a known state."""
    if cur == "on" and prev != "on":
        log("TV -> on")
        amp_on_tv_powered_on()
    elif cur == "standby" and prev not in ("standby", None):
        log("TV -> standby")
        amp_on_tv_standby()


# ---------------------------------------------------------------------------
# Entry points
# ---------------------------------------------------------------------------

def run_daemon() -> int:
    log(f"tv-amp-sync started | TV={TV_IP} AMP={AMP_IP} "
        f"ref={AMP_REFERENCE_DB}dB source={AMP_SOURCE} "
        f"power_off_with_tv={POWER_OFF_WITH_TV} poll={POLL_INTERVAL}s")
    prev: Optional[str] = None
    # Seed with the current state so we don't fire an action on startup.
    seed = tv_power_state()
    if seed is not None:
        prev = seed
        log(f"initial TV state: {seed}")
    while True:
        cur = tv_power_state()
        if cur is not None:
            reconcile(prev, cur)
            prev = cur
        time.sleep(POLL_INTERVAL)


def run_once() -> int:
    cur = tv_power_state()
    log(f"TV state: {cur}")
    if cur == "on":
        amp_on_tv_powered_on()
    elif cur == "standby":
        amp_on_tv_standby()
    else:
        log("TV unreachable — no action")
    return 0


def run_status() -> int:
    tv = tv_power_state()
    print(f"TV  {TV_IP}: PowerState = {tv}")
    try:
        with NADAmp(AMP_IP, model=AMP_MODEL) as amp:
            print(f"AMP {AMP_IP}: {json.dumps(amp.get_status().as_dict())}")
    except NADError as e:
        print(f"AMP {AMP_IP}: error {e}")
    return 0


def main(argv=None) -> int:
    p = argparse.ArgumentParser(description="Sync a NAD amp to a Samsung TV's power state (local).")
    p.add_argument("--once", action="store_true", help="reconcile once and exit")
    p.add_argument("--status", action="store_true", help="print TV + amp state and exit")
    p.add_argument("--tv", help="override TV IP")
    p.add_argument("--amp", help="override amp IP")
    p.add_argument("--reference-db", type=float, help="override fixed reference dB")
    args = p.parse_args(argv)

    global TV_IP, AMP_IP, AMP_REFERENCE_DB
    if args.tv:
        TV_IP = args.tv
    if args.amp:
        AMP_IP = args.amp
    if args.reference_db is not None:
        AMP_REFERENCE_DB = args.reference_db

    if args.status:
        return run_status()
    if args.once:
        return run_once()
    try:
        return run_daemon()
    except KeyboardInterrupt:
        log("stopped")
        return 0


if __name__ == "__main__":
    sys.exit(main())
