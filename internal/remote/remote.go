// Package remote is devcrate's SSH client to an Environment's VM.
package remote

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Key is devcrate's own keypair, kept at <configDir>/id_ed25519{,.pub} and
// created on first use. Its public half is the Providers' ssh_public_key.
type Key struct {
	Signer        ssh.Signer
	AuthorizedKey string // one line, authorized_keys format
}

func LoadKey(configDir string) (*Key, error) {
	path := filepath.Join(configDir, "id_ed25519")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		data, err = createKey(path)
	}
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	pub := string(ssh.MarshalAuthorizedKey(signer.PublicKey()))
	return &Key{Signer: signer, AuthorizedKey: pub[:len(pub)-1] + " devcrate"}, nil
}

func createKey(path string) ([]byte, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	block, err := ssh.MarshalPrivateKey(priv, "devcrate")
	if err != nil {
		return nil, err
	}
	data := pem.EncodeToMemory(block)
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return nil, err
	}
	return data, os.WriteFile(path+".pub", ssh.MarshalAuthorizedKey(signer.PublicKey()), 0o644)
}

// Dial connects to the VM, retrying until wait has passed. The VM's host key
// must match the one pinned under alias in knownHosts; with none pinned yet,
// the key is trusted and pinned (trust on first use).
func Dial(host string, port int, user string, key *Key, knownHosts, alias string, wait time.Duration) (*ssh.Client, error) {
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(key.Signer)},
		HostKeyCallback: pin(knownHosts, alias),
		// OpenSSH prefers ed25519 too, so plain ssh checks the key pinned here.
		HostKeyAlgorithms: []string{ssh.KeyAlgoED25519},
		Timeout:           5 * time.Second,
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	deadline := time.Now().Add(wait)
	for {
		client, err := ssh.Dial("tcp", addr, cfg)
		if err == nil || time.Now().After(deadline) {
			return client, err
		}
		time.Sleep(time.Second)
	}
}

// pin checks the server's host key against the known_hosts entry for alias,
// the HostKeyAlias of the Environment's SSH config block, ignoring the
// address it was reached at (which changes on every resume).
func pin(knownHosts, alias string) ssh.HostKeyCallback {
	return func(_ string, remote net.Addr, key ssh.PublicKey) error {
		if err := os.MkdirAll(filepath.Dir(knownHosts), 0o700); err != nil {
			return err
		}
		f, err := os.OpenFile(knownHosts, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		check, err := knownhosts.New(knownHosts)
		if err != nil {
			return err
		}
		err = check(alias+":22", remote, key)
		var keyErr *knownhosts.KeyError
		if errors.As(err, &keyErr) {
			if len(keyErr.Want) > 0 {
				return fmt.Errorf("host key for %s does not match the one pinned in %s", alias, knownHosts)
			}
			_, err = fmt.Fprintln(f, knownhosts.Line([]string{alias}, key))
		}
		return err
	}
}

// Unpin removes alias's entries from knownHosts.
func Unpin(knownHosts, alias string) error {
	data, err := os.ReadFile(knownHosts)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	var keep []string
	for _, line := range strings.SplitAfter(string(data), "\n") {
		if f := strings.Fields(line); len(f) == 0 || f[0] != alias {
			keep = append(keep, line)
		}
	}
	return os.WriteFile(knownHosts, []byte(strings.Join(keep, "")), 0o600)
}

// RunWithAgent is Run with the laptop's SSH agent at agentSock forwarded to
// the session, as `ssh -A` does. An empty agentSock forwards nothing.
func RunWithAgent(client *ssh.Client, agentSock, cmd string, stdin io.Reader, stdout, stderr io.Writer) error {
	if agentSock == "" {
		return Run(client, cmd, stdin, stdout, stderr)
	}
	if err := agent.ForwardToRemote(client, agentSock); err != nil {
		return err
	}
	s, err := client.NewSession()
	if err != nil {
		return err
	}
	defer s.Close()
	if err := agent.RequestAgentForwarding(s); err != nil {
		return err
	}
	s.Stdin, s.Stdout, s.Stderr = stdin, stdout, stderr
	return s.Run(cmd)
}

// Run runs cmd on the VM with the given stdio and returns its error
// (*ssh.ExitError for a non-zero exit).
func Run(client *ssh.Client, cmd string, stdin io.Reader, stdout, stderr io.Writer) error {
	s, err := client.NewSession()
	if err != nil {
		return err
	}
	defer s.Close()
	s.Stdin, s.Stdout, s.Stderr = stdin, stdout, stderr
	return s.Run(cmd)
}
