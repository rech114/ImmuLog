// SPDX-License-Identifier: Apache-2.0

package feed

// crypto.go —— epoch 密钥与「逐收件人封装」。
//
// 全部用标准库：crypto/ecdh(X25519) + crypto/hkdf(RFC 5869) +
// crypto/aes(GCM) + crypto/rand。没有自研原语，也没有第三方依赖。
//
// 为什么要逐收件人封装：
// 完整性和隐私在这里是**相反**的要求 —— 完整性想要副本越广越强，
// 隐私要求密钥的传播范围必须比密文窄得多。
// 把 epoch 密钥用每个成员的公钥各封一份，就同时满足了：
// 密文随仓库自由复制，而**只有成员解得开**。

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
)

// KeySize 是对称密钥长度。
const KeySize = 32

// encInfo 把 HKDF 导出的密钥绑定到「封装」这一个用途上。
var encInfo = []byte("immulog/epoch-wrap/v1")

// bodyInfo 把正文加密的密钥和封装用的密钥分开。
var bodyInfo = []byte("immulog/body/v1")

var (
	// ErrNoKey 表示本地没有这个 epoch 的密钥。
	ErrNoKey = errors.New("本地没有该 epoch 的密钥")
	// ErrNotRecipient 表示这份封装不是给我们的 —— 直白说就是"你不是成员"。
	ErrNotRecipient = errors.New("这份密钥不是封装给你的")
	// ErrBadCiphertext 表示密文被改动或格式不对（AEAD 认证失败会落到这里）。
	ErrBadCiphertext = errors.New("密文校验失败")
)

// ── 加密身份 ──────────────────────────────────────────────────────

// EncIdentity 是一对 X25519 密钥，用于**收**封装给自己的 epoch 密钥。
//
// 它与签名密钥无关：签名用的是 git 的 ssh 签名，加密用的是这里的 X25519。
// 分开是有意的 —— 签名密钥通常由 ssh-agent 托管，拿不到私钥原始字节。
type EncIdentity struct {
	priv *ecdh.PrivateKey
}

// GenerateEncIdentity 生成一对新的加密身份。
func GenerateEncIdentity() (*EncIdentity, error) {
	p, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &EncIdentity{priv: p}, nil
}

// ParseEncIdentity 从 32 字节种子还原身份（私钥落盘用）。
func ParseEncIdentity(seed []byte) (*EncIdentity, error) {
	if len(seed) != KeySize {
		return nil, fmt.Errorf("加密私钥种子应为 %d 字节，得到 %d", KeySize, len(seed))
	}
	p, err := ecdh.X25519().NewPrivateKey(seed)
	if err != nil {
		return nil, err
	}
	return &EncIdentity{priv: p}, nil
}

// Seed 导出 32 字节私钥种子。
func (e *EncIdentity) Seed() []byte { return e.priv.Bytes() }

// Public 返回 base64 编码的公钥，可直接写进 trailer。
func (e *EncIdentity) Public() string {
	return base64.StdEncoding.EncodeToString(e.priv.PublicKey().Bytes())
}

// ParseEncPublic 解析 base64 公钥。
func ParseEncPublic(s string) (*ecdh.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("公钥不是合法 base64: %w", err)
	}
	return ecdh.X25519().NewPublicKey(raw)
}

// ── 逐收件人封装 ──────────────────────────────────────────────────

// WrapKey 把 epoch 密钥封装给某个收件人公钥。
//
// 构造是标准的 ECIES 式：
//
//	ephemeral ← 随机 X25519 密钥对
//	shared    ← ECDH(ephemeral, recipient)
//	wrapKey   ← HKDF-SHA256(shared, info="immulog/epoch-wrap/v1")
//	输出       ← base64(ephemeralPub ‖ nonce ‖ AES-GCM(wrapKey, key))
//
// 每次封装都用新的临时密钥对，所以同一个 epoch 密钥对不同成员的两份封装
// 互不可比 —— 外部看不出"这两个人是同一个房间的"。
func WrapKey(recipient *ecdh.PublicKey, key []byte) (string, error) {
	if len(key) != KeySize {
		return "", fmt.Errorf("密钥应为 %d 字节", KeySize)
	}
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	shared, err := eph.ECDH(recipient)
	if err != nil {
		return "", err
	}
	wk, err := hkdf.Key(sha256.New, shared, nil, string(encInfo), KeySize)
	if err != nil {
		return "", err
	}
	sealed, err := seal(wk, key)
	if err != nil {
		return "", err
	}
	out := append(append([]byte{}, eph.PublicKey().Bytes()...), sealed...)
	return base64.StdEncoding.EncodeToString(out), nil
}

// UnwrapKey 用私钥解出 epoch 密钥。不是给你的封装会返回 ErrNotRecipient。
func UnwrapKey(priv *ecdh.PrivateKey, wrapped string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(wrapped)
	if err != nil {
		return nil, fmt.Errorf("封装不是合法 base64: %w", err)
	}
	if len(raw) <= 32 {
		return nil, ErrNotRecipient
	}
	ephPub, err := ecdh.X25519().NewPublicKey(raw[:32])
	if err != nil {
		return nil, ErrNotRecipient
	}
	shared, err := priv.ECDH(ephPub)
	if err != nil {
		// X25519 对低阶点会拒绝 —— 换个说法，就是"这不是给我的"
		return nil, ErrNotRecipient
	}
	wk, err := hkdf.Key(sha256.New, shared, nil, string(encInfo), KeySize)
	if err != nil {
		return nil, err
	}
	return open(wk, raw[32:])
}

// ── 正文加解密 ────────────────────────────────────────────────────

// SealBody 用 epoch 密钥加密正文，返回 base64(nonce‖ciphertext)。
//
// 正文密钥由 epoch 密钥经 HKDF 派生，与封装用的密钥**不同** ——
// 这样即使某处误用了封装输出，也解不开正文。
func SealBody(key []byte, plaintext string) (string, error) {
	bk, err := hkdf.Key(sha256.New, key, nil, string(bodyInfo), KeySize)
	if err != nil {
		return "", err
	}
	sealed, err := seal(bk, []byte(plaintext))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// OpenBody 解密正文。密文被改动过会返回 ErrBadCiphertext。
func OpenBody(key []byte, b64 string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", fmt.Errorf("密文不是合法 base64: %w", err)
	}
	bk, err := hkdf.Key(sha256.New, key, nil, string(bodyInfo), KeySize)
	if err != nil {
		return "", err
	}
	plain, err := open(bk, raw)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// ── AEAD ──────────────────────────────────────────────────────────

func seal(key, plaintext []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

func open(key, blob []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	n := gcm.NonceSize()
	if len(blob) < n {
		return nil, ErrBadCiphertext
	}
	out, err := gcm.Open(nil, blob[:n], blob[n:], nil)
	if err != nil {
		return nil, ErrBadCiphertext
	}
	return out, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
