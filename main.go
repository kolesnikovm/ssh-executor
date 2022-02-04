package main

import (
	"bytes"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/pkg/sftp"
	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v2"
)

type Config []struct {
	Hosts []string `yaml:"hosts"`
	User  string   `yaml:"user"`
	Pass  string   `yaml:"pass"`
}

var (
	mode     = flag.String("mode", "exec", "exec or upload")
	cmd      = flag.String("cmd", "", "cmd")
	srcFile  = flag.String("srcFile", "", "file")
	dstFile  = flag.String("dstFile", "", "file")
	hosts    = flag.String("hosts", "hosts.yml", "hosts")
	timeout  = flag.Duration("timeout", 10*time.Second, "timeout")
	logLevel = flag.String("logLevel", "info", "info or debug")
)

func main() {
	flag.Parse()

	if *logLevel == "debug" {
		log.SetLevel(log.DebugLevel)
	}

	log.Debug("Starting ssh executor")

	services := parseHosts(*hosts)

	hostCount := getHostCount(services)

	log.Debugf("Host count: %d", hostCount)

	connections := make(chan *ssh.Client, hostCount)

	for service, configs := range services {
		log.Debugf("Creating connections for: %s", service)
		configs.getConnections(connections)
	}

	done := make(chan bool, hostCount)
	out := make(chan string, hostCount)
	err := make(chan string, hostCount)

	switch *mode {
	case "exec":
		for i := 0; i < hostCount; i++ {
			select {
			case conn := <-connections:
				go func(conn *ssh.Client) {
					stdout, stderr := executeCmd(*cmd, conn)

					if len(stdout) != 0 {
						out <- conn.Conn.RemoteAddr().String() + "\t" + stdout
					}
					if len(stderr) != 0 {
						err <- conn.Conn.RemoteAddr().String() + "\t" + stderr
					}

					done <- true
				}(conn)
			case <-time.After(*timeout):
				log.Warn("Timeout creating connection")
			}
		}
	case "upload":
		for i := 0; i < hostCount; i++ {
			select {
			case conn := <-connections:
				go func(conn *ssh.Client) {
					err := SSHCopyFile(*srcFile, *dstFile, conn)
					if err != nil {
						log.Error("failed to upload file")
					}

					done <- true
				}(conn)
			case <-time.After(*timeout):
				log.Warn("Timeout creating connection")
			}
		}
	default:
		log.Error("wrong mode")
	}

	for i := 0; i < hostCount; i++ {
		select {
		case <-done:
		case <-time.After(*timeout):
			log.Warn("Operation timeout")
		}
	}

	close(out)
	close(err)

	log.Info("stdout")
	for stdout := range out {
		fmt.Println(stdout)
	}

	log.Info("stderr")
	for stderr := range err {
		fmt.Println(stderr)
	}
}

func parseHosts(path string) map[string]Config {
	services := make(map[string]Config)

	filename, err := filepath.Abs(path)
	if err != nil {
		panic(err)
	}

	yamlFile, err := ioutil.ReadFile(filename)
	if err != nil {
		panic(err)
	}

	err = yaml.Unmarshal(yamlFile, &services)
	if err != nil {
		panic(err)
	}

	return services
}

func getHostCount(services map[string]Config) (count int) {
	for _, configs := range services {
		for _, config := range configs {
			count += len(config.Hosts)
		}
	}

	return count
}

func (c *Config) getConnections(connections chan *ssh.Client) {
	for _, config := range *c {
		sshConfig := &ssh.ClientConfig{
			User: config.User,
			Auth: []ssh.AuthMethod{
				ssh.Password(config.Pass),
			},
			HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		}
		for _, host := range config.Hosts {
			go func(host string, conf *ssh.ClientConfig) {
				connection := createConnection(host, sshConfig)
				if connection != nil {
					connections <- connection
				}
			}(host, sshConfig)
		}
	}
}

func createConnection(host string, conf *ssh.ClientConfig) *ssh.Client {
	conn, err := ssh.Dial("tcp", host+":22", conf)
	if err != nil {
		log.Error(err.Error())
		return nil
	}

	return conn
}

func executeCmd(cmd string, conn *ssh.Client) (stdout, stderr string) {
	session, err := conn.NewSession()
	if err != nil {
		log.Error("Failed to create session" + err.Error())
	}
	defer session.Close()

	var stdoutBuf, stderrBuf bytes.Buffer
	session.Stdout = &stdoutBuf
	session.Stderr = &stderrBuf
	session.Run(cmd)

	return stdoutBuf.String(), stderrBuf.String()
}

func SSHCopyFile(srcPath, dstPath string, conn *ssh.Client) error {
	// open an SFTP session over an existing ssh connection.
	sftp, err := sftp.NewClient(conn)
	if err != nil {
		return err
	}
	defer sftp.Close()

	if strings.HasPrefix(srcPath, "~/") {
		usr, err := user.Current()
		if err != nil {
			return err
		}
		srcPath = filepath.Join(usr.HomeDir, srcPath[1:])
	}
	// Open the source file
	srcFile, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	if strings.HasPrefix(dstPath, "~/") {
		dstPath = filepath.Join("/home/", conn.Conn.User(), dstPath[1:])
	}
	err = sftp.MkdirAll(filepath.Dir(dstPath))
	if err != nil {
		return err
	}

	// Create the destination file
	dstFile, err := sftp.Create(dstPath)
	if err != nil {
		return err
	}
	defer dstFile.Close()

	// write to file
	if _, err := dstFile.ReadFrom(srcFile); err != nil {
		return err
	}
	return nil
}
