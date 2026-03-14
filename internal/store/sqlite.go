package store

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	_ "github.com/glebarez/sqlite"

	"github.com/guohuiyuan/qzonewall-go/internal/model"
)

// Store SQLite 持久化存储
type Store struct {
	db *sql.DB
}

// New 创建并初始化 SQLite 存储
func New(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	// WAL 模式, 更好的并发
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return nil, fmt.Errorf("set WAL: %w", err)
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS posts (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			uin         INTEGER NOT NULL DEFAULT 0,
			name        TEXT    NOT NULL DEFAULT '',
			group_id    INTEGER NOT NULL DEFAULT 0,
			text        TEXT    NOT NULL DEFAULT '',
			images      TEXT    NOT NULL DEFAULT '[]',
			anon        INTEGER NOT NULL DEFAULT 0,
			status      TEXT    NOT NULL DEFAULT 'pending',
			reason      TEXT    NOT NULL DEFAULT '',
			tid         TEXT    NOT NULL DEFAULT '',
			avatar_url  TEXT    NOT NULL DEFAULT '',
			create_time INTEGER NOT NULL DEFAULT 0,
			update_time INTEGER NOT NULL DEFAULT 0
		);
		CREATE INDEX IF NOT EXISTS idx_posts_status ON posts(status);

		CREATE TABLE IF NOT EXISTS accounts (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			username      TEXT NOT NULL UNIQUE,
			password_hash TEXT NOT NULL,
			salt          TEXT NOT NULL,
			role          TEXT NOT NULL DEFAULT 'user',
			create_time   INTEGER NOT NULL DEFAULT 0
		);

		CREATE TABLE IF NOT EXISTS sessions (
			token      TEXT PRIMARY KEY,
			account_id INTEGER NOT NULL,
			expire_time INTEGER NOT NULL
		);
	`)
	return err
}

// ──────────────────────────────────────────
// Post CRUD
// ──────────────────────────────────────────

// SavePost 保存投稿, 若 ID==0 则插入并回填 ID, 否则更新
func (s *Store) SavePost(p *model.Post) error {
	imagesJSON, _ := json.Marshal(p.Images)
	now := time.Now().Unix()

	if p.ID == 0 {
		if p.CreateTime == 0 {
			p.CreateTime = now
		}
		res, err := s.db.Exec(
			`INSERT INTO posts (uin,name,group_id,text,images,anon,status,reason,tid,avatar_url,create_time,update_time)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			p.UIN, p.Name, p.GroupID, p.Text, string(imagesJSON),
			b2i(p.Anon), string(p.Status), p.Reason, p.TID, p.AvatarURL,
			p.CreateTime, now,
		)
		if err != nil {
			return err
		}
		p.ID, _ = res.LastInsertId()
	} else {
		_, err := s.db.Exec(
			`UPDATE posts SET uin=?,name=?,group_id=?,text=?,images=?,anon=?,status=?,reason=?,tid=?,avatar_url=?,update_time=?
			 WHERE id=?`,
			p.UIN, p.Name, p.GroupID, p.Text, string(imagesJSON),
			b2i(p.Anon), string(p.Status), p.Reason, p.TID, p.AvatarURL,
			now, p.ID,
		)
		if err != nil {
			return err
		}
	}
	return nil
}

// GetPost 获取单条投稿
func (s *Store) GetPost(id int64) (*model.Post, error) {
	row := s.db.QueryRow(postCols("WHERE id=?"), id)
	return scanPost(row)
}

// DeletePost 删除投稿
func (s *Store) DeletePost(id int64) error {
	_, err := s.db.Exec("DELETE FROM posts WHERE id=?", id)
	return err
}

// DeletePostsByIDs 批量删除投稿
func (s *Store) DeletePostsByIDs(ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	ph := make([]string, len(ids))
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		ph[i] = "?"
		args[i] = id
	}
	res, err := s.db.Exec(
		fmt.Sprintf("DELETE FROM posts WHERE id IN (%s)", strings.Join(ph, ",")),
		args...,
	)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ListByStatus 按状态列出投稿
func (s *Store) ListByStatus(status model.PostStatus) ([]*model.Post, error) {
	rows, err := s.db.Query(postCols("WHERE status=? ORDER BY id ASC"), string(status))
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = rows.Close()
	}()
	return scanPosts(rows)
}

// GetPostsByIDs 批量获取指定ID的投稿
func (s *Store) GetPostsByIDs(ids []int64) ([]*model.Post, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ph := make([]string, len(ids))
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		ph[i] = "?"
		args[i] = id
	}
	q := fmt.Sprintf(postCols("WHERE id IN (%s) ORDER BY id ASC"), strings.Join(ph, ","))
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = rows.Close()
	}()
	return scanPosts(rows)
}

// GetApprovedPosts 获取已通过但还未发布(tid=”)的投稿
func (s *Store) GetApprovedPosts(limit int) ([]*model.Post, error) {
	rows, err := s.db.Query(
		postCols("WHERE status='approved' AND tid='' ORDER BY id ASC LIMIT ?"), limit,
	)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = rows.Close()
	}()
	return scanPosts(rows)
}

// ListAll 分页列出所有投稿（最新在前）
func (s *Store) ListAll(limit, offset int) ([]*model.Post, error) {
	rows, err := s.db.Query(
		postCols("ORDER BY id DESC LIMIT ? OFFSET ?"), limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = rows.Close()
	}()
	return scanPosts(rows)
}

// CountByStatus 统计各状态数量
func (s *Store) CountByStatus(status model.PostStatus) (int, error) {
	var n int
	err := s.db.QueryRow("SELECT COUNT(*) FROM posts WHERE status=?", string(status)).Scan(&n)
	return n, err
}

// CountAll 统计全部投稿数量
func (s *Store) CountAll() (int, error) {
	var n int
	err := s.db.QueryRow("SELECT COUNT(*) FROM posts").Scan(&n)
	return n, err
}

// ──────────────────────────────────────────
// Account CRUD
// ──────────────────────────────────────────

func (s *Store) CreateAccount(username, passwordHash, salt, role string) error {
	_, err := s.db.Exec(
		"INSERT INTO accounts (username,password_hash,salt,role,create_time) VALUES (?,?,?,?,?)",
		username, passwordHash, salt, role, time.Now().Unix(),
	)
	return err
}

func (s *Store) GetAccount(username string) (*model.Account, error) {
	var a model.Account
	err := s.db.QueryRow(
		"SELECT id,username,password_hash,salt,role,create_time FROM accounts WHERE username=?",
		username,
	).Scan(&a.ID, &a.Username, &a.PasswordHash, &a.Salt, &a.Role, &a.CreateTime)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &a, err
}

func (s *Store) GetAccountByID(id int64) (*model.Account, error) {
	var a model.Account
	err := s.db.QueryRow(
		"SELECT id,username,password_hash,salt,role,create_time FROM accounts WHERE id=?",
		id,
	).Scan(&a.ID, &a.Username, &a.PasswordHash, &a.Salt, &a.Role, &a.CreateTime)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &a, err
}

func (s *Store) AccountCount() (int, error) {
	var n int
	err := s.db.QueryRow("SELECT COUNT(*) FROM accounts").Scan(&n)
	return n, err
}

// UpdateAccountPassword 更新账号密码
func (s *Store) UpdateAccountPassword(username, passwordHash, salt string) error {
	_, err := s.db.Exec(
		"UPDATE accounts SET password_hash=?, salt=? WHERE username=?",
		passwordHash, salt, username,
	)
	return err
}

// ──────────────────────────────────────────
// Session CRUD
// ──────────────────────────────────────────

func (s *Store) CreateSession(token string, accountID, expireTime int64) error {
	_, err := s.db.Exec(
		"INSERT OR REPLACE INTO sessions (token,account_id,expire_time) VALUES (?,?,?)",
		token, accountID, expireTime,
	)
	return err
}

func (s *Store) GetSession(token string) (int64, error) {
	var accountID, expire int64
	err := s.db.QueryRow(
		"SELECT account_id,expire_time FROM sessions WHERE token=?", token,
	).Scan(&accountID, &expire)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if time.Now().Unix() > expire {
		_, _ = s.db.Exec("DELETE FROM sessions WHERE token=?", token)
		return 0, nil
	}
	return accountID, nil
}

func (s *Store) DeleteSession(token string) error {
	_, err := s.db.Exec("DELETE FROM sessions WHERE token=?", token)
	return err
}

func (s *Store) CleanExpiredSessions() {
	_, _ = s.db.Exec("DELETE FROM sessions WHERE expire_time < ?", time.Now().Unix())
}

// Close 关闭数据库连接
func (s *Store) Close() error {
	return s.db.Close()
}

// ──────────────────────────────────────────
// 敏感词辅助
// ──────────────────────────────────────────

// LoadCensorWords 加载敏感词列表（内置 + 文件）
func LoadCensorWords(words []string, filePath string) []string {
	result := make([]string, 0, len(words))
	for _, w := range words {
		w = strings.TrimSpace(w)
		if w != "" {
			result = append(result, strings.ToLower(w))
		}
	}
	if filePath != "" {
		if f, err := os.Open(filePath); err == nil {
			defer func() {
				_ = f.Close()
			}()
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				w := strings.TrimSpace(strings.ToLower(sc.Text()))
				if w != "" && !strings.HasPrefix(w, "#") {
					result = append(result, w)
				}
			}
		}
	}
	return result
}

// CheckCensor 检测文本是否包含敏感词, 返回命中的词
func CheckCensor(text string, words []string) (bool, string) {
	lower := strings.ToLower(text)
	for _, w := range words {
		if strings.Contains(lower, w) {
			return true, w
		}
	}
	return false, ""
}

// ──────────────────────────────────────────
// 内部辅助
// ──────────────────────────────────────────

func postCols(where string) string {
	return "SELECT id,uin,name,group_id,text,images,anon,status,reason,tid,avatar_url,create_time,update_time FROM posts " + where
}

func scanPost(row *sql.Row) (*model.Post, error) {
	var p model.Post
	var imgs string
	var anon int
	err := row.Scan(&p.ID, &p.UIN, &p.Name, &p.GroupID, &p.Text, &imgs, &anon,
		&p.Status, &p.Reason, &p.TID, &p.AvatarURL, &p.CreateTime, &p.UpdateTime)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.Anon = anon != 0
	_ = json.Unmarshal([]byte(imgs), &p.Images)
	return &p, nil
}

func scanPosts(rows *sql.Rows) ([]*model.Post, error) {
	var posts []*model.Post
	for rows.Next() {
		var p model.Post
		var imgs string
		var anon int
		if err := rows.Scan(&p.ID, &p.UIN, &p.Name, &p.GroupID, &p.Text, &imgs, &anon,
			&p.Status, &p.Reason, &p.TID, &p.AvatarURL, &p.CreateTime, &p.UpdateTime); err != nil {
			return nil, err
		}
		p.Anon = anon != 0
		_ = json.Unmarshal([]byte(imgs), &p.Images)
		posts = append(posts, &p)
	}
	return posts, rows.Err()
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
