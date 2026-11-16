package vault

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/richaction/onebox-collector/internal/model"
)

func TestConcurrentAppendAndInterruptedReplace(t *testing.T) {
	writer := New(t.TempDir(), 1024)
	sent := time.Date(2026, 11, 16, 9, 0, 0, 0, model.Shanghai)
	var group sync.WaitGroup
	for i := 49; i >= 0; i-- {
		group.Go(func() {
			msg := model.Message{Channel: "kf", ID: fmt.Sprint(i), Type: model.Text, Sent: sent.Add(time.Duration(i) * time.Minute), Text: fmt.Sprintf("第 %02d 条", i)}
			if err := writer.Append(msg, model.Result{}); err != nil {
				t.Error(err)
			}
			if err := writer.Append(msg, model.Result{}); err != nil {
				t.Error(err)
			}
		})
	}
	group.Wait()
	date := sent.Format(time.DateOnly)
	note, _, err := writer.ReadDay(date)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(note), "<!-- onebox:") != 50 {
		t.Fatalf("entry count: %s", note)
	}
	previous := -1
	for i := 0; i < 50; i++ {
		offset := strings.Index(string(note), fmt.Sprintf("第 %02d 条", i))
		if offset <= previous {
			t.Fatalf("entry %d is missing or out of order", i)
		}
		previous = offset
	}
	dir := filepath.Dir(writer.NotePath(date))
	stale := filepath.Join(dir, "."+date+".md.tmp-1234")
	if err := os.WriteFile(stale, []byte("interrupted write"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writer.Init(func(name string) { t.Errorf("unexpected entry: %s", name) }); err != nil {
		t.Fatal(err)
	}
	after, _, err := writer.ReadDay(date)
	if err != nil || !bytes.Equal(note, after) {
		t.Fatalf("restart damaged note: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale temp still present: %v", err)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 2 {
		t.Fatalf("extra files: %v %v", files, err)
	}
}

func TestAssetCollisionAndSize(t *testing.T) {
	const date = "2026-11-16"
	writer := New(t.TempDir(), 25)
	png := []byte("\x89PNG\r\n\x1a\nabc")
	first, _, err := writer.StoreAsset(date, "image", bytes.NewReader(png))
	if err != nil {
		t.Fatal(err)
	}
	filename := filepath.Base(first)
	if err := os.WriteFile(writer.AssetPath(date, filename), []byte("different"), 0600); err != nil {
		t.Fatal(err)
	}
	second, n, err := writer.StoreAsset(date, "image", bytes.NewReader(png))
	if err != nil || n != int64(len(png)) || len(second) != len(first)+4 {
		t.Fatalf("collision: %q %d %v", second, n, err)
	}
	reuse, n, err := writer.StoreAsset(date, "image", bytes.NewReader(png))
	if err != nil || n != 0 || reuse != second {
		t.Fatalf("reuse: %q %d %v", reuse, n, err)
	}
	data, err := os.ReadFile(writer.AssetPath(date, filename))
	if err != nil || string(data) != "different" {
		t.Fatalf("collision overwrote existing asset: %s %v", data, err)
	}
	if _, _, err := writer.StoreAsset(date, "image", bytes.NewReader(append(png, make([]byte, 30)...))); err != ErrAssetSize {
		t.Fatalf("size: %v", err)
	}
	if _, _, err := writer.StoreAsset(date, "image", strings.NewReader("not media")); err != ErrAssetFormat {
		t.Fatalf("format: %v", err)
	}
}

func TestMergeConflictCopies(t *testing.T) {
	sent := time.Date(2026, 11, 16, 9, 0, 0, 0, model.Shanghai)
	first := model.Message{Channel: "kf", ID: "first", Type: model.Text, Sent: sent, Text: "第一条"}
	other := model.Message{Channel: "kf", ID: "second", Type: model.Text, Sent: sent.Add(20 * time.Minute), Text: "第二条"}
	header := "# 2026-11-16 收藏记录\n\n"
	main := header + render(first, model.Result{}) + "\n" + render(other, model.Result{})
	copy := header + render(first, model.Result{}) + "\n## 09:10\n本地新增\n"
	merged, err := Merge([]byte(main), []byte(copy))
	if err != nil || strings.Count(string(merged), "第一条") != 1 || strings.Count(string(merged), "本地新增") != 1 ||
		strings.Index(string(merged), "本地新增") > strings.Index(string(merged), "第二条") {
		t.Fatalf("merge: %s %v", merged, err)
	}
	if _, err := Merge([]byte(main), []byte(strings.Replace(copy, "第一条", "修改同一条", 1))); err != ErrConflict {
		t.Fatalf("unsafe edit: %v", err)
	}
	if _, err := Merge([]byte(main), []byte("无法归属的前言\n"+copy)); err != ErrConflict {
		t.Fatalf("unsafe preface: %v", err)
	}
}
