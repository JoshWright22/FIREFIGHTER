// Command releasekey makes the release-signing key and signs a release's checksums.txt.
//
//	go run ./scripts/releasekey gen  <key file>                   prints the public key for internal/update/sign.go
//	go run ./scripts/releasekey sign <key file> <checksums.txt>   writes <checksums.txt>.sig
//
// The private key is stored encrypted with a passphrase (PBKDF2-SHA256, 600,000 rounds, AES-256-GCM),
// asked for on the terminal or read from FIREFIGHTER_SIGN_PASSPHRASE. Agents on this PC can read
// the key file but not the passphrase: keep the passphrase out of every file, prompt and chat.
package main

import (
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

type keyFile struct {
	Public string `json:"public"`
	Salt   string `json:"salt"`
	Nonce  string `json:"nonce"`
	Sealed string `json:"sealed"` // the ed25519 seed, AES-256-GCM
}

const rounds = 600_000

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "releasekey:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	switch {
	case len(args) == 2 && args[0] == "gen":
		return gen(args[1])
	case len(args) == 3 && args[0] == "sign":
		return sign(args[1], args[2])
	}
	return errors.New("usage: releasekey gen <key file> | releasekey sign <key file> <checksums.txt>")
}

func passphrase(confirm bool) (string, error) {
	if p := os.Getenv("FIREFIGHTER_SIGN_PASSPHRASE"); p != "" {
		return p, nil
	}
	in := bufio.NewReader(os.Stdin)
	fmt.Fprint(os.Stderr, "passphrase: ")
	p, err := in.ReadString('\n')
	if err != nil {
		return "", err
	}
	p = strings.TrimRight(p, "\r\n")
	if len(p) < 12 {
		return "", errors.New("use a passphrase of at least 12 characters")
	}
	if confirm {
		fmt.Fprint(os.Stderr, "again: ")
		q, _ := in.ReadString('\n')
		if strings.TrimRight(q, "\r\n") != p {
			return "", errors.New("the passphrases differ")
		}
	}
	return p, nil
}

func aead(pass string, salt []byte) (cipher.AEAD, error) {
	k, err := pbkdf2.Key(sha256.New, pass, salt, rounds, 32)
	if err != nil {
		return nil, err
	}
	b, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

func gen(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s exists: not overwriting a key", path)
	}
	pass, err := passphrase(true)
	if err != nil {
		return err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	salt, nonce := make([]byte, 16), make([]byte, 12)
	_, _ = rand.Read(salt)
	_, _ = rand.Read(nonce)
	g, err := aead(pass, salt)
	if err != nil {
		return err
	}
	kf := keyFile{Public: base64.StdEncoding.EncodeToString(pub), Salt: base64.StdEncoding.EncodeToString(salt),
		Nonce: base64.StdEncoding.EncodeToString(nonce), Sealed: base64.StdEncoding.EncodeToString(g.Seal(nil, nonce, priv.Seed(), pub))}
	b, _ := json.MarshalIndent(kf, "", "  ")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return err
	}
	fmt.Println(kf.Public)
	fmt.Fprintln(os.Stderr, "public key above: add it to releaseKeys in internal/update/sign.go")
	return nil
}

func sign(path, sums string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var kf keyFile
	if err := json.Unmarshal(b, &kf); err != nil {
		return err
	}
	pub, _ := base64.StdEncoding.DecodeString(kf.Public)
	salt, _ := base64.StdEncoding.DecodeString(kf.Salt)
	nonce, _ := base64.StdEncoding.DecodeString(kf.Nonce)
	sealed, _ := base64.StdEncoding.DecodeString(kf.Sealed)
	pass, err := passphrase(false)
	if err != nil {
		return err
	}
	g, err := aead(pass, salt)
	if err != nil {
		return err
	}
	seed, err := g.Open(nil, nonce, sealed, pub)
	if err != nil {
		return errors.New("wrong passphrase (or a damaged key file)")
	}
	data, err := os.ReadFile(sums)
	if err != nil {
		return err
	}
	sig := ed25519.Sign(ed25519.NewKeyFromSeed(seed), data)
	if err := os.WriteFile(sums+".sig", []byte(base64.StdEncoding.EncodeToString(sig)+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s.sig: upload it to the release next to checksums.txt\n", sums)
	return nil
}
