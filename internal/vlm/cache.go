package vlm

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// fileCacheName 是上传缓存的落点，与配置文件同目录（见 Service 的注释）。
const fileCacheName = "vlm-files.json"

// fileIDCache 记住「本地这张图已经传上去过了」，键是图片的内容 hash。
//
// 为什么要落盘：同一张题图会在「打标签」与「就题讨论」两条路径上反复用到，
// 而 Files API 的整个卖点就是传一次、反复引用（票据：避免同一张图反复上传）。
// 只在内存里记的话，重启一次就白传了。
//
// 为什么不去 SQLite：它与配置是同一类东西 —— 不属于错题的数据模型，也不该跟着导出包走。
// 服务方的 file_id 是绑在账号上的一次性引用，导出到别的手机上毫无意义。
//
// 文件里**没有凭据**，只有一份指纹：file_id 与上传它的那把 key 绑定，换了 key 之后
// 旧 id 一律作废，指纹就是用来在换 key 时让整份缓存自然失效的。
type fileIDCache struct {
	path string

	mu      sync.Mutex
	entries map[string]fileIDEntry
}

type fileIDEntry struct {
	FileID string `json:"file_id"`
	// KeyPrint 是上传时那把 key 的指纹（**不是** key 本身）。
	KeyPrint string `json:"key_print"`
}

// openFileIDCache 打开（或新建）一份缓存文件。
//
// 文件不在、读坏了、甚至目录建不出来，都返回一份**空的内存缓存**而不是错：
// 缓存的全部作用是省一次上传，它不工作只是退化成每次重传，
// 不值得为此让整个打标签功能起不来。
func openFileIDCache(path string) *fileIDCache {
	c := &fileIDCache{path: path, entries: map[string]fileIDEntry{}}

	raw, err := os.ReadFile(path)
	if err != nil {
		return c // 第一次跑就是没有，正常
	}
	var entries map[string]fileIDEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return c
	}
	if entries != nil {
		c.entries = entries
	}
	return c
}

// get 问「这张图用这把 key 传上去过没有」。指纹对不上就当没有 ——
// 换了 key 之后，旧 key 传上去的 id 引用不了。
func (c *fileIDCache) get(hash, keyPrint string) (string, bool) {
	if c == nil || hash == "" {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.entries[hash]
	if !ok || e.KeyPrint != keyPrint || e.FileID == "" {
		return "", false
	}
	return e.FileID, true
}

// put 记下这张图的引用，并立刻写回盘上。
//
// 每次都落盘是刻意的：打标签是个低频动作（用户按一下才跑一次），
// 为它省一次文件写不值得引入「什么时候该 flush」这个问题。
func (c *fileIDCache) put(hash, fileID, keyPrint string) {
	if c == nil || hash == "" || fileID == "" {
		return
	}
	c.mu.Lock()
	c.entries[hash] = fileIDEntry{FileID: fileID, KeyPrint: keyPrint}
	raw, err := json.MarshalIndent(c.entries, "", "  ")
	c.mu.Unlock()
	if err != nil {
		return
	}
	_ = writeFileAtomic(c.path, raw)
}

// writeFileAtomic 先写同目录的临时文件再改名。
//
// 缓存写坏了大不了下次重传，但**不能**留下半份 JSON：下一次启动会把它当成
// 「文件在但解析不了」而整个丢掉，那之前的记录就白攒了。
func writeFileAtomic(path string, raw []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("VLM: 建缓存目录: %w", err)
	}
	f, err := os.CreateTemp(dir, "vlm-files-*.json")
	if err != nil {
		return fmt.Errorf("VLM: 建临时文件: %w", err)
	}
	tmp := f.Name()
	renamed := false
	defer func() {
		f.Close()
		if !renamed {
			os.Remove(tmp)
		}
	}()

	if _, err := f.Write(raw); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	renamed = true
	return nil
}

// keyPrint 是凭据的指纹：进缓存文件、进不了别处。
//
// 取 SHA-256 的前 16 位十六进制就够用了 —— 它的用途只是「还是不是同一把 key」，
// 不是防谁反推。凭据本身在任何情况下都不落进缓存文件、日志或报错里。
func keyPrint(apiKey string) string {
	if apiKey == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(apiKey))
	return hex.EncodeToString(sum[:8])
}
