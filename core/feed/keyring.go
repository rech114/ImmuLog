// SPDX-License-Identifier: Apache-2.0

package feed

// keyring.go —— 本机的密钥保管。
//
// 这里是 crypto-shredding 唯一真正落地的地方：**明文 epoch 密钥只存在本机**，
// 不进 git 对象库、不进任何 ref、不参与同步。
// 「丢弃密钥」= 把本机这份覆写后删掉。
//
// 诚实的天花板（docs/DESIGN.md §6.4）：
//   · 别人手里可能还留着副本 —— 丢弃只在**所有持有者都照做**时才是彻底的
//   · 闪存/文件系统的覆写不保证物理擦除
// 所以这件事的强度上限是「保管纪律」，不是「密码学」。

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// keysDir 是密钥目录名，位于仓库目录内但不在 git 管辖的命名空间里。
const keysDir = "immulog-keys"

// identityFile 保存本机的 X25519 私钥种子。
const identityFile = "identity"

// 文件权限：只有本用户可读写。
const keyPerm = 0o600

// ErrKeysMissing 表示本机没有任何密钥材料。
var ErrKeysMissing = errors.New("本机没有加密身份")

// ErrShredded 表示这个世代的密钥已被本机**主动丢弃**。
//
// 它和 ErrNoKey 是两回事：ErrNoKey 是"没拿到"，ErrShredded 是"拿到了又扔了"。
// 这个区分是整个 crypto-shredding 的支点 —— 见 ShredEpochKey 的注释。
var ErrShredded = errors.New("该世代的密钥已被主动丢弃")

// keyPath 返回仓库内的密钥目录。不存在时按需创建。
func keyPath(repoDir string) (string, error) {
	dir := filepath.Join(repoDir, keysDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func epochFile(dir string, n int) string {
	return filepath.Join(dir, fmt.Sprintf("epoch-%d", n))
}

// shredMarker 是"这个世代被我丢掉了"的持久标记。
//
// 为什么需要它：链上存着封装给我们的那份密钥，所以只要还留着身份，
// **删掉本地文件之后随时能从链上重新解开** —— 那删除就只是个假动作。
// 丢弃必须是一个**决定**，而不是一次文件删除。
func shredMarker(dir string, n int) string {
	return filepath.Join(dir, fmt.Sprintf("shredded-%d", n))
}

// IsShredded 判断本机是否已主动丢弃某个世代。
func IsShredded(repoDir string, n int) bool {
	dir, err := keyPath(repoDir)
	if err != nil {
		return false
	}
	_, err = os.Stat(shredMarker(dir, n))
	return err == nil
}

// LoadIdentity 读取本机加密身份；不存在返回 ErrKeysMissing。
func LoadIdentity(repoDir string) (*EncIdentity, error) {
	dir, err := keyPath(repoDir)
	if err != nil {
		return nil, err
	}
	seed, err := os.ReadFile(filepath.Join(dir, identityFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrKeysMissing
	}
	if err != nil {
		return nil, err
	}
	return ParseEncIdentity(seed)
}

// SaveIdentity 落盘本机加密身份（已存在则覆盖）。
func SaveIdentity(repoDir string, id *EncIdentity) error {
	dir, err := keyPath(repoDir)
	if err != nil {
		return err
	}
	return writeSecret(filepath.Join(dir, identityFile), id.Seed())
}

// LoadEpochKey 读取某个 epoch 的明文密钥。
// 已被丢弃时返回 ErrShredded；从未持有过返回 ErrNoKey。
func LoadEpochKey(repoDir string, n int) ([]byte, error) {
	if IsShredded(repoDir, n) {
		return nil, ErrShredded
	}
	dir, err := keyPath(repoDir)
	if err != nil {
		return nil, err
	}
	key, err := os.ReadFile(epochFile(dir, n))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoKey
	}
	if err != nil {
		return nil, err
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("epoch %d 的密钥长度不对（%d 字节）", n, len(key))
	}
	return key, nil
}

// SaveEpochKey 落盘某个 epoch 的明文密钥。
func SaveEpochKey(repoDir string, n int, key []byte) error {
	if len(key) != KeySize {
		return fmt.Errorf("密钥应为 %d 字节", KeySize)
	}
	if IsShredded(repoDir, n) {
		// 已经决定丢弃的世代，不许再存回来 —— 否则丢弃会被悄悄撤销
		return ErrShredded
	}
	dir, err := keyPath(repoDir)
	if err != nil {
		return err
	}
	return writeSecret(epochFile(dir, n), key)
}

// ShredEpochKey 覆写并删除某个 epoch 的明文密钥，并留下持久标记。
//
// 三步缺一不可：
//  1. 覆写 —— 对抗「删了但没真删」这类恢复手段
//  2. 删除
//  3. **留下标记** —— 否则链上的封装会让密钥被自动解回来，删除变成假动作
//
// 它对日志型文件系统和闪存的磨损均衡**不构成物理擦除保证** —— 见文件头。
func ShredEpochKey(repoDir string, n int) error {
	dir, err := keyPath(repoDir)
	if err != nil {
		return err
	}
	path := epochFile(dir, n)

	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		// 没有密钥文件：要么已经丢过（标记在），要么从来没有
		if IsShredded(repoDir, n) {
			return nil
		}
		return ErrNoKey
	}
	if err := writeSecret(path, make([]byte, KeySize)); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return writeSecret(shredMarker(dir, n), []byte("1"))
}

// HasEpochKey 判断本机是否还持有某个 epoch 的明文密钥。
func HasEpochKey(repoDir string, n int) bool {
	_, err := LoadEpochKey(repoDir, n)
	return err == nil
}

// HasIdentity 判断本机是否已有加密身份。
func HasIdentity(repoDir string) bool {
	_, err := LoadIdentity(repoDir)
	return err == nil
}

// writeSecret 以 0600 原子落盘。
func writeSecret(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, keyPerm); err != nil {
		return err
	}
	if err := os.Chmod(tmp, keyPerm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
