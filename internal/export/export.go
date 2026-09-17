// Package export 把整个错题本打成一个 zip —— **导出包**（CONTEXT.md）。
//
// 为什么导出必须是一个包而不是一个库文件：图片不在库里、按内容 hash 命名落在库外
// （ADR-0004），裸拷一份 library.db 得到的是「一堆指向不存在图片的记录」。
// 沙箱意味着卸载即全没，这个包是防丢的唯一手段（spec 的导出）。
//
// 包里的布局就两样，与库和图片在本机私有目录下的布局同名 —— 解包回去即同一份：
//
//	library.db          库
//	cards/<hash>.png    库里引用到的每一张图，按内容 hash 命名
//
// 打包本身是纯的：往哪儿写由调用方给一个 io.Writer（WriteBundle），本包不碰安卓存储。
// 落到系统「下载」目录要走 MediaStore，那段在 Java 宿主里，与打包分开做
// （票据 13 的第 2 步）：本包负责把包**打好在私有目录里**（Stage），宿主负责搬出去。
package export

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"questionbook/internal/library"
)

const (
	// DBName 是包里库文件的名字。
	DBName = "library.db"

	// ImageDir 是包里图片所在的那一层目录。
	//
	// 与 main.go 交给 capture.NewStore 的那个目录同名，里头的图也同名（<hash>.png）：
	// 包是本机私有目录的镜像，解包那一侧不必再做一次名字映射。
	ImageDir = "cards"
)

// SettingsNames 是包里可能带的那两份**库外设置**的名字（都不含凭据）。
//
// 两处都要这份清单：导出那边由 main.go 按它拼路径交给 WithBundleExtras，导入那边
// 按它去包里找。定义在这里，是因为「包里有什么」这件事归这个包管 —— 抄第二份，
// 迟早会有一边改了名字而另一边没有。
//
// vlm.json 刻意不在里面：它带 API key，而包会落到「下载」目录。
var SettingsNames = []string{"review.json", "digest.json"}

// ErrImageMissing 表示库里引用的一张图在盘上找不到。判断用 errors.Is。
//
// 单独立一个哨兵，是因为它要告知用户的东西与「磁盘写满」那类失败不一样：
// 一张图已经丢了，重试没有用；而且这时**包不能出** —— 一个缺图的备份看着像备份，
// 等真要用的时候才发现少了东西，那比没有备份更坏。
var ErrImageMissing = errors.New("导出: 库里的图在盘上找不到")

// ImageFiles 是导出对图片文件层要的那一面：按内容 hash 拿到库外那张图的**文件路径**。
//
// 要路径而不是要 io.Reader，是因为导出在动手写第一个字节之前要把每张图都核一遍
// （见 WriteBundle 的「要么是个完整的包，要么什么都不写」）；换成逐个开文件句柄核完
// 再一路开着写，在手机上就是几百个 fd 的事。
//
// 名字也不由导出拼：文件名怎么拼是文件层的事（capture.Store.Path 那句「调用方别自己
// 去拼」），包里的文件名就该是盘上的文件名。采集那边的 *capture.Store 满足不了这个形状
// （它的 Path 收的是具名类型 capture.Hash），所以接线时要在 main.go 里加一层薄壳，
// 与 library.ImageStore 那条边界同一个做法。
type ImageFiles interface {
	PathByHash(hash string) string
}

// Service 是「导出」对前端暴露的那一面。
//
// 与标签、复习一样，它借的是**题库那一条连接**（ADR-0004 只把图片放到库外，
// 元数据都在同一份 SQLite 里），不再 Open 第二个到同一文件的连接。
type Service struct {
	questions *library.Store
	images    ImageFiles
	// 中间那份库快照落哪儿。默认 os.TempDir()，安卓上得由 main.go 换成应用私有目录
	// —— 见 WithTempDir。暂存的导出包也落在这儿（见 Stage）。
	tempDir string
	// extras 是除库与图之外还要打进包的**库外设置文件**（见 WithBundleExtras）。
	// 存的是绝对路径，条目名取 base name —— 与「包里与私有目录同名」那条规矩一致。
	extras []string
	// now 取当前时刻。做成字段只为一件事：Stage 用日期当文件名，测试要能把它钉死。
	now func() time.Time
}

// Option 给服务补一样可选能力。
type Option func(*Service)

// WithTempDir 换掉中间那份库快照的落点。
//
// 快照要写一个临时文件（见 snapshotDB），而 os.TempDir() 在安卓上是 /tmp 或
// $TMPDIR —— 都没有保证可写。接线时把它指到应用私有目录下。
func WithTempDir(dir string) Option {
	return func(s *Service) { s.tempDir = dir }
}

// WithBundleExtras 指定除库与图之外**还要打进包**的库外文件（绝对路径）。
//
// 目前是 review.json 与 digest.json：换手机时那两样（间隔模糊、考试日期、保留率、
// 提醒钟点）重填很烦，而它们又不含凭据 —— vlm.json 刻意**不在**这里，它带着 API key，
// 而包会落到「下载」目录，那儿别的应用读得到。
//
// 条目名取文件的 base name：包是私有目录的镜像，导入那一侧就不必再做一次名字映射。
// **文件不存在就跳过**：刚装上的机器还没有 review.json，那不是错误。
func WithBundleExtras(paths ...string) Option {
	return func(s *Service) { s.extras = append(s.extras, paths...) }
}

// NewService 用一个已经开好的错题库、一个图片文件层构造导出服务。
func NewService(questions *library.Store, images ImageFiles, opts ...Option) *Service {
	s := &Service{questions: questions, images: images, tempDir: os.TempDir(), now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Result 是一次导出的清点，够界面说清「导出了什么、多大」。
type Result struct {
	Questions int   // 库里的错题数
	Images    int   // 打进包里的图片张数（按内容 hash 去重之后）
	Bytes     int64 // 包的字节数
}

// WriteBundle 把整个错题本打进 w，返回这次导出的清点。
//
// 包里有什么是**定死**的：一份自洽的库文件，加上**库里引用到的每一张图**，一张不多一张不少，
// 再加上（若配了 WithBundleExtras）那两份不含凭据的库外设置。前两句都要当真 ——
//
//   - 库里引用的每一张图都在包里：少一张，这个包就在骗人（缺图直接报错，见下）。
//   - 包里没有库不引用的图：盘上那些没人引用的残留文件（删错题时没回收掉的）不进包。
//     去重按的是**库里的 hash 集合**，不是按错题去重：同一个 hash 完全可能既是 A 的题图、
//     又是 B 的答案图（ADR-0004），按题去重会把还被引用的图当成孤儿漏掉。
//
// 出错时 w 上可能已经落了半截数据 —— 调用方应当先写到临时文件、成功了再改名到最终落点，
// 别让一个半截的包顶着备份的名字留在盘上（zip 没收尾就没有中央目录，它不是一个能打开的包）。
//
// 缺图时返回的错误带着 ErrImageMissing 与那个 hash（errors.Is 可判）。
func (s *Service) WriteBundle(w io.Writer) (Result, error) {
	hashes, questions, err := s.bundleHashes()
	if err != nil {
		return Result{}, err
	}

	// 先把图核一遍，再动手写：宁可什么都没写，也不出一个缺图的包。
	for _, h := range hashes {
		if err := s.checkImage(h); err != nil {
			return Result{}, err
		}
	}

	dbPath, cleanup, err := s.snapshotDB()
	defer cleanup() // 失败时可能已经建出目录了，也要清
	if err != nil {
		return Result{}, err
	}

	// 数一下字节数：界面要能说「导出到下载目录，一共 12 MB」。
	counted := &countingWriter{w: w}
	zw := zip.NewWriter(counted)

	if err := addFile(zw, DBName, dbPath, zip.Deflate); err != nil {
		return Result{}, err
	}
	for _, h := range hashes {
		path := s.images.PathByHash(h)
		if err := addFile(zw, imageEntry(path), path, zip.Store); err != nil {
			return Result{}, err
		}
	}
	// 库外那两份设置（见 WithBundleExtras）。放在最后：包里的条目顺序是确定的，
	// 同一份库导出两次要得到同样的字节。
	for _, path := range s.extras {
		info, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue // 还没配过，不是错误
		}
		if err != nil {
			return Result{}, fmt.Errorf("导出: 看 %s: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return Result{}, fmt.Errorf("导出: %s 不是普通文件", path)
		}
		if err := addFile(zw, filepath.Base(path), path, zip.Deflate); err != nil {
			return Result{}, err
		}
	}
	if err := zw.Close(); err != nil {
		return Result{}, fmt.Errorf("导出: 收尾 zip: %w", err)
	}

	return Result{Questions: questions, Images: len(hashes), Bytes: counted.n}, nil
}

// imageEntry 返回一张图在包里的条目名。
//
// 名字取盘上那个文件的名字，只把目录换成包里的 ImageDir —— 盘上的名字（<hash>.png）
// 是文件层定的（见 ImageFiles），导出不自己拼。
func imageEntry(path string) string {
	return ImageDir + "/" + filepath.Base(path)
}

// bundleHashes 返回库里引用的**全部图片 hash**（去重、按字典序排），以及库里的错题数。
//
// 判据是「库里的 hash 集合」：题图列与答案图列都算，因为去重发生在文件那一层而不是
// 错题那一层（ADR-0004）—— 同一个 hash 完全可能既是这道题的题图、又是那道题的答案图。
// 空串不是图片，那是「还没拍答案」的占位（library.Question.AnswerHash）。
//
// 排序不是为了好看：包里的条目顺序一旦确定，同一份库导出两次就是同样的字节，
// 两份备份能直接比对（见 addFile 那句「不设 Modified」）。
//
// 走 ListQuestions 而不是自己写一条 SELECT DISTINCT：行到 Question 的翻译只有 library
// 一处（scanQuestion），本包不该再抄一份列名与顺序。量级是「一个人的错题本」，
// 元数据整份读进来无妨 —— 图片是流着拷的，不占内存。
func (s *Service) bundleHashes() ([]string, int, error) {
	qs, err := s.questions.ListQuestions()
	if err != nil {
		return nil, 0, err
	}

	set := make(map[string]bool, len(qs))
	for _, q := range qs {
		for _, h := range [2]string{q.QuestionHash, q.AnswerHash} {
			if h != "" {
				set[h] = true
			}
		}
	}

	hashes := make([]string, 0, len(set))
	for h := range set {
		hashes = append(hashes, h)
	}
	sort.Strings(hashes)
	return hashes, len(qs), nil
}

// checkImage 确认库里引用的这张图在盘上、而且是张图（普通文件）。
//
// 只看存在与类型，不读内容：内容对不对不是这一层的事 —— 文件名就是内容的 hash，
// 谁改了盘上那个文件，谁也改不回它该有的名字。
func (s *Service) checkImage(hash string) error {
	path := s.images.PathByHash(hash)
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%w: %s（%s）: %w", ErrImageMissing, hash, path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s（%s）不是普通文件", ErrImageMissing, hash, path)
	}
	return nil
}

// snapshotDB 做一份库的**一致性快照**，返回它的路径与收尾用的清理函数。
//
// ── 为什么不能直接拷 library.db ──
//
// 库开在 WAL 模式下（library.dsn）：最近提交的东西可能只落在 library.db-wal 里，
// 主库文件上还没有。直接拷主库文件就是把最近的写入丢掉 —— 而导出正是为了「刚记的题
// 别丢」，丢的恰好是这一截。把 -wal / -shm 一起塞进 zip 也不对：那是**别的库文件的**
// 旁挂文件，解包那一侧拿到的若是过期的或对不上的，SQLite 会按它恢复出一个错误的库；
// 何况一个带着旁挂文件的包也不是「解包即用」。
//
// ── 用的办法：SQLite 自己的 VACUUM INTO ──
//
// 它按当前连接看到的**已提交内容**生成一份全新的、自洽的单文件库：含 WAL 里那些还没落进
// 主库文件的提交，不含 -wal / -shm，顺带把碎片整理掉。它读的是快照，导出期间不给写操作
// 添堵，也不需要先关库。
//
// 另一条路是 PRAGMA wal_checkpoint(TRUNCATE) 之后再拷主库文件，没用它：checkpoint 与拷贝
// 之间隔着一个窗口，那期间来的写入要么进不了包，要么让拷到一半的文件成为撕裂的镜像。
// VACUUM INTO 是一条语句，没有那个窗口。
func (s *Service) snapshotDB() (string, func(), error) {
	dir, err := os.MkdirTemp(s.tempDir, "export-*")
	if err != nil {
		return "", func() {}, fmt.Errorf("导出: 建临时目录: %w", err)
	}
	cleanup := func() { os.RemoveAll(dir) }

	// 目标文件必须**不存在**，VACUUM INTO 见到已存在的文件会直接报错 ——
	// 所以给的是临时目录里的一个名字，而不是先建出来的空文件。
	path := filepath.Join(dir, DBName)
	if _, err := s.questions.DB().Exec(`VACUUM INTO ?`, path); err != nil {
		return "", cleanup, fmt.Errorf("导出: 给库做一致性快照: %w", err)
	}
	return path, cleanup, nil
}

// addFile 把一个文件原样塞进包里。
//
// 图片走 Store（不压缩）：PNG 自己就是 deflate 压出来的，再压一遍几乎压不出东西，
// 却要在手机上白烧一遍 CPU 与电。库走 Deflate：SQLite 的页里大片是空的与重复的。
//
// 刻意不设 Modified：包的内容应当只由库决定 —— 同一份库导出两次得到同样的字节，
// 两份备份可以直接比。时间戳这种东西留给包外面的文件属性。
func addFile(zw *zip.Writer, name, path string, method uint16) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("导出: 打开 %s: %w", path, err)
	}
	defer f.Close()

	w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: method})
	if err != nil {
		return fmt.Errorf("导出: 建 %s 条目: %w", name, err)
	}
	if _, err := io.Copy(w, f); err != nil {
		return fmt.Errorf("导出: 写入 %s: %w", name, err)
	}
	return nil
}

// countingWriter 只为把包的大小带回给调用方。
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
