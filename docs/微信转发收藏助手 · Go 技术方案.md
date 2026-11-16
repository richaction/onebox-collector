# 微信转发收藏助手 · Go 技术方案（原型版：微信客服 Webhook）

本方案面向一个轻量收藏服务：用户在微信侧转发内容，服务通过 Webhook 接收消息，按日期写入本地 Markdown 笔记。原型阶段接入企业微信「微信客服」通道，先跑通接收、分类与落盘全链路；架构上以适配器隔离接入层，后期可平滑切换到「企微成员 \+ 会话存档」的好友模式（Cubox 同款体验），业务核心代码不重做。

**当前结论：**原型用微信客服做 Webhook 接收；文本按时间落多行、图片下载后嵌入 MD、抖音链接解析为视频、公众号卡片暂存链接。接入层与业务层分离，后期仅替换适配器即可迁移到会话存档。

# 1\. 背景、目标与范围

## 1\.1 目标

- 提供一个 HTTPS 可访问的 Webhook 服务，接收企业微信客服推送的用户消息事件。

- 按消息类型分类处理，内容按日期归档到 Markdown 文件，媒体文件落本地资源目录。

- 代码分层清晰，为后期切换会话存档通道预留扩展点。

## 1\.2 阶段划分

|阶段|接入通道|体验与约束|
|---|---|---|
|**原型（本期）**|企业微信 · 微信客服 Webhook|无需加好友；客服会话内发送，回调直推；存在会话超时与被动回复条数限制。|
|**正式（后期）**|企微成员号 \+ 会话存档服务商|好友在通讯录，支持长按文章直接转发；需企业认证、合规告知与服务商成本。|

## 1\.3 本期范围与非目标

- **本期做：**Webhook 校验与解密、消息拉取、标准化、四类内容分类落盘、基础日志与失败重试。

- **本期不做：**多用户账号体系完善、公众号正文抓取与摘要、视频转码、云端对象存储与同步、前端管理界面。

# 2\. 总体架构与分层

## 2\.1 数据流

1. 用户在微信客服会话中发送文本、图片或转发链接卡片。

2. 企业微信服务器向配置的回调 URL 发起 GET 校验（启用时）或 POST 事件推送（运行时）。

3. 接入层完成签名校验、报文解密与消息拉取，输出统一标准化消息结构体。

4. 业务核心层按消息类型分发到对应处理器：文本、图片、抖音、公众号。

5. 存储层按日期目录写入 Markdown，并把图片、视频等资源落到对应资源目录。

## 2\.2 分层职责

|层级|职责|后期迁移时是否改动|
|---|---|---|
|接入层（Webhook）|HTTP 路由、GET 校验、加解密、access\_token 获取、详情拉取、可信 IP 校验。|切换通道时整体替换。|
|适配器层（Adapter）|把不同通道的原始报文映射为统一标准结构体，抹平字段与媒体拉取差异。|新增一个存档适配器，业务层不动。|
|业务核心层|消息分类、解析、入库判定、MD 组装、回复话术。|不改动。|
|存储层|日期目录管理、MD 追加写入、媒体文件落盘与引用路径生成。|不改动，仅可扩展存储后端。|

关键设计原则：业务核心只认识标准化结构体，不感知消息来自客服会话还是会话存档。这样后期接入存档时，只需要新增适配器并对接存档增量拉取接口。

# 3\. 技术选型（Go）

|用途|选型|说明|
|---|---|---|
|HTTP 服务|标准库 net/http，或 chi / gin|原型阶段标准库即可；路由简单，无强制框架依赖。|
|加解密|crypto/aes、crypto/sha1、encoding/xml|按企微官方加解密方案实现 AES\-256\-CBC 解密与签名校验。|
|配置管理|环境变量 \+ viper（可选）|Token、EncodingAESKey、CorpID、回调路径、落盘根目录等走配置。|
|媒体下载|net/http \+ io|按 media\_id 调接口拉取图片/视频二进制，落资源目录。|
|链接解析|net/url、golang\.org/x/net/html（按需）|抖音链接提取作品标识；公众号卡片先保留原始链接与元信息。|
|存储|本地文件系统|MD 与资源文件按日期目录存放；后期可替换为对象存储。|
|日志与重试|log/slog \+ 内置队列（可选 asynq）|记录回调报文与处理结果；失败消息进重试队列，保证不丢。|

# 4\. 目录结构与 MD 落盘规范

## 4\.1 目录结构

以落盘根目录为基础，按日期组织正文与资源：

**命名铁律：一天一个 MD。**文件名严格等于日期，即 `2026-11-16.md`；不追加序号、不用标题命名、不按消息切分、不出现第二份当日文件。同一天的所有内容都进这一个文件；资源文件命名为「日期\-hash前8位\.扩展名」。

```text
root/
├── 2026-11-16/
│   ├── 2026-11-16.md        # 当日唯一正文，一天一个
│   └── assets/              # 当日媒体，命名见 13.2
│       ├── 2026-11-16-a1b2c3d4.jpg
│       └── 2026-11-16-9f8e7d6c.mp4
├── 2026-11-17/
│   ├── 2026-11-17.md
│   └── assets/
└── ...
```

- 目录与文件名日期采用本地时区（Asia/Shanghai），格式 `YYYY-MM-DD`。

- 同一日期的所有消息追加到当日 MD；首次写入时初始化文件与标题。

- 媒体文件名统一按 13\.2 的「日期\-hash前8位\.扩展名」生成；hash 取文件内容，同图重复转发只存一份。MD 内一律用相对路径引用。

## 4\.2 各类型写入规则

|消息类型|处理动作|MD 呈现|
|---|---|---|
|文字|记录发送时间与原文；同一用户连续多条文字按多行追加，时间可弱化，仅在条目首行保留时间。|按时间顺序写入多行文本；时间作为行内前缀或小标题，不强制每条都单独成段。|
|图片|按 media\_id 下载到当日 assets 目录，记录原始图片标识。|MD 中以相对路径嵌入图片，下方可附时间与来源说明。|
|抖音链接|识别抖音作品链接，解析作品标识，后续拉取视频地址并存为本地视频；解析失败时降级为保留链接。|成功则嵌入本地视频引用；失败则在 MD 中保留原始链接与解析备注。|
|公众号卡片|本期仅保留标题、摘要、原文链接与封面地址，不抓取正文。|MD 中以链接条目呈现：标题 \+ 原文链接；正文抓取为开关后的增强能力，见第 11 章。|

## 4\.3 当日 MD 模板示例

```markdown
# 2026-11-16 收藏记录

## 09:12
今天看的这篇讲 Go 适配器分层，思路比较清晰。

## 09:15
![图片](assets/2026-11-16-a1b2c3d4.jpg)

## 09:20
### 抖音视频
![视频](assets/2026-11-16-9f8e7d6c.mp4)
- 原始链接：https://v.douyin.com/xxxx/

## 09:25
### 公众号
- [文章标题](https://mp.weixin.qq.com/s/xxxx)
- 摘要：这里是卡片摘要（可选）
```

# 5\. Webhook 接入核心（重点）

## 5\.1 回调流程

1. **GET 校验：**后台保存回调地址时，企微携带 signature、timestamp、nonce、echostr 发起 GET 请求；服务端按 Token、timestamp、nonce 排序拼接后做 SHA1 校验，校验通过原样返回 echostr。

2. **POST 事件：**运行时用户发消息，企微 POST 加密后的 XML 报文；服务端解密得到事件类型与消息体。

3. **详情拉取：**客服通道通常先推送事件通知，需凭事件中的会话与消息标识，调用客服消息接口拉取完整内容（access\_token 提前获取并缓存）。

4. **标准化与分发：**适配器把原始报文转为标准结构体，业务层按类型处理，最后返回 200；超时或异常按重试策略处理。

## 5\.2 加解密与校验要点

- 签名：将 Token、timestamp、nonce 字典序排序后拼接，SHA1 哈希与 signature 比对。

- 报文：EncodingAESKey 对应 AES\-256\-CBC，解密后去除随机字节、消息长度与 CorpID/SuiteID 前缀，再解析 XML。

- 身份：解密后校验企业标识，非本企业报文直接拒绝。

- 响应：GET 校验返回 echostr 原文；POST 处理成功返回 success 或 200，处理失败记录日志并按通道要求返回错误，便于企微重试。

## 5\.3 标准化消息模型

适配器输出如下统一结构体，业务层只依赖该模型：

```go
package model

import "time"

// MsgType 标准化消息类型
type MsgType string

const (
	MsgText     MsgType = "text"
	MsgImage    MsgType = "image"
	MsgDouyin   MsgType = "douyin"
	MsgOfficial MsgType = "official" // 公众号卡片
	MsgUnknown  MsgType = "unknown"
)

// StandardMsg 接入层/适配器层输出的统一消息结构
type StandardMsg struct {
	SenderID   string    // 通道内用户唯一标识（原型为 openid）
	MsgType    MsgType   // 标准化类型
	SendTime   time.Time // 消息时间
	Text       string    // 文本内容
	Title      string    // 卡片标题（公众号/链接）
	URL        string    // 原始链接
	Cover      string    // 封面地址（可选）
	Desc       string    // 摘要（可选）
	MediaID    string    // 通道原始媒体标识
	LocalPath  string    // 落盘后的本地相对路径（图片/视频）
	RawPayload string    // 原始报文留档，便于排查
}
```

## 5\.4 Handler 骨架

```go
package handler

import (
	"io"
	"net/http"
)

// Webhook 同时承载 GET 校验与 POST 事件
func Webhook(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		verify(w, r) // 签名校验后原样返回 echostr
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body failed", http.StatusBadRequest)
		return
	}

	// 1. 解密 + 校验企业标识
	rawEvent, err := decrypt(body)
	if err != nil {
		http.Error(w, "decrypt failed", http.StatusBadRequest)
		return
	}

	// 2. 适配器：原始报文 -> 标准化消息（客服通道实现）
	msg, err := adapter.ToStandard(rawEvent)
	if err != nil {
		http.Error(w, "adapt failed", http.StatusInternalServerError)
		return
	}

	// 3. 业务核心：分类处理并落盘
	if err := service.Handle(msg); err != nil {
		http.Error(w, "handle failed", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("success"))
}
```

## 5\.5 落盘处理器骨架

```go
package service

import (
	"fmt"
	"path/filepath"
	"time"

	"collector/model"
)

func Handle(msg *model.StandardMsg) error {
	date := msg.SendTime.Format("2006-01-02")
	dir := filepath.Join(cfg.RootDir, date)
	assetsDir := filepath.Join(dir, "assets")

	switch msg.MsgType {
	case model.MsgText:
		return writer.AppendText(dir, date, msg)
	case model.MsgImage:
		if err := media.DownloadImage(msg, assetsDir); err != nil {
			return err
		}
		return writer.AppendImage(dir, date, msg)
	case model.MsgDouyin:
		if err := douyin.ResolveAndDownload(msg, assetsDir); err != nil {
			// 解析失败降级：保留原始链接
			return writer.AppendLinkFallback(dir, date, msg, fmt.Sprintf("抖音解析失败：%v", err))
		}
		return writer.AppendDouyin(dir, date, msg)
	case model.MsgOfficial:
		return writer.AppendOfficial(dir, date, msg)
	default:
		return writer.AppendUnknown(dir, date, msg)
	}
}
```

## 5\.6 幂等、重试与安全

- **幂等：**以通道消息唯一标识做去重记录，重复推送不重复写入 MD。

- **重试：**拉取媒体、解析链接等可失败步骤进入重试队列；MD 追加写入保证顺序与不覆盖。

- **可信 IP：**服务出口 IP 配置到企微后台可信 IP 列表；回调路径仅接收企微请求。

- **超时控制：**外部接口调用设置超时，避免长连接阻塞回调响应。

# 6\. 消息类型处理细则

|类型|识别方式|处理要点|当前状态|
|---|---|---|---|
|文字|消息类型为文本，无媒体标识与链接。|清洗换行与特殊字符；连续短文本合并为同一条目，时间弱化。|本期实现|
|图片|图片消息，携带 media\_id。|下载二进制到 assets，校验格式与大小；MD 相对路径引用。|本期实现|
|抖音链接|文本或卡片 URL 命中抖音域名/短链。|提取作品 ID，解析无水印视频地址并下载；失败降级为链接留存。|本期实现解析，稳定性待调|
|公众号卡片|mpnews 类型卡片，含标题、链接、封面、摘要。|仅落标题、链接、摘要、封面；正文抓取方案待定。|暂存链接|
|其他（语音/文件/位置等）|未在本期分类内的类型。|记录原始类型与报文摘要，留待后续支持。|兜底记录|

# 7\. 核心配置项

|配置项|示例|说明|
|---|---|---|
|CallbackToken|自定义字符串|回调签名校验 Token。|
|EncodingAESKey|43 位随机串|报文加解密密钥。|
|CorpID|企业 ID|解密后校验企业身份。|
|CallbackPath|/webhook/kf|回调路由路径，需 HTTPS 公网可访问。|
|RootDir|\./collect|MD 与资源文件落盘根目录。|
|MediaMaxSize|20MB|单张图片/视频大小上限，超限记录并跳过。|

# 8\. 后期迁移：会话存档通道

## 8\.1 通道差异

|维度|原型：微信客服|正式：会话存档|
|---|---|---|
|用户标识|openid|external\_userid，需在绑定时建立与历史账号的映射。|
|消息获取|Webhook 事件推送 \+ 主动拉详情|服务商接口增量拉取（cursor 断点续拉），非实时推送。|
|媒体获取|客服媒体下载接口|存档媒体拉取接口，字段与鉴权方式不同。|
|会话形态|客服会话，不在通讯录|企微外部好友，支持长按直接转发。|

## 8\.2 迁移做法

1. 新增存档适配器：对接服务商增量拉取接口，输出同一套 StandardMsg。

2. 业务核心、MD 写入与目录规范保持不变。

3. 并行灰度：客服通道与好友通道同时运行，引导用户重新加好友并完成绑定，映射 openid 与 external\_userid。

4. 迁移完成后下线客服入口；合规上须告知用户会话被存档。

# 9\. 风险、限制与未决事项

- **公众号处理待定：**本期只存链接与卡片元信息；正文抓取、摘要、封面归档的方案见第 11 章。

- **抖音解析稳定性：**抖音页面存在反爬与链接时效问题，解析失败降级为链接留存，三级降级与可观测指标见第 12 章。

- **客服通道限制：**会话超时关闭、用户发一条后被动回复条数受限；正式体验需切换存档通道。

- **合规前提：**会话存档需企业认证、官方服务商与用户告知，不能以非官方协议监听私聊。

- **待确认：**是否需要多用户隔离；媒体是否后期转对象存储；文本条目是否需要标签/分组。

# 10\. 原型落地步骤

1. 企微后台开通微信客服，创建客服账号并配置回调 URL、Token、EncodingAESKey。

2. Go 项目初始化：HTTP 服务、配置读取、目录与日志。

3. 实现 GET 校验与报文加解密，跑通后台回调验证。

4. 实现客服消息拉取与标准化适配器，先打通文本消息落盘。

5. 补齐图片下载与嵌入、抖音链接解析、公众号链接留存。

6. 加入幂等去重、失败重试与基础监控日志。

7. 用客服二维码在微信侧转发各类内容，核对 MD 与资源目录产出。

# 11\. 公众号处理方案：从暂存链接到正文归档

第 9 章把公众号列为待定项。这里收口成一条可执行策略：**本期默认做「链接 \+ 元信息 \+ 封面」的稳定归档，正文抓取作为可开关的增强能力并行接入，抓取失败只降级、不阻断落盘。**

## 11\.1 三档策略对比

|档位|做什么|依赖与成本|风险|
|---|---|---|---|
|**L0**|只写标题 \+ 原文链接，丢了也就丢了。|无|日后无法检索正文，收藏价值最低。|
|**L1**|标题、摘要、原文链接、封面图落进当日 assets，正文不抓。|卡片自带字段，零外部请求|几乎无；封面图有防盗链时需要带 Referer。|
|**L2**|抓正文 HTML，清洗成 Markdown 写进当日 MD，正文图一并入库。|一次 HTTP 请求 \+ HTML 转 Markdown|有反爬与频控；页面结构变动会导致抓空；单篇耗时可到秒级。|

**决策：默认跑 L1，L2 挂在配置开关后。**理由很直接：公众号正文是可再生的公开内容，链接丢了才是永久损失；而正文抓取是唯一会引入外部不确定性的步骤，不能让它卡住主链路。

## 11\.2 正文抓取的实现要点

1. **取链接：**卡片的 url 字段或文本里的 `mp.weixin.qq.com` 链接，统一 http → https，去掉 `#rd` 之类尾部参数。

2. **带对头：**请求头用移动端 UA 加 Referer，直接裸 UA 拉取常被返回「环境异常」页。

3. **定位正文：**正文在 `div#js_content`；正文图片在 `data-src` 而不是 `src`（懒加载），只读 src 会抓到空图。

4. **清洗：**剔除 script、style、广告位、点赞引导、二维码段落；`span` 样式一律拍平，避免 Markdown 里出现大量行内 CSS。

5. **转换：**HTML 转 Markdown 用成熟库，不要手写正则；段落、小标题、列表、图片、引用各自映射。

6. **图片：**正文图下载到当日 assets，沿用同一套命名规则（见 13\.2），Markdown 里写相对路径。

7. **降级：**任何一步失败都退回 L1，并在 MD 的条目末尾补一行「正文抓取失败：原因」，不静默丢弃。

**频控与合规：**单篇抓取、间隔请求、不批量爬取历史文章列表。抓取结果只用于个人笔记归档，不做再分发；若遇到验证页或 302 到验证，直接降级，不要尝试绕过。

## 11\.3 落盘呈现

```markdown
## 09:25
### 微信公众平台：Go 里的并发模型

![封面](assets/2026-11-16-a1b2c3d4.jpg)

> 摘要：这篇文章把 channel 与 goroutine 的关系讲得很清楚。

- 原文链接：https://mp.weixin.qq.com/s/xxxx
- 正文：已归档（L2）
- 正文抓取时间：2026-11-16 09:26:10
```

L1 时最后两行不出现，只有封面、摘要与链接；正文段落按原文顺序追加在链接下方，图片紧跟所在段落。

## 11\.4 配置开关

|配置项|默认|说明|
|---|---|---|
|OfficialFetchEnabled|false|是否启用 L2 正文抓取；关闭时走 L1。|
|OfficialFetchTimeout|8s|单篇抓取超时，超时即降级，避免拖住队列。|
|OfficialFetchMaxImages|20|单篇正文图上限，超出只保留前 N 张并记备注。|
|OfficialFetchUA|移动端 UA|抓取请求头，可按需替换。|

# 12\. 抖音解析的稳定性方案

抖音是本期唯一需要「解析第三方页面」的消息类型，也是最容易在某天突然全挂的一环。原则是：**解析是增强，不是前置条件；任何失败都要退化成链接留存，且失败原因必须可统计。**

## 12\.1 解析链路

1. 从文本或卡片里匹配 `v.douyin.com` 短链、`www.douyin.com/video/{id}`、分享文案里的作品 ID。

2. 短链先跟一次 301/302 拿到带 `aweme_id` 的真实地址；只跟一跳，跳数超限即判失败。

3. 以作品 ID 请求详情，拿到标题、作者、时长、封面、播放地址。

4. 校验文件大小与时长，超限则只记元信息。

5. 下载到当日 assets，MD 里嵌入本地引用并保留原始链接。

## 12\.2 已知不稳定点

|现象|成因|对策|
|---|---|---|
|短链 404 或无跳转|分享链接过期、作品被删|判为终态失败，降级 L3，不再重试。|
|详情接口返回空或风控页|请求参数签名、IP 频控|限速 \+ 单实例低并发；仍失败则 L2 降级。|
|播放地址下载 403|直链有时效、需 Referer|取地址后立即下载，带 Referer；失败进重试队列。|
|文件过大|长视频、高清码率|超 MediaMaxSize 只记元信息与链接。|
|解析成功但内容错位|短链指向合集/直播|只接受视频作品类型，其余按 L3 处理。|

## 12\.3 三级降级

|级别|触发条件|落到 MD 的内容|是否重试|
|---|---|---|---|
|**L1**|解析与下载全部成功|本地视频引用 \+ 标题 \+ 作者 \+ 原始链接|否|
|**L2**|元信息拿到、视频下载失败|封面 \+ 标题 \+ 作者 \+ 原始链接 \+ 下载失败备注|进入重试队列，最多 3 次|
|**L3**|解析整体失败|原始链接 \+ 一行失败原因（不含报错堆栈）|否|

降级是**写进 MD 的可见结果**，不是藏在日志里。这样回看笔记时立刻知道哪条没抓全，而不是以为库里就是全部。

## 12\.4 缓存与幂等

- 解析结果按 `aweme_id` 缓存 24 小时，重复转发同一条不重新解析。

- 下载先落临时文件，算完内容 hash 再按规则重命名；目标已存在则直接复用并删除临时文件，不重复占空间。

- 同一 `msg_id` 在去重表里只处理一次，重复回调直接返回 200 不落盘。

## 12\.5 可观测指标

|指标|口径|用途|
|---|---|---|
|douyin\_resolve\_total|按结果分级计数（L1/L2/L3）|一眼看出今天解析成功率|
|douyin\_resolve\_latency|从识别到落盘的耗时分布|定位是哪一步在拖|
|douyin\_fail\_reason|失败原因分类计数|区分「链接过期」与「风控」|
|download\_bytes|按日累计下载体积|防止磁盘被视频吃满|

# 13\. 文件契约：spec/SPEC\.md

服务端与本地编辑器之间不共享代码，只共享 Vault 里的文件。**把契约写死在这一章，两边就能各自演进而不互相踩。**这份契约的读者是服务端实现者、编辑器作者，以及以后想换个采集通道的自己。

## 13\.1 目录契约

```text
Vault/
└── 2026-11-16/
    ├── 2026-11-16.md          # 当日唯一主文件
    └── assets/                # 当日资源，与 MD 同级
        ├── 2026-11-16-a1b2c3d4.jpg
        ├── 2026-11-16-c3d4e5f6.jpg
        └── 2026-11-16-9f8e7d6c.mp4
```

- 日期目录名与 MD 文件名都用 `YYYY-MM-DD`，不用中文、不含时分秒。

- 每天恰好一个 MD、一个 assets 目录；assets 只存媒体，不存中间产物。

- MD 内所有资源引用一律相对路径 `assets/...`，不写绝对路径、不写远程 URL 作为唯一引用。

## 13\.2 命名规则

|对象|规则|示例|
|---|---|---|
|主文件|`{date}.md`，一天一个，文件名即日期，不加序号|2026\-11\-16\.md|
|资源文件|`{date}-{hash8}.{ext}`|2026\-11\-16\-a1b2c3d4\.jpg|
|临时文件|`.{target}.tmp-{pid}`，rename 后消失|\.2026\-11\-16\.md\.tmp\-4821|

日期前缀让资源归属当日目录、一眼可见；hash 前缀保证不同内容不撞名、相同内容天然复用同一份文件；扩展名保留原始类型。**不追加语义名**——用途由 MD 里所在条目的上下文说明，文件名只负责唯一性与归属。

**hash8 怎么取：**对资源内容做 SHA\-256，取十六进制小写前 8 位；内容尚未落地时退化为 `msg_id` 的 SHA\-256 前 8 位。8 位 hex 约 43 亿种取值，当日几十到几百个资源的量级下碰撞可忽略；若同名文件已存在但内容不同，把 hash 扩到 12 位重命名，不做覆盖。

## 13\.3 写入原子性与并发

1. **进程内：**以文件路径为 key 的互斥锁（keyed mutex），同日写入串行，读完内容再整篇重写。

2. **原子替换：**先写同目录临时文件，`fsync` 后 `rename` 覆盖目标，避免写到一半断电留下半截文件。

3. **追加语义：**不加锁直接 `O_APPEND` 也不安全——我们要在文件头维护日期标题、在末尾追加条目，所以走「读—改—写」整篇路径。

4. **跨进程：**服务端与本地编辑器不在同一台机器，无法共享锁；靠 13\.5 的冲突检测兜底。

## 13\.4 幂等

- 主键是通道消息 ID（原型为 `msg_id`，存档模式为服务商消息 ID）。

- 去重窗口 7 天，写入即登记；命中重复直接返回 200，不产生任何文件改动。

- 去重记录不放在 Vault 里（避免污染笔记），放本地 SQLite，见第 15 章。

## 13\.5 冲突副本与自愈

坚果云和本地编辑器同时改同一文件时，会出现 `2026-11-16 (冲突副本).md` 之类的副本。处理策略：

- **检测：**同步完成后扫描当日目录，匹配冲突副本命名模式。

- **合并：**以服务端主文件为基准，把副本里「本地新增的条目段」按时间戳拼回主文件，不整篇覆盖。

- **归档：**合并成功后把副本移到 `.conflicts/` 保留 30 天，不做删除。

- **兜底：**无法判定归属时保留两份并告警，交人工处理——宁可留冗余，不可丢内容。

## 13\.6 fixtures 与契约测试

契约不靠文档约束，靠夹具校验。接入层的每个通道都要能跑同一批 fixtures：

```json
{
  "name": "客服通道-纯文本",
  "raw": {
    "msgtype": "text",
    "msgid": "abcd1234ef567890",
    "text": { "content": "今天看的这篇讲 Go 适配器分层" },
    "send_time": "2026-11-16T09:12:00+08:00",
    "openid": "oXXXX"
  },
  "want": {
    "msg_type": "text",
    "text": "今天看的这篇讲 Go 适配器分层",
    "date": "2026-11-16",
    "file": "2026-11-16.md"
  }
}
```

同一份 fixtures 用三处复用：适配器单测（raw → 标准化）、写盘测试（标准化 → MD 变更）、通道迁移验收（存档通道输出必须与原通道同构）。换通道时不用重写测试，只换 raw。

## 13\.7 契约版本

- `spec/SPEC.md` 首行声明 `contract: 1`，规则变更必须递增版本。

- 服务端启动时校验 RootDir 结构，发现历史目录不符合当版命名时打印告警，不擅自改名。

- 编辑器侧只依赖本契约的目录、命名、相对路径三条，不依赖 MD 内部排版细节。

# 14\. 接口与字段设计

## 14\.1 Webhook 入口

**GET 校验参数**（后台保存回调地址时触发）：

|字段|类型|说明|
|---|---|---|
|msg\_signature|string|签名，参与校验|
|timestamp|string|秒级时间戳|
|nonce|string|随机串|
|echostr|string|加密串，校验通过后原样返回|

**POST 外层报文**（解密前）：

|字段|位置|必填|说明|
|---|---|---|---|
|ToUserName|解密后 XML|是|企业标识，用于校验 CorpID|
|FromUserName|解密后 XML|是|发送方标识|
|CreateTime|解密后 XML|是|秒级时间戳，落盘时转本地时区|
|MsgType|解密后 XML|是|event / text / image / link 等|
|Event|解密后 XML|条件|MsgType=event 时必填，如 kf\_msg\_or\_event|
|Token|事件体|条件|事件拉取凭证，换取完整消息|
|OpenKfId|事件体|条件|客服账号标识|

## 14\.2 字段映射

客服消息体到标准模型（第 5\.3 节结构体）的映射关系：

|通道字段|标准字段|转换规则|
|---|---|---|
|msgid|MsgID|原样，作为幂等主键|
|openid / external\_userid|SenderID|原样；两通道靠绑定表映射|
|send\_time（秒）|SendTime|转 `Asia/Shanghai`，落盘用本地日期|
|msgtype|MsgType|text/image/voice/file → 标准类型；link/mpnews 按 URL 再分类|
|text\.content|Text|去首尾空白，保留内部换行|
|image\.media\_id|MediaID|原样，下载用|
|link\.title / mpnews\.title|Title|缺省时用 URL 主机名兜底|
|link\.url|URL|规范化：补协议、去追踪参数|
|link\.desc / mpnews\.digest|Desc|截断到 200 字|
|link\.thumb\_media\_id|Cover|下载后替换为本地相对路径|

**分类优先级：**先看 URL 主机——抖音域名进抖音处理器，`mp.weixin.qq.com` 进公众号处理器，其余按普通链接；再看消息类型。这样卡片与纯文本链接走同一套判断，不会因为用户换了转发方式就漏处理。

## 14\.3 服务内部接口

```go
package core

import (
	"context"
	"time"

	"collector/internal/model"
)

// Channel 接入通道：原型为微信客服，后期为会话存档
type Channel interface {
	Name() string
	// Verify 处理回调校验请求，返回待响应体
	Verify(ctx context.Context, q map[string]string) (body string, err error)
	// Receive 接收回调报文，输出标准化消息（可为多条）
	Receive(ctx context.Context, raw []byte) ([]model.StandardMsg, error)
	// FetchMedia 下载通道媒体到目标路径
	FetchMedia(ctx context.Context, mediaID, dst string) error
}

// Resolver 链接解析：抖音、公众号正文等外部增强
type Resolver interface {
	Match(url string) bool
	Resolve(ctx context.Context, msg *model.StandardMsg, assetsDir string) (Result, error)
}

// Result 解析结果，Level 决定 MD 呈现方式
type Result struct {
	Level     int       // 1 完整 2 部分 3 降级
	LocalPath string    // 本地资源相对路径
	Note      string    // 写入 MD 的备注
}

// Store 幂等与任务状态
type Store interface {
	Seen(msgID string) (bool, error)
	Mark(msgID string, state State, err error) error
	Enqueue(msgID string, retryAt time.Time) error
	DueTasks(now time.Time, limit int) ([]Task, error)
}

// Writer 按日期主文件的追加写入
type Writer interface {
	AppendText(dir, date string, msg *model.StandardMsg) error
	AppendImage(dir, date string, msg *model.StandardMsg) error
	AppendVideo(dir, date string, msg *model.StandardMsg, r Result) error
	AppendLink(dir, date string, msg *model.StandardMsg, r Result) error
}
```

四个接口对应四层职责：通道、解析、状态、写盘。任一实现可替换而不动其他——这也是后期换通道不用重做业务的根本原因。

## 14\.4 运维接口

|路径|方法|用途|
|---|---|---|
|/webhook/kf|GET / POST|通道回调，仅此路径对外|
|/healthz|GET|存活探针，检查磁盘可写与 DB 可读|
|/metrics|GET|Prometheus 指标，仅内网|
|/admin/replay/\{msg\_id\}|POST|重放失败消息，需固定 token|

## 14\.5 错误码

|错误码|HTTP|含义与动作|
|---|---|---|
|E\_SIGN|403|签名校验失败，直接拒绝，不重试|
|E\_DECRYPT|400|解密失败，记原始报文摘要|
|E\_CORP|403|企业标识不符，拒绝|
|E\_ADAPT|500|标准化失败，进重试队列|
|E\_DUP|200|重复消息，不落盘，正常返回|
|E\_WRITE|500|落盘失败（磁盘/锁），进重试队列|

**响应策略：**回调必须在 3 秒内返回 200，重活（下载、解析）投递到队列异步做，不阻塞回调线程，避免企微判超时重推。

# 15\. 数据与状态设计

## 15\.1 为什么用 SQLite 而不是纯文件

去重记录、重试队列、解析缓存都需要「按主键查 \+ 事务写」，落成 JSON 文件会带来并发写坏文件的风险。**用纯 Go 的 SQLite 驱动（无需 CGO），单文件、零部署成本**，放在数据目录而不是 Vault 内，避免被同步到坚果云。

## 15\.2 表结构

```sql
CREATE TABLE messages (
  msg_id      TEXT PRIMARY KEY,   -- 通道消息唯一标识
  channel     TEXT NOT NULL,      -- kf / archive
  sender_id   TEXT NOT NULL,
  msg_type    TEXT NOT NULL,
  send_time   DATETIME NOT NULL,
  state       TEXT NOT NULL,      -- received/fetched/parsed/written/synced/failed
  level       INTEGER NOT NULL DEFAULT 0,
  note        TEXT,
  retry_count INTEGER NOT NULL DEFAULT 0,
  retry_at    DATETIME,
  created_at  DATETIME NOT NULL,
  updated_at  DATETIME NOT NULL
);

CREATE INDEX idx_messages_due ON messages(state, retry_at);

CREATE TABLE resolve_cache (
  cache_key   TEXT PRIMARY KEY,   -- 如 aweme:12345
  payload     TEXT NOT NULL,
  expires_at  DATETIME NOT NULL
);

CREATE TABLE official_fetch (
  url_hash    TEXT PRIMARY KEY,
  url         TEXT NOT NULL,
  status      TEXT NOT NULL,      -- ok / blocked / failed
  fetched_at  DATETIME
);
```

`messages` 一张表同时承担幂等、状态追踪与重试调度；`resolve_cache` 削掉重复解析；`official_fetch` 记住哪些公众号抓取过、哪些被拦，避免反复撞墙。

## 15\.3 状态机

|状态|进入条件|后继|
|---|---|---|
|received|回调报文解密并标准化成功|fetched / failed|
|fetched|媒体或详情拉到本地暂存|parsed|
|parsed|解析完成，得到 Result 与级别|written|
|written|MD 已原子替换写入|synced|
|synced|WebDAV 同步成功（或本地模式跳过）|终态|
|failed|任一步失败且可重试|retry\_at 到期回到 received|

重试上限 3 次、指数退避（1 分钟 → 5 分钟 → 30 分钟）；超限落 `failed` 终态并告警。终态不删记录，方便事后重放。

## 15\.4 目录与状态的关系

文件是最终事实，DB 只是过程记录。**所以 DB 丢了不影响已落盘的笔记，重建幂等记录最多导致少量重复条目。**反过来，任何「先写 DB 再写文件」的顺序都要保证文件写成功才算 written，避免 DB 说有、文件里没有。

# 16\. 服务端代码骨架

## 16\.1 目录树

```text
onebox-collector/
├── cmd/
│   └── collector/
│       └── main.go            # 装配与启动
├── internal/
│   ├── channel/
│   │   └── wecomkf/           # 微信客服通道：校验、解密、拉取
│   ├── core/                   # 接口与业务编排
│   ├── resolver/
│   │   ├── douyin/            # 抖音解析
│   │   └── official/          # 公众号 L1/L2
│   ├── store/                  # SQLite 实现
│   ├── vault/                  # 目录、命名、MD 写入、原子替换
│   ├── sync/                   # WebDAV 同步与冲突检测
│   └── config/
├── spec/
│   ├── SPEC.md                # 第 13 章内容，contract: 1
│   └── fixtures/              # 契约夹具
├── deploy/
│   ├── docker-compose.yml
│   └── Caddyfile
└── docs/
    └── 技术方案.md            # 本方案的仓库版
```

## 16\.2 启动装配

```go
func main() {
	cfg := config.Load()
	db := store.Open(cfg.DataDir)
	w := vault.NewWriter(cfg.RootDir, vault.WithMutex(vault.NewKeyedMutex()))
	ch := wecomkf.New(cfg.CorpID, cfg.Token, cfg.AESKey, cfg.Secret)

	c := &amp;core.Core{
		Channel:   ch,
		Store:     db,
		Writer:    w,
		Resolvers: []core.Resolver{douyin.New(cfg), official.New(cfg)},
		Sync:      sync.NewWebDAV(cfg.DavURL, cfg.DavUser, cfg.DavPass),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/webhook/kf", c.Webhook)
	mux.HandleFunc("/healthz", c.Health)
	mux.Handle("/metrics", promhttp.Handler())

	go c.Work()  // 重试与降级任务循环，与回调线程分离
	http.ListenAndServe(cfg.Listen, mux)
}
```

## 16\.3 回调时延预算

|步骤|预算|超限处理|
|---|---|---|
|签名 \+ 解密|10ms|失败直接拒绝|
|标准化 \+ 幂等登记|20ms|失败返回 500，靠重推|
|回调响应|总计 3s 内|其余全部转异步队列|
|下载 / 解析 / 写盘|异步，不占回调预算|按 15\.3 重试|

这一条是整份方案的关键约束：**企微对回调响应有超时要求，把下载和解析放进回调路径，等于把第三方站点的抖动直接转嫁成消息丢失。**

# 17\. 测试与验收

## 17\.1 分层测试

|层级|覆盖内容|依赖|
|---|---|---|
|单元测试|签名算法、AES 解密、URL 分类、命名生成、Markdown 拼装|无外部依赖，自带向量|
|契约测试|spec/fixtures 全部用例跑通，raw → 标准化 → MD 期望|本地临时目录|
|并发测试|同日 50 条并发写入，条目数正确、无覆盖、无临时文件残留|本地临时目录|
|集成测试|模拟通道回调 → SQLite → 写盘 → 假 WebDAV 全链路|httptest 假服务|
|外部依赖测试|抖音、公众号走录制回放，不在 CI 里打真实网络|golden 响应文件|

## 17\.2 必须写死的断言

- 同一条 `msg_id` 推两次，当日 MD 只多一个条目。

- 解密失败的报文不产生任何文件，只留错误日志。

- 解析失败的消息一定在 MD 里留下链接，不允许出现「什么都没写」。

- 写入过程中断（模拟 rename 前崩溃）后，重启能自愈，不丢当日已有条目。

- 冲突副本合并后，主文件条目数等于两边条目去重后的并集。

## 17\.3 端到端验收清单

1. 企微后台保存回调地址，GET 校验通过并返回 echostr。

2. 微信侧发一条纯文字，当日 MD 出现带时间的条目。

3. 连发三条文字，按时间顺序追加，不新建文件。

4. 发一张图，assets 出现文件，MD 里相对路径能正常渲染。

5. 转发抖音视频，L1 成功时 MD 引用本地视频；断网重试时降级为链接且带原因。

6. 转发公众号文章，L1 出现标题、摘要、封面、链接；开 L2 后正文段落写进 MD。

7. 转发一条无法识别的链接，落到兜底分支并保留原文。

8. 同一内容连发两次，第二次不产生新条目。

9. Vault 同步到坚果云后，在本地编辑器打开排版正常、图片可显示。

10. 服务重启后继续追加到同一当日文件，不覆盖历史。

## 17\.4 验收标准

|指标|目标|测法|
|---|---|---|
|回调响应时延 P99|\< 3s|压测 50 QPS，看 P99|
|文本落盘成功率|100%|人工发 30 条全对账|
|媒体落盘成功率|≥ 95%|30 张图 \+ 10 条视频|
|抖音解析 L1 占比|≥ 70%|连续 3 天抽样 20 条|
|重复写入|0 条|全量扫描当日 MD 比对 msg\_id|

# 18\. 部署与运维

## 18\.1 拓扑

1. 单台小规格云主机跑容器，Go 服务 \+ SQLite 单文件，无状态外置依赖。

2. Caddy 做 HTTPS 终止与固定域名，仅暴露 `/webhook/kf`，其它路径限内网。

3. 数据目录挂卷，与 Vault 目录分开：数据目录是本机的，Vault 是待同步的。

4. 出口固定 IP 并登记到企微可信 IP 列表。

## 18\.2 环境变量

|变量|说明|敏感|
|---|---|---|
|WECOM\_CORP\_ID|企业标识，解密后校验|否|
|WECOM\_TOKEN|回调签名 Token|是|
|WECOM\_AES\_KEY|43 位 EncodingAESKey|是|
|WECOM\_SECRET|拉取消息用的 Secret|是|
|VAULT\_ROOT|落盘根目录|否|
|DAV\_URL / DAV\_USER / DAV\_PASS|坚果云 WebDAV 同步|是|
|OFFICIAL\_FETCH|公众号正文抓取开关|否|

密钥只走环境变量或只读挂载文件，不写进镜像、不进仓库。

## 18\.3 坚果云同步

- 优先用 WebDAV 直接 `PUT` 当日 MD 与新增 assets，只传变更，不做全量镜像。

- 同步失败重试 3 次后留给下一轮，不让同步卡住写入。

- 同步完成后拉一次当日目录列表，用于 13\.5 的冲突副本检测。

- 若用 rclone 而非自研，必须禁用 `--delete` 之类的破坏性参数。

## 18\.4 日志与监控

|类别|必记字段|告警条件|
|---|---|---|
|回调日志|msg\_id、事件类型、耗时、结果码|连续 5 分钟 E\_SIGN / E\_DECRYPT|
|落盘日志|日期、文件、条目类型、级别|E\_WRITE 出现即告警|
|解析日志|平台、结果级别、失败原因分类|L3 占比超 30%|
|磁盘|剩余空间、当日资源体积|剩余低于 10%|

## 18\.5 备份与恢复

- SQLite 每日快照一次，保留 7 天；Vault 本身靠坚果云版本历史兜底。

- 恢复顺序：先恢复 Vault 文件，再用 DB 快照恢复幂等记录；DB 缺失时按「文件为准」重建。

- 不做自动删除，磁盘满时先告警并停写媒体，文本落盘保持可用。

# 19\. 里程碑与排期

|阶段|交付内容|完成判据|
|---|---|---|
|**M0**|契约先行：SPEC\.md、fixtures、目录与命名规则落地|契约测试可跑，命名断言全绿|
|**M1**|通道打通：校验、解密、拉取、文本落盘|微信侧发文字，当日 MD 正确出现|
|**M2**|媒体与解析：图片、抖音三级降级、公众号 L1|验收清单 1–7 条全过|
|**M3**|稳态：幂等、重试、WebDAV 同步、冲突自愈、监控|连续 3 天真实使用无丢条、无冲突副本|
|**M4**|通道升级：会话存档适配器接入与灰度|两通道并行同构产出|

每个阶段的出口都是「可验证的现象」，而不是「代码写完」。M0 先落契约，是因为后面所有实现都以契约为准，契约改一次，测试和落盘全要跟着改。

> （注：部分内容由豆包工作 AI 生成）
