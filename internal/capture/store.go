package capture

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
)

// Hash 是题图内容的 SHA-256，小写十六进制。
//
// 它既是文件名也是去重键：库里存的就是它（ADR-0004）。
type Hash string

// String 返回十六进制文本，方便直接塞进 SQLite 或者发给前端。
func (h Hash) String() string { return string(h) }

// ext 是题图的扩展名。存 PNG：无损、标准库自带、没有专利尾巴。
// 「不做不可逆增强、存原始像素」这条契约靠它落实 —— 别换 JPEG。
const ext = ".png"

// Store 把题图按内容 hash 落在应用私有目录里。
//
// 目录由外部注入：安卓上沙箱路径只有宿主知道，测试里注入 t.TempDir()。
// 同一个 Store 可以并发用 —— 落盘走「同目录临时文件 + 原子改名」，
// 撞名时后到的那次覆盖成同样的内容，不会留下半个文件，也不会多出第二个。
type Store struct {
	dir string
}

// NewStore 以 dir 为图片目录开一个 Store，目录不存在就建。
func NewStore(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("capture: 图片目录不能为空")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("capture: 建图片目录: %w", err)
	}
	return &Store{dir: dir}, nil
}

// Dir 返回图片目录。
func (s *Store) Dir() string { return s.dir }

// Path 返回某个 hash 对应文件的完整路径。
//
// 文件名怎么拼只在这里定义一次，调用方别自己去拼。
func (s *Store) Path(h Hash) string { return filepath.Join(s.dir, string(h)+ext) }

// Save 把 img 编码成 PNG 落盘，返回它的内容 hash。
//
// 内容 hash 天然去重：同一张图落两次只会有一个文件，第二次直接返回已有的 hash，
// 连编码都省了。所谓「同一张图」是逐像素相同 —— 落盘前先归一到 RGBA，
// 所以背不同 stride、或者从不同色彩模型转换过来的同一画面会得到同一个 hash。
func (s *Store) Save(img image.Image) (Hash, error) {
	if img == nil || img.Bounds().Empty() {
		return "", ErrEmptyImage
	}
	src, _ := canonicalRGBA(img)
	h := hashOf(src)
	if _, err := os.Stat(s.Path(h)); err == nil {
		return h, nil // 库里已经有了
	}
	if err := s.writePNG(h, src); err != nil {
		return "", err
	}
	return h, nil
}

// LoadByHash 按十六进制 hash 文本读回题图。
//
// 题库那边的错题记的是 string 型 hash（library.Question.QuestionHash），这一层薄壳
// 让 *Store 直接满足题库要的读接口 —— 依赖只能有一个方向：采集用题库建错题。
func (s *Store) LoadByHash(hash string) (image.Image, error) { return s.Load(Hash(hash)) }

// Load 读回某个 hash 对应的题图。
func (s *Store) Load(h Hash) (image.Image, error) {
	f, err := os.Open(s.Path(h))
	if err != nil {
		return nil, fmt.Errorf("capture: 打开题图 %s: %w", h, err)
	}
	defer f.Close()

	img, err := png.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("capture: 解码题图 %s: %w", h, err)
	}
	return img, nil
}

// writePNG 先写同目录的临时文件、收尾后再改名到目标路径。
// 中途崩了只会留下一个临时文件，不会留下一个顶着正确 hash 的半个文件。
func (s *Store) writePNG(h Hash, img image.Image) error {
	f, err := os.CreateTemp(s.dir, "tmp-*")
	if err != nil {
		return fmt.Errorf("capture: 建临时文件: %w", err)
	}
	tmp := f.Name()
	renamed := false
	defer func() {
		f.Close() // 正常路径上已经关过了，这里只会拿到 ErrClosed，忽略
		if !renamed {
			os.Remove(tmp)
		}
	}()

	if err := png.Encode(f, img); err != nil {
		return fmt.Errorf("capture: 编码 PNG: %w", err)
	}
	// Windows 上改名之前必须先放手，否则句柄还开着。
	if err := f.Close(); err != nil {
		return fmt.Errorf("capture: 收尾临时文件: %w", err)
	}
	if err := os.Rename(tmp, s.Path(h)); err != nil {
		return fmt.Errorf("capture: 落盘 %s: %w", h, err)
	}
	renamed = true
	return nil
}

// hashOf 对图像的像素内容取 SHA-256。
//
// 刻意不 hash 编码之后的字节：Go 的 PNG 编码器输出不保证跨版本一致，
// 那样一来升一次 Go 就会让库里存着的 hash 全部对不上文件。像素是稳定的。
// 尺寸也进 hash —— 否则「宽度不同、像素串恰好首尾相同」的两张图会撞成一个文件。
func hashOf(img *image.RGBA) Hash {
	b := img.Bounds()
	var header [8]byte
	binary.BigEndian.PutUint32(header[0:4], uint32(b.Dx()))
	binary.BigEndian.PutUint32(header[4:8], uint32(b.Dy()))

	d := sha256.New()
	d.Write(header[:])
	// 逐行喂，绕开 stride 上的填充字节：同一画面哪怕背着不同的 stride 也得 hash 一致。
	for y := b.Min.Y; y < b.Max.Y; y++ {
		start := img.PixOffset(b.Min.X, y)
		d.Write(img.Pix[start : img.PixOffset(b.Max.X-1, y)+4])
	}
	return Hash(hex.EncodeToString(d.Sum(nil)))
}
