// SPDX-License-Identifier: Apache-2.0

// Package feed 是 ImmuLog 的领域层。
//
// 一条 feed 就是一个用户的 append-only 消息链，落地为 refs/feeds/<pub>。
// 每条消息是一个 commit 对象，结构元数据放在 commit trailer 里
// （git 原生语法，人可读、机器可解析）。撤回是一条**追加事件**，
// 原对象永不删除 —— 对应 docs/DESIGN.md §6.1。
//
// 本包不碰 os/exec（只经 gitx），也不碰 HTTP（只被 web 调用）。
package feed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"immulog/core/gitx"
)

// Kind 是消息类型。用 kind 区分事件而不是开多个端点（§7.2）。
type Kind string

const (
	KindMsg     Kind = "msg"
	KindRetract Kind = "retract"
	KindRotate  Kind = "rotate" // 密钥轮换公告
	KindEpoch   Kind = "epoch"  // 加密世代记录
	KindShred   Kind = "shred"  // 丢弃某个世代的密钥
)

// trailer 键名。改这里等于改协议。
const (
	trailerKind     = "ImmuLog-Kind"
	trailerSeq      = "ImmuLog-Seq"
	trailerRetracts = "ImmuLog-Retracts"
	trailerReason   = "ImmuLog-Reason"
	trailerKey      = "ImmuLog-Key"
	trailerEpoch    = "ImmuLog-Epoch"
	trailerEnc      = "ImmuLog-Enc"
)

// MaxBody 是单条消息正文的上限。
const MaxBody = 8192

// MaxReason 是撤回理由的上限。
const MaxReason = 512

// ErrTooLong 表示正文超长。
var ErrTooLong = errors.New("消息过长")

// ErrNoTarget 表示撤回没有指定目标对象。
var ErrNoTarget = errors.New("缺少要撤回的对象")

// ErrBadTarget 表示撤回目标不是一个合法的对象名。
var ErrBadTarget = errors.New("撤回目标格式不合法")

// ErrNotMine 表示撤回的目标不在本机 feed 里 —— 只能撤回自己说过的话。
var ErrNotMine = errors.New("只能撤回自己 feed 里的消息")

// ErrEmpty 表示正文为空。
var ErrEmpty = errors.New("正文不能为空")

// Message 是发往前端的消息形态。
type Message struct {
	OID      string    `json:"oid"`
	Seq      int       `json:"seq"`
	Author   string    `json:"author"`
	Feed     string    `json:"feed,omitempty"`
	Body     string    `json:"body"`
	Sig      string    `json:"sig,omitempty"`
	Key      string    `json:"key,omitempty"` // 签名密钥指纹（短）
	Kind     Kind      `json:"kind"`
	Retracts string    `json:"retracts,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	Epoch    int       `json:"epoch,omitempty"`  // 加密世代；0 表示明文
	Locked   bool      `json:"locked,omitempty"` // 有世代但本机解不开（密钥已丢弃或不是成员）
	At       time.Time `json:"at"`
}

// FeedID 由身份派生 feed 标识。
//
// 输出固定为十六进制，天然是安全的 ref 名 —— 身份里的任何字符都不可能
// 变成路径穿越或 ref 注入的载体。身份变了就是另一条 feed，这正是我们要的。
func FeedID(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:8])
}

// Store 绑定一条 feed。所有写操作在进程内串行 —— 本地只有一个写者，
// 串行化消除自竞争；对外的正确性仍然由 CAS 保证（见 append）。
type Store struct {
	repo *gitx.Repo
	pub  string
	ref  string
	name string
	sign bool

	mu   sync.Mutex
	tree string // 空 tree 的对象名，惰性计算后复用

	// 当前加密世代的缓存。写路径每条消息都要用，
	// 不能每次都去遍历 refs/keys/*（那是 N 次进程调用）。
	epoch    int
	epochKey []byte
}

// New 校验 git 身份并绑定 feed。身份只来自 git 配置，永不来自请求体（§4.2）。
func New(ctx context.Context, repo *gitx.Repo, pub string) (*Store, error) {
	name, _, err := repo.Identity(ctx)
	if err != nil {
		return nil, err
	}
	key, err := repo.SigningKey(ctx)
	if err != nil {
		return nil, err
	}
	return &Store{
		repo: repo,
		pub:  pub,
		ref:  FeedRef(pub),
		name: name,
		sign: key != "",
	}, nil
}

// Pub 返回本 feed 的所有者标识。
func (s *Store) Pub() string { return s.pub }

// FeedRef 返回本 feed 的引用名。
func (s *Store) FeedRef() string { return s.ref }

// Signed 表示本 feed 是否对 commit 签名。
func (s *Store) Signed() bool { return s.sign }

// Send 追加一条普通消息。
func (s *Store) Send(ctx context.Context, body string) (Message, error) {
	if strings.TrimSpace(body) == "" {
		return Message{}, ErrEmpty
	}
	if len(body) > MaxBody {
		return Message{}, ErrTooLong
	}
	return s.append(ctx, KindMsg, body, "", "")
}

// Retract 追加一条撤回事件。原消息对象不会被删除。
//
// 只允许撤回**自己 feed 里**的消息：目标必须是本机链尾的祖先。
// 这条规则让「撤回」成为作者的权利，而不是任何人都能对别人做的事。
func (s *Store) Retract(ctx context.Context, oid, reason string) (Message, error) {
	if oid == "" {
		return Message{}, ErrNoTarget
	}
	if !isOID(oid) {
		return Message{}, ErrBadTarget
	}
	if len(reason) > MaxReason {
		reason = reason[:MaxReason]
	}

	s.mu.Lock()
	tip, err := s.repo.Resolve(ctx, s.ref)
	s.mu.Unlock()
	if err != nil {
		return Message{}, err
	}
	if tip == "" {
		return Message{}, ErrNotMine
	}
	// 失败即拒绝：对象不存在（IsAncestor 会报 128）或不是祖先，都算"不是你的"
	exists, err := s.repo.Exists(ctx, oid)
	if err != nil {
		return Message{}, err
	}
	if !exists {
		return Message{}, ErrNotMine
	}
	ok, err := s.repo.IsAncestor(ctx, oid, tip)
	if err != nil {
		return Message{}, err
	}
	if !ok {
		return Message{}, ErrNotMine
	}

	return s.append(ctx, KindRetract, "", oid, reason)
}

// History 读回最近的若干条消息（一次进程调用）。
func (s *Store) History(ctx context.Context, limit int) ([]Message, error) {
	raw, err := s.repo.Log(ctx, s.ref, limit)
	if err != nil {
		return nil, err
	}
	return Decode(raw, s.ref, s.OpenBody), nil
}

// Tip 返回当前链尾；空 feed 返回空串。
func (s *Store) Tip(ctx context.Context) (string, error) {
	return s.repo.Resolve(ctx, s.ref)
}

// ── 内部 ──────────────────────────────────────────────────────────

func (s *Store) append(ctx context.Context, kind Kind, body, retracts, reason string) (Message, error) {
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

	env := envelope{kind: kind, body: body, seq: seq, retracts: retracts, reason: reason}

	// 有当前世代就把正文加密。密文进 git 对象（可自由复制），
	// 明文密钥只在本机 —— 这条分界线就是整个阶段的全部意义。
	if n, key := s.currentEpochLocked(ctx); n > 0 && key != nil {
		ct, err := SealBody(key, body)
		if err != nil {
			return Message{}, err
		}
		env.body, env.epoch = ct, n
	}
	// 每条消息捎带自己的加密公钥，让别处知道该封装给谁
	if me, err := LoadIdentity(s.repo.Dir); err == nil {
		env.enc = me.Public()
	}

	oid, err := s.repo.Commit(ctx, tree, tip, render(env), s.sign)
	if err != nil {
		return Message{}, err
	}

	// CAS：old 与实际不符即中止。这是篡改检测的信号源，必须原样上报。
	if err := s.repo.UpdateRef(ctx, s.ref, oid, tip); err != nil {
		return Message{}, err
	}
	// 我们自己刚造的 commit，按构造必以旧 tip 为祖先 —— 直接推进见证锚。
	_ = s.advanceWitness(ctx, oid)

	return Message{
		OID: oid, Seq: seq, Author: s.name, Body: body,
		Kind: kind, Retracts: retracts, Reason: reason, Epoch: env.epoch,
		At: time.Now().UTC(),
	}, nil
}

// currentEpochLocked 返回当前世代及其明文密钥（0, nil 表示未启用加密）。
// 调用方必须持有 s.mu。
func (s *Store) currentEpochLocked(ctx context.Context) (int, []byte) {
	if s.epoch > 0 {
		return s.epoch, s.epochKey
	}
	n, err := s.CurrentEpoch(ctx)
	if err != nil {
		return 0, nil
	}
	key, err := s.OpenEpoch(ctx, n)
	if err != nil {
		return 0, nil
	}
	s.epoch, s.epochKey = n, key
	return n, key
}

func (s *Store) nextSeq(ctx context.Context, tip string) (int, error) {
	if tip == "" {
		return 1, nil
	}
	if raw, err := s.repo.Log(ctx, s.ref, 1); err != nil {
		return 0, err
	} else if len(raw) > 0 {
		if n, err := strconv.Atoi(raw[0].Seq); err == nil && n > 0 {
			return n + 1, nil
		}
	}
	// 外来 commit 没有 Seq trailer：退回按链长计数
	n, err := s.repo.Count(ctx, s.ref)
	return n + 1, err
}

func (s *Store) emptyTree(ctx context.Context) (string, error) {
	if s.tree != "" {
		return s.tree, nil
	}
	t, err := s.repo.EmptyTree(ctx)
	if err != nil {
		return "", err
	}
	s.tree = t
	return t, nil
}

// envelope 是一条待编码的消息。用结构体而不是六个位置参数 ——
// 加了 epoch / enc 之后位置参数已经数不清了。
type envelope struct {
	kind     Kind
	body     string // 明文，或已加密的密文
	seq      int
	retracts string
	reason   string
	epoch    int    // 0 = 明文
	enc      string // 本机加密公钥（启用加密时非空）
}

// render 把一条消息编码成 commit message。
//
// 正文与 trailer 块之间永远隔一个空行，且正文的尾部空白被清掉 ——
// 于是解码时"最后一个空行之前"就是正文，边界确定。
// git 的 trailer 解析取**最后一次**出现，因此正文里伪造的同名 trailer 不会生效。
//
// 首段绝不能为空：否则 commit message 以空行开头，git 的 trailer 解析会失效。
//
// ⚠️ trailer 值必须过 sanitizeValue：值里一个换行就能凭空造出
// `ImmuLog-Seq: 999` 这种伪行，而 git 取最后一次出现 —— 伪造的会赢。
func render(env envelope) string {
	var head string
	switch env.kind {
	case KindRetract:
		head = sanitizeValue(env.reason)
		if head == "" {
			head = "撤回 " + short(oidOr(env.retracts))
		}
	default:
		head = sanitizeText(env.body) // 正文保留换行，多行消息是合法的
	}
	if head == "" {
		head = "（空消息）"
	}

	var b strings.Builder
	b.WriteString(head)
	b.WriteString("\n\n")
	b.WriteString(trailerKind + ": " + string(env.kind) + "\n")
	b.WriteString(trailerSeq + ": " + strconv.Itoa(env.seq) + "\n")
	if env.retracts != "" {
		b.WriteString(trailerRetracts + ": " + sanitizeValue(env.retracts) + "\n")
	}
	if env.reason != "" {
		b.WriteString(trailerReason + ": " + sanitizeValue(env.reason) + "\n")
	}
	if env.epoch > 0 {
		b.WriteString(trailerEpoch + ": " + strconv.Itoa(env.epoch) + "\n")
	}
	if env.enc != "" {
		b.WriteString(trailerEnc + ": " + sanitizeValue(env.enc) + "\n")
	}
	return b.String()
}

// BodyOpener 解密某个世代的密文。nil 表示只处理明文。
type BodyOpener func(epoch int, ciphertext string) (string, error)

// Decode 把 gitx 的原始提交翻译成领域消息。
//
// feedRef 决定归属 —— 撤回只对**同一条 feed** 里的消息生效（见 docs/DESIGN.md §6.1）。
// open 为 nil 或解不开时，消息会被标记为 Locked：**密钥丢了不等于消息不存在**，
// 链上的位置、作者、时间全都还在，只是正文不再可读。
func Decode(raw []gitx.RawCommit, feedRef string, open BodyOpener) []Message {
	out := make([]Message, 0, len(raw))
	for _, r := range raw {
		seq, _ := strconv.Atoi(r.Seq)
		kind := Kind(r.Kind)
		if kind == "" {
			kind = KindMsg
			if r.Retracts != "" {
				kind = KindRetract
			}
		}
		epoch, _ := strconv.Atoi(r.Epoch)

		m := Message{
			OID:      r.OID,
			Seq:      seq,
			Author:   r.Author,
			Feed:     feedRef,
			Body:     bodyOf(r.Body),
			Sig:      sigLabel(r.Sig),
			Key:      ShortKey(r.Key),
			Kind:     kind,
			Retracts: r.Retracts,
			Reason:   r.Reason,
			Epoch:    epoch,
			At:       r.At,
		}
		if epoch > 0 {
			plain, err := "", ErrNoKey
			if open != nil {
				plain, err = open(epoch, m.Body)
			}
			if err != nil {
				m.Locked, m.Body = true, ""
			} else {
				m.Body = plain
			}
		}
		out = append(out, m)
	}
	return out
}

// OpenBody 实现 BodyOpener：用本机保管的世代密钥解密。
func (s *Store) OpenBody(epoch int, ciphertext string) (string, error) {
	key, err := s.OpenEpoch(context.Background(), epoch)
	if err != nil {
		return "", err
	}
	return OpenBody(key, ciphertext)
}

// sigLabel 把 git 的签名状态翻成给人看的话。空串表示没签名。
func sigLabel(status string) string {
	switch status {
	case "good":
		return "签名有效"
	case "untrusted":
		return "签名有效（密钥未知）"
	case "bad":
		return "签名损坏"
	default:
		return ""
	}
}

// ShortKey 只取指纹尾部，够人眼区分即可。
func ShortKey(k string) string {
	if len(k) > 16 {
		return k[len(k)-16:]
	}
	return k
}

// bodyOf 剥掉 commit message 末尾的 trailer 块，返回纯净正文。
func bodyOf(raw string) string {
	s := strings.TrimRight(strings.ReplaceAll(raw, "\r\n", "\n"), " \t\n")
	if s == "" {
		return ""
	}
	if i := strings.LastIndex(s, "\n\n"); i >= 0 {
		tail := s[i+2:]
		if looksLikeTrailers(tail) {
			return s[:i]
		}
	}
	return s
}

func looksLikeTrailers(block string) bool {
	for _, line := range strings.Split(block, "\n") {
		if line == "" {
			continue
		}
		k, _, ok := strings.Cut(line, ":")
		if !ok || !strings.HasPrefix(k, "ImmuLog-") {
			return false
		}
	}
	return true
}

// sanitizeText 清掉会破坏记录分隔的控制字符，并去掉尾部空白。
// **保留换行** —— 多行正文是合法的。
func sanitizeText(s string) string {
	s = strings.ReplaceAll(s, "\x00", "")
	s = strings.ReplaceAll(s, "\x1f", "")
	s = strings.ReplaceAll(s, "\x1e", "")
	return strings.TrimRight(s, " \t\r\n")
}

// sanitizeValue 把内容压成单行，供 trailer 值使用。
//
// 这是条安全边界：trailer 值里只要有换行，就能凭空造出一行
// `ImmuLog-Seq: 999`；而 git 的 trailer 解析取**最后一次**出现，
// 于是伪造的会覆盖真的。压成单行即可根除。
func sanitizeValue(s string) string {
	s = sanitizeText(s)
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

// isOID 判断是否为 40 位十六进制对象名。
// 撤回目标来自请求体，必须严格校验 —— 否则又是一个注入面。
func isOID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

func oidOr(s string) string {
	if s == "" {
		return "?"
	}
	return s
}

func short(oid string) string {
	if len(oid) > 6 {
		return oid[:6]
	}
	return oid
}
