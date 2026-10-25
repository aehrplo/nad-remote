// Command tv-amp-sync turns a NAD amp on/off in step with a Samsung TV, locally.
//
// When the Samsung TV powers on, this powers on the NAD amp, switches it to the
// TV input, and sets it to a fixed reference volume (so the *TV* controls actual
// loudness via its digital audio out). When the TV goes to standby, the amp is
// (optionally) powered off again.
//
// No SmartThings cloud, no OAuth, no expiring token: TV power is read from the
// TV's own local Tizen endpoint  http://<tv>:8001/api/v2/  -> device.PowerState
// ('on' | 'standby'). Because a Samsung-TV-as-SmartThings-hub keeps its network
// alive in standby, that endpoint is always reachable and reports state reliably.
//
// Config is via env vars; CLI flags override for one-off testing:
//
//	tv-amp-sync            # run the daemon
//	tv-amp-sync --once     # reconcile a single time, then exit
//	tv-amp-sync --status   # print TV + amp state and exit
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // the container image is FROM scratch; honour $TZ in logs

	"github.com/aehrplo/nad-remote/nad"
)

// ---------------------------------------------------------------------------
// Configuration  (env var overrides the default; CLI flag overrides both)
// ---------------------------------------------------------------------------

var (
	tvIP     = env("TV_IP", "192.168.1.10")
	ampIP    = env("AMP_IP", "192.168.1.20")
	ampModel = env("AMP_MODEL", "") // "" = auto-detect

	// Input to select on the amp when the TV turns on ("" or "none" = don't
	// change source). C338 tokens: Stream, Wireless, TV, Phono, Coax1, Coax2, Opt1, Opt2
	ampSource = env("AMP_SOURCE", "TV")

	// Fixed reference level set on the amp each time the TV turns on.
	//
	//   This is the amp's fixed operating point; the TV's digital audio-out
	//   level then rides on top of it.
	//
	//   NOTE ON LOUDNESS: on the C338, 0.0 dB is essentially maximum. This
	//   defaults to a SAFE -20 dB. Tune it to your room:
	//     * If the TV can't get loud enough at its max, raise this toward 0.
	//     * If your TV's OPTICAL out is fixed/full-scale (many Samsung sets do
	//       NOT attenuate optical), then this value effectively *is* your
	//       volume — set it to a comfortable listening level and control
	//       volume here, not on the TV. (HDMI-ARC/CEC volume passthrough isn't
	//       available: the C338 has no HDMI.)
	ampReferenceDB = envFloat("AMP_REFERENCE_DB", -20)

	// Power the amp off when the TV goes to standby?
	powerOffWithTV = envBool("POWER_OFF_WITH_TV", true)

	pollInterval  = envSeconds("POLL_INTERVAL", 3)
	tvHTTPTimeout = envSeconds("TV_HTTP_TIMEOUT", 2)

	// After a cold power-on the C338 ignores commands for ~3 s, then PUSHES its
	// remembered volume/source. Setting volume inside that window races the
	// restore and is clobbered intermittently. So we wait until the amp
	// answers again, then set volume and VERIFY it stuck (retrying if the
	// restore overwrote it).
	ampReadyTimeout   = envSeconds("AMP_READY_TIMEOUT", 12) // max wait for responsiveness
	volumeSetAttempts = int(envFloat("VOLUME_SET_ATTEMPTS", 5))
	volumeToleranceDB = envFloat("VOLUME_TOLERANCE_DB", 0.6)
)

const ampTimeout = 3 * time.Second

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func envFloat(key string, def float64) float64 {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid %s=%q: %v\n", key, v, err)
		os.Exit(2)
	}
	return f
}

func envBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "false", "no", "off":
		return false
	}
	return true
}

func envSeconds(key string, def float64) time.Duration {
	return time.Duration(envFloat(key, def) * float64(time.Second))
}

func sourceEnabled() bool {
	s := strings.TrimSpace(ampSource)
	return s != "" && !strings.EqualFold(s, "none")
}

func logf(format string, a ...any) {
	// Timestamped, one line per event — plays nicely with `docker logs`.
	fmt.Printf("[%s] %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, a...))
}

func fmtDB(v *float64) string {
	if v == nil {
		return "None"
	}
	return strconv.FormatFloat(*v, 'f', -1, 64)
}

// ---------------------------------------------------------------------------
// TV power state (local, no cloud)
// ---------------------------------------------------------------------------

// tvPowerState returns "on", "standby", or "" (unreachable / unknown).
//
// "" is deliberately distinct from "standby" so a transient network blip never
// looks like a real power-off and spuriously kills the amp.
func tvPowerState() string {
	client := http.Client{Timeout: tvHTTPTimeout}
	resp, err := client.Get("http://" + tvIP + ":8001/api/v2/")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var data struct {
		Device struct {
			PowerState string `json:"PowerState"`
		} `json:"device"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return ""
	}
	return strings.ToLower(data.Device.PowerState)
}

// ---------------------------------------------------------------------------
// Amp actions
// ---------------------------------------------------------------------------

// withAmp opens one control connection for the duration of fn. The amp accepts
// only one connection at a time, so never hold it across polls.
func withAmp(fn func(*nad.Amp) error) error {
	amp := nad.NewAmp(ampIP, ampModel, 0, ampTimeout)
	if err := amp.Connect(); err != nil {
		return err
	}
	defer amp.Close()
	return fn(amp)
}

// waitAmpReady blocks until the amp answers commands again after a cold
// power-on, so our volume set lands *after* the amp's own boot-time volume
// restore (not during it, where it gets clobbered).
func waitAmpReady(amp *nad.Amp) error {
	deadline := time.Now().Add(ampReadyTimeout)
	for time.Now().Before(deadline) {
		ok, err := amp.IsResponsive()
		if err != nil {
			return err
		}
		if ok {
			time.Sleep(time.Second) // let the boot-time state push fully land
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	logf("WARNING: amp not responsive within %v after power-on", ampReadyTimeout)
	return nil
}

// setVolumeVerified sets the reference volume and confirms it stuck, retrying
// if the amp's power-on restore overwrote it.
func setVolumeVerified(amp *nad.Amp) (bool, *float64, error) {
	var last *float64
	for i := range volumeSetAttempts {
		if err := amp.SetVolume(ampReferenceDB); err != nil {
			return false, last, err
		}
		time.Sleep(700 * time.Millisecond)
		v, err := amp.GetVolumeDB()
		if err != nil {
			return false, last, err
		}
		last = v
		if last != nil && math.Abs(*last-ampReferenceDB) <= volumeToleranceDB {
			return true, last, nil
		}
		logf("  volume retry %d/%d: read %s dB, want %s",
			i+1, volumeSetAttempts, fmtDB(last), fmtDB(&ampReferenceDB))
		time.Sleep(500 * time.Millisecond)
	}
	return false, last, nil
}

// ampOnTVPoweredOn powers the amp, selects the TV input, pins the reference volume.
func ampOnTVPoweredOn() {
	err := withAmp(func(amp *nad.Amp) error {
		st, err := amp.GetStatus()
		if err != nil {
			return err
		}
		if !st.IsOn() {
			if err := amp.PowerOn(); err != nil {
				return err
			}
			if err := waitAmpReady(amp); err != nil { // wait out the boot/volume-restore race
				return err
			}
		}
		if sourceEnabled() {
			if err := amp.SetSource(ampSource); err != nil {
				return err
			}
			time.Sleep(300 * time.Millisecond)
		}
		ok, vol, err := setVolumeVerified(amp)
		if err != nil {
			return err
		}
		src := "None"
		if st, err := amp.GetStatus(); err == nil && st.Source != nil {
			src = *st.Source
		}
		note := ""
		if !ok {
			note = "  [UNCONFIRMED]"
		}
		logf("amp -> on | source=%s | volume=%s dB%s", src, fmtDB(vol), note)
		return nil
	})
	if err != nil {
		logf("amp on-action failed: %v", err)
	}
}

func ampOnTVStandby() {
	if !powerOffWithTV {
		return
	}
	err := withAmp(func(amp *nad.Amp) error {
		st, err := amp.GetStatus()
		if err != nil {
			return err
		}
		if st.IsOn() {
			if err := amp.PowerOff(); err != nil {
				return err
			}
			logf("amp -> off (TV standby)")
		}
		return nil
	})
	if err != nil {
		logf("amp off-action failed: %v", err)
	}
}

// reconcile acts only on a real edge into a known state.
func reconcile(prev, cur string) {
	switch {
	case cur == "on" && prev != "on":
		logf("TV -> on")
		ampOnTVPoweredOn()
	case cur == "standby" && prev != "standby" && prev != "":
		logf("TV -> standby")
		ampOnTVStandby()
	}
}

// ---------------------------------------------------------------------------
// Entry points
// ---------------------------------------------------------------------------

func runDaemon(ctx context.Context) int {
	src := ampSource
	if !sourceEnabled() {
		src = "None"
	}
	logf("tv-amp-sync started | TV=%s AMP=%s ref=%sdB source=%s power_off_with_tv=%t poll=%v",
		tvIP, ampIP, fmtDB(&ampReferenceDB), src, powerOffWithTV, pollInterval)
	// Seed with the current state so we don't fire an action on startup.
	prev := tvPowerState()
	if prev != "" {
		logf("initial TV state: %s", prev)
	}
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			logf("stopped")
			return 0
		case <-tick.C:
		}
		if cur := tvPowerState(); cur != "" {
			reconcile(prev, cur)
			prev = cur
		}
	}
}

func runOnce() int {
	cur := tvPowerState()
	logf("TV state: %s", orNone(cur))
	switch cur {
	case "on":
		ampOnTVPoweredOn()
	case "standby":
		ampOnTVStandby()
	default:
		logf("TV unreachable — no action")
	}
	return 0
}

func runStatus() int {
	fmt.Printf("TV  %s: PowerState = %s\n", tvIP, orNone(tvPowerState()))
	err := withAmp(func(amp *nad.Amp) error {
		st, err := amp.GetStatus()
		if err != nil {
			return err
		}
		out, _ := json.Marshal(st)
		fmt.Printf("AMP %s: %s\n", ampIP, out)
		return nil
	})
	if err != nil {
		fmt.Printf("AMP %s: error %v\n", ampIP, err)
	}
	return 0
}

func orNone(s string) string {
	if s == "" {
		return "None"
	}
	return s
}

func main() {
	once := flag.Bool("once", false, "reconcile once and exit")
	status := flag.Bool("status", false, "print TV + amp state and exit")
	tv := flag.String("tv", "", "override TV IP")
	amp := flag.String("amp", "", "override amp IP")
	ref := flag.String("reference-db", "", "override fixed reference dB")
	flag.Parse()

	if *tv != "" {
		tvIP = *tv
	}
	if *amp != "" {
		ampIP = *amp
	}
	if *ref != "" {
		v, err := strconv.ParseFloat(*ref, 64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid --reference-db %q\n", *ref)
			os.Exit(2)
		}
		ampReferenceDB = v
	}

	switch {
	case *status:
		os.Exit(runStatus())
	case *once:
		os.Exit(runOnce())
	}
	// Docker sends SIGTERM on `docker stop`; exit cleanly on it and on Ctrl-C.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(runDaemon(ctx))
}
