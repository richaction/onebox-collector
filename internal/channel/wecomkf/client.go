package wecomkf

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/richaction/onebox-collector/internal/model"
)

const APIBase = "https://qyapi.weixin.qq.com/cgi-bin"

type Client struct {
	Base, Corp, Secret string
	HTTP               *http.Client

	mu        sync.Mutex
	token     string
	refreshAt time.Time
}

type apiError struct {
	Code int `json:"errcode"`
}

func (e apiError) Error() string { return fmt.Sprintf("WeCom API error %d", e.Code) }

func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.refreshAt) {
		return c.token, nil
	}
	var result struct {
		apiError
		Token   string `json:"access_token"`
		Expires int    `json:"expires_in"`
	}
	q := url.Values{"corpid": {c.Corp}, "corpsecret": {c.Secret}}
	if err := c.call(ctx, http.MethodGet, "/gettoken", q, nil, &result); err != nil {
		return "", err
	}
	if result.Code != 0 {
		return "", result.apiError
	}
	if result.Token == "" || result.Expires <= 0 {
		return "", errors.New("WeCom returned no usable access token")
	}
	lifetime := time.Duration(result.Expires) * time.Second
	c.token = result.Token
	c.refreshAt = time.Now().Add(lifetime - min(time.Minute, lifetime/2))
	return c.token, nil
}

func (c *Client) call(ctx context.Context, method, path string, q url.Values, payload []byte, out any) error {
	resp, err := c.request(ctx, method, path, q, payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	const limit = 16 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || len(body) > limit {
		return fmt.Errorf("WeCom %s unreadable response", path)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("WeCom %s invalid JSON response", path)
	}
	return nil
}

func (c *Client) request(ctx context.Context, method, path string, q url.Values, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path+"?"+q.Encode(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("WeCom %s invalid request", path)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// The request URL in a transport error can contain credentials.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("WeCom %s request failed", path)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("WeCom %s HTTP %d", path, resp.StatusCode)
	}
	return resp, nil
}

func (c *Client) withToken(ctx context.Context, call func(string) error) error {
	for attempt := 0; ; attempt++ {
		token, err := c.accessToken(ctx)
		if err != nil {
			return err
		}
		err = call(token)
		var api apiError
		if attempt != 0 || !errors.As(err, &api) || (api.Code != 40001 && api.Code != 40014 && api.Code != 42001) {
			return err
		}
		c.mu.Lock()
		if c.token == token {
			c.token = ""
		}
		c.mu.Unlock()
	}
}

type Page struct {
	Cursor string
	More   bool
	Raw    []json.RawMessage
}

func (c *Client) Pull(ctx context.Context, account, callbackToken, cursor string) (Page, error) {
	payload, err := json.Marshal(struct {
		Account string `json:"open_kfid"`
		Token   string `json:"token,omitempty"`
		Cursor  string `json:"cursor,omitempty"`
		Limit   int    `json:"limit"`
	}{account, callbackToken, cursor, 100})
	if err != nil {
		return Page{}, err
	}
	var result struct {
		apiError
		Cursor string            `json:"next_cursor"`
		More   int               `json:"has_more"`
		Raw    []json.RawMessage `json:"msg_list"`
	}
	err = c.withToken(ctx, func(token string) error {
		if err := c.call(ctx, http.MethodPost, "/kf/sync_msg", url.Values{"access_token": {token}}, payload, &result); err != nil {
			return err
		}
		if result.Code != 0 {
			return result.apiError
		}
		return nil
	})
	if err != nil {
		return Page{}, err
	}
	if (result.More == 1 || len(result.Raw) > 0) && (result.Cursor == "" || result.Cursor == cursor) {
		return Page{}, errors.New("WeCom sync_msg returned a non-advancing cursor")
	}
	return Page{Cursor: result.Cursor, More: result.More == 1, Raw: result.Raw}, nil
}

func (c *Client) FetchMedia(ctx context.Context, id string) (io.ReadCloser, error) {
	var body io.ReadCloser
	err := c.withToken(ctx, func(token string) error {
		q := url.Values{"access_token": {token}, "media_id": {id}}
		resp, err := c.request(ctx, http.MethodGet, "/media/get", q, nil)
		if err != nil {
			return err
		}
		if strings.Contains(resp.Header.Get("Content-Type"), "json") {
			defer resp.Body.Close()
			var api apiError
			if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&api); err == nil && api.Code != 0 {
				return api
			}
			return errors.New("WeCom media/get returned JSON instead of media")
		}
		body = resp.Body
		return nil
	})
	return body, err
}

type card struct {
	Title    string `json:"title"`
	URL      string `json:"url"`
	Desc     string `json:"desc"`
	Digest   string `json:"digest"`
	Picture  string `json:"pic_url"`
	ThumbURL string `json:"thumb_url"`
	ThumbID  string `json:"thumb_media_id"`
}

func Adapt(raw json.RawMessage) (model.Message, bool, error) {
	var msg struct {
		ID     string          `json:"msgid"`
		User   string          `json:"external_userid"`
		OpenID string          `json:"openid"`
		Time   json.RawMessage `json:"send_time"`
		Origin int             `json:"origin"`
		Type   string          `json:"msgtype"`
		Text   struct {
			Content string `json:"content"`
		} `json:"text"`
		Image struct {
			ID string `json:"media_id"`
		} `json:"image"`
		Link   card `json:"link"`
		MPNews card `json:"mpnews"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return model.Message{}, false, err
	}
	if (msg.Origin != 3 && msg.Origin != 0) || msg.Type == "event" {
		return model.Message{}, false, nil
	}
	sent, err := messageTime(msg.Time)
	if msg.ID == "" || err != nil || msg.Type == "" {
		return model.Message{}, false, errors.New("WeCom message is missing a valid msgid, send_time or msgtype")
	}
	m := model.Message{ID: msg.ID, Channel: "kf", SenderID: msg.User, Sent: sent,
		RawType: msg.Type, Raw: string(raw), Type: model.Unknown}
	if m.SenderID == "" {
		m.SenderID = msg.OpenID
	}
	switch msg.Type {
	case "text":
		m.Text = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(msg.Text.Content, "\r\n", "\n"), "\r", "\n"))
		m.URL, m.Type = model.TextLink(m.Text)
	case "image":
		m.Type, m.MediaID = model.Image, msg.Image.ID
	case "link", "mpnews":
		c := msg.Link
		if msg.Type == "mpnews" {
			c = msg.MPNews
		}
		m.URL, m.Title, m.Description = model.NormalizeURL(c.URL), strings.TrimSpace(c.Title), c.Desc
		if m.Description == "" {
			m.Description = c.Digest
		}
		description := []rune(m.Description)
		m.Description = string(description[:min(200, len(description))])
		m.CoverURL, m.CoverID = c.Picture, c.ThumbID
		if m.CoverURL == "" {
			m.CoverURL = c.ThumbURL
		}
		if m.CoverURL == "" && strings.Contains(c.ThumbID, "://") {
			m.CoverURL, m.CoverID = c.ThumbID, ""
		}
		if m.URL != "" {
			m.Type = model.Platform(m.URL)
			if m.Title == "" {
				u, _ := url.Parse(m.URL)
				m.Title = u.Hostname()
			}
		}
	}
	return m, true, nil
}

func messageTime(raw json.RawMessage) (time.Time, error) {
	var seconds int64
	if err := json.Unmarshal(raw, &seconds); err == nil && seconds > 0 {
		return time.Unix(seconds, 0), nil
	}
	var formatted string
	if err := json.Unmarshal(raw, &formatted); err == nil {
		if sent, err := time.Parse(time.RFC3339, formatted); err == nil && sent.Unix() > 0 {
			return sent, nil
		}
	}
	return time.Time{}, errors.New("invalid message timestamp")
}
