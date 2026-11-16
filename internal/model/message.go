package model

import (
	"net/url"
	"regexp"
	"strings"
	"time"
	_ "time/tzdata"
)

type Type string

const (
	Text     Type = "text"
	Image    Type = "image"
	Douyin   Type = "douyin"
	Official Type = "official"
	Link     Type = "link"
	Unknown  Type = "unknown"
)

type Message struct {
	ID, Channel, SenderID string
	Type                  Type
	Sent                  time.Time
	Text, Title, URL      string
	Description           string
	CoverURL, CoverID     string
	MediaID, RawType      string
	Raw                   string
}

type Result struct {
	Level                                     int
	Media, Cover, Title, Author, Note, Reason string
	Body                                      string
	FetchedAt                                 time.Time
}

var Shanghai = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		panic(err)
	}
	return loc
}()

func (m Message) Date() string { return m.Sent.In(Shanghai).Format(time.DateOnly) }

var embeddedWork = regexp.MustCompile(`(?i)\baweme_id[=:：]\s*([0-9]{8,24})\b`)

var links = regexp.MustCompile(`(?i)(?:https?://|(?:v\.|www\.)?douyin\.com/|mp\.weixin\.qq\.com/)[^\s<>"，。！？、；：）】》]+`)

func NormalizeURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	switch {
	case strings.HasPrefix(raw, "//"):
		raw = "https:" + raw
	case !strings.Contains(raw, "://"):
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	if Platform(u.String()) == Official {
		u.Scheme = "https"
		u.Fragment = ""
	}
	if q, err := url.ParseQuery(u.RawQuery); err == nil {
		changed := false
		for k := range q {
			switch {
			case strings.HasPrefix(strings.ToLower(k), "utm_"), k == "fbclid", k == "gclid":
				q.Del(k)
				changed = true
			}
		}
		if changed {
			u.RawQuery = q.Encode()
		}
	}
	return u.String()
}

func Platform(raw string) Type {
	u, err := url.Parse(raw)
	if err != nil {
		return Unknown
	}
	switch strings.ToLower(u.Hostname()) {
	case "v.douyin.com", "www.douyin.com", "douyin.com", "m.douyin.com", "iesdouyin.com", "www.iesdouyin.com":
		return Douyin
	case "mp.weixin.qq.com":
		return Official
	default:
		return Link
	}
}

func TextLink(content string) (string, Type) {
	var standalone string
	for _, loc := range links.FindAllStringIndex(content, -1) {
		candidate := content[loc[0]:loc[1]]
		if !strings.Contains(candidate, "://") && loc[0] > 0 &&
			strings.ContainsAny(content[loc[0]-1:loc[0]], "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789.-/@") {
			continue
		}
		link := NormalizeURL(strings.TrimRight(candidate, ".,!?;:)]}"))
		if link == "" {
			continue
		}
		if kind := Platform(link); kind != Link {
			return link, kind
		}
		if strings.TrimSpace(content) == candidate {
			standalone = link
		}
	}
	if match := embeddedWork.FindStringSubmatch(content); len(match) == 2 {
		return "https://www.douyin.com/video/" + match[1], Douyin
	}
	if standalone != "" {
		return standalone, Link
	}
	return "", Text
}
