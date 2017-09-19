package auth

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/emersion/hydroxide/protonmail"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/crypto/nacl/secretbox"
	"golang.org/x/crypto/openpgp"
)

const authFile = "auth.json"

type CachedAuth struct {
	protonmail.Auth
	LoginPassword   string
	MailboxPassword string
	// TODO: add padding
}

func readCachedAuths() (map[string]string, error) {
	f, err := os.Open(authFile)
	if os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	defer f.Close()

	auths := make(map[string]string)
	err = json.NewDecoder(f).Decode(&auths)
	return auths, err
}

func saveAuths(auths map[string]string) error {
	f, err := os.Create(authFile)
	if err != nil {
		return err
	}
	defer f.Close()

	return json.NewEncoder(f).Encode(auths)
}

func encrypt(msg []byte, secretKey *[32]byte) (string, error) {
	var nonce [24]byte
	if _, err := io.ReadFull(rand.Reader, nonce[:]); err != nil {
		return "", err
	}

	encrypted := secretbox.Seal(nonce[:], msg, &nonce, secretKey)
	return base64.StdEncoding.EncodeToString(encrypted), nil
}

func decrypt(encryptedString string, secretKey *[32]byte) ([]byte, error) {
	encrypted, err := base64.StdEncoding.DecodeString(encryptedString)
	if err != nil {
		return nil, err
	}

	var nonce [24]byte
	copy(nonce[:], encrypted[:24])
	decrypted, ok := secretbox.Open(nil, encrypted[24:], &nonce, secretKey)
	if !ok {
		return nil, errors.New("decryption error")
	}
	return decrypted, nil
}

func EncryptAndSave(auth *CachedAuth, username string, secretKey *[32]byte) error {
	cleartext, err := json.Marshal(auth)
	if err != nil {
		return err
	}

	encrypted, err := encrypt(cleartext, secretKey)
	if err != nil {
		return err
	}

	auths, err := readCachedAuths()
	if err != nil {
		return err
	}

	if auths == nil {
		auths = make(map[string]string)
	}
	auths[username] = encrypted

	return saveAuths(auths)
}

func authenticate(c *protonmail.Client, CachedAuth *CachedAuth) (openpgp.EntityList, error) {
	auth, err := c.AuthRefresh(&CachedAuth.Auth)
	if err != nil {
		// TODO: handle expired token, re-authenticate
		return nil, err
	}
	CachedAuth.Auth = *auth

	return c.Unlock(auth, CachedAuth.MailboxPassword)
}

func GeneratePassword() (secretKey *[32]byte, password string, err error) {
	var key [32]byte
	if _, err = io.ReadFull(rand.Reader, key[:]); err != nil {
		return
	}
	secretKey = &key
	password = base64.StdEncoding.EncodeToString(key[:])
	return
}

type session struct {
	hashedSecretKey []byte
	c               *protonmail.Client
	privateKeys     openpgp.EntityList
}

var ErrUnauthorized = errors.New("Invalid username or password")

type Manager struct {
	newClient func() *protonmail.Client
	sessions  map[string]*session
}

func (m *Manager) Auth(username, password string) (*protonmail.Client, openpgp.EntityList, error) {
	var secretKey [32]byte
	passwordBytes, err := base64.StdEncoding.DecodeString(password)
	if err != nil || len(passwordBytes) != len(secretKey) {
		return nil, nil, ErrUnauthorized
	}
	copy(secretKey[:], passwordBytes)

	s, ok := m.sessions[username]
	if ok {
		err := bcrypt.CompareHashAndPassword(s.hashedSecretKey, secretKey[:])
		if err != nil {
			return nil, nil, ErrUnauthorized
		}
	} else {
		auths, err := readCachedAuths()
		if err != nil && !os.IsNotExist(err) {
			return nil, nil, err
		}

		encrypted, ok := auths[username]
		if !ok {
			return nil, nil, ErrUnauthorized
		}

		decrypted, err := decrypt(encrypted, &secretKey)
		if err != nil {
			return nil, nil, ErrUnauthorized
		}

		var cachedAuth CachedAuth
		if err := json.Unmarshal(decrypted, &cachedAuth); err != nil {
			return nil, nil, err
		}

		// authenticate updates cachedAuth with the new refresh token
		c := m.newClient()
		privateKeys, err := authenticate(c, &cachedAuth)
		if err != nil {
			return nil, nil, err
		}

		if err := EncryptAndSave(&cachedAuth, username, &secretKey); err != nil {
			return nil, nil, err
		}

		hashed, err := bcrypt.GenerateFromPassword(secretKey[:], bcrypt.DefaultCost)
		if err != nil {
			return nil, nil, err
		}

		s = &session{
			c:               c,
			privateKeys:     privateKeys,
			hashedSecretKey: hashed,
		}
		m.sessions[username] = s
	}

	return s.c, s.privateKeys, nil
}

func NewManager(newClient func() *protonmail.Client) *Manager {
	return &Manager{
		newClient: newClient,
		sessions:  make(map[string]*session),
	}
}
