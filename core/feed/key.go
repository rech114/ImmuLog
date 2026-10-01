// SPDX-License-Identifier: Apache-2.0

package feed

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"immulog/core/gitx"
)

// ErrNoSigningKey 表示仓库没配签名密钥。
var ErrNoSigningKey = errors.New("未配置 user.signingkey")

// ErrSigningBroken 表示配了密钥但签不出来 —— 绝不静默降级成不签名。
var ErrSigningBroken = errors.New("签名密钥不可用")

// 判定结果的原因码（密钥链条）。
const ReasonKeyChanged = "keychain"

// ProbeSigning 真签一次，确认密钥真的可用，并返回其指纹。
//
// 配置了 user.signingkey 却签不出来（密钥文件不存在 / 权限不对 /
// gpg 未安装）是最容易发生的事故：如果静默降级，用户会以为自己在签名，
// 实际上没有。**这里选择失败即报错，不假装安全。**
func ProbeSigning(ctx context.Context, repo *gitx.Repo) (string, error) {
	key, err := repo.SigningKey(ctx)
	if err != nil {
		return "", err
	}
	if key == "" {
		return "", ErrNoSigningKey
	}
	tree, err := repo.EmptyTree(ctx)
	if err != nil {
		return "", err
	}
	// 会产生一个游离对象，无所谓 —— 换来的是"签名真的能用"这个确定性
	oid, err := repo.Commit(ctx, tree, "", "ImmuLog 签名自检\n", true)
	if err != nil {
		return "", errors.Join(ErrSigningBroken, err)
	}
	c, err := KeyAt(ctx, repo, oid)
	if err != nil {
		return "", err
	}
	if c.Key == "" {
		return "", ErrSigningBroken
	}
	return c.Key, nil
}

// KeyAt 读某个提交的签名密钥指纹；未签名或对象不存在时返回空串。
func KeyAt(ctx context.Context, repo *gitx.Repo, oid string) (gitx.RawCommit, error) {
	if oid == "" {
		return gitx.RawCommit{}, nil
	}
	raw, err := repo.Log(ctx, oid, 1)
	if err != nil || len(raw) == 0 {
		return gitx.RawCommit{}, err
	}
	return raw[0], nil
}

// CheckKeyChain 校验一段新提交里的密钥链条是否连续。
//
// 规则只有一条：
//
//	密钥可以变 —— 但**只有当上一条提交是一条「轮换公告」，
//	且明文声明了新密钥**时才算合法。
//
// 于是「换密钥」和「换人」被区分开：合法轮换一定留痕，
// 没留痕的密钥变更就是攻击。
//
// prev 是这段之前那一条提交（本地链尾）。它是零值时表示这是首次接触，
// 此时首个密钥就是初始密钥（TOFU）。
//
// ⚠️ 一个诚实的边界：若 `prev.Key` 为空（历史全是未签名的），
// 首次出现的签名密钥无法被任何东西背书 —— 未签名的前缀本来就保护不了。
// 这种情况会被允许，但调用方应当把它当作"从此才开始可验证"。
func CheckKeyChain(prev gitx.RawCommit, raw []gitx.RawCommit) (Verdict, error) {
	carry := prev
	for _, c := range raw {
		if c.Key != carry.Key {
			// 未签名的前缀本来就保护不了：首次出现的密钥只能被接受，
			// 调用方应据此把它标注为"从此才开始可验证"。
			unsignedPrefix := carry.Key == ""
			declared := carry.Declared != "" && carry.Declared == c.Key
			if !unsignedPrefix && !declared {
				return Verdict{
					Reason:  ReasonKeyChanged,
					Witness: carry.Key,
					Current: c.Key,
				}, nil
			}
		}
		carry = c
	}
	return Verdict{OK: true}, nil
}

// CurrentKey 返回本机 feed 链尾的签名密钥指纹；未签名时为空。
func (s *Store) CurrentKey(ctx context.Context) (string, error) {
	tip, err := s.repo.Resolve(ctx, s.ref)
	if err != nil || tip == "" {
		return "", err
	}
	c, err := KeyAt(ctx, s.repo, tip)
	if err != nil {
		return "", err
	}
	return c.Key, nil
}

// DeclareKey 追加一条「密钥轮换公告」。
//
// **它用当前（旧）密钥签名，并在 trailer 里声明新密钥的指纹。**
// 之后的消息再用新密钥签 —— 链条因此连续且可审计。
//
// 顺序很重要：**先公告，再换配置**。
//
//	git config user.signingkey <新密钥>   ← 不要先做这一步
//	ImmuLog 先调用 DeclareKey(新指纹)
//	然后才把 user.signingkey 指过去
//
// 为什么不让调用方直接传旧密钥路径去签：git 的 ssh 签名里 `-S<keyid>`
// 解析的是密钥引用而不是指纹，跨密钥签名没有可移植的写法。
// 因此把顺序约束显式写在这里，而不是假装能自动处理。
func (s *Store) DeclareKey(ctx context.Context, newKey string) (Message, error) {
	newKey = strings.TrimSpace(newKey)
	if newKey == "" {
		return Message{}, errors.New("新密钥不能为空")
	}
	if !s.sign {
		return Message{}, ErrNoSigningKey
	}

	tip, err := s.repo.Resolve(ctx, s.ref)
	if err != nil {
		return Message{}, err
	}
	oldKey, err := s.CurrentKey(ctx)
	if err != nil {
		return Message{}, err
	}
	if oldKey == "" {
		return Message{}, ErrNoSigningKey
	}
	if oldKey == newKey {
		return Message{}, errors.New("新旧密钥相同")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	seq, err := s.nextSeq(ctx, tip)
	if err != nil {
		return Message{}, err
	}
	tree, err := s.emptyTree(ctx)
	if err != nil {
		return Message{}, err
	}

	body := renderRotate(seq, newKey)
	oid, err := s.repo.Commit(ctx, tree, tip, body, true)
	if err != nil {
		return Message{}, err
	}
	if err := s.repo.UpdateRef(ctx, s.ref, oid, tip); err != nil {
		return Message{}, err
	}
	_ = s.advanceWitness(ctx, oid)

	return Message{
		OID: oid, Seq: seq, Author: s.name, Feed: s.ref,
		Kind: KindRotate, Key: ShortKey(newKey), At: time.Now().UTC(),
	}, nil
}

// renderRotate 生成轮换公告的 commit message。
func renderRotate(seq int, newKey string) string {
	var b strings.Builder
	b.WriteString("密钥轮换公告\n\n")
	b.WriteString(trailerKind + ": " + string(KindRotate) + "\n")
	b.WriteString(trailerSeq + ": " + strconv.Itoa(seq) + "\n")
	b.WriteString(trailerKey + ": " + sanitizeValue(newKey) + "\n")
	return b.String()
}
