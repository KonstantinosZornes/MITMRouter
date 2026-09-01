//go:build !windows

package store

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// P0-2: router.db 保存 CA 私钥、上游凭据、管理员口令哈希、会话签名密钥。
// 即便数据目录本身是 0700，db 文件及其 WAL 伴随文件也必须收紧到 0600，
// 防止同组/其他用户读取。
func TestOpenTightensDBFilePerms(t *testing.T) {
	prev := syscall.Umask(0o022) // 宽松 umask 下验证 Open 主动收紧
	t.Cleanup(func() { syscall.Umask(prev) })

	// 预先以宽松权限创建数据目录，模拟"曾以宽松权限存在"的场景
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod dir loose: %v", err)
	}
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	// 数据目录必须被收紧到 0700
	if info, err := os.Stat(dir); err != nil {
		t.Fatalf("stat dir: %v", err)
	} else if got := info.Mode() & 0o777; got != 0o700 {
		t.Errorf("data dir mode=%o, want 0700", got)
	}
	// db 及 WAL 伴随文件必须 0600
	for _, name := range []string{"router.db", "router.db-wal", "router.db-shm"} {
		p := filepath.Join(dir, name)
		info, err := os.Stat(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue // 伴随文件可能尚未生成，跳过
			}
			t.Fatalf("stat %s: %v", name, err)
		}
		if got := info.Mode() & 0o777; got != 0o600 {
			t.Errorf("%s mode=%o, want 0600", name, got)
		}
	}
}
