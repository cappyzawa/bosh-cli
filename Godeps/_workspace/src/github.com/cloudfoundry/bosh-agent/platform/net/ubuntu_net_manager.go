package net

import (
	"bytes"
	"path/filepath"
	"strings"
	"text/template"

	bosherr "github.com/cloudfoundry/bosh-agent/errors"
	boshlog "github.com/cloudfoundry/bosh-agent/logger"
	bosharp "github.com/cloudfoundry/bosh-agent/platform/net/arp"
	boship "github.com/cloudfoundry/bosh-agent/platform/net/ip"
	boshsettings "github.com/cloudfoundry/bosh-agent/settings"
	boshsys "github.com/cloudfoundry/bosh-agent/system"
)

const ubuntuNetManagerLogTag = "ubuntuNetManager"

type ubuntuNetManager struct {
	DefaultNetworkResolver

	cmdRunner          boshsys.CmdRunner
	fs                 boshsys.FileSystem
	ipResolver         boship.Resolver
	addressBroadcaster bosharp.AddressBroadcaster
	logger             boshlog.Logger
}

func NewUbuntuNetManager(
	fs boshsys.FileSystem,
	cmdRunner boshsys.CmdRunner,
	defaultNetworkResolver DefaultNetworkResolver,
	ipResolver boship.Resolver,
	addressBroadcaster bosharp.AddressBroadcaster,
	logger boshlog.Logger,
) Manager {
	return ubuntuNetManager{
		DefaultNetworkResolver: defaultNetworkResolver,
		cmdRunner:              cmdRunner,
		fs:                     fs,
		ipResolver:             ipResolver,
		addressBroadcaster:     addressBroadcaster,
		logger:                 logger,
	}
}

func (net ubuntuNetManager) SetupDhcp(networks boshsettings.Networks, errCh chan error) error {
	net.logger.Debug(ubuntuNetManagerLogTag, "Configuring DHCP networking")

	err := net.writeDhcpNetworkInterfaces()
	if err != nil {
		return bosherr.WrapError(err, "Generating interfaces config from template")
	}

	buffer := bytes.NewBuffer([]byte{})
	t := template.Must(template.New("dhcp-config").Parse(ubuntuDHCPConfigTemplate))

	// Keep DNS servers in the order specified by the network
	// because they are added by a *single* DHCP's prepend command
	dnsNetwork, _ := networks.DefaultNetworkFor("dns")
	dnsServersList := strings.Join(dnsNetwork.DNS, ", ")
	err = t.Execute(buffer, dnsServersList)
	if err != nil {
		return bosherr.WrapError(err, "Generating config from template")
	}

	dhclientConfigFile := "/etc/dhcp/dhclient.conf"
	written, err := net.fs.ConvergeFileContents(dhclientConfigFile, buffer.Bytes())
	if err != nil {
		return bosherr.WrapErrorf(err, "Writing to %s", dhclientConfigFile)
	}

	if written {
		args := net.restartNetworkArguments()

		net.logger.Debug(ubuntuNetManagerLogTag, "Restarting network interfaces")

		_, _, _, err := net.cmdRunner.RunCommand("ifdown", args...)
		if err != nil {
			net.logger.Error(ubuntuNetManagerLogTag, "Ignoring ifdown failure: %s", err.Error())
		}

		_, _, _, err = net.cmdRunner.RunCommand("ifup", args...)
		if err != nil {
			net.logger.Error(ubuntuNetManagerLogTag, "Ignoring ifup failure: %s", err.Error())
		}
	}

	addresses := []boship.InterfaceAddress{
		// eth0 is hard coded in AWS and OpenStack stemcells.
		// TODO: abstract hardcoded network interface name to the Manager
		boship.NewResolvingInterfaceAddress("eth0", net.ipResolver),
	}

	go func() {
		net.addressBroadcaster.BroadcastMACAddresses(addresses)
		if errCh != nil {
			errCh <- nil
		}
	}()

	return nil
}

// DHCP Config file - /etc/dhcp/dhclient.conf
// Ubuntu 14.04 accepts several DNS as a list in a single prepend directive
const ubuntuDHCPConfigTemplate = `# Generated by bosh-agent

option rfc3442-classless-static-routes code 121 = array of unsigned integer 8;

send host-name "<hostname>";

request subnet-mask, broadcast-address, time-offset, routers,
	domain-name, domain-name-servers, domain-search, host-name,
	netbios-name-servers, netbios-scope, interface-mtu,
	rfc3442-classless-static-routes, ntp-servers;
{{ if . }}
prepend domain-name-servers {{ . }};{{ end }}
`

func (net ubuntuNetManager) SetupManualNetworking(networks boshsettings.Networks, errCh chan error) error {
	net.logger.Debug(ubuntuNetManagerLogTag, "Configuring manual networking")

	modifiedNetworks, written, err := net.writeNetworkInterfaces(networks)
	if err != nil {
		return bosherr.WrapError(err, "Writing network interfaces")
	}

	if written {
		net.restartNetworkingInterfaces(modifiedNetworks)
	}

	addresses := toInterfaceAddresses(modifiedNetworks)

	go func() {
		net.addressBroadcaster.BroadcastMACAddresses(addresses)
		if errCh != nil {
			errCh <- nil
		}
	}()

	return nil
}

func (net ubuntuNetManager) writeNetworkInterfaces(networks boshsettings.Networks) ([]customNetwork, bool, error) {
	var modifiedNetworks []customNetwork

	macAddresses, err := net.detectMacAddresses()
	if err != nil {
		return modifiedNetworks, false, bosherr.WrapError(err, "Detecting mac addresses")
	}

	for _, aNet := range networks {
		network, broadcast, err := boshsys.CalculateNetworkAndBroadcast(aNet.IP, aNet.Netmask)
		if err != nil {
			return modifiedNetworks, false, bosherr.WrapError(err, "Calculating network and broadcast")
		}

		newNet := customNetwork{
			aNet,
			macAddresses[aNet.Mac],
			network,
			broadcast,
			true,
		}
		modifiedNetworks = append(modifiedNetworks, newNet)
	}

	networkInterfaceValues := networkInterfaceConfigArg{
		Networks:          modifiedNetworks,
		HasDNSNameServers: false,
	}

	buffer := bytes.NewBuffer([]byte{})

	dnsNetwork, _ := networks.DefaultNetworkFor("dns")
	networkInterfaceValues.HasDNSNameServers = true
	networkInterfaceValues.DNSServers = dnsNetwork.DNS

	t := template.Must(template.New("network-interfaces").Parse(networkInterfacesTemplate))

	err = t.Execute(buffer, networkInterfaceValues)
	if err != nil {
		return modifiedNetworks, false, bosherr.WrapError(err, "Generating config from template")
	}

	written, err := net.fs.ConvergeFileContents("/etc/network/interfaces", buffer.Bytes())
	if err != nil {
		return modifiedNetworks, false, bosherr.WrapError(err, "Writing to /etc/network/interfaces")
	}

	return modifiedNetworks, written, nil
}

const networkInterfacesTemplate = `# Generated by bosh-agent
auto lo
iface lo inet loopback
{{ range .Networks }}
auto {{ .Interface }}
iface {{ .Interface }} inet static
    address {{ .IP }}
    network {{ .NetworkIP }}
    netmask {{ .Netmask }}
    broadcast {{ .Broadcast }}
{{ if .HasDefaultGateway }}    gateway {{ .Gateway }}{{ end }}{{ end }}
{{ if .HasDNSNameServers }}dns-nameservers{{ range .DNSServers }} {{ . }}{{ end }}{{ end }}`

func (net ubuntuNetManager) writeResolvConf(networks boshsettings.Networks) error {
	net.logger.Debug(ubuntuNetManagerLogTag, "Writing resolv.conf")

	buffer := bytes.NewBuffer([]byte{})
	t := template.Must(template.New("resolv-conf").Parse(ubuntuResolvConfTemplate))

	// Keep DNS servers in the order specified by the network
	dnsNetwork, _ := networks.DefaultNetworkFor("dns")
	dnsServersArg := dnsConfigArg{dnsNetwork.DNS}
	err := t.Execute(buffer, dnsServersArg)
	if err != nil {
		return bosherr.WrapError(err, "Generating config from template")
	}

	err = net.fs.WriteFile("/etc/resolv.conf", buffer.Bytes())
	if err != nil {
		return bosherr.WrapError(err, "Writing to /etc/resolv.conf")
	}

	return nil
}

const ubuntuResolvConfTemplate = `# Generated by bosh-agent
{{ range .DNSServers }}nameserver {{ . }}
{{ end }}`

func (net ubuntuNetManager) detectMacAddresses() (map[string]string, error) {
	addresses := map[string]string{}

	filePaths, err := net.fs.Glob("/sys/class/net/*")
	if err != nil {
		return addresses, bosherr.WrapError(err, "Getting file list from /sys/class/net")
	}

	var macAddress string
	for _, filePath := range filePaths {
		macAddress, err = net.fs.ReadFileString(filepath.Join(filePath, "address"))
		if err != nil {
			return addresses, bosherr.WrapError(err, "Reading mac address from file")
		}

		macAddress = strings.Trim(macAddress, "\n")

		interfaceName := filepath.Base(filePath)
		addresses[macAddress] = interfaceName
	}

	return addresses, nil
}

func (net ubuntuNetManager) restartNetworkingInterfaces(networks []customNetwork) {
	for _, network := range networks {
		net.logger.Debug(ubuntuNetManagerLogTag, "Restarting network interface %s", network.Interface)

		_, _, _, err := net.cmdRunner.RunCommand("service", "network-interface", "stop", "INTERFACE="+network.Interface)
		if err != nil {
			net.logger.Error(ubuntuNetManagerLogTag, "Ignoring network stop failure: %s", err.Error())
		}

		_, _, _, err = net.cmdRunner.RunCommand("service", "network-interface", "start", "INTERFACE="+network.Interface)
		if err != nil {
			net.logger.Error(ubuntuNetManagerLogTag, "Ignoring network start failure: %s", err.Error())
		}
	}
}

func (net ubuntuNetManager) restartNetworkArguments() []string {
	_, _, _, err := net.cmdRunner.RunCommand("ifup", "--version")
	if err != nil {
		net.logger.Error(ubuntuNetManagerLogTag, "Ignoring ifup version failure: %s", err.Error())
	}

	return []string{"-a", "--no-loopback"}
}

func (net ubuntuNetManager) writeDhcpNetworkInterfaces() error {
	interfaces, err := net.detectNetworkInterfaces()
	if err != nil {
		return bosherr.WrapError(err, "Detecting network interfaces")
	}

	buffer := bytes.NewBuffer([]byte{})
	t := template.Must(template.New("network-interfaces").Parse(ubuntuDhcpNetworkInterfacesTemplate))

	err = t.Execute(buffer, interfaces)
	if err != nil {
		return bosherr.WrapError(err, "Generating config from template")
	}

	_, err = net.fs.ConvergeFileContents("/etc/network/interfaces", buffer.Bytes())
	if err != nil {
		return bosherr.WrapError(err, "Writing to /etc/network/interfaces")
	}

	return nil
}

const ubuntuDhcpNetworkInterfacesTemplate = `# Generated by bosh-agent
auto lo
iface lo inet loopback
{{ range . }}
auto {{ . }}
iface {{ . }} inet dhcp
{{ end }}`

func (net ubuntuNetManager) detectNetworkInterfaces() ([]string, error) {
	interfaces := []string{}

	filePaths, err := net.fs.Glob("/sys/class/net/*")
	if err != nil {
		return nil, bosherr.WrapError(err, "Getting file list from /sys/class/net")
	}

	for _, filePath := range filePaths {
		exists := net.fs.FileExists(filepath.Join(filePath, "device"))
		if !exists {
			net.logger.Info(ubuntuNetManagerLogTag, "Ignoring virtual network device: %s", filePath)
			continue
		}

		interfaceName := filepath.Base(filePath)
		interfaces = append(interfaces, interfaceName)
	}

	return interfaces, nil
}
