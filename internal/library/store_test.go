package library

import (
	"errors"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// openAt 打开 path 处的库，测试结束自动关掉。
func openAt(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// newStore 在临时目录里开一个空库。
func newStore(t *testing.T) *Store {
	t.Helper()
	return openAt(t, filepath.Join(t.TempDir(), "library.db"))
}

// addQuestion 落库一道错题，失败即终止测试。
func addQuestion(t *testing.T, s *Store, q Question) Question {
	t.Helper()
	got, err := s.AddQuestion(q)
	if err != nil {
		t.Fatalf("AddQuestion(%+v): %v", q, err)
	}
	return got
}

func assertQuestion(t *testing.T, want, got Question) {
	t.Helper()
	if got.ID != want.ID {
		t.Errorf("ID = %d，想要 %d", got.ID, want.ID)
	}
	if got.QuestionHash != want.QuestionHash {
		t.Errorf("QuestionHash = %q，想要 %q", got.QuestionHash, want.QuestionHash)
	}
	if got.AnswerHash != want.AnswerHash {
		t.Errorf("AnswerHash = %q，想要 %q", got.AnswerHash, want.AnswerHash)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("CreatedAt = %v，想要 %v", got.CreatedAt, want.CreatedAt)
	}
}

var baseTime = time.Date(2026, 9, 16, 10, 30, 0, 123_000_000, time.UTC)

func TestAddQuestion(t *testing.T) {
	s := newStore(t)

	added := addQuestion(t, s, Question{
		QuestionHash: "sha256:题图",
		AnswerHash:   "sha256:答案图",
		CreatedAt:    baseTime,
	})

	if added.ID == 0 {
		t.Error("新错题应当拿到一个非零 ID")
	}
	if !added.HasAnswer() {
		t.Error("存了答案图 hash，HasAnswer 应当为真")
	}
	assertQuestion(t, Question{
		ID:           added.ID,
		QuestionHash: "sha256:题图",
		AnswerHash:   "sha256:答案图",
		CreatedAt:    baseTime,
	}, added)

	fetched, err := s.GetQuestion(added.ID)
	if err != nil {
		t.Fatalf("GetQuestion: %v", err)
	}
	assertQuestion(t, added, fetched)
}

func TestAddQuestionWithoutAnswer(t *testing.T) {
	s := newStore(t)

	added := addQuestion(t, s, Question{QuestionHash: "sha256:只有题图"})
	if added.HasAnswer() {
		t.Error("没存答案图 hash，HasAnswer 应当为假")
	}

	fetched, err := s.GetQuestion(added.ID)
	if err != nil {
		t.Fatalf("GetQuestion: %v", err)
	}
	if fetched.AnswerHash != "" {
		t.Errorf("AnswerHash = %q，想要空串", fetched.AnswerHash)
	}
	if fetched.HasAnswer() {
		t.Error("答案图为空的错题读回来 HasAnswer 应当为假")
	}
}

// CreatedAt 留空时由存储层补当前时间。
func TestAddQuestionFillsCreatedAt(t *testing.T) {
	s := newStore(t)

	before := time.Now().Add(-time.Second)
	added := addQuestion(t, s, Question{QuestionHash: "sha256:题图"})
	after := time.Now().Add(time.Second)

	if added.CreatedAt.Before(before) || added.CreatedAt.After(after) {
		t.Errorf("CreatedAt = %v，不在 %v 与 %v 之间", added.CreatedAt, before, after)
	}
	if added.CreatedAt.Location() != time.UTC {
		t.Errorf("CreatedAt 的时区 = %v，想要 UTC", added.CreatedAt.Location())
	}
}

func TestAddQuestionRejectsEmptyQuestionHash(t *testing.T) {
	s := newStore(t)

	if _, err := s.AddQuestion(Question{AnswerHash: "sha256:答案图"}); err == nil {
		t.Fatal("题图 hash 为空时应当报错")
	}

	qs, err := s.ListQuestions()
	if err != nil {
		t.Fatalf("ListQuestions: %v", err)
	}
	if len(qs) != 0 {
		t.Errorf("失败的写入不该留下记录，库里有 %d 条", len(qs))
	}
}

func TestListQuestionsNewestFirst(t *testing.T) {
	s := newStore(t)

	oldest := addQuestion(t, s, Question{QuestionHash: "sha256:旧", CreatedAt: baseTime})
	middle := addQuestion(t, s, Question{QuestionHash: "sha256:中", CreatedAt: baseTime.Add(time.Hour)})
	newest := addQuestion(t, s, Question{QuestionHash: "sha256:新", CreatedAt: baseTime.Add(2 * time.Hour)})
	// 同一毫秒落库的两道题也要有确定的先后：后写进来的排前面。
	tie := addQuestion(t, s, Question{QuestionHash: "sha256:同刻", CreatedAt: baseTime.Add(2 * time.Hour)})

	qs, err := s.ListQuestions()
	if err != nil {
		t.Fatalf("ListQuestions: %v", err)
	}
	want := []Question{tie, newest, middle, oldest}
	if len(qs) != len(want) {
		t.Fatalf("列出 %d 条，想要 %d 条", len(qs), len(want))
	}
	for i := range want {
		assertQuestion(t, want[i], qs[i])
	}
}

func TestListQuestionsEmpty(t *testing.T) {
	s := newStore(t)

	qs, err := s.ListQuestions()
	if err != nil {
		t.Fatalf("ListQuestions: %v", err)
	}
	if qs == nil {
		t.Error("空库应当返回空切片而不是 nil，前端要拿到 [] 不是 null")
	}
	if len(qs) != 0 {
		t.Errorf("空库列出了 %d 条", len(qs))
	}
}

func TestGetQuestionNotFound(t *testing.T) {
	s := newStore(t)

	if _, err := s.GetQuestion(404); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetQuestion 不存在的 id 返回 %v，想要 ErrNotFound", err)
	}
}

func TestDeleteQuestion(t *testing.T) {
	s := newStore(t)
	added := addQuestion(t, s, Question{QuestionHash: "sha256:题图", CreatedAt: baseTime})

	deleted, err := s.DeleteQuestion(added.ID)
	if err != nil {
		t.Fatalf("DeleteQuestion: %v", err)
	}
	assertQuestion(t, added, deleted)

	if _, err := s.GetQuestion(added.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("删掉后 GetQuestion 返回 %v，想要 ErrNotFound", err)
	}
	qs, err := s.ListQuestions()
	if err != nil {
		t.Fatalf("ListQuestions: %v", err)
	}
	if len(qs) != 0 {
		t.Errorf("删掉后还列得出 %d 条", len(qs))
	}
}

func TestDeleteQuestionNotFound(t *testing.T) {
	s := newStore(t)

	if _, err := s.DeleteQuestion(404); !errors.Is(err, ErrNotFound) {
		t.Errorf("删不存在的错题返回 %v，想要 ErrNotFound", err)
	}
}

// 删过一遍之后再删同一个 id，还是 ErrNotFound，不会把别人删掉。
func TestDeleteQuestionTwice(t *testing.T) {
	s := newStore(t)
	added := addQuestion(t, s, Question{QuestionHash: "sha256:题图"})

	if _, err := s.DeleteQuestion(added.ID); err != nil {
		t.Fatalf("第一次 DeleteQuestion: %v", err)
	}
	if _, err := s.DeleteQuestion(added.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("第二次 DeleteQuestion 返回 %v，想要 ErrNotFound", err)
	}
}

func TestDeleteQuestionLeavesOthers(t *testing.T) {
	s := newStore(t)
	keep := addQuestion(t, s, Question{QuestionHash: "sha256:留着", CreatedAt: baseTime})
	doomed := addQuestion(t, s, Question{QuestionHash: "sha256:删掉", CreatedAt: baseTime.Add(time.Hour)})

	if _, err := s.DeleteQuestion(doomed.ID); err != nil {
		t.Fatalf("DeleteQuestion: %v", err)
	}

	qs, err := s.ListQuestions()
	if err != nil {
		t.Fatalf("ListQuestions: %v", err)
	}
	if len(qs) != 1 {
		t.Fatalf("剩下 %d 条，想要 1 条", len(qs))
	}
	assertQuestion(t, keep, qs[0])
}

// 去重发生在文件那一层，错题这一层允许两道题共用一张图（ADR-0004）。
func TestSameImageBacksTwoQuestions(t *testing.T) {
	s := newStore(t)
	first := addQuestion(t, s, Question{QuestionHash: "sha256:同一张图", CreatedAt: baseTime})
	second := addQuestion(t, s, Question{QuestionHash: "sha256:同一张图", CreatedAt: baseTime.Add(time.Hour)})

	if _, err := s.DeleteQuestion(second.ID); err != nil {
		t.Fatalf("DeleteQuestion: %v", err)
	}

	qs, err := s.ListQuestions()
	if err != nil {
		t.Fatalf("ListQuestions: %v", err)
	}
	if len(qs) != 1 {
		t.Fatalf("剩下 %d 条，想要 1 条", len(qs))
	}
	assertQuestion(t, first, qs[0])
}

// 关掉再打开：数据还在，且迁移不会重跑（重跑就会把表里的数据冲掉）。
func TestReopenKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "library.db")

	s := openAt(t, path)
	added := addQuestion(t, s, Question{QuestionHash: "sha256:题图", CreatedAt: baseTime})
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened := openAt(t, path)
	fetched, err := reopened.GetQuestion(added.ID)
	if err != nil {
		t.Fatalf("重开后 GetQuestion: %v", err)
	}
	assertQuestion(t, added, fetched)

	var version int
	if err := reopened.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("读模式版本: %v", err)
	}
	if version != latestVersion() {
		t.Errorf("重开后模式版本 = %d，想要 %d", version, latestVersion())
	}
}

// 空库起来要能建出完整模式：版本号推到最新，questions 表的列一个不少。
func TestMigrateFromEmptyDatabase(t *testing.T) {
	s := newStore(t)

	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("读模式版本: %v", err)
	}
	if version != latestVersion() {
		t.Errorf("模式版本 = %d，想要 %d", version, latestVersion())
	}

	rows, err := s.db.Query("PRAGMA table_info(questions)")
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	defer rows.Close()

	type column struct {
		name    string
		notNull bool
		pk      int
	}
	var got []column
	for rows.Next() {
		var (
			cid, notNull, pk int
			name, typ        string
			dflt             any
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("读列信息: %v", err)
		}
		got = append(got, column{name: name, notNull: notNull == 1, pk: pk})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("读列信息: %v", err)
	}

	want := []column{
		{name: "id", pk: 1},
		{name: "question_hash", notNull: true},
		{name: "answer_hash", notNull: true},
		{name: "created_at", notNull: true},
	}
	if len(got) != len(want) {
		t.Fatalf("questions 有 %d 列，想要 %d 列：%+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].name != want[i].name || got[i].notNull != want[i].notNull || got[i].pk != want[i].pk {
			t.Errorf("第 %d 列 = %+v，想要 %+v", i, got[i], want[i])
		}
	}
}

// 库比程序新时必须拒绝打开，免得老程序往不认识的新表里写。
func TestOpenRejectsNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "library.db")

	s := openAt(t, path)
	future := latestVersion() + 1
	if _, err := s.db.Exec("PRAGMA user_version = " + strconv.Itoa(future)); err != nil {
		t.Fatalf("抬高模式版本: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := Open(path); err == nil {
		t.Error("模式版本比程序新时 Open 应当报错")
	}
}

func TestOpenCreatesParentDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "library.db")

	s := openAt(t, path)
	if _, err := s.AddQuestion(Question{QuestionHash: "sha256:题图"}); err != nil {
		t.Fatalf("AddQuestion: %v", err)
	}
}

func TestOpenRejectsEmptyPath(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Error("路径为空时 Open 应当报错")
	}
}

// 内存库：进程内用完即弃，给以后不想落盘的测试留的口子。
func TestOpenMemoryPath(t *testing.T) {
	s := openAt(t, MemoryPath)

	added := addQuestion(t, s, Question{QuestionHash: "sha256:题图", CreatedAt: baseTime})
	fetched, err := s.GetQuestion(added.ID)
	if err != nil {
		t.Fatalf("GetQuestion: %v", err)
	}
	assertQuestion(t, added, fetched)
}
