package kvm

import (
	"archive/tar"
	"bytes"
	//"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"
	"syscall"

	libvirt "github.com/libvirt/libvirt-go"

	"github.com/rancher/machine/libmachine/drivers"
	"github.com/rancher/machine/libmachine/log"
	"github.com/rancher/machine/libmachine/mcnflag"
	"github.com/rancher/machine/libmachine/mcnutils"
	"github.com/rancher/machine/libmachine/ssh"
	"github.com/rancher/machine/libmachine/state"
)

const (
	connectionString   = "qemu:///system"
	privateNetworkName = "docker-machines"
	isoFilename        = "boot2docker.iso"
	dnsmasqLeases      = "/var/lib/libvirt/dnsmasq/%s.leases"
	dnsmasqStatus      = "/var/lib/libvirt/dnsmasq/%s.status"
	defaultSSHUser     = "docker"

	domainXMLTemplate = `<domain type='kvm'>
  <name>{{.MachineName}}</name> <memory unit='M'>{{.Memory}}</memory>
  <vcpu>{{.CPU}}</vcpu>
  <features><acpi/><apic/><pae/></features>
  <cpu mode='host-passthrough'></cpu>
  <os>
    <type>hvm</type>
    <boot dev='cdrom'/>
    <boot dev='hd'/>
    <bootmenu enable='no'/>
  </os>
  <devices>
    <disk type='file' device='cdrom'>
      <source file='{{.ISO}}'/>
      <target dev='hdc' bus='ide'/>
      <readonly/>
    </disk>
    <disk type='file' device='disk'>
      <driver name='qemu' type='raw' cache='{{.CacheMode}}' io='{{.IOMode}}' />
      <source file='{{.DiskPath}}'/>
      <target dev='hda' bus='ide'/>
    </disk>
    <graphics type='vnc' autoport='yes' websocket='-1' listen='127.0.0.1'>
      <listen type='address' address='127.0.0.1'/>
    </graphics>
    <interface type='network'>
	  <source network='{{.Network}}'/>
	  <model type='virtio'/>
    </interface>
    <interface type='network'>
	  <source network='{{.PrivateNetwork}}'/>
	  <model type='virtio'/>
    </interface>
  </devices>
</domain>`
	networkXML = `<network>
  <name>%s</name>
  <ip address='%s' netmask='%s'>
    <dhcp>
      <range start='%s' end='%s'/>
    </dhcp>
  </ip>
</network>`
)

type Driver struct {
	*drivers.BaseDriver

	Memory           int
	DiskSize         int
	Timeout         int
	CPU              int
	Network          string
	PrivateNetwork   string
	ISO              string
	Boot2DockerURL   string
	CaCertPath       string
	PrivateKeyPath   string
	DiskPath         string
	CacheMode        string
	IOMode           string
	LibvirtdHostPath string
	ConnectionString string
	conn             *libvirt.Connect
	VM               *libvirt.Domain
	vmLoaded         bool
}

func (d *Driver) GetCreateFlags() []mcnflag.Flag {
	return []mcnflag.Flag{
		mcnflag.IntFlag{
			Name:  "kvm-memory",
			Usage: "Size of memory for host in MB",
			Value: 1024,
		},
		mcnflag.IntFlag{
			Name:  "kvm-disk-size",
			Usage: "Size of disk for host in MB",
			Value: 20000,
		},
		mcnflag.IntFlag{
			Name:  "kvm-timeout",
			Usage: "Number of Seconds to wait for VM to come up",
			Value: 90,
		},
		mcnflag.IntFlag{
			Name:  "kvm-cpu-count",
			Usage: "Number of CPUs",
			Value: 1,
		},
		mcnflag.StringFlag{
			Name:  "kvm-network",
			Usage: "Name of network to connect to",
			Value: "default",
		},
		mcnflag.StringFlag{
			EnvVar: "KVM_BOOT2DOCKER_URL",
			Name:   "kvm-boot2docker-url",
			Usage:  "The URL of the boot2docker image. Defaults to the latest available version",
			Value:  "",
		},
		mcnflag.StringFlag{
			Name:  "kvm-cache-mode",
			Usage: "Disk cache mode: default, none, writethrough, writeback, directsync, or unsafe",
			Value: "default",
		},
		mcnflag.StringFlag{
			Name:  "kvm-io-mode",
			Usage: "Disk IO mode: threads, native",
			Value: "threads",
		},
		mcnflag.StringFlag{
			EnvVar: "KVM_SSH_USER",
			Name:   "kvm-ssh-user",
			Usage:  "SSH username",
			Value:  defaultSSHUser,
		},
		mcnflag.StringFlag{
			EnvVar: "KVM_LIBVIRTD_HOST_PATH",
			Name:   "kvm-libvirtd-host-path",
			Usage:  "Location of iso and disk on the libvirtd host",
			Value:  "",
		},
		mcnflag.StringFlag{
			EnvVar: "KVM_LIBVIRTD_CONNECTION_STRING",
			Name:   "kvm-libvirtd-connection-string",
			Usage:  "Libvirtd connection string",
			Value:  connectionString,
		},
	}
}

func (d *Driver) GetMachineName() string {
	return d.MachineName
}

func (d *Driver) GetSSHHostname() (string, error) {
	return d.GetIP()
}

func (d *Driver) GetSSHKeyPath() string {
	return d.ResolveStorePath("id_rsa")
}

func (d *Driver) GetSSHPort() (int, error) {
	if d.SSHPort == 0 {
		d.SSHPort = 22
	}

	return d.SSHPort, nil
}

func (d *Driver) GetSSHUsername() string {
	if d.SSHUser == "" {
		d.SSHUser = "docker"
	}

	return d.SSHUser
}

func (d *Driver) DriverName() string {
	return "kvm"
}

func (d *Driver) SetConfigFromFlags(flags drivers.DriverOptions) error {
	log.Debugf("SetConfigFromFlags called")
	d.Memory = flags.Int("kvm-memory")
	d.DiskSize = flags.Int("kvm-disk-size")
	d.Timeout = flags.Int("kvm-timeout")	
	d.CPU = flags.Int("kvm-cpu-count")
	d.Network = flags.String("kvm-network")
	d.Boot2DockerURL = flags.String("kvm-boot2docker-url")
	d.CacheMode = flags.String("kvm-cache-mode")
	d.IOMode = flags.String("kvm-io-mode")
	d.LibvirtdHostPath = flags.String("kvm-libvirtd-host-path")
	d.ConnectionString = flags.String("kvm-libvirtd-connection-string")
	d.SwarmMaster = flags.Bool("swarm-master")
	d.SwarmHost = flags.String("swarm-host")
	d.SwarmDiscovery = flags.String("swarm-discovery")
	d.ISO = d.ResolveStorePath(isoFilename)
	d.SSHUser = flags.String("kvm-ssh-user")
	d.SSHPort = 22
	d.DiskPath = d.ResolveStorePath(fmt.Sprintf("%s.img", d.MachineName))
	return nil
}

func (d *Driver) GetURL() (string, error) {
	log.Debugf("GetURL called")
	ip, err := d.GetIP()
	if err != nil {
		log.Warnf("Failed to get IP: %s", err)
		return "", err
	}
	if ip == "" {
		return "", nil
	}
	return fmt.Sprintf("tcp://%s:2376", ip), nil // TODO - don't hardcode the port!
}

func (d *Driver) getConn() (*libvirt.Connect, error) {
	if d.conn == nil {
		conn, err := libvirt.NewConnect(d.ConnectionString)
		if err != nil {
			log.Errorf("Failed to connect to libvirt: %s", err)
			return &libvirt.Connect{}, errors.New("Unable to connect to kvm driver, did you add yourself to the libvirtd group?")
		}
		d.conn = conn
	}
	return d.conn, nil
}

// Create, or verify the private network is properly configured
func (d *Driver) validatePrivateNetwork() error {
	log.Debug("Validating private network")
	conn, err := d.getConn()
	if err != nil {
		return err
	}
	network, err := conn.LookupNetworkByName(d.PrivateNetwork)
	if err == nil {
		xmldoc, err := network.GetXMLDesc(0)
		if err != nil {
			return err
		}
		/* XML structure:
		<network>
		    ...
		    <ip address='a.b.c.d' netmask='255.255.255.0'>
		        <dhcp>
		            <range start='a.b.c.d' end='w.x.y.z'/>
		        </dhcp>
		*/
		type Ip struct {
			Address string `xml:"address,attr"`
			Netmask string `xml:"netmask,attr"`
		}
		type Network struct {
			Ip Ip `xml:"ip"`
		}

		var nw Network
		err = xml.Unmarshal([]byte(xmldoc), &nw)
		if err != nil {
			return err
		}

		if nw.Ip.Address == "" {
			return fmt.Errorf("%s network doesn't have DHCP configured properly", d.PrivateNetwork)
		}
		// Corner case, but might happen...
		if active, err := network.IsActive(); !active {
			log.Debugf("Reactivating private network: %s", err)
			err = network.Create()
			if err != nil {
				log.Warnf("Failed to Start network: %s", err)
				return err
			}
		}
		return nil
	}
	// TODO - try a couple pre-defined networks and look for conflicts before
	//        settling on one
	xml := fmt.Sprintf(networkXML, d.PrivateNetwork,
		"192.168.42.1",
		"255.255.255.0",
		"192.168.42.2",
		"192.168.42.254")

	network, err = conn.NetworkDefineXML(xml)
	if err != nil {
		log.Errorf("Failed to create private network: %s", err)
		return nil
	}
	err = network.SetAutostart(true)
	if err != nil {
		log.Warnf("Failed to set private network to autostart: %s", err)
	}
	err = network.Create()
	if err != nil {
		log.Warnf("Failed to Start network: %s", err)
		return err
	}
	return nil
}

func (d *Driver) validateNetwork(name string) error {
	log.Debugf("Validating network %s", name)
	conn, err := d.getConn()
	if err != nil {
		return err
	}
	_, err = conn.LookupNetworkByName(name)
	if err != nil {
		log.Errorf("Unable to locate network %s", name)
		return err
	}
	return nil
}

func (d *Driver) PreCreateCheck() error {
	conn, err := d.getConn()
	if err != nil {
		return err
	}

	// TODO We could look at conn.GetCapabilities()
	// parse the XML, and look for kvm
	log.Debug("About to check libvirt version")

	// TODO might want to check minimum version
	_, err = conn.GetLibVersion()
	if err != nil {
		log.Warnf("Unable to get libvirt version")
		return err
	}
	err = d.validatePrivateNetwork()
	if err != nil {
		return err
	}
	err = d.validateNetwork(d.Network)
	if err != nil {
		return err
	}
	// Others...?
	return nil
}
func (d *Driver) publicSSHKeyPath() string {
	return d.GetSSHKeyPath() + ".pub"
}

func (d *Driver) Create() error {

	//TODO(r2d4): rewrite this, not using b2dutils
	b2dutils := mcnutils.NewB2dUtils(d.StorePath)
	if err := b2dutils.CopyIsoToMachineDir(d.Boot2DockerURL, d.MachineName); err != nil {
		return err
	}

	log.Info("Creating ssh key...")
	if err := ssh.GenerateSSHKey(d.GetSSHKeyPath()); err != nil {
		return err
	}

	log.Info("Creating raw disk image...")
	//TODO: REVIEW THIS LINE
	//diskPath := GetDiskPath(d)
	if _, err := os.Stat(d.DiskPath); os.IsNotExist(err) {
		if err := createRawDiskImage( d.publicSSHKeyPath(), d.DiskPath, d.DiskSize); err != nil {
			return err
		}
		if err := fixPermissions(d.ResolveStorePath(".")); err != nil {
			return err
		}
	}
	log.Info("Testing ISO Path: %s",d.ISO)
	log.Info("Testing DISK Path: %s",d.DiskPath)
	log.Info("Testing Local Path: %s",d.ResolveStorePath("."))
	log.Debugf("Defining VM...")
	prepareKVMDiskAndISO(d.DiskPath,d.ISO,d.MachineName)
	if d.LibvirtdHostPath != "" {
		
		d.ISO = fmt.Sprintf("%s/%s_persistant/boot2docker.iso",d.LibvirtdHostPath, d.MachineName)
		d.DiskPath = fmt.Sprintf("%s/%s_persistant/%s.img",d.LibvirtdHostPath, d.MachineName,d.MachineName)
	}
	tmpl, err := template.New("domain").Parse(domainXMLTemplate)
	if err != nil {
		return err
	}
	var xml bytes.Buffer
	err = tmpl.Execute(&xml, d)
	if err != nil {
		return err
	}

	conn, err := d.getConn()
	if err != nil {
		return err
	}
	vm, err := conn.DomainDefineXML(xml.String())
	if err != nil {
		log.Warnf("Failed to create the VM: %s", err)
		return err
	}
	d.VM = vm
	d.vmLoaded = true
	//TODO: (HACK) FIX FILE PERMISSION ISSUE WITH LONG TERM FIX 
	err = os.Chmod(fmt.Sprintf("/management-state/node/nodes/%s",d.MachineName), 0o777)
	err = os.Chmod(fmt.Sprintf("/management-state/node/nodes/%s/machines",d.MachineName), 0o777)
	err = os.Chmod(fmt.Sprintf("/management-state/node/nodes/%s/machines/%s",d.MachineName,d.MachineName), 0o777)
	err = os.Chmod(fmt.Sprintf("/management-state/node/nodes/%s/machines/%s/config.json",d.MachineName,d.MachineName), 0o777)
	err = os.Chmod(fmt.Sprintf("/management-state/node/nodes/%s/machines/%s/id_rsa.pub",d.MachineName,d.MachineName), 0o400)
	err = os.Chmod(fmt.Sprintf("/management-state/node/nodes/%s/machines/%s/id_rsa",d.MachineName,d.MachineName), 0o400)

	if err != nil {
		log.Warnf("Failed to open file permssions: %s", err)
	}
	return d.Start()
}

func prepareKVMDiskAndISO(diskPath string, isoPath string, machineName string) error {
	err := os.Mkdir(fmt.Sprintf("/management-state/node/nodes/%s_persistant",machineName), 0755)
	if err != nil {
		return err
	}
	diskNewPath := fmt.Sprintf("/management-state/node/nodes/%s_persistant/%s.img",machineName,machineName)
	isoNewPath := fmt.Sprintf("/management-state/node/nodes/%s_persistant/boot2docker.iso",machineName)
	err = os.Rename(diskPath, diskNewPath)
	if err != nil {
		return err
	}
	err = os.Rename(isoPath, isoNewPath)
	if err != nil {
		return err
	}
	return nil
 }


func createRawDiskImage(sshKeyPath, diskPath string, diskSizeMb int) error {
	//tarBuf, err := mcnutils.MakeDiskImage(sshKeyPath)
	tarBuf, err := mcnutils.MakeDiskImage(sshKeyPath)
	if err != nil {
		return err
	}

	file, err := os.OpenFile(diskPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer file.Close()
	file.Seek(0, os.SEEK_SET)

	if _, err := file.Write(tarBuf.Bytes()); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}

	if err := os.Truncate(diskPath, int64(diskSizeMb*1000000)); err != nil {
		return err
	}
	return nil
}

func (d *Driver) Start() error {
	log.Debugf("Starting VM %s", d.MachineName)
	if err := d.validateVMRef(); err != nil {
		return err
	}
	if err := d.VM.Create(); err != nil {
		log.Warnf("Failed to start: %s", err)
		return err
	}

	// They wont start immediately
	time.Sleep(time.Duration(d.Timeout) * time.Second)

	for i := 0; i < 350; i++ {
		time.Sleep(time.Second)
		ip, _ := d.GetIP()
		if ip != "" {
			// Add a second to let things settle
			time.Sleep(time.Second)
			return nil
		}
		log.Debugf("Waiting for the VM to come up... %d", i)
	}
	log.Warnf("Unable to determine VM's IP address, did it fail to boot?")
	return nil
}

func (d *Driver) Stop() error {
	log.Debugf("Stopping VM %s", d.MachineName)
	if err := d.validateVMRef(); err != nil {
		return err
	}
	s, err := d.GetState()
	if err != nil {
		return err
	}

	if s != state.Stopped {
		err := d.VM.Shutdown()
		if err != nil {
			log.Warnf("Failed to gracefully shutdown VM")
			return err
		}
		for i := 0; i < 90; i++ {
			time.Sleep(time.Second)
			s, _ := d.GetState()
			log.Debugf("VM state: %s", s)
			if s == state.Stopped {
				return nil
			}
		}
		return errors.New("VM Failed to gracefully shutdown, try the kill command")
	}
	return nil
}

func (d *Driver) Remove() error {
	log.Debugf("Removing VM %s", d.MachineName)
	if err := d.validateVMRef(); err != nil {
		return err
	}
	err := os.RemoveAll(fmt.Sprintf("/management-state/node/nodes/%s_persistant",d.MachineName))
    if err != nil {
		return err
    }
	// Note: If we switch to qcow disks instead of raw the user
	//       could take a snapshot.  If you do, then Undefine
	//       will fail unless we nuke the snapshots first
	d.VM.Destroy() // Ignore errors
	return d.VM.Undefine()
}

func (d *Driver) Restart() error {
	log.Debugf("Restarting VM %s", d.MachineName)
	if err := d.Stop(); err != nil {
		return err
	}
	return d.Start()
}

func (d *Driver) Kill() error {
	log.Debugf("Killing VM %s", d.MachineName)
	if err := d.validateVMRef(); err != nil {
		return err
	}
	return d.VM.Destroy()
}

func (d *Driver) GetState() (state.State, error) {
	log.Debugf("Getting current state...")
	if err := d.validateVMRef(); err != nil {
		return state.None, err
	}
	virState, _, err := d.VM.GetState()
	if err != nil {
		return state.None, err
	}
	switch virState {
	case libvirt.DOMAIN_NOSTATE:
		return state.None, nil
	case libvirt.DOMAIN_RUNNING:
		return state.Running, nil
	case libvirt.DOMAIN_BLOCKED:
		// TODO - Not really correct, but does it matter?
		return state.Error, nil
	case libvirt.DOMAIN_PAUSED:
		return state.Paused, nil
	case libvirt.DOMAIN_SHUTDOWN:
		return state.Stopped, nil
	case libvirt.DOMAIN_CRASHED:
		return state.Error, nil
	case libvirt.DOMAIN_PMSUSPENDED:
		return state.Saved, nil
	case libvirt.DOMAIN_SHUTOFF:
		return state.Stopped, nil
	}
	return state.None, nil
}

func (d *Driver) validateVMRef() error {
	if !d.vmLoaded {
		log.Debugf("Fetching VM...")
		conn, err := d.getConn()
		if err != nil {
			return err
		}
		vm, err := conn.LookupDomainByName(d.MachineName)
		if err != nil {
			log.Warnf("Failed to fetch machine")
		} else {
			d.VM = vm
			d.vmLoaded = true
		}
	}
	return nil
}

// This implementation is specific to default networking in libvirt
// with dnsmasq
func (d *Driver) getMAC() (string, error) {
	if err := d.validateVMRef(); err != nil {
		return "", err
	}
	xmldoc, err := d.VM.GetXMLDesc(0)
	if err != nil {
		return "", err
	}
	/* XML structure:
	<domain>
	    ...
	    <devices>
	        ...
	        <interface type='network'>
	            ...
	            <mac address='52:54:00:d2:3f:ba'/>
	            ...
	        </interface>
	        ...
	*/
	type Mac struct {
		Address string `xml:"address,attr"`
	}
	type Source struct {
		Network string `xml:"network,attr"`
	}
	type Interface struct {
		Type   string `xml:"type,attr"`
		Mac    Mac    `xml:"mac"`
		Source Source `xml:"source"`
	}
	type Devices struct {
		Interfaces []Interface `xml:"interface"`
	}
	type Domain struct {
		Devices Devices `xml:"devices"`
	}

	var dom Domain
	err = xml.Unmarshal([]byte(xmldoc), &dom)
	if err != nil {
		return "", err
	}
	// Always assume the second interface is the one we want
	if len(dom.Devices.Interfaces) < 2 {
		return "", fmt.Errorf("VM doesn't have enough network interfaces.  Expected at least 2, found %d",
			len(dom.Devices.Interfaces))
	}
	return dom.Devices.Interfaces[1].Mac.Address, nil
}

func (d *Driver) getIPByMACFromLeaseFile(mac string) (string, error) {
	leaseFile := fmt.Sprintf(dnsmasqLeases, d.PrivateNetwork)
	data, err := ioutil.ReadFile(leaseFile)
	if err != nil {
		log.Debugf("Failed to retrieve dnsmasq leases from %s", leaseFile)
		return "", err
	}
	for lineNum, line := range strings.Split(string(data), "\n") {
		if len(line) == 0 {
			continue
		}
		entries := strings.Split(line, " ")
		if len(entries) < 3 {
			log.Warnf("Malformed dnsmasq line %d", lineNum+1)
			return "", errors.New("Malformed dnsmasq file")
		}
		if strings.ToLower(entries[1]) == strings.ToLower(mac) {
			log.Debugf("IP address: %s", entries[2])
			return entries[2], nil
		}
	}
	return "", nil
}

func (d *Driver) getIPByMacFromSettings(mac string) (string, error) {
	conn, err := d.getConn()
	if err != nil {
		return "", err
	}
	network, err := conn.LookupNetworkByName(d.PrivateNetwork)
	networkName, err := network.GetName()
	if err != nil {
		log.Warnf("Failed to find network: %s", err)
		return "", err
	}
	log.Info("Pulled network from libvirt: %s", networkName)
	dhcpLeases, err := network.GetDHCPLeases()
	if err != nil {
		log.Warnf("Failed to get DHCP Leases: %s", err)
		return "", err
	}
	ipAddr := ""

	for _, l := range dhcpLeases {
		if mac == l.Mac {
			ipAddr = l.IPaddr
		}	
	}

	return ipAddr, nil
}

func (d *Driver) GetIP() (string, error) {
	log.Debugf("GetIP called for %s", d.MachineName)
	mac, err := d.getMAC()
	if err != nil {
		return "", err
	}
	/*
	 * TODO - Figure out what version of libvirt changed behavior and
	 *        be smarter about selecting which algorithm to use
	 */
	ip, err := d.getIPByMACFromLeaseFile(mac)
	if ip == "" {
		ip, err = d.getIPByMacFromSettings(mac)
	}
	if ip != "" {
	 	d.IPAddress = ip
	}
	//log.Debugf("Unable to locate IP address for MAC %s", mac)
	return ip, err
}

// Make a boot2docker VM disk image.
func (d *Driver) generateDiskImage(size int) error {
	log.Debugf("Creating %d MB hard disk image...", size)

	magicString := "boot2docker, please format-me"

	buf := new(bytes.Buffer)
	tw := tar.NewWriter(buf)

	// magicString first so the automount script knows to format the disk
	file := &tar.Header{Name: magicString, Size: int64(len(magicString))}
	if err := tw.WriteHeader(file); err != nil {
		return err
	}
	if _, err := tw.Write([]byte(magicString)); err != nil {
		return err
	}
	// .ssh/key.pub => authorized_keys
	file = &tar.Header{Name: ".ssh", Typeflag: tar.TypeDir, Mode: 0700}
	if err := tw.WriteHeader(file); err != nil {
		return err
	}
	pubKey, err := ioutil.ReadFile(d.publicSSHKeyPath())
	if err != nil {
		return err
	}
	file = &tar.Header{Name: ".ssh/authorized_keys", Size: int64(len(pubKey)), Mode: 0644}
	if err := tw.WriteHeader(file); err != nil {
		return err
	}
	if _, err := tw.Write([]byte(pubKey)); err != nil {
		return err
	}
	file = &tar.Header{Name: ".ssh/authorized_keys2", Size: int64(len(pubKey)), Mode: 0644}
	if err := tw.WriteHeader(file); err != nil {
		return err
	}
	if _, err := tw.Write([]byte(pubKey)); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	raw := bytes.NewReader(buf.Bytes())
	return createDiskImage(d.DiskPath, size, raw)
}

// createDiskImage makes a disk image at dest with the given size in MB. If r is
// not nil, it will be read as a raw disk image to convert from.
func createDiskImage(dest string, size int, r io.Reader) error {
	// Convert a raw image from stdin to the dest VMDK image.
	sizeBytes := int64(size) << 20 // usually won't fit in 32-bit int (max 2GB)
	f, err := os.Create(dest)
	if err != nil {
		return err
	}

	_, err = io.Copy(f, r)
	if err != nil {
		return err
	}
	// Rely on seeking to create a sparse raw file for qemu
	f.Seek(sizeBytes-1, 0)
	f.Write([]byte{0})
	return f.Close()
}

func NewDriver(hostName, storePath string) drivers.Driver {
	return &Driver{
		PrivateNetwork: privateNetworkName,
		BaseDriver: &drivers.BaseDriver{
			SSHUser:     defaultSSHUser,
			MachineName: hostName,
			StorePath:   storePath,
		},
	}
}
func fixPermissions(path string) error {
	os.Chown(path, syscall.Getuid(), syscall.Getegid())
	files, _ := ioutil.ReadDir(path)
	for _, f := range files {
		fp := filepath.Join(path, f.Name())
		if err := os.Chown(fp, syscall.Getuid(), syscall.Getegid()); err != nil {
			return err
		}
	}
	return nil
}
