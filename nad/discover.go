package nad

import (
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// Found is one amplifier located by Discover.
type Found struct {
	Host  string
	Model string // "" if unknown
	Port  int
	Name  string
}

const (
	svcCast  = "_googlecast._tcp.local."
	svcD7050 = "_JB2XiOSApp._tcp.local."
)

// mdnsGroup is a var so tests can point discovery at a local fake responder.
var mdnsGroup = "224.0.0.251:5353"

// Discover finds NAD amps on the local network.
//
// Tries mDNS first. If that yields nothing, optionally falls back to a TCP port
// sweep of the local /24 for ports 30001 and 50001.
func Discover(timeout time.Duration, scanFallback bool) []Found {
	results := discoverMDNS(timeout)
	if len(results) == 0 && scanFallback {
		results = discoverScan(timeout)
	}
	return results
}

// ---------------------------------------------------------------------------
// mDNS
// ---------------------------------------------------------------------------

// discoverMDNS sends one-shot "legacy unicast" queries (RFC 6762 §6.7) from an
// ephemeral port, so it never needs to bind 5353 — which avahi/Bonjour on the
// host (e.g. a Synology NAS) usually holds. Responders answer straight back to
// us by unicast.
func discoverMDNS(timeout time.Duration) []Found {
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return nil
	}
	defer conn.Close()
	group, _ := net.ResolveUDPAddr("udp4", mdnsGroup)

	query := func(name string, t dnsmessage.Type) {
		b := dnsmessage.NewBuilder(nil, dnsmessage.Header{})
		b.StartQuestions()
		b.Question(dnsmessage.Question{
			Name: dnsmessage.MustNewName(name), Type: t, Class: dnsmessage.ClassINET,
		})
		if msg, err := b.Finish(); err == nil {
			conn.WriteToUDP(msg, group)
		}
	}
	query(svcCast, dnsmessage.TypePTR)
	query(svcD7050, dnsmessage.TypePTR)

	type instance struct {
		service string
		txt     map[string]string
		target  string
		port    int
		from    string // responder address; fallback when no A record
		askedTX bool
	}
	instances := map[string]*instance{}
	addrs := map[string]string{} // hostname -> IPv4
	get := func(name string) *instance {
		if instances[name] == nil {
			instances[name] = &instance{}
		}
		return instances[name]
	}

	deadline := time.Now().Add(timeout)
	buf := make([]byte, 9000)
	for {
		conn.SetReadDeadline(deadline)
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			break
		}
		var p dnsmessage.Parser
		if _, err := p.Start(buf[:n]); err != nil {
			continue
		}
		p.SkipAllQuestions()
		var rrs []dnsmessage.Resource
		for _, section := range []func() ([]dnsmessage.Resource, error){p.AllAnswers, p.AllAuthorities, p.AllAdditionals} {
			r, err := section()
			if err != nil {
				break
			}
			rrs = append(rrs, r...)
		}
		for _, rr := range rrs {
			name := strings.ToLower(rr.Header.Name.String())
			switch body := rr.Body.(type) {
			case *dnsmessage.PTRResource:
				svc := strings.ToLower(name)
				if svc != strings.ToLower(svcCast) && svc != strings.ToLower(svcD7050) {
					continue
				}
				in := get(strings.ToLower(body.PTR.String()))
				in.service = svc
				in.from = from.IP.String()
			case *dnsmessage.SRVResource:
				in := get(name)
				in.target = strings.ToLower(body.Target.String())
				in.port = int(body.Port)
			case *dnsmessage.TXTResource:
				in := get(name)
				in.txt = map[string]string{}
				for _, kv := range body.TXT {
					k, v, _ := strings.Cut(kv, "=")
					in.txt[k] = v
				}
			case *dnsmessage.AResource:
				addrs[name] = netip.AddrFrom4(body.A).String()
			}
		}
		// Cast devices sometimes leave TXT out of the PTR reply; ask for it.
		for name, in := range instances {
			if in.service != "" && in.txt == nil && !in.askedTX {
				in.askedTX = true
				query(name, dnsmessage.TypeTXT)
			}
		}
	}

	found := map[string]Found{}
	for name, in := range instances {
		host := addrs[in.target]
		if host == "" {
			host = in.from
		}
		if host == "" {
			continue
		}
		switch in.service {
		case strings.ToLower(svcCast):
			if !strings.Contains(in.txt["md"], "C338") {
				continue
			}
			fn := in.txt["fn"]
			if fn == "" {
				fn = name
			}
			found[host] = Found{Host: host, Model: "C338", Port: TextPort, Name: fn}
		case strings.ToLower(svcD7050):
			found[host] = Found{Host: host, Model: "D7050", Port: BinaryPort, Name: name}
		}
	}
	out := make([]Found, 0, len(found))
	for _, f := range found {
		out = append(out, f)
	}
	return out
}

// ---------------------------------------------------------------------------
// TCP sweep fallback
// ---------------------------------------------------------------------------

func localSubnet() string {
	c, err := net.Dial("udp4", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer c.Close()
	ip := c.LocalAddr().(*net.UDPAddr).IP.String()
	return ip[:strings.LastIndex(ip, ".")+1]
}

func discoverScan(timeout time.Duration) []Found {
	prefix := localSubnet()
	if prefix == "" {
		return nil
	}
	perHost := min(500*time.Millisecond, max(200*time.Millisecond, timeout/255))

	var (
		mu   sync.Mutex
		hits []Found
		wg   sync.WaitGroup
		sem  = make(chan struct{}, 256)
	)
	for h := 1; h < 255; h++ {
		for _, port := range []int{TextPort, BinaryPort} {
			ip := prefix + strconv.Itoa(h)
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer func() { <-sem; wg.Done() }()
				c, err := dial(ip, port, perHost)
				if err != nil {
					return
				}
				c.Close()
				f := Found{Host: ip, Port: port, Model: "D7050"}
				if port == TextPort {
					f.Model = probeModel(ip)
				}
				mu.Lock()
				hits = append(hits, f)
				mu.Unlock()
			}()
		}
	}
	wg.Wait()
	return hits
}

func probeModel(ip string) string {
	c := NewTextClient(ip, TextPort, 1500*time.Millisecond)
	if c.Connect() != nil {
		return ""
	}
	defer c.Close()
	r, err := c.Query("Main.Model", 500*time.Millisecond, 2)
	if err != nil {
		return ""
	}
	return cleanModel(r["Main.Model"])
}
