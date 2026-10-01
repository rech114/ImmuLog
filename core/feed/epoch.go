// SPDX-License-Identifier: Apache-2.0

package feed

// epoch.go —— 加密世代的链上记录与生命周期。
//
// 一个 epoch 就是一个对称密钥覆盖的一段消息区间。密钥**不在**这里 ——
// 这里只有「用每个收件人的公钥各封一份」的结果，写进 refs/keys/<n>。
// 明文密钥只在本机（见 keyring.go）。
//
// 于是同一份密文可以随仓库自由复制，而只有成员解得开，
// 并且**新成员拿不到他加入之前的 epoch** —— 这一条不需要任何人配合。

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"immulog/core/gitx"
)

// KeysPrefix 是 epoch 密钥记录的命名空间。
const KeysPrefix = "refs/keys/"

// KeyRef 返回某个 epoch 的密钥记录引用。
func KeyRef(n int) string { return KeysPrefix + strconv.Itoa(n) }

// maxMemberScan 是收集成员公钥时最多回溯多少条历史。
const maxMemberScan = 500

// Epoch 是一个加密世代。
type Epoch struct {
	N       int       `json:"n"`
	Members int       `json:"members"`
	OID     string    `json:"oid,omitempty"`
	At      time.Time `json:"at"`
	// Held 表示本机还持有这个 epoch 的明文密钥。
	Held bool `json:"held"`
}

// ErrNoEpoch 表示链上还没有任何 epoch。
var ErrNoEpoch = errors.New("尚未建立加密世代")

// ── 建立 ──────────────────────────────────────────────────────────

// SetupEncryption 确保本机有加密身份，并且至少有一个 epoch。
//
// 幂等：已经有身份和 epoch 时什么都不做。
func (s *Store) SetupEncryption(ctx context.Context) error {
	if _, err := LoadIdentity(s.repo.Dir); errors.Is(err, ErrKeysMissing) {
		id, err := GenerateEncIdentity()
		if err != nil {
			return err
		}
		if err := SaveIdentity(s.repo.Dir, id); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	if _, err := s.CurrentEpoch(ctx); errors.Is(err, ErrNoEpoch) {
		_, err = s.RotateEpoch(ctx, nil)
		return err
	} else if err != nil {
		return err
	}
	return nil
}

// CurrentEpoch 返回链上最新的 epoch 编号；未启用加密时返回 ErrNoEpoch。
func (s *Store) CurrentEpoch(ctx context.Context) (int, error) {
	epochs, err := s.Epochs(ctx)
	if err != nil {
		return 0, err
	}
	if len(epochs) == 0 {
		return 0, ErrNoEpoch
	}
	return epochs[len(epochs)-1].N, nil
}

// Epochs 列出链上全部 epoch 记录，按编号升序。
func (s *Store) Epochs(ctx context.Context) ([]Epoch, error) {
	refs, err := s.repo.Refs(ctx, KeysPrefix)
	if err != nil {
		return nil, err
	}
	var out []Epoch
	for _, r := range refs {
		n, err := strconv.Atoi(strings.TrimPrefix(r.Name, KeysPrefix))
		if err != nil {
			continue
		}
		e := Epoch{N: n, OID: r.OID, Held: HasEpochKey(s.repo.Dir, n)}
		if raw, err := s.repo.Log(ctx, r.Name, 1); err == nil && len(raw) > 0 {
			e.At = raw[0].At
			e.Members = countRecipients(raw[0].Body)
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].N < out[j].N })
	return out, nil
}

// ── 轮换 ──────────────────────────────────────────────────────────

// RotateEpoch 建立一个新的 epoch，把密钥封装给每个收件人（含本机）。
//
// recipients 是 base64 编码的 X25519 公钥。为空时只封装给本机 ——
// 那就是一个只有自己能读的世代。
func (s *Store) RotateEpoch(ctx context.Context, recipients []string) (Epoch, error) {
	me, err := LoadIdentity(s.repo.Dir)
	if err != nil {
		return Epoch{}, err
	}

	// 去重 + 必须包含自己，否则轮换完自己都读不了
	set := map[string]bool{me.Public(): true}
	for _, r := range recipients {
		r = strings.TrimSpace(r)
		if r != "" {
			set[r] = true
		}
	}
	pubs := make([]string, 0, len(set))
	for p := range set {
		pubs = append(pubs, p)
	}
	sort.Strings(pubs) // 确定性：同样的成员集合产生同样的记录

	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return Epoch{}, err
	}

	var body strings.Builder
	for _, pub := range pubs {
		recipient, err := ParseEncPublic(pub)
		if err != nil {
			return Epoch{}, fmt.Errorf("收件人公钥无效（%s…）: %w", short(pub), err)
		}
		wrapped, err := WrapKey(recipient, key)
		if err != nil {
			return Epoch{}, err
		}
		body.WriteString(pub + " " + wrapped + "\n")
	}

	n := 1
	if cur, err := s.CurrentEpoch(ctx); err == nil {
		n = cur + 1
	} else if !errors.Is(err, ErrNoEpoch) {
		return Epoch{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// 世代记录串成链：前驱是**上一个世代**的记录（n=1 时才是根）
	prev := ""
	if n > 1 {
		prev, err = s.repo.Resolve(ctx, KeyRef(n-1))
		if err != nil {
			return Epoch{}, err
		}
	}
	tree, err := s.emptyTree(ctx)
	if err != nil {
		return Epoch{}, err
	}
	text := body.String() + "\n" + trailerKind + ": " + string(KindEpoch) + "\n" +
		trailerEpoch + ": " + strconv.Itoa(n) + "\n"

	oid, err := s.repo.Commit(ctx, tree, prev, text, s.sign)
	if err != nil {
		return Epoch{}, err
	}
	// refs/keys/<n> 是**新建**引用：用 EnsureRef（old 为全零）而不是普通 CAS，
	// 这样它已存在时会拒绝，而不是被覆盖。
	if err := s.repo.EnsureRef(ctx, KeyRef(n), oid); err != nil {
		return Epoch{}, err
	}
	// 明文密钥只落本机 —— 这一步是「密文可广传、密钥不可」的分界线
	if err := SaveEpochKey(s.repo.Dir, n, key); err != nil {
		return Epoch{}, err
	}
	// 写路径的缓存换新。**这里必须直接赋值**：本函数已经持有 s.mu，
	// 再 Lock 一次就是自锁死。
	s.epoch, s.epochKey = n, key

	return Epoch{N: n, Members: len(pubs), OID: oid, At: time.Now().UTC(), Held: true}, nil
}

// ── 丢弃 ──────────────────────────────────────────────────────────

// ShredEpoch 丢弃本机持有的某个 epoch 的明文密钥，并在链上留下公告。
//
// **它做不到什么，必须说清楚**：它只丢本机这一份。
// 别人手里的副本不会因此消失 —— 除非每个持有者都照做。
// 公告的意义是让这件事**可被审计**：任何人都能看到"谁在何时丢弃了哪个世代"。
func (s *Store) ShredEpoch(ctx context.Context, n int) (Message, error) {
	if err := ShredEpochKey(s.repo.Dir, n); err != nil {
		return Message{}, err
	}
	// 如果丢的正是当前世代，缓存必须失效，否则后续还会拿旧密钥加密
	s.mu.Lock()
	if s.epoch == n {
		s.epoch, s.epochKey = 0, nil
	}
	s.mu.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()

	tip, err := s.repo.Resolve(ctx, s.ref)
	if err != nil {
		return Message{}, err
	}
	seq, err := s.nextSeq(ctx, tip)
	if err != nil {
		return Message{}, err
	}
	tree, err := s.emptyTree(ctx)
	if err != nil {
		return Message{}, err
	}

	var b strings.Builder
	b.WriteString("丢弃 epoch " + strconv.Itoa(n) + " 的密钥\n\n")
	b.WriteString(trailerKind + ": " + string(KindShred) + "\n")
	b.WriteString(trailerSeq + ": " + strconv.Itoa(seq) + "\n")
	b.WriteString(trailerEpoch + ": " + strconv.Itoa(n) + "\n")

	oid, err := s.repo.Commit(ctx, tree, tip, b.String(), s.sign)
	if err != nil {
		return Message{}, err
	}
	if err := s.repo.UpdateRef(ctx, s.ref, oid, tip); err != nil {
		return Message{}, err
	}
	_ = s.advanceWitness(ctx, oid)

	return Message{
		OID: oid, Seq: seq, Author: s.name, Feed: s.ref,
		Kind: KindShred, Epoch: n, At: time.Now().UTC(),
	}, nil
}

// ── 收件人与解密 ──────────────────────────────────────────────────

// KnownRecipients 从近期历史里收集加密公钥（含本机）。
//
// 只看最近 maxMemberScan 条：一个从不发言的成员会被漏掉，
// 这是有意的取舍 —— 否则每轮轮换都要遍历整条链。
func (s *Store) KnownRecipients(ctx context.Context) ([]string, error) {
	me, err := LoadIdentity(s.repo.Dir)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{me.Public(): true}

	raw, err := s.repo.Log(ctx, s.ref, maxMemberScan)
	if err != nil {
		return nil, err
	}
	for _, r := range raw {
		if p := strings.TrimSpace(r.Enc); p != "" {
			set[p] = true
		}
	}

	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// OpenEpoch 取本机某个 epoch 的明文密钥。
func (s *Store) OpenEpoch(ctx context.Context, n int) ([]byte, error) {
	return openEpochKey(ctx, s.repo, n)
}

// openEpochKey 取某个世代的明文密钥：先用本机保管的那份，
// 本机没有再从 refs/keys/<n> 里找封装给自己的那份解开。
//
// 解开之后就存下来，所以同一个世代只有第一条消息付 X25519 的代价。
func openEpochKey(ctx context.Context, repo *gitx.Repo, n int) ([]byte, error) {
	// 丢弃是一个**决定**：链上还留着封装给我们的那份，所以必须在这里挡住，
	// 否则删掉本地文件之后密钥会被自动解回来，删除就成了假动作。
	if IsShredded(repo.Dir, n) {
		return nil, ErrShredded
	}
	if key, err := LoadEpochKey(repo.Dir, n); err == nil {
		return key, nil
	}
	me, err := LoadIdentity(repo.Dir)
	if err != nil {
		return nil, ErrNoKey
	}
	raw, err := repo.Log(ctx, KeyRef(n), 1)
	if err != nil || len(raw) == 0 {
		return nil, ErrNoKey
	}
	mine := me.Public()
	for _, line := range strings.Split(bodyOf(raw[0].Body), "\n") {
		pub, wrapped, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || pub != mine {
			continue
		}
		key, err := UnwrapKey(me.priv, wrapped)
		if err != nil {
			return nil, err
		}
		_ = SaveEpochKey(repo.Dir, n, key)
		return key, nil
	}
	return nil, ErrNotRecipient
}

// Opener 用仓库构造一个解密器，供 Sync 在解码外来消息时使用。
func Opener(ctx context.Context, repo *gitx.Repo) BodyOpener {
	return func(epoch int, ciphertext string) (string, error) {
		key, err := openEpochKey(ctx, repo, epoch)
		if err != nil {
			return "", err
		}
		return OpenBody(key, ciphertext)
	}
}

// countRecipients 数一个密钥记录里封装了几份。
func countRecipients(body string) int {
	n := 0
	for _, line := range strings.Split(bodyOf(body), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}
