package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
	"io"
)

const vaultMagic = "CLEARLINE1"

func seal(password string, profiles []Profile) (string, error) {
	if len(password) < 10 || len(password) > 1024 {
		return "", errors.New("迁移口令需至少 10 个字符")
	}
	if len(profiles) == 0 || len(profiles) > 32 {
		return "", errors.New("请选择需要迁移的连接")
	}
	for _, p := range profiles {
		if e := ValidateProfile(p); e != nil {
			return "", e
		}
	}
	plain, e := json.Marshal(profiles)
	if e != nil || len(plain) > maxInput {
		return "", errors.New("配置过大")
	}
	salt := make([]byte, 16)
	nonce := make([]byte, chacha20poly1305.NonceSizeX)
	if _, e = io.ReadFull(rand.Reader, salt); e != nil {
		return "", e
	}
	if _, e = io.ReadFull(rand.Reader, nonce); e != nil {
		return "", e
	}
	key := argon2.IDKey([]byte(password), salt, 3, 64*1024, 1, 32)
	defer clear(key)
	aead, e := chacha20poly1305.NewX(key)
	if e != nil {
		return "", e
	}
	header := append([]byte(vaultMagic), salt...)
	header = append(header, nonce...)
	out := append(header, aead.Seal(nil, nonce, plain, header)...)
	return base64.StdEncoding.EncodeToString(out), nil
}
func unseal(password, data string) ([]Profile, error) {
	if len(data) > 2*maxInput || len(password) > 1024 {
		return nil, errors.New("迁移文件过大")
	}
	b, e := base64.StdEncoding.DecodeString(data)
	h := len(vaultMagic) + 16 + chacha20poly1305.NonceSizeX
	if e != nil || len(b) < h+16 || string(b[:len(vaultMagic)]) != vaultMagic {
		return nil, errors.New("迁移文件格式无效")
	}
	key := argon2.IDKey([]byte(password), b[len(vaultMagic):len(vaultMagic)+16], 3, 64*1024, 1, 32)
	defer clear(key)
	aead, e := chacha20poly1305.NewX(key)
	if e != nil {
		return nil, e
	}
	plain, e := aead.Open(nil, b[len(vaultMagic)+16:h], b[h:], b[:h])
	if e != nil {
		return nil, errors.New("口令错误或文件已损坏")
	}
	defer clear(plain)
	var ps []Profile
	if json.Unmarshal(plain, &ps) != nil || len(ps) == 0 || len(ps) > 32 {
		return nil, errors.New("迁移文件内容无效")
	}
	for _, p := range ps {
		if e := ValidateProfile(p); e != nil {
			return nil, e
		}
	}
	return ps, nil
}
