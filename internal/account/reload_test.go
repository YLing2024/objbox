package account

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// writeAccountsErr 直接改写 accounts.json，模拟另一个进程（CLI）的原子写入。
func writeAccountsErr(dir string, accounts ...*Account) error {
	f := fileFormat{Version: Version, Accounts: accounts}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')

	path := filepath.Join(dir, FileName)
	tmp, err := os.CreateTemp(dir, ".accounts-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(FileMode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func writeAccounts(t *testing.T, dir string, accounts ...*Account) {
	t.Helper()
	if err := writeAccountsErr(dir, accounts...); err != nil {
		t.Fatalf("写 accounts.json 失败: %v", err)
	}
}

// TestReloadPicksUpChanges：外部改表后，新账号与轮换后的 SK 立即生效。
func TestReloadPicksUpChanges(t *testing.T) {
	s, dir := newStore(t)
	alice, err := s.Add("alice", "", false)
	if err != nil {
		t.Fatal(err)
	}
	s.SetReloadInterval(0)

	const carolAK = "AKCAROL000000000000000000000000000000"
	writeAccounts(t, dir,
		&Account{Name: "alice", AK: alice.AK, SK: "NEW-ALICE-SECRET"},
		&Account{Name: "carol", AK: carolAK, SK: "CAROL-SECRET"},
	)

	s.MaybeReload()

	got, ok := s.GetByAK(alice.AK)
	if !ok || got.SK != "NEW-ALICE-SECRET" {
		t.Fatalf("alice 应重载为新 SK，得到 %+v ok=%v", got, ok)
	}
	if carol, ok := s.GetByAK(carolAK); !ok || carol.Name != "carol" {
		t.Fatalf("新增账号 carol 未生效: %+v ok=%v", carol, ok)
	}
	if _, ok := s.Find("carol"); !ok {
		t.Fatal("carol 未出现在账号表中")
	}
	if n := s.Count(); n != 2 {
		t.Fatalf("账号数 = %d，期望 2", n)
	}
}

// TestReloadParseErrorKeepsOldTable：文件半写/非法 JSON 时保留旧表。
func TestReloadParseErrorKeepsOldTable(t *testing.T) {
	s, dir := newStore(t)
	alice, err := s.Add("alice", "", false)
	if err != nil {
		t.Fatal(err)
	}
	s.SetReloadInterval(0)

	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("{ 这不是合法 JSON"), FileMode); err != nil {
		t.Fatal(err)
	}

	s.MaybeReload()
	if n := s.Count(); n != 1 {
		t.Fatalf("解析失败后账号数 = %d，期望保留 1", n)
	}
	if got, ok := s.GetByAK(alice.AK); !ok || got.SK != alice.SK {
		t.Fatalf("解析失败后旧账号应仍可用: %+v ok=%v", got, ok)
	}

	if err := s.Reload(); err == nil {
		t.Fatal("Reload 对非法 JSON 应返回错误")
	}
	if n := s.Count(); n != 1 {
		t.Fatalf("Reload 失败后账号数 = %d，期望保留 1", n)
	}
}

// TestReloadMissingFileKeepsOldTable：文件被删除时保留旧表，避免在线账号被清空。
func TestReloadMissingFileKeepsOldTable(t *testing.T) {
	s, dir := newStore(t)
	alice, err := s.Add("alice", "", false)
	if err != nil {
		t.Fatal(err)
	}
	s.SetReloadInterval(0)

	if err := os.Remove(filepath.Join(dir, FileName)); err != nil {
		t.Fatal(err)
	}
	s.MaybeReload()
	if _, ok := s.GetByAK(alice.AK); !ok {
		t.Fatal("账号表被删除后应保留旧表")
	}
	if err := s.Reload(); err != nil {
		t.Fatalf("文件缺失时 Reload 不应报错（保留旧表）: %v", err)
	}
}

// TestReloadRace：并发读账号表 + 反复外部改表，验证 -race 下无数据竞争。
func TestReloadRace(t *testing.T) {
	s, dir := newStore(t)
	alice, err := s.Add("alice", "", false)
	if err != nil {
		t.Fatal(err)
	}
	s.SetReloadInterval(0)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				s.GetByAK(alice.AK)
				s.Find("alice")
				s.List()
				s.MaybeReload()
				_ = s.Reload()
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(stop)
		for i := 0; i < 30; i++ {
			if err := writeAccountsErr(dir, &Account{
				Name: "alice", AK: alice.AK, SK: fmt.Sprintf("sk-%d", i),
			}); err != nil {
				t.Errorf("写账号表失败: %v", err)
				return
			}
		}
	}()

	wg.Wait()
}
