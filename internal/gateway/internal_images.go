package gateway

import (
	"encoding/base64"
	"net/http"
	"time"

	"github.com/432539/gpt2api/internal/upstream/chatgpt"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// InternalImageRequest gpt2api 内部生图请求（由 sub2api 直接调用，绕过账号池）
type InternalImageRequest struct {
	Token    string `json:"token"`     // OAuth access token
	DeviceID string `json:"device_id"` // oai-device-id（可选，留空自动生成）
	Model    string `json:"model"`     // gpt-image-2 等
	Prompt   string `json:"prompt"`
	Size     string `json:"size"`
	N        int    `json:"n"`
	ProxyURL string `json:"proxy_url"` // 可选代理
}

// InternalImageResponse 生图结果
type InternalImageResponse struct {
	Images []InternalImageItem `json:"images"`
}

// InternalImageItem 单张图片
type InternalImageItem struct {
	B64JSON string `json:"b64_json,omitempty"`
	URL     string `json:"url,omitempty"`
	Size    string `json:"size,omitempty"`
	Error   string `json:"error,omitempty"`
}

// HandleInternalImageGenerate 内部生图端点，接受 token 直传
// POST /internal/generate
func HandleInternalImageGenerate(c *gin.Context) {
	var req InternalImageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request: " + err.Error()})
		return
	}
	if req.Token == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "token is required"})
		return
	}
	if req.Prompt == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "prompt is required"})
		return
	}
	if req.N <= 0 {
		req.N = 1
	}
	if req.Size == "" {
		req.Size = "1024x1024"
	}
	if req.DeviceID == "" {
		req.DeviceID = uuid.New().String()
	}

	opts := chatgpt.Options{
		AuthToken:  req.Token,
		DeviceID:   req.DeviceID,
		SessionID:  uuid.New().String(),
		Timeout:    300 * time.Second,
		SSETimeout: 120 * time.Second,
		UserAgent:  chatgpt.DefaultUserAgent,
	}
	if req.ProxyURL != "" {
		opts.ProxyURL = req.ProxyURL
	}

	client, err := chatgpt.New(opts)
	if err != nil {
		zap.L().Error("internal_images: create client failed", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create upstream client"})
		return
	}

	// 初始化会话（收 cookie + oai-did）
	_ = client.Bootstrap(c.Request.Context())
	_ = client.InitConversation(c.Request.Context(), "picture_v2")

	var result InternalImageResponse
	for i := 0; i < req.N; i++ {
		imageOpt := chatgpt.ImageConvOpts{
			Prompt:        req.Prompt,
			UpstreamModel: req.Model,
			SSETimeout:    120 * time.Second,
		}

		// prepare → 拿 conduit_token
		conduitToken, err := client.PrepareFConversation(c.Request.Context(), imageOpt)
		if err != nil {
			zap.L().Error("internal_images: prepare failed", zap.Error(err))
			result.Images = append(result.Images, InternalImageItem{Error: "prepare: " + err.Error()})
			continue
		}
		imageOpt.ConduitToken = conduitToken

		// chat-requirements → 拿 chat_token + proof
		cr, err := client.ChatRequirementsV2(c.Request.Context())
		if err != nil {
			zap.L().Error("internal_images: chat requirements failed", zap.Error(err))
			result.Images = append(result.Images, InternalImageItem{Error: "requirements: " + err.Error()})
			continue
		}
		imageOpt.ChatToken = cr.Token
		if proof := cr.SolveProof(opts.UserAgent); proof != "" {
			imageOpt.ProofToken = proof
		}

		// SSE 流式生图
		stream, err := client.StreamFConversation(c.Request.Context(), imageOpt)
		if err != nil {
			zap.L().Error("internal_images: stream failed", zap.Error(err))
			result.Images = append(result.Images, InternalImageItem{Error: "stream: " + err.Error()})
			continue
		}

		sseResult := chatgpt.ParseImageSSE(stream)

		// SSE 直出没图则轮询（用 FileIDs + SedimentIDs）
		fileRefs := sseResult.FileIDs
		fileRefs = append(fileRefs, sseResult.SedimentIDs...)
		if len(fileRefs) == 0 && sseResult.ConversationID != "" {
			_, newRefs, _ := client.PollConversationForImages(c.Request.Context(), sseResult.ConversationID, chatgpt.PollOpts{
				MaxWait:   240 * time.Second,
				Interval:  3 * time.Second,
				ExpectedN: 1,
			})
			fileRefs = newRefs
		}

		// 下载图片
		var b64 string
		if len(fileRefs) > 0 {
			signedURL, err := client.ImageDownloadURL(c.Request.Context(), sseResult.ConversationID, fileRefs[0])
			if err != nil {
				zap.L().Error("internal_images: download url failed", zap.Error(err))
				result.Images = append(result.Images, InternalImageItem{Error: "download: " + err.Error()})
				continue
			}
			imgBytes, mime, err := client.FetchImage(c.Request.Context(), signedURL, 20<<20)
			if err != nil {
				zap.L().Error("internal_images: fetch image failed", zap.Error(err))
				result.Images = append(result.Images, InternalImageItem{Error: "fetch: " + err.Error()})
				continue
			}
			b64 = "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(imgBytes)
		}

		result.Images = append(result.Images, InternalImageItem{
			B64JSON: b64,
			Size:    req.Size,
		})
	}

	c.JSON(http.StatusOK, result)
}
