package linkmgr

import (
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

const (
	ScopeGlobal  AddrScope = 0
	ScopeLink    AddrScope = 1
	wgBinary               = "wireguard-go"
	wgRunPath              = "/var/run/wireguard"
	ifconfigPath           = "/sbin/ifconfig"
	routePath              = "/sbin/route"
)

type LinkError struct {
	LinkName string
	E        error
}

func prepareWGDir() error {
	_, err := os.Stat(wgRunPath)
	if os.IsNotExist(err) {
		err := os.Mkdir(wgRunPath, 0755)
		if err != nil {
			return fmt.Errorf("link mngr: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("link mngr: %w", err)
	}
	return nil
}

//
// link implements the Link interface
//

type linkTUN struct {
	name              string
	realInterface     string
	interfaceNameFile string
	interfaceSockFile string
	iface             net.Interface
	mngr              *linkMngr
}

func (l *linkTUN) Interface() net.Interface {
	iface, err := net.InterfaceByName(l.realInterface)
	if err != nil {
		panic(err)
	}
	l.iface = *iface
	return l.iface
}

func (l *linkTUN) Name() string {
	return l.name
}

func (l *linkTUN) Index() int {
	return l.iface.Index
}

func (l *linkTUN) IsUp() bool {
	// refresh interface
	iface, err := net.InterfaceByName(l.realInterface)
	if err != nil {
		panic(err)
	}
	l.iface = *iface
	// use flags to figure out status
	flags := l.iface.Flags.String()
	if strings.Contains(flags, "up") {
		return true
	}
	return false
}

func (l *linkTUN) ListenUDP(local net.UDPAddr) (*net.UDPConn, error) {
	// Apparentlty there is a weird race condition between link configuration
	// and binding that seems to disappear if index-based address zone is used.
	local.Zone = strconv.Itoa(l.iface.Index)
	return net.ListenUDP("udp", &local)
}

func (l *linkTUN) DialUDP(local, remote net.UDPAddr) (*net.UDPConn, error) {
	// Apparentlty there is a weird race condition between link configuration
	// and binding that seems to disappear if index-based address zone is used.
	local.Zone = strconv.Itoa(l.iface.Index)
	remote.Zone = strconv.Itoa(l.iface.Index)

	localPtr := &local
	if localPtr.IP == nil {
		localPtr = nil
	}

	return net.DialUDP("udp", localPtr, &remote)
}

func (l *linkTUN) SetUp(status bool) error {
	var cmd *exec.Cmd

	if status {
		cmd = exec.Command(ifconfigPath, l.realInterface, "up")
	} else {
		cmd = exec.Command(ifconfigPath, l.realInterface, "down")
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to set up link '%s': %s", l.name, string(output))
	}
	return nil
}

func (l *linkTUN) Addrs() ([]Address, error) {
	addresses := []Address{}

	iface, err := net.InterfaceByName(l.realInterface)
	if err != nil {
		return addresses, fmt.Errorf("failed to retrieve addresses for link '%s': %w", l.name, err)
	}

	addrs, err := iface.Addrs()
	if err != nil {
		return addresses, fmt.Errorf("failed to retrieve addresses for link '%s': %w", l.name, err)
	}
	for _, addr := range addrs {
		ip, netw, err := net.ParseCIDR(addr.String())
		if err != nil {
			return addresses, fmt.Errorf("failed to retrieve addresses for link '%s': %w", l.name, err)
		}
		netw.IP = ip
		addresses = append(addresses, Address{IPNet: *netw})
	}

	return addresses, nil
}

func (l *linkTUN) DelAddr(a Address) error {
	var cmd *exec.Cmd

	// use ifconfig to add address to interface. If address has 2 or more semi-colons, it is an IPv6 address
	if strings.Count(a.String(), ":") >= 2 {
		cmd = exec.Command(ifconfigPath, l.realInterface, "inet6", a.String(), "-alias")
	} else {
		cmd = exec.Command(ifconfigPath, l.realInterface, "inet", a.String(), a.IP.String(), "-alias")
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to delete address from link '%s': %s", l.name, string(output))
	}
	return nil
}

func (l *linkTUN) AddAddr(a Address) error {
	var cmd *exec.Cmd

	addr := a.IP.String()
	if a.Scope == ScopeLink {
		addr = a.IP.String() + "%" + l.realInterface
	}

	// use ifconfig to add address to interface. If address has 2 or more semi-colons, it is an IPv6 address
	if strings.Count(a.String(), ":") >= 2 {
		// IPv6
		len, _ := a.Mask.Size()
		cmd = exec.Command(ifconfigPath, l.realInterface, "inet6", addr, "prefixlen", strconv.Itoa(len))
	} else {
		// IPv4
		cmd = exec.Command(ifconfigPath, l.realInterface, "inet", a.IPNet.String())

		if a.Peer != nil && a.Peer.IP != nil {
			cmd.Args = append(cmd.Args, a.Peer.IP.String())
		} else {
			cmd.Args = append(cmd.Args, a.IP.String())
		}
	}

	cmd.Args = append(cmd.Args, "alias")

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to add address to link '%s': %s", l.name, string(output))
	}
	return nil
}

func (l *linkTUN) ConfigureWG(c wgtypes.Config) error {
	if err := l.mngr.wg.ConfigureDevice(l.iface.Name, c); err != nil {
		return fmt.Errorf("failed to configure link '%s': %w", l.name, err)
	}
	return nil
}

func (l *linkTUN) WGConfig() (*wgtypes.Device, error) {
	dev, err := l.mngr.wg.Device(l.iface.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve device for link '%s': %w", l.name, err)
	}
	return dev, nil
}

func (l *linkTUN) GetRoutes() ([]Route, error) {
	routes := []Route{}

	return routes, nil
}

func (l *linkTUN) AddRoute(r Route) error {
	cmd := exec.Command(routePath, "-n", "add", "-net", r.Dest.String(), "-interface", l.realInterface)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to add route to link '%s': %s", l.name, string(output))
	}
	return nil
}

func (l *linkTUN) DelRoute(r Route) error {
	cmd := exec.Command(routePath, "-n", "delete", "-net", r.Dest.String(), "-interface", l.realInterface)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to add route to link '%s': %s", l.name, string(output))
	}
	return nil
}

//
// linkMngr implements the Manager interface
//

type linkMngr struct {
	wg           *wgctrl.Client
	wgBinaryPath string
}

func (m *linkMngr) Links() ([]Link, error) {

	if err := prepareWGDir(); err != nil {
		return []Link{}, err
	}

	// retrieve all files in the wireguard run path
	f, err := os.Open(wgRunPath)
	if err != nil {
		return []Link{}, fmt.Errorf("failed to retrieve wireguard links: %w", err)
	}
	files, err := f.Readdir(-1)
	f.Close()
	if err != nil {
		return []Link{}, fmt.Errorf("failed to retrieve wireguard links: %w", err)
	}

	// make list of interface files
	interfaceNameFiles := []string{}
	for _, file := range files {
		if strings.Contains(file.Name(), ".name") {
			interfaceNameFiles = append(interfaceNameFiles, strings.TrimSuffix(file.Name(), ".name"))
		}
	}

	// make list of links
	links := []Link{}
	for _, name := range interfaceNameFiles {
		lnk, err := m.GetLink(name)
		if err != nil {
			return []Link{}, fmt.Errorf("failed to retrieve wireguard links: %w", err)
		}
		links = append(links, lnk)
	}

	return links, nil
}

func (m *linkMngr) CreateLink(name string) (Link, error) {
	if err := prepareWGDir(); err != nil {
		return &linkTUN{}, err
	}

	_, err := m.GetLink(name)
	if err == nil {
		return &linkTUN{}, fmt.Errorf("failed to create link using wireguard-go: link '%s' already exists", name)
	}

	interfaceFile := fmt.Sprintf("%s/%s.name", wgRunPath, name)
	additionalEnv := fmt.Sprintf("WG_TUN_NAME_FILE=%s", interfaceFile)
	newEnv := append(os.Environ(), additionalEnv)

	// execute wireguard-go
	cmd := exec.Command(m.wgBinaryPath, "utun")
	cmd.Env = newEnv
	output, err := cmd.CombinedOutput()
	if err != nil {
		return &linkTUN{}, fmt.Errorf("failed to create link using wireguard-go: \n---- WG output ----\n%s-------------------", string(output))
	}

	// read interface file and figure out the real interface
	sockData, err := ioutil.ReadFile(interfaceFile)
	if err != nil {
		return &linkTUN{}, fmt.Errorf("failed to create link using wireguard-go: %w", err)
	}
	realInterfaceSock := strings.TrimSuffix(string(sockData), "\n")
	if realInterfaceSock == "" {
		return &linkTUN{}, fmt.Errorf("failed to create link using wireguard-go: '%s' contains invalid data", interfaceFile)
	}

	return m.GetLink(name)
}

func (m *linkMngr) DelLink(name string) error {
	lnk, err := m.GetLink(name)
	if err != nil {
		return err
	}

	link := lnk.(*linkTUN)

	// remove the sock file which will lead to the shutdown of wireguard-go
	err = os.Remove(link.interfaceSockFile)
	if err != nil {
		return fmt.Errorf("could not delete link '%s': %w", name, err)
	}

	// remove the .name file
	err = os.Remove(link.interfaceNameFile)
	if err != nil {
		return fmt.Errorf("could not delete link '%s': %w", name, err)
	}
	return nil
}

func (m *linkMngr) GetLink(name string) (Link, error) {

	if err := prepareWGDir(); err != nil {
		return &linkTUN{}, err
	}

	interfaceFile := fmt.Sprintf("%s/%s.name", wgRunPath, name)

	// read interface file and figure out the real interface
	sockData, err := ioutil.ReadFile(interfaceFile)
	if err != nil {
		return &linkTUN{}, fmt.Errorf("failed to get link '%s': %w", name, err)
	}
	realInterface := strings.TrimSuffix(string(sockData), "\n")
	if realInterface == "" {
		return &linkTUN{}, fmt.Errorf("failed to get link '%s': '%s' contains invalid data", name, interfaceFile)
	}

	iface, err := net.InterfaceByName(realInterface)
	if err != nil {
		return &linkTUN{}, fmt.Errorf("failed to get link '%s': %w", name, err)
	}

	return &linkTUN{
		name:              name,
		realInterface:     realInterface,
		interfaceNameFile: interfaceFile,
		interfaceSockFile: fmt.Sprintf("%s/%s.sock", wgRunPath, realInterface),
		iface:             *iface,
		mngr:              m,
	}, nil
}

func (m *linkMngr) Close() error {
	return m.wg.Close()
}

// NewManager returns a link manager based on the wireguard-go userspace implementation
func NewManager() (Manager, error) {
	wgBinaryPath, err := exec.LookPath(wgBinary)
	if err != nil {
		return nil, fmt.Errorf("link mngr: %w", fmt.Errorf("macOS requires the WireGuard userspace implementation (%s) to be installed: %w", wgBinaryPath, err))
	}

	wg, err := wgctrl.New()
	if err != nil {
		return nil, fmt.Errorf("link mngr: %w", err)
	}
	return &linkMngr{wg: wg, wgBinaryPath: wgBinaryPath}, nil
}
