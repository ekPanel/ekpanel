package agentproto

import (
	"crypto/rsa"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// NormalizeSSHKey parses an authorized_keys line and returns it without any options
// (command=, from=, …), so a pasted key can never carry instructions for sshd.
func NormalizeSSHKey(line string) (string, error) {
	pub, comment, _, _, err := ssh.ParseAuthorizedKey([]byte(strings.TrimSpace(line)))
	if err != nil {
		return "", fmt.Errorf("not a valid SSH public key")
	}
	switch pub.Type() {
	case ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521,
		ssh.KeyAlgoSKED25519, ssh.KeyAlgoSKECDSA256:
	case ssh.KeyAlgoRSA:
		if ck, ok := pub.(ssh.CryptoPublicKey); ok {
			if rk, ok := ck.CryptoPublicKey().(*rsa.PublicKey); ok && rk.N.BitLen() < 2048 {
				return "", errors.New("RSA keys must be at least 2048 bits")
			}
		}
	default:
		return "", fmt.Errorf("SSH key type %s is not supported", pub.Type())
	}
	out := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))
	comment = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, comment)
	if comment != "" {
		out += " " + comment
	}
	return out, nil
}
