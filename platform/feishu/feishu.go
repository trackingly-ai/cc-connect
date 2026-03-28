package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

func init() {
	core.RegisterPlatform("feishu", New)
}

type replyContext struct {
	messageID string
	chatID    string
}

type quotedMessage struct {
	messageID string
	userID    string
	userName  string
	content   string
}

type Platform struct {
	appID                 string
	appSecret             string
	reactionEmoji         string
	progressStyle         string
	allowFrom             string
	groupReplyAll         bool
	shareSessionInChannel bool
	replyInThread         bool
	client                *lark.Client
	wsClient              *larkws.Client
	handler               core.MessageHandler
	cardNavHandler        core.CardNavigationHandler
	cancel                context.CancelFunc
	dedup                 core.MessageDedup
	botOpenID             string
}

func New(opts map[string]any) (core.Platform, error) {
	appID, _ := opts["app_id"].(string)
	appSecret, _ := opts["app_secret"].(string)
	if appID == "" || appSecret == "" {
		return nil, fmt.Errorf("feishu: app_id and app_secret are required")
	}
	reactionEmoji, _ := opts["reaction_emoji"].(string)
	if reactionEmoji == "" {
		reactionEmoji = "OnIt"
	}
	if v, ok := opts["reaction_emoji"].(string); ok && v == "none" {
		reactionEmoji = ""
	}
	allowFrom, _ := opts["allow_from"].(string)
	groupReplyAll, _ := opts["group_reply_all"].(bool)
	shareSessionInChannel, _ := opts["share_session_in_channel"].(bool)
	replyInThread, _ := opts["reply_in_thread"].(bool)
	progressStyle := "legacy"
	if v, ok := opts["progress_style"].(string); ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "", "legacy":
			progressStyle = "legacy"
		case "compact", "card":
			progressStyle = strings.ToLower(strings.TrimSpace(v))
		default:
			return nil, fmt.Errorf("feishu: invalid progress_style %q (want legacy, compact, or card)", v)
		}
	}

	return &Platform{
		appID:                 appID,
		appSecret:             appSecret,
		reactionEmoji:         reactionEmoji,
		progressStyle:         progressStyle,
		allowFrom:             allowFrom,
		groupReplyAll:         groupReplyAll,
		shareSessionInChannel: shareSessionInChannel,
		replyInThread:         replyInThread,
		client:                lark.NewClient(appID, appSecret),
	}, nil
}

func (p *Platform) Name() string { return "feishu" }

func (p *Platform) ProgressStyle() string { return p.progressStyle }

func (p *Platform) SetCardNavigationHandler(fn core.CardNavigationHandler) {
	p.cardNavHandler = fn
}

func (p *Platform) Start(handler core.MessageHandler) error {
	p.handler = handler

	if openID, err := p.fetchBotOpenID(); err != nil {
		slog.Warn("feishu: failed to get bot open_id, group chat filtering disabled", "error", err)
	} else {
		p.botOpenID = openID
		slog.Info("feishu: bot identified", "open_id", openID)
	}

	eventHandler := dispatcher.NewEventDispatcher("", "").
		OnP2MessageReceiveV1(func(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
			slog.Debug("feishu: message received", "app_id", p.appID)
			return p.onMessage(event)
		}).
		OnP2MessageReadV1(func(ctx context.Context, event *larkim.P2MessageReadV1) error {
			return nil // ignore read receipts
		}).
		OnP2ChatAccessEventBotP2pChatEnteredV1(func(ctx context.Context, event *larkim.P2ChatAccessEventBotP2pChatEnteredV1) error {
			slog.Debug("feishu: user opened bot chat", "app_id", p.appID)
			return nil
		}).
		OnP1P2PChatCreatedV1(func(ctx context.Context, event *larkim.P1P2PChatCreatedV1) error {
			slog.Debug("feishu: p2p chat created", "app_id", p.appID)
			return nil
		}).
		OnP2MessageReactionCreatedV1(func(ctx context.Context, event *larkim.P2MessageReactionCreatedV1) error {
			return nil // ignore reaction events (triggered by our own addReaction)
		}).
		OnP2MessageReactionDeletedV1(func(ctx context.Context, event *larkim.P2MessageReactionDeletedV1) error {
			return nil // ignore reaction removal events (triggered by our own removeReaction)
		}).
		OnP2CardActionTrigger(func(ctx context.Context, event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
			slog.Debug("feishu: card action trigger received", "app_id", p.appID)
			return p.onCardAction(event)
		})

	p.wsClient = larkws.NewClient(p.appID, p.appSecret,
		larkws.WithEventHandler(eventHandler),
		larkws.WithLogLevel(larkcore.LogLevelInfo),
	)

	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel

	go func() {
		if err := p.wsClient.Start(ctx); err != nil {
			slog.Error("feishu: websocket error", "error", err)
		}
	}()

	return nil
}

func (p *Platform) addReaction(messageID string) string {
	if p.reactionEmoji == "" {
		return ""
	}
	emojiType := p.reactionEmoji
	resp, err := p.client.Im.MessageReaction.Create(context.Background(),
		larkim.NewCreateMessageReactionReqBuilder().
			MessageId(messageID).
			Body(larkim.NewCreateMessageReactionReqBodyBuilder().
				ReactionType(&larkim.Emoji{EmojiType: &emojiType}).
				Build()).
			Build())
	if err != nil {
		slog.Debug("feishu: add reaction failed", "error", err)
		return ""
	}
	if !resp.Success() {
		slog.Debug("feishu: add reaction failed", "code", resp.Code, "msg", resp.Msg)
		return ""
	}
	if resp.Data != nil && resp.Data.ReactionId != nil {
		return *resp.Data.ReactionId
	}
	return ""
}

func (p *Platform) removeReaction(messageID, reactionID string) {
	if reactionID == "" || messageID == "" {
		return
	}
	resp, err := p.client.Im.MessageReaction.Delete(context.Background(),
		larkim.NewDeleteMessageReactionReqBuilder().
			MessageId(messageID).
			ReactionId(reactionID).
			Build())
	if err != nil {
		slog.Debug("feishu: remove reaction failed", "error", err)
		return
	}
	if !resp.Success() {
		slog.Debug("feishu: remove reaction failed", "code", resp.Code, "msg", resp.Msg)
	}
}

func applyQuotedMessage(m *core.Message, quoted *quotedMessage) *core.Message {
	if m == nil || quoted == nil {
		return m
	}
	m.QuotedMessageID = quoted.messageID
	m.QuotedUserID = quoted.userID
	m.QuotedUserName = quoted.userName
	m.QuotedContent = quoted.content
	return m
}

// StartTyping adds an emoji reaction to the user's message and returns a stop
// function that removes the reaction when processing is complete.
func (p *Platform) StartTyping(ctx context.Context, rctx any) (stop func()) {
	rc, ok := rctx.(replyContext)
	if !ok || rc.messageID == "" {
		return func() {}
	}
	reactionID := p.addReaction(rc.messageID)
	return func() {
		go p.removeReaction(rc.messageID, reactionID)
	}
}

func (p *Platform) onMessage(event *larkim.P2MessageReceiveV1) error {
	msg := event.Event.Message
	sender := event.Event.Sender

	msgType := ""
	if msg.MessageType != nil {
		msgType = *msg.MessageType
	}

	chatID := ""
	if msg.ChatId != nil {
		chatID = *msg.ChatId
	}
	userID := ""
	userName := ""
	if sender.SenderId != nil && sender.SenderId.OpenId != nil {
		userID = *sender.SenderId.OpenId
	}
	if sender.SenderType != nil {
		userName = *sender.SenderType
	}

	messageID := ""
	if msg.MessageId != nil {
		messageID = *msg.MessageId
	}

	if p.dedup.IsDuplicate(messageID) {
		slog.Debug("feishu: duplicate message ignored", "message_id", messageID)
		return nil
	}

	if msg.CreateTime != nil {
		if ms, err := strconv.ParseInt(*msg.CreateTime, 10, 64); err == nil {
			msgTime := time.Unix(ms/1000, (ms%1000)*int64(time.Millisecond))
			if core.IsOldMessage(msgTime) {
				slog.Debug("feishu: ignoring old message after restart", "create_time", *msg.CreateTime)
				return nil
			}
		}
	}

	chatType := ""
	if msg.ChatType != nil {
		chatType = *msg.ChatType
	}

	if chatType == "group" && !p.groupReplyAll && p.botOpenID != "" {
		if !isBotMentioned(msg.Mentions, p.botOpenID) {
			slog.Debug("feishu: ignoring group message without bot mention", "chat_id", chatID)
			return nil
		}
	}

	if !core.AllowList(p.allowFrom, userID) {
		slog.Debug("feishu: message from unauthorized user", "user", userID)
		return nil
	}

	if msg.Content == nil {
		slog.Debug("feishu: message content is nil", "message_id", messageID, "type", msgType)
		return nil
	}

	var sessionKey string
	if p.shareSessionInChannel {
		sessionKey = fmt.Sprintf("feishu:%s", chatID)
	} else {
		sessionKey = fmt.Sprintf("feishu:%s:%s", chatID, userID)
	}
	rctx := replyContext{messageID: messageID, chatID: chatID}
	quoted := p.fetchQuotedMessage(msg.ParentId, msg.RootId)

	switch msgType {
	case "text":
		var textBody struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(*msg.Content), &textBody); err != nil {
			slog.Error("feishu: failed to parse text content", "error", err)
			return nil
		}
		text := stripMentions(textBody.Text, msg.Mentions)
		if text == "" {
			return nil
		}
		p.handler(p, applyQuotedMessage(&core.Message{
			SessionKey: sessionKey, Platform: "feishu",
			MessageID: messageID,
			UserID:    userID, UserName: userName,
			Content: text, ReplyCtx: rctx,
		}, quoted))

	case "image":
		var imgBody struct {
			ImageKey string `json:"image_key"`
		}
		if err := json.Unmarshal([]byte(*msg.Content), &imgBody); err != nil {
			slog.Error("feishu: failed to parse image content", "error", err)
			return nil
		}
		imgData, mimeType, err := p.downloadImage(messageID, imgBody.ImageKey)
		if err != nil {
			slog.Error("feishu: download image failed", "error", err)
			return nil
		}
		p.handler(p, applyQuotedMessage(&core.Message{
			SessionKey: sessionKey, Platform: "feishu",
			MessageID: messageID,
			UserID:    userID, UserName: userName,
			Images:   []core.ImageAttachment{{MimeType: mimeType, Data: imgData}},
			ReplyCtx: rctx,
		}, quoted))

	case "audio":
		var audioBody struct {
			FileKey  string `json:"file_key"`
			Duration int    `json:"duration"` // milliseconds
		}
		if err := json.Unmarshal([]byte(*msg.Content), &audioBody); err != nil {
			slog.Error("feishu: failed to parse audio content", "error", err)
			return nil
		}
		slog.Debug("feishu: audio received", "user", userID, "file_key", audioBody.FileKey)
		audioData, err := p.downloadResource(messageID, audioBody.FileKey, "file")
		if err != nil {
			slog.Error("feishu: download audio failed", "error", err)
			return nil
		}
		p.handler(p, applyQuotedMessage(&core.Message{
			SessionKey: sessionKey, Platform: "feishu",
			MessageID: messageID,
			UserID:    userID, UserName: userName,
			Audio: &core.AudioAttachment{
				MimeType: "audio/opus",
				Data:     audioData,
				Format:   "ogg",
				Duration: audioBody.Duration / 1000,
			},
			ReplyCtx: rctx,
		}, quoted))

	case "file":
		var fileBody struct {
			FileKey  string `json:"file_key"`
			FileName string `json:"file_name"`
		}
		if err := json.Unmarshal([]byte(*msg.Content), &fileBody); err != nil {
			slog.Error("feishu: failed to parse file content", "error", err)
			return nil
		}
		fileData, err := p.downloadResource(messageID, fileBody.FileKey, "file")
		if err != nil {
			slog.Error("feishu: download file failed", "error", err)
			return nil
		}
		p.handler(p, applyQuotedMessage(&core.Message{
			SessionKey: sessionKey, Platform: "feishu",
			MessageID: messageID,
			UserID:    userID, UserName: userName,
			Files: []core.FileAttachment{{
				MimeType: detectMimeType(fileData),
				Data:     fileData,
				FileName: fileBody.FileName,
			}},
			ReplyCtx: rctx,
		}, quoted))

	case "media":
		var mediaBody struct {
			FileKey  string `json:"file_key"`
			ImageKey string `json:"image_key"`
		}
		if err := json.Unmarshal([]byte(*msg.Content), &mediaBody); err != nil {
			slog.Error("feishu: failed to parse media content", "error", err)
			return nil
		}
		if mediaBody.FileKey == "" {
			slog.Debug("feishu: media message missing file_key", "message_id", messageID)
			return nil
		}
		fileData, err := p.downloadResource(messageID, mediaBody.FileKey, "media")
		if err != nil {
			slog.Error("feishu: download media failed", "error", err)
			return nil
		}
		mimeType := detectMimeType(fileData)
		p.handler(p, applyQuotedMessage(&core.Message{
			SessionKey: sessionKey, Platform: "feishu",
			MessageID: messageID,
			UserID:    userID, UserName: userName,
			Files: []core.FileAttachment{{
				MimeType: mimeType,
				Data:     fileData,
				FileName: defaultFileNameForMime("media", mimeType),
			}},
			ReplyCtx: rctx,
		}, quoted))

	case "post":
		textParts, images := p.parsePostContent(messageID, *msg.Content)
		text := stripMentions(strings.Join(textParts, "\n"), msg.Mentions)
		if text == "" && len(images) == 0 {
			return nil
		}
		p.handler(p, applyQuotedMessage(&core.Message{
			SessionKey: sessionKey, Platform: "feishu",
			MessageID: messageID,
			UserID:    userID, UserName: userName,
			Content: text, Images: images,
			ReplyCtx: rctx,
		}, quoted))

	default:
		slog.Debug("feishu: ignoring unsupported message type", "type", msgType)
	}

	return nil
}

func (p *Platform) Reply(ctx context.Context, rctx any, content string) error {
	rc, ok := rctx.(replyContext)
	if !ok {
		return fmt.Errorf("feishu: invalid reply context type %T", rctx)
	}

	msgType, msgBody := buildReplyContent(content)

	resp, err := p.client.Im.Message.Reply(ctx, larkim.NewReplyMessageReqBuilder().
		MessageId(rc.messageID).
		Body(larkim.NewReplyMessageReqBodyBuilder().
			MsgType(msgType).
			Content(msgBody).
			Build()).
		Build())
	if err != nil {
		return fmt.Errorf("feishu: reply api call: %w", err)
	}
	if !resp.Success() {
		return fmt.Errorf("feishu: reply failed code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

// Send sends a new message to the same chat (not a reply to original message).
// When reply_in_thread is enabled, threads the message to the original message instead.
func (p *Platform) Send(ctx context.Context, rctx any, content string) error {
	rc, ok := rctx.(replyContext)
	if !ok {
		return fmt.Errorf("feishu: invalid reply context type %T", rctx)
	}

	if p.replyInThread && rc.messageID != "" {
		return p.Reply(ctx, rctx, content)
	}

	if rc.chatID == "" {
		return fmt.Errorf("feishu: chatID is empty, cannot send new message")
	}

	msgType, msgBody := buildReplyContent(content)

	// Send a new message to the chat (not a reply)
	resp, err := p.client.Im.Message.Create(ctx, larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(larkim.ReceiveIdTypeChatId).
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(rc.chatID).
			MsgType(msgType).
			Content(msgBody).
			Build()).
		Build())
	if err != nil {
		return fmt.Errorf("feishu: send api call: %w", err)
	}
	if !resp.Success() {
		return fmt.Errorf("feishu: send failed code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

// downloadImage fetches an image from Feishu by message_id and image_key.
func (p *Platform) downloadImage(messageID, imageKey string) ([]byte, string, error) {
	resp, err := p.client.Im.MessageResource.Get(context.Background(),
		larkim.NewGetMessageResourceReqBuilder().
			MessageId(messageID).
			FileKey(imageKey).
			Type("image").
			Build())
	if err != nil {
		return nil, "", fmt.Errorf("feishu: image API: %w", err)
	}
	if !resp.Success() {
		return nil, "", fmt.Errorf("feishu: image API code=%d msg=%s", resp.Code, resp.Msg)
	}
	data, err := io.ReadAll(resp.File)
	if err != nil {
		return nil, "", fmt.Errorf("feishu: read image: %w", err)
	}

	mimeType := detectMimeType(data)
	slog.Debug("feishu: downloaded image", "key", imageKey, "size", len(data), "mime", mimeType)
	return data, mimeType, nil
}

// downloadResource fetches a file resource (audio, etc.) from Feishu by message_id and file_key.
func (p *Platform) downloadResource(messageID, fileKey, resType string) ([]byte, error) {
	resp, err := p.client.Im.MessageResource.Get(context.Background(),
		larkim.NewGetMessageResourceReqBuilder().
			MessageId(messageID).
			FileKey(fileKey).
			Type(resType).
			Build())
	if err != nil {
		return nil, fmt.Errorf("feishu: resource API: %w", err)
	}
	if !resp.Success() {
		return nil, fmt.Errorf("feishu: resource API code=%d msg=%s", resp.Code, resp.Msg)
	}
	data, err := io.ReadAll(resp.File)
	if err != nil {
		return nil, fmt.Errorf("feishu: read resource: %w", err)
	}
	slog.Debug("feishu: downloaded resource", "key", fileKey, "type", resType, "size", len(data))
	return data, nil
}

func detectMimeType(data []byte) string {
	if len(data) == 0 {
		return "application/octet-stream"
	}
	if mime := http.DetectContentType(data); mime != "" {
		return mime
	}
	return "application/octet-stream"
}

func defaultFileNameForMime(prefix, mime string) string {
	switch mime {
	case "video/mp4":
		return prefix + ".mp4"
	case "video/quicktime":
		return prefix + ".mov"
	case "video/webm":
		return prefix + ".webm"
	case "audio/mpeg":
		return prefix + ".mp3"
	case "audio/wav":
		return prefix + ".wav"
	case "image/jpeg":
		return prefix + ".jpg"
	case "image/png":
		return prefix + ".png"
	case "image/gif":
		return prefix + ".gif"
	default:
		return prefix + ".bin"
	}
}

func buildReplyContent(content string) (msgType string, body string) {
	if !containsMarkdown(content) {
		b, _ := json.Marshal(map[string]string{"text": content})
		return larkim.MsgTypeText, string(b)
	}
	// Three-tier rendering strategy:
	// 1. Code blocks / tables → card (schema 2.0 markdown)
	// 2. Many \n\n paragraphs (help, status, etc.) → post rich-text (preserves blank lines)
	// 3. Other markdown → post md tag (best native rendering)
	if hasComplexMarkdown(content) {
		return larkim.MsgTypeInteractive, buildCardJSON(preprocessFeishuMarkdown(content))
	}
	if strings.Count(content, "\n\n") >= 2 {
		return larkim.MsgTypePost, buildPostJSON(content)
	}
	return larkim.MsgTypePost, buildPostMdJSON(content)
}

// hasComplexMarkdown detects code blocks or tables that require card rendering.
func hasComplexMarkdown(s string) bool {
	if strings.Contains(s, "```") {
		return true
	}
	// Table: line starting and ending with |
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if len(trimmed) > 1 && trimmed[0] == '|' && trimmed[len(trimmed)-1] == '|' {
			return true
		}
	}
	return false
}

// buildPostMdJSON builds a Feishu post message using the md tag,
// which renders markdown at normal chat font size.
func buildPostMdJSON(content string) string {
	post := map[string]any{
		"zh_cn": map[string]any{
			"content": [][]map[string]any{
				{
					{"tag": "md", "text": content},
				},
			},
		},
	}
	b, _ := json.Marshal(post)
	return string(b)
}

// preprocessFeishuMarkdown ensures code fences have a newline before them,
// which prevents rendering issues in Feishu card markdown.
// Tables, headings, blockquotes, etc. are rendered natively by the card markdown element.
func preprocessFeishuMarkdown(md string) string {
	// Ensure ``` has a newline before it (unless at start of text)
	var b strings.Builder
	b.Grow(len(md) + 32)
	for i := 0; i < len(md); i++ {
		if i > 0 && md[i] == '`' && i+2 < len(md) && md[i+1] == '`' && md[i+2] == '`' && md[i-1] != '\n' {
			b.WriteByte('\n')
		}
		b.WriteByte(md[i])
	}
	return b.String()
}

var markdownIndicators = []string{
	"```", "**", "~~", "`", "\n- ", "\n* ", "\n1. ", "\n# ", "---",
}

func containsMarkdown(s string) bool {
	for _, ind := range markdownIndicators {
		if strings.Contains(s, ind) {
			return true
		}
	}
	return false
}

// buildPostJSON converts markdown content to Feishu post (rich text) format.
func buildPostJSON(content string) string {
	lines := strings.Split(content, "\n")
	var postLines [][]map[string]any
	inCodeBlock := false
	var codeLines []string
	codeLang := ""

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "```") {
			if !inCodeBlock {
				inCodeBlock = true
				codeLang = strings.TrimPrefix(trimmed, "```")
				codeLines = nil
			} else {
				inCodeBlock = false
				postLines = append(postLines, []map[string]any{{
					"tag":      "code_block",
					"language": codeLang,
					"text":     strings.Join(codeLines, "\n"),
				}})
			}
			continue
		}

		if inCodeBlock {
			codeLines = append(codeLines, line)
			continue
		}

		// Convert # headers to bold
		headerLine := line
		for level := 6; level >= 1; level-- {
			prefix := strings.Repeat("#", level) + " "
			if strings.HasPrefix(line, prefix) {
				headerLine = "**" + strings.TrimPrefix(line, prefix) + "**"
				break
			}
		}

		elements := parseInlineMarkdown(headerLine)
		if len(elements) > 0 {
			postLines = append(postLines, elements)
		} else {
			postLines = append(postLines, []map[string]any{{"tag": "text", "text": ""}})
		}
	}

	// Handle unclosed code block
	if inCodeBlock && len(codeLines) > 0 {
		postLines = append(postLines, []map[string]any{{
			"tag":      "code_block",
			"language": codeLang,
			"text":     strings.Join(codeLines, "\n"),
		}})
	}

	post := map[string]any{
		"zh_cn": map[string]any{
			"content": postLines,
		},
	}
	b, _ := json.Marshal(post)
	return string(b)
}

// isValidFeishuHref checks whether a URL is acceptable as a Feishu post href.
// Feishu rejects non-HTTP(S) URLs with "invalid href" (code 230001).
func isValidFeishuHref(u string) bool {
	return strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")
}

// parseInlineMarkdown parses a single line of markdown into Feishu post elements.
// Supports **bold** and `code` inline formatting.
func parseInlineMarkdown(line string) []map[string]any {
	type markerDef struct {
		pattern string
		tag     string
		style   string // for text elements with style
	}
	markers := []markerDef{
		{pattern: "**", tag: "text", style: "bold"},
		{pattern: "~~", tag: "text", style: "lineThrough"},
		{pattern: "`", tag: "text", style: "code"},
		{pattern: "*", tag: "text", style: "italic"},
	}

	var elements []map[string]any
	remaining := line

	for len(remaining) > 0 {
		// Check for link [text](url)
		linkIdx := strings.Index(remaining, "[")
		if linkIdx >= 0 {
			parenClose := -1
			bracketClose := strings.Index(remaining[linkIdx:], "](")
			if bracketClose >= 0 {
				bracketClose += linkIdx
				parenClose = strings.Index(remaining[bracketClose+2:], ")")
				if parenClose >= 0 {
					parenClose += bracketClose + 2
				}
			}
			if parenClose >= 0 {
				// Check if any marker comes before this link
				foundEarlierMarker := false
				for _, m := range markers {
					idx := strings.Index(remaining, m.pattern)
					if idx >= 0 && idx < linkIdx {
						foundEarlierMarker = true
						break
					}
				}
				if !foundEarlierMarker {
					linkText := remaining[linkIdx+1 : bracketClose]
					linkURL := remaining[bracketClose+2 : parenClose]
					if isValidFeishuHref(linkURL) {
						if linkIdx > 0 {
							elements = append(elements, map[string]any{"tag": "text", "text": remaining[:linkIdx]})
						}
						elements = append(elements, map[string]any{
							"tag":  "a",
							"text": linkText,
							"href": linkURL,
						})
						remaining = remaining[parenClose+1:]
						continue
					}
				}
			}
		}

		// Find the earliest formatting marker
		bestIdx := -1
		var bestMarker markerDef
		for _, m := range markers {
			idx := strings.Index(remaining, m.pattern)
			if idx < 0 {
				continue
			}
			// For single * marker, skip if it's actually ** (bold)
			if m.pattern == "*" && idx+1 < len(remaining) && remaining[idx+1] == '*' {
				idx = findSingleAsterisk(remaining)
				if idx < 0 {
					continue
				}
			}
			if bestIdx < 0 || idx < bestIdx {
				bestIdx = idx
				bestMarker = m
			}
		}

		if bestIdx < 0 {
			if remaining != "" {
				elements = append(elements, map[string]any{"tag": "text", "text": remaining})
			}
			break
		}

		if bestIdx > 0 {
			elements = append(elements, map[string]any{"tag": "text", "text": remaining[:bestIdx]})
		}
		remaining = remaining[bestIdx+len(bestMarker.pattern):]

		closeIdx := strings.Index(remaining, bestMarker.pattern)
		// For single *, make sure we don't match ** as close
		if bestMarker.pattern == "*" {
			closeIdx = findSingleAsterisk(remaining)
		}
		if closeIdx < 0 {
			elements = append(elements, map[string]any{"tag": "text", "text": bestMarker.pattern + remaining})
			remaining = ""
			break
		}

		inner := remaining[:closeIdx]
		remaining = remaining[closeIdx+len(bestMarker.pattern):]

		elements = append(elements, map[string]any{
			"tag":   bestMarker.tag,
			"text":  inner,
			"style": []string{bestMarker.style},
		})
	}

	return elements
}

// findSingleAsterisk finds the index of a single '*' not part of '**' in s.
func findSingleAsterisk(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '*' {
			if i+1 < len(s) && s[i+1] == '*' {
				i++ // skip **
				continue
			}
			return i
		}
	}
	return -1
}

// fetchBotOpenID retrieves the bot's open_id via the Feishu bot info API.
func (p *Platform) fetchBotOpenID() (string, error) {
	resp, err := p.client.Get(context.Background(),
		"/open-apis/bot/v3/info", nil, larkcore.AccessTokenTypeTenant)
	if err != nil {
		return "", fmt.Errorf("api call: %w", err)
	}
	var result struct {
		Code int `json:"code"`
		Bot  struct {
			OpenID string `json:"open_id"`
		} `json:"bot"`
	}
	if err := json.Unmarshal(resp.RawBody, &result); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}
	if result.Code != 0 {
		return "", fmt.Errorf("api code=%d", result.Code)
	}
	return result.Bot.OpenID, nil
}

func isBotMentioned(mentions []*larkim.MentionEvent, botOpenID string) bool {
	for _, m := range mentions {
		if m.Id != nil && m.Id.OpenId != nil && *m.Id.OpenId == botOpenID {
			return true
		}
	}
	return false
}

// stripMentions removes @mention placeholders (e.g. @_user_1) from text
// so that group-chat messages like "@Bot /help" become "/help".
func stripMentions(text string, mentions []*larkim.MentionEvent) string {
	if len(mentions) == 0 {
		return strings.TrimSpace(text)
	}
	keys := make([]string, 0, len(mentions))
	for _, m := range mentions {
		if m.Key != nil {
			keys = append(keys, *m.Key)
		}
	}
	return stripMentionKeys(text, keys)
}

func stripMentionsMessage(text string, mentions []*larkim.Mention) string {
	if len(mentions) == 0 {
		return strings.TrimSpace(text)
	}
	keys := make([]string, 0, len(mentions))
	for _, m := range mentions {
		if m.Key != nil {
			keys = append(keys, *m.Key)
		}
	}
	return stripMentionKeys(text, keys)
}

func stripMentionKeys(text string, keys []string) string {
	for _, key := range keys {
		text = strings.ReplaceAll(text, key, "")
	}
	return strings.TrimSpace(text)
}

func (p *Platform) ReconstructReplyCtx(sessionKey string) (any, error) {
	// feishu:{chatID}:{userID}
	parts := strings.SplitN(sessionKey, ":", 3)
	if len(parts) < 2 || parts[0] != "feishu" {
		return nil, fmt.Errorf("feishu: invalid session key %q", sessionKey)
	}
	return replyContext{chatID: parts[1]}, nil
}

// feishuPreviewHandle stores the message ID for an editable preview message.
type feishuPreviewHandle struct {
	messageID string
	chatID    string
}

// buildCardJSON builds a Feishu interactive card JSON string with a markdown element.
// Uses schema 2.0 which supports code blocks, tables, and inline formatting.
// Card font is inherently smaller than Post/Text — this is a Feishu platform limitation.
func buildCardJSON(content string) string {
	card := map[string]any{
		"schema": "2.0",
		"config": map[string]any{
			"wide_screen_mode": true,
		},
		"body": map[string]any{
			"elements": []map[string]any{
				{
					"tag":     "markdown",
					"content": content,
				},
			},
		},
	}
	b, _ := json.Marshal(card)
	return string(b)
}

// SendPreviewStart sends a new card message and returns a handle for subsequent edits.
// Using card (interactive) type for both preview and final message so updates
// are in-place without needing to delete and resend.
func (p *Platform) SendPreviewStart(ctx context.Context, rctx any, content string) (any, error) {
	rc, ok := rctx.(replyContext)
	if !ok {
		return nil, fmt.Errorf("feishu: invalid reply context type %T", rctx)
	}

	chatID := rc.chatID
	if chatID == "" {
		return nil, fmt.Errorf("feishu: chatID is empty")
	}

	cardJSON := buildCardJSON(content)

	var msgID string
	if p.replyInThread && rc.messageID != "" {
		resp, err := p.client.Im.Message.Reply(ctx, larkim.NewReplyMessageReqBuilder().
			MessageId(rc.messageID).
			Body(larkim.NewReplyMessageReqBodyBuilder().
				MsgType(larkim.MsgTypeInteractive).
				Content(cardJSON).
				Build()).
			Build())
		if err != nil {
			return nil, fmt.Errorf("feishu: send preview (reply): %w", err)
		}
		if !resp.Success() {
			return nil, fmt.Errorf("feishu: send preview (reply) code=%d msg=%s", resp.Code, resp.Msg)
		}
		if resp.Data != nil && resp.Data.MessageId != nil {
			msgID = *resp.Data.MessageId
		}
	} else {
		resp, err := p.client.Im.Message.Create(ctx, larkim.NewCreateMessageReqBuilder().
			ReceiveIdType(larkim.ReceiveIdTypeChatId).
			Body(larkim.NewCreateMessageReqBodyBuilder().
				ReceiveId(chatID).
				MsgType(larkim.MsgTypeInteractive).
				Content(cardJSON).
				Build()).
			Build())
		if err != nil {
			return nil, fmt.Errorf("feishu: send preview: %w", err)
		}
		if !resp.Success() {
			return nil, fmt.Errorf("feishu: send preview code=%d msg=%s", resp.Code, resp.Msg)
		}
		if resp.Data != nil && resp.Data.MessageId != nil {
			msgID = *resp.Data.MessageId
		}
	}

	if msgID == "" {
		return nil, fmt.Errorf("feishu: send preview: no message ID returned")
	}

	return &feishuPreviewHandle{messageID: msgID, chatID: chatID}, nil
}

// UpdateMessage edits an existing card message identified by previewHandle.
// Uses the Patch API (HTTP PATCH) which is required for interactive card messages.
func (p *Platform) UpdateMessage(ctx context.Context, previewHandle any, content string) error {
	h, ok := previewHandle.(*feishuPreviewHandle)
	if !ok {
		return fmt.Errorf("feishu: invalid preview handle type %T", previewHandle)
	}

	processed := content
	if containsMarkdown(content) {
		processed = preprocessFeishuMarkdown(content)
	}
	cardJSON := buildCardJSON(processed)
	resp, err := p.client.Im.Message.Patch(ctx, larkim.NewPatchMessageReqBuilder().
		MessageId(h.messageID).
		Body(larkim.NewPatchMessageReqBodyBuilder().
			Content(cardJSON).
			Build()).
		Build())
	if err != nil {
		return fmt.Errorf("feishu: patch message: %w", err)
	}
	if !resp.Success() {
		return fmt.Errorf("feishu: patch message code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

// onCardAction handles card button click callbacks from Feishu.
// It extracts the action value and routes it as a regular user message,
// similar to Telegram's handleCallbackQuery.
func (p *Platform) onCardAction(event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
	if event.Event == nil || event.Event.Action == nil {
		return nil, nil
	}

	action := event.Event.Action
	operator := event.Event.Operator

	userID := ""
	if operator != nil {
		userID = operator.OpenID
	}

	if !core.AllowList(p.allowFrom, userID) {
		slog.Debug("feishu: card action from unauthorized user", "user", userID)
		return nil, nil
	}

	chatID := ""
	messageID := ""
	if event.Event.Context != nil {
		chatID = event.Event.Context.OpenChatID
		messageID = event.Event.Context.OpenMessageID
	}

	// Support both the legacy button payload key ("data") and the card framework
	// payload key ("action") so old and new interactive cards keep working.
	data := extractCardActionData(action.Value)
	if data == "" {
		slog.Debug("feishu: card action with no data value", "value", action.Value)
		return nil, nil
	}

	var sessionKey string
	if p.shareSessionInChannel {
		sessionKey = fmt.Sprintf("feishu:%s", chatID)
	} else {
		sessionKey = fmt.Sprintf("feishu:%s:%s", chatID, userID)
	}
	rctx := replyContext{messageID: messageID, chatID: chatID}

	if (strings.HasPrefix(data, "nav:") || strings.HasPrefix(data, "act:")) && p.cardNavHandler != nil {
		if card := p.cardNavHandler(data, sessionKey); card != nil {
			return p.buildCardActionCardResponse(card), nil
		}
	}

	// Handle command callbacks (cmd:/lang en, cmd:/mode yolo, etc.)
	if strings.HasPrefix(data, "cmd:") {
		command := strings.TrimPrefix(data, "cmd:")
		p.handler(p, &core.Message{
			SessionKey: sessionKey,
			Platform:   "feishu",
			UserID:     userID,
			Content:    command,
			MessageID:  messageID,
			ReplyCtx:   rctx,
		})
		// Update the card to show the chosen option and remove buttons
		return p.buildCardActionResponse(event, command), nil
	}

	if strings.HasPrefix(data, "ia:") {
		p.handler(p, &core.Message{
			SessionKey: sessionKey,
			Platform:   "feishu",
			UserID:     userID,
			Content:    data,
			MessageID:  messageID,
			ReplyCtx:   rctx,
		})
		return p.buildCardActionResponseWithLabel(event, "✅ Selected"), nil
	}

	if data == "tts:read_last" {
		p.handler(p, &core.Message{
			SessionKey: sessionKey,
			Platform:   "feishu",
			UserID:     userID,
			Content:    "tts read last",
			MessageID:  messageID,
			ReplyCtx:   rctx,
		})
		return p.buildCardActionResponseWithLabel(event, "🔊 Requested"), nil
	}

	// Handle voice confirmation callbacks (voice:confirm, voice:modify)
	switch data {
	case "voice:confirm":
		p.handler(p, &core.Message{
			SessionKey: sessionKey,
			Platform:   "feishu",
			UserID:     userID,
			Content:    "voice confirm",
			MessageID:  messageID,
			ReplyCtx:   rctx,
		})
		return p.buildCardActionResponseWithLabel(event, "✅ Confirmed"), nil
	case "voice:modify":
		p.handler(p, &core.Message{
			SessionKey: sessionKey,
			Platform:   "feishu",
			UserID:     userID,
			Content:    "voice modify",
			MessageID:  messageID,
			ReplyCtx:   rctx,
		})
		return p.buildCardActionResponseWithLabel(event, "✏️ Modify requested"), nil
	}

	// Handle permission callbacks (perm:allow, perm:deny, perm:allow_all)
	var responseText string
	var choiceLabel string
	switch data {
	case "perm:allow":
		responseText = "allow"
		choiceLabel = "✅ Allowed"
	case "perm:deny":
		responseText = "deny"
		choiceLabel = "❌ Denied"
	case "perm:allow_all":
		responseText = "allow all"
		choiceLabel = "✅ Allow All"
	default:
		slog.Debug("feishu: unknown card action data", "data", data)
		return nil, nil
	}

	p.handler(p, &core.Message{
		SessionKey: sessionKey,
		Platform:   "feishu",
		UserID:     userID,
		Content:    responseText,
		MessageID:  messageID,
		ReplyCtx:   rctx,
	})

	// Update the card to show the choice and remove buttons
	return p.buildCardActionResponseWithLabel(event, choiceLabel), nil
}

func (p *Platform) buildCardActionCardResponse(card *core.Card) *callback.CardActionTriggerResponse {
	if card == nil {
		return nil
	}
	data := renderCardMap(card, "")
	return &callback.CardActionTriggerResponse{
		Toast: &callback.Toast{Type: "info", Content: "Updated"},
		Card:  &callback.Card{Type: "raw", Data: data},
	}
}

// buildCardActionResponse returns a card update that shows the chosen command.
func (p *Platform) buildCardActionResponse(event *callback.CardActionTriggerEvent, command string) *callback.CardActionTriggerResponse {
	card := map[string]any{
		"schema": "2.0",
		"config": map[string]any{"wide_screen_mode": true},
		"body": map[string]any{
			"elements": []map[string]any{
				{"tag": "markdown", "content": "> " + command},
			},
		},
	}
	return &callback.CardActionTriggerResponse{
		Toast: &callback.Toast{Type: "info", Content: command},
		Card:  &callback.Card{Type: "raw", Data: card},
	}
}

// buildCardActionResponseWithLabel returns a card update that shows the permission choice.
func (p *Platform) buildCardActionResponseWithLabel(event *callback.CardActionTriggerEvent, label string) *callback.CardActionTriggerResponse {
	card := map[string]any{
		"schema": "2.0",
		"config": map[string]any{"wide_screen_mode": true},
		"body": map[string]any{
			"elements": []map[string]any{
				{"tag": "markdown", "content": label},
			},
		},
	}
	return &callback.CardActionTriggerResponse{
		Toast: &callback.Toast{Type: "info", Content: label},
		Card:  &callback.Card{Type: "raw", Data: card},
	}
}

func extractCardActionData(value map[string]any) string {
	if value == nil {
		return ""
	}
	if data, _ := value["action"].(string); data != "" {
		return data
	}
	data, _ := value["data"].(string)
	return data
}

// SendWithButtons sends a card message with interactive buttons.
// Implements core.InlineButtonSender interface.
func (p *Platform) SendWithButtons(ctx context.Context, rctx any, content string, buttons [][]core.ButtonOption) error {
	rc, ok := rctx.(replyContext)
	if !ok {
		return fmt.Errorf("feishu: invalid reply context type %T", rctx)
	}

	cardJSON := renderCard(cardFromButtons(content, buttons), "")

	if p.replyInThread && rc.messageID != "" {
		resp, err := p.client.Im.Message.Reply(ctx, larkim.NewReplyMessageReqBuilder().
			MessageId(rc.messageID).
			Body(larkim.NewReplyMessageReqBodyBuilder().
				MsgType(larkim.MsgTypeInteractive).
				Content(cardJSON).
				Build()).
			Build())
		if err != nil {
			return fmt.Errorf("feishu: sendWithButtons reply: %w", err)
		}
		if !resp.Success() {
			return fmt.Errorf("feishu: sendWithButtons reply code=%d msg=%s", resp.Code, resp.Msg)
		}
		return nil
	}

	if rc.chatID == "" {
		return fmt.Errorf("feishu: chatID is empty, cannot send message with buttons")
	}

	resp, err := p.client.Im.Message.Create(ctx, larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(larkim.ReceiveIdTypeChatId).
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(rc.chatID).
			MsgType(larkim.MsgTypeInteractive).
			Content(cardJSON).
			Build()).
		Build())
	if err != nil {
		return fmt.Errorf("feishu: sendWithButtons: %w", err)
	}
	if !resp.Success() {
		return fmt.Errorf("feishu: sendWithButtons code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

func (p *Platform) ReplyCard(ctx context.Context, rctx any, card *core.Card) error {
	rc, ok := rctx.(replyContext)
	if !ok {
		return fmt.Errorf("feishu: invalid reply context type %T", rctx)
	}

	cardJSON := renderCard(card, "")
	if p.replyInThread && rc.messageID != "" {
		resp, err := p.client.Im.Message.Reply(ctx, larkim.NewReplyMessageReqBuilder().
			MessageId(rc.messageID).
			Body(larkim.NewReplyMessageReqBodyBuilder().
				MsgType(larkim.MsgTypeInteractive).
				Content(cardJSON).
				Build()).
			Build())
		if err != nil {
			return fmt.Errorf("feishu: reply card: %w", err)
		}
		if !resp.Success() {
			return fmt.Errorf("feishu: reply card code=%d msg=%s", resp.Code, resp.Msg)
		}
		return nil
	}
	if rc.chatID == "" {
		return fmt.Errorf("feishu: chatID is empty, cannot send card")
	}
	resp, err := p.client.Im.Message.Create(ctx, larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(larkim.ReceiveIdTypeChatId).
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(rc.chatID).
			MsgType(larkim.MsgTypeInteractive).
			Content(cardJSON).
			Build()).
		Build())
	if err != nil {
		return fmt.Errorf("feishu: reply card create: %w", err)
	}
	if !resp.Success() {
		return fmt.Errorf("feishu: reply card code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

func (p *Platform) SendCard(ctx context.Context, rctx any, card *core.Card) error {
	return p.ReplyCard(ctx, rctx, card)
}

func cardFromButtons(content string, buttons [][]core.ButtonOption) *core.Card {
	cb := core.NewCard().Markdown(content)
	for _, row := range buttons {
		if len(row) == 0 {
			continue
		}
		cardButtons := make([]core.CardButton, 0, len(row))
		for _, btn := range row {
			cardButtons = append(cardButtons, core.DefaultBtn(btn.Text, btn.Data))
		}
		cb.Buttons(cardButtons...)
	}
	return cb.Build()
}

// buildCardWithButtonsJSON builds a Feishu interactive card with markdown content and action buttons.
func buildCardWithButtonsJSON(content string, buttons [][]core.ButtonOption) string {
	processed := content
	if containsMarkdown(content) {
		processed = preprocessFeishuMarkdown(content)
	}

	// Schema 2.0: buttons go directly in elements array (no "action" wrapper)
	elements := []any{
		map[string]any{
			"tag":     "markdown",
			"content": processed,
		},
	}
	for _, row := range buttons {
		for _, b := range row {
			btnType := "default"
			if strings.Contains(b.Data, "allow") {
				btnType = "primary"
			} else if strings.Contains(b.Data, "deny") {
				btnType = "danger"
			}
			elements = append(elements, map[string]any{
				"tag":  "button",
				"text": map[string]any{"tag": "plain_text", "content": b.Text},
				"type": btnType,
				"size": "medium",
				"value": map[string]any{
					"data": b.Data,
				},
			})
		}
	}

	card := map[string]any{
		"schema": "2.0",
		"config": map[string]any{"wide_screen_mode": true},
		"body": map[string]any{
			"elements": elements,
		},
	}
	b, _ := json.Marshal(card)
	return string(b)
}

func (p *Platform) Stop() error {
	if p.cancel != nil {
		p.cancel()
	}
	return nil
}

// SendAudio uploads audio bytes to Feishu and sends a voice message.
// Implements core.AudioSender interface.
// Feishu audio messages require opus format; non-opus input is converted via ffmpeg.
func (p *Platform) SendAudio(ctx context.Context, rctx any, audio []byte, format string) error {
	rc, ok := rctx.(replyContext)
	if !ok {
		return fmt.Errorf("feishu: SendAudio: invalid reply context type %T", rctx)
	}

	// Feishu only supports opus for audio messages; convert if needed
	if format != "opus" {
		converted, err := core.ConvertAudioToOpus(ctx, audio, format)
		if err != nil {
			return fmt.Errorf("feishu: convert to opus: %w", err)
		}
		audio = converted
		format = "opus"
	}

	// Upload file to Feishu as opus
	uploadResp, err := p.client.Im.File.Create(ctx,
		larkim.NewCreateFileReqBuilder().
			Body(larkim.NewCreateFileReqBodyBuilder().
				FileType(larkim.FileTypeOpus).
				FileName("tts_audio.opus").
				File(bytes.NewReader(audio)).
				Build()).
			Build())
	if err != nil {
		return fmt.Errorf("feishu: upload audio: %w", err)
	}
	if !uploadResp.Success() {
		return fmt.Errorf("feishu: upload audio code=%d msg=%s", uploadResp.Code, uploadResp.Msg)
	}
	if uploadResp.Data == nil || uploadResp.Data.FileKey == nil {
		return fmt.Errorf("feishu: upload audio: no file_key returned")
	}
	fileKey := *uploadResp.Data.FileKey

	slog.Debug("feishu: audio uploaded", "file_key", fileKey, "format", format, "size", len(audio))

	// Build audio message content
	audioMsg := larkim.MessageAudio{FileKey: fileKey}
	audioContent, err := audioMsg.String()
	if err != nil {
		return fmt.Errorf("feishu: build audio message: %w", err)
	}

	// Send audio message to chat
	sendResp, err := p.client.Im.Message.Create(ctx, larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(larkim.ReceiveIdTypeChatId).
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(rc.chatID).
			MsgType(larkim.MsgTypeAudio).
			Content(audioContent).
			Build()).
		Build())
	if err != nil {
		return fmt.Errorf("feishu: send audio message: %w", err)
	}
	if !sendResp.Success() {
		return fmt.Errorf("feishu: send audio message code=%d msg=%s", sendResp.Code, sendResp.Msg)
	}
	return nil
}

type postElement struct {
	Tag      string `json:"tag"`
	Text     string `json:"text,omitempty"`
	Content  string `json:"content,omitempty"`
	Code     string `json:"code,omitempty"`
	Language string `json:"language,omitempty"`
	UserName string `json:"user_name,omitempty"`
	ImageKey string `json:"image_key,omitempty"`
	Href     string `json:"href,omitempty"`
}

type postLang struct {
	Title   string          `json:"title"`
	Content [][]postElement `json:"content"`
}

// parsePostContent handles both formats of feishu post content:
// 1. {"title":"...", "content":[[...]]}  (receive event)
// 2. {"zh_cn":{"title":"...", "content":[[...]]}}  (some SDK versions)
func (p *Platform) parsePostContent(messageID, raw string) ([]string, []core.ImageAttachment) {
	// try flat format first
	var flat postLang
	if err := json.Unmarshal([]byte(raw), &flat); err == nil && flat.Content != nil {
		return p.extractPostParts(messageID, &flat)
	}
	// try language-keyed format
	var langMap map[string]postLang
	if err := json.Unmarshal([]byte(raw), &langMap); err == nil {
		for _, lang := range langMap {
			return p.extractPostParts(messageID, &lang)
		}
	}
	slog.Error("feishu: failed to parse post content", "raw", raw)
	return nil, nil
}

func (p *Platform) fetchQuotedMessage(parentID, rootID *string) *quotedMessage {
	refID := ""
	switch {
	case parentID != nil && strings.TrimSpace(*parentID) != "":
		refID = strings.TrimSpace(*parentID)
	case rootID != nil && strings.TrimSpace(*rootID) != "":
		refID = strings.TrimSpace(*rootID)
	}
	if refID == "" {
		return nil
	}

	resp, err := p.client.Im.Message.Get(context.Background(), larkim.NewGetMessageReqBuilder().
		MessageId(refID).
		Build())
	if err != nil {
		slog.Debug("feishu: fetch quoted message failed", "message_id", refID, "error", err)
		return nil
	}
	if !resp.Success() {
		slog.Debug("feishu: fetch quoted message unsuccessful", "message_id", refID, "code", resp.Code, "msg", resp.Msg)
		return nil
	}
	if resp.Data == nil || len(resp.Data.Items) == 0 || resp.Data.Items[0] == nil {
		return nil
	}
	return p.extractQuotedMessage(resp.Data.Items[0])
}

func (p *Platform) extractQuotedMessage(msg *larkim.Message) *quotedMessage {
	if msg == nil || msg.Body == nil || msg.Body.Content == nil {
		return nil
	}

	msgType := ""
	if msg.MsgType != nil {
		msgType = *msg.MsgType
	}

	content := p.extractQuotedMessageContent(msg.MessageId, msgType, *msg.Body.Content, msg.Mentions)
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}

	quoted := &quotedMessage{content: content}
	if msg.MessageId != nil {
		quoted.messageID = *msg.MessageId
	}
	if msg.Sender != nil {
		if msg.Sender.Id != nil {
			quoted.userID = *msg.Sender.Id
		}
		if msg.Sender.SenderType != nil && *msg.Sender.SenderType != "user" {
			quoted.userName = *msg.Sender.SenderType
		}
	}
	return quoted
}

func (p *Platform) extractQuotedMessageContent(messageID *string, msgType, raw string, mentions []*larkim.Mention) string {
	switch msgType {
	case "text":
		var textBody struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(raw), &textBody); err != nil {
			return ""
		}
		return stripMentionsMessage(textBody.Text, mentions)
	case "post":
		id := ""
		if messageID != nil {
			id = *messageID
		}
		textParts, _ := p.parsePostContent(id, raw)
		return stripMentionsMessage(strings.Join(textParts, "\n"), mentions)
	default:
		return ""
	}
}

func (p *Platform) extractPostParts(messageID string, post *postLang) ([]string, []core.ImageAttachment) {
	var textParts []string
	var images []core.ImageAttachment
	if post.Title != "" {
		textParts = append(textParts, post.Title)
	}
	for _, line := range post.Content {
		for _, elem := range line {
			switch elem.Tag {
			case "text", "a", "at":
				if text := postElementText(elem); text != "" {
					textParts = append(textParts, text)
				}
			case "md", "code", "code_block", "pre":
				if text := postElementCode(elem); text != "" {
					textParts = append(textParts, text)
				}
			case "img":
				if elem.ImageKey != "" {
					imgData, mimeType, err := p.downloadImage(messageID, elem.ImageKey)
					if err != nil {
						slog.Error("feishu: download post image failed", "error", err, "key", elem.ImageKey)
						continue
					}
					images = append(images, core.ImageAttachment{MimeType: mimeType, Data: imgData})
				}
			default:
				if text := postElementText(elem); text != "" {
					textParts = append(textParts, text)
				}
			}
		}
	}
	return textParts, images
}

func postElementText(elem postElement) string {
	switch {
	case elem.Text != "":
		return elem.Text
	case elem.Content != "":
		return elem.Content
	case elem.Code != "":
		return elem.Code
	case elem.UserName != "":
		return "@" + elem.UserName
	default:
		return ""
	}
}

func postElementCode(elem postElement) string {
	code := postElementText(elem)
	if code == "" {
		return ""
	}
	if strings.Contains(code, "```") {
		return code
	}
	if elem.Language != "" {
		return "```" + elem.Language + "\n" + code + "\n```"
	}
	return "```\n" + code + "\n```"
}
