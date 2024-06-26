package wboxclient

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/nustiueudinastea/wirebox"
	"github.com/nustiueudinastea/wirebox/linkmgr"
	wboxproto "github.com/nustiueudinastea/wirebox/proto"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func configureTunnel(m linkmgr.Manager, cfg Config) error {
	log.Println("configuring tunnel")
	pubKey := cfg.PrivateKey.PublicFromPrivate()
	configIPv6 := wirebox.IPv6LLForClient(pubKey)

	tunLink, created, err := createConfigTun(m, cfg, configIPv6)
	if err != nil {
		return fmt.Errorf("configure tun: %w", err)
	}

	clCfg, err := solictCfg(cfg, configIPv6, pubKey, tunLink)
	if err != nil {
		if created {
			if err := m.DelLink(tunLink.Name()); err != nil {
				log.Println("error: failed to delete link:", err)
			}
		}
		return fmt.Errorf("configure tun: %w", err)
	}

	if err := setTunnelCfg(m, cfg, configIPv6, clCfg); err != nil {
		if created {
			if err := m.DelLink(tunLink.Name()); err != nil {
				log.Println("error: failed to delete link:", err)
			}
		}
		return fmt.Errorf("configure tun: %w", err)
	}
	return nil
}

func setTunnelCfg(m linkmgr.Manager, cfg Config, configIPv6 net.IP, clCfg *wboxproto.Cfg) error {
	wgCfg := wgtypes.Config{
		PrivateKey: &cfg.PrivateKey.Bytes,
		Peers: []wgtypes.PeerConfig{
			{
				PublicKey:         cfg.ServerKey.Bytes,
				ReplaceAllowedIPs: true,
				AllowedIPs: []net.IPNet{
					{
						IP:   wirebox.SolictIPv6,
						Mask: net.CIDRMask(128, 128),
					},
					{
						IP:   configIPv6,
						Mask: net.CIDRMask(128, 128),
					},
				},
			},
		},
	}

	srvEndpoint := cfg.ConfigEndpoint
	if port := clCfg.GetTunPort(); port != 0 {
		srvEndpoint.Port = int(port)
	}
	if endp := clCfg.GetTun4Endpoint(); endp != 0 {
		srvEndpoint.IP = wboxproto.IPv4(clCfg.GetTun4Endpoint())
	}
	if endp := clCfg.GetTun6Endpoint(); endp != nil {
		srvEndpoint.IP = clCfg.GetTun6Endpoint().AsIP()
	}
	// TODO: Test IPv6 connectivity and do not attempt to use it?
	log.Printf("tunnel via %v:%v", srvEndpoint.IP, srvEndpoint.Port)
	wgCfg.Peers[0].Endpoint = &srvEndpoint.UDPAddr

	if clCfg.GetServer4() != 0 {
		log.Println("server4:", wboxproto.IPv4(clCfg.GetServer4()))
	}
	if clCfg.GetServer6() != nil {
		log.Println("server6:", clCfg.GetServer6().AsIP())
	}

	addrs := make([]linkmgr.Address, 0, len(clCfg.Net6)+len(clCfg.Net4))
	for _, net6 := range clCfg.Net6 {
		wgCfg.Peers[0].AllowedIPs = append(wgCfg.Peers[0].AllowedIPs, net.IPNet{
			IP:   net6.GetAddr().AsIP(),
			Mask: net.CIDRMask(int(net6.GetPrefixLen()), 128),
		})

		log.Printf("using addr %v/%v", net6.Addr.AsIP(), net6.GetPrefixLen())
		addr := linkmgr.Address{
			IPNet: net.IPNet{
				IP:   net6.Addr.AsIP(),
				Mask: net.CIDRMask(int(net6.GetPrefixLen()), 128),
			},
			Scope: linkmgr.ScopeGlobal,
		}
		if net6.GetPrefixLen() == 128 {
			addr.Peer = &net.IPNet{
				IP:   clCfg.GetServer6().AsIP(),
				Mask: net.CIDRMask(128, 128),
			}
		}
		addrs = append(addrs, addr)
	}
	for _, net4 := range clCfg.Net4 {
		ip := wboxproto.IPv4(net4.GetAddr()).To4()
		mask := net.CIDRMask(int(net4.GetPrefixLen()), 32)
		wgCfg.Peers[0].AllowedIPs = append(wgCfg.Peers[0].AllowedIPs, net.IPNet{
			IP:   ip,
			Mask: mask,
		})

		log.Printf("using addr %v/%v", wboxproto.IPv4(net4.Addr), net4.GetPrefixLen())
		addr := linkmgr.Address{
			IPNet: net.IPNet{
				IP:   wboxproto.IPv4(net4.Addr),
				Mask: net.CIDRMask(int(net4.GetPrefixLen()), 32),
			},
			Scope: linkmgr.ScopeGlobal,
		}
		if net4.GetPrefixLen() == 32 {
			addr.Peer = &net.IPNet{
				IP:   wboxproto.IPv4(clCfg.GetServer4()),
				Mask: net.CIDRMask(32, 32),
			}
		}
		addrs = append(addrs, addr)
	}

	for _, route4 := range clCfg.Routes4 {
		log.Printf("using route %v/%v src %v",
			wboxproto.IPv4(route4.Dest.Addr), route4.Dest.PrefixLen,
			wboxproto.IPv4(route4.Src))
		wgCfg.Peers[0].AllowedIPs = append(wgCfg.Peers[0].AllowedIPs, net.IPNet{
			IP:   wboxproto.IPv4(route4.GetDest().Addr),
			Mask: net.CIDRMask(int(route4.GetDest().GetPrefixLen()), 32),
		})
	}
	for _, route6 := range clCfg.Routes6 {
		log.Printf("using route %v/%v src %v",
			route6.Dest.Addr.AsIP(), route6.Dest.PrefixLen,
			route6.Src.AsIP())
		wgCfg.Peers[0].AllowedIPs = append(wgCfg.Peers[0].AllowedIPs, net.IPNet{
			IP:   route6.GetDest().Addr.AsIP(),
			Mask: net.CIDRMask(int(route6.GetDest().GetPrefixLen()), 128),
		})
	}

	tunLink, _, err := wirebox.CreateWG(m, cfg.If, wgCfg, addrs)
	if err != nil {
		return fmt.Errorf("set config: %w", err)
	}
	log.Println("tunnel reconfigured")

	for i, route4 := range clCfg.Routes4 {
		route := linkmgr.Route{
			Dest: net.IPNet{
				IP:   wboxproto.IPv4(route4.GetDest().Addr),
				Mask: net.CIDRMask(int(route4.GetDest().GetPrefixLen()), 32),
			},
		}
		if route4.GetSrc() != 0 {
			route.Src = wboxproto.IPv4(route4.GetSrc())
		}
		if err := tunLink.AddRoute(route); err != nil {
			if errors.Is(err, syscall.EEXIST) {
				continue
			}
			return fmt.Errorf("set config: route4 add %v: %w", i, err)
		}
	}
	log.Println("installed IPv4 routes")

	for i, route6 := range clCfg.Routes6 {
		route := linkmgr.Route{
			Dest: net.IPNet{
				IP:   route6.GetDest().Addr.AsIP(),
				Mask: net.CIDRMask(int(route6.GetDest().GetPrefixLen()), 128),
			},
		}
		if route6.GetSrc() != nil {
			route.Src = route6.GetSrc().AsIP()
		}
		if err := tunLink.AddRoute(route); err != nil {
			if errors.Is(err, syscall.EEXIST) {
				continue
			}
			return fmt.Errorf("set config: route6 add %v: %w", i, err)
		}
	}
	log.Println("installed IPv6 routes")

	return nil
}

func createConfigTun(m linkmgr.Manager, cfg Config, configIPv6 net.IP) (linkmgr.Link, bool, error) {
	tunLink, created, err := wirebox.CreateWG(m, cfg.If, wgtypes.Config{
		PrivateKey: &cfg.PrivateKey.Bytes,
		Peers: []wgtypes.PeerConfig{
			{
				PublicKey: cfg.ServerKey.Bytes,
				Endpoint:  &cfg.ConfigEndpoint.UDPAddr,
				// ReplaceAllowedIPs: false
				//  We want to permit regular traffic while we attempt tunnel
				//  reconfiguration.
				AllowedIPs: []net.IPNet{
					{
						IP:   wirebox.SolictIPv6,
						Mask: net.CIDRMask(128, 128),
					},
					{
						IP:   configIPv6,
						Mask: net.CIDRMask(128, 128),
					},
				},
			},
		},
	}, []linkmgr.Address{
		{
			IPNet: net.IPNet{
				IP:   configIPv6,
				Mask: net.CIDRMask(128, 128),
			},
			Peer: &net.IPNet{
				IP:   wirebox.SolictIPv6,
				Mask: net.CIDRMask(128, 128),
			},
			Scope: linkmgr.ScopeLink,
		},
	})
	if err != nil {
		return nil, false, fmt.Errorf("create config tun: %w", err)
	}
	if created {
		log.Println("created link", tunLink.Name())
	} else {
		log.Println("using existing link", tunLink.Name())
	}
	return tunLink, created, nil
}

func solictCfg(cfg Config, configIPv6 net.IP, pubKey wirebox.PeerKey, tunLink linkmgr.Link) (*wboxproto.Cfg, error) {
	c, err := tunLink.DialUDP(net.UDPAddr{
		IP: configIPv6,
	}, net.UDPAddr{
		IP:   wirebox.SolictIPv6,
		Port: wirebox.SolictPort,
	})
	if err != nil {
		return nil, fmt.Errorf("solict cfg: %w", err)
	}
	defer c.Close()

	for {
		log.Println("solicting configuration")
		solictMsg, err := wboxproto.Pack(&wboxproto.CfgSolict{
			PeerPubkey: pubKey.Bytes[:],
		})
		if err != nil {
			return nil, fmt.Errorf("solict cfg: %w", err)
		}
		if _, err := c.Write(solictMsg); err != nil {
			// We can get ICMP errors reported at the next Write. Stop if we got ICMP "No route to host",
			// "Port unreachable" (EREFUSED) or whatever.
			return nil, fmt.Errorf("solict cfg: %w", err)
		}

		if err := c.SetReadDeadline(time.Now().Add(cfg.ConfigTimeout.Duration)); err != nil {
			log.Println("error: cannot set timeout, configuration may hang:", err)
		}

		buffer := make([]byte, 1420)
		readBytes, sender, err := c.ReadFromUDP(buffer)
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Temporary() {
				log.Println("timed out waiting for response, retrying")
				continue
			}
			return nil, fmt.Errorf("solict cfg: %w", err)
		}

		if !sender.IP.Equal(wirebox.SolictIPv6) {
			return nil, fmt.Errorf("solict cfg: unexpected response sender %v", sender.IP)
		}
		if sender.Port != wirebox.SolictPort {
			return nil, fmt.Errorf("solict cfg: unexpected response source port %v", sender.Port)
		}

		resp, err := wboxproto.Unpack(buffer[:readBytes])
		if err != nil {
			log.Println("malformed response, retrying:", err)
			continue
		}
		switch resp := resp.(type) {
		case *wboxproto.Cfg:
			return resp, nil
		case *wboxproto.Nack:
			return nil, fmt.Errorf("solict cfg: server refused to give us config: %v", resp.GetDescription())
		default:
			return nil, fmt.Errorf("solict cfg: unexpected reply: %T", resp)
		}
	}
}

func Main() int {
	// Read configuration and command line flags.
	cfgPath := flag.String("config", "wbox.toml", "path to configuration file")
	flag.Parse()
	cfgF, err := os.Open(*cfgPath)
	if err != nil {
		log.Println("error:", err)
		return 2
	}
	var cfg Config
	if _, err := toml.DecodeReader(cfgF, &cfg); err != nil {
		log.Println("error: config load:", err)
		return 2
	}
	if cfg.ConfigTimeout.Duration == 0 {
		cfg.ConfigTimeout.Duration = 5 * time.Second
	}

	m, err := linkmgr.NewManager()
	if err != nil {
		log.Println("error: link mngr init:", err)
		return 1
	}

	log.Println("client public key:", cfg.PrivateKey.PublicFromPrivate())

	if err := configureTunnel(m, cfg); err != nil {
		log.Println("error:", err)
		return 1
	}

	return 0
}
