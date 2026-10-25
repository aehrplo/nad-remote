// Package nad controls NAD network amplifiers.
//
// Reverse-engineered from the (unmaintained) NAD Remote Android app
// (com.nadelectronics.nad.remote, versionCode 28). Standard library only for
// control; mDNS discovery uses golang.org/x/net/dns/dnsmessage.
//
// Two on-wire protocols exist, selected by model:
//
//   - Text protocol — C338 / C368 / C388 / M32 and NAD "AVR"-style receivers.
//     TCP port 30001. ASCII commands terminated with CR ('\r').
//     Requests:  "Main.Power=On\r", "Main.Volume=-40.0\r", "Main.Source?\r"
//     Responses: "Main.Power=On\r\n"  (Key=Value, CR/LF terminated)
//
//   - Binary protocol — D 7050.
//     TCP port 50001. 5-byte packets: [0x00, 0x01, 0x02, <msg>, <value>].
//     A query is [0x00, 0x01, 0x02, 0x02, <msg>].
//
// Discovery (as the app does it):
//
//   - C338  -> mDNS _googlecast._tcp.local. with TXT md=C338 -> port 30001
//   - D7050 -> mDNS _JB2XiOSApp._tcp.local.                  -> port 50001
//
// Typical use:
//
//	amp := nad.NewAmp("192.168.1.20", "", 0, 3*time.Second) // model auto-detected
//	if err := amp.Connect(); err != nil { ... }
//	defer amp.Close()
//	amp.PowerOn()
//	amp.SetVolume(-45) // dB
//	amp.SetSource("Coax1")
package nad

import (
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// Model / protocol tables (verbatim from the decompiled app)
// ---------------------------------------------------------------------------

const (
	TextPort   = 30001
	BinaryPort = 50001

	// Volume limits for the text-protocol integrated amps (real dB, 0.5 steps).
	// The C338 hardware accepts roughly -80.0 .. +12.0 dB.
	TextVolMin = -80.0
	TextVolMax = 12.0
)

// C338 source token <-> friendly name. index == protocol order.
// (Remote.PROTOCOL_COMMANDS_AVR.SOURCE_C338_0..7)
var c338Sources = [][2]string{
	{"Stream", "Streaming"},
	{"Wireless", "BT/Wi-Fi"},
	{"TV", "TV"},
	{"Phono", "Phono"},
	{"Coax1", "Coaxial 1"},
	{"Coax2", "Coaxial 2"},
	{"Opt1", "Optical 1"},
	{"Opt2", "Optical 2"},
}

// C368 / C388 use a 1-based numeric source index on the wire
// (Main.Source=1..N). Default physical order from the app.
var c3x8Sources = []string{
	"Optical 1", "Optical 2", "Coaxial 1", "Coaxial 2",
	"Phono", "Line 1", "Line 2", "Bluetooth",
}

// D7050 source index -> friendly name (PROTOCOL_COMMANDS_7050 order)
var d7050Sources = []string{
	"Coaxial 1", "Coaxial 2", "Optical 1", "Optical 2",
	"Computer", "AirPlay", "Dock", "Bluetooth",
}

// D7050 binary message types (PROTOCOL_COMMANDS_7050)
const (
	msgQuery       byte = 2
	msgSource      byte = 3
	msgVolume      byte = 4
	msgCrossover   byte = 5
	msgIRChannel   byte = 6
	msgPowerSave   byte = 7
	msgAutoStandby byte = 8
	msgPower       byte = 9
	msgMute        byte = 10
	msgVersion     byte = 11
)

// ErrNAD is the base error; every error from this package wraps it.
var ErrNAD = errors.New("nad")

// ErrConnection is returned when the amp cannot be reached or the socket drops.
// It wraps ErrNAD.
var ErrConnection = fmt.Errorf("%w: connection", ErrNAD)

func nadErr(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrNAD, fmt.Sprintf(format, a...))
}

func connErr(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrConnection, fmt.Sprintf(format, a...))
}

// Status is a snapshot of the amp's state. Nil fields are unknown.
type Status struct {
	Model       *string           `json:"model"`
	Version     *string           `json:"version"`
	Power       *bool             `json:"power"`
	Mute        *bool             `json:"mute"`
	Source      *string           `json:"source"`       // friendly name
	SourceToken *string           `json:"source_token"` // on-wire token / index
	VolumeDB    *float64          `json:"volume_db"`    // real dB
	Raw         map[string]string `json:"-"`
}

// IsOn reports whether the amp is known to be powered on.
func (s *Status) IsOn() bool { return s.Power != nil && *s.Power }

func ptr[T any](v T) *T { return &v }
