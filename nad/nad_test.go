package nad

import (
	"bufio"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// fakeC338 is a minimal text-protocol amp: answers "<key>?" from state, applies
// "<key>=<value>", and records every line it receives.
type fakeC338 struct {
	ln    net.Listener
	mu    sync.Mutex
	state map[string]string
	lines []string
	// silentQueries makes the first N queries go unanswered (post-power-on gap).
	silentQueries int
}

func newFakeC338(t *testing.T) *fakeC338 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeC338{ln: ln, state: map[string]string{
		"Main.Model": "NAD C338", "Main.Version": "1.76", "Main.Power": "On",
		"Main.Source": "Stream", "Main.Volume": "-20.0", "Main.Mute": "Off",
	}}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeC338) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

func (f *fakeC338) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	for {
		line, err := r.ReadString('\r')
		if err != nil {
			return
		}
		line = strings.TrimSuffix(line, "\r")
		f.mu.Lock()
		f.lines = append(f.lines, line)
		var reply string
		switch {
		case strings.HasSuffix(line, "?"):
			if f.silentQueries > 0 {
				f.silentQueries--
				break
			}
			k := strings.TrimSuffix(line, "?")
			reply = k + "=" + f.state[k]
		case strings.Contains(line, "="):
			k, v, _ := strings.Cut(line, "=")
			f.state[k] = v
			reply = line
		}
		f.mu.Unlock()
		if reply != "" {
			c.Write([]byte(reply + "\r\n"))
		}
	}
}

func (f *fakeC338) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.lines...)
}

func connectFake(t *testing.T, f *fakeC338) *Amp {
	t.Helper()
	a := NewAmp("127.0.0.1", "", f.port(), time.Second)
	if err := a.Connect(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

func TestTextStatusAndModelDetect(t *testing.T) {
	f := newFakeC338(t)
	a := connectFake(t, f)
	if a.Model != "C338" {
		t.Fatalf("model = %q, want C338", a.Model)
	}
	st, err := a.GetStatus()
	if err != nil {
		t.Fatal(err)
	}
	if !st.IsOn() || *st.Mute || *st.Source != "Streaming" || *st.SourceToken != "Stream" ||
		*st.VolumeDB != -20 || *st.Version != "1.76" || *st.Model != "C338" {
		t.Fatalf("unexpected status: %+v", st)
	}
}

func TestTextCommandsOnWire(t *testing.T) {
	f := newFakeC338(t)
	a := connectFake(t, f)
	steps := []func() error{
		a.PowerOff,
		func() error { return a.SetVolume(-45.26) }, // rounds to 0.5
		func() error { return a.SetVolume(99) },     // clamps to +12
		func() error { return a.SetVolume(-0.1) },   // no "-0.0"
		a.VolumeUp,
		func() error { return a.SetSource("coaxial 1") },
		func() error { return a.SetSource("6") },
		func() error { return a.SetMute(true) },
	}
	for i, s := range steps {
		if err := s(); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	want := []string{
		"Main.Power=Off", "Main.Volume=-45.5", "Main.Volume=12.0", "Main.Volume=0.0",
		"Main.Volume+", "Main.Source=Coax1", "Main.Source=Opt1", "Main.Mute=On",
	}
	got := f.sent()[1:] // [0] is the Main.Model? probe from Connect
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("wire lines\n got %q\nwant %q", got, want)
	}
}

func TestQueryRetriesThroughSilence(t *testing.T) {
	f := newFakeC338(t)
	a := connectFake(t, f)
	f.mu.Lock()
	f.silentQueries = 1
	f.mu.Unlock()
	v, err := a.GetVolumeDB()
	if err != nil || v == nil || *v != -20 {
		t.Fatalf("GetVolumeDB = %v, %v", v, err)
	}
	f.mu.Lock()
	f.silentQueries = 1
	f.mu.Unlock()
	if ok, _ := a.IsResponsive(); ok {
		t.Fatal("IsResponsive should not retry")
	}
}

func TestResolveSourceErrors(t *testing.T) {
	a := NewAmp("x", "C338", 0, time.Second)
	if _, _, err := a.ResolveSource("Nope"); err == nil {
		t.Fatal("want error for unknown source")
	}
	if _, _, err := a.ResolveSource("8"); err == nil {
		t.Fatal("want error for out-of-range index")
	}
	c368 := NewAmp("x", "C368", 0, time.Second)
	if c368.friendlyFromToken("3") != "Coaxial 1" {
		t.Fatal("C368 1-based token mapping")
	}
}

func TestD7050Binary(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan []byte, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 5*5)
		c.SetReadDeadline(time.Now().Add(time.Second))
		n, _ := readFull(c, buf)
		got <- buf[:n]
		// power=1, source=2, volume=110 (-35 dB), mute=0, with junk in between
		c.Write([]byte{0, 1, 2, 9, 1, 0xff, 0, 1, 2, 3, 2, 0, 1, 2, 4, 110, 0, 1, 2, 10, 0})
		time.Sleep(time.Second)
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	a := NewAmp("127.0.0.1", "d7050", port, time.Second)
	if err := a.Connect(); err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	st, err := a.GetStatus()
	if err != nil {
		t.Fatal(err)
	}
	q := <-got
	wantQ := []byte{0, 1, 2, 2, 11, 0, 1, 2, 2, 9, 0, 1, 2, 2, 3, 0, 1, 2, 2, 4, 0, 1, 2, 2, 10}
	if string(q) != string(wantQ) {
		t.Fatalf("queries % x, want % x", q, wantQ)
	}
	if !st.IsOn() || *st.Source != "Optical 1" || *st.VolumeDB != -35 || *st.Mute {
		t.Fatalf("unexpected status: %+v", st)
	}
	if r := d7050Raw(-35); r != 110 {
		t.Fatalf("d7050Raw(-35) = %d", r)
	}
	if clampRaw(d7050Raw(50)) != 200 || clampRaw(d7050Raw(-200)) != 0 {
		t.Fatal("d7050 clamp")
	}
}

func readFull(c net.Conn, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := c.Read(buf[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// TestDiscoverMDNS answers the PTR query like a Chromecast-enabled C338 does
// (PTR + TXT, no A record) and a D7050 (PTR + SRV + A).
func TestDiscoverMDNS(t *testing.T) {
	pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	old := mdnsGroup
	mdnsGroup = pc.LocalAddr().String()
	defer func() { mdnsGroup = old }()

	name := dnsmessage.MustNewName
	hdr := func(n string, typ dnsmessage.Type) dnsmessage.ResourceHeader {
		return dnsmessage.ResourceHeader{Name: name(n), Type: typ, Class: dnsmessage.ClassINET, TTL: 120}
	}
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFromUDP(buf)
			if err != nil {
				return
			}
			var p dnsmessage.Parser
			p.Start(buf[:n])
			q, _ := p.Question()
			b := dnsmessage.NewBuilder(nil, dnsmessage.Header{Response: true})
			b.StartAnswers()
			switch q.Name.String() {
			case svcCast:
				b.PTRResource(hdr(svcCast, dnsmessage.TypePTR),
					dnsmessage.PTRResource{PTR: name("NAD-Amplifier-C338-abc." + svcCast)})
				b.TXTResource(hdr("NAD-Amplifier-C338-abc."+svcCast, dnsmessage.TypeTXT),
					dnsmessage.TXTResource{TXT: []string{"md=NAD C338", "fn=Living Room"}})
				// a non-NAD cast device must be ignored
				b.PTRResource(hdr(svcCast, dnsmessage.TypePTR),
					dnsmessage.PTRResource{PTR: name("TV-xyz." + svcCast)})
				b.TXTResource(hdr("TV-xyz."+svcCast, dnsmessage.TypeTXT),
					dnsmessage.TXTResource{TXT: []string{"md=Samsung TV"}})
			case svcD7050:
				b.PTRResource(hdr(svcD7050, dnsmessage.TypePTR),
					dnsmessage.PTRResource{PTR: name("D7050." + svcD7050)})
				b.SRVResource(hdr("D7050."+svcD7050, dnsmessage.TypeSRV),
					dnsmessage.SRVResource{Target: name("d7050.local."), Port: BinaryPort})
				b.AResource(hdr("d7050.local.", dnsmessage.TypeA),
					dnsmessage.AResource{A: netip.MustParseAddr("10.0.0.7").As4()})
			default:
				continue
			}
			msg, _ := b.Finish()
			pc.WriteToUDP(msg, from)
		}
	}()

	got := map[string]Found{}
	for _, f := range discoverMDNS(300 * time.Millisecond) {
		got[f.Model] = f
	}
	if len(got) != 2 {
		t.Fatalf("found %d amps: %+v", len(got), got)
	}
	if c := got["C338"]; c.Host != "127.0.0.1" || c.Port != TextPort || c.Name != "Living Room" {
		t.Fatalf("C338: %+v", c)
	}
	if d := got["D7050"]; d.Host != "10.0.0.7" || d.Port != BinaryPort {
		t.Fatalf("D7050: %+v", d)
	}
}
