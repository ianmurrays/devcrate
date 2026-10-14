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
	"time"

	"golang.org/x/crypto/ssh"
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

// Dial connects to the VM, retrying until wait has passed.
func Dial(host string, port int, user string, key *Key, wait time.Duration) (*ssh.Client, error) {
	cfg := &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(key.Signer)},
		// Host keys are pinned per Environment by the SSH config ticket; until
		// then the VM is trusted on first and every use.
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
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
