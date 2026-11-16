package vault

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/richaction/onebox-collector/internal/model"
	"golang.org/x/sys/unix"
)

var (
	ErrAssetSize   = errors.New("媒体超过大小上限")
	ErrLowSpace    = errors.New("磁盘余量不足，已暂停媒体写入")
	ErrAssetFormat = errors.New("媒体格式不受支持")
	ErrConflict    = errors.New("存在无法自动合并的笔记内容")
	assetName      = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}-(?:[0-9a-f]{8}|[0-9a-f]{12}|[0-9a-f]{64})\.(?:jpg|png|gif|webp|bmp|mp4)$`)
	entryPattern   = regexp.MustCompile(`(?ms)^<!-- onebox:([0-9a-f]{64}) (\d+) -->\n.*?^<!-- /onebox -->\n`)
)

type Writer struct {
	root   string
	limit  int64
	locks  sync.Map
	stored atomic.Uint64
}

func New(root string, limit int64) *Writer { return &Writer{root: root, limit: limit} }

func ValidDate(date string) bool {
	_, err := time.Parse(time.DateOnly, date)
	return err == nil
}

func (w *Writer) lock(date string) func() {
	item, _ := w.locks.LoadOrStore(date, new(sync.Mutex))
	mu := item.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (w *Writer) Init(warn func(string)) error {
	if err := os.MkdirAll(w.root, 0700); err != nil {
		return err
	}
	days, err := os.ReadDir(w.root)
	if err != nil {
		return err
	}
	for _, day := range days {
		date := day.Name()
		if strings.HasPrefix(date, ".") {
			continue
		}
		if !day.IsDir() || !ValidDate(date) {
			warn(date)
			continue
		}
		for _, folder := range []string{filepath.Join(w.root, date), filepath.Join(w.root, date, "assets")} {
			files, err := os.ReadDir(folder)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			for _, file := range files {
				name := file.Name()
				if !file.IsDir() && (strings.HasPrefix(name, "."+date+".md.tmp-") || strings.HasPrefix(name, ".asset.tmp-")) {
					if err := os.Remove(filepath.Join(folder, name)); err != nil {
						return err
					}
					continue
				}
				if !strings.HasPrefix(name, ".") && name != "assets" && name != date+".md" && !validAsset(date, name) {
					warn(filepath.Join(date, name))
				}
			}
		}
	}
	return nil
}

func (w *Writer) BytesStored() uint64 { return w.stored.Load() }

func (w *Writer) Check() error {
	f, err := os.CreateTemp(w.root, ".health-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	return f.Close()
}

func (w *Writer) Space() (uint64, float64, error) {
	var disk unix.Statfs_t
	if err := unix.Statfs(w.root, &disk); err != nil {
		return 0, 0, err
	}
	if disk.Blocks == 0 {
		return 0, 0, errors.New("filesystem has no blocks")
	}
	return uint64(disk.Bavail) * uint64(disk.Bsize), float64(disk.Bavail) / float64(disk.Blocks), nil
}

func (w *Writer) StoreAsset(date, kind string, source io.Reader) (string, int64, error) {
	if !ValidDate(date) {
		return "", 0, errors.New("invalid asset date")
	}
	dir := filepath.Join(w.root, date, "assets")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", 0, err
	}
	if free, _, err := w.Space(); err == nil && free < uint64(w.limit)+(32<<20) {
		return "", 0, ErrLowSpace
	}
	file, err := os.CreateTemp(dir, ".asset.tmp-")
	if err != nil {
		return "", 0, err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(source, w.limit+1))
	if err != nil {
		return "", 0, err
	}
	if n > w.limit {
		return "", 0, ErrAssetSize
	}
	var head [512]byte
	length, err := file.ReadAt(head[:], 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", 0, err
	}
	ext := mediaExtension(head[:length], kind)
	if ext == "" {
		return "", 0, ErrAssetFormat
	}
	digest := fmt.Sprintf("%x", hash.Sum(nil))
	if err := file.Sync(); err != nil {
		return "", 0, err
	}
	if err := file.Close(); err != nil {
		return "", 0, err
	}
	unlock := w.lock(date)
	defer unlock()
	for _, size := range []int{8, 12, 64} {
		name := date + "-" + digest[:size] + "." + ext
		dst := filepath.Join(dir, name)
		prior, err := fileDigest(dst)
		if errors.Is(err, fs.ErrNotExist) {
			if err := os.Rename(file.Name(), dst); err != nil {
				return "", 0, err
			}
			w.stored.Add(uint64(n))
			return "assets/" + name, n, syncDir(dir)
		}
		if err != nil {
			return "", 0, err
		}
		if prior == digest {
			return "assets/" + name, 0, nil
		}
	}
	return "", 0, errors.New("asset hash collision")
}

func mediaExtension(head []byte, kind string) string {
	mime := http.DetectContentType(head)
	if kind == "video" {
		if mime == "video/mp4" {
			return "mp4"
		}
		return ""
	}
	switch mime {
	case "image/jpeg":
		return "jpg"
	case "image/png":
		return "png"
	case "image/gif":
		return "gif"
	case "image/webp":
		return "webp"
	case "image/bmp":
		return "bmp"
	default:
		return ""
	}
}

func fileDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	_, err = io.Copy(hash, file)
	return fmt.Sprintf("%x", hash.Sum(nil)), err
}

func validAsset(date, name string) bool {
	return strings.HasPrefix(name, date+"-") && assetName.MatchString(name)
}

func (w *Writer) NotePath(date string) string { return filepath.Join(w.root, date, date+".md") }

func (w *Writer) ReadDay(date string) ([]byte, []string, error) {
	unlock := w.lock(date)
	defer unlock()
	note, err := os.ReadFile(w.NotePath(date))
	if err != nil {
		return nil, nil, err
	}
	assets, err := os.ReadDir(filepath.Join(w.root, date, "assets"))
	if err != nil {
		return nil, nil, err
	}
	names := make([]string, 0, len(assets))
	for _, f := range assets {
		if f.Type().IsRegular() && validAsset(date, f.Name()) {
			names = append(names, f.Name())
		}
	}
	return note, names, nil
}

func (w *Writer) AssetPath(date, name string) string {
	return filepath.Join(w.root, date, "assets", name)
}

func (w *Writer) Append(m model.Message, result model.Result) error {
	date := m.Date()
	unlock := w.lock(date)
	defer unlock()
	path := w.NotePath(date)
	if err := os.MkdirAll(filepath.Join(filepath.Dir(path), "assets"), 0700); err != nil {
		return err
	}
	body, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		body = []byte("# " + date + " 收藏记录\n\n")
	} else if err != nil {
		return err
	}
	entry := render(m, result)
	block := entryPattern.FindStringSubmatch(entry)
	updated, added := insertEntry(string(body), entry, block[1], block[2])
	if !added {
		return nil
	}
	return atomicWrite(path, []byte(updated))
}

func insertEntry(note, entry, id, stamp string) (string, bool) {
	index := len(note)
	for _, match := range entryPattern.FindAllStringSubmatchIndex(note, -1) {
		if note[match[2]:match[3]] == id {
			return note, false
		}
		existing := note[match[4]:match[5]]
		if index == len(note) && (len(existing) > len(stamp) || (len(existing) == len(stamp) && existing > stamp)) {
			index = match[0]
		}
	}
	left := note[:index]
	if !strings.HasSuffix(left, "\n\n") && left != "" {
		left += "\n"
		if !strings.HasSuffix(left, "\n\n") {
			left += "\n"
		}
	}
	return left + entry + "\n" + note[index:], true
}

func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, fmt.Sprintf(".%s.tmp-%d-", filepath.Base(path), os.Getpid()))
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
