// Package meshcentral integrates with a self-hosted MeshCentral server for
// remote desktop, terminal and file access.
//
// Two mechanisms are used, both based on MeshCentral's own encrypted cookie
// format (meshcentral.js encodeCookie/decodeCookieAESGCM), keyed with the
// server's LoginCookieEncryptionKey (80 random bytes, 160 hex characters,
// set as settings.LoginCookieEncryptionKey in MeshCentral's config.json):
//
//   - a browser login token ({u, a:3}) appended as ?login= to a deep link, so
//     an admin who is signed in here lands in MeshCentral already signed in,
//     on the right device and tab;
//   - an API cookie ({userid, domainid}) used as ?auth= on control.ashx to
//     list devices and device groups.
//
// The key lets its holder act as any MeshCentral user, so it is a secret on
// a par with the admin password.
package meshcentral

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// KeyLen is the length of MeshCentral's login cookie encryption key.
const KeyLen = 80

// ParseKey decodes the 160-hex-character LoginCookieEncryptionKey.
func ParseKey(h string) ([]byte, error) {
	k, err := hex.DecodeString(strings.TrimSpace(h))
	if err != nil {
		return nil, fmt.Errorf("MeshCentral login key is not hex: %w", err)
	}
	if len(k) != KeyLen {
		return nil, fmt.Errorf("MeshCentral login key must be %d bytes (%d hex characters), got %d bytes",
			KeyLen, 2*KeyLen, len(k))
	}
	return k, nil
}

// EncodeCookie produces a cookie MeshCentral's decodeCookie accepts:
// AES-256-GCM with the first 32 bytes of the key, laid out as
// IV(12) || tag(16) || ciphertext, standard base64 with '+'→'@' and '/'→'$'.
// The creation time (unix seconds) is added as "time", as encodeCookie does.
func EncodeCookie(key []byte, payload map[string]any, now time.Time) (string, error) {
	if len(key) < 32 {
		return "", errors.New("key too short")
	}
	o := make(map[string]any, len(payload)+1)
	for k, v := range payload {
		o[k] = v
	}
	o["time"] = now.Unix()
	plain, err := json.Marshal(o)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key[:32])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	iv := make([]byte, gcm.NonceSize()) // 12
	if _, err := rand.Read(iv); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nil, iv, plain, nil) // ciphertext || tag
	ct, tag := sealed[:len(sealed)-gcm.Overhead()], sealed[len(sealed)-gcm.Overhead():]
	buf := make([]byte, 0, len(iv)+len(tag)+len(ct))
	buf = append(append(append(buf, iv...), tag...), ct...)
	s := base64.StdEncoding.EncodeToString(buf)
	return strings.NewReplacer("+", "@", "/", "$").Replace(s), nil
}

// DecodeCookie reverses EncodeCookie (used by tests and for diagnostics). It
// does not apply MeshCentral's expiry rules.
func DecodeCookie(key []byte, cookie string) (map[string]any, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.NewReplacer("@", "+", "$", "/").Replace(cookie))
	if err != nil {
		return nil, err
	}
	if len(raw) < 28 {
		return nil, errors.New("cookie too short")
	}
	block, err := aes.NewCipher(key[:32])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	iv, tag, ct := raw[:12], raw[12:28], raw[28:]
	plain, err := gcm.Open(nil, iv, append(append([]byte{}, ct...), tag...), nil)
	if err != nil {
		return nil, err
	}
	var o map[string]any
	if err := json.Unmarshal(plain, &o); err != nil {
		return nil, err
	}
	return o, nil
}

// UserID returns MeshCentral's internal id for a user in the default domain.
func UserID(username string) string {
	return "user//" + strings.ToLower(strings.TrimSpace(username))
}

// LoginToken returns a single-use browser login token for user that expires
// after expireMin minutes. MeshCentral honours "expire" (minutes) in place of
// its fixed timeout, and "once" makes decodeCookie reject any reuse.
func LoginToken(key []byte, username string, expireMin int, now time.Time) (string, error) {
	once := make([]byte, 12)
	if _, err := rand.Read(once); err != nil {
		return "", err
	}
	return EncodeCookie(key, map[string]any{
		"u":      UserID(username),
		"a":      3,
		"expire": expireMin,
		"once":   hex.EncodeToString(once),
	}, now)
}

// apiCookie authenticates a control.ashx websocket as username.
func apiCookie(key []byte, username string, now time.Time) (string, error) {
	return EncodeCookie(key, map[string]any{
		"userid":   UserID(username),
		"domainid": "",
		"expire":   1,
	}, now)
}
