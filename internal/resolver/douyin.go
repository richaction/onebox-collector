package resolver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/richaction/onebox-collector/internal/model"
	"github.com/richaction/onebox-collector/internal/vault"
	"golang.org/x/net/html"
)

type Douyin struct {
	HTTP        *http.Client
	Assets      Assets
	Cache       Cache
	MaxDuration time.Duration
}

type video struct {
	Title, Author, Cover, Play string
	Duration                   int64
}

var videoPath = regexp.MustCompile(`/(?:share/)?video/([0-9]+)(?:/|$)`)
var workID = regexp.MustCompile(`^[0-9]{8,24}$`)

func videoID(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || model.Platform(raw) != model.Douyin || !validHTTP(u) {
		return ""
	}
	if strings.Contains(u.Path, "/note/") || strings.Contains(u.Path, "/live/") || strings.Contains(u.Path, "/collection/") {
		return ""
	}
	if match := videoPath.FindStringSubmatch(u.Path); len(match) == 2 && workID.MatchString(match[1]) {
		return match[1]
	}
	if id := u.Query().Get("aweme_id"); workID.MatchString(id) {
		return id
	}
	return ""
}

func (d *Douyin) Resolve(ctx context.Context, msg model.Message, refresh bool) (model.Result, bool) {
	id, reason := d.id(ctx, msg.URL)
	if id == "" {
		return model.Result{Level: 3, Reason: reason, Note: "抖音解析失败：" + reason}, false
	}
	key := "aweme:" + id
	var meta video
	hit, _ := d.Cache.CacheGet(ctx, key, &meta)
	if !hit || refresh {
		if fresh, ok := d.details(ctx, id); ok {
			meta, hit = fresh, true
			_ = d.Cache.CachePut(ctx, key, meta, 24*time.Hour)
		}
	}
	if !hit {
		const reason = "详情不可用或触发风控"
		return model.Result{Level: 3, Reason: reason, Note: "抖音解析失败：" + reason}, false
	}
	result := model.Result{Level: 2, Title: meta.Title, Author: meta.Author}
	referer := "https://www.douyin.com/video/" + id
	if meta.Duration > d.MaxDuration.Milliseconds() {
		result.Reason, result.Note = "时长超限", "视频超过时长上限"
	} else if meta.Play == "" {
		result.Reason, result.Note = "播放地址缺失", "未获得视频下载地址"
	} else {
		media, err := saveRemote(ctx, d.HTTP, d.Assets, msg.Date(), "video", meta.Play, referer)
		if err == nil {
			result.Level, result.Media = 1, media
			return result, false
		}
		result.Reason, result.Note = "下载失败", "视频下载失败"
		if !errors.Is(err, vault.ErrAssetSize) && !errors.Is(err, vault.ErrAssetFormat) && !errors.Is(err, vault.ErrLowSpace) {
			if meta.Cover != "" {
				result.Cover, _ = saveRemote(ctx, d.HTTP, d.Assets, msg.Date(), "image", meta.Cover, referer)
			}
			return result, true
		}
		if errors.Is(err, vault.ErrAssetSize) {
			result.Reason, result.Note = "大小超限", "视频超过大小上限"
		} else if errors.Is(err, vault.ErrLowSpace) {
			result.Reason, result.Note = "磁盘不足", err.Error()
		} else {
			result.Reason, result.Note = "格式无效", "视频响应格式无效"
		}
	}
	if meta.Cover != "" {
		result.Cover, _ = saveRemote(ctx, d.HTTP, d.Assets, msg.Date(), "image", meta.Cover, referer)
	}
	return result, false
}

func (d *Douyin) id(ctx context.Context, raw string) (string, string) {
	if id := videoID(raw); id != "" {
		return id, ""
	}
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Hostname(), "v.douyin.com") {
		return "", "未识别到视频作品"
	}
	short := *d.HTTP
	short.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := request(ctx, &short, raw, "", "")
	if err != nil {
		return "", "短链请求失败"
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		return "", "短链失效"
	}
	if resp.StatusCode != http.StatusMovedPermanently && resp.StatusCode != http.StatusFound {
		return "", "短链没有有效跳转"
	}
	target, err := resp.Location()
	if err != nil {
		return "", "短链没有有效跳转"
	}
	if id := videoID(target.String()); id != "" {
		return id, ""
	}
	return "", "短链首跳未指向视频作品"
}

func (d *Douyin) details(ctx context.Context, id string) (video, bool) {
	client := *d.HTTP
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > 2 || !validHTTP(req.URL) || model.Platform(req.URL.String()) != model.Douyin {
			return http.ErrUseLastResponse
		}
		return nil
	}
	for _, page := range []string{"https://www.iesdouyin.com/share/video/" + id + "/", "https://www.douyin.com/video/" + id} {
		resp, err := request(ctx, &client, page, "https://www.douyin.com/", "")
		if err != nil {
			continue
		}
		body, err := readPage(resp)
		if err == nil {
			if found, ok := parseVideoPage(body, id); ok {
				return found, true
			}
		}
	}
	return video{}, false
}

func parseVideoPage(body []byte, id string) (video, bool) {
	z := html.NewTokenizer(bytes.NewReader(body))
	script := false
	for {
		switch z.Next() {
		case html.ErrorToken:
			return video{}, false
		case html.StartTagToken:
			script = z.Token().Data == "script"
		case html.EndTagToken:
			script = false
		case html.TextToken:
			if !script {
				continue
			}
			data := strings.TrimSpace(string(z.Text()))
			if strings.HasPrefix(data, "%7B") || strings.HasPrefix(data, "%7b") {
				if decoded, err := url.PathUnescape(data); err == nil {
					data = decoded
				}
			}
			if start := strings.IndexByte(data, '{'); start >= 0 {
				var root any
				decoder := json.NewDecoder(strings.NewReader(data[start:]))
				decoder.UseNumber()
				if decoder.Decode(&root) == nil {
					if meta, ok := findVideo(root, id); ok {
						return meta, true
					}
				}
			}
		}
	}
}

func findVideo(node any, id string) (video, bool) {
	switch value := node.(type) {
	case []any:
		for _, child := range value {
			if meta, ok := findVideo(child, id); ok {
				return meta, true
			}
		}
	case map[string]any:
		key := field(value, "aweme_id", "awemeId", "id")
		content, ok := value["video"].(map[string]any)
		images, _ := value["images"].([]any)
		if key == id && ok && len(images) == 0 {
			author, _ := value["author"].(map[string]any)
			addr := field(content, "playAddr", "playUrl")
			if addr == "" {
				addr = nestedURL(content, "play_addr", "playAddr", "playAddrH264", "play_api")
			}
			if play, err := url.Parse(addr); err == nil && model.Platform(addr) == model.Douyin && strings.HasPrefix(play.Path, "/aweme/v1/playwm/") {
				play.Path = strings.Replace(play.Path, "/playwm/", "/play/", 1)
				addr = play.String()
			}
			cover := field(content, "cover", "originCover")
			if cover == "" {
				cover = nestedURL(content, "cover", "origin_cover", "dynamic_cover")
			}
			duration, _ := strconv.ParseInt(field(content, "duration"), 10, 64)
			if duration == 0 {
				duration, _ = strconv.ParseInt(field(value, "duration"), 10, 64)
			}
			return video{Title: field(value, "desc", "title"), Author: field(author, "nickname"), Cover: cover, Play: addr, Duration: duration}, true
		}
		for _, child := range value {
			if meta, ok := findVideo(child, id); ok {
				return meta, true
			}
		}
	}
	return video{}, false
}

func field(m map[string]any, keys ...string) string {
	for _, k := range keys {
		switch v := m[k].(type) {
		case string:
			if v != "" {
				return v
			}
		case json.Number:
			return v.String()
		}
	}
	return ""
}

func nestedURL(m map[string]any, keys ...string) string {
	for _, key := range keys {
		node, _ := m[key].(map[string]any)
		for _, list := range []string{"url_list", "urlList"} {
			items, _ := node[list].([]any)
			for _, item := range items {
				if link, ok := item.(string); ok && (strings.HasPrefix(link, "https://") || strings.HasPrefix(link, "http://")) {
					return link
				}
			}
		}
	}
	return ""
}
