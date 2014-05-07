package net

import (
	"bytes"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
	"time"

	bosherr "bosh/errors"
	boshlog "bosh/logger"
	boshsettings "bosh/settings"
	boshsys "bosh/system"
)

const ubuntuNetManagerLogTag = "ubuntuNetManager"

var (
	ifupVersion07Regex = regexp.MustCompile(`ifup version 0\.7`)
)

type ubuntuNetManager struct {
	arpWaitInterval time.Duration
	cmdRunner       boshsys.CmdRunner
	fs              boshsys.FileSystem
	logger          boshlog.Logger
}

func NewUbuntuNetManager(
	fs boshsys.FileSystem,
	cmdRunner boshsys.CmdRunner,
	arpWaitInterval time.Duration,
	logger boshlog.Logger,
) ubuntuNetManager {
	return ubuntuNetManager{
		arpWaitInterval: arpWaitInterval,
		cmdRunner:       cmdRunner,
		fs:              fs,
		logger:          logger,
	}
}

func (net ubuntuNetManager) getDNSServers(networks boshsettings.Networks) []string {
	var dnsServers []string
	dnsNetwork, found := networks.DefaultNetworkFor("dns")
	if found {
		for i := len(dnsNetwork.DNS) - 1; i >= 0; i-- {
			dnsServers = append(dnsServers, dnsNetwork.DNS[i])
		}
	}
	return dnsServers
}

func (net ubuntuNetManager) SetupDhcp(networks boshsettings.Networks) error {
	dnsServers := net.getDNSServers(networks)
	dnsServersList := strings.Join(dnsServers, ", ")
	buffer := bytes.NewBuffer([]byte{})
	t := template.Must(template.New("dhcp-config").Parse(ubuntuDHCPConfigTemplate))

	err := t.Execute(buffer, dnsServersList)
	if err != nil {
		return bosherr.WrapError(err, "Generating config from template")
	}

	dhclientConfigFile := net.dhclientConfigFile()
	written, err := net.fs.ConvergeFileContents(dhclientConfigFile, buffer.Bytes())
	if err != nil {
		return bosherr.WrapError(err, "Writing to %s", dhclientConfigFile)
	}

	if written {
		args := net.restartNetworkArguments()

		_, _, _, err := net.cmdRunner.RunCommand("ifdown", args...)
		if err != nil {
			net.logger.Info(ubuntuNetManagerLogTag, "Ignoring ifdown failure: %#v", err)
		}

		_, _, _, err = net.cmdRunner.RunCommand("ifup", args...)
		if err != nil {
			net.logger.Info(ubuntuNetManagerLogTag, "Ignoring ifup failure: %#v", err)
		}
	}

	return nil
}

// DHCP Config file - /etc/dhcp3/dhclient.conf
// Ubuntu 14.04 accepts several DNS as a list in a single prepend directive
const ubuntuDHCPConfigTemplate = `# Generated by bosh-agent

option rfc3442-classless-static-routes code 121 = array of unsigned integer 8;

send host-name "<hostname>";

request subnet-mask, broadcast-address, time-offset, routers,
	domain-name, domain-name-servers, domain-search, host-name,
	netbios-name-servers, netbios-scope, interface-mtu,
	rfc3442-classless-static-routes, ntp-servers;

prepend domain-name-servers {{ . }};
`

func (net ubuntuNetManager) SetupManualNetworking(networks boshsettings.Networks, errCh chan error) error {
	modifiedNetworks, written, err := net.writeNetworkInterfaces(networks)
	if err != nil {
		return bosherr.WrapError(err, "Writing network interfaces")
	}

	if written {
		net.restartNetworkingInterfaces(modifiedNetworks)
	}

	err = net.writeResolvConf(networks)
	if err != nil {
		return bosherr.WrapError(err, "Writing resolv.conf")
	}

	go net.gratuitiousArp(modifiedNetworks, errCh)

	return nil
}

func (net ubuntuNetManager) GetDefaultNetwork() (boshsettings.Network, error) {
	return boshsettings.Network{}, nil
}

func (net ubuntuNetManager) gratuitiousArp(networks []customNetwork, errCh chan error) {
	for i := 0; i < 6; i++ {
		for _, network := range networks {
			for !net.fs.FileExists(filepath.Join("/sys/class/net", network.Interface)) {
				time.Sleep(100 * time.Millisecond)
			}

			_, _, _, err := net.cmdRunner.RunCommand("arping", "-c", "1", "-U", "-I", network.Interface, network.IP)
			if err != nil {
				net.logger.Info(ubuntuNetManagerLogTag, "Ignoring arping failure: %#v", err)
			}

			time.Sleep(net.arpWaitInterval)
		}
	}

	if errCh != nil {
		errCh <- nil
	}
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

	buffer := bytes.NewBuffer([]byte{})
	t := template.Must(template.New("network-interfaces").Parse(ubuntuNetworkInterfacesTemplate))

	err = t.Execute(buffer, modifiedNetworks)
	if err != nil {
		return modifiedNetworks, false, bosherr.WrapError(err, "Generating config from template")
	}

	written, err := net.fs.ConvergeFileContents("/etc/network/interfaces", buffer.Bytes())
	if err != nil {
		return modifiedNetworks, false, bosherr.WrapError(err, "Writing to /etc/network/interfaces")
	}

	return modifiedNetworks, written, nil
}

const ubuntuNetworkInterfacesTemplate = `# Generated by bosh-agent
auto lo
iface lo inet loopback
{{ range . }}
auto {{ .Interface }}
iface {{ .Interface }} inet static
    address {{ .IP }}
    network {{ .NetworkIP }}
    netmask {{ .Netmask }}
    broadcast {{ .Broadcast }}
{{ if .HasDefaultGateway }}    gateway {{ .Gateway }}{{ end }}{{ end }}`

func (net ubuntuNetManager) writeResolvConf(networks boshsettings.Networks) error {
	buffer := bytes.NewBuffer([]byte{})
	t := template.Must(template.New("resolv-conf").Parse(ubuntuResolvConfTemplate))

	dnsServers := net.getDNSServers(networks)
	dnsServersArg := dnsConfigArg{dnsServers}
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
		_, _, _, err := net.cmdRunner.RunCommand("service", "network-interface", "stop", "INTERFACE="+network.Interface)
		if err != nil {
			net.logger.Info(ubuntuNetManagerLogTag, "Ignoring network stop failure: %#v", err)
		}

		_, _, _, err = net.cmdRunner.RunCommand("service", "network-interface", "start", "INTERFACE="+network.Interface)
		if err != nil {
			net.logger.Info(ubuntuNetManagerLogTag, "Ignoring network start failure: %#v", err)
		}
	}
}

func (net ubuntuNetManager) dhclientConfigFile() string {
	if net.cmdRunner.CommandExists("dhclient3") {
		// Using dhclient3
		return "/etc/dhcp3/dhclient.conf"
	}

	return "/etc/dhcp/dhclient.conf"
}

func (net ubuntuNetManager) restartNetworkArguments() []string {
	stdout, _, _, err := net.cmdRunner.RunCommand("ifup", "--version")
	if err != nil {
		net.logger.Info(ubuntuNetManagerLogTag, "Ignoring ifup version failure: %#v", err)
	}

	// Check if command accepts --no-loopback argument
	// --exclude does not work with ifup > 0.7 which comes in Ubuntu 14.04
	if ifupVersion07Regex.MatchString(stdout) {
		return []string{"-a", "--no-loopback"}
	}

	return []string{"-a", "--exclude=lo"}
}
