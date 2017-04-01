package machines

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"io/ioutil"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"text/template"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/docker/docker/client"
)

var (
	VirshDiskDir = "/e2e"        // Default
	VirshOS      = "ubuntu16.04" // Default
)

const (
	domainXMLTemplate = `<domain type='kvm'>
  <name>{{.MachineName}}</name> <memory unit='M'>{{.Memory}}</memory>
  <vcpu>{{.CPUCount}}</vcpu>
  <features><acpi/><apic/><pae/></features>
  <cpu mode='host-passthrough'></cpu>
  <os>
    <type>hvm</type>
    <boot dev='hd'/>
    <bootmenu enable='no'/>
  </os>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2' cache='unsafe' io='threads' />
      <source file='{{.DiskPath}}'/>
      <target dev='vda' bus='virtio'/>
    </disk>
    <graphics type='vnc' autoport='yes' listen='127.0.0.1'>
      <listen type='address' address='127.0.0.1'/>
    </graphics>
    <interface type='network'>
      <source network='default'/>
      <model type='virtio'/>
    </interface>
  </devices>
</domain>`
)

type VirshMachine struct {
	MachineName string
	dockerHost  string
	tlsConfig   *tls.Config
	sshKeyPath  string
	sshUser     string
	ip          string // Cache so we don't have to look it up so much
	internalip  string // Cache so we don't have to look it up so much
	BaseDisk    string
	DiskPath    string
	CPUCount    int
	Memory      int
}

func init() {
	VirshDiskDir = os.Getenv("VIRSH_DISK_DIR")
	baseOS := os.Getenv("VIRSH_OS")
	if baseOS != "" {
		VirshOS = baseOS
	}
}

func getActiveMachines() []string {
	cmd := exec.Command("virsh", "-q", "list")
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Info("Failed to get list - assuming no VMs: %s", err)
	}
	nameRegex := regexp.MustCompile(`\s+(\S+)\s+running`)
	machines := []string{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		matches := nameRegex.FindStringSubmatch(line)
		if len(matches) > 0 {
			machines = append(machines, matches[1])
		}
	}
	return machines
}

// Generate a new machine using docker-machine CLI
func NewVirshMachines(linuxCount, windowsCount int) ([]Machine, []Machine, error) {
	if VirshDiskDir == "" {
		return nil, nil, fmt.Errorf("To use the vrish driver, you must set VIRSH_DISK_DIR to point to where your base OS disks and ssh key live")
	}

	if windowsCount != 0 {
		return nil, nil, fmt.Errorf("Windows not yet supported for virsh")
	}

	baseOS := filepath.Join(VirshDiskDir, VirshOS+".qcow2")

	if _, err := os.Stat(baseOS); err != nil {
		return nil, nil, fmt.Errorf("Unable to locate %s: %s", baseOS, err)
	}

	timer := time.NewTimer(60 * time.Minute) // TODO - make configurable
	errChan := make(chan error)
	resChan := make(chan []Machine)

	go func() {
		log.Debugf("Attempting %s machine creation for %d nodes", VirshOS, linuxCount)
		id, _ := rand.Int(rand.Reader, big.NewInt(0xffffff))
		machines := []*VirshMachine{}
		for index := 0; index < linuxCount; index++ {
			m := &VirshMachine{
				MachineName: fmt.Sprintf("%s-%X-%d", NamePrefix, id, index),
				BaseDisk:    baseOS,
				CPUCount:    1,        // TODO - make configurable
				Memory:      2048,     // TODO - make configurable
				sshUser:     "docker", // TODO - make configurable
				sshKeyPath:  filepath.Join(VirshDiskDir, "id_rsa"),
			}
			if err := m.cloneDisk(); err != nil {
				errChan <- err
				return
			}
			if err := m.define(); err != nil {
				errChan <- err
				return
			}
			if err := m.Start(); err != nil {
				errChan <- err
				return
			}
			cert, err := tls.LoadX509KeyPair(filepath.Join(VirshDiskDir, "cert.pem"), filepath.Join(VirshDiskDir, "key.pem"))
			if err != nil {
				errChan <- err
				return
			}
			caCert, err := ioutil.ReadFile(filepath.Join(VirshDiskDir, "ca.pem"))
			if err != nil {
				errChan <- err
				return
			}
			caCertPool := x509.NewCertPool()
			caCertPool.AppendCertsFromPEM(caCert)
			m.tlsConfig = &tls.Config{
				Certificates: []tls.Certificate{cert},
				RootCAs:      caCertPool,

				// NOTE:This is insecure, but the test VMs have a short-lifespan
				InsecureSkipVerify: true, // We don't verify so we can recyle the same certs regardless of VM IP

			}
			machines = append(machines, m)
		}

		var wg sync.WaitGroup

		res := []Machine{}
		machineErrChan := make(chan error, linuxCount)
		for _, m := range machines {
			wg.Add(1)
			go func(m *VirshMachine) {
				// Set the hostname
				out, err := m.MachineSSH(
					fmt.Sprintf(`sudo hostname "%s"; sudo sed -e 's/.*/%s/' -i /etc/hostname`,
						m.GetName(), m.GetName()))
				if err != nil {
					log.Warnf("Failed to set hostname to %s: %s: %s", m.GetName(), err, out)
				}

				res := VerifyDockerEngine(m, VirshDiskDir)

				wg.Done()
				machineErrChan <- res
			}(m)
			res = append(res, m)
		}
		wg.Wait()
		close(machineErrChan)
		for err := range machineErrChan {
			if err != nil {
				log.Debugf("XXX sleeping for 10s to allow you to suspend and poke around")
				time.Sleep(10 * time.Second)
				// Detected errors, destroy all the machines we created
				for _, m := range machines {
					m.Remove()
				}
				errChan <- err
				return
			}
		}
		resChan <- res
		return
	}()
	select {
	case res := <-resChan:
		log.Debugf("XXX Got %v on resChan", res)
		return res, nil, nil
	case err := <-errChan:
		log.Debugf("XXX Got %v on errChan", err)
		return nil, nil, err
	case <-timer.C:
		return nil, nil, fmt.Errorf("Unable to create %d machines within timeout", linuxCount)
	}
}

func (m *VirshMachine) cloneDisk() error {
	dir := path.Dir(m.BaseDisk)
	linkedCloneName := filepath.Join(dir, m.MachineName+".qcow2")
	if _, err := os.Stat(linkedCloneName); err == nil {
		return fmt.Errorf("Linked clone %s of base disk %s already exists!", linkedCloneName, m.BaseDisk)
	}
	log.Debugf("Creating linked clone %s with base disk %s", linkedCloneName, m.BaseDisk)
	cmd := exec.Command("qemu-img", "create", "-f", "qcow2", "-o", "backing_fmt=qcow2", "-b", m.BaseDisk, linkedCloneName)
	data, err := cmd.CombinedOutput()
	out := strings.TrimSpace(string(data))
	if err != nil {
		return fmt.Errorf("Failed to create linked clone %s on %s: %s: %s", linkedCloneName, m.BaseDisk, out, err)
	}
	log.Debug(out)
	m.DiskPath = linkedCloneName
	return nil
}

func (m *VirshMachine) define() error {
	log.Debugf("Creating vm %s", m.MachineName)
	tmpl, err := template.New("domain").Parse(domainXMLTemplate)
	if err != nil {
		return err
	}
	var xml bytes.Buffer
	err = tmpl.Execute(&xml, m)
	if err != nil {
		return err
	}

	// Write it out to a temporary file
	defFile := filepath.Join(path.Dir(m.DiskPath), m.MachineName+".xml")
	err = ioutil.WriteFile(defFile, xml.Bytes(), 0644)
	if err != nil {
		return err
	}
	defer os.Remove(defFile)

	cmd := exec.Command("virsh", "define", defFile)
	data, err := cmd.CombinedOutput()
	out := strings.TrimSpace(string(data))
	if err != nil {
		return fmt.Errorf("Failed to create %s: %s: %s", m.MachineName, err, out)
	}
	return nil
}

// GetName retrieves the machines name
func (m *VirshMachine) GetName() string {
	return m.MachineName
}

// GetDockerHost reports the machines docker host
func (m *VirshMachine) GetDockerHost() string {
	return m.dockerHost
}

// GetEngineAPIWithTimeout gets an engine API client with a default timeout
func (m *VirshMachine) GetEngineAPI() (*client.Client, error) {
	return m.GetEngineAPIWithTimeout(Timeout)
}

// GetEngineAPIWithTimeout gets an engine API client with a timeout set
func (m *VirshMachine) GetEngineAPIWithTimeout(timeout time.Duration) (*client.Client, error) {
	transport := &http.Transport{
		TLSClientConfig: m.tlsConfig,
	}
	httpClient := &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}
	version := "" // TODO
	return client.NewClient(m.dockerHost, version, httpClient, nil)
}

// IsRunning returns true if this machine is currently running
func (m *VirshMachine) IsRunning() bool {
	names := getActiveMachines()

	for _, name := range names {
		if m.MachineName == name {
			return true
		}

	}
	return false
}

// Remove the machiine after the tests have completed
func (m *VirshMachine) Remove() error {
	if os.Getenv("PRESERVE_TEST_MACHINE") != "" {
		log.Infof("Skipping removal of machine %s with PRESERVE_TEST_MACHINE set", m.GetName())
		return nil
	}
	if m.IsRunning() {
		m.Kill()
	}

	cmd := exec.Command("virsh", "undefine", "--storage", m.DiskPath, m.MachineName)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Error(string(out))
		return err
	}

	// If the disk still exists, nuke it, but ignore errors
	os.Remove(m.DiskPath)

	log.Infof("Machine %s deleted", m.MachineName)
	m.MachineName = ""
	return nil
}

// Remove the machiine after the tests have completed
func (m *VirshMachine) RemoveAndPreserveDisk() error {
	if os.Getenv("PRESERVE_TEST_MACHINE") != "" {
		log.Infof("Skipping removal of machine %s with PRESERVE_TEST_MACHINE set", m.GetName())
		return nil
	}
	if m.IsRunning() {
		m.Stop()
	}

	cmd := exec.Command("virsh", "undefine", m.MachineName)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Error(string(out))
		return err
	}

	log.Infof("Preserving %s", m.DiskPath)

	log.Infof("Machine %s deleted", m.MachineName)
	m.MachineName = ""
	return nil
}

// Stop gracefully shuts down the machine
func (m *VirshMachine) Stop() error {
	cmd := exec.Command("virsh", "shutdown", m.MachineName)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Error(string(out))
		return err
	}
	return nil
}

// Kill forcefully stops the virtual machine (likely to corrupt the machine, so
// do not use this if you intend to start the machine again)
func (m *VirshMachine) Kill() error {
	cmd := exec.Command("virsh", "destroy", m.MachineName)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Error(string(out))
		return err
	}

	// Make sure it's stopped before returning...
	resChan := make(chan error)

	go func(m *VirshMachine) {
		for {
			if !m.IsRunning() {
				resChan <- nil
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
	}(m)

	timer := time.NewTimer(1 * time.Minute) // TODO - make configurable
	select {
	case res := <-resChan:
		log.Debugf("Got %v on resChan", res)
		return res
	case <-timer.C:
		return fmt.Errorf("Unable to verify docker engine on %s within timeout", m.MachineName)
	}
}

// Start powers on the VM
func (m *VirshMachine) Start() error {
	cmd := exec.Command("virsh", "start", m.MachineName)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Error(string(out))
		return err
	}
	resChan := make(chan error)
	ipRegex := regexp.MustCompile(`ipv4\s+([^/]+)`)
	// wait for it to power on (by checking virsh -q domifaddr m.GetName())
	go func(m *VirshMachine) {
		log.Debugf("Waiting for IP to appear for %s", m.GetName())
		for {
			cmd := exec.Command("virsh", "-q", "domifaddr", m.GetName())
			data, err := cmd.CombinedOutput()
			out := strings.TrimSpace(string(data))
			if err == nil {
				lines := strings.Split(string(out), "\n")

				if len(lines) > 0 {
					matches := ipRegex.FindStringSubmatch(lines[0])
					if len(matches) > 0 {
						ip := matches[1]
						m.ip = ip
						m.internalip = ip
						m.dockerHost = fmt.Sprintf("tcp://%s:2376", ip)
						// TODO validate the IP looks good
						break
					}
				}
			}
			time.Sleep(1 * time.Second)
		}
		log.Debugf("Machine %s has IP %s", m.GetName(), m.ip)

		// Loop until we can ssh in
		for {
			out, err := m.MachineSSH("uptime")
			if err != nil {
				//log.Debugf("XXX Failed to ssh to %s: %s", m.GetName(), err)
				time.Sleep(500 * time.Millisecond)
			} else {
				log.Debugf("%s has been up %s", m.GetName(), out)
				break
			}
		}

		resChan <- nil
	}(m)

	timer := time.NewTimer(60 * time.Second) // TODO - make configurable
	select {
	case res := <-resChan:
		return res
	case <-timer.C:
		return fmt.Errorf("Unable to verify docker engine on %s within timeout", m.GetName())
	}
}

// Return the public IP of the machine
func (m *VirshMachine) GetIP() (string, error) {
	return m.ip, nil
}

// Get the internal IP (useful for join operations)
func (m *VirshMachine) GetInternalIP() (string, error) {
	return m.internalip, nil
}

// MachineSSH runs an ssh command and returns a string of the combined stdout/stderr output once done
func (m *VirshMachine) MachineSSH(command string) (string, error) {
	args := []string{
		"ssh", "-q",
		"-o", "StrictHostKeyChecking=no",
		"-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "UpdateHostKeys=no",
		"-o", "CheckHostIP=no",
		"-o", "ConnectTimeout=8",
		"-o", "VerifyHostKeyDNS=no",
		"-i", m.sshKeyPath,
		m.sshUser + "@" + m.ip,
		command,
	}
	log.Debugf("SSH to %s: %v", m.MachineName, args)
	cmd := exec.Command(args[0], args[1:]...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// Get the contents of a specific file on the engine
func (m *VirshMachine) CatHostFile(hostPath string) ([]byte, error) {
	return catHostFile(m, hostPath)
}

// Get the content of a directory as a tar file from the engine
func (m *VirshMachine) TarHostDir(hostPath string) ([]byte, error) {
	return tarHostDir(m, hostPath)
}

// Write data from an io.Reader to a file on the machine with 0600 perms.
func (m *VirshMachine) WriteFile(filePath string, data io.Reader) error {
	f, err := ioutil.TempFile("/tmp", "E2ETestTempFile")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()

	_, err = io.Copy(f, data)
	if err != nil {
		return err
	}
	return m.writeLocalFile(f.Name(), filePath)
}

func (m *VirshMachine) writeLocalFile(localFilePath, remoteFilePath string) error {
	cmd := exec.Command("scp", "-i", m.sshKeyPath, "-q",
		"-o", "StrictHostKeyChecking=no",
		"-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "UpdateHostKeys=no",
		"-o", "CheckHostIP=no",
		"-o", "VerifyHostKeyDNS=no",
		localFilePath,
		fmt.Sprintf("%s@%s:%s", m.sshUser, m.ip, remoteFilePath))
	data, err := cmd.CombinedOutput()
	out := strings.TrimSpace(string(data))
	if out != "" {
		log.Debug(out)
	}
	if err != nil {
		log.Error(string(out))
		return err
	}
	return nil
}
