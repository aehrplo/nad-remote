"""
nad_remote — pure-Python control for NAD network amplifiers.

Reverse-engineered from the (unmaintained) NAD Remote Android app
(com.nadelectronics.nad.remote, versionCode 28). No external dependencies
required for control; optional `zeroconf` is used for mDNS auto-discovery.

Two on-wire protocols exist, selected by model:

  * Text protocol  — C338 / C368 / C388 / M32 and NAD "AVR"-style receivers.
      TCP port 30001. ASCII commands terminated with CR ('\\r').
      Requests:  "Main.Power=On\\r", "Main.Volume=-40.0\\r", "Main.Source?\\r"
      Responses: "Main.Power=On\\r\\n"  (Key=Value, CR/LF terminated)

  * Binary protocol — D 7050.
      TCP port 50001. 5-byte packets: [0x00, 0x01, 0x02, <msg>, <value>].
      A query is [0x00, 0x01, 0x02, 0x02, <msg>].

Discovery (as the app does it):
  * C338         -> mDNS _googlecast._tcp.local. with TXT md=C338  -> port 30001
  * D7050        -> mDNS _JB2XiOSApp._tcp.local.                    -> port 50001

Typical use:

    from nad_remote import NADAmp, discover

    amp = NADAmp("192.168.1.20")        # auto-detects model on connect
    with amp:
        amp.power_on()
        amp.set_volume(-45)              # dB
        amp.set_source("Coax1")
        print(amp.get_status())

or let it find the amp for you:

    amp = NADAmp.autodiscover()
"""

from __future__ import annotations

import socket
import time
import threading
from dataclasses import dataclass, field
from typing import Dict, List, Optional, Tuple

__all__ = [
    "NADAmp",
    "NADTextClient",
    "NADBinaryClient",
    "discover",
    "NADError",
    "NADConnectionError",
]

# ---------------------------------------------------------------------------
# Model / protocol tables (verbatim from the decompiled app)
# ---------------------------------------------------------------------------

TEXT_PORT = 30001
BINARY_PORT = 50001

# C338 source token <-> friendly name. index == protocol order.
# (Remote.PROTOCOL_COMMANDS_AVR.SOURCE_C338_0..7)
C338_SOURCES: List[Tuple[str, str]] = [
    ("Stream", "Streaming"),
    ("Wireless", "BT/Wi-Fi"),
    ("TV", "TV"),
    ("Phono", "Phono"),
    ("Coax1", "Coaxial 1"),
    ("Coax2", "Coaxial 2"),
    ("Opt1", "Optical 1"),
    ("Opt2", "Optical 2"),
]

# C368 / C388 use a 1-based numeric source index on the wire
# (Main.Source=1..N). Default physical order from the app.
C3X8_SOURCES: List[str] = [
    "Optical 1", "Optical 2", "Coaxial 1", "Coaxial 2",
    "Phono", "Line 1", "Line 2", "Bluetooth",
]

# D7050 source index -> friendly name (PROTOCOL_COMMANDS_7050 order)
D7050_SOURCES: List[str] = [
    "Coaxial 1", "Coaxial 2", "Optical 1", "Optical 2",
    "Computer", "AirPlay", "Dock", "Bluetooth",
]

# D7050 binary message types (PROTOCOL_COMMANDS_7050)
_D7050_MSG = {
    "QUERY": 2, "SOURCE": 3, "VOLUME": 4, "CROSSOVER": 5, "IR_CHANNEL": 6,
    "POWER_SAVE": 7, "AUTO_STANDBY": 8, "POWER": 9, "MUTE": 10, "VERSION": 11,
}
_D7050_MSG_INV = {v: k for k, v in _D7050_MSG.items()}

# Volume limits for the text-protocol integrated amps (real dB, 0.5 steps).
# The C338 hardware accepts roughly -80.0 .. +12.0 dB.
TEXT_VOL_MIN = -80.0
TEXT_VOL_MAX = 12.0


class NADError(Exception):
    """Base class for library errors."""


class NADConnectionError(NADError):
    """Raised when the amp cannot be reached or the socket drops."""


@dataclass
class NADStatus:
    model: Optional[str] = None
    version: Optional[str] = None
    power: Optional[bool] = None
    mute: Optional[bool] = None
    source: Optional[str] = None          # friendly name
    source_token: Optional[str] = None     # on-wire token / index
    volume_db: Optional[float] = None       # real dB (text models)
    raw: Dict[str, str] = field(default_factory=dict)

    def as_dict(self) -> Dict[str, object]:
        return {
            "model": self.model,
            "version": self.version,
            "power": self.power,
            "mute": self.mute,
            "source": self.source,
            "source_token": self.source_token,
            "volume_db": self.volume_db,
        }


# ---------------------------------------------------------------------------
# Text protocol client  (C338 / C368 / C388 / M32 / AVR)
# ---------------------------------------------------------------------------

class NADTextClient:
    """Low-level client for the CR-terminated ASCII protocol (port 30001)."""

    def __init__(self, host: str, port: int = TEXT_PORT, timeout: float = 3.0):
        self.host = host
        self.port = port
        self.timeout = timeout
        self._sock: Optional[socket.socket] = None
        self._buf = b""
        self._lock = threading.RLock()

    # -- connection ---------------------------------------------------------
    def connect(self) -> "NADTextClient":
        try:
            self._sock = socket.create_connection((self.host, self.port), self.timeout)
            self._sock.settimeout(self.timeout)
        except OSError as e:
            raise NADConnectionError(f"cannot connect to {self.host}:{self.port}: {e}") from e
        return self

    def close(self) -> None:
        if self._sock is not None:
            try:
                self._sock.close()
            finally:
                self._sock = None

    def __enter__(self):
        return self.connect()

    def __exit__(self, *exc):
        self.close()

    # -- raw i/o ------------------------------------------------------------
    def _send(self, line: str) -> None:
        if self._sock is None:
            raise NADConnectionError("not connected")
        try:
            self._sock.sendall(line.encode("ascii") + b"\r")
        except OSError as e:
            raise NADConnectionError(f"send failed: {e}") from e

    def _read_lines(self, settle: float = 0.4) -> List[Tuple[str, str]]:
        """Read whatever is available for `settle` seconds, return (key,value) pairs."""
        if self._sock is None:
            raise NADConnectionError("not connected")
        deadline = time.monotonic() + settle
        self._sock.settimeout(settle)
        while time.monotonic() < deadline:
            try:
                chunk = self._sock.recv(4096)
            except socket.timeout:
                break
            except OSError as e:
                raise NADConnectionError(f"recv failed: {e}") from e
            if not chunk:
                break
            self._buf += chunk
        pairs: List[Tuple[str, str]] = []
        while b"\r" in self._buf or b"\n" in self._buf:
            idx = min([i for i in (self._buf.find(b"\r"), self._buf.find(b"\n")) if i != -1])
            raw, self._buf = self._buf[:idx], self._buf[idx + 1:]
            text = raw.decode("latin1").strip()
            if "=" in text:
                k, v = text.split("=", 1)
                pairs.append((k.strip(), v.strip()))
        return pairs

    def query(self, key: str, settle: float = 0.5, retries: int = 2) -> Dict[str, str]:
        """Send `<key>?` and collect responses into a dict.

        Retries when the amp returns nothing — it briefly stops answering while
        its control interface reboots in the ~1-2s after a power-on command.
        """
        with self._lock:
            out: Dict[str, str] = {}
            for attempt in range(retries + 1):
                self._send(key + "?")
                for k, v in self._read_lines(settle):
                    out[k] = v
                if out:
                    break
                if attempt < retries:
                    time.sleep(0.6)
            return out

    def set(self, key: str, value: str, settle: float = 0.4) -> Dict[str, str]:
        """Send `<key>=<value>` (or bare `<key>` for +/-) and collect the echo."""
        with self._lock:
            self._send(key + value if value in ("+", "-") else f"{key}={value}")
            out: Dict[str, str] = {}
            for k, v in self._read_lines(settle):
                out[k] = v
            return out


# ---------------------------------------------------------------------------
# Binary protocol client  (D 7050)
# ---------------------------------------------------------------------------

class NADBinaryClient:
    """Low-level client for the D7050 binary protocol (port 50001)."""

    def __init__(self, host: str, port: int = BINARY_PORT, timeout: float = 3.0):
        self.host = host
        self.port = port
        self.timeout = timeout
        self._sock: Optional[socket.socket] = None
        self._lock = threading.RLock()

    def connect(self) -> "NADBinaryClient":
        try:
            self._sock = socket.create_connection((self.host, self.port), self.timeout)
            self._sock.settimeout(self.timeout)
        except OSError as e:
            raise NADConnectionError(f"cannot connect to {self.host}:{self.port}: {e}") from e
        return self

    def close(self) -> None:
        if self._sock is not None:
            try:
                self._sock.close()
            finally:
                self._sock = None

    def __enter__(self):
        return self.connect()

    def __exit__(self, *exc):
        self.close()

    def _write(self, msg: int, value: int) -> None:
        if self._sock is None:
            raise NADConnectionError("not connected")
        pkt = bytes([0x00, 0x01, 0x02, msg & 0xFF, value & 0xFF])
        try:
            self._sock.sendall(pkt)
        except OSError as e:
            raise NADConnectionError(f"send failed: {e}") from e

    def set(self, msg_name: str, value: int) -> None:
        self._write(_D7050_MSG[msg_name], value)

    def query(self, msg_name: str) -> None:
        # query = [00 01 02 02 <msg>]
        self._write(_D7050_MSG["QUERY"], _D7050_MSG[msg_name])

    def read_updates(self, settle: float = 0.6) -> List[Tuple[str, int]]:
        """Parse pushed [00 01 <n> <msg> <val>...] frames into (msg_name, value)."""
        if self._sock is None:
            raise NADConnectionError("not connected")
        self._sock.settimeout(settle)
        data = b""
        deadline = time.monotonic() + settle
        while time.monotonic() < deadline:
            try:
                chunk = self._sock.recv(4096)
            except socket.timeout:
                break
            if not chunk:
                break
            data += chunk
        out: List[Tuple[str, int]] = []
        i = 0
        while i + 5 <= len(data):
            if data[i] == 0x00 and data[i + 1] == 0x01:
                n = data[i + 2]
                msg = data[i + 3]
                val = data[i + 4]
                out.append((_D7050_MSG_INV.get(msg, str(msg)), val))
                i += 3 + max(n, 2)
            else:
                i += 1
        return out


# ---------------------------------------------------------------------------
# High-level facade
# ---------------------------------------------------------------------------

def _clamp(v: float, lo: float, hi: float) -> float:
    return max(lo, min(hi, v))


class NADAmp:
    """
    High-level, model-aware control facade.

    Construct with a host (and optional model/port). If the model is unknown it
    is detected on first connect via `Main.Model?` (text) or defaults sensibly.
    """

    def __init__(self, host: str, model: Optional[str] = None,
                 port: Optional[int] = None, timeout: float = 3.0):
        self.host = host
        self.model = model.upper() if model else None
        self.timeout = timeout
        self._is_binary = self.model == "D7050"
        if port is None:
            port = BINARY_PORT if self._is_binary else TEXT_PORT
        self.port = port
        self._client = None  # type: ignore

    # -- discovery ----------------------------------------------------------
    @classmethod
    def autodiscover(cls, timeout: float = 5.0) -> "NADAmp":
        found = discover(timeout=timeout)
        if not found:
            raise NADConnectionError("no NAD amplifier found on the local network")
        d = found[0]
        return cls(d["host"], model=d.get("model"), port=d.get("port"))

    # -- lifecycle ----------------------------------------------------------
    def connect(self) -> "NADAmp":
        if self._is_binary:
            self._client = NADBinaryClient(self.host, self.port, self.timeout).connect()
        else:
            self._client = NADTextClient(self.host, self.port, self.timeout).connect()
            if self.model is None:
                info = self._client.query("Main.Model")
                m = info.get("Main.Model", "")
                self.model = m.replace("NAD", "").strip().upper() or None
        return self

    def close(self) -> None:
        if self._client is not None:
            self._client.close()
            self._client = None

    def __enter__(self):
        return self.connect()

    def __exit__(self, *exc):
        self.close()

    def _need(self):
        if self._client is None:
            raise NADConnectionError("not connected — call connect() or use a 'with' block")
        return self._client

    # -- source maps --------------------------------------------------------
    def _source_tokens(self) -> List[str]:
        if self._is_binary:
            return D7050_SOURCES
        if self.model in ("C368", "C388"):
            return C3X8_SOURCES
        return [tok for tok, _ in C338_SOURCES]

    def _source_friendly(self) -> List[str]:
        if self._is_binary:
            return D7050_SOURCES
        if self.model in ("C368", "C388"):
            return C3X8_SOURCES
        return [name for _, name in C338_SOURCES]

    def list_sources(self) -> List[str]:
        """Human-friendly source names available on this model."""
        return list(self._source_friendly())

    def _resolve_source(self, source) -> Tuple[str, int]:
        """Return (wire_token_or_index_str, index) for a name/token/index."""
        toks = self._source_tokens()
        friendly = self._source_friendly()
        if isinstance(source, int):
            idx = source
        else:
            s = str(source).strip().lower()
            idx = None
            for i, (t, f) in enumerate(zip(toks, friendly)):
                if s in (t.lower(), f.lower()):
                    idx = i
                    break
            if idx is None:
                raise NADError(f"unknown source {source!r}; valid: {friendly}")
        if not 0 <= idx < len(toks):
            raise NADError(f"source index {idx} out of range 0..{len(toks)-1}")
        return toks[idx], idx

    # -- commands -----------------------------------------------------------
    def power_on(self) -> None:
        c = self._need()
        if self._is_binary:
            c.set("POWER", 1)
        else:
            c.set("Main.Power", "On")

    def power_off(self) -> None:
        c = self._need()
        if self._is_binary:
            c.set("POWER", 0)
        else:
            c.set("Main.Power", "Off")

    def set_power(self, on: bool) -> None:
        self.power_on() if on else self.power_off()

    def mute(self, on: bool = True) -> None:
        c = self._need()
        if self._is_binary:
            c.set("MUTE", 1 if on else 0)
        else:
            c.set("Main.Mute", "On" if on else "Off")

    def unmute(self) -> None:
        self.mute(False)

    def set_source(self, source) -> None:
        c = self._need()
        token, idx = self._resolve_source(source)
        if self._is_binary:
            c.set("SOURCE", idx)
        elif self.model in ("C368", "C388"):
            c.set("Main.Source", str(idx + 1))     # 1-based numeric
        else:
            c.set("Main.Source", token)             # C338 token, e.g. "Coax1"

    def set_volume(self, db: float) -> None:
        """Set absolute volume in real dB (text models). Rounded to 0.5 dB."""
        c = self._need()
        if self._is_binary:
            # D7050 volume is a raw 0..200 step (each step 0.5 dB, -90..+10 dB).
            raw = int(round((db + 90.0) * 2))
            c.set("VOLUME", _clamp(raw, 0, 200))
            return
        val = _clamp(db, TEXT_VOL_MIN, TEXT_VOL_MAX)
        val = round(val * 2) / 2.0
        c.set("Main.Volume", f"{val:.1f}")

    def volume_up(self) -> None:
        c = self._need()
        if self._is_binary:
            st = self.get_status()
            raw = int(round(((st.volume_db or -90) + 90) * 2))
            c.set("VOLUME", _clamp(raw + 2, 0, 200))
        else:
            c.set("Main.Volume", "+")

    def volume_down(self) -> None:
        c = self._need()
        if self._is_binary:
            st = self.get_status()
            raw = int(round(((st.volume_db or -90) + 90) * 2))
            c.set("VOLUME", _clamp(raw - 2, 0, 200))
        else:
            c.set("Main.Volume", "-")

    # -- status -------------------------------------------------------------
    def is_responsive(self) -> bool:
        """Fast single-shot liveness probe (no retry). Returns False while the
        amp's control interface is still booting after a power-on."""
        c = self._need()
        if self._is_binary:
            c.query("POWER")
            return len(c.read_updates(0.5)) > 0
        return bool(c.query("Main.Power", settle=0.4, retries=0))

    def get_volume_db(self) -> Optional[float]:
        """Read just the current volume in dB (cheaper than get_status)."""
        c = self._need()
        if self._is_binary:
            c.query("VOLUME")
            for name, val in c.read_updates(0.6):
                if name == "VOLUME":
                    return val / 2.0 - 90.0
            return None
        r = c.query("Main.Volume", settle=0.4, retries=1)
        if "Main.Volume" in r:
            try:
                return float(r["Main.Volume"])
            except ValueError:
                return None
        return None

    def get_status(self) -> NADStatus:
        c = self._need()
        st = NADStatus(model=self.model)
        if self._is_binary:
            for m in ("VERSION", "POWER", "SOURCE", "VOLUME", "MUTE"):
                c.query(m)
            for name, val in c.read_updates(0.8):
                if name == "POWER":
                    st.power = bool(val)
                elif name == "MUTE":
                    st.mute = bool(val)
                elif name == "SOURCE":
                    st.source_token = str(val)
                    if 0 <= val < len(D7050_SOURCES):
                        st.source = D7050_SOURCES[val]
                elif name == "VOLUME":
                    st.volume_db = val / 2.0 - 90.0
                elif name == "VERSION":
                    st.version = str(val)
            return st

        raw: Dict[str, str] = {}
        for key in ("Main.Model", "Main.Version", "Main.Power",
                    "Main.Source", "Main.Volume", "Main.Mute"):
            raw.update(c.query(key))
        st.raw = raw
        if "Main.Model" in raw:
            st.model = raw["Main.Model"].replace("NAD", "").strip()
            self.model = st.model.upper()
        st.version = raw.get("Main.Version")
        if "Main.Power" in raw:
            st.power = raw["Main.Power"].strip().lower() == "on"
        if "Main.Mute" in raw:
            st.mute = raw["Main.Mute"].strip().lower() == "on"
        if "Main.Source" in raw:
            st.source_token = raw["Main.Source"].strip()
            st.source = self._friendly_from_token(st.source_token)
        if "Main.Volume" in raw:
            try:
                st.volume_db = float(raw["Main.Volume"])
            except ValueError:
                pass
        return st

    def _friendly_from_token(self, token: str) -> str:
        toks = self._source_tokens()
        friendly = self._source_friendly()
        # C368/C388 report a 1-based number
        if self.model in ("C368", "C388") and token.isdigit():
            i = int(token) - 1
            return friendly[i] if 0 <= i < len(friendly) else token
        for t, f in zip(toks, friendly):
            if token.lower() == t.lower():
                return f
        return token


# ---------------------------------------------------------------------------
# Discovery
# ---------------------------------------------------------------------------

def _discover_zeroconf(timeout: float) -> List[Dict[str, object]]:
    try:
        from zeroconf import Zeroconf, ServiceBrowser
    except Exception:
        return []

    found: Dict[str, Dict[str, object]] = {}

    class _Listener:
        def _handle(self, zc, type_, name):
            info = zc.get_service_info(type_, name, timeout=int(timeout * 1000))
            if not info:
                return
            addrs = info.parsed_addresses() if hasattr(info, "parsed_addresses") else []
            if not addrs:
                return
            host = addrs[0]
            props = {k.decode() if isinstance(k, bytes) else k:
                     (v.decode() if isinstance(v, bytes) else v)
                     for k, v in (info.properties or {}).items()}
            if type_.startswith("_googlecast") and "C338" in (props.get("md") or ""):
                found[host] = {"host": host, "model": "C338", "port": TEXT_PORT,
                               "name": props.get("fn", name)}
            elif type_.startswith("_JB2XiOSApp"):
                found[host] = {"host": host, "model": "D7050", "port": BINARY_PORT,
                               "name": name}

        def add_service(self, zc, type_, name):
            self._handle(zc, type_, name)

        def update_service(self, zc, type_, name):
            self._handle(zc, type_, name)

        def remove_service(self, zc, type_, name):
            pass

    zc = Zeroconf()
    try:
        ServiceBrowser(zc, "_googlecast._tcp.local.", _Listener())
        ServiceBrowser(zc, "_JB2XiOSApp._tcp.local.", _Listener())
        time.sleep(timeout)
    finally:
        zc.close()
    return list(found.values())


def _local_subnet() -> Optional[str]:
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    try:
        s.connect(("8.8.8.8", 80))
        ip = s.getsockname()[0]
        return ip.rsplit(".", 1)[0] + "."
    except OSError:
        return None
    finally:
        s.close()


def _discover_scan(timeout: float) -> List[Dict[str, object]]:
    import concurrent.futures as cf
    prefix = _local_subnet()
    if not prefix:
        return []
    per_host = min(0.5, max(0.2, timeout / 255))

    def probe(ip: str, port: int):
        s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        s.settimeout(per_host)
        try:
            return (ip, port) if s.connect_ex((ip, port)) == 0 else None
        except OSError:
            return None
        finally:
            s.close()

    hits: List[Dict[str, object]] = []
    with cf.ThreadPoolExecutor(max_workers=256) as ex:
        futs = [ex.submit(probe, f"{prefix}{h}", p)
                for h in range(1, 255) for p in (TEXT_PORT, BINARY_PORT)]
        for f in cf.as_completed(futs):
            r = f.result()
            if not r:
                continue
            ip, port = r
            if port == TEXT_PORT:
                model = None
                try:
                    with NADTextClient(ip, port, timeout=1.5) as c:
                        model = c.query("Main.Model").get("Main.Model", "")
                        model = model.replace("NAD", "").strip() or None
                except NADError:
                    pass
                hits.append({"host": ip, "model": model, "port": port})
            else:
                hits.append({"host": ip, "model": "D7050", "port": port})
    return hits


def discover(timeout: float = 5.0, use_scan_fallback: bool = True) -> List[Dict[str, object]]:
    """
    Find NAD amps on the local network.

    Tries mDNS first (needs the optional `zeroconf` package). If that yields
    nothing (or zeroconf is unavailable), falls back to a TCP port sweep of the
    local /24 for ports 30001 and 50001. Returns a list of dicts:
        {"host": ip, "model": "C338"|"D7050"|None, "port": int}
    """
    results = _discover_zeroconf(timeout)
    if not results and use_scan_fallback:
        results = _discover_scan(timeout)
    return results


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------

def _main(argv: Optional[List[str]] = None) -> int:
    import argparse
    import json

    p = argparse.ArgumentParser(
        prog="nad_remote",
        description="Control a NAD network amplifier (C338/C368/C388/D7050).")
    p.add_argument("--host", help="amp IP address (skip to auto-discover)")
    p.add_argument("--model", help="force model (C338, C368, C388, D7050)")
    p.add_argument("--port", type=int, help="override TCP port")
    p.add_argument("--timeout", type=float, default=3.0)
    sub = p.add_subparsers(dest="cmd", required=True)

    sub.add_parser("discover", help="find amps on the LAN")
    sub.add_parser("status", help="print current state as JSON")
    sp = sub.add_parser("power", help="power on/off")
    sp.add_argument("state", choices=["on", "off"])
    sv = sub.add_parser("volume", help="set volume in dB, or +/-")
    sv.add_argument("value", help="dB value (e.g. -40) or + / -")
    ss = sub.add_parser("source", help="select input source")
    ss.add_argument("name", help="source name/token/index (see 'sources')")
    sub.add_parser("sources", help="list available sources")
    sm = sub.add_parser("mute", help="mute/unmute")
    sm.add_argument("state", choices=["on", "off"])

    args = p.parse_args(argv)

    if args.cmd == "discover":
        for d in discover(timeout=args.timeout):
            print(f"{d['host']:15s}  {d.get('model') or '?':6s}  port {d['port']}")
        return 0

    if args.host:
        amp = NADAmp(args.host, model=args.model, port=args.port, timeout=args.timeout)
    else:
        print("no --host given, discovering...")
        amp = NADAmp.autodiscover(timeout=args.timeout)
        print(f"using {amp.host} ({amp.model})")

    try:
        with amp:
            if args.cmd == "status":
                print(json.dumps(amp.get_status().as_dict(), indent=2))
            elif args.cmd == "sources":
                for i, s in enumerate(amp.list_sources()):
                    print(f"{i}: {s}")
            elif args.cmd == "power":
                amp.set_power(args.state == "on")
                print(f"power -> {args.state}")
            elif args.cmd == "volume":
                if args.value in ("+", "-"):
                    amp.volume_up() if args.value == "+" else amp.volume_down()
                    print(f"volume {args.value}")
                else:
                    amp.set_volume(float(args.value))
                    print(f"volume -> {args.value} dB")
            elif args.cmd == "source":
                amp.set_source(args.name)
                print(f"source -> {args.name}")
            elif args.cmd == "mute":
                amp.mute(args.state == "on")
                print(f"mute -> {args.state}")
    except NADError as e:
        print(f"error: {e}")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(_main())
