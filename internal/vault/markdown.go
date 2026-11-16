package vault

import (
	"crypto/sha256"
	"fmt"
	"os"
	"strings"

	"github.com/richaction/onebox-collector/internal/model"
)

func render(m model.Message, result model.Result) string {
	var b strings.Builder
	key := sha256.Sum256([]byte(m.Channel + "\x00" + m.ID))
	fmt.Fprintf(&b, "<!-- onebox:%x %d -->\n## %s\n", key, m.Sent.Unix(), m.Sent.In(model.Shanghai).Format("15:04"))
	if m.Type == model.Text {
		b.WriteString(plain(m.Text) + "\n")
	}
	switch m.Type {
	case model.Image:
		image(&b, "图片", result.Media)
		if m.MediaID != "" {
			fmt.Fprintf(&b, "\n- 图片标识：%s\n", inline(m.MediaID))
		}
	case model.Douyin:
		b.WriteString("### 抖音视频\n\n")
		image(&b, "视频", result.Media)
		image(&b, "封面", result.Cover)
		if result.Title != "" || m.Title != "" {
			fmt.Fprintf(&b, "- 标题：%s\n", inline(first(result.Title, m.Title)))
		}
		if result.Author != "" {
			fmt.Fprintf(&b, "- 作者：%s\n", inline(result.Author))
		}
		writeLink(&b, "原始链接", m.URL)
	case model.Official:
		b.WriteString("### 微信公众平台")
		if title := first(m.Title, result.Title); title != "" {
			b.WriteString("：" + inline(title))
		}
		b.WriteString("\n\n")
		image(&b, "封面", result.Cover)
		if result.Cover == "" {
			writeLink(&b, "封面来源", model.NormalizeURL(m.CoverURL))
		}
		if m.Description != "" {
			fmt.Fprintf(&b, "> 摘要：%s\n\n", inline(m.Description))
		}
		writeLink(&b, "原文链接", m.URL)
		if result.Body != "" {
			fmt.Fprintf(&b, "- 正文：已归档（L2）\n- 正文抓取时间：%s\n\n%s\n",
				result.FetchedAt.In(model.Shanghai).Format("2006-01-02 15:04:05"), stripMarkers(result.Body))
		}
	case model.Link:
		writeLink(&b, first(m.Title, "原始链接"), m.URL)
		if m.Description != "" {
			fmt.Fprintf(&b, "- 摘要：%s\n", inline(m.Description))
		}
	case model.Unknown:
		fmt.Fprintf(&b, "- 未识别消息类型：%s\n", inline(m.RawType))
		summary := []rune(m.Raw)
		fmt.Fprintf(&b, "- 报文摘要：%s\n", inline(string(summary[:min(len(summary), 200)])))
	}
	if m.Type != model.Text && m.Text != "" {
		b.WriteString("\n" + plain(m.Text) + "\n")
	}
	if result.Note != "" {
		fmt.Fprintf(&b, "\n- %s\n", inline(result.Note))
	}
	b.WriteString("\n<!-- /onebox -->\n")
	return b.String()
}

func first(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func image(b *strings.Builder, label, path string) {
	if path != "" {
		fmt.Fprintf(b, "![%s](%s)\n\n", label, path)
	}
}

func writeLink(b *strings.Builder, label, link string) {
	if link != "" {
		link = strings.NewReplacer("<", "%3C", ">", "%3E").Replace(link)
		fmt.Fprintf(b, "- [%s](<%s>)\n", inline(label), link)
	}
}

func stripMarkers(s string) string {
	return strings.NewReplacer("<!-- onebox:", "&lt;!-- onebox:", "<!-- /onebox -->", "&lt;!-- /onebox -->").Replace(s)
}

var literal = strings.NewReplacer(
	"&", "&amp;", "<", "&lt;", ">", "&gt;", "\\", "\\\\", "[", "\\[", "]", "\\]", "*", "\\*", "_", "\\_", "`", "\\`",
	"#", "\\#", "-", "\\-", "~", "\\~",
)

func plain(s string) string { return literal.Replace(s) }

func inline(s string) string { return plain(strings.Join(strings.Fields(s), " ")) }

// ReplaceIfCurrent avoids overwriting an append that arrived during remote sync.
func (w *Writer) ReplaceIfCurrent(date string, previous, merged []byte) error {
	unlock := w.lock(date)
	defer unlock()
	path := w.NotePath(date)
	current, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if string(current) != string(previous) {
		merged, err = Merge(current, merged)
		if err != nil {
			return err
		}
	}
	if string(current) == string(merged) {
		return nil
	}
	return atomicWrite(path, merged)
}
