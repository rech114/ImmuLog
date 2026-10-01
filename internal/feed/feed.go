// Package feed 是 Immutalk 的领域层。
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

	"immutalk/internal/gitx"
)

// Kind 是消息类型。用 kind 区分事件而不是开多个端点（§7.2）。
type Kind string

const (
	KindMsg     Kind = "msg"
	KindRetract Kind = "retract"
)

// trailer 键名。改这里等于改协议。
const (
	trailerKind     = "Immutalk-Kind"
	trailerSeq      = "Immutalk-Seq"
	trailerRetracts = "Immutalk-Retracts"
	trailerReason   = "Immutalk-Reason"
)

// MaxBody 是单条消息正文的上限。
const MaxBody = 8192

// ErrTooLong 表示正文超长。
var ErrTooLong = errors.New("消息过长")

// ErrNoTarget 表示撤回没有指定目标对象。
var ErrNoTarget = errors.New("缺少要撤回的对象")

// ErrEmpty 表示正文为空。
var ErrEmpty = errors.New("正文不能为空")

// Message 是发往前端的消息形态。
type Message struct {
	OID      string    `json:"oid"`
	Seq      int       `json:"seq"`
	Author   string    `json:"author"`
	Body     string    `json:"body"`
	Sig      string    `json:"sig,omitempty"`
	Kind     Kind      `json:"kind"`
	Retracts string    `json:"retracts,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	At       time.Time `json:"at"`
}

// Ref 返回某个用户的 feed 引用名。
func Ref(pub string) string { return "refs/feeds/" + pub }

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
		ref:  Ref(pub),
		name: name,
		sign: key != "",
	}, nil
}

// Pub 返回本 feed 的所有者标识。
func (s *Store) Pub() string { return s.pub }

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
func (s *Store) Retract(ctx context.Context, oid, reason string) (Message, error) {
	if oid == "" {
		return Message{}, ErrNoTarget
	}
	if len(reason) > 512 {
		reason = reason[:512]
	}
	return s.append(ctx, KindRetract, "", oid, reason)
}

// History 读回最近的若干条消息（一次进程调用）。
func (s *Store) History(ctx context.Context, limit int) ([]Message, error) {
	raw, err := s.repo.Log(ctx, s.ref, limit)
	if err != nil {
		return nil, err
	}
	return decode(raw, s.name, s.sign), nil
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

	oid, err := s.repo.Commit(ctx, tree, tip, render(kind, body, seq, retracts, reason), s.sign)
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
		Kind: kind, Retracts: retracts, Reason: reason,
		At: time.Now().UTC(),
	}, nil
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

// render 把一条消息编码成 commit message。
//
// 正文与 trailer 块之间永远隔一个空行，且正文的尾部空白被清掉 ——
// 于是解码时"最后一个空行之前"就是正文，边界确定。
// git 的 trailer 解析取**最后一次**出现，因此正文里伪造的同名 trailer 不会生效。
//
// 首段绝不能为空：否则 commit message 以空行开头，git 的 trailer 解析会失效。
func render(kind Kind, body string, seq int, retracts, reason string) string {
	var head string
	if kind == KindRetract {
		head = sanitize(reason)
		if head == "" {
			head = "撤回 " + short(oidOr(retracts))
		}
	} else {
		head = sanitize(body)
	}
	if head == "" {
		head = "（空消息）"
	}

	var b strings.Builder
	b.WriteString(head)
	b.WriteString("\n\n")
	b.WriteString(trailerKind + ": " + string(kind) + "\n")
	b.WriteString(trailerSeq + ": " + strconv.Itoa(seq) + "\n")
	if retracts != "" {
		b.WriteString(trailerRetracts + ": " + retracts + "\n")
	}
	if reason != "" {
		b.WriteString(trailerReason + ": " + sanitize(reason) + "\n")
	}
	return b.String()
}

// decode 把 gitx 的原始提交翻译成领域消息。
func decode(raw []gitx.RawCommit, me string, signed bool) []Message {
	out := make([]Message, 0, len(raw))
	for _, r := range raw {
		seq, _ := strconv.Atoi(r.Seq)
		kind := KindMsg
		if r.Retracts != "" {
			kind = KindRetract
		}
		m := Message{
			OID:      r.OID,
			Seq:      seq,
			Author:   r.Author,
			Body:     bodyOf(r.Body),
			Kind:     kind,
			Retracts: r.Retracts,
			Reason:   r.Reason,
			At:       r.At,
		}
		if signed && r.Author == me {
			m.Sig = "ssh" // 仅表示"本机签过名"，具体指纹由 gpg 层给出
		}
		out = append(out, m)
	}
	return out
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
		if !ok || !strings.HasPrefix(k, "Immutalk-") {
			return false
		}
	}
	return true
}

// sanitize 清掉会破坏记录分隔的控制字符，并去掉尾部空白。
func sanitize(s string) string {
	s = strings.ReplaceAll(s, "\x00", "")
	s = strings.ReplaceAll(s, "\x1f", "")
	s = strings.ReplaceAll(s, "\x1e", "")
	return strings.TrimRight(s, " \t\r\n")
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
