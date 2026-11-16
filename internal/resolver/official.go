package resolver

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown/v2"
	"github.com/richaction/onebox-collector/internal/model"
	"golang.org/x/net/html"
)

type OfficialHistory interface {
	OfficialStatus(ctx context.Context, link string) (string, error)
	SetOfficialStatus(ctx context.Context, link, status string) error
}

type Official struct {
	HTTP      *http.Client
	Assets    Assets
	History   OfficialHistory
	Enabled   bool
	Timeout   time.Duration
	MaxImages int
	UA        string
}

func (o *Official) Resolve(ctx context.Context, m model.Message, cover string) model.Result {
	result := model.Result{Level: 1, Cover: cover}
	if result.Cover == "" && m.CoverURL != "" {
		link := model.NormalizeURL(m.CoverURL)
		result.Cover, _ = saveRemote(ctx, o.HTTP, o.Assets, m.Date(), "image", link, m.URL)
	}
	if result.Cover == "" && (m.CoverURL != "" || m.CoverID != "") {
		result.Note = "封面下载失败"
	}
	if !o.Enabled {
		return result
	}
	if status, _ := o.History.OfficialStatus(ctx, m.URL); status == "blocked" {
		result.Note = addNote(result.Note, "正文抓取失败：近期曾触发公众号验证")
		return result
	}
	fetchCtx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	body, title, remoteCover, omitted, err := o.article(fetchCtx, m)
	if err != nil {
		status := "failed"
		if errors.Is(err, errBlocked) {
			status = "blocked"
		}
		_ = o.History.SetOfficialStatus(ctx, m.URL, status)
		result.Note = addNote(result.Note, "正文抓取失败："+err.Error())
		return result
	}
	_ = o.History.SetOfficialStatus(ctx, m.URL, "ok")
	result.Level, result.Body, result.Title, result.FetchedAt = 2, body, title, time.Now()
	if result.Cover == "" && remoteCover != "" {
		result.Cover, _ = saveRemote(fetchCtx, o.HTTP, o.Assets, m.Date(), "image", remoteCover, m.URL)
	}
	if omitted {
		result.Note = addNote(result.Note, "正文图片超过数量上限，已保留前面的图片")
	}
	return result
}

func addNote(note, more string) string {
	if note == "" {
		return more
	}
	return note + "；" + more
}

var errBlocked = errors.New("页面需要验证")

func (o *Official) article(ctx context.Context, m model.Message) (body, title, cover string, omitted bool, err error) {
	client := *o.HTTP
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > 2 || !validHTTP(req.URL) || model.Platform(req.URL.String()) != model.Official || strings.Contains(req.URL.Path, "verify") ||
			strings.Contains(req.URL.RawQuery, "wappoc_appmsgcaptcha") {
			return http.ErrUseLastResponse
		}
		return nil
	}
	resp, err := request(ctx, &client, m.URL, "https://mp.weixin.qq.com/", o.UA)
	if err != nil {
		return "", "", "", false, err
	}
	if resp.StatusCode == http.StatusForbidden || (resp.StatusCode >= 300 && resp.StatusCode < 400) {
		resp.Body.Close()
		return "", "", "", false, errBlocked
	}
	raw, err := readPage(resp)
	if err != nil {
		return "", "", "", false, err
	}
	doc, err := html.Parse(bytes.NewReader(raw))
	if err != nil {
		return "", "", "", false, errors.New("文章 HTML 解析失败")
	}
	var section *html.Node
	walk(doc, func(n *html.Node) {
		if n.Type != html.ElementNode {
			return
		}
		if n.Data == "div" && attr(n, "id") == "js_content" {
			section = n
		}
		if n.Data == "meta" {
			switch attr(n, "property") {
			case "og:title":
				title = attr(n, "content")
			case "og:image":
				cover = resolvedLink(resp.Request.URL, attr(n, "content"))
			}
		}
	})
	if section == nil {
		if bytes.Contains(raw, []byte("环境异常")) || bytes.Contains(raw, []byte("访问验证")) {
			return "", "", "", false, errBlocked
		}
		return "", "", "", false, errors.New("页面缺少正文")
	}
	count := 0
	var clean func(*html.Node) error
	clean = func(parent *html.Node) error {
		for n := parent.FirstChild; n != nil; {
			next := n.NextSibling
			if n.Type == html.CommentNode {
				parent.RemoveChild(n)
				n = next
				continue
			}
			if n.Type == html.ElementNode {
				selector := strings.ToLower(attr(n, "id") + " " + attr(n, "class"))
				switch {
				case strings.Contains(selector, "qrcode") || strings.Contains(selector, "js_bottom_ad") ||
					strings.Contains(selector, "js_ad_area") || strings.Contains(selector, "js_like"):
					parent.RemoveChild(n)
				case n.Data == "script" || n.Data == "style" || n.Data == "iframe" || n.Data == "svg" || n.Data == "noscript":
					parent.RemoveChild(n)
				case n.Data == "img":
					if count >= o.MaxImages {
						parent.RemoveChild(n)
						omitted = true
						break
					}
					src := attr(n, "data-src")
					if src == "" {
						src = attr(n, "src")
					}
					link := resolvedLink(resp.Request.URL, src)
					if link == "" {
						return errors.New("正文图片地址无效")
					}
					path, err := saveRemote(ctx, o.HTTP, o.Assets, m.Date(), "image", link, m.URL)
					if err != nil {
						return errors.New("正文图片下载失败")
					}
					count++
					n.Attr = []html.Attribute{{Key: "src", Val: path}, {Key: "alt", Val: attr(n, "alt")}}
				case n.Data == "a":
					n.Attr = []html.Attribute{{Key: "href", Val: resolvedLink(resp.Request.URL, attr(n, "href"))}}
				default:
					n.Attr = nil
				}
			}
			if n.Parent != nil {
				if err := clean(n); err != nil {
					return err
				}
			}
			n = next
		}
		return nil
	}
	if err := clean(section); err != nil {
		return "", "", "", false, err
	}
	md, err := htmltomarkdown.ConvertNode(section)
	if err != nil || strings.TrimSpace(string(md)) == "" {
		return "", "", "", false, errors.New("正文转换为空")
	}
	return strings.TrimSpace(string(md)), title, cover, omitted, nil
}

func walk(n *html.Node, visit func(*html.Node)) {
	visit(n)
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		walk(child, visit)
	}
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func resolvedLink(base *url.URL, raw string) string {
	if raw == "" {
		return ""
	}
	u, err := base.Parse(raw)
	if err != nil || !validHTTP(u) {
		return ""
	}
	return u.String()
}
