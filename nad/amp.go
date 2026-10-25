package nad

import (
	"math"
	"strconv"
	"strings"
	"time"
)

// Amp is a high-level, model-aware control facade.
//
// Construct with a host (and optional model/port). If the model is unknown it
// is detected on connect via "Main.Model?" (text protocol).
type Amp struct {
	Host    string
	Model   string // upper-case, e.g. "C338"; "" = unknown
	Port    int
	Timeout time.Duration

	binary bool
	text   *TextClient
	bin    *BinaryClient
}

// NewAmp builds an Amp. model may be "" (auto-detect) and port 0 (model default).
func NewAmp(host, model string, port int, timeout time.Duration) *Amp {
	model = strings.ToUpper(model)
	a := &Amp{Host: host, Model: model, Timeout: timeout, binary: model == "D7050"}
	if port == 0 {
		port = TextPort
		if a.binary {
			port = BinaryPort
		}
	}
	a.Port = port
	return a
}

// Autodiscover returns an Amp for the first amplifier found on the LAN.
func Autodiscover(timeout time.Duration) (*Amp, error) {
	found := Discover(timeout, true)
	if len(found) == 0 {
		return nil, connErr("no NAD amplifier found on the local network")
	}
	d := found[0]
	return NewAmp(d.Host, d.Model, d.Port, 3*time.Second), nil
}

// -- lifecycle --------------------------------------------------------------

func (a *Amp) Connect() error {
	if a.binary {
		a.bin = NewBinaryClient(a.Host, a.Port, a.Timeout)
		return a.bin.Connect()
	}
	a.text = NewTextClient(a.Host, a.Port, a.Timeout)
	if err := a.text.Connect(); err != nil {
		a.text = nil
		return err
	}
	if a.Model == "" {
		info, err := a.text.Query("Main.Model", 500*time.Millisecond, 2)
		if err != nil {
			a.Close()
			return err
		}
		a.Model = strings.ToUpper(cleanModel(info["Main.Model"]))
	}
	return nil
}

func (a *Amp) Close() error {
	var err error
	if a.text != nil {
		err = a.text.Close()
		a.text = nil
	}
	if a.bin != nil {
		err = a.bin.Close()
		a.bin = nil
	}
	return err
}

func (a *Amp) need() error {
	if a.text == nil && a.bin == nil {
		return connErr("not connected — call Connect() first")
	}
	return nil
}

func cleanModel(m string) string {
	return strings.TrimSpace(strings.ReplaceAll(m, "NAD", ""))
}

func (a *Amp) isC3x8() bool { return a.Model == "C368" || a.Model == "C388" }

// -- source maps ------------------------------------------------------------

func (a *Amp) sourceTokens() []string {
	switch {
	case a.binary:
		return d7050Sources
	case a.isC3x8():
		return c3x8Sources
	}
	toks := make([]string, len(c338Sources))
	for i, s := range c338Sources {
		toks[i] = s[0]
	}
	return toks
}

func (a *Amp) sourceFriendly() []string {
	switch {
	case a.binary:
		return d7050Sources
	case a.isC3x8():
		return c3x8Sources
	}
	names := make([]string, len(c338Sources))
	for i, s := range c338Sources {
		names[i] = s[1]
	}
	return names
}

// ListSources returns the human-friendly source names available on this model.
func (a *Amp) ListSources() []string {
	return append([]string(nil), a.sourceFriendly()...)
}

// ResolveSource maps a friendly name, wire token or numeric index to
// (wire token, index).
func (a *Amp) ResolveSource(source string) (string, int, error) {
	toks, friendly := a.sourceTokens(), a.sourceFriendly()
	s := strings.ToLower(strings.TrimSpace(source))
	idx := -1
	for i := range toks {
		if s == strings.ToLower(toks[i]) || s == strings.ToLower(friendly[i]) {
			idx = i
			break
		}
	}
	if idx < 0 {
		n, err := strconv.Atoi(s)
		if err != nil {
			return "", 0, nadErr("unknown source %q; valid: %s", source, strings.Join(friendly, ", "))
		}
		idx = n
	}
	if idx < 0 || idx >= len(toks) {
		return "", 0, nadErr("source index %d out of range 0..%d", idx, len(toks)-1)
	}
	return toks[idx], idx, nil
}

func (a *Amp) friendlyFromToken(token string) string {
	toks, friendly := a.sourceTokens(), a.sourceFriendly()
	// C368/C388 report a 1-based number
	if a.isC3x8() {
		if n, err := strconv.Atoi(token); err == nil {
			if i := n - 1; i >= 0 && i < len(friendly) {
				return friendly[i]
			}
			return token
		}
	}
	for i, t := range toks {
		if strings.EqualFold(token, t) {
			return friendly[i]
		}
	}
	return token
}

// -- commands ---------------------------------------------------------------

const setSettle = 400 * time.Millisecond

func (a *Amp) setText(key, value string) error {
	_, err := a.text.Set(key, value, setSettle)
	return err
}

func onOff(on bool) string {
	if on {
		return "On"
	}
	return "Off"
}

func b2u(on bool) byte {
	if on {
		return 1
	}
	return 0
}

func (a *Amp) SetPower(on bool) error {
	if err := a.need(); err != nil {
		return err
	}
	if a.binary {
		return a.bin.Set(msgPower, b2u(on))
	}
	return a.setText("Main.Power", onOff(on))
}

func (a *Amp) PowerOn() error  { return a.SetPower(true) }
func (a *Amp) PowerOff() error { return a.SetPower(false) }

func (a *Amp) SetMute(on bool) error {
	if err := a.need(); err != nil {
		return err
	}
	if a.binary {
		return a.bin.Set(msgMute, b2u(on))
	}
	return a.setText("Main.Mute", onOff(on))
}

func (a *Amp) SetSource(source string) error {
	if err := a.need(); err != nil {
		return err
	}
	token, idx, err := a.ResolveSource(source)
	if err != nil {
		return err
	}
	switch {
	case a.binary:
		return a.bin.Set(msgSource, byte(idx))
	case a.isC3x8():
		return a.setText("Main.Source", strconv.Itoa(idx+1)) // 1-based numeric
	default:
		return a.setText("Main.Source", token) // C338 token, e.g. "Coax1"
	}
}

// D7050 volume is a raw 0..200 step (each step 0.5 dB, -90..+10 dB).
func d7050Raw(db float64) int         { return int(math.RoundToEven((db + 90) * 2)) }
func d7050DB(raw byte) float64        { return float64(raw)/2 - 90 }
func clampRaw(raw int) byte           { return byte(min(max(raw, 0), 200)) }
func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }

// SetVolume sets absolute volume in real dB, rounded to 0.5 dB.
func (a *Amp) SetVolume(db float64) error {
	if err := a.need(); err != nil {
		return err
	}
	if a.binary {
		return a.bin.Set(msgVolume, clampRaw(d7050Raw(db)))
	}
	v := math.RoundToEven(clamp(db, TextVolMin, TextVolMax)*2) / 2
	if v == 0 {
		v = 0 // normalise -0.0
	}
	return a.setText("Main.Volume", strconv.FormatFloat(v, 'f', 1, 64))
}

func (a *Amp) stepVolume(dir string, delta int) error {
	if err := a.need(); err != nil {
		return err
	}
	if !a.binary {
		return a.setText("Main.Volume", dir)
	}
	st, err := a.GetStatus()
	if err != nil {
		return err
	}
	cur := -90.0
	if st.VolumeDB != nil {
		cur = *st.VolumeDB
	}
	return a.bin.Set(msgVolume, clampRaw(d7050Raw(cur)+delta))
}

func (a *Amp) VolumeUp() error   { return a.stepVolume("+", 2) }
func (a *Amp) VolumeDown() error { return a.stepVolume("-", -2) }

// -- status -----------------------------------------------------------------

// IsResponsive is a fast single-shot liveness probe (no retry). It returns
// false while the amp's control interface is still booting after a power-on.
func (a *Amp) IsResponsive() (bool, error) {
	if err := a.need(); err != nil {
		return false, err
	}
	if a.binary {
		if err := a.bin.Query(msgPower); err != nil {
			return false, err
		}
		u, err := a.bin.ReadUpdates(500 * time.Millisecond)
		return len(u) > 0, err
	}
	r, err := a.text.Query("Main.Power", 400*time.Millisecond, 0)
	return len(r) > 0, err
}

// GetVolumeDB reads just the current volume in dB (cheaper than GetStatus).
// It returns nil if the amp did not report one.
func (a *Amp) GetVolumeDB() (*float64, error) {
	if err := a.need(); err != nil {
		return nil, err
	}
	if a.binary {
		if err := a.bin.Query(msgVolume); err != nil {
			return nil, err
		}
		ups, err := a.bin.ReadUpdates(600 * time.Millisecond)
		if err != nil {
			return nil, err
		}
		for _, u := range ups {
			if u.Msg == msgVolume {
				return ptr(d7050DB(u.Value)), nil
			}
		}
		return nil, nil
	}
	r, err := a.text.Query("Main.Volume", 400*time.Millisecond, 1)
	if err != nil {
		return nil, err
	}
	if v, ok := r["Main.Volume"]; ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return &f, nil
		}
	}
	return nil, nil
}

func (a *Amp) GetStatus() (*Status, error) {
	if err := a.need(); err != nil {
		return nil, err
	}
	st := &Status{}
	if a.Model != "" {
		st.Model = ptr(a.Model)
	}
	if a.binary {
		for _, m := range []byte{msgVersion, msgPower, msgSource, msgVolume, msgMute} {
			if err := a.bin.Query(m); err != nil {
				return nil, err
			}
		}
		ups, err := a.bin.ReadUpdates(800 * time.Millisecond)
		if err != nil {
			return nil, err
		}
		for _, u := range ups {
			switch u.Msg {
			case msgPower:
				st.Power = ptr(u.Value != 0)
			case msgMute:
				st.Mute = ptr(u.Value != 0)
			case msgSource:
				st.SourceToken = ptr(strconv.Itoa(int(u.Value)))
				if int(u.Value) < len(d7050Sources) {
					st.Source = ptr(d7050Sources[u.Value])
				}
			case msgVolume:
				st.VolumeDB = ptr(d7050DB(u.Value))
			case msgVersion:
				st.Version = ptr(strconv.Itoa(int(u.Value)))
			}
		}
		return st, nil
	}

	raw := map[string]string{}
	for _, key := range []string{"Main.Model", "Main.Version", "Main.Power",
		"Main.Source", "Main.Volume", "Main.Mute"} {
		r, err := a.text.Query(key, 500*time.Millisecond, 2)
		if err != nil {
			return nil, err
		}
		for k, v := range r {
			raw[k] = v
		}
	}
	st.Raw = raw
	if m, ok := raw["Main.Model"]; ok {
		st.Model = ptr(cleanModel(m))
		a.Model = strings.ToUpper(*st.Model)
	}
	if v, ok := raw["Main.Version"]; ok {
		st.Version = ptr(v)
	}
	if v, ok := raw["Main.Power"]; ok {
		st.Power = ptr(strings.EqualFold(strings.TrimSpace(v), "on"))
	}
	if v, ok := raw["Main.Mute"]; ok {
		st.Mute = ptr(strings.EqualFold(strings.TrimSpace(v), "on"))
	}
	if v, ok := raw["Main.Source"]; ok {
		tok := strings.TrimSpace(v)
		st.SourceToken = ptr(tok)
		st.Source = ptr(a.friendlyFromToken(tok))
	}
	if v, ok := raw["Main.Volume"]; ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			st.VolumeDB = &f
		}
	}
	return st, nil
}
