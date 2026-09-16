package tags_test

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"questionbook/internal/library"
	"questionbook/internal/tags"
)

// baseTime 是测试里错题的时间基准。
var baseTime = time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)

// 全应用只有一条测试缝：服务层（spec）。这里从头到尾只调 tags.Service 的方法，
// 不断言它内部调了谁几次，也不启动 Wails。

// newService 在临时目录里搭一套「错题库 + 标签服务」，与 main.go 的接线同一个形状：
// 标签服务共用题库那条连接，不是自己 Open 的第二个。
func newService(t *testing.T) (*tags.Service, *library.Store) {
	t.Helper()

	lib, err := library.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	t.Cleanup(func() { lib.Close() })

	return tags.NewService(lib), lib
}

// addQuestion 落一道错题，返回它 —— 标签服务只认 id，得先有题可挂。
func addQuestion(t *testing.T, lib *library.Store, hash string) library.Question {
	t.Helper()

	q, err := lib.AddQuestion(library.Question{QuestionHash: hash})
	if err != nil {
		t.Fatalf("AddQuestion(%q): %v", hash, err)
	}
	return q
}

// addQuestionAt 同 addQuestion，但显式给创建时间 —— 要断言「筛出来的题新的在前」时
// 必须这样：同一毫秒落库的两道题只能按 id 分先后，那断言的就是 id 而不是排序规则了。
func addQuestionAt(t *testing.T, lib *library.Store, hash string, at time.Time) library.Question {
	t.Helper()

	q, err := lib.AddQuestion(library.Question{QuestionHash: hash, CreatedAt: at})
	if err != nil {
		t.Fatalf("AddQuestion(%q): %v", hash, err)
	}
	return q
}

// list 取全部标签，失败即终止测试。
func list(t *testing.T, svc *tags.Service) []tags.Tag {
	t.Helper()

	ts, err := svc.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return ts
}

// create 建一个标签，失败即终止测试。
func create(t *testing.T, svc *tags.Service, parentID int64, name string) tags.Tag {
	t.Helper()

	tag, err := svc.Create(parentID, name)
	if err != nil {
		t.Fatalf("Create(%d, %q): %v", parentID, name, err)
	}
	return tag
}

// presets 是迁移里预置的考研四门。
var presets = []string{"数学", "英语", "政治", "专业课"}

// findSubject 按名字取一个预置的顶层学科。
func findSubject(t *testing.T, svc *tags.Service, name string) tags.Tag {
	t.Helper()

	for _, tag := range list(t, svc) {
		if tag.Level == tags.LevelSubject && tag.Name == name {
			return tag
		}
	}
	t.Fatalf("找不到顶层学科 %q", name)
	return tags.Tag{}
}

// seedTree 建出「学科 > 章节 > 知识点」一条链：数学 > 高等数学 > 洛必达法则。
// 返回这三段，测验里到处都要用它们。
func seedTree(t *testing.T, svc *tags.Service) (subject, chapter, point tags.Tag) {
	t.Helper()

	subject = findSubject(t, svc, "数学")
	chapter = create(t, svc, subject.ID, "高等数学")
	point = create(t, svc, chapter.ID, "洛必达法则")
	return subject, chapter, point
}

// names 把标签摊成名字，方便按集合断言。
func names(ts []tags.Tag) []string {
	out := make([]string, 0, len(ts))
	for _, tag := range ts {
		out = append(out, tag.Name)
	}
	return out
}

// assertNames 断言标签集合与 want 完全一致（不看顺序）。
func assertNames(t *testing.T, got []tags.Tag, want ...string) {
	t.Helper()

	have := map[string]int{}
	for _, tag := range got {
		have[tag.Name]++
	}
	for _, name := range want {
		if have[name] == 0 {
			t.Errorf("标签里没有 %q，实际有 %v", name, names(got))
			return
		}
		have[name]--
	}
	for name, n := range have {
		if n > 0 {
			t.Errorf("标签里多了 %d 个 %q，实际有 %v", n, name, names(got))
		}
	}
}

// assertQuestionIDs 断言筛出来的错题 id 序列与 want 完全一致（顺序也算）。
func assertQuestionIDs(t *testing.T, got []library.Question, want ...int64) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("筛出 %d 道题 %v，想要 %d 道 %v", len(got), questionIDs(got), len(want), want)
	}
	for i := range want {
		if got[i].ID != want[i] {
			t.Fatalf("筛出的 id 序列 = %v，想要 %v", questionIDs(got), want)
		}
	}
}

func questionIDs(qs []library.Question) []int64 {
	out := make([]int64, 0, len(qs))
	for _, q := range qs {
		out = append(out, q.ID)
	}
	return out
}

// 新库里预置考研四门，它们是普通的顶层学科 —— 用户可以改也可以删。
func TestPresetSubjects(t *testing.T) {
	svc, _ := newService(t)

	roots := list(t, svc)
	if len(roots) != len(presets) {
		t.Fatalf("新库里有 %d 个标签 %v，想要 %d 个：%v", len(roots), names(roots), len(presets), presets)
	}
	assertNames(t, roots, presets...)

	for _, tag := range roots {
		if tag.Level != tags.LevelSubject {
			t.Errorf("%q 的层级 = %v，想要学科", tag.Name, tag.Level)
		}
		if tag.ParentID != 0 {
			t.Errorf("%q 的 ParentID = %d，顶层学科应当是 0", tag.Name, tag.ParentID)
		}
	}
}

// 层级由父节点推出来，不由调用方传。
func TestCreateDerivesLevel(t *testing.T) {
	svc, _ := newService(t)

	subject, chapter, point := seedTree(t, svc)

	if chapter.Level != tags.LevelChapter || chapter.ParentID != subject.ID {
		t.Errorf("章节 = %+v，想要 level=章节、parent=%d", chapter, subject.ID)
	}
	if point.Level != tags.LevelPoint || point.ParentID != chapter.ID {
		t.Errorf("知识点 = %+v，想要 level=知识点、parent=%d", point, chapter.ID)
	}

	// 名字两边的空白应当被去掉，别让「 高等数学」和「高等数学」变成两个标签。
	padded := create(t, svc, subject.ID, "  线性代数  ")
	if padded.Name != "线性代数" {
		t.Errorf("名字 = %q，想要去掉空白的 %q", padded.Name, "线性代数")
	}
}

// 同一父下的兄弟按创建先后排（先建的在前）。刻意不按名字排：SQLite 对中文按码点比。
func TestListKeepsCreationOrderAmongSiblings(t *testing.T) {
	svc, _ := newService(t)
	subject := findSubject(t, svc, "数学")

	create(t, svc, subject.ID, "高等数学")
	create(t, svc, subject.ID, "线性代数")
	create(t, svc, subject.ID, "概率论")

	var got []string
	for _, tag := range list(t, svc) {
		if tag.Level == tags.LevelChapter && tag.ParentID == subject.ID {
			got = append(got, tag.Name)
		}
	}
	want := []string{"高等数学", "线性代数", "概率论"}
	if len(got) != len(want) {
		t.Fatalf("章节有 %v，想要 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("章节顺序 = %v，想要 %v", got, want)
		}
	}
}

// 四层不存在：知识点下面不能再挂。
func TestCreateRejectsFourthLevel(t *testing.T) {
	svc, _ := newService(t)
	_, _, point := seedTree(t, svc)

	before := len(list(t, svc))
	if _, err := svc.Create(point.ID, "再往下"); !errors.Is(err, tags.ErrMaxDepth) {
		t.Errorf("在知识点下建标签返回 %v，想要 ErrMaxDepth", err)
	}
	if after := len(list(t, svc)); after != before {
		t.Errorf("失败的建标签留下了记录：%d → %d", before, after)
	}
}

func TestCreateRejectsEmptyName(t *testing.T) {
	svc, _ := newService(t)
	subject := findSubject(t, svc, "数学")

	for _, name := range []string{"", "   ", "\t"} {
		if _, err := svc.Create(subject.ID, name); !errors.Is(err, tags.ErrInvalidName) {
			t.Errorf("名字 %q 返回 %v，想要 ErrInvalidName", name, err)
		}
	}

	// 顶层学科也一样：预置之外用户可以自由加，但不能加个没名字的。
	if _, err := svc.Create(0, " "); !errors.Is(err, tags.ErrInvalidName) {
		t.Errorf("顶层空名返回 %v，想要 ErrInvalidName", err)
	}
}

func TestCreateRejectsDuplicateSibling(t *testing.T) {
	svc, _ := newService(t)
	subject, chapter, _ := seedTree(t, svc)

	// 同一个父下重名。
	if _, err := svc.Create(subject.ID, "高等数学"); !errors.Is(err, tags.ErrDuplicate) {
		t.Errorf("同级重名返回 %v，想要 ErrDuplicate", err)
	}
	// 与预置学科重名。
	if _, err := svc.Create(0, "数学"); !errors.Is(err, tags.ErrDuplicate) {
		t.Errorf("与预置学科重名返回 %v，想要 ErrDuplicate", err)
	}
	// 空白不算区别：去掉之后重名的照样拦。
	if _, err := svc.Create(0, " 数学 "); !errors.Is(err, tags.ErrDuplicate) {
		t.Errorf("带空白的重名返回 %v，想要 ErrDuplicate", err)
	}

	// 不同父下同名是允许的：两个章节各有一个「重点」很正常。
	other := create(t, svc, subject.ID, "线性代数")
	create(t, svc, chapter.ID, "重点")
	create(t, svc, other.ID, "重点")
}

func TestCreateRejectsMissingParent(t *testing.T) {
	svc, _ := newService(t)

	if _, err := svc.Create(404, "孤儿章节"); !errors.Is(err, tags.ErrNotFound) {
		t.Errorf("父不存在返回 %v，想要 ErrNotFound", err)
	}
}

func TestRename(t *testing.T) {
	svc, _ := newService(t)
	_, chapter, _ := seedTree(t, svc)

	renamed, err := svc.Rename(chapter.ID, "  一元微积分  ")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if renamed.Name != "一元微积分" {
		t.Errorf("名字 = %q，想要去掉空白的 %q", renamed.Name, "一元微积分")
	}
	// 改名不动位置：层级与父节点照旧。
	if renamed.Level != chapter.Level || renamed.ParentID != chapter.ParentID {
		t.Errorf("改名动了位置: %+v，原来 %+v", renamed, chapter)
	}

	fetched, err := svc.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, tag := range fetched {
		if tag.ID == chapter.ID {
			found = true
			if tag.Name != "一元微积分" {
				t.Errorf("重新读出来的名字 = %q", tag.Name)
			}
		}
		if tag.Name == "高等数学" {
			t.Error("旧名字还在，改名没生效")
		}
	}
	if !found {
		t.Error("改完名之后标签不见了")
	}
}

func TestRenameRejectsBadInput(t *testing.T) {
	svc, _ := newService(t)

	if _, err := svc.Rename(404, "新名字"); !errors.Is(err, tags.ErrNotFound) {
		t.Errorf("改不存在的标签返回 %v，想要 ErrNotFound", err)
	}

	subject, chapter, _ := seedTree(t, svc)
	create(t, svc, subject.ID, "线性代数")

	if _, err := svc.Rename(chapter.ID, "线性代数"); !errors.Is(err, tags.ErrDuplicate) {
		t.Errorf("改成同级已有的名字返回 %v，想要 ErrDuplicate", err)
	}
	if _, err := svc.Rename(chapter.ID, "  "); !errors.Is(err, tags.ErrInvalidName) {
		t.Errorf("改成空名返回 %v，想要 ErrInvalidName", err)
	}

	// 失败的改名不能动到原数据。
	tag, err := svc.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, item := range tag {
		if item.ID == chapter.ID && item.Name != "高等数学" {
			t.Errorf("失败的改名把名字改成了 %q", item.Name)
		}
	}
}

// 删叶子标签：它自己没了，错题上挂的它也一并摘掉，题本身不动。
func TestDeleteLeaf(t *testing.T) {
	svc, lib := newService(t)
	_, _, point := seedTree(t, svc)
	q := addQuestion(t, lib, "sha256:题图")

	if err := svc.SetQuestionTags(q.ID, []int64{point.ID}); err != nil {
		t.Fatalf("SetQuestionTags: %v", err)
	}

	res, err := svc.Delete(point.ID)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if res.DeletedTags != 1 {
		t.Errorf("DeletedTags = %d，想要 1", res.DeletedTags)
	}
	if res.UntaggedQuestions != 1 {
		t.Errorf("UntaggedQuestions = %d，想要 1", res.UntaggedQuestions)
	}

	for _, tag := range list(t, svc) {
		if tag.ID == point.ID {
			t.Error("删掉的标签还在 List 里")
		}
	}
	if got, err := svc.TagsOfQuestion(q.ID); err != nil || len(got) != 0 {
		t.Errorf("错题上还挂着删掉的标签: %v, %v", names(got), err)
	}
	// 题本身必须还在。
	qs, err := lib.ListQuestions()
	if err != nil {
		t.Fatalf("ListQuestions: %v", err)
	}
	if len(qs) != 1 {
		t.Errorf("库里有 %d 道题，想要 1 道 —— 删标签不该删题", len(qs))
	}
}

// 删中间节点（章节）：**整棵子树级联删掉**，题本身留着、只是被摘了标签。
// 这是票里点名要有明确行为的那一条。
func TestDeleteMiddleNodeCascades(t *testing.T) {
	svc, lib := newService(t)
	subject, chapter, point := seedTree(t, svc)
	// 章节下第二个知识点，兄弟章节（在这棵子树之外）。
	point2 := create(t, svc, chapter.ID, "泰勒展开")
	sibling := create(t, svc, subject.ID, "线性代数")
	siblingPoint := create(t, svc, sibling.ID, "矩阵的秩")

	bothLevels := addQuestion(t, lib, "sha256:挂了章节和知识点")
	onlyPoint := addQuestion(t, lib, "sha256:只挂了知识点")
	elsewhere := addQuestion(t, lib, "sha256:挂了兄弟章节的知识点")
	untagged := addQuestion(t, lib, "sha256:没挂标签")

	if err := svc.SetQuestionTags(bothLevels.ID, []int64{chapter.ID, point.ID}); err != nil {
		t.Fatalf("SetQuestionTags(章节+知识点): %v", err)
	}
	if err := svc.SetQuestionTags(onlyPoint.ID, []int64{point2.ID}); err != nil {
		t.Fatalf("SetQuestionTags(知识点): %v", err)
	}
	if err := svc.SetQuestionTags(elsewhere.ID, []int64{siblingPoint.ID}); err != nil {
		t.Fatalf("SetQuestionTags(兄弟子树): %v", err)
	}

	res, err := svc.Delete(chapter.ID)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if res.DeletedTags != 3 { // 章节 + 两个知识点
		t.Errorf("DeletedTags = %d，想要 3", res.DeletedTags)
	}
	if res.UntaggedQuestions != 2 { // 两道题，各管各的
		t.Errorf("UntaggedQuestions = %d，想要 2", res.UntaggedQuestions)
	}

	// 子树没了，子树之外的三个标签一个不少。
	assertNames(t, list(t, svc), "数学", "英语", "政治", "专业课", "线性代数", "矩阵的秩")

	// 被删标签的题还在，只是没标签了。
	assertQuestionIDs(t, mustFilter(t, svc, nil), untagged.ID, elsewhere.ID, onlyPoint.ID, bothLevels.ID)
	for _, q := range []library.Question{bothLevels, onlyPoint} {
		got, err := svc.TagsOfQuestion(q.ID)
		if err != nil {
			t.Fatalf("TagsOfQuestion(%d): %v", q.ID, err)
		}
		if len(got) != 0 {
			t.Errorf("错题 %d 上还剩 %v", q.ID, names(got))
		}
	}
	// 兄弟子树上的标签没被波及。
	got, err := svc.TagsOfQuestion(elsewhere.ID)
	if err != nil {
		t.Fatalf("TagsOfQuestion: %v", err)
	}
	assertNames(t, got, "矩阵的秩")

	// 按删掉的章节筛，什么都不该出来；按学科筛，只剩兄弟子树的那道题。
	assertQuestionIDs(t, mustFilter(t, svc, []int64{chapter.ID}))
	assertQuestionIDs(t, mustFilter(t, svc, []int64{subject.ID}), elsewhere.ID)
	assertQuestionIDs(t, mustFilter(t, svc, []int64{point.ID}))
	assertQuestionIDs(t, mustFilter(t, svc, []int64{point2.ID}))

	// 再删一次：已经没了。
	if _, err := svc.Delete(chapter.ID); !errors.Is(err, tags.ErrNotFound) {
		t.Errorf("删已经删掉的标签返回 %v，想要 ErrNotFound", err)
	}
	// 连子孙一起删过的 id，也不该留下能删的东西。
	if _, err := svc.Delete(point.ID); !errors.Is(err, tags.ErrNotFound) {
		t.Errorf("删已经被级联删掉的子节点返回 %v，想要 ErrNotFound", err)
	}
}

// 删顶层学科：整棵树连根拔掉，别的学科与题都不动。
func TestDeleteSubjectCascades(t *testing.T) {
	svc, lib := newService(t)
	subject, _, point := seedTree(t, svc)
	q := addQuestion(t, lib, "sha256:题图")
	keep := findSubject(t, svc, "英语")

	if err := svc.SetQuestionTags(q.ID, []int64{point.ID}); err != nil {
		t.Fatalf("SetQuestionTags: %v", err)
	}

	res, err := svc.Delete(subject.ID)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if res.DeletedTags != 3 { // 学科 + 章节 + 知识点
		t.Errorf("DeletedTags = %d，想要 3", res.DeletedTags)
	}
	if res.UntaggedQuestions != 1 {
		t.Errorf("UntaggedQuestions = %d，想要 1", res.UntaggedQuestions)
	}

	assertNames(t, list(t, svc), "英语", "政治", "专业课")
	if got := mustFilter(t, svc, []int64{keep.ID}); len(got) != 0 {
		t.Errorf("删掉的学科波及了别的学科: %v", questionIDs(got))
	}
	if got := mustFilter(t, svc, nil); len(got) != 1 || got[0].ID != q.ID {
		t.Errorf("删标签把题也弄丢了: %v", questionIDs(got))
	}
}

// 一道题挂多个标签；整体替换是替换，不是追加。
func TestSetQuestionTagsReplaces(t *testing.T) {
	svc, lib := newService(t)
	subject, chapter, point := seedTree(t, svc)
	q := addQuestion(t, lib, "sha256:题图")

	if err := svc.SetQuestionTags(q.ID, []int64{subject.ID, chapter.ID, point.ID}); err != nil {
		t.Fatalf("SetQuestionTags: %v", err)
	}
	got, err := svc.TagsOfQuestion(q.ID)
	if err != nil {
		t.Fatalf("TagsOfQuestion: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("挂了 %d 个标签 %v，想要 3 个", len(got), names(got))
	}
	// 按层级排：学科、章节、知识点。
	wantLevels := []tags.Level{tags.LevelSubject, tags.LevelChapter, tags.LevelPoint}
	for i, level := range wantLevels {
		if got[i].Level != level {
			t.Errorf("第 %d 个标签的层级 = %v，想要 %v", i, got[i].Level, level)
		}
	}

	// 再存一次只留知识点：整份替换，旧的不能留。
	if err := svc.SetQuestionTags(q.ID, []int64{point.ID}); err != nil {
		t.Fatalf("SetQuestionTags(替换): %v", err)
	}
	if got, err := svc.TagsOfQuestion(q.ID); err != nil {
		t.Fatalf("TagsOfQuestion: %v", err)
	} else if len(got) != 1 || got[0].ID != point.ID {
		t.Errorf("替换之后挂着 %v，想要只剩 %q", names(got), point.Name)
	}

	// 空集 = 摘掉全部。
	if err := svc.SetQuestionTags(q.ID, nil); err != nil {
		t.Fatalf("SetQuestionTags(清空): %v", err)
	}
	if got, err := svc.TagsOfQuestion(q.ID); err != nil {
		t.Fatalf("TagsOfQuestion: %v", err)
	} else if len(got) != 0 {
		t.Errorf("清空之后还剩 %v", names(got))
	}
}

// 多道题共用同一个标签各挂各的，重复传的 id 会被去重。
func TestSetQuestionTagsDeduplicates(t *testing.T) {
	svc, lib := newService(t)
	_, chapter, point := seedTree(t, svc)
	q := addQuestion(t, lib, "sha256:题图")

	if err := svc.SetQuestionTags(q.ID, []int64{chapter.ID, point.ID, chapter.ID, chapter.ID}); err != nil {
		t.Fatalf("SetQuestionTags: %v", err)
	}
	got, err := svc.TagsOfQuestion(q.ID)
	if err != nil {
		t.Fatalf("TagsOfQuestion: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("挂了 %d 个标签 %v，想要 2 个（重复的应当去掉）", len(got), names(got))
	}
}

// 写操作里的 id 必须是真实存在的：拿不存在的 id 来写，要收到 ErrNotFound，
// 而不是一个「悄悄什么都没发生」的成功。
func TestSetQuestionTagsRejectsUnknownIDs(t *testing.T) {
	svc, lib := newService(t)
	_, chapter, point := seedTree(t, svc)
	q := addQuestion(t, lib, "sha256:题图")

	if err := svc.SetQuestionTags(404, []int64{point.ID}); !errors.Is(err, library.ErrNotFound) {
		t.Errorf("题不存在返回 %v，想要 library.ErrNotFound", err)
	}
	if err := svc.SetQuestionTags(q.ID, []int64{chapter.ID, 404}); !errors.Is(err, tags.ErrNotFound) {
		t.Errorf("标签不存在返回 %v，想要 tags.ErrNotFound", err)
	}
	// 一处不存在 → 整份不生效，不能留下改了一半的标签集。
	if got, err := svc.TagsOfQuestion(q.ID); err != nil {
		t.Fatalf("TagsOfQuestion: %v", err)
	} else if len(got) != 0 {
		t.Errorf("失败的替换留下了 %v", names(got))
	}
}

func TestAddRemoveQuestionTag(t *testing.T) {
	svc, lib := newService(t)
	_, _, point := seedTree(t, svc)
	q := addQuestion(t, lib, "sha256:题图")

	if err := svc.AddQuestionTag(q.ID, point.ID); err != nil {
		t.Fatalf("AddQuestionTag: %v", err)
	}
	// 幂等：再挂一次不该报错也不该挂出第二份。
	if err := svc.AddQuestionTag(q.ID, point.ID); err != nil {
		t.Fatalf("重复 AddQuestionTag: %v", err)
	}
	if got, err := svc.TagsOfQuestion(q.ID); err != nil {
		t.Fatalf("TagsOfQuestion: %v", err)
	} else if len(got) != 1 {
		t.Errorf("挂着 %v，想要 1 个", names(got))
	}

	// 幂等：摘一个本来就没挂的也不报错。
	if err := svc.RemoveQuestionTag(q.ID, findSubject(t, svc, "英语").ID); err != nil {
		t.Fatalf("RemoveQuestionTag(没挂过): %v", err)
	}
	if err := svc.RemoveQuestionTag(q.ID, point.ID); err != nil {
		t.Fatalf("RemoveQuestionTag: %v", err)
	}
	if err := svc.RemoveQuestionTag(q.ID, point.ID); err != nil {
		t.Fatalf("重复 RemoveQuestionTag: %v", err)
	}
	if got, err := svc.TagsOfQuestion(q.ID); err != nil {
		t.Fatalf("TagsOfQuestion: %v", err)
	} else if len(got) != 0 {
		t.Errorf("摘完还剩 %v", names(got))
	}

	if err := svc.AddQuestionTag(404, point.ID); !errors.Is(err, library.ErrNotFound) {
		t.Errorf("给不存在的题挂标签返回 %v，想要 library.ErrNotFound", err)
	}
	if err := svc.AddQuestionTag(q.ID, 404); !errors.Is(err, tags.ErrNotFound) {
		t.Errorf("挂不存在的标签返回 %v，想要 tags.ErrNotFound", err)
	}
	if err := svc.RemoveQuestionTag(404, point.ID); !errors.Is(err, library.ErrNotFound) {
		t.Errorf("从不存在的题上摘标签返回 %v，想要 library.ErrNotFound", err)
	}
}

func TestTagsOfQuestions(t *testing.T) {
	svc, lib := newService(t)
	subject, chapter, point := seedTree(t, svc)

	rich := addQuestion(t, lib, "sha256:两个标签")
	lean := addQuestion(t, lib, "sha256:一个标签")
	bare := addQuestion(t, lib, "sha256:没标签")

	if err := svc.SetQuestionTags(rich.ID, []int64{subject.ID, point.ID}); err != nil {
		t.Fatalf("SetQuestionTags: %v", err)
	}
	if err := svc.SetQuestionTags(lean.ID, []int64{chapter.ID}); err != nil {
		t.Fatalf("SetQuestionTags: %v", err)
	}

	got, err := svc.TagsOfQuestions([]int64{rich.ID, lean.ID, bare.ID, rich.ID})
	if err != nil {
		t.Fatalf("TagsOfQuestions: %v", err)
	}
	// 顺序跟着传入的 ids，重复的去掉；没标签的题也在结果里。
	if len(got) != 3 {
		t.Fatalf("返回 %d 组，想要 3 组：%+v", len(got), got)
	}
	if got[0].QuestionID != rich.ID || got[1].QuestionID != lean.ID || got[2].QuestionID != bare.ID {
		t.Fatalf("返回的顺序不对: %d, %d, %d", got[0].QuestionID, got[1].QuestionID, got[2].QuestionID)
	}
	assertNames(t, got[0].Tags, subject.Name, point.Name)
	assertNames(t, got[1].Tags, chapter.Name)
	if got[2].Tags == nil {
		t.Error("没标签的题应当回空切片而不是 nil，前端要拿到 [] 不是 null")
	}
	if len(got[2].Tags) != 0 {
		t.Errorf("没标签的题却带着 %v", names(got[2].Tags))
	}

	empty, err := svc.TagsOfQuestions(nil)
	if err != nil {
		t.Fatalf("TagsOfQuestions(nil): %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Errorf("传空列表应当回空切片，回了 %+v", empty)
	}
}

// mustFilter 按标签筛错题，失败即终止测试。
func mustFilter(t *testing.T, svc *tags.Service, tagIDs []int64) []library.Question {
	t.Helper()

	qs, err := svc.QuestionsByTags(tagIDs)
	if err != nil {
		t.Fatalf("QuestionsByTags(%v): %v", tagIDs, err)
	}
	return qs
}

// 按标签筛：三个层级各自筛得出什么、多个标签取并集、没标签的题只出现在全量列表里。
func TestQuestionsByTags(t *testing.T) {
	svc, lib := newService(t)
	subject, chapter, point := seedTree(t, svc)
	sibling := create(t, svc, subject.ID, "线性代数")
	other := findSubject(t, svc, "英语")
	otherChapter := create(t, svc, other.ID, "阅读理解")

	// 三个层级的标签各挂了一道题，另有一道挂了别的学科，还有一道谁都没挂。
	// 创建时间一道比一道新（见 addQuestionAt 的注释）。
	oldest := addQuestionAt(t, lib, "sha256:挂了学科", baseTime)
	byChapter := addQuestionAt(t, lib, "sha256:挂了章节", baseTime.Add(time.Minute))
	newest := addQuestionAt(t, lib, "sha256:挂了知识点", baseTime.Add(2*time.Minute))
	foreign := addQuestionAt(t, lib, "sha256:挂了英语", baseTime.Add(3*time.Minute))
	bare := addQuestionAt(t, lib, "sha256:没挂标签", baseTime.Add(4*time.Minute))

	if err := svc.SetQuestionTags(oldest.ID, []int64{subject.ID}); err != nil {
		t.Fatalf("SetQuestionTags: %v", err)
	}
	if err := svc.SetQuestionTags(byChapter.ID, []int64{chapter.ID}); err != nil {
		t.Fatalf("SetQuestionTags: %v", err)
	}
	if err := svc.SetQuestionTags(newest.ID, []int64{point.ID}); err != nil {
		t.Fatalf("SetQuestionTags: %v", err)
	}
	if err := svc.SetQuestionTags(foreign.ID, []int64{otherChapter.ID}); err != nil {
		t.Fatalf("SetQuestionTags: %v", err)
	}

	// 一个标签都没勾 = 不筛，返回全部错题、新的在前。
	assertQuestionIDs(t, mustFilter(t, svc, nil), bare.ID, foreign.ID, newest.ID, byChapter.ID, oldest.ID)
	assertQuestionIDs(t, mustFilter(t, svc, []int64{}), bare.ID, foreign.ID, newest.ID, byChapter.ID, oldest.ID)

	// 筛知识点只出挂了它的那道。
	assertQuestionIDs(t, mustFilter(t, svc, []int64{point.ID}), newest.ID)
	// 筛章节带上它下面的知识点。
	assertQuestionIDs(t, mustFilter(t, svc, []int64{chapter.ID}), newest.ID, byChapter.ID)
	// 筛学科带上它下面所有章节与知识点。
	assertQuestionIDs(t, mustFilter(t, svc, []int64{subject.ID}), newest.ID, byChapter.ID, oldest.ID)
	// 兄弟章节下面没有题。
	assertQuestionIDs(t, mustFilter(t, svc, []int64{sibling.ID}))
	// 别的学科不受影响。
	assertQuestionIDs(t, mustFilter(t, svc, []int64{other.ID}), foreign.ID)

	// 多个标签取并集，同一道题只出现一次（它挂了学科又挂了章节），仍然新的在前。
	assertQuestionIDs(t, mustFilter(t, svc, []int64{subject.ID, other.ID}), foreign.ID, newest.ID, byChapter.ID, oldest.ID)
	// 重复传同一个 id 不该把题重复列出来。
	assertQuestionIDs(t, mustFilter(t, svc, []int64{point.ID, point.ID}), newest.ID)

	// 不存在的标签筛不出东西，但不算错（它可能刚被别处删掉）。
	assertQuestionIDs(t, mustFilter(t, svc, []int64{404}))
	// 不存在的 id 混在有效的里面：有效的照常生效。
	assertQuestionIDs(t, mustFilter(t, svc, []int64{404, point.ID}), newest.ID)
}

// 标签被删掉之后，那道题仍在全量列表里 —— 摘的是标签，不是题。
func TestQuestionsSurviveTagDeletion(t *testing.T) {
	svc, lib := newService(t)
	_, _, point := seedTree(t, svc)
	q := addQuestion(t, lib, "sha256:题图")

	if err := svc.SetQuestionTags(q.ID, []int64{point.ID}); err != nil {
		t.Fatalf("SetQuestionTags: %v", err)
	}
	if _, err := svc.Delete(point.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	assertQuestionIDs(t, mustFilter(t, svc, nil), q.ID)
	assertQuestionIDs(t, mustFilter(t, svc, []int64{point.ID}))
}

// 删错题要把它的标签关联一起带走，不能留下指向不存在错题的孤儿行
// （外键 cascade 做的，dsn 里开着 foreign_keys —— 这条测试就是钉住那件事）。
func TestDeleteQuestionLeavesNoOrphanLinks(t *testing.T) {
	svc, lib := newService(t)
	_, _, point := seedTree(t, svc)
	q := addQuestion(t, lib, "sha256:题图")

	if err := svc.SetQuestionTags(q.ID, []int64{point.ID}); err != nil {
		t.Fatalf("SetQuestionTags: %v", err)
	}
	if _, err := lib.DeleteQuestion(q.ID); err != nil {
		t.Fatalf("DeleteQuestion: %v", err)
	}

	// 孤儿行只影响存储、不影响查询结果（查询会 join 掉它们），服务层看不出来，
	// 所以这里直接读一次库。这是本文件唯一一处绕过服务层的地方。
	var orphans int
	err := lib.DB().QueryRow(`SELECT count(*) FROM question_tags WHERE question_id = ?`, q.ID).Scan(&orphans)
	if err != nil {
		t.Fatalf("数孤儿行: %v", err)
	}
	if orphans != 0 {
		t.Errorf("删题之后还剩 %d 条关联行", orphans)
	}
}

// 关掉再打开：预置学科不会长第二遍，用户建的标签与挂的标签都还在。
func TestReopenKeepsTags(t *testing.T) {
	path := filepath.Join(t.TempDir(), "library.db")

	lib, err := library.Open(path)
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	svc := tags.NewService(lib)

	subject, chapter, point := seedTree(t, svc)
	q := addQuestion(t, lib, "sha256:题图")
	if err := svc.SetQuestionTags(q.ID, []int64{point.ID}); err != nil {
		t.Fatalf("SetQuestionTags: %v", err)
	}
	// 删掉一个预置学科，重开之后它不该自己长回来。
	if _, err := svc.Delete(findSubject(t, svc, "政治").ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := lib.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := library.Open(path)
	if err != nil {
		t.Fatalf("重开失败: %v", err)
	}
	t.Cleanup(func() { reopened.Close() })
	svc = tags.NewService(reopened)

	assertNames(t, list(t, svc), "数学", "英语", "专业课", "高等数学", "洛必达法则")
	if got, err := svc.TagsOfQuestion(q.ID); err != nil {
		t.Fatalf("TagsOfQuestion: %v", err)
	} else if len(got) != 1 || got[0].ID != point.ID {
		t.Errorf("重开之后挂着 %v，想要 %q", names(got), point.Name)
	}
	if got := mustFilter(t, svc, []int64{subject.ID}); len(got) != 1 || got[0].ID != q.ID {
		t.Errorf("重开之后按学科筛出 %v，想要那道题", questionIDs(got))
	}
	if got := mustFilter(t, svc, []int64{chapter.ID}); len(got) != 1 {
		t.Errorf("重开之后按章节筛出 %v", questionIDs(got))
	}
}

// 老库（票据 05 时期的形状：只有 questions 表、模式版本停在 1）升上来：
// 标签表建出来、预置学科补齐、原来的错题一条不少。
func TestUpgradeFromOlderSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "library.db")

	lib, err := library.Open(path)
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	q := addQuestion(t, lib, "sha256:老库里就有的题")

	// 把库退回票据 05 的形状：标签表还没建、版本号停在 1。
	// 这是测试在**扮演**一个老库文件，不是绕过服务层的实现细节。
	//
	// 每加一条新迁移，这里就要把那条迁移建的**表**也删掉（像下面 review 那两张）：
	// 退回的是「那个年代真实存在过的库」，而不是「只少了标签表的库」。漏掉的话，
	// 重开时那条迁移会撞上已经存在的表。
	for _, stmt := range []string{
		`DROP TABLE question_tags`,
		`DROP TABLE tags`,
		`DROP TABLE review_logs`,
		`DROP TABLE review_states`,
		`PRAGMA user_version = 1`,
	} {
		if _, err := lib.DB().Exec(stmt); err != nil {
			t.Fatalf("退回老模式 (%s): %v", stmt, err)
		}
	}
	if err := lib.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	upgraded, err := library.Open(path)
	if err != nil {
		t.Fatalf("重开失败: %v", err)
	}
	t.Cleanup(func() { upgraded.Close() })
	svc := tags.NewService(upgraded)

	assertNames(t, list(t, svc), presets...)

	// 老数据还在，而且能给它打标签。
	qs, err := upgraded.ListQuestions()
	if err != nil {
		t.Fatalf("ListQuestions: %v", err)
	}
	if len(qs) != 1 || qs[0].ID != q.ID {
		t.Fatalf("升级之后错题变了: %v，想要 %d", questionIDs(qs), q.ID)
	}
	subject := findSubject(t, svc, "数学")
	if err := svc.SetQuestionTags(q.ID, []int64{subject.ID}); err != nil {
		t.Fatalf("SetQuestionTags: %v", err)
	}
	if got := mustFilter(t, svc, []int64{subject.ID}); len(got) != 1 {
		t.Errorf("升级之后按标签筛出 %v", questionIDs(got))
	}
}
