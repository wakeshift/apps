package agentchat_test

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
)

// pkce returns a new code verifier and its S256 challenge, as an MCP client
// makes them for Agent Chat's own sign-in.
func pkce() (verifier, challenge string) {
	b := make([]byte, 32)
	rand.Read(b)
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}
