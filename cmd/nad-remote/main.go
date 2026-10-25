// Command nad-remote controls a NAD network amplifier (C338/C368/C388/D7050).
//
//	nad-remote discover
//	nad-remote --host 192.168.1.20 status
//	nad-remote --host 192.168.1.20 power on
//	nad-remote --host 192.168.1.20 volume -45   # dB, or + / -
//	nad-remote --host 192.168.1.20 source Coax1 # token, name, or index
//	nad-remote --host 192.168.1.20 mute on
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/aehrplo/nad-remote/nad"
)

const usage = `usage: nad-remote [--host IP] [--model M] [--port N] [--timeout S] <command> [arg]

commands:
  discover          find amps on the LAN
  status            print current state as JSON
  sources           list available sources
  power on|off      power on/off
  volume DB|+|-     set volume in dB, or step up/down
  source NAME       select input source (name, token or index; see 'sources')
  mute on|off       mute/unmute

flags:
`

func main() { os.Exit(run()) }

func run() int {
	fs := flag.NewFlagSet("nad-remote", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage); fs.PrintDefaults() }
	host := fs.String("host", "", "amp IP address (skip to auto-discover)")
	model := fs.String("model", "", "force model (C338, C368, C388, D7050)")
	port := fs.Int("port", 0, "override TCP port")
	timeout := fs.Float64("timeout", 3.0, "timeout in seconds")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return 2
	}
	args := fs.Args()
	if len(args) == 0 {
		fs.Usage()
		return 2
	}
	cmd, args := args[0], args[1:]
	to := time.Duration(*timeout * float64(time.Second))

	needArg := func(choices ...string) (string, bool) {
		if len(args) != 1 {
			fs.Usage()
			return "", false
		}
		if len(choices) == 0 {
			return args[0], true
		}
		for _, c := range choices {
			if args[0] == c {
				return c, true
			}
		}
		fmt.Fprintf(os.Stderr, "%s: invalid choice %q (choose from %v)\n", cmd, args[0], choices)
		return "", false
	}

	switch cmd {
	case "discover":
		for _, d := range nad.Discover(to, true) {
			m := d.Model
			if m == "" {
				m = "?"
			}
			fmt.Printf("%-15s  %-6s  port %d\n", d.Host, m, d.Port)
		}
		return 0
	case "status", "sources", "power", "volume", "source", "mute":
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", cmd)
		fs.Usage()
		return 2
	}

	var amp *nad.Amp
	if *host != "" {
		amp = nad.NewAmp(*host, *model, *port, to)
	} else {
		fmt.Println("no --host given, discovering...")
		var err error
		if amp, err = nad.Autodiscover(to); err != nil {
			fmt.Printf("error: %v\n", err)
			return 1
		}
		fmt.Printf("using %s (%s)\n", amp.Host, amp.Model)
	}

	if err := amp.Connect(); err != nil {
		fmt.Printf("error: %v\n", err)
		return 1
	}
	defer amp.Close()

	var err error
	switch cmd {
	case "status":
		var st *nad.Status
		if st, err = amp.GetStatus(); err == nil {
			out, _ := json.MarshalIndent(st, "", "  ")
			fmt.Println(string(out))
		}
	case "sources":
		for i, s := range amp.ListSources() {
			fmt.Printf("%d: %s\n", i, s)
		}
	case "power":
		state, ok := needArg("on", "off")
		if !ok {
			return 2
		}
		if err = amp.SetPower(state == "on"); err == nil {
			fmt.Printf("power -> %s\n", state)
		}
	case "volume":
		v, ok := needArg()
		if !ok {
			return 2
		}
		switch v {
		case "+":
			err = amp.VolumeUp()
		case "-":
			err = amp.VolumeDown()
		default:
			db, perr := strconv.ParseFloat(v, 64)
			if perr != nil {
				fmt.Fprintf(os.Stderr, "volume: invalid dB value %q\n", v)
				return 2
			}
			if err = amp.SetVolume(db); err == nil {
				fmt.Printf("volume -> %s dB\n", v)
			}
		}
		if err == nil && (v == "+" || v == "-") {
			fmt.Printf("volume %s\n", v)
		}
	case "source":
		name, ok := needArg()
		if !ok {
			return 2
		}
		if err = amp.SetSource(name); err == nil {
			fmt.Printf("source -> %s\n", name)
		}
	case "mute":
		state, ok := needArg("on", "off")
		if !ok {
			return 2
		}
		if err = amp.SetMute(state == "on"); err == nil {
			fmt.Printf("mute -> %s\n", state)
		}
	}
	if err != nil {
		fmt.Printf("error: %v\n", err)
		return 1
	}
	return 0
}
