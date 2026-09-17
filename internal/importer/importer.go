// Package importer 是「整库导出」（票据 13）的另一半：把一个**导出包**装回本机。
//
// ── 为什么要跨一次重启 ──
//
// 正式数据被一条已经打开的 SQLite 连接与一个图片目录攥着，而 library / tags / review /
// discussion / workload / digest 全都注册在 Wails 上、各自握着那条连接的引用。当场把库换掉，
// 等于把整张服务图重建一遍 —— 那就是把「重启应用」用代码再实现一次。所以这一层只做两件事：
// 把包**解好、校验好、摆在那儿**（PickAndPrepare + Commit），然后在下次启动、库还没打开的
// 那一刻换上去（ApplyPending，见 apply.go）。
//
// ── 三条不变量 ──
//
//   - 要么完整生效、要么什么都不动：校验不过时正式数据一个字节都不动。与导出那句
//     「要么一个完整的包、要么什么都不写」是对称的。
//   - 只认库里引用的图：解压时只取 library.db、库引用到的 cards/<hash>.png，以及那两份
//     库外设置。包里别的条目一概不看 —— 选中的 zip 是**应用之外**的文件，它的条目名
//     不配决定往哪儿写（见 copyEntry 上那段 zip-slip 的说明）。
//   - 图要与它的名字相符：hash 是内容的 hash，所以解出来重算一遍，对不上就拒绝整个包。
//     导出那边不核（文件名是我们自己写的），而这里面对的是一个来路在应用之外的文件。
package importer

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"

	"questionbook/internal/capture"
	"questionbook/internal/export"
	"questionbook/internal/library"
)

const (
	// stagingDirName 是解好、等人点头的那个包的落点，在应用数据根目录下。
	// 与 library.db、cards/ 并列 —— 导入包换的就是它们俩。
	stagingDirName = "import-pending"

	// readyName 是 Commit 写下的就绪标记。**没有它就不落地** —— 它区分「用户确认过」
	// 与「解到一半被杀掉了」这两件事，而这两件事在磁盘上留下的东西长得一模一样。
	readyName = "ready"

	// manifestName 记这次导入的清点，给启动那一步与界面用。
	manifestName = "manifest.json"

	// resultName 是落地结果（成功或失败）的落点，界面在下次打开设置页时读它。
	resultName = "last-import.json"

	// backupPrefix 是旧库被挪到哪儿去的前缀。带时间戳，界面把它显示出来。
	backupPrefix = "bak-"

	// incomingDBName 是解出来的那个库在暂存目录里的临时名字。
	// 它会被 library.Open 迁到最新模式、再 VACUUM INTO 成最终的 library.db，
	// 所以这个名字不会活到落地那一刻。
	incomingDBName = "incoming.db"
)

// maxImageBytes 是单张图解压后的大小上限。
//
// 包是应用之外的文件，一个精心构造的 zip 完全可以声明「这张图解开有 4GB」—— 手机上那就是
// 把磁盘与内存一起打满。真实的题图是降采样过的 PNG，几 MB 顶天，64MB 已经宽得离谱。
const maxImageBytes = 64 << 20

// cardEntry 认出一张图的条目名：cards/<64 位小写十六进制>.png。
//
// **只取 hash 那一段**，路径拿它拼，绝不拿条目名拼 —— 见 copyEntry。
var cardEntry = regexp.MustCompile(`^cards/([0-9a-f]{64})\.png$`)

var (
	// ErrNoPending 表示当前没有等落地的导入。
	ErrNoPending = errors.New("没有待落地的导入")

	// ErrBundleIncomplete 表示这个包不能装：缺了库、缺了图、图与名字对不上、或者
	// 库的模式比本应用还新。判断用 errors.Is —— 界面要说的是「这个包不可用」，
	// 而不是把底层那句话直接抖给用户看。
	ErrBundleIncomplete = errors.New("这个导出包装不了")
)

// ZipPicker 是「让用户挑一个导出包」这件事。
//
// 单独抽成一个口子，是因为它只能由宿主实现（安卓上是系统文档选择器，见 Wails 的
// app.Dialog.OpenFile），而这一层要能在测试里用假的跑。返回空串表示用户取消了。
type ZipPicker interface {
	PickZip() (string, error)
}

// Service 是「从导出包恢复」对前端暴露的那一面。
type Service struct {
	// root 是应用数据根目录：library.db、cards/ 与那几份 json 都在它下面。
	root string
	// live 是**当前**的库，只用来数「你现在有多少道题」—— 确认框要靠它对比。
	// 这一层从不往它里面写。
	live   *library.Store
	picker ZipPicker
	now    func() time.Time
}

// Option 给服务补一样可选能力。
type Option func(*Service)

// WithNow 换掉取当前时刻的函数。给测试用：备份目录名与 manifest 里的时间要能钉死。
func WithNow(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// NewService 用应用数据根目录、当前那个库、以及一个「挑文件」的口子构造服务。
//
// live 只读，且必须是**已经打开**的那个库（main.go 里 library.Open 之后）。
func NewService(root string, live *library.Store, picker ZipPicker, opts ...Option) *Service {
	s := &Service{root: root, live: live, picker: picker, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Preview 是一个已经解好、校验过的包，等人点头。
type Preview struct {
	Questions int   // 包里有多少道错题
	Images    int   // 包里有多少张图（去重后）
	Bytes     int64 // zip 文件本身有多大

	// Settings 是包里带的那几份库外设置（目前是 review.json / digest.json）。
	// 空切片表示这个包只有库与图。
	Settings []string

	// Current 是**现在**库里的错题数。单独列出来，是因为确认框真正该给人看的是
	// 「包里 42 道 / 你现在 57 道」这个对比 —— 只说前者，用户点下去之前没法判断
	// 自己是不是在拿一份旧备份盖掉新数据。
	Current int
}

// Pending 描述一个已经就绪、等下次启动落地的导入。
type Pending struct {
	Ready      bool
	Preview    Preview
	PreparedAt time.Time
}

// Result 是落地那一步的结果：成功也好、失败了也好，都写下来给界面看。
//
// 失败也要留痕，是因为这一步在启动时跑，那时没有任何界面在场 —— 用户唯一能知道
// 「上次导入没成」的地方就是这里。
type Result struct {
	OK        bool
	Questions int
	Images    int
	At        time.Time

	// Backup 是旧库被挪到哪儿了（空串表示本来就没有旧库）。
	// 有它才说得清「你的旧数据还在」。
	Backup string

	// Problem 非空表示这次落地失败了，这里是原因。
	Problem string
}

// manifest 是暂存目录里那份清点，形状与 Preview 一样再加一个时刻。
//
// 单独一个类型而不是直接存 Preview：那个类型的字段会随界面需要变，而这份文件是
// 「上次那个进程写给这次这个进程」的，形状要稳（与 digest 的 scheduleFile 同一个道理）。
type manifest struct {
	Questions  int      `json:"questions"`
	Images     int      `json:"images"`
	Bytes      int64    `json:"bytes"`
	Settings   []string `json:"settings"`
	PreparedAt int64    `json:"prepared_at_ms"`
}

// Picked 是一次「挑一个包」的结果。
//
// 取消做成**正常结果里的一个字段**，而不是一个错误：用户点了取消什么都没发生，
// 界面不该弹一句红的。做成错误的话前端只能去认错误文案，而文案一改就坏。
type Picked struct {
	// Cancelled 为真表示用户取消了（或者什么都没选），此时 Preview 是零值。
	Cancelled bool
	Preview   Preview
}

// PickAndPrepare 让用户挑一个导出包，解出来校验好，摆进暂存目录。
//
// 它**不动正式数据**：通过的话要用户再点一次确认（Commit），然后在下次启动才落地。
// 这中间用户随时可以 Cancel 掉。
func (s *Service) PickAndPrepare() (Picked, error) {
	if s.picker == nil {
		return Picked{}, errors.New("这个平台没有文件选择器")
	}
	path, err := s.picker.PickZip()
	if err != nil {
		return Picked{}, err
	}
	if path == "" {
		return Picked{Cancelled: true}, nil
	}
	p, err := s.prepare(path)
	if err != nil {
		return Picked{}, err
	}
	return Picked{Preview: p}, nil
}

// prepare 解一个指定路径的包。
//
// **不导出**：界面要的是「让用户挑一个」（PickAndPrepare），没有任何一处需要按路径
// 解包 —— 而多一个导出方法就多一条进绑定的口子（前端能传任意路径进来）。
// 解包本身与挑文件分开写，是为了让测试能喂一个自己造的包。
func (s *Service) prepare(zipPath string) (Preview, error) {
	staging := s.stagingDir()

	// 上一次解到一半留下的先清掉：它要么是垃圾，要么是用户改主意了。
	if err := os.RemoveAll(staging); err != nil {
		return Preview{}, fmt.Errorf("清暂存目录: %w", err)
	}
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return Preview{}, fmt.Errorf("建暂存目录: %w", err)
	}
	// 从这里开始，任何一条失败路径都要把暂存目录收干净 —— 一个半截的暂存目录顶着
	// 「待落地」的位置留在盘上，比没有它更坏。
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(staging)
		}
	}()

	info, err := os.Stat(zipPath)
	if err != nil {
		return Preview{}, fmt.Errorf("打不开这个包: %w", err)
	}

	p, err := s.extract(zipPath, staging)
	if err != nil {
		return Preview{}, err
	}
	p.Bytes = info.Size()
	if s.live != nil {
		qs, err := s.live.ListQuestions()
		if err != nil {
			return Preview{}, fmt.Errorf("数当前库里的题: %w", err)
		}
		p.Current = len(qs)
	}

	// 把这次的清点写下来：Commit、Pending 与启动那一步都读它。
	//
	// **就绪标记不在这儿写**：那个只有在用户点头之后才有（Commit）。两份文件分开，
	// 是因为它们回答的是两个问题 ——「解好了什么」与「用户确认了没有」。
	if err := writeManifest(staging, p, s.now()); err != nil {
		return Preview{}, err
	}

	ok = true
	return p, nil
}

// writeManifest 把这次解包的清点落成 manifest.json。
func writeManifest(staging string, p Preview, now time.Time) error {
	raw, err := json.Marshal(manifest{
		Questions:  p.Questions,
		Images:     p.Images,
		Bytes:      p.Bytes,
		Settings:   p.Settings,
		PreparedAt: now.UnixMilli(),
	})
	if err != nil {
		return fmt.Errorf("写 manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(staging, manifestName), raw, 0o600); err != nil {
		return fmt.Errorf("写 manifest: %w", err)
	}
	return nil
}

// extract 把包解进 staging，返回它的清点（Bytes 由调用方补）。
//
// 分两趟走：先取库，因为「哪些图要取」只有打开库才知道；再按那份清单取图。
func (s *Service) extract(zipPath, staging string) (Preview, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return Preview{}, fmt.Errorf("%w: 它不是一个能打开的 zip: %v", ErrBundleIncomplete, err)
	}
	defer zr.Close()

	// ── 第一趟：库与那两份设置 ──
	incoming := filepath.Join(staging, incomingDBName)
	found, err := copyNamedEntry(&zr.Reader, export.DBName, incoming, 0)
	if err != nil {
		return Preview{}, err
	}
	if !found {
		return Preview{}, fmt.Errorf("%w: 包里没有 %s", ErrBundleIncomplete, export.DBName)
	}

	var settings []string
	for _, name := range export.SettingsNames {
		dest := filepath.Join(staging, name)
		found, err := copyNamedEntry(&zr.Reader, name, dest, 0)
		if err != nil {
			return Preview{}, err
		}
		if !found {
			continue // 这个包没带这一份，不是错误（刚装上的机器也没有）
		}
		// 是 JSON 吗？包里带一份解不开的设置，说明这个包本身就不对劲 ——
		// 与其装进去让设置页之后报错，不如现在就说这个包不可用。
		raw, err := os.ReadFile(dest)
		if err != nil {
			return Preview{}, fmt.Errorf("读包里的 %s: %w", name, err)
		}
		var probe any
		if err := json.Unmarshal(raw, &probe); err != nil {
			return Preview{}, fmt.Errorf("%w: 包里的 %s 不是合法的 JSON", ErrBundleIncomplete, name)
		}
		settings = append(settings, name)
	}

	// ── 打开解出来的库：顺带完成两件事 ──
	//
	//   - 模式比本应用新 → library.Open 直接拒绝（"库的模式版本是 N，本程序只认到 M"）。
	//     这一条不用我们另外写，它是那句话惟一的出处。
	//   - 模式比本应用旧 → 它照常往前迁，于是**老备份也能装回来**。
	db, err := library.Open(incoming)
	if err != nil {
		return Preview{}, fmt.Errorf("%w: 库打不开（%v）", ErrBundleIncomplete, err)
	}
	qs, err := db.ListQuestions()
	if err != nil {
		db.Close()
		return Preview{}, fmt.Errorf("读包里的库: %w", err)
	}
	hashes := referencedHashes(qs)

	// 库的最终形态：一份**自洽的单文件**库（VACUUM INTO）。
	//
	// 不直接用 incoming 那个文件：它刚被迁移写过，旁边带着 -wal / -shm。那两个文件
	// 跟着库一起搬进正式目录是**错的** —— 它们是这个临时库的旁挂文件，落地的位置换了、
	// 上一份库的旁挂文件也可能还在（见 apply.go 里把 -wal 一并挪走那段）。VACUUM INTO
	// 按当前连接看到的已提交内容生成一份干净的库，与导出那边取快照是同一个办法。
	final := filepath.Join(staging, export.DBName)
	if _, err := db.DB().Exec(`VACUUM INTO ?`, final); err != nil {
		db.Close()
		return Preview{}, fmt.Errorf("把包里的库整理成单文件: %w", err)
	}
	if err := db.Close(); err != nil {
		return Preview{}, fmt.Errorf("关掉临时库: %w", err)
	}
	removeSQLiteSidecars(incoming)
	if err := os.Remove(incoming); err != nil {
		return Preview{}, fmt.Errorf("清掉临时库: %w", err)
	}

	// ── 第二趟：库里引用到的每一张图，一张都不能少 ──
	store, err := capture.NewStore(filepath.Join(staging, export.ImageDir))
	if err != nil {
		return Preview{}, fmt.Errorf("建暂存图片目录: %w", err)
	}
	entries := map[string]*zip.File{}
	for _, f := range zr.File {
		if m := cardEntry.FindStringSubmatch(f.Name); m != nil {
			entries[m[1]] = f
		}
	}
	for _, h := range hashes {
		f := entries[h]
		if f == nil {
			// 缺图是硬失败：一个装进去却缺图的库，看着像恢复成功，等真要用的时候
			// 才发现少了东西 —— 那比不装更坏。
			return Preview{}, fmt.Errorf("%w: 库里引用的图 %s 不在包里", ErrBundleIncomplete, h)
		}
		if err := extractCard(store, f, h); err != nil {
			return Preview{}, err
		}
	}

	return Preview{Questions: len(qs), Images: len(hashes), Settings: settings}, nil
}

// Commit 给这次导入盖上「用户确认过了」的章，之后它会在下次启动落地。
//
// 到这一步仍然**没有动正式数据**。盖章与落地分开，是因为落地只能在启动时做
// （见包注释），而用户点头与下次启动之间可能隔着很久 —— 中间他还能反悔（Cancel）。
func (s *Service) Commit() error {
	staging := s.stagingDir()
	if _, err := os.Stat(filepath.Join(staging, manifestName)); err != nil {
		// 没有解好的包可确认（清点文件是解包那一刻写下的）。
		return fmt.Errorf("%w: %v", ErrNoPending, err)
	}
	if err := os.WriteFile(filepath.Join(staging, readyName), []byte(s.now().Format(time.RFC3339)), 0o600); err != nil {
		return fmt.Errorf("写就绪标记: %w", err)
	}
	return nil
}

// Cancel 丢掉暂存目录里那个还没落地的导入。没有待落地的也算成功 ——
// 调用方要的是「它不在了」，而它确实不在。
func (s *Service) Cancel() error {
	if err := os.RemoveAll(s.stagingDir()); err != nil {
		return fmt.Errorf("丢掉待落地的导入: %w", err)
	}
	return nil
}

// Pending 报告当前有没有等落地的导入。没有的话 Ready 为 false。
func (s *Service) Pending() (Pending, error) {
	staging := s.stagingDir()
	if _, err := os.Stat(filepath.Join(staging, readyName)); err != nil {
		return Pending{}, nil
	}
	p, err := readManifest(staging)
	if err != nil {
		return Pending{}, err
	}
	return Pending{Ready: true, Preview: p.Preview, PreparedAt: time.UnixMilli(p.preparedAt)}, nil
}

// Last 读回上一次落地的结果。从没落地过就返回零值（OK 为 false）。
func (s *Service) Last() Result {
	return readResult(filepath.Join(s.root, resultName))
}

// stagingDir 是暂存目录的落点。
func (s *Service) stagingDir() string { return filepath.Join(s.root, stagingDirName) }

// manifestPreview 是 manifest 读回来之后的内部形状（多带一个时刻）。
type manifestPreview struct {
	Preview
	preparedAt int64
}

// readManifest 读暂存目录里那份清点。
func readManifest(staging string) (manifestPreview, error) {
	raw, err := os.ReadFile(filepath.Join(staging, manifestName))
	if err != nil {
		return manifestPreview{}, fmt.Errorf("%w: 读不到包的信息（%v）", ErrNoPending, err)
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return manifestPreview{}, fmt.Errorf("%w: 包的信息读不出来（%v）", ErrNoPending, err)
	}
	return manifestPreview{
		Preview: Preview{
			Questions: m.Questions,
			Images:    m.Images,
			Bytes:     m.Bytes,
			Settings:  m.Settings,
		},
		preparedAt: m.PreparedAt,
	}, nil
}

// referencedHashes 返回这批错题引用到的**全部图片 hash**（去重、按字典序排）。
//
// 与导出的 bundleHashes 是同一个判据（题图列与答案图列都算、空串不算），只是这里的
// 库是刚解出来的那一份。排序是为了让行为确定 —— 出错时报的是第一个缺的 hash。
func referencedHashes(qs []library.Question) []string {
	set := make(map[string]bool, len(qs))
	for _, q := range qs {
		for _, h := range [2]string{q.QuestionHash, q.AnswerHash} {
			if h != "" {
				set[h] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for h := range set {
		out = append(out, h)
	}
	slices.Sort(out)
	return out
}

// copyNamedEntry 把包里一个**名字完全确定**的条目拷到 dest，并报告它到底在不在。
//
// 只认整名相等，所以条目名永远不参与拼路径 —— 这是 zip-slip 那条防线的一半
// （另一半在 extractCard：那张图的路径是用**正则捕获到的 hash** 拼的，不是条目名）。
//
// 「在不在」要报出来而不是自己吞掉：库缺了是硬错，而那两份设置缺了不是 —— 同一句
// 「找不到就返回 nil」用在这两处会把前者也放过（这一版最初就是这么错的）。
func copyNamedEntry(zr *zip.Reader, name, dest string, limit int64) (bool, error) {
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		if f.FileInfo().IsDir() {
			return false, nil
		}
		return true, writeEntry(f, dest, limit)
	}
	return false, nil
}

// extractCard 把一张图从包里解到 store 里，并**核对它确实是那张图**。
//
// 图在包里是按内容 hash 命名的，而这句话在搬运过一道之后要能被核：解出来重算一遍
// 像素 hash，对不上就说明这个文件不是它该是的那张图（被换过、或者截断了之后又凑巧
// 能被解码）。核不过就把刚落下的那个文件删掉，免得暂存目录里留一张名字骗人的图。
func extractCard(store *capture.Store, f *zip.File, want string) error {
	h := capture.Hash(want)
	dest := store.Path(h)
	if err := writeEntry(f, dest, maxImageBytes); err != nil {
		return err
	}

	img, err := store.Load(h)
	if err != nil {
		os.Remove(dest)
		return fmt.Errorf("%w: 包里的图 %s 解不开（%v）", ErrBundleIncomplete, want, err)
	}
	if got := capture.HashOf(img); got != h {
		os.Remove(dest)
		return fmt.Errorf("%w: 包里的图 %s 的内容是 %s", ErrBundleIncomplete, want, got)
	}
	return nil
}

// writeEntry 把一个 zip 条目原样写到 dest。
//
// limit 大于 0 时限制解压后的字节数：条目自己声明的 UncompressedSize64 是**不可信**的
// （它就是那个 zip bomb 的尺寸），所以这里按实际写出来的字节数封顶，超了就报错。
func writeEntry(f *zip.File, dest string, limit int64) error {
	if limit > 0 && int64(f.UncompressedSize64) > limit {
		return fmt.Errorf("%w: 包里的 %s 声明自己有 %d 字节，太大了",
			ErrBundleIncomplete, f.Name, f.UncompressedSize64)
	}

	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("打开包里的 %s: %w", f.Name, err)
	}
	defer rc.Close()

	out, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("建 %s: %w", dest, err)
	}
	defer out.Close()

	var src io.Reader = rc
	if limit > 0 {
		// 多读一个字节：恰好读到 limit 时才算没超。
		src = io.LimitReader(rc, limit+1)
	}
	n, err := io.Copy(out, src)
	if err != nil {
		os.Remove(dest)
		return fmt.Errorf("摊开包里的 %s: %w", f.Name, err)
	}
	if limit > 0 && n > limit {
		os.Remove(dest)
		return fmt.Errorf("%w: 包里的 %s 解开之后比 %d 字节还大", ErrBundleIncomplete, f.Name, limit)
	}
	return nil
}

// removeSQLiteSidecars 删掉某个库文件旁边的 -wal / -shm。
//
// 只在「这个库刚被我们独占打开又关掉」之后调用（那种时候它们已经是空的或者不存在）。
func removeSQLiteSidecars(dbPath string) {
	os.Remove(dbPath + "-wal")
	os.Remove(dbPath + "-shm")
}
