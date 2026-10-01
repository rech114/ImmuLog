// SPDX-License-Identifier: Apache-2.0

package feed

import (
	"crypto/ecdh"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func mustIdentity(t *testing.T) *EncIdentity {
	t.Helper()
	id, err := GenerateEncIdentity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestWrapUnwrapRoundTrip(t *testing.T) {
	alice, bob := mustIdentity(t), mustIdentity(t)
	key := make([]byte, KeySize)
	for i := range key {
		key[i] = byte(i)
	}

	wrapped, err := WrapKey(mustPub(t, bob.Public()), key)
	if err != nil {
		t.Fatalf("WrapKey: %v", err)
	}
	got, err := UnwrapKey(bob.priv, wrapped)
	if err != nil {
		t.Fatalf("UnwrapKey: %v", err)
	}
	if string(got) != string(key) {
		t.Fatal("解出来的密钥与原文不符")
	}
	// 不是给 alice 的，alice 解不开
	if _, err := UnwrapKey(alice.priv, wrapped); err == nil {
		t.Fatal("非收件人竟然解开了")
	}
}

// 每次封装都用新的临时密钥对 —— 同一个密钥的两份封装必须互不可比，
// 否则外部一眼就能看出"这两个人是同一个房间的"。
func TestWrapIsUniquePerCall(t *testing.T) {
	bob := mustIdentity(t)
	pub := mustPub(t, bob.Public())
	key := make([]byte, KeySize)

	a, _ := WrapKey(pub, key)
	b, _ := WrapKey(pub, key)
	if a == b {
		t.Fatal("两次封装结果相同 —— 临时密钥没有随机化")
	}
	for _, w := range []string{a, b} {
		if got, err := UnwrapKey(bob.priv, w); err != nil || string(got) != string(key) {
			t.Fatalf("两份封装都该能解开：%v", err)
		}
	}
}

func TestWrapRejectsWrongKeySize(t *testing.T) {
	bob := mustIdentity(t)
	if _, err := WrapKey(mustPub(t, bob.Public()), []byte("too short")); err == nil {
		t.Fatal("密钥长度不对应报错")
	}
}

func TestUnwrapRejectsGarbage(t *testing.T) {
	bob := mustIdentity(t)
	for _, bad := range []string{"", "!!!not-base64!!!", "AAAA", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"} {
		if _, err := UnwrapKey(bob.priv, bad); err == nil {
			t.Fatalf("垃圾输入 %q 不该被接受", bad)
		}
	}
}

func TestSealOpenRoundTrip(t *testing.T) {
	key := make([]byte, KeySize)
	key[0] = 7

	for _, plain := range []string{"", "一句话", "多行\n正文\n\n带空行", strings.Repeat("x", 10000)} {
		ct, err := SealBody(key, plain)
		if err != nil {
			t.Fatalf("SealBody: %v", err)
		}
		if plain != "" && strings.Contains(ct, plain) {
			t.Fatal("密文里能直接看到明文")
		}
		got, err := OpenBody(key, ct)
		if err != nil {
			t.Fatalf("OpenBody: %v", err)
		}
		if got != plain {
			t.Fatalf("往返不符：%q vs %q", got, plain)
		}
	}
}

// AEAD 的意义就在这里：改一个字节就解不开，而不是解出一段垃圾。
func TestOpenRejectsTamperedCiphertext(t *testing.T) {
	key := make([]byte, KeySize)
	ct, _ := SealBody(key, "原文")

	// 在**解码后的字节**上翻位，而不是 base64 字符串
	raw, err := base64.StdEncoding.DecodeString(ct)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-2] ^= 0x01
	tampered := base64.StdEncoding.EncodeToString(raw)

	if _, err := OpenBody(key, tampered); !errors.Is(err, ErrBadCiphertext) {
		t.Fatalf("被改动的密文应返回 ErrBadCiphertext，得到 %v", err)
	}
}

// 密文被截断同样要拒绝，不能解出半截。
func TestOpenRejectsTruncatedCiphertext(t *testing.T) {
	key := make([]byte, KeySize)
	ct, _ := SealBody(key, "一段足够长的原文用来确保有内容可截断")
	raw, _ := base64.StdEncoding.DecodeString(ct)

	for _, cut := range []int{1, len(raw) / 2, len(raw) - 1} {
		short := base64.StdEncoding.EncodeToString(raw[:cut])
		if _, err := OpenBody(key, short); err == nil {
			t.Fatalf("截断到 %d 字节不该解得开", cut)
		}
	}
}

func TestOpenWithWrongKeyFails(t *testing.T) {
	k1, k2 := make([]byte, KeySize), make([]byte, KeySize)
	k2[0] = 1
	ct, _ := SealBody(k1, "原文")
	if _, err := OpenBody(k2, ct); err == nil {
		t.Fatal("换一把密钥不该解得开")
	}
}

// 正文密钥与封装密钥必须由不同的 info 派生，不能互相串用。
func TestBodyAndWrapKeysAreSeparated(t *testing.T) {
	key := make([]byte, KeySize)
	ct, _ := SealBody(key, "原文")

	// 把密文当成"封装"去解，必须失败
	bob := mustIdentity(t)
	if _, err := UnwrapKey(bob.priv, ct); err == nil {
		t.Fatal("正文密文不该能当封装解开")
	}
}

func TestIdentitySeedRoundTrip(t *testing.T) {
	id := mustIdentity(t)
	again, err := ParseEncIdentity(id.Seed())
	if err != nil {
		t.Fatal(err)
	}
	if again.Public() != id.Public() {
		t.Fatal("从种子还原出的公钥不一致")
	}
	if len(id.Seed()) != KeySize {
		t.Fatalf("种子应为 %d 字节", KeySize)
	}
}

func mustPub(t *testing.T, b64 string) *ecdh.PublicKey {
	t.Helper()
	p, err := ParseEncPublic(b64)
	if err != nil {
		t.Fatalf("ParseEncPublic: %v", err)
	}
	return p
}
