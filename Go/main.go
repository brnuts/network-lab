package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"strconv"
	"strings"

	"github.com/melbahja/goph"
	"golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v3"
)

var (
	host         string
	user         string
	passwd       string
	port         uint
	topologyFile string
)

func init() {

	flag.StringVar(&host, "host", "localhost", "Host IP for netlab")
	flag.UintVar(&port, "port", 22, "SSH port to access Host IP for netlab")
	flag.StringVar(&user, "user", "netlab", "Username to access netlab host")
	flag.StringVar(&passwd, "pw", "netlab", "Password to access netlab host")
	flag.StringVar(&topologyFile, "topo", "topology.yaml", "Topology yaml file")

}

func (conf *ConfType) readTopologyFile(fileName string) error {
	yfile, err := ioutil.ReadFile(fileName)
	if err != nil {
		return err
	}
	if err = yaml.Unmarshal(yfile, &conf.Topology); err != nil {
		return err
	}

	return nil

}

func (conf *ConfType) connectHost(host string, port uint, user string, pw string) error {

	// Unfortunately we can't use goph.New() as the port is fixed
	// To allow port input, we need to use goph.NewConn()
	client, err := goph.NewConn(&goph.Config{
		User:     user,
		Addr:     host,
		Port:     port,
		Auth:     goph.Password(pw),
		Callback: ssh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		return err
	}

	conf.Client = client

	return nil

}

func runCommand(client *goph.Client, cmd string) error {
	out, err := client.Run(cmd)
	if err != nil {
		trimOut := strings.TrimSuffix(string(out), "\n")
		return fmt.Errorf(
			"failed to run '%s', output: %s ,error: %v", cmd, trimOut, err,
		)
	}
	return nil
}

func runCommandOut(client *goph.Client, cmd string) (string, error) {
	out, err := client.Run(cmd)
	if err != nil {
		trimOut := strings.TrimSuffix(string(out), "\n")
		return "", fmt.Errorf(
			"failed to run '%s', output: %s ,error: %v", cmd, trimOut, err,
		)
	}
	return string(out), nil
}

func appendVeth(conf *ConfType, link LinkTopologyType) error {
	DeviceA := link.Connection[0]
	DeviceB := link.Connection[1]
	// Back-to-back veths
	var veth VethPeerType
	veth.DeviceA.Device = DeviceA
	// Interface name DeviceA-DeviceB
	veth.DeviceA.InterfaceName = link.Name[0] + "-" + link.Name[1]
	ns, ok := conf.DeviceToNS[DeviceA]
	if !ok {
		return fmt.Errorf("could not find NS for device %s", DeviceA)
	}
	veth.DeviceA.NameSpace = ns
	veth.DeviceB.Device = DeviceB
	// Interface name DeviceB-DeviceA
	veth.DeviceB.InterfaceName = link.Name[1] + "-" + link.Name[0]
	ns, ok = conf.DeviceToNS[DeviceB]
	if !ok {
		return fmt.Errorf("could not find NS for device %s", DeviceB)
	}
	veth.DeviceB.NameSpace = ns

	conf.Veths = append(conf.Veths, veth)

	return nil
}

func appendBackboneVeth(conf *ConfType, link LinkTopologyType) error {

	var veth VethPeerType
	for _, device := range link.Connection {
		veth.DeviceA.Device = device
		veth.DeviceA.InterfaceName = device + "-" + link.Name[0]
		ns, ok := conf.DeviceToNS[device]
		if !ok {
			return fmt.Errorf("could not find NS for device %s", device)
		}
		veth.DeviceA.NameSpace = ns

		veth.DeviceB.Device = "host"
		veth.DeviceB.InterfaceName = link.Name[0] + "-" + device
		veth.DeviceB.NameSpace = 0

		conf.Veths = append(conf.Veths, veth)
	}

	return nil

}

func (conf *ConfType) loadVeths() error {
	conf.DeviceToNS = make(DeviceToNSType)

	for _, device := range conf.Topology.Devices {
		cmd := fmt.Sprintf(
			"docker inspect -f '{{.State.Pid}}' %s",
			device.Name,
		)
		out, err := runCommandOut(conf.Client, cmd)
		if err != nil {
			return err
		}
		trimOut := strings.TrimSuffix(out, "\n")
		ns, err := strconv.Atoi(trimOut)
		if err != nil {
			return err
		}
		conf.DeviceToNS[device.Name] = ns
	}

	for _, link := range conf.Topology.Links {
		// If greater than 2 is the backbone veths
		if len(link.Connection) == 2 {
			err := appendVeth(conf, link)
			if err != nil {
				return err
			}
		} else if len(link.Connection) > 2 {
			// This is the backbone veths
			err := appendBackboneVeth(conf, link)
			if err != nil {
				return err
			}

		} else {
			return fmt.Errorf("link with unexpected size: %s", link.Connection)
		}

	}
	return nil
}

func createPeerVeth(conf *ConfType, v VethPeerType) error {
	// Add veth peer
	cmd := vethAddPeer(v.DeviceA.InterfaceName, v.DeviceB.InterfaceName)
	if err := runCommand(conf.Client, cmd); err != nil {
		return err
	}
	// Move interface to namespace on DeviceA
	cmd = vethSetNameSpace(v.DeviceA.InterfaceName, v.DeviceA.NameSpace)
	if err := runCommand(conf.Client, cmd); err != nil {
		return err
	}
	// If greater than 0, move to namespace on DeviceB
	// Otherwise just ignore, as the default namespace is on host
	if v.DeviceB.NameSpace > 0 {
		cmd = vethSetNameSpace(v.DeviceB.InterfaceName, v.DeviceB.NameSpace)
		if err := runCommand(conf.Client, cmd); err != nil {
			return err
		}
	}
	// Bringing Device A interfaces UP
	cmd = vethInterfaceUp(v.DeviceA.Device, v.DeviceA.InterfaceName)
	if err := runCommand(conf.Client, cmd); err != nil {
		return err
	}
	// Bringing Device B interface UP
	cmd = vethInterfaceUp(v.DeviceB.Device, v.DeviceB.InterfaceName)
	if err := runCommand(conf.Client, cmd); err != nil {
		return err
	}

	return nil

}

func (conf *ConfType) createVeths() error {
	for _, veth := range conf.Veths {
		if err := createPeerVeth(conf, veth); err != nil {
			return err
		}
	}
	return nil
}

func (conf *ConfType) addVethsToBackbone() error {
	// create bridge name backbone

	cmd := "sudo ip link add name backbone type bridge"
	if err := runCommand(conf.Client, cmd); err != nil {
		return err
	}
	cmd = "sudo ip link set backbone up"
	if err := runCommand(conf.Client, cmd); err != nil {
		return err
	}
	for _, veth := range conf.Veths {
		if veth.DeviceB.NameSpace == 0 {
			cmd := fmt.Sprintf(
				"sudo ip link set %s master backbone",
				veth.DeviceB.InterfaceName,
			)
			if err := runCommand(conf.Client, cmd); err != nil {
				return err
			}
		}
	}

	return nil

}

func main() {

	flag.Parse()

	var conf = new(ConfType)

	log.Printf("reading %s file", topologyFile)
	if err := conf.readTopologyFile(topologyFile); err != nil {
		log.Fatalf("read topology failed: %v", err)
	}

	log.Printf("connecting via SSH to %s@%s", user, host)
	if err := conf.connectHost(host, port, user, passwd); err != nil {
		log.Fatalf("failed to connect to host: %v", err)
	}
	defer conf.Client.Close()

	log.Printf("loading veth information")
	if err := conf.loadVeths(); err != nil {
		log.Fatalf("failed to load veths: %v", err)
	}

	log.Printf("connecting devices with %d veths", len(conf.Veths))
	if err := conf.createVeths(); err != nil {
		log.Fatalf("failed to create veths: %v", err)
	}

	log.Printf("creating bridge and adding veth backbones")
	if err := conf.addVethsToBackbone(); err != nil {
		log.Fatalf("failed to add veths to backbone: %v", err)
	}

	log.Print("all done successfully")
}
