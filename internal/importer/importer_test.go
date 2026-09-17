package importer_test

// 这个文件测「解包这一半」：解出来、验一遍、摆进暂存目录。
//
// 三条贯穿全篇的断言，每条都是**两侧都断**（正例与反例）：
//
//   - 装不了的东西必须被拒，而且拒的时候**正式数据一个字节都没动**（见 assertUntouched）。
//   - 能装的东西装进去之后，整库都过来了 —— 判据是「每张表的行数」这个签名，不是几张写死的表。
//   - 包是应用之外的文件：里面的条目名不配决定往哪儿写（见 zip-slip 那两条）。

import (
	"archive/zip"
	"bytes"
	"errors"
	"image"
	"image/color"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"questionbook/internal/capture"
	"questionbook/internal/export"
	"questionbook/internal/importer"
	"questionbook/internal/library"
	"questionbook/internal/review"
	"questionbook/internal/tags"
)

// fixedNow 是一个钉死的时刻：备份目录名与 manifest 里的时间要能对上。
var fixedNow = func() time.Time { return time.Date(2026, 9, 17, 21, 30, 0, 0, time.UTC) }

// bundleImages 把采集那侧的图片文件层接到导出的 ImageFiles 上（与 main.go 那层薄壳同形）。
type bundleImages struct{ store *capture.Store }

func (i bundleImages) PathByHash(hash string) string { return i.store.Path(capture.Hash(hash)) }

// fakePicker 冒充宿主的选择器 —— 这一层要能在没有界面、没有安卓的情况下跑。
type fakePicker struct {
	path string
	err  error
}

func (p *fakePicker) PickZip() (string, error) { return p.path, p.err }

// env 是一次导入测试的环境：一个数据根目录（库、图片、那几份 json 都在它下面）、
// 一个真的导出服务（用来造包），以及接好的导入服务。
type env struct {
	t      *testing.T
	root   string
	store  *library.Store
	images *capture.Store
	expo   *export.Service
	picker *fakePicker
	svc    *importer.Service
}

func newEnv(t *testing.T) *env { return newEnvIn(t, t.TempDir()) }

// newEnvIn 在一个指定目录上开一套环境 —— 往返测试要两台「机器」，各自一个根目录。
func newEnvIn(t *testing.T, root string) *env {
	t.Helper()

	store, err := library.Open(filepath.Join(root, export.DBName))
	if err != nil {
		t.Fatalf("开库: %v", err)
	}
	images, err := capture.NewStore(filepath.Join(root, export.ImageDir))
	if err != nil {
		t.Fatalf("开图片目录: %v", err)
	}

	e := &env{t: t, root: root, store: store, images: images, picker: &fakePicker{}}
	e.expo = export.NewService(store, bundleImages{store: images},
		export.WithTempDir(root), export.WithBundleExtras(e.settingPaths()...))
	e.svc = importer.NewService(root, store, e.picker, importer.WithNow(fixedNow))
	t.Cleanup(func() {
		if e.store != nil {
			e.store.Close()
		}
	})
	return e
}

// settingPaths 是那两份库外设置在本机的位置（导出要按绝对路径指它们）。
func (e *env) settingPaths() []string {
	out := make([]string, 0, len(export.SettingsNames))
	for _, name := range export.SettingsNames {
		out = append(out, filepath.Join(e.root, name))
	}
	return out
}

// close 把库关掉。ApplyPending 要在**没有任何连接**的时候跑 —— 不只是并发问题：
// 库还开着的时候，Windows 上连改名都做不到。
func (e *env) close() {
	e.t.Helper()
	if e.store != nil {
		if err := e.store.Close(); err != nil {
			e.t.Fatalf("关库: %v", err)
		}
		e.store = nil
	}
}

// reopen 把库与那一整套服务重新开起来（模拟下一次启动）。
func (e *env) reopen() {
	e.t.Helper()
	*e = *newEnvIn(e.t, e.root)
	// newEnvIn 里挂的 cleanup 会再关一次，那是 no-op（Close 之后又是同一个对象）。
}

// relaunch 模拟一次重启：关库 → 落地 → 重新开库。
func (e *env) relaunch() importer.Result {
	e.t.Helper()
	e.close()
	res, err := importer.ApplyPending(e.root, fixedNow)
	if err != nil {
		e.t.Fatalf("ApplyPending: %v", err)
	}
	e.reopen()
	return res
}

// rgba 造一个不透明的颜色，省得每处都写 A: 255。
func rgba(r, g, b uint8) color.RGBA { return color.RGBA{R: r, G: g, B: b, A: 255} }

// saveImage 造一张纯色图落进图片目录，返回它的内容 hash。
func (e *env) saveImage(c color.RGBA) string {
	e.t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for y := range 4 {
		for x := range 4 {
			img.SetRGBA(x, y, c)
		}
	}
	h, err := e.images.Save(img)
	if err != nil {
		e.t.Fatalf("存图: %v", err)
	}
	return h.String()
}

// addQuestion 往库里落一道错题。
func (e *env) addQuestion(questionHash, answerHash string) library.Question {
	e.t.Helper()
	q, err := e.store.AddQuestion(library.Question{QuestionHash: questionHash, AnswerHash: answerHash})
	if err != nil {
		e.t.Fatalf("落库错题: %v", err)
	}
	return q
}

// writeSetting 在本机放一份库外设置。
func (e *env) writeSetting(name, content string) {
	e.t.Helper()
	if err := os.WriteFile(filepath.Join(e.root, name), []byte(content), 0o600); err != nil {
		e.t.Fatalf("写 %s: %v", name, err)
	}
}

// bundleZip 导出一次，把包写成一个真文件，返回它的路径（选择器要交给导入的就是这个）。
func (e *env) bundleZip() string {
	e.t.Helper()
	var buf bytes.Buffer
	if _, err := e.expo.WriteBundle(&buf); err != nil {
		e.t.Fatalf("导出: %v", err)
	}
	path := filepath.Join(e.t.TempDir(), "错题本.zip")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		e.t.Fatalf("写出包: %v", err)
	}
	return path
}

// pickAndPrepare 走一遍「用户点了按钮」那条路（选择器 + 解包）。
func (e *env) pickAndPrepare(zipPath string) (importer.Preview, error) {
	e.t.Helper()
	e.picker.path = zipPath
	picked, err := e.svc.PickAndPrepare()
	if err != nil {
		return importer.Preview{}, err
	}
	return picked.Preview, nil
}

// confirm 走一遍「用户点了确认」那一步。
func (e *env) confirm() {
	e.t.Helper()
	if err := e.svc.Commit(); err != nil {
		e.t.Fatalf("Commit: %v", err)
	}
}

// tableCounts 给库里每张表数一遍行数，作为「整库都搬过来了」的签名。
//
// 不写死表名：搬的是整个库，所以复习状态、讨论、待批准这些表自然也在里面。写死几张表的
// 写法会让「以后新加的表没搬过来」悄悄溜过去 —— 而那正是这个功能唯一要保证的事。
func tableCounts(t *testing.T, s *library.Store) map[string]int {
	t.Helper()
	rows, err := s.DB().Query(
		`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatalf("列库里的表: %v", err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("读表名: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("读表名: %v", err)
	}
	rows.Close()

	if len(names) == 0 {
		t.Fatal("一张表都没有 —— 这个签名是空的，等于什么都没断言")
	}
	out := make(map[string]int, len(names))
	for _, name := range names {
		var n int
		// 表名来自 sqlite_master（我们自己的库），不是外部输入。
		if err := s.DB().QueryRow(`SELECT COUNT(*) FROM ` + name).Scan(&n); err != nil {
			t.Fatalf("数 %s 的行数: %v", name, err)
		}
		out[name] = n
	}
	return out
}

// assertUntouched 断言这一次尝试**没有动过正式数据**：行数签名一样，而且没留下备份目录
// （留下备份目录说明换库那一步已经开跑了 —— 那正是要排除的情形）。
func (e *env) assertUntouched(before map[string]int, what string) {
	e.t.Helper()
	if after := tableCounts(e.t, e.store); !reflect.DeepEqual(before, after) {
		e.t.Errorf("%s：库被动了。前 %v，后 %v", what, before, after)
	}
	entries, err := os.ReadDir(e.root)
	if err != nil {
		e.t.Fatalf("列数据根目录: %v", err)
	}
	for _, entry := range entries {
		if len(entry.Name()) > 4 && entry.Name()[:4] == "bak-" {
			e.t.Errorf("%s：留下了备份目录 %s，说明换库已经开跑了", what, entry.Name())
		}
	}
}

// ── 正例：整库往返 ──

// 换手机那条路：源机器上一个有内容的错题本导出成一个包，新机器（空库）把它装进去，
// 两边的**每张表行数**逐项对上，图片读得回来，设置也跟过去。
func TestImportRestoresWholeLibrary(t *testing.T) {
	src := newEnv(t)
	first := src.addQuestion(src.saveImage(color.RGBA{R: 255, A: 255}), src.saveImage(color.RGBA{G: 255, A: 255}))
	second := src.addQuestion(src.saveImage(color.RGBA{B: 255, A: 255}), "")
	third := src.addQuestion(src.saveImage(color.RGBA{R: 255, G: 255, A: 255}), src.saveImage(color.RGBA{R: 255, G: 255, B: 255, A: 255}))

	// 一个自己建的标签挂在题上（证明 tags / question_tags 两张表也过来了）。
	tagsSvc := tags.NewService(src.store)
	subject, err := tagsSvc.Create(0, "信号与系统")
	if err != nil {
		t.Fatalf("建标签: %v", err)
	}
	if err := tagsSvc.SetQuestionTags(first.ID, []int64{subject.ID}); err != nil {
		t.Fatalf("挂标签: %v", err)
	}
	// 一次复习记录（证明复习状态那张表也过来了 —— 它决定了下次到期。
	// 搬不过去的话，新手机上整本错题会一次全到期）。
	reviewSvc := review.NewService(src.store, filepath.Join(src.root, "review.json"))
	if _, err := reviewSvc.Grade(second.ID, review.Good); err != nil {
		t.Fatalf("评级: %v", err)
	}
	// 两份库外设置。
	src.writeSetting("review.json", `{"fuzz":true,"retention":0.95}`)
	src.writeSetting("digest.json", `{"enabled":true,"hour":21,"minute":0}`)

	want := tableCounts(t, src.store)
	zipPath := src.bundleZip()
	if len(want) == 0 {
		t.Fatal("源库里一张表都没有")
	}

	// 新机器：一个空库（刚装上的样子，迁移已经跑过、预置了四个学科）。
	dst := newEnvIn(t, t.TempDir())
	before := tableCounts(t, dst.store)

	preview, err := dst.pickAndPrepare(zipPath)
	if err != nil {
		t.Fatalf("PickAndPrepare: %v", err)
	}
	// 三道题上一共 5 个不同的 hash：R、G、B、R+G、R+G+B。
	if preview.Questions != 3 || preview.Images != 5 {
		t.Errorf("预览说 %d 道题 / %d 张图，想要 3 / 5", preview.Questions, preview.Images)
	}
	if preview.Current != 0 {
		t.Errorf("现在库里有 %d 道题，想要 0（新机器）", preview.Current)
	}
	if !slices.Equal(preview.Settings, export.SettingsNames) {
		t.Errorf("预览说包里带了 %v，想要 %v", preview.Settings, export.SettingsNames)
	}
	// 还没确认，正式数据不该动。
	if after := tableCounts(t, dst.store); !reflect.DeepEqual(before, after) {
		t.Errorf("解包阶段就动了正式数据：前 %v，后 %v", before, after)
	}

	// 解好了但还没确认：磁盘上有那个包，但**它不会**在下次启动落地。
	// 这条与下面那条一起把 Pending 的语义钉死 —— 它说的是「已经确认、等重启」，
	// 不是「解好了什么东西放在那儿」。
	pending, err := dst.svc.Pending()
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if pending.Ready {
		t.Errorf("还没确认就报就绪：%+v", pending)
	}

	dst.confirm()

	pending, err = dst.svc.Pending()
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if !pending.Ready || pending.Preview.Questions != 3 || pending.Preview.Images != 5 {
		t.Errorf("Pending = %+v，想要就绪且 3 道题 5 张图", pending)
	}
	res := dst.relaunch()

	if !res.OK {
		t.Fatalf("落地失败：%+v", res)
	}
	if res.Questions != 3 || res.Images != 5 {
		t.Errorf("结果说 %d 道题 / %d 张图，想要 3 / 5", res.Questions, res.Images)
	}
	if res.Backup == "" {
		t.Error("旧库该被挪到备份目录里（换机器时它是个空库，那也是旧库）")
	}

	// 逐张表对上 —— 这是「整库都过来了」唯一说得清的判据。
	if got := tableCounts(t, dst.store); !reflect.DeepEqual(want, got) {
		t.Errorf("导进来之后每张表的行数与源库不同：\n源 %v\n新 %v", want, got)
	}

	// 图片真的能读回来（不是只搬了个名字）。
	for _, q := range []library.Question{first, second, third} {
		img, err := dst.images.Load(capture.Hash(q.QuestionHash))
		if err != nil {
			t.Fatalf("读回题图 %s: %v", q.QuestionHash, err)
		}
		if img.Bounds().Empty() {
			t.Errorf("读回来的题图 %s 是空的", q.QuestionHash)
		}
	}

	// 标签按名字找得到（行数对上已经说明问题，这里顺带看一眼内容）。
	got, err := tags.NewService(dst.store).List()
	if err != nil {
		t.Fatalf("列标签: %v", err)
	}
	found := false
	for _, tag := range got {
		if tag.Name == "信号与系统" {
			found = true
		}
	}
	if !found {
		t.Errorf("自己建的那个学科没跟过来：%v", got)
	}

	// 那两份设置原样落地。
	for _, name := range export.SettingsNames {
		wantRaw, err := os.ReadFile(filepath.Join(src.root, name))
		if err != nil {
			t.Fatalf("读源机器的 %s: %v", name, err)
		}
		gotRaw, err := os.ReadFile(filepath.Join(dst.root, name))
		if err != nil {
			t.Fatalf("读新机器的 %s: %v", name, err)
		}
		if !bytes.Equal(wantRaw, gotRaw) {
			t.Errorf("%s 跟过来之后内容不同：%q vs %q", name, wantRaw, gotRaw)
		}
	}

	// 暂存目录用完了就该没了。
	if _, err := os.Stat(filepath.Join(dst.root, "import-pending")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("落地之后暂存目录还在：%v", err)
	}
}

// 包是**按内容**认图的：同一个 hash 被两道题引用时，导入只写一份，且不报错。
func TestImportDeduplicatesSharedImage(t *testing.T) {
	src := newEnv(t)
	shared := src.saveImage(color.RGBA{R: 255, A: 255})
	src.addQuestion(shared, "")
	src.addQuestion(src.saveImage(color.RGBA{G: 255, A: 255}), shared)

	zipPath := src.bundleZip()

	dst := newEnvIn(t, t.TempDir())
	preview, err := dst.pickAndPrepare(zipPath)
	if err != nil {
		t.Fatalf("PickAndPrepare: %v", err)
	}
	if preview.Images != 2 {
		t.Errorf("包里说 %d 张图，想要 2 张（共用的那张只算一次）", preview.Images)
	}
}

// ── 反例：装不了的东西 ──

// 包是应用之外的文件，所以每一种坏法都要在**动手之前**被认出来，而且
// 正式数据一个字节都不能动。
func TestPrepareRejectsBadBundles(t *testing.T) {
	// goodBundle 造一个正常的包，再由各个子测试去改坏它。
	goodBundle := func(t *testing.T) string {
		t.Helper()
		src := newEnv(t)
		src.addQuestion(src.saveImage(color.RGBA{R: 255, A: 255}), src.saveImage(color.RGBA{G: 255, A: 255}))
		src.writeSetting("review.json", `{"fuzz":true}`)
		return src.bundleZip()
	}

	cases := []struct {
		name    string
		corrupt func(t *testing.T, path string)
	}{
		{
			"包里没有 library.db",
			func(t *testing.T, path string) {
				rewriteZip(t, path, func(entries map[string][]byte) {
					delete(entries, export.DBName)
				})
			},
		},
		{
			"库里引用的图不在包里",
			func(t *testing.T, path string) {
				rewriteZip(t, path, func(entries map[string][]byte) {
					for name := range entries {
						if len(name) > 0 && name != export.DBName && name != "review.json" {
							delete(entries, name)
							return
						}
					}
				})
			},
		},
		{
			"图的内容与它的名字对不上",
			func(t *testing.T, path string) {
				// 把两张图的内容互换：名字还是那个 hash，内容已经不是了。
				rewriteZip(t, path, func(entries map[string][]byte) {
					var cards []string
					for name := range entries {
						if len(name) > len(export.ImageDir)+1 && name[:len(export.ImageDir)+1] == export.ImageDir+"/" {
							cards = append(cards, name)
						}
					}
					if len(cards) != 2 {
						t.Fatalf("这个包里有 %d 张图，互换内容这条前提不成立", len(cards))
					}
					entries[cards[0]], entries[cards[1]] = entries[cards[1]], entries[cards[0]]
				})
			},
		},
		{
			"库的模式比本应用新",
			func(t *testing.T, path string) {
				future := filepath.Join(t.TempDir(), "future.db")
				db, err := library.Open(future)
				if err != nil {
					t.Fatalf("建库: %v", err)
				}
				if _, err := db.DB().Exec("PRAGMA user_version = 999"); err != nil {
					t.Fatalf("改版本号: %v", err)
				}
				db.Close()

				raw, err := os.ReadFile(future)
				if err != nil {
					t.Fatalf("读: %v", err)
				}
				rewriteZip(t, path, func(entries map[string][]byte) { entries[export.DBName] = raw })
			},
		},
		{
			"包里那份设置不是 JSON",
			func(t *testing.T, path string) {
				rewriteZip(t, path, func(entries map[string][]byte) { entries["review.json"] = []byte("这不是 json") })
			},
		},
		{
			"它根本不是一个 zip",
			func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte("我不是压缩包"), 0o600); err != nil {
					t.Fatalf("写坏包: %v", err)
				}
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			zipPath := goodBundle(t)
			c.corrupt(t, zipPath)

			// 目标机器：有一份自己的数据，它必须**一个字节都不动**。
			dst := newEnvIn(t, t.TempDir())
			dst.addQuestion(dst.saveImage(color.RGBA{B: 255, A: 255}), "")
			dst.writeSetting("review.json", `{"fuzz":false,"mine":true}`)
			before := tableCounts(t, dst.store)
			beforeReview := readFile(t, filepath.Join(dst.root, "review.json"))

			_, err := dst.pickAndPrepare(zipPath)
			if !errors.Is(err, importer.ErrBundleIncomplete) {
				t.Fatalf("PickAndPrepare 返回 %v，想要 ErrBundleIncomplete", err)
			}
			dst.assertUntouched(before, "拒绝这个包的时候")
			if got := readFile(t, filepath.Join(dst.root, "review.json")); got != beforeReview {
				t.Errorf("设置被动了：%q → %q", beforeReview, got)
			}
			// 解坏的包不该留下暂存目录。
			if _, err := os.Stat(filepath.Join(dst.root, "import-pending")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("拒绝了还留着暂存目录：%v", err)
			}
		})
	}
}

// 包里塞进来的越界条目名不能决定往哪儿写 —— 这是 zip-slip。
//
// 两种写法都试：`../` 直接跳出去、以及伪装成一张图的 `cards/../../`。判据是
// **暂存目录之外一个字节都没多出来**，而包本身照常能装（多出来的条目一律不看）。
func TestPrepareIgnoresEntriesOutsideTheBundleShape(t *testing.T) {
	src := newEnv(t)
	src.addQuestion(src.saveImage(color.RGBA{R: 255, A: 255}), "")
	zipPath := src.bundleZip()

	outside := filepath.Join(filepath.Dir(zipPath), "被越界写出来的东西.txt")
	rewriteZip(t, zipPath, func(entries map[string][]byte) {
		entries["../"+filepath.Base(outside)] = []byte("越界")
		entries["cards/../../"+filepath.Base(outside)] = []byte("越界")
		entries["cards/not-a-hash.png"] = []byte("名字不是 hash")
	})

	dst := newEnvIn(t, t.TempDir())
	preview, err := dst.pickAndPrepare(zipPath)
	if err != nil {
		t.Fatalf("PickAndPrepare 不该因为这些条目失败: %v", err)
	}
	if preview.Questions != 1 || preview.Images != 1 {
		t.Errorf("预览 = %+v，想要 1 道题 1 张图（多出来的条目不算）", preview)
	}
	dst.confirm()
	if res := dst.relaunch(); !res.OK {
		t.Fatalf("落地失败：%+v", res)
	}

	if _, err := os.Stat(outside); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("越界条目真的写出去了一份：%s", outside)
	}
	// 那个名字不像 hash 的条目也不该在图片目录里落地。
	if _, err := os.Stat(filepath.Join(dst.root, export.ImageDir, "not-a-hash.png")); !errors.Is(err, os.ErrNotExist) {
		t.Error("名字不是 <hash>.png 的条目被当成图写进去了")
	}
}

// 用户取消选择（选择器返回空路径）**不是错误** —— 界面不该弹一句红的，也不该留下任何东西。
func TestPickCancelledIsNotAnError(t *testing.T) {
	e := newEnvIn(t, t.TempDir())
	e.picker.path = "" // 什么都没选

	picked, err := e.svc.PickAndPrepare()
	if err != nil {
		t.Errorf("取消不该报成错误: %v", err)
	}
	if !picked.Cancelled {
		t.Errorf("取消该被报成 Cancelled: %+v", picked)
	}
	if _, err := os.Stat(filepath.Join(e.root, "import-pending")); !errors.Is(err, os.ErrNotExist) {
		t.Error("取消之后不该留下暂存目录")
	}
}

// 没有待落地的导入时，Pending 是「没有」，Cancel 也不报错。
func TestNothingPending(t *testing.T) {
	e := newEnvIn(t, t.TempDir())

	pending, err := e.svc.Pending()
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if pending.Ready {
		t.Errorf("什么都没解就有待落地的：%+v", pending)
	}
	if err := e.svc.Cancel(); err != nil {
		t.Errorf("Cancel 在没有待落地时也该成功: %v", err)
	}
	if last := e.svc.Last(); last.OK {
		t.Errorf("从没落地过，Last() 该是零值：%+v", last)
	}
}

// 用户改主意：解完不确认，或者确认了又取消 —— 暂存的东西都得清干净，正式数据不动。
func TestCancelDropsStagedBundle(t *testing.T) {
	src := newEnv(t)
	src.addQuestion(src.saveImage(color.RGBA{R: 255, A: 255}), "")
	zipPath := src.bundleZip()

	dst := newEnvIn(t, t.TempDir())
	before := tableCounts(t, dst.store)
	if _, err := dst.pickAndPrepare(zipPath); err != nil {
		t.Fatalf("PickAndPrepare: %v", err)
	}
	dst.confirm()

	if err := dst.svc.Cancel(); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst.root, "import-pending")); !errors.Is(err, os.ErrNotExist) {
		t.Error("取消之后暂存目录还在")
	}
	// 之后重启也不该发生任何事。
	res := dst.relaunch()
	if res.OK || res.Questions != 0 {
		t.Errorf("取消之后重启竟然落地了：%+v", res)
	}
	dst.assertUntouched(before, "取消之后")
}

// 老包也能装回来：包里那份库的模式比本应用旧时，导入这一侧要把它迁到最新。
//
// 造法是把库里「比第 1 版新的那些表」删掉、把 user_version 退回 1（library 那边给了
// TablesIntroducedAfter，正是为了这种测试）。不这么造的话，这份「老包」只存在于想象里。
func TestImportMigratesOlderBundle(t *testing.T) {
	src := newEnv(t)
	src.addQuestion(src.saveImage(color.RGBA{R: 255, A: 255}), "")
	zipPath := src.bundleZip()

	// 把包里的库退回版本 1：删掉那之后建的表，user_version 也跟着退。
	old := filepath.Join(t.TempDir(), "old.db")
	db, err := library.Open(old)
	if err != nil {
		t.Fatalf("建库: %v", err)
	}
	for _, table := range library.TablesIntroducedAfter(1) {
		if _, err := db.DB().Exec("DROP TABLE IF EXISTS " + table); err != nil {
			t.Fatalf("删表 %s: %v", table, err)
		}
	}
	if _, err := db.DB().Exec("PRAGMA user_version = 1"); err != nil {
		t.Fatalf("退回版本号: %v", err)
	}
	db.Close()

	raw, err := os.ReadFile(old)
	if err != nil {
		t.Fatalf("读: %v", err)
	}
	// 老库旁边的旁挂文件不跟着走 —— 包里那份库是单独一个文件。
	rewriteZip(t, zipPath, func(entries map[string][]byte) { entries[export.DBName] = raw })

	dst := newEnvIn(t, t.TempDir())
	if _, err := dst.pickAndPrepare(zipPath); err != nil {
		t.Fatalf("PickAndPrepare: %v", err)
	}
	dst.confirm()
	if res := dst.relaunch(); !res.OK {
		t.Fatalf("落地失败：%+v", res)
	}

	// 迁到位了就说明那些表又回来了（它们跟库是一个整体）。
	var version int
	if err := dst.store.DB().QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("读模式版本: %v", err)
	}
	if version != latestVersionOf(t) {
		t.Errorf("导进来之后模式版本是 %d，想要 %d", version, latestVersionOf(t))
	}
}

// latestVersionOf 问一个**刚开出来的空库**要当前模式版本 —— 测试不该自己去抄那个数字。
func latestVersionOf(t *testing.T) int {
	t.Helper()
	db, err := library.Open(filepath.Join(t.TempDir(), "probe.db"))
	if err != nil {
		t.Fatalf("建探针库: %v", err)
	}
	defer db.Close()
	var version int
	if err := db.DB().QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("读模式版本: %v", err)
	}
	return version
}

// ── 小工具 ──

// rewriteZip 把包拆成「条目名 → 内容」，交给 mutate 改，再原样写回。
//
// 测试要造的那些坏包（缺图、内容对不上、越界条目）都得从**真包**出发 —— 手搓一个 zip
// 会连「包该长什么样」这件事一起抄错，而那正是这些测试要盯的东西。
func rewriteZip(t *testing.T, path string, mutate func(entries map[string][]byte)) {
	t.Helper()

	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("打开包: %v", err)
	}
	entries := map[string][]byte{}
	var order []string
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("打开条目 %s: %v", f.Name, err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("读条目 %s: %v", f.Name, err)
		}
		entries[f.Name] = data
		order = append(order, f.Name)
	}
	zr.Close()

	mutate(entries)

	// 新增的条目排在后面（顺序不影响导入，但保持确定）。
	for name := range entries {
		if !slices.Contains(order, name) {
			order = append(order, name)
		}
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range order {
		data, ok := entries[name]
		if !ok {
			continue
		}
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate})
		if err != nil {
			t.Fatalf("建条目 %s: %v", name, err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatalf("写条目 %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("收尾包: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("写回包: %v", err)
	}
}

// readFile 读一个文件，读不到就 Fatal。给「设置没被动过」那类断言用。
func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	return string(raw)
}
